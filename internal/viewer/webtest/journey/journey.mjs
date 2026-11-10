// journey.mjs drives the dashboard page's own client, store, attention and
// card modules against a real server, without a DOM. A Go test starts it
// with the server's base URL, sends one JSON command per stdin line and
// reads one JSON answer per stdout line. It is not a *.test.js file, so the
// page test run does not pick it up.
//
// Usage: node journey.mjs http://127.0.0.1:PORT

import { createInterface } from "node:readline";
import { Client, signIn } from "../../static/net.js";
import { laneIdentity, firstPageKey, laneTimeline, orderLanes, defaultLane, keeper, occurrencesOf, earlierCursor } from "../../static/model.js";
import { attentionFor } from "../../static/attention.js";
import { timeline, entry, entryKey } from "../../static/cards.js";
import { describeStatus } from "../../static/status.js";

let base = process.argv[2]; // The rebase command points the page at a restarted server.
if (!base) { process.stderr.write("usage: node journey.mjs <base URL>\n"); process.exit(2); }

// The page's requests are same-origin browser requests; Node's fetch sends
// none of the headers the viewer guard checks, so they are added here.
// A browser refuses a request whose signal is already aborted without any
// traffic; so does this wrapper, and such a request is not counted.
const counts = new Map(); // API path → requests sent.
const recordReads = new Map(); // Occurrence ID → record requests sent.
let streamCut = new AbortController(); // Aborted by the cut command.
const holds = new Map(); // API path → {promise, release}: new requests wait (the hold command).
async function pageFetch(url, init = {}) {
  const path = url.replace("/dashboard/api/", "");
  const held = holds.get(path);
  if (held) {
    const aborted = new Promise((r) => { if (init.signal) { init.signal.addEventListener("abort", r, { once: true }); } });
    await Promise.race([held.promise, aborted]);
  }
  if (init.signal && init.signal.aborted) { throw new DOMException("aborted", "AbortError"); }
  counts.set(path, (counts.get(path) || 0) + 1);
  if (path === "record") {
    const occ = JSON.parse(init.body).occurrence_id;
    recordReads.set(occ, (recordReads.get(occ) || 0) + 1);
  }
  const headers = { ...(init.headers || {}), "Origin": base, "Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty" };
  const signals = [init.signal, path === "stream" ? streamCut.signal : null].filter(Boolean);
  return fetch(base + url, { ...init, headers, signal: signals.length ? AbortSignal.any(signals) : undefined });
}

const storage = new Map();
const memoryStorage = {
  getItem: (k) => (storage.has(k) ? storage.get(k) : null),
  setItem: (k, v) => { storage.set(k, String(v)); },
  removeItem: (k) => { storage.delete(k); },
};

let token = null;
let client = null;
let attention = null;
let clock = 0; // Milliseconds of the injected clock for attention.
let ended = null;
const switches = []; // Lanes auto-switch chose.
let current = ""; // The selected lane identity, as the page's view.selected.
const requested = new Set(); // First pages requested, as the page's view.requested.
let autoSwitch = false;
let deferred = false;

// textOf flattens a view tree to its text, in order; with firstView, the
// card's folded Details section is left out (what shows at a glance).
function textOf(node, firstView = false) {
  if (node === null || node === undefined || node === false) { return ""; }
  if (typeof node === "string" || typeof node === "number") { return String(node); }
  if (Array.isArray(node)) { return node.map((n) => textOf(n, firstView)).join(""); }
  if (firstView && node.attrs && /(^| )more( |$)/.test(node.attrs.class || "")) { return ""; }
  const inner = (node.children || []).map((n) => textOf(n, firstView)).join("");
  return /^(div|p|li|pre|dt|dd|tr|h\d|summary|details|ol|ul|dl|table)$/.test(node.tag) ? inner + "\n" : inner;
}

// actionsOf lists the data-action controls of a view tree.
function actionsOf(node, out = []) {
  if (!node || typeof node !== "object") { return out; }
  if (Array.isArray(node)) { for (const c of node) { actionsOf(c, out); } return out; }
  if (node.attrs && node.attrs["data-action"]) { out.push(node.attrs["data-action"]); }
  for (const c of node.children || []) { actionsOf(c, out); }
  return out;
}

// As app.js deferred(): reading defers a switch, and so does the archive view.
const context = () => ({ autoSwitch, current, deferred: deferred || archive !== null, laneKind: (id) => (client.store.laneByIdentity(id) || {}).kind });

// resume is app.js tryResume: a deferred switch happens once nothing defers it.
function resume() {
  const t = attention.resume(context());
  if (t) { switches.push(t); current = t; attention.selected(); render(); }
}

const laneByIdentity = (identity) => client.store.laneByIdentity(identity);

