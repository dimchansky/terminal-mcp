//go:build windows

package filesystem

import (
	"fmt"
	"os"

	"terminalmcp/internal/winsecure"
)

// Windows doesn't support POSIX O_NONBLOCK. Only regular files are read;
// pipes, devices, reparse points and directories are rejected by os.Root.
func readOpenFlags() int { return os.O_RDONLY }

func ownedSingleLink(parent *os.Root, name string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("replacement requires a regular file")
	}
	file, err := parent.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return winsecure.SingleLinkOwned(file)
}
