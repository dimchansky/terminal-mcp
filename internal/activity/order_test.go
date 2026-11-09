package activity

import (
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
)

func ref(i int) OccurrenceKey { return OccurrenceKey{Ref: fmt.Sprintf("r%d", i)} }

// span is one page read: records [lo, hi) delivered newest first (history) or
// oldest first (live).
type span struct{ lo, hi int }

// historyReads models the backward chain over a journal of n records whose
// history snapshot saw the first s records: pages of size page from the
// newest record down to r0. If fresh >= s, the first page is retried with a
// fresh snapshot that saw the first fresh records, and the walk continues from
// that page. rereads lists (after, again) pairs: after read index `after`,
// read again the earlier read with index `again` (a retry or re-read).
func historyReads(s, n, page, fresh int, rereads [][2]int) []span {
	reads := []span{{max(0, s-page), s}}
	cursor := max(0, s-page)
	if fresh >= s {
		reads = append(reads, span{max(0, fresh-page), fresh})
		cursor = max(0, fresh-page)
	}
	for cursor > 0 {
		reads = append(reads, span{max(0, cursor-page), cursor})
		cursor = max(0, cursor-page)
	}
	var out []span
	for i, read := range reads {
		out = append(out, read)
		for _, rr := range rereads {
			switch {
			case rr[0] != i:
			case rr[1] == 0:
				// Re-reading the first page takes a fresh snapshot of
				// everything appended so far.
				out = append(out, span{max(0, n-page), n})
			case rr[1] < len(reads):
				out = append(out, reads[rr[1]])
			}
		}
	}
	return out
}

func deliveriesFor(history, live []span) (h, l []Delivery) {
	rank := int64(0)
	for _, read := range history {
		for i := read.hi - 1; i >= read.lo; i-- {
			rank--
			h = append(h, Delivery{Key: ref(i), Chain: ChainHistory, Rank: rank})
		}
	}
	rank = 0
	for _, read := range live {
		for i := read.lo; i < read.hi; i++ {
			rank++
			l = append(l, Delivery{Key: ref(i), Chain: ChainLive, Rank: rank})
		}
	}
	return h, l
}

// interleavings merges the two chains while keeping each chain's own order,
// which is all an adapter guarantees.
func interleavings(h, l []Delivery, salt uint64) [][]Delivery {
	alternate := make([]Delivery, 0, len(h)+len(l))
	for i := 0; i < max(len(h), len(l)); i++ {
		if i < len(l) {
			alternate = append(alternate, l[i])
		}
		if i < len(h) {
			alternate = append(alternate, h[i])
		}
	}
	out := [][]Delivery{slices.Concat(h, l), slices.Concat(l, h), alternate}
	for seed := uint64(1); seed <= 1; seed++ {
		r := rand.New(rand.NewPCG(seed, salt))
		merged := make([]Delivery, 0, len(h)+len(l))
		i, j := 0, 0
		for i < len(h) || j < len(l) {
			if j == len(l) || (i < len(h) && r.IntN(2) == 0) {
				merged = append(merged, h[i])
				i++
			} else {
				merged = append(merged, l[j])
				j++
			}
		}
		out = append(out, merged)
	}
	return out
}

func sortedByOrder(t *testing.T, deliveries []Delivery) []OccurrenceKey {
	t.Helper()
	occ := newOccurrences()
	for _, d := range deliveries {
		if _, err := occ.observe(d); err != nil {
			t.Fatalf("%+v: %v", d, err)
		}
	}
	keys := make([]OccurrenceKey, 0, len(occ.byKey))
	for key := range occ.byKey {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b OccurrenceKey) int {
		c := occ.byKey[a].order().Compare(occ.byKey[b].order())
		if c == 0 {
			t.Fatalf("distinct occurrences %v and %v share an order", a, b)
		}
		return c
	})
	return keys
}

// liveRuns enumerates forward chains starting after F (record f) that end at
// the journal end n, with up to two reconnects. A reconnect resumes from any
// boundary the chain had already reached, possibly before the end.
func liveRuns(f, n int, visit func([]span)) {
	seen := map[string]bool{}
	emit := func(runs []span) {
		var kept []span
		for i, run := range runs {
			if run.lo < run.hi || i == len(runs)-1 {
				kept = append(kept, run) // Drop empty intermediate runs.
			}
		}
		if id := fmt.Sprint(kept); !seen[id] {
			seen[id] = true
			visit(kept)
		}
	}
	emit([]span{{f, n}})
	for k1 := f; k1 <= n; k1++ {
		for j1 := f; j1 <= k1; j1++ {
			emit([]span{{f, k1}, {j1, n}})
			for k2 := j1; k2 <= n; k2++ {
				for j2 := f; j2 <= max(k1, k2); j2++ {
					emit([]span{{f, k1}, {j1, k2}, {j2, n}})
				}
			}
		}
	}
}

