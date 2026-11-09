package activity

import (
	"errors"
	"reflect"
	"testing"
)

// journalRow is a test journal entry: a Ref, or a ref-less damaged row with a
// byte range.
type journalRow struct {
	ref        string
	start, end int64
}

func (r journalRow) row() Row { return Row{Ref: r.ref, Start: r.start, End: r.end} }

// testJournal: A, x, y, B, z, C (oldest first); x, y, z have no Ref. The
// spans of x and z are equal on purpose: they are in different places.
var testJournal = []journalRow{
	{ref: "A"}, {start: 10, end: 20}, {start: 20, end: 30}, {ref: "B"}, {start: 10, end: 20}, {ref: "C"},
}

// feed applies rows on one chain through the carry helpers.
type feeder struct {
	t    *testing.T
	p    *Projection
	h    HistoryCarry
	l    LiveCarry
	hist int64
	live int64
}

// history reads rows as one backward page from cursor "" (the newest record)
// that either reaches the start or ends at cursor "next".
func (f *feeder) history(rows []journalRow, reachedStart bool) {
	f.t.Helper()
	f.page("", rows, reachedStart, "next")
}

func (f *feeder) page(from string, rows []journalRow, reachedStart bool, next string) {
	f.t.Helper()
	f.h.BeginPage(from)
	for i := len(rows) - 1; i >= 0; i-- {
		key, resolutions := f.h.Key(rows[i].row())
		f.hist--
		f.apply(Record{Delivery: Delivery{key, ChainHistory, f.hist}, Raw: rawFor(rows[i]), Problem: problemFor(rows[i])})
		f.resolve(resolutions)
	}
	if reachedStart {
		f.resolve(f.h.ReachedStart())
	} else {
		f.h.EndPage(next)
	}
}

func (f *feeder) liveRows(rows []journalRow) {
	f.t.Helper()
	for _, r := range rows {
		key := f.l.Key(r.row())
		f.live++
		f.apply(Record{Delivery: Delivery{key, ChainLive, f.live}, Raw: rawFor(r), Problem: problemFor(r)})
	}
}

func (f *feeder) apply(rec Record) {
	f.t.Helper()
	if _, err := f.p.Apply(rec); err != nil {
		f.t.Fatal(err)
	}
}

func (f *feeder) resolve(rs []Resolution) {
	f.t.Helper()
	for _, r := range rs {
		if _, err := f.p.Resolve(r); err != nil {
			f.t.Fatal(err)
		}
	}
}

func rawFor(r journalRow) []byte {
	if r.ref == "" {
		return nil
	}
	return []byte(reqStart(current, "req-"+r.ref, "terminal_list", "", ""))
}

func problemFor(r journalRow) string {
	if r.ref != "" {
		return ""
	}
	return "record too large"
}

func weak(older string, between int, start, end int64) OccurrenceKey {
	return OccurrenceKey{Weak: WeakKey{OlderRef: older, Between: between, Start: start, End: end}}
}

func diagnosticKeys(p *Projection) []OccurrenceKey {
	var out []OccurrenceKey
	for _, d := range p.Diagnostics() {
		out = append(out, d.Key)
	}
	return out
}

func TestRefLessRowsGetTheSameKeyOnBothChains(t *testing.T) {
	want := []OccurrenceKey{weak("A", 0, 10, 20), weak("A", 1, 20, 30), weak("B", 0, 10, 20)}
	// History snapshot saw everything; the live chain starts at F after A
	// and overlaps with history.
	f := &feeder{t: t, p: NewProjection(current), l: NewLiveCarry("A")}
	f.history(testJournal, true)
	f.history(testJournal[3:], false) // Retried newest page.
	f.liveRows(testJournal[1:])
	if got := diagnosticKeys(f.p); !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostics %+v, want %+v", got, want)
	}
	for _, d := range f.p.Diagnostics() {
		if d.PossibleRepeat {
			t.Fatalf("resolved row marked as possible repeat: %+v", d)
		}
	}
	// Journal order across the merge.
	if got := f.p.Diagnostics(); got[0].Order.Compare(got[1].Order) >= 0 || got[1].Order.Compare(got[2].Order) >= 0 {
		t.Fatalf("order %+v", got)
	}
}

