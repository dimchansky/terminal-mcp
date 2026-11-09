package activity

import (
	"cmp"
	"crypto/sha256"
	"errors"
	"slices"
)

// maxPhaseOccurrences bounds how many journal copies of one lifecycle record
// are tracked. Further copies are counted but not listed.
const maxPhaseOccurrences = 16

var (
	// ErrMissingKey means a request or operation record lacks the identifier
	// needed to correlate it; the record becomes a diagnostic instead.
	ErrMissingKey = errors.New("activity: record lacks its correlation key")
	// ErrRecordContract means a Record carries neither or both of Raw and
	// Problem, or a weak (ref-less) key with record bytes. Deferred rows must
	// be read in full before they are applied: an occurrence is decoded only
	// on its first delivery.
	ErrRecordContract = errors.New("activity: record needs exactly one of Raw or Problem")
	// ErrResolve means a Resolution does not name a provisional occurrence of
	// this window and a valid final weak key.
	ErrResolve = errors.New("activity: invalid provisional resolution")
)

// RequestKey identifies one MCP tool call. ServerID is empty for legacy
// records written before server IDs existed.
type RequestKey struct {
	ServerID  string
	RequestID string
}

// EffectKey identifies one admitted mutation. A retry with the same
// operation_id is a new request, never a second effect.
type EffectKey struct {
	ServerID    string
	OperationID string
}

// ProcessKey identifies a managed terminal process, not an AI conversation.
type ProcessKey struct {
	ServerID  string
	SessionID string
}

// Record is one journal row delivered to a projection window. Exactly one of
// Raw and Problem is set.
type Record struct {
	Delivery Delivery
	// Raw is the complete original JSONL record (page data, or a
	// single-record read for a row the page deferred). The projection keeps
	// only its digest and decoded correlation fields, never the bytes.
	Raw []byte
	// Problem describes a row the reader reported as damaged.
	Problem string
}

// ChangeKind classifies the effect of one Apply call. Unread counting and
// focus decisions belong to the frontend; it should treat only NewAction,
// NewEffect and Transition delivered on the live chain as new activity (a live
// NewEffect is, for example, a long download finishing whose request lies
// outside the window).
type ChangeKind uint8

const (
	// ChangeRedelivery: the occurrence was already applied.
	ChangeRedelivery ChangeKind = iota + 1
	// ChangeDiagnostic: a damaged, undecodable or uncorrelatable row.
	ChangeDiagnostic
	// ChangeServerStart: a server instance marker.
	ChangeServerStart
	// ChangeNewAction: the first record of a tool call not seen before.
	ChangeNewAction
	// ChangeNewEffect: the first record of an effect whose origin request
	// has no record in this window yet.
	ChangeNewEffect
	// ChangeTransition: a new lifecycle record of a known action or effect.
	ChangeTransition
	// ChangeReattributed: the origin request_start arrived after other
	// records of the action, so its attribution changed. Not new activity.
	ChangeReattributed
	// ChangeDuplicate: another journal copy of a known record, same bytes.
	ChangeDuplicate
	// ChangeConflict: another journal copy of a known record, other bytes.
	ChangeConflict
	// ChangeResolved: a provisional ref-less row got its final key. Moved
	// reports that it merged into an occurrence already known by that key
	// and the merge changed that occurrence's order.
	ChangeResolved
)

// Change reports what one Apply call changed.
type Change struct {
	Kind    ChangeKind
	Chain   Chain
	Moved   bool        // A redelivery changed the occurrence's order.
	Request *RequestKey // Affected action, if any.
	Effect  *EffectKey  // Affected effect, if any.
	// For ChangeResolved: the provisional key and its replacement.
	Replaced, Occurrence *OccurrenceKey
}

type phaseSlot uint8

const (
	slotStart phaseSlot = iota
	slotFinish
	slotRejected
)

type fact struct {
	event  Event
	digest [sha256.Size]byte
}

// phase holds the journal copies of one lifecycle record, up to
// maxPhaseOccurrences; further copies are only counted.
type phase struct {
	occ      []OccurrenceKey
	more     int
	conflict bool // Some copy has other bytes than the first tracked one.
}

type action struct {
	key    RequestKey
	phases [3]phase
}

type effect struct {
	key    EffectKey
	phases [2]phase // slotStart, slotFinish
}

type diagnostic struct {
	problem string
}

// Projection correlates the records of one window into actions and effects.
// It is not safe for concurrent use. Views are derived on demand from the
// applied occurrences, so the result does not depend on how the history and
// live chains were interleaved.
type Projection struct {
	currentServer string
	occ           *occurrences
	facts         map[OccurrenceKey]fact
	diagnostics   map[OccurrenceKey]diagnostic
	servers       map[OccurrenceKey]string
	actions       map[RequestKey]*action
	effects       map[EffectKey]*effect

	// Indexes from a referenced key to the entities whose records name it.
	byOrigin         map[RequestKey]map[EffectKey]bool  // operation_* request_id.
	byProcessEffect  map[ProcessKey]map[EffectKey]bool  // terminal_start operation_finish session_id.
	byProcessRequest map[ProcessKey]map[RequestKey]bool // terminal_start request_finish session_id.
}

