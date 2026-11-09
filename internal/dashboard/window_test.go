package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"terminalmcp/internal/activity"
	"terminalmcp/internal/audit"
)

const testServer = "srv-test"

type journal struct {
	t      testing.TB
	path   string
	log    *audit.Log
	reader *audit.Reader
	n      int
}

func openJournal(t testing.TB) *journal {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "logs")
	if err := audit.PrepareDirectory(dir); err != nil {
		t.Fatal(err)
	}
	return openJournalAt(t, filepath.Join(dir, "audit.jsonl"))
}

func openJournalAt(t testing.TB, path string) *journal {
	t.Helper()
	log, err := audit.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	reader, err := audit.NewReader(log)
	if err != nil {
		t.Fatal(err)
	}
	return &journal{t: t, path: path, log: log, reader: reader}
}

// request writes a finished read-only call and returns its request ID.
func (j *journal) request(args string) string {
	j.t.Helper()
	j.n++
	id := fmt.Sprintf("req-%05d", j.n)
	j.write(audit.Event{ServerID: testServer, RequestID: id, Tool: "file_read", Phase: "request_start",
		Args: json.RawMessage(args)})
	j.write(audit.Event{ServerID: testServer, RequestID: id, Tool: "file_read", Phase: "request_finish", Status: "ok"})
	return id
}

func (j *journal) write(e audit.Event) {
	j.t.Helper()
	if err := j.log.Write(audit.NewEvent(e)); err != nil {
		j.t.Fatal(err)
	}
}

func (j *journal) requests(n int) []string {
	var ids []string
	for i := 0; i < n; i++ {
		ids = append(ids, j.request(`{"path":"a.txt"}`))
	}
	return ids
}

// bigRequest writes a request whose start is about size bytes.
func (j *journal) bigRequest(size int) string {
	return j.request(`{"content":"` + strings.Repeat("x", size) + `"}`)
}

func buildLive(t *testing.T, j *journal, cfg WindowConfig, hooks func(*Window)) *Window {
	t.Helper()
	cfg.ServerID = testServer
	w := newWindow(j.reader, cfg)
	if hooks != nil {
		hooks(w)
	}
	w.mu.Lock()
	err := w.build(context.Background())
	w.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func actionIDs(w *Window) []string {
	var ids []string
	w.With(func(p *activity.Projection) {
		for _, a := range p.Actions() {
			ids = append(ids, a.Key.RequestID)
		}
	})
	return ids
}

func occurrences(w *Window) int {
	n := 0
	w.With(func(p *activity.Projection) { n = p.Occurrences() })
	return n
}

func TestLiveWindowJoinsAndCatchesUp(t *testing.T) {
	j := openJournal(t)
	want := j.requests(5)
	w := buildLive(t, j, WindowConfig{}, nil)
	if got := actionIDs(w); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("actions %v, want %v", got, want)
	}
	_, state, head, unseen := w.Info()
	if state != StateOK || unseen {
		t.Fatalf("state %v unseen %v", state, unseen)
	}
	more := j.requests(2)
	changes, next, hasMore, _, err := w.CatchUp(context.Background(), head, 4)
	if err != nil || hasMore || next == head {
		t.Fatalf("catch-up: %v more=%v next=%q", err, hasMore, next)
	}
	if len(changes.Actions) != 2 || changes.Actions[0].RequestID != more[0] || len(changes.Occurrences) != 4 {
		t.Fatalf("changes %+v", changes)
	}
	if _, _, _, _, err := w.CatchUp(context.Background(), "not-issued", 1); !errors.Is(err, ErrRebuild) {
		t.Fatalf("unknown cursor: %v", err)
	}
}

// TestAppendBetweenFrontierAndHistory (design A3): a row appended after F
// but before the history snapshot is delivered on both chains and applied
// once, in journal order.
func TestAppendBetweenFrontierAndHistory(t *testing.T) {
	j := openJournal(t)
	want := j.requests(3)
	var late string
	w := buildLive(t, j, WindowConfig{}, func(w *Window) {
		w.afterFrontier = func() { late = j.request(`{"path":"late"}`) }
	})
	want = append(want, late)
	if got := actionIDs(w); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("actions %v, want %v", got, want)
	}
	if occurrences(w) != 2*len(want)+0 {
		t.Fatalf("occurrences %d, want %d", occurrences(w), 2*len(want))
	}
}

