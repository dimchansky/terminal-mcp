//go:build linux || darwin

package audit

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

func lockAuditFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open audit lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.Join(fmt.Errorf("audit lock must be a private regular file (0600): %s", path), f.Close())
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeErr := f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errors.Join(fmt.Errorf("%w: %s", ErrInUse, path), closeErr)
		}
		return nil, errors.Join(fmt.Errorf("acquire audit lock: %w", err), closeErr)
	}
	return f, nil
}

func checkAuditPrivateDir(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("audit directory must exist and be private (0700): %s", path)
	}
	return nil
}

func checkAuditPrivateFile(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("audit file must be private regular file (0600): %s", path)
	}
	return nil
}

func prepareAuditDir(path string) error { return os.MkdirAll(path, 0700) }
