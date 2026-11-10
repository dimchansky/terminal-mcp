package dashboard

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"terminalmcp/internal/activity"
	"terminalmcp/internal/audit"
	"terminalmcp/internal/viewer"
)

// Delivery limits: a response carries at most responseBudget bytes of
// base64 record data and maxRecords records, and stops at the time budget;
// it always carries at least one whole item so paging never stalls. Every
// request may also read up to scanPages history pages (8 MiB of scan each).
const (
	responseBudget = 1 << 20
	maxRecords     = 200
	requestTime    = 2 * time.Second
	scanPages      = 4
	catchUpPages   = 4
	maxClientNames = 8
)

// Lane kinds. A conversation lane is client-claimed; the others group
// actions without a usable claim, effects whose origin is not loaded, and
// journal rows (diagnostics and server starts).
const (
	laneConversation    = "conversation"
	laneUnattributed    = "unattributed"
	laneInvalidMetadata = "invalid_metadata"
	laneOriginNotLoaded = "origin_not_loaded"
	laneJournal         = "journal"
)

// Item kinds.
const (
	ItemAction      = "action"
	ItemEffect      = "effect" // An effect whose origin request is not loaded.
	ItemDiagnostic  = "diagnostic"
	ItemServerStart = "server_start"
)

// OrderKey is the wire form of an order (valid within one generation).
type OrderKey struct {
	Live bool  `json:"live"`
	Rank int64 `json:"rank"`
}

// Continuation pages older items: those ordered before Order, with Key
// breaking ties.
type Continuation struct {
	Order OrderKey `json:"order"`
	Key   string   `json:"key"`
}

// Record is one original journal record on the wire. DataB64 holds the exact
// JSONL bytes; Deferred records are loaded in full with the record endpoint
// (Size is zero when a deferred record was not read).
// Unavailable records cannot be read now; Retryable ones may be read later.
type Record struct {
	Occurrence  string `json:"occurrence"`
	DataB64     string `json:"data_b64,omitempty"`
	Size        int    `json:"size,omitempty"`
	Deferred    bool   `json:"deferred,omitempty"`
	Unavailable bool   `json:"unavailable,omitempty"`
	Retryable   bool   `json:"retryable,omitempty"`
	Problem     string `json:"problem,omitempty"`
}

// Item is one timeline entry: an action (with the effect it admitted), an
// effect whose origin is not loaded, a journal diagnostic or a server start.
// Replay and observe links name their effect but do not carry it.
type Item struct {
	Kind        string                    `json:"kind"`
	Key         string                    `json:"key"`
	Lane        string                    `json:"lane"`
	Order       OrderKey                  `json:"order"`
	Action      *activity.ActionView      `json:"action,omitempty"`
	Effect      *activity.EffectView      `json:"effect,omitempty"`
	Diagnostic  *activity.DiagnosticView  `json:"diagnostic,omitempty"`
	ServerStart *activity.ServerStartView `json:"server_start,omitempty"`
	Summary     *Hint                     `json:"summary,omitempty"`
}

// Coverage tells how far the window's history reaches. End is one of open,
// reached_start (the oldest visible record, not proof of the journal's
// beginning), gap, changed or evicted.
type Coverage struct {
	End string `json:"end"`
	// UseArchive: older rows were evicted; read them with an archive window.
	UseArchive bool `json:"use_archive"`
	// Recorded times of the oldest and newest loaded items (writer clock).
	OldestTime string `json:"oldest_time,omitempty"`
	NewestTime string `json:"newest_time,omitempty"`
}

// Page is a delivery of whole items, newest first, with their records.
type Page struct {
	Generation uint64        `json:"generation"`
	Items      []Item        `json:"items"`
	Records    []Record      `json:"records"`
	Next       *Continuation `json:"next,omitempty"`
	Coverage   Coverage      `json:"coverage"`
	ScanUpdates
}

