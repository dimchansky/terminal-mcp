package activity

import (
	"errors"
	"slices"
)

var (
	// ErrCapacity means the window holds its maximum number of occurrences;
	// the delivery was not applied and consumed no rank. After RaiseFloor the
	// same Delivery, with the same rank, may be applied again.
	ErrCapacity = errors.New("activity: window capacity reached")
	// ErrFloor means a floor is not one of the window's history page
	// boundaries, is older than the current floor, or the window has not
	// joined yet.
	ErrFloor = errors.New("activity: invalid floor")
)

// liveResumeWindow is how many issued live cursors a window accepts for
// resuming a stream. Rows after the oldest of them are never evicted, so a
// resume cannot redeliver an evicted row as if it were new.
const liveResumeWindow = 8

// CoverageEnd tells why the history chain of a window stopped.
type CoverageEnd uint8

const (
	// CoverageOpen: the history chain can still read older pages.
	CoverageOpen CoverageEnd = iota
	// CoverageReachedStart: the oldest visible record was read. It is not
	// proof of the journal's beginning (an unreferenced archive may have
	// been removed undetectably).
	CoverageReachedStart
	// CoverageGap: a referenced journal source is missing (reader gap).
	CoverageGap
	// CoverageChanged: a cursor no longer validates (reader invalid cursor).
	CoverageChanged
	// CoverageEvicted: the window evicted its oldest rows; older history is
	// read by an archive window from Floor.
	CoverageEvicted
)

// boundary is the Next cursor of an applied history page and the last
// history rank applied when that page ended.
type boundary struct {
	cursor string
	rank   int64
}

// liveCursor is an issued live cursor and the last live rank before it.
type liveCursor struct {
	cursor string
	rank   int64
}

// CoverageView describes what the window covers. End keeps the reason the
// history chain stopped first; a later eviction is reported by FloorSet.
type CoverageView struct {
	End CoverageEnd
	// Floor is the history cursor below which rows were evicted, when
	// FloorSet. An empty Floor with FloorSet means rows of the live region
	// were evicted too: an archive window must start from the newest record.
	Floor    string
	FloorSet bool
}

// SetCapacity limits the number of occurrences the window keeps; zero means
// unlimited.
func (p *Projection) SetCapacity(maxOccurrences int) { p.capacity = maxOccurrences }

// EndHistoryPage records the Next cursor of a history page that has been
// applied in full. Only such boundaries can become the floor.
func (p *Projection) EndHistoryPage(next string) error {
	if !p.pageOpen || p.coverage != CoverageOpen || next == "" {
		return ErrJoinProtocol
	}
	p.pageOpen, p.h1First = false, false
	p.boundaries = append(p.boundaries, boundary{cursor: next, rank: p.occ.lastHistory})
	return nil
}

// EndHistory records that the history chain stopped; Needs then report
// unresolved origins as beyond coverage.
func (p *Projection) EndHistory(reason CoverageEnd) error {
	if p.coverage != CoverageOpen || reason == CoverageOpen || reason == CoverageEvicted {
		return ErrJoinProtocol
	}
	p.coverage, p.pageOpen, p.h1First = reason, false, false
	return nil
}

// Coverage returns the window's coverage state.
func (p *Projection) Coverage() CoverageView {
	return CoverageView{End: p.coverage, Floor: p.floor, FloorSet: p.floorSet}
}

// IssueLiveCursor records a live cursor handed to a viewer. Only the latest
// liveResumeWindow cursors are accepted by CanResume.
func (p *Projection) IssueLiveCursor(cursor string) {
	p.liveCursors = append(p.liveCursors, liveCursor{cursor: cursor, rank: p.occ.lastLive})
	if len(p.liveCursors) > liveResumeWindow {
		p.liveCursors = p.liveCursors[len(p.liveCursors)-liveResumeWindow:]
	}
}

// CanResume reports whether a stream may resume from cursor in this window.
// Otherwise the adapter starts a new window generation.
func (p *Projection) CanResume(cursor string) bool {
	for _, c := range p.liveCursors {
		if c.cursor == cursor {
			return true
		}
	}
	return false
}

// RaiseFloor evicts finished or unknown-outcome entities whose occurrences
// are all older than floor, and closes the history chain. floor must be a
// recorded history page boundary not older than the current floor, or ""
// to also evict rows of the live region up to the oldest resumable live
// cursor; that may be repeated as the live chain grows. An archive window has
// no live region and refuses "". Unfinished work of the current server
// instance is never evicted unless SyncIndex marked it possibly unrecorded.
// It returns the number of evicted occurrences.
//
// Eviction depends on when it is requested, so after a raise the window is
// no longer a pure function of the applied rows; it is still deterministic
// for the same sequence of calls. A process whose creator was evicted shows
// no creator; the coverage tells the UI it is outside the loaded history.
func (p *Projection) RaiseFloor(floor string) (int, error) {
	if !p.Joined() {
		return 0, ErrFloor
	}
	var older func(*chainRanks) bool
	if floor == "" {
		if p.join == joinHistoryOnly {
			return 0, ErrFloor
		}
		limit := p.occ.lastLive
		if len(p.liveCursors) > 0 {
			limit = p.liveCursors[0].rank
		}
		older = func(r *chainRanks) bool { return r.live == 0 || r.live <= limit }
	} else {
		rank, ok := int64(0), false
		for _, b := range p.boundaries {
			if b.cursor == floor {
				rank, ok = b.rank, true
				break
			}
		}
		if !ok || (p.floorSet && (p.floor == "" || rank < p.floorRank)) {
			return 0, ErrFloor
		}
		p.floorRank = rank
		older = func(r *chainRanks) bool { return r.live == 0 && r.history < rank }
	}
	p.floor, p.floorSet = floor, true
	if p.coverage == CoverageOpen {
		p.coverage, p.pageOpen = CoverageEvicted, false
	}
	return p.evict(older), nil
}

