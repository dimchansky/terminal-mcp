package dashboard

import (
	"context"
	"errors"
	"sync"

	"terminalmcp/internal/activity"
	"terminalmcp/internal/audit"
)

// indexLimit bounds in-flight effects and open requests the index keeps.
const indexLimit = 1024

// Index keeps the server-wide in-flight index caught up with the journal.
// Catch-up is lazy (on viewer requests), bounded per call and single-flight;
// it never blocks the audit writer. Lock order: a window's lock, then the
// index lock; never the reverse.
type Index struct {
	reader *audit.Reader
	status func() audit.Status

	flight sync.Mutex // One catch-up at a time; others wait for its result.

	mu           sync.Mutex
	idx          *activity.InFlightIndex
	degradedSeen bool // Latched until catch-up reaches the frontier.
}

// NewIndex starts the index at f0, the frontier captured after server_start
// and before MCP admission opened; ok is false if it could not be captured.
// status reports audit health (nil means always ok).
func NewIndex(reader *audit.Reader, serverID string, f0 audit.Cursor, ok bool, status func() audit.Status) *Index {
	if reader == nil {
		ok = false
	}
	return &Index{reader: reader, status: status,
		idx: activity.NewInFlightIndex(serverID, string(f0), ok, indexLimit)}
}

// CatchUp reads at most maxPages forward pages from the index cursor.
func (x *Index) CatchUp(ctx context.Context, maxPages int) error {
	x.flight.Lock()
	defer x.flight.Unlock()
	x.mu.Lock()
	unknown := x.idx.Status() == activity.IndexUnknown
	cursor := audit.Cursor(x.idx.Cursor())
	x.mu.Unlock()
	if unknown {
		return nil
	}
	if x.status != nil && x.status().State == "degraded" {
		x.mu.Lock()
		x.degradedSeen = true
		x.mu.Unlock()
	}
	for i := 0; i < maxPages; i++ {
		page, err := x.reader.ReadPage(ctx, cursor, audit.Forward, pageBudget)
		if err == nil {
			var rows []activity.IndexRow
			if rows, err = x.rows(ctx, page); err == nil {
				x.mu.Lock()
				err = x.idx.ApplyPage(string(cursor), rows, string(page.Next), page.HasMore)
				x.mu.Unlock()
			}
		}
		if errors.Is(err, audit.ErrHistoryGap) || errors.Is(err, audit.ErrInvalidCursor) {
			x.mu.Lock()
			x.idx.Fail() // F0 cannot be re-derived until restart.
			x.mu.Unlock()
			return nil
		}
		if err != nil {
			return err
		}
		cursor = page.Next
		if !page.HasMore {
			x.mu.Lock()
			if x.degradedSeen {
				x.idx.ObserveAuditDegraded()
				x.degradedSeen = false
			}
			x.mu.Unlock()
			return nil
		}
	}
	return nil
}

func (x *Index) rows(ctx context.Context, page audit.HistoryPage) ([]activity.IndexRow, error) {
	rows := make([]activity.IndexRow, len(page.Records))
	for i, rec := range page.Records {
		if rec.Ref != (audit.RecordRef{}) {
			rows[i].Key = activity.OccurrenceKey{Ref: rec.Ref.String()}
		}
		switch {
		case rec.Problem != nil:
			rows[i].Problem = rec.Problem.Error()
		case rec.Deferred:
			data, _, err := x.reader.ReadRecord(ctx, rec.Ref)
			if err != nil {
				return nil, err
			}
			rows[i].Raw = data
		default:
			rows[i].Raw = rec.Data
		}
	}
	return rows, nil
}

// Sync copies possibly-unrecorded flags into a window's projection. Call it
// with the window lock held.
func (x *Index) Sync(p *activity.Projection) {
	x.mu.Lock()
	defer x.mu.Unlock()
	p.SyncIndex(x.idx)
}

// IndexInfo is the index state for envelopes.
type IndexInfo struct {
	Status  string                    `json:"status"`
	Dropped int                       `json:"dropped,omitempty"`
	Skipped int                       `json:"skipped,omitempty"`
	Outside []activity.InFlightEffect `json:"in_flight_outside"`
}

// Info reports the index state and the in-flight effects the window does not
// contain. Call it with the window lock held.
func (x *Index) Info(p *activity.Projection) IndexInfo {
	x.mu.Lock()
	defer x.mu.Unlock()
	info := IndexInfo{Dropped: x.idx.Dropped(), Skipped: x.idx.Skipped(), Outside: p.InFlightOutside(x.idx)}
	switch x.idx.Status() {
	case activity.IndexUnknown:
		info.Status = "unknown"
	case activity.IndexCatchingUp:
		info.Status = "catching_up"
	case activity.IndexReady:
		info.Status = "ready"
	case activity.IndexIncomplete:
		info.Status = "incomplete"
	}
	if info.Outside == nil {
		info.Outside = []activity.InFlightEffect{}
	}
	return info
}
