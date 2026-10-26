//go:build linux || darwin

package session

import (
	"context"
	"testing"
	"time"
)

func TestAutoCloseCompletionRetainsOutputAndReplay(t *testing.T) {
	m := testManager(t)
	a := StartArgs{ServerID: m.id, OperationID: "auto", Argv: []string{"/bin/sh", "-c", "printf output; exit 7"}, CloseOnExit: true, WaitFor: "completion", WaitMS: 3000}
	r, err := m.Start(context.Background(), a)
	if err != nil || r.Observation == nil {
		t.Fatalf("%+v %v", r, err)
	}
	o := r.Observation
	if o.ReturnReason != "completed" || !o.Session.Closed || !o.Session.OutputDone || o.Session.Exit == nil || *o.Session.Exit.Code != 7 || string(eventBytes(o.Events)) != "output" {
		t.Fatalf("%+v", o)
	}
	a.WaitFor, a.WaitMS = "output", 0
	retry, err := m.Start(context.Background(), a)
	if err != nil || !retry.Replayed || retry.Receipt.SessionID != r.Receipt.SessionID {
		t.Fatalf("%+v %v", retry, err)
	}
	again, err := m.Read(context.Background(), ReadArgs{SessionID: r.Receipt.SessionID})
	if err != nil || string(eventBytes(again.Events)) != "output" {
		t.Fatalf("%+v %v", again, err)
	}
	a.CloseOnExit = false
	if _, err = m.Start(context.Background(), a); err == nil {
		t.Fatal("close_on_exit excluded from retry identity")
	}
}

func TestCompletionWaitDoesNotConfuseOutputEOFWithExit(t *testing.T) {
	m := testManager(t)
	a := StartArgs{ServerID: m.id, OperationID: "eof-before-exit", Argv: []string{"/bin/sh", "-c", "exec 1>&- 2>&-; sleep .4"}, CloseOnExit: true, WaitFor: "completion", WaitMS: 50}
	r, err := m.Start(context.Background(), a)
	if err != nil || r.Observation == nil {
		t.Fatalf("%+v %v", r, err)
	}
	if r.Observation.Session.Exit != nil || r.Observation.ReturnReason != "deadline" {
		t.Fatalf("early EOF treated as exit: %+v", r)
	}
	o, err := m.Read(context.Background(), ReadArgs{SessionID: r.Receipt.SessionID, WaitFor: "completion", WaitMS: 3000})
	if err != nil || o.ReturnReason != "completed" || !o.Session.Closed {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestCompletionFloodIsBoundedAndCancelDoesNotKill(t *testing.T) {
	m := testManager(t)
	a := StartArgs{ServerID: m.id, OperationID: "bounded", Argv: []string{"/bin/sh", "-c", "printf '0123456789'; sleep 2"}, CloseOnExit: true, WaitFor: "completion", WaitMS: 1000, MaxBytes: 4}
	start := time.Now()
	r, err := m.Start(context.Background(), a)
	if err != nil || r.Observation == nil || !r.Observation.HasMore || r.Observation.ReturnReason != "limit" || time.Since(start) > time.Second {
		t.Fatalf("%+v %v", r, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = m.Read(ctx, ReadArgs{SessionID: r.Receipt.SessionID, WaitFor: "completion", WaitMS: 1000}); err == nil {
		t.Fatal("expected cancellation")
	}
	o, err := m.Read(context.Background(), ReadArgs{SessionID: r.Receipt.SessionID})
	if err != nil || o.Session.Exit != nil {
		t.Fatalf("cancel killed process: %+v %v", o, err)
	}
}

func TestAutoCloseWithoutAnyReader(t *testing.T) {
	m := testManager(t)
	r, err := m.Start(context.Background(), StartArgs{ServerID: m.id, OperationID: "unobserved", Argv: []string{"/bin/sh", "-c", "printf retained"}, CloseOnExit: true})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.get(r.Receipt.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.closeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("no automatic cleanup")
	}
	o, err := m.Read(context.Background(), ReadArgs{SessionID: s.id})
	if err != nil || !o.Session.Closed || string(eventBytes(o.Events)) != "retained" {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestCompletionValidation(t *testing.T) {
	m := testManager(t)
	for _, a := range []ReadArgs{{WaitFor: "bad"}, {WaitFor: "completion", Until: "needle"}} {
		if _, err := m.readOptions(a); err == nil {
			t.Fatal("accepted invalid wait")
		}
	}
}

func TestDrainClosesAdmissionBeforeWaitingForActiveProcess(t *testing.T) {
	m := testManager(t)
	r, err := m.Start(context.Background(), StartArgs{ServerID: m.id, OperationID: "live", Argv: []string{"/bin/sh", "-c", "sleep .2"}, CloseOnExit: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.PrepareRestart(m.id); err != nil {
		t.Fatalf("drain should begin while the process is active: %v", err)
	}
	if !m.ops.raw.Draining() {
		t.Fatal("drain gate did not close immediately")
	}
	if err := m.CanShutdownForUpdate(m.id); err == nil {
		t.Fatal("attempted to shut down an active process")
	}
	if _, err = m.Start(context.Background(), StartArgs{ServerID: m.id, OperationID: "blocked", Argv: []string{"/bin/true"}}); err == nil {
		t.Fatal("accepted new work after drain began")
	}
	_, err = m.Read(context.Background(), ReadArgs{SessionID: r.Receipt.SessionID, WaitFor: "completion", WaitMS: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.CanShutdownForUpdate(m.id); err != nil {
		t.Fatalf("idle manager did not acquire final fence: %v", err)
	}
	list, err := m.List(context.Background(), ListArgs{})
	if err != nil || !list.Draining {
		t.Fatalf("%+v %v", list, err)
	}
	if err = m.CancelRestart("old"); err == nil {
		t.Fatal("old instance cancelled drain")
	}
	if err = m.CancelRestart(m.id); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start(context.Background(), StartArgs{ServerID: m.id, OperationID: "after", Argv: []string{"/bin/true"}, CloseOnExit: true, WaitFor: "completion", WaitMS: 1000}); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitCancelOnlyRestoresAdmission(t *testing.T) {
	m := testManager(t)
	if err := m.PrepareRestart(m.id); err != nil {
		t.Fatal(err)
	}
	if !m.ops.raw.Draining() {
		t.Fatal("missing drain state")
	}
	if err := m.CancelRestart(m.id); err != nil {
		t.Fatal(err)
	}
	if m.ops.raw.Draining() {
		t.Fatal("cancel failed to release admission")
	}
	if err := m.PrepareRestart(m.id); err != nil {
		t.Fatal(err)
	}
	if !m.ops.raw.Draining() {
		t.Fatal("second drain did not close admission")
	}
}
