// Package dashboard connects the read-only audit history reader to the
// activity projection for the owner dashboard: it builds projection windows,
// keeps them joined to the live journal, and serves bounded history.
package dashboard

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"terminalmcp/internal/activity"
	"terminalmcp/internal/audit"
)

// pageBudget reads projection pages so that only an exceptional
// near-4-MiB row is deferred; deferred rows are hydrated before applying.
var pageBudget = audit.ReadBudget{MaxRecords: 200, MaxBytes: 8 << 20, MaxScanBytes: 8 << 20}

// State is a window's lifecycle state.
type State string

const (
	StateOK       State = "ok"
	StateDisabled State = "disabled" // No file-backed audit.
	StateCapacity State = "capacity" // Live rows could not be applied within capacity.
	StateFailed   State = "failed"   // Rebuilds kept failing.
)

// errNotLive: archive windows have no live position (a bad request).
var errNotLive = errors.New("dashboard: archive windows have no live position")

var (
	// ErrUseArchive: the window's history chain is closed by eviction;
	// older history is read by an archive window from the floor.
	ErrUseArchive = errors.New("dashboard: history chain closed; open an archive window")
	// ErrBeyondCoverage: the history chain stopped (start of visible
	// history, gap or changed source); there is nothing older to scan here.
	ErrBeyondCoverage = errors.New("dashboard: no further history in this window")
	// ErrRebuild: the window's live chain or delivery contract broke; the
	// window must be rebuilt as a new generation.
	ErrRebuild = errors.New("dashboard: window must be rebuilt")
	// ErrCapacity: live rows cannot be applied within the window capacity.
	ErrCapacity = errors.New("dashboard: window capacity exhausted")
	// ErrDisabled: the server has no file-backed audit.
	ErrDisabled = errors.New("dashboard: audit history is disabled")
)

// issued is the window state captured with each live cursor it hands out.
type issued struct {
	rank  int64 // Last live rank applied when the cursor was issued.
	carry activity.LiveCarry
	floor activity.CoverageView // Coverage when issued, to report floor changes.
}

// Window is one projection over the journal. Its methods are safe for
// concurrent use; operations serialize on the window lock.
type Window struct {
	reader   *audit.Reader
	serverID string
	capacity int
	archive  bool
	from     audit.Cursor // Archive windows: where history starts.

	// Test hooks; nil in production.
	afterFrontier func()
	afterHistory  func()
	buildErr      func() error

	mu         sync.Mutex
	proj       *activity.Projection
	hist       activity.HistoryCarry
	histRank   int64
	histNext   audit.Cursor
	histClosed bool
	liveRank   int64
	head       audit.Cursor
	carry      activity.LiveCarry
	issued     map[string]issued
	generation uint64
	state      State
	unseen     bool // possible_unseen_activity after a rebuild.
	// version changes whenever the projection may have changed; synced is
	// the key of the last index sync.
	version uint64
	synced  ViewKey
	hasSync bool
	// changed is closed and replaced whenever the window may have changed
	// (a version bump, an issued cursor, a close), to wake streams.
	changed chan struct{}
	// track collects what history scans change while a caller tracks them.
	track *ChangeSet
	// newest is the most recently issued live cursor and its live rank.
	newest      string
	newestRank  int64
	onApply     func(activity.OccurrenceKey, []byte, activity.Change)
	beforeRaise func(*activity.Projection)
}

// WindowConfig configures a window.
type WindowConfig struct {
	ServerID string
	Capacity int // Occurrences; 0 selects 50,000.
	// OnApply sees every newly applied decoded record (for summary hints).
	OnApply func(activity.OccurrenceKey, []byte, activity.Change)
	// BeforeRaise runs before every floor raise (index sync).
	BeforeRaise func(*activity.Projection)
}

func newWindow(reader *audit.Reader, cfg WindowConfig) *Window {
	if cfg.Capacity <= 0 {
		cfg.Capacity = 50_000
	}
	return &Window{reader: reader, serverID: cfg.ServerID, capacity: cfg.Capacity,
		onApply: cfg.OnApply, beforeRaise: cfg.BeforeRaise, state: StateOK, changed: make(chan struct{})}
}

