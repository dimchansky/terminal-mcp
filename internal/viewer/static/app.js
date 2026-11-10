// The dashboard page: sign-in, then the activity view of one live window.
// The viewer token is kept in memory and, for page reloads, in this tab's
// sessionStorage; it is sent only as an Authorization header and never put
// in a URL, cookie or log. Rendering goes through view trees (view.js), so
// recorded text is always inert.

import { Client } from "./net.js";
import { h, materialize } from "./view.js";
import { entry, entryKey, timeline, text, localTime } from "./cards.js";
import { reconcile } from "./reconcile.js";
import { icon, conversationKey, shortLabels } from "./identicon.js";
import { laneIdentity, orderCompare } from "./model.js";
import { parseRecord, field, decode } from "./json.js";
import { occurrencesOf } from "./model.js";
import { commandLine } from "./shell.js";

const KEY = "terminal-mcp-viewer-token";
const RENEW_MS = 5 * 60 * 1000;
let renewTimer = null;
let current = null;
let persisted = true;
let client = null;

const $ = (id) => document.getElementById(id);
function load() {
  try { return sessionStorage.getItem(KEY); } catch (e) { return null; }
}
function save(t) {
  current = t;
  try { sessionStorage.setItem(KEY, t); persisted = true; } catch (e) { persisted = false; }
}
function forget() {
  current = null;
  try { sessionStorage.removeItem(KEY); } catch (e) { /* ignore */ }
}

function show(id) {
  for (const s of ["login", "signed-in", "ended", "offline"]) { $(s).hidden = s !== id; }
}

function stopTimers() {
  if (renewTimer !== null) { clearInterval(renewTimer); renewTimer = null; }
}

const reasons = {
  unauthorized: "Your session has ended. Sign in again with a new code.",
  session_replaced: "This session was replaced by a newer sign-in.",
  server_stopping: "The server is stopping or restarting. Sign in again once it is back.",
  rate_limited: "Too many failed attempts. Wait a minute and try again.",
  duplicate_tab: "This dashboard is already open in another tab. Sign in with a new code to view it here as well.",
};
// These end the session for good; anything else may be temporary.
const final = new Set(["unauthorized", "session_replaced", "server_stopping", "duplicate_tab"]);

function failed(code) {
  if (!final.has(code) && client && !client.stopped) {
    // A renewal that failed for a moment does not end a working view; the
    // next renewal retries, and the server ends the session if it must.
    view.renewNote = "Could not renew the session just now; retrying.";
    schedule();
    return;
  }
  stopTimers();
  if (client) { client.stop(); if (final.has(code)) { client.forget(); } client = null; }
  resetView();
  if (final.has(code)) {
    forget(); // Only this tab's copy: a duplicate never signs out the original tab's session.
    $("ended-reason").textContent = reasons[code];
    show("ended");
    return;
  }
  $("offline-reason").textContent = "The server cannot be reached right now.";
  show("offline");
}

async function api(method, path, body, auth) {
  const headers = {};
  if (auth) { headers["Authorization"] = "Viewer " + auth; }
  if (body !== undefined) { headers["Content-Type"] = "application/json"; }
  let resp;
  try {
    resp = await fetch("/dashboard/api/" + path, {
      method, headers, body: body === undefined ? undefined : JSON.stringify(body),
      credentials: "omit", cache: "no-store", redirect: "error", referrerPolicy: "no-referrer",
    });
  } catch (e) {
    return { ok: false, code: "network" };
  }
  let data = null;
  try { data = resp.status === 204 ? null : await resp.json(); } catch (e) { data = null; }
  if (!resp.ok) { return { ok: false, code: (data && typeof data.error === "string") ? data.error : "internal" }; }
  return { ok: true, data };
}

function signedIn(data) {
  const until = new Date(data.absolute_expires_at);
  let s = "Session ends at the latest at " + until.toLocaleString() + ".";
  if (!persisted) { s += " This tab cannot keep the sign-in across reloads."; }
  $("status").textContent = s;
  show("signed-in");
  stopTimers();
  renewTimer = setInterval(renew, RENEW_MS);
  if (!client) { startView(); }
}

