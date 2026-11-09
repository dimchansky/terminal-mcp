package activity

import (
	"errors"
	"testing"
)

func indexRows(items ...[2]string) []IndexRow {
	var rows []IndexRow
	for _, it := range items {
		if it[1] == "" {
			rows = append(rows, IndexRow{Key: OccurrenceKey{Weak: WeakKey{Start: 1, End: 2, Provisional: it[0]}}, Problem: "too large"})
			continue
		}
		rows = append(rows, IndexRow{Key: OccurrenceKey{Ref: it[0]}, Raw: []byte(it[1])})
	}
	return rows
}

func TestInFlightIndexTracksLongRunningEffects(t *testing.T) {
	x := NewInFlightIndex(current, "f0", true, 10)
	if x.Status() != IndexCatchingUp {
		t.Fatalf("status %v", x.Status())
	}
	err := x.ApplyPage("f0", indexRows(
		[2]string{"1", reqStart(current, "dl", "file_receive", "op-dl", meta("chat-a"))},
		[2]string{"2", opStart(current, "dl", "file_receive", "op-dl")},
		[2]string{"3", opStart(current, "w", "file_write", "op-w")},
		[2]string{"4", opFinish(current, "w", "file_write", "op-w", "")},
		[2]string{"5", opStart("srv-old", "o", "file_write", "op-o")}, // Not this instance.
		[2]string{"6", `{"phase":`},
		[2]string{"7", ""},                                               // Damaged row.
		[2]string{"8", opStart(current, "dl", "file_receive", "op-dl")},  // Copy.
		[2]string{"9", opFinish(current, "x", "file_write", "op-x", "")}, // Finish without start.
	), "c1", true)
	if err != nil {
		t.Fatal(err)
	}
	if x.Skipped() != 2 {
		t.Fatalf("skipped %d, want the malformed and the damaged row", x.Skipped())
	}
	got := x.InFlight()
	if len(got) != 1 || got[0].Key != (EffectKey{current, "op-dl"}) || *got[0].Origin != (RequestKey{current, "dl"}) ||
		got[0].Start.Ref != "2" || x.Status() != IndexCatchingUp {
		t.Fatalf("in flight %+v status %v", got, x.Status())
	}
	// Hours later.
	if err := x.ApplyPage("c1", indexRows([2]string{"7", opFinish(current, "dl", "file_receive", "op-dl", "")}), "c2", false); err != nil {
		t.Fatal(err)
	}
	if len(x.InFlight()) != 0 || x.Status() != IndexReady || x.Cursor() != "c2" {
		t.Fatalf("after finish %+v %v", x.InFlight(), x.Status())
	}
}

func TestInFlightIndexRefusesRepeatedOrSkippedPages(t *testing.T) {
	x := NewInFlightIndex(current, "f0", true, 10)
	page := indexRows([2]string{"1", opStart(current, "a", "file_write", "op")})
	if err := x.ApplyPage("f0", page, "c1", false); err != nil {
		t.Fatal(err)
	}
	if err := x.ApplyPage("c1", indexRows([2]string{"2", opFinish(current, "a", "file_write", "op", "")}), "c2", false); err != nil {
		t.Fatal(err)
	}
	// A retry of the first page must not resurrect the finished effect.
	if err := x.ApplyPage("f0", page, "c1", false); !errors.Is(err, ErrIndexCursor) {
		t.Fatalf("repeated page: %v", err)
	}
	if err := x.ApplyPage("elsewhere", nil, "c9", false); !errors.Is(err, ErrIndexCursor) {
		t.Fatalf("skipped page: %v", err)
	}
	// A deferred row that was not read in full is refused with its page.
	deferred := []IndexRow{{Key: OccurrenceKey{Ref: "3"}, Raw: []byte(opStart(current, "b", "file_write", "op-b"))}, {Key: OccurrenceKey{Ref: "4"}}}
	if err := x.ApplyPage("c2", deferred, "c3", false); !errors.Is(err, ErrRecordContract) {
		t.Fatalf("deferred row: %v", err)
	}
	if len(x.InFlight()) != 0 || x.Cursor() != "c2" {
		t.Fatal("a refused page must not change the index")
	}
	if len(x.InFlight()) != 0 {
		t.Fatalf("in flight %+v", x.InFlight())
	}
}

func TestInFlightIndexLimitAndDegradedEntries(t *testing.T) {
	x := NewInFlightIndex(current, "f0", true, 2)
	start := func(from, op, next string) {
		t.Helper()
		if err := x.ApplyPage(from, indexRows([2]string{op, opStart(current, "r-"+op, "file_receive", op)}), next, false); err != nil {
			t.Fatal(err)
		}
	}
	start("f0", "a", "c1")
	start("c1", "b", "c2")
	x.ObserveAuditDegraded() // a and b may never record their finish.
	start("c2", "c", "c3")   // Full: the oldest degraded entry (a) is dropped.
	start("c3", "d", "c4")   // Then b.
	if x.Status() != IndexReady {
		t.Fatalf("degraded entries must make room: %v", x.Status())
	}
	start("c4", "e", "c5") // No degraded entry left: the index is incomplete.
	if x.Status() != IndexIncomplete {
		t.Fatalf("status %v", x.Status())
	}
	got := x.InFlight()
	if len(got) != 2 || got[0].Key.OperationID != "c" || got[1].Key.OperationID != "d" {
		t.Fatalf("in flight %+v", got)
	}
	// Incomplete is sticky: later pages cannot prove the list is whole.
	if err := x.ApplyPage("c5", nil, "c6", false); err != nil || x.Status() != IndexIncomplete {
		t.Fatalf("after more pages: %v %v", x.Status(), err)
	}
}

