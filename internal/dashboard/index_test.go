package dashboard

import (
	"context"
	"os"
	"sync"
	"testing"

	"terminalmcp/internal/activity"
	"terminalmcp/internal/audit"
)

func frontier(t *testing.T, j *journal) audit.Cursor {
	t.Helper()
	f, err := j.reader.FrontierCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestIndexListsEffectsStartedBeforeAWindow(t *testing.T) {
	j := openJournal(t)
	f0 := frontier(t, j)
	j.write(audit.Event{ServerID: testServer, RequestID: "dl", Tool: "file_receive", Phase: "request_start", OperationID: "op-dl"})
	j.write(audit.Event{ServerID: testServer, RequestID: "dl", Tool: "file_receive", Phase: "operation_start", OperationID: "op-dl"})
	j.write(audit.Event{ServerID: testServer, RequestID: "dl", Tool: "file_receive", Phase: "request_finish", OperationID: "op-dl", Status: "running"})
	j.requests(150) // The window's first history page misses the download.
	x := NewIndex(j.reader, testServer, f0, true, nil)
	if err := x.CatchUp(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	w := buildLive(t, j, WindowConfig{}, nil)
	var info IndexInfo
	w.With(func(p *activity.Projection) { info = x.Info(p) })
	if info.Status != "ready" || len(info.Outside) != 1 || info.Outside[0].Key.OperationID != "op-dl" {
		t.Fatalf("index %+v", info)
	}
}

func TestIndexFailsWhenASealedArchiveDisappears(t *testing.T) {
	j := openJournal(t)
	f0 := frontier(t, j)
	j.requests(1)
	for i := 0; i < 5; i++ {
		j.bigRequest(1 << 20) // Rotate the file that holds F0.
	}
	if err := os.Remove(j.path + ".1"); err != nil {
		t.Fatal(err)
	}
	x := NewIndex(j.reader, testServer, f0, true, nil)
	if err := x.CatchUp(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	var info IndexInfo
	x.mu.Lock()
	status := x.idx.Status()
	x.mu.Unlock()
	if status != activity.IndexUnknown {
		t.Fatalf("status %v info %+v", status, info)
	}
}

func TestIndexLatchesDegradedUntilTheFrontier(t *testing.T) {
	j := openJournal(t)
	f0 := frontier(t, j)
	j.write(audit.Event{ServerID: testServer, RequestID: "lost", Tool: "file_write", Phase: "request_start", OperationID: "op"})
	j.write(audit.Event{ServerID: testServer, RequestID: "lost", Tool: "file_write", Phase: "operation_start", OperationID: "op"})
	j.requests(300) // Several pages: the latch must wait for the frontier.
	degraded := true
	x := NewIndex(j.reader, testServer, f0, true, func() audit.Status {
		if degraded {
			return audit.Status{State: "degraded"}
		}
		return audit.Status{State: "ok"}
	})
	if err := x.CatchUp(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	degraded = false // The episode ended before catch-up reached the frontier.
	if err := x.CatchUp(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	w := buildLive(t, j, WindowConfig{}, nil)
	_ = w.Scan(context.Background(), 10, farFuture())
	w.With(func(p *activity.Projection) {
		x.Sync(p)
		if e, ok := p.Effect(activity.EffectKey{ServerID: testServer, OperationID: "op"}); !ok || !e.PossiblyUnrecorded {
			t.Fatalf("effect %+v", e)
		}
		if a, _ := p.Action(activity.RequestKey{ServerID: testServer, RequestID: "lost"}); !a.PossiblyUnrecorded {
			t.Fatalf("request %+v", a)
		}
	})
}

func TestIndexCatchUpIsSingleFlight(t *testing.T) {
	j := openJournal(t)
	f0 := frontier(t, j)
	j.requests(500)
	x := NewIndex(j.reader, testServer, f0, true, nil)
	w := buildLive(t, j, WindowConfig{}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = x.CatchUp(context.Background(), 2)
			w.With(x.Sync)
		}()
	}
	wg.Wait()
	for x.CatchUp(context.Background(), 10) != nil {
	}
	w.With(func(p *activity.Projection) {
		if info := x.Info(p); info.Status != "ready" {
			t.Fatalf("index %+v", info)
		}
	})
}