// Lane is a grouping within the window's coverage. Times are the writer's
// recorded event times.
type Lane struct {
	ID             string   `json:"id"`
	Kind           string   `json:"kind"`
	IdentitySource *string  `json:"identitySource"`
	ConversationID *string  `json:"conversationId"`
	ClientNames    []string `json:"client_names,omitempty"`
	Items          int      `json:"items"`
	FirstTime      string   `json:"first_time,omitempty"`
	LastTime       string   `json:"last_time,omitempty"`
}

func orderKey(o activity.Order) OrderKey {
	live, rank := o.SortKey()
	return OrderKey{Live: live, Rank: rank}
}

func compareOrder(a, b OrderKey) int {
	switch {
	case a.Live != b.Live && a.Live:
		return 1
	case a.Live != b.Live:
		return -1
	case a.Rank < b.Rank:
		return -1
	case a.Rank > b.Rank:
		return 1
	}
	return 0
}

func compareItems(a, b Item) int {
	if c := compareOrder(a.Order, b.Order); c != 0 {
		return c
	}
	return strings.Compare(a.Key, b.Key)
}

// laneKey classifies an action into a lane.
func laneKey(v activity.ActionView) (key, kind string) {
	switch {
	case v.Attribution == activity.OriginNotLoaded:
		return laneOriginNotLoaded, laneOriginNotLoaded
	case v.Attribution == activity.Attributed:
		c, _ := v.Identity.Key()
		return "c\x00" + c.IdentitySource + "\x00" + c.ConversationID, laneConversation
	case v.Identity.MetadataInvalid:
		return laneInvalidMetadata, laneInvalidMetadata
	}
	return laneUnattributed, laneUnattributed
}

func actionKey(k activity.RequestKey) string { return "a:" + k.ServerID + "/" + k.RequestID }
func effectKey(k activity.EffectKey) string  { return "e:" + k.ServerID + "/" + k.OperationID }

// laneIDs maps lanes to short opaque IDs namespaced by generation, so a long
// client-supplied conversation ID never has to travel in a request.
type laneIDs struct {
	generation uint64
	byKey      map[string]string
	byID       map[string]string
}

func (m *managed) laneID(generation uint64, key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lanes == nil || m.lanes.generation != generation {
		m.lanes = &laneIDs{generation: generation, byKey: map[string]string{}, byID: map[string]string{}}
	}
	if id, ok := m.lanes.byKey[key]; ok {
		return id
	}
	id := fmt.Sprintf("g%d-l%d", generation, len(m.lanes.byKey)+1)
	m.lanes.byKey[key], m.lanes.byID[id] = id, key
	return id
}

func (m *managed) laneKeyOf(generation uint64, id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lanes == nil || m.lanes.generation != generation {
		return "", false
	}
	key, ok := m.lanes.byID[id]
	return key, ok
}

// item is an entry ready for delivery, with its lane key and occurrences.
type item struct {
	Item
	laneKey string
	occ     []activity.OccurrenceKey
}

func phaseOccurrences(phases ...*activity.PhaseView) []activity.OccurrenceKey {
	var out []activity.OccurrenceKey
	for _, ph := range phases {
		if ph != nil {
			out = append(out, ph.Occurrences...)
		}
	}
	return out
}

// snapshot is a consistent view of the window taken under its lock. It is
// cached and shared between requests: never modify its items or views.
type snapshot struct {
	generation uint64
	items      []item // Newest first.
	coverage   Coverage
	// head and unseen were read with the items. A cached snapshot keeps an
	// older head only while no rows were applied since, so catching up
	// from it never skips a row.
	head   string
	unseen bool
}

