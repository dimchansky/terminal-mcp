// Texts for the connection and health of the page. Pure: the page writes
// them to the DOM, and tests check them without one.

export const CONNECTION = {
  connecting: "Connecting…", live: "Connected", reconnecting: "Reconnecting…", rebuilding: "Rebuilding the view…",
  offline: "Offline", audit_closed: "Audit unavailable: no live activity", stopped: "Server stopped",
};

// How each connection state looks: ok, warn or bad.
const CONNECTION_KIND = { live: "ok", connecting: "warn", reconnecting: "warn", rebuilding: "warn", offline: "bad", audit_closed: "bad", stopped: "bad" };

// What the connection label means, for its tooltip.
const CONNECTION_TITLE = {
  live: "Receiving new audit records as they are written. This says nothing about whether any process is running.",
  audit_closed: "The audit is unavailable, so no live activity can be shown.",
};

// reasonText explains why the page is offline or reconnecting, and what
// happens next. The same code can mean different things by status: a 409
// capacity is the window's limit, a 429 capacity a busy server.
export function reasonText(code, status) {
  switch (code) {
    case undefined: case null: case "": case "aborted": return "";
    case "network": return "The server cannot be reached; retrying.";
    case "capacity":
      return status === 409
        ? "The recent activity does not fit within one view's capacity; opening a new view with a growing delay."
        : "The server is busy; retrying.";
    case "failed": return "The view kept failing to rebuild; opening a new view with a growing delay.";
    case "window_limit": return "Too many dashboard views are open; close another tab or the archive view. Retrying.";
    case "rebuilding": return "The server is rebuilding the view; retrying.";
    case "timeout": return "The server took too long to answer; retrying.";
    default: return "The server answered " + code + "; retrying.";
  }
}

// loadErrorText explains why a requested page (Earlier, Discover, the
// archive) did not load.
export function loadErrorText(code) {
  switch (code) {
    case "archive_reloaded": return "The archive view was rebuilt on the server and reloaded; try again.";
    case "archive_capacity": case "archive_failed": return "The archive view can no longer be read on the server; go back to the live view and open older history again.";
    case "generation_changed": return "The view changed on the server; try again.";
    case "window_limit": return "Too many dashboard views are open; close another tab first.";
    case "no_floor": return "No older history is beyond this view.";
    case "capacity": return "The server is busy or the view is full; try again later.";
    case "network": return "The server cannot be reached; try again.";
    case "window_gone": return "The view was closed on the server; try again shortly.";
    case "use_archive": return "Older history needs the archive view: use Open older history.";
    default: return "The server answered " + code + ".";
  }
}

// describeStatus maps the client's status to the header: the connection
// {connection, connectionKind, connectionTitle} and the warnings, each
// {kind, text} (bad, warn or info; extra notes come last as warnings).
export function describeStatus(s, extra = []) {
  const connection = Object.hasOwn(CONNECTION, s.connection) ? CONNECTION[s.connection] : String(s.connection);
  const connectionKind = Object.hasOwn(CONNECTION_KIND, s.connection) ? CONNECTION_KIND[s.connection] : "warn";
  const connectionTitle = Object.hasOwn(CONNECTION_TITLE, s.connection) ? CONNECTION_TITLE[s.connection] : connection;
  const warnings = [];
  const why = s.connection === "offline" || s.connection === "reconnecting" ? reasonText(s.reason, s.reasonStatus) : "";
  if (why) { warnings.push({ kind: s.connection === "offline" ? "bad" : "warn", text: why }); }
  if (s.health && s.health.state && s.health.state !== "ok") { warnings.push({ kind: "bad", text: "Audit " + s.health.state }); }
  if (s.index && s.index.status && s.index.status !== "ready") { warnings.push({ kind: "warn", text: "In-flight index " + s.index.status.replaceAll("_", " ") }); }
  if (s.index && s.index.in_flight_outside && s.index.in_flight_outside.length) {
    warnings.push({ kind: "warn", text: s.index.in_flight_outside.length + " running effect(s) outside the loaded history" });
  }
  if (s.possibleUnseen) { warnings.push({ kind: "warn", text: "Some activity may have been missed while the view was rebuilt." }); }
  if (s.floorChanged) { warnings.push({ kind: "info", text: "Older activity was unloaded to stay within memory; use Earlier to see it again." }); }
  for (const e of extra) { if (e) { warnings.push({ kind: "warn", text: e }); } }
  return { connection, connectionKind, connectionTitle, warnings };
}

// sessionText says when the viewer session ends at the latest, compactly:
// {text, title}, the exact time in the title.
export function sessionText(until, now = new Date()) {
  const d = new Date(until);
  if (Number.isNaN(d.getTime())) { return { text: "", title: "" }; }
  const time = d.toLocaleTimeString(undefined, { hourCycle: "h23", hour: "2-digit", minute: "2-digit" });
  const sameDay = d.toDateString() === now.toDateString();
  return { text: "Session ends by " + (sameDay ? time : d.toLocaleDateString(undefined, { month: "short", day: "numeric" }) + " " + time),
    title: "The session ends at the latest at " + d.toLocaleString(undefined, { dateStyle: "full", timeStyle: "long", hourCycle: "h23" }) + "." };
}

// pendingNotice describes a switch auto-switch is holding back, for the
// header: {lane, name, canShow}, or null. The archive view keeps its own
// selection, so Show is not offered there.
export function pendingNotice(pending, { archive = false, names = new Map() } = {}) {
  if (!pending) { return null; }
  return { lane: pending, name: names.get(pending) || "another chat", canShow: !archive };
}