// render does what the page's render does besides drawing: it selects the
// default lane when none is, and requests the selected lane's first page
// once per store epoch (app.js selectedLane and renderTimeline).
const firstPages = []; // Pending first-page requests.
function render() {
  if (!client) { return; }
  let lane = current ? client.store.laneByIdentity(current) : null;
  if (!lane) { lane = defaultLane(orderLanes([...client.store.lanes.values()])); current = lane ? laneIdentity(lane) : ""; }
  if (!lane) { return; }
  const key = firstPageKey(client.store, lane, requested);
  if (key) { requested.add(key); firstPages.push(client.laneItems(lane.id)); }
}

async function until(cond, what, ms = 15000) {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) { throw new Error("timed out waiting for " + what); }
    await new Promise((r) => setTimeout(r, 10));
  }
}

const settled = () => client.store.pendingRecords().length === 0 && client.loading.size === 0;


// lanesOf summarizes a store's lanes as the driver reports them.
function lanesOf(store) {
  return [...store.lanes.values()].map((l) => ({ identity: laneIdentity(l), kind: l.kind, conversation: l.conversationId || "",
    items: l.items, loaded: store.laneItems(l.id).length, unread: store === client.store ? attention.count(laneIdentity(l)) : 0 }));
}

// renderCards renders a lane's timeline like the page.
function renderCards(store, lane) {
  const items = laneTimeline(store, lane.id);
  const ctx = { records: store.records, serverOS: client.status.serverOS, expanded: new Set(), open: new Set() };
  return timeline(items, ctx).map((e) => {
    const tree = entry(e, ctx);
    const first = e.item || e.items[0];
    const its = e.item ? [e.item] : e.items;
    return { key: entryKey(e), tool: first.action ? first.action.tool : first.kind, size: its.length, text: textOf(tree), head: textOf(tree, true), actions: actionsOf(tree),
      occurrences: its.flatMap((it) => occurrencesOf(it)) };
  });
}

let archive = null; // The archive view: {store, requested}.