// TestOverlapLargerThanAPage: many appends between F and the history page;
// the window joins only after the forward drain covered them all.
func TestOverlapLargerThanAPage(t *testing.T) {
	j := openJournal(t)
	want := j.requests(10)
	w := buildLive(t, j, WindowConfig{}, func(w *Window) {
		w.afterFrontier = func() { want = append(want, j.requests(250)...) }
	})
	// The first history page holds the newest rows; read older pages too.
	for w.Scan(context.Background(), 10, time.Now().Add(time.Minute)) == nil {
	}
	got := actionIDs(w)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %d actions in order? first %v last %v; want %d", len(got), got[:3], got[len(got)-3:], len(want))
	}
	w.With(func(p *activity.Projection) {
		if !p.Joined() {
			t.Fatal("not joined")
		}
	})
}

// TestRotationKeepsOccurrenceKeys (design A1): rows read by history in the
// active file and again by the live chain after the file was rotated into an
// archive are the same occurrences.
func TestRotationKeepsOccurrenceKeys(t *testing.T) {
	j := openJournal(t)
	want := j.requests(3)
	w := buildLive(t, j, WindowConfig{}, func(w *Window) {
		w.afterFrontier = func() { want = append(want, j.requests(2)...) } // Overlap rows.
		w.afterHistory = func() {
			for i := 0; i < 5; i++ { // Rotate the active file (4 MiB limit).
				want = append(want, j.bigRequest(1<<20))
			}
		}
	})
	matches, _ := filepath.Glob(j.path + ".*")
	if len(matches) == 0 {
		t.Fatal("expected a rotation")
	}
	if got := actionIDs(w); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("actions %v, want %v", got, want)
	}
	if occurrences(w) != 2*len(want) {
		t.Fatalf("occurrences %d, want %d: overlap rows were not recognized after rotation", occurrences(w), 2*len(want))
	}
}

func TestDeferredRowsAreHydrated(t *testing.T) {
	j := openJournal(t)
	var want []string
	for i := 0; i < 3; i++ { // Three ~3 MiB starts exceed the 8 MiB page budget.
		want = append(want, j.bigRequest(3<<20))
	}
	w := buildLive(t, j, WindowConfig{}, nil)
	if got := actionIDs(w); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("actions %v, want %v", got, want)
	}
	w.With(func(p *activity.Projection) {
		for _, a := range p.Actions() {
			if a.Input != activity.InputRecorded {
				t.Fatalf("%v input %v", a.Key, a.Input)
			}
		}
	})
}

func TestHistoryCapacityClosesTheChain(t *testing.T) {
	j := openJournal(t)
	j.requests(300) // 600 rows: three history pages.
	w := buildLive(t, j, WindowConfig{Capacity: 450}, nil)
	err := w.Scan(context.Background(), 5, time.Now().Add(time.Minute))
	if !errors.Is(err, ErrUseArchive) {
		t.Fatalf("scan: %v", err)
	}
	w.With(func(p *activity.Projection) {
		if c := p.Coverage(); !c.FloorSet || c.End != activity.CoverageEvicted {
			t.Fatalf("coverage %+v", c)
		}
	})
	if err := w.Scan(context.Background(), 1, time.Now().Add(time.Minute)); !errors.Is(err, ErrUseArchive) {
		t.Fatalf("scan after close: %v", err)
	}
}

func TestLiveCapacityRaisesTheFloor(t *testing.T) {
	j := openJournal(t)
	j.requests(100)
	w := buildLive(t, j, WindowConfig{Capacity: 300}, nil)
	_, _, head, _ := w.Info()
	newer := j.requests(150) // 300 more rows.
	changes, _, _, floorChanged, err := w.CatchUp(context.Background(), head, 10)
	if err != nil {
		t.Fatalf("catch-up: %v", err)
	}
	if len(changes.Actions) != len(newer) || occurrences(w) > 300 || !floorChanged {
		t.Fatalf("changes %d actions, occurrences %d", len(changes.Actions), occurrences(w))
	}
}