// NewLiveWindow builds a window joined to the live journal. With
// ErrDisabled it still returns the (disabled) window, so callers can report
// that state.
func NewLiveWindow(ctx context.Context, reader *audit.Reader, cfg WindowConfig) (*Window, error) {
	w := newWindow(reader, cfg)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w, w.build(ctx)
}

// NewArchiveWindow builds a history-only window starting at from ("" reads
// from the newest record).
func NewArchiveWindow(ctx context.Context, reader *audit.Reader, from audit.Cursor, cfg WindowConfig) (*Window, error) {
	w := newWindow(reader, cfg)
	w.archive, w.from = true, from
	w.mu.Lock()
	defer w.mu.Unlock()
	return w, w.build(ctx)
}

func (w *Window) reset() {
	w.proj = activity.NewProjection(w.serverID)
	w.proj.SetCapacity(w.capacity)
	w.hist = activity.HistoryCarry{}
	w.histRank, w.liveRank = 0, 0
	w.histNext, w.histClosed, w.head = "", false, ""
	w.issued = make(map[string]issued)
	w.newest, w.newestRank = "", 0
}

// bump marks a possible change of the projection.
func (w *Window) bump() {
	w.version++
	w.signal()
}

func (w *Window) signal() {
	close(w.changed)
	w.changed = make(chan struct{})
}

// Changed returns a channel closed on the window's next change.
func (w *Window) Changed() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.changed
}

// Notify wakes the window's streams (for example when it is closed).
func (w *Window) Notify() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.signal()
}

func (w *Window) build(ctx context.Context) error {
	w.bump()
	w.reset()
	if w.buildErr != nil {
		if err := w.buildErr(); err != nil {
			return err
		}
	}
	if w.reader == nil {
		w.state = StateDisabled
		return ErrDisabled
	}
	if w.archive {
		if err := w.proj.HistoryOnly(); err != nil {
			return err
		}
		if err := w.historyPage(ctx, w.from); err != nil && !errors.Is(err, ErrBeyondCoverage) {
			return err
		}
		return nil
	}
	w.state = StateOK
	f, err := w.reader.FrontierCursor(ctx)
	if errors.Is(err, audit.ErrHistoryDisabled) {
		w.state = StateDisabled
		return ErrDisabled
	}
	if err != nil {
		return err
	}
	if w.afterFrontier != nil {
		w.afterFrontier()
	}
	if err := w.historyPage(ctx, ""); err != nil && !errors.Is(err, ErrBeyondCoverage) {
		return err
	}
	if c := w.proj.Coverage(); c.End == activity.CoverageGap || c.End == activity.CoverageChanged {
		// The newest page itself was unreadable: try again as a rebuild.
		return fmt.Errorf("%w: newest history page unavailable", ErrRebuild)
	}
	if w.afterHistory != nil {
		w.afterHistory()
	}
	if err := w.proj.StartJoin(); err != nil {
		return err
	}
	w.head = f
	first := true
	for {
		page, err := w.reader.ReadPage(ctx, w.head, audit.Forward, pageBudget)
		if err != nil {
			return liveError(err)
		}
		if first {
			w.carry = activity.NewLiveCarry(w.proj.LiveSeed(len(page.Records) == 0))
			first = false
		}
		if err := w.applyLive(ctx, page); err != nil {
			return err
		}
		if !page.HasMore {
			break
		}
	}
	if err := w.proj.LiveCaughtUp(); err != nil {
		return err
	}
	w.issue()
	return nil
}

// issue records the live head as a resumable cursor with its rank and carry.
func (w *Window) issue() string {
	c := string(w.head)
	w.proj.IssueLiveCursor(c)
	w.issued[c] = issued{rank: w.liveRank, carry: w.carry, floor: w.proj.Coverage()}
	w.newest, w.newestRank = c, w.liveRank
	w.signal()
	return c
}

func (w *Window) nextHist() int64 { w.histRank--; return w.histRank }

