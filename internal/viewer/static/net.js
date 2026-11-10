// The page's connection to its dashboard window: snapshot, live stream,
// catch-up, history requests and record loads, with reconnects. All browser
// facilities (fetch, storage, BroadcastChannel, timers) are injected, so
// the logic runs under Node's test runner with fakes.

import { Store } from "./model.js";

const API = "/dashboard/api/";
const WINDOW_KEY = "terminal-mcp-dashboard-window";
const ARCHIVE_KEY = "terminal-mcp-dashboard-archive";
const CHANNEL = "terminal-mcp-dashboard";

// Codes that end the viewer session; the page signs in again.
// duplicate_tab: this tab is a copy of another tab's session.
export const SESSION_ENDED = new Set(["unauthorized", "session_replaced", "server_stopping", "duplicate_tab"]);
const TERMINAL_STATES = new Set(["failed", "capacity"]); // 409: open another window.

// Queue runs at most `limit` tasks at a time.
export class Queue {
  constructor(limit) { this.limit = limit; this.running = 0; this.waiting = []; }
  run(task) {
    return new Promise((resolve, reject) => {
      const start = () => {
        this.running++;
        Promise.resolve().then(task).then(resolve, reject).finally(() => {
          this.running--;
          const next = this.waiting.shift();
          if (next) { next(); }
        });
      };
      if (this.running < this.limit) { start(); } else { this.waiting.push(start); }
    });
  }
}

// sseParser feeds text chunks and calls onEvent(name, data) per event.
export function sseParser(onEvent) {
  let buffer = "";
  let name = "";
  let data = "";
  return (chunk) => {
    buffer += chunk;
    let nl;
    while ((nl = buffer.indexOf("\n")) >= 0) {
      const line = buffer.slice(0, nl).replace(/\r$/, "");
      buffer = buffer.slice(nl + 1);
      if (line === "") {
        if (name) { onEvent(name, data ? JSON.parse(data) : null); }
        name = ""; data = "";
      } else if (line.startsWith("event: ")) {
        name = line.slice(7);
      } else if (line.startsWith("data: ")) {
        data = line.slice(6);
      }
    }
  };
}

function abortableSleep(ms, signal) {
  return new Promise((resolve) => {
    if (signal && signal.aborted) { resolve(); return; }
    const done = () => { clearTimeout(t); if (signal) { signal.removeEventListener("abort", done); } resolve(); };
    const t = setTimeout(done, ms);
    if (signal) { signal.addEventListener("abort", done, { once: true }); }
  });
}

const defaults = {
  fetch: (...a) => globalThis.fetch(...a),
  storage: null, // sessionStorage-like; null keeps nothing.
  channel: () => (typeof BroadcastChannel === "function" ? new BroadcastChannel(CHANNEL) : null),
  sleep: abortableSleep,
  claimWait: 300,
  backoff: [1000, 2000, 5000, 10000, 30000],
};

// Client keeps one live window in sync. Callbacks: onChange(changes, info)
// after every applied answer, onStatus(status) on state changes,
// onSessionEnded(code) when this tab's viewer session ends.
export class Client {
  constructor(token, callbacks, options = {}) {
    this.token = token;
    this.cb = { onChange() {}, onStatus() {}, onSessionEnded() {}, ...callbacks };
    this.o = { ...defaults, ...options };
    this.store = new Store();
    this.pages = new Queue(1); // Page slots: 2 per session; one stays free for the stream start and archive.
    this.records = new Queue(2); // Detail slots.
    this.loading = new Set();
    this.abort = new AbortController();
    this.windowId = null;
    this.archiveId = null;
    this.cursor = null;
    this.catching = null;
    this.catchAgain = false;
    this.catchDirty = false;
    this.status = { connection: "connecting" };
    this.nonce = Math.random().toString(36).slice(2) + Date.now().toString(36);
    this.channel = null;
    this.keep = () => false; // The page says which records are in use.
    this.archiveKeep = () => false; // The same for the archive window.
    this.held = new Set(); // Window IDs this tab has held (for duplicate-tab probes).
  }

  backoff(n) { return this.o.backoff[Math.min(n, this.o.backoff.length - 1)]; }

  sleep(ms) { return this.o.sleep(ms, this.abort.signal); }

