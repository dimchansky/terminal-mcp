package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
)

// ReadRecord retrieves an exact persisted JSONL line using a stable RecordPos
// and its filename hint. A hash mismatch or changed acknowledged frontier is
// an explicit resync, not a request to guess the original input.
func (r *Reader) ReadRecord(ctx context.Context, ref RecordRef) ([]byte, bool, error) {
	if r == nil || r.writer == nil {
		return nil, false, ErrHistoryDisabled
	}
	pos := ref.Pos
	if pos.length == 0 || int64(pos.length) > maxFileBytes || pos.start < 0 ||
		pos.start > int64(^uint64(0)>>1)-int64(pos.length) {
		return nil, false, ErrInvalidCursor
	}
	state := cursorState{dir: Forward, segment: pos.segment, hint: ref.Hint,
		offset: pos.start + int64(pos.length), prev: pos.digest}
	seg, err := r.resolveCursor(ctx, state)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = seg.close() }()
	if pos.start+int64(pos.length) > seg.size {
		return nil, false, ErrInvalidCursor
	}
	if pos.start > 0 {
		var previous [1]byte
		if err := readExactAt(seg.file, previous[:], pos.start-1); err != nil {
			return nil, false, err
		}
		if previous[0] != '\n' {
			return nil, false, ErrInvalidCursor
		}
	}
	line := make([]byte, pos.length)
	if err := readExactAt(seg.file, line, pos.start); err != nil {
		return nil, false, err
	}
	if len(line) == 0 || line[len(line)-1] != '\n' ||
		bytes.IndexByte(line[:len(line)-1], '\n') >= 0 ||
		sha256.Sum256(line) != pos.digest || !json.Valid(line[:len(line)-1]) {
		return nil, false, ErrInvalidCursor
	}
	return line, pos.start < seg.baseline, nil
}
