import { test } from "node:test";
import assert from "node:assert/strict";
import { Attention } from "../static/attention.js";

const act = (key, lane, tool, extra = {}) => ({ key, lane, kind: "action", order: { live: true, rank: 1 },
  action: { tool, state: "open", start: { primary: "s" + key }, ...extra.action }, ...extra });
const laneKind = (l) => (l.startsWith("chat") ? "conversation" : l);

function setup(now = { t: 0 }) {
  const att = new Attention({ laneOf: (i) => i.lane, unitOf: (i) => (i.action.tool === "terminal_read" ? "poll:" + i.lane : i.key), now: () => now.t });
  return { att, now };
}

const ctx = (over = {}) => ({ autoSwitch: true, current: "chat-a", deferred: false, laneKind, ...over });

test("a start and its finish count once; a status change after seen counts again", () => {
  const { att } = setup();
  const it = act("a1", "chat-b", "file_write");
  att.observe([{ type: "added", item: it }], { live: true }, ctx({ autoSwitch: false }));
  const done = { ...it, action: { ...it.action, state: "finished", status: "ok" } };
  att.observe([{ type: "changed", item: done }], { live: true }, ctx({ autoSwitch: false }));
  assert.equal(att.count("chat-b"), 1);
  att.seen([done]);
  assert.equal(att.count("chat-b"), 0);
  const failed = { ...done, effect: { state: "finished", status: "error" } };
  att.observe([{ type: "changed", item: failed }], { live: true }, ctx({ autoSwitch: false }));
  assert.equal(att.count("chat-b"), 1);
});

test("snapshots, history, replays and moves never count", () => {
  const { att } = setup();
  const it = act("h1", "chat-b", "file_read");
  att.observe([{ type: "added", item: it }], { snapshot: true }, ctx());
  att.observe([{ type: "added", item: act("h2", "chat-b", "file_read") }], { history: true }, ctx());
  for (let i = 0; i < 50; i++) { att.observe([{ type: "changed", item: it }], { live: true }, ctx()); }
  att.observe([{ type: "moved", item: { ...it, lane: "chat-c" } }], { live: true }, ctx());
  assert.equal(att.count("chat-b") + att.count("chat-c"), 0);
});

test("a poll storm counts once while unseen", () => {
  const { att } = setup();
  const polls = Array.from({ length: 30 }, (_, i) => act("p" + i, "chat-b", "terminal_read"));
  att.observe(polls.map((item) => ({ type: "added", item })), { live: true }, ctx({ autoSwitch: false }));
  assert.equal(att.count("chat-b"), 1);
});

test("auto-switch: only new live starts of known non-polling tools in claimed lanes", () => {
  const { att, now } = setup();
  now.t = 10_000;
  assert.equal(att.observe([{ type: "added", item: act("x", "chat-b", "terminal_start") }], { live: true }, ctx()), "chat-b");
  now.t = 20_000;
  assert.equal(att.observe([{ type: "added", item: act("y", "chat-c", "terminal_read") }], { live: true }, ctx()), null, "polling");
  assert.equal(att.observe([{ type: "added", item: act("z", "unattributed", "file_write") }], { live: true }, ctx()), null, "unattributed");
  assert.equal(att.observe([{ type: "added", item: act("u", "chat-c", "brand_new_tool") }], { live: true }, ctx()), null, "unknown tool");
  assert.equal(att.observe([{ type: "added", item: act("v", "chat-c", "file_read") }], { history: true }, ctx()), null, "history");
  const fin = act("w", "chat-c", "file_write");
  att.observe([{ type: "added", item: fin }], { live: true }, ctx({ autoSwitch: false }));
  assert.equal(att.observe([{ type: "changed", item: { ...fin, action: { ...fin.action, state: "finished" } } }], { live: true }, ctx()), null, "finish");
  assert.equal(att.observe([{ type: "added", item: act("o", "chat-c", "file_read") }], { live: true }, ctx({ autoSwitch: false })), null, "off");
});

test("focus is held after any selection; candidates during the hold are dropped", () => {
  const { att, now } = setup();
  now.t = 1000;
  att.selected();
  now.t = 3000;
  assert.equal(att.observe([{ type: "added", item: act("a", "chat-b", "file_write") }], { live: true }, ctx()), null);
  now.t = 7000; // The hold ended; the earlier candidate is not replayed.
  assert.equal(att.observe([], { live: true }, ctx()), null);
  assert.equal(att.observe([{ type: "added", item: act("b", "chat-c", "file_write") }], { live: true }, ctx()), "chat-c");
});