// historyPage reads and applies one backward page from `from`.
func (w *Window) historyPage(ctx context.Context, from audit.Cursor) error {
	w.bump()
	if w.histClosed {
		return ErrUseArchive
	}
	if c := w.proj.Coverage(); c.End != activity.CoverageOpen {
		if c.FloorSet {
			return ErrUseArchive
		}
		return ErrBeyondCoverage
	}
	if w.proj.Occurrences()+pageBudget.MaxRecords > w.capacity {
		return w.closeHistoryForCapacity()
	}
	page, err := w.reader.ReadPage(ctx, from, audit.Backward, pageBudget)
	if err != nil {
		return w.historyError(err)
	}
	records, err := w.hydrate(ctx, page)
	if err != nil {
		return w.historyError(err)
	}
	if err := w.proj.BeginHistoryPage(string(from)); err != nil {
		return fmt.Errorf("%w: %v", ErrRebuild, err)
	}
	w.hist.BeginPage(string(from))
	for i, rec := range page.Records {
		key, resolutions := w.hist.Key(rowOf(rec))
		r := records[i]
		r.Delivery = activity.Delivery{Key: key, Chain: activity.ChainHistory, Rank: w.nextHist()}
		change, err := w.proj.Apply(r)
		if errors.Is(err, activity.ErrCapacity) {
			return w.closeHistoryForCapacity()
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrRebuild, err)
		}
		w.applied(key, r.Raw, change)
		if err := w.resolve(resolutions); err != nil {
			return err
		}
	}
	if page.HasMore {
		if err := w.proj.EndHistoryPage(string(page.Next)); err != nil {
			return fmt.Errorf("%w: %v", ErrRebuild, err)
		}
		w.hist.EndPage(string(page.Next))
		w.histNext = page.Next
		return nil
	}
	if err := w.resolve(w.hist.ReachedStart()); err != nil {
		return err
	}
	_ = w.proj.EndHistory(activity.CoverageReachedStart)
	return ErrBeyondCoverage
}

func (w *Window) resolve(rs []activity.Resolution) error {
	for _, r := range rs {
		change, err := w.proj.Resolve(r)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrRebuild, err)
		}
		change.Chain = activity.ChainHistory // Resolutions come only from history pages.
		w.track.add(change)
	}
	return nil
}

func (w *Window) applied(key activity.OccurrenceKey, raw []byte, change activity.Change) {
	w.track.add(change)
	if w.onApply != nil && raw != nil && change.Kind != activity.ChangeRedelivery {
		w.onApply(key, raw, change)
	}
}

// historyError ends the history chain on a missing or changed source and
// passes transient errors through unchanged.
func (w *Window) historyError(err error) error {
	switch {
	case errors.Is(err, audit.ErrHistoryGap):
		_ = w.proj.EndHistory(activity.CoverageGap)
		return ErrBeyondCoverage
	case errors.Is(err, audit.ErrInvalidCursor):
		_ = w.proj.EndHistory(activity.CoverageChanged)
		return ErrBeyondCoverage
	case errors.Is(err, audit.ErrHistoryDisabled):
		w.state = StateDisabled
		return ErrDisabled
	}
	return err
}

// liveError: a broken live chain means the window must be rebuilt.
func liveError(err error) error {
	if errors.Is(err, audit.ErrHistoryGap) || errors.Is(err, audit.ErrInvalidCursor) {
		return fmt.Errorf("%w: %v", ErrRebuild, err)
	}
	if errors.Is(err, audit.ErrHistoryDisabled) {
		return ErrDisabled
	}
	return err
}

func (w *Window) closeHistoryForCapacity() error {
	if !w.proj.Joined() {
		return fmt.Errorf("%w: history snapshot exceeds the window capacity", ErrCapacity)
	}
	bounds := w.proj.Boundaries()
	if len(bounds) > 0 {
		w.raise(bounds[len(bounds)-1])
	} else {
		w.raise("")
	}
	if !w.histClosed {
		w.state = StateCapacity // An archive window has no floor to raise.
		return ErrCapacity
	}
	return ErrUseArchive
}

func (w *Window) raise(floor string) int {
	w.bump()
	if w.beforeRaise != nil {
		w.beforeRaise(w.proj)
	}
	n, err := w.proj.RaiseFloor(floor)
	if err != nil {
		return 0
	}
	w.histClosed = true
	w.hist = activity.HistoryCarry{} // Its pending rows stay provisional.
	return n
}