func TestInFlightIndexUnknownStates(t *testing.T) {
	x := NewInFlightIndex(current, "", false, 10)
	if err := x.ApplyPage("", indexRows([2]string{"1", opStart(current, "a", "file_write", "op")}), "c1", false); err != nil {
		t.Fatal(err)
	}
	if x.Status() != IndexUnknown || len(x.InFlight()) != 0 {
		t.Fatalf("no F0: %v %+v", x.Status(), x.InFlight())
	}
	y := NewInFlightIndex(current, "f0", true, 10)
	_ = y.ApplyPage("f0", indexRows([2]string{"1", opStart(current, "a", "file_write", "op")}), "c1", true)
	y.Fail()
	if y.Status() != IndexUnknown || len(y.InFlight()) != 0 {
		t.Fatalf("after failure: %v %+v", y.Status(), y.InFlight())
	}
	if err := y.ApplyPage("c1", nil, "c2", false); err != nil || y.Status() != IndexUnknown {
		t.Fatalf("page after failure: %v %v", err, y.Status())
	}
}

func TestSyncIndexMarksAndUnpinsPossiblyUnrecordedWork(t *testing.T) {
	x := NewInFlightIndex(current, "f0", true, 2)
	err := x.ApplyPage("f0", indexRows(
		[2]string{"1", reqStart(current, "a", "file_receive", "op-a", "")},
		[2]string{"2", opStart(current, "a", "file_receive", "op-a")},
		[2]string{"3", reqStart(current, "b", "file_write", "op-b", "")},
		[2]string{"4", opStart(current, "b", "file_write", "op-b")},
		[2]string{"5", opFinish(current, "b", "file_write", "op-b", "")},
	), "c1", false)
	if err != nil {
		t.Fatal(err)
	}
	x.ObserveAuditDegraded()
	// Limit 2: the second new start drops the degraded op-a entry; its flag
	// survives.
	_ = x.ApplyPage("c1", indexRows(
		[2]string{"6", opStart(current, "c", "file_write", "op-c")},
		[2]string{"7", opStart(current, "d", "file_write", "op-d")},
	), "c2", false)
	if x.Status() != IndexReady {
		t.Fatalf("status %v", x.Status())
	}
	if _, listed := x.Lookup(EffectKey{current, "op-a"}); listed || !x.effectUnrecorded(EffectKey{current, "op-a"}) || x.Dropped() != 1 {
		t.Fatal("dropped degraded entry must keep its flag and be counted")
	}

	w := newWindow(t)
	w.onLive("1", reqStart(current, "a", "file_receive", "op-a", ""))
	w.onLive("2", opStart(current, "a", "file_receive", "op-a"))
	w.onLive("3", reqStart(current, "b", "file_write", "op-b", ""))
	w.onLive("4", opStart(current, "b", "file_write", "op-b"))
	w.onLive("5", opFinish(current, "b", "file_write", "op-b", "")) // b's request_finish failed to append.
	w.p.SyncIndex(x)
	if a := w.action(current, "a"); a.State != RequestOpen || !a.PossiblyUnrecorded {
		t.Fatalf("request a %+v", a)
	}
	if e := w.effect(current, "op-a"); e.State != EffectInFlight || !e.PossiblyUnrecorded {
		t.Fatalf("effect a %+v", e)
	}
	if e := w.effect(current, "op-b"); e.State != EffectFinished || e.PossiblyUnrecorded {
		t.Fatalf("window finish must win: %+v", e)
	}
	if b := w.action(current, "b"); b.State != RequestOpen || !b.PossiblyUnrecorded {
		t.Fatalf("request b %+v", b)
	}
	if out := w.p.InFlightOutside(x); len(out) != 2 || out[0].Key.OperationID != "op-c" {
		t.Fatalf("outside %+v", out)
	}
	// Possibly unrecorded work is no longer pinned against eviction.
	if err := w.p.LiveCaughtUp(); err != nil {
		t.Fatal(err)
	}
	w.p.IssueLiveCursor("cur")
	if _, err := w.p.RaiseFloor(""); err != nil {
		t.Fatal(err)
	}
	if len(w.p.Actions()) != 0 || len(w.p.Effects()) != 0 {
		t.Fatalf("unpinned work kept: %+v %+v", w.p.Actions(), w.p.Effects())
	}
}
