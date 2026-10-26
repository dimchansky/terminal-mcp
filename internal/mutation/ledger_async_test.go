package mutation

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type asyncAuditObserver struct {
	starts   atomic.Int32
	finishes atomic.Int32
	startErr error
}

func (o *asyncAuditObserver) Started(string) error {
	o.starts.Add(1)
	return o.startErr
}
func (o *asyncAuditObserver) Finished(Receipt) { o.finishes.Add(1) }

func TestStartReturnsBeforeCompletionAndKeepsStatus(t *testing.T) {
	var l Ledger
	observer := &asyncAuditObserver{}
	ctx := WithObserver(context.Background(), observer)
	hold := make(chan struct{})
	downloading := make(chan struct{})
	var invoked atomic.Int32
	submitted, replayed, err := l.Start(ctx, 8, "slow-file", "file_receive", "same-input",
		func(report func(Progress)) Receipt {
			invoked.Add(1)
			total := int64(100)
			report(Progress{Stage: "downloading", Path: "/tmp/test.bin",
				BytesDownloaded: 40, TotalBytes: &total})
			close(downloading)
			<-hold
			return Receipt{Status: "applied", Data: []byte("null")}
		})
	if err != nil || replayed || submitted.OperationID != "slow-file" || submitted.Status != "running" {
		t.Fatalf("async submission failed or blocked: %+v %v", submitted, err)
	}
	select {
	case <-downloading:
	case <-time.After(time.Second):
		t.Fatal("effect was not started")
	}
	status, err := l.Snapshot("slow-file", "file_receive")
	if err != nil || status.Status != "running" || status.Progress.BytesDownloaded != 40 ||
		status.Progress.TotalBytes == nil || *status.Progress.TotalBytes != 100 {
		t.Fatalf("incorrect live snapshot: %+v %v", status, err)
	}
	l.BeginDrain()
	if _, _, err := l.Start(context.Background(), 8, "blocked-file", "file_receive", "new-input",
		func(func(Progress)) Receipt { return Receipt{Status: "applied"} }); err == nil || !strings.Contains(err.Error(), "server_draining") {
		t.Fatalf("new work entered draining ledger: %v", err)
	}
	replay, already, err := l.Start(context.Background(), 8, "slow-file", "file_receive", "same-input",
		func(func(Progress)) Receipt { t.Error("replay executed again"); return Receipt{Status: "failed"} })
	if err != nil || !already || replay.Status != "running" {
		t.Fatalf("bad in-flight replay: %+v %v", replay, err)
	}
	close(hold)
	l.StopAndWait()
	finished, err := l.Snapshot("slow-file", "file_receive")
	if err != nil || finished.Status != "completed" || finished.Receipt == nil ||
		finished.CompletedAt == nil || finished.StartedAt.IsZero() || observer.starts.Load() != 1 ||
		observer.finishes.Load() != 1 || invoked.Load() != 1 {
		t.Fatalf("lost independent completion or audit: %+v %v", finished, err)
	}
	again, already, err := l.Start(context.Background(), 8, "slow-file", "file_receive", "same-input",
		func(func(Progress)) Receipt { t.Error("completed replay executed"); return Receipt{Status: "failed"} })
	if err != nil || !already || again.Status != "completed" || again.CompletedAt == nil {
		t.Fatalf("completed snapshot must survive reads: %+v %v", again, err)
	}
	if _, err := l.Snapshot("slow-file", "other_method"); err == nil {
		t.Fatal("snapshot accepted wrong method")
	}
}

func TestStartFailClosedWhenInitialAuditFails(t *testing.T) {
	var l Ledger
	observer := &asyncAuditObserver{startErr: errors.New("disk full")}
	ctx := WithObserver(context.Background(), observer)
	var effects atomic.Int32
	_, _, err := l.Start(ctx, 8, "rejected", "file_receive", "signed-url",
		func(func(Progress)) Receipt { effects.Add(1); return Receipt{Status: "applied"} })
	if err == nil || !strings.Contains(err.Error(), "operation_not_executed") || effects.Load() != 0 {
		t.Fatalf("effect ran without audit: %v", err)
	}
	l.StopAndWait()
	if observer.starts.Load() != 1 || observer.finishes.Load() != 0 {
		t.Fatalf("bad lifecycle: %+v", observer)
	}
	if _, err := l.Snapshot("rejected", "file_receive"); err == nil {
		t.Fatal("unaudited rejected operation retained")
	}
}

type delayedRejectAudit struct {
	entered chan struct{}
	release chan struct{}
}

func (o *delayedRejectAudit) Started(string) error {
	close(o.entered)
	<-o.release
	return errors.New("deliberate audit failure")
}
func (o *delayedRejectAudit) Finished(Receipt) {}

func TestStatusWhilePreeffectAuditIsPending(t *testing.T) {
	var l Ledger
	audit := &delayedRejectAudit{entered: make(chan struct{}), release: make(chan struct{})}
	ctx := WithObserver(context.Background(), audit)
	done := make(chan error, 1)
	var effects atomic.Int32
	go func() {
		_, _, err := l.Start(ctx, 8, "slow-audit", "file_receive", "sensitive-input",
			func(func(Progress)) Receipt {
				effects.Add(1)
				return Receipt{Status: "applied"}
			})
		done <- err
	}()
	select {
	case <-audit.entered:
	case <-time.After(time.Second):
		t.Fatal("pre-effect audit was not started")
	}
	for range 50 {
		snap, err := l.Snapshot("slow-audit", "file_receive")
		if err != nil || snap.Status != "running" || snap.Progress.Stage != "queued" {
			t.Fatalf("unaudited operation was reported completed: %+v %v", snap, err)
		}
	}
	close(audit.release)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "operation_not_executed") {
			t.Fatalf("failed pre-effect audit admitted work: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tool call did not finish after audit was rejected")
	}
	l.StopAndWait()
	if effects.Load() != 0 {
		t.Fatal("effect ran despite rejected audit")
	}
	if _, err := l.Snapshot("slow-audit", "file_receive"); err == nil {
		t.Fatal("rejected unaudited operation retained")
	}
}
