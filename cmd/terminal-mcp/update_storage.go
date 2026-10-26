//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockUpdateRoot serializes queueing an update and pruning completed update
// payloads. The OS releases the lock on crash, unlike a mkdir-based sentinel.
func lockUpdateRoot(root string) (func(), error) {
	lock, err := os.OpenFile(filepath.Join(root, "controller.lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("update control busy: %w", err)
	}
	return func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
	}, nil
}

// saveRollbackSlot keeps one long-lived last-known-good executable, replacing
// the previous slot atomically. This happens after preflight but BEFORE the
// running server stops, so an unexpected crash during activation retains a
// verified recovery binary outside the transient update job.
//
// Custom update roots also get their own rollback slot next to their root.
func saveRollbackSlot(root, previous, expectedHash string) (string, error) {
	rollbackDir := filepath.Join(filepath.Dir(root), "rollback")
	if err := secureUpdateDir(rollbackDir); err != nil {
		return "", err
	}
	slot := filepath.Join(rollbackDir, "terminal-mcp")
	if err := replaceExecutable(previous, slot, 0700); err != nil {
		return "", err
	}
	actual, err := fileSHA256(slot)
	if err != nil {
		return "", err
	}
	if actual != expectedHash {
		return "", fmt.Errorf("rollback slot digest mismatch: %s", slot)
	}
	return slot, nil
}

func cleanupCompletedJobPayloads(jobDir string) error {
	var err error
	for _, name := range []string{"candidate", "previous", "worker"} {
		path := filepath.Join(jobDir, name)
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("cleanup %s: %w", path, removeErr))
		}
	}
	return err
}
