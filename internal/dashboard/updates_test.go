package dashboard

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"terminalmcp/internal/audit"
	"terminalmcp/internal/viewer/viewertest"
)

// TestItemWireFormat pins the item JSON: snake_case views, the documented
// camelCase identity, the order key and wire occurrence IDs that name the
// records of the same answer.
func TestItemWireFormat(t *testing.T) {
	j := openJournal(t)
	chatStart(j, "w1", "chat-w")
	chatFinish(j, "w1")
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	env, err := mg.Snapshot(context.Background(), s, id)
	if err != nil {
		t.Fatal(err)
	}
	if env.ServerOS != runtime.GOOS {
		t.Fatalf("server_os %q", env.ServerOS)
	}
	data, _ := json.Marshal(env.Recent)
	var raw struct {
		Items   []map[string]json.RawMessage `json:"items"`
		Records []struct {
			Occurrence string `json:"occurrence"`
		} `json:"records"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var action struct {
		Key      map[string]string `json:"key"`
		Identity map[string]any    `json:"identity"`
		State    string            `json:"state"`
		Input    string            `json:"input"`
		Order    struct {
			Live bool  `json:"live"`
			Rank int64 `json:"rank"`
		} `json:"order"`
		Start struct {
			Primary     string   `json:"primary"`
			Occurrences []string `json:"occurrences"`
		} `json:"start"`
	}
	if err := json.Unmarshal(raw.Items[0]["action"], &action); err != nil {
		t.Fatal(err)
	}
	if action.Key["request_id"] != "w1" || action.Identity["conversationId"] != "chat-w" || action.State != "finished" ||
		action.Input != "recorded" || action.Order.Rank == 0 {
		t.Fatalf("action %s", raw.Items[0]["action"])
	}
	found := false
	for _, r := range raw.Records {
		found = found || r.Occurrence == action.Start.Primary
	}
	if !found || action.Start.Primary == "" {
		t.Fatalf("start.primary %q is not a record of the answer", action.Start.Primary)
	}
}

// TestOriginsAbsorbsTheSeparateEffect: an effect delivered on its own until
// its request is found is removed, and the found action is an update.
func TestOriginsAbsorbsTheSeparateEffect(t *testing.T) {
	j := openJournal(t)
	j.write(audit.Event{ServerID: testServer, RequestID: "orig", Tool: "file_write", Phase: "request_start",
		Args: map[string]any{"path": "a.txt", "content": "x"}, CallMeta: map[string]any{"openai/session": "chat-o"}})
	j.requests(150) // Push the start out of the first history page.
	j.write(audit.Event{ServerID: testServer, RequestID: "orig", OperationID: "op-1", Tool: "file_write", Phase: "operation_start"})
	j.write(audit.Event{ServerID: testServer, RequestID: "orig", OperationID: "op-1", Tool: "file_write", Phase: "operation_finish", Status: "ok"})
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	env, _ := mg.Snapshot(context.Background(), s, id)
	if !strings.Contains(requestIDs(env.Recent), "effect") {
		t.Fatalf("expected a separate effect item first: %s", requestIDs(env.Recent))
	}
	page, err := mg.Backfill(context.Background(), s, id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(page.Removed, "e:"+testServer+"/op-1") {
		t.Fatalf("removed %v", page.Removed)
	}
	var updated bool
	for _, it := range page.Updated {
		updated = updated || (it.Action != nil && it.Action.Key.RequestID == "orig" && it.Effect != nil)
	}
	if !updated || len(page.AffectedLanes) < 2 || len(page.Records) == 0 {
		t.Fatalf("updated %d lanes %v records %d", len(page.Updated), page.AffectedLanes, len(page.Records))
	}
}

// TestEarlierReportsLateOrigins: an earlier-page scan that finds the start
// of an action loaded without it reports the action with its new lane.
func TestEarlierReportsLateOrigins(t *testing.T) {
	j := openJournal(t)
	chatStart(j, "late", "chat-late")
	j.requests(100)
	chatFinish(j, "late")
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	env, _ := mg.Snapshot(context.Background(), s, id)
	var unattributed string
	for _, l := range env.Lanes {
		if l.Kind == laneUnattributed {
			unattributed = l.ID
		}
	}
	// Page the unattributed lane to its end, then ask for earlier items.
	var before *Continuation
	for {
		p, err := mg.LaneItems(context.Background(), s, id, unattributed, before)
		if err != nil {
			t.Fatal(err)
		}
		last := p.Items[len(p.Items)-1]
		before = &Continuation{Order: last.Order, Key: last.Key}
		if p.Next == nil {
			break
		}
	}
	page, err := mg.Earlier(context.Background(), s, id, unattributed, before)
	if err != nil {
		t.Fatal(err)
	}
	var moved *Item
	for i, it := range page.Updated {
		if it.Action != nil && it.Action.Key.RequestID == "late" {
			moved = &page.Updated[i]
		}
	}
	if moved == nil || moved.Action.Identity.ConversationID == nil || *moved.Action.Identity.ConversationID != "chat-late" {
		t.Fatalf("late origin not reported: %+v", page.ScanUpdates)
	}
	if !slices.Contains(page.AffectedLanes, moved.Lane) {
		t.Fatalf("affected lanes %v miss %s", page.AffectedLanes, moved.Lane)
	}
}

func TestLanePagesIncludeRestarts(t *testing.T) {
	j := openJournal(t)
	chatStart(j, "r1", "chat-r")
	chatFinish(j, "r1")
	j.write(audit.Event{ServerID: "srv-second", Tool: "server", Phase: "server_start", Status: "ok"})
	j.write(audit.Event{ServerID: "srv-second", RequestID: "r2", Tool: "terminal_start", Phase: "request_start",
		Args: map[string]any{"argv": []string{"ls"}}, CallMeta: map[string]any{"openai/session": "chat-r"}})
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	lanes, _ := mg.Lanes(context.Background(), s, id)
	var lane string
	for _, l := range lanes {
		if l.Kind == laneConversation {
			lane = l.ID
		}
	}
	page, err := mg.LaneItems(context.Background(), s, id, lane, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestIDs(page); got != "r2 server_start r1" {
		t.Fatalf("lane items %q", got)
	}
	for _, l := range lanes {
		if l.ID == lane && l.Items != 2 {
			t.Fatalf("lane counts %d items", l.Items)
		}
	}
}

// TestScanReportsAResolvedProvisionalRow: an oversized row at the oldest
// edge of the first page is provisional; the scan that loads its older
// neighbour replaces it, and the client is told both sides.
func TestScanReportsAResolvedProvisionalRow(t *testing.T) {
	j := openJournal(t)
	j.requests(5)
	_ = j.log.Close()
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"x":"` + strings.Repeat("o", 5<<20) + "\"}\n") // Oversized: no reader reference.
	_ = f.Close()
	j = openJournalAt(t, j.path)
	j.requests(99)
	j.write(audit.Event{ServerID: testServer, Tool: "server", Phase: "server_start", Status: "ok"}) // 199 newer rows.
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	env, _ := mg.Snapshot(context.Background(), s, id)
	var provisional string
	for _, it := range allItems(t, mg, s, id, env) {
		if it.Kind == ItemDiagnostic && strings.HasPrefix(it.Key, "d:p:") {
			provisional = it.Key
		}
	}
	if provisional == "" {
		t.Skip("the oversized row was not provisional at the page edge")
	}
	page, err := mg.Discover(context.Background(), s, id)
	if err != nil {
		t.Fatal(err)
	}
	var resolved bool
	for _, it := range page.Updated {
		resolved = resolved || (it.Kind == ItemDiagnostic && strings.HasPrefix(it.Key, "d:w:"))
	}
	if !slices.Contains(page.Removed, provisional) || !resolved {
		t.Fatalf("removed %v updated %d", page.Removed, len(page.Updated))
	}
}

