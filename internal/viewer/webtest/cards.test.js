import { readFileSync } from "node:fs";
import { test } from "node:test";
import assert from "node:assert/strict";
import { card, timeline, entry, entryKey, compactable, groupTarget, chipClass, seenText, shortTime, localTime, fullTime } from "../static/cards.js";
import { textOf, find, materialize } from "../static/view.js";

// glance is the text of a view as first seen: closed notes show only their
// "i" (their text is an accessible description), Details is folded.
function glance(node) {
  if (typeof node === "string" || typeof node === "number") { return String(node); }
  const cls = (node.attrs.class || "").split(" ");
  if ((cls.includes("info") && !node.attrs.open) || cls.includes("more")) { return ""; }
  return node.children.map(glance).join("").replace(/\s+/g, " ");
}

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
  assert.match(textOf(find(v, "context")[0]), /^in \/src$/, "the working directory in the context line");
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
  assert.match(textOf(find(v, "undefined-fields")[0]), /mystery/);
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
      action: { key: { server_id: "s", request_id: "p" + n }, tool: "terminal_read", input: "recorded", state: "finished", status: "ok", start: { primary: "ref-" + n, time: "t" } } };
  });
  const groups = timeline(polls, { records });
  assert.equal(groups.length, 1);
  assert.equal(groups[0].items.length, 3);
  const collapsed = entry(groups[0], { records, expanded: new Set() });
  assert.match(glance(collapsed), /^Read output × 3 Checked: output read session_id sess-1 last /);
  assert.equal(find(collapsed, "members")[0].attrs.hidden, true, "collapsed: one row, the polls hidden but complete");
  assert.equal(find(find(collapsed, "members")[0], "compact").length, 3);
  assert.equal(find(entry(groups[0], { records, expanded: new Set([entryKey(groups[0])]) }), "compact").length, 3, "expanded: every poll, as a row");
  // A failed poll is its own card, between groups.
  polls[1].action.status = "error";
  const split = timeline(polls, { records });
  assert.deepEqual(split.map((e) => (e.item ? "card" : e.items.length)), [1, "card", 1]);
});

test("listing calls fold to one row that expands to the full card", () => {
  const { it, ctx } = item("file_search", { path: "/src", pattern: "TODO", max_results: 10 });
  it.action.finish = { primary: "ref-fin", occurrences: ["ref-fin"] };
  ctx.records.set("ref-fin", { bytes: enc(JSON.stringify({ phase: "request_finish", status: "ok", search_matches: 2, outcome: "ok" }) + "\n") });
  let v = card(it, ctx);
  assert.match(glance(find(v, "row-head")[0]), /^Search files Done path \/src pattern TODO /);
  assert.equal(find(v, "full")[0].attrs.hidden, true, "collapsed, the card is hidden but complete");
  assert.match(textOf(find(v, "full")[0]), /max_results/);
  ctx.open = new Set([it.key + ":row"]);
  v = card(it, ctx);
  assert.equal(find(v, "full")[0].attrs.hidden, undefined);
  // An undefined field keeps the full card.
  const odd = item("file_search", { path: "/src", pattern: "x", sneaky: 1 });
  assert.equal(find(card(odd.it, odd.ctx), "compact").length, 0);
});

// leaves collects every string and number value of an args object.
function leaves(o, out = []) {
  if (typeof o === "string") { out.push(o); } else if (typeof o === "number") { out.push(String(o)); }
  else if (o && typeof o === "object") { for (const v of Object.values(o)) { leaves(v, out); } }
  return out;
}