  // request posts to an API route; it answers {ok, data} or
  // {ok: false, code, status, retryAfter (seconds, or 0)}.
  async request(path, body, { bytes = false } = {}) {
    let resp;
    try {
      resp = await this.o.fetch(API + path, {
        method: "POST", headers: { "Authorization": "Viewer " + this.token, "Content-Type": "application/json" },
        body: JSON.stringify(body), credentials: "omit", cache: "no-store", redirect: "error",
        referrerPolicy: "no-referrer", signal: this.abort.signal,
      });
    } catch (e) {
      return { ok: false, code: this.abort.signal.aborted ? "aborted" : "network", status: 0, retryAfter: 0 };
    }
    try {
      if (resp.ok) {
        if (bytes) { return { ok: true, data: new Uint8Array(await resp.arrayBuffer()) }; }
        return { ok: true, data: resp.status === 204 ? null : await resp.json() };
      }
      let code = "internal";
      try { const e = await resp.json(); if (typeof e.error === "string") { code = e.error; } } catch (e) { /* keep internal */ }
      const retry = Number(resp.headers.get("Retry-After"));
      this.sessionCheck(code);
      return { ok: false, code, status: resp.status, retryAfter: Number.isFinite(retry) && retry > 0 ? retry : 0 };
    } catch (e) {
      return { ok: false, code: "network", status: 0, retryAfter: 0 }; // The body was cut.
    }
  }

  sessionCheck(code) {
    if (code === "server_stopping") { this.setStatus({ connection: "stopped" }); }
    if (SESSION_ENDED.has(code)) { this.end(code); }
  }

  // page runs a page-slot request, retrying a busy slot.
  async page(path, body) {
    let r;
    for (let attempt = 0; attempt < 5 && !this.stopped; attempt++) {
      r = await this.pages.run(() => this.request(path, body));
      if (r.ok || !(r.status === 429 && r.code === "capacity")) { return r; }
      await this.sleep((r.retryAfter || 1) * 1000);
    }
    return r || { ok: false, code: "aborted", status: 0, retryAfter: 0 };
  }

  setStatus(patch) {
    this.status = { ...this.status, ...patch };
    this.cb.onStatus(this.status);
  }

  end(code) {
    if (this.ended) { return; }
    this.ended = true;
    this.stop();
    this.forget();
    this.cb.onSessionEnded(code);
  }

  stop() {
    this.stopped = true;
    this.abort.abort();
    if (this.channel) { this.channel.close(); this.channel = null; }
  }

  // forget clears the stored window IDs (sign-out, session end).
  forget() {
    for (const k of [WINDOW_KEY, ARCHIVE_KEY]) {
      try { this.o.storage?.removeItem(k); } catch (e) { /* storage may be unavailable */ }
    }
  }

  remember(key, id) { try { this.o.storage?.setItem(key, id); } catch (e) { /* the window is then not reused */ } }

  stored(key) { try { return this.o.storage?.getItem(key) || null; } catch (e) { return null; } }

  // epoch identifies the current window and generation; answers to requests
  // made under another epoch are discarded.
  epoch() { return this.windowId + "\u0000" + this.store.generation; }

  // start reuses this tab's window after a reload, or opens one, then
  // keeps it live until stopped.
  async start() {
    this.channel = this.o.channel();
    this.listen();
    const id = this.stored(WINDOW_KEY);
    if (id) {
      if (!await this.claim(id)) {
        this.end("duplicate_tab"); // A copy of another tab: it signs in for its own session.
        return;
      }
      const stale = this.stored(ARCHIVE_KEY);
      if (stale) { // An archive panel does not survive a reload (of this tab, not of a copy).
        this.request("close", { window: stale });
        try { this.o.storage?.removeItem(ARCHIVE_KEY); } catch (e) { /* ignore */ }
      }
      const r = await this.resnapshot();
      if (r === "ok") { return this.loop(); }
      if (r !== "reopen") { return; }
    }
    if (!await this.open()) { return; }
    return this.loop();
  }

  // claim asks other tabs (a duplicated tab copies sessionStorage) whether
  // they hold the window; probes from tabs claiming the same window at the
  // same time are resolved by nonce, so at most one keeps it.
  claim(id) {
    const ch = this.channel;
    if (!ch) { this.windowId = id; this.held.add(id); return Promise.resolve(true); }
    return new Promise((resolve) => {
      let yielded = false;
      const onMessage = (ev) => {
        const m = ev.data || {};
        if (m.window !== id) { return; }
        if (m.type === "held" && m.to === this.nonce) { yielded = true; }
        if (m.type === "probe" && m.nonce < this.nonce) { yielded = true; }
      };
      this.claiming = id;
      ch.addEventListener("message", onMessage);
      ch.postMessage({ type: "probe", window: id, nonce: this.nonce });
      this.sleep(this.o.claimWait).then(() => {
        ch.removeEventListener("message", onMessage);
        this.claiming = null;
        if (!yielded) { this.windowId = id; this.held.add(id); }
        resolve(!yielded);
      });
    });
  }