// TestOrderMatchesJournalAfterJoin enumerates small handoff shapes: overlap of
// history and live, history retries and re-reads of older pages, a first page
// retried with a fresh snapshot, and live reconnects before and at the end.
// After the live chain reached the end (the join point) the order must equal
// journal order for every interleaving of the two chains.
func TestOrderMatchesJournalAfterJoin(t *testing.T) {
	var rereadSets [][][2]int
	rereadSets = append(rereadSets, nil)
	for a := 0; a < 3; a++ {
		for b := 0; b <= a; b++ {
			rereadSets = append(rereadSets, [][2]int{{a, b}})
			for c := a; c < 3; c++ {
				for d := 0; d <= c; d++ {
					rereadSets = append(rereadSets, [][2]int{{a, b}, {c, d}})
				}
			}
		}
	}
	cases := 0
	for n := 0; n <= 4; n++ {
		want := make([]OccurrenceKey, n)
		for i := range want {
			want[i] = ref(i)
		}
		for f := 0; f <= n; f++ {
			for s := f; s <= n; s++ {
				for page := 1; page <= 2; page++ {
					for fresh := s - 1; fresh <= n; fresh++ {
						for _, rereads := range rereadSets {
							history := historyReads(s, n, page, fresh, rereads)
							liveRuns(f, n, func(live []span) {
								// One interleaving suffices here: the
								// interleaving test shows the result
								// does not depend on it.
								h, l := deliveriesFor(history, live)
								merged := interleavings(h, l, 0)[2]
								if got := sortedByOrder(t, merged); !slices.Equal(got, want) {
									t.Fatalf("n=%d f=%d s=%d page=%d fresh=%d rereads=%v live=%v:\n got %v\nwant %v",
										n, f, s, page, fresh, rereads, live, got, want)
								}
								cases++
							})
						}
					}
				}
			}
		}
	}
	if cases < 3000 {
		t.Fatalf("only %d shapes exercised", cases)
	}
}

// TestInterleavingDoesNotMatter applies the same per-chain deliveries in
// several interleavings of the two chains and expects identical registries:
// each chain only updates its own rank, and ranks are checked per chain.
func TestInterleavingDoesNotMatter(t *testing.T) {
	shapes := 0
	for f := 0; f <= 4; f++ {
		for s := f; s <= 4; s++ {
			for fresh := s - 1; fresh <= 4; fresh++ {
				history := historyReads(s, 4, 2, fresh, [][2]int{{1, 0}, {2, 1}})
				liveRuns(f, 4, func(live []span) {
					h, l := deliveriesFor(history, live)
					var reference map[OccurrenceKey]chainRanks
					for i, merged := range interleavings(h, l, uint64(shapes)) {
						occ := newOccurrences()
						for _, d := range merged {
							if _, err := occ.observe(d); err != nil {
								t.Fatal(err)
							}
						}
						got := map[OccurrenceKey]chainRanks{}
						for key, ranks := range occ.byKey {
							got[key] = *ranks
						}
						if i == 0 {
							reference = got
						} else if !maps.Equal(got, reference) {
							t.Fatalf("f=%d s=%d fresh=%d live=%v: interleaving %d differs", f, s, fresh, live, i)
						}
					}
					shapes++
				})
			}
		}
	}
	if shapes < 100 {
		t.Fatalf("only %d shapes", shapes)
	}
}

