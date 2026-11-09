package dashboard

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/viewer"
	"terminalmcp/internal/viewer/viewertest"
)

// TestJournalRowsAndOrphanEffectsAreDelivered: server starts, diagnostics
// and effects whose origin is not loaded are timeline items with records.
func TestJournalRowsAndOrphanEffectsAreDelivered(t *testing.T) {
	j := openJournal(t)
	j.write(audit.Event{ServerID: testServer, Tool: "server", Phase: "server_start", Status: "ok"})
	j.write(audit.Event{ServerID: testServer, RequestID: "gone", OperationID: "op-1", Tool: "file_write",
		Phase: "operation_start"})
	j.requests(1)
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{not json}\n")
	_ = f.Close()
	_ = j.log.Close()
	j2 := openJournalAt(t, j.path) // The foreign row is visible history now.
	mg := newManager(j2)
	s := viewertest.NewSession(nil)
	id, err := mg.Open(context.Background(), s, ModeLive, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mg.Origins(context.Background(), s, id); err != nil {
		t.Fatal(err)
	}
	page, err := mg.Recent(context.Background(), s, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestIDs(page); got != "diagnostic req-00001 effect server_start" {
		t.Fatalf("items %q", got)
	}
	if page.Coverage.End != "reached_start" || len(page.Records) != 5 {
		t.Fatalf("coverage %+v records %d", page.Coverage, len(page.Records))
	}
	for _, r := range page.Records {
		data, err := base64.StdEncoding.DecodeString(r.DataB64)
		if err != nil || r.Unavailable || len(data) != r.Size {
			t.Fatalf("record %+v", r)
		}
	}
	lanes, err := mg.Lanes(context.Background(), s, id)
	if err != nil || !hasLaneKind(lanes, laneJournal) || !hasLaneKind(lanes, laneOriginNotLoaded) {
		t.Fatalf("lanes %+v %v", lanes, err)
	}
}

// TestEarlierDeliversWhatTheFinalScanFound: when the scan reaches the start,
// the rows it found are still delivered, then an empty page reports the end.
func TestEarlierDeliversWhatTheFinalScanFound(t *testing.T) {
	j := openJournal(t)
	j.requests(150) // The first history page holds 100 of them.
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	lanes, _ := mg.Lanes(context.Background(), s, id)
	if len(lanes) != 1 || lanes[0].Items != 100 {
		t.Fatalf("lanes %+v", lanes)
	}
	lane := lanes[0].ID
	var before *Continuation
	loaded := 0
	for {
		page, err := mg.LaneItems(context.Background(), s, id, lane, before)
		if err != nil {
			t.Fatal(err)
		}
		loaded += len(page.Items)
		if page.Next == nil {
			last := page.Items[len(page.Items)-1]
			before = &Continuation{Order: last.Order, Key: last.Key}
			break
		}
		before = page.Next
	}
	older, err := mg.Earlier(context.Background(), s, id, lane, before)
	if err != nil || loaded != 100 || len(older.Items) != 50 || older.Coverage.End != "reached_start" {
		t.Fatalf("earlier %d items coverage %+v loaded %d: %v", len(older.Items), older.Coverage, loaded, err)
	}
	last := older.Items[len(older.Items)-1]
	end, err := mg.Earlier(context.Background(), s, id, lane, &Continuation{Order: last.Order, Key: last.Key})
	if err != nil || len(end.Items) != 0 || end.Coverage.End != "reached_start" {
		t.Fatalf("end %+v %v", end, err)
	}
}

func TestOldGenerationIsRefusedWithoutRebuild(t *testing.T) {
	j := openJournal(t)
	j.requests(2)
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	lanes, _ := mg.Lanes(context.Background(), s, id)
	m, _ := mg.get(s, id)
	gen, _, head, _ := m.win.Info()
	if _, err := mg.CatchUp(context.Background(), s, id, gen+1, head, nil); !errors.Is(err, ErrGenerationChanged) {
		t.Fatalf("other generation: %v", err)
	}
	if now, _, _, _ := m.win.Info(); now != gen {
		t.Fatal("a stale cursor rebuilt the window")
	}
	// After a rebuild, IDs of the previous generation are refused.
	if err := mg.run(context.Background(), s, id, func(*managed) error { return ErrRebuild }); !errors.Is(err, ErrRebuild) {
		t.Fatal(err)
	}
	if _, err := mg.LaneItems(context.Background(), s, id, lanes[0].ID, nil); !errors.Is(err, ErrGenerationChanged) {
		t.Fatalf("old lane: %v", err)
	}
	if _, err := mg.CatchUp(context.Background(), s, id, gen, head, nil); !errors.Is(err, ErrGenerationChanged) {
		t.Fatalf("old cursor: %v", err)
	}
}

func TestRebuildIsSingleFlight(t *testing.T) {
	j := openJournal(t)
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	m, _ := mg.get(s, id)
	gen, _, _, _ := m.win.Info()
	m.mu.Lock()
	m.rebuilding = true
	m.mu.Unlock()
	var rebuilding *RebuildingError
	if err := mg.run(context.Background(), s, id, func(*managed) error { return ErrRebuild }); !errors.As(err, &rebuilding) {
		t.Fatalf("concurrent rebuild: %v", err)
	}
	if now, _, _, _ := m.win.Info(); now != gen {
		t.Fatal("a second rebuild ran")
	}
}

// TestRebuildsAndReadsDoNotDeadlock exercises the window and managed locks
// from both directions.
func TestRebuildsAndReadsDoNotDeadlock(t *testing.T) {
	j := openJournal(t)
	j.requests(20)
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	done := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				_, _ = mg.Recent(context.Background(), s, id, nil)
				_, _ = mg.Lanes(context.Background(), s, id)
				_ = mg.run(context.Background(), s, id, func(*managed) error { return ErrRebuild })
			}
		}()
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		buf := make([]byte, 1<<20)
		t.Fatalf("deadlock:\n%s", buf[:runtime.Stack(buf, true)])
	}
}

