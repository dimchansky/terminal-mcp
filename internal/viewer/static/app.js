// The dashboard page: sign-in, then the activity view of one live window.
// The viewer token is kept in memory and, for page reloads, in this tab's
// sessionStorage; it is sent only as an Authorization header and never put
// in a URL, cookie or log. Rendering goes through view trees (view.js), so
// recorded text is always inert.

import { Client, checkSession, signIn, renewSession, signOut } from "./net.js";
import { h, materialize } from "./view.js";
import { entry, entryKey, timeline, text, localTime, processKey } from "./cards.js";
import { attentionFor } from "./attention.js";
import { describeStatus, loadErrorText } from "./status.js";
import { reconcile } from "./reconcile.js";
import { icon, conversationKey, shortLabels } from "./identicon.js";
import { parseRecord, field, decode } from "./json.js";
import { laneIdentity, firstPageKey, occurrencesOf, laneTimeline, orderLanes, defaultLane, keeper, earlierCursor } from "./model.js";
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
  const r = await renewSession(current);
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
    const r = await signIn(code);
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
  if (t) { await signOut(t); }
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
  attention: null,
  autoSwitch: loadPreference(),
  aliases: loadAliases(), // SHA-256 hex of a conversation key → owner alias.
  archive: null, // {store, id} of an open archive window.
  observer: null, // IntersectionObserver marking entries seen.
  visible: new Set(), // Entry elements in view.
  itemsOf: new WeakMap(), // Entry element → its items.
  follow: true, // Following the live tail while the owner is at the bottom.
  programmatic: null, // scrollTop set by the page itself (its scroll event is ignored).
  scheduled: false,
};

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
  view.archive = null;
  view.visible.clear();
  if (view.observer) { view.observer.disconnect(); view.observer = null; }
  for (const id of ["lane-list", "timeline", "lane-title", "connection", "health", "notice"]) { $(id).replaceChildren(); }
}

const AUTO_KEY = "terminal-mcp-dashboard-auto-switch";
const ALIAS_KEY = "terminal-mcp-dashboard-aliases";

// Two non-secret per-browser preferences are kept: auto-switch, and owner
// aliases keyed by a SHA-256 of the conversation key (never the ID).
function loadPreference() { try { return localStorage.getItem(AUTO_KEY) === "1"; } catch (e) { return false; } }
function loadAliases() { try { return new Map(Object.entries(JSON.parse(localStorage.getItem(ALIAS_KEY) || "{}"))); } catch (e) { return new Map(); } }
function saveAliases() { try { localStorage.setItem(ALIAS_KEY, JSON.stringify(Object.fromEntries(view.aliases))); } catch (e) { /* kept for this page only */ } }

function hex(digest) { return [...digest].map((b) => b.toString(16).padStart(2, "0")).join(""); }

function aliasOf(lane) {
  const d = lane.kind === "conversation" ? view.digests.get(laneIdentity(lane)) : null;
  return d ? view.aliases.get(hex(d)) || "" : "";
}

// onChange feeds attention and follows an automatic switch.
function attentionContext() {
  return { autoSwitch: view.autoSwitch, current: view.selected, deferred: deferred(),
    laneKind: (identity) => (client.store.laneByIdentity(identity) || {}).kind };
}

function onChange(changes, info) {
  if (info.archive) { schedule(); return; }
  // Unread is keyed by lane identity: a new snapshot (or a record that
  // reveals a poll target) only moves entries to their current lane.
  if (info.snapshot || info.record) { view.attention.refresh(client.store.items); }
  const target = view.attention.observe(changes, info, attentionContext());
  if (target) { selectLane(target, true); }
  schedule();
}

// tryResume performs a deferred switch once the owner stops reading.
function tryResume() {
  if (!view.attention || !view.attention.pending) { return; }
  const target = view.attention.resume(attentionContext());
  view.renderNonce++;
  if (target) { selectLane(target, true); } else { schedule(); }
}

// deferred: the owner is reading, so auto-switch waits.
function deferred() {
  const box = $("timeline");
  const sel = window.getSelection();
  const selecting = sel && !sel.isCollapsed && box.contains(sel.anchorNode);
  // An open block counts only when it shows: not inside a closed Details.
  const reading = [...box.querySelectorAll("details[open]")].some((d) => !(d.parentElement && d.parentElement.closest("details:not([open])"))) ||
    box.querySelector(".group-head[aria-expanded=\"true\"]") !== null;
  const active = document.activeElement;
  const working = active !== box && box.contains(active); // A focused card control, not the box itself.
  return !view.follow || selecting || reading || working || !!view.archive;
}

