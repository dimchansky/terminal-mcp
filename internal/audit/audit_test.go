package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFullCommandArgumentExceedsOldMetadataLimit(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	command := strings.Repeat("x", 64000)
	if err := log.Write(NewEvent(Event{RequestID: RequestID(), Tool: "terminal_start", Phase: "start", Args: map[string]any{"argv": []string{"/bin/sh", "-c", command}}})); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), command) {
		t.Fatalf("full command truncated or unavailable: %v", err)
	}
}

func TestEventsPrivateAndMetadataOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := RequestID()
	if err := log.Write(NewEvent(Event{RequestID: id, Tool: "terminal_start", Phase: "start"})); err != nil {
		t.Fatal(err)
	}
	if err := log.Write(NewEvent(Event{RequestID: id, Tool: "terminal_start", Phase: "finish", Status: "applied", SessionID: "s1", DurationMS: 2})); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe log permissions: %v %v", info, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "password") || strings.Count(string(b), "\n") != 2 {
		t.Fatalf("unexpected audit content: %q", b)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(b)))
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event.Time == "" || event.RequestID != id || event.Tool != "terminal_start" {
			t.Fatalf("bad event: %+v", event)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestRefusesPublicDirectoryAndSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(dir, "audit.jsonl")); err == nil {
		t.Fatal("accepted public audit directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "victim")
	if err := os.WriteFile(secret, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "audit.jsonl")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Fatal("accepted symlink audit path")
	}
	if data, err := os.ReadFile(secret); err != nil || string(data) != "preserve" {
		t.Fatalf("victim changed: %q %v", data, err)
	}
}

func TestConcurrentCallsRotateWithBoundedBackups(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 6
	const perWorker = 9000
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				for {
					err := log.Write(NewEvent(Event{RequestID: RequestID(), Tool: "file_read", Phase: "finish", Status: "ok"}))
					if err == nil {
						break
					}
					if !errors.Is(err, ErrQueueFull) {
						t.Errorf("write: %v", err)
						return
					}
					runtime.Gosched()
				}
			}
		}()
	}
	wg.Wait()
	// The stress test intentionally retries when the bounded queue is full.
	// Rejected attempts still count as lost audit events.
	if err := log.Close(); err != nil && !strings.Contains(err.Error(), "audit dropped") {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatal("expected rotated log:", err)
	}
	for _, file := range []string{path, path + ".1"} {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > maxFileBytes || info.Mode().Perm() != 0600 {
			t.Fatalf("bad rotation size/permission: %s %+v", file, info)
		}
	}
	if err := log.Write(NewEvent(Event{Tool: "terminal_list", Phase: "start"})); err == nil {
		t.Fatal("accepted write after close")
	}
}

func TestEventKeepsTimeFromCaller(t *testing.T) {
	event := NewEvent(Event{Tool: "terminal_start", Phase: "start"})
	stamp := event.Time
	if stamp == "" {
		t.Fatal("constructor omitted time")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Write(event); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	lines, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(lines))), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Time != stamp {
		t.Fatalf("timestamp changed between enqueue and flush: %q != %q", saved.Time, stamp)
	}
}

func TestQueueIsNonblockingAndCloseDrains(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	log, err := Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var processed atomic.Int64
	log.writeLineFn = func([]byte) error {
		if processed.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	event := NewEvent(Event{Tool: "terminal_list", Phase: "start"})
	if err := log.Write(event); err != nil {
		t.Fatal(err)
	}
	<-entered
	for i := 0; i < queueCapacity; i++ {
		if err := log.Write(event); err != nil {
			t.Fatalf("queue slot %d: %v", i, err)
		}
	}
	start := time.Now()
	if err := log.Write(event); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected full queue, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("queue overflow blocked the caller")
	}
	done := make(chan error, 1)
	go func() { done <- log.Close() }()
	select {
	case err := <-done:
		t.Fatalf("Close did not wait for queued events: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "dropped") {
		t.Fatalf("Close must report dropped event: %v", err)
	}
	if got := processed.Load(); got != int64(queueCapacity+1) {
		t.Fatalf("processed %d, want %d", got, queueCapacity+1)
	}
	if err := log.Write(event); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
	if err := log.Close(); err == nil {
		t.Fatal("second Close must report earlier loss")
	}
}

func TestRecoverIncompleteLineOnOpen(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	valid := "{\"time\":\"valid\",\"tool\":\"file_read\"}\n"
	if err := os.WriteFile(path, []byte(valid+"{\"incomplete\":"), 0600); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Write(NewEvent(Event{Tool: "terminal_list", Phase: "start"})); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 || lines[0] != strings.TrimSpace(valid) {
		t.Fatalf("incomplete tail not removed: %q", data)
	}
	for _, line := range lines {
		var entry Event
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid JSONL line %q: %v", line, err)
		}
	}
}

func TestWriteFailureDoesNotStopQueueWorker(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var count atomic.Int64
	writer.writeLineFn = func([]byte) error {
		if count.Add(1) == 1 {
			return errors.New("temporary disk error")
		}
		return nil
	}
	for i := 0; i < 3; i++ {
		if err := writer.Write(NewEvent(Event{Tool: "terminal_list", Phase: "start"})); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err == nil || !strings.Contains(err.Error(), "temporary disk error") {
		t.Fatalf("expected diagnostic error from background worker: %v", err)
	}
	if got := count.Load(); got != 3 {
		t.Fatalf("worker stopped after write error: %d events processed", got)
	}
}
