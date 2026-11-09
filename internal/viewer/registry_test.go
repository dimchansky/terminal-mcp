package viewer

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clock   *fakeClock
	at      time.Time
	f       func()
	stopped bool
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

// Advance moves time and fires due timers outside the clock lock.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	for _, t := range c.timers {
		if !t.stopped && !t.at.After(c.now) {
			t.stopped = true
			due = append(due, t)
		}
	}
	c.mu.Unlock()
	for _, t := range due {
		t.f()
	}
}

type recorder struct {
	mu      sync.Mutex
	causes  []error
	at      []time.Time
	stopped chan struct{}
}

func newRecorder() *recorder { return &recorder{stopped: make(chan struct{}, 16)} }

func (r *recorder) Stop(cause error, deadline time.Time) {
	r.mu.Lock()
	r.causes = append(r.causes, cause)
	r.at = append(r.at, deadline)
	r.mu.Unlock()
	r.stopped <- struct{}{}
}

func (r *recorder) calls() ([]error, []time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.causes...), append([]time.Time(nil), r.at...)
}

func newTestRegistry(t *testing.T) (*Registry, *fakeClock, *bytes.Buffer) {
	t.Helper()
	clock := newFakeClock()
	var logs bytes.Buffer
	r := NewRegistry(Config{Clock: clock, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	return r, clock, &logs
}

func login(t *testing.T, r *Registry) (string, *Session) {
	t.Helper()
	code, _, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	token, s, err := r.Login(code)
	if err != nil {
		t.Fatal(err)
	}
	return token, s
}

func TestCodesAreSingleUseAndExpire(t *testing.T) {
	r, clock, _ := newTestRegistry(t)
	code, expires, err := r.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 26+5 || strings.Count(code, "-") != 5 || !expires.Equal(clock.Now().Add(5*time.Minute)) {
		t.Fatalf("code %q expires %v", code, expires)
	}
	// What a person pastes: lower case, spaces, look-alike letters.
	pasted := strings.ToLower(strings.ReplaceAll(code, "-", " "))
	pasted = strings.NewReplacer("0", "o", "1", "l").Replace(pasted)
	if _, _, err := r.Login(pasted); err != nil {
		t.Fatalf("normalized code: %v", err)
	}
	if _, _, err := r.Login(code); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("reused code: %v", err)
	}

	code, _, _ = r.Issue()
	clock.Advance(5 * time.Minute)
	if _, _, err := r.Login(code); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired code: %v", err)
	}

	var codes []string
	for i := 0; i < 5; i++ {
		c, _, _ := r.Issue()
		codes = append(codes, c)
	}
	if _, pending := r.Counts(); pending != 4 {
		t.Fatalf("pending %d", pending)
	}
	if _, _, err := r.Login(codes[0]); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("oldest code must be dropped: %v", err)
	}
	if _, _, err := r.Login(codes[4]); err != nil {
		t.Fatalf("newest code: %v", err)
	}
}

