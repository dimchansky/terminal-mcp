package audit

import (
	"context"
)

// WaitNextPage returns new history records after a Forward cursor, or blocks
// until a writer-acknowledged append, terminal audit Close, or context cancel.
// Its first read is immediate: already recorded history is never hidden just
// because the caller is interested in live continuation.
//
// No polling, background goroutine, network subscription or reader-held lock
// is involved. Each attempt uses ONE immutable observation for both its page
// boundary and Changed channel. A coalesced wake causes a fresh snapshot and
// replay from the original cursor, so multiple appends cannot be lost.
//
// Closed means the writer has terminated AND the observed backlog was fully
// returned (no further records can arrive from this writer). Changed is nil
// on terminal pages; otherwise it is the wake hint from the same observation
// that bounded the returned data. ReadPage remains a non-waiting API.
func (r *Reader) WaitNextPage(ctx context.Context, after Cursor, budget ReadBudget) (HistoryPage, error) {
	if r == nil || r.writer == nil {
		return HistoryPage{}, ErrHistoryDisabled
	}
	if err := ctx.Err(); err != nil {
		return HistoryPage{}, err
	}
	snapshot, err := r.captureSnapshot()
	if err != nil {
		return HistoryPage{}, err
	}
	return r.waitNextPageFromSnapshot(ctx, snapshot, after, budget, nil)
}

// waitNextPageFromSnapshot permits deterministic tests of a wake racing with
// the scan or the select. beforeWait is an optional synchronous test callback,
// invoked on EACH actual wait, after page scan has closed all reader
// descriptors and immediately before select. Production always passes nil.
func (r *Reader) waitNextPageFromSnapshot(ctx context.Context, snapshot journalSnapshot,
	after Cursor, budget ReadBudget, beforeWait func()) (HistoryPage, error) {
	for {
		if err := ctx.Err(); err != nil {
			return HistoryPage{}, err
		}
		page, err := r.readPageFromSnapshot(ctx, snapshot, after, Forward, budget)
		if err != nil {
			return HistoryPage{}, err
		}
		if page.HasMore && len(page.Records) == 0 && page.Next == after {
			// Never offer an empty page that makes no progress while claiming
			// more history. This would cause a busy caller to loop forever.
			return HistoryPage{}, ErrHistoryScanLimit
		}
		page.AtLiveEdge = !page.HasMore
		page.Closed = snapshot.observation.Closed && !page.HasMore
		if !snapshot.observation.Closed {
			page.Changed = snapshot.observation.Changed
		}
		if len(page.Records) > 0 || page.HasMore || page.Closed {
			return page, nil
		}

		// Empty page at the frozen edge. The capture's Changed channel was
		// obtained BEFORE scanning, so a write between scan and select closes
		// that exact channel, making select immediately retry from 'after'.
		if beforeWait != nil {
			beforeWait()
		}
		select {
		case <-ctx.Done():
			return HistoryPage{}, ctx.Err()
		case <-snapshot.observation.Changed:
		}
		snapshot, err = r.captureSnapshot()
		if err != nil {
			return HistoryPage{}, err
		}
		// ErrHistoryDisabled and real cursor errors are never reclassified
		// into wakeups. A failed append emits no Changed and cannot deliver
		// unaudited bytes; the next successful append retries from 'after'.
	}
}