function startView() {
  view.observer = new IntersectionObserver((entries) => {
    for (const e of entries) {
      // Seen means really shown: half of the card, or half of the view for a tall one.
      const shown = e.intersectionRatio >= 0.5 || (e.rootBounds && e.intersectionRect.height >= e.rootBounds.height / 2);
      if (shown) { view.visible.add(e.target); } else { view.visible.delete(e.target); }
    }
    markSeen();
  }, { root: $("timeline"), threshold: [0, 0.25, 0.5, 0.75, 1] });
  client = new Client(current, {
    onChange,
    onStatus: () => schedule(),
    onSessionEnded: (code) => failed(code),
  }, { storage: safeStorage() });
  view.attention = attentionFor(client.store);
  // Each store keeps the records of its own selected lane.
  client.keep = keeper(() => client.store, () => view.selected);
  client.archiveKeep = keeper(() => view.archive && view.archive.store, () => view.archive && view.archive.selected);
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

// source is the store on screen: the archive window's while it is open.
function source() { return view.archive ? view.archive.store : client.store; }

function lanesInOrder() {
  const lanes = [...source().lanes.values()];
  if (view.pointerInLanes && view.laneOrder.length) {
    // Never reorder under the pointer: keep the order last shown.
    const at = new Map(view.laneOrder.map((id, i) => [id, i]));
    return lanes.sort((a, b) => (at.get(laneIdentity(a)) ?? 1e9) - (at.get(laneIdentity(b)) ?? 1e9));
  }
  const ordered = orderLanes(lanes);
  view.laneOrder = ordered.map(laneIdentity);
  return ordered;
}

// selectedLane is the lane shown; the archive view keeps its own selection,
// so the live one is intact after going back.
function selectedLane() {
  if (!client) { return null; }
  const lanes = lanesInOrder();
  const slot = view.archive || view;
  let lane = slot.selected ? source().laneByIdentity(slot.selected) : null;
  if (!lane && lanes.length) { lane = defaultLane(lanes); slot.selected = laneIdentity(lane); }
  return lane;
}

// markSeen marks what is in view as seen, only while the tab is visible.
function markSeen() {
  if (document.visibilityState !== "visible" || !view.attention || view.archive) { return; }
  let any = false;
  for (const el of view.visible) {
    if (!el.isConnected) { view.visible.delete(el); continue; }
    const items = view.itemsOf.get(el);
    if (items && view.attention.seen(items)) { any = true; }
  }
  if (any) { view.lanesSignature = ""; schedule(); }
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

function renderStatus() {
  const t = describeStatus(client.status, [view.renewNote]);
  $("connection").textContent = t.connection;
  $("health").textContent = t.health;
  $("notice").textContent = t.notice;
}

function renderLanes() {
  const lanes = lanesInOrder();
  const sel = selectedLane();
  // Replace the list only when it changed: replacing it under the pointer
  // would lose a click between press and release.
  const counts = lanes.map((l) => view.attention.count(laneIdentity(l)));
  const signature = JSON.stringify([lanes, laneIdentity(sel), [...view.digests.values()].filter(Boolean).length, counts, [...view.aliases]]);
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
    const name = l.kind === "conversation" ? (aliasOf(l) || laneName(l)) : LANE_NAMES[l.kind] || l.kind;
    const unread = view.attention.count(laneIdentity(l));
    const node = h("li", { role: "option", tabindex: l === sel ? 0 : -1, "aria-selected": l === sel, "data-lane": laneIdentity(l) },
    icon(l.kind === "conversation" ? digestFor(l) : null),
    h("span", { class: "lane-name" }, text(name)),
    h("span", {}, unread ? h("span", { class: "unread" }, unread + " new") : null,
      l.kind === "conversation" ? h("span", { class: "claimed" }, " client-claimed") : null),
    h("span", { class: "lane-sub" }, aliasOf(l) ? [text(laneName(l)), " · "] : null, l.items + " item(s)", l.client_names && l.client_names.length ? [" · ", text(l.client_names.join(", "))] : null,
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
function recordSig(item, store) {
  return occurrencesOf(item).map((id) => {
    const r = store.records.get(id);
    if (!r) { return "-"; }
    return r.bytes ? "b" + r.bytes.length : r.released ? "r" : r.unavailable ? "u" + r.retryable : r.deferred ? "d" + r.size : "?";
  }).join(",");
}

function entrySig(e, ctx) {
  const items = e.item ? [e.item] : e.items;
  const store = source();
  // The view is part of the signature: live and archive entries never share an element.
  return (view.archive ? "a" + view.archive.id : "l") + "|" + JSON.stringify(items) + "|" + items.map((it) => recordSig(it, store)).join(";") + "|" +
    items.map((it) => ["originals", "details", "meta"].map((k) => view.open.has(it.key + ":" + k)).join(",")).join(";") + "|" + view.expanded.has(entryKey(e)) + "|" + client.status.serverOS + "|" +
    // A command's card shows what later calls saw of its process.
    JSON.stringify(items.map((it) => (it.action && it.action.tool === "terminal_start" && ctx.processes && ctx.processes.get(processKey(it))) || null));
}

function renderTimeline() {
  const lane = selectedLane();
  const box = $("timeline");
  const src = view.archive ? view.archive.store : client.store;
  const signature = [src.version, laneIdentity(lane), view.renderNonce, view.loading, view.loadError, !!view.archive,
    lane ? aliasOf(lane) : "", lane ? !!view.digests.get(laneIdentity(lane)) : false, !!view.attention.pending].join("|");
  if (signature === view.timelineSignature) { return; }
  view.timelineSignature = signature;
  if (!lane) {
    $("lane-title").textContent = "No activity loaded yet.";
    box.replaceChildren();
    return;
  }
  const store = src;
  const shown = lane;
  const items = laneTimeline(store, shown.id);
  const ctx = { records: store.records, serverOS: client.status.serverOS, expanded: view.expanded, open: view.open };
  const alias = aliasOf(lane);
  $("lane-title").replaceChildren(materialize(document, h("div", {},
    lane.kind === "conversation" ? [alias ? [h("strong", {}, text(alias)), " · "] : null, h("strong", {}, text(lane.conversationId || "")), " ",
      h("span", { class: "claimed" }, "client-claimed via ", text(lane.identitySource || "")), " ",
      view.digests.get(laneIdentity(lane)) ? h("button", { type: "button", "data-action": "rename" }, alias ? "Rename or remove alias" : "Set alias") : null]
      : h("strong", {}, LANE_NAMES[lane.kind] || lane.kind),
    view.archive ? h("div", { class: "warning" }, "Older history (archive view, separate from the live view). ",
      h("button", { type: "button", "data-action": "close-archive" }, "Back to the live view")) : null,
    view.attention.pending ? h("div", { class: "note" }, "New activity in another chat; auto-switch waits while you read.") : null,
    h("div", { class: "note" }, "The audit records what was asked for and the outcome, not file contents read or process output."))));

  // Keep the reader's place: remember the first visible entry and its offset.
  const atTail = view.follow;
  let anchor = null;
  let anchorOffset = 0;
  for (const el of box.children) {
    const top = el.getBoundingClientRect().top - box.getBoundingClientRect().top;
    if (el.dataset.entry && top + el.offsetHeight > 0) { anchor = el; anchorOffset = top; break; }
  }
  const focused = box.contains(document.activeElement) ? document.activeElement : null;
  const focusKey = !focused ? null : focused.dataset.action ? focused.dataset.action + "\u0000" + (focused.dataset.key || "")
    : focused.tagName === "SUMMARY" && focused.parentElement.dataset.key ? "summary\u0000" + focused.parentElement.dataset.key : null;

  const archiveButton = !view.archive && store.coverage && store.coverage.use_archive
    ? h("button", { type: "button", "data-action": "open-archive" }, "Open older history") : null;
  const controlsView = materialize(document, h("div", { class: "history-controls" }, archiveButton,
    h("button", { type: "button", "data-action": "earlier", disabled: !!view.loading }, view.loading === "earlier" ? "Loading…" : "Earlier in this conversation"),
    lane.kind === "origin_not_loaded" && !view.archive ? h("button", { type: "button", "data-action": "origins", disabled: !!view.loading }, "Look for origins") : null,
    view.loadError ? h("span", { class: "warning" }, "Could not load: ", text(loadErrorText(view.loadError))) : null,
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
    const sig = entrySig(e, ctx);
    let el = current.get(key);
    if (!el || view.entries.get(key) !== sig) {
      if (el) { view.observer.unobserve(el); view.visible.delete(el); }
      el = materialize(document, entry(e, ctx));
      el.dataset.entry = key;
      view.entries.set(key, sig);
      view.observer.observe(el);
    }
    view.itemsOf.set(el, e.item ? [e.item] : e.items);
    seen.add(key);
    nodes.push(el);
  }
  for (const key of [...view.entries.keys()]) { if (!seen.has(key)) { view.entries.delete(key); } }
  const kept = new Set(nodes);
  for (const el of current.values()) { if (!kept.has(el)) { view.observer.unobserve(el); view.visible.delete(el); } }
  reconcile(box, nodes);
  markSeen();

  if (focused && document.activeElement !== focused && focusKey) {
    const again = [...box.querySelectorAll("[data-action], details[data-key] > summary")].find((x) =>
      (x.dataset.action ? x.dataset.action + "\u0000" + (x.dataset.key || "") : "summary\u0000" + x.parentElement.dataset.key) === focusKey);
    if (again) { again.focus(); }
  }
  const requested = firstPageKey(store, lane, view.requested);
  if (requested) {
    view.requested.add(requested);
    if (view.archive) { client.archivePage(store, "lane-items", lane.id); } else { client.laneItems(lane.id); }
  }
  // Place the view in the same frame (reading layout forces it).
  if (atTail) {
    toTail();
  } else if (anchor && anchor.isConnected) {
    scrollTo(box.scrollTop + (anchor.getBoundingClientRect().top - box.getBoundingClientRect().top) - anchorOffset);
  }
  $("jump").hidden = view.follow;
}

function itemByKey(key) { return client.store.items.get(key); }

async function onTimelineClick(ev) {
  const button = ev.target.closest("[data-action]");
  if (!button || !client) { return; }
  const key = button.dataset.key;
  const lane = selectedLane();
  switch (button.dataset.action) {
    case "load-record": if (view.archive) { client.loadArchiveRecord(view.archive.store, key); } else { client.loadRecord(key); } break;
    case "copy-record":
    case "copy-record-base64": {
      const rec = source().records.get(key);
      if (!rec || !rec.bytes) { if (view.archive) { client.loadArchiveRecord(view.archive.store, key); } else { client.loadRecord(key); } break; }
      // decode keeps a byte order mark, so the copy is the exact record text.
      copy(button, button.dataset.action === "copy-record" ? decode(rec.bytes).text : base64(rec.bytes));
      break;
    }
    case "copy-command": {
      const item = source().items.get(key);
      const rec = source().records.get(item.action.start.primary);
      if (!rec || !rec.bytes) { if (view.archive) { client.loadArchiveRecord(view.archive.store, item.action.start.primary); } else { client.loadRecord(item.action.start.primary); } break; }
      const args = field(parseRecord(rec.bytes).value, "args");
      const argv = field(args, "argv").items.map((i) => i.value);
      const envNode = field(args, "env");
      const env = envNode ? envNode.entries.map((e) => [e.key, e.value.value]) : [];
      const cmd = commandLine(env, argv);
      if (cmd.exact) { copy(button, cmd.line); }
      break;
    }
    case "toggle-group":
      if (view.expanded.has(key)) { view.expanded.delete(key); } else { view.expanded.add(key); view.follow = false; }
      view.renderNonce++;
      render();
      tryResume();
      break;
    case "earlier":
    case "origins": {
      if (view.loading) { break; }
      view.loading = button.dataset.action;
      view.loadError = "";
      render();
      const asked = client;
      let r;
      if (view.archive) {
        const store = view.archive.store;
        r = await client.archivePage(store, "earlier", lane.id, earlierCursor(store, lane.id));
      } else {
        r = button.dataset.action === "earlier"
          ? await client.earlier(lane.id, earlierCursor(client.store, lane.id))
          : await client.origins();
      }
      if (client !== asked) { break; } // Signed out meanwhile.
      view.loading = "";
      view.loadError = r && !r.ok ? r.code : "";
      render();
      break;
    }
    case "open-archive": {
      if (view.loading) { break; }
      view.loading = "archive";
      render();
      const asked = client;
      const r = await client.openArchive();
      if (client !== asked) { break; }
      view.loading = "";
      if (r.ok) { view.archive = { store: r.store, id: r.id, selected: view.selected }; } else { view.loadError = r.code; }
      view.lanesSignature = "";
      view.renderNonce++;
      render();
      break;
    }
    case "close-archive":
      view.archive = null;
      await client.closeArchive();
      view.lanesSignature = "";
      view.renderNonce++;
      render();
      toTail(); // The live view opens at its tail, following it.
      tryResume();
      break;
    case "rename": {
      const d = view.digests.get(laneIdentity(lane));
      if (!d) { break; }
      // The alias stays in this browser, keyed by a hash of the conversation key.
      const next = window.prompt("Alias for this chat (empty removes it):", view.aliases.get(hex(d)) || "");
      if (next === null) { break; }
      if (next.trim()) { view.aliases.set(hex(d), next.trim().slice(0, 80)); } else { view.aliases.delete(hex(d)); }
      saveAliases();
      view.lanesSignature = "";
      view.renderNonce++;
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

// selectLane selects a lane; an automatic switch never moves keyboard focus.
function selectLane(identity, auto = false) {
  if (view.archive) {
    view.archive.selected = identity; // The live selection and a deferred switch stay as they are.
  } else {
    view.selected = identity;
    view.attention.selected();
  }
  view.follow = true;
  if (!auto) { view.lanesSignature = ""; }
  render();
  toTail();
}

// toTail shows the newest entries and keeps following them; card sizes are
// estimated until shown, so the position is corrected once more after layout.
function toTail() {
  const box = $("timeline");
  view.follow = true;
  scrollTo(box.scrollHeight);
  requestAnimationFrame(() => { if (view.follow) { scrollTo(box.scrollHeight); } });
}

// scrollTo moves the timeline; the scroll event it causes is not the
// owner's and must not change following.
function scrollTo(top) {
  const box = $("timeline");
  const before = box.scrollTop;
  box.scrollTop = top;
  if (box.scrollTop !== before) { view.programmatic = box.scrollTop; }
}

// onScroll follows the tail exactly while the owner is at the bottom.
function onScroll() {
  const box = $("timeline");
  if (view.programmatic !== null && Math.abs(box.scrollTop - view.programmatic) < 2) { view.programmatic = null; return; }
  view.programmatic = null;
  const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
  view.follow = atBottom;
  $("jump").hidden = view.follow;
  if (atBottom) { tryResume(); }
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
  $("lane-title").addEventListener("click", onTimelineClick); // Alias and archive controls.
  // Leaving a card control may end reading; focus moves after focusout.
  $("timeline").addEventListener("focusout", () => setTimeout(tryResume, 0));
  $("timeline").addEventListener("toggle", (e) => {
    const d = e.target;
    if (!(d instanceof HTMLDetailsElement)) { return; }
    if (d.classList.contains("originals") || d.classList.contains("more") || d.classList.contains("meta")) {
      if (d.open) { view.open.add(d.dataset.key); view.follow = false; } else { view.open.delete(d.dataset.key); }
      view.renderNonce++;
      render();
    }
    if (!d.open) {
      // Closing a block at the tail follows it again.
      const box = $("timeline");
      if (box.scrollHeight - box.scrollTop - box.clientHeight < 40) { view.follow = true; }
      tryResume();
    }
  }, true);
  $("lane-list").addEventListener("pointerenter", () => { view.pointerInLanes = true; });
  $("lane-list").addEventListener("pointerleave", () => { view.pointerInLanes = false; schedule(); });
  $("timeline").addEventListener("scroll", onScroll);
  $("jump").addEventListener("click", () => { toTail(); tryResume(); });
  document.addEventListener("selectionchange", () => { const s = window.getSelection(); if (s && s.isCollapsed) { tryResume(); } });
  $("lane-list").addEventListener("click", (e) => { const li = e.target.closest("li[data-lane]"); if (li) { selectLane(li.dataset.lane); } });
  $("lane-list").addEventListener("keydown", onLaneKey);
  $("discover").addEventListener("click", () => client && client.discover());
  $("auto-switch").checked = view.autoSwitch;
  $("auto-switch").addEventListener("change", (e) => {
    view.autoSwitch = e.target.checked;
    if (!view.autoSwitch && view.attention) { view.attention.pending = null; view.renderNonce++; schedule(); }
    try { localStorage.setItem(AUTO_KEY, view.autoSwitch ? "1" : "0"); } catch (err) { /* this page only */ }
  });
  document.addEventListener("visibilitychange", () => { markSeen(); });
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible" && renewTimer !== null) { renew(); }
  });
  current = load();
  if (!current) { show("login"); return; }
  const r = await checkSession(current);
  if (!r.ok) { failed(r.code); return; }
  signedIn(r.data);
});
