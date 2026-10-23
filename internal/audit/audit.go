// Package audit records bounded local MCP tool invocation events.
package audit

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
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

// NewEvent fills the timestamp before the event enters the audit queue.
func NewEvent(e Event) Event {
	e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	return e
}

// Log queues encoded events without disk I/O on the calling goroutine.
// One worker appends all accepted events to bounded JSONL files.
// A normal Close drains the queue, then syncs and closes the active file.
type Log struct {
	mu           sync.Mutex
	path         string
	f            *os.File
	size         int64
	queue        chan []byte
	done         chan struct{}
	closed       bool
	pendingBytes int
	dropped      uint64
	workerErr    error
	repair       bool
	repairTo     int64

	// Test hook: a blocked writer lets tests verify queue limits and Close.
	writeLineFn func([]byte) error
}

func Open(path string) (*Log, error) {
	if path == "" {
		return nil, errors.New("audit path is empty")
	}
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("audit directory must exist and be private (0700): %s", dir)
	}
	l := &Log{
		path:  path,
		queue: make(chan []byte, queueCapacity),
		done:  make(chan struct{}),
	}
	if err := l.open(); err != nil {
		return nil, err
	}
	go l.run()
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
	f, err := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 {
		_ = f.Close()
		return fmt.Errorf("unsafe audit file: %v", err)
	}
	size, err := recoverTail(f, stat.Size())
	if err != nil {
		_ = f.Close()
		return err
	}
	if size != stat.Size() {
		log.Printf("audit: removed %d bytes from an incomplete JSONL tail", stat.Size()-size)
	}
	l.f, l.size = f, size
	return nil
}

// recoverTail removes a partial JSONL line left by a crash or failed write.
// Complete lines end with a newline. The scan reads only one block at a time.
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

// Write queues a snapshot of the event. It never waits for disk I/O.
// A full queue drops the event and returns ErrQueueFull.
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
	if l.closed {
		l.mu.Unlock()
		return ErrClosed
	}
	if l.pendingBytes+len(line) > queueByteLimit {
		l.dropped++
		l.mu.Unlock()
		return ErrQueueFull
	}
	select {
	case l.queue <- line:
		l.pendingBytes += len(line)
		l.mu.Unlock()
		return nil
	default:
		l.dropped++
		l.mu.Unlock()
		return ErrQueueFull
	}
}

// appendLine runs only on the worker goroutine.
func (l *Log) appendLine(line []byte) error {
	if l.f == nil {
		if err := l.open(); err != nil {
			return err
		}
	}
	if l.repair {
		if err := l.f.Truncate(l.repairTo); err != nil {
			return fmt.Errorf("repair incomplete audit record: %w", err)
		}
		l.size, l.repair = l.repairTo, false
	}
	if l.size+int64(len(line)) > maxFileBytes {
		if err := l.rotate(); err != nil {
			return fmt.Errorf("audit rotation: %w", err)
		}
	}
	before := l.size
	n, writeErr := l.f.Write(line)
	if writeErr == nil && n == len(line) {
		l.size += int64(n)
		return nil
	}
	if writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	if err := l.f.Truncate(before); err != nil {
		l.repair, l.repairTo = true, before
		return errors.Join(writeErr, fmt.Errorf("remove incomplete audit record: %w", err))
	}
	return writeErr
}

func (l *Log) run() {
	defer close(l.done)
	failures := 0
	var reportedDrops uint64
	for line := range l.queue {
		l.mu.Lock()
		l.pendingBytes -= len(line)
		dropped := l.dropped
		l.mu.Unlock()
		if dropped != reportedDrops && (reportedDrops == 0 || dropped-reportedDrops >= 1000) {
			log.Printf("audit: queue overflow; %d total events dropped", dropped)
			reportedDrops = dropped
		}
		var err error
		if l.writeLineFn != nil {
			err = l.writeLineFn(line)
		} else {
			err = l.appendLine(line)
		}
		if err != nil {
			if l.workerErr == nil {
				l.workerErr = fmt.Errorf("audit write: %w", err)
			}
			failures++
			if failures == 1 {
				log.Printf("audit: disk write failed: %v", err)
			}
		} else if failures != 0 {
			log.Printf("audit: disk writes resumed; %d events lost during failure", failures)
			failures = 0
		}
	}
	if failures != 0 {
		log.Printf("audit: %d queued events could not be written", failures)
	}
	l.mu.Lock()
	dropped := l.dropped
	l.mu.Unlock()
	if dropped != reportedDrops {
		log.Printf("audit: queue overflow; %d total events dropped", dropped)
	}
	if l.f != nil {
		if l.repair {
			if err := l.f.Truncate(l.repairTo); err != nil {
				l.workerErr = errors.Join(l.workerErr, fmt.Errorf("audit final repair: %w", err))
			}
		}
		if err := l.f.Sync(); err != nil {
			l.workerErr = errors.Join(l.workerErr, fmt.Errorf("audit sync: %w", err))
		}
		if err := l.f.Close(); err != nil {
			l.workerErr = errors.Join(l.workerErr, fmt.Errorf("audit close: %w", err))
		}
		l.f = nil
	}
}

// Close stops accepting events and waits until the worker has processed
// every event that was accepted before Close.
func (l *Log) Close() error {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.queue)
	}
	dropped := l.dropped
	l.mu.Unlock()
	<-l.done
	if dropped != 0 {
		return errors.Join(l.workerErr, fmt.Errorf("audit dropped %d events from a full queue", dropped))
	}
	return l.workerErr
}

func RequestID() string {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("system randomness unavailable")
	}
	return hex.EncodeToString(id[:])
}
