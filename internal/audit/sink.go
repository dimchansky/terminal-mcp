package audit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// eventSink is used only while Log.mu is held. No additional sink mutex
// is required, including across append, rotation, and close.
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
	total    int64  // Active file plus all numbered archives, including legacy.
	limit    int64  // Hard cap; never evict old audit records.
	next     uint64 // Next strictly increasing archive suffix.

	// Filesystem renames are replaceable in tests to inject rotation errors.
	rename func(string, string) error
}

func openFileSink(path string) (*fileSink, error) {
	return openFileSinkWithLimit(path, DefaultMaxHistoryBytes)
}

func openFileSinkWithLimit(path string, limit int64) (*fileSink, error) {
	if path == "" {
		return nil, errors.New("audit path is empty")
	}
	if limit <= 0 {
		return nil, errors.New("audit history limit must be positive")
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
	if err := checkAuditPrivateDir(dir); err != nil {
		return nil, err
	}
	path = filepath.Join(dir, filepath.Base(abs))
	lock, err := lockAuditFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	s := &fileSink{path: path, lock: lock, limit: limit, rename: os.Rename}
	if err := s.open(); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return s, nil
}

// PrepareDirectory ensures a private directory using platform-native
// security controls (POSIX modes on Unix, ACLs on Windows).
func PrepareDirectory(path string) error { return prepareAuditDir(path) }

// ErrInUse is returned when another live audit writer owns the same log path.
// The lock file is deliberately retained across restarts. Removing it while
// another process is active would allow two processes to lock different inodes.
var ErrInUse = errors.New("audit log is already in use")

// scanArchives inventories the existing audit history exactly once per sink.
// All numbered archives count, including files copied during legacy migration.
// Archive numbering is append-only; older files are never renamed or removed.
func (s *fileSink) scanArchives() error {
	entries, err := os.ReadDir(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	s.next = 1
	prefix := filepath.Base(s.path) + "."
	for _, entry := range entries {
		suffix, ok := strings.CutPrefix(entry.Name(), prefix)
		if !ok || suffix == "" || strings.Trim(suffix, "0123456789") != "" {
			continue
		}
		n, parseErr := strconv.ParseUint(suffix, 10, 64)
		if parseErr != nil || n == 0 || strconv.FormatUint(n, 10) != suffix || n == math.MaxUint64 {
			return fmt.Errorf("invalid audit archive suffix: %s", entry.Name())
		}
		name := filepath.Join(filepath.Dir(s.path), entry.Name())
		info, err := os.Lstat(name)
		if err != nil {
			return err
		}
		if err := checkAuditPrivateFile(name, info); err != nil {
			return err
		}
		if info.Size() > math.MaxInt64-s.total {
			return errors.New("audit archive history size overflows int64")
		}
		s.total += info.Size()
		if n >= s.next {
			s.next = n + 1
		}
	}
	return nil
}

func (s *fileSink) open() error {
	initial := s.next == 0
	if initial {
		if err := s.scanArchives(); err != nil {
			return err
		}
	}
	info, err := os.Lstat(s.path)
	if err == nil {
		if err := checkAuditPrivateFile(s.path, info); err != nil {
			return err
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
	if err := checkAuditPrivateFile(s.path, stat); err != nil {
		return errors.Join(err, f.Close())
	}
	size, err := recoverTail(f, stat.Size())
	if err != nil {
		return errors.Join(err, f.Close())
	}
	if size != stat.Size() {
		slog.Warn("repaired incomplete audit JSONL tail", "removed_bytes", stat.Size()-size)
	}
	if initial {
		if size > math.MaxInt64-s.total {
			return errors.Join(errors.New("audit history size overflows int64"), f.Close())
		}
		s.total += size
		if s.total > s.limit {
			return errors.Join(fmt.Errorf("%w: existing history %d bytes exceeds limit %d", ErrHistoryLimit, s.total, s.limit), f.Close())
		}
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
	if s.next == math.MaxUint64 {
		return errors.New("audit archive sequence exhausted")
	}
	archive := fmt.Sprintf("%s.%d", s.path, s.next)
	if _, err := os.Lstat(archive); err == nil {
		return fmt.Errorf("audit archive already exists; refusing overwrite: %s", archive)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A failed Close may nevertheless have closed the descriptor. Never leave
	// a potentially unusable handle in the sink state. No historical archive
	// is renamed or removed, even when the budget is exhausted.
	f := s.f
	s.f = nil
	if err := f.Close(); err != nil {
		return fmt.Errorf("close audit file before rotation: %w", err)
	}
	if err := s.rename(s.path, archive); err != nil {
		return err
	}
	s.next++
	return s.open()
}

func (s *fileSink) appendLine(line []byte) error {
	if int64(len(line)) > maxFileBytes {
		return ErrEventTooLarge
	}
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
	// The limit is checked *before* rotation and append. In particular,
	// exhausting history capacity never triggers deletion of an archive.
	if int64(len(line)) > s.limit || s.total > s.limit-int64(len(line)) {
		return fmt.Errorf("%w: %d bytes used, %d bytes requested, %d bytes allowed",
			ErrHistoryLimit, s.total, len(line), s.limit)
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
		s.total += int64(n)
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
