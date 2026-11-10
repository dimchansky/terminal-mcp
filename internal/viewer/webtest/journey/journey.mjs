// journey.mjs drives the dashboard page's own client, store, attention and
// card modules against a real server, without a DOM. A Go test starts it
// with the server's base URL, sends one JSON command per stdin line and
// reads one JSON answer per stdout line. It is not a *.test.js file, so the
// page test run does not pick it up.
//
// Usage: node journey.mjs http://127.0.0.1:PORT

import { createInterface } from "node:readline";
import { Client, signIn } from "../../static/net.js";
import { laneIdentity, firstPageKey, laneTimeline, orderLanes, defaultLane, keeper } from "../../static/model.js";
import { attentionFor } from "../../static/attention.js";
import { timeline, entry, entryKey } from "../../static/cards.js";
import { describeStatus } from "../../static/status.js";

const base = process.argv[2];
if (!base) { process.stderr.write("usage: node journey.mjs <base URL>\n"); process.exit(2); }

// The page's requests are same-origin browser requests; Node's fetch sends
// none of the headers the viewer guard checks, so they are added here.
// A browser refuses a request whose signal is already aborted without any
// traffic; so does this wrapper, and such a request is not counted.
const counts = new Map(); // API path → requests sent.
let streamCut = new AbortController(); // Aborted by the cut command.
async function pageFetch(url, init = {}) {
  const path = url.replace("/dashboard/api/", "");
  if (init.signal && init.signal.aborted) { throw new DOMException("aborted", "AbortError"); }
  counts.set(path, (counts.get(path) || 0) + 1);
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

// textOf flattens a view tree to its visible text, in order.
function textOf(node) {
  if (node === null || node === undefined || node === false) { return ""; }
  if (typeof node === "string" || typeof node === "number") { return String(node); }
  if (Array.isArray(node)) { return node.map(textOf).join(""); }
  const inner = (node.children || []).map(textOf).join("");
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

const context = () => ({ autoSwitch, current, deferred, laneKind: (id) => (client.store.laneByIdentity(id) || {}).kind });

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
  async lanes() {
    return {
      ok: true,
      lanes: [...client.store.lanes.values()].map((l) => ({ identity: laneIdentity(l), kind: l.kind, conversation: l.conversationId || "",
        items: l.items, loaded: client.store.laneItems(l.id).length, unread: attention.count(laneIdentity(l)) })),
    };
  },
  // select chooses a lane like a click; the page's render then loads its
  // first page. It answers once that page and its records arrived.
  async select({ identity }) {
    if (!laneByIdentity(identity)) { return { ok: false, code: "no_lane" }; }
    current = identity;
    attention.selected();
    return commands.settle();
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
    const items = laneTimeline(client.store, lane.id);
    const ctx = { records: client.store.records, serverOS: client.status.serverOS, expanded: new Set(), open: new Set() };
    const out = timeline(items, ctx).map((e) => {
      const tree = entry(e, ctx);
      const first = e.item || e.items[0];
      return { key: entryKey(e), tool: first.action ? first.action.tool : first.kind, size: e.item ? 1 : e.items.length, text: textOf(tree), actions: actionsOf(tree) };
    });
    if (seen) { attention.seen(items); }
    return { ok: true, cards: out };
  },
  async record({ occurrence }) {
    const r = client.store.records.get(occurrence);
    if (r && !r.bytes) { client.loadRecord(occurrence); await until(() => client.store.records.get(occurrence).bytes || client.store.records.get(occurrence).unavailable, "record"); }
    const now = client.store.records.get(occurrence);
    return { ok: true, b64: now && now.bytes ? Buffer.from(now.bytes).toString("base64") : "", unavailable: !!(now && now.unavailable) };
  },
  async requests() { return { ok: true, counts: Object.fromEntries(counts) }; },
  async reset() { counts.clear(); return { ok: true }; },
  async status() { return { ok: true, status: client.status, text: describeStatus(client.status), window: client.windowId, storage: Object.fromEntries(storage), ended }; },
  async cut() { streamCut.abort(); streamCut = new AbortController(); return { ok: true }; },
  async attention(opts) {
    if (opts.clock !== undefined) { clock = opts.clock; }
    if (opts.autoSwitch !== undefined) { autoSwitch = opts.autoSwitch; }
    if (opts.deferred !== undefined) { deferred = opts.deferred; }
    if (opts.resume) { const t = attention.resume(context()); if (t) { switches.push(t); current = t; attention.selected(); render(); } }
    return { ok: true, switches: [...switches], pending: attention.pending, current };
  },
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
