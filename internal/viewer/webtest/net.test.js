import { test } from "node:test";
import assert from "node:assert/strict";
import { Client, Queue, sseParser } from "../static/net.js";
import { Store, laneIdentity } from "../static/model.js";

const b64 = (s) => Buffer.from(s).toString("base64");

function json(status, body, headers = {}) {
  return { ok: status >= 200 && status < 300, status, headers: new Headers(headers),
    json: async () => body, arrayBuffer: async () => new TextEncoder().encode(String(body)).buffer };
}

// A stream response whose events the test pushes.
function streamResponse() {
  let controller;
  const body = new ReadableStream({ start(c) { controller = c; } });
  const enc = new TextEncoder();
  return {
    resp: { ok: true, status: 200, headers: new Headers({ "Content-Type": "text/event-stream" }), body },
    send(name, data) { controller.enqueue(enc.encode("event: " + name + "\ndata: " + JSON.stringify(data) + "\n\n")); },
    close() { try { controller.close(); } catch (e) { /* closed */ } },
  };
}

const lane = (id, conv) => ({ id, kind: "conversation", identitySource: "openai/session", conversationId: conv, items: 1 });
const action = (id, laneId, rank, extra = {}) => ({ kind: "action", key: "a:s/" + id, lane: laneId, order: { live: rank > 0, rank },
  action: { key: { server_id: "s", request_id: id }, tool: "file_read", state: "finished", start: { primary: "ref-" + id, occurrences: ["ref-" + id] } }, ...extra });
const envelope = (gen, items, lanes, cursor = "c0", window = "w1") => ({ window, generation: gen, state: "ok", live_cursor: cursor,
  possible_unseen: false, lanes, recent: { generation: gen, items, records: items.map((i) => ({ occurrence: "ref-" + i.action.key.request_id, data_b64: b64("{}\n"), size: 3 })) } });

// waitFor polls a condition instead of sleeping for a fixed time.
async function waitFor(cond, what = "condition") {
  for (let i = 0; i < 400; i++) {
    if (cond()) { return; }
    await new Promise((r) => setTimeout(r, 5));
  }
  assert.fail("timed out waiting for " + what);
}

// fakeServer routes requests to handlers; unknown routes answer 204.
function fakeServer(handlers) {
  const calls = [];
  const streams = [];
  const fetch = async (url, init) => {
    const path = url.replace("/dashboard/api/", "");
    const body = JSON.parse(init.body);
    calls.push({ path, body });
    if (path === "stream") {
      if (handlers.stream) { const r = handlers.stream(body); if (r) { return r; } }
      const s = streamResponse();
      streams.push(s);
      init.signal?.addEventListener("abort", () => s.close());
      return s.resp;
    }
    return handlers[path] ? handlers[path](body, calls) : json(204, null);
  };
  const nextStream = async (n) => { await waitFor(() => streams.length >= n, "stream " + n); return streams[n - 1]; };
  return { fetch, calls, streams, nextStream, count: (p) => calls.filter((c) => c.path === p).length };
}

const fast = { claimWait: 5, backoff: [5, 5, 5], channel: () => null };

test("snapshot, stream, catch-up; duplicates change nothing", async () => {
  let head = 0;
  const server = fakeServer({
    window: () => json(200, envelope(1, [action("r1", "L1", -1)], [lane("L1", "chat-a")])),
    "catch-up": () => {
      head++;
      return json(200, { generation: 1, items: [action("r2", "L1", 1)], records: [{ occurrence: "ref-r2", data_b64: b64("{}\n") }],
        lanes: [lane("L1", "chat-a")], head: "c" + head, more: false });
    },
  });
  const seen = [];
  const c = new Client("tok", { onChange: (ch) => seen.push(...ch) }, { ...fast, fetch: server.fetch });
  c.start();
  const s = await server.nextStream(1);
  s.send("ready", {});
  s.send("activity", { items: [] });
  await waitFor(() => c.cursor === "c1", "first catch-up");
  assert.equal(c.store.items.size, 2);
  const before = seen.length;
  for (let i = 0; i < 50; i++) { s.send("activity", { items: [] }); }
  await waitFor(() => server.count("catch-up") >= 2 && !c.catching, "replayed catch-ups");
  assert.deepEqual(seen.slice(before), [], "replayed deliveries reported changes");
  assert.equal(server.calls.find((x) => x.path === "catch-up").body.after, "c0");
  c.stop();
});

