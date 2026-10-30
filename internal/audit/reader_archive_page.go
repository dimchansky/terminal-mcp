package audit

import (
	"context"
	"errors"
)

// ReadPage walks archived JSONL files and the frozen active journal in source
// order, without mixing in appends or rotations admitted after the call's
// initial snapshot. Forward is oldest-first; Backward is newest-first.
//
// Direct historical ReadPage leaves live/terminal fields unset; WaitNextPage
// sets them using the same snapshot that bounds its scan. The cursor carries
// a stable source key, validated preceding bytes, and the eventual archive number for active
// records (so a long-lived cursor survives arbitrarily many normal rotations).
// Entirely empty files are skipped inside this call: they cannot create a
// legitimate RecordPos or a progress-making page cursor. Historical archives
// are discovered afresh on each request; deletion BETWEEN requests cannot
// be proven from a suffix gap alone. Missing sealed writer-owned archives
// still produce explicit errors when encountered.
func (r *Reader) ReadPage(ctx context.Context, cursor Cursor, direction PageDirection, budget ReadBudget) (HistoryPage, error) {
	var page HistoryPage
	if r == nil || r.writer == nil {
		return page, ErrHistoryDisabled
	}
	if err := ctx.Err(); err != nil {
		return page, err
	}
	if direction != Forward && direction != Backward {
		return page, ErrInvalidCursor
	}
	snapshot, err := r.captureSnapshot()
	if err != nil {
		return page, err
	}
	return r.readPageFromSnapshot(ctx, snapshot, cursor, direction, budget)
}

