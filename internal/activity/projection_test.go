package activity

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

const current = "srv-now"

// window feeds a projection with ranks assigned per chain, as the adapter
// does. Records are JSON strings; occurrence refs are arbitrary names.
type window struct {
	t          *testing.T
	p          *Projection
	hist, live int64
}

// startedProjection is a test harness: its newest history page H1 was empty
// (as at a gap) and the join has started, so the live chain may deliver and
// an older history page (from cursor "h1") is open. It is not yet joined.
func startedProjection(t *testing.T) *Projection {
	t.Helper()
	p := NewProjection(current)
	for _, err := range []error{p.BeginHistoryPage(""), p.EndHistoryPage("h1"), p.StartJoin(), p.BeginHistoryPage("h1")} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func newWindow(t *testing.T) *window {
	return &window{t: t, p: startedProjection(t)}
}

func (w *window) history(ref, raw string) Change {
	w.t.Helper()
	w.hist--
	return w.apply(Record{Delivery: Delivery{OccurrenceKey{Ref: ref}, ChainHistory, w.hist}, Raw: rawOrNil(raw)})
}

func (w *window) onLive(ref, raw string) Change {
	w.t.Helper()
	w.live++
	return w.apply(Record{Delivery: Delivery{OccurrenceKey{Ref: ref}, ChainLive, w.live}, Raw: rawOrNil(raw)})
}

func (w *window) apply(rec Record) Change {
	w.t.Helper()
	change, err := w.p.Apply(rec)
	if err != nil {
		w.t.Fatal(err)
	}
	return change
}

func rawOrNil(raw string) []byte {
	if raw == "" {
		return nil
	}
	return []byte(raw + "\n")
}

func (w *window) action(server, request string) ActionView {
	w.t.Helper()
	v, ok := w.p.Action(RequestKey{server, request})
	if !ok {
		w.t.Fatalf("action %s/%s missing", server, request)
	}
	return v
}

func (w *window) effect(server, op string) EffectView {
	w.t.Helper()
	v, ok := w.p.Effect(EffectKey{server, op})
	if !ok {
		w.t.Fatalf("effect %s/%s missing", server, op)
	}
	return v
}

func meta(session string) string {
	return fmt.Sprintf(`,"call_meta":{"openai/session":%q,"io.modelcontextprotocol/clientInfo":{"name":"openai-mcp"}}`, session)
}

func reqStart(server, req, tool, op, extra string) string {
	return fmt.Sprintf(`{"schema_version":2,"time":"t","server_id":%q,"request_id":%q,"tool":%q,"phase":"request_start","operation_id":%q,"args":{"operation_id":%q}%s}`,
		server, req, tool, op, op, extra)
}

func reqFinish(server, req, tool, extra string) string {
	return fmt.Sprintf(`{"schema_version":2,"time":"t","server_id":%q,"request_id":%q,"tool":%q,"phase":"request_finish"%s}`,
		server, req, tool, extra)
}

func opStart(server, req, tool, op string) string {
	return fmt.Sprintf(`{"schema_version":2,"time":"t","server_id":%q,"request_id":%q,"tool":%q,"phase":"operation_start","operation_id":%q}`,
		server, req, tool, op)
}

func opFinish(server, req, tool, op, extra string) string {
	return fmt.Sprintf(`{"schema_version":2,"time":"t","server_id":%q,"request_id":%q,"tool":%q,"phase":"operation_finish","operation_id":%q,"status":"applied"%s}`,
		server, req, tool, op, extra)
}

func TestRedeliveryChangesNothing(t *testing.T) {
	w := newWindow(t)
	start := reqStart(current, "r1", "file_write", "op1", meta("chat-a"))
	if c := w.history("a", start); c.Kind != ChangeNewAction {
		t.Fatalf("first delivery: %+v", c)
	}
	before := w.p.Actions()
	for i, c := range []Change{w.history("a", start), w.onLive("a", start), w.onLive("a", start)} {
		if c.Kind != ChangeRedelivery {
			t.Fatalf("redelivery %d: %+v", i, c)
		}
	}
	if after := w.p.Actions(); len(after) != 1 || after[0].Start.Primary != before[0].Start.Primary {
		t.Fatalf("redelivery changed the projection: %+v", after)
	}
}

func TestIdenticalCopiesAreTwoOccurrencesOneAction(t *testing.T) {
	w := newWindow(t)
	records := []string{
		reqStart(current, "r1", "file_write", "op1", meta("chat-a")),
		opStart(current, "r1", "file_write", "op1"),
		opFinish(current, "r1", "file_write", "op1", `,"duration_ms":3`),
		reqFinish(current, "r1", "file_write", `,"operation_id":"op1","status":"applied","duration_ms":5`),
	}
	for i, raw := range records { // Archive .1
		w.history(fmt.Sprintf("x@1-%d", i), raw)
	}
	for i, raw := range records { // Byte-identical copy in archive .2
		if c := w.history(fmt.Sprintf("x@2-%d", i), raw); c.Kind != ChangeDuplicate {
			t.Fatalf("copy %d: %+v", i, c)
		}
	}
	actions, effects := w.p.Actions(), w.p.Effects()
	if len(actions) != 1 || len(effects) != 1 {
		t.Fatalf("copies must not multiply actions (%d) or effects (%d)", len(actions), len(effects))
	}
	a := actions[0]
	if len(a.Start.Occurrences) != 2 || a.Start.Conflict || a.Effect == nil || a.Effect.Kind != LinkOwn {
		t.Fatalf("action %+v start %+v", a, a.Start)
	}
	if len(effects[0].Start.Occurrences) != 2 || effects[0].State != EffectFinished {
		t.Fatalf("effect %+v", effects[0])
	}
}

func TestConflictingCopiesDoNotStealIdentity(t *testing.T) {
	original := reqStart(current, "r1", "file_write", "op1", meta("chat-a"))
	forged := reqStart(current, "r1", "file_write", "op1", meta("chat-b"))
	for _, forgedFirst := range []bool{false, true} {
		w := newWindow(t)
		// The original is older in journal order. History delivers newest
		// first, so a forged copy delivered first on history is newer.
		if forgedFirst {
			w.history("copy", forged)
			if c := w.history("orig", original); c.Kind != ChangeConflict {
				t.Fatalf("conflict not reported: %+v", c)
			}
		} else {
			w.history("orig", original)
			w.onLive("copy", forged)
		}
		a := w.action(current, "r1")
		if !a.Start.Conflict || a.Start.Primary.Ref != "orig" {
			t.Fatalf("forgedFirst=%v: start %+v", forgedFirst, a.Start)
		}
		if key, _ := a.Identity.Key(); key.ConversationID != "chat-a" {
			t.Fatalf("forgedFirst=%v: identity taken from the later copy: %v", forgedFirst, key)
		}
	}
}

func TestLateOriginIsReattributedNotNewActivity(t *testing.T) {
	w := newWindow(t)
	if c := w.onLive("f", reqFinish(current, "r1", "terminal_read", `,"status":"ok"`)); c.Kind != ChangeNewAction {
		t.Fatalf("finish without start: %+v", c)
	}
	a := w.action(current, "r1")
	if a.Attribution != OriginNotLoaded || a.Input != InputUnknown || a.Duration.State != DurationUnknown {
		t.Fatalf("before backfill: %+v", a)
	}
	if needs := w.p.Needs(); len(needs) != 1 || *needs[0].Request != (RequestKey{current, "r1"}) {
		t.Fatalf("needs %+v", needs)
	}
	if c := w.history("s", reqStart(current, "r1", "terminal_read", "", meta("chat-a"))); c.Kind != ChangeReattributed {
		t.Fatalf("backfilled origin: %+v", c)
	}
	a = w.action(current, "r1")
	if a.Attribution != Attributed || a.Input != InputRecorded || a.Duration.State != DurationSubMillisecond ||
		a.Order != w.p.order(OccurrenceKey{Ref: "s"}) || len(w.p.Needs()) != 0 {
		t.Fatalf("after backfill: %+v needs %+v", a, w.p.Needs())
	}
}

func TestEffectOriginsAndNeeds(t *testing.T) {
	w := newWindow(t)
	// operation_finish only: its request_id still names the origin.
	if c := w.onLive("of", opFinish(current, "r1", "file_receive", "op1", "")); c.Kind != ChangeNewEffect {
		t.Fatalf("effect without origin: %+v", c)
	}
	e := w.effect(current, "op1")
	if e.Origin == nil || *e.Origin != (RequestKey{current, "r1"}) || e.State != EffectFinished {
		t.Fatalf("effect %+v", e)
	}
	// A replay whose effect is outside the window.
	w.onLive("rs", reqStart(current, "r2", "file_write", "op9", meta("chat-b")))
	w.onLive("rf", reqFinish(current, "r2", "file_write", `,"operation_id":"op9","replayed":true,"status":"applied"`))
	want := []Need{{Request: &RequestKey{current, "r1"}}, {Effect: &EffectKey{current, "op9"}}}
	if got := w.p.Needs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("needs %+v, want %+v", got, want)
	}
	if a := w.action(current, "r2"); a.Effect == nil || a.Effect.Kind != LinkReplay || a.Effect.Loaded {
		t.Fatalf("replay link %+v", a.Effect)
	}
}

