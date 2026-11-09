package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"terminalmcp/internal/activity"
	"terminalmcp/internal/viewer"
	"terminalmcp/internal/viewer/viewertest"
)

type event struct {
	name string
	data any
}

// streamFixture runs a stream of a fresh live window with short timings.
type streamFixture struct {
	t      *testing.T
	j      *journal
	mg     *Manager
	s      *viewertest.Session
	id     string
	env    Envelope
	events chan event
	done   chan error
	cancel context.CancelFunc
}

func startStream(t *testing.T, j *journal, cfg ManagerConfig) *streamFixture {
	t.Helper()
	return startStreamWith(t, j, cfg, 10*time.Millisecond)
}

func startStreamWith(t *testing.T, j *journal, cfg ManagerConfig, coalesce time.Duration) *streamFixture {
	t.Helper()
	cfg.ServerID = testServer
	mg := NewManager(j.reader, cfg)
	mg.heartbeat, mg.coalesce = 300*time.Millisecond, coalesce
	f := &streamFixture{t: t, j: j, mg: mg, s: viewertest.NewSession(nil), events: make(chan event, 1000), done: make(chan error, 1)}
	var err error
	if f.id, err = mg.Open(context.Background(), f.s, ModeLive, ""); err != nil {
		t.Fatal(err)
	}
	if f.env, err = mg.Snapshot(context.Background(), f.s, f.id); err != nil {
		t.Fatal(err)
	}
	f.run(f.env.Generation, f.env.LiveCursor)
	return f
}

func (f *streamFixture) run(generation uint64, after string) {
	f.t.Helper()
	st, err := f.mg.OpenStream(context.Background(), f.s, f.id, generation, after)
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.t.Cleanup(cancel)
	go func() {
		f.done <- st.Run(ctx, func(name string, data any) error {
			f.events <- event{name, data}
			return nil
		})
	}()
	f.next(EventReady)
}

// next waits for the next event with the given name, skipping heartbeats.
func (f *streamFixture) next(name string) event {
	f.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-f.events:
			if ev.name == name {
				return ev
			}
			// A rebuild still running elsewhere is reported as rebuilding.
			if ev.name != EventHeartbeat && ev.name != EventActivity && ev.name != EventRebuilding {
				f.t.Fatalf("got %s %+v while waiting for %s", ev.name, ev.data, name)
			}
		case <-timeout:
			f.t.Fatalf("no %s event", name)
		}
	}
}

// activityKeys collects activity item keys until want are all seen.
func (f *streamFixture) activityKeys(want []string) {
	f.t.Helper()
	seen := map[string]bool{}
	missing := func() int {
		n := 0
		for _, k := range want {
			if !seen[k] {
				n++
			}
		}
		return n
	}
	for missing() > 0 {
		ev := f.next(EventActivity).data.(ActivityEvent)
		for _, it := range ev.Items {
			seen[it.Key] = true
		}
	}
}

func keysOf(ids []string) []string {
	var keys []string
	for _, id := range ids {
		keys = append(keys, actionKey(requestKey(id)))
	}
	return keys
}