  // listen answers other tabs' probes for the window this tab holds, and,
  // while claiming, for the window it claims when its own nonce wins.
  listen() {
    if (!this.channel) { return; }
    this.channel.addEventListener("message", (ev) => {
      const m = ev.data || {};
      if (m.type !== "probe" || this.stopped) { return; }
      const held = this.held.has(m.window); // Every window this tab has held, even while reopening.
      const wins = m.window === this.claiming && m.nonce > this.nonce;
      if (held || wins) { this.channel.postMessage({ type: "held", window: m.window, to: m.nonce }); }
    });
  }

  // open opens a new live window, retrying with a growing backoff. The
  // window it replaces is closed, so it does not hold one of the session's
  // window slots until the idle release.
  async open() {
    if (this.windowId) { await this.request("close", { window: this.windowId }); } // Free its slot first.
    this.windowId = null;
    for (let n = 0; !this.stopped; n++) {
      const r = await this.page("window", { mode: "live" });
      if (r.ok) {
        this.windowId = r.data.window;
        this.held.add(this.windowId);
        this.remember(WINDOW_KEY, this.windowId);
        return this.applySnapshot(r.data);
      }
      if (SESSION_ENDED.has(r.code)) { return false; }
      if (r.code === "window_limit" && this.archiveId) { await this.closeArchive(); continue; }
      this.setStatus({ connection: "offline", reason: r.code, reasonStatus: r.status });
      await this.sleep(r.retryAfter ? r.retryAfter * 1000 : this.backoff(n));
    }
    return false;
  }

  // resnapshot takes a new snapshot of the current window, waiting out
  // rebuilds and transient errors. It answers "ok", "reopen" (the window
  // is gone or terminal) or "stop".
  async resnapshot() {
    for (let n = 0; !this.stopped; n++) {
      const r = await this.page("window", { window: this.windowId });
      if (r.ok) { return this.applySnapshot(r.data) ? "ok" : "stop"; }
      if (SESSION_ENDED.has(r.code)) { return "stop"; }
      if (r.code === "window_gone" || (r.status === 409 && TERMINAL_STATES.has(r.code))) { return "reopen"; }
      if (r.code === "disabled") { this.auditUnavailable(); return "stop"; }
      this.setStatus({ connection: r.code === "rebuilding" ? "rebuilding" : "reconnecting", reason: r.code, reasonStatus: r.status });
      await this.sleep(r.retryAfter ? r.retryAfter * 1000 : this.backoff(n));
    }
    return "stop";
  }

  auditUnavailable() { this.setStatus({ connection: "audit_closed" }); }

  // applySnapshot replaces the view; false means the window cannot stream
  // (the audit is disabled).
  applySnapshot(env) {
    const { remap, changes, generationChanged } = this.store.snapshot(env);
    this.cursor = env.live_cursor;
    this.setStatus({ reason: "", reasonStatus: 0, state: env.state, possibleUnseen: env.possible_unseen, health: env.health, index: env.index,
      needs: env.needs, coverage: env.coverage, serverOS: env.server_os });
    this.cb.onChange(changes, { snapshot: true, remap, generationChanged });
    this.loadRecords();
    if (env.state === "disabled") { this.auditUnavailable(); return false; }
    return true;
  }