// NewProjection returns an empty window for the server instance that owns the
// journal. Unfinished records of that instance may still be running; those of
// any other instance have an unknown outcome.
func NewProjection(currentServerID string) *Projection {
	return &Projection{
		currentServer:    currentServerID,
		occ:              newOccurrences(),
		facts:            make(map[OccurrenceKey]fact),
		diagnostics:      make(map[OccurrenceKey]diagnostic),
		servers:          make(map[OccurrenceKey]string),
		actions:          make(map[RequestKey]*action),
		effects:          make(map[EffectKey]*effect),
		byOrigin:         make(map[RequestKey]map[EffectKey]bool),
		byProcessEffect:  make(map[ProcessKey]map[EffectKey]bool),
		byProcessRequest: make(map[ProcessKey]map[RequestKey]bool),
	}
}

// Apply adds one delivered row. A row already applied is a redelivery and
// changes nothing but possibly its order. An error means the adapter broke
// the delivery contract and the window must be rebuilt.
func (p *Projection) Apply(rec Record) (Change, error) {
	if (rec.Raw == nil) == (rec.Problem == "") || (rec.Delivery.Key.Ref == "" && rec.Raw != nil) {
		return Change{}, ErrRecordContract
	}
	obs, err := p.occ.observe(rec.Delivery)
	if err != nil {
		return Change{}, err
	}
	key, chain := rec.Delivery.Key, rec.Delivery.Chain
	if !obs.first {
		return Change{Kind: ChangeRedelivery, Chain: chain, Moved: obs.moved}, nil
	}
	if rec.Raw == nil {
		p.diagnostics[key] = diagnostic{problem: rec.Problem}
		return Change{Kind: ChangeDiagnostic, Chain: chain}, nil
	}
	e, err := Decode(rec.Raw)
	if err == nil {
		err = correlatable(e)
	}
	if err != nil {
		p.diagnostics[key] = diagnostic{problem: err.Error()}
		return Change{Kind: ChangeDiagnostic, Chain: chain}, nil
	}
	f := fact{event: e, digest: sha256.Sum256(rec.Raw)}
	switch e.Kind {
	case KindServerStart:
		p.facts[key] = f
		p.servers[key] = e.ServerID
		return Change{Kind: ChangeServerStart, Chain: chain}, nil
	case KindOperationStart, KindOperationFinish:
		return p.applyEffect(key, chain, f), nil
	default:
		return p.applyRequest(key, chain, f), nil
	}
}

// Resolution replaces a provisional ref-less occurrence with its final key.
type Resolution struct {
	Provisional OccurrenceKey
	Final       OccurrenceKey
}

// Resolve gives a provisional ref-less row its final key. If the final key is
// already known (the same row arrived on the other chain or in an earlier
// read), the two merge and keep the first rank of each chain.
func (p *Projection) Resolve(r Resolution) (Change, error) {
	from, ok := p.occ.byKey[r.Provisional]
	if !r.Provisional.provisional() || !ok || !r.Final.valid() || r.Final.Ref != "" || r.Final.provisional() ||
		r.Final.Weak.Start != r.Provisional.Weak.Start || r.Final.Weak.End != r.Provisional.Weak.End {
		return Change{}, ErrResolve
	}
	diag := p.diagnostics[r.Provisional]
	delete(p.occ.byKey, r.Provisional)
	delete(p.diagnostics, r.Provisional)
	change := Change{Kind: ChangeResolved, Replaced: &r.Provisional, Occurrence: &r.Final}
	if to, merged := p.occ.byKey[r.Final]; merged {
		before := to.order()
		if from.history != 0 && (to.history == 0 || from.history > to.history) {
			to.history = from.history
		}
		if from.live != 0 && (to.live == 0 || from.live < to.live) {
			to.live = from.live
		}
		change.Moved = to.order() != before
		return change, nil
	}
	p.occ.byKey[r.Final] = from
	p.diagnostics[r.Final] = diag
	return change, nil
}

func correlatable(e Event) error {
	switch e.Kind {
	case KindUnknown:
		return errors.New("activity: unknown audit phase " + e.Phase)
	case KindRequestStart, KindRequestFinish, KindRequestRejected:
		if e.RequestID == "" {
			return ErrMissingKey
		}
	case KindOperationStart, KindOperationFinish:
		if e.OperationID == "" {
			return ErrMissingKey
		}
	}
	return nil
}