func TestFailedLoginsAreThrottledButValidCodesWin(t *testing.T) {
	r, clock, _ := newTestRegistry(t)
	code, _, _ := r.Issue()
	for i := 0; i < 5; i++ {
		if _, _, err := r.Login("junk"); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	if _, _, err := r.Login("junk"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("sixth failure: %v", err)
	}
	if _, _, err := r.Login(code); err != nil {
		t.Fatalf("valid code while throttled: %v", err)
	}
	clock.Advance(time.Minute + time.Second)
	if _, _, err := r.Login("junk"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("after the window: %v", err)
	}
}

func TestSessionsExpireAndRenew(t *testing.T) {
	r, clock, _ := newTestRegistry(t)
	token, s := login(t, r)
	res := newRecorder()
	if _, err := s.Attach(KindStream, res); err != nil {
		t.Fatal(err)
	}
	clock.Advance(20 * time.Minute)
	if _, err := r.Renew(token); err != nil {
		t.Fatal(err)
	}
	clock.Advance(20 * time.Minute) // 40 min since login, 20 since renew.
	if _, err := r.Authorize(token); err != nil {
		t.Fatalf("renewed session: %v", err)
	}
	clock.Advance(10 * time.Minute) // Idle expiry fires.
	<-res.stopped
	if !errors.Is(context.Cause(s.Context()), ErrExpired) {
		t.Fatalf("cause %v", context.Cause(s.Context()))
	}
	if causes, _ := res.calls(); len(causes) != 1 || !errors.Is(causes[0], ErrExpired) {
		t.Fatalf("resource stops %v", causes)
	}
	if _, err := r.Authorize(token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired token: %v", err)
	}
}

func TestRenewNeverPassesTheAbsoluteExpiry(t *testing.T) {
	r, clock, _ := newTestRegistry(t)
	token, s := login(t, r)
	for i := 0; i < 24; i++ { // 12 h of renewals every 29 min.
		clock.Advance(29 * time.Minute)
		if _, err := r.Renew(token); err != nil {
			idle, abs := s.Expiry()
			if !errors.Is(err, ErrUnauthorized) || clock.Now().Before(abs) {
				t.Fatalf("renew %d at %v (idle %v, absolute %v): %v", i, clock.Now(), idle, abs, err)
			}
			return
		}
		if idle, abs := s.Expiry(); idle.After(abs) {
			t.Fatalf("idle %v after absolute %v", idle, abs)
		}
	}
	clock.Advance(time.Hour)
	if _, err := r.Authorize(token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("after absolute expiry: %v", err)
	}
}

func TestNewLoginReplacesTheLeastRecentlyUsedSession(t *testing.T) {
	r, clock, _ := newTestRegistry(t)
	var tokens []string
	var sessions []*Session
	for i := 0; i < 8; i++ {
		token, s := login(t, r)
		tokens, sessions = append(tokens, token), append(sessions, s)
		clock.Advance(time.Second)
	}
	if _, err := r.Authorize(tokens[0]); err != nil { // The oldest is used again.
		t.Fatal(err)
	}
	login(t, r)
	if _, err := r.Authorize(tokens[1]); !errors.Is(err, ErrReplaced) {
		t.Fatalf("LRU session: %v", err)
	}
	if !errors.Is(context.Cause(sessions[1].Context()), ErrReplaced) {
		t.Fatalf("cause %v", context.Cause(sessions[1].Context()))
	}
	if _, err := r.Authorize(tokens[0]); err != nil {
		t.Fatalf("recently used session: %v", err)
	}
	if n, _ := r.Counts(); n != 8 {
		t.Fatalf("sessions %d", n)
	}
}

func TestLogoutAndRevoke(t *testing.T) {
	r, _, _ := newTestRegistry(t)
	token, s := login(t, r)
	if err := r.Logout(token); err != nil || !errors.Is(context.Cause(s.Context()), ErrLoggedOut) {
		t.Fatalf("logout: %v %v", err, context.Cause(s.Context()))
	}
	if err := r.Logout(token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("second logout: %v", err)
	}
	t1, s1 := login(t, r)
	code, _, _ := r.Issue()
	r.RevokeAll()
	if _, err := r.Authorize(t1); !errors.Is(err, ErrUnauthorized) || !errors.Is(context.Cause(s1.Context()), ErrRevoked) {
		t.Fatalf("revoked: %v %v", err, context.Cause(s1.Context()))
	}
	if _, _, err := r.Login(code); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("pending code after revoke: %v", err)
	}
	if _, _, err := r.Issue(); err != nil {
		t.Fatalf("revoke does not close admission: %v", err)
	}
}