  // loop runs the stream and reconnects until a final state.
  async loop() {
    let failures = 0;
    let reopens = 0; // Consecutive reopens without a ready stream.
    let changed = 0; // Consecutive generation changes without a ready stream.
    while (!this.stopped) {
      const end = await this.streamOnce();
      if (this.stopped) { return; }
      if (end.type === "start_error" && end.code === "generation_changed") { changed++; } else { changed = 0; }
      if (changed >= 2) { await this.sleep(this.backoff(changed - 2)); }
      let next = "backoff";
      switch (end.type) {
        case "ready":
          failures = 0;
          reopens = 0;
          break;
        case "resync":
          next = await this.resnapshot();
          break;
        case "window_gone":
          next = "reopen";
          break;
        case "window_state": // A terminal window state: say which, then open another window.
          this.setStatus({ connection: "offline", reason: (end.data && end.data.state) || "failed", reasonStatus: 409 });
          next = "reopen";
          break;
        case "audit_closed":
          this.auditUnavailable();
          return;
        case "server_stopping":
          this.setStatus({ connection: "stopped", reason: end.data && end.data.reason });
          this.end("server_stopping");
          return;
        case "start_error":
          if (end.code === "generation_changed") { next = await this.resnapshot(); }
          else if (end.code === "window_gone") { next = "reopen"; }
          else if (end.status === 409 && TERMINAL_STATES.has(end.code)) {
            this.setStatus({ connection: "offline", reason: end.code, reasonStatus: 409 });
            next = "reopen";
          }
          else if (end.code === "disabled") { this.auditUnavailable(); return; }
          else if (SESSION_ENDED.has(end.code)) { return; }
          break;
        default:
          break;
      }
      if (next === "stop") { return; }
      if (next === "reopen") {
        // A window that keeps ending (capacity, failed) must not spin:
        // the second reopen in a row waits with a growing backoff.
        if (reopens > 0) { await this.sleep(this.backoff(reopens - 1)); }
        reopens++;
        if (this.stopped || !await this.open()) { return; }
        failures = 0;
        continue;
      }
      if (next === "ok") { failures = 0; continue; }
      // Ended without a final event (network, an error event): back off.
      this.setStatus({ connection: "reconnecting", reason: end.type === "start_error" ? end.code : end.type === "network" ? "network" : "",
        reasonStatus: end.status || 0 });
      await this.sleep(end.retryAfter ? end.retryAfter * 1000 : this.backoff(failures));
      failures++;
    }
  }

  // streamOnce reads one stream until it ends; it returns how it ended.
  async streamOnce() {
    let resp;
    try {
      resp = await this.o.fetch(API + "stream", {
        method: "POST", headers: { "Authorization": "Viewer " + this.token, "Content-Type": "application/json" },
        body: JSON.stringify({ window: this.windowId, generation: this.store.generation, after: this.cursor }),
        credentials: "omit", cache: "no-store", redirect: "error", referrerPolicy: "no-referrer", signal: this.abort.signal,
      });
    } catch (e) {
      return { type: "network" };
    }
    if (!resp.ok) {
      let code = "internal";
      try { const e = await resp.json(); if (typeof e.error === "string") { code = e.error; } } catch (e) { /* keep */ }
      this.sessionCheck(code);
      const retry = Number(resp.headers.get("Retry-After"));
      return { type: "start_error", code, status: resp.status, retryAfter: Number.isFinite(retry) && retry > 0 ? retry : 0 };
    }
    let final = { type: "ended" };
    let ready = false;
    const parse = sseParser((name, data) => {
      switch (name) {
        case "ready":
          ready = true;
          this.setStatus({ connection: "live", reason: "" });
          break;
        case "activity":
          this.setStatus({ waiting: data.waiting, possibleUnseen: data.possible_unseen, health: data.health, index: data.index });
          this.catchUp();
          break;
        case "heartbeat":
          this.setStatus({ connection: "live", possibleUnseen: data.possible_unseen, health: data.health, index: data.index });
          if (this.catchDirty) { this.catchUp(); }
          break;
        case "rebuilding":
          this.setStatus({ connection: "rebuilding", retryAfter: data.retry_after });
          break;
        default:
          final = { type: name, data };
      }
    });
    const reader = resp.body.getReader();
    try {
      const decoder = new TextDecoder();
      for (;;) {
        const { value, done } = await reader.read();
        if (done) { break; }
        parse(decoder.decode(value, { stream: true }));
        if (final.type !== "ended") { break; }
      }
    } catch (e) {
      // The connection ended; reconnect below.
    }
    reader.cancel().catch(() => {}); // Never leave the previous fetch open.
    return final.type === "ended" && ready ? { type: "ready" } : final;
  }

  // catchUp fetches what changed since the cursor; requests arriving while
  // one runs coalesce into one more round, and a failed round is retried.
  catchUp() {
    if (this.catching) { this.catchAgain = true; return this.catching; }
    this.catching = (async () => {
      try {
        let failures = 0;
        do {
          this.catchAgain = false;
          const r = await this.catchUpOnce();
          if (r.ok) { failures = 0; this.catchDirty = false; continue; }
          if (r.stop || this.stopped) { return; }
          this.catchDirty = true; // Also retried on the next heartbeat.
          this.catchAgain = true;
          await this.sleep(r.retryAfter ? r.retryAfter * 1000 : this.backoff(failures++));
        } while (this.catchAgain && !this.stopped);
      } catch (e) {
        this.catchDirty = true;
      } finally {
        this.catching = null;
      }
    })();
    return this.catching;
  }