// collect snapshots the window's items newest first, reusing the views of
// an unchanged window state. Views are built under the window lock; the
// cache and lane IDs are handled outside it, so the window lock and the
// managed lock are never nested. The snapshot is shared and read-only.
func (mg *Manager) collect(m *managed) snapshot {
	key, head := m.win.keyAndHead()
	m.mu.Lock()
	cached, hit := m.cached, m.cached != nil && m.cachedKey == key
	m.mu.Unlock()
	if hit {
		s := *cached
		s.head = head // Issued since the view was built; the view holds its rows.
		return s
	}
	var s snapshot
	m.win.View(func(p *activity.Projection, meta ViewMeta) {
		s.generation, key, s.head, s.unseen = meta.Generation, meta.Key, meta.Head, meta.PossibleUnseen
		s.coverage = coverageOf(p.Coverage())
		m.hints.prune(p.HasAction)
		for _, v := range p.Actions() {
			key, _ := laneKey(v)
			it := item{Item: Item{Kind: ItemAction, Key: actionKey(v.Key), Order: orderKey(v.Order), Action: &v},
				laneKey: key, occ: phaseOccurrences(v.Start, v.Rejected, v.Finish)}
			if h, ok := m.hints.get(v.Key); ok {
				it.Summary = &h
			}
			if v.Effect != nil && v.Effect.Kind == activity.LinkOwn {
				if e, ok := p.Effect(v.Effect.Effect); ok {
					it.Effect = &e
					it.occ = append(it.occ, phaseOccurrences(e.Start, e.Finish)...)
				}
			}
			s.items = append(s.items, it)
		}
		for _, e := range p.Effects() {
			if e.Origin != nil && p.HasAction(*e.Origin) && ownedBy(p, e) {
				continue // Delivered with its action.
			}
			s.items = append(s.items, item{Item: Item{Kind: ItemEffect, Key: effectKey(e.Key), Order: orderKey(e.Order),
				Effect: &e}, laneKey: laneOriginNotLoaded, occ: phaseOccurrences(e.Start, e.Finish)})
		}
		for _, d := range p.Diagnostics() {
			s.items = append(s.items, item{Item: Item{Kind: ItemDiagnostic, Key: "d:" + occurrenceID(d.Key),
				Order: orderKey(d.Order), Diagnostic: &d}, laneKey: laneJournal, occ: []activity.OccurrenceKey{d.Key}})
		}
		for _, st := range p.ServerStarts() {
			s.items = append(s.items, item{Item: Item{Kind: ItemServerStart, Key: "s:" + occurrenceID(st.Key),
				Order: orderKey(st.Order), ServerStart: &st}, laneKey: laneJournal, occ: []activity.OccurrenceKey{st.Key}})
		}
	})
	slices.SortFunc(s.items, func(a, b item) int { return compareItems(b.Item, a.Item) })
	for i := range s.items {
		s.items[i].Lane = m.laneID(s.generation, s.items[i].laneKey)
		if t := itemTime(s.items[i].Item); t != "" {
			if s.coverage.NewestTime == "" {
				s.coverage.NewestTime = t
			}
			s.coverage.OldestTime = t
		}
	}
	m.mu.Lock()
	m.cached, m.cachedKey = &s, key
	m.mu.Unlock()
	return s
}

// ownedBy reports whether the effect is delivered with its origin action.
func ownedBy(p *activity.Projection, e activity.EffectView) bool {
	a, ok := p.Action(*e.Origin)
	return ok && a.Effect != nil && a.Effect.Kind == activity.LinkOwn && a.Effect.Effect == e.Key
}

func coverageOf(c activity.CoverageView) Coverage {
	names := map[activity.CoverageEnd]string{
		activity.CoverageOpen: "open", activity.CoverageReachedStart: "reached_start",
		activity.CoverageGap: "gap", activity.CoverageChanged: "changed", activity.CoverageEvicted: "evicted",
	}
	return Coverage{End: names[c.End], UseArchive: c.FloorSet}
}

// deliver packs whole items older than `before` (nil: from the newest) until
// a budget is reached, always including at least one item. Records that do
// not fit are marked deferred with their size; want limits which records
// are sent.
func (mg *Manager) deliver(ctx context.Context, s snapshot, items []item, before *Continuation,
	want func(activity.OccurrenceKey) bool) Page {
	return mg.deliverFrom(ctx, s, items, before, want, responseBudget, 0, time.Now().Add(requestTime))
}

