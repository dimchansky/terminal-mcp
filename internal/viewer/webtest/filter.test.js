import { test } from "node:test";
import assert from "node:assert/strict";
import { matches, textMatch, needsAttention, alwaysShown, active, apply } from "../static/filter.js";
import { timeline } from "../static/cards.js";

const enc = (s) => new TextEncoder().encode(s);
const startRecord = (tool, args) => enc(JSON.stringify({ schema_version: 2, time: "2026-11-10T10:00:00Z", server_id: "s",
  request_id: "r", tool, phase: "request_start", args }) + "\n");

let n = 0;
function item(tool, args, { records = new Map(), status = "ok", raw, summary } = {}) {
  n++;
  const id = "ref-" + n;
  if (args !== undefined || raw) { records.set(id, { bytes: raw || startRecord(tool, args), size: 1 }); }
  const it = { kind: "action", key: "a:s/" + n, lane: "L", order: { live: true, rank: n }, summary,
    action: { key: { server_id: "s", request_id: String(n) }, tool, input: "recorded", state: "finished", status,
      start: { primary: id, occurrences: [id], time: "2026-11-10T10:00:00Z" } } };
  return { it, ctx: { records, processes: new Map() } };
}

const q = (query) => ({ query, tools: new Set(), attention: false });

test("a search looks at every key and value, and at a command as written", () => {
  const { it, ctx } = item("terminal_start", { argv: ["rm", "-rf", "/tmp/build"], env: { DEPLOY_TOKEN: "s3cret" }, cwd: "/src", extra: { nested: [42] } });
  assert.equal(matches(it, q("rm -rf /tmp"), ctx).where, "main", "across argv tokens");
  assert.equal(matches(it, q("deploy_token"), ctx).where, "main", "an env name, case-insensitive");
  assert.equal(matches(it, q("s3cret"), ctx).where, "main");
  assert.equal(matches(it, q("nested"), ctx).where, "main", "an unknown field's key");
  assert.equal(matches(it, q("42"), ctx).where, "main", "a number");
  assert.equal(matches(it, q("Run command"), ctx).where, "main", "the verb");
  assert.equal(matches(it, q("terminal_start"), ctx).where, "main", "the tool");
  assert.equal(matches(it, q("nothing-like-this"), ctx).match, false);
});

test("a match only in a folded field says it is in Details", () => {
  const { it, ctx } = item("file_write", { path: "/a", content: "x", operation_id: "op-unique-7" });
  assert.equal(matches(it, q("op-unique-7"), ctx).where, "details");
});

test("control characters match as recorded, not as their escapes", () => {
  const { it, ctx } = item("terminal_write", { session_id: "S", input: [{ text: "echo\u001b[31m" }] });
  assert.equal(matches(it, q("\u001b[31m"), ctx).match, true);
  assert.equal(matches(it, q("\\e[31m"), ctx).match, false);
});

test("a lossy record is searched in its loaded text; an unloaded one only in the server's hint", () => {
  const lossy = item("file_write", undefined, { raw: enc('{"tool":"file_write","args":{"path":"/x","content":"dup"},"args":{"content":"other"}}\n') });
  assert.equal(textMatch(lossy.it, "other", lossy.ctx).where, "raw");
  const bad = item("file_write", undefined, { raw: new Uint8Array([...enc('{"tool":"file_write","args":{"content":"caf'), 0xff, ...enc(' latte"}}\n')]) });
  assert.equal(textMatch(bad.it, "latte", bad.ctx).where, "raw", "invalid UTF-8 is still searched");
  const unloaded = item("terminal_start", undefined, { summary: { text: "make deploy", truncated: false } });
  assert.deepEqual(textMatch(unloaded.it, "deploy", unloaded.ctx), { where: "hint", searchable: true });
  const nothing = item("terminal_start", undefined);
  assert.deepEqual(textMatch(nothing.it, "deploy", nothing.ctx), { where: "", searchable: false }, "not searchable, said so");
});

test("type filters key on the tool, not the verb", () => {
  const { it, ctx } = item("file_write", { path: "/a", content: "x" });
  assert.equal(matches(it, { query: "", tools: new Set(["file_write"]), attention: false }, ctx).match, true);
  assert.equal(matches(it, { query: "", tools: new Set(["file_edit"]), attention: false }, ctx).match, false);
});

