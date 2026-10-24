//go:build linux || darwin

package audit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// eventSink is owned exclusively by the audit worker. It does not require a
// mutex and must never be called concurrently with itself.
type eventSink interface {
	appendLine([]byte) error
	close() error
}

// auditFile allows tests to inject short writes, sync errors, and failed
// truncation without replacing the whole file sink.
type auditFile interface {
	Write([]byte) (int, error)
	Truncate(int64) error
	Sync() error
	Close() error
}

type fileSink struct {
	path     string
	lock     *os.File
	f        auditFile
	size     int64
	repair   bool
	repairTo int64

	// Filesystem operations are replaceable in package tests to exercise
	// partial rotation failures; production uses os.Remove and os.Rename.
	remove func(string) error
	rename func(string, string) error
}

func openFileSink(path string) (*fileSink, error) {
	if path == "" {
		return nil, errors.New("audit path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Use the real parent path so aliases to the same directory share a lock.
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("audit directory must exist and be private (0700): %s", dir)
	}
	path = filepath.Join(dir, filepath.Base(abs))
	lock, err := lockAuditFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	s := &fileSink{path: path, lock: lock, remove: os.Remove, rename: os.Rename}
	if err := s.open(); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return s, nil
}

// ErrInUse is returned when another live audit writer owns the same log path.
// The lock file is deliberately retained across restarts. Removing it while
// another process is active would allow two processes to lock different inodes.
var ErrInUse = errors.New("audit log is already in use")

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

func (s *fileSink) open() error {
	info, err := os.Lstat(s.path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("audit log must be a private regular file (0600): %s", s.path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	stat, err := f.Stat()
	if err != nil {
		return errors.Join(fmt.Errorf("stat audit file: %w", err), f.Close())
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 {
		return errors.Join(fmt.Errorf("unsafe audit file: %s", s.path), f.Close())
	}
	size, err := recoverTail(f, stat.Size())
	if err != nil {
		return errors.Join(err, f.Close())
	}
	if size != stat.Size() {
		log.Printf("audit: removed %d bytes from an incomplete JSONL tail", stat.Size()-size)
	}
	s.f, s.size, s.repair = f, size, false
	return nil
}

// recoverTail removes the incomplete last JSONL line, if any. It reads only
// bounded blocks, even for very large files.
func recoverTail(f *os.File, size int64) (int64, error) {
	if size == 0 {
		return 0, nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], size-1); err != nil {
		return 0, err
	}
	if last[0] == '\n' {
		return size, nil
	}
	var block [4096]byte
	for end := size; end > 0; {
		start := max(int64(0), end-int64(len(block)))
		n := int(end - start)
		if _, err := f.ReadAt(block[:n], start); err != nil {
			return 0, err
		}
		if index := bytes.LastIndexByte(block[:n], '\n'); index >= 0 {
			cut := start + int64(index) + 1
			return cut, f.Truncate(cut)
		}
		end = start
	}
	return 0, f.Truncate(0)
}

func (s *fileSink) rotate() error {
	// A failed Close may nevertheless have closed the descriptor. Never leave
	// a potentially unusable handle in the sink state.
	f := s.f
	s.f = nil
	if err := f.Close(); err != nil {
		return fmt.Errorf("close audit file before rotation: %w", err)
	}
	if err := s.remove(s.path + ".3"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for i := maxArchives - 1; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", s.path, i)
		next := fmt.Sprintf("%s.%d", s.path, i+1)
		if err := s.rename(old, next); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := s.rename(s.path, s.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.open()
}

func (s *fileSink) appendLine(line []byte) error {
	if s.f == nil {
		if err := s.open(); err != nil {
			return err
		}
	}
	if s.repair {
		if err := s.f.Truncate(s.repairTo); err != nil {
			return fmt.Errorf("repair incomplete audit record: %w", err)
		}
		s.size, s.repair = s.repairTo, false
	}
	if s.size+int64(len(line)) > maxFileBytes {
		if err := s.rotate(); err != nil {
			return fmt.Errorf("audit rotation: %w", err)
		}
	}
	before := s.size
	n, writeErr := s.f.Write(line)
	if writeErr == nil && n == len(line) {
		s.size += int64(n)
		return nil
	}
	if writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	if err := s.f.Truncate(before); err != nil {
		s.repair, s.repairTo = true, before
		return errors.Join(writeErr, fmt.Errorf("remove incomplete audit record: %w", err))
	}
	return writeErr
}

func (s *fileSink) close() error {
	var result error
	if s.f != nil {
		if s.repair {
			if err := s.f.Truncate(s.repairTo); err != nil {
				result = errors.Join(result, fmt.Errorf("audit final repair: %w", err))
			}
		}
		if err := s.f.Sync(); err != nil {
			result = errors.Join(result, fmt.Errorf("audit sync: %w", err))
		}
		f := s.f
		s.f = nil
		if err := f.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("audit close: %w", err))
		}
	}
	if s.lock != nil {
		lock := s.lock
		s.lock = nil
		if err := lock.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("release audit lock: %w", err))
		}
	}
	return result
}