// deliverFrom is deliver with part of the budget and time already spent.
func (mg *Manager) deliverFrom(ctx context.Context, s snapshot, items []item, before *Continuation,
	want func(activity.OccurrenceKey) bool, budget, records int, deadline time.Time) Page {
	page := Page{Generation: s.generation, Items: []Item{}, Records: []Record{}, Coverage: s.coverage}
	for _, it := range items {
		if before != nil && compareItems(it.Item, Item{Order: before.Order, Key: before.Key}) >= 0 {
			continue
		}
		if len(page.Items) > 0 && (budget <= 0 || records >= maxRecords || time.Now().After(deadline)) {
			last := page.Items[len(page.Items)-1]
			page.Next = &Continuation{Order: last.Order, Key: last.Key}
			return page
		}
		page.Items = append(page.Items, it.Item)
		for _, key := range it.occ {
			if want != nil && !want(key) {
				continue // The client already has it.
			}
			if key.Ref != "" && (budget <= 0 || records >= maxRecords) {
				// Not read at all: the client loads it with the record
				// endpoint (its size is then unknown here).
				records++
				page.Records = append(page.Records, Record{Occurrence: occurrenceID(key), Deferred: true})
				continue
			}
			rec := mg.readRecord(ctx, key)
			encoded := len(rec.DataB64)
			if rec.DataB64 != "" && (encoded > budget || records >= maxRecords) {
				rec.DataB64, rec.Deferred = "", true
			}
			budget -= encoded // A deferred record still ends the page.
			records++
			page.Records = append(page.Records, rec)
		}
	}
	return page
}

// readRecord loads one occurrence's original bytes by its reader reference.
func (mg *Manager) readRecord(ctx context.Context, key activity.OccurrenceKey) Record {
	rec := Record{Occurrence: occurrenceID(key)}
	if key.Ref == "" {
		rec.Unavailable, rec.Problem = true, "the row has no readable reference (oversized or incomplete)"
		return rec
	}
	ref, err := audit.ParseRecordRef(key.Ref)
	if err != nil {
		rec.Unavailable, rec.Problem = true, "invalid reference"
		return rec
	}
	data, _, err := mg.reader.ReadRecord(ctx, ref)
	switch {
	case errors.Is(err, audit.ErrInvalidCursor), errors.Is(err, audit.ErrHistoryGap):
		rec.Unavailable, rec.Problem = true, "the journal source changed or is missing"
		return rec
	case err != nil:
		rec.Unavailable, rec.Retryable, rec.Problem = true, true, "temporarily unreadable"
		return rec
	}
	rec.Size = len(data)
	rec.DataB64 = base64.StdEncoding.EncodeToString(data)
	return rec
}

// occurrenceID is the wire ID of an occurrence (see OccurrenceKey.String).
func occurrenceID(key activity.OccurrenceKey) string { return key.String() }

// Recent delivers the newest items across all lanes.
func (mg *Manager) Recent(ctx context.Context, owner viewer.Attacher, id string, before *Continuation) (Page, error) {
	var page Page
	err := mg.run(ctx, owner, id, func(m *managed) error {
		s := mg.collect(m)
		page = mg.deliver(ctx, s, s.items, before, nil)
		return nil
	})
	return page, err
}

// LaneItems delivers already-loaded items of a lane older than before.
func (mg *Manager) LaneItems(ctx context.Context, owner viewer.Attacher, id, lane string, before *Continuation) (Page, error) {
	var page Page
	err := mg.run(ctx, owner, id, func(m *managed) error {
		var err error
		page, err = mg.laneItems(ctx, m, lane, before)
		return err
	})
	return page, err
}

func (mg *Manager) laneItems(ctx context.Context, m *managed, lane string, before *Continuation) (Page, error) {
	s := mg.collect(m)
	key, ok := m.laneKeyOf(s.generation, lane)
	if !ok {
		return Page{}, ErrGenerationChanged
	}
	return mg.deliver(ctx, s, laneWithRestarts(s.items, key), before, nil), nil
}