test("needs attention: failed, not run, partly done, unknown, a bad exit or a stopped call", () => {
  const f = item("terminal_write", { session_id: "S", input: [] }, { status: "error" });
  assert.equal(needsAttention(f.it, f.ctx), true);
  const ok = item("terminal_list", {});
  assert.equal(needsAttention(ok.it, ok.ctx), false);
  const stopped = item("file_read", { files: [{ path: "a" }] }, { status: "canceled" });
  assert.equal(needsAttention(stopped.it, stopped.ctx), true);
  const unk = item("terminal_close", { session_id: "S" }, { status: "unknown" });
  assert.equal(needsAttention(unk.it, unk.ctx), true);
});

test("a hidden poll still decides its command's result", () => {
  const records = new Map();
  const start = item("terminal_start", { argv: ["make"], env: {} }, { records, status: "applied" });
  start.it.action.process = { server_id: "s", session_id: "S" };
  start.it.effect = { tool: "terminal_start", state: "finished", status: "applied" };
  const read = item("terminal_read", { session_id: "S" }, { records });
  read.it.action.process = { server_id: "s", session_id: "S" };
  read.it.action.finish = { primary: "fin", occurrences: ["fin"], time: "2026-11-10T10:01:00Z" };
  records.set("fin", { bytes: enc(JSON.stringify({ phase: "request_finish", status: "ok", process: { state: "exited", exit: { code: 3 } } }) + "\n") });
  const ctx = { records };
  timeline([start.it, read.it], ctx); // Observations come from the full timeline.
  const f = { query: "", tools: new Set(["terminal_start"]), attention: true };
  assert.equal(matches(read.it, f, ctx).match, false, "the read is filtered out");
  assert.equal(matches(start.it, f, ctx).match, true, "its exit 3 still makes the command need attention");
});

test("the history's own warnings are always shown, without counting as matches", () => {
  assert.equal(alwaysShown({ kind: "diagnostic", key: "d" }), true);
  assert.equal(alwaysShown({ kind: "server_start", key: "x" }), true);
  assert.equal(alwaysShown({ kind: "effect", key: "e", effect: {} }), true);
  const w = item("terminal_list", {});
  w.it.action.possibly_unrecorded = true;
  assert.equal(alwaysShown(w.it), true);
  const m = matches(w.it, q("zzz"), w.ctx);
  assert.deepEqual([m.match, m.keep], [false, true]);
  const plain = item("terminal_list", {});
  assert.equal(alwaysShown(plain.it), false);
  assert.deepEqual([matches({ kind: "diagnostic", key: "d" }, q("x"), {}).match, matches({ kind: "diagnostic", key: "d" }, q("x"), {}).keep], [false, true]);
  assert.equal(matches({ kind: "diagnostic", key: "d" }, q("x"), {}).searchable, true, "a row with nothing to search is not 'not searchable'");
});

test("filters are active only when set", () => {
  assert.equal(active({ query: "  ", tools: new Set(), attention: false }), false);
  assert.equal(active({ query: "x", tools: new Set(), attention: false }), true);
  assert.equal(active({ query: "", tools: new Set(["file_read"]), attention: false }), true);
  assert.equal(active({ query: "", tools: new Set(), attention: true }), true);
  assert.equal(active(null), false);
});

test("a rejected or failed call shows every field inline, so its matches are not in Details", () => {
  const r = item("file_write", { path: "/a", content: "x", operation_id: "opz" }, { status: "rejected" });
  r.it.action.rejected = { primary: "x", time: "t" };
  assert.equal(matches(r.it, q("opz"), r.ctx).where, "main");
  const f = item("file_write", { path: "/a", content: "x", operation_id: "opf" }, { status: "failed" });
  assert.equal(matches(f.it, q("opf"), f.ctx).where, "main");
  const u = item("terminal_start", { argv: "ls -la", operation_id: "opu" }, { status: "ok" });
  assert.equal(matches(u.it, q("opu"), u.ctx).where, "main", "input the card cannot render shows everything inline");
});

test("a zero time limit and a script's full line are in Details; Windows shows no POSIX line", () => {
  const z = item("terminal_start", { argv: ["make"], env: {}, max_runtime_ms: 0 });
  assert.equal(matches(z.it, q("max_runtime_ms"), z.ctx).where, "details");
  const sc = item("terminal_start", { argv: ["bash", "-c", "echo hi"], env: {} });
  assert.equal(matches(sc.it, q("bash -c 'echo"), sc.ctx).where, "details");
  assert.equal(matches(sc.it, q("echo hi"), sc.ctx).where, "main");
  const w = item("terminal_start", { argv: ["echo", "a b"], env: {} });
  w.ctx.serverOS = "windows";
  assert.equal(matches(w.it, q("'a b'"), w.ctx).match, false, "no POSIX quoting on Windows");
  assert.equal(matches(w.it, q("echo a b"), w.ctx).where, "main", "tokens joined still match");
});