func TestCrossChatReplayAndLinks(t *testing.T) {
	w := newWindow(t)
	// Chat A launches a process and writes a file.
	w.onLive("a1", reqStart(current, "a-start", "terminal_start", "op-start", meta("chat-a")))
	w.onLive("a2", opStart(current, "a-start", "terminal_start", "op-start"))
	w.onLive("a3", opFinish(current, "a-start", "terminal_start", "op-start", `,"session_id":"proc-1"`))
	w.onLive("a4", reqFinish(current, "a-start", "terminal_start", `,"operation_id":"op-start","status":"applied","session_id":"proc-1"`))
	// Chat B replays A's operation_id: no second effect.
	w.onLive("b1", reqStart(current, "b-replay", "terminal_start", "op-start", meta("chat-b")))
	w.onLive("b2", reqFinish(current, "b-replay", "terminal_start", `,"operation_id":"op-start","replayed":true,"status":"applied","session_id":"proc-1"`))
	// Chat B reads A's process.
	w.onLive("b3", reqStart(current, "b-read", "terminal_read", "", meta("chat-b")))
	w.onLive("b4", reqFinish(current, "b-read", "terminal_read", `,"status":"ok","session_id":"proc-1"`))
	// Chat C reuses the ID with different arguments: rejected before admission.
	w.onLive("c1", reqStart(current, "c-conflict", "terminal_start", "op-start", meta("chat-c")))
	w.onLive("c2", reqFinish(current, "c-conflict", "terminal_start", `,"operation_id":"op-start","status":"error"`))
	// Chat B observes a download it did not start.
	w.onLive("b5", reqStart(current, "b-observe", "file_receive_result", "op-dl", meta("chat-b")))
	w.onLive("b6", reqFinish(current, "b-observe", "file_receive_result", `,"operation_id":"op-dl","status":"running"`))

	if effects := w.p.Effects(); len(effects) != 1 || *effects[0].Origin != (RequestKey{current, "a-start"}) {
		t.Fatalf("effects %+v", effects)
	}
	if a := w.action(current, "a-start"); a.Effect.Kind != LinkOwn || a.ProcessCreator == nil ||
		*a.ProcessCreator != (RequestKey{current, "a-start"}) {
		t.Fatalf("origin action %+v", a)
	}
	replay := w.action(current, "b-replay")
	if key, _ := replay.Identity.Key(); key.ConversationID != "chat-b" ||
		replay.Effect == nil || replay.Effect.Kind != LinkReplay || !replay.Effect.Loaded ||
		replay.ProcessCreator == nil || *replay.ProcessCreator != (RequestKey{current, "a-start"}) {
		t.Fatalf("replay %+v", replay)
	}
	read := w.action(current, "b-read")
	if key, _ := read.Identity.Key(); key.ConversationID != "chat-b" || read.Effect != nil ||
		read.ProcessCreator == nil || *read.ProcessCreator != (RequestKey{current, "a-start"}) {
		t.Fatalf("cross-chat read %+v", read)
	}
	if c := w.action(current, "c-conflict"); c.Effect != nil {
		t.Fatalf("raw conflicting operation_id was linked: %+v", c.Effect)
	}
	if o := w.action(current, "b-observe"); o.Effect == nil || o.Effect.Kind != LinkObserve || o.Effect.Loaded {
		t.Fatalf("observe link %+v", o.Effect)
	}
}

