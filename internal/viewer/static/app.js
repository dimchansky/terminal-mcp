// The dashboard page: sign-in, then the activity view of one live window.
// The viewer token is kept in memory and, for page reloads, in this tab's
// sessionStorage; it is sent only as an Authorization header and never put
// in a URL, cookie or log. Rendering goes through view trees (view.js), so
// recorded text is always inert.

import { Client, checkSession, signIn, renewSession, signOut } from "./net.js";
import { h, materialize } from "./view.js";
import { entry, entryKey, timeline, withDays, jumpTarget, text, fullTime, processKey } from "./cards.js";
import { laneName, laneTime, newestActions, lanePreview, rename, shortIds, laneMatches, hiddenSummary, shownLanes } from "./lanes.js";
import { attentionFor } from "./attention.js";
import { describeStatus, sessionText, pendingNotice, loadErrorText } from "./status.js";
import { reconcile } from "./reconcile.js";
import { icon, conversationKey } from "./identicon.js";
import { parseRecord, field, decode } from "./json.js";
import { laneIdentity, firstPageKey, occurrencesOf, laneTimeline, orderLanes, defaultLane, keeper, earlierCursor } from "./model.js";
import { commandLine } from "./shell.js";
import { isReading, canSee, closeEffects, seenEntries } from "./reading.js";
import { active as filtersActive, apply as applyFilters } from "./filter.js";
import { verb } from "./outcome.js";

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
  const session = sessionText(data.absolute_expires_at);
  $("status").textContent = session.text;
  $("status").title = session.title;
  view.sessionNote = persisted ? "" : "This tab cannot keep the sign-in across reloads.";
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
  titleSignature: "",
  open: new Set(), // Open "Original audit JSONL" blocks, by key.
  entries: new Map(), // Timeline entry key → signature of its rendered element.
  laneOrder: [], // Lane identities in the order last shown.
  pointerInLanes: false,
  loading: "", // A history request in flight.
  loadError: "",
  renewNote: "",
  sessionNote: "", // Why this tab cannot keep its sign-in, if so.
  statusSignature: "", // The header's connection and warnings as last shown.
  pendingSignature: "", // The waiting switch as last shown.
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
  renaming: rename.cancel(), // The alias editor in the chat's title.
  panelOpen: false, // The conversation list is open over the timeline (narrow windows).
  laneQuery: "", // The chat search, in memory only.
  filters: { query: "", tools: new Set(), attention: false }, // The history filters, in memory only.
  filterSignature: "", // The filter bar as last built.
};