func TestLiveCapacityWithNothingToEvict(t *testing.T) {
	j := openJournal(t)
	w := buildLive(t, j, WindowConfig{Capacity: 220}, nil)
	_, _, head, _ := w.Info()
	for i := 0; i < 230; i++ { // Unfinished current-server requests are pinned.
		j.write(audit.Event{ServerID: testServer, RequestID: fmt.Sprintf("open-%d", i), Tool: "file_read", Phase: "request_start"})
	}
	if _, _, _, _, err := w.CatchUp(context.Background(), head, 10); !errors.Is(err, ErrCapacity) {
		t.Fatalf("catch-up: %v", err)
	}
	if _, state, _, _ := w.Info(); state != StateCapacity {
		t.Fatalf("state %v", state)
	}
}

// TestArchiveFromFloorLosesNothing (design A6): after eviction to a history
// boundary, an archive window from the floor reads every evicted row.
func TestArchiveFromFloorLosesNothing(t *testing.T) {
	j := openJournal(t)
	want := j.requests(300)
	w := buildLive(t, j, WindowConfig{Capacity: 450}, nil)
	_ = w.Scan(context.Background(), 5, time.Now().Add(time.Minute))
	var floor string
	w.With(func(p *activity.Projection) { floor = p.Coverage().Floor })
	archive, err := NewArchiveWindow(context.Background(), j.reader, audit.Cursor(floor), WindowConfig{ServerID: testServer})
	if err != nil {
		t.Fatal(err)
	}
	for archive.Scan(context.Background(), 10, time.Now().Add(time.Minute)) == nil {
	}
	seen := map[string]bool{}
	for _, id := range append(actionIDs(w), actionIDs(archive)...) {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("%s is in neither window", id)
		}
	}
}

func TestEmptyJournalAndRebuild(t *testing.T) {
	j := openJournal(t)
	w := buildLive(t, j, WindowConfig{}, nil)
	if len(actionIDs(w)) != 0 {
		t.Fatal("empty journal has actions")
	}
	j.requests(2)
	if err := w.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	gen, state, _, unseen := w.Info()
	if gen != 1 || state != StateOK || !unseen || len(actionIDs(w)) != 2 {
		t.Fatalf("after rebuild gen=%d state=%v unseen=%v actions=%v", gen, state, unseen, actionIDs(w))
	}
}

func TestDisabledReader(t *testing.T) {
	if _, err := NewLiveWindow(context.Background(), nil, WindowConfig{}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("nil reader: %v", err)
	}
}