func TestRequestAndEffectStates(t *testing.T) {
	w := newWindow(t)
	// Canceled request on the current server; its effect is still running.
	w.onLive("1", reqStart(current, "r1", "file_receive", "op1", ""))
	w.onLive("2", opStart(current, "r1", "file_receive", "op1"))
	w.onLive("3", reqFinish(current, "r1", "file_receive", `,"operation_id":"op1","status":"canceled","outcome":"unknown"`))
	// An unfinished request of an earlier server instance.
	w.history("4", reqStart("srv-old", "r2", "file_write", "op2", ""))
	w.history("5", opStart("srv-old", "r2", "file_write", "op2"))
	// An unfinished read-only request on the current server.
	w.onLive("6", reqStart(current, "r3", "terminal_list", "", ""))

	if a := w.action(current, "r1"); a.State != RequestFinished {
		t.Fatalf("canceled request %+v", a)
	}
	if e := w.effect(current, "op1"); e.State != EffectInFlight {
		t.Fatalf("running effect %+v", e)
	}
	if a, e := w.action("srv-old", "r2"), w.effect("srv-old", "op2"); a.State != RequestUnknown || e.State != EffectUnknown {
		t.Fatalf("prior server: %v %v", a.State, e.State)
	}
	if a := w.action(current, "r3"); a.State != RequestOpen {
		t.Fatalf("open request %+v", a)
	}
	if a := w.action(current, "r1"); a.Status != "canceled" || a.Outcome != "unknown" {
		t.Fatalf("canceled request result %+v", a)
	}
	// Hours later the independent download finishes.
	if c := w.onLive("7", opFinish(current, "r1", "file_receive", "op1", `,"duration_ms":7200000`)); c.Kind != ChangeTransition {
		t.Fatalf("late effect finish: %+v", c)
	}
	if e := w.effect(current, "op1"); e.State != EffectFinished || e.Status != "applied" || e.Duration != (Duration{DurationMeasured, 7200000}) {
		t.Fatalf("late finish: %+v", e)
	}
}