// raiseLive frees room for live rows: each history boundary from the oldest
// to the newest, then the live region, until a tenth of the capacity is
// free (hysteresis, so each later row does not trigger another eviction
// pass). It reports whether anything was evicted.
func (w *Window) raiseLive() bool {
	target := w.capacity - w.capacity/10
	evicted := 0
	bounds := w.proj.Boundaries()
	for i := len(bounds) - 1; i >= 0 && w.proj.Occurrences() > target; i-- {
		evicted += w.raise(bounds[i])
	}
	if w.proj.Occurrences() > target {
		evicted += w.raise("")
	}
	return evicted > 0
}

// hydrate returns one activity.Record per page row, reading deferred rows in
// full; the page is applied only after every row is available.
func (w *Window) hydrate(ctx context.Context, page audit.HistoryPage) ([]activity.Record, error) {
	out := make([]activity.Record, len(page.Records))
	for i, rec := range page.Records {
		switch {
		case rec.Problem != nil:
			out[i].Problem = rec.Problem.Error()
		case rec.Deferred:
			data, _, err := w.reader.ReadRecord(ctx, rec.Ref)
			if err != nil {
				return nil, err
			}
			if int64(len(data)) != rec.Size {
				return nil, audit.ErrInvalidCursor
			}
			out[i].Raw = data
		default:
			out[i].Raw = rec.Data
		}
	}
	return out, nil
}

func rowOf(rec audit.HistoryRecord) activity.Row {
	if rec.Ref != (audit.RecordRef{}) {
		return activity.Row{Ref: rec.Ref.String()}
	}
	row := activity.Row{}
	if rec.Problem != nil {
		row.Start, row.End = rec.Problem.Start, rec.Problem.End
	}
	return row
}

// applyLive applies one forward page. On capacity it raises the floor and
// retries the same delivery with the same rank while that frees room.
func (w *Window) applyLive(ctx context.Context, page audit.HistoryPage) (err error) {
	if len(page.Records) > 0 {
		w.bump()
	}
	records, err := w.hydrate(ctx, page)
	if err != nil {
		return liveError(err)
	}
	saved := w.carry
	defer func() {
		if err != nil {
			w.carry = saved // A failed page is read again from the same head.
		}
	}()
	for i, rec := range page.Records {
		key := w.carry.Key(rowOf(rec))
		w.liveRank++
		r := records[i]
		r.Delivery = activity.Delivery{Key: key, Chain: activity.ChainLive, Rank: w.liveRank}
		for {
			change, err := w.proj.Apply(r)
			if errors.Is(err, activity.ErrCapacity) {
				if !w.proj.Joined() {
					return fmt.Errorf("%w: live catch-up before join exceeds the capacity", ErrCapacity)
				}
				if w.raiseLive() {
					continue
				}
				w.state = StateCapacity
				return ErrCapacity
			}
			if err != nil {
				return fmt.Errorf("%w: %v", ErrRebuild, err)
			}
			w.applied(key, r.Raw, change)
			break
		}
	}
	w.head = page.Next
	return nil
}

// Scan continues the history chain for up to maxPages pages or until the
// deadline, applying every row. It returns ErrUseArchive or
// ErrBeyondCoverage when the chain cannot continue.
func (w *Window) Scan(ctx context.Context, maxPages int, deadline time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != StateOK {
		return &StateError{State: w.state}
	}
	for i := 0; i < maxPages && time.Now().Before(deadline); i++ {
		if err := w.historyPage(ctx, w.histNext); err != nil {
			return err
		}
	}
	return nil
}

