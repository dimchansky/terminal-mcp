// A smoke test of the page's DOM glue: app.js is loaded against a
// permissive fake DOM and started with a stored token, so a name that no
// longer exists, or a call that throws on the boot and first-render path,
// fails here instead of only in a browser. It does not check layout.

import { test } from "node:test";
import assert from "node:assert/strict";

// anything is a callable stand-in for DOM objects: properties that were set
// read back, other properties are stand-ins too, and it iterates as empty.
function anything(name) {
  const own = new Map();
  const fn = function () { return anything(name + "()"); };
  return new Proxy(fn, {
    get(_, key) {
      if (own.has(key)) { return own.get(key); }
      if (key === Symbol.iterator) { return function* () {}; }
      if (key === Symbol.toPrimitive) { return () => 0; }
      if (key === "then") { return undefined; } // Not a promise.
      if (key === "length") { return 0; }
      const v = anything(name + "." + String(key));
      own.set(key, v);
      return v;
    },
    set(_, key, value) { own.set(key, value); return true; },
    apply() { return anything(name + "()"); },
    construct() { return anything("new " + name); },
  });
}

test("app.js boots with a stored token and renders without undefined names", async () => {
  const errors = [];
  const onError = (e) => errors.push(e);
  process.on("unhandledRejection", onError);
  process.on("uncaughtException", onError);

  const elements = new Map();
  const listeners = new Map();
  const frames = [];
  const requests = [];
  const store = new Map([["terminal-mcp-viewer-token", "tok"]]);
  const storage = { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => store.set(k, String(v)), removeItem: (k) => store.delete(k) };
  const doc = anything("document");
  doc.getElementById = (id) => { if (!elements.has(id)) { elements.set(id, anything("#" + id)); } return elements.get(id); };
  doc.addEventListener = (type, fn) => { listeners.set(type, [...(listeners.get(type) || []), fn]); };
  doc.visibilityState = "visible";
  doc.activeElement = null;

  const lane = { id: "g1-l1", kind: "conversation", identitySource: "openai/session", conversationId: "chat-a", items: 1 };
  const item = { kind: "action", key: "a:s/r1", lane: "g1-l1", order: { live: false, rank: -1 },
    action: { key: { server_id: "s", request_id: "r1" }, tool: "file_read", state: "finished", start: { primary: "ref-r1", occurrences: ["ref-r1"] } } };
  const record = Buffer.from(JSON.stringify({ ts: "2026-11-10T10:00:00Z", request_id: "r1", tool: "file_read", phase: "request_start", args: { path: "a.txt" } }) + "\n");
  const answer = (status, body) => ({ ok: status < 300, status, headers: new Headers(), json: async () => body, arrayBuffer: async () => new Uint8Array(record).buffer,
    body: new ReadableStream({ start() {} }) });
  const fetchFn = async (url, init) => {
    const path = url.replace("/dashboard/api/", "");
    requests.push([init.method, path]);
    switch (path) {
      case "session": return answer(200, { absolute_expires_at: "2026-11-10T22:00:00Z" });
      case "window": return answer(200, { window: "w1", generation: 1, state: "ok", live_cursor: "c0", possible_unseen: false, lanes: [lane],
        recent: { generation: 1, items: [item], records: [{ occurrence: "ref-r1", data_b64: record.toString("base64"), size: record.length }] } });
      default: return answer(204, null);
    }
  };

  const globals = {
    document: doc, window: anything("window"), sessionStorage: storage, localStorage: storage, fetch: fetchFn,
    requestAnimationFrame: (fn) => { frames.push(fn); return frames.length; },
    IntersectionObserver: class { observe() {} unobserve() {} disconnect() {} },
    BroadcastChannel: undefined,
    setInterval: () => 0, clearInterval: () => {},
  };
  // The fakes stay installed: the page's client keeps running on its
  // never-ending stream after the test (each test file has its own
  // process), and must not meet a missing global then.
  for (const [k, v] of Object.entries(globals)) { Object.defineProperty(globalThis, k, { value: v, configurable: true, writable: true }); }
  try {
    await import("../static/app.js");
    for (const fn of listeners.get("DOMContentLoaded") || []) { await fn(); }
    for (let i = 0; i < 50 && !requests.some(([, p]) => p === "window"); i++) { await new Promise((r) => setTimeout(r, 5)); }
    await new Promise((r) => setTimeout(r, 20));
    while (frames.length) { frames.shift()(performance.now()); } // Render the scheduled frames.
    await new Promise((r) => setTimeout(r, 20));
    while (frames.length) { frames.shift()(performance.now()); }
  } finally {
    process.off("unhandledRejection", onError);
    process.off("uncaughtException", onError);
  }
  assert.deepEqual(errors.map(String), []);
  assert.deepEqual(requests[0], ["GET", "session"]);
  assert.ok(requests.some(([m, p]) => m === "POST" && p === "window"), "the live window was opened");
  assert.equal(elements.get("signed-in").hidden, false, "the signed-in view is shown");
  assert.equal(typeof elements.get("connection").textContent, "string", "the header was rendered");
});