func TestRejectedAndLegacyRequests(t *testing.T) {
	w := newWindow(t)
	w.onLive("rej", `{"schema_version":2,"time":"t","server_id":"srv-now","request_id":"r1","tool":"file_write","phase":"request_rejected","status":"rejected","reason":"audit_record_too_large","operation_id":"op1"}`)
	a := w.action(current, "r1")
	if a.Attribution != Unattributed || a.Input != InputNotRecorded || a.Reason != "audit_record_too_large" || a.Status != "rejected" ||
		a.State != RequestFinished || a.Effect != nil || len(w.p.Needs()) != 0 {
		t.Fatalf("rejected %+v", a)
	}
	// A rejection that kept its call_meta belongs to the caller's chat.
	w.onLive("rej2", `{"schema_version":2,"time":"t","server_id":"srv-now","request_id":"r2","tool":"file_write","phase":"request_rejected","status":"rejected","reason":"audit_record_too_large","call_meta":{"openai/session":"chat-x"}}`)
	named := w.action(current, "r2")
	if c, ok := named.Identity.Key(); named.Attribution != Attributed || !ok || c.ConversationID != "chat-x" || named.Input != InputNotRecorded {
		t.Fatalf("rejected with call_meta %+v", named)
	}

	w.history("ls", `{"time":"t","request_id":"old","tool":"terminal_start","phase":"start","args":{"argv":["sh"]}}`)
	w.history("lf", `{"time":"t","request_id":"old","tool":"terminal_start","phase":"finish","status":"applied"}`)
	w.history("lr", `{"time":"t","request_id":"old2","tool":"file_write","phase":"rejected","reason":"schema_validation"}`)
	legacy := w.action("", "old")
	if !legacy.Legacy || legacy.Input != InputLegacy || legacy.Attribution != Unattributed || legacy.State != RequestFinished || legacy.Duration.State != DurationUnknown {
		t.Fatalf("legacy %+v", legacy)
	}
	if r := w.action("", "old2"); r.Input != InputNotRecorded || r.Reason != "schema_validation" {
		t.Fatalf("legacy rejected %+v", r)
	}
}