// laneWithRestarts selects a lane's items plus the server starts within the
// lane's order range, so restarts show inside its timeline.
func laneWithRestarts(all []item, key string) []item {
	var oldest, newest *OrderKey
	for i := range all {
		if all[i].laneKey != key {
			continue
		}
		if newest == nil {
			newest = &all[i].Order
		}
		oldest = &all[i].Order
	}
	var items []item
	for _, it := range all {
		inRange := oldest != nil && compareOrder(it.Order, *oldest) >= 0 && compareOrder(it.Order, *newest) <= 0
		if it.laneKey == key || (it.Kind == ItemServerStart && inRange) {
			items = append(items, it)
		}
	}
	return items
}

// ScanUpdates reports what a history scan changed in items the client may
// hold: Updated items (with their new lane; records in the answer's
// records), Removed item keys (effects now delivered with their action,
// replaced provisional rows) and the lanes whose membership or items
// changed. When Updated does not fit the answer, UpdatedMore is set and the
// client re-fetches AffectedLanes; Removed and AffectedLanes are complete.
type ScanUpdates struct {
	Updated       []Item   `json:"updated,omitempty"`
	UpdatedMore   bool     `json:"updated_more,omitempty"`
	Removed       []string `json:"removed,omitempty"`
	AffectedLanes []string `json:"affected_lanes,omitempty"`
}

// tracked runs a history scan and reports its updates to loaded items. The
// caller holds m.scanMu, so tracked scans of one window never interleave.
func (mg *Manager) tracked(m *managed, scan func() error) (snapshot, ChangeSet, error) {
	before := mg.collect(m)
	m.win.Track()
	err := scan()
	return before, m.win.Untrack(), err
}

// updates delivers the scan's changes to items that existed before it, plus
// actions that now own an effect delivered separately before, within the
// budget left after the answer's own records.
func (mg *Manager) updates(ctx context.Context, m *managed, before snapshot, cs ChangeSet, delivered []Item, spent []Record,
	deadline time.Time) (ScanUpdates, []Record) {
	var up ScanUpdates
	after := mg.collect(m)
	if after.generation != before.generation {
		return up, nil // The client re-snapshots on the generation change.
	}
	existed := map[string]string{} // Item key → lane before the scan.
	for _, it := range before.items {
		existed[it.Key] = it.Lane
	}
	present := map[string]bool{}
	for _, it := range after.items {
		present[it.Key] = true
	}
	lanes := map[string]bool{}
	removedEffects := map[string]bool{}
	for _, it := range before.items {
		if (it.Kind == ItemEffect || it.Kind == ItemDiagnostic) && !present[it.Key] {
			up.Removed = append(up.Removed, it.Key)
			lanes[it.Lane] = true
			if it.Kind == ItemEffect {
				removedEffects[it.Key] = true
			}
		}
	}
	inPage := map[string]bool{}
	for _, it := range delivered {
		inPage[it.Key] = true
	}
	var changed []item
	for _, it := range after.items {
		_, had := existed[it.Key]
		var hit bool
		switch it.Kind {
		case ItemAction:
			own := it.Effect != nil && (cs.Effects[it.Effect.Key] || removedEffects[effectKey(it.Effect.Key)])
			hit = (had && (cs.Requests[it.Action.Key] || own)) || (it.Effect != nil && removedEffects[effectKey(it.Effect.Key)])
		case ItemEffect:
			hit = had && cs.Effects[it.Effect.Key]
		case ItemDiagnostic:
			if replaced, ok := cs.Occurrences[it.Diagnostic.Key]; ok {
				_, held := existed["d:"+occurrenceID(replaced)]
				hit = had || held // A row the client holds was resolved.
			}
		}
		if !hit {
			continue
		}
		lanes[it.Lane] = true
		if old, ok := existed[it.Key]; ok {
			lanes[old] = true
		}
		if !inPage[it.Key] { // Already delivered by the answer's own page.
			changed = append(changed, it)
		}
	}
	for l := range lanes {
		up.AffectedLanes = append(up.AffectedLanes, l)
	}
	slices.Sort(up.AffectedLanes)
	if len(changed) == 0 {
		return up, nil
	}
	budget, n := responseBudget, len(spent)
	for _, r := range spent {
		budget -= len(r.DataB64)
	}
	if budget <= 0 || n >= maxRecords || !time.Now().Before(deadline) {
		up.UpdatedMore = true // Nothing fits: the client re-fetches the affected lanes.
		return up, nil
	}
	page := mg.deliverFrom(ctx, after, changed, nil, nil, budget, n, deadline)
	up.Updated, up.UpdatedMore = page.Items, page.Next != nil
	return up, page.Records
}