// readPageFromSnapshot uses the same immutable writer boundary throughout
// the scan, including its filename relocations. It NEVER calls Observe; the
// caller may safely wait on snapshot.observation.Changed after this returns.
func (r *Reader) readPageFromSnapshot(ctx context.Context, snapshot journalSnapshot,
	cursor Cursor, direction PageDirection, budget ReadBudget) (HistoryPage, error) {
	var page HistoryPage
	if err := ctx.Err(); err != nil {
		return page, err
	}
	if direction != Forward && direction != Backward {
		return page, ErrInvalidCursor
	}
	budget = budget.normalize()

	var err error
	var state cursorState
	if cursor != "" {
		state, err = decodePageCursor(cursor)
		if err != nil || state.dir != direction {
			return HistoryPage{}, ErrInvalidCursor
		}
		page.Next = cursor
	}
	// Do not scan the entire archive directory just to read the busy tail.
	needHistorical := direction == Forward && cursor == ""
	if cursor != "" && state.hint > 0 {
		first := snapshot.observation.NextArchive - uint64(len(snapshot.observation.Sealed))
		needHistorical = state.hint < first
	}
	sources, err := r.frozenSources(ctx, snapshot, needHistorical)
	if err != nil {
		return HistoryPage{}, err
	}
	reloadHistorical := func(index *int) error {
		if needHistorical {
			return nil
		}
		expanded, err := r.frozenSources(ctx, snapshot, true)
		if err != nil {
			return err
		}
		*index += len(expanded) - len(sources) // Preserve logical source.
		sources, needHistorical = expanded, true
		return nil
	}

	index, offset := 0, int64(0)
	if direction == Backward {
		index, offset = len(sources)-1, -1 // -1 selects the next segment's frozen end.
		if index < 0 && !needHistorical {
			if err := reloadHistorical(&index); err != nil {
				return HistoryPage{}, err
			}
			index = len(sources) - 1
		}
	}
	var matched *readerSegment
	if cursor != "" {
		// A missing historical archive is a gap, even when another archive
		// happens to have byte-identical contents. Check before fallback.
		if needHistorical && state.hint > 0 {
			first := snapshot.observation.NextArchive - uint64(len(snapshot.observation.Sealed))
			if state.hint < first {
				present := false
				for _, source := range sources {
					if !source.active && source.suffix == state.hint {
						present = true
						break
					}
				}
				if !present {
					return HistoryPage{}, ErrHistoryGap
				}
			}
		}
		matched, index, err = r.frozenCursorSource(ctx, snapshot, state, sources, needHistorical)
		if errors.Is(err, errNeedHistorical) ||
			(errors.Is(err, ErrInvalidCursor) && !needHistorical && state.hint == 0) {
			if loadErr := reloadHistorical(new(int)); loadErr != nil {
				return HistoryPage{}, loadErr
			}
			matched, index, err = r.frozenCursorSource(ctx, snapshot, state, sources, needHistorical)
		}
		if err != nil {
			return HistoryPage{}, err
		}
		if matched == nil {
			return HistoryPage{}, ErrInvalidCursor
		}
		offset = state.offset
		// Reuse this validated handle for the first page scan. A historical
		// pathname might otherwise change between proof and reopening.
	}
	defer func() {
		if matched != nil {
			_ = matched.close()
		}
	}()
	var openedNonempty int
	var returnedBytes int64
	transitionFailure := func(err error) (HistoryPage, error) {
		if err != nil && len(page.Records) > 0 && ctx.Err() == nil {
			page.HasMore = true
			return page, nil // Preserve records/cursor; next request exposes gap.
		}
		return HistoryPage{}, err
	}

	// move navigates the logical sequence; backward loads historical names
	// only when it reaches the writer's oldest sealed archive.
	move := func() error {
		if direction == Forward {
			index++
			return nil
		}
		if index == 0 && !needHistorical {
			if err := reloadHistorical(&index); err != nil {
				return err
			}
		}
		index--
		return nil
	}
	for index >= 0 && index < len(sources) {
		if err := ctx.Err(); err != nil {
			return HistoryPage{}, err
		}
		var seg *readerSegment
		if matched != nil {
			seg, matched = matched, nil
		} else {
			seg, err = r.openArchiveSource(ctx, snapshot, sources[index])
			if err != nil {
				return transitionFailure(err)
			}
		}

		if seg.size == 0 {
			_ = seg.close()
			if err := move(); err != nil {
				return transitionFailure(err)
			}
			if direction == Backward {
				offset = -1
			} else {
				offset = 0
			}
			continue
		}
		openedNonempty++
		// A pathological chain of nonempty files must not hold one page
		// request forever. If a real cursor exists, return resumable progress.
		if openedNonempty > maxPageNonemptySegments {
			_ = seg.close()
			if len(page.Records) > 0 {
				page.HasMore = true
				return page, nil
			}
			return HistoryPage{}, ErrHistoryScanLimit
		}
		if direction == Backward && offset == -1 {
			offset = seg.size // Frozen end of newly entered segment.
		}
		// A page with HasMore must move its cursor. With zero rows the
		// aggregate must scan its first complete record before returning.
		exhausted := len(page.Records) > 0 &&
			(len(page.Records) >= budget.MaxRecords || page.ScannedBytes >= budget.MaxScanBytes)
		if exhausted {
			page.HasMore = true // Current segment contains at least one row.
			_ = seg.close()
			return page, nil
		}
		remaining := ReadBudget{
			MaxRecords: budget.MaxRecords - len(page.Records),
			MaxBytes:   max(int64(0), budget.MaxBytes-returnedBytes), // 0 => only deferred.
			// A soft scan budget must still permit the first complete row
			// when the aggregate page has no progress yet.
			MaxScanBytes: max(int64(1), budget.MaxScanBytes-page.ScannedBytes),
		}
		part, partErr := readSegmentPageBudget(ctx, seg, direction, offset, remaining)
		_ = seg.close()
		if partErr != nil {
			// A bad source encountered AFTER completed rows must not erase
			// the previous segment's usable history or its cursor.
			if len(page.Records) > 0 && !errors.Is(partErr, context.Canceled) &&
				!errors.Is(partErr, context.DeadlineExceeded) {
				page.HasMore = true
				return page, nil
			}
			return HistoryPage{}, partErr
		}
		page.ScannedBytes += part.ScannedBytes
		if len(part.Records) > 0 {
			page.Records = append(page.Records, part.Records...)
			page.Next = part.Next
			for _, record := range part.Records {
				returnedBytes += int64(len(record.Data))
			}
		}
		if part.HasMore {
			page.HasMore = true
			return page, nil
		}
		if err := move(); err != nil {
			return transitionFailure(err)
		}
		if direction == Backward {
			offset = -1
		} else {
			offset = 0
		}
	}
	// No remaining nonempty source exists within this captured observation.
	page.HasMore = false
	return page, nil
}
