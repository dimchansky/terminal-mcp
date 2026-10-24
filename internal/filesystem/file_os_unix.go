//go:build linux || darwin

package filesystem

import (
	"fmt"
	"os"
	"syscall"
)

func readOpenFlags() int { return os.O_RDONLY | syscall.O_NONBLOCK }

func ownedSingleLink(_ *os.Root, _ string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink != 1 || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("replacement requires a single-link file owned by the service user")
	}
	return nil
}
