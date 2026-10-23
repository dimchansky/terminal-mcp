// Package audit records bounded local MCP tool invocation events.
package audit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	maxFileBytes int64 = 4 << 20
	maxArchives        = 3
)

type Event struct {
	Time       string `json:"time"`
	RequestID  string `json:"request_id"`
	Tool       string `json:"tool"`
	Phase      string `json:"phase"`
	Status     string `json:"status,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Args       any    `json:"args,omitempty"`
	Replayed   bool   `json:"replayed,omitempty"`
	Error      string `json:"error,omitempty"`
	Process    any    `json:"process,omitempty"`
}

// Log writes one bounded JSON line per event, serializing concurrent calls.
// It is not a tamper-proof or crash-synchronous audit trail.
type Log struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func Open(path string) (*Log, error) {
	if path == "" {
		return nil, errors.New("audit path is empty")
	}
	dir := filepath.Dir(path)
	// Do not create or weaken arbitrary parent directories.
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("audit directory must exist and be private (0700): %s", dir)
	}
	l := &Log{path: path}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Log) open() error {
	info, err := os.Lstat(l.path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("audit log must be a private regular file (0600): %s", l.path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 {
		_ = f.Close()
		return fmt.Errorf("unsafe audit file: %v", err)
	}
	l.f, l.size = f, stat.Size()
	return nil
}

func (l *Log) rotate() error {
	if err := l.f.Close(); err != nil {
		return err
	}
	l.f = nil
	if err := os.Remove(l.path + ".3"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for i := maxArchives - 1; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", l.path, i)
		next := fmt.Sprintf("%s.%d", l.path, i+1)
		if err := os.Rename(old, next); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return l.open()
}

func (l *Log) Write(e Event) error {
	e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	// Terminal command/PTY input is intentionally logged in full. The tool
	// transport bounds a request, and a single event must fit one log file.
	if int64(len(line)) > maxFileBytes {
		return errors.New("audit event exceeds maximum file size")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return errors.New("audit log closed")
	}
	if l.size+int64(len(line)) > maxFileBytes {
		if err := l.rotate(); err != nil {
			return fmt.Errorf("audit rotation: %w", err)
		}
	}
	n, err := l.f.Write(line)
	l.size += int64(n)
	if err != nil {
		return err
	}
	if n != len(line) {
		return io.ErrShortWrite
	}
	return nil
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

func RequestID() string {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("system randomness unavailable")
	}
	return hex.EncodeToString(id[:])
}
