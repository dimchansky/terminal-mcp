package activity

import (
	"errors"
	"testing"
)

// pagedWindow applies history pages with recorded boundaries.
func pagedWindow(t *testing.T) *window {
	t.Helper()
	w := newWindow(t)
	page := func(next string, items ...[2]string) {
		for _, it := range items {
			w.history(it[0], it[1])
		}
		if err := w.p.EndHistoryPage(next); err != nil {
			t.Fatal(err)
		}
		if next != "p3" {
			if err := w.p.BeginHistoryPage(next); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Newest page: a finished action r3 and an unfinished read r4.
	page("p1",
		[2]string{"r4s", reqStart(current, "r4", "terminal_list", "", "")},
		[2]string{"r3f", reqFinish(current, "r3", "terminal_list", `,"status":"ok"`)},
		[2]string{"r3s", reqStart(current, "r3", "terminal_list", "", "")})
	// Older page: a finished action r2 and a download still in flight.
	page("p2",
		[2]string{"r2f", reqFinish(current, "r2", "terminal_list", `,"status":"ok"`)},
		[2]string{"r2s", reqStart(current, "r2", "terminal_list", "", "")},
		[2]string{"dlf", reqFinish(current, "dl", "file_receive", `,"operation_id":"op","status":"running"`)},
		[2]string{"dlo", opStart(current, "dl", "file_receive", "op")},
		[2]string{"dls", reqStart(current, "dl", "file_receive", "op", "")})
	// Oldest page: a finished action of an earlier server and a diagnostic.
	page("p3",
		[2]string{"old", reqStart("srv-old", "r1", "file_write", "op1", "")},
		[2]string{"bad", `{"phase":`})
	if err := w.p.LiveCaughtUp(); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestRaiseFloorEvictsOnlyOlderFinishedWork(t *testing.T) {
	w := pagedWindow(t)
	n, err := w.p.RaiseFloor("p1")
	if err != nil {
		t.Fatal(err)
	}
	// Evicted: r2 (2 rows), r1 of the old server (1), the diagnostic (1).
	// Kept: the in-flight download and its origin, and everything newer.
	if n != 4 {
		t.Fatalf("evicted %d occurrences", n)
	}
	for _, key := range []RequestKey{{current, "r3"}, {current, "r4"}, {current, "dl"}} {
		if _, ok := w.p.Action(key); !ok {
			t.Fatalf("%v evicted", key)
		}
	}
	for _, key := range []RequestKey{{current, "r2"}, {"srv-old", "r1"}} {
		if _, ok := w.p.Action(key); ok {
			t.Fatalf("%v kept", key)
		}
	}
	if e := w.effect(current, "op"); e.State != EffectInFlight {
		t.Fatalf("in-flight effect %+v", e)
	}
	if len(w.p.Diagnostics()) != 0 {
		t.Fatalf("diagnostics %+v", w.p.Diagnostics())
	}
	if c := w.p.Coverage(); c.End != CoverageEvicted || c.Floor != "p1" || !c.FloorSet {
		t.Fatalf("coverage %+v", c)
	}
	// The history chain is closed: a re-read cannot resurrect evicted rows.
	if _, err := w.p.Apply(Record{Delivery: Delivery{OccurrenceKey{Ref: "r2s"}, ChainHistory, -100}, Raw: []byte(reqStart(current, "r2", "terminal_list", "", ""))}); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("history after floor: %v", err)
	}
}

func TestFloorRules(t *testing.T) {
	p := NewProjection(current)
	if _, err := p.RaiseFloor(""); !errors.Is(err, ErrFloor) {
		t.Fatalf("floor before join: %v", err)
	}
	w := pagedWindow(t)
	if _, err := w.p.RaiseFloor("not-a-boundary"); !errors.Is(err, ErrFloor) {
		t.Fatalf("unknown boundary: %v", err)
	}
	if _, err := w.p.RaiseFloor("p2"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.p.RaiseFloor("p3"); !errors.Is(err, ErrFloor) {
		t.Fatalf("lowering the floor: %v", err)
	}
	if _, err := w.p.RaiseFloor("p1"); err != nil {
		t.Fatalf("raising further: %v", err)
	}
	if _, err := w.p.RaiseFloor(""); err != nil {
		t.Fatalf("live-region floor: %v", err)
	}
	if _, err := w.p.RaiseFloor("p1"); !errors.Is(err, ErrFloor) {
		t.Fatalf("history floor after live-region floor: %v", err)
	}
	if err := w.p.EndHistoryPage("p4"); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("page after the chain closed: %v", err)
	}
}

func TestLiveRegionEvictionRespectsResumeWindow(t *testing.T) {
	w := newWindow(t)
	for i := 1; i <= liveResumeWindow+3; i++ {
		req := string(rune('a' - 1 + i))
		w.onLive(req+"s", reqStart(current, req, "terminal_list", "", ""))
		w.onLive(req+"f", reqFinish(current, req, "terminal_list", `,"status":"ok"`))
		w.p.IssueLiveCursor("c" + req)
	}
	if err := w.p.LiveCaughtUp(); err != nil {
		t.Fatal(err)
	}
	if w.p.CanResume("ca") || !w.p.CanResume("cd") {
		t.Fatal("resume window must hold the latest cursors only")
	}
	if _, err := w.p.RaiseFloor(""); err != nil {
		t.Fatal(err)
	}
	// The oldest resumable cursor is after "d": a, b, c, d are evicted.
	for i, req := range []string{"a", "b", "c", "d", "e"} {
		_, ok := w.p.Action(RequestKey{current, req})
		if ok != (i == 4) {
			t.Fatalf("action %s kept=%v", req, ok)
		}
	}
	if !w.p.CanResume("cd") {
		t.Fatal("eviction must not shrink the resume window")
	}
	// Resuming from the oldest resumable cursor redelivers only newer rows,
	// which are still known: nothing reappears as new activity.
	if c := w.onLive("es", reqStart(current, "e", "terminal_list", "", "")); c.Kind != ChangeRedelivery {
		t.Fatalf("resume after eviction: %+v", c)
	}
	// The live chain grows; a second live-region floor evicts again.
	for _, req := range []string{"x", "y"} {
		w.onLive(req+"s", reqStart(current, req, "terminal_list", "", ""))
		w.onLive(req+"f", reqFinish(current, req, "terminal_list", `,"status":"ok"`))
		w.p.IssueLiveCursor("c" + req)
	}
	if n, err := w.p.RaiseFloor(""); err != nil || n != 4 {
		t.Fatalf("second live-region floor evicted %d: %v", n, err)
	}
	if _, ok := w.p.Action(RequestKey{current, "e"}); ok {
		t.Fatal("e must be evicted once its cursor left the resume window")
	}
}

func TestArchiveWindowRefusesLiveRegionFloor(t *testing.T) {
	p := NewProjection(current)
	if err := p.HistoryOnly(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RaiseFloor(""); !errors.Is(err, ErrFloor) {
		t.Fatalf("archive live-region floor: %v", err)
	}
}

func TestCapacityRetryUsesTheSameRank(t *testing.T) {
	w := pagedWindow(t)
	w.p.SetCapacity(len(w.p.occ.byKey))
	rec := Record{Delivery: Delivery{OccurrenceKey{Ref: "new"}, ChainLive, 1}, Raw: []byte(reqStart(current, "n", "terminal_list", "", ""))}
	if _, err := w.p.Apply(rec); !errors.Is(err, ErrCapacity) {
		t.Fatalf("over capacity: %v", err)
	}
	if _, err := w.p.RaiseFloor("p1"); err != nil {
		t.Fatal(err)
	}
	if c, err := w.p.Apply(rec); err != nil || c.Kind != ChangeNewAction {
		t.Fatalf("retry with the same rank: %+v %v", c, err)
	}
}

func TestCapacityRefusesNewOccurrencesOnly(t *testing.T) {
	w := newWindow(t)
	w.p.SetCapacity(2)
	w.onLive("a", reqStart(current, "a", "terminal_list", "", ""))
	w.onLive("b", reqFinish(current, "a", "terminal_list", `,"status":"ok"`))
	_, err := w.p.Apply(Record{Delivery: Delivery{OccurrenceKey{Ref: "c"}, ChainLive, 99}, Raw: []byte(reqStart(current, "c", "terminal_list", "", ""))})
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("over capacity: %v", err)
	}
	if c := w.onLive("a", reqStart(current, "a", "terminal_list", "", "")); c.Kind != ChangeRedelivery {
		t.Fatalf("redelivery at capacity: %+v", c)
	}
	if len(w.p.Actions()) != 1 {
		t.Fatal("refused delivery changed the window")
	}
}

func TestCoverageEndMarksNeedsBeyond(t *testing.T) {
	w := newWindow(t)
	w.onLive("f", reqFinish(current, "r", "terminal_read", `,"status":"ok"`))
	if needs := w.p.Needs(); len(needs) != 1 || needs[0].BeyondCoverage {
		t.Fatalf("open coverage: %+v", needs)
	}
	if err := w.p.EndHistory(CoverageEvicted); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("eviction is not an EndHistory reason: %v", err)
	}
	if err := w.p.EndHistory(CoverageGap); err != nil {
		t.Fatal(err)
	}
	if needs := w.p.Needs(); len(needs) != 1 || !needs[0].BeyondCoverage {
		t.Fatalf("after gap: %+v", needs)
	}
	if err := w.p.EndHistory(CoverageReachedStart); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("second end: %v", err)
	}
	if c := w.p.Coverage(); c.End != CoverageGap || c.FloorSet {
		t.Fatalf("coverage %+v", c)
	}
}