// resetView forgets everything shown, including recorded text in the DOM.
function resetView() {
  view.selected = null;
  view.expanded.clear();
  view.open.clear();
  view.entries.clear();
  view.requested.clear();
  view.lanesSignature = view.timelineSignature = view.titleSignature = "";
  view.laneOrder = [];
  view.loading = view.loadError = view.renewNote = "";
  view.controls = null;
  view.shown = null;
  view.renaming = rename.cancel();
  view.newest = null;
  setPanel(false, "reset");
  if (view.midnight) { clearInterval(view.midnight); view.midnight = null; }
  view.archive = null;
  view.visible.clear();
  if (view.observer) { view.observer.disconnect(); view.observer = null; }
  for (const id of ["lane-list", "timeline", "lane-title", "connection", "warnings", "pending", "chats-unread"]) { $(id).replaceChildren(); }
  view.laneQuery = "";
  $("lane-search").value = "";
  view.filters = { query: "", tools: new Set(), attention: false };
  view.filterSignature = "";
  $("filter-query").value = "";
  for (const id of ["filter-tools", "filter-status"]) { $(id).replaceChildren(); }
  $("lanes-hidden").hidden = true;
  $("lanes-hidden-text").textContent = "";
  view.statusSignature = view.pendingSignature = "";
  $("connection").className = "connection";
  $("connection").title = "";
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
  // A small note ("i") is not reading: it never holds a switch back.
  // Hidden blocks (in a closed Details or a collapsed row) do not count.
  const reading = [...box.querySelectorAll("details[open]:not(.info)")].some((d) => !(d.parentElement && d.parentElement.closest("details:not([open]), [hidden]"))) ||
    [...box.querySelectorAll(".group-head[aria-expanded=\"true\"], .row-head[aria-expanded=\"true\"]")].some((b) => !b.closest("[hidden]"));
  const active = document.activeElement;
  const working = active !== box && box.contains(active); // A focused card control, not the box itself.
  // Naming a chat is reading too: a switch would move the title away.
  return isReading({ follow: view.follow, selecting, reading, working, archive: !!view.archive, renaming: view.renaming.editing, panel: view.panelOpen, filtering: filtersActive(view.filters) });
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

// markSeen marks what is in view as seen, only while the tab is visible;
// once the last entry is in view, every entry the timeline shows.
function markSeen() {
  if (!view.attention || !canSee({ visible: document.visibilityState === "visible", archive: !!view.archive, panel: view.panelOpen })) { return; }
  for (const el of view.visible) { if (!el.isConnected) { view.visible.delete(el); } }
  // Nothing to do while the chat shown has nothing unread (the archive view
  // marks nothing, so the live selection is the chat shown).
  if (!view.visible.size || !view.attention.count(view.selected)) { return; }
  const entries = [...$("timeline").children].filter((el) => view.itemsOf.has(el));
  let any = false;
  for (const el of seenEntries(entries, (x) => view.visible.has(x))) {
    if (view.attention.seen(view.itemsOf.get(el))) { any = true; }
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
  renderLanes(); // First: the header's waiting switch names a chat by it.
  renderStatus();
  renderTimeline();
}

// renderStatus fills the header's groups: the connection, the warnings,
// a waiting switch. Each is replaced only when it changed, so a click or
// keyboard focus on Show is never lost between frames.
function renderStatus() {
  const t = describeStatus(client.status, [view.renewNote, view.sessionNote]);
  const c = $("connection");
  // Written only when it changes, so a screen reader is not told again.
  if (c.textContent !== t.connection) { c.textContent = t.connection; }
  c.className = "connection " + t.connectionKind;
  c.title = t.connectionTitle;
  const sig = JSON.stringify(t.warnings);
  if (sig !== view.statusSignature) {
    view.statusSignature = sig;
    $("warnings").replaceChildren(...t.warnings.map((w) => materialize(document, h("span", { class: "warn-chip " + w.kind }, text(w.text)))));
  }
  // A deferred switch is said where auto-switch is, with a way to follow
  // it now. The archive view keeps its own selection, so none is offered.
  const p = pendingNotice(view.attention && view.attention.pending, { archive: !!view.archive, names: view.laneNames || new Map(), filtering: filtersActive(view.filters) });
  const psig = JSON.stringify(p);
  if (psig !== view.pendingSignature) {
    view.pendingSignature = psig;
    $("pending").replaceChildren(...(p ? [materialize(document, h("span", { class: "pending-chip" },
      "New activity in ", h("strong", {}, text(p.name)), ". ", p.why, " ",
      p.canShow ? h("button", { type: "button", "data-action": "show-pending" }, "Show") : null))] : []));
  }
}

function renderLanes() {
  const lanes = lanesInOrder();
  const sel = selectedLane();
  // Replace the list only when it changed: replacing it under the pointer
  // would lose a click between press and release.
  const counts = lanes.map((l) => view.attention.count(laneIdentity(l)));
  const store = source();
  if (!view.newest || view.newest.store !== store || view.newest.version !== store.version) {
    view.newest = { store, version: store.version, map: newestActions(store.items.values()) };
  }
  const newest = view.newest.map;
  const previews = lanes.map((l) => lanePreview(l, newest.get(l.id)));
  const now = new Date();
  // Today's date is part of it, so "Yesterday" and times update after midnight.
  const signature = JSON.stringify([lanes, laneIdentity(sel), [...view.digests.values()].filter(Boolean).length, counts, [...view.aliases],
    previews, now.toDateString(), view.laneQuery]);
  if (signature === view.lanesSignature) { return; }
  view.lanesSignature = signature;
  armMidnight();
  const ids = shortIds(lanes);
  const names = new Map(lanes.map((l) => [laneIdentity(l), laneName(l, { alias: aliasOf(l), shortId: ids.get(laneIdentity(l)) || "" })]));
  view.laneNames = new Map([...names].map(([k, n]) => [k, n.name])); // The chat's title uses the same name.
  const list = $("lane-list");
  const focused = document.activeElement && document.activeElement.parentElement === list ? document.activeElement.dataset.lane : null;
  const existing = new Map([...list.children].map((li) => [li.dataset.lane, li]));
  // A search hides rows, never the selected chat, whose timeline is shown.
  const shown = shownLanes(lanes, sel, aliasOf, view.laneQuery);
  const hidden = hiddenSummary(lanes, shown, counts);
  const selMisses = !!sel && !!view.laneQuery.trim() && !laneMatches(sel, aliasOf(sel), view.laneQuery);
  $("lanes-hidden").hidden = hidden.chats === 0 && !selMisses;
  $("lanes-hidden-text").textContent = hidden.chats
    ? hidden.chats + (hidden.chats === 1 ? " chat" : " chats") + " hidden by the search" + (hidden.unread ? ", " + hidden.unread + " new" : "") + "."
    : selMisses ? "No other chat matches." : "";
  const fresh = lanes.map((l, i) => {
    const n = names.get(laneIdentity(l));
    const unread = counts[i];
    const p = previews[i];
    const when = laneTime(l.last_time, now);
    const node = h("li", { role: "option", tabindex: l === sel ? 0 : -1, "aria-selected": l === sel, "data-lane": laneIdentity(l) },
      icon(l.kind === "conversation" ? digestFor(l) : null),
      // The short ID tells chats apart, so a long client name gives way
      // to it; other client names are counted after the name.
      h("span", { class: "lane-label" },
        n.client ? [h("span", { class: "lane-client" }, text(n.client)), h("span", { class: "lane-id" }, " · ", text(n.shortId))]
          : h("span", { class: "lane-client" }, text(n.name)),
        n.more ? h("span", { class: "more" }, "+" + n.more) : null),
      h("span", { class: "lane-meta" }, when ? h("time", { title: fullTime(l.last_time) }, when) : null,
        unread ? h("span", { class: "unread" }, unread + " new") : null),
      // A preview of the last loaded action, labelled so it never reads
      // as the chat's name; it is the server's hint, not exact input.
      h("span", { class: "lane-sub" }, p
        ? [h("span", { class: "preview-label" }, "Last: "), p.time ? [h("time", { title: fullTime(p.time) }, laneTime(p.time, now)), " "] : null, text(p.verb), p.text ? [": ", text(p.text), p.cut ? "…" : ""] : null]
        : l.items + " item(s)"));
    if (l === sel && !laneMatches(l, aliasOf(l), view.laneQuery)) {
      node.children.push(h("span", { class: "lane-sub no-match" }, "Selected; does not match the search"));
    }
    const li = materialize(document, node);
    li.hidden = !shown[i];
    // Keep the element of a lane and change only its content, so a click
    // that is in progress still lands on the same element.
    const old = existing.get(li.dataset.lane);
    if (!old) { return li; }
    old.hidden = li.hidden;
    for (const name of ["tabindex", "aria-selected", "title"]) {
      if (li.hasAttribute(name)) { old.setAttribute(name, li.getAttribute(name)); } else { old.removeAttribute(name); }
    }
    old.replaceChildren(...li.childNodes);
    return old;
  });
  list.replaceChildren(...fresh);
  // The narrow layout's Chats button carries the unread total.
  const total = counts.reduce((x, y) => x + y, 0);
  $("chats-unread").textContent = total ? " · " + total + " new" : "";
  if (focused) {
    const again = [...list.children].find((li) => li.dataset.lane === focused);
    // A focused row the search now hides gives focus to the selected one.
    if (again && !again.hidden) { again.focus(); } else if (again) {
      const selected = list.querySelector("[aria-selected=\"true\"]");
      if (selected) { selected.focus(); }
    }
  }
}

// armMidnight renders the list again at the next local midnight, so
// "14:53" becomes "Yesterday" without new activity.
function armMidnight() {
  if (view.midnight) { return; }
  let day = new Date().toDateString();
  // A minute's check, not a timer to midnight: it follows clock changes
  // and sleep, and never holds anything open longer than the page.
  view.midnight = setInterval(() => {
    const today = new Date().toDateString();
    if (today !== day) { day = today; view.lanesSignature = ""; schedule(); }
  }, 60 * 1000);
}

// filterKey is the filter state as text, for signatures.
function filterKey() {
  const f = view.filters;
  return JSON.stringify([f.query, [...f.tools].sort(), f.attention]);
}

// renderFilters builds the filter bar: tool chips for the tools of this
// chat (and any active one it lacks), Needs attention, Reset, and a status
// line saying what is active and what the search could and could not see.
function renderFilters(items, stats, filtering, coverage) {
  const f = view.filters;
  const here = new Set(items.filter((it) => it.action && it.action.tool).map((it) => it.action.tool));
  const tools = [...here];
  for (const t of f.tools) { if (!here.has(t)) { tools.push(t); } }
  tools.sort((a, b) => verb(a).localeCompare(verb(b)));
  const absent = tools.filter((t) => !here.has(t));
  // The chips are rebuilt only when the set of tools changes; pressing one
  // only updates it, so focus stays on it.
  const sig = JSON.stringify([tools, absent]);
  if (sig !== view.filterSignature) {
    view.filterSignature = sig;
    const focused = document.activeElement && document.activeElement.closest && document.activeElement.closest("#filter-tools button") ? document.activeElement.dataset.key : null;
    $("filter-tools").replaceChildren(...tools.map((t) => materialize(document, h("button", { type: "button", class: "chip", "data-key": t, title: t },
      text(verb(t)), absent.includes(t) ? " (none here)" : ""))));
    if (focused) { const again = $("filter-tools").querySelector("button[data-key=\"" + CSS.escape(focused) + "\"]"); if (again) { again.focus(); } }
  }
  for (const b of $("filter-tools").querySelectorAll("button[data-key]")) { b.setAttribute("aria-pressed", String(f.tools.has(b.dataset.key))); }
  $("filter-attention").setAttribute("aria-pressed", String(f.attention));
  $("filter-reset").hidden = !filtering;
  let status = "";
  if (filtering) {
    const parts = [stats.matched + (stats.matched === 1 ? " loaded action matches" : " loaded actions match")];
    if (stats.unsearchable && f.query.trim()) { parts.push(stats.unsearchable + " not searchable (record not loaded)"); }
    if (stats.kept) { parts.push(stats.kept + " warning" + (stats.kept === 1 ? "" : "s") + " shown regardless of filters"); }
    if (stats.hiddenUnread) { parts.push(stats.hiddenUnread + " new in hidden items"); }
    status = parts.join(" · ") + ".";
    if (stats.matched === 0) {
      status += " No match in the loaded history. Older or unloaded records were not searched." + (coverage && coverage.end !== "reached_start" ? " " + coverageText(coverage) + "." : "");
    }
  }
  // Said once typing settles, so a screen reader is not told every count.
  if (view.statusTimer) { clearTimeout(view.statusTimer); }
  if (!status || !f.query) { if ($("filter-status").textContent !== status) { $("filter-status").textContent = status; } return; }
  view.statusTimer = setTimeout(() => { view.statusTimer = null; if ($("filter-status").textContent !== status) { $("filter-status").textContent = status; } }, 400);
}

// setFilters changes the filters and shows the result; turning them all
// off returns to the live tail when the owner was following it.
function setFilters(change) {
  const was = filtersActive(view.filters);
  change(view.filters);
  view.renderNonce++;
  render();
  if (was && !filtersActive(view.filters)) {
    if (view.follow) { toTail(); }
    tryResume();
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
  if (e.day) { return (view.archive ? "a" + view.archive.id : "l") + "|day|" + e.day; }
  const items = e.item ? [e.item] : e.items;
  const store = source();
  // The view is part of the signature: live and archive entries never share an element.
  return (view.archive ? "a" + view.archive.id : "l") + "|" + JSON.stringify(items) + "|" + items.map((it) => recordSig(it, store)).join(";") + "|" +
    items.map((it) => ["originals", "details", "meta", "row"].map((k) => view.open.has(it.key + ":" + k)).join(",")).join(";") + "|" + view.expanded.has(entryKey(e)) + "|" + client.status.serverOS + "|" +
    items.map((it) => (ctx.matchWhere && ctx.matchWhere.get(it.key)) || "").join(",") + "|" +
    // A command's card shows what later calls saw of its process.
    JSON.stringify(items.map((it) => {
      const seen = it.action && it.action.tool === "terminal_start" && ctx.processes && ctx.processes.get(processKey(it));
      // Whether its "Show that call" can be offered changes the card too.
      return seen ? [seen, !!jumpTarget(seen.source, ctx.entries, ctx)] : null;
    }));
}

function renderTimeline() {
  const lane = selectedLane();
  const box = $("timeline");
  const src = view.archive ? view.archive.store : client.store;
  const signature = [src.version, laneIdentity(lane), view.renderNonce, view.loading, view.loadError, !!view.archive, filterKey(),
    lane ? aliasOf(lane) + "\u0000" + ((view.laneNames && view.laneNames.get(laneIdentity(lane))) || "") : "", lane ? !!view.digests.get(laneIdentity(lane)) : false, !!view.attention.pending].join("|");
  if (signature === view.timelineSignature) { return; }
  view.timelineSignature = signature;
  if (!lane) {
    renderFilters([], { matched: 0, unsearchable: 0, kept: 0, hiddenUnread: 0 }, filtersActive(view.filters), null);
    $("lane-title").textContent = "No activity loaded yet.";
    view.titleSignature = "";
    box.replaceChildren();
    return;
  }
  const store = src;
  const shown = lane;
  const items = laneTimeline(store, shown.id);
  const ctx = { records: store.records, serverOS: client.status.serverOS, expanded: view.expanded, open: view.open };
  const alias = aliasOf(lane);
  // The title changes only with its own inputs (not with new records), so
  // an open "About this chat" and a selection in it survive new activity.
  const d = lane.kind === "conversation" ? view.digests.get(laneIdentity(lane)) : null;
  const editing = !!(view.renaming.editing && d && view.renaming.digest === hex(d));
  // An editor whose chat is no longer shown is closed, so it never holds
  // auto-switch back on its own.
  if (view.renaming.editing && !editing) { view.renaming = rename.cancel(); }
  const clientNames = [...(lane.client_names || [])].sort();
  const titleSig = [laneIdentity(lane), alias, (view.laneNames && view.laneNames.get(laneIdentity(lane))) || "", !!d,
    !!view.archive, !!view.attention.pending, lane.conversationId || "", lane.identitySource || "", JSON.stringify(clientNames),
    editing, filtersActive(view.filters)].join("\u0000");
  // While the owner types an alias, the title is not rebuilt under them.
  if (titleSig !== view.titleSignature && !(editing && $("lane-title").querySelector("input"))) {
    view.titleSignature = titleSig;
    const source = lane.identitySource || "?";
    $("lane-title").replaceChildren(materialize(document, h("div", {},
      lane.kind === "conversation" ? [
        h("strong", { title: lane.conversationId || "" }, text((view.laneNames && view.laneNames.get(laneIdentity(lane))) || lane.conversationId || "")), " ",
        h("span", { class: "badge claimed", tabindex: 0, title: "Named by the client's own metadata (" + source + "); not verified",
          "aria-label": "client-claimed: named by the client's own metadata (" + source + "), not verified" }, "client-claimed"), " ",
        editing
          ? h("span", { class: "rename-editor" }, h("label", { for: "tm-alias" }, "Alias "),
            h("input", { id: "tm-alias", type: "text", "aria-label": "Alias for this chat (empty removes it)" }),
            h("button", { type: "button", "data-action": "rename-save" }, "Save"), h("button", { type: "button", "data-action": "rename-cancel" }, "Cancel"))
          : d ? h("button", { type: "button", "data-action": "rename", title: "Name this chat in this browser" }, "✎ Rename") : null, " ",
        h("details", { class: "conv-id" }, h("summary", {}, "About this chat"),
          h("dl", { class: "obj" },
            h("dt", {}, "Conversation ID"), h("dd", {}, h("code", {}, text(lane.conversationId || ""))),
            h("dt", {}, "Identity source"), h("dd", {}, text(source)),
            h("dt", {}, "Client names"), h("dd", {}, text(clientNames.join(", ") || "none recorded"))))]
        : h("strong", {}, text((view.laneNames && view.laneNames.get(laneIdentity(lane))) || lane.kind)),
      view.archive ? h("div", { class: "warning" }, "Older history (archive view, separate from the live view). ",
        h("button", { type: "button", "data-action": "close-archive" }, "Back to the live view")) : null,
      view.attention.pending ? h("div", { class: "note" }, "New activity in another chat. ", pendingNotice(view.attention.pending, { filtering: filtersActive(view.filters) }).why) : null,
      h("div", { class: "note" }, "The audit records what was asked for and the outcome, not file contents read or process output."))));
    const input = $("lane-title").querySelector("input");
    if (input) {
      input.value = view.renaming.value;
      input.focus();
      input.select();
    }
  }

  // Keep the reader's place: remember the first visible entry and its offset.
  const atTail = view.follow;
  let anchor = null;
  let anchorOffset = 0;
  for (const el of box.children) {
    const top = el.getBoundingClientRect().top - box.getBoundingClientRect().top;
    // A day separator is never the anchor: its key follows the entry it
    // heads, so it is rebuilt when earlier history arrives.
    if (el.dataset.entry && !el.dataset.day && top + el.offsetHeight > 0) { anchor = el; anchorOffset = top; break; }
  }
  const focused = box.contains(document.activeElement) ? document.activeElement : null;
  const focusKey = !focused ? null : focused.dataset.action ? focused.dataset.action + "\u0000" + (focused.dataset.key || "")
    : focused.tagName === "SUMMARY" && focused.parentElement.dataset.key ? "summary\u0000" + focused.parentElement.dataset.key : null;
  // A "Show that call" button names another call, which a newer
  // observation may change: it is found again within its own entry.
  const focusEntry = focused && focused.dataset.action === "jump" && focused.closest("[data-entry]") ? focused.closest("[data-entry]").dataset.entry : null;

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
  // Observations come from the full lane first, so a hidden Read output
  // still decides a command's known result; then the filters choose what
  // shows. While filtering, polls do not group, so a group never hides a
  // match or a failure.
  const filtering = filtersActive(view.filters);
  let entries = timeline(items, ctx, { group: !filtering });
  let stats = { matched: 0, unsearchable: 0, kept: 0, hiddenUnread: 0 };
  if (filtering) {
    const r = applyFilters(entries, view.filters, ctx, (key) => { const u = view.attention.unread.get(key); return u ? u.unit : undefined; });
    entries = r.entries;
    stats = r.stats;
    ctx.matchWhere = r.matchWhere;
    ctx.entries = entries; // A jump is offered only to a call that shows.
  }
  renderFilters(items, stats, filtering, store.coverage);
  const rendered = withDays(entries);
  view.shown = { entries: ctx.entries, ctx }; // For a jump to a related call.
  for (const e of rendered) {
    const key = entryKey(e);
    const sig = entrySig(e, ctx);
    let el = current.get(key);
    if (!el || view.entries.get(key) !== sig) {
      if (el) { view.observer.unobserve(el); view.visible.delete(el); }
      el = materialize(document, entry(e, ctx));
      el.dataset.entry = key;
      if (e.day) { el.dataset.day = e.day; }
      view.entries.set(key, sig);
      // A day separator is not activity: never seen, never read.
      if (!e.day) { view.observer.observe(el); }
    }
    if (!e.day) { view.itemsOf.set(el, e.item ? [e.item] : e.items); }
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
    const sameEntry = !again && focusEntry ? [...box.children].find((x) => x.dataset.entry === focusEntry) : null;
    const fallback = sameEntry ? sameEntry.querySelector("[data-action=\"jump\"]") : null;
    if (again || fallback) { (again || fallback).focus(); }
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
    case "load-record": {
      // A card that becomes a row once its record loads stays open.
      const owner = button.closest(".card[data-key]");
      if (owner && !owner.classList.contains("compact")) { view.open.add(owner.dataset.key + ":row"); } // Harmless if it never folds.
      if (view.archive) { client.loadArchiveRecord(view.archive.store, key); } else { client.loadRecord(key); }
      break;
    }
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
    case "toggle-row": {
      // A collapsed row keeps no open blocks: they would hide as reading.
      const itemKey = key.slice(0, -":row".length);
      // Open as the row shows it: any of its blocks open.
      if (["row", "details", "originals", "meta"].some((k) => view.open.has(itemKey + ":" + k))) {
        for (const k of ["row", "details", "originals", "meta"]) { view.open.delete(itemKey + ":" + k); }
      } else { view.open.add(key); view.follow = false; }
      view.renderNonce++;
      render();
      if (!["row", "details", "originals", "meta"].some((k) => view.open.has(itemKey + ":" + k))) { tryResume(); }
      break;
    }
    case "jump": {
      // Show the call an observation came from: open its group or row as
      // the owner's own click would, then bring it into view and focus it.
      const t = view.shown && jumpTarget(key, view.shown.entries, view.shown.ctx);
      if (!t) { break; }
      for (const k of t.open) { view.open.add(k); }
      for (const k of t.expanded) { view.expanded.add(k); }
      view.follow = false;
      view.renderNonce++;
      render();
      const target = [...$("timeline").querySelectorAll(".card[data-key]")].find((x) => x.dataset.key === key);
      if (!target) { break; }
      // Centre it in the timeline as the page's own scroll, so following
      // stays off and the next live record does not move it away.
      const box = $("timeline");
      scrollTo(box.scrollTop + target.getBoundingClientRect().top - box.getBoundingClientRect().top - (box.clientHeight - target.offsetHeight) / 2);
      view.follow = false;
      $("jump").hidden = false;
      const head = target.querySelector(":scope > .row-head") || target.querySelector(":scope > .card-head") || target;
      if (!head.matches("button")) {
        // Focusable only until focus leaves it: a later click on its text
        // must not count as working with a control.
        head.tabIndex = -1;
        head.addEventListener("blur", () => head.removeAttribute("tabindex"), { once: true });
      }
      head.focus({ preventScroll: true });
      target.classList.add("flash");
      setTimeout(() => target.classList.remove("flash"), 1600);
      break;
    }
    case "toggle-group":
      if (view.expanded.has(key)) {
        view.expanded.delete(key);
        // A collapsed group keeps no open member blocks: they would hide as reading.
        const group = button.closest(".group");
        for (const m of group ? group.querySelectorAll(".members .card[data-key]") : []) {
          for (const k of ["row", "details", "originals", "meta"]) { view.open.delete(m.dataset.key + ":" + k); }
        }
      } else { view.expanded.add(key); view.follow = false; }
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
      view.renaming = rename.cancel(); // Another view: the title is rebuilt.
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
      view.renaming = rename.cancel();
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
      view.renaming = rename.open(hex(d), view.aliases.get(hex(d)) || "");
      view.renderNonce++;
      render();
      break;
    }
    case "rename-save":
      finishRename(true);
      break;
    case "rename-cancel":
      finishRename(false);
      break;
  }
}

// finishRename closes the alias editor; saving names the chat it was
// opened for, whatever is selected now.
function finishRename(save) {
  const input = $("lane-title").querySelector("input");
  if (save && input) {
    const [closed, change] = rename.save(view.renaming, input.value);
    view.renaming = closed;
    if (change) {
      if (change.alias) { view.aliases.set(change.digest, change.alias); } else { view.aliases.delete(change.digest); }
      saveAliases();
    }
  } else {
    view.renaming = rename.cancel();
  }
  view.lanesSignature = "";
  view.titleSignature = "";
  view.renderNonce++;
  render();
  // Focus returns to Rename; a waiting switch resumes on the next scroll,
  // selection or focus change, not under the owner's hand.
  const again = $("lane-title").querySelector("[data-action=\"rename\"]");
  if (again) { again.focus(); }
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

// NARROW matches the layout where the conversation list is a panel over
// the timeline.
const NARROW = typeof matchMedia === "function" ? matchMedia("(max-width: 48rem)") : null;

// setPanel opens or closes the conversation list over the timeline. The
// timeline itself is never re-rendered by it: its scroll position, open
// blocks and selection stay as they are. While open, the timeline is inert,
// nothing in it counts as seen, and auto-switch waits.
function setPanel(open, reason = "toggle") {
  const app = $("signed-in");
  const toggle = $("chats-toggle");
  if (!app || !toggle) { return; }
  const was = view.panelOpen;
  view.panelOpen = !!open && !!NARROW && NARROW.matches;
  app.classList.toggle("lanes-open", view.panelOpen);
  toggle.setAttribute("aria-expanded", String(view.panelOpen));
  if (view.panelOpen) { $("timeline-wrap").setAttribute("inert", ""); } else { $("timeline-wrap").removeAttribute("inert"); }
  if (view.panelOpen) {
    const sel = $("lane-list").querySelector("[aria-selected=\"true\"]") || $("lane-list").firstElementChild;
    if (sel) { sel.focus(); }
  }
  if (was && !view.panelOpen) {
    const fx = closeEffects(reason);
    if (fx.focus) { toggle.focus(); }
    if (fx.seen) { markSeen(); }
    if (fx.resume) { tryResume(); }
  }
}

// selectLane selects a lane; an automatic switch never moves keyboard focus.
function selectLane(identity, auto = false) {
  if (!auto && view.panelOpen) { setPanel(false, "lane"); } // Choosing a chat closes the list.
  // Choosing another chat by hand closes an open alias editor unsaved.
  if (!auto && view.renaming.editing && identity !== (view.archive ? view.archive.selected : view.selected)) {
    view.renaming = rename.cancel();
    view.titleSignature = "";
  }
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
  // Choosing the chat already shown re-renders nothing, so what is in view
  // (perhaps hidden under the list until now) is marked seen here.
  markSeen();
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
  const items = [...$("lane-list").children].filter((li) => !li.hidden); // Rows a search hides are skipped.
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
  $("chats-toggle").addEventListener("click", () => setPanel(!view.panelOpen, "toggle"));
  document.addEventListener("keydown", (e) => { if (e.key === "Escape" && view.panelOpen) { e.preventDefault(); setPanel(false, "escape"); } });
  // A click outside the open list closes it (the timeline under it is
  // inert, so such clicks land on the page itself).
  $("signed-in").addEventListener("pointerdown", (e) => {
    if (view.panelOpen && !e.target.closest("#lanes, #chats-toggle")) { setPanel(false, e.target.closest(".bar") ? "bar" : "outside"); }
  });
  // Widening the window past the narrow layout closes the panel.
  if (NARROW) { NARROW.addEventListener("change", () => { if (!NARROW.matches && view.panelOpen) { setPanel(false, "resize"); } }); }
  $("pending").addEventListener("click", (e) => {
    const b = e.target.closest("[data-action=\"show-pending\"]");
    const target = view.attention && view.attention.pending;
    if (b && target && !view.archive) {
      selectLane(target);
      // The chip goes away with its button: focus moves to the chat shown
      // (to Chats in the narrow layout, where the list is closed).
      const sel = NARROW && NARROW.matches ? $("chats-toggle") : $("lane-list").querySelector("[aria-selected=\"true\"]");
      if (sel) { sel.focus(); }
    }
  });
  $("retry").addEventListener("click", renew);
  $("again").addEventListener("click", () => { $("login-error").textContent = ""; show("login"); });
  $("timeline").addEventListener("click", onTimelineClick);
  $("lane-title").addEventListener("click", onTimelineClick); // Alias and archive controls.
  $("lane-title").addEventListener("keydown", (e) => {
    if (e.target.id !== "tm-alias" || e.isComposing || e.keyCode === 229) { return; }
    if (e.key === "Enter") { e.preventDefault(); finishRename(true); }
    if (e.key === "Escape") { e.preventDefault(); finishRename(false); }
  });
  $("lane-title").addEventListener("input", (e) => { if (e.target.id === "tm-alias") { view.renaming.value = e.target.value; } });
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
  // Typing renders once per frame, not once per key.
  $("filter-query").addEventListener("input", (e) => {
    const was = filtersActive(view.filters);
    view.filters.query = e.target.value;
    view.renderNonce++;
    schedule();
    if (was && !filtersActive(view.filters)) { requestAnimationFrame(() => { if (view.follow) { toTail(); } tryResume(); }); }
  });
  $("filter-tools").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-key]");
    if (b) { setFilters((f) => { if (f.tools.has(b.dataset.key)) { f.tools.delete(b.dataset.key); } else { f.tools.add(b.dataset.key); } }); }
  });
  $("filter-attention").addEventListener("click", () => setFilters((f) => { f.attention = !f.attention; }));
  $("filter-reset").addEventListener("click", () => {
    $("filter-query").value = "";
    setFilters((f) => { f.query = ""; f.tools.clear(); f.attention = false; });
    $("filter-query").focus();
  });
  $("lane-search").addEventListener("input", (e) => { view.laneQuery = e.target.value; view.lanesSignature = ""; schedule(); });
  $("lane-search-clear").addEventListener("click", () => {
    view.laneQuery = "";
    $("lane-search").value = "";
    view.lanesSignature = "";
    schedule();
    $("lane-search").focus();
  });
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
