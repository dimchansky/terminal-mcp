"use strict";
// The viewer token is kept in memory and, for page reloads, in this tab's
// sessionStorage; it is sent only as an Authorization header and never put in
// a URL, cookie or log.
(function () {
  const KEY = "terminal-mcp-viewer-token";
  const RENEW_MS = 5 * 60 * 1000;
  let renewTimer = null;
  let current = null;
  let persisted = true;

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
  };
  // These end the session for good; anything else may be temporary.
  const final = new Set(["unauthorized", "session_replaced", "server_stopping"]);

  function failed(code) {
    stopTimers();
    if (final.has(code)) {
      forget();
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
    let text = "Signed in. The session ends at the latest at " + until.toLocaleString() + ".";
    if (!persisted) { text += " This tab cannot keep the sign-in across reloads."; }
    $("status").textContent = text;
    show("signed-in");
    stopTimers();
    renewTimer = setInterval(renew, RENEW_MS);
  }

  async function renew() {
    if (!current) { failed("unauthorized"); return; }
    const r = await api("POST", "renew", undefined, current);
    if (!r.ok) { failed(r.code); return; }
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
    forget();
    if (t) { await api("POST", "logout", undefined, t); }
    $("ended-reason").textContent = "Signed out.";
    show("ended");
  }

  document.addEventListener("DOMContentLoaded", async () => {
    $("submit").addEventListener("click", login);
    $("code").addEventListener("keydown", (e) => { if (e.key === "Enter") { login(); } });
    $("logout").addEventListener("click", logout);
    $("retry").addEventListener("click", renew);
    $("again").addEventListener("click", () => { $("login-error").textContent = ""; show("login"); });
    document.addEventListener("visibilitychange", () => {
      if (document.visibilityState === "visible" && renewTimer !== null) { renew(); }
    });
    current = load();
    if (!current) { show("login"); return; }
    const r = await api("GET", "session", undefined, current);
    if (!r.ok) { failed(r.code); return; }
    signedIn(r.data);
  });
})();