func TestHistoryStopsForGoodAfterAGap(t *testing.T) {
	w := newWindow(t) // Page "h1" is open.
	if err := w.p.EndHistory(CoverageGap); err != nil {
		t.Fatal(err)
	}
	if err := w.p.BeginHistoryPage("h1"); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("page after a gap: %v", err)
	}
	if _, err := w.p.Apply(Record{Delivery: Delivery{OccurrenceKey{Ref: "x"}, ChainHistory, -50}, Raw: []byte(reqStart(current, "x", "terminal_list", "", ""))}); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("history row after a gap: %v", err)
	}
}

func TestRaiseFloorClosesAnOpenPageAndDropsProvisionalRows(t *testing.T) {
	w := newWindow(t)
	var carry HistoryCarry
	carry.BeginPage("h1")
	key, _ := carry.Key(Row{Start: 10, End: 20}) // Provisional: its neighbour never arrives.
	w.hist--
	w.apply(Record{Delivery: Delivery{key, ChainHistory, w.hist}, Problem: "too large"})
	if err := w.p.EndHistoryPage("p1"); err != nil {
		t.Fatal(err)
	}
	if err := w.p.BeginHistoryPage("p1"); err != nil { // Left open.
		t.Fatal(err)
	}
	if err := w.p.LiveCaughtUp(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.p.RaiseFloor(""); err != nil {
		t.Fatal(err)
	}
	if len(w.p.Diagnostics()) != 0 {
		t.Fatalf("provisional row kept: %+v", w.p.Diagnostics())
	}
	if _, err := w.p.Apply(Record{Delivery: Delivery{OccurrenceKey{Ref: "y"}, ChainHistory, w.hist - 1}, Raw: []byte(reqStart(current, "y", "terminal_list", "", ""))}); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("history on a page open before the raise: %v", err)
	}
	// A late resolution from the discarded carry names an evicted row.
	_, rs := carry.Key(Row{Ref: "A"})
	if _, err := w.p.Resolve(rs[0]); !errors.Is(err, ErrResolve) {
		t.Fatalf("late resolution: %v", err)
	}
}