async function renew() {
  if (!current) { failed("unauthorized"); return; }
  const r = await api("POST", "renew", undefined, current);
  if (!r.ok) { failed(r.code); return; }
  view.renewNote = "";
  signedIn(r.data);
}

async function login() {
  const button = $("submit");
  if (button.disabled) { return; }
  button.disabled = true;
  $("code").disabled = true;
  $("login-error").textContent = "";
  const code = $("code").value;
  try {
    const r = await api("POST", "login", { code });
    if (!r.ok) {
      $("login-error").textContent = r.code === "rate_limited" ? reasons.rate_limited
        : r.code === "server_stopping" ? reasons.server_stopping
        : r.code === "unauthorized" ? "The code was not accepted."
        : "The server cannot be reached right now.";
      return;
    }
    $("code").value = "";
    save(r.data.token);
    signedIn(r.data);
  } finally {
    button.disabled = false;
    $("code").disabled = false;
  }
}

async function logout() {
  const t = current;
  stopTimers();
  if (client) { client.stop(); client.forget(); client = null; }
  resetView();
  forget();
  if (t) { await api("POST", "logout", undefined, t); }
  $("ended-reason").textContent = "Signed out.";
  show("ended");
}

// ---- The activity view ------------------------------------------------

const view = {
  selected: null, // Lane identity.
  expanded: new Set(), // Poll groups opened by the owner.
  digests: new Map(), // Lane identity → SHA-256 digest (or null while computing).
  requested: new Set(), // Lanes whose first page was requested (per generation).
  renderNonce: 0, // Forces a timeline render after a local view change.
  lanesSignature: "",
  timelineSignature: "",
  open: new Set(), // Open "Original audit JSONL" blocks, by key.
  entries: new Map(), // Timeline entry key → signature of its rendered element.
  laneOrder: [], // Lane identities in the order last shown.
  pointerInLanes: false,
  loading: "", // A history request in flight.
  loadError: "",
  renewNote: "",
  controls: null, // The history controls element, updated in place.
  scheduled: false,
};

const LANE_ORDER = { conversation: 0, unattributed: 1, invalid_metadata: 2, origin_not_loaded: 3, journal: 4 };
const LANE_NAMES = { unattributed: "Unattributed", invalid_metadata: "Invalid metadata", origin_not_loaded: "Pending origin", journal: "Journal" };

// resetView forgets everything shown, including recorded text in the DOM.
function resetView() {
  view.selected = null;
  view.expanded.clear();
  view.open.clear();
  view.entries.clear();
  view.requested.clear();
  view.lanesSignature = view.timelineSignature = "";
  view.laneOrder = [];
  view.loading = view.loadError = view.renewNote = "";
  view.controls = null;
  for (const id of ["lane-list", "timeline", "lane-title", "connection", "health", "notice"]) { $(id).replaceChildren(); }
}

function startView() {
  client = new Client(current, {
    onChange: () => schedule(),
    onStatus: () => schedule(),
    onSessionEnded: (code) => failed(code),
  }, { storage: safeStorage() });
  let kept = null;
  let keptAt = -1;
  client.keep = (id) => {
    if (keptAt !== client.store.version) { kept = keptRecords(); keptAt = client.store.version; }
    return kept.has(id);
  };
  client.start();
}

function safeStorage() {
  try { return sessionStorage; } catch (e) { return null; }
}

function schedule() {
  if (view.scheduled) { return; }
  view.scheduled = true;
  requestAnimationFrame(() => { view.scheduled = false; render(); });
}