// allItems pages the whole snapshot.
func allItems(t *testing.T, mg *Manager, s *viewertest.Session, id string, env Envelope) []Item {
	t.Helper()
	items := env.Recent.Items
	for next := env.Recent.Next; next != nil; {
		p, err := mg.Recent(context.Background(), s, id, next)
		if err != nil {
			t.Fatal(err)
		}
		items, next = append(items, p.Items...), p.Next
	}
	return items
}

// TestScanReportsAnOlderConflictingCopy: an older copy of a loaded start
// with other bytes becomes its primary record and marks the conflict.
func TestScanReportsAnOlderConflictingCopy(t *testing.T) {
	j := openJournal(t)
	chatStart(j, "c1", "chat-old")
	j.requests(150)
	chatStart(j, "c1", "chat-new") // A later copy with other bytes.
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	page, err := mg.Discover(context.Background(), s, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range page.Updated {
		if it.Action != nil && it.Action.Key.RequestID == "c1" {
			if !it.Action.Start.Conflict || len(page.AffectedLanes) == 0 {
				t.Fatalf("c1 %+v lanes %v", it.Action.Start, page.AffectedLanes)
			}
			return
		}
	}
	t.Fatalf("the conflicting copy was not reported: %+v", page.ScanUpdates)
}

// TestConcurrentScansKeepTheirChanges: tracked scans of one window are
// serialized, so neither loses the other's changes.
func TestConcurrentScansKeepTheirChanges(t *testing.T) {
	j := openJournal(t)
	chatStart(j, "late", "chat-late")
	j.requests(300)
	chatFinish(j, "late")
	mg := newManager(j)
	s := viewertest.NewSession(nil)
	id, _ := mg.Open(context.Background(), s, ModeLive, "")
	var wg sync.WaitGroup
	results := make([]LanesPage, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := mg.Discover(context.Background(), s, id)
			if err != nil {
				t.Error(err)
			}
			results[i] = p
		}()
	}
	wg.Wait()
	found := 0
	for _, p := range results {
		for _, it := range p.Updated {
			if it.Action != nil && it.Action.Key.RequestID == "late" {
				found++
			}
		}
	}
	if found != 1 {
		t.Fatalf("the late origin was reported %d times", found)
	}
}