test("every supplied value is in the card; protocol fields fold into Details", () => {
  const cases = {
    terminal_start: { argv: ["go", "test"], env: { A: "1" }, cwd: "/w", operation_id: "OP-1", wait_ms: 500, extra: { evil: "NESTED" } },
    terminal_write: { session_id: "S1", operation_id: "OP-2", input: [{ text: "hi", key: "ENTER", extra: "XTRA" }, { bytes_base64: "aGk=" }] },
    file_edit: { path: "p.go", operation_id: "OP-3", edits: [{ old_text: "a", new_text: "b", flag: "F1" }], mystery: [1, "two"] },
    file_write: { path: "w.txt", content: "body", operation_id: "OP-4", mode: "0644" },
    file_receive: { server_id: "srv", file: { download_url: "https://x/y?sig=SIG", file_id: "f1" }, path: "z" },
  };
  for (const [tool, args] of Object.entries(cases)) {
    const { it, ctx } = item(tool, args);
    const v = card(it, ctx);
    const all = textOf(v);
    for (const leaf of leaves(args)) {
      assert.ok(all.includes(leaf), tool + " hides " + JSON.stringify(leaf));
    }
  }
  // Protocol fields are in Details; the substance and anything the schema
  // does not define stay in the body.
  const { it, ctx } = item("terminal_start", cases.terminal_start);
  const v = card(it, ctx);
  const body = textOf(find(v, "input")[0]);
  const more = textOf(find(v, "more")[0]);
  assert.ok(body.includes("NESTED") && body.includes("Not in this server version's schema"), "undefined fields stay visible");
  assert.ok(!body.includes("OP-1") && more.includes("OP-1") && more.includes("500"), "operation_id and wait_ms are in Details");
  const w = item("file_write", cases.file_write);
  assert.ok(textOf(find(card(w.it, w.ctx), "input")[0]).includes("0644"), "file_write has no mode field: it stays visible");
});

test("a rejected call keeps every field in the body", () => {
  const { it, ctx } = item("terminal_start", { argv: "ls -la", operation_id: "OP-R", wait_ms: "soon" });
  it.action.status = "rejected";
  it.action.reason = "schema_validation";
  const body = textOf(find(card(it, ctx), "input")[0]);
  assert.ok(body.includes("OP-R") && body.includes("soon") && body.includes("ls -la"));
});

