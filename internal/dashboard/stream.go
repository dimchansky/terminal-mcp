package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"terminalmcp/internal/activity"
	"terminalmcp/internal/audit"
	"terminalmcp/internal/viewer"
)

// Stream limits. A stream is a notification channel: it carries compact
// references, never records; clients fetch complete items with catch-up.
const (
	streamHeartbeat   = 15 * time.Second
	streamCoalesce    = 250 * time.Millisecond
	streamEventLimit  = 64 << 10
	streamMaxAhead    = catchUpPages * maxRecords // Live rows beyond the client's newest cursor.
	streamRetry       = time.Second
	streamRetries     = 5
	streamReplaceWait = 2 * time.Second
)

// wakeBudget reads at most one record: the journal wait only needs to know
// that something follows the head. A large record is still scanned but not
// kept (deferred); Advance then reads it again.
var wakeBudget = audit.ReadBudget{MaxRecords: 1, MaxBytes: 1, MaxScanBytes: 64 << 10}

// Stream event names.
const (
	EventReady         = "ready"
	EventActivity      = "activity"
	EventHeartbeat     = "heartbeat"
	EventRebuilding    = "rebuilding"
	EventResync        = "resync"
	EventWindowState   = "window_state"
	EventWindowGone    = "window_gone"
	EventAuditClosed   = "audit_closed"
	EventError         = "error"
	EventServerStopped = "server_stopping"
)

// StreamItem is a compact reference to a changed item.
type StreamItem struct {
	Key                string   `json:"key"`
	Kind               string   `json:"kind"`
	Lane               string   `json:"lane"`
	Order              OrderKey `json:"order"`
	Tool               string   `json:"tool,omitempty"`
	State              string   `json:"state,omitempty"`        // Request state of an action, or the effect's state.
	EffectState        string   `json:"effect_state,omitempty"` // State of an action's own effect.
	Status             string   `json:"status,omitempty"`
	Time               string   `json:"time,omitempty"`
	PossiblyUnrecorded bool     `json:"possibly_unrecorded,omitempty"`
	Summary            *Hint    `json:"summary,omitempty"`
}

// StreamLane counts a lane's changed items; Lane is the full lane object the
// first time this stream reports the lane.
type StreamLane struct {
	ID      string `json:"id"`
	Changed int    `json:"changed"`
	Lane    *Lane  `json:"lane,omitempty"`
}

// ActivityEvent tells a client to catch up.
type ActivityEvent struct {
	Generation     uint64       `json:"generation"`
	Items          []StreamItem `json:"items"`
	Lanes          []StreamLane `json:"lanes"`
	More           bool         `json:"more"`
	Waiting        bool         `json:"waiting"`
	FloorChanged   bool         `json:"floor_changed"`
	PossibleUnseen bool         `json:"possible_unseen"`
	Truncated      bool         `json:"truncated"`
	Health         Health       `json:"health"`
	Index          *IndexInfo   `json:"index,omitempty"`
}

// StatusEvent is a heartbeat.
type StatusEvent struct {
	Generation     uint64     `json:"generation"`
	PossibleUnseen bool       `json:"possible_unseen"`
	Health         Health     `json:"health"`
	Index          *IndexInfo `json:"index,omitempty"`
}

// FinalEvent ends a stream (resync, window_state, window_gone, audit_closed,
// error); RetryAfter is set on rebuilding, which does not end it.
type FinalEvent struct {
	Generation uint64 `json:"generation"`
	State      State  `json:"state,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"` // Seconds.
}

// StreamSink writes one event; it bounds its own write time.
type StreamSink func(event string, data any) error

// Stream is a started stream of one live window.
type Stream struct {
	mg         *Manager
	m          *managed
	owner      viewer.Attacher
	id         string
	generation uint64
	rank       int64
	floor      activity.CoverageView
	reported   map[string]bool // Lanes sent as full objects.
	rebuild    bool            // A failed rebuild is pending: retry it, not Advance.
	failures   int             // Consecutive transient failures.
}

// streamHold is the stream currently attached to a window.
type streamHold struct {
	stop func()
	done chan struct{}
}

var errAuditClosed = errors.New("dashboard: the audit writer closed")

