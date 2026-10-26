package audit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuditAppendFailureIsStickyAndBlocksFutureStarts(t *testing.T) {
	failure := errors.New("injected disk full")
	var writes atomic.Int32
	recorder := newLog(&testSink{write: func([]byte) error {
		if writes.Add(1) == 1 {
			return failure
		}
		return nil
	}})
	event := NewEvent(Event{Tool: "terminal_start", Phase: "request_start"})
	if err := recorder.Start(event); !errors.Is(err, failure) {
		t.Fatalf("did not return immediate append error: %v", err)
	}
	if err := recorder.Ready(); !errors.Is(err, ErrDegraded) || !errors.Is(err, failure) {
		t.Fatalf("health check did not retain first failure: %v", err)
	}
	status := recorder.Status()
	if status.State != "degraded" || !strings.Contains(status.LastError, failure.Error()) {
		t.Fatalf("incorrect degraded status: %+v", status)
	}

	// A record from work already admitted may still be written after a
	// transient disk fault. That must not reset the sticky failure.
	if err := recorder.Write(NewEvent(Event{Tool: "terminal_start", Phase: "operation_finish", Status: "applied"})); err != nil {
		t.Fatalf("could not record an already-accepted completion: %v", err)
	}
	if err := recorder.Start(event); !errors.Is(err, ErrDegraded) {
		t.Fatalf("subsequent new work allowed despite degraded audit: %v", err)
	}
	if got := writes.Load(); got != 2 {
		t.Fatalf("unexpected sink attempts: %d (expected failed start and completion only)", got)
	}
	// The first failure is the diagnostic cause, even if later failures
	// occur or a caller attempts to degrade the log twice.
	recorder.Degrade(errors.New("new unrelated error"))
	if !strings.Contains(recorder.Status().LastError, "injected disk full") {
		t.Fatalf("first error overwritten: %+v", recorder.Status())
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditStatusDoesNotBlockOnSlowFileSink(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	recorder := newLog(&testSink{write: func([]byte) error {
		close(entered)
		<-release
		return nil
	}})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- recorder.Start(NewEvent(Event{Tool: "terminal_list", Phase: "request_start"}))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("disk write did not start")
	}
	statusDone := make(chan Status, 1)
	go func() { statusDone <- recorder.Status() }()
	select {
	case status := <-statusDone:
		if status.State != "ok" {
			t.Fatalf("wrong initial health: %+v", status)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("audit status blocked on a stalled disk writer")
	}
	recorder.Degrade(errors.New("injected external I/O alarm"))
	if status := recorder.Status(); status.State != "degraded" {
		t.Fatalf("health snapshot did not update: %+v", status)
	}
	once.Do(func() { close(release) })
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err := recorder.Start(NewEvent(Event{Tool: "file_read", Phase: "request_start"})); !errors.Is(err, ErrDegraded) {
		t.Fatalf("healthy sink improperly cleared sticky failure: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditRotationErrorDegradesButCanRetainOldCompletion(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	recorder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recorder.Close() }()
	// Set up a rotation without needing a 4 MiB JSON test payload.
	s := recorder.sink.(*fileSink)
	if err := s.appendLine(almostFullLogLine()); err != nil {
		t.Fatal(err)
	}
	rotateFailure := errors.New("injected rename failure")
	s.rename = func(_, _ string) error { return rotateFailure }
	event := NewEvent(Event{Tool: "file_write", Phase: "request_start"})
	if err := recorder.Start(event); !errors.Is(err, rotateFailure) {
		t.Fatalf("rotation error not propagated: %v", err)
	}
	if recorder.Status().State != "degraded" {
		t.Fatalf("rotation failure did not degrade audit: %+v", recorder.Status())
	}
	// The failed rotation is recoverable by the original sink on a later
	// outcome append, but admission still remains blocked.
	s.rename = os.Rename
	if err := recorder.Write(NewEvent(Event{Tool: "file_write", Phase: "operation_finish", Status: "applied"})); err != nil {
		t.Fatalf("recovered sink could not preserve later completion: %v", err)
	}
	if err := recorder.Start(event); !errors.Is(err, ErrDegraded) {
		t.Fatalf("unintentionally reenabled new work after rotation recovery: %v", err)
	}
}

func TestUnrepresentableInputDoesNotDegradeHealthyLog(t *testing.T) {
	recorder := newLog(&testSink{})
	bad := NewEvent(Event{Tool: "file_write", Phase: "request_start", Args: make(chan int)})
	if err := recorder.Start(bad); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("expected serialization refusal: %v", err)
	}
	if err := recorder.Ready(); err != nil {
		t.Fatalf("pre-execution input rejection poisoned health: %v", err)
	}
	if recorder.Status().State != "ok" {
		t.Fatalf("input rejection degraded healthy audit: %+v", recorder.Status())
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}
