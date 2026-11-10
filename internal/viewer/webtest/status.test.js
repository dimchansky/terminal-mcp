import { test } from "node:test";
import assert from "node:assert/strict";
import { describeStatus, reasonText, loadErrorText } from "../static/status.js";

test("each offline reason says what it means and what happens next", () => {
  assert.match(reasonText("capacity", 409), /does not fit within one view's capacity/);
  assert.match(reasonText("capacity", 429), /busy/);
  assert.match(reasonText("window_limit", 409), /close another tab or the archive view/);
  assert.match(reasonText("network", 0), /cannot be reached/);
  assert.match(reasonText("rebuilding", 503), /rebuilding/);
  assert.match(reasonText("something_new", 500), /answered something_new; retrying/);
  assert.equal(reasonText("", 0), "");
});

test("the reason shows only while offline or reconnecting", () => {
  const off = describeStatus({ connection: "offline", reason: "capacity", reasonStatus: 409 });
  assert.equal(off.connection, "Offline");
  assert.match(off.notice, /capacity/);
  const live = describeStatus({ connection: "live", reason: "capacity", reasonStatus: 409 });
  assert.equal(live.connection, "Live");
  assert.equal(live.notice, "");
});

test("health, index and notices are joined in order", () => {
  const t = describeStatus({ connection: "live", health: { state: "degraded" }, index: { status: "catching_up", in_flight_outside: [1, 2] },
    possibleUnseen: true }, ["Renew failed.", ""]);
  assert.equal(t.health, "Audit degraded · In-flight index catching up · 2 running effect(s) outside the loaded history");
  assert.equal(t.notice, "Some activity may have been missed while the view was rebuilt. Renew failed.");
});

test("terminal window states and reconnects explain themselves", () => {
  assert.match(describeStatus({ connection: "offline", reason: "capacity", reasonStatus: 409 }).notice, /one view's capacity; opening a new view/);
  assert.match(describeStatus({ connection: "offline", reason: "failed", reasonStatus: 409 }).notice, /kept failing/);
  assert.match(describeStatus({ connection: "reconnecting", reason: "network" }).notice, /cannot be reached/);
  assert.equal(describeStatus({ connection: "reconnecting", reason: "aborted" }).notice, "");
});

test("load errors say what to do", () => {
  assert.match(loadErrorText("archive_reloaded"), /rebuilt on the server and reloaded; try again/);
  assert.match(loadErrorText("use_archive"), /Open older history/);
  assert.match(loadErrorText("no_floor"), /No older history/);
  assert.equal(loadErrorText("bad_request"), "The server answered bad_request.");
});
