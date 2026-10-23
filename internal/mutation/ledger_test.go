//go:build linux || darwin

package mutation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func mustReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for mutation operation")
		var zero T
		return zero
	}
}

func TestValidID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"single letter", "x", true},
		{"allowed punctuation", "run:02_A-b.7", true},
		{"exact limit", strings.Repeat("x", 128), true},
		{"empty", "", false},
		{"too long", strings.Repeat("x", 129), false},
		{"space", "two words", false},
		{"newline", "key\nother", false},
		{"slash", "run/test", false},
		{"unicode", "ключ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidID(tt.id); got != tt.want {
				t.Fatalf("ValidID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestDoReplaysReceiptWithoutReexecution(t *testing.T) {
	var ledger Ledger
	var calls atomic.Int32
	effect := map[string]any{"path": "/tmp/file", "content": "unchanged"}
	first, replayed, err := ledger.Do(context.Background(), 2, "write-1", "file_write", effect, func() Receipt {
		calls.Add(1)
		return Receipt{Status: "applied", Data: json.RawMessage(`{"version":"v1"}`)}
	})
	if err != nil || replayed {
		t.Fatalf("first execution = %+v, replayed %v, err %v", first, replayed, err)
	}
	replay, replayed, err := ledger.Do(context.Background(), 2, "write-1", "file_write", effect, func() Receipt {
		calls.Add(1)
		return Receipt{Status: "failed"}
	})
	if err != nil || !replayed || first.Status != replay.Status || first.OperationID != replay.OperationID ||
		string(first.Data) != string(replay.Data) || calls.Load() != 1 || ledger.Count() != 1 {
		t.Fatalf("replay = %+v, replayed %v, err %v, calls %d, count %d", replay, replayed, err, calls.Load(), ledger.Count())
	}
}

func TestDoIdenticalEffectIsOrderIndependent(t *testing.T) {
	var ledger Ledger
	firstEffect := map[string]any{"b": "second", "a": "first"}
	secondEffect := map[string]any{"a": "first", "b": "second"}
	_, _, err := ledger.Do(context.Background(), 2, "order", "file_edit", firstEffect, func() Receipt { return Receipt{Status: "applied"} })
	if err != nil {
		t.Fatal(err)
	}
	_, replayed, err := ledger.Do(context.Background(), 2, "order", "file_edit", secondEffect, func() Receipt {
		t.Error("effect with the same JSON meaning ran twice")
		return Receipt{}
	})
	if err != nil || !replayed {
		t.Fatalf("same effect was not replayed: %v, %v", replayed, err)
	}
}

func TestDoConflictingEffectOrMethodCannotRun(t *testing.T) {
	tests := []struct {
		name   string
		method string
		effect any
	}{
		{"different effect", "write", map[string]string{"data": "different"}},
		{"different method", "close", map[string]string{"data": "original"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ledger Ledger
			var calls atomic.Int32
			original := map[string]string{"data": "original"}
			_, _, err := ledger.Do(context.Background(), 2, "same-id", "write", original, func() Receipt {
				calls.Add(1)
				return Receipt{Status: "applied"}
			})
			if err != nil {
				t.Fatal(err)
			}
			_, replayed, err := ledger.Do(context.Background(), 2, "same-id", tt.method, tt.effect, func() Receipt {
				calls.Add(1)
				return Receipt{Status: "applied"}
			})
			if !replayed || err == nil || !strings.Contains(err.Error(), "operation_id_conflict") || calls.Load() != 1 {
				t.Fatalf("conflict = replayed %v, err %v, callbacks %d", replayed, err, calls.Load())
			}
		})
	}
}

func TestDoConcurrentDuplicatesShareOneAction(t *testing.T) {
	var ledger Ledger
	const goroutines = 48
	var calls atomic.Int32
	start := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	type result struct {
		receipt  Receipt
		replayed bool
		err      error
	}
	results := make(chan result, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, replayed, err := ledger.Do(context.Background(), goroutines, "same", "start", []string{"/bin/true"}, func() Receipt {
				calls.Add(1)
				<-release
				return Receipt{Status: "applied", Data: json.RawMessage(`{"pid":123}`)}
			})
			results <- result{r, replayed, err}
		}()
	}
	close(start)
	// The leader has to be admitted before its action completes.
	deadline := time.After(10 * time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("no operation started")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	releaseOnce.Do(func() { close(release) })
	wg.Wait()
	close(results)
	replayCount := 0
	for got := range results {
		if got.err != nil || got.receipt.Status != "applied" || got.receipt.OperationID != "same" ||
			string(got.receipt.Data) != `{"pid":123}` {
			t.Fatalf("concurrent operation = %+v", got)
		}
		if got.replayed {
			replayCount++
		}
	}
	if calls.Load() != 1 || replayCount != goroutines-1 || ledger.Count() != 1 {
		t.Fatalf("calls %d, replays %d, entries %d", calls.Load(), replayCount, ledger.Count())
	}
}

func TestDoCancelledBeforeAdmissionHasNoEffect(t *testing.T) {
	var ledger Ledger
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, _, err := ledger.Do(ctx, 2, "cancel-first", "start", nil, func() Receipt {
		called = true
		return Receipt{Status: "applied"}
	})
	if !errors.Is(err, context.Canceled) || called || ledger.Count() != 0 {
		t.Fatalf("canceled call: err %v, executed %v, count %d", err, called, ledger.Count())
	}
}

func TestDoCancellationAfterAdmissionKeepsOriginalReceipt(t *testing.T) {
	var ledger Ledger
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	finish := make(chan struct{})
	var finishOnce sync.Once
	t.Cleanup(func() { finishOnce.Do(func() { close(finish) }) })
	type result struct {
		receipt  Receipt
		replayed bool
		err      error
	}
	done := make(chan result, 1)
	var calls atomic.Int32
	go func() {
		r, replayed, err := ledger.Do(ctx, 2, "cancel-mid", "write", "input", func() Receipt {
			calls.Add(1)
			close(started)
			<-finish
			return Receipt{Status: "partial", Error: "write partially completed"}
		})
		done <- result{r, replayed, err}
	}()
	mustReceive(t, started)
	cancel()
	cancelled := mustReceive(t, done)
	if !errors.Is(cancelled.err, context.Canceled) || cancelled.replayed || ledger.Count() != 1 {
		t.Fatalf("canceled waiter = %+v, entries %d", cancelled, ledger.Count())
	}
	finishOnce.Do(func() { close(finish) })
	receipt, replayed, err := ledger.Do(context.Background(), 2, "cancel-mid", "write", "input", func() Receipt {
		calls.Add(1)
		return Receipt{Status: "applied"}
	})
	if err != nil || !replayed || receipt.Status != "partial" ||
		receipt.Error != "write partially completed" || calls.Load() != 1 {
		t.Fatalf("recovered receipt %+v, replayed %v, err %v, callbacks %d", receipt, replayed, err, calls.Load())
	}
}

func TestDoConflictWhileActionIsRunning(t *testing.T) {
	var ledger Ledger
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	done := make(chan struct{})
	go func() {
		_, _, _ = ledger.Do(context.Background(), 3, "running", "start", "first", func() Receipt {
			close(started)
			<-release
			return Receipt{Status: "applied"}
		})
		close(done)
	}()
	mustReceive(t, started)
	_, replayed, err := ledger.Do(context.Background(), 3, "running", "start", "different", func() Receipt {
		t.Error("conflicting operation executed")
		return Receipt{}
	})
	if !replayed || err == nil || !strings.Contains(err.Error(), "operation_id_conflict") {
		t.Fatalf("inflight conflict accepted: replayed %v, err %v", replayed, err)
	}
	once.Do(func() { close(release) })
	mustReceive(t, done)
}

func TestFreezeRejectsInFlightThenAllowsReplay(t *testing.T) {
	var ledger Ledger
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	done := make(chan struct{})
	go func() {
		_, _, _ = ledger.Do(context.Background(), 4, "first", "file_write", "a", func() Receipt {
			close(started)
			<-release
			return Receipt{Status: "applied"}
		})
		close(done)
	}()
	mustReceive(t, started)
	if err := ledger.Freeze(); err == nil || !strings.Contains(err.Error(), "mutations_in_flight") || ledger.Frozen() {
		t.Fatalf("freeze succeeded during an active effect: %v", err)
	}
	once.Do(func() { close(release) })
	mustReceive(t, done)
	if err := ledger.Freeze(); err != nil || !ledger.Frozen() {
		t.Fatalf("could not freeze a drained ledger: %v", err)
	}
	newCalls := 0
	_, replayed, err := ledger.Do(context.Background(), 4, "second", "start", "new", func() Receipt {
		newCalls++
		return Receipt{Status: "applied"}
	})
	if err == nil || replayed || newCalls != 0 || ledger.Count() != 1 {
		t.Fatalf("new mutation accepted during freeze: %v %v %d %d", err, replayed, newCalls, ledger.Count())
	}
	receipt, replayed, err := ledger.Do(context.Background(), 4, "first", "file_write", "a", func() Receipt {
		t.Error("frozen ledger reran an accepted effect")
		return Receipt{}
	})
	if err != nil || !replayed || receipt.Status != "applied" {
		t.Fatalf("frozen retry = %+v, replayed %v, err %v", receipt, replayed, err)
	}
	ledger.Resume()
	if ledger.Frozen() {
		t.Fatal("resume did not unfreeze ledger")
	}
	receipt, replayed, err = ledger.Do(context.Background(), 4, "second", "start", "new", func() Receipt {
		newCalls++
		return Receipt{Status: "applied"}
	})
	if err != nil || replayed || receipt.Status != "applied" || newCalls != 1 {
		t.Fatalf("resumed operation = %+v, replayed %v, err %v, calls %d", receipt, replayed, err, newCalls)
	}
}

func TestCapacityPreservesOldReceipts(t *testing.T) {
	var ledger Ledger
	const capacity = 2
	var calls atomic.Int32
	for _, id := range []string{"first", "second"} {
		result, replayed, err := ledger.Do(context.Background(), capacity, id, "write", id, func() Receipt {
			calls.Add(1)
			return Receipt{Status: "failed", Error: "target rejected"}
		})
		if err != nil || replayed || result.Status != "failed" {
			t.Fatalf("first attempt %s = %+v, %v, %v", id, result, replayed, err)
		}
	}
	_, _, err := ledger.Do(context.Background(), capacity, "third", "write", "third", func() Receipt {
		calls.Add(1)
		return Receipt{Status: "applied"}
	})
	if err == nil || !strings.Contains(err.Error(), "operation_ledger_full") || ledger.Count() != capacity {
		t.Fatalf("capacity violation: err %v, count %d", err, ledger.Count())
	}
	for _, id := range []string{"first", "second"} {
		result, replayed, err := ledger.Do(context.Background(), capacity, id, "write", id, func() Receipt {
			calls.Add(1)
			return Receipt{Status: "applied"}
		})
		if err != nil || !replayed || result.Status != "failed" || result.Error != "target rejected" {
			t.Fatalf("old receipt %s = %+v, %v, %v", id, result, replayed, err)
		}
	}
	if got := calls.Load(); got != capacity {
		t.Fatalf("callback invoked %d times, want %d", got, capacity)
	}
}

func TestPanicsProduceStableUnknownReceipt(t *testing.T) {
	tests := []struct {
		name       string
		panicValue any
	}{
		{"ordinary panic", "boom"},
		{"nil panic", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ledger Ledger
			var calls atomic.Int32
			receipt, replayed, err := ledger.Do(context.Background(), 2, "panicking", "start", "effect", func() Receipt {
				calls.Add(1)
				panic(tt.panicValue)
			})
			if err != nil || replayed || receipt.Status != "unknown" ||
				!strings.Contains(receipt.Error, "outcome may be unknown") {
				t.Fatalf("panicking action = %+v, replayed %v, err %v", receipt, replayed, err)
			}
			retry, replayed, err := ledger.Do(context.Background(), 2, "panicking", "start", "effect", func() Receipt {
				calls.Add(1)
				return Receipt{Status: "applied"}
			})
			if err != nil || !replayed || retry.Status != receipt.Status || retry.Error != receipt.Error || retry.OperationID != receipt.OperationID || calls.Load() != 1 {
				t.Fatalf("panic retry = %+v, replayed %v, err %v, calls %d", retry, replayed, err, calls.Load())
			}
			if err := ledger.Freeze(); err != nil {
				t.Fatalf("panic left an operation in flight: %v", err)
			}
		})
	}
}