test("catch-up follows next pages and more rounds", async () => {
  const server = fakeServer({
    window: () => json(200, envelope(1, [], [])),
    "catch-up": (b) => {
      if (b.after === "c0" && !b.before) { return json(200, { generation: 1, items: [action("a", "L", 1)], records: [], head: "c1", more: true, next: { order: { live: true, rank: 1 }, key: "a:s/a" } }); }
      if (b.after === "c0") { return json(200, { generation: 1, items: [action("b", "L", 2)], records: [], head: "c1", more: true }); }
      return json(200, { generation: 1, items: [action("c", "L", 3)], records: [], head: "c2", more: false });
    },
  });
  const c = new Client("tok", {}, { ...fast, fetch: server.fetch });
  c.start();
  (await server.nextStream(1)).send("activity", {});
  await waitFor(() => c.cursor === "c2", "two rounds");
  assert.deepEqual([...c.store.items.keys()].sort(), ["a:s/a", "a:s/b", "a:s/c"]);
  c.stop();
});

test("a failed catch-up is retried, and a heartbeat retries it too", async () => {
  let n = 0;
  const server = fakeServer({
    window: () => json(200, envelope(1, [], [])),
    "catch-up": () => (++n < 3 ? json(503, { error: "timeout" }, { "Retry-After": "0.01" }) : json(200, { generation: 1, items: [action("x", "L", 1)], records: [], head: "c9", more: false })),
  });
  const c = new Client("tok", {}, { ...fast, fetch: server.fetch });
  c.start();
  (await server.nextStream(1)).send("activity", {});
  await waitFor(() => c.cursor === "c9", "retried catch-up");
  assert.equal(n, 3);
  c.stop();
});

test("a replacement window never mixes lanes of the old one", () => {
  const st = new Store();
  st.snapshot(envelope(0, [action("A1", "g0-l1", -1)], [lane("g0-l1", "chat-a")], "c0", "w1"));
  const r = st.snapshot(envelope(0, [action("B1", "g0-l1", -1)], [lane("g0-l1", "chat-b"), lane("g0-l2", "chat-a")], "c0", "w2"));
  assert.equal(r.generationChanged, true);
  assert.deepEqual(st.laneItems("g0-l1").map((i) => i.key), ["a:s/B1"]);
  assert.equal(r.remap.get("g0-l1"), "g0-l2");
});

test("a separate effect is dropped when its action arrives; removed keys are applied", () => {
  const st = new Store();
  st.snapshot(envelope(1, [], []));
  st.apply({ generation: 1, items: [{ kind: "effect", key: "e:s/op1", lane: "P", order: { live: true, rank: 1 }, effect: { key: { server_id: "s", operation_id: "op1" } } }] });
  const own = action("r1", "L1", 2);
  own.action.effect = { kind: "own", effect: { server_id: "s", operation_id: "op1" }, loaded: true };
  own.effect = { key: { server_id: "s", operation_id: "op1" } };
  const { changes } = st.apply({ generation: 1, items: [own] });
  assert.deepEqual(changes.map((c) => c.type + " " + c.key), ["added a:s/r1", "removed e:s/op1"]);
  assert.deepEqual(st.apply({ generation: 1, items: [], removed: ["a:s/r1"] }).changes.map((c) => c.type), ["removed"]);
  assert.equal(st.apply({ generation: 2, items: [own] }).stale, true);
});

