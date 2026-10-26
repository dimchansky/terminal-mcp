package mutation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type observedEvent struct {
	phase   string
	id      string
	receipt Receipt
}

type channelObserver struct{ events chan observedEvent }

func newChannelObserver() *channelObserver {
	return &channelObserver{events: make(chan observedEvent, 8)}
}

func (o *channelObserver) Started(id string) {
	o.events <- observedEvent{phase: "start", id: id}
}

func (o *channelObserver) Finished(receipt Receipt) {
	o.events <- observedEvent{phase: "finish", id: receipt.OperationID, receipt: receipt}
}

func receiveObserved(t *testing.T, o *channelObserver) observedEvent {
	t.Helper()
	select {
	case event := <-o.events:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("missing operation observer event")
		return observedEvent{}
	}
}

func assertNoObserved(t *testing.T, o *channelObserver) {
	t.Helper()
	select {
	case event := <-o.events:
		t.Fatalf("unexpected operation observer event: %+v", event)
	default:
	}
}

func TestObserverTracksEffectAfterRequestCancellation(t *testing.T) {
	var l Ledger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := newChannelObserver()
	ctx = WithObserver(ctx, first)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var effects atomic.Int32
	type result struct {
		receipt  Receipt
		replayed bool
		err      error
	}
	waiting := make(chan result, 1)
	go func() {
		r, replayed, err := l.Do(ctx, 4, "same", "file_write", "effect", func() Receipt {
			effects.Add(1)
			<-release
			return Receipt{Status: "applied"}
		})
		waiting <- result{r, replayed, err}
	}()
	if e := receiveObserved(t, first); e.phase != "start" || e.id != "same" {
		t.Fatalf("unexpected start: %+v", e)
	}
	cancel()
	select {
	case r := <-waiting:
		if !errors.Is(r.err, context.Canceled) || r.replayed {
			t.Fatalf("cancelled request: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not return after cancellation")
	}
	assertNoObserved(t, first)
	releaseOnce.Do(func() { close(release) })
	if e := receiveObserved(t, first); e.phase != "finish" || e.id != "same" || e.receipt.Status != "applied" {
		t.Fatalf("unexpected final effect result: %+v", e)
	}
	second := newChannelObserver()
	r, replayed, err := l.Do(WithObserver(context.Background(), second), 4, "same", "file_write", "effect", func() Receipt {
		effects.Add(1)
		return Receipt{Status: "failed"}
	})
	if err != nil || !replayed || r.Status != "applied" || effects.Load() != 1 {
		t.Fatalf("replay executed effect or lost result: %+v, replayed=%v, err=%v", r, replayed, err)
	}
	assertNoObserved(t, second)
}

func TestObserverExcludesConflictAndReportsRecoveredPanic(t *testing.T) {
	var l Ledger
	original := newChannelObserver()
	r, replayed, err := l.Do(WithObserver(context.Background(), original), 4, "panic-effect", "write", "original", func() Receipt {
		panic("broken operation")
	})
	if err != nil || replayed || r.Status != "unknown" || !strings.Contains(r.Error, "outcome may be unknown") {
		t.Fatalf("panicked effect: %+v replayed=%v err=%v", r, replayed, err)
	}
	if e := receiveObserved(t, original); e.phase != "start" || e.id != "panic-effect" {
		t.Fatalf("start: %+v", e)
	}
	if e := receiveObserved(t, original); e.phase != "finish" || e.receipt.Status != "unknown" ||
		e.receipt.OperationID != "panic-effect" {
		t.Fatalf("panic finish: %+v", e)
	}
	second := newChannelObserver()
	_, replayed, err = l.Do(WithObserver(context.Background(), second), 4, "panic-effect", "write", "different", func() Receipt {
		t.Error("conflicting effect executed")
		return Receipt{Status: "applied"}
	})
	if err == nil || !replayed || !strings.Contains(err.Error(), "operation_id_conflict") {
		t.Fatalf("conflicting effect accepted: replayed=%v err=%v", replayed, err)
	}
	assertNoObserved(t, second)
}

type blockingFinishObserver struct {
	started   chan struct{}
	finishing chan struct{}
	release   chan struct{}
}

func (o *blockingFinishObserver) Started(string) { close(o.started) }
func (o *blockingFinishObserver) Finished(Receipt) {
	close(o.finishing)
	<-o.release
}

func TestStopAndWaitDrainsEffectAndObserver(t *testing.T) {
	var l Ledger
	effectRelease := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(effectRelease) }) })
	observer := &blockingFinishObserver{
		started: make(chan struct{}), finishing: make(chan struct{}), release: make(chan struct{}),
	}
	var finishOnce sync.Once
	t.Cleanup(func() { finishOnce.Do(func() { close(observer.release) }) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = l.Do(WithObserver(context.Background(), observer), 3, "accepted", "start", "effect", func() Receipt {
			<-effectRelease
			return Receipt{Status: "applied"}
		})
	}()
	select {
	case <-observer.started:
	case <-time.After(5 * time.Second):
		t.Fatal("effect not started")
	}
	drained := make(chan struct{})
	go func() {
		l.StopAndWait()
		close(drained)
	}()
	deadline := time.After(5 * time.Second)
	for !l.Frozen() {
		select {
		case <-deadline:
			t.Fatal("StopAndWait failed to freeze the ledger")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	_, _, err := l.Do(context.Background(), 3, "new", "start", "effect", func() Receipt {
		t.Error("accepted mutation after StopAndWait began")
		return Receipt{Status: "applied"}
	})
	if err == nil || !strings.Contains(err.Error(), "server_draining") {
		t.Fatalf("new effect not refused after shutdown: %v", err)
	}
	releaseOnce.Do(func() { close(effectRelease) })
	select {
	case <-observer.finishing:
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not reach its finish observer")
	}
	select {
	case <-drained:
		t.Fatal("StopAndWait returned before observer finished")
	default:
	}
	// Caller-facing completion and shutdown must both wait for the audit hook.
	finishOnce.Do(func() { close(observer.release) })
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("StopAndWait never completed")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("original caller never received its completed effect")
	}
}

func TestStopAndWaitEmptyLedger(t *testing.T) {
	var l Ledger
	l.StopAndWait()
	if !l.Frozen() {
		t.Fatal("empty drained ledger is not frozen")
	}
	l.StopAndWait() // Idempotent.
}

func TestFreezeWaitsForOperationAuditFinish(t *testing.T) {
	var l Ledger
	effectRelease := make(chan struct{})
	var effectOnce sync.Once
	defer effectOnce.Do(func() { close(effectRelease) })
	observer := &blockingFinishObserver{
		started: make(chan struct{}), finishing: make(chan struct{}), release: make(chan struct{}),
	}
	var finishOnce sync.Once
	defer finishOnce.Do(func() { close(observer.release) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = l.Do(WithObserver(context.Background(), observer), 2, "update-fence", "start", "effect", func() Receipt {
			<-effectRelease
			return Receipt{Status: "applied"}
		})
	}()
	select {
	case <-observer.started:
	case <-time.After(5 * time.Second):
		t.Fatal("operation_start hook not reached")
	}
	effectOnce.Do(func() { close(effectRelease) })
	select {
	case <-observer.finishing:
	case <-time.After(5 * time.Second):
		t.Fatal("operation_finish hook not reached")
	}
	if err := l.Freeze(); err == nil || !strings.Contains(err.Error(), "mutations_in_flight") {
		t.Fatalf("updater froze before operation_finish completed: %v", err)
	}
	finishOnce.Do(func() { close(observer.release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("effect did not finish")
	}
	if err := l.Freeze(); err != nil || !l.Frozen() {
		t.Fatalf("updater could not freeze fully audited ledger: %v", err)
	}
	r, replayed, err := l.Do(context.Background(), 2, "update-fence", "start", "effect", func() Receipt {
		t.Error("replayed effect executed during update fence")
		return Receipt{Status: "failed"}
	})
	if err != nil || !replayed || r.Status != "applied" {
		t.Fatalf("frozen receipt replay failed: %+v replayed=%v err=%v", r, replayed, err)
	}
}
