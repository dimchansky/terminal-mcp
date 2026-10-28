package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// RecordPos is an equality-comparable, rotation-independent record identity.
// Its private encoding identifies both the first complete line of a segment
// and the exact target record. It is not a proof of tamper resistance.
type RecordPos struct {
	segment [sha256.Size]byte
	start   int64
	length  uint32
	digest  [sha256.Size]byte
}

// Cursor identifies an acknowledged boundary for forward or backward pages.
// It is opaque to clients; callers must not extract names or offsets from it.
type Cursor string

type PageDirection uint8

const (
	Forward  PageDirection = 1
	Backward PageDirection = 2
)

const (
	cursorVersion = byte(1)
	cursorBytes   = 1 + 1 + sha256.Size + 8 + 8 + sha256.Size
	positionBytes = 1 + sha256.Size + 8 + 4 + sha256.Size
)

var ErrInvalidCursor = errors.New("invalid or stale audit history cursor")

type cursorState struct {
	dir     PageDirection
	segment [sha256.Size]byte
	hint    uint64 // Initial filename hint: 0 for active, otherwise suffix.
	offset  int64
	prev    [sha256.Size]byte
}

// segmentFingerprint is a stable identity for a live file and its renamed
// archive. It hashes the *raw* first complete line; malformed JSON must not
// make other valid cursor candidates unreachable. An unclosed/corrupt first
// line gets a domain-separated hash of at most 4 MiB of its raw prefix.
// These hashes identify bytes, not authenticated writer admission.
func segmentFingerprint(ctx context.Context, f *os.File, maxCommitted int64) ([sha256.Size]byte, error) {
	if maxCommitted <= 0 {
		return [sha256.Size]byte{}, io.EOF
	}
	prefix := make([]byte, 0, int(min(int64(readerBlockBytes), maxCommitted)))
	var block [readerBlockBytes]byte
	for offset := int64(0); offset < maxCommitted && int64(len(prefix)) < maxFileBytes; {
		if err := ctx.Err(); err != nil {
			return [sha256.Size]byte{}, err
		}
		n := int(min(int64(len(block)), min(maxCommitted-offset, maxFileBytes-int64(len(prefix)))))
		if err := readExactAt(f, block[:n], offset); err != nil {
			return [sha256.Size]byte{}, err
		}
		got := n
		if boundary := bytes.IndexByte(block[:got], '\n'); boundary >= 0 {
			prefix = append(prefix, block[:boundary+1]...)
			return sha256.Sum256(append([]byte{0}, prefix...)), nil
		}
		prefix = append(prefix, block[:got]...)
		offset += int64(got)
	}
	return sha256.Sum256(append([]byte{1}, prefix...)), nil
}

func positionForRecord(segment [sha256.Size]byte, start int64, record []byte) RecordPos {
	return RecordPos{
		segment: segment, start: start, length: uint32(len(record)),
		digest: sha256.Sum256(record),
	}
}

// String yields a stable, opaque and round-trippable reference for #7's
// authenticated record-detail API. It intentionally contains no filesystem
// pathname or raw argument text.
func (p RecordPos) String() string {
	var data [positionBytes]byte
	data[0] = cursorVersion
	copy(data[1:33], p.segment[:])
	binary.BigEndian.PutUint64(data[33:41], uint64(p.start))
	binary.BigEndian.PutUint32(data[41:45], p.length)
	copy(data[45:], p.digest[:])
	return base64.RawURLEncoding.EncodeToString(data[:])
}

func ParseRecordPos(raw string) (RecordPos, error) {
	var result RecordPos
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(data) != positionBytes || data[0] != cursorVersion {
		return result, ErrInvalidCursor
	}
	copy(result.segment[:], data[1:33])
	start := binary.BigEndian.Uint64(data[33:41])
	if start > math.MaxInt64 {
		return RecordPos{}, ErrInvalidCursor
	}
	result.start = int64(start)
	result.length = binary.BigEndian.Uint32(data[41:45])
	copy(result.digest[:], data[45:])
	if result.length == 0 || int64(result.length) > maxFileBytes ||
		uint64(result.length) > math.MaxInt64-start ||
		result.segment == ([sha256.Size]byte{}) {
		return RecordPos{}, ErrInvalidCursor
	}
	return result, nil
}

// newPageCursor accepts a verified raw-byte boundary proof, not the last
// decoded event. In particular, a completed corrupt/oversized line is allowed
// and must produce a resumable cursor rather than a zero-digest dead cursor.
func newPageCursor(dir PageDirection, segment [sha256.Size]byte, hint uint64, offset int64, proof [sha256.Size]byte) Cursor {
	if (offset == 0 && proof != ([sha256.Size]byte{})) ||
		(offset > 0 && proof == ([sha256.Size]byte{})) {
		panic("audit page cursor requires a proof for nonzero boundaries")
	}
	var raw [cursorBytes]byte
	raw[0], raw[1] = cursorVersion, byte(dir)
	copy(raw[2:34], segment[:])
	binary.BigEndian.PutUint64(raw[34:42], hint)
	binary.BigEndian.PutUint64(raw[42:50], uint64(offset))
	copy(raw[50:], proof[:])
	return Cursor(base64.RawURLEncoding.EncodeToString(raw[:]))
}

func decodePageCursor(value Cursor) (cursorState, error) {
	var state cursorState
	raw, err := base64.RawURLEncoding.DecodeString(string(value))
	if err != nil || len(raw) != cursorBytes || raw[0] != cursorVersion {
		return state, ErrInvalidCursor
	}
	state.dir = PageDirection(raw[1])
	if state.dir != Forward && state.dir != Backward {
		return cursorState{}, ErrInvalidCursor
	}
	copy(state.segment[:], raw[2:34])
	state.hint = binary.BigEndian.Uint64(raw[34:42])
	offset := binary.BigEndian.Uint64(raw[42:50])
	if offset > math.MaxInt64 || state.segment == ([sha256.Size]byte{}) {
		return cursorState{}, ErrInvalidCursor
	}
	state.offset = int64(offset)
	copy(state.prev[:], raw[50:])
	if state.offset == 0 && state.prev != ([sha256.Size]byte{}) {
		return cursorState{}, ErrInvalidCursor
	}
	return state, nil
}

// verifyCursorBoundary authenticates the content immediately before a cursor
// against the recorded digest and rejects stale offsets, modified lines and
// boundaries beyond the acknowledged frontier.
func verifyCursorBoundary(ctx context.Context, f *os.File, frontier int64, cursor cursorState) error {
	if cursor.offset < 0 || cursor.offset > frontier {
		return ErrInvalidCursor
	}
	if cursor.offset == 0 {
		if cursor.prev != ([sha256.Size]byte{}) {
			return ErrInvalidCursor
		}
		return nil
	}
	actual, err := boundaryProof(ctx, f, cursor.offset, frontier)
	if err != nil {
		if errors.Is(err, ErrRecordBoundary) || errors.Is(err, ErrRecordTooLarge) {
			return fmt.Errorf("%w: %w", ErrInvalidCursor, err)
		}
		return err // Cancellation and disk I/O failures are not invalid cursors.
	}
	if actual != cursor.prev {
		return ErrInvalidCursor
	}
	return nil
}