// endOfHistory reports whether a scan error only means the history chain
// cannot continue; the coverage in every page reports it.
func endOfHistory(err error) bool {
	return errors.Is(err, ErrBeyondCoverage) || errors.Is(err, ErrUseArchive)
}

// scan continues the window's history chain within the per-request budget.
func (mg *Manager) scan(ctx context.Context, m *managed, pages int) error {
	if err := m.win.Scan(ctx, pages, time.Now().Add(requestTime)); err != nil && !endOfHistory(err) {
		return err
	}
	return nil
}

// Earlier returns older items of a lane: loaded ones first; when none are
// left it scans older history once within the budget and delivers what that
// found. An empty page whose coverage is not open means the lane has no more
// history in this window.
func (mg *Manager) Earlier(ctx context.Context, owner viewer.Attacher, id, lane string, before *Continuation) (Page, error) {
	var page Page
	deadline := time.Now().Add(requestTime)
	err := mg.run(ctx, owner, id, func(m *managed) error {
		var err error
		if page, err = mg.laneItems(ctx, m, lane, before); err != nil || len(page.Items) > 0 {
			return err
		}
		m.scanMu.Lock()
		defer m.scanMu.Unlock()
		prior, cs, err := mg.tracked(m, func() error { return mg.scan(ctx, m, scanPages) })
		if err != nil {
			return err
		}
		if page, err = mg.laneItems(ctx, m, lane, before); err != nil {
			return err
		}
		var records []Record
		page.ScanUpdates, records = mg.updates(ctx, m, prior, cs, page.Items, page.Records, deadline.Add(requestTime))
		page.Records = append(page.Records, records...)
		return nil
	})
	return page, err
}

// Scan continues the window's history chain within the per-request budget
// (discovery of older lanes). The end of the chain is not an error.
func (mg *Manager) Scan(ctx context.Context, owner viewer.Attacher, id string) (Coverage, error) {
	var cov Coverage
	err := mg.run(ctx, owner, id, func(m *managed) error {
		m.scanMu.Lock() // Never inside another request's tracked scan.
		defer m.scanMu.Unlock()
		err := mg.scan(ctx, m, scanPages)
		m.win.With(func(p *activity.Projection) { cov = coverageOf(p.Coverage()) })
		return err
	})
	return cov, err
}

// Origins scans older history until the window's needs are resolved, the
// chain ends or the scan budget is spent.
func (mg *Manager) Origins(ctx context.Context, owner viewer.Attacher, id string) (Coverage, error) {
	var cov Coverage
	err := mg.run(ctx, owner, id, func(m *managed) error {
		m.scanMu.Lock()
		defer m.scanMu.Unlock()
		defer m.win.With(func(p *activity.Projection) { cov = coverageOf(p.Coverage()) })
		return mg.origins(ctx, m)
	})
	return cov, err
}

