package activity

import (
	"errors"
	"testing"
)

func applyAll(t *testing.T, p *Projection, recs ...Record) {
	t.Helper()
	for _, rec := range recs {
		if _, err := p.Apply(rec); err != nil {
			t.Fatal(err)
		}
	}
}

func row(ref string, chain Chain, rank int64) Record {
	return Record{Delivery: Delivery{OccurrenceKey{Ref: ref}, chain, rank}, Raw: []byte(reqStart(current, "req-"+ref, "terminal_list", "", ""))}
}

// h1 reads the newest history page with the given rows (newest first).
func h1(t *testing.T, p *Projection, next string, recs ...Record) {
	t.Helper()
	if err := p.BeginHistoryPage(""); err != nil {
		t.Fatal(err)
	}
	applyAll(t, p, recs...)
	if err := p.EndHistoryPage(next); err != nil {
		t.Fatal(err)
	}
}

func TestJoinOnQuietJournal(t *testing.T) {
	p := NewProjection(current)
	h1(t, p, "n1", row("B", ChainHistory, -1))
	if p.Joined() {
		t.Fatal("joined before StartJoin")
	}
	if err := p.StartJoin(); err != nil {
		t.Fatal(err)
	}
	if p.Joined() {
		t.Fatal("joined before the live chain caught up")
	}
	// H1's newest row is at or before F: live never delivers it; the empty
	// forward read joins.
	if err := p.LiveCaughtUp(); err != nil || !p.Joined() {
		t.Fatalf("quiet journal must join without a new append: %v", err)
	}
	if err := p.LiveCaughtUp(); err != nil {
		t.Fatalf("repeated catch-up: %v", err)
	}
}

func TestEarlyJoinUsesTheLatestH1Read(t *testing.T) {
	p := NewProjection(current)
	h1(t, p, "n1", row("B", ChainHistory, -1))
	// H1 retried with a fresh snapshot: C was appended meanwhile.
	h1(t, p, "n2", row("C", ChainHistory, -2), row("B", ChainHistory, -3))
	if err := p.StartJoin(); err != nil {
		t.Fatal(err)
	}
	applyAll(t, p, row("B", ChainLive, 1))
	if p.Joined() {
		t.Fatal("joined on the first H1 read's newest row")
	}
	applyAll(t, p, row("C", ChainLive, 2))
	if !p.Joined() {
		t.Fatal("live delivery of the latest H1's newest row must join")
	}
	applyAll(t, p, row("C", ChainLive, 3)) // Reconnect redelivery keeps it joined.
	if !p.Joined() {
		t.Fatal("redelivery unjoined the window")
	}
}

func TestEarlyJoinRefusesWeakNewestRow(t *testing.T) {
	p := NewProjection(current)
	newest := weak("B", 0, 1, 2)
	if err := p.BeginHistoryPage(""); err != nil {
		t.Fatal(err)
	}
	applyAll(t, p, Record{Delivery: Delivery{newest, ChainHistory, -1}, Problem: "too large"})
	if err := p.EndHistoryPage("n1"); err != nil {
		t.Fatal(err)
	}
	_ = p.StartJoin()
	applyAll(t, p, Record{Delivery: Delivery{newest, ChainLive, 1}, Problem: "too large"})
	if p.Joined() {
		t.Fatal("a weak key must not prove the join")
	}
}

func TestJoinProtocolViolations(t *testing.T) {
	p := NewProjection(current)
	if _, err := p.Apply(row("A", ChainLive, 1)); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("live before the snapshot: %v", err)
	}
	if _, err := p.Apply(row("A", ChainHistory, -1)); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("history row outside a page: %v", err)
	}
	if err := p.StartJoin(); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("join without H1: %v", err)
	}
	if err := p.LiveCaughtUp(); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("catch-up before the snapshot: %v", err)
	}
	if err := p.BeginHistoryPage("somewhere"); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("live window must start at the newest page: %v", err)
	}
	h1(t, p, "n1", row("B", ChainHistory, -1))
	if err := p.StartJoin(); err != nil {
		t.Fatal(err)
	}
	if err := p.StartJoin(); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("second StartJoin: %v", err)
	}
	// A fresh newest-page read after StartJoin could deliver rows newer than
	// the live chain: refused.
	if err := p.BeginHistoryPage(""); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("newest page after StartJoin: %v", err)
	}
	if err := p.BeginHistoryPage("n1"); err != nil {
		t.Fatalf("continuation after StartJoin: %v", err)
	}
	if err := p.BeginHistoryPage("n1"); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("page already open: %v", err)
	}
	if err := p.HistoryOnly(); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("history-only after join started: %v", err)
	}

	archive := NewProjection(current)
	if err := archive.HistoryOnly(); err != nil || !archive.Joined() {
		t.Fatalf("archive window: %v", err)
	}
	if err := archive.BeginHistoryPage("floor-cursor"); err != nil {
		t.Fatalf("archive starts at its floor: %v", err)
	}
	if _, err := archive.Apply(row("A", ChainLive, 1)); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("live delivery in an archive window: %v", err)
	}
	applyAll(t, archive, row("A", ChainHistory, -1))
	if err := archive.EndHistoryPage("older"); err != nil {
		t.Fatal(err)
	}
	if err := archive.BeginHistoryPage("elsewhere"); !errors.Is(err, ErrJoinProtocol) {
		t.Fatalf("archive must continue from its own cursors: %v", err)
	}
}

func TestLiveSeed(t *testing.T) {
	t.Run("quiet, newest has ref", func(t *testing.T) {
		p := NewProjection(current)
		h1(t, p, "n1", row("C", ChainHistory, -1), row("B", ChainHistory, -2))
		if got := p.LiveSeed(true); got != "C" {
			t.Fatalf("seed %q", got)
		}
		if got := p.LiveSeed(false); got != "" {
			t.Fatalf("seed after appends %q", got)
		}
	})
	t.Run("latest H1 read counts", func(t *testing.T) {
		p := NewProjection(current)
		h1(t, p, "n1", row("B", ChainHistory, -1))
		h1(t, p, "n2", row("C", ChainHistory, -2))
		if got := p.LiveSeed(true); got != "C" {
			t.Fatalf("seed %q", got)
		}
	})
	t.Run("quiet, newest ref-less", func(t *testing.T) {
		p := NewProjection(current)
		_ = p.BeginHistoryPage("")
		applyAll(t, p, Record{Delivery: Delivery{OccurrenceKey{Weak: WeakKey{Start: 1, End: 2, Provisional: "h1"}}, ChainHistory, -1}, Problem: "x"})
		_ = p.EndHistoryPage("n1")
		if got := p.LiveSeed(true); got != "" {
			t.Fatalf("seed %q", got)
		}
	})
	t.Run("empty complete history", func(t *testing.T) {
		p := NewProjection(current)
		_ = p.BeginHistoryPage("")
		_ = p.EndHistory(CoverageReachedStart)
		if got := p.LiveSeed(true); got != StartOfHistory {
			t.Fatalf("seed %q", got)
		}
	})
	t.Run("empty page at a gap", func(t *testing.T) {
		p := NewProjection(current)
		_ = p.BeginHistoryPage("")
		_ = p.EndHistory(CoverageGap)
		if got := p.LiveSeed(true); got != "" {
			t.Fatalf("seed %q", got)
		}
	})
	t.Run("no H1 yet", func(t *testing.T) {
		if got := NewProjection(current).LiveSeed(true); got != "" {
			t.Fatalf("seed %q", got)
		}
	})
}