  // catchUpOnce follows next pages and more rounds; it answers {ok} or
  // {ok: false, retryAfter, stop}.
  async catchUpOnce() {
    for (let rounds = 0; !this.stopped; rounds++) {
      if (rounds >= 20) { this.catchAgain = true; return { ok: true }; } // Yield; continue in the next round.
      const after = this.cursor;
      const epoch = this.epoch();
      const generation = this.store.generation;
      let r = await this.page("catch-up", { window: this.windowId, generation, after });
      for (;;) {
        if (!r.ok) {
          if (r.code === "generation_changed") { return { ok: (await this.resnapshot()) === "ok", stop: true }; }
          if (SESSION_ENDED.has(r.code)) { return { ok: false, stop: true }; }
          return { ok: false, retryAfter: r.retryAfter };
        }
        if (epoch !== this.epoch()) { return { ok: true }; } // A snapshot replaced the view meanwhile.
        this.applyLive(r.data);
        if (!r.data.next) { break; }
        r = await this.page("catch-up", { window: this.windowId, generation, after, before: r.data.next });
      }
      if (epoch !== this.epoch()) { return { ok: true }; }
      this.cursor = r.data.head;
      this.setStatus({ state: r.data.state, possibleUnseen: r.data.possible_unseen, index: r.data.index, needs: r.data.needs,
        floorChanged: !!r.data.floor_changed || !!this.status.floorChanged });
      if (!r.data.more) { return { ok: true }; }
    }
    return { ok: false, stop: true };
  }

  applyLive(page) {
    const { stale, changes } = this.store.apply(page);
    if (!stale) { this.cb.onChange(changes, { live: true }); }
    this.loadRecords();
  }

  applyHistory(page) {
    const { stale, changes } = this.store.apply(page, { history: true });
    if (!stale) { this.cb.onChange(changes, { history: true }); }
    this.loadRecords();
    return !stale;
  }

  // loadRecords loads records the page needs, two at a time, and keeps
  // the decoded records within their memory bound.
  loadRecords() {
    for (const id of this.store.pendingRecords()) { this.loadRecord(id); }
    this.store.release(this.keep);
  }

  // loadRecord loads one complete record (also used by the card's control).
  loadRecord(id) {
    if (this.loading.has(id)) { return; }
    this.loading.add(id);
    const epoch = this.epoch();
    this.records.run(async () => {
      for (let attempt = 0; attempt < 5 && !this.stopped && epoch === this.epoch(); attempt++) {
        const r = await this.request("record", { window: this.windowId, occurrence_id: id }, { bytes: true });
        if (epoch !== this.epoch() || this.stopped) { return; }
        if (r.ok) {
          this.store.loaded(id, r.data);
          this.store.release(this.keep);
          this.cb.onChange([], { record: id });
          return;
        }
        if (r.status === 429) { await this.sleep((r.retryAfter || 1) * 1000); continue; }
        this.store.putRecord({ occurrence: id, unavailable: true, retryable: r.status === 0 || r.status >= 500, problem: r.code });
        this.cb.onChange([], { record: id });
        return;
      }
    }).finally(() => this.loading.delete(id));
  }

  // laneItems loads a lane's newest page (or the page before `before`).
  async laneItems(laneId, before) {
    const epoch = this.epoch();
    const r = await this.page("lane-items", before ? { window: this.windowId, lane_id: laneId, before } : { window: this.windowId, lane_id: laneId });
    if (r.ok && epoch === this.epoch()) { this.applyHistory(r.data); }
    return r;
  }

  // earlier loads older items of a lane, scanning older history if needed.
  async earlier(laneId, before) {
    const epoch = this.epoch();
    const r = await this.page("earlier", before ? { window: this.windowId, lane_id: laneId, before } : { window: this.windowId, lane_id: laneId });
    if (r.ok && epoch === this.epoch()) { await this.scanAnswer(r.data); }
    return r;
  }

  async discover() {
    const epoch = this.epoch();
    const r = await this.page("discover", { window: this.windowId });
    if (r.ok && epoch === this.epoch()) { await this.scanAnswer(r.data); }
    return r;
  }