test("a lane move is reported; generation changes remap lanes by identity", () => {
  const st = new Store();
  st.snapshot(envelope(1, [action("r1", "P", -1)], [lane("L1", "chat-a")]));
  assert.equal(st.apply({ generation: 1, updated: [action("r1", "L1", -1)] }, { history: true }).changes[0].type, "moved");
  const next = st.snapshot(envelope(2, [action("r1", "L9", -1)], [lane("L9", "chat-a")]));
  assert.equal(next.remap.get("L1"), "L9");
  assert.equal(laneIdentity(st.lanes.get("L9")), "c\u0000openai/session\u0000chat-a");
  assert.equal(st.records.has("ref-r1"), true, "reference records survive generations");
});

test("records missing from a catch-up and budget-deferred records load, two at a time", async () => {
  let inFlight = 0;
  let peak = 0;
  const items = ["a", "b", "c", "d"].map((x, i) => action(x, "L1", -i - 1));
  const env = envelope(1, items, [lane("L1", "x")]);
  env.recent.records = items.map((i) => ({ occurrence: "ref-" + i.action.key.request_id, deferred: true }));
  env.recent.records.push({ occurrence: "ref-big", deferred: true, size: 3 << 20 });
  items[0].action.finish = { primary: "ref-big", occurrences: ["ref-big"] }; // A large record of a loaded item.
  const server = fakeServer({
    window: () => json(200, env),
    "catch-up": () => json(200, { generation: 1, items: [action("late", "L1", 1)], records: [], head: "c1", more: false }),
    record: async () => { inFlight++; peak = Math.max(peak, inFlight); await new Promise((r) => setTimeout(r, 5)); inFlight--; return json(200, "{\"x\":1}\n"); },
  });
  const c = new Client("tok", {}, { ...fast, fetch: server.fetch });
  c.start();
  (await server.nextStream(1)).send("activity", {});
  await waitFor(() => c.store.records.get("ref-late")?.bytes, "the start record of a catch-up item");
  await waitFor(() => server.count("record") === 5, "all small records");
  assert.equal(peak <= 2, true);
  assert.equal(c.store.records.get("ref-big").bytes, null, "the 3 MiB record waits for the owner");
  c.stop();
});

test("resync re-snapshots; a rebuilding window is waited out, not hammered", async () => {
  let rebuilding = 2;
  const server = fakeServer({
    window: (b) => (b.window && rebuilding-- > 0 ? json(503, { error: "rebuilding" }, { "Retry-After": "0.02" }) : json(200, envelope(b.window ? 2 : 1, [], [], b.window ? "c2" : "c0"))),
  });
  const c = new Client("tok", {}, { ...fast, fetch: server.fetch });
  c.start();
  (await server.nextStream(1)).send("resync", { generation: 2 });
  await server.nextStream(2);
  assert.equal(c.store.generation, 2);
  assert.equal(c.cursor, "c2");
  assert.equal(server.count("window"), 4, "one open, two waits, one snapshot");
  c.stop();
});

test("a reload reuses the stored window; a gone window opens a new one and is closed", async () => {
  const storage = new Map([["terminal-mcp-dashboard-window", "w-old"]]);
  const store = { getItem: (k) => storage.get(k) ?? null, setItem: (k, v) => storage.set(k, v), removeItem: (k) => storage.delete(k) };
  const server = fakeServer({ window: (b) => (b.window ? json(404, { error: "window_gone" }) : json(200, envelope(1, [], [], "c0", "w-new"))) });
  const c = new Client("tok", {}, { ...fast, fetch: server.fetch, storage: store });
  c.start();
  await server.nextStream(1);
  assert.deepEqual(server.calls.filter((x) => x.path === "window").map((x) => x.body), [{ window: "w-old" }, { mode: "live" }]);
  assert.deepEqual(server.calls.find((x) => x.path === "close").body, { window: "w-old" });
  assert.equal(storage.get("terminal-mcp-dashboard-window"), "w-new");
  c.stop();
  c.forget();
  assert.equal(storage.has("terminal-mcp-dashboard-window"), false);
});

