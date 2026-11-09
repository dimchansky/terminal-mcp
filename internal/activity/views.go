package activity

import (
	"cmp"
	"slices"
)

// Attribution tells which observed conversation an action belongs to.
type Attribution uint8

const (
	// OriginNotLoaded: the action's request_start is outside the loaded
	// window; it may still be found by backfill. Not "unattributed".
	OriginNotLoaded Attribution = iota + 1
	// Unattributed: the recorded start has no usable conversation hint, or
	// the input was rejected without a recorded start.
	Unattributed
	// Attributed: the recorded start names a client-claimed conversation.
	Attributed
)

// InputState tells whether the model-supplied input is in the journal.
type InputState uint8

const (
	// InputUnknown: the request_start is not loaded.
	InputUnknown InputState = iota + 1
	// InputRecorded: the request_start record holds the input the server
	// received (current schema records arguments before validation).
	InputRecorded
	// InputLegacy: an unversioned start record. Writers of that era
	// summarized some arguments (for example omitting file contents), so it
	// must not be presented as the complete input. A brief window of early
	// schema-2 records shares this limitation and cannot be told apart.
	InputLegacy
	// InputNotRecorded: only a request_rejected record exists; the input was
	// never stored and the tool did not run.
	InputNotRecorded
)

// RequestState is the observed state of the MCP request itself.
type RequestState uint8

const (
	// RequestFinished: a request_finish or request_rejected was recorded.
	RequestFinished RequestState = iota + 1
	// RequestOpen: no finish yet on the current server instance.
	RequestOpen
	// RequestUnknown: no finish, and the request belongs to another (earlier)
	// server instance or its records are not loaded.
	RequestUnknown
)

// EffectState is the observed state of an admitted mutation, independent of
// the request that admitted it (a canceled request can leave its effect
// running).
type EffectState uint8

const (
	// EffectFinished: an operation_finish was recorded.
	EffectFinished EffectState = iota + 1
	// EffectInFlight: started on the current server instance, no finish yet.
	EffectInFlight
	// EffectUnknown: no finish, and the effect belongs to another instance.
	EffectUnknown
)

// LinkKind says how an action relates to an effect.
type LinkKind uint8

const (
	// LinkOwn: the action admitted the effect.
	LinkOwn LinkKind = iota + 1
	// LinkReplay: the action replayed an existing operation_id; it did not
	// create a second effect.
	LinkReplay
	// LinkObserve: the action observed the effect (file_receive_result).
	LinkObserve
)

// EffectLink connects an action to an effect. Loaded is false when the effect
// records are outside the window.
type EffectLink struct {
	Kind   LinkKind
	Effect EffectKey
	Loaded bool
}

// PhaseView lists the journal occurrences of one lifecycle record in journal
// order. Primary is the earliest; Conflict reports copies with other bytes.
type PhaseView struct {
	Primary     OccurrenceKey
	Occurrences []OccurrenceKey
	More        int // Copies beyond the tracked limit.
	Conflict    bool
}

// ActionView is the derived state of one MCP tool call. Status, Outcome,
// Reason and Replayed are copied from the recorded request_finish (or
// request_rejected) and are empty when not recorded.
type ActionView struct {
	Key         RequestKey
	Tool        string
	Legacy      bool
	Identity    Identity
	Attribution Attribution
	Input       InputState
	State       RequestState
	// PossiblyUnrecorded: open, but the audit was degraded meanwhile, so the
	// finish may never be recorded (see SyncIndex).
	PossiblyUnrecorded bool
	// RequestState is the SDK requestState recorded in request_start.
	RequestState string
	Status       string
	Outcome      string
	Reason       string
	Replayed     bool
	Duration     Duration
	Start        *PhaseView
	Finish       *PhaseView
	Rejected     *PhaseView
	Effect       *EffectLink
	// Process is the managed process the call created or used. The creator
	// is the terminal_start effect (and its request) that launched it, when
	// loaded; a call from another conversation stays in its own lane.
	Process        *ProcessKey
	ProcessEffect  *EffectKey
	ProcessCreator *RequestKey
	Order          Order
}