func TestUncorrelatableRowsAreDiagnostics(t *testing.T) {
	w := newWindow(t)
	cases := []Record{
		{Delivery: Delivery{OccurrenceKey{Ref: "d1"}, ChainHistory, -1}, Problem: "corrupt JSONL"},
		{Delivery: Delivery{OccurrenceKey{Ref: "d2"}, ChainHistory, -2}, Raw: []byte(`{"phase":`)},
		{Delivery: Delivery{OccurrenceKey{Ref: "d3"}, ChainHistory, -3}, Raw: []byte(`{"schema_version":2,"phase":"future"}`)},
		{Delivery: Delivery{OccurrenceKey{Ref: "d4"}, ChainHistory, -4}, Raw: []byte(`{"schema_version":2,"phase":"request_start"}`)},
		{Delivery: Delivery{OccurrenceKey{Ref: "d5"}, ChainHistory, -5}, Raw: []byte(`{"schema_version":2,"phase":"operation_start","request_id":"r"}`)},
		{Delivery: Delivery{OccurrenceKey{Ref: "d6"}, ChainHistory, -6}, Raw: []byte(`{"phase":"request_start","request_id":"a","request_id":"b"}`)},
	}
	for _, rec := range cases {
		if c := w.apply(rec); c.Kind != ChangeDiagnostic {
			t.Fatalf("%s: %+v", rec.Delivery.Key.Ref, c)
		}
	}
	diags := w.p.Diagnostics()
	if len(diags) != len(cases) || len(w.p.Actions()) != 0 || len(w.p.Effects()) != 0 {
		t.Fatalf("diagnostics %+v", diags)
	}
	if diags[0].Key.Ref != "d6" || diags[5].Key.Ref != "d1" { // Journal order: newest delivered first.
		t.Fatalf("diagnostics out of journal order: %+v", diags)
	}
	w.hist = -int64(len(cases))
	w.history("srv", `{"schema_version":2,"time":"t","server_id":"srv-now","tool":"server","phase":"server_start","status":"ok"}`)
	if starts := w.p.ServerStarts(); len(starts) != 1 || starts[0].ServerID != current {
		t.Fatalf("server starts %+v", starts)
	}
}

func TestApplyRejectsAdapterErrors(t *testing.T) {
	w := newWindow(t)
	w.history("a", reqStart(current, "r1", "terminal_list", "", ""))
	_, err := w.p.Apply(Record{Delivery: Delivery{OccurrenceKey{Ref: "b"}, ChainHistory, -1}, Raw: []byte(reqStart(current, "r2", "terminal_list", "", ""))})
	if !errors.Is(err, ErrInvalidRank) {
		t.Fatalf("reused rank accepted: %v", err)
	}
	if len(w.p.Actions()) != 1 {
		t.Fatal("a rejected delivery must not change the projection")
	}
}

// TestProjectionIgnoresChainInterleaving applies the same per-chain
// deliveries in different interleavings and compares every view.
func TestProjectionIgnoresChainInterleaving(t *testing.T) {
	type item struct{ ref, raw string }
	history := []item{ // Newest first.
		{"h4", reqFinish(current, "r1", "file_write", `,"operation_id":"op1","status":"applied"`)},
		{"h3", opFinish(current, "r1", "file_write", "op1", "")},
		{"h2", opStart(current, "r1", "file_write", "op1")},
		{"h1", reqStart(current, "r1", "file_write", "op1", meta("chat-a"))},
		{"h0", `{"schema_version":2,"time":"t","server_id":"srv-now","tool":"server","phase":"server_start"}`},
	}
	live := []item{
		{"h3", opFinish(current, "r1", "file_write", "op1", "")}, // Overlap.
		{"h4", reqFinish(current, "r1", "file_write", `,"operation_id":"op1","status":"applied"`)},
		{"l1", reqStart(current, "r2", "file_write", "op1", meta("chat-b"))},
		{"l2", reqFinish(current, "r2", "file_write", `,"operation_id":"op1","replayed":true,"status":"applied"`)},
		{"l3", reqFinish(current, "r0", "terminal_read", `,"status":"ok"`)},
	}
	var h, l []Record
	for i, it := range history {
		h = append(h, Record{Delivery: Delivery{OccurrenceKey{Ref: it.ref}, ChainHistory, int64(-1 - i)}, Raw: []byte(it.raw)})
	}
	for i, it := range live {
		l = append(l, Record{Delivery: Delivery{OccurrenceKey{Ref: it.ref}, ChainLive, int64(1 + i)}, Raw: []byte(it.raw)})
	}
	type snapshot struct {
		Actions []ActionView
		Effects []EffectView
		Needs   []Need
		Servers []ServerStartView
	}
	var reference *snapshot
	for split := 0; split <= len(h); split++ {
		for _, liveFirst := range []bool{false, true} {
			p := startedProjection(t)
			order := slices.Concat(h[:split], l, h[split:])
			if liveFirst {
				order = slices.Concat(l, h)
			}
			for _, rec := range order {
				if _, err := p.Apply(rec); err != nil {
					t.Fatal(err)
				}
			}
			got := &snapshot{p.Actions(), p.Effects(), p.Needs(), p.ServerStarts()}
			if reference == nil {
				reference = got
			} else if !reflect.DeepEqual(got, reference) {
				t.Fatalf("split=%d liveFirst=%v: views differ\n got %+v\nwant %+v", split, liveFirst, got, reference)
			}
		}
	}
	if len(reference.Actions) != 3 || reference.Actions[0].Key.RequestID != "r1" || reference.Actions[2].Key.RequestID != "r0" {
		t.Fatalf("unexpected actions %+v", reference.Actions)
	}
}

