// Texts for the connection and health of the page. Pure: the page writes
// them to the DOM, and tests check them without one.

export const CONNECTION = {
  connecting: "Connecting…", live: "Live", reconnecting: "Reconnecting…", rebuilding: "Rebuilding the view…",
  offline: "Offline", audit_closed: "Audit unavailable: no live activity", stopped: "Server stopped",
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

// describeStatus maps the client's status to the texts of the header:
// the connection label, health and notices (extra notes come last).
export function describeStatus(s, extra = []) {
  const connection = CONNECTION[s.connection] || s.connection;
  const why = s.connection === "offline" || s.connection === "reconnecting" ? reasonText(s.reason, s.reasonStatus) : "";
  const health = s.health && s.health.state && s.health.state !== "ok" ? "Audit " + s.health.state : "";
  const index = s.index && s.index.status && s.index.status !== "ready" ? "In-flight index " + s.index.status.replaceAll("_", " ") : "";
  const outside = s.index && s.index.in_flight_outside && s.index.in_flight_outside.length
    ? s.index.in_flight_outside.length + " running effect(s) outside the loaded history" : "";
  const notes = [];
  if (why) { notes.push(why); }
  if (s.possibleUnseen) { notes.push("Some activity may have been missed while the view was rebuilt."); }
  if (s.floorChanged) { notes.push("Older activity was unloaded to stay within memory; use Earlier to see it again."); }
  notes.push(...extra.filter(Boolean));
  return { connection, health: [health, index, outside].filter(Boolean).join(" · "), notice: notes.join(" ") };
}
