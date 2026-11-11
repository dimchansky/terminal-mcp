import { test } from "node:test";
import assert from "node:assert/strict";
import { describeStatus, sessionText, pendingNotice, reasonText, loadErrorText } from "../static/status.js";

test("each offline reason says what it means and what happens next", () => {
  assert.match(reasonText("capacity", 409), /does not fit within one view's capacity/);
  assert.match(reasonText("capacity", 429), /busy/);
  assert.match(reasonText("window_limit", 409), /close another tab or the archive view/);
  assert.match(reasonText("network", 0), /cannot be reached/);
  assert.match(reasonText("rebuilding", 503), /rebuilding/);
  assert.match(reasonText("something_new", 500), /answered something_new; retrying/);
  assert.equal(reasonText("", 0), "");
});

const texts = (t) => t.warnings.map((w) => w.text).join(" | ");

test("the reason shows only while offline or reconnecting", () => {
  const off = describeStatus({ connection: "offline", reason: "capacity", reasonStatus: 409 });
  assert.deepEqual([off.connection, off.connectionKind], ["Offline", "bad"]);
  assert.match(texts(off), /capacity/);
  assert.equal(off.warnings[0].kind, "bad");
  const live = describeStatus({ connection: "live", reason: "capacity", reasonStatus: 409 });
  assert.deepEqual([live.connection, live.connectionKind], ["Connected", "ok"]);
  assert.match(live.connectionTitle, /says nothing about whether any process is running/);
  assert.deepEqual(live.warnings, []);
});

test("every connection state has a label and a look", () => {
  const want = { connecting: ["Connecting…", "warn"], live: ["Connected", "ok"], reconnecting: ["Reconnecting…", "warn"], rebuilding: ["Rebuilding the view…", "warn"],
    offline: ["Offline", "bad"], audit_closed: ["Audit unavailable: no live activity", "bad"], stopped: ["Server stopped", "bad"], constructor: ["constructor", "warn"] };
  for (const [state, [label, kind]] of Object.entries(want)) {
    const t = describeStatus({ connection: state });
    assert.deepEqual([t.connection, t.connectionKind], [label, kind], state);
  }
});

test("health, index and notes are separate warnings, in order", () => {
  const t = describeStatus({ connection: "live", health: { state: "degraded" }, index: { status: "catching_up", in_flight_outside: [1, 2] },
    possibleUnseen: true, floorChanged: true }, ["Renew failed.", ""]);
  assert.deepEqual(t.warnings, [
    { kind: "bad", text: "Audit degraded" },
    { kind: "warn", text: "In-flight index catching up" },
    { kind: "warn", text: "2 running effect(s) outside the loaded history" },
    { kind: "warn", text: "Some activity may have been missed while the view was rebuilt." },
    { kind: "info", text: "Older activity was unloaded to stay within memory; use Earlier to see it again." },
    { kind: "warn", text: "Renew failed." },
  ]);
});

test("terminal window states and reconnects explain themselves", () => {
  assert.match(texts(describeStatus({ connection: "offline", reason: "capacity", reasonStatus: 409 })), /one view's capacity; opening a new view/);
  assert.match(texts(describeStatus({ connection: "offline", reason: "failed", reasonStatus: 409 })), /kept failing/);
  assert.match(texts(describeStatus({ connection: "reconnecting", reason: "network" })), /cannot be reached/);
  assert.deepEqual(describeStatus({ connection: "reconnecting", reason: "aborted" }).warnings, []);
});

test("the session end is short, the exact time in its title", () => {
  const now = new Date(2026, 10, 11, 9, 0);
  const today = sessionText(new Date(2026, 10, 11, 18, 30).toISOString(), now);
  assert.match(today.text, /^Session ends by \D*18\D30\D*$/);
  assert.match(today.title, /at the latest/);
  const later = sessionText(new Date(2026, 10, 12, 1, 30).toISOString(), now).text;
  assert.ok(later.includes(new Date(2026, 10, 12).toLocaleDateString(undefined, { month: "short", day: "numeric" })), later);
  assert.deepEqual(sessionText("nope", now), { text: "", title: "" });
});

test("load errors say what to do", () => {
  assert.match(loadErrorText("archive_reloaded"), /rebuilt on the server and reloaded; try again/);
  assert.match(loadErrorText("use_archive"), /Open older history/);
  assert.match(loadErrorText("no_floor"), /No older history/);
  assert.equal(loadErrorText("bad_request"), "The server answered bad_request.");
});

test("a held-back switch is offered in the header, never in the archive view", () => {
  const names = new Map([["c\u0000s\u0000x", "Release work"]]);
  assert.deepEqual(pendingNotice("c\u0000s\u0000x", { names }), { lane: "c\u0000s\u0000x", name: "Release work", canShow: true, why: "Auto-switch waits while you read." });
  assert.deepEqual(pendingNotice("journal", {}), { lane: "journal", name: "another chat", canShow: true, why: "Auto-switch waits while you read." });
  assert.equal(pendingNotice("journal", { filtering: true }).why, "Auto-switch waits while filters are active.");
  assert.equal(pendingNotice("c\u0000s\u0000x", { archive: true, names }).canShow, false, "said, but not offered, in the archive view");
  assert.equal(pendingNotice(null, { names }), null);
});