// CatchUp reads forward from the window's own head (at most maxPages pages)
// and reports what changed since the live cursor `after`, which must be one
// this window issued and can still resume from. It returns the new head
// cursor, whether more journal follows and whether the floor moved since
// `after` was issued.
func (w *Window) CatchUp(ctx context.Context, after string, maxPages int) (_ activity.Changes, _ string, _ bool, floorChanged bool, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != StateOK {
		return activity.Changes{}, "", false, false, &StateError{State: w.state}
	}
	from, ok := w.issued[after]
	if !ok || !w.proj.CanResume(after) || w.archive {
		return activity.Changes{}, "", false, false, ErrRebuild
	}
	before := w.head
	defer func() {
		if err != nil && w.head != before && w.state == StateOK {
			w.issue() // Applied pages: keep the head a cursor a snapshot may hand out.
		}
	}()
	more := false
	for i := 0; i < maxPages; i++ {
		page, err := w.reader.ReadPage(ctx, w.head, audit.Forward, pageBudget)
		if err != nil {
			return activity.Changes{}, "", false, false, liveError(err)
		}
		if err := w.applyLive(ctx, page); err != nil {
			return activity.Changes{}, "", false, false, err
		}
		if more = page.HasMore; !more {
			break
		}
	}
	head := string(w.head)
	if w.head != before || head != after {
		head = w.issue() // Only a moved head is a new resumable cursor.
	}
	for c := range w.issued {
		if !w.proj.CanResume(c) {
			delete(w.issued, c)
		}
	}
	cov := w.proj.Coverage()
	return w.proj.ChangedSince(from.rank), head, more, cov.FloorSet != from.floor.FloorSet || cov.Floor != from.floor.Floor, nil
}

// Rebuild replaces the projection with a fresh one: a new generation whose
// client must replace its snapshot; activity between the old live position
// and the new snapshot may be unseen.
func (w *Window) Rebuild(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.generation++
	w.unseen = true
	return w.build(ctx)
}

// With runs fn with the projection under the window lock. fn must only
// read: mutations are not versioned, so cached views would go stale (index
// sync goes through SyncIndex).
func (w *Window) With(fn func(p *activity.Projection)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fn(w.proj)
}

// Info reports the window's generation, state, head cursor and whether
// activity may have been missed across a rebuild.
func (w *Window) Info() (generation uint64, state State, head string, possibleUnseen bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.generation, w.state, string(w.head), w.unseen
}

func (w *Window) fail() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.bump()
	w.state = StateFailed
}

// ViewKey identifies a state of the window's views: it changes whenever the
// projection or the index state synced into it changes.
type ViewKey struct{ Window, Index uint64 }

// Key returns the current view key.
func (w *Window) Key() ViewKey {
	k, _ := w.keyAndHead()
	return k
}

// keyAndHead returns the view key with the newest issued live cursor, read
// together: a view of that key holds every row up to that cursor, because
// issuing a cursor applies no rows.
func (w *Window) keyAndHead() (ViewKey, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	head := w.newest
	if w.archive {
		head = ""
	}
	return ViewKey{Window: w.version, Index: w.synced.Index}, head
}

// Newest returns the newest issued live cursor.
func (w *Window) Newest() string {
	_, head := w.keyAndHead()
	return head
}

// SyncIndex copies the index's possibly-unrecorded flags into the projection
// unless neither changed since the last sync (index lock inside the window
// lock).
func (w *Window) SyncIndex(x *Index) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := ViewKey{Window: w.version, Index: x.Version()}
	if w.hasSync && key == w.synced {
		return
	}
	x.Sync(w.proj) // The index may move on meanwhile; the next sync sees a newer version.
	w.synced, w.hasSync = key, true
}

// ViewMeta describes the window state a view was built from. Head is the
// newest issued live cursor (empty for archive windows): the view holds at
// least its rows, possibly more applied by a stream since, which a catch-up
// from Head delivers again.
type ViewMeta struct {
	Generation     uint64
	Key            ViewKey
	Head           string
	PossibleUnseen bool
}

// View runs fn with the projection and its state under the window lock.
func (w *Window) View(fn func(p *activity.Projection, meta ViewMeta)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	meta := ViewMeta{Generation: w.generation, Key: ViewKey{Window: w.version, Index: w.synced.Index}, PossibleUnseen: w.unseen}
	if !w.archive {
		meta.Head = w.newest
	}
	fn(w.proj, meta)
}

// Resumable reports whether a live cursor can still be caught up from.
func (w *Window) Resumable(cursor string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.issued[cursor]
	return ok && w.proj.CanResume(cursor)
}