  async origins() {
    const epoch = this.epoch();
    const r = await this.page("origins", { window: this.windowId });
    if (r.ok && epoch === this.epoch()) { await this.scanAnswer(r.data); }
    return r;
  }

  // scanAnswer applies a scan's page and updates; when the updates did not
  // fit, the newest page of each affected lane is fetched again (older
  // loaded items of those lanes refresh when they are paged again).
  async scanAnswer(data) {
    if (!this.applyHistory(data)) { await this.resnapshot(); return; }
    if (data.updated_more) {
      for (const lane of data.affected_lanes || []) { await this.laneItems(lane); }
    }
  }

  // openArchive opens a history-only window from the live window's floor.
  async openArchive() {
    if (this.archiveId) { await this.closeArchive(); }
    const r = await this.page("window", { mode: "archive", from_window: this.windowId });
    if (!r.ok) { return r; }
    this.archiveId = r.data.window;
    this.remember(ARCHIVE_KEY, this.archiveId);
    const store = new Store();
    store.snapshot(r.data);
    this.loadArchiveRecords(store);
    return { ok: true, id: this.archiveId, store, envelope: r.data };
  }

  // archivePage loads a lane page or an earlier page of the archive window
  // into its store, and loads the records it needs. Like scanAnswer for the
  // live window, it fetches the newest page of each affected lane again when
  // a scan's updates did not fit. When the archive window was rebuilt (409
  // generation_changed, or an answer of another generation), it takes a new
  // snapshot of the archive window and answers archive_reloaded: lane IDs
  // changed, so the request itself must be made again.
  async archivePage(store, path, laneId, before) {
    const id = this.archiveId;
    const rebuilt = (r) => (!r.ok && r.code === "generation_changed") || (r.ok && store.apply(r.data, { history: true }).stale);
    const body = before ? { window: id, lane_id: laneId, before } : { window: id, lane_id: laneId };
    const r = await this.page(path, body);
    if (id !== this.archiveId) { return r; }
    let reload = rebuilt(r);
    if (!reload && !r.ok) { return r; }
    if (!reload && r.data.updated_more) {
      for (const lane of r.data.affected_lanes || []) {
        const page = await this.page("lane-items", { window: id, lane_id: lane });
        if (id !== this.archiveId) { return page; }
        if (rebuilt(page)) { reload = true; break; }
        if (!page.ok) { return page; }
      }
    }
    if (reload) {
      const snap = await this.page("window", { window: id });
      if (id !== this.archiveId) { return snap; }
      if (!snap.ok) { return snap; }
      // A rebuilt archive window that ended (capacity, failed) has no lanes:
      // keep what is shown and say why.
      if (snap.data.state !== "ok") { return { ok: false, code: "archive_" + snap.data.state, status: 409, retryAfter: 0 }; }
      store.snapshot(snap.data);
    }
    this.loadArchiveRecords(store);
    this.cb.onChange([], { archive: true });
    return reload ? { ok: false, code: "archive_reloaded", status: 0, retryAfter: 0 } : r;
  }

  // loadArchiveRecords loads the archive store's records like the live ones.
  loadArchiveRecords(store) {
    for (const occ of store.pendingRecords()) { this.loadArchiveRecord(store, occ); }
  }

  // loadArchiveRecord loads one record of the archive window into its store.
  loadArchiveRecord(store, occ) {
    const id = this.archiveId;
    const tag = "archive\u0000" + occ;
    if (this.loading.has(tag)) { return; }
    this.loading.add(tag);
    this.records.run(async () => {
      for (let attempt = 0; attempt < 5 && !this.stopped && id === this.archiveId; attempt++) {
        const r = await this.request("record", { window: id, occurrence_id: occ }, { bytes: true });
        if (id !== this.archiveId || this.stopped) { return; }
        if (r.ok) {
          store.loaded(occ, r.data);
          store.release(this.archiveKeep);
        } else if (r.status === 429) {
          await this.sleep((r.retryAfter || 1) * 1000);
          continue;
        } else {
          store.putRecord({ occurrence: occ, unavailable: true, retryable: r.status === 0 || r.status >= 500, problem: r.code });
        }
        this.cb.onChange([], { archive: true });
        return;
      }
    }).finally(() => this.loading.delete(tag));
  }

  async closeArchive() {
    const id = this.archiveId;
    this.archiveId = null;
    try { this.o.storage?.removeItem(ARCHIVE_KEY); } catch (e) { /* ignore */ }
    if (id) { await this.request("close", { window: id }); }
  }
}