test("switching is deferred while reading, with a pending hint", () => {
  const { att, now } = setup();
  now.t = 10_000;
  assert.equal(att.observe([{ type: "added", item: act("a", "chat-b", "file_write") }], { live: true }, ctx({ deferred: true })), null);
  assert.equal(att.pending, "chat-b");
  assert.equal(att.resume(ctx({ deferred: true })), null, "still reading");
  assert.equal(att.resume(ctx()), "chat-b", "resumed at the tail");
  assert.equal(att.pending, null);
  att.observe([{ type: "added", item: act("c", "chat-c", "file_write") }], { live: true }, ctx({ deferred: true }));
  assert.equal(att.resume(ctx({ autoSwitch: false })), null, "dropped when turned off");
  att.observe([{ type: "added", item: act("d", "chat-c", "file_write") }], { live: true }, ctx({ deferred: true }));
  att.selected();
  assert.equal(att.pending, null, "a manual selection drops it");
});

test("a move combined with a finish counts; removed items leave; refresh relanes", () => {
  const { att } = setup();
  const it = act("m", "pending", "file_write");
  att.observe([{ type: "added", item: it }], { live: true }, ctx({ autoSwitch: false }));
  att.seen([it]);
  const moved = { ...it, lane: "chat-b", action: { ...it.action, state: "finished" } };
  att.observe([{ type: "moved", item: moved }], { live: true }, ctx({ autoSwitch: false }));
  assert.equal(att.count("chat-b"), 1);
  att.observe([{ type: "removed", key: "m" }], { live: true }, ctx());
  assert.equal(att.count("chat-b"), 0);
  att.observe([{ type: "added", item: act("n", "chat-b", "file_write") }], { live: true }, ctx({ autoSwitch: false }));
  att.refresh(new Map([["n", act("n", "chat-z", "file_write")]]));
  assert.equal(att.count("chat-z"), 1, "a new snapshot keeps unread by lane identity");
});

test("an effect starting on a seen action does not count; its finish does", () => {
  const { att } = setup();
  const it = act("e", "chat-b", "file_receive");
  att.observe([{ type: "added", item: it }], { live: true }, ctx({ autoSwitch: false }));
  att.seen([it]);
  att.observe([{ type: "changed", item: { ...it, effect: { state: "in_flight" } } }], { live: true }, ctx({ autoSwitch: false }));
  assert.equal(att.count("chat-b"), 0);
  att.observe([{ type: "changed", item: { ...it, effect: { state: "finished", status: "ok" } } }], { live: true }, ctx({ autoSwitch: false }));
  assert.equal(att.count("chat-b"), 1);
});

test("attentionFor counts by lane identity and poll target of the store", async () => {
  const { attentionFor } = await import("../static/attention.js");
  const store = { lanes: new Map([["g1-l1", { id: "g1-l1", kind: "conversation", identitySource: "openai/session", conversationId: "c-1" }]]), records: new Map() };
  const att = attentionFor(store, { now: () => 0 });
  att.observe([{ type: "added", item: act("k1", "g1-l1", "file_write") }], { live: true }, ctx({ autoSwitch: false }));
  assert.equal(att.count("c\u0000openai/session\u0000c-1"), 1, "counted in the lane identity");
  assert.equal(att.count("g1-l1"), 0, "never in the per-generation lane ID");
});

test("marking an older copy of an item seen never makes a repeated delivery count", () => {
  const { att } = setup();
  const it = act("s", "chat-b", "file_receive");
  att.observe([{ type: "added", item: it }], { live: true }, ctx({ autoSwitch: false }));
  const done = { ...it, effect: { state: "finished", status: "ok" } };
  att.observe([{ type: "changed", item: done }], { live: true }, ctx({ autoSwitch: false }));
  att.seen([it]); // The card on screen was rendered before the finish arrived.
  assert.equal(att.count("chat-b"), 0);
  att.observe([{ type: "changed", item: done }], { live: true }, ctx({ autoSwitch: false }));
  assert.equal(att.count("chat-b"), 0, "the same finish delivered again is not new");
});
