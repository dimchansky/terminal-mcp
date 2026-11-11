import { test } from "node:test";
import assert from "node:assert/strict";
import { laneName, laneTime, newestActions, lanePreview, rename, shortIds } from "../static/lanes.js";

const conv = (over = {}) => ({ id: "L1", kind: "conversation", identitySource: "openai/session", conversationId: "v1/abc", items: 3, ...over });

test("a chat is named by its alias, else a client name and its short ID", () => {
  assert.deepEqual(laneName(conv({ client_names: ["codex"] }), { alias: "Release work", shortId: "v1/abc" }), { name: "Release work", client: "", shortId: "", more: 0 });
  assert.deepEqual(laneName(conv({ client_names: ["codex"] }), { shortId: "v1/abc" }), { name: "codex · v1/abc", client: "codex", shortId: "v1/abc", more: 0 });
  assert.deepEqual(laneName(conv(), { shortId: "v1/abc" }), { name: "v1/abc", client: "", shortId: "v1/abc", more: 0 });
  assert.equal(laneName({ kind: "unattributed" }).name, "Unattributed");
  assert.equal(laneName({ kind: "constructor" }).name, "constructor", "names from records are never object keys");
});

test("a chat's name stays when new client names arrive after it", () => {
  // The server lists client names newest first; the name does not follow.
  const before = laneName(conv({ client_names: ["chatgpt"] }), { shortId: "v1/abc" });
  const after = laneName(conv({ client_names: ["zeta-client", "chatgpt"] }), { shortId: "v1/abc" });
  assert.equal(after.name, before.name);
  assert.equal(after.more, 1, "the other name is counted apart from the name");
  // Only a name that sorts first changes it.
  assert.equal(laneName(conv({ client_names: ["alpha", "chatgpt"] }), { shortId: "v1/abc" }).name, "alpha · v1/abc");
});

test("last activity reads today's time, Yesterday or a date", () => {
  const now = new Date(2026, 10, 11, 15, 0, 0);
  assert.match(laneTime(new Date(2026, 10, 11, 9, 5).toISOString(), now), /^\D*0?9\D05\D*$/);
  assert.equal(laneTime(new Date(2026, 10, 10, 23, 59).toISOString(), now), "Yesterday");
  assert.equal(laneTime(new Date(2026, 10, 9, 12).toISOString(), now), new Date(2026, 10, 9, 12).toLocaleDateString(undefined, { month: "short", day: "numeric" }));
  assert.match(laneTime(new Date(2025, 10, 9, 12).toISOString(), now), /2025/);
  assert.equal(laneTime("not a time", now), "");
  assert.equal(laneTime("", now), "");
});

test("the preview is the newest loaded action, never an invented title", () => {
  const act = (key, lane, rank, time, summary) => ({ kind: "action", key, lane, order: { live: true, rank },
    action: { tool: "terminal_start", start: { time } }, summary });
  const items = [
    act("a:1", "L1", 1, "2026-11-11T10:00:00Z", { text: "go test ./...", truncated: false }),
    act("a:3", "L1", 3, "2026-11-11T10:02:00Z", { text: "make check", truncated: true }),
    { kind: "server_start", key: "s:1", lane: "L1", order: { live: true, rank: 9 } },
    act("a:2", "L2", 2, "2026-11-11T10:01:00Z"),
    { kind: "action", key: "a:0", lane: "L1", order: { live: false, rank: 99 }, action: { tool: "file_write", start: { time: "2026-11-11T09:00:00Z" } } },
  ];
  const newest = newestActions(items);
  assert.equal(newest.get("L1").key, "a:3", "by the timeline's order (history before live), not by map order; markers skipped");
  assert.deepEqual(lanePreview(conv({ last_time: "2026-11-11T10:02:00Z" }), newest.get("L1")), { verb: "Run command", text: "make check", cut: true, time: "" });
  assert.deepEqual(lanePreview(conv({ id: "L2", last_time: "2026-11-11T11:00:00Z" }), newest.get("L2")),
    { verb: "Run command", text: "", cut: false, time: "2026-11-11T10:01:00Z" }, "not the newest activity: says when; no hint, just the verb");
  assert.equal(lanePreview(conv(), undefined), null);
});

test("rename saves to the chat it was opened for", () => {
  let st = rename.open("digest-A", "Old");
  assert.deepEqual(st, { editing: true, digest: "digest-A", value: "Old" });
  const [closed, change] = rename.save(st, "  " + "x".repeat(100) + "  ");
  assert.equal(closed.editing, false);
  assert.deepEqual(change, { digest: "digest-A", alias: "x".repeat(80) });
  assert.deepEqual(rename.save(rename.open("d", ""), "   ")[1], { digest: "d", alias: "" }, "empty removes");
  assert.deepEqual(rename.save(rename.cancel(), "x"), [{ editing: false }, null]);
  st = rename.open("d", "");
  assert.equal(Array.from(rename.save(st, "😀".repeat(90))[1].alias).length, 80, "the cap counts characters, not code units");
});

test("short IDs tell chats apart: collisions lengthen them, a shared ID names its source", () => {
  const a = conv({ conversationId: "v1/AAAAAAAAAAAAAAAA-one" });
  const b = conv({ conversationId: "v1/AAAAAAAAAAAAAAAA-two" });
  const c = conv({ conversationId: "v1/short" });
  const d = conv({ conversationId: "v1/short", identitySource: "other/source" });
  const e = conv({ conversationId: "" , identitySource: "x" });
  const ids = shortIds([a, b, c, d, e, { kind: "journal" }]);
  const id = (l) => ids.get("c\u0000" + l.identitySource + "\u0000" + l.conversationId);
  assert.equal(id(a), "v1/AAAAAAAAAAAAAAAA-o…");
  assert.equal(id(b), "v1/AAAAAAAAAAAAAAAA-t…");
  assert.equal(id(c), "v1/short (openai/session)");
  assert.equal(id(d), "v1/short (other/source)");
  assert.equal(id(e), "(empty conversation ID)");
  assert.equal(ids.size, 5, "only conversations");
  // When the colliding chat leaves, the ID shortens again.
  assert.equal(shortIds([a]).get("c\u0000openai/session\u0000" + a.conversationId), "v1/AAAAAAAAAAAAA…", "16 code points");
});

test("the newest action breaks an order tie by key", () => {
  const it = (key) => ({ kind: "action", key, lane: "L", order: { live: true, rank: 1 }, action: { tool: "file_write", start: {} } });
  assert.equal(newestActions([it("a:2"), it("a:1")]).get("L").key, "a:2");
  assert.equal(newestActions([it("a:1"), it("a:2")]).get("L").key, "a:2");
});