func TestLargeErrorIsBoundedAndValidUTF8(t *testing.T) {
	var ledger Ledger
	long := strings.Repeat("x", 1023) + "€" + strings.Repeat("z", 50)
	receipt, replayed, err := ledger.Do(context.Background(), 2, "large-error", "edit", "x", func() Receipt {
		return Receipt{Status: "failed", Error: long}
	})
	if err != nil || replayed || !utf8.ValidString(receipt.Error) ||
		len(receipt.Error) > 1040 || !strings.HasSuffix(receipt.Error, "[truncated]") {
		t.Fatalf("bad error truncation: %q, err %v", receipt.Error, err)
	}
}

func TestInvalidEffectDoesNotReserveKey(t *testing.T) {
	var ledger Ledger
	called := false
	_, _, err := ledger.Do(context.Background(), 2, "unserializable", "start", func() {}, func() Receipt {
		called = true
		return Receipt{Status: "applied"}
	})
	var jsonErr *json.UnsupportedTypeError
	if !errors.As(err, &jsonErr) || called || ledger.Count() != 0 {
		t.Fatalf("invalid effect = %v, ran %v, count %d", err, called, ledger.Count())
	}
}

func TestManyDistinctMutationsDoNotLoseReceipts(t *testing.T) {
	var ledger Ledger
	const operations = 80
	var wg sync.WaitGroup
	var calls atomic.Int32
	errorsFound := make(chan error, operations)
	for i := 0; i < operations; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			id := fmt.Sprintf("op-%d", index)
			r, replayed, err := ledger.Do(context.Background(), operations, id, "start", index, func() Receipt {
				calls.Add(1)
				return Receipt{Status: "applied"}
			})
			if err != nil || replayed || r.OperationID != id || r.Status != "applied" {
				errorsFound <- fmt.Errorf("operation %d: receipt %+v, replayed %v, err %v", index, r, replayed, err)
			}
		}(i)
	}
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	if ledger.Count() != operations || calls.Load() != operations {
		t.Fatalf("lost accepted mutation: entries %d, callbacks %d", ledger.Count(), calls.Load())
	}
	if err := ledger.Freeze(); err != nil {
		t.Fatalf("cannot freeze drained concurrent operations: %v", err)
	}
}