function lanesInOrder() {
  const lanes = [...client.store.lanes.values()];
  if (view.pointerInLanes && view.laneOrder.length) {
    // Never reorder under the pointer: keep the order last shown.
    const at = new Map(view.laneOrder.map((id, i) => [id, i]));
    return lanes.sort((a, b) => (at.get(laneIdentity(a)) ?? 1e9) - (at.get(laneIdentity(b)) ?? 1e9));
  }
  const time = (l) => Date.parse(l.last_time || "") || 0;
  lanes.sort((a, b) => (LANE_ORDER[a.kind] - LANE_ORDER[b.kind]) || (time(b) - time(a)) ||
    (laneIdentity(a) < laneIdentity(b) ? -1 : laneIdentity(a) > laneIdentity(b) ? 1 : 0));
  view.laneOrder = lanes.map(laneIdentity);
  return lanes;
}

function selectedLane() {
  if (!client) { return null; }
  const lanes = lanesInOrder();
  let lane = view.selected ? client.store.laneByIdentity(view.selected) : null;
  if (!lane && lanes.length) { lane = lanes.find((l) => l.kind === "conversation") || lanes[0]; view.selected = laneIdentity(lane); }
  return lane;
}

function keptRecords() {
  const out = new Set();
  const lane = selectedLane();
  if (!lane) { return out; }
  for (const item of [...client.store.laneItems(lane.id), ...client.store.restartsIn(lane.id)]) {
    for (const id of occurrencesOf(item)) { out.add(id); }
  }
  return out;
}

function digestFor(lane) {
  const id = laneIdentity(lane);
  if (lane.kind !== "conversation") { return null; }
  if (view.digests.has(id)) { return view.digests.get(id); }
  view.digests.set(id, null);
  crypto.subtle.digest("SHA-256", conversationKey(lane.identitySource || "", lane.conversationId || ""))
    .then((d) => { view.digests.set(id, new Uint8Array(d)); schedule(); });
  return null;
}

function render() {
  if (!client) { return; }
  renderStatus();
  renderLanes();
  renderTimeline();
}

const CONNECTION = {
  connecting: "Connecting…", live: "Live", reconnecting: "Reconnecting…", rebuilding: "Rebuilding the view…",
  offline: "Offline", audit_closed: "Audit unavailable: no live activity", stopped: "Server stopped",
};

function renderStatus() {
  const s = client.status;
  $("connection").textContent = CONNECTION[s.connection] || s.connection;
  const health = s.health && s.health.state && s.health.state !== "ok" ? "Audit " + s.health.state : "";
  const index = s.index && s.index.status && s.index.status !== "ready" ? "In-flight index " + s.index.status.replace("_", " ") : "";
  const outside = s.index && s.index.in_flight_outside && s.index.in_flight_outside.length
    ? s.index.in_flight_outside.length + " running effect(s) outside the loaded history" : "";
  $("health").textContent = [health, index, outside].filter(Boolean).join(" · ");
  const notes = [];
  if (s.possibleUnseen) { notes.push("Some activity may have been missed while the view was rebuilt."); }
  if (s.floorChanged) { notes.push("Older activity was unloaded to stay within memory; use Earlier to see it again."); }
  if (view.renewNote) { notes.push(view.renewNote); }
  $("notice").textContent = notes.join(" ");
}

