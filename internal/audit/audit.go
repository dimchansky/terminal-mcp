// Package audit records local MCP tool invocation events.
package audit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxFileBytes int64 = 4 << 20
	// Zero disables the optional total history budget. Archives are never pruned.
	DefaultMaxHistoryBytes int64 = 0
)

var (
	ErrClosed        = errors.New("audit log is closed")
	ErrEventTooLarge = errors.New("audit event exceeds maximum file size")
	ErrInvalidEvent  = errors.New("audit event cannot be serialized")
	ErrDegraded      = errors.New("audit_degraded: new MCP work is refused")
	ErrHistoryLimit  = errors.New("audit_history_limit: storage budget exhausted")
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
	bytesUsed  atomic.Int64
	bytesLimit int64
}

type auditFailure struct {
	err       error
	permanent bool // Invariant failures require an operator restart.
}

// Status is a read-only snapshot of the audit health. It never waits on file
// I/O and can be served by authenticated administration during a disk stall.
type Status struct {
	State      string `json:"state"`
	Path       string `json:"path,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	BytesUsed  int64  `json:"bytes_used,omitempty"`
	BytesLimit int64  `json:"bytes_limit,omitempty"`
}

func DisabledStatus() Status { return Status{State: "disabled"} }

func (l *Log) Status() Status {
	if l == nil {
		return DisabledStatus()
	}
	s := Status{State: "ok", Path: l.path, BytesUsed: l.bytesUsed.Load(), BytesLimit: l.bytesLimit}
	if failed := l.failure.Load(); failed != nil {
		s.State, s.LastError = "degraded", failed.err.Error()
	}
	return s
}

// Ready reports current health without waiting on the sink. A failed append
// remains visible until a later successful write proves recovery.
func (l *Log) Ready() error {
	if failed := l.failure.Load(); failed != nil {
		return errors.Join(ErrDegraded, failed.err)
	}
	return nil
}

// Degrade is reserved for non-recoverable audit invariants (for example an
// observer panic). Ordinary sink write failures are recorded by write and are
// retried automatically on subsequent requests.
func (l *Log) Degrade(err error) {
	if l != nil && l.markDegraded(err, true) {
		reportDegraded(err)
	}
}

// markDegraded is nonblocking so status never waits on disk or stderr.
// Keep the first transient error, but permit a permanent failure to supersede
// it. Callers must report a transition only after releasing Log.mu.
func (l *Log) markDegraded(err error, permanent bool) bool {
	if err == nil {
		return false
	}
	for {
		previous := l.failure.Load()
		if previous != nil && (previous.permanent || !permanent) {
			return false
		}
		if l.failure.CompareAndSwap(previous, &auditFailure{err: err, permanent: permanent}) {
			return true
		}
	}
}

func reportDegraded(err error) {
	slog.Error("audit degraded; refusing unaudited MCP work", "error", err)
}

// newLog allows deterministic sink failure and concurrency tests.
func newLog(sink eventSink) *Log {
	return &Log{sink: sink}
}

func Open(path string) (*Log, error) { return OpenWithLimit(path, DefaultMaxHistoryBytes) }

// OpenWithLimit counts the active file and all numbered archives against an
// optional hard budget; zero means unlimited. It never deletes history.
func OpenWithLimit(path string, maxHistoryBytes int64) (*Log, error) {
	sink, err := openFileSinkWithLimit(path, maxHistoryBytes)
	if err != nil {
		return nil, err
	}
	l := newLog(sink)
	l.path, l.bytesLimit = sink.path, maxHistoryBytes
	l.bytesUsed.Store(sink.total)
	return l, nil
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
		err := errors.New("audit event has no time; use NewEvent")
		if !requireHealthy {
			l.Degrade(err) // Missing completion metadata is an invariant failure.
		}
		return err
	}
	line, err := json.Marshal(e)
	if err != nil {
		writeErr := fmt.Errorf("%w: %w", ErrInvalidEvent, err)
		if !requireHealthy {
			l.Degrade(writeErr) // An accepted action's completion cannot be represented.
		}
		return writeErr
	}
	line = append(line, '\n')
	if int64(len(line)) > maxFileBytes {
		writeErr := fmt.Errorf("%w: %d bytes, limit %d", ErrEventTooLarge, len(line), maxFileBytes)
		if !requireHealthy {
			l.Degrade(writeErr)
		}
		return writeErr
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return ErrClosed
	}
	if requireHealthy {
		if failed := l.failure.Load(); failed != nil && failed.permanent {
			l.mu.Unlock()
			return errors.Join(ErrDegraded, failed.err)
		}
	}
	if err := l.sink.appendLine(line); err != nil {
		writeErr := fmt.Errorf("audit write: %w", err)
		// A failed pre-effect append never permits dispatch. A later request
		// may retry the sink, but cannot run until its own start is written.
		// An explicit history cap cannot change within this process, unlike
		// a temporary filesystem error. Keep that denial until reconfiguration.
		first := l.markDegraded(writeErr, errors.Is(err, ErrHistoryLimit) || errors.Is(err, ErrJournalChanged))
		l.mu.Unlock()
		if first {
			reportDegraded(writeErr) // Slow stderr must not hold Log.mu.
		}
		return writeErr
	}
	if sink, ok := l.sink.(*fileSink); ok {
		l.bytesUsed.Store(sink.total)
	}
	var recovered error
	if failed := l.failure.Load(); failed != nil && !failed.permanent &&
		l.failure.CompareAndSwap(failed, nil) {
		recovered = failed.err
	}
	l.mu.Unlock()
	if recovered != nil {
		slog.Info("audit writes recovered; MCP work may resume", "previous_error", recovered)
	}
	return nil
}

// Close prevents further writes, waits for the current write if necessary,
// syncs and closes the sink, and releases its file lock. Repeated calls return
// the same error; concurrent Close calls cannot close the sink twice.
func (l *Log) Close() error {
	l.mu.Lock()
	if l.closed {
		result := l.closeErr
		l.mu.Unlock()
		return result
	}
	l.closed = true
	l.closeErr = l.sink.close()
	if l.legacyLock != nil {
		l.closeErr = errors.Join(l.closeErr, l.legacyLock.Close())
		l.legacyLock = nil
	}
	first := l.markDegraded(l.closeErr, true)
	result := l.closeErr
	l.mu.Unlock()
	if first {
		reportDegraded(result) // Keep best-effort stderr outside Log.mu.
	}
	return result
}

func RequestID() string {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("system randomness unavailable")
	}
	return hex.EncodeToString(id[:])
}
