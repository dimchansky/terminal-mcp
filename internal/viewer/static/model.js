// The page's store for one window: items by key, records by occurrence,
// lanes by ID, all of one generation. Reducers merge what the server sends
// and report what changed, so the attention logic can decide what is new.
// Nothing here is persisted; private data lives in memory only.

// laneIdentity is a lane's identity across generations (lane IDs are valid
// within one generation only): the kind, plus the claimed conversation for
// conversation lanes.
export function laneIdentity(lane) {
  if (!lane) { return ""; }
  if (lane.kind === "conversation") { return "c\u0000" + (lane.identitySource ?? "") + "\u0000" + (lane.conversationId ?? ""); }
  return lane.kind;
}

// orderCompare sorts orders oldest first: history before live, then rank.
export function orderCompare(a, b) {
  if (a.live !== b.live) { return a.live ? 1 : -1; }
  return a.rank - b.rank;
}

function itemCompare(a, b) {
  return orderCompare(a.order, b.order) || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0);
}

// decodeBase64 turns record data into bytes.
export function decodeBase64(b64) {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) { out[i] = bin.charCodeAt(i); }
  return out;
}

// Reference IDs (not weak "w:" or provisional "p:" rows) name the same row
// for the whole writer instance, so their records survive generations.
export function refOccurrence(id) { return !id.startsWith("w:") && !id.startsWith("p:"); }

const OWN = "own";

// occurrencesOf lists every occurrence ID an item's records have.
export function occurrencesOf(item) {
  const out = [];
  const phase = (p) => { if (p) { out.push(...(p.occurrences && p.occurrences.length ? p.occurrences : [p.primary])); } };
  if (item.action) { phase(item.action.start); phase(item.action.finish); phase(item.action.rejected); }
  if (item.effect) { phase(item.effect.start); phase(item.effect.finish); }
  if (item.diagnostic) { out.push(item.diagnostic.key); }
  if (item.server_start) { out.push(item.server_start.key); }
  return out.filter(Boolean);
}

export class Store {
  constructor(maxRecordBytes = 64 << 20) {
    this.maxRecordBytes = maxRecordBytes;
    this.reset();
    this.records = new Map(); // occurrence → {bytes, size, deferred, unavailable, retryable, problem}
    this.recordBytes = 0;
  }

  reset() {
    this.window = null;
    this.generation = null;
    this.items = new Map(); // key → item
    this.lanes = new Map(); // lane ID → lane
    this.envelope = null;
  }

  // snapshot replaces the window's view with an envelope. It returns the
  // lane ID remapping (old ID → new ID, by identity) so lane-keyed page
  // state can follow. Records with reference IDs are kept.
  snapshot(env) {
    const oldLanes = this.lanes;
    // A new window restarts at generation 0 with its own lane IDs, so the
    // view is reset whenever the window or the generation changes.
    const sameGeneration = this.window === env.window && this.generation === env.generation;
    const byIdentity = new Map();
    for (const l of env.lanes) { byIdentity.set(laneIdentity(l), l.id); }
    const remap = new Map();
    for (const [id, l] of oldLanes) {
      const next = byIdentity.get(laneIdentity(l));
      if (next) { remap.set(id, next); }
    }
    if (!sameGeneration) {
      this.reset();
      for (const id of [...this.records.keys()]) {
        if (!refOccurrence(id)) { this.dropRecord(id); }
      }
    }
    this.window = env.window;
    this.generation = env.generation;
    this.envelope = env;
    for (const l of env.lanes) { this.lanes.set(l.id, l); }
    const { changes } = this.apply(env.recent, { history: true });
    if (!sameGeneration) {
      // Records no current item uses are dropped; pages deliver them again.
      const used = new Set();
      for (const item of this.items.values()) { for (const id of occurrencesOf(item)) { used.add(id); } }
      for (const id of [...this.records.keys()]) { if (!used.has(id)) { this.dropRecord(id); } }
    }
    return { remap, changes, generationChanged: !sameGeneration };
  }

