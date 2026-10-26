package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"terminalmcp/internal/mutation"
)

type shutdownObserver struct {
	started      chan struct{}
	finishing    chan struct{}
	releaseFinal chan struct{}
}

func (o *shutdownObserver) Started(string) { close(o.started) }
func (o *shutdownObserver) Finished(mutation.Receipt) {
	close(o.finishing)
	<-o.releaseFinal
}

func TestManagerCloseWaitsForCanceledRequestEffectAndAuditFinish(t *testing.T) {
	m := New(nil)
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	effectRelease := make(chan struct{})
	var effectOnce sync.Once
	defer effectOnce.Do(func() { close(effectRelease) })
	observer := &shutdownObserver{
		started: make(chan struct{}), finishing: make(chan struct{}), releaseFinal: make(chan struct{}),
	}
	var finishOnce sync.Once
	defer finishOnce.Do(func() { close(observer.releaseFinal) })
	requestDone := make(chan error, 1)
	go func() {
		_, _, err := m.Mutate(mutation.WithObserver(ctx, observer), m.ID(), "close-drain", "file_write",
			"independent effect", func() mutation.Receipt {
				<-effectRelease
				return mutation.Receipt{Status: "applied"}
			})
		requestDone <- err
	}()
	select {
	case <-observer.started:
	case <-time.After(5 * time.Second):
		t.Fatal("mutation did not start")
	}
	cancel()
	select {
	case err := <-requestDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled request returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled request did not return")
	}
	closed := make(chan struct{})
	go func() {
		m.Close()
		close(closed)
	}()
	effectOnce.Do(func() { close(effectRelease) })
	select {
	case <-observer.finishing:
	case <-time.After(5 * time.Second):
		t.Fatal("independent effect never reached audit finish")
	}
	select {
	case <-closed:
		t.Fatal("Manager.Close returned before operation_finish audit completed")
	default:
	}
	finishOnce.Do(func() { close(observer.releaseFinal) })
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Manager.Close did not finish after audit hook")
	}
	m.expireDrain(m.drainGeneration) // A delayed updater timer must not unfreeze a closed manager.
	if !m.ops.raw.Frozen() {
		t.Fatal("expired update timer reopened a closed mutation ledger")
	}
}