function renderLanes() {
  const lanes = lanesInOrder();
  const sel = selectedLane();
  // Replace the list only when it changed: replacing it under the pointer
  // would lose a click between press and release.
  const signature = JSON.stringify([lanes, laneIdentity(sel), [...view.digests.values()].filter(Boolean).length]);
  if (signature === view.lanesSignature) { return; }
  view.lanesSignature = signature;
  const conv = lanes.filter((l) => l.kind === "conversation");
  const labels = shortLabels(conv.map((l) => l.conversationId || ""), 16);
  const uses = new Map();
  for (const l of conv) { uses.set(l.conversationId || "", (uses.get(l.conversationId || "") || 0) + 1); }
  const laneName = (l) => {
    const id = l.conversationId || "";
    const base = id ? labels.get(id) : "(empty conversation ID)";
    return uses.get(id) > 1 ? base + " (" + (l.identitySource || "?") + ")" : base;
  };
  const list = $("lane-list");
  const focused = document.activeElement && document.activeElement.parentElement === list ? document.activeElement.dataset.lane : null;
  const existing = new Map([...list.children].map((li) => [li.dataset.lane, li]));
  const fresh = lanes.map((l) => {
    const name = l.kind === "conversation" ? laneName(l) : LANE_NAMES[l.kind] || l.kind;
    const node = h("li", { role: "option", tabindex: l === sel ? 0 : -1, "aria-selected": l === sel, "data-lane": laneIdentity(l) },
    icon(l.kind === "conversation" ? digestFor(l) : null),
    h("span", { class: "lane-name" }, text(name)),
    h("span", {}, l.kind === "conversation" ? h("span", { class: "claimed" }, "client-claimed") : null),
    h("span", { class: "lane-sub" }, l.items + " item(s)", l.client_names && l.client_names.length ? [" · ", text(l.client_names.join(", "))] : null,
      l.last_time ? [" · ", localTime(l.last_time)] : null));
    const li = materialize(document, node);
    // Keep the element of a lane and change only its content, so a click
    // that is in progress still lands on the same element.
    const old = existing.get(li.dataset.lane);
    if (!old) { return li; }
    for (const name of ["tabindex", "aria-selected", "title"]) {
      if (li.hasAttribute(name)) { old.setAttribute(name, li.getAttribute(name)); } else { old.removeAttribute(name); }
    }
    old.replaceChildren(...li.childNodes);
    return old;
  });
  list.replaceChildren(...fresh);
  if (focused) {
    const again = [...list.children].find((li) => li.dataset.lane === focused);
    if (again) { again.focus(); }
  }
}

function coverageText(c) {
  if (!c) { return ""; }
  const ends = { open: "older history can be loaded", reached_start: "the oldest visible record is loaded (not proof of the journal's beginning)",
    gap: "an older journal file is missing", changed: "an older journal file changed", evicted: "older rows were unloaded" };
  return "Coverage: " + (ends[c.end] || c.end) + (c.use_archive ? "; older history needs an archive view" : "");
}

// recordSig summarizes the state of an item's records for its signature.
function recordSig(item) {
  return occurrencesOf(item).map((id) => {
    const r = client.store.records.get(id);
    if (!r) { return "-"; }
    return r.bytes ? "b" + r.bytes.length : r.released ? "r" : r.unavailable ? "u" + r.retryable : r.deferred ? "d" + r.size : "?";
  }).join(",");
}

function entrySig(e) {
  const items = e.item ? [e.item] : e.items;
  return JSON.stringify(items) + "|" + items.map(recordSig).join(";") + "|" +
    items.map((it) => view.open.has(it.key + ":originals")).join("") + "|" + view.expanded.has(entryKey(e)) + "|" + client.status.serverOS;
}