const commands = {
  async login({ code }) {
    const r = await signIn(code, pageFetch);
    if (!r.ok) { return { ok: false, code: r.code }; }
    token = r.data.token;
    return { ok: true, token };
  },
  async start() {
    ended = null;
    current = "";
    switches.length = 0;
    firstPages.length = 0;
    requested.clear();
    client = new Client(token, {
      onChange: (changes, info) => {
        if (info.archive) { render(); return; }
        if (info.snapshot || info.record) { attention.refresh(client.store.items); }
        const target = attention.observe(changes, info, context());
        if (target) { switches.push(target); current = target; attention.selected(); }
        render();
      },
      onSessionEnded: (code) => { ended = code; },
    }, { fetch: pageFetch, storage: memoryStorage, channel: () => null, claimWait: 1, backoff: [20, 50, 100] });
    attention = attentionFor(client.store, { now: () => clock });
    client.keep = keeper(() => client.store, () => current); // As the page: the selected lane's records.
    client.start();
    return { ok: true };
  },
  async wait({ connection }) {
    await until(() => client.status.connection === connection || (ended && connection === "ended"), "connection " + connection);
    return { ok: true, connection: client.status.connection, ended };
  },
  async lanes() { return { ok: true, lanes: lanesOf(client.store) }; },
  // select chooses a lane like a click; the page's render then loads its
  // first page. It answers once that page and its records arrived.
  async select({ identity }) {
    if (!laneByIdentity(identity)) { return { ok: false, code: "no_lane" }; }
    current = identity;
    attention.selected();
    return commands.settle();
  },
  // earlier is the page's Earlier in this conversation for the selected
  // lane; it answers whether more is left and waits for the records.
  async earlier() {
    const lane = laneByIdentity(current);
    if (!lane) { return { ok: false, code: "no_lane" }; }
    const r = await client.earlier(lane.id, earlierCursor(client.store, lane.id));
    if (!r.ok) { return { ok: false, code: r.code }; }
    await until(settled, "records");
    return { ok: true, more: !!(r.data && r.data.next), loaded: client.store.laneItems(lane.id).length };
  },
  // settle waits for the first pages the page requested and their records.
  async settle() {
    const end = Date.now() + 15000;
    for (;;) {
      while (firstPages.length) {
        const r = await firstPages.shift();
        if (r && !r.ok) { return { ok: false, code: r.code }; }
      }
      render();
      if (!firstPages.length && settled()) { return { ok: true, current }; }
      if (Date.now() > end) { throw new Error("timed out waiting for first pages and records"); }
      await new Promise((r) => setTimeout(r, 10));
    }
  },
  // cards renders the lane's timeline like the page. With seen, it marks the
  // whole lane seen, as if every card had been in view of a visible tab.
  async cards({ identity, seen }) {
    const lane = laneByIdentity(identity);
    if (!lane) { return { ok: false, code: "no_lane" }; }
    const out = renderCards(client.store, lane);
    if (seen) { attention.seen(laneTimeline(client.store, lane.id)); }
    return { ok: true, cards: out };
  },

  async record({ occurrence }) {
    const r = client.store.records.get(occurrence);
    if (r && !r.bytes) { client.loadRecord(occurrence); await until(() => client.store.records.get(occurrence).bytes || client.store.records.get(occurrence).unavailable, "record"); }
    const now = client.store.records.get(occurrence);
    return { ok: true, b64: now && now.bytes ? Buffer.from(now.bytes).toString("base64") : "", unavailable: !!(now && now.unavailable) };
  },
  // discover is the page's Discover older conversations.
  async discover() {
    const r = await client.discover();
    if (!r.ok) { return { ok: false, code: r.code }; }
    await until(settled, "records");
    return { ok: true, coverage: client.store.coverage };
  },
  // archive opens the archive view (Open older history) and answers its lanes.
  async archive() {
    const r = await client.openArchive();
    if (!r.ok) { return { ok: false, code: r.code }; }
    archive = { store: r.store, requested: new Set() };
    return { ok: true, lanes: lanesOf(archive.store) };
  },
  // archiveCards selects a lane in the archive view, loads its first page
  // like the page does, and renders it.
  async archiveCards({ identity }) {
    const store = archive.store;
    const lane = store.laneByIdentity(identity);
    if (!lane) { return { ok: false, code: "no_lane" }; }
    const key = firstPageKey(store, lane, archive.requested);
    if (key) {
      archive.requested.add(key);
      const r = await client.archivePage(store, "lane-items", lane.id);
      if (!r.ok) { return { ok: false, code: r.code }; }
    }
    await until(() => store.pendingRecords().length === 0 && client.loading.size === 0, "archive records");
    return { ok: true, cards: renderCards(store, lane) };
  },
  async closeArchive() { archive = null; await client.closeArchive(); resume(); return { ok: true }; },
  // item answers the live store's item with this key.
  async item({ key }) { return { ok: true, item: client.store.items.get(key) || null }; },
  async requests() { return { ok: true, counts: Object.fromEntries(counts), records: Object.fromEntries(recordReads) }; },
  async reset() { counts.clear(); recordReads.clear(); return { ok: true }; },
  async status() { return { ok: true, status: client.status, text: describeStatus(client.status), window: client.windowId, generation: client.store.generation,
      loading: client.loading.size, stopped: client.stopped, storage: Object.fromEntries(storage), ended }; },
  // cut ends the open stream and answers once the page noticed it.
  async cut() {
    streamCut.abort();
    streamCut = new AbortController();
    await until(() => client.status.connection !== "live", "the page to notice the cut");
    return { ok: true, connection: client.status.connection };
  },
  // hold makes new requests of a path (default: stream) wait (on: true)
  // until released (on: false); an aborted waiting request is never sent.
  async hold({ on, path = "stream" }) {
    if (on && !holds.has(path)) { let release; const promise = new Promise((r) => { release = r; }); holds.set(path, { promise, release }); }
    if (!on && holds.has(path)) { holds.get(path).release(); holds.delete(path); }
    return { ok: true };
  },
  // load is the page's Load control: it queues a record read and answers
  // at once.
  async load({ occurrence }) { client.loadRecord(occurrence); return { ok: true, loading: client.loading.size }; },
  // cursor answers the live cursor; rewind sets an earlier one and catches
  // up from it, so the page receives rows it already applied again.
  async cursor() { return { ok: true, cursor: client.cursor }; },
  async rewind({ cursor }) {
    client.cursor = cursor;
    await client.catchUp();
    await until(settled, "records");
    return { ok: true, cursor: client.cursor };
  },
  async attention(opts) {
    if (opts.clock !== undefined) { clock = opts.clock; }
    if (opts.autoSwitch !== undefined) { autoSwitch = opts.autoSwitch; }
    if (opts.deferred !== undefined) { deferred = opts.deferred; }
    if (opts.resume) { resume(); }
    return { ok: true, switches: [...switches], pending: attention.pending, current };
  },
  // rebase points the page at a restarted server: it stands for the same
  // page origin reaching a server with new sessions (a real page cannot
  // move to another port).
  async rebase({ url }) { base = url; return { ok: true }; },
  async stop() { if (client) { client.stop(); } return { ok: true }; },
};

const rl = createInterface({ input: process.stdin });
for await (const line of rl) {
  let answer;
  try {
    const msg = JSON.parse(line);
    const fn = commands[msg.cmd];
    answer = fn ? await fn(msg) : { ok: false, code: "unknown_command" };
  } catch (e) {
    answer = { ok: false, code: "driver_error", message: String(e && e.stack || e) };
  }
  process.stdout.write(JSON.stringify(answer) + "\n");
}
if (client) { client.stop(); }
process.exit(0);