test("rejections with a start show their reason; finish diagnostics are shown", () => {
  const { it, ctx } = item("file_read", { path: "a" });
  it.action.rejected = { primary: "ref-rej", occurrences: ["ref-rej"] };
  it.action.reason = "schema_validation";
  ctx.records.set("ref-rej", { bytes: enc(JSON.stringify({ phase: "request_rejected", status: "rejected", reason: "schema_validation", error: "invalid tool arguments" }) + "\n") });
  const v = glance(card(it, ctx));
  assert.match(v, /^Read files Not run /);
  assert.match(v, /the arguments did not match the tool's schema/);
  assert.match(textOf(card(it, ctx)), /Recorded rejection[\s\S]*invalid tool arguments/);
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
  let head = glance(card(it, { ...ctx, processes: undefined }));
  assert.ok(head.startsWith("Run command Last seen running seen " + shortTime("2026-11-10T10:00:01Z") + " "), head);
  assert.match(head, /^Run command Last seen running /, "only what the launch saw, with its time");
  assert.equal(find(card(it, { ...ctx, processes: undefined }), "outcome-chip")[0].attrs.class, "outcome-chip seen", "a historic observation is not a success");
  // A later read of the same process recorded its exit.
  const read = { kind: "action", key: "a:s/r2", lane: "L", order: { live: true, rank: 2 },
    action: { key: { server_id: "s", request_id: "r2" }, tool: "terminal_read", input: "recorded", state: "finished", status: "ok",
      duration: { state: "measured", milliseconds: 2 }, process: { server_id: "s", session_id: "sess-1" },
      finish: { primary: "ref-read", occurrences: ["ref-read"], time: "2026-11-10T10:05:00Z" } } };
  ctx.records.set("ref-read", { bytes: enc(JSON.stringify({ phase: "request_finish", status: "ok", process: { state: "exited", exit: { code: 2 } } }) + "\n") });
  const entries = timeline([it, read], ctx);
  head = glance(entry(entries[0], ctx));
  assert.ok(head.startsWith("Run command Exited · 2 exit seen " + shortTime("2026-11-10T10:05:00Z") + " "), head);
  assert.match(glance(entry(entries[1], ctx)), /^Read output Checked: exited · 2 /);
  assert.match(find(entry(entries[1], ctx), "outcome-chip")[0].attrs.class, /warn/, "a poll that saw a bad exit is a warning");
  assert.doesNotMatch(head, /started in|15 ms/, "the launch time is in Details");
  assert.match(textOf(entry(entries[0], ctx)), /The process launched in 15 ms/);
});

test("results that matter are inline: version and size of a written file", () => {
  const { it, ctx } = item("file_write", { path: "/a", content: "x", expected_version: "sha256:old" });
  it.action.status = "applied";
  it.effect = { tool: "file_write", state: "finished", status: "applied", duration: { state: "measured", milliseconds: 4 },
    finish: { primary: "ref-op", occurrences: ["ref-op"] } };
  ctx.records.set("ref-op", { bytes: enc(JSON.stringify({ phase: "operation_finish", status: "applied",
    receipt: { operation_id: "o", status: "applied", data: { path: "/a", version: "sha256:new", bytes: 1 } } }) + "\n") });
  const v = glance(card(it, ctx));
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
  const v = glance(card(it, ctx));
  assert.match(v, /^Download file Failed /);
  assert.match(v, /HTTP 404/);
  assert.doesNotMatch(v, /Saved to|0 bytes/);
  const w = item("file_write", { path: "/a", content: "x", expected_version: "" });
  assert.match(glance(card(w.it, w.ctx)), /^Create file /);
});

test("a command that exited non-zero is a warning, not a success; recorded statuses stay visible", () => {
  const { it, ctx } = item("terminal_start", { argv: ["false"], env: {} });
  it.action.status = "applied";
  it.action.finish = { primary: "ref-fin", occurrences: ["ref-fin"], time: "2026-11-10T10:00:01Z" };
  it.effect = { tool: "terminal_start", state: "finished", status: "applied", duration: { state: "measured", milliseconds: 3 } };
  ctx.records.set("ref-fin", { bytes: enc(JSON.stringify({ phase: "request_finish", status: "applied", process: { state: "exited", exit: { code: 1 } } }) + "\n") });
  const v = card(it, ctx);
  const chip = find(v, "outcome-chip")[0];
  assert.equal(textOf(chip), "Exited · 1");
  assert.match(chip.attrs.class, /warn/);
  assert.match(textOf(v), /Recorded status: request applied, action applied\./);
});

test("keys the schema does not define are marked at any depth", () => {
  const r = item("file_receive", { path: "/x", file: { download_url: "http://h/f", file_id: "f", sneaky: "S" }, operation_id: "o", server_id: "s" });
  r.it.action.status = "running";
  let body = find(card(r.it, r.ctx), "input")[0];
  assert.match(glance(find(body, "undefined-key")[0]), /^sneaky \(not in schema\)$/);
  const e = item("file_edit", { path: "/a", edits: [{ old_text: "a", new_text: "b", zzz: "Z" }], operation_id: "o", server_id: "s" });
  body = find(card(e.it, e.ctx), "input")[0];
  assert.match(textOf(find(body, "undefined-fields")[0]), /Not in this server version's schema\s*zzz\s*Z/);
  const w = item("terminal_write", { session_id: "S", input: [{ text: "hi", key: "ENTER", zz: 1 }], operation_id: "o", server_id: "s" });
  body = find(card(w.it, w.ctx), "input")[0];
  assert.match(textOf(find(body, "other")[0]), /Other fields of this input\s*key\s*ENTER/, "a schema field stays under the plain label");
  assert.match(textOf(find(body, "undefined-fields")[0]), /zz/);
  const s = item("terminal_start", { argv: ["a"], env: { ANY_NAME: "1" }, operation_id: "o", server_id: "s" });
  assert.equal(find(card(s.it, s.ctx), "undefined-key").length, 0, "env keys are values, not fields");
});

test("a zero runtime limit folds only when fields are split", () => {
  const ok = item("terminal_start", { argv: ["a"], env: {}, max_runtime_ms: 0, operation_id: "o", server_id: "s" });
  assert.ok(!textOf(find(card(ok.it, ok.ctx), "input")[0]).includes("max_runtime_ms"));
  assert.ok(textOf(find(card(ok.it, ok.ctx), "more")[0]).includes("max_runtime_ms"));
  const limited = item("terminal_start", { argv: ["a"], env: {}, max_runtime_ms: 5000, operation_id: "o", server_id: "s" });
  assert.ok(textOf(find(card(limited.it, limited.ctx), "input")[0]).includes("5000"));
  const bad = item("terminal_start", { argv: ["a"], env: {}, max_runtime_ms: 0, operation_id: "o", server_id: "s" });
  bad.it.action.status = "rejected";
  assert.ok(textOf(find(card(bad.it, bad.ctx), "input")[0]).includes("max_runtime_ms"), "a rejected call keeps everything inline");
});

test("a shell script shows once, with every token around it; the full line is in Details", () => {
  const argv = ["/bin/sh", "-e", "-c", "echo \"$1\"; uname -a", "_", "rm -rf /tmp/x"];
  const { it, ctx } = item("terminal_start", { argv, env: { LANG: "C" }, operation_id: "o", server_id: "s" });
  const v = card(it, ctx);
  const body = textOf(find(v, "input")[0]);
  assert.match(body, /via LANG=C \/bin\/sh -e -c/);
  assert.equal(textOf(find(v, "script")[0]), "echo \"$1\"; uname -a");
  assert.match(body, /Arguments after the script \(\$0 first\)\s*_\s*'rm -rf \/tmp\/x'/);
  assert.equal(body.split("uname -a").length - 1, 1, "the script once at a glance");
  assert.match(textOf(find(v, "more")[0]), /Full command line\s*LANG=C \/bin\/sh -e -c 'echo "\$1"; uname -a' _ 'rm -rf \/tmp\/x'/);
  assert.equal(find(v, "copy").length, 1);
  // A line that cannot be copied keeps its exact tokens inline.
  const odd = item("terminal_start", { argv: ["/bin/sh", "-c", "echo‮x"], env: {} });
  assert.match(textOf(find(card(odd.it, odd.ctx), "input")[0]), /Not copyable/);
});

test("caveats sit behind an info control and stay in the card", () => {
  const { it, ctx } = item("file_read", { files: [{ path: "/a" }] });
  const v = card(it, ctx);
  const note = find(v, "info")[0];
  assert.equal(note.tag, "details");
  assert.match(textOf(note), /The audit records the request, not the file contents returned\./);
  const summary = note.children[0];
  assert.equal(summary.attrs["aria-describedby"], note.children[1].attrs.id, "the note's text describes its control");
  assert.match(summary.attrs["aria-describedby"], /^tm-note-/);
});

test("an empty script says so", () => {
  const { it, ctx } = item("terminal_start", { argv: ["sh", "-c", ""], env: {} });
  assert.match(textOf(find(card(it, ctx), "input")[0]), /\(empty script\)/);
});

test("only calls that need no attention fold to a row", () => {
  const base = () => {
    const r = item("file_list", { path: "/src", limit: 5 });
    r.it.action.finish = { primary: "ref-fin", occurrences: ["ref-fin"] };
    r.ctx.records.set("ref-fin", { bytes: enc(JSON.stringify({ phase: "request_finish", status: "ok" }) + "\n") });
    return r;
  };
  assert.ok(compactable(base().it, base().ctx), "a plain listing folds");
  const cases = {
    rejected: (it) => { it.action.rejected = { primary: "ref-start" }; it.action.status = "rejected"; },
    failed: (it) => { it.action.status = "error"; },
    unknown: (it) => { it.action.state = "unknown"; it.action.status = ""; },
    stopped: (it) => { it.action.status = "canceled"; },
    legacy: (it) => { it.action.legacy = true; },
    "not recorded": (it) => { it.action.input = "not_recorded"; },
    "possibly unrecorded": (it) => { it.action.possibly_unrecorded = true; },
    conflict: (it) => { it.action.start.conflict = true; },
    "pending identity": (it) => { it.action.start.occurrences = ["p:1"]; },
    "unusable metadata": (it) => { it.action.identity = { metadataInvalid: true }; },
  };
  for (const [what, change] of Object.entries(cases)) {
    const { it, ctx } = base();
    change(it);
    assert.equal(compactable(it, ctx), null, what);
  }
  const lossy = item("file_list", null, { raw: enc('{"args":{"path":"a"},"args":{"path":"b"}}\n') });
  assert.equal(compactable(lossy.it, lossy.ctx), null, "lossy");
  const nested = item("file_read", { files: [{ path: "/a", extra: 1 }] });
  assert.equal(compactable(nested.it, nested.ctx), null, "an undefined nested field");
  const notLoaded = base();
  notLoaded.ctx.records.set("ref-start", { bytes: null, size: 10, deferred: true });
  assert.equal(compactable(notLoaded.it, notLoaded.ctx), null, "input not loaded");
});

test("a row shows every main field, in schema order, each value apart", () => {
  const spoof = "x · path /tmp/harmless" + " ".repeat(200);
  const { it, ctx } = item("file_search", { pattern: spoof, path: "/Users/owner/.ssh", max_results: 3 });
  const head = find(card(it, ctx), "row-head")[0];
  const keys = find(head, "row-key").map(textOf);
  assert.deepEqual(keys, ["path", "pattern"], "schema order, protocol fields left out");
  assert.match(textOf(head), /path \/Users\/owner\/\.ssh pattern x · path \/tmp\/harmless/);
  assert.ok(!textOf(head).includes("max_results"));
});

test("unread counts a failed poll on its own, like the timeline shows it", async () => {
  const { attentionFor } = await import("../static/attention.js");
  const records = new Map();
  const store = { lanes: new Map([["L", { id: "L", kind: "conversation", identitySource: "openai/session", conversationId: "c" }]]), records };
  const poll = (n, status) => {
    records.set("ref-" + n, { bytes: startRecord("terminal_read", { session_id: "s1" }), size: 1 });
    return { kind: "action", key: "a:s/p" + n, lane: "L", order: { live: true, rank: n },
      action: { key: { server_id: "s", request_id: "p" + n }, tool: "terminal_read", input: "recorded", state: "finished", status,
        start: { primary: "ref-" + n, occurrences: ["ref-" + n], time: "t" } } };
  };
  const att = attentionFor(store, { now: () => 0 });
  const items = [poll(1, "ok"), poll(2, "ok"), poll(3, "error"), poll(4, "error")];
  att.observe(items.map((item) => ({ type: "added", item })), { live: true }, { autoSwitch: false, current: "", deferred: false, laneKind: () => "conversation" });
  assert.equal(att.count("c\u0000openai/session\u0000c"), 3, "two good polls as one, each failure apart");
  assert.equal(groupTarget(items[2], { records }), null);
});

test("each outcome kind has its own look; only a confirmed success is ok", () => {
  const cls = (kind, warn = false) => chipClass({ kind, warn });
  assert.deepEqual([cls("done"), cls("exited_ok")], ["ok", "ok"]);
  assert.deepEqual([cls("failed"), cls("not_run")], ["bad", "bad"]);
  assert.deepEqual([cls("exited_bad"), cls("signaled"), cls("partial"), cls("stopped"), cls("checked", true)], ["warn", "warn", "warn", "warn", "warn"]);
  assert.equal(cls("unknown"), "unknown");
  assert.equal(cls("running"), "live");
  assert.equal(cls("observed"), "seen");
  assert.deepEqual([cls("started"), cls("exited_unknown"), cls("checked"), cls("repeat"), cls("constructor")], ["neutral", "neutral", "neutral", "neutral", "neutral"]);
});

test("an exit seen on another day says which day", () => {
  const t = "2026-11-11T09:02:10Z";
  assert.equal(seenText(t, "2026-11-11T09:00:00Z"), shortTime(t));
  const later = seenText("2026-11-13T09:02:10Z", "2026-11-11T09:00:00Z");
  assert.ok(later.endsWith(" " + shortTime("2026-11-13T09:02:10Z")) && later.length > shortTime(t).length + 1, later);
  assert.equal(seenText("", "2026-11-11T09:00:00Z"), "");
});

test("every outcome look has a glyph, with a fallback for older browsers", () => {
  const css = readFileSync(new URL("../static/app.css", import.meta.url), "utf8");
  for (const cls of ["ok", "bad", "warn", "unknown", "live", "seen", "neutral"]) {
    assert.match(css, new RegExp("\\.outcome-chip\\." + cls + "::before \\{ content: \"[^\"]+\"; content: \"[^\"]+\" / \"\"; \\}"), cls);
  }
});

test("the header ends with the time; the command block carries Copy; notes sit by their labels", () => {
  const { it, ctx } = item("terminal_start", { argv: ["/bin/sh", "-c", "make check"], env: { GOFLAGS: "-count=1", TOKEN: "s3cret" }, cwd: "/src/a very/long path" });
  const v = card(it, ctx);
  const head = find(v, "card-head")[0];
  const last = head.children[head.children.length - 1];
  assert.equal(last.attrs.class, "when", "the time is the header's right-hand end");
  assert.equal(find(last, "dur").length, 1, "an ordinary call keeps its duration by the time");
  // Every env override and the full cwd are in the context line, in full.
  assert.equal(textOf(find(v, "context")[0]).replace(/\s+/g, " "), "via GOFLAGS=-count=1 TOKEN=s3cret /bin/sh -c in /src/a very/long path");
  const blk = find(v, "block")[0];
  assert.equal(find(find(blk, "corner")[0], "copy").length, 1, "Copy in the block's corner");
  assert.equal(textOf(find(blk, "copy")[0]), "Copy command line", "a visible label");
  assert.match(textOf(find(v, "more")[0]), /Full command line[\s\S]*GOFLAGS=-count=1 TOKEN=s3cret \/bin\/sh -c 'make check'/);
  assert.match(textOf(find(v, "more")[0]), /Recorded at .*recorded 2026-11-10T10:00:00Z \(writer clock\)/, "the full time is reachable on touch");
  // A note is never a line of its own: it sits in the header, a block's
  // corner or a labelled line, beside something else.
  const homes = (node, parent, out = []) => {
    if (typeof node !== "object") { return out; }
    if ((node.attrs.class || "").split(" ").includes("info")) { out.push(parent); }
    for (const c of node.children) { homes(c, node, out); }
    return out;
  };
  for (const v2 of [v, card(item("terminal_start", { argv: ["ls"], env: {} }).it, item("terminal_start", { argv: ["ls"], env: {} }).ctx)]) {
    for (const p of homes(v2, null)) {
      assert.ok(["card-head", "corner", "kv"].some((c) => (p.attrs.class || "").split(" ").includes(c)), p.attrs.class);
      assert.ok(p.children.filter((c) => typeof c === "object" && !(c.attrs.class || "").includes("info")).length > 0, "a note shares its line");
    }
  }
  const e = item("file_edit", { path: "/a", edits: [{ old_text: "a", new_text: "b" }] });
  const kv = find(find(card(e.it, e.ctx), "edit")[0], "kv")[0];
  assert.equal(find(kv, "info").length, 1, "the edit note sits with its file");
});

test("a cwd that is not a string still shows as recorded; a launch has no header duration", () => {
  const { it, ctx } = item("terminal_start", { argv: ["ls"], env: {}, cwd: 42 });
  it.action.status = "rejected";
  it.action.reason = "schema_validation";
  const v = card(it, ctx);
  assert.match(textOf(find(v, "context")[0]), /^in 42$/);
  const s = item("terminal_start", { argv: ["ls"], env: {} });
  s.it.action.status = "applied";
  s.it.effect = { tool: "terminal_start", state: "finished", status: "applied", duration: { state: "measured", milliseconds: 7 } };
  assert.equal(find(find(card(s.it, s.ctx), "card-head")[0], "dur").length, 0);
});

test("a compact row's button holds no note; its card's header does", () => {
  const { it, ctx } = item("terminal_read", { session_id: "S" });
  const v = card(it, ctx);
  assert.match(v.attrs.class, /compact/);
  const button = find(v, "row-head")[0];
  assert.equal(find(button, "info").length, 0, "no details inside a button");
  assert.equal(find(find(v, "full")[0], "info").length, 1);
});

test("each branch keeps its layout: Windows, a line that cannot be copied, an empty script", () => {
  let r = item("terminal_start", { argv: ["cmd.exe", "/c", "dir"], env: { A: "1" }, cwd: "C:\\src" });
  r.ctx.serverOS = "windows";
  let v = card(r.it, r.ctx);
  assert.match(textOf(find(v, "context")[0]), /^in C:\\src$/);
  r = item("terminal_start", { argv: ["echo", "a\u202eb"], env: {} });
  v = card(r.it, r.ctx);
  const corner = find(find(v, "block")[0], "corner")[0];
  assert.equal(find(corner, "copy").length, 0, "no Copy for a line that does not parse back");
  assert.equal(find(corner, "info").length, 1);
  r = item("terminal_start", { argv: ["/bin/sh", "-c", ""], env: {} });
  v = card(r.it, r.ctx);
  assert.match(textOf(find(v, "block")[0]), /\(empty script\)/);
});

test("in flight there is no header duration; an effect-only card and a group keep the time at the right", () => {
  const { it, ctx } = item("file_receive", { path: "/x", file: { download_url: "http://h/f", file_id: "f" } });
  it.action.status = "running";
  it.effect = { tool: "file_receive", state: "in_flight", status: "", start: { primary: "ref-op", occurrences: ["ref-op"], time: "2026-11-10T10:00:00Z" } };
  const head = find(card(it, ctx), "card-head")[0];
  assert.equal(find(head, "dur").length, 0);
  assert.equal(find(head, "when").length, 1);
  const eo = { kind: "effect", key: "e:s/o", lane: "L", order: { live: true, rank: 1 },
    effect: { key: { server_id: "s", operation_id: "o" }, tool: "file_write", state: "finished", status: "applied", start: { primary: "ref-op", occurrences: ["ref-op"], time: "2026-11-10T10:00:00Z" } } };
  const eh = find(card(eo, { records: new Map() }), "card-head")[0];
  assert.equal(eh.children[eh.children.length - 1].attrs.class, "when");
  assert.equal(textOf(localTime("not a time")), "not a time", "an invalid time shows as recorded");
  assert.equal(fullTime("not a time"), "not a time (not a valid time)");
});
