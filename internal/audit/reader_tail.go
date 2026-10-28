package audit

import (
	"bytes"
	"context"
	"os"
)

// historicalTail reports an incomplete trailing JSONL fragment on an old
// archive, without rewriting or silently dropping evidence. A current
// acknowledged writer range is never repaired or skipped by the reader.
// The returned validEnd is a newline boundary and can safely be resumed.
func historicalTail(ctx context.Context, f *os.File, limit int64) (validEnd int64, problem *RecordProblem, err error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if limit < 0 {
		return 0, nil, ErrRecordBoundary
	}
	if limit == 0 {
		return 0, nil, nil
	}
	var last [1]byte
	if err := readExactAt(f, last[:], limit-1); err != nil {
		return 0, nil, err
	}
	if last[0] == '\n' {
		return limit, nil, nil
	}
	var block [readerBlockBytes]byte
	for end := limit; end > 0; {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		start := max(int64(0), end-readerBlockBytes)
		if limit-start > maxCorruptScan {
			// The prefix might contain valid records, but the exact tail
			// boundary is unknown. Expose a non-skippable problem and fail
			// rather than silently treating it as complete history.
			return 0, &RecordProblem{Cause: ErrRecordTooLarge,
				Start: limit - maxCorruptScan, End: limit}, ErrRecordTooLarge
		}
		n := int(end - start)
		if err := readExactAt(f, block[:n], start); err != nil {
			return 0, nil, err
		}
		if i := bytes.LastIndexByte(block[:n], '\n'); i >= 0 {
			validEnd := start + int64(i+1)
			return validEnd, &RecordProblem{Cause: ErrCorruptRecord,
				Start: validEnd, End: limit}, nil
		}
		end = start
	}
	return 0, &RecordProblem{Cause: ErrCorruptRecord, Start: 0, End: limit}, nil
}