// TestEndedSessionFreesWindowsWithoutRequests: the manager forgets windows
// of an ended session without waiting for another request.
func TestEndedSessionFreesWindowsWithoutRequests(t *testing.T) {
	j := openJournal(t)
	j.requests(5)
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	if _, err := mg.Open(context.Background(), s, ModeLive, ""); err != nil {
		t.Fatal(err)
	}
	s.End(viewer.ErrLoggedOut, time.Now())
	deadline := time.Now().Add(5 * time.Second)
	for {
		mg.mu.Lock()
		n := len(mg.windows)
		mg.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("window kept after the session ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOtherSessionsAreRefusedEverywhere(t *testing.T) {
	j := openJournal(t)
	j.requests(2)
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	lanes, _ := mg.Lanes(context.Background(), s, id)
	m, _ := mg.get(s, id)
	gen, _, head, _ := m.win.Info()
	other := viewertest.NewSession(nil)
	ctx := context.Background()
	calls := map[string]func() error{
		"recent":   func() error { _, err := mg.Recent(ctx, other, id, nil); return err },
		"lane":     func() error { _, err := mg.LaneItems(ctx, other, id, lanes[0].ID, nil); return err },
		"earlier":  func() error { _, err := mg.Earlier(ctx, other, id, lanes[0].ID, nil); return err },
		"scan":     func() error { _, err := mg.Scan(ctx, other, id); return err },
		"origins":  func() error { _, err := mg.Origins(ctx, other, id); return err },
		"lanes":    func() error { _, err := mg.Lanes(ctx, other, id); return err },
		"catch-up": func() error { _, err := mg.CatchUp(ctx, other, id, gen, head, nil); return err },
		"record":   func() error { _, err := mg.RecordBytes(ctx, other, id, "x"); return err },
		"archive":  func() error { _, err := mg.Open(ctx, other, ModeArchive, id); return err },
		"close":    func() error { return mg.Close(other, id) },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrWindowGone) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := mg.Recent(ctx, s, id, nil); err != nil {
		t.Fatalf("owner after refusals: %v", err)
	}
}

func TestClientNamesAreBounded(t *testing.T) {
	j := openJournal(t)
	for i := 0; i < 2*maxClientNames; i++ {
		j.write(audit.Event{ServerID: testServer, RequestID: "c" + itoa(i), Tool: "file_read", Phase: "request_start",
			CallMeta: map[string]any{"openai/session": "chat", "io.modelcontextprotocol/clientInfo": map[string]any{"name": "client-" + itoa(i)}}})
	}
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	lanes, err := mg.Lanes(context.Background(), s, id)
	if err != nil || len(lanes) != 1 || lanes[0].Items != 2*maxClientNames {
		t.Fatalf("lanes %+v %v", lanes, err)
	}
	if n := len(lanes[0].ClientNames); n != maxClientNames {
		t.Fatalf("%d client names", n)
	}
}

// TestRebuildAfterAnotherRebuildIsSkipped: an operation that saw an older
// generation only gets the new snapshot, not a second rebuild.
func TestRebuildAfterAnotherRebuildIsSkipped(t *testing.T) {
	j := openJournal(t)
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	m, _ := mg.get(s, id)
	stale := func(*managed) error {
		if err := mg.run(context.Background(), s, id, func(*managed) error { return ErrRebuild }); !errors.Is(err, ErrRebuild) {
			t.Fatal(err)
		}
		return ErrRebuild // Another request rebuilt meanwhile.
	}
	if err := mg.run(context.Background(), s, id, stale); !errors.Is(err, ErrRebuild) {
		t.Fatal(err)
	}
	if gen, _, _, _ := m.win.Info(); gen != 1 {
		t.Fatalf("generation %d: expected exactly one rebuild", gen)
	}
}

// TestSessionEndingDuringOpenReleasesTheWindow: Stop may arrive while the
// window is still being built.
func TestSessionEndingDuringOpenReleasesTheWindow(t *testing.T) {
	j := openJournal(t)
	j.requests(50)
	for i := 0; i < 20; i++ {
		mg := newManager(j)
		s := viewertest.NewSession(nil)
		go s.End(viewer.ErrLoggedOut, time.Now())
		id, err := mg.Open(context.Background(), s, ModeLive, "")
		if err == nil {
			if _, err := mg.Recent(context.Background(), s, id, nil); err != nil && !errors.Is(err, ErrWindowGone) {
				t.Fatal(err)
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for mg.Windows() != 0 {
			if time.Now().After(deadline) {
				t.Fatal("window kept after the session ended")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// TestSharedSnapshotUnderConcurrentReaders runs readers on a cached
// snapshot while catch-up changes the window (run with -race).
func TestSharedSnapshotUnderConcurrentReaders(t *testing.T) {
	j := openJournal(t)
	j.requests(30)
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	m, _ := mg.get(s, id)
	gen, _, head, _ := m.win.Info()
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := mg.Recent(context.Background(), s, id, nil); err != nil {
					t.Error(err)
				}
				if _, err := mg.Lanes(context.Background(), s, id); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	for i := 0; i < 10; i++ {
		j.requests(1)
		res, err := mg.CatchUp(context.Background(), s, id, gen, head, nil)
		if err != nil {
			t.Fatal(err)
		}
		head = res.Head
	}
	wg.Wait()
}