// TestOrderBeforeJoinIsNotTrusted documents why the adapter must not publish
// an ordering until the live chain has delivered every record after F that the
// history chain delivered.
func TestOrderBeforeJoinIsNotTrusted(t *testing.T) {
	compare := func(occ *occurrences, a, b int) int {
		return occ.byKey[ref(a)].order().Compare(occ.byKey[ref(b)].order())
	}
	apply := func(occ *occurrences, ds ...Delivery) {
		t.Helper()
		for _, d := range ds {
			if _, err := occ.observe(d); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Journal r0 r1 r2; F after r0; history snapshot saw r0..r2; live has
	// redelivered only r1 so far.
	occ := newOccurrences()
	apply(occ, Delivery{ref(2), ChainHistory, -1}, Delivery{ref(1), ChainHistory, -2},
		Delivery{ref(0), ChainHistory, -3}, Delivery{ref(1), ChainLive, 1})
	if compare(occ, 2, 1) >= 0 {
		t.Fatal("expected the pre-join order to be wrong for r2")
	}
	apply(occ, Delivery{ref(2), ChainLive, 2})
	if compare(occ, 2, 1) <= 0 {
		t.Fatal("after join r2 must follow r1")
	}

	// A first history page retried with a fresh snapshot sees a newer r3 and
	// ranks it below r2 until the live chain delivers it.
	occ = newOccurrences()
	apply(occ, Delivery{ref(2), ChainHistory, -1}, Delivery{ref(3), ChainHistory, -2})
	if compare(occ, 3, 2) >= 0 {
		t.Fatal("expected the fresh-snapshot record to be misordered before join")
	}
	apply(occ, Delivery{ref(3), ChainLive, 1})
	if compare(occ, 3, 2) <= 0 {
		t.Fatal("after join r3 must follow r2")
	}
}

func TestObserveReportsFirstDeliveryAndMoves(t *testing.T) {
	occ := newOccurrences()
	step := func(d Delivery, wantFirst, wantMoved bool) {
		t.Helper()
		got, err := occ.observe(d)
		if err != nil || got.first != wantFirst || got.moved != wantMoved {
			t.Fatalf("%+v: first=%v moved=%v err=%v", d, got.first, got.moved, err)
		}
	}
	step(Delivery{ref(1), ChainHistory, -1}, true, false)
	step(Delivery{ref(1), ChainHistory, -5}, false, false) // Retried page.
	step(Delivery{ref(1), ChainLive, 3}, false, true)      // Overlap row joins live.
	step(Delivery{ref(1), ChainLive, 9}, false, false)     // Reconnect replay.
	step(Delivery{ref(1), ChainHistory, -9}, false, false)
	if len(occ.byKey) != 1 {
		t.Fatalf("redeliveries created %d occurrences", len(occ.byKey))
	}
}

func TestObserveRejectsAdapterContractViolations(t *testing.T) {
	tests := []struct {
		name  string
		prior []Delivery
		bad   Delivery
	}{
		{"empty key", nil, Delivery{OccurrenceKey{}, ChainHistory, -1}},
		{"zero history rank", nil, Delivery{ref(1), ChainHistory, 0}},
		{"positive history rank", nil, Delivery{ref(1), ChainHistory, 1}},
		{"zero live rank", nil, Delivery{ref(1), ChainLive, 0}},
		{"negative live rank", nil, Delivery{ref(1), ChainLive, -1}},
		{"unknown chain", nil, Delivery{ref(1), Chain(0), -1}},
		{"other chain", nil, Delivery{ref(1), Chain(9), 1}},
		{"history rank reused", []Delivery{{ref(1), ChainHistory, -1}}, Delivery{ref(2), ChainHistory, -1}},
		{"history rank goes back", []Delivery{{ref(1), ChainHistory, -2}}, Delivery{ref(2), ChainHistory, -1}},
		{"live rank reused", []Delivery{{ref(1), ChainLive, 4}}, Delivery{ref(2), ChainLive, 4}},
		{"live rank goes back", []Delivery{{ref(1), ChainLive, 4}}, Delivery{ref(1), ChainLive, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			occ := newOccurrences()
			for _, d := range tt.prior {
				if _, err := occ.observe(d); err != nil {
					t.Fatal(err)
				}
			}
			before := len(occ.byKey)
			_, err := occ.observe(tt.bad)
			if !errors.Is(err, ErrInvalidOccurrence) && !errors.Is(err, ErrInvalidRank) {
				t.Fatalf("accepted: %v", err)
			}
			if len(occ.byKey) != before {
				t.Fatal("a rejected delivery must not create an occurrence")
			}
		})
	}
}

func TestDistinctRefsAreDistinctOccurrences(t *testing.T) {
	// The #7 adapter test (design A2) proves that byte-identical copies in
	// different archives yield different RecordRef strings; here two such
	// strings must stay two occurrences.
	occ := newOccurrences()
	for i, key := range []OccurrenceKey{{Ref: "pos-X@1"}, {Ref: "pos-X@2"}} {
		got, err := occ.observe(Delivery{key, ChainHistory, int64(-1 - i)})
		if err != nil || !got.first {
			t.Fatalf("%v: first=%v err=%v", key, got.first, err)
		}
	}
	if len(occ.byKey) != 2 {
		t.Fatalf("got %d occurrences, want 2", len(occ.byKey))
	}
}
