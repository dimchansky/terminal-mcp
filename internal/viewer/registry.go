// Package viewer owns read-only dashboard viewer authorization: single-use
// pairing codes issued to the owner's CLI, expiring viewer sessions, their
// attached resources, and the fence that stops viewers at shutdown.
//
// A viewer credential grants read access to the complete audit, which can
// contain secrets; it never authorizes MCP tools or administration. Holders of
// the MCP bearer can issue codes, so this is not a new trust boundary.
package viewer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
)

var (
	// ErrUnauthorized: unknown, used or expired code or token. Responses
	// for all of these are identical, so they reveal nothing.
	ErrUnauthorized = errors.New("viewer: unauthorized")
	// ErrRateLimited: too many failed logins recently.
	ErrRateLimited = errors.New("viewer: too many failed logins")
	// ErrStopping: the server is stopping; viewer admission is closed.
	ErrStopping = errors.New("viewer: server stopping")
	// ErrCapacity: a resource limit was reached.
	ErrCapacity = errors.New("viewer: capacity reached")
	// ErrSessionClosed: the session ended before the call.
	ErrSessionClosed = errors.New("viewer: session closed")

	// Causes delivered through Session.Context and Resource.Stop.
	ErrLoggedOut = errors.New("viewer: logged out")
	ErrExpired   = errors.New("viewer: session expired")
	ErrRevoked   = errors.New("viewer: sessions revoked by the owner")
	ErrReplaced  = errors.New("viewer: session replaced by a newer login")
)

// StopReason says why the server stops.
type StopReason string

const (
	StopNormal   StopReason = "stop"
	StopUpdate   StopReason = "update"
	StopForce    StopReason = "force"
	StopAbnormal StopReason = "abnormal"
)

// StopError is the cause of sessions ended by a server stop.
type StopError struct{ Reason StopReason }

func (e *StopError) Error() string { return "viewer: server stopping (" + string(e.Reason) + ")" }

// Kind classifies attached resources for limits.
type Kind string

const (
	KindStream Kind = "stream" // A long-lived live stream response.
	KindWindow Kind = "window" // A projection window.
	KindDetail Kind = "detail" // A concurrent full-record read.
)

// Limit bounds resources of one kind per session and in total.
type Limit struct{ PerSession, Total int }

// Clock abstracts time for deterministic tests.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a stoppable timer.
type Timer interface{ Stop() bool }

type realClock struct{}

