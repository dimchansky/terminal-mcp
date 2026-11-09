package activity

import "strconv"

// Row is what key assignment needs from one reader row: its Ref, or for a
// ref-less damaged row the byte range the reader reported.
type Row struct {
	Ref        string
	Start, End int64
}

// HistoryCarry assigns occurrence keys on the history chain. Rows arrive
// newest first, so a ref-less row's older neighbour is not known when it is
// delivered: it gets a provisional key, resolved once the next older row with
// a Ref (or the start of visible history) arrives.
//
// Only rows of one contiguous backward read are neighbours. Retried and
// re-read pages restart elsewhere, so every page is bracketed by BeginPage
// with the cursor it reads from and EndPage with its Next cursor: rows still
// pending at the end of a page wait for the page that continues from that
// cursor. Call BeginPage only for a page that was read successfully and close
// every page with EndPage or ReachedStart. Rows waiting at a cursor that is
// never continued (the chain stopped at a gap, or a retry ended elsewhere)
// stay provisional. Use one HistoryCarry per window and discard it when the
// window's history chain stops (coverage end or raised floor): a resolution
// applied after an eviction may name an evicted provisional row and fail with
// ErrResolve, which means the window must be rebuilt.
type HistoryCarry struct {
	pending []OccurrenceKey // Provisional keys of the current page, newest first.
	waiting map[string][]OccurrenceKey
	seq     int
}

// BeginPage starts a backward page read from cursor ("" reads from the newest
// record). Rows left pending by an earlier page that ended at this cursor
// continue here.
func (c *HistoryCarry) BeginPage(cursor string) {
	c.pending = nil
	if cursor != "" {
		c.pending = c.waiting[cursor]
		delete(c.waiting, cursor)
	}
}

// EndPage ends the current page; next is its Next cursor. Pending rows wait
// for a page read from next. If a retried page ends at the same cursor, its
// rows replace the earlier pending ones, which then stay provisional and are
// shown as possible repeats. After a page with HasMore=false call
// ReachedStart instead.
func (c *HistoryCarry) EndPage(next string) {
	if len(c.pending) == 0 {
		return
	}
	if c.waiting == nil {
		c.waiting = make(map[string][]OccurrenceKey)
	}
	c.waiting[next] = c.pending
	c.pending = nil
}

// Key returns the key for the next row of the current page and any
// provisional keys that row resolves. Apply the row first, then the
// resolutions.
func (c *HistoryCarry) Key(row Row) (OccurrenceKey, []Resolution) {
	if row.Ref == "" {
		c.seq++
		key := OccurrenceKey{Weak: WeakKey{Start: row.Start, End: row.End, Provisional: "h" + strconv.Itoa(c.seq)}}
		c.pending = append(c.pending, key)
		return key, nil
	}
	return OccurrenceKey{Ref: row.Ref}, c.resolve(row.Ref)
}

// ReachedStart ends a page that returned the oldest visible record
// (HasMore=false) and resolves its pending rows. A page that stops at a gap or
// coverage limit must end with EndPage instead; its rows stay provisional.
func (c *HistoryCarry) ReachedStart() []Resolution {
	return c.resolve(StartOfHistory)
}

func (c *HistoryCarry) resolve(older string) []Resolution {
	if len(c.pending) == 0 {
		return nil
	}
	out := make([]Resolution, 0, len(c.pending))
	for i, key := range c.pending {
		final := key
		final.Weak.Provisional = ""
		final.Weak.OlderRef = older
		final.Weak.Between = len(c.pending) - 1 - i
		out = append(out, Resolution{Provisional: key, Final: final})
	}
	c.pending = nil
	return out
}

// LiveCarry assigns occurrence keys on the live chain, where rows arrive
// oldest first and the older neighbour of a ref-less row is the last row with
// a Ref. It is a small value: store a copy with every issued live cursor and
// resume from that copy on reconnect.
type LiveCarry struct {
	older   string
	between int
	seq     int
}

// NewLiveCarry starts the live chain. seed is the Ref of the row immediately
// before the frontier F, StartOfHistory when F is at the start of the visible
// history, or "" when unknown. The adapter knows it exactly only when nothing
// was appended between F and the history snapshot (the first forward read
// from F returned no rows): then it is the history snapshot's newest row, if
// that row has a Ref, or StartOfHistory if the snapshot is empty and complete.
// With an unknown seed, ref-less rows before the first row with a Ref stay
// provisional (possible repeats); the adapter may derive more seeds from the
// overlap, but this conservative rule never invents a neighbour.
func NewLiveCarry(seed string) LiveCarry {
	return LiveCarry{older: seed}
}

// Key returns the key for the next live row.
func (c *LiveCarry) Key(row Row) OccurrenceKey {
	if row.Ref != "" {
		c.older, c.between = row.Ref, 0
		return OccurrenceKey{Ref: row.Ref}
	}
	if c.older == "" {
		c.seq++
		return OccurrenceKey{Weak: WeakKey{Start: row.Start, End: row.End, Provisional: "l" + strconv.Itoa(c.seq)}}
	}
	key := OccurrenceKey{Weak: WeakKey{OlderRef: c.older, Between: c.between, Start: row.Start, End: row.End}}
	c.between++
	return key
}
