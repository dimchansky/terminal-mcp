package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
)

// Only the existing journal's maximum record size is supported. A source
// segment can contain many records, but no single input line may exceed 4 MiB.
const readerBlockBytes = 64 << 10

var (
	ErrCorruptRecord  = errors.New("audit history contains an invalid record")
	ErrRecordTooLarge = errors.New("audit history record exceeds the 4 MiB limit")
	ErrRecordBoundary = errors.New("audit history cursor is not at a record boundary")
)

// readRecordForward returns the byte-exact newline-terminated JSONL record
// starting at offset. limit MUST be the writer's acknowledged size for a
// current segment, or the validated historical file size for old archives.
// It never looks beyond limit, even if the physical file has more bytes.
func readRecordForward(ctx context.Context, f *os.File, offset, limit int64) ([]byte, int64, error) {
	if offset < 0 || limit < offset {
		return nil, offset, ErrRecordBoundary
	}
	if offset == limit {
		return nil, offset, io.EOF
	}
	if offset > 0 {
		var previous [1]byte
		if err := readExactAt(f, previous[:], offset-1); err != nil {
			return nil, offset, err
		}
		if previous[0] != '\n' {
			return nil, offset, ErrRecordBoundary
		}
	}

	line := make([]byte, 0, int(min(int64(readerBlockBytes), limit-offset)))
	var block [readerBlockBytes]byte
	for at := offset; at < limit; {
		if err := ctx.Err(); err != nil {
			return nil, offset, err
		}
		remaining := int64(maxFileBytes) - int64(len(line))
		if remaining <= 0 {
			next, complete, err := findNextNewline(ctx, f, at, limit)
			if err != nil {
				return nil, offset, err
			}
			return nil, next, &RecordProblem{Cause: ErrRecordTooLarge, Start: offset, End: next, Complete: complete}
		}
		count := int(min(int64(len(block)), min(limit-at, remaining)))
		if err := readExactAt(f, block[:count], at); err != nil {
			return nil, offset, err
		}
		n := count
		if stop := bytes.IndexByte(block[:n], '\n'); stop >= 0 {
			line = append(line, block[:stop+1]...)
			if !json.Valid(line[:len(line)-1]) {
				next := offset + int64(len(line))
				return nil, next, &RecordProblem{Cause: ErrCorruptRecord, Start: offset, End: next, Complete: true}
			}
			return line, offset + int64(len(line)), nil
		}
		line = append(line, block[:n]...)
		at += int64(n)
	}
	return nil, offset, &RecordProblem{Cause: ErrCorruptRecord, Start: offset, End: limit}
}

// readRecordBefore finds the complete record ending at boundary, and returns
// its exact bytes and starting offset. Reverse scanning uses bounded blocks
// and allocates the record only once after locating its preceding newline.
func readRecordBefore(ctx context.Context, f *os.File, boundary, limit int64) ([]byte, int64, error) {
	if boundary < 0 || boundary > limit {
		return nil, boundary, ErrRecordBoundary
	}
	if boundary == 0 {
		return nil, 0, io.EOF
	}
	var last [1]byte
	if err := readExactAt(f, last[:], boundary-1); err != nil {
		return nil, boundary, err
	}
	if last[0] != '\n' {
		return nil, boundary, ErrRecordBoundary
	}

	var block [readerBlockBytes]byte
	end := boundary - 1 // Exclude the record's trailing newline.
	for {
		if err := ctx.Err(); err != nil {
			return nil, boundary, err
		}
		start := max(int64(0), end-readerBlockBytes)
		n := int(end - start)
		if n > 0 {
			if err := readExactAt(f, block[:n], start); err != nil {
				return nil, boundary, err
			}
			if index := bytes.LastIndexByte(block[:n], '\n'); index >= 0 {
				lineStart := start + int64(index+1)
				return readRecordSpan(ctx, f, lineStart, boundary)
			}
		}
		if start == 0 {
			return readRecordSpan(ctx, f, 0, boundary)
		}
		if boundary-start > maxCorruptScan {
			return nil, boundary, &RecordProblem{Cause: ErrRecordTooLarge, End: boundary}
		}
		end = start
	}
}

func readRecordSpan(ctx context.Context, f *os.File, from, to int64) ([]byte, int64, error) {
	if to <= from {
		return nil, from, &RecordProblem{Cause: ErrCorruptRecord, Start: from, End: to, Complete: true}
	}
	if to-from > maxFileBytes {
		return nil, from, &RecordProblem{Cause: ErrRecordTooLarge, Start: from, End: to, Complete: true}
	}
	if err := ctx.Err(); err != nil {
		return nil, from, err
	}
	line := make([]byte, int(to-from))
	if err := readExactAt(f, line, from); err != nil {
		return nil, from, err
	}
	if line[len(line)-1] != '\n' || !json.Valid(line[:len(line)-1]) {
		return nil, from, &RecordProblem{Cause: ErrCorruptRecord, Start: from, End: to, Complete: true}
	}
	return line, from, nil
}
