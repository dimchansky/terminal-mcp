package audit

import (
	"bufio"
	"bytes"
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
	dir := privateAuditDir(t)
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
	dir := privateAuditDir(t)
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
	if err != nil {
		t.Fatal(err)
	}
	if err := checkAuditPrivateFile(path, info); err != nil {
		t.Fatalf("unsafe log permissions: %v", err)
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
	dir := privateAuditDir(t)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if writer, err := Open(filepath.Join(dir, "audit.jsonl")); err == nil {
			_ = writer.Close()
			t.Fatal("accepted public audit directory")
		}
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// A separate Windows test modifies the actual DACL rather than
	// treating os.Chmod as a Windows access-control mechanism.
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

func TestConcurrentCallsRotateWithoutDiscardingEvents(t *testing.T) {
	dir := privateAuditDir(t)
	path := filepath.Join(dir, "audit.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const workers, perWorker = 6, 9000
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				if err := log.Write(NewEvent(Event{RequestID: RequestID(), Tool: "file_read", Phase: "finish", Status: "ok"})); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatal("expected rotated log:", err)
	}

	// Count all records across the active file and archives. The test
	// volume fits in a few 4-MiB files, with no eviction permitted.
	records := 0
	for _, file := range []string{path, path + ".1", path + ".2", path + ".3"} {
		data, err := os.ReadFile(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > maxFileBytes {
			t.Fatalf("bad rotation size: %s %+v", file, info)
		}
		if err := checkAuditPrivateFile(file, info); err != nil {
			t.Fatalf("bad rotation permissions for %s: %v", file, err)
		}
		if len(data) > 0 && data[len(data)-1] != '\n' {
			t.Fatalf("incomplete JSONL tail: %s", file)
		}
		for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'}) {
			if len(line) == 0 {
				continue
			}
			if !json.Valid(line) {
				t.Fatalf("invalid JSONL record in %s: %.100s", file, line)
			}
			records++
		}
	}
	if records != workers*perWorker {
		t.Fatalf("kept %d audit records, want %d", records, workers*perWorker)
	}
	if err := log.Write(NewEvent(Event{Tool: "terminal_list", Phase: "start"})); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close: %v", err)
	}
}

func TestEventKeepsTimeFromCaller(t *testing.T) {
	event := NewEvent(Event{Tool: "terminal_start", Phase: "start"})
	stamp := event.Time
	if stamp == "" {
		t.Fatal("constructor omitted time")
	}
	dir := privateAuditDir(t)
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

func TestCloseWaitsForInFlightSynchronousWrite(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var writes, closes atomic.Int64
	log := newLog(&testSink{
		write: func([]byte) error {
			writes.Add(1)
			close(entered)
			<-release
			return nil
		},
		finish: func() error { closes.Add(1); return nil },
	})
	event := NewEvent(Event{Tool: "terminal_list", Phase: "start"})
	writeDone := make(chan error, 1)
	go func() { writeDone <- log.Write(event) }()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- log.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before pending Write completed: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 1 || closes.Load() != 1 {
		t.Fatalf("write/close counts = %d/%d, want 1/1", writes.Load(), closes.Load())
	}
	if err := log.Write(event); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}
}

func TestRecoverIncompleteLineOnOpen(t *testing.T) {
	dir := privateAuditDir(t)
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

func TestInvalidAndOversizedEventsNeverReachSink(t *testing.T) {
	var writes atomic.Int64
	writer := newLog(&testSink{write: func([]byte) error {
		writes.Add(1)
		return nil
	}})
	if err := writer.Write(Event{Tool: "terminal_list", Phase: "start"}); err == nil {
		t.Fatal("accepted event without timestamp")
	}
	if err := writer.Write(NewEvent(Event{Tool: "terminal_start", Phase: "start", Args: make(chan int)})); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("event that cannot be serialized must return ErrInvalidEvent: %v", err)
	}
	if err := writer.Write(NewEvent(Event{Tool: "terminal_start", Phase: "start", Args: json.RawMessage("{")})); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("invalid raw JSON must return ErrInvalidEvent: %v", err)
	}
	huge := NewEvent(Event{Tool: "terminal_start", Phase: "start", Args: strings.Repeat("x", int(maxFileBytes))})
	if err := writer.Write(huge); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("oversized event must return ErrEventTooLarge: %v", err)
	}
	if got := writes.Load(); got != 0 {
		t.Fatalf("invalid events reached sink: %d", got)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseReturnsSameSinkError(t *testing.T) {
	var closes atomic.Int64
	sentinel := errors.New("injected sync error")
	writer := newLog(&testSink{finish: func() error {
		closes.Add(1)
		return sentinel
	}})
	if err := writer.Close(); !errors.Is(err, sentinel) {
		t.Fatalf("Close did not return sink error: %v", err)
	}
	if err := writer.Close(); !errors.Is(err, sentinel) {
		t.Fatalf("repeated Close lost original error: %v", err)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("sink Close called %d times, want once", got)
	}
}

func TestWriteIsVisibleOnDiskBeforeClose(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	}()
	event := NewEvent(Event{ServerID: "before-close", Tool: "terminal_start", Phase: "start"})
	if err := writer.Write(event); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Event
	if err := json.Unmarshal(bytes.TrimSpace(data), &got); err != nil {
		t.Fatalf("Write returned before record was complete: %v", err)
	}
	if got.Time != event.Time || got.ServerID != event.ServerID || got.Tool != event.Tool {
		t.Fatalf("audit entry not visible after successful Write: %+v", got)
	}
}