func TestDashboardAccessors(t *testing.T) {
	w := pagedWindow(t)
	if got := w.p.Boundaries(); len(got) != 4 || got[0] != "h1" || got[3] != "p3" {
		t.Fatalf("boundaries %v", got)
	}
	before := w.p.Occurrences()
	w.onLive("l1", reqFinish(current, "r4", "terminal_list", `,"status":"ok"`))
	w.onLive("l2", opFinish(current, "dl", "file_receive", "op", ""))
	if w.p.Occurrences() != before+2 {
		t.Fatalf("occurrences %d, before %d", w.p.Occurrences(), before)
	}
	c := w.p.ChangedSince(0)
	if len(c.Occurrences) != 2 || c.Occurrences[0].Ref != "l1" ||
		len(c.Actions) != 1 || c.Actions[0].RequestID != "r4" || len(c.Effects) != 1 {
		t.Fatalf("changes %+v", c)
	}
	if c := w.p.ChangedSince(w.live); len(c.Occurrences)+len(c.Actions)+len(c.Effects) != 0 {
		t.Fatalf("nothing after the head: %+v", c)
	}
	a := w.action(current, "r4")
	if live, rank := a.Order.SortKey(); live || rank >= 0 || a.Start.Time != "t" {
		t.Fatalf("history action order %v %v time %q", live, rank, a.Start.Time)
	}
}
