//go:build linux || darwin

package session

import (
	"context"
	"testing"
)

func TestListFiltersRetainedClosedSessionsAndCountsAll(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()

	running, err := m.Start(ctx, StartArgs{
		ServerID: m.id, OperationID: "list-running",
		Argv: []string{"/bin/sh", "-c", "sleep 30"},
	})
	if err != nil || running.Receipt.Status != "applied" {
		t.Fatalf("running: %+v %v", running, err)
	}

	exited, err := m.Start(ctx, StartArgs{
		ServerID: m.id, OperationID: "list-exited",
		Argv:    []string{"/bin/sh", "-c", "printf unfinished"},
		WaitFor: "completion", WaitMS: 3000,
	})
	if err != nil || exited.Observation == nil || exited.Observation.Session.Exit == nil {
		t.Fatalf("exited: %+v %v", exited, err)
	}

	closed, err := m.Start(ctx, StartArgs{
		ServerID: m.id, OperationID: "list-closed",
		Argv:        []string{"/bin/sh", "-c", "printf retained"},
		CloseOnExit: true, WaitFor: "completion", WaitMS: 3000,
	})
	if err != nil || closed.Observation == nil || !closed.Observation.Session.Closed {
		t.Fatalf("closed: %+v %v", closed, err)
	}

	normal, err := m.List(ctx, ListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	want := SessionCounts{Running: 1, ExitedUnclosed: 1, ClosedRetained: 1}
	if normal.Counts != want || len(normal.Sessions) != 2 {
		t.Fatalf("default list: %+v; want counts %+v, two unclosed sessions", normal, want)
	}
	for _, item := range normal.Sessions {
		if item.Closed || item.SessionID == closed.Receipt.SessionID {
			t.Fatalf("default list exposed closed session: %+v", item)
		}
	}

	full, err := m.List(ctx, ListArgs{IncludeClosed: true})
	if err != nil || full.Counts != want || len(full.Sessions) != 3 {
		t.Fatalf("full list: %+v %v", full, err)
	}
	found := false
	for _, item := range full.Sessions {
		if item.SessionID == closed.Receipt.SessionID && item.Closed {
			found = true
		}
	}
	if !found {
		t.Fatal("full list omitted the retained closed session")
	}
	after, err := m.Read(ctx, ReadArgs{SessionID: closed.Receipt.SessionID})
	if err != nil || string(eventBytes(after.Events)) != "retained" {
		t.Fatalf("closed session lost readable output: %+v %v", after, err)
	}
}