func (p *Projection) evict(older func(*chainRanks) bool) int {
	allOlder := func(phases []phase) bool {
		for _, ph := range phases {
			if ph.more > 0 {
				return false // Untracked copies cannot be checked.
			}
			for _, key := range ph.occ {
				if !older(p.occ.byKey[key]) {
					return false
				}
			}
		}
		return true
	}
	running := func(server string, phases []phase) bool {
		// Unfinished work of the current instance may still complete.
		return server != "" && server == p.currentServer &&
			len(phases[slotFinish].occ) == 0 && (len(phases) <= int(slotRejected) || len(phases[slotRejected].occ) == 0)
	}
	effectEvictable := func(ek EffectKey) bool {
		f := p.effects[ek]
		pinned := running(ek.ServerID, f.phases[:]) && !p.unrecordedEffects[ek]
		return !pinned && allOlder(f.phases[:])
	}
	actionEvictable := func(rk RequestKey) bool {
		a := p.actions[rk]
		pinned := running(rk.ServerID, a.phases[:]) && !p.unrecordedRequests[rk]
		if pinned || !allOlder(a.phases[:]) {
			return false
		}
		for ek := range p.byOrigin[rk] {
			if !effectEvictable(ek) {
				return false
			}
		}
		return true
	}

	var actions []RequestKey
	var effects []EffectKey
	for rk := range p.actions {
		if actionEvictable(rk) {
			actions = append(actions, rk)
		}
	}
	for ek, f := range p.effects {
		origin, hasOrigin := p.effectOrigin(f)
		if effectEvictable(ek) && (!hasOrigin || p.actions[origin] == nil || actionEvictable(origin)) {
			effects = append(effects, ek)
		}
	}
	evicted := 0
	drop := func(key OccurrenceKey) {
		delete(p.occ.byKey, key)
		delete(p.facts, key)
		evicted++
	}
	goneActions := make(map[RequestKey]bool, len(actions))
	for _, rk := range actions {
		for _, ph := range p.actions[rk].phases {
			for _, key := range ph.occ {
				drop(key)
			}
		}
		delete(p.actions, rk)
		goneActions[rk] = true
	}
	goneEffects := make(map[EffectKey]bool, len(effects))
	for _, ek := range effects {
		for _, ph := range p.effects[ek].phases {
			for _, key := range ph.occ {
				drop(key)
			}
		}
		delete(p.effects, ek)
		goneEffects[ek] = true
	}
	dropFromIndex(p.byProcessRequest, goneActions)
	dropFromIndex(p.byOrigin, goneEffects)
	dropFromIndex(p.byProcessEffect, goneEffects)
	for key := range p.diagnostics {
		if older(p.occ.byKey[key]) {
			delete(p.diagnostics, key)
			drop(key)
		}
	}
	for key := range p.servers {
		if older(p.occ.byKey[key]) {
			delete(p.servers, key)
			drop(key)
		}
	}
	return evicted
}

// dropFromIndex removes evicted values in one pass over the index.
func dropFromIndex[K, V comparable](index map[K]map[V]bool, gone map[V]bool) {
	if len(gone) == 0 {
		return
	}
	for key, set := range index {
		for value := range set {
			if gone[value] {
				delete(set, value)
			}
		}
		if len(set) == 0 {
			delete(index, key)
		}
	}
}

// Occurrences reports how many occurrences the window holds, for capacity
// prechecks.
func (p *Projection) Occurrences() int { return len(p.occ.byKey) }

// Boundaries lists the recorded history page boundaries in reading order,
// newest first; the last one is where the history chain would continue.
func (p *Projection) Boundaries() []string {
	out := make([]string, len(p.boundaries))
	for i, b := range p.boundaries {
		out[i] = b.cursor
	}
	return out
}

// Changes lists what holds an occurrence first delivered on the live chain
// after liveRank: the occurrences themselves in journal order (including
// diagnostics), and the actions and effects they belong to.
type Changes struct {
	Occurrences []OccurrenceKey
	Actions     []RequestKey
	Effects     []EffectKey
}

// ChangedSince reports the changes after a live rank the caller recorded when
// it issued a live cursor.
func (p *Projection) ChangedSince(liveRank int64) Changes {
	var c Changes
	newer := func(key OccurrenceKey) bool {
		r := p.occ.byKey[key]
		return r != nil && r.live > liveRank
	}
	for key := range p.occ.byKey {
		if newer(key) {
			c.Occurrences = append(c.Occurrences, key)
		}
	}
	slices.SortFunc(c.Occurrences, func(a, b OccurrenceKey) int { return p.order(a).Compare(p.order(b)) })
	for rk, a := range p.actions {
		if anyNewer(a.phases[:], newer) {
			c.Actions = append(c.Actions, rk)
		}
	}
	for ek, f := range p.effects {
		if anyNewer(f.phases[:], newer) {
			c.Effects = append(c.Effects, ek)
		}
	}
	slices.SortFunc(c.Actions, compareRequest)
	slices.SortFunc(c.Effects, compareEffect)
	return c
}

func anyNewer(phases []phase, newer func(OccurrenceKey) bool) bool {
	for _, ph := range phases {
		for _, key := range ph.occ {
			if newer(key) {
				return true
			}
		}
	}
	return false
}

// Knows reports whether the window holds an occurrence.
func (p *Projection) Knows(key OccurrenceKey) bool { return p.occ.byKey[key] != nil }

// HasAction reports whether the window holds an action, without building
// its view.
func (p *Projection) HasAction(key RequestKey) bool { return p.actions[key] != nil }
