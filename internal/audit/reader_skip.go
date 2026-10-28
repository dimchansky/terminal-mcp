package audit

import (
	"bytes"
	"context"
	"os"
)

// maxCorruptScan bounds the search for a newline in a malformed external
// archive. An incomplete result is not a valid cursor boundary.
const maxCorruptScan int64 = 16 << 20

func findNextNewline(ctx context.Context, f *os.File, start, limit int64) (int64, bool, error) {
	end := min(limit, start+maxCorruptScan-maxFileBytes)
	var chunk [readerBlockBytes]byte
	for at := start; at < end; {
		if err := ctx.Err(); err != nil {
			return start, false, err
		}
		n := int(min(int64(len(chunk)), end-at))
		if err := readExactAt(f, chunk[:n], at); err != nil {
			return at, false, err
		}
		got := n
		if index := bytes.IndexByte(chunk[:got], '\n'); index >= 0 {
			return at + int64(index+1), true, nil
		}
		at += int64(got)
	}
	return end, false, nil
}
