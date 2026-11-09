package activity

import (
	"cmp"
	"errors"
	"slices"
)

// ErrIndexCursor means a page was not read from the index's current cursor:
// it was already applied, or pages were skipped.
var ErrIndexCursor = errors.New("activity: in-flight index page does not continue its cursor")

// IndexStatus is the state of the server-wide in-flight index.
type IndexStatus uint8

const (
	// IndexUnknown: the startup frontier F0 was not captured, or catching up
	// failed (for example a sealed archive of this server disappeared).
	// In-flight work of this server instance cannot be listed until restart.
	IndexUnknown IndexStatus = iota + 1
	// IndexCatchingUp: pages after the cursor have not been read yet.
	IndexCatchingUp
	// IndexReady: the index reflects the journal up to its last read.
	IndexReady
	// IndexIncomplete: more work was in flight than the index keeps.
	IndexIncomplete
)

// InFlightEffect is an effect of the current server instance whose
// operation_start is recorded and whose operation_finish is not.
type InFlightEffect struct {
	Key    EffectKey
	Tool   string
	Origin *RequestKey
	Start  OccurrenceKey
	// PossiblyUnrecorded: the audit was degraded after the start, so the
	// finish may have failed to append and may never be recorded.
	PossiblyUnrecorded bool
	seq                uint64
}

// openRequest is a request of the current instance with a recorded start and
// no recorded finish or rejection.
type openRequest struct {
	possiblyUnrecorded bool
	seq                uint64
}

// InFlightIndex lists unfinished work of the current server instance: every
// in-flight effect, however long ago it started, so a window that starts
// later can still show it; and open requests, only so that a degraded audit
// can mark them possibly unrecorded instead of open forever.
//
// It is derived from the journal: the adapter reads forward from the
// frontier F0 captured at startup before MCP admission opened, hydrates
// deferred rows, and applies whole pages in order. It keeps no raw bytes and
// is not safe for concurrent use (the adapter catches up single-flight). A
// damaged copy of an operation_start that appears after its finish would
// re-list a finished effect until restart; journals written by the server do
// not contain such copies.
type InFlightIndex struct {
	server   string
	cursor   string
	status   IndexStatus
	limit    int
	seq      uint64
	effects  map[EffectKey]*InFlightEffect
	requests map[RequestKey]*openRequest
	// Possibly unrecorded keys dropped to make room; their flag survives
	// (up to limit keys each).
	droppedEffects  map[EffectKey]bool
	droppedRequests map[RequestKey]bool
	dropped         int // Possibly unrecorded entries dropped from the lists.
	skipped         int // Damaged or undecodable rows after F0.
}

// NewInFlightIndex starts an index for serverID from cursor f0. ok is false
// when F0 could not be captured; the index then stays IndexUnknown. limit
// bounds effects and open requests separately (0 = unlimited).
func NewInFlightIndex(serverID, f0 string, ok bool, limit int) *InFlightIndex {
	x := &InFlightIndex{
		server: serverID, cursor: f0, status: IndexCatchingUp, limit: limit,
		effects: make(map[EffectKey]*InFlightEffect), requests: make(map[RequestKey]*openRequest),
		droppedEffects: make(map[EffectKey]bool), droppedRequests: make(map[RequestKey]bool),
	}
	if !ok {
		x.status = IndexUnknown
	}
	return x
}

// Cursor is the forward cursor the next page must be read from.
func (x *InFlightIndex) Cursor() string { return x.cursor }

// IndexRow is one row of a forward page. Exactly one of Raw (the complete
// record; deferred rows must be read in full first) and Problem (a row the
// reader reported as damaged, which the index skips) is set.
type IndexRow struct {
	Key     OccurrenceKey
	Raw     []byte
	Problem string
}

// ApplyPage applies one forward page read from `from`, whose Next cursor is
// next; hasMore is the page's HasMore. A page from any other cursor is
// refused, so a retried page is never applied twice; a page with a row that
// breaks the row contract is refused as a whole.
func (x *InFlightIndex) ApplyPage(from string, rows []IndexRow, next string, hasMore bool) error {
	if x.status == IndexUnknown {
		return nil
	}
	if from != x.cursor {
		return ErrIndexCursor
	}
	for _, row := range rows {
		if (row.Raw == nil) == (row.Problem == "") {
			return ErrRecordContract
		}
	}
	for _, row := range rows {
		if row.Raw == nil {
			x.skipped++
			continue
		}
		e, err := Decode(row.Raw)
		if err != nil {
			x.skipped++
			continue
		}
		if e.ServerID == x.server {
			x.apply(row.Key, e)
		}
	}
	x.cursor = next
	switch {
	case x.status == IndexIncomplete:
	case hasMore:
		x.status = IndexCatchingUp
	default:
		x.status = IndexReady
	}
	return nil
}

func (x *InFlightIndex) apply(key OccurrenceKey, e Event) {
	rk := RequestKey{ServerID: e.ServerID, RequestID: e.RequestID}
	ek := EffectKey{ServerID: e.ServerID, OperationID: e.OperationID}
	switch e.Kind {
	case KindRequestStart:
		if e.RequestID == "" || x.requests[rk] != nil {
			return
		}
		if x.limit > 0 && len(x.requests) >= x.limit && !x.dropOldestRequest() {
			x.status = IndexIncomplete
			return
		}
		x.seq++
		x.requests[rk] = &openRequest{seq: x.seq}
	case KindRequestFinish, KindRequestRejected:
		delete(x.requests, rk)
		delete(x.droppedRequests, rk)
	case KindOperationStart:
		if e.OperationID == "" || x.effects[ek] != nil {
			return
		}
		if x.limit > 0 && len(x.effects) >= x.limit && !x.dropOldestEffect() {
			x.status = IndexIncomplete
			return
		}
		x.seq++
		entry := &InFlightEffect{Key: ek, Tool: e.Tool, Start: key, seq: x.seq}
		if e.RequestID != "" {
			entry.Origin = &rk
		}
		x.effects[ek] = entry
	case KindOperationFinish:
		delete(x.effects, ek)
		delete(x.droppedEffects, ek)
	}
}

