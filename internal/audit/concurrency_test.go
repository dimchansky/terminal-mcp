package audit

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestQueueByteBudgetIncludesInFlightRecord(t *testing.T) {
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
	e := NewEvent(Event{Tool: "terminal_start", Phase: "start", Args: strings.Repeat("a", 1<<20)})
	if err := l.Write(e); err != nil {
		t.Fatal(err)
	}
	<-entered
	// Seven 1 MiB records plus the worker's record fit within 8 MiB;
	// the next attempt must be rejected without waiting for disk I/O.
	accepted := int64(1)
	for {
		err := l.Write(e)
		if errors.Is(err, ErrQueueFull) {
			break
		}
		if err != nil {
			t.Fatalf("unexpected write error: %v", err)
		}
		accepted++
		if accepted > 8 {
			t.Fatalf("byte budget excluded in-flight record; admitted %d MiB records", accepted)
		}
	}
	l.mu.Lock()
	pending := l.pendingBytes
	l.mu.Unlock()
	if pending > queueByteLimit {
		t.Fatalf("queue exceeds byte budget: %d > %d", pending, queueByteLimit)
	}
	close(release)
	if err := l.Close(); err == nil || !strings.Contains(err.Error(), "dropped 1") {
		t.Fatalf("did not count byte-budget rejection: %v", err)
	}
	if got := written.Load(); got != accepted {
		t.Fatalf("accepted %d events but worker wrote %d", accepted, got)
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
	for i := 0; i < producers; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			<-start
			for j := 0; j < writesPerProducer; j++ {
				err := l.Write(NewEvent(Event{Tool: "file_search", Phase: "start"}))
				switch {
				case err == nil:
					accepted.Add(1)
					firstOnce.Do(func() { close(firstAccepted) })
				case errors.Is(err, ErrQueueFull), errors.Is(err, ErrClosed):
					// Expected when producers race with a bounded queue and Close.
				default:
					errorsSeen <- err
					return
				}
			}
		}()
	}
	close(start)
	// Close races with writers after at least one accepted event.
	<-firstAccepted
	var wg sync.WaitGroup
	results := make(chan error, closers)
	for i := 0; i < closers; i++ {
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
		t.Fatalf("accepted %d events but worker wrote %d", accepted.Load(), got)
	}
	if got := closed.Load(); got != 1 {
		t.Fatalf("worker closed sink %d times, want once", got)
	}
	if err := l.Write(NewEvent(Event{Tool: "terminal_list", Phase: "start"})); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close: %v", err)
	}
}

func TestWorkerReportsEveryDiskWriteFailure(t *testing.T) {
	var calls atomic.Int64
	s := &testSink{write: func([]byte) error {
		if calls.Add(1) <= 2 {
			return errors.New("injected disk error")
		}
		return nil
	}}
	l := newLog(s)
	for i := 0; i < 4; i++ {
		if err := l.Write(NewEvent(Event{Tool: "file_read", Phase: "finish"})); err != nil {
			t.Fatal(err)
		}
	}
	err := l.Close()
	if err == nil || !strings.Contains(err.Error(), "injected disk error") ||
		!strings.Contains(err.Error(), "lost 2 events") {
		t.Fatalf("disk failure count missing from Close: %v", err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("worker stopped after disk failure: %d of 4", got)
	}
	if again := l.Close(); again == nil || again.Error() != err.Error() {
		t.Fatalf("repeat Close changed reported losses: %v vs %v", again, err)
	}
}