// EffectView is the derived state of one admitted mutation.
type EffectView struct {
	Key            EffectKey
	Tool           string
	Origin         *RequestKey
	OriginConflict bool // Start and finish name different requests.
	State          EffectState
	Status         string // From operation_finish, when recorded.
	// PossiblyUnrecorded: in flight, but the audit was degraded meanwhile,
	// so the finish may never be recorded (see SyncIndex).
	PossiblyUnrecorded bool
	Duration           Duration
	Start              *PhaseView
	Finish             *PhaseView
	Order              Order
}

// DiagnosticView is a journal row that could not be correlated. Its original
// bytes, if any, remain available from the journal. PossibleRepeat marks a
// ref-less row whose identity could not be established; it may be the same
// row as another diagnostic and is shown rather than hidden.
type DiagnosticView struct {
	Key            OccurrenceKey
	Problem        string
	Order          Order
	PossibleRepeat bool
}

// ServerStartView marks the start of a server instance.
type ServerStartView struct {
	Key      OccurrenceKey
	ServerID string
	Order    Order
}

// Need asks the adapter to look further back for a missing origin.
// BeyondCoverage is set when the window's history chain has stopped (start
// reached, gap, changed source or eviction): the adapter must not scan for it
// in this window, and the origin is shown as outside the loaded history.
type Need struct {
	Request        *RequestKey
	Effect         *EffectKey
	BeyondCoverage bool
}

func (p *Projection) phaseView(ph phase) *PhaseView {
	primary, _, ok := p.primary(ph)
	if !ok {
		return nil
	}
	view := &PhaseView{Primary: primary, Occurrences: slices.Clone(ph.occ), More: ph.more, Conflict: ph.conflict}
	slices.SortFunc(view.Occurrences, func(a, b OccurrenceKey) int { return p.order(a).Compare(p.order(b)) })
	return view
}

// Action returns the view of one action.
func (p *Projection) Action(key RequestKey) (ActionView, bool) {
	a, ok := p.actions[key]
	if !ok {
		return ActionView{}, false
	}
	v := ActionView{
		Key:      key,
		Start:    p.phaseView(a.phases[slotStart]),
		Finish:   p.phaseView(a.phases[slotFinish]),
		Rejected: p.phaseView(a.phases[slotRejected]),
	}
	v.Attribution = p.attribution(a)
	_, start, hasStart := p.primary(a.phases[slotStart])
	_, finish, hasFinish := p.primary(a.phases[slotFinish])
	_, rejected, hasRejected := p.primary(a.phases[slotRejected])
	for _, candidate := range []struct {
		e  Event
		ok bool
	}{{start, hasStart}, {finish, hasFinish}, {rejected, hasRejected}} {
		if candidate.ok {
			v.Tool, v.Legacy = candidate.e.Tool, candidate.e.Legacy
			break
		}
	}
	switch {
	case hasStart && start.Legacy:
		v.Identity, v.Input = start.Identity, InputLegacy
		v.Order = p.order(v.Start.Primary)
	case hasStart:
		v.Identity, v.Input = start.Identity, InputRecorded
		v.Order = p.order(v.Start.Primary)
	case hasRejected:
		v.Input = InputNotRecorded
	default:
		v.Input = InputUnknown
	}
	if hasStart {
		v.RequestState = start.RequestState
	}
	switch {
	case hasFinish:
		v.Status, v.Outcome, v.Reason, v.Replayed = finish.Status, finish.Outcome, finish.Reason, finish.Replayed
	case hasRejected:
		v.Status, v.Reason = rejected.Status, rejected.Reason
	}
	if v.Order == (Order{}) {
		v.Order = p.earliest(a.phases[:])
	}

	switch {
	case hasFinish || hasRejected:
		v.State = RequestFinished
	case key.ServerID != "" && key.ServerID == p.currentServer:
		v.State, v.PossiblyUnrecorded = RequestOpen, p.unrecordedRequests[key]
	default:
		v.State = RequestUnknown
	}
	if hasFinish {
		v.Duration = FinishDuration(finish, hasStart)
	}

	v.Effect = p.effectLink(key, v.Tool, finish, hasFinish)
	if pk, ok := p.actionProcess(key, v.Effect, finish, hasFinish); ok {
		v.Process = &pk
		v.ProcessEffect, v.ProcessCreator = p.processOrigin(pk)
	}
	return v, true
}