func (x *InFlightIndex) dropOldestRequest() bool {
	return dropOldest(x, x.requests, x.droppedRequests,
		func(r *openRequest) (bool, uint64) { return r.possiblyUnrecorded, r.seq })
}

func (x *InFlightIndex) dropOldestEffect() bool {
	return dropOldest(x, x.effects, x.droppedEffects,
		func(f *InFlightEffect) (bool, uint64) { return f.PossiblyUnrecorded, f.seq })
}

// dropOldest removes the oldest possibly unrecorded entry and remembers its
// key (while the remembered set is below the limit) so its flag survives.
// It reports whether room was made.
func dropOldest[K comparable, V any](x *InFlightIndex, m map[K]V, dropped map[K]bool, flag func(V) (bool, uint64)) bool {
	var oldest K
	var oldestSeq uint64
	found := false
	for k, v := range m {
		if unrecorded, seq := flag(v); unrecorded && (!found || seq < oldestSeq) {
			oldest, oldestSeq, found = k, seq, true
		}
	}
	if found {
		delete(m, oldest)
		if len(dropped) < x.limit {
			dropped[oldest] = true
		}
		x.dropped++
	}
	return found
}

// ObserveAuditDegraded records that audit health was reported degraded: the
// finish of any listed request or effect may have failed to append. Catch
// the index up to the current frontier first; admission refuses new work
// while degraded, so all affected work started before that frontier. Polling
// health is best-effort and can miss a short episode.
func (x *InFlightIndex) ObserveAuditDegraded() {
	for _, e := range x.effects {
		e.PossiblyUnrecorded = true
	}
	for _, r := range x.requests {
		r.possiblyUnrecorded = true
	}
}

// Fail makes the index permanently unknown, for example after a reader gap
// or invalid cursor during catch-up: F0 cannot be re-derived.
func (x *InFlightIndex) Fail() {
	x.status = IndexUnknown
	clear(x.effects)
	clear(x.requests)
	clear(x.droppedEffects)
	clear(x.droppedRequests)
}

// Status reports the index state.
func (x *InFlightIndex) Status() IndexStatus { return x.status }

// Dropped counts possibly unrecorded entries removed from the lists to make
// room; the UI should say that such work may be missing from the list.
func (x *InFlightIndex) Dropped() int { return x.dropped }

// Skipped counts damaged or undecodable rows read after F0. If one was a
// finish record of this instance, its start stays listed until restart, so
// the UI should say that the list may include finished work.
func (x *InFlightIndex) Skipped() int { return x.skipped }

// InFlight lists in-flight effects in start order.
func (x *InFlightIndex) InFlight() []InFlightEffect {
	out := make([]InFlightEffect, 0, len(x.effects))
	for _, e := range x.effects {
		out = append(out, *e)
	}
	slices.SortFunc(out, func(a, b InFlightEffect) int { return cmp.Compare(a.seq, b.seq) })
	return out
}

// Lookup returns the index entry of an effect, if it is in flight.
func (x *InFlightIndex) Lookup(key EffectKey) (InFlightEffect, bool) {
	e, ok := x.effects[key]
	if !ok {
		return InFlightEffect{}, false
	}
	return *e, true
}

func (x *InFlightIndex) effectUnrecorded(key EffectKey) bool {
	if e, ok := x.effects[key]; ok {
		return e.PossiblyUnrecorded
	}
	return x.droppedEffects[key]
}

func (x *InFlightIndex) requestUnrecorded(key RequestKey) bool {
	if r, ok := x.requests[key]; ok {
		return r.possiblyUnrecorded
	}
	return x.droppedRequests[key]
}

// SyncIndex copies the index's possibly-unrecorded flags into the window.
// Flagged requests and effects that the window still sees unfinished are
// reported as possibly unrecorded and are no longer pinned against eviction.
// The window's own evidence wins: a recorded finish clears the flag. Like
// eviction, this depends on when it is called: call it after every index
// catch-up step and before every RaiseFloor.
func (p *Projection) SyncIndex(x *InFlightIndex) {
	clear(p.unrecordedRequests)
	clear(p.unrecordedEffects)
	if x == nil {
		return
	}
	for rk := range p.actions {
		if x.requestUnrecorded(rk) {
			p.unrecordedRequests[rk] = true
		}
	}
	for ek := range p.effects {
		if x.effectUnrecorded(ek) {
			p.unrecordedEffects[ek] = true
		}
	}
}

// InFlightOutside lists the index's in-flight effects that this window does
// not contain, for example a download started before the window; the window
// shows them as running work whose origin is outside it. This includes
// effects the window evicted after they were marked possibly unrecorded; the
// entry's flag must be shown with it.
func (p *Projection) InFlightOutside(x *InFlightIndex) []InFlightEffect {
	var out []InFlightEffect
	for _, e := range x.InFlight() {
		if _, ok := p.effects[e.Key]; !ok {
			out = append(out, e)
		}
	}
	return out
}