// StreamStart returns the rank and coverage of a resumable live cursor of
// the given generation: a stream reports changes after it.
func (w *Window) StreamStart(generation uint64, after string) (int64, activity.CoverageView, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.state != StateOK:
		return 0, activity.CoverageView{}, &StateError{State: w.state}
	case w.archive:
		return 0, activity.CoverageView{}, errNotLive
	case w.generation != generation:
		return 0, activity.CoverageView{}, ErrGenerationChanged
	}
	from, ok := w.issued[after]
	if !ok || !w.proj.CanResume(after) {
		return 0, activity.CoverageView{}, ErrRebuild
	}
	return from.rank, from.floor, nil
}

// AdvanceResult is what a stream learns from one advance.
type AdvanceResult struct {
	Changes  activity.Changes // Changed since the stream's rank.
	Rank     int64            // The stream's new rank.
	Head     audit.Cursor     // Where to wait for new journal data.
	More     bool             // Journal beyond Head was not read yet.
	Waiting  bool             // Backpressure: the client must catch up first.
	Coverage activity.CoverageView
	Changed  <-chan struct{} // Closed on the next change after this result.
}

// Advance applies forward pages for a stream without issuing a cursor, so
// streams never push a client's cursors out of the resume window. It stops
// once the window holds maxAhead live rows beyond the newest issued cursor
// (backpressure), after maxPages pages or at the deadline. The result is
// read under one lock with the generation check.
func (w *Window) Advance(ctx context.Context, generation uint64, since int64, maxPages int, deadline time.Time,
	maxAhead int64) (AdvanceResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.state != StateOK:
		return AdvanceResult{}, &StateError{State: w.state}
	case w.archive:
		return AdvanceResult{}, errNotLive
	case w.generation != generation:
		return AdvanceResult{}, ErrGenerationChanged
	}
	var res AdvanceResult
	for i := 0; i < maxPages && time.Now().Before(deadline); i++ {
		if w.liveRank-w.newestRank >= maxAhead {
			res.Waiting = true
			break
		}
		page, err := w.reader.ReadPage(ctx, w.head, audit.Forward, pageBudget)
		if errors.Is(err, audit.ErrHistoryScanLimit) {
			// No progress from the head: treat it as a source problem.
			return AdvanceResult{}, fmt.Errorf("%w: %v", ErrRebuild, err)
		}
		if err != nil {
			return AdvanceResult{}, liveError(err)
		}
		if err := w.applyLive(ctx, page); err != nil {
			return AdvanceResult{}, err
		}
		if res.More = page.HasMore; !res.More {
			break
		}
	}
	res.Changes = w.proj.ChangedSince(since)
	res.Rank, res.Head, res.Coverage, res.Changed = w.liveRank, w.head, w.proj.Coverage(), w.changed
	return res, nil
}

// ChangeSet is what history-chain applies changed in items that may already
// be loaded: transitions, reattributions, older journal copies (which can
// change an item's primary record, order, conflict mark and even lane),
// resolutions, and new actions (which may absorb an effect delivered
// separately until then).
type ChangeSet struct {
	Requests    map[activity.RequestKey]bool
	Effects     map[activity.EffectKey]bool
	Occurrences map[activity.OccurrenceKey]activity.OccurrenceKey // Resolved row → its provisional key.
}

func (c *ChangeSet) add(change activity.Change) {
	if c == nil || change.Chain != activity.ChainHistory {
		return
	}
	switch change.Kind {
	case activity.ChangeTransition, activity.ChangeReattributed, activity.ChangeNewAction,
		activity.ChangeDuplicate, activity.ChangeConflict:
	case activity.ChangeResolved:
		if change.Occurrence != nil && change.Replaced != nil {
			c.Occurrences[*change.Occurrence] = *change.Replaced
		}
	default:
		return
	}
	if change.Request != nil {
		c.Requests[*change.Request] = true
	}
	if change.Effect != nil {
		c.Effects[*change.Effect] = true
	}
}

// Track starts collecting history-chain changes; Untrack returns them.
func (w *Window) Track() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.track = &ChangeSet{Requests: map[activity.RequestKey]bool{}, Effects: map[activity.EffectKey]bool{},
		Occurrences: map[activity.OccurrenceKey]activity.OccurrenceKey{}}
}

// Untrack stops collecting and returns the changes since Track.
func (w *Window) Untrack() ChangeSet {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.track
	w.track = nil
	if t == nil {
		return ChangeSet{}
	}
	return *t
}
