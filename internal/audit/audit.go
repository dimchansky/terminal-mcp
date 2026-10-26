// Package audit records local MCP tool invocation events.
package audit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxFileBytes int64 = 4 << 20
	maxArchives        = 3
)

var (
	ErrClosed        = errors.New("audit log is closed")
	ErrEventTooLarge = errors.New("audit event exceeds maximum file size")
	ErrInvalidEvent  = errors.New("audit event cannot be serialized")
	ErrDegraded      = errors.New("audit_degraded: new MCP work is refused")
)

const EventSchemaVersion = 2

type Event struct {
	SchemaVersion  int    `json:"schema_version,omitempty"`
	OperationID    string `json:"operation_id,omitempty"`
	Receipt        any    `json:"receipt,omitempty"`
	Time           string `json:"time"`
	ServerID       string `json:"server_id,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
	Tool           string `json:"tool"`
	Phase          string `json:"phase"`
	Status         string `json:"status,omitempty"`
	DurationMS     int64  `json:"duration_ms,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	Args           any    `json:"args,omitempty"`
	CallMeta       any    `json:"call_meta,omitempty"`
	InputResponses any    `json:"input_responses,omitempty"`
	RequestState   string `json:"request_state,omitempty"`
	Replayed       bool   `json:"replayed,omitempty"`
	Error          string `json:"error,omitempty"`
	Process        any    `json:"process,omitempty"`
	Outcome        string `json:"outcome,omitempty"`
	FilesTotal     int    `json:"files_total,omitempty"`
	FilesFailed    int    `json:"files_failed,omitempty"`
	SearchMatches  *int   `json:"search_matches,omitempty"`
	Truncated      bool   `json:"truncated,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// NewEvent timestamps an event before its synchronous write.
func NewEvent(e Event) Event {
	e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	if e.SchemaVersion == 0 {
		e.SchemaVersion = EventSchemaVersion
	}
	return e
}

// Log serializes all sink operations, including writes, rotation, and Close.
// Write waits for the sink to append the record; there is no in-memory queue.
type Log struct {
	mu         sync.Mutex
	sink       eventSink
	closed     bool
	closeErr   error
	path       string
	legacyLock *os.File                     // Kept until Close to fence out the old server.
	failure    atomic.Pointer[auditFailure] // Accessible without waiting on a stalled sink.
}

type auditFailure struct{ err error }

// Status is a read-only snapshot of the audit health. It never waits on file
// I/O and can be served by authenticated administration during a disk stall.
type Status struct {
	State     string `json:"state"`
	Path      string `json:"path,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

func DisabledStatus() Status { return Status{State: "disabled"} }

func (l *Log) Status() Status {
	if l == nil {
		return DisabledStatus()
	}
	s := Status{State: "ok", Path: l.path}
	if failed := l.failure.Load(); failed != nil {
		s.State, s.LastError = "degraded", failed.err.Error()
	}
	return s
}

// Ready is a nonblocking admission check. Only the first failure matters;
// writing a later finish event never silently re-enables the server.
func (l *Log) Ready() error {
	if failed := l.failure.Load(); failed != nil {
		return errors.Join(ErrDegraded, failed.err)
	}
	return nil
}

// Degrade latches the first audit failure and emits one immediate diagnostic
// outside the audit sink. The service requires a controlled restart to recover.
func (l *Log) Degrade(err error) {
	if l == nil || err == nil {
		return
	}
	if l.failure.CompareAndSwap(nil, &auditFailure{err: err}) {
		log.Printf("audit DEGRADED; refusing new MCP work until restart: %v", err)
	}
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
	log := newLog(sink)
	log.path = sink.path
	return log, nil
}

// Write snapshots an event before taking the sink lock. A nil error means the
// entire JSONL record was reported written by the OS, not that it was fsynced.
// An append error is returned immediately and may leave a failed partial record
// for the sink to repair. Concurrent writers wait instead of dropping events.
func (l *Log) Write(e Event) error { return l.write(e, false) }

// Start is for pre-effect events. The health check and append occur under the
// *same* mutex, so no later admission can race with an earlier sink failure.
// Completion events use Write, which still attempts to record the true outcome
// of already-admitted operations even after degradation.
func (l *Log) Start(e Event) error { return l.write(e, true) }

func (l *Log) write(e Event, requireHealthy bool) error {
	if e.Time == "" {
		return errors.New("audit event has no time; use NewEvent")
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	line = append(line, '\n')
	if int64(len(line)) > maxFileBytes {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrEventTooLarge, len(line), maxFileBytes)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	if requireHealthy {
		if err := l.Ready(); err != nil {
			return err
		}
	}
	if err := l.sink.appendLine(line); err != nil {
		writeErr := fmt.Errorf("audit write: %w", err)
		l.Degrade(writeErr)
		return writeErr
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
		if l.legacyLock != nil {
			l.closeErr = errors.Join(l.closeErr, l.legacyLock.Close())
			l.legacyLock = nil
		}
		if l.closeErr != nil {
			l.Degrade(l.closeErr)
		}
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