// actionProcess is the process named by the call's finish, or else the one
// its own terminal_start effect reported (a canceled or failed request_finish
// omits the session ID, while operation_finish records it).
func (p *Projection) actionProcess(key RequestKey, link *EffectLink, finish Event, hasFinish bool) (ProcessKey, bool) {
	if hasFinish && finish.SessionID != "" {
		return ProcessKey{ServerID: key.ServerID, SessionID: finish.SessionID}, true
	}
	if link != nil && link.Kind == LinkOwn {
		if _, e, ok := p.primary(p.effects[link.Effect].phases[slotFinish]); ok &&
			e.Tool == "terminal_start" && e.SessionID != "" {
			return ProcessKey{ServerID: key.ServerID, SessionID: e.SessionID}, true
		}
	}
	return ProcessKey{}, false
}

func (p *Projection) earliest(phases []phase) Order {
	var best Order
	found := false
	for _, ph := range phases {
		for _, key := range ph.occ {
			if o := p.order(key); !found || o.Compare(best) < 0 {
				best, found = o, true
			}
		}
	}
	return best
}

// effectLink prefers the action's own effect, then a replay, then an
// observation. The raw operation_id in request_start is never used: a
// conflicting ID or a request denied before admission has no effect.
func (p *Projection) effectLink(key RequestKey, tool string, finish Event, hasFinish bool) *EffectLink {
	var own []EffectKey
	for ek := range p.byOrigin[key] {
		if origin, ok := p.effectOrigin(p.effects[ek]); ok && origin == key {
			own = append(own, ek)
		}
	}
	if len(own) > 0 {
		slices.SortFunc(own, compareEffect)
		return &EffectLink{Kind: LinkOwn, Effect: own[0], Loaded: true}
	}
	if !hasFinish || finish.OperationID == "" {
		return nil
	}
	ek := EffectKey{ServerID: key.ServerID, OperationID: finish.OperationID}
	_, loaded := p.effects[ek]
	switch {
	case tool == "file_receive_result":
		return &EffectLink{Kind: LinkObserve, Effect: ek, Loaded: loaded}
	case finish.Replayed:
		return &EffectLink{Kind: LinkReplay, Effect: ek, Loaded: loaded}
	}
	return nil
}

// processOrigin finds the terminal_start effect that launched a process,
// primarily from its operation_finish; a non-replayed terminal_start
// request_finish is the fallback when the effect records are not loaded.
func (p *Projection) processOrigin(pk ProcessKey) (*EffectKey, *RequestKey) {
	var effects []EffectKey
	for ek := range p.byProcessEffect[pk] {
		if _, e, ok := p.primary(p.effects[ek].phases[slotFinish]); ok && e.SessionID == pk.SessionID {
			effects = append(effects, ek)
		}
	}
	if len(effects) > 0 {
		slices.SortFunc(effects, compareEffect)
		ek := effects[0]
		if origin, ok := p.effectOrigin(p.effects[ek]); ok {
			return &ek, &origin
		}
		return &ek, nil
	}
	var requests []RequestKey
	for rk := range p.byProcessRequest[pk] {
		if _, e, ok := p.primary(p.actions[rk].phases[slotFinish]); ok && e.SessionID == pk.SessionID && !e.Replayed {
			requests = append(requests, rk)
		}
	}
	if len(requests) == 0 {
		return nil, nil
	}
	slices.SortFunc(requests, compareRequest)
	return nil, &requests[0]
}

