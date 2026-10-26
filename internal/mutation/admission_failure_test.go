package mutation

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

type rejectingObserver struct {
	startErr error
	starts   atomic.Int32
	finishes atomic.Int32
}

func (o *rejectingObserver) Started(string) error {
	o.starts.Add(1)
	return o.startErr
}

func (o *rejectingObserver) Finished(Receipt) { o.finishes.Add(1) }

func TestFailedPreEffectAuditDoesNotRunOrReserveRetryKey(t *testing.T) {
	var ledger Ledger
	sentinel := errors.New("injected audit I/O failure")
	rejector := &rejectingObserver{startErr: sentinel}
	var effects atomic.Int32
	fn := func() Receipt {
		effects.Add(1)
		return Receipt{Status: "applied"}
	}
	receipt, replayed, err := ledger.Do(WithObserver(context.Background(), rejector), 2, "same-key", "file_write", "effect", fn)
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "operation_not_executed") ||
		replayed || receipt.Status != "" || effects.Load() != 0 {
		t.Fatalf("effect executed or wrong error: receipt=%+v replayed=%v err=%v", receipt, replayed, err)
	}
	if ledger.Count() != 0 || rejector.starts.Load() != 1 || rejector.finishes.Load() != 0 {
		t.Fatalf("failed pre-effect audit consumed retry key or notified completion: entries=%d", ledger.Count())
	}
	if err := ledger.Freeze(); err != nil {
		t.Fatalf("failed pre-effect operation remained in flight: %v", err)
	}
	ledger.Resume()
	// Once an external service has been explicitly restarted/repaired,
	// the same key can safely execute: it was never reserved or executed.
	receipt, replayed, err = ledger.Do(context.Background(), 2, "same-key", "file_write", "effect", fn)
	if err != nil || replayed || receipt.Status != "applied" || effects.Load() != 1 {
		t.Fatalf("safe retry of unexecuted key failed: %+v replayed=%v err=%v", receipt, replayed, err)
	}
}

type panicStartObserver struct{ rejectingObserver }

func (*panicStartObserver) Started(string) error { panic("observer unexpectedly panicked") }

func TestPanickingPreEffectObserverCannotExecuteMutation(t *testing.T) {
	var ledger Ledger
	var effects atomic.Int32
	_, replayed, err := ledger.Do(WithObserver(context.Background(), &panicStartObserver{}), 2,
		"panic-start", "terminal_write", "effect", func() Receipt {
			effects.Add(1)
			return Receipt{Status: "applied"}
		})
	if err == nil || replayed || !strings.Contains(err.Error(), "observer panicked") ||
		effects.Load() != 0 || ledger.Count() != 0 {
		t.Fatalf("panicking pre-effect hook admitted mutation: replayed=%v err=%v effects=%d entries=%d",
			replayed, err, effects.Load(), ledger.Count())
	}
}
