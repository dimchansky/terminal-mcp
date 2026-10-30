package audit

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
)

// readSegmentPage reads only one previously opened and snapshot-validated
// segment. It never opens another file, observes the writer, or crosses an
// archive boundary: the outer history reader will own those transitions in
// #4d.2. offset must be an authenticated record boundary supplied by the
// caller (or 0 / seg.size for an initial read); the scanner additionally
// checks that its preceding byte is a newline.
//
// Forward rows are oldest-first, backward rows newest-first. The returned
// Next always identifies the final complete boundary for a nonempty segment,
// even if this call was already at the edge. An entirely empty segment has
// no fingerprint and returns an empty Next, which #4d.2 must handle before
// exposing a cursor. HasMore only describes this one segment.
//
// Scan bytes count source records, not the one predecessor proof that may be
// read when resuming backwards. AtLiveEdge and Changed are left unset: those
// are cross-segment/live concerns, not properties of a single file.
func readSegmentPage(ctx context.Context, seg *readerSegment, direction PageDirection, offset int64, budget ReadBudget) (HistoryPage, error) {
	return readSegmentPageBudget(ctx, seg, direction, offset, budget.normalize())
}

// readSegmentPageBudget accepts already-normalized limits. Unlike the entry
// point above, zero MaxBytes means all subsequent valid rows are Deferred;
// the aggregate history pager uses that after spending its whole data budget.
func readSegmentPageBudget(ctx context.Context, seg *readerSegment, direction PageDirection, offset int64, budget ReadBudget) (HistoryPage, error) {
	var page HistoryPage
	if err := ctx.Err(); err != nil {
		return page, err
	}
	if seg == nil || seg.file == nil || seg.size < 0 || seg.baseline < 0 ||
		seg.baseline > seg.size || offset < 0 || offset > seg.size {
		return page, ErrRecordBoundary
	}
	if direction != Forward && direction != Backward {
		return page, ErrInvalidCursor
	}
	if seg.size == 0 {
		return page, nil
	}
	if seg.key == ([sha256.Size]byte{}) {
		return page, ErrInvalidCursor
	}

	var forward *forwardPageScanner
	var backward *backwardPageScanner
	var err error
	switch direction {
	case Forward:
		forward, err = newForwardPageScanner(ctx, seg.file, offset, seg.size)
	case Backward:
		backward, err = newBackwardPageScanner(ctx, seg.file, offset, seg.size)
	}
	if err != nil {
		if direction == Backward && offset == seg.size && seg.baseline == seg.size &&
			errors.Is(err, ErrRecordBoundary) {
			// An unverified historical archive may end with an incomplete
			// tail. Report that actual corruption instead of blaming the
			// caller's valid end-of-file starting offset. Never skip it.
			_, problem, tailErr := historicalTail(ctx, seg.file, seg.size)
			if tailErr != nil {
				return HistoryPage{}, tailErr
			}
			if problem != nil {
				return HistoryPage{}, problem
			}
		}
		return HistoryPage{}, err
	}

	var returnedBytes int64
	var lastRaw []byte
	// finish is called at most once, including for an already reached edge
	// or after an incomplete row following at least one good record.
	finish := func() (HistoryPage, error) {
		var proof [sha256.Size]byte
		if direction == Forward && lastRaw != nil {
			proof = cursorProof(lastRaw)
		} else {
			var proofErr error
			proof, proofErr = boundaryProof(ctx, seg.file, offset, seg.size)
			if proofErr != nil {
				// A damaged predecessor longer than the bounded proof limit
				// must not make ALL newer rows disappear. When possible,
				// give back the oldest valid row: the cursor now points
				// just after it and its already-read bytes prove that edge.
				// The next page will encounter the same damaged predecessor.
				if direction != Backward ||
					(!errors.Is(proofErr, ErrRecordTooLarge) && !errors.Is(proofErr, ErrRecordBoundary)) ||
					len(page.Records) < 2 || lastRaw == nil ||
					page.Records[len(page.Records)-1].Problem != nil {
					return HistoryPage{}, proofErr
				}
				oldest := page.Records[len(page.Records)-1]
				page.Records = page.Records[:len(page.Records)-1]
				page.ScannedBytes -= oldest.Size
				offset += oldest.Size
				proof = cursorProof(lastRaw)
			}
		}
		page.Next = newPageCursor(direction, seg.key, seg.archive, offset, proof)
		page.HasMore = (direction == Forward && offset < seg.size) ||
			(direction == Backward && offset > 0)
		return page, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return HistoryPage{}, err
		}
		if (direction == Forward && offset == seg.size) ||
			(direction == Backward && offset == 0) ||
			len(page.Records) >= budget.MaxRecords ||
			page.ScannedBytes >= budget.MaxScanBytes {
			return finish()
		}

		var line []byte
		var from, after int64
		var lineErr error
		switch direction {
		case Forward:
			from = offset
			line, after, lineErr = forward.next()
		case Backward:
			after = offset
			line, from, lineErr = backward.previous()
		}

		var problem *RecordProblem
		if lineErr != nil {
			if errors.Is(lineErr, io.EOF) {
				return HistoryPage{}, io.ErrUnexpectedEOF
			}
			if !errors.As(lineErr, &problem) || !problem.Complete {
				// Do not discard earlier valid records simply because a later
				// tail is incomplete. The next page encounters the SAME bad
				// boundary and reports the error without skipping it.
				if problem != nil && len(page.Records) > 0 && !problem.Complete {
					return finish()
				}
				return HistoryPage{}, lineErr
			}
		}
		length := after - from
		if length <= 0 || after > seg.size {
			return HistoryPage{}, ErrRecordBoundary
		}

		record := HistoryRecord{Size: length, Historical: from < seg.baseline}
		if problem != nil {
			issue := *problem
			record.Problem = &issue
		}
		// Complete malformed JSONL has verifiable raw bytes even though it
		// is not a valid event. Oversized/partial rows get no detail ref.
		if (problem == nil || errors.Is(problem, ErrCorruptRecord)) &&
			len(line) > 0 && int64(len(line)) == length {
			record.Ref = RecordRef{
				Pos:  positionForRecord(seg.key, from, line),
				Hint: seg.archive,
			}
		}
		if problem == nil {
			// The scanners return owned byte slices; no browser-facing record
			// can be overwritten by the next 64 KiB window read.
			if returnedBytes+length <= budget.MaxBytes {
				record.Data = line
				returnedBytes += length
			} else {
				record.Deferred = true
			}
		}
		page.Records = append(page.Records, record)
		page.ScannedBytes += length

		if direction == Forward {
			offset = after
		} else {
			offset = from
		}
		lastRaw = line
	}
}
