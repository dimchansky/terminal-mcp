// What the conversation list says about each chat: a stable name, when it
// was last active, and a preview of the last loaded action. Pure: the page
// renders the results, and tests check them without a DOM.

import { orderCompare, laneIdentity } from "./model.js";
import { verb } from "./outcome.js";
import { shortLabels } from "./identicon.js";

// Fixed names of the lanes that are not conversations.
export const LANE_NAMES = { unattributed: "Unattributed", invalid_metadata: "Invalid metadata", origin_not_loaded: "Pending origin", journal: "Journal" };

// shortIds gives each conversation lane (by identity) its distinguishing
// short ID: the conversation ID cut to at least 16 code points and
// lengthened until unique, with the source added when one ID comes from
// several sources. It changes when a colliding ID enters or leaves.
export function shortIds(lanes) {
  const conv = lanes.filter((l) => l.kind === "conversation");
  const labels = shortLabels(conv.map((l) => l.conversationId || ""), 16);
  const uses = new Map();
  for (const l of conv) { uses.set(l.conversationId || "", (uses.get(l.conversationId || "") || 0) + 1); }
  return new Map(conv.map((l) => {
    const id = l.conversationId || "";
    const base = id ? labels.get(id) : "(empty conversation ID)";
    return [laneIdentity(l), uses.get(id) > 1 ? base + " (" + (l.identitySource || "?") + ")" : base];
  }));
}

// laneName names a lane: {name, client, shortId, more}; name is the whole
// text, client and shortId its parts when it is not an alias. A conversation is named by its
// alias; otherwise by a client name and its short ID ("Codex · v1/5bdg…"),
// or by the short ID alone. The client name is the lexicographically
// smallest one, so ordinary new events do not rename a chat; more counts
// the other client names, shown apart from the name. shortId is the
// conversation's distinguishing short label (identicon.js shortLabels).
export function laneName(lane, { alias = "", shortId = "" } = {}) {
  if (lane.kind !== "conversation") {
    return { name: Object.hasOwn(LANE_NAMES, lane.kind) ? LANE_NAMES[lane.kind] : lane.kind, client: "", shortId: "", more: 0 };
  }
  const names = Array.isArray(lane.client_names) ? lane.client_names.filter((n) => typeof n === "string" && n !== "") : [];
  if (alias) { return { name: alias, client: "", shortId: "", more: 0 }; }
  const client = names.length ? [...names].sort()[0] : "";
  return { name: client ? client + " · " + shortId : shortId, client, shortId, more: Math.max(0, names.length - 1) };
}

function sameDay(a, b) {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

// laneTime is a short label for a chat's last activity: "14:53" today,
// "Yesterday", "Nov 9", or "Nov 9, 2025" in another year; "" for no time.
export function laneTime(t, now = new Date()) {
  const d = new Date(t);
  if (!t || Number.isNaN(d.getTime())) { return ""; }
  if (sameDay(d, now)) { return d.toLocaleTimeString(undefined, { hourCycle: "h23", hour: "2-digit", minute: "2-digit" }); }
  const yesterday = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1);
  if (sameDay(d, yesterday)) { return "Yesterday"; }
  const opts = d.getFullYear() === now.getFullYear() ? { month: "short", day: "numeric" } : { month: "short", day: "numeric", year: "numeric" };
  return d.toLocaleDateString(undefined, opts);
}

// newestActions returns, per lane ID, the newest action item the store has
// loaded, by the timeline's order. Only what is already loaded is used:
// nothing is requested for a preview.
export function newestActions(items) {
  const out = new Map();
  for (const it of items) {
    if (!it.action) { continue; }
    const prev = out.get(it.lane);
    if (!prev || orderCompare(prev.order, it.order) < 0 || (orderCompare(prev.order, it.order) === 0 && prev.key < it.key)) { out.set(it.lane, it); }
  }
  return out;
}

// lanePreview describes a lane's newest loaded action: {verb, text, cut,
// time}, where text is the server's summary hint (argv joined with spaces,
// without quoting or env: a hint, never exact) and time is set only when
// the action is not the lane's last activity, so the preview never poses
// as the newest. null when no action of the lane is loaded.
export function lanePreview(lane, item) {
  if (!item || !item.action) { return null; }
  const a = item.action;
  const time = (a.start || a.rejected || a.finish || {}).time || "";
  const hint = item.summary && typeof item.summary.text === "string" ? item.summary.text : "";
  return { verb: verb(a.tool), text: hint, cut: !!(item.summary && item.summary.truncated),
    time: time && lane.last_time && time !== lane.last_time ? time : "" };
}

// Renaming: a small state machine, so the editor names the chat it was
// opened for even if the selection changes meanwhile.
export const rename = {
  open(digest, current) { return { editing: true, digest, value: current || "" }; },
  cancel() { return { editing: false }; },
  // save returns the closed state and the change: {digest, alias}, where an
  // empty alias removes it; aliases are capped at 80 characters.
  save(state, value) {
    if (!state || !state.editing) { return [{ editing: false }, null]; }
    const alias = Array.from(String(value).trim()).slice(0, 80).join("");
    return [{ editing: false }, { digest: state.digest, alias }];
  },
};