// Effect returns the view of one effect.
func (p *Projection) Effect(key EffectKey) (EffectView, bool) {
	f, ok := p.effects[key]
	if !ok {
		return EffectView{}, false
	}
	v := EffectView{Key: key, Start: p.phaseView(f.phases[slotStart]), Finish: p.phaseView(f.phases[slotFinish])}
	_, start, hasStart := p.primary(f.phases[slotStart])
	_, finish, hasFinish := p.primary(f.phases[slotFinish])
	if origin, ok := p.effectOrigin(f); ok {
		v.Origin = &origin
	}
	if hasStart && hasFinish && start.RequestID != "" && finish.RequestID != "" && start.RequestID != finish.RequestID {
		v.OriginConflict = true
	}
	if hasStart {
		v.Tool = start.Tool
	} else if hasFinish {
		v.Tool = finish.Tool
	}
	v.Order = p.earliest(f.phases[:])
	switch {
	case hasFinish:
		v.State, v.Status = EffectFinished, finish.Status
		v.Duration = FinishDuration(finish, hasStart)
	case key.ServerID != "" && key.ServerID == p.currentServer:
		v.State, v.PossiblyUnrecorded = EffectInFlight, p.unrecordedEffects[key]
	default:
		v.State = EffectUnknown
	}
	return v, true
}

// Actions returns every action in journal order.
func (p *Projection) Actions() []ActionView {
	out := make([]ActionView, 0, len(p.actions))
	for key := range p.actions {
		v, _ := p.Action(key)
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b ActionView) int {
		return cmp.Or(a.Order.Compare(b.Order), compareRequest(a.Key, b.Key))
	})
	return out
}

// Effects returns every effect in journal order.
func (p *Projection) Effects() []EffectView {
	out := make([]EffectView, 0, len(p.effects))
	for key := range p.effects {
		v, _ := p.Effect(key)
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b EffectView) int {
		return cmp.Or(a.Order.Compare(b.Order), compareEffect(a.Key, b.Key))
	})
	return out
}

// Diagnostics returns uncorrelatable rows in journal order.
func (p *Projection) Diagnostics() []DiagnosticView {
	out := make([]DiagnosticView, 0, len(p.diagnostics))
	for key, d := range p.diagnostics {
		out = append(out, DiagnosticView{Key: key, Problem: d.problem, Order: p.order(key), PossibleRepeat: key.provisional()})
	}
	slices.SortFunc(out, func(a, b DiagnosticView) int { return a.Order.Compare(b.Order) })
	return out
}

// ServerStarts returns server instance markers in journal order.
func (p *Projection) ServerStarts() []ServerStartView {
	out := make([]ServerStartView, 0, len(p.servers))
	for key, id := range p.servers {
		out = append(out, ServerStartView{Key: key, ServerID: id, Order: p.order(key)})
	}
	slices.SortFunc(out, func(a, b ServerStartView) int { return a.Order.Compare(b.Order) })
	return out
}

// Needs lists origins the adapter should look for with a bounded backward
// scan: requests whose start is not loaded, and effects referenced by a
// replay or observation but not loaded.
func (p *Projection) Needs() []Need {
	requests := map[RequestKey]bool{}
	effects := map[EffectKey]bool{}
	for key, a := range p.actions {
		if p.attribution(a) == OriginNotLoaded {
			requests[key] = true
		}
		_, finish, hasFinish := p.primary(a.phases[slotFinish])
		tool := finish.Tool
		if _, start, ok := p.primary(a.phases[slotStart]); ok {
			tool = start.Tool
		}
		if link := p.effectLink(key, tool, finish, hasFinish); link != nil && !link.Loaded {
			effects[link.Effect] = true
		}
	}
	for _, f := range p.effects {
		if origin, ok := p.effectOrigin(f); ok {
			if a, loaded := p.actions[origin]; !loaded || len(a.phases[slotStart].occ) == 0 &&
				len(a.phases[slotRejected].occ) == 0 {
				requests[origin] = true
			}
		}
	}
	beyond := p.coverage != CoverageOpen
	var out []Need
	for _, key := range sortedKeys(requests, compareRequest) {
		out = append(out, Need{Request: &key, BeyondCoverage: beyond})
	}
	for _, key := range sortedKeys(effects, compareEffect) {
		out = append(out, Need{Effect: &key, BeyondCoverage: beyond})
	}
	return out
}
