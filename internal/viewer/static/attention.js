// Per-tab attention: which actions are unread in which lane, and whether
// new activity may move the selection (auto-switch). Nothing here is sent to
// the server or persisted; a reload starts with nothing unread.

import { mayAutoSwitch } from "./classes.js";
import { laneIdentity } from "./model.js";
import { groupTarget } from "./cards.js";

export const HOLD_MS = 5000;

// transition summarizes the parts of an item whose change counts as new
// activity: request and effect states, status, outcome, rejection and the
// possibly-unrecorded marks.
export function transition(item) {
  const a = item.action || {};
  const e = item.effect || {};
  // An effect counts when it finishes (or its status changes), not when it starts.
  return [a.state, a.status, a.outcome, !!a.rejected, !!a.possibly_unrecorded, e.state === "finished", e.status, !!e.possibly_unrecorded].join("|");
}

export class Attention {
  // laneOf(item) → lane identity; unitOf(item) → the logical unit an item
  // counts as (its key, or its poll group); now() → ms.
  constructor({ laneOf, unitOf = (item) => item.key, now = () => Date.now(), hold = HOLD_MS }) {
    this.laneOf = laneOf;
    this.unitOf = unitOf;
    this.now = now;
    this.hold = hold;
    this.known = new Map(); // Item key → transition last delivered.
    this.unread = new Map(); // Item key → {lane, unit}.
    this.heldUntil = 0;
    this.pending = null; // The lane a switch was deferred to while the owner was reading.
  }

  // selected records any selection, manual or automatic: focus is held.
  selected() { this.heldUntil = this.now() + this.hold; this.pending = null; }

  // resume answers the deferred lane once the owner is no longer reading,
  // if auto-switching may still move the selection there; it is dropped
  // otherwise. ctx as for observe.
  resume(ctx) {
    const lane = this.pending;
    if (!lane || ctx.deferred) { return null; }
    this.pending = null;
    if (!ctx.autoSwitch || lane === ctx.current || this.now() < this.heldUntil || ctx.laneKind(lane) !== "conversation") { return null; }
    return lane;
  }

  // observe takes the store's changes with their source. It answers the
  // lane to switch to, if auto-switching may move the selection now.
  // ctx: {autoSwitch, current (lane identity), deferred (bool), laneKind(identity)}.
  observe(changes, info, ctx) {
    const live = !!info.live;
    let candidate = null;
    for (const c of changes) {
      if (c.type === "removed") { this.unread.delete(c.key); this.known.delete(c.key); continue; }
      const item = c.item;
      const t = transition(item);
      if (!live) { this.known.set(item.key, t); this.relane(item); continue; } // History and snapshots are not new activity.
      if (c.type === "moved") { this.relane(item); } // A move itself is not new activity; a change with it is.
      const before = this.known.get(item.key);
      this.known.set(item.key, t);
      if (before === t) { continue; } // A repeated delivery or a lane-only change.
      this.unread.set(item.key, { lane: this.laneOf(item), unit: this.unitOf(item) });
      const a = item.action;
      const newStart = c.type === "added" && a && a.start && item.order && item.order.live;
      if (newStart && mayAutoSwitch(a.tool) && ctx.laneKind(this.laneOf(item)) === "conversation") {
        candidate = this.laneOf(item); // The newest start wins.
      }
    }
    if (!candidate || !ctx.autoSwitch || candidate === ctx.current) { return null; }
    if (this.now() < this.heldUntil) { return null; } // Held: dropped, never replayed late.
    if (ctx.deferred) { this.pending = candidate; return null; }
    return candidate;
  }

  relane(item) {
    const u = this.unread.get(item.key);
    if (u) { u.lane = this.laneOf(item); u.unit = this.unitOf(item); }
  }

  // seen marks items seen (the tab is visible and the owner reached them);
  // it answers whether anything was unread. The items may be older copies
  // than the store holds (a card not re-rendered yet), so they never update
  // known: every delivery already does.
  seen(items) {
    let any = false;
    for (const item of items) { any = this.unread.delete(item.key) || any; }
    return any;
  }

  // refresh recomputes lanes and units of unread items (after a new
  // snapshot or a record load: lane IDs and poll targets may change).
  // items maps key → current item; keys not delivered again keep their lane.
  refresh(items) {
    for (const [key, u] of this.unread) {
      const item = items.get(key);
      if (item) { u.lane = this.laneOf(item); u.unit = this.unitOf(item); }
    }
  }

  // count is the number of unread logical actions of a lane.
  count(lane) {
    const units = new Set();
    for (const u of this.unread.values()) { if (u.lane === lane) { units.add(u.unit); } }
    return units.size;
  }

  // reset forgets everything (a new session).
  reset() { this.unread.clear(); this.known.clear(); this.pending = null; }
}

// attentionFor builds the page's Attention for a store: items count in the
// lane identity of their lane, and polls of one target as one unit.
export function attentionFor(store, options = {}) {
  return new Attention({
    laneOf: (item) => laneIdentity(store.lanes.get(item.lane)) || item.lane,
    unitOf: (item) => { const t = groupTarget(item, { records: store.records }); return t ? "poll\u0000" + t : item.key; },
    ...options,
  });
}