test("the verb a card shows can be searched; a compact row's hidden card counts as Details", () => {
  const r = item("file_write", { path: "/a", content: "x", expected_version: "sha256:1" });
  assert.equal(matches(r.it, q("replace file"), r.ctx).where, "main");
  const l = item("file_search", { pattern: "needle", glob: "*.go" });
  l.it.action.finish = { primary: "f", time: "t" };
  assert.equal(matches(l.it, q("needle"), l.ctx).where, "main", "a value on the row");
  const long = item("file_search", { pattern: "p", path: "/" + "x".repeat(70) + "/tail-only" });
  assert.equal(matches(long.it, q("tail-only"), long.ctx).where, "details", "cut from the row: in its hidden card");
});

test("attention covers warnings and rejections; a matching warning item both matches and stays", () => {
  const nz = item("terminal_start", { argv: ["false"], env: {} }, { status: "applied" });
  nz.it.effect = { tool: "terminal_start", state: "finished", status: "applied" };
  nz.it.action.finish = { primary: "fz", time: "t" };
  nz.ctx.records.set("fz", { bytes: enc(JSON.stringify({ phase: "request_finish", status: "applied", process: { state: "exited", exit: { code: 1 } } }) + "\n") });
  assert.equal(needsAttention(nz.it, nz.ctx), true, "exit 1 seen by its own result");
  const sig = item("terminal_read", { session_id: "S" });
  sig.it.action.finish = { primary: "fs", time: "t" };
  sig.ctx.records.set("fs", { bytes: enc(JSON.stringify({ phase: "request_finish", status: "ok", process: { state: "exited", exit: { signal: "killed" } } }) + "\n") });
  assert.equal(needsAttention(sig.it, sig.ctx), true, "a poll that saw a signal");
  const part = item("terminal_write", { session_id: "S", input: [] }, { status: "partial" });
  assert.equal(needsAttention(part.it, part.ctx), true);
  const rej = item("file_write", { path: "/a" }, { status: "rejected" });
  assert.equal(needsAttention(rej.it, rej.ctx), true);
  const w = item("terminal_list", { session_id: "zz" });
  w.it.action.identity = { metadataInvalid: true };
  const m = matches(w.it, q("terminal_list"), w.ctx);
  assert.deepEqual([m.match, m.keep], [true, true]);
  assert.throws(() => needsAttention(w.it, { records: w.ctx.records }), /processes/);
});

test("every header warning keeps an item shown", () => {
  const mk = (patch) => { const x = item("terminal_list", {}); patch(x.it); return alwaysShown(x.it); };
  assert.equal(mk((it) => { it.effect = { origin_conflict: true }; }), true);
  assert.equal(mk((it) => { it.action.start.more = 2; }), true);
  assert.equal(mk((it) => { it.action.start.occurrences = ["p:1"]; }), true);
  assert.equal(mk((it) => { it.action.start.conflict = true; }), true);
});

test("apply keeps warnings, counts what the search could not see, and new activity by unit", () => {
  const records = new Map();
  const a = item("terminal_start", { argv: ["make", "deploy"], env: {} }, { records });
  const b = item("terminal_list", {}, { records });
  const c = item("terminal_start", undefined, { records }); // Not loaded, no hint.
  const d = item("terminal_read", { session_id: "S" }, { records });
  const diag = { kind: "diagnostic", key: "d:1" };
  const ctx = { records };
  const entries = timeline([a.it, b.it, c.it, d.it], ctx, { group: false });
  entries.push({ item: diag });
  const units = new Map([[b.it.key, "u1"], [d.it.key, "u1"], [c.it.key, "u2"]]);
  const r = apply(entries, q("deploy"), ctx, (k) => units.get(k));
  assert.deepEqual(r.entries.map((e) => e.item.key), [a.it.key, "d:1"], "the match and the unreadable row");
  assert.deepEqual(r.stats, { matched: 1, unsearchable: 1, kept: 1, hiddenUnread: 2 }, "two units, not three items");
  assert.equal(r.matchWhere.get(a.it.key), "main");
});