func TestRecordContract(t *testing.T) {
	p := startedProjection(t)
	for _, rec := range []Record{
		{Delivery: Delivery{OccurrenceKey{Ref: "a"}, ChainHistory, -1}},
		{Delivery: Delivery{OccurrenceKey{Ref: "a"}, ChainHistory, -1}, Raw: []byte(`{}`), Problem: "x"},
	} {
		if _, err := p.Apply(rec); !errors.Is(err, ErrRecordContract) {
			t.Fatalf("%+v accepted: %v", rec, err)
		}
	}
	// A rejected record must not consume the occurrence: the hydrated row
	// is still decoded on its first valid delivery.
	c, err := p.Apply(Record{Delivery: Delivery{OccurrenceKey{Ref: "a"}, ChainHistory, -1}, Raw: []byte(reqStart(current, "r1", "file_write", "op1", ""))})
	if err != nil || c.Kind != ChangeNewAction {
		t.Fatalf("hydrated row: %+v %v", c, err)
	}
}

func TestSchemaRejectedRequestKeepsItsResult(t *testing.T) {
	w := newWindow(t)
	w.onLive("s", `{"schema_version":2,"time":"t","server_id":"srv-now","request_id":"r1","tool":"file_edit","phase":"request_start","operation_id":"op1","args":{"operation_id":"op1","unknown":1}`+meta("chat-a")+`}`)
	w.onLive("f", reqFinish(current, "r1", "file_edit", `,"operation_id":"op1","status":"rejected","reason":"schema_validation","duration_ms":1`))
	a := w.action(current, "r1")
	if a.Input != InputRecorded || a.Status != "rejected" || a.Reason != "schema_validation" || a.Effect != nil {
		t.Fatalf("schema rejection %+v", a)
	}
}

func TestProcessComesFromTheEffectWhenTheRequestFailed(t *testing.T) {
	w := newWindow(t)
	// The launching request was canceled: its request_finish has no session.
	w.onLive("1", reqStart(current, "a", "terminal_start", "op-a", meta("chat-a")))
	w.onLive("2", opStart(current, "a", "terminal_start", "op-a"))
	w.onLive("3", reqFinish(current, "a", "terminal_start", `,"operation_id":"op-a","status":"canceled","outcome":"unknown"`))
	w.onLive("4", opFinish(current, "a", "terminal_start", "op-a", `,"session_id":"proc-1"`))
	w.onLive("5", reqStart(current, "b", "terminal_read", "", meta("chat-b")))
	w.onLive("6", reqFinish(current, "b", "terminal_read", `,"status":"ok","session_id":"proc-1"`))

	launch := w.action(current, "a")
	if launch.Process == nil || launch.Process.SessionID != "proc-1" || launch.ProcessEffect == nil ||
		*launch.ProcessEffect != (EffectKey{current, "op-a"}) {
		t.Fatalf("launch %+v", launch)
	}
	read := w.action(current, "b")
	if key, _ := read.Identity.Key(); key.ConversationID != "chat-b" || read.ProcessCreator == nil ||
		*read.ProcessCreator != (RequestKey{current, "a"}) {
		t.Fatalf("read from another chat %+v", read)
	}
}

func TestDownloadReplayFromAnotherChat(t *testing.T) {
	w := newWindow(t)
	w.onLive("1", reqStart(current, "a", "file_receive", "dl", meta("chat-a")))
	w.onLive("2", opStart(current, "a", "file_receive", "dl"))
	w.onLive("3", reqFinish(current, "a", "file_receive", `,"operation_id":"dl","status":"running"`))
	w.onLive("4", reqStart(current, "b", "file_receive", "dl", meta("chat-b")))
	w.onLive("5", reqFinish(current, "b", "file_receive", `,"operation_id":"dl","status":"running","replayed":true`))
	b := w.action(current, "b")
	if b.Effect == nil || b.Effect.Kind != LinkReplay || !b.Effect.Loaded || !b.Replayed || len(w.p.Effects()) != 1 {
		t.Fatalf("replay %+v", b)
	}
	if a := w.action(current, "a"); a.Effect == nil || a.Effect.Kind != LinkOwn {
		t.Fatalf("origin %+v", a.Effect)
	}
}

