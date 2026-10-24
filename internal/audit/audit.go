// Package audit records bounded local MCP tool invocation events.
package audit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

const (
	maxFileBytes   int64 = 4 << 20
	maxArchives          = 3
	queueCapacity        = 1024
	queueByteLimit       = 8 << 20
)

var (
	ErrQueueFull = errors.New("audit queue is full")
	ErrClosed    = errors.New("audit log is closed")
)

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

// NewEvent fills the timestamp before the event enters the audit queue.
func NewEvent(e Event) Event {
	e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	return e
}

// Log accepts events from concurrent callers and transfers them to one worker.
// Only its enqueue state is protected by mu; the worker exclusively owns sink.
// Close drains accepted events and waits for the sink to release its file lock.
type Log struct {
	mu           sync.Mutex
	queue        chan []byte
	done         chan struct{}
	closed       bool
	pendingBytes int
	dropped      uint64

	sink      eventSink
	workerErr error  // Worker-owned; read only after <-done.
	diskLost  uint64 // Worker-owned; read only after <-done.
}

// newLog is a package-local injection point for deterministic queue tests.
// The sink is supplied before the worker starts, avoiding test hook races.
func newLog(sink eventSink) *Log {
	l := &Log{sink: sink, queue: make(chan []byte, queueCapacity), done: make(chan struct{})}
	go l.run()
	return l
}

func Open(path string) (*Log, error) {
	sink, err := openFileSink(path)
	if err != nil {
		return nil, err
	}
	return newLog(sink), nil
}

// Write snapshots the event before enqueueing. Never waits for disk I/O.
// The queue is bounded both by count and by encoded bytes, including the
// record currently handled by the worker. Rejected submissions are counted.
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
	if l.pendingBytes+len(line) > queueByteLimit {
		l.dropped++
		return ErrQueueFull
	}
	select {
	case l.queue <- line:
		l.pendingBytes += len(line)
		return nil
	default:
		l.dropped++
		return ErrQueueFull
	}
}

// run owns all sink operations; all accepted records are processed exactly
// once, even if an earlier attempt fails. It records both the first disk error
// and the total number of records that could not be written.
func (l *Log) run() {
	defer close(l.done)
	consecutiveFailures := uint64(0)
	var reportedDrops uint64
	for line := range l.queue {
		err := l.sink.appendLine(line)
		l.mu.Lock()
		l.pendingBytes -= len(line)
		dropped := l.dropped
		l.mu.Unlock()
		if dropped != reportedDrops && (reportedDrops == 0 || dropped-reportedDrops >= 1000) {
			log.Printf("audit: queue overflow; %d total events dropped", dropped)
			reportedDrops = dropped
		}
		if err != nil {
			if l.workerErr == nil {
				l.workerErr = fmt.Errorf("audit write: %w", err)
			}
			consecutiveFailures++
			l.diskLost++
			if consecutiveFailures == 1 {
				log.Printf("audit: disk write failed: %v", err)
			}
		} else if consecutiveFailures != 0 {
			log.Printf("audit: disk writes resumed; %d events lost during failure", consecutiveFailures)
			consecutiveFailures = 0
		}
	}
	if consecutiveFailures != 0 {
		log.Printf("audit: %d queued events could not be written", consecutiveFailures)
	}
	l.mu.Lock()
	dropped := l.dropped
	l.mu.Unlock()
	if dropped != reportedDrops {
		log.Printf("audit: queue overflow; %d total events dropped", dropped)
	}
	if err := l.sink.close(); err != nil {
		l.workerErr = errors.Join(l.workerErr, err)
	}
}

// Close rejects future calls to Write and drains accepted records. It waits
// for disk sync, descriptor close, and release of the exclusive audit lock.
// Repeated Close calls observe the same result.
func (l *Log) Close() error {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.queue)
	}
	dropped := l.dropped
	l.mu.Unlock()
	<-l.done
	err := l.workerErr
	if l.diskLost != 0 {
		err = errors.Join(err, fmt.Errorf("audit lost %d events to disk write errors", l.diskLost))
	}
	if dropped != 0 {
		err = errors.Join(err, fmt.Errorf("audit dropped %d events from a full queue", dropped))
	}
	return err
}

func RequestID() string {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("system randomness unavailable")
	}
	return hex.EncodeToString(id[:])
}