test("a duplicated tab ends its copied session; the original keeps the window", async () => {
  const listeners = [];
  const channel = () => {
    const self = { handlers: [], addEventListener(_, h) { this.handlers.push(h); }, removeEventListener(_, h) { this.handlers = this.handlers.filter((x) => x !== h); },
      postMessage(data) { for (const l of listeners) { if (l !== self) { l.handlers.forEach((h) => h({ data })); } } }, close() {} };
    listeners.push(self);
    return self;
  };
  const storage = () => { const m = new Map([["terminal-mcp-dashboard-window", "w1"]]); return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => m.set(k, v), removeItem: (k) => m.delete(k) }; };
  const server = fakeServer({ window: () => json(200, envelope(1, [], [])) });
  const ended = [];
  const tabs = [0, 1, 2].map(() => new Client("tok", { onSessionEnded: (code) => ended.push(code) }, { ...fast, claimWait: 20, fetch: server.fetch, storage: storage(), channel }));
  tabs.forEach((t) => t.start());
  await waitFor(() => ended.length === 2, "two duplicates ended");
  assert.deepEqual(ended, ["duplicate_tab", "duplicate_tab"]);
  await server.nextStream(1);
  assert.equal(tabs.filter((t) => t.windowId === "w1" && !t.stopped).length, 1);
  tabs.forEach((t) => t.stop());
});

test("busy page slots are retried after Retry-After", async () => {
  let n = 0;
  const server = fakeServer({ window: () => (++n === 1 ? json(429, { error: "capacity" }, { "Retry-After": "0.01" }) : json(200, envelope(1, [], []))) });
  const c = new Client("tok", {}, { ...fast, fetch: server.fetch });
  c.start();
  await server.nextStream(1);
  assert.equal(n, 2);
  c.stop();
});

test("a disabled audit does not stream or reconnect", async () => {
  const env = envelope(0, [], []);
  env.state = "disabled";
  const server = fakeServer({ window: () => json(200, env) });
  const c = new Client("tok", {}, { ...fast, fetch: server.fetch });
  await c.start();
  assert.equal(c.status.connection, "audit_closed");
  assert.equal(server.count("stream"), 0);
  c.stop();
});

test("the window limit closes the archive window first", async () => {
  let opened = 0;
  const server = fakeServer({ window: (b) => (b.mode === "live" && opened++ === 1 ? json(409, { error: "window_limit" }) : json(200, envelope(1, [], [], "c0", b.mode === "archive" ? "arch" : "w" + opened))) });
  const c = new Client("tok", {}, { ...fast, fetch: server.fetch });
  c.start();
  const s = await server.nextStream(1);
  await c.openArchive();
  assert.equal(c.archiveId, "arch");
  s.send("window_state", { state: "failed" });
  await server.nextStream(2);
  assert.equal(c.archiveId, null);
  assert.ok(server.calls.some((x) => x.path === "close" && x.body.window === "arch"));
  c.stop();
});

test("session ends: unauthorized JSON, server_stopping JSON and event", async () => {
  for (const [how, code] of [["json-401", "unauthorized"], ["json-503", "server_stopping"], ["event", "server_stopping"]]) {
    const server = fakeServer({
      window: () => (how === "json-401" ? json(401, { error: "unauthorized" }) : how === "json-503" ? json(503, { error: "server_stopping" }) : json(200, envelope(1, [], []))),
    });
    let ended = null;
    const c = new Client("tok", { onSessionEnded: (x) => { ended = x; } }, { ...fast, fetch: server.fetch });
    const done = c.start();
    if (how === "event") { (await server.nextStream(1)).send("server_stopping", { reason: "update" }); }
    await done;
    assert.equal(ended, code, how);
    if (code === "server_stopping") { assert.equal(c.status.connection, "stopped", how); }
  }
});

test("a stream that ends without an event reconnects", async () => {
  const server = fakeServer({ window: () => json(200, envelope(1, [], [])) });
  const c = new Client("tok", {}, { ...fast, fetch: server.fetch });
  c.start();
  const s1 = await server.nextStream(1);
  s1.send("ready", {});
  s1.close();
  await server.nextStream(2);
  c.stop();
});

