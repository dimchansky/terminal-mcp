package activity

import (
	"cmp"
	"errors"
)

// Chain identifies how a record reached the projection. A projection window
// has one history chain, walked backward from the snapshot (and continued for
// earlier pages, source discovery and origin backfill), and at most one live
// chain, walked forward from the frontier F captured before that snapshot.
type Chain uint8

const (
	// ChainHistory is the backward walk. Its ranks strictly decrease in
	// delivery order, for example -1, -2, ...; gaps are allowed.
	ChainHistory Chain = iota + 1
	// ChainLive is the forward walk. Its ranks strictly increase in
	// delivery order, for example 1, 2, ...; gaps are allowed.
	ChainLive
)

var (
	// ErrInvalidOccurrence means a delivery has no usable occurrence key.
	ErrInvalidOccurrence = errors.New("activity: invalid occurrence key")
	// ErrInvalidRank means the adapter broke the rank contract: wrong sign
	// for the chain, or not strictly beyond the previous rank of that chain.
	// It is an adapter assertion failure; the window must be rebuilt.
	ErrInvalidRank = errors.New("activity: invalid chain rank")
)

// OccurrenceKey identifies one physical journal occurrence within one writer
// instance. Ref is the reader's opaque RecordRef string: the byte-derived
// record position plus the archive suffix hint. Equal positions alone are not
// unique (byte-identical copies in different archives share them); the hint
// keeps such copies distinct, while one row keeps its key when the active
// file is rotated into its archive. Keys must not be compared across server
// instances: after a restart an archive suffix can be reused.
//
// Ref is currently required; rows without a reader reference get a separate
// key form in a later change.
type OccurrenceKey struct {
	Ref string
}

// Delivery is one arrival of an occurrence on a chain. Ranks are assigned
// when the delivery is applied, by one counter per chain per window: strictly
// decreasing negative ranks on the history chain and strictly increasing
// positive ranks on the live chain. Pages of one chain are therefore never
// applied concurrently or out of order. A retried page or a reconnect delivers
// the same occurrence again with a new rank. The two chains may interleave in
// any way; the result does not depend on it.
type Delivery struct {
	Key   OccurrenceKey
	Chain Chain
	Rank  int64
}

// Order is the journal-order key of an occurrence. Records seen on the live
// chain follow every record seen only on the history chain.
type Order struct {
	live bool
	rank int64
}

// Compare returns -1, 0 or +1 when o is older than, equal to or newer than p.
func (o Order) Compare(p Order) int {
	if o.live != p.live {
		if o.live {
			return 1
		}
		return -1
	}
	return cmp.Compare(o.rank, p.rank)
}

// chainRanks keeps the first rank an occurrence received on each chain. As
// ranks move away from zero over time, the first history rank is the largest
// and the first live rank is the smallest.
//
// Why first ranks yield journal order (design section 4): every history page
// covers a fixed, append-only range of the journal, so records at or before F
// are first delivered on the history chain in reverse journal order, however
// pages are retried or re-read later. The live chain starts after F, never
// delivers a record at or before F, and first delivers records in journal
// order, however often it reconnects. Hence the order is journal order once
// every record after F that the history chain delivered (including records
// first seen by a history page retried with a fresh snapshot) has a live rank.
// That is the join point; callers must not publish an ordering before it.
type chainRanks struct {
	history int64 // 0 when never seen on the history chain.
	live    int64 // 0 when never seen on the live chain.
}

func (r chainRanks) order() Order {
	if r.live != 0 {
		return Order{live: true, rank: r.live}
	}
	return Order{rank: r.history}
}

// observation is the effect of one delivery on the occurrence registry.
type observation struct {
	ranks *chainRanks
	first bool // First delivery of this occurrence.
	moved bool // A redelivery changed the occurrence's order.
}

// occurrences deduplicates deliveries: every OccurrenceKey has exactly one
// chainRanks, however often and on whichever chain it arrives.
type occurrences struct {
	byKey       map[OccurrenceKey]*chainRanks
	lastHistory int64
	lastLive    int64
}

func newOccurrences() *occurrences {
	return &occurrences{byKey: make(map[OccurrenceKey]*chainRanks)}
}

// observe records d after checking the adapter's rank contract.
func (o *occurrences) observe(d Delivery) (observation, error) {
	if d.Key.Ref == "" {
		return observation{}, ErrInvalidOccurrence
	}
	switch {
	case d.Chain == ChainHistory && d.Rank < 0 && (o.lastHistory == 0 || d.Rank < o.lastHistory):
		o.lastHistory = d.Rank
	case d.Chain == ChainLive && d.Rank > 0 && d.Rank > o.lastLive:
		o.lastLive = d.Rank
	default:
		return observation{}, ErrInvalidRank
	}
	ranks, seen := o.byKey[d.Key]
	if !seen {
		ranks = &chainRanks{}
		o.byKey[d.Key] = ranks
	}
	before := ranks.order()
	switch d.Chain {
	case ChainHistory:
		if ranks.history == 0 {
			ranks.history = d.Rank
		}
	case ChainLive:
		if ranks.live == 0 {
			ranks.live = d.Rank
		}
	}
	return observation{ranks: ranks, first: !seen, moved: seen && ranks.order() != before}, nil
}
