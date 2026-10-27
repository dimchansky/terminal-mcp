package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuditAppendFailureRetriesWithoutAdmittingUnauditedWork(t *testing.T) {
	failure := errors.New("injected disk full")
	var writes atomic.Int32
	recorder := newLog(&testSink{write: func([]byte) error {
		if writes.Add(1) <= 2 {
			return failure
		}
		return nil
	}})
	event := NewEvent(Event{Tool: "terminal_start", Phase: "request_start"})
	for range 2 {
		if err := recorder.Start(event); !errors.Is(err, failure) {
			t.Fatalf("failed audit write admitted work: %v", err)
		}
		if err := recorder.Ready(); !errors.Is(err, ErrDegraded) || !errors.Is(err, failure) {
			t.Fatalf("health check lost the failure: %v", err)
		}
		if status := recorder.Status(); status.State != "degraded" ||
			!strings.Contains(status.LastError, failure.Error()) {
			t.Fatalf("incorrect degraded status: %+v", status)
		}
	}
	// Only a *successful* request-start append permits new execution.
	if err := recorder.Start(event); err != nil {
		t.Fatalf("recovered sink did not admit its first audited request: %v", err)
	}
	if err := recorder.Ready(); err != nil {
		t.Fatalf("successful append did not restore health: %v", err)
	}
	if status := recorder.Status(); status.State != "ok" || status.LastError != "" {
		t.Fatalf("successful append did not clear transient error: %+v", status)
	}
	if got := writes.Load(); got != 3 {
		t.Fatalf("sink attempted %d writes; want two failures and one success", got)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditCompletionWriteCanRestoreTransientHealth(t *testing.T) {
	var writes atomic.Int32
	recorder := newLog(&testSink{write: func([]byte) error {
		if writes.Add(1) == 1 {
			return errors.New("temporary write failure")
		}
		return nil
	}})
	start := NewEvent(Event{Tool: "file_write", Phase: "request_start"})
	if err := recorder.Start(start); err == nil {
		t.Fatal("initial request_start unexpectedly succeeded")
	}
	if err := recorder.Write(NewEvent(Event{Tool: "file_write", Phase: "operation_finish"})); err != nil {
		t.Fatalf("completion did not recover sink: %v", err)
	}
	if recorder.Status().State != "ok" {
		t.Fatalf("successful completion did not restore health: %+v", recorder.Status())
	}
	if err := recorder.Start(start); err != nil {
		t.Fatalf("audited request after recovery refused: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPermanentAuditFailureCannotBeClearedByAppending(t *testing.T) {
	const reason = "audit observer panic"
	var writes atomic.Int32
	recorder := newLog(&testSink{write: func([]byte) error {
		writes.Add(1)
		return nil
	}})
	recorder.Degrade(errors.New(reason))
	if err := recorder.Write(NewEvent(Event{Tool: "file_write", Phase: "operation_finish"})); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Start(NewEvent(Event{Tool: "file_read", Phase: "request_start"})); !errors.Is(err, ErrDegraded) {
		t.Fatalf("permanent invariant failure allowed work: %v", err)
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("unhealthy Start appended %d new events", got-1)
	}
	if got := recorder.Status(); got.State != "degraded" || !strings.Contains(got.LastError, reason) {
		t.Fatalf("permanent failure was lost: %+v", got)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUntimestampedCompletionIsPermanent(t *testing.T) {
	recorder := newLog(&testSink{})
	if err := recorder.Write(Event{Tool: "file_write", Phase: "operation_finish"}); err == nil {
		t.Fatal("untimestamped completion unexpectedly accepted")
	}
	if err := recorder.Start(NewEvent(Event{Tool: "file_read", Phase: "request_start"})); !errors.Is(err, ErrDegraded) {
		t.Fatalf("missing completion metadata allowed new work: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUnrepresentableCompletionDoesNotAutoRecover(t *testing.T) {
	recorder := newLog(&testSink{})
	if err := recorder.Write(NewEvent(Event{Tool: "file_write", Phase: "operation_finish", Args: make(chan int)})); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("unrepresentable completion error: %v", err)
	}
	if err := recorder.Start(NewEvent(Event{Tool: "file_read", Phase: "request_start"})); !errors.Is(err, ErrDegraded) {
		t.Fatalf("lost completion allowed new work: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditPartialWriteRecoveryRepairsJSONLBeforeNextRequest(t *testing.T) {
	path := filepath.Join(privateAuditDir(t), "audit.jsonl")
	recorder, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sink := recorder.sink.(*fileSink)
	actual := sink.f
	sink.f = &faultFile{
		auditFile: actual,
		writeFn: func(b []byte) (int, error) {
			n, err := actual.Write(b[:min(9, len(b))])
			if err != nil {
				return n, err
			}
			return n, io.ErrShortWrite
		},
		truncateFn: func(int64) error { return errors.New("disk full during repair") },
	}
	first := NewEvent(Event{RequestID: "failed-request", Tool: "file_write", Phase: "request_start"})
	if err := recorder.Start(first); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("partial write did not deny work: %v", err)
	}
	if !sink.repair || recorder.Status().State != "degraded" {
		t.Fatal("partial record was not marked for repair")
	}
	// Disk space has returned. Reuse the original file descriptor and let
	// the normal admission path perform the pending repair before appending.
	sink.f = actual
	second := NewEvent(Event{RequestID: "recovered-request", Tool: "file_write", Phase: "request_start"})
	if err := recorder.Start(second); err != nil {
		t.Fatalf("failed to repair and admit an audited request: %v", err)
	}
	if recorder.Status().State != "ok" {
		t.Fatalf("health did not recover: %+v", recorder.Status())
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 1 || !json.Valid(lines[0]) {
		t.Fatalf("partial JSONL record retained after recovery: %q", data)
	}
	var got Event
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatal(err)
	}
	if got.RequestID != second.RequestID {
		t.Fatalf("recovery recorded a failed request as executed: %+v", got)
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
	// Recovery must repair the descriptor/rotation and resume admission
	// after the first successful append, without rewriting any archive.
	s.rename = os.Rename
	if err := recorder.Write(NewEvent(Event{Tool: "file_write", Phase: "operation_finish", Status: "applied"})); err != nil {
		t.Fatalf("recovered sink could not preserve later completion: %v", err)
	}
	if recorder.Status().State != "ok" {
		t.Fatalf("successful completion did not restore health: %+v", recorder.Status())
	}
	if err := recorder.Start(event); err != nil {
		t.Fatalf("recovered sink refused audited work: %v", err)
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
