package audit

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A slow disk must apply backpressure rather than dropping concurrent events.
func TestConcurrentWritesWaitForSlowSink(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var written atomic.Int64
	l := newLog(&testSink{write: func([]byte) error {
		if written.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}})
	event := NewEvent(Event{Tool: "terminal_start", Phase: "start"})
	firstDone := make(chan error, 1)
	go func() { firstDone <- l.Write(event) }()
	<-entered

	const waiters = 24
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	for range waiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- l.Write(event)
		}()
	}
	// With the sink blocked, no competing Write may return success (or a
	// queue-overflow error). All calls must wait their turn at the sink.
	select {
	case err := <-errs:
		t.Fatalf("concurrent Write returned before sink was released: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Write failed: %v", err)
		}
	}
	if got := written.Load(); got != waiters+1 {
		t.Fatalf("sink handled %d events; want %d", got, waiters+1)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentWritesSerializeSink(t *testing.T) {
	var active, overlaps, written atomic.Int64
	l := newLog(&testSink{write: func([]byte) error {
		if active.Add(1) != 1 {
			overlaps.Add(1)
		}
		runtime.Gosched()
		written.Add(1)
		active.Add(-1)
		return nil
	}})
	const writers, perWriter = 16, 200
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWriter {
				if err := l.Write(NewEvent(Event{Tool: "file_read", Phase: "finish"})); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if got := overlaps.Load(); got != 0 {
		t.Fatalf("sink called concurrently %d times", got)
	}
	if got := written.Load(); got != writers*perWriter {
		t.Fatalf("sink handled %d, want %d", got, writers*perWriter)
	}
}

func TestConcurrentWriteCloseAndRepeatClose(t *testing.T) {
	const producers, writesPerProducer, closers = 48, 100, 8
	var written, closed atomic.Int64
	l := newLog(&testSink{
		write:  func([]byte) error { written.Add(1); return nil },
		finish: func() error { closed.Add(1); return nil },
	})
	start := make(chan struct{})
	firstAccepted := make(chan struct{})
	var firstOnce sync.Once
	var accepted atomic.Int64
	var writers sync.WaitGroup
	errorsSeen := make(chan error, producers)
	for range producers {
		writers.Add(1)
		go func() {
			defer writers.Done()
			<-start
			for range writesPerProducer {
				err := l.Write(NewEvent(Event{Tool: "file_search", Phase: "start"}))
				switch {
				case err == nil:
					accepted.Add(1)
					firstOnce.Do(func() { close(firstAccepted) })
				case errors.Is(err, ErrClosed):
					// Close races with these writes. No queue-overflow error exists.
				default:
					errorsSeen <- err
					return
				}
			}
		}()
	}
	close(start)
	<-firstAccepted
	var wg sync.WaitGroup
	results := make(chan error, closers)
	for range closers {
		wg.Add(1)
		go func() { defer wg.Done(); results <- l.Close() }()
	}
	writers.Wait()
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("unexpected concurrent Write error: %v", err)
	}
	close(results)
	var last string
	first := true
	for err := range results {
		value := ""
		if err != nil {
			value = err.Error()
		}
		if first {
			first = false
			last = value
		} else if last != value {
			t.Fatalf("Close returned inconsistent results: %q versus %q", last, value)
		}
	}
	if got := written.Load(); got != accepted.Load() {
		t.Fatalf("accepted %d events but sink wrote %d", accepted.Load(), got)
	}
	if got := closed.Load(); got != 1 {
		t.Fatalf("sink closed %d times, want once", got)
	}
	if err := l.Write(NewEvent(Event{Tool: "terminal_list", Phase: "start"})); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close: %v", err)
	}
}

func TestWriteReportsEveryDiskFailureImmediately(t *testing.T) {
	var calls atomic.Int64
	sentinel := errors.New("injected disk error")
	l := newLog(&testSink{write: func([]byte) error {
		if calls.Add(1) <= 2 {
			return sentinel
		}
		return nil
	}})
	for i := range 4 {
		err := l.Write(NewEvent(Event{Tool: "file_read", Phase: "finish"}))
		if i < 2 && !errors.Is(err, sentinel) {
			t.Fatalf("Write %d did not report sink error immediately: %v", i, err)
		}
		if i >= 2 && err != nil {
			t.Fatalf("Write %d failed after sink recovered: %v", i, err)
		}
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("sink handled %d of 4 events", got)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close should report only its own failure, got %v", err)
	}
}