func TestConflictBeyondTheCopyLimitIsKept(t *testing.T) {
	w := newWindow(t)
	start := reqStart(current, "r1", "terminal_list", "", meta("chat-a"))
	for i := 0; i < maxPhaseOccurrences+2; i++ {
		w.history(fmt.Sprintf("copy-%d", i), start)
	}
	if c := w.history("forged", reqStart(current, "r1", "terminal_list", "", meta("chat-x"))); c.Kind != ChangeConflict {
		t.Fatalf("late conflicting copy: %+v", c)
	}
	a := w.action(current, "r1")
	if !a.Start.Conflict || a.Start.More != 3 || len(a.Start.Occurrences) != maxPhaseOccurrences ||
		len(w.p.facts) != maxPhaseOccurrences {
		t.Fatalf("start %+v facts %d", a.Start, len(w.p.facts))
	}
	// The primary is the earliest tracked copy, not an older untracked one.
	if key, _ := a.Identity.Key(); key.ConversationID != "chat-a" ||
		a.Start.Primary.Ref != fmt.Sprintf("copy-%d", maxPhaseOccurrences-1) {
		t.Fatalf("primary %+v identity %v", a.Start.Primary, key)
	}
}

func TestRequestStateIsSurfaced(t *testing.T) {
	w := newWindow(t)
	w.onLive("s", reqStart(current, "r", "terminal_list", "", `,"request_state":"opaque-state"`))
	if a := w.action(current, "r"); a.RequestState != "opaque-state" {
		t.Fatalf("request state %q", a.RequestState)
	}
}

func TestRareLinkShapes(t *testing.T) {
	t.Run("process creator from request_finish only", func(t *testing.T) {
		w := newWindow(t)
		w.history("1", reqFinish(current, "a", "terminal_start", `,"operation_id":"op-a","status":"applied","session_id":"p"`))
		w.onLive("2", reqFinish(current, "b", "terminal_write", `,"status":"applied","session_id":"p"`))
		b := w.action(current, "b")
		if b.ProcessEffect != nil || b.ProcessCreator == nil || *b.ProcessCreator != (RequestKey{current, "a"}) {
			t.Fatalf("%+v", b)
		}
	})
	t.Run("unknown process creator", func(t *testing.T) {
		w := newWindow(t)
		w.onLive("1", reqFinish(current, "b", "terminal_read", `,"status":"ok","session_id":"p"`))
		b := w.action(current, "b")
		if b.Process == nil || b.ProcessEffect != nil || b.ProcessCreator != nil {
			t.Fatalf("%+v", b)
		}
	})
	t.Run("effect without request id", func(t *testing.T) {
		w := newWindow(t)
		c := w.onLive("1", `{"schema_version":2,"time":"t","server_id":"srv-now","tool":"terminal_start","phase":"operation_finish","operation_id":"op","status":"applied","session_id":"p"}`)
		if c.Kind != ChangeNewEffect || c.Request != nil {
			t.Fatalf("%+v", c)
		}
		w.onLive("2", reqFinish(current, "b", "terminal_read", `,"status":"ok","session_id":"p"`))
		b := w.action(current, "b")
		if b.ProcessEffect == nil || *b.ProcessEffect != (EffectKey{current, "op"}) || b.ProcessCreator != nil {
			t.Fatalf("%+v", b)
		}
		if e := w.effect(current, "op"); e.Origin != nil {
			t.Fatalf("%+v", e)
		}
	})
	t.Run("effect records disagree on origin", func(t *testing.T) {
		w := newWindow(t)
		w.onLive("1", opStart(current, "a", "file_write", "op"))
		w.onLive("2", opFinish(current, "b", "file_write", "op", ""))
		if e := w.effect(current, "op"); !e.OriginConflict || e.Origin == nil || *e.Origin != (RequestKey{current, "a"}) {
			t.Fatalf("%+v", e)
		}
	})
}