test("scan answers apply updates and re-fetch affected lanes when they did not fit", async () => {
  const server = fakeServer({
    window: () => json(200, envelope(1, [action("r1", "P", -1)], [lane("L1", "x")])),
    origins: () => json(200, { generation: 1, lanes: [], records: [], updated: [action("r1", "L1", -1)], removed: ["e:s/op"], updated_more: true, affected_lanes: ["L1", "P"] }),
    "lane-items": () => json(200, { generation: 1, items: [], records: [] }),
  });
  const c = new Client("tok", {}, { ...fast, fetch: server.fetch });
  c.start();
  await server.nextStream(1);
  await c.origins();
  assert.equal(c.store.items.get("a:s/r1").lane, "L1");
  assert.deepEqual(server.calls.filter((x) => x.path === "lane-items").map((x) => x.body.lane_id), ["L1", "P"]);
  c.stop();
});

test("queue and SSE parser", async () => {
  const q = new Queue(1);
  const order = [];
  await Promise.all([q.run(async () => { await new Promise((r) => setTimeout(r, 5)); order.push(1); }), q.run(async () => order.push(2))]);
  assert.deepEqual(order, [1, 2]);
  const got = [];
  const feed = sseParser((n, d) => got.push([n, d]));
  feed("event: a\ndata: {\"x\"");
  feed(":1}\n\nevent: b\ndata: 2\r\n\r\n");
  assert.deepEqual(got, [["a", { x: 1 }], ["b", 2]]);
});

test("archive scans apply updates that did not fit and reload a rebuilt archive window", async () => {
  let archiveGen = 1;
  let rebuilt = false;
  const server = fakeServer({
    window: (b) => {
      if (b.mode === "live") { return json(200, envelope(1, [], [], "c0", "w1")); }
      return json(200, envelope(archiveGen, [action("o1", "A1", -1)], [lane("A1", "chat-old")], "", "arch"));
    },
    earlier: () => (rebuilt ? json(409, { error: "generation_changed" })
      : json(200, { generation: archiveGen, items: [], lanes: [lane("A2", "chat-older")], updated_more: true, affected_lanes: ["A1", "A2"] })),
    "lane-items": (b) => json(200, { generation: archiveGen, items: [action("x-" + b.lane_id, b.lane_id, -2)], lanes: [lane(b.lane_id, "chat-" + b.lane_id)] }),
  });
  const c = new Client("tok", {}, { ...fast, fetch: server.fetch });
  c.start();
  await server.nextStream(1);
  const { store } = await c.openArchive();
  assert.equal((await c.archivePage(store, "earlier", "A1")).ok, true);
  const refetched = server.calls.filter((x) => x.path === "lane-items").map((x) => [x.body.window, x.body.lane_id]);
  assert.deepEqual(refetched, [["arch", "A1"], ["arch", "A2"]]);
  assert.ok(store.items.has("a:s/x-A2"));

  // The server rebuilt the archive window: lane IDs of the old generation
  // answer 409 generation_changed.
  archiveGen = 2;
  rebuilt = true;
  const r = await c.archivePage(store, "earlier", "A1");
  assert.equal(r.code, "archive_reloaded");
  assert.ok(server.calls.some((x) => x.path === "window" && x.body.window === "arch"), "a new snapshot of the archive window");
  assert.equal(store.generation, 2);

  // Closing the archive while a page is in flight leaves its store alone.
  rebuilt = false;
  const pending = c.archivePage(store, "earlier", "A1");
  await c.closeArchive();
  const before = store.version;
  await pending;
  assert.equal(store.version, before, "a page answered after closing is not applied");
  c.stop();
});

