//go:build windows

package audit

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/windows"
	"terminalmcp/internal/winsecure"
)

// Windows O_APPEND removes FILE_WRITE_DATA from the descriptor, making
// Truncate fail with ERROR_ACCESS_DENIED. The exclusive audit-writer lock and
// Log.mu serialize writes, so a seek to EOF on an O_RDWR handle preserves
// append semantics without sacrificing partial-record repair.
func openAuditJournal(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
}

func checkAuditJournalSize(f auditFile, expectedSize int64) error {
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seek audit journal: %w", err)
	}
	if end != expectedSize {
		return fmt.Errorf("%w: expected %d bytes, got %d", ErrJournalChanged, expectedSize, end)
	}
	return nil
}

func appendAuditJournal(f auditFile, line []byte, expectedSize int64) (int, bool, error) {
	if err := checkAuditJournalSize(f, expectedSize); err != nil {
		return 0, false, err
	}
	n, err := f.Write(line)
	return n, true, err
}

func checkAuditPrivateDir(path string) error { return winsecure.CheckPrivate(path) }

func checkAuditPrivateFile(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("audit file must be regular: %s", path)
	}
	return winsecure.CheckPrivate(path)
}

func prepareAuditDir(path string) error { return winsecure.EnsurePrivateDir(path) }

// The lock file deliberately survives restarts. Windows byte-range locks are
// released by the kernel when the owning process closes its handle or exits.
func lockAuditFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		err = checkAuditPrivateFile(path, info)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	var overlapped windows.Overlapped
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped)
	if err != nil {
		closeErr := f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errors.Join(fmt.Errorf("%w: %s", ErrInUse, path), closeErr)
		}
		return nil, errors.Join(fmt.Errorf("acquire audit lock: %w", err), closeErr)
	}
	return f, nil
}
