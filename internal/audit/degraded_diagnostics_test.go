package audit

import (
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockedDiagnostics simulates stderr being blocked (for example, a stalled
// filesystem). This test ensures diagnostics never hold the journal mutex.
type blockedDiagnostics struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (w *blockedDiagnostics) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(data), nil
}

func TestStalledDiagnosticOutputCannotBlockAuditAdmissionOrClose(t *testing.T) {
	original := slog.Default()
	logOutput := &blockedDiagnostics{entered: make(chan struct{}), release: make(chan struct{})}
	slog.SetDefault(slog.New(slog.NewTextHandler(logOutput, nil)))
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(logOutput.release) })
		slog.SetDefault(original)
	}()

	fault := errors.New("injected audit sink failure")
	recorder := newLog(&testSink{write: func([]byte) error { return fault }})
	first := make(chan error, 1)
	event := NewEvent(Event{Tool: "file_write", Phase: "request_start"})
	go func() { first <- recorder.Start(event) }()
	select {
	case <-logOutput.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first error failed to reach diagnostics")
	}
	if status := recorder.Status(); status.State != "degraded" || !strings.Contains(status.LastError, fault.Error()) {
		t.Fatalf("first audit fault was not visible before diagnostic returned: %+v", status)
	}
	checkDone := make(chan error, 1)
	go func() {
		checkDone <- recorder.Start(NewEvent(Event{Tool: "terminal_read", Phase: "request_start"}))
	}()
	select {
	case err := <-checkDone:
		if !errors.Is(err, fault) {
			t.Fatalf("retry bypassed broken sink while diagnostics stalled: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("pending stderr write held journal mutex or health admission")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- recorder.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close refused to complete while diagnostics blocked: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stalled diagnostics held journal mutex during Close")
	}
	releaseOnce.Do(func() { close(logOutput.release) })
	if err := <-first; !errors.Is(err, fault) {
		t.Fatalf("original write error changed: %v", err)
	}
}

func TestCloseFailureDoesNotHoldLockWhileDiagnosticBlocked(t *testing.T) {
	original := slog.Default()
	logOutput := &blockedDiagnostics{entered: make(chan struct{}), release: make(chan struct{})}
	slog.SetDefault(slog.New(slog.NewTextHandler(logOutput, nil)))
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(logOutput.release) })
		slog.SetDefault(original)
	}()

	fault := errors.New("injected sync failure")
	recorder := newLog(&testSink{finish: func() error { return fault }})
	closeResult := make(chan error, 1)
	go func() { closeResult <- recorder.Close() }()
	select {
	case <-logOutput.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("close failure did not report diagnostics")
	}
	startResult := make(chan error, 1)
	go func() {
		startResult <- recorder.Start(NewEvent(Event{Tool: "file_write", Phase: "request_start"}))
	}()
	select {
	case err := <-startResult:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("closed log unexpectedly admitted request: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Close error diagnostic kept journal mutex locked")
	}
	result := make(chan error, 1)
	go func() { result <- recorder.Close() }()
	select {
	case err := <-result:
		if !errors.Is(err, fault) {
			t.Fatalf("repeated Close changed error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("repeated Close blocked on stderr diagnostics")
	}
	releaseOnce.Do(func() { close(logOutput.release) })
	if err := <-closeResult; !errors.Is(err, fault) {
		t.Fatalf("original Close lost its error: %v", err)
	}
}
