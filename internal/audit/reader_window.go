package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
)

// lineWindow reuses a single fixed 64 KiB window across all rows in one page,
// rather than performing a fresh 64 KiB ReadAt for each 100-byte log entry.
type lineWindow struct {
	file  *os.File
	start int64
	end   int64
	buf   [readerBlockBytes]byte
}

func (w *lineWindow) at(offset, limit int64) ([]byte, error) {
	if offset < w.start || offset >= w.end {
		base := offset / readerBlockBytes * readerBlockBytes
		n := int(min(int64(readerBlockBytes), limit-base))
		if n <= 0 {
			return nil, io.EOF
		}
		if err := readExactAt(w.file, w.buf[:n], base); err != nil {
			return nil, err
		}
		w.start, w.end = base, base+int64(n)
	}
	if offset >= w.end {
		return nil, io.ErrUnexpectedEOF
	}
	return w.buf[offset-w.start : w.end-w.start], nil
}

type forwardPageScanner struct {
	ctx    context.Context
	window lineWindow
	limit  int64
	offset int64
	failed error // Incomplete-row errors are sticky; never resume mid-line.
}

func newForwardPageScanner(ctx context.Context, f *os.File, offset, limit int64) (*forwardPageScanner, error) {
	if offset < 0 || offset > limit {
		return nil, ErrRecordBoundary
	}
	if offset > 0 {
		var previous [1]byte
		if err := readExactAt(f, previous[:], offset-1); err != nil {
			return nil, err
		}
		if previous[0] != '\n' {
			return nil, ErrRecordBoundary
		}
	}
	return &forwardPageScanner{ctx: ctx, window: lineWindow{file: f}, limit: limit, offset: offset}, nil
}

func (s *forwardPageScanner) next() ([]byte, int64, error) {
	from := s.offset
	if s.failed != nil {
		return nil, from, s.failed
	}
	if from == s.limit {
		return nil, from, io.EOF
	}
	line := make([]byte, 0, 256)
	oversize := false
	for s.offset < s.limit {
		if err := s.ctx.Err(); err != nil {
			return s.fail(from, err)
		}
		if s.offset-from >= maxCorruptScan {
			return s.fail(from, &RecordProblem{Cause: ErrRecordTooLarge, Start: from, End: s.offset})
		}
		available, err := s.window.at(s.offset, s.limit)
		if err != nil {
			return s.fail(from, err)
		}
		remaining := int(maxCorruptScan - (s.offset - from))
		if len(available) > remaining {
			available = available[:remaining]
		}
		count := len(available)
		if stop := bytes.IndexByte(available, '\n'); stop >= 0 {
			count = stop + 1
		}
		if !oversize && int64(len(line)+count) <= maxFileBytes {
			line = append(line, available[:count]...)
		} else {
			line = nil
			oversize = true
		}
		s.offset += int64(count)
		if available[count-1] == '\n' {
			if oversize {
				return nil, s.offset, &RecordProblem{Cause: ErrRecordTooLarge, Start: from, End: s.offset, Complete: true}
			}
			if !json.Valid(line[:len(line)-1]) {
				return line, s.offset, &RecordProblem{Cause: ErrCorruptRecord, Start: from, End: s.offset, Complete: true}
			}
			return line, s.offset, nil
		}
	}
	return s.fail(from, &RecordProblem{Cause: ErrCorruptRecord, Start: from, End: s.limit})
}

func (s *forwardPageScanner) fail(from int64, err error) ([]byte, int64, error) {
	s.offset = from
	s.failed = err
	return nil, from, err
}

type backwardPageScanner struct {
	ctx    context.Context
	window lineWindow
	limit  int64
	offset int64
	failed error // Incomplete failure must not advance to a new record.
}

func newBackwardPageScanner(ctx context.Context, f *os.File, offset, limit int64) (*backwardPageScanner, error) {
	if offset < 0 || offset > limit {
		return nil, ErrRecordBoundary
	}
	if offset > 0 {
		var previous [1]byte
		if err := readExactAt(f, previous[:], offset-1); err != nil {
			return nil, err
		}
		if previous[0] != '\n' {
			return nil, ErrRecordBoundary
		}
	}
	return &backwardPageScanner{ctx: ctx, window: lineWindow{file: f}, limit: limit, offset: offset}, nil
}

func (s *backwardPageScanner) previous() ([]byte, int64, error) {
	to := s.offset
	if s.failed != nil {
		return nil, to, s.failed
	}
	if to == 0 {
		return nil, 0, io.EOF
	}
	scan := to - 2
	for scan >= 0 {
		if err := s.ctx.Err(); err != nil {
			return s.fail(to, err)
		}
		if to-scan > maxCorruptScan {
			return s.fail(to, &RecordProblem{Cause: ErrRecordTooLarge, Start: -1, End: to})
		}
		_, err := s.window.at(scan, s.limit)
		if err != nil {
			return s.fail(to, err)
		}
		start := s.window.start
		prefix := int(scan-start) + 1
		if i := bytes.LastIndexByte(s.window.buf[:prefix], '\n'); i >= 0 {
			return s.finish(start+int64(i+1), to)
		}
		scan = start - 1
	}
	return s.finish(0, to)
}

func (s *backwardPageScanner) finish(from, to int64) ([]byte, int64, error) {
	if to-from > maxCorruptScan {
		return s.fail(to, &RecordProblem{Cause: ErrRecordTooLarge, Start: from, End: to})
	}
	if to-from > maxFileBytes {
		s.offset = from
		return nil, from, &RecordProblem{Cause: ErrRecordTooLarge, Start: from, End: to, Complete: true}
	}
	if err := s.ctx.Err(); err != nil {
		return s.fail(to, err)
	}
	var line []byte
	if from >= s.window.start && to <= s.window.end {
		line = bytes.Clone(s.window.buf[from-s.window.start : to-s.window.start])
	} else {
		line = make([]byte, int(to-from))
		if err := readExactAt(s.window.file, line, from); err != nil {
			return s.fail(to, err)
		}
	}
	s.offset = from
	if len(line) == 0 || line[len(line)-1] != '\n' || !json.Valid(line[:len(line)-1]) {
		return line, from, &RecordProblem{Cause: ErrCorruptRecord, Start: from, End: to, Complete: true}
	}
	return line, from, nil
}

func (s *backwardPageScanner) fail(to int64, err error) ([]byte, int64, error) {
	s.offset = to
	s.failed = err
	return nil, to, err
}
