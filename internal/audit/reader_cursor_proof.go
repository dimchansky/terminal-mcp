package audit

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"os"
)

// cursorProof works on exact JSONL bytes, not decoded JSON. A cursor after a
// corrupt but newline-terminated row must remain resumable; the reader reports
// corruption in the page rather than declaring the cursor invalid.
func cursorProof(data []byte) [sha256.Size]byte {
	if len(data) <= int(maxFileBytes) {
		return sha256.Sum256(data)
	}
	// A bounded proof for unusually large corrupted rows. A full 16 MiB
	// historical line is never allocated just to resume scanning.
	tail := data[max(0, len(data)-readerBlockBytes):]
	var lenBytes [8]byte
	binary.BigEndian.PutUint64(lenBytes[:], uint64(len(data)))
	sum := sha256.New()
	sum.Write([]byte{0xff})
	sum.Write(lenBytes[:])
	sum.Write(tail)
	var result [sha256.Size]byte
	copy(result[:], sum.Sum(nil))
	return result
}

// boundaryProof validates a newline boundary and fingerprints the preceding
// raw span, including malformed JSON or a record larger than 4 MiB. Hard
// 16-MiB scan limit is intentional: above that no safe cursor is emitted.
func boundaryProof(ctx context.Context, f *os.File, boundary, frontier int64) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	if boundary < 0 || boundary > frontier {
		return zero, ErrRecordBoundary
	}
	if boundary == 0 {
		return zero, nil
	}
	var last [1]byte
	if err := readExactAt(f, last[:], boundary-1); err != nil {
		return zero, err
	}
	if last[0] != '\n' {
		return zero, ErrRecordBoundary
	}
	var buf [readerBlockBytes]byte
	start := int64(0)
	for end := boundary - 1; end > 0; {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if boundary-end > maxCorruptScan {
			return zero, ErrRecordTooLarge
		}
		begin := max(int64(0), end-readerBlockBytes)
		size := int(end - begin)
		if err := readExactAt(f, buf[:size], begin); err != nil {
			return zero, err
		}
		if i := lastNewline(buf[:size]); i >= 0 {
			start = begin + int64(i+1)
			break
		}
		end = begin
	}
	span := boundary - start
	if span > maxCorruptScan {
		return zero, ErrRecordTooLarge
	}
	// Full proof of ordinary records (including invalid JSON).
	if span <= maxFileBytes {
		raw := make([]byte, int(span))
		if err := readExactAt(f, raw, start); err != nil {
			return zero, err
		}
		return cursorProof(raw), nil
	}
	// Large record proof: signed length and trailing 64 KiB, with domain.
	var lenBytes [8]byte
	binary.BigEndian.PutUint64(lenBytes[:], uint64(span))
	tail := make([]byte, readerBlockBytes)
	if err := readExactAt(f, tail, boundary-readerBlockBytes); err != nil {
		return zero, err
	}
	sum := sha256.New()
	sum.Write([]byte{0xff})
	sum.Write(lenBytes[:])
	sum.Write(tail)
	copy(zero[:], sum.Sum(nil))
	return zero, nil
}

func lastNewline(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == '\n' {
			return i
		}
	}
	return -1
}