func (p *Projection) applyRequest(key OccurrenceKey, chain Chain, f fact) Change {
	e := f.event
	rk := RequestKey{ServerID: e.ServerID, RequestID: e.RequestID}
	a, known := p.actions[rk]
	if !known {
		a = &action{key: rk}
		p.actions[rk] = a
	}
	slot := slotStart
	switch e.Kind {
	case KindRequestFinish:
		slot = slotFinish
	case KindRequestRejected:
		slot = slotRejected
	}
	before := p.attribution(a)
	kind, tracked := p.addOccurrence(&a.phases[slot], key, f)
	if kind == ChangeTransition {
		switch {
		case !known:
			kind = ChangeNewAction
		case before != p.attribution(a):
			kind = ChangeReattributed
		}
	}
	if tracked && e.Kind == KindRequestFinish && e.Tool == "terminal_start" && e.SessionID != "" && !e.Replayed {
		addIndex(p.byProcessRequest, ProcessKey{ServerID: rk.ServerID, SessionID: e.SessionID}, rk)
	}
	return Change{Kind: kind, Chain: chain, Request: &rk}
}

func (p *Projection) applyEffect(key OccurrenceKey, chain Chain, rec fact) Change {
	e := rec.event
	ek := EffectKey{ServerID: e.ServerID, OperationID: e.OperationID}
	f, known := p.effects[ek]
	if !known {
		f = &effect{key: ek}
		p.effects[ek] = f
	}
	slot := slotStart
	if e.Kind == KindOperationFinish {
		slot = slotFinish
	}
	kind, tracked := p.addOccurrence(&f.phases[slot], key, rec)
	if tracked && slot == slotFinish && e.Tool == "terminal_start" && e.SessionID != "" {
		addIndex(p.byProcessEffect, ProcessKey{ServerID: ek.ServerID, SessionID: e.SessionID}, ek)
	}
	if tracked && e.RequestID != "" {
		addIndex(p.byOrigin, RequestKey{ServerID: ek.ServerID, RequestID: e.RequestID}, ek)
	}
	change := Change{Kind: kind, Chain: chain, Effect: &ek}
	if origin, ok := p.effectOrigin(f); ok {
		change.Request = &origin
	}
	if kind == ChangeTransition && !known {
		if change.Request == nil {
			change.Kind = ChangeNewEffect
		} else if _, ok := p.actions[*change.Request]; !ok {
			change.Kind = ChangeNewEffect
		}
	}
	return change
}

// addOccurrence attaches a new occurrence to a lifecycle slot. The first
// occurrence of a slot is a transition; later ones are journal copies. Copies
// beyond the tracked limit keep neither their key nor their decoded fields,
// and do not feed the lookup indexes.
func (p *Projection) addOccurrence(ph *phase, key OccurrenceKey, f fact) (ChangeKind, bool) {
	kind := ChangeTransition
	if len(ph.occ) > 0 {
		kind = ChangeConflict
		for _, other := range ph.occ {
			if p.facts[other].digest == f.digest {
				kind = ChangeDuplicate
				break
			}
		}
		if kind == ChangeConflict {
			ph.conflict = true
		}
	}
	if len(ph.occ) < maxPhaseOccurrences {
		ph.occ = append(ph.occ, key)
		p.facts[key] = f
		return kind, true
	}
	ph.more++
	return kind, false
}

// primary returns the earliest tracked occurrence of a slot in journal order
// and its decoded event. Later copies never override it. Beyond
// maxPhaseOccurrences copies an older untracked copy cannot become primary. Before the window's
// join point the order, and so the primary of conflicting copies, may still
// change without a change event; windows are not published before joining.
func (p *Projection) primary(ph phase) (OccurrenceKey, Event, bool) {
	if len(ph.occ) == 0 {
		return OccurrenceKey{}, Event{}, false
	}
	best := ph.occ[0]
	for _, key := range ph.occ[1:] {
		if p.order(key).Compare(p.order(best)) < 0 {
			best = key
		}
	}
	return best, p.facts[best].event, true
}

func (p *Projection) order(key OccurrenceKey) Order {
	return p.occ.byKey[key].order()
}

func (p *Projection) effectOrigin(f *effect) (RequestKey, bool) {
	for _, slot := range []phaseSlot{slotStart, slotFinish} {
		if _, e, ok := p.primary(f.phases[slot]); ok && e.RequestID != "" {
			return RequestKey{ServerID: f.key.ServerID, RequestID: e.RequestID}, true
		}
	}
	return RequestKey{}, false
}

func (p *Projection) attribution(a *action) Attribution {
	if _, start, ok := p.primary(a.phases[slotStart]); ok {
		if _, named := start.Identity.Key(); named {
			return Attributed
		}
		return Unattributed
	}
	if len(a.phases[slotRejected].occ) > 0 {
		return Unattributed // request_rejected carries no call_meta.
	}
	return OriginNotLoaded
}

func addIndex[K, V comparable](index map[K]map[V]bool, key K, value V) {
	set, ok := index[key]
	if !ok {
		set = make(map[V]bool)
		index[key] = set
	}
	set[value] = true
}

func sortedKeys[K comparable](m map[K]bool, less func(a, b K) int) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, less)
	return keys
}

func compareRequest(a, b RequestKey) int {
	return cmp.Or(cmp.Compare(a.ServerID, b.ServerID), cmp.Compare(a.RequestID, b.RequestID))
}

func compareEffect(a, b EffectKey) int {
	return cmp.Or(cmp.Compare(a.ServerID, b.ServerID), cmp.Compare(a.OperationID, b.OperationID))
}