test("a terminal window state reopens with a growing delay and says why", async () => {
  const sleeps = [];
  let opened = 0;
  const server = fakeServer({ window: () => json(200, envelope(1, [], [], "c0", "w" + ++opened)) });
  const c = new Client("tok", {}, { ...fast, fetch: server.fetch, sleep: async (ms) => { sleeps.push(ms); await new Promise((r) => setTimeout(r, 1)); } });
  c.start();
  for (let n = 1; n <= 3; n++) {
    const s = await server.nextStream(n);
    s.send("window_state", { state: "capacity" });
  }
  await server.nextStream(4);
  assert.equal(c.status.reason, "", "a new snapshot clears the reason");
  assert.deepEqual(sleeps.slice(0, 2), [5, 5], "the second and third reopen wait");
  c.stop();
});

test("a reconnect shows its own reason, not an earlier one", async () => {
  let opened = 0;
  const statuses = [];
  const server = fakeServer({
    window: () => (opened++ === 0 ? json(409, { error: "window_limit" }) : json(200, envelope(1, [], [], "c0", "w1"))),
    stream: () => json(503, { error: "timeout" }, { "Retry-After": "1" }),
  });
  const c = new Client("tok", { onStatus: (s) => statuses.push({ ...s }) }, { ...fast, fetch: server.fetch, sleep: async () => { await new Promise((r) => setTimeout(r, 1)); } });
  c.start();
  await waitFor(() => statuses.some((s) => s.connection === "reconnecting"), "reconnecting");
  const offline = statuses.find((s) => s.connection === "offline");
  assert.deepEqual([offline.reason, offline.reasonStatus], ["window_limit", 409]);
  const rec = statuses.find((s) => s.connection === "reconnecting");
  assert.deepEqual([rec.reason, rec.reasonStatus], ["timeout", 503]);
  c.stop();
});

test("session requests carry only the viewer token and answer error codes", async () => {
  const { signIn, renewSession, signOut, checkSession } = await import("../static/net.js");
  const seen = [];
  const fetchFn = async (url, init) => {
    seen.push({ url, init });
    if (url.endsWith("login")) { return json(200, { token: "t1", absolute_expires_at: "2026-11-10T12:00:00Z" }); }
    if (url.endsWith("renew")) { return json(401, { error: "unauthorized" }); }
    throw new Error("offline");
  };
  assert.deepEqual(await signIn("CODE", fetchFn), { ok: true, data: { token: "t1", absolute_expires_at: "2026-11-10T12:00:00Z" } });
  assert.deepEqual(await renewSession("t1", fetchFn), { ok: false, code: "unauthorized" });
  assert.deepEqual(await signOut("t1", fetchFn), { ok: false, code: "network" });
  assert.equal(seen[0].url, "/dashboard/api/login");
  assert.equal(seen[0].init.headers.Authorization, undefined, "login sends no token");
  assert.equal(seen[1].init.headers.Authorization, "Viewer t1");
  assert.equal(seen[0].init.credentials, "omit");
  assert.deepEqual(await checkSession("t1", fetchFn), { ok: false, code: "network" });
  assert.equal(seen[3].url, "/dashboard/api/session");
  assert.deepEqual([seen[3].init.method, seen[3].init.headers.Authorization, seen[3].init.body], ["GET", "Viewer t1", undefined]);
});

test("the first page of a lane is requested once per store epoch", async () => {
  const { firstPageKey } = await import("../static/model.js");
  const store = new Store();
  store.snapshot(envelope(1, [action("r1", "L1", -1)], [{ ...lane("L1", "chat-a"), items: 3 }]));
  const requested = new Set();
  const key = firstPageKey(store, store.lanes.get("L1"), requested);
  assert.ok(key);
  requested.add(key);
  assert.equal(firstPageKey(store, store.lanes.get("L1"), requested), null, "not again in the same epoch");
  store.snapshot(envelope(2, [action("r1", "L1", -1)], [{ ...lane("L1", "chat-a"), items: 3 }]));
  assert.ok(firstPageKey(store, store.lanes.get("L1"), requested), "again after a new snapshot");
  assert.equal(firstPageKey(store, { ...store.lanes.get("L1"), items: 1 }, new Set()), null, "nothing missing");
});
