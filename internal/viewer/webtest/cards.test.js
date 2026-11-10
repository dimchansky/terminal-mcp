import { test } from "node:test";
import assert from "node:assert/strict";
import { card, timeline, entry, entryKey } from "../static/cards.js";
import { textOf, find, materialize } from "../static/view.js";

const enc = (s) => new TextEncoder().encode(s);
const startRecord = (tool, args, extra = {}) => enc(JSON.stringify({ schema_version: 2, time: "2026-11-10T10:00:00Z", server_id: "s",
  request_id: "r", tool, phase: "request_start", args, ...extra }) + "\n");

function item(tool, args, { input = "recorded", extra = {}, raw } = {}) {
  const records = new Map([["ref-start", { bytes: raw || startRecord(tool, args, extra), size: 1 }]]);
  const it = { kind: "action", key: "a:s/r", lane: "L", order: { live: true, rank: 1 },
    action: { key: { server_id: "s", request_id: "r" }, tool, input, state: "finished", status: "ok", duration: { state: "measured", milliseconds: 340 },
      start: { primary: "ref-start", occurrences: ["ref-start"], time: "2026-11-10T10:00:00Z" } } };
  return { it, ctx: { records, serverOS: "darwin" } };
}

test("terminal_start: the acceptance example is one readable command", () => {
  const { it, ctx } = item("terminal_start", { argv: ["go", "test", "-race", "./..."], env: { GOFLAGS: "-count=1" }, cwd: "/src" });
  const v = card(it, ctx);
  assert.equal(textOf(find(v, "command")[0]), "GOFLAGS=-count=1 go test -race ./...");
  assert.equal(find(v, "copy").length, 1, "copy command; the record's copy is in the closed originals");
  ctx.open = new Set(["a:s/r:originals"]);
  assert.equal(find(card(it, ctx), "copy").length, 2, "copy command and copy record");
  assert.match(textOf(v), /Working directory\s*\/src/);
  assert.doesNotMatch(textOf(find(v, "input")[0]), /argv\[|env\.GOFLAGS/, "no field names as the main view");
});

test("terminal_start: unrepresentable input shows exact tokens; Windows gets no POSIX notation", () => {
  let { it, ctx } = item("terminal_start", { argv: ["echo", "a‮b"], env: {} });
  let v = card(it, ctx);
  assert.match(textOf(v), /Not copyable/);
  assert.equal(find(v, "esc").length >= 1, true);
  ({ it, ctx } = item("terminal_start", { argv: ["cmd.exe", "/c", "dir"], env: { A: "1" } }));
  ctx.serverOS = "windows";
  v = card(it, ctx);
  assert.equal(find(v, "command").length, 0);
  assert.match(textOf(v), /Windows; exact tokens/);
});

test("terminal_start: an interpreter script is shown as a script", () => {
  const { it, ctx } = item("terminal_start", { argv: ["/bin/zsh", "-lc", "cd web && npm test"] });
  assert.equal(textOf(find(card(it, ctx), "script")[0]), "cd web && npm test");
});

test("file_edit: every replacement is a red/green diff", () => {
  const { it, ctx } = item("file_edit", { path: "main.go", edits: [
    { old_text: "timeout := 5 * time.Second\n", new_text: "timeout := 10 * time.Second\n" },
    { old_text: "a\nb\n", new_text: "a\nB\nc\n" }] });
  const v = card(it, ctx);
  const diffs = find(v, "diff");
  assert.equal(diffs.length, 2);
  assert.deepEqual(find(diffs[0], "del").map(textOf), ["- timeout := 5 * time.Second"]);
  assert.deepEqual(find(diffs[0], "ins").map(textOf), ["+ timeout := 10 * time.Second"]);
  assert.deepEqual(find(diffs[1], "ins").map(textOf), ["+ B", "+ c"]);
  assert.doesNotMatch(textOf(find(v, "edit")[0]), /edits\[0\]|old_text/);
});

test("file_write shows the whole content; unknown fields are listed", () => {
  const content = "package main\n\nfunc main() {}\n";
  const { it, ctx } = item("file_write", { path: "m.go", content, mystery: { nested: [1, 2] } });
  const v = card(it, ctx);
  assert.equal(textOf(find(v, "content")[0]), content);
  assert.match(textOf(find(v, "other")[0]), /mystery/);
});

test("lossy, legacy and rejected records are labelled honestly", () => {
  let { it, ctx } = item("file_read", null, { raw: enc('{"args":{"path":"a"},"args":{"path":"b"}}\n') });
  assert.match(textOf(card(it, ctx)), /Shown as recorded text: duplicate key "args"/);
  ({ it, ctx } = item("file_write", { path: "x" }, { input: "legacy" }));
  assert.match(textOf(card(it, ctx)), /Legacy record: the input may be incomplete/);
  ({ it, ctx } = item("file_write", {}));
  it.action.input = "not_recorded"; it.action.reason = "audit_record_too_large"; delete it.action.start;
  assert.match(textOf(card(it, ctx)), /Not run [\s\S]*the input was too large to record in the audit[\s\S]*Input not recorded · tool not executed/);
});

test("a large record offers loading it in full; hostile text stays inert", () => {
  let { it, ctx } = item("file_read", {});
  ctx.records = new Map([["ref-start", { bytes: null, size: 3 << 20, deferred: true }]]);
  it.summary = { text: "<script>alert(1)</script>", truncated: true };
  const v = card(it, ctx);
  assert.match(textOf(v), /Large record \(3,145,728 bytes\)/);
  const doc = { createElement: (t) => ({ t, attrs: {}, children: [], setAttribute(n, x) { this.attrs[n] = x; }, appendChild(c) { this.children.push(c); } }),
    createElementNS: (n, t) => ({ t, attrs: {}, children: [], setAttribute() {}, appendChild() {} }), createTextNode: (text) => ({ text }) };
  const el = materialize(doc, v);
  assert.ok(JSON.stringify(el).includes("<script>alert(1)</script>"), "kept as a text node");
});

test("consecutive polls of one target collapse into a group", () => {
  const records = new Map();
  const polls = [1, 2, 3].map((n) => {
    records.set("ref-" + n, { bytes: startRecord("terminal_read", { session_id: "sess-1" }), size: 1 });
    return { kind: "action", key: "a:s/p" + n, lane: "L", order: { live: true, rank: n },
      action: { key: { server_id: "s", request_id: "p" + n }, tool: "terminal_read", state: "finished", start: { primary: "ref-" + n, time: "t" } } };
  });
  const groups = timeline(polls, { records });
  assert.equal(groups.length, 1);
  assert.equal(groups[0].items.length, 3);
  assert.match(textOf(entry(groups[0], { records, expanded: new Set() })), /terminal_read × 3 \(polling\)/);
});

// leaves collects every string and number value of an args object.
function leaves(o, out = []) {
  if (typeof o === "string") { out.push(o); } else if (typeof o === "number") { out.push(String(o)); }
  else if (o && typeof o === "object") { for (const v of Object.values(o)) { leaves(v, out); } }
  return out;
}

test("every supplied value is visible in the card body", () => {
  const cases = {
    terminal_start: { argv: ["go", "test"], env: { A: "1" }, cwd: "/w", operation_id: "OP-1", wait_ms: 500, extra: { evil: "NESTED" } },
    terminal_write: { session_id: "S1", operation_id: "OP-2", input: [{ text: "hi", key: "ENTER", extra: "XTRA" }, { bytes_base64: "aGk=" }] },
    file_edit: { path: "p.go", operation_id: "OP-3", edits: [{ old_text: "a", new_text: "b", flag: "F1" }], mystery: [1, "two"] },
    file_write: { path: "w.txt", content: "body", operation_id: "OP-4", mode: "0644" },
    file_receive: { server_id: "srv", file: { download_url: "https://x/y?sig=SIG", file_id: "f1" }, path: "z" },
  };
  for (const [tool, args] of Object.entries(cases)) {
    const { it, ctx } = item(tool, args);
    const body = textOf(find(card(it, ctx), "input")[0]);
    for (const leaf of leaves(args)) {
      assert.ok(body.includes(leaf), tool + " hides " + JSON.stringify(leaf));
    }
  }
});

test("rejections with a start show their reason; finish diagnostics are shown", () => {
  const { it, ctx } = item("file_read", { path: "a" });
  it.action.rejected = { primary: "ref-rej", occurrences: ["ref-rej"] };
  it.action.reason = "schema_validation";
  ctx.records.set("ref-rej", { bytes: enc(JSON.stringify({ phase: "request_rejected", status: "rejected", reason: "schema_validation", error: "invalid tool arguments" }) + "\n") });
  const v = textOf(card(it, ctx));
  assert.match(v, /^Read files Not run /);
  assert.match(v, /the arguments did not match the tool's schema/);
  assert.match(v, /Recorded rejection[\s\S]*invalid tool arguments/);
  const f = item("file_search", { pattern: "x" });
  f.it.action.finish = { primary: "ref-fin", occurrences: ["ref-fin"] };
  f.ctx.records.set("ref-fin", { bytes: enc(JSON.stringify({ phase: "request_finish", status: "ok", search_matches: 3, truncated: true }) + "\n") });
  assert.match(textOf(card(f.it, f.ctx)), /result to the model was truncated; this record is complete\.[\s\S]*search_matches\s*3/);
  const g = item("file_read", { path: "x" });
  g.it.action.finish = { primary: "ref-err", occurrences: ["ref-err"] };
  g.ctx.records.set("ref-err", { bytes: enc(JSON.stringify({ phase: "request_finish", status: "error", error: "e".repeat(2048) + "..." }) + "\n") });
  assert.match(textOf(card(g.it, g.ctx)), /likely cut by the server at 2048 bytes/);
  g.ctx.records.set("ref-err", { bytes: enc(JSON.stringify({ phase: "request_finish", status: "error", error: "e".repeat(5000) + "..." }) + "\n") });
  assert.doesNotMatch(textOf(card(g.it, g.ctx)), /cut by the server/, "a longer organic error is not called cut");
});

test("the audit's missing outputs are stated", () => {
  for (const tool of ["file_read", "terminal_read", "terminal_start"]) {
    const { it, ctx } = item(tool, tool === "terminal_start" ? { argv: ["ls"] } : { path: "x", session_id: "s" });
    assert.match(textOf(card(it, ctx)), /The audit records the (request|launch), not the/, tool);
  }
});

test("timeline entry keys are unique when polls of one target alternate with other calls", () => {
  const records = new Map();
  const mk = (n, tool, args) => {
    records.set("ref-" + n, { bytes: startRecord(tool, args), size: 1 });
    return { kind: "action", key: "a:s/" + n, lane: "L", order: { live: true, rank: n },
      action: { key: { server_id: "s", request_id: String(n) }, tool, state: "finished", start: { primary: "ref-" + n, time: "t" } } };
  };
  const items = [mk(1, "terminal_write", { session_id: "S" }), mk(2, "terminal_read", { session_id: "S" }), mk(3, "terminal_read", { session_id: "S" }),
    mk(4, "terminal_write", { session_id: "S" }), mk(5, "terminal_read", { session_id: "S" }), mk(6, "terminal_read", { session_id: "S" })];
  const keys = timeline(items, { records }).map(entryKey);
  assert.equal(new Set(keys).size, keys.length, keys.join());
});

test("a command's card shows the exit a later call saw, with when", () => {
  const { it, ctx } = item("terminal_start", { argv: ["make", "check"], env: {} });
  it.action.status = "applied";
  it.action.process = { server_id: "s", session_id: "sess-1" };
  it.effect = { tool: "terminal_start", state: "finished", status: "applied", duration: { state: "measured", milliseconds: 15 } };
  it.action.finish = { primary: "ref-fin", occurrences: ["ref-fin"], time: "2026-11-10T10:00:01Z" };
  ctx.records.set("ref-fin", { bytes: enc(JSON.stringify({ phase: "request_finish", status: "applied", process: { state: "running" } }) + "\n") });
  let head = textOf(card(it, { ...ctx, processes: undefined }));
  assert.match(head, /^Run command Started, running \(seen /, "only what the launch saw, with its time");
  // A later read of the same process recorded its exit.
  const read = { kind: "action", key: "a:s/r2", lane: "L", order: { live: true, rank: 2 },
    action: { key: { server_id: "s", request_id: "r2" }, tool: "terminal_read", input: "recorded", state: "finished", status: "ok",
      duration: { state: "measured", milliseconds: 2 }, process: { server_id: "s", session_id: "sess-1" },
      finish: { primary: "ref-read", occurrences: ["ref-read"], time: "2026-11-10T10:05:00Z" } } };
  ctx.records.set("ref-read", { bytes: enc(JSON.stringify({ phase: "request_finish", status: "ok", process: { state: "exited", exit: { code: 2 } } }) + "\n") });
  const entries = timeline([it, read], ctx);
  head = textOf(entry(entries[0], ctx));
  assert.match(head, /^Run command Started, exited 2 \(seen /);
  assert.match(textOf(entry(entries[1], ctx)), /^Read output Checked: exited 2 /);
  assert.match(head, /started in 15 ms/);
});

test("results that matter are inline: version and size of a written file", () => {
  const { it, ctx } = item("file_write", { path: "/a", content: "x", expected_version: "sha256:old" });
  it.action.status = "applied";
  it.effect = { tool: "file_write", state: "finished", status: "applied", duration: { state: "measured", milliseconds: 4 },
    finish: { primary: "ref-op", occurrences: ["ref-op"] } };
  ctx.records.set("ref-op", { bytes: enc(JSON.stringify({ phase: "operation_finish", status: "applied",
    receipt: { operation_id: "o", status: "applied", data: { path: "/a", version: "sha256:new", bytes: 1 } } }) + "\n") });
  const v = textOf(card(it, ctx));
  assert.match(v, /^Replace file Done /);
  assert.match(v, /version sha256:new · 1 byte/);
});

test("a failed action shows no result values; an empty expected_version creates", () => {
  const { it, ctx } = item("file_receive", { path: "/x", file: { download_url: "http://h/f", file_id: "f" } });
  it.action.status = "running";
  it.effect = { tool: "file_receive", state: "finished", status: "failed", duration: { state: "measured", milliseconds: 4 },
    finish: { primary: "ref-op", occurrences: ["ref-op"] } };
  ctx.records.set("ref-op", { bytes: enc(JSON.stringify({ phase: "operation_finish", status: "failed", error: "HTTP 404",
    receipt: { operation_id: "o", status: "failed", error: "HTTP 404", data: { path: "/x", bytes: 0 } } }) + "\n") });
  const v = textOf(card(it, ctx));
  assert.match(v, /^Download file Failed /);
  assert.match(v, /HTTP 404/);
  assert.doesNotMatch(v, /Saved to|0 bytes/);
  const w = item("file_write", { path: "/a", content: "x", expected_version: "" });
  assert.match(textOf(card(w.it, w.ctx)), /^Create file /);
});

test("a command that exited non-zero is a warning, not a success; recorded statuses stay visible", () => {
  const { it, ctx } = item("terminal_start", { argv: ["false"], env: {} });
  it.action.status = "applied";
  it.action.finish = { primary: "ref-fin", occurrences: ["ref-fin"], time: "2026-11-10T10:00:01Z" };
  it.effect = { tool: "terminal_start", state: "finished", status: "applied", duration: { state: "measured", milliseconds: 3 } };
  ctx.records.set("ref-fin", { bytes: enc(JSON.stringify({ phase: "request_finish", status: "applied", process: { state: "exited", exit: { code: 1 } } }) + "\n") });
  const v = card(it, ctx);
  const chip = find(v, "outcome-chip")[0];
  assert.match(textOf(chip), /^Started, exited 1/);
  assert.match(chip.attrs.class, /warn/);
  assert.match(textOf(v), /Recorded status: request applied, action applied\./);
});