func TestFenceClosesAdmissionThenStops(t *testing.T) {
	r, clock, _ := newTestRegistry(t)
	token, s := login(t, r)
	res := newRecorder()
	if _, err := s.Attach(KindStream, res); err != nil {
		t.Fatal(err)
	}
	code, _, _ := r.Issue()

	r.CloseAdmission(StopForce)
	r.CloseAdmission(StopNormal) // The first reason wins.
	for name, err := range map[string]error{
		"issue": func() error { _, _, err := r.Issue(); return err }(),
		"login": func() error { _, _, err := r.Login(code); return err }(),
		"auth":  func() error { _, err := r.Authorize(token); return err }(),
		"renew": func() error { _, err := r.Renew(token); return err }(),
	} {
		if !errors.Is(err, ErrStopping) {
			t.Fatalf("%s after admission closed: %v", name, err)
		}
	}
	if s.Context().Err() != nil {
		t.Fatal("admission close alone must not end sessions")
	}
	r.StopAll(time.Second)
	causes, deadlines := res.calls()
	var stop *StopError
	if len(causes) != 1 || !errors.As(causes[0], &stop) || stop.Reason != StopForce ||
		!deadlines[0].Equal(clock.Now().Add(time.Second)) {
		t.Fatalf("stop %v %v", causes, deadlines)
	}
	if !errors.As(context.Cause(s.Context()), &stop) {
		t.Fatalf("session cause %v", context.Cause(s.Context()))
	}
	r.StopAll(0) // Idempotent.
	if causes, _ := res.calls(); len(causes) != 1 {
		t.Fatalf("resource stopped twice: %v", causes)
	}

	other, _, _ := newTestRegistry(t)
	_, s2 := login(t, other)
	other.StopAll(0)
	if !errors.As(context.Cause(s2.Context()), &stop) || stop.Reason != StopAbnormal {
		t.Fatalf("StopAll without a reason: %v", context.Cause(s2.Context()))
	}
}

func TestAttachLimitsAndRelease(t *testing.T) {
	clock := newFakeClock()
	r := NewRegistry(Config{Clock: clock, Logger: slog.New(slog.DiscardHandler),
		Limits: map[Kind]Limit{KindStream: {PerSession: 1, Total: 2}}})
	_, s1 := login(t, r)
	_, s2 := login(t, r)
	_, s3 := login(t, r)
	rel1, err := s1.Attach(KindStream, newRecorder())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Attach(KindStream, newRecorder()); !errors.Is(err, ErrCapacity) {
		t.Fatalf("per-session limit: %v", err)
	}
	if _, err := s1.Attach(KindWindow, newRecorder()); !errors.Is(err, ErrCapacity) {
		t.Fatalf("unconfigured kind: %v", err)
	}
	rel2, err := s2.Attach(KindStream, newRecorder())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s3.Attach(KindStream, newRecorder()); !errors.Is(err, ErrCapacity) {
		t.Fatalf("total limit: %v", err)
	}
	rel1()
	rel1() // Idempotent.
	rel3, err := s3.Attach(KindStream, newRecorder())
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	rel2()
	rel3()

	// A released resource is never stopped; a closed session refuses.
	res := newRecorder()
	rel, err := s1.Attach(KindStream, res)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	r.RevokeAll()
	if causes, _ := res.calls(); len(causes) != 0 {
		t.Fatalf("released resource stopped: %v", causes)
	}
	if _, err := s1.Attach(KindStream, newRecorder()); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("attach after close: %v", err)
	}
	if r.resources[KindStream] != 0 {
		t.Fatalf("totals leaked: %v", r.resources)
	}
}

func TestLogsNeverContainSecrets(t *testing.T) {
	r, clock, logs := newTestRegistry(t)
	const junk = "JUNK1-JUNK2-JUNK3"
	for i := 0; i < 6; i++ {
		_, _, _ = r.Login(junk)
	}
	for i := 0; i < 9; i++ { // The ninth replaces a session.
		_, _ = login(t, r)
	}
	code, _, _ := r.Issue()
	token, _, _ := r.Login(code)
	_ = r.Logout(token)
	_, _ = login(t, r)
	clock.Advance(time.Hour)
	r.RevokeAll()
	r.CloseAdmission(StopNormal)
	out := logs.String()
	if strings.Contains(out, code) || strings.Contains(out, strings.ReplaceAll(code, "-", "")) || strings.Contains(out, token) ||
		strings.Contains(out, "JUNK") {
		t.Fatalf("secret in logs:\n%s", out)
	}
	for _, event := range []string{"issued", "login", "logout", "expired", "revoked", "admission closed", "replaced", "throttled"} {
		if !strings.Contains(out, event) {
			t.Fatalf("missing %q event in logs:\n%s", event, out)
		}
	}
}

