//go:build darwin || linux

package audit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// openAuditReader opens an existing audit segment read-only, without following
// symlinks and without taking the writer lock. The caller closes the handle.
func openAuditReader(path string) (*os.File, error) {
	if err := checkAuditPrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	// O_NONBLOCK prevents a malicious FIFO in the audit directory from
	// hanging Open before we can check the opened file type.
	var fd int
	var err error
	for {
		fd, err = unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("open audit reader: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, errors.Join(err, unix.Close(fd))
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.Join(fmt.Errorf("audit reader requires a regular file"), unix.Close(fd))
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		return nil, errors.Join(err, unix.Close(fd))
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err == nil {
		err = checkAuditPrivateFile(path, info)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
