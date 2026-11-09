package dashboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"terminalmcp/internal/activity"
	"terminalmcp/internal/audit"
	"terminalmcp/internal/viewer"
	"terminalmcp/internal/viewer/viewertest"
)

func chatStart(j *journal, id, chat string) {
	j.write(audit.Event{ServerID: testServer, RequestID: id, Tool: "terminal_start", Phase: "request_start",
		Args:     json.RawMessage(`{"argv":["go","test","./..."],"cwd":"/tmp"}`),
		CallMeta: json.RawMessage(`{"openai/session":"` + chat + `"}`)})
}

func chatFinish(j *journal, id string) {
	j.write(audit.Event{ServerID: testServer, RequestID: id, Tool: "terminal_start", Phase: "request_finish", Status: "ok"})
}

func newManager(j *journal) *Manager {
	return NewManager(j.reader, ManagerConfig{ServerID: testServer})
}

func journalLines(t *testing.T, j *journal) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(j.path)
	if err != nil {
		t.Fatal(err)
	}
	lines := map[string]bool{}
	for _, l := range bytes.SplitAfter(data, []byte("\n")) {
		if len(l) > 0 {
			lines[string(l)] = true
		}
	}
	return lines
}

func TestRecentDeliversWholeActionsWithExactRecords(t *testing.T) {
	j := openJournal(t)
	for i, chat := range []string{"chat-a", "chat-b", "chat-a"} {
		id := []string{"a1", "b1", "a2"}[i]
		chatStart(j, id, chat)
		chatFinish(j, id)
	}
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, err := mg.Open(context.Background(), s, ModeLive, "")
	if err != nil {
		t.Fatal(err)
	}
	page, err := mg.Recent(context.Background(), s, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestIDs(page); got != "a2 b1 a1" || len(page.Records) != 6 || page.Next != nil {
		t.Fatalf("page %+v", page)
	}
	lines := journalLines(t, j)
	for _, r := range page.Records {
		data, err := base64.StdEncoding.DecodeString(r.DataB64)
		if err != nil || !lines[string(data)] || r.Size != len(data) {
			t.Fatalf("record %s is not an exact journal line", r.Occurrence)
		}
	}
	if page.Items[0].Summary == nil || page.Items[0].Summary.Text != "go test ./..." {
		t.Fatalf("summary %+v", page.Items[0].Summary)
	}
	lanes, err := mg.Lanes(context.Background(), s, id)
	if err != nil || len(lanes) != 2 || *lanes[0].ConversationID != "chat-a" || lanes[0].Items != 2 {
		t.Fatalf("lanes %+v %v", lanes, err)
	}
	lanePage, err := mg.LaneItems(context.Background(), s, id, lanes[0].ID, nil)
	if err != nil || requestIDs(lanePage) != "a2 a1" {
		t.Fatalf("lane page %+v %v", lanePage, err)
	}
	// Another session cannot use this window.
	if _, err := mg.Recent(context.Background(), viewertest.NewSession(nil), id, nil); !errors.Is(err, ErrWindowGone) {
		t.Fatalf("cross-session: %v", err)
	}
}

func TestPagingContinuesAndRetriesAreIdempotent(t *testing.T) {
	j := openJournal(t)
	for i := 0; i < 150; i++ { // 300 records: over the 200-record limit.
		chatStart(j, "r"+strings.Repeat("0", 3-len(itoa(i)))+itoa(i), "chat")
		chatFinish(j, "r"+strings.Repeat("0", 3-len(itoa(i)))+itoa(i))
	}
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	_, _ = mg.Scan(context.Background(), s, id) // Load all 300 rows.
	first, _ := mg.Recent(context.Background(), s, id, nil)
	again, _ := mg.Recent(context.Background(), s, id, nil)
	if first.Next == nil || requestIDs(first) != requestIDs(again) {
		t.Fatalf("first page %d items next=%v; retry %d", len(first.Items), first.Next, len(again.Items))
	}
	second, _ := mg.Recent(context.Background(), s, id, first.Next)
	seen := map[string]bool{}
	for _, a := range append(first.Items, second.Items...) {
		if seen[a.Key] {
			t.Fatalf("%v delivered twice", a.Key)
		}
		seen[a.Key] = true
	}
	if len(first.Items)+len(second.Items) != 150 || second.Next != nil {
		t.Fatalf("delivered %d items", len(first.Items)+len(second.Items))
	}
}

func itoa(i int) string {
	return strings.TrimSpace(strings.Replace(time.Duration(i).String(), "ns", "", 1))
}

// TestLateOriginIsDeliveredWithItsFinish: the finish is in the first history
// page, the start in an older one; after scanning, the lane delivers the
// action with both records.
func TestLateOriginIsDeliveredWithItsFinish(t *testing.T) {
	j := openJournal(t)
	chatStart(j, "late", "chat-late")
	j.requests(100) // 200 rows push the start out of the first page.
	chatFinish(j, "late")
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	lanes, _ := mg.Lanes(context.Background(), s, id)
	if !hasLaneKind(lanes, laneOriginNotLoaded) {
		t.Fatalf("expected an origin_not_loaded lane: %+v", lanes)
	}
	if _, err := mg.Origins(context.Background(), s, id); err != nil {
		t.Fatal(err)
	}
	lanes, _ = mg.Lanes(context.Background(), s, id)
	var lane string
	for _, l := range lanes {
		if l.ConversationID != nil && *l.ConversationID == "chat-late" {
			lane = l.ID
		}
	}
	page, err := mg.LaneItems(context.Background(), s, id, lane, nil)
	if err != nil || len(page.Items) != 1 || len(page.Records) != 2 || page.Items[0].Action.Input != activity.InputRecorded {
		t.Fatalf("late lane %+v %v", page, err)
	}
}

func hasLaneKind(lanes []Lane, kind string) bool {
	for _, l := range lanes {
		if l.Kind == kind {
			return true
		}
	}
	return false
}

func TestLargeRecordsAreDeferredButPagingAdvances(t *testing.T) {
	j := openJournal(t)
	big := []string{j.bigRequest(3 << 20), j.bigRequest(3 << 20)}
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	page, _ := mg.Recent(context.Background(), s, id, nil)
	if requestIDs(page) != big[1] || page.Next == nil {
		t.Fatalf("first page %s", requestIDs(page))
	}
	deferred := 0
	for _, r := range page.Records {
		if r.Deferred && r.Size > 3<<20 {
			deferred++
			data, err := mg.RecordBytes(context.Background(), s, id, r.Occurrence)
			if err != nil || len(data) != r.Size {
				t.Fatalf("record %v %d", err, len(data))
			}
		}
	}
	if deferred != 1 {
		t.Fatalf("deferred %d", deferred)
	}
	next, _ := mg.Recent(context.Background(), s, id, page.Next)
	if requestIDs(next) != big[0] {
		t.Fatalf("second page %s", requestIDs(next))
	}
	if _, err := mg.RecordBytes(context.Background(), s, id, "not-known"); !errors.Is(err, ErrUnknownOccurrence) {
		t.Fatalf("unknown occurrence: %v", err)
	}
}

func TestArchiveWindowsSlideAndSessionsRelease(t *testing.T) {
	j := openJournal(t)
	j.requests(300)
	now := time.Now()
	mg := NewManager(j.reader, ManagerConfig{ServerID: testServer, Capacity: 450, Now: func() time.Time { return now }})
	s := viewertest.NewSession(nil)
	live, _ := mg.Open(context.Background(), s, ModeLive, "")
	if _, err := mg.Open(context.Background(), s, ModeArchive, live); !errors.Is(err, ErrNoFloor) {
		t.Fatalf("archive without floor: %v", err)
	}
	if cov, err := mg.Scan(context.Background(), s, live); err != nil || !cov.UseArchive {
		t.Fatalf("scan: %+v %v", cov, err)
	}
	a1, err := mg.Open(context.Background(), s, ModeArchive, live)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := mg.Open(context.Background(), s, ModeArchive, live)
	if err != nil || mg.Windows() != 2 {
		t.Fatalf("sliding archive: %v windows %d", err, mg.Windows())
	}
	if _, err := mg.Recent(context.Background(), s, a1, nil); !errors.Is(err, ErrWindowGone) {
		t.Fatalf("replaced archive: %v", err)
	}
	if _, err := mg.Recent(context.Background(), s, a2, nil); err != nil {
		t.Fatal(err)
	}
	s.End(viewer.ErrLoggedOut, time.Now())
	if mg.Windows() != 0 {
		t.Fatalf("windows after logout %d", mg.Windows())
	}
	s2 := viewertest.NewSession(nil)
	if _, err := mg.Open(context.Background(), s2, ModeLive, ""); err != nil {
		t.Fatal(err)
	}
	now = now.Add(idleRelease)
	if mg.Windows() != 0 {
		t.Fatal("idle window kept")
	}
}

func TestRebuildBacksOffAndFails(t *testing.T) {
	j := openJournal(t)
	now := time.Now()
	mg := NewManager(j.reader, ManagerConfig{ServerID: testServer, Now: func() time.Time { return now }})
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	m, _ := mg.get(s, id)
	broken := func(*managed) error { return ErrRebuild }
	if err := mg.run(context.Background(), s, id, broken); !errors.Is(err, ErrRebuild) {
		t.Fatalf("successful rebuild must ask for a new snapshot: %v", err)
	}
	m.win.buildErr = func() error { return errors.New("disk") }
	var rebuilding *RebuildingError
	for i := 0; i < len(rebuildBackoff); i++ {
		if err := mg.run(context.Background(), s, id, broken); !errors.As(err, &rebuilding) {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if err := mg.run(context.Background(), s, id, broken); !errors.As(err, &rebuilding) {
			t.Fatalf("backoff %d: %v", i, err)
		}
		now = now.Add(rebuilding.RetryAfter)
	}
	var state *StateError
	if err := mg.run(context.Background(), s, id, broken); !errors.As(err, &state) || state.State != StateFailed {
		t.Fatalf("after backoff: %v", err)
	}
}

func TestCatchUpSendsOnlyMissingRecordsAndPages(t *testing.T) {
	j := openJournal(t)
	chatStart(j, "long", "chat")
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	m, _ := mg.get(s, id)
	gen, _, head, _ := m.win.Info()
	chatFinish(j, "long") // The long-running action finishes.
	for i := 0; i < 120; i++ {
		chatStart(j, "n"+itoa(i), "chat")
		chatFinish(j, "n"+itoa(i))
	}
	res, err := mg.CatchUp(context.Background(), s, id, gen, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Records {
		if r.Deferred {
			continue
		}
		data, _ := base64.StdEncoding.DecodeString(r.DataB64)
		if strings.Contains(string(data), `"request_id":"long","tool":"terminal_start","phase":"request_start"`) {
			t.Fatal("catch-up resent a record the client already had")
		}
	}
	total := len(res.Items)
	for next := res.Next; next != nil; {
		more, err := mg.CatchUp(context.Background(), s, id, gen, head, next)
		if err != nil {
			t.Fatal(err)
		}
		total += len(more.Items)
		next = more.Next
	}
	if total != 121 || res.Head == head {
		t.Fatalf("changed actions %d head moved %v", total, res.Head != head)
	}
	if _, err := mg.CatchUp(context.Background(), s, id, gen, "other", res.Next); !errors.Is(err, ErrGenerationChanged) {
		t.Fatalf("stale continuation: %v", err)
	}
}

func requestIDs(p Page) string {
	var ids []string
	for _, it := range p.Items {
		if it.Action != nil {
			ids = append(ids, it.Action.Key.RequestID)
		} else {
			ids = append(ids, it.Kind)
		}
	}
	return strings.Join(ids, " ")
}