func (realClock) Now() time.Time                            { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// Config holds limits and lifetimes. Zero values select the defaults the
// owner confirmed for #6.
type Config struct {
	CodeTTL         time.Duration // 5 min.
	IdleTTL         time.Duration // 30 min; renew extends.
	AbsoluteTTL     time.Duration // 12 h.
	MaxSessions     int           // 8; a new login replaces the least recently used.
	MaxPendingCodes int           // 4; the oldest is dropped.
	FailuresPerMin  int           // 5; failed logins beyond this get ErrRateLimited.
	Limits          map[Kind]Limit
	Clock           Clock
	Rand            io.Reader
	Logger          *slog.Logger
}

// DefaultLimits are sized to HTTP/1.1's ~6 connections per origin.
func DefaultLimits() map[Kind]Limit {
	return map[Kind]Limit{
		KindStream: {PerSession: 1, Total: 4},
		KindWindow: {PerSession: 2, Total: 16},
		KindDetail: {PerSession: 2, Total: 4},
	}
}

func (c Config) withDefaults() Config {
	if c.CodeTTL <= 0 {
		c.CodeTTL = 5 * time.Minute
	}
	if c.IdleTTL <= 0 {
		c.IdleTTL = 30 * time.Minute
	}
	if c.AbsoluteTTL <= 0 {
		c.AbsoluteTTL = 12 * time.Hour
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = 8
	}
	if c.MaxPendingCodes <= 0 {
		c.MaxPendingCodes = 4
	}
	if c.FailuresPerMin <= 0 {
		c.FailuresPerMin = 5
	}
	if c.Limits == nil {
		c.Limits = DefaultLimits()
	}
	if c.Clock == nil {
		c.Clock = realClock{}
	}
	if c.Rand == nil {
		c.Rand = rand.Reader
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

type digest [sha256.Size]byte

type pendingCode struct {
	hash    digest
	expires time.Time
}

// rememberedReplaced bounds how many replaced tokens still answer
// ErrReplaced instead of ErrUnauthorized.
const rememberedReplaced = 64

// Registry is safe for concurrent use.
type Registry struct {
	cfg Config

	mu        sync.Mutex
	codes     []pendingCode
	sessions  map[digest]*Session
	replaced  []digest
	nextID    uint64
	failures  []time.Time // Failed logins within the last minute.
	stopping  *StopError
	resources map[Kind]int // Totals across sessions.
}

// NewRegistry returns an empty registry.
func NewRegistry(cfg Config) *Registry {
	return &Registry{
		cfg:       cfg.withDefaults(),
		sessions:  make(map[digest]*Session),
		resources: make(map[Kind]int),
	}
}

// Issue creates a single-use pairing code for the owner's CLI.
func (r *Registry) Issue() (code string, expires time.Time, err error) {
	var raw [17]byte // 136 bits; the code keeps 130 of them.
	if _, err := io.ReadFull(r.cfg.Rand, raw[:]); err != nil {
		return "", time.Time{}, err
	}
	code = formatCode(raw[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping != nil {
		return "", time.Time{}, ErrStopping
	}
	now := r.cfg.Clock.Now()
	r.pruneCodes(now)
	if len(r.codes) >= r.cfg.MaxPendingCodes {
		r.codes = r.codes[1:]
	}
	expires = now.Add(r.cfg.CodeTTL)
	r.codes = append(r.codes, pendingCode{hash: hashCode(code), expires: expires})
	r.cfg.Logger.Info("viewer pairing code issued", "pending", len(r.codes))
	return code, expires, nil
}

// Login exchanges a code for a viewer token. A valid code always succeeds;
// only failures are throttled, so junk codes cannot lock the owner out. The
// 130-bit code and its short lifetime bound guessing.
func (r *Registry) Login(code string) (string, *Session, error) {
	var raw [32]byte
	if _, err := io.ReadFull(r.cfg.Rand, raw[:]); err != nil {
		return "", nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	want := hashCode(code)

	r.mu.Lock()
	if r.stopping != nil {
		r.mu.Unlock()
		return "", nil, ErrStopping
	}
	now := r.cfg.Clock.Now()
	r.pruneCodes(now)
	found := -1
	for i, c := range r.codes {
		if c.hash == want {
			found = i
		}
	}
	if found < 0 {
		limited := r.failed(now)
		r.mu.Unlock()
		if limited {
			r.cfg.Logger.Warn("viewer login throttled after repeated failures")
			return "", nil, ErrRateLimited
		}
		return "", nil, ErrUnauthorized
	}
	r.codes = append(r.codes[:found], r.codes[found+1:]...)
	var evicted *Session
	if len(r.sessions) >= r.cfg.MaxSessions {
		evicted = r.leastRecentlyUsed()
		r.detach(evicted, true)
		r.cfg.Logger.Info("viewer session replaced by a newer login", "session", evicted.id)
	}
	r.nextID++
	ctx, cancel := context.WithCancelCause(context.Background())
	s := &Session{
		registry: r, id: r.nextID, hash: digest(sha256.Sum256([]byte(token))),
		ctx: ctx, cancel: cancel, lastUsed: now, absolute: now.Add(r.cfg.AbsoluteTTL),
		resources: make(map[*attachment]bool), counts: make(map[Kind]int),
	}
	s.idle = now.Add(r.cfg.IdleTTL)
	r.sessions[s.hash] = s
	r.schedule(s)
	r.cfg.Logger.Info("viewer login", "session", s.id, "sessions", len(r.sessions))
	r.mu.Unlock()
	if evicted != nil {
		evicted.finish(ErrReplaced, now)
	}
	return token, s, nil
}

// Authorize returns the live session for token and marks it used. A token of
// a session replaced by a newer login reports ErrReplaced.
func (r *Registry) Authorize(token string) (*Session, error) {
	return r.withSession(token, nil)
}

// withSession authorizes token and runs fn on its session in the same
// critical section, so a concurrent stop, eviction or revocation reports its
// own error code. A session found expired is finished outside the lock.
func (r *Registry) withSession(token string, fn func(*Session) (finish error)) (*Session, error) {
	h := digest(sha256.Sum256([]byte(token)))
	r.mu.Lock()
	if r.stopping != nil {
		r.mu.Unlock()
		return nil, ErrStopping
	}
	s, ok := r.sessions[h]
	if !ok {
		replaced := false
		for _, old := range r.replaced {
			if old == h {
				replaced = true
			}
		}
		r.mu.Unlock()
		if replaced {
			return nil, ErrReplaced
		}
		return nil, ErrUnauthorized
	}
	now := r.cfg.Clock.Now()
	if !now.Before(s.idle) || !now.Before(s.absolute) {
		r.detach(s, false)
		r.mu.Unlock()
		s.finish(ErrExpired, now)
		return nil, ErrUnauthorized
	}
	s.lastUsed = now
	var cause error
	if fn != nil {
		cause = fn(s)
	}
	r.mu.Unlock()
	if cause != nil {
		s.finish(cause, now)
	}
	return s, nil
}

// Renew extends the idle expiry of the token's session, never beyond its
// absolute expiry.
func (r *Registry) Renew(token string) (*Session, error) {
	return r.withSession(token, func(s *Session) error {
		s.idle = r.cfg.Clock.Now().Add(r.cfg.IdleTTL)
		if s.idle.After(s.absolute) {
			s.idle = s.absolute
		}
		r.schedule(s)
		return nil
	})
}

// Logout ends the token's session.
func (r *Registry) Logout(token string) error {
	_, err := r.withSession(token, func(s *Session) error {
		r.detach(s, false)
		r.cfg.Logger.Info("viewer logout", "session", s.id)
		return ErrLoggedOut
	})
	return err
}

// RevokeAll ends every session and drops pending codes (owner action).
func (r *Registry) RevokeAll() {
	r.mu.Lock()
	r.codes, r.replaced = nil, nil
	ended := r.detachAll()
	r.cfg.Logger.Info("viewer sessions revoked", "sessions", len(ended))
	r.mu.Unlock()
	now := r.cfg.Clock.Now()
	for _, s := range ended {
		s.finish(ErrRevoked, now)
	}
}

// CloseAdmission rejects every later issue, login, renew and authorization
// with ErrStopping. The first reason wins.
func (r *Registry) CloseAdmission(reason StopReason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping == nil {
		r.stopping = &StopError{Reason: reason}
		r.codes = nil
		r.cfg.Logger.Info("viewer admission closed", "reason", string(reason))
	}
}

// StopAll ends every session with the stop cause recorded by CloseAdmission
// (StopAbnormal if none). Resources are asked to stop by now+grace: a stream
// may use the grace to write a final event. It never waits for handlers.
func (r *Registry) StopAll(grace time.Duration) {
	r.CloseAdmission(StopAbnormal)
	r.mu.Lock()
	cause := r.stopping
	ended := r.detachAll()
	r.mu.Unlock()
	deadline := r.cfg.Clock.Now().Add(max(grace, 0))
	for _, s := range ended {
		s.finish(cause, deadline)
	}
}

// Counts reports live sessions and pending codes for status output.
func (r *Registry) Counts() (sessions, pendingCodes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneCodes(r.cfg.Clock.Now())
	return len(r.sessions), len(r.codes)
}

func (r *Registry) pruneCodes(now time.Time) {
	kept := r.codes[:0]
	for _, c := range r.codes {
		if now.Before(c.expires) {
			kept = append(kept, c)
		}
	}
	r.codes = kept
}

// failed records a failed login and reports whether the failure rate is
// exceeded.
func (r *Registry) failed(now time.Time) bool {
	cut := now.Add(-time.Minute)
	kept := r.failures[:0]
	for _, t := range r.failures {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	r.failures = kept
	if len(r.failures) >= r.cfg.FailuresPerMin {
		return true
	}
	r.failures = append(r.failures, now)
	return false
}

func (r *Registry) leastRecentlyUsed() *Session {
	var lru *Session
	for _, s := range r.sessions {
		if lru == nil || s.lastUsed.Before(lru.lastUsed) || (s.lastUsed.Equal(lru.lastUsed) && s.id < lru.id) {
			lru = s
		}
	}
	return lru
}

// detach removes s from the registry; the caller finishes it outside the lock.
func (r *Registry) detach(s *Session, replaced bool) {
	delete(r.sessions, s.hash)
	if s.timer != nil {
		s.timer.Stop()
	}
	if replaced {
		r.replaced = append(r.replaced, s.hash)
		if len(r.replaced) > rememberedReplaced {
			r.replaced = r.replaced[1:]
		}
	}
}

func (r *Registry) detachAll() []*Session {
	ended := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		r.detach(s, false)
		ended = append(ended, s)
	}
	return ended
}

// schedule (re)arms the expiry timer so open requests end on time.
func (r *Registry) schedule(s *Session) {
	if s.timer != nil {
		s.timer.Stop()
	}
	at := s.idle
	if s.absolute.Before(at) {
		at = s.absolute
	}
	s.timer = r.cfg.Clock.AfterFunc(at.Sub(r.cfg.Clock.Now()), func() { r.expire(s) })
}

func (r *Registry) expire(s *Session) {
	r.mu.Lock()
	now := r.cfg.Clock.Now()
	if r.sessions[s.hash] != s || (now.Before(s.idle) && now.Before(s.absolute)) {
		r.mu.Unlock()
		return // Renewed, or already ended.
	}
	r.detach(s, false)
	r.cfg.Logger.Info("viewer session expired", "session", s.id)
	r.mu.Unlock()
	s.finish(ErrExpired, now)
}

var codeEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// formatCode renders 130 random bits as 26 Crockford base32 characters in
// groups of five.
func formatCode(raw []byte) string {
	s := codeEncoding.EncodeToString(raw)[:26]
	var b strings.Builder
	for i := 0; i < len(s); i += 5 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(s[i:min(i+5, len(s))])
	}
	return b.String()
}

// hashCode normalizes what a person may paste (case, separators, and the
// Crockford look-alikes O, I and L) before hashing.
func hashCode(code string) digest {
	var b strings.Builder
	for _, c := range strings.ToUpper(code) {
		if unicode.IsSpace(c) {
			continue
		}
		switch c {
		case '-', '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2212':
			continue
		case 'O':
			c = '0'
		case 'I', 'L':
			c = '1'
		}
		b.WriteRune(c)
	}
	return digest(sha256.Sum256([]byte("viewer-code:" + b.String())))
}

// String is for logs and tests; it names the session without its token.
func (s *Session) String() string { return fmt.Sprintf("viewer session %d", s.id) }