  // apply merges a page (snapshot page, lane page, catch-up, scan answer).
  // Items the server stops delivering without a "removed" key (a
  // re-snapshot of the same generation) are kept: the server never drops
  // items within a generation except through removed keys or eviction.
  // history marks answers that are not new activity. It returns the
  // changes: [{type: "added" | "changed" | "moved" | "removed", key, item, previous}].
  apply(page, { history = false } = {}) {
    if (page.generation !== undefined && this.generation !== null && page.generation !== this.generation) {
      return { stale: true, changes: [] };
    }
    const changes = [];
    for (const l of page.lanes || []) { this.lanes.set(l.id, l); }
    for (const r of page.records || []) { this.putRecord(r); }
    const incoming = [...(page.items || []), ...(page.updated || [])];
    for (const item of incoming) {
      const previous = this.items.get(item.key);
      this.items.set(item.key, item);
      if (!previous) {
        changes.push({ type: "added", key: item.key, item, history });
      } else if (previous.lane !== item.lane) {
        changes.push({ type: "moved", key: item.key, item, previous, history });
      } else if (JSON.stringify(previous) !== JSON.stringify(item)) {
        changes.push({ type: "changed", key: item.key, item, previous, history });
      }
      // An effect delivered on its own until now belongs to this action.
      if (item.action && item.effect && item.action.effect && item.action.effect.kind === OWN) {
        const own = "e:" + item.effect.key.server_id + "/" + item.effect.key.operation_id;
        const separate = this.items.get(own);
        if (separate) {
          this.items.delete(own);
          changes.push({ type: "removed", key: own, previous: separate, history });
        }
      }
    }
    for (const key of page.removed || []) {
      const previous = this.items.get(key);
      if (previous) {
        this.items.delete(key);
        changes.push({ type: "removed", key, previous, history });
      }
    }
    return { stale: false, changes };
  }

  putRecord(r) {
    const old = this.records.get(r.occurrence);
    if (r.data_b64) {
      const bytes = decodeBase64(r.data_b64);
      if (old && old.bytes) { this.recordBytes -= old.bytes.length; }
      this.records.set(r.occurrence, { bytes, size: bytes.length, epoch: this.epoch() });
      this.recordBytes += bytes.length;
      return;
    }
    if (old && old.bytes) { return; } // Never replace loaded bytes with a stub.
    this.records.set(r.occurrence, { bytes: null, size: r.size || 0, deferred: !!r.deferred,
      unavailable: !!r.unavailable, retryable: !!r.retryable, problem: r.problem || "" });
  }

  // loaded stores the bytes of a record fetched on its own.
  loaded(occurrence, bytes) {
    const old = this.records.get(occurrence);
    if (old && old.bytes) { this.recordBytes -= old.bytes.length; }
    this.records.set(occurrence, { bytes, size: bytes.length, epoch: this.epoch() });
    this.recordBytes += bytes.length;
  }

  dropRecord(occurrence) {
    const old = this.records.get(occurrence);
    if (!old) { return; }
    if (old.bytes) { this.recordBytes -= old.bytes.length; }
    this.records.delete(occurrence);
  }

  // overBudget reports whether decoded records exceed the memory bound.
  overBudget() { return this.recordBytes > this.maxRecordBytes; }

  // release drops bytes of records that can be fetched again (reference
  // IDs of the current generation), keeping a stub with their size, until
  // the store is within its bound. keep says which occurrences are in use.
  release(keep) {
    for (const [id, r] of this.records) {
      if (!this.overBudget()) { return; }
      if (!r.bytes || keep(id) || !refOccurrence(id) || r.epoch !== this.epoch()) { continue; }
      this.recordBytes -= r.bytes.length;
      this.records.set(id, { bytes: null, size: r.bytes.length, deferred: true, released: true });
    }
  }

  // epoch names the (window, generation) whose record endpoint can serve
  // a record again.
  epoch() { return this.window + "\u0000" + this.generation; }

  // pendingRecords lists records the page loads automatically: those a page
  // deferred for its budget (size 0), and records of loaded items that no
  // answer has carried (a catch-up sends only records that changed).
  pendingRecords() {
    const out = new Set();
    for (const [id, r] of this.records) {
      if (!r.bytes && r.deferred && !r.released && r.size === 0 && refOccurrence(id)) { out.add(id); }
    }
    for (const item of this.items.values()) {
      for (const id of occurrencesOf(item)) {
        if (refOccurrence(id) && !this.records.has(id)) { out.add(id); }
      }
    }
    return [...out];
  }

  // laneItems returns a lane's items oldest first.
  laneItems(laneId) {
    const out = [];
    for (const item of this.items.values()) { if (item.lane === laneId) { out.push(item); } }
    return out.sort(itemCompare);
  }

  // restarts returns server starts within a lane's order range.
  restartsIn(laneId) {
    const own = this.laneItems(laneId);
    if (own.length === 0) { return []; }
    const first = own[0].order;
    const last = own[own.length - 1].order;
    const out = [];
    for (const item of this.items.values()) {
      if (item.kind === "server_start" && item.lane !== laneId &&
        orderCompare(item.order, first) >= 0 && orderCompare(item.order, last) <= 0) { out.push(item); }
    }
    return out.sort(itemCompare);
  }

  laneByIdentity(identity) {
    for (const l of this.lanes.values()) { if (laneIdentity(l) === identity) { return l; } }
    return null;
  }
}
