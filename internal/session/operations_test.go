//go:build linux || darwin

package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLedgerConcurrentDeduplication(t *testing.T) {
	var l ledger
	var calls atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _, err := l.do(context.Background(), 10, "same", "write", "input", func() Receipt {
				calls.Add(1)
				time.Sleep(10 * time.Millisecond)
				return Receipt{Status: "applied", BytesWritten: 5}
			})
			if err != nil || r.BytesWritten != 5 {
				t.Errorf("duplicate failed: %#v %v", r, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("action executed %d times", calls.Load())
	}
	if _, _, err := l.do(context.Background(), 10, "same", "write", "different", func() Receipt { t.Error("conflicting action ran"); return Receipt{} }); err == nil {
		t.Fatal("missing conflict")
	}
}

func TestLedgerCancellationDoesNotReplayAcceptedAction(t *testing.T) {
	var l ledger
	accepted, finish := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, err := l.do(ctx, 10, "id", "start", 1, func() Receipt { close(accepted); <-finish; return Receipt{SessionID: "original", Status: "applied"} })
		result <- err
	}()
	<-accepted
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	close(finish)
	r, replayed, err := l.do(context.Background(), 10, "id", "start", 1, func() Receipt { t.Error("action replayed"); return Receipt{} })
	if err != nil || !replayed || r.SessionID != "original" {
		t.Fatalf("receipt not recovered: %#v %v", r, err)
	}
}

func TestLedgerCapacityAndPanic(t *testing.T) {
	var l ledger
	r, _, err := l.do(context.Background(), 1, "id", "start", 1, func() Receipt { panic("uncertain outcome") })
	if err != nil || r.Status != "unknown" {
		t.Fatal("panic outcome not retained")
	}
	if _, _, err := l.do(context.Background(), 1, "new", "start", 1, func() Receipt { t.Error("ran past ledger capacity"); return Receipt{} }); err == nil {
		t.Fatal("capacity not enforced")
	}
	r, replayed, err := l.do(context.Background(), 1, "id", "start", 1, func() Receipt { t.Error("unknown action reran"); return Receipt{} })
	if err != nil || !replayed || r.Status != "unknown" {
		t.Fatal("ledger evicted uncertain outcome")
	}
}

func TestOperationIDValidation(t *testing.T) {
	for _, bad := range []string{"", "with spaces", "x\n", "русский"} {
		if validOperationID(bad) {
			t.Fatalf("accepted invalid id %q", bad)
		}
	}
	if !validOperationID("build-1:phase_2.3") {
		t.Fatal("rejected valid key")
	}
}

func TestLedgerBoundsStoredError(t *testing.T) {
	var l ledger
	r, _, err := l.do(context.Background(), 2, "large-error", "start", nil, func() Receipt { return Receipt{Status: "failed", Error: string(make([]byte, 10000))} })
	if err != nil || len(r.Error) > 1100 || len(r.Error) < 1024 {
		t.Fatalf("unbounded receipt: %d %v", len(r.Error), err)
	}
}