func TestHistoryRefLessRowsWaitForTheirOlderNeighbour(t *testing.T) {
	var c HistoryCarry
	c.BeginPage("")
	keyC, rs := c.Key(Row{Ref: "C"})
	if keyC.Ref != "C" || len(rs) != 0 {
		t.Fatalf("%+v %+v", keyC, rs)
	}
	z, _ := c.Key(Row{Start: 10, End: 20})
	if !z.provisional() {
		t.Fatalf("z must be provisional until B arrives: %+v", z)
	}
	_, rs = c.Key(Row{Ref: "B"})
	if len(rs) != 1 || rs[0].Provisional != z || rs[0].Final != weak("B", 0, 10, 20) {
		t.Fatalf("resolutions %+v", rs)
	}
	y, _ := c.Key(Row{Start: 20, End: 30})
	x, _ := c.Key(Row{Start: 10, End: 20})
	if got := c.ReachedStart(); !reflect.DeepEqual(got, []Resolution{
		{y, weak(StartOfHistory, 1, 20, 30)}, {x, weak(StartOfHistory, 0, 10, 20)},
	}) {
		t.Fatalf("reached start: %+v", got)
	}
}

func TestUnresolvedRowsArePossibleRepeatsNotHidden(t *testing.T) {
	// The history chain stops at a gap before x and y find their neighbour,
	// and the live seed is unknown.
	f := &feeder{t: t, p: NewProjection(current), l: NewLiveCarry("")}
	f.history(testJournal[1:], false)
	f.liveRows(testJournal[1:])
	diags := f.p.Diagnostics()
	repeats := 0
	for _, d := range diags {
		if d.PossibleRepeat {
			repeats++
		}
	}
	// x and y appear once from history and once from live: four
	// provisional rows; z is resolved on both chains and merged.
	if len(diags) != 5 || repeats != 4 {
		t.Fatalf("diagnostics %+v", diags)
	}
}

func TestLiveCarryResumesFromAStoredCopy(t *testing.T) {
	c := NewLiveCarry("A")
	c.Key(Row{Start: 10, End: 20})
	saved := c // Stored with the cursor issued after x.
	y1 := c.Key(Row{Start: 20, End: 30})
	resumed := saved
	if y2 := resumed.Key(Row{Start: 20, End: 30}); y1 != y2 || y1 != weak("A", 1, 20, 30) {
		t.Fatalf("resume produced %+v, want %+v", y2, y1)
	}
}

func TestEqualSpansAfterDifferentNeighboursStayDistinct(t *testing.T) {
	f := &feeder{t: t, p: NewProjection(current), l: NewLiveCarry("A")}
	f.liveRows(testJournal[1:])
	if got := diagnosticKeys(f.p); len(got) != 3 || got[0] == got[2] {
		t.Fatalf("x and z merged: %+v", got)
	}
}