func (mg *Manager) origins(ctx context.Context, m *managed) error {
	deadline := time.Now().Add(requestTime)
	for i := 0; i < scanPages && time.Now().Before(deadline); i++ {
		pending := false
		m.win.With(func(p *activity.Projection) {
			for _, n := range p.Needs() {
				pending = pending || !n.BeyondCoverage
			}
		})
		if !pending {
			return nil
		}
		if err := m.win.Scan(ctx, 1, deadline); err != nil {
			if endOfHistory(err) {
				return nil
			}
			return err
		}
	}
	return nil
}

// Lanes summarizes the lanes in the window's coverage, newest activity first.
func (mg *Manager) Lanes(ctx context.Context, owner viewer.Attacher, id string) ([]Lane, error) {
	var lanes []Lane
	err := mg.run(ctx, owner, id, func(m *managed) error {
		lanes = lanesOf(mg.collect(m).items)
		return nil
	})
	return lanes, err
}

// lanesOf summarizes the lanes of items given newest first.
func lanesOf(items []item) []Lane {
	lanes := []Lane{}
	index := map[string]int{}
	for _, it := range items {
		i, ok := index[it.Lane]
		if !ok {
			l := Lane{ID: it.Lane, Kind: laneJournal}
			switch {
			case it.Action != nil:
				_, l.Kind = laneKey(*it.Action)
			case it.Kind == ItemEffect:
				l.Kind = laneOriginNotLoaded
			}
			if l.Kind == laneConversation {
				l.IdentitySource, l.ConversationID = it.Action.Identity.IdentitySource, it.Action.Identity.ConversationID
			}
			i = len(lanes)
			index[it.Lane] = i
			lanes = append(lanes, l)
		}
		l := &lanes[i]
		l.Items++
		if t := itemTime(it.Item); t != "" {
			if l.LastTime == "" {
				l.LastTime = t
			}
			l.FirstTime = t
		}
		if it.Action != nil {
			if n := it.Action.Identity.ClientName; n != nil && len(l.ClientNames) < maxClientNames &&
				!slices.Contains(l.ClientNames, *n) {
				l.ClientNames = append(l.ClientNames, *n)
			}
		}
	}
	return lanes
}

func itemTime(it Item) string {
	var phases []*activity.PhaseView
	switch {
	case it.Action != nil:
		phases = []*activity.PhaseView{it.Action.Start, it.Action.Rejected, it.Action.Finish}
	case it.Effect != nil:
		phases = []*activity.PhaseView{it.Effect.Start, it.Effect.Finish}
	}
	for _, ph := range phases {
		if ph != nil && ph.Time != "" {
			return ph.Time
		}
	}
	return ""
}

// catchUpState fixes the changed set of the first call of a catch-up, so
// continuation calls page it without reading or issuing cursors.
type catchUpState struct {
	generation   uint64
	after        string
	head         string
	more         bool
	floorChanged bool
	seq          uint64          // Creation order, to evict the oldest set.
	keys         map[string]bool // Keys of changed items.
	records      map[activity.OccurrenceKey]bool
}

// CatchUpResult is what changed since a live cursor.
type CatchUpResult struct {
	Page
	Head string `json:"head"`
	More bool   `json:"more"` // More journal follows the head.
	// FloorChanged: the window evicted older rows meanwhile; items the
	// client holds below the floor are no longer in the window.
	FloorChanged bool `json:"floor_changed"`
	// Lanes are the current lane objects of the lanes of Items, so a client
	// learns lanes that appeared after its snapshot.
	Lanes []Lane `json:"lanes"`
}

// maxCatchUps bounds the fixed changed sets kept for continuations.
const maxCatchUps = 4

