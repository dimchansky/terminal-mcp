package activity

import "errors"

// ErrJoinProtocol means the adapter broke the history-to-live handoff order
// of a window; the window must be rebuilt.
var ErrJoinProtocol = errors.New("activity: history/live join protocol violated")

type joinPhase uint8

const (
	joinWaiting     joinPhase = iota // No history snapshot applied yet.
	joinPending                      // Snapshot applied; live chain catching up.
	joinDone                         // Order is journal order; publishable.
	joinHistoryOnly                  // Archive window: no live chain at all.
)

// The handoff of one live window (design section 4):
//
//  1. Capture the forward frontier F (FrontierCursor).
//  2. Read the newest history page H1: BeginHistoryPage(""), apply its rows,
//     EndHistoryPage(next) (or EndHistory if it reached the start). If H1 is
//     retried, repeat this step; the most recent read counts.
//  3. StartJoin. From now on history pages may only continue from cursors
//     returned by earlier history pages: a fresh read of the newest page could
//     return rows newer than the live chain and break the order. A window that
//     needs one starts a new generation instead.
//  4. Read forward from F without waiting until a page returns HasMore=false,
//     applying every row of every page on the live chain; then LiveCaughtUp.
//     Those reads must start after H1 returned; the adapter owns that.
//  5. Joined is now true: views may be published and live waiting begins.
//
// If the live chain delivers H1's newest row before step 4 ends, the window
// joins early, but only when that row has a Ref: a weak key cannot prove it.
// An archive window that never has a live chain calls HistoryOnly first.

// BeginHistoryPage opens a history page read from cursor. A live window
// reads the newest page ("") only before StartJoin; afterwards, and in an
// archive window after its first page, a page must start at the Next cursor
// of an earlier page of this window. An archive window's first page may start
// anywhere (its floor, or "").
func (p *Projection) BeginHistoryPage(cursor string) error {
	if p.pageOpen || p.coverage != CoverageOpen {
		return ErrJoinProtocol
	}
	allowed := false
	switch {
	case p.join == joinWaiting:
		allowed = cursor == ""
	case p.join == joinHistoryOnly && !p.anyPage:
		allowed = true
	default:
		for _, b := range p.boundaries {
			if b.cursor == cursor {
				allowed = true
				break
			}
		}
	}
	if !allowed {
		return ErrJoinProtocol
	}
	p.pageOpen, p.anyPage = true, true
	if p.join == joinWaiting {
		p.h1Read, p.joinNewest, p.h1First = true, OccurrenceKey{}, true
	}
	return nil
}

// StartJoin records that the newest history page H1 has been applied; its
// newest row is taken from the most recent read of that page.
func (p *Projection) StartJoin() error {
	if p.join != joinWaiting || p.pageOpen || !p.h1Read {
		return ErrJoinProtocol
	}
	p.join = joinPending
	return nil
}

// LiveCaughtUp records that the forward reads from F, started after H1
// returned and all applied, reached HasMore=false; it joins the window.
func (p *Projection) LiveCaughtUp() error {
	switch p.join {
	case joinPending:
		p.join = joinDone
		return nil
	case joinDone:
		return nil
	default:
		return ErrJoinProtocol
	}
}

// HistoryOnly marks an archive window that has no live chain. Its order is
// final as soon as rows are applied; live deliveries are refused.
func (p *Projection) HistoryOnly() error {
	if p.join != joinWaiting || p.anyPage {
		return ErrJoinProtocol
	}
	p.join = joinHistoryOnly
	return nil
}

// Joined reports whether the window's order equals journal order, so its
// views may be published.
func (p *Projection) Joined() bool {
	return p.join == joinDone || p.join == joinHistoryOnly
}

// LiveSeed returns the seed for NewLiveCarry: the Ref of the row just before
// the frontier F when it is provable, otherwise "". It is provable only when
// the first forward read from F (started after H1 returned) produced no rows,
// because then nothing was appended between F and H1: the seed is the newest
// row of the most recent H1 read if it has a Ref, or StartOfHistory if that
// read was empty and reached the start of the visible history.
func (p *Projection) LiveSeed(firstForwardReadEmpty bool) string {
	if !firstForwardReadEmpty || !p.h1Read {
		return ""
	}
	if p.joinNewest == (OccurrenceKey{}) {
		if p.coverage == CoverageReachedStart && len(p.boundaries) == 0 {
			return StartOfHistory
		}
		return ""
	}
	return p.joinNewest.Ref
}