func TestResolveRejectsInvalidResolutions(t *testing.T) {
	p := NewProjection(current)
	prov := OccurrenceKey{Weak: WeakKey{Start: 1, End: 2, Provisional: "h1"}}
	if _, err := p.Apply(Record{Delivery: Delivery{prov, ChainHistory, -1}, Problem: "too large"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Resolution{
		{Provisional: OccurrenceKey{Weak: WeakKey{Start: 1, End: 2, Provisional: "h9"}}, Final: weak("A", 0, 1, 2)},
		{Provisional: prov, Final: OccurrenceKey{Ref: "A"}},
		{Provisional: prov, Final: weak("A", 0, 1, 3)},
		{Provisional: prov, Final: weak("", 0, 1, 2)},
		{Provisional: prov, Final: OccurrenceKey{Weak: WeakKey{Start: 1, End: 2, Provisional: "h2"}}},
	} {
		if _, err := p.Resolve(r); !errors.Is(err, ErrResolve) {
			t.Fatalf("%+v accepted: %v", r, err)
		}
	}
	if len(p.Diagnostics()) != 1 {
		t.Fatal("a rejected resolution must not change the window")
	}
	if _, err := p.Apply(Record{Delivery: Delivery{weak("A", 0, 1, 2), ChainHistory, -2}, Raw: []byte(`{}`)}); !errors.Is(err, ErrRecordContract) {
		t.Fatalf("weak key with record bytes accepted: %v", err)
	}
	for _, key := range []OccurrenceKey{
		{Ref: "A", Weak: WeakKey{Start: 1, End: 2}},
		{Weak: WeakKey{Start: 2, End: 2, OlderRef: "A"}},
		{Weak: WeakKey{Start: 1, End: 2, OlderRef: "A", Provisional: "x"}},
		{Weak: WeakKey{Start: 1, End: 2, Between: -1, OlderRef: "A"}},
	} {
		if _, err := p.Apply(Record{Delivery: Delivery{key, ChainHistory, -3}, Problem: "x"}); !errors.Is(err, ErrInvalidOccurrence) {
			t.Fatalf("%+v accepted: %v", key, err)
		}
	}
}

// TestRetriedPagesDoNotInventNeighbours: a page ends with x and y pending;
// a retried newer page must not resolve them, the continuation must.
func TestRetriedPagesDoNotInventNeighbours(t *testing.T) {
	f := &feeder{t: t, p: NewProjection(current), l: NewLiveCarry("")}
	f.page("", testJournal[1:], false, "before-x") // y.. C: x, y wait at "before-x"
	f.page("", testJournal[3:], false, "before-B") // Retry of the newest page.
	f.page("before-x", testJournal[:1], true, "")  // Continuation: A, start.
	want := []OccurrenceKey{weak("A", 0, 10, 20), weak("A", 1, 20, 30), weak("B", 0, 10, 20)}
	if got := diagnosticKeys(f.p); !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostics %+v, want %+v", got, want)
	}
}

// TestRepeatedPageEndKeepsRowsVisible: the same page is read twice and ends
// at the same cursor before its continuation; the first read's provisional
// rows cannot be matched and stay visible as possible repeats.
func TestRepeatedPageEndKeepsRowsVisible(t *testing.T) {
	f := &feeder{t: t, p: NewProjection(current), l: NewLiveCarry("")}
	f.page("", testJournal[1:], false, "before-x")
	f.page("", testJournal[1:], false, "before-x")
	f.page("before-x", testJournal[:1], true, "")
	repeats, resolved := 0, 0
	for _, d := range f.p.Diagnostics() {
		if d.PossibleRepeat {
			repeats++
		} else {
			resolved++
		}
	}
	if repeats != 2 || resolved != 3 {
		t.Fatalf("diagnostics %+v", f.p.Diagnostics())
	}
}

func TestRefLessPagesCarryInheritedRows(t *testing.T) {
	// Pages of one row each: y waits, x continues it, then A resolves both.
	f := &feeder{t: t, p: NewProjection(current), l: NewLiveCarry("")}
	f.page("", testJournal[2:3], false, "c1")
	f.page("c1", testJournal[1:2], false, "c2")
	f.page("unknown", testJournal[3:4], false, "c9") // Unrelated cursor: no inherited rows.
	f.page("c2", testJournal[:1], true, "")
	want := []OccurrenceKey{weak("A", 0, 10, 20), weak("A", 1, 20, 30)}
	if got := diagnosticKeys(f.p); !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostics %+v, want %+v", got, want)
	}
}

func TestResolveMergeKeepsFirstRanks(t *testing.T) {
	p := NewProjection(current)
	final := weak("A", 0, 10, 20)
	if _, err := p.Apply(Record{Delivery: Delivery{final, ChainLive, 1}, Problem: "too large"}); err != nil {
		t.Fatal(err)
	}
	prov := OccurrenceKey{Weak: WeakKey{Start: 10, End: 20, Provisional: "h1"}}
	if _, err := p.Apply(Record{Delivery: Delivery{prov, ChainHistory, -1}, Problem: "too large"}); err != nil {
		t.Fatal(err)
	}
	c, err := p.Resolve(Resolution{Provisional: prov, Final: final})
	if err != nil || c.Kind != ChangeResolved || c.Moved {
		t.Fatalf("%+v %v", c, err)
	}
	if got := *p.occ.byKey[final]; got != (chainRanks{history: -1, live: 1}) {
		t.Fatalf("ranks %+v", got)
	}
	if len(p.Diagnostics()) != 1 {
		t.Fatalf("diagnostics %+v", p.Diagnostics())
	}
}