function renderTimeline() {
  const lane = selectedLane();
  const box = $("timeline");
  const signature = [client.store.version, laneIdentity(lane), view.renderNonce, view.loading, view.loadError].join("|");
  if (signature === view.timelineSignature) { return; }
  view.timelineSignature = signature;
  if (!lane) {
    $("lane-title").textContent = "No activity loaded yet.";
    box.replaceChildren();
    return;
  }
  const store = client.store;
  const items = [...store.laneItems(lane.id), ...store.restartsIn(lane.id)].sort((a, b) => orderCompare(a.order, b.order));
  const ctx = { records: store.records, serverOS: client.status.serverOS, expanded: view.expanded, open: view.open };
  $("lane-title").replaceChildren(materialize(document, h("div", {},
    lane.kind === "conversation" ? [h("strong", {}, text(lane.conversationId || "")), " ", h("span", { class: "claimed" }, "client-claimed via ", text(lane.identitySource || ""))]
      : h("strong", {}, LANE_NAMES[lane.kind] || lane.kind),
    h("div", { class: "note" }, "The audit records what was asked for and the outcome, not file contents read or process output."))));

  // Keep the reader's place: remember the first visible entry and its offset.
  const atTail = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
  let anchor = null;
  let anchorOffset = 0;
  for (const el of box.children) {
    const top = el.getBoundingClientRect().top - box.getBoundingClientRect().top;
    if (el.dataset.entry && top + el.offsetHeight > 0) { anchor = el; anchorOffset = top; break; }
  }
  const focused = box.contains(document.activeElement) ? document.activeElement : null;
  const focusKey = !focused ? null : focused.dataset.action ? focused.dataset.action + "\u0000" + (focused.dataset.key || "")
    : focused.tagName === "SUMMARY" && focused.parentElement.dataset.key ? "summary\u0000" + focused.parentElement.dataset.key : null;

  const controlsView = materialize(document, h("div", { class: "history-controls" },
    h("button", { type: "button", "data-action": "earlier", disabled: !!view.loading }, view.loading === "earlier" ? "Loading…" : "Earlier in this conversation"),
    lane.kind === "origin_not_loaded" ? h("button", { type: "button", "data-action": "origins", disabled: !!view.loading }, "Look for origins") : null,
    view.loadError ? h("span", { class: "warning" }, "Could not load: ", text(view.loadError)) : null,
    h("span", {}, coverageText(store.coverage))));
  if (!view.controls) { view.controls = controlsView; } else { view.controls.replaceChildren(...controlsView.childNodes); }
  const controls = view.controls;

  // Reconcile by key: an unchanged entry keeps its element (and with it
  // open blocks, selection and focus); a changed one is rebuilt.
  const current = new Map();
  for (const el of box.children) { if (el.dataset.entry) { current.set(el.dataset.entry, el); } }
  const nodes = [controls];
  const seen = new Set();
  for (const e of timeline(items, ctx)) {
    const key = entryKey(e);
    const sig = entrySig(e);
    let el = current.get(key);
    if (!el || view.entries.get(key) !== sig) {
      el = materialize(document, entry(e, ctx));
      el.dataset.entry = key;
      view.entries.set(key, sig);
    }
    seen.add(key);
    nodes.push(el);
  }
  for (const key of [...view.entries.keys()]) { if (!seen.has(key)) { view.entries.delete(key); } }
  reconcile(box, nodes);

  if (focused && document.activeElement !== focused && focusKey) {
    const again = [...box.querySelectorAll("[data-action], details[data-key] > summary")].find((x) =>
      (x.dataset.action ? x.dataset.action + "\u0000" + (x.dataset.key || "") : "summary\u0000" + x.parentElement.dataset.key) === focusKey);
    if (again) { again.focus(); }
  }
  const requested = lane.id + "\u0000" + store.epoch();
  if (store.laneItems(lane.id).length < lane.items && !view.requested.has(requested)) {
    view.requested.add(requested);
    client.laneItems(lane.id);
  }
  // Place the view in the same frame (reading layout forces it).
  if (atTail) {
    box.scrollTop = box.scrollHeight;
  } else if (anchor && anchor.isConnected) {
    box.scrollTop += (anchor.getBoundingClientRect().top - box.getBoundingClientRect().top) - anchorOffset;
  }
  $("jump").hidden = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
}

function itemByKey(key) { return client.store.items.get(key); }