// TestWriterSchemaDecodes is the schema-drift guard: every event kind the
// real writer produces decodes with the expected kind and keys.
func TestWriterSchemaDecodes(t *testing.T) {
	j := openJournal(t)
	events := []struct {
		e    audit.Event
		kind activity.Kind
	}{
		{audit.Event{ServerID: testServer, Tool: "server", Phase: "server_start", Status: "ok"}, activity.KindServerStart},
		{audit.Event{ServerID: testServer, RequestID: "r1", Tool: "file_write", Phase: "request_start", OperationID: "op1",
			Args:     json.RawMessage(`{"operation_id":"op1","content":"x"}`),
			CallMeta: json.RawMessage(`{"openai/session":"chat","io.modelcontextprotocol/clientInfo":{"name":"c"}}`)}, activity.KindRequestStart},
		{audit.Event{ServerID: testServer, RequestID: "r1", Tool: "file_write", Phase: "operation_start", OperationID: "op1"}, activity.KindOperationStart},
		{audit.Event{ServerID: testServer, RequestID: "r1", Tool: "file_write", Phase: "operation_finish", OperationID: "op1", Status: "applied", DurationMS: 3}, activity.KindOperationFinish},
		{audit.Event{ServerID: testServer, RequestID: "r1", Tool: "file_write", Phase: "request_finish", OperationID: "op1", Status: "applied", Replayed: true}, activity.KindRequestFinish},
		{audit.Event{ServerID: testServer, RequestID: "r2", Tool: "file_write", Phase: "request_rejected", Status: "rejected", Reason: "audit_record_too_large"}, activity.KindRequestRejected},
	}
	for _, ev := range events {
		j.write(ev.e)
	}
	f, err := os.Open(j.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	i := 0
	for scanner.Scan() {
		e, err := activity.Decode(scanner.Bytes())
		if err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if e.Kind != events[i].kind || e.ServerID != testServer || e.Legacy || e.SchemaVersion != audit.EventSchemaVersion {
			t.Fatalf("line %d: %+v", i, e)
		}
		if e.Kind == activity.KindRequestStart {
			if key, ok := e.Identity.Key(); !ok || key.ConversationID != "chat" || *e.Identity.ClientName != "c" {
				t.Fatalf("identity %+v", e.Identity)
			}
		}
		if e.Kind == activity.KindRequestFinish && (!e.Replayed || e.OperationID != "op1") {
			t.Fatalf("finish %+v", e)
		}
		i++
	}
	if i != len(events) {
		t.Fatalf("decoded %d of %d", i, len(events))
	}
}

// TestQuietJournalJoinsWithoutAnAppend (design A4).
func TestQuietJournalJoinsWithoutAnAppend(t *testing.T) {
	j := openJournal(t)
	j.requests(3)
	w := buildLive(t, j, WindowConfig{}, nil)
	w.With(func(p *activity.Projection) {
		if !p.Joined() || p.LiveSeed(true) == "" {
			t.Fatalf("joined=%v seed=%q", p.Joined(), p.LiveSeed(true))
		}
	})
}

// TestCatchUpFromAnOlderCursor: the cursor issued is always the head, and a
// later live-region eviction never makes a resume redeliver rows as new.
func TestCatchUpFromAnOlderCursor(t *testing.T) {
	j := openJournal(t)
	w := buildLive(t, j, WindowConfig{}, nil)
	_, _, first, _ := w.Info()
	j.requests(3)
	_, second, _, _, err := w.CatchUp(context.Background(), first, 4)
	if err != nil {
		t.Fatal(err)
	}
	j.requests(2)
	changes, head, _, _, err := w.CatchUp(context.Background(), first, 4) // Older cursor.
	if err != nil {
		t.Fatal(err)
	}
	_, _, current, _ := w.Info()
	if head != current || len(changes.Actions) != 5 {
		t.Fatalf("head %q current %q changes %d", head, current, len(changes.Actions))
	}
	// Idle catch-up does not issue a duplicate cursor.
	if _, again, _, _, err := w.CatchUp(context.Background(), head, 4); err != nil || again != head {
		t.Fatalf("idle catch-up %q %v", again, err)
	}
	w.With(func(p *activity.Projection) {
		if !p.CanResume(second) || !p.CanResume(first) {
			t.Fatal("older cursors left the resume window")
		}
		if _, err := p.RaiseFloor(""); err != nil {
			t.Fatal(err)
		}
	})
	before := occurrences(w)
	changes, _, _, _, err = w.CatchUp(context.Background(), second, 4)
	if err != nil || len(changes.Occurrences) != 4 || occurrences(w) != before {
		t.Fatalf("resume after eviction: %v changes %d occurrences %d→%d", err, len(changes.Occurrences), before, occurrences(w))
	}
}

func TestMalformedRowKeepsItsReference(t *testing.T) {
	j := openJournal(t)
	j.requests(1)
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{not json}\n")
	_ = f.Close()
	// The writer did not append it: reopen so it becomes visible history.
	_ = j.log.Close()
	j2 := openJournalAt(t, j.path)
	w := buildLive(t, j2, WindowConfig{}, nil)
	w.With(func(p *activity.Projection) {
		d := p.Diagnostics()
		if len(d) != 1 || d[0].Key.Ref == "" || d[0].PossibleRepeat {
			t.Fatalf("diagnostics %+v", d)
		}
	})
}

func farFuture() time.Time { return time.Now().Add(time.Hour) }
