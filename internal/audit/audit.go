// Package audit records local MCP tool invocation events.
package audit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	maxFileBytes int64 = 4 << 20
	maxArchives        = 3
)

var ErrClosed = errors.New("audit log is closed")

type Event struct {
	Time          string `json:"time"`
	ServerID      string `json:"server_id,omitempty"`
	RequestID     string `json:"request_id,omitempty"`
	Tool          string `json:"tool"`
	Phase         string `json:"phase"`
	Status        string `json:"status,omitempty"`
	DurationMS    int64  `json:"duration_ms,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	Args          any    `json:"args,omitempty"`
	Replayed      bool   `json:"replayed,omitempty"`
	Error         string `json:"error,omitempty"`
	Process       any    `json:"process,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
	FilesTotal    int    `json:"files_total,omitempty"`
	FilesFailed   int    `json:"files_failed,omitempty"`
	SearchMatches *int   `json:"search_matches,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// NewEvent timestamps an event before its synchronous write.
func NewEvent(e Event) Event {
	e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	return e
}

// Log serializes all sink operations, including writes, rotation, and Close.
// Write waits for the sink to append the record; there is no in-memory queue.
type Log struct {
	mu       sync.Mutex
	sink     eventSink
	closed   bool
	closeErr error
}

// newLog allows deterministic sink failure and concurrency tests.
func newLog(sink eventSink) *Log {
	return &Log{sink: sink}
}

func Open(path string) (*Log, error) {
	sink, err := openFileSink(path)
	if err != nil {
		return nil, err
	}
	return newLog(sink), nil
}

// Write snapshots an event before taking the sink lock. A nil error means the
// entire JSONL record was reported written by the OS, not that it was fsynced.
// An append error is returned immediately and may leave a failed partial record
// for the sink to repair. Concurrent writers wait instead of dropping events.
func (l *Log) Write(e Event) error {
	if e.Time == "" {
		return errors.New("audit event has no time; use NewEvent")
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if int64(len(line)) > maxFileBytes {
		return errors.New("audit event exceeds maximum file size")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	if err := l.sink.appendLine(line); err != nil {
		return fmt.Errorf("audit write: %w", err)
	}
	return nil
}

// Close prevents further writes, waits for the current write if necessary,
// syncs and closes the sink, and releases its file lock. Repeated calls return
// the same error; concurrent Close calls cannot close the sink twice.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		l.closeErr = l.sink.close()
	}
	return l.closeErr
}

func RequestID() string {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("system randomness unavailable")
	}
	return hex.EncodeToString(id[:])
}
