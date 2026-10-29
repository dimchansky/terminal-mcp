package audit

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// FrontierCursor returns the boundary immediately after the newest retained
// record visible to ONE immutable writer observation. It never reads an
// unacknowledged append, even if a full JSONL line is physically present.
//
// In a quiescent journal, this cursor is byte-for-byte the same as the final
// Next of a complete Forward ReadPage walk. If no complete record exists it
// returns an empty cursor; a later live consumer must then replay from the
// beginning. An incomplete historical tail is an explicit error, not a
// silently skipped record. This method does NOT wait for new events.
func (r *Reader) FrontierCursor(ctx context.Context) (Cursor, error) {
	if r == nil || r.writer == nil {
		return "", ErrHistoryDisabled
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	snapshot, err := r.captureSnapshot()
	if err != nil {
		return "", err
	}
	return r.frontierCursorFromSnapshot(ctx, snapshot)
}

// frontierCursorFromSnapshot accepts a pre-captured observation so #4d.3b can
// obtain its corresponding Changed channel and frontier from the SAME writer
// generation. This function never calls Observe or changes the snapshot.
//
// Logical source ordering is active > writer-sealed > preexisting history.
// A later append or rotation may increase the physical size or rename the
// active file to snapshot.NextArchive; openArchiveSource verifies the opened
// file's identity and clamps reads to the captured acknowledged byte limit.
func (r *Reader) frontierCursorFromSnapshot(ctx context.Context, snapshot journalSnapshot) (Cursor, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	state := snapshot.observation
	if state.Disabled || state.NextArchive == 0 {
		return "", ErrHistoryDisabled
	}
	if state.HasActive && state.Active.Size > 0 {
		return r.frontierFromSource(ctx, snapshot, archiveSource{active: true})
	}
	// All writer-sealed segments are already present in the immutable
	// observation. Do not reopen empty ones just to discover their size.
	for i := len(state.Sealed) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if state.Sealed[i].Size == 0 {
			continue
		}
		return r.frontierFromSource(ctx, snapshot, archiveSource{suffix: state.Sealed[i].Archive})
	}
	// Only when there is no recorded current-process data do we enumerate
	// historical archives; their complete newline-terminated data is visible,
	// but it is not retroactively certified as admitted by this writer.
	sources, err := r.frozenSources(ctx, snapshot, true)
	if err != nil {
		return "", err
	}
	first := state.NextArchive - uint64(len(state.Sealed))
	for i := len(sources) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		source := sources[i]
		if source.active || source.suffix >= first {
			continue
		}
		seg, err := r.openArchiveSource(ctx, snapshot, source)
		if err != nil {
			return "", frontierSourceError(err) // No silent rewind or raw internal errors.
		}
		if seg.size == 0 {
			if err := seg.close(); err != nil {
				return "", err
			}
			continue
		}
		return frontierFromOpenedSegment(ctx, seg)
	}
	return "", nil
}

func (r *Reader) frontierFromSource(ctx context.Context, snapshot journalSnapshot, source archiveSource) (Cursor, error) {
	seg, err := r.openArchiveSource(ctx, snapshot, source)
	if err != nil {
		return "", frontierSourceError(err)
	}
	if seg.size == 0 {
		_ = seg.close()
		return "", ErrHistoryGap // Frozen source was nonempty when selected.
	}
	return frontierFromOpenedSegment(ctx, seg)
}

// No owner-provided cursor is being parsed here. Missing or replaced
// selected journal sources are history gaps, not invalid client cursors.
// Do not wrap the internal ErrInvalidCursor: callers must see only the
// public ErrHistoryGap category while retaining diagnostic text.
func frontierSourceError(err error) error {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, errOutsideSnapshot) ||
		errors.Is(err, ErrInvalidCursor) {
		return fmt.Errorf("%w: latest journal source unavailable (%v)", ErrHistoryGap, err)
	}
	return err
}

func frontierFromOpenedSegment(ctx context.Context, seg *readerSegment) (Cursor, error) {
	defer func() { _ = seg.close() }()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if seg.baseline == seg.size {
		// Legacy/history is not guaranteed to have been admitted. Never
		// claim its incomplete tail is a committed, resumable record.
		end, problem, err := historicalTail(ctx, seg.file, seg.size)
		if err != nil {
			return "", err
		}
		if problem != nil {
			return "", problem
		}
		if end != seg.size {
			return "", ErrRecordBoundary
		}
	}
	proof, err := boundaryProof(ctx, seg.file, seg.size, seg.size)
	if err != nil {
		return "", err // Propagate disk/cancellation/corruption distinctly.
	}
	return newPageCursor(Forward, seg.key, seg.archive, seg.size, proof), nil
}