async function onTimelineClick(ev) {
  const button = ev.target.closest("[data-action]");
  if (!button || !client) { return; }
  const key = button.dataset.key;
  const lane = selectedLane();
  switch (button.dataset.action) {
    case "load-record": client.loadRecord(key); break;
    case "copy-record":
    case "copy-record-base64": {
      const rec = client.store.records.get(key);
      if (!rec || !rec.bytes) { client.loadRecord(key); break; } // Not in memory: load it first.
      // decode keeps a byte order mark, so the copy is the exact record text.
      copy(button, button.dataset.action === "copy-record" ? decode(rec.bytes).text : base64(rec.bytes));
      break;
    }
    case "copy-command": {
      const item = itemByKey(key);
      const rec = client.store.records.get(item.action.start.primary);
      if (!rec || !rec.bytes) { client.loadRecord(item.action.start.primary); break; }
      const args = field(parseRecord(rec.bytes).value, "args");
      const argv = field(args, "argv").items.map((i) => i.value);
      const envNode = field(args, "env");
      const env = envNode ? envNode.entries.map((e) => [e.key, e.value.value]) : [];
      const cmd = commandLine(env, argv);
      if (cmd.exact) { copy(button, cmd.line); }
      break;
    }
    case "toggle-group":
      if (view.expanded.has(key)) { view.expanded.delete(key); } else { view.expanded.add(key); }
      view.renderNonce++;
      render();
      break;
    case "earlier":
    case "origins": {
      if (view.loading) { break; }
      view.loading = button.dataset.action;
      view.loadError = "";
      render();
      const asked = client;
      const oldest = client.store.laneItems(lane.id)[0];
      const r = button.dataset.action === "earlier"
        ? await client.earlier(lane.id, oldest ? { order: oldest.order, key: oldest.key } : undefined)
        : await client.origins();
      if (client !== asked) { break; } // Signed out meanwhile.
      view.loading = "";
      view.loadError = r && !r.ok ? r.code : "";
      render();
      break;
    }
  }
}

function base64(bytes) {
  let bin = "";
  for (let i = 0; i < bytes.length; i += 0x8000) { bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000)); }
  return btoa(bin);
}

// copy writes text to the clipboard and says on the button whether it worked.
async function copy(button, s) {
  const label = button.textContent;
  try {
    await navigator.clipboard.writeText(s);
    button.textContent = "Copied";
  } catch (e) {
    button.textContent = "Copy failed";
  }
  setTimeout(() => { if (button.isConnected) { button.textContent = label; } }, 1500);
}

function selectLane(identity) {
  view.selected = identity;
  render();
  const box = $("timeline");
  box.scrollTop = box.scrollHeight;
}

function onLaneKey(ev) {
  const items = [...$("lane-list").children];
  const i = items.indexOf(document.activeElement);
  if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
    ev.preventDefault();
    const next = items[Math.max(0, Math.min(items.length - 1, i + (ev.key === "ArrowDown" ? 1 : -1)))];
    if (next) { next.focus(); }
  } else if ((ev.key === "Enter" || ev.key === " ") && i >= 0) {
    ev.preventDefault();
    selectLane(items[i].dataset.lane);
  }
}

document.addEventListener("DOMContentLoaded", async () => {
  $("submit").addEventListener("click", login);
  $("code").addEventListener("keydown", (e) => { if (e.key === "Enter") { login(); } });
  $("logout").addEventListener("click", logout);
  $("retry").addEventListener("click", renew);
  $("again").addEventListener("click", () => { $("login-error").textContent = ""; show("login"); });
  $("timeline").addEventListener("click", onTimelineClick);
  $("timeline").addEventListener("toggle", (e) => {
    const d = e.target;
    if (!(d instanceof HTMLDetailsElement) || !d.classList.contains("originals")) { return; }
    if (d.open) { view.open.add(d.dataset.key); } else { view.open.delete(d.dataset.key); }
    view.renderNonce++;
    render();
  }, true);
  $("lane-list").addEventListener("pointerenter", () => { view.pointerInLanes = true; });
  $("lane-list").addEventListener("pointerleave", () => { view.pointerInLanes = false; schedule(); });
  $("timeline").addEventListener("scroll", () => { const b = $("timeline"); $("jump").hidden = b.scrollHeight - b.scrollTop - b.clientHeight < 40; });
  $("jump").addEventListener("click", () => { const b = $("timeline"); b.scrollTop = b.scrollHeight; });
  $("lane-list").addEventListener("click", (e) => { const li = e.target.closest("li[data-lane]"); if (li) { selectLane(li.dataset.lane); } });
  $("lane-list").addEventListener("keydown", onLaneKey);
  $("discover").addEventListener("click", () => client && client.discover());
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible" && renewTimer !== null) { renew(); }
  });
  current = load();
  if (!current) { show("login"); return; }
  const r = await api("GET", "session", undefined, current);
  if (!r.ok) { failed(r.code); return; }
  signedIn(r.data);
});