// OpenStream checks a stream request: a live window of the session, the
// client's generation and a resumable cursor. An unknown cursor rebuilds the
// window, like every request that finds one.
func (mg *Manager) OpenStream(ctx context.Context, owner viewer.Attacher, id string, generation uint64, after string) (*Stream, error) {
	var rank int64
	var floor activity.CoverageView
	err := mg.run(ctx, owner, id, func(m *managed) error {
		r, f, err := m.win.StreamStart(generation, after)
		if errors.Is(err, errNotLive) {
			return errBadRequest
		}
		rank, floor = r, f
		return err
	})
	if err != nil {
		return nil, err
	}
	m, err := mg.get(owner, id)
	if err != nil {
		return nil, err
	}
	return &Stream{mg: mg, m: m, owner: owner, id: id, generation: generation, rank: rank, floor: floor,
		reported: map[string]bool{}}, nil
}

// ClaimStream makes the caller the window's only stream: a previous stream
// of the window is stopped (stop must not block) and awaited for up to 2 s.
// If ctx ends while waiting, the previous stream stays stopped (its client
// reconnected or the session is ending). The returned done must be called
// exactly once, when the new stream has
// fully ended (after its viewer attachment was released).
func (mg *Manager) ClaimStream(ctx context.Context, owner viewer.Attacher, id string, stop func()) (func(), error) {
	m, err := mg.get(owner, id)
	if err != nil {
		return nil, err
	}
	hold := &streamHold{stop: stop, done: make(chan struct{})}
	deadline := time.After(streamReplaceWait)
	for {
		m.mu.Lock()
		old := m.stream
		if old == nil {
			m.stream = hold
			m.mu.Unlock()
			return func() {
				m.mu.Lock()
				if m.stream == hold {
					m.stream = nil
				}
				m.mu.Unlock()
				close(hold.done)
			}, nil
		}
		m.mu.Unlock()
		old.stop()
		select {
		case <-old.done:
		case <-deadline:
			return nil, viewer.ErrCapacity
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
}

// Run streams events until a final event, the end of ctx (the request or
// the session) or a failed write. It returns nil after a final event.
func (s *Stream) Run(ctx context.Context, send StreamSink) error {
	if err := send(EventReady, FinalEvent{Generation: s.generation}); err != nil {
		return err
	}
	heartbeat := time.Now().Add(s.mg.streamHeartbeat())
	var lastActivity time.Time
	lastWaiting := false
	for {
		if s.m.isClosed() {
			return s.final(send, EventWindowGone, FinalEvent{Generation: s.generation})
		}
		if err := sleep(ctx, time.Until(lastActivity.Add(s.mg.streamCoalesce()))); err != nil {
			return err
		}
		var res AdvanceResult
		var err error
		if s.rebuild {
			// Drive the pending rebuild; Advance would only see the
			// generation a failed rebuild already moved. Once another
			// request rebuilt it, take the new generation.
			gen, state, _, _ := s.m.win.Info()
			if state != StateOK { // Terminal meanwhile: never revive it.
				_, final := s.failure(send, &StateError{State: state})
				return final
			}
			if !s.mg.rebuildPending(s.m) {
				return s.final(send, EventResync, FinalEvent{Generation: gen})
			}
			err = s.mg.rebuild(ctx, s.m, gen)
		} else {
			err = s.mg.run(ctx, s.owner, s.id, func(m *managed) error {
				var err error
				res, err = m.win.Advance(ctx, s.generation, s.rank, catchUpPages, time.Now().Add(requestTime), streamMaxAhead)
				return err
			})
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err != nil {
			retry, final := s.failure(send, err)
			if final != nil || retry < 0 {
				return final
			}
			if err := sleepOrChange(ctx, retry, s.m.win.Changed()); err != nil {
				return err
			}
			continue
		}
		s.failures = 0
		changed := len(res.Changes.Actions)+len(res.Changes.Effects)+len(res.Changes.Occurrences) > 0
		floorChanged := res.Coverage.FloorSet != s.floor.FloorSet || res.Coverage.Floor != s.floor.Floor
		if changed || floorChanged || res.Waiting != lastWaiting {
			ev, ok := s.activity(res, floorChanged)
			if !ok { // Rebuilt between the advance and the view.
				gen, _, _, _ := s.m.win.Info()
				return s.final(send, EventResync, FinalEvent{Generation: gen})
			}
			if err := send(EventActivity, ev); err != nil {
				return err
			}
			lastActivity, lastWaiting, s.floor = time.Now(), res.Waiting, res.Coverage
		}
		s.rank = res.Rank
		if res.More && !res.Waiting {
			continue
		}
		due, err := s.wait(ctx, res, heartbeat)
		switch {
		case errors.Is(err, errAuditClosed):
			return s.final(send, EventAuditClosed, FinalEvent{Generation: s.generation})
		case ctx.Err() != nil:
			return context.Cause(ctx)
		case err != nil: // A read error: counted like a failed advance.
			retry, final := s.failure(send, err)
			if final != nil || retry < 0 {
				return final
			}
			if err := sleepOrChange(ctx, retry, s.m.win.Changed()); err != nil {
				return err
			}
		}
		if due || !time.Now().Before(heartbeat) {
			if err := send(EventHeartbeat, s.status()); err != nil {
				return err
			}
			heartbeat = time.Now().Add(s.mg.streamHeartbeat())
		}
	}
}

// failure handles an error: it returns a retry delay, or a final result
// (nil after a final event) with a negative delay.
func (s *Stream) failure(send StreamSink, err error) (time.Duration, error) {
	var rebuilding *RebuildingError
	var state *StateError
	switch {
	case errors.Is(err, ErrWindowGone):
		return -1, s.final(send, EventWindowGone, FinalEvent{Generation: s.generation})
	case errors.Is(err, ErrGenerationChanged) && s.mg.rebuildPending(s.m):
		// Another request's rebuild failed: wait it out here.
		s.rebuild = true
		return s.rebuilding(send, time.Second)
	case errors.Is(err, ErrGenerationChanged), errors.Is(err, ErrRebuild):
		gen, _, _, _ := s.m.win.Info()
		return -1, s.final(send, EventResync, FinalEvent{Generation: gen})
	case errors.As(err, &rebuilding):
		s.rebuild = true
		return s.rebuilding(send, rebuilding.RetryAfter)
	case errors.Is(err, ErrDisabled), errors.As(err, &state) && state.State == StateDisabled:
		return -1, s.final(send, EventAuditClosed, FinalEvent{Generation: s.generation})
	case errors.As(err, &state):
		return -1, s.final(send, EventWindowState, FinalEvent{Generation: s.generation, State: state.State})
	case errors.Is(err, ErrCapacity):
		return -1, s.final(send, EventWindowState, FinalEvent{Generation: s.generation, State: StateCapacity})
	}
	s.failures++
	if s.failures >= streamRetries {
		return -1, s.final(send, EventError, FinalEvent{Generation: s.generation, Retryable: true})
	}
	return streamRetry, nil
}

func (s *Stream) rebuilding(send StreamSink, retry time.Duration) (time.Duration, error) {
	retry = max(retry, time.Second)
	if err := send(EventRebuilding, FinalEvent{Generation: s.generation, RetryAfter: int(retry.Round(time.Second) / time.Second)}); err != nil {
		return -1, err
	}
	return retry, nil
}

func (s *Stream) final(send StreamSink, event string, data FinalEvent) error {
	if err := send(event, data); err != nil {
		return err
	}
	return nil
}

// wait blocks until new journal data follows the head (unless the stream
// waits for the client or more data is already known), the window changes,
// the heartbeat is due or ctx ends. due reports the heartbeat.
func (s *Stream) wait(ctx context.Context, res AdvanceResult, heartbeat time.Time) (due bool, err error) {
	wctx, cancel := context.WithDeadline(ctx, heartbeat)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-res.Changed:
			cancel()
		case <-stop:
		}
	}()
	if res.Waiting || res.More {
		<-wctx.Done()
	} else {
		page, err := s.mg.reader.WaitNextPage(wctx, res.Head, wakeBudget)
		switch {
		case err == nil && page.Closed && len(page.Records) == 0:
			return false, errAuditClosed // Rows before the close were notified first.
		case err != nil && wctx.Err() == nil && !errors.Is(err, audit.ErrHistoryScanLimit):
			if errors.Is(err, audit.ErrHistoryDisabled) {
				return false, errAuditClosed
			}
			return false, err
		}
	}
	return !time.Now().Before(heartbeat), nil
}

// activity builds the compact event for the changes of one advance; ok is
// false when the window was rebuilt meanwhile.
func (s *Stream) activity(res AdvanceResult, floorChanged bool) (ActivityEvent, bool) {
	snap := s.mg.collect(s.m)
	if snap.generation != s.generation {
		return ActivityEvent{}, false
	}
	keys := map[string]bool{}
	for _, k := range res.Changes.Actions {
		keys[actionKey(k)] = true
	}
	for _, k := range res.Changes.Effects {
		keys[effectKey(k)] = true
	}
	for _, k := range res.Changes.Occurrences {
		keys["d:"+occurrenceID(k)] = true
		keys["s:"+occurrenceID(k)] = true
	}
	ev := ActivityEvent{Generation: snap.generation, Items: []StreamItem{}, Lanes: []StreamLane{}, More: res.More,
		Waiting: res.Waiting, FloorChanged: floorChanged, PossibleUnseen: snap.unseen, Health: s.mg.health()}
	ev.Index, _ = s.mg.liveStatus(s.m)
	counts := map[string]int{}
	var order []string
	for _, it := range snap.items {
		if !keys[it.Key] && (it.Effect == nil || !keys[effectKey(it.Effect.Key)]) {
			continue
		}
		ev.Items = append(ev.Items, compact(it))
		if counts[it.Lane] == 0 {
			order = append(order, it.Lane)
		}
		counts[it.Lane]++
	}
	var lanes map[string]Lane
	for _, id := range order {
		l := StreamLane{ID: id, Changed: counts[id]}
		if !s.reported[id] {
			if lanes == nil {
				lanes = map[string]Lane{}
				for _, full := range lanesOf(snap.items) {
					lanes[full.ID] = full
				}
			}
			full := lanes[id]
			l.Lane = &full
		}
		ev.Lanes = append(ev.Lanes, l)
	}
	ev = fit(ev, streamEventLimit)
	for _, l := range ev.Lanes {
		if l.Lane != nil {
			s.reported[l.ID] = true // Sent in full only in an untruncated event.
		}
	}
	return ev, true
}

// fit cuts an event to the size limit: first the full lane objects, then
// items, marking it truncated.
func fit(ev ActivityEvent, limit int) ActivityEvent {
	size := func() int { b, _ := json.Marshal(ev); return len(b) }
	if size() <= limit {
		return ev
	}
	ev.Truncated = true
	lanes := make([]StreamLane, len(ev.Lanes))
	for i, l := range ev.Lanes {
		lanes[i] = StreamLane{ID: l.ID, Changed: l.Changed}
	}
	ev.Lanes = lanes
	for size() > limit && len(ev.Items) > 0 {
		ev.Items = ev.Items[:len(ev.Items)/2]
	}
	for size() > limit && len(ev.Lanes) > 0 {
		ev.Lanes = ev.Lanes[:len(ev.Lanes)/2]
	}
	return ev
}

func compact(it item) StreamItem {
	c := StreamItem{Key: it.Key, Kind: it.Kind, Lane: it.Lane, Order: it.Order, Summary: it.Summary}
	if it.Action != nil {
		a := it.Action
		c.Tool, c.State, c.Status, c.PossiblyUnrecorded = a.Tool, a.State.String(), a.Status, a.PossiblyUnrecorded
		c.Time = itemTime(it.Item)
		if it.Effect != nil {
			c.EffectState = it.Effect.State.String()
		}
		return c
	}
	if it.Effect != nil {
		e := it.Effect
		c.Tool, c.State, c.Status, c.PossiblyUnrecorded = e.Tool, e.State.String(), e.Status, e.PossiblyUnrecorded
		c.Time = itemTime(it.Item)
	}
	return c
}

func (s *Stream) status() StatusEvent {
	gen, _, _, unseen := s.m.win.Info()
	ev := StatusEvent{Generation: gen, PossibleUnseen: unseen, Health: s.mg.health()}
	ev.Index, _ = s.mg.liveStatus(s.m)
	return ev
}

// sleepOrChange sleeps like sleep but also ends when the window changes
// (for example when it is closed during a backoff).
func sleepOrChange(ctx context.Context, d time.Duration, changed <-chan struct{}) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
	case <-changed:
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}

func (mg *Manager) streamHeartbeat() time.Duration { return cmpOr(mg.heartbeat, streamHeartbeat) }
func (mg *Manager) streamCoalesce() time.Duration  { return cmpOr(mg.coalesce, streamCoalesce) }

func cmpOr(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}