// TestConcurrentUse runs logins, authorizations, attachments, releases,
// renewals, expiry and a fence concurrently under -race.
func TestConcurrentUse(t *testing.T) {
	r := NewRegistry(Config{Logger: slog.New(slog.DiscardHandler), IdleTTL: 5 * time.Millisecond})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				code, _, err := r.Issue()
				if err != nil {
					return // Fenced.
				}
				token, s, err := r.Login(code)
				if err != nil {
					continue
				}
				if rel, err := s.Attach(KindDetail, newRecorder()); err == nil {
					_, _ = r.Renew(token)
					rel()
				}
				_, _ = r.Authorize(token)
				if i%3 == 0 {
					_ = r.Logout(token)
				}
			}
		}()
	}
	time.Sleep(5 * time.Millisecond)
	r.CloseAdmission(StopNormal)
	r.StopAll(0)
	wg.Wait()
	if n, _ := r.Counts(); n != 0 {
		t.Fatalf("sessions after stop: %d", n)
	}
	for kind, n := range r.resources {
		if n != 0 {
			t.Fatalf("%s total leaked: %d", kind, n)
		}
	}
}

// TestReleaseWaitsForAnInFlightStop: after release returns, Stop is neither
// running nor called later.
func TestReleaseWaitsForAnInFlightStop(t *testing.T) {
	r, _, _ := newTestRegistry(t)
	_, s := login(t, r)
	entered, proceed := make(chan struct{}), make(chan struct{})
	var stopping sync.Mutex
	running := false
	slow := resourceFunc(func(error, time.Time) {
		stopping.Lock()
		running = true
		stopping.Unlock()
		close(entered)
		<-proceed
		stopping.Lock()
		running = false
		stopping.Unlock()
	})
	rel, _ := s.Attach(KindWindow, slow)
	go r.RevokeAll()
	<-entered
	released := make(chan struct{})
	go func() { rel(); close(released) }()
	select {
	case <-released:
		t.Fatal("release returned while Stop was running")
	case <-time.After(20 * time.Millisecond):
	}
	close(proceed)
	<-released
	stopping.Lock()
	defer stopping.Unlock()
	if running {
		t.Fatal("Stop still running after release returned")
	}
}

type resourceFunc func(error, time.Time)

func (f resourceFunc) Stop(cause error, deadline time.Time) { f(cause, deadline) }

func TestPasteNormalizationIgnoresAnySpaceOrDash(t *testing.T) {
	r, _, _ := newTestRegistry(t)
	code, _, _ := r.Issue()
	pasted := " " + strings.ReplaceAll(code, "-", "\u00a0\u2013") + "\n"
	if _, _, err := r.Login(pasted); err != nil {
		t.Fatalf("pasted code: %v", err)
	}
}

func TestStopFollowsContextCancellation(t *testing.T) {
	r, _, _ := newTestRegistry(t)
	_, s := login(t, r)
	sawCanceled := make(chan bool, 1)
	_, err := s.Attach(KindStream, resourceFunc(func(error, time.Time) {
		sawCanceled <- s.Context().Err() != nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	r.RevokeAll()
	if !<-sawCanceled {
		t.Fatal("Stop ran before the session context was canceled")
	}
}

func TestStopDuringRenewReportsStopping(t *testing.T) {
	r, _, _ := newTestRegistry(t)
	token, _ := login(t, r)
	r.CloseAdmission(StopUpdate)
	if _, err := r.Renew(token); !errors.Is(err, ErrStopping) {
		t.Fatalf("renew while stopping: %v", err)
	}
	if err := r.Logout(token); !errors.Is(err, ErrStopping) {
		t.Fatalf("logout while stopping: %v", err)
	}
}
