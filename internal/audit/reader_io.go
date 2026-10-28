package audit

import (
	"fmt"
	"io"
	"os"
)

// readExactAt treats a truncated declared audit range as damaged history,
// never as io.EOF (which means a legitimate page boundary).
func readExactAt(f *os.File, buf []byte, offset int64) error {
	n, err := f.ReadAt(buf, offset)
	if n != len(buf) {
		return fmt.Errorf("%w: audit bytes at %d (got %d of %d, underlying %v)",
			io.ErrUnexpectedEOF, offset, n, len(buf), err)
	}
	return err
}