func TestStreamNotifiesAndCatchUpDelivers(t *testing.T) {
	j := openJournal(t)
	j.requests(3)
	f := startStream(t, j, ManagerConfig{})
	ids := j.requests(5)
	f.activityKeys(keysOf(ids))
	res, err := f.mg.CatchUp(context.Background(), f.s, f.id, f.env.Generation, f.env.LiveCursor, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, it := range res.Items {
		got[it.Key] = true
	}
	for _, k := range keysOf(ids) {
		if !got[k] {
			t.Fatalf("catch-up misses %s", k)
		}
	}
	// A heartbeat follows when nothing happens.
	f.next(EventHeartbeat)
}

// TestSnapshotCursorStaysResumableWhileStreaming: the stream advances the
// window without issuing; a new snapshot hands out an issued cursor.
func TestSnapshotCursorStaysResumableWhileStreaming(t *testing.T) {
	j := openJournal(t)
	f := startStream(t, j, ManagerConfig{})
	ids := j.requests(4)
	f.activityKeys(keysOf(ids))
	again, err := f.mg.Snapshot(context.Background(), f.s, f.id)
	if err != nil {
		t.Fatal(err)
	}
	if again.Generation != f.env.Generation || again.LiveCursor != f.env.LiveCursor {
		t.Fatalf("snapshot gen %d cursor changed %v", again.Generation, again.LiveCursor != f.env.LiveCursor)
	}
	res, err := f.mg.CatchUp(context.Background(), f.s, f.id, again.Generation, again.LiveCursor, nil)
	if err != nil || len(res.Items) != 4 {
		t.Fatalf("catch-up %d items: %v", len(res.Items), err)
	}
}

// TestStreamBackpressureKeepsTheWindowAlive: a client that never catches up
// cannot drive the window into the terminal capacity state.
func TestStreamBackpressureKeepsTheWindowAlive(t *testing.T) {
	j := openJournal(t)
	f := startStream(t, j, ManagerConfig{Capacity: 15000})
	j.requests(10000) // 20,000 rows: without backpressure the stream would exceed the capacity.
	for {
		ev := f.next(EventActivity).data.(ActivityEvent)
		if ev.Waiting {
			break
		}
	}
	if _, state, _, _ := f.mg.windowsInfo(f.id); state != StateOK {
		t.Fatalf("window state %s", state)
	}
	// The client catches up; the stream continues to the end.
	head := f.env.LiveCursor
	for i := 0; i < 100; i++ {
		res, err := f.mg.CatchUp(context.Background(), f.s, f.id, f.env.Generation, head, nil)
		if err != nil {
			t.Fatal(err)
		}
		for next := res.Next; next != nil; {
			more, err := f.mg.CatchUp(context.Background(), f.s, f.id, f.env.Generation, head, next)
			if err != nil {
				t.Fatal(err)
			}
			next = more.Next
		}
		head = res.Head
		if !res.More {
			break
		}
	}
	if _, state, _, _ := f.mg.windowsInfo(f.id); state != StateOK {
		t.Fatalf("window state after catch-up %s", state)
	}
	// Released from backpressure, the stream goes on notifying.
	for {
		if ev := f.next(EventActivity).data.(ActivityEvent); !ev.Waiting {
			break
		}
	}
}

func (mg *Manager) windowsInfo(id string) (uint64, State, string, bool) {
	mg.mu.Lock()
	m := mg.windows[id]
	mg.mu.Unlock()
	return m.win.Info()
}

func TestStreamEndsWithResyncAfterARebuild(t *testing.T) {
	j := openJournal(t)
	f := startStream(t, j, ManagerConfig{})
	if err := f.mg.run(context.Background(), f.s, f.id, func(*managed) error { return ErrRebuild }); !errors.Is(err, ErrRebuild) {
		t.Fatal(err)
	}
	j.requests(1)
	ev := f.next(EventResync).data.(FinalEvent)
	if ev.Generation != f.env.Generation+1 {
		t.Fatalf("resync generation %d", ev.Generation)
	}
	if err := <-f.done; err != nil {
		t.Fatal(err)
	}
}

func TestStreamEndsWhenTheWindowCloses(t *testing.T) {
	j := openJournal(t)
	f := startStream(t, j, ManagerConfig{})
	start := time.Now()
	if err := f.mg.Close(f.s, f.id); err != nil {
		t.Fatal(err)
	}
	f.next(EventWindowGone)
	if time.Since(start) > 2*time.Second {
		t.Fatal("window_gone was late")
	}
}

func TestStreamEndsWithTheSession(t *testing.T) {
	j := openJournal(t)
	f := startStream(t, j, ManagerConfig{})
	f.cancel()
	select {
	case err := <-f.done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end")
	}
}

func TestStreamReportsAClosedWriter(t *testing.T) {
	j := openJournal(t)
	f := startStream(t, j, ManagerConfig{})
	_ = j.log.Close()
	f.next(EventAuditClosed)
}

func TestStreamStartChecks(t *testing.T) {
	j := openJournal(t)
	j.requests(1)
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	env, _ := mg.Snapshot(context.Background(), s, id)
	for name, c := range map[string]struct {
		gen   uint64
		after string
		want  error
	}{
		"other generation": {env.Generation + 5, env.LiveCursor, ErrGenerationChanged},
		"unknown cursor":   {env.Generation, "nope", ErrRebuild},
	} {
		if _, err := mg.OpenStream(context.Background(), s, id, c.gen, c.after); !errors.Is(err, c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := mg.OpenStream(context.Background(), viewertest.NewSession(nil), id, 1, env.LiveCursor); !errors.Is(err, ErrWindowGone) {
		t.Errorf("other session: %v", err)
	}
}

func TestClaimStreamReplacesThePreviousStream(t *testing.T) {
	j := openJournal(t)
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	var firstDone func()
	stopped := make(chan struct{})
	firstDone, err := mg.ClaimStream(context.Background(), s, id, func() { close(stopped); go firstDone() })
	if err != nil {
		t.Fatal(err)
	}
	second, err := mg.ClaimStream(context.Background(), s, id, func() {})
	if err != nil {
		t.Fatal(err)
	}
	<-stopped
	second()
	// A stream that does not end in time leaves the slot busy.
	_, err = mg.ClaimStream(context.Background(), s, id, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mg.ClaimStream(context.Background(), s, id, func() {}); !errors.Is(err, viewer.ErrCapacity) {
		t.Fatalf("stuck stream: %v", err)
	}
}

func TestActivityEventsFitTheLimit(t *testing.T) {
	ev := ActivityEvent{Items: []StreamItem{}, Lanes: []StreamLane{}}
	long := strings.Repeat("c", 8<<10)
	for i := 0; i < 40; i++ {
		ev.Items = append(ev.Items, StreamItem{Key: "a:" + itoa(i), Summary: &Hint{Text: long}})
		ev.Lanes = append(ev.Lanes, StreamLane{ID: itoa(i), Changed: 1, Lane: &Lane{ConversationID: &long}})
	}
	got := fit(ev, streamEventLimit)
	data, _ := json.Marshal(got)
	if len(data) > streamEventLimit || !got.Truncated || len(got.Items) == 0 {
		t.Fatalf("%d bytes truncated %v items %d", len(data), got.Truncated, len(got.Items))
	}
	for _, l := range got.Lanes {
		if l.Lane != nil {
			t.Fatal("a truncated event keeps a full lane object")
		}
	}
}

func requestKey(id string) activity.RequestKey {
	return activity.RequestKey{ServerID: testServer, RequestID: id}
}

// TestStreamWaitsOutAFailedRebuild: a failing rebuild is reported as
// rebuilding and retried inside the stream until it succeeds.
func TestStreamWaitsOutAFailedRebuild(t *testing.T) {
	j := openJournal(t)
	f := startStream(t, j, ManagerConfig{})
	m, _ := f.mg.get(f.s, f.id)
	setBuildErr := func(err error) {
		m.win.With(func(*activity.Projection) { m.win.buildErr = func() error { return err } })
	}
	setBuildErr(errors.New("disk"))
	var rebuilding *RebuildingError
	if err := f.mg.run(context.Background(), f.s, f.id, func(*managed) error { return ErrRebuild }); !errors.As(err, &rebuilding) {
		t.Fatalf("failed rebuild: %v", err)
	}
	ev := f.next(EventRebuilding).data.(FinalEvent)
	if ev.RetryAfter < 1 {
		t.Fatalf("retry after %d", ev.RetryAfter)
	}
	setBuildErr(nil)
	var resync FinalEvent
	for resync.Generation == 0 {
		select {
		case ev := <-f.events:
			switch ev.name {
			case EventResync:
				resync = ev.data.(FinalEvent)
			case EventRebuilding, EventHeartbeat, EventActivity:
			default:
				t.Fatalf("got %s", ev.name)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no resync")
		}
	}
	if gen, state, _, _ := m.win.Info(); resync.Generation != gen || state != StateOK {
		t.Fatalf("resync %d, window %d %s", resync.Generation, gen, state)
	}
}

func TestStreamCoalescesBursts(t *testing.T) {
	j := openJournal(t)
	f := startStreamWith(t, j, ManagerConfig{}, 300*time.Millisecond)
	ids := j.requests(60)
	events := 0
	seen := map[string]bool{}
	for len(seen) < len(ids) {
		ev := f.next(EventActivity).data.(ActivityEvent)
		events++
		for _, it := range ev.Items {
			seen[it.Key] = true
		}
	}
	if events > 10 {
		t.Fatalf("%d events for one burst", events)
	}
}

func TestStreamSendsFullLanesOnce(t *testing.T) {
	j := openJournal(t)
	f := startStream(t, j, ManagerConfig{})
	chatStart(j, "c1", "chat-x")
	first := f.next(EventActivity).data.(ActivityEvent)
	if len(first.Lanes) != 1 || first.Lanes[0].Lane == nil || *first.Lanes[0].Lane.ConversationID != "chat-x" {
		t.Fatalf("first lanes %+v", first.Lanes)
	}
	chatFinish(j, "c1")
	second := f.next(EventActivity).data.(ActivityEvent)
	if len(second.Lanes) != 1 || second.Lanes[0].Lane != nil || second.Lanes[0].ID != first.Lanes[0].ID {
		t.Fatalf("second lanes %+v", second.Lanes)
	}
}

// TestStreamNotifiesRowsBeforeAWriterClose: the last rows are reported
// before audit_closed.
func TestStreamNotifiesRowsBeforeAWriterClose(t *testing.T) {
	j := openJournal(t)
	f := startStream(t, j, ManagerConfig{})
	id := j.request(`{"path":"last.txt"}`)
	_ = j.log.Close()
	seen := false
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-f.events:
			if a, ok := ev.data.(ActivityEvent); ok {
				for _, it := range a.Items {
					seen = seen || it.Key == actionKey(requestKey(id))
				}
			}
			if ev.name == EventAuditClosed {
				if !seen {
					t.Fatal("audit_closed before the last rows were notified")
				}
				return
			}
		case <-timeout:
			t.Fatal("no audit_closed")
		}
	}
}

func TestStreamHeartbeatReportsStatus(t *testing.T) {
	j := openJournal(t)
	f := startStream(t, j, ManagerConfig{})
	hb := f.next(EventHeartbeat).data.(StatusEvent)
	if hb.Generation != f.env.Generation || hb.Health.State != "ok" || hb.PossibleUnseen {
		t.Fatalf("heartbeat %+v", hb)
	}
}