// CatchUp reports items changed since the live cursor after, with only the
// records the client lacks. generation is the client's generation: a cursor
// of another generation is refused without a rebuild. With cont it pages the
// changed set of the first call instead of reading.
func (mg *Manager) CatchUp(ctx context.Context, owner viewer.Attacher, id string, generation uint64, after string,
	cont *Continuation) (CatchUpResult, error) {
	var res CatchUpResult
	err := mg.run(ctx, owner, id, func(m *managed) error {
		if m.mode != ModeLive {
			return errBadRequest // Never a rebuild: archive windows have no live cursor.
		}
		if gen, _, _, _ := m.win.Info(); gen != generation {
			return ErrGenerationChanged
		}
		m.mu.Lock()
		state := m.catchUps[after]
		m.mu.Unlock()
		if cont == nil {
			changes, head, more, floorChanged, err := m.win.CatchUp(ctx, after, catchUpPages)
			if err != nil {
				return err
			}
			state = &catchUpState{generation: generation, after: after, head: head, more: more,
				floorChanged: floorChanged, keys: map[string]bool{}, records: map[activity.OccurrenceKey]bool{}}
			for _, k := range changes.Actions {
				state.keys[actionKey(k)] = true
			}
			for _, k := range changes.Effects {
				state.keys[effectKey(k)] = true
			}
			for _, k := range changes.Occurrences {
				state.records[k] = true
				state.keys["d:"+occurrenceID(k)] = true
				state.keys["s:"+occurrenceID(k)] = true
			}
			m.mu.Lock()
			if m.catchUps == nil {
				m.catchUps = map[string]*catchUpState{} // Concurrent catch-ups keep their own sets.
			}
			if len(m.catchUps) >= maxCatchUps {
				var oldest string
				for k, st := range m.catchUps {
					if oldest == "" || st.seq < m.catchUps[oldest].seq {
						oldest = k
					}
				}
				delete(m.catchUps, oldest)
			}
			m.catchUpSeq++
			state.seq = m.catchUpSeq
			m.catchUps[after] = state
			m.mu.Unlock()
		} else if state == nil || state.after != after || state.generation != generation {
			return ErrGenerationChanged
		}
		s := mg.collect(m)
		if s.generation != generation {
			return ErrGenerationChanged
		}
		var changed []item
		for _, it := range s.items {
			// An action whose own effect changed is changed too.
			if state.keys[it.Key] || (it.Effect != nil && state.keys[effectKey(it.Effect.Key)]) {
				changed = append(changed, it)
			}
		}
		res = CatchUpResult{Page: mg.deliver(ctx, s, changed, cont, func(k activity.OccurrenceKey) bool { return state.records[k] }),
			Head: state.head, More: state.more, FloorChanged: state.floorChanged, Lanes: []Lane{}}
		used := map[string]bool{}
		for _, it := range res.Items {
			used[it.Lane] = true
		}
		for _, l := range lanesOf(s.items) {
			if used[l.ID] {
				res.Lanes = append(res.Lanes, l)
			}
		}
		return nil
	})
	return res, err
}

var (
	// ErrUnknownOccurrence: the record endpoint only serves occurrences the
	// window has loaded.
	ErrUnknownOccurrence = errors.New("dashboard: occurrence not in this window")
	// ErrUnavailable: the record cannot be read (a row without a readable
	// reference, or a journal source that changed or is missing).
	ErrUnavailable = errors.New("dashboard: record unavailable")
)

// RecordBytes returns one complete original record known to the window.
func (mg *Manager) RecordBytes(ctx context.Context, owner viewer.Attacher, id, occurrence string) ([]byte, error) {
	var data []byte
	err := mg.run(ctx, owner, id, func(m *managed) error {
		known := false
		m.win.With(func(p *activity.Projection) {
			known = occurrence != "" && p.Knows(activity.OccurrenceKey{Ref: occurrence})
		})
		if !known {
			if strings.HasPrefix(occurrence, "w:") || strings.HasPrefix(occurrence, "p:") {
				return ErrUnavailable // A weak key names a row without a readable reference.
			}
			return ErrUnknownOccurrence
		}
		ref, err := audit.ParseRecordRef(occurrence)
		if err != nil {
			return ErrUnknownOccurrence
		}
		data, _, err = mg.reader.ReadRecord(ctx, ref)
		if errors.Is(err, audit.ErrHistoryGap) || errors.Is(err, audit.ErrInvalidCursor) {
			return ErrUnavailable
		}
		return err
	})
	return data, err
}
