// What a card says first: a verb for the call and one plain outcome,
// derived from the recorded request and effect lifecycle. Pure: the card
// module extracts the facts from the records and renders the result.

// Durations as the page shows them.
export function durationText(d) {
  if (!d || d.state === "unknown") { return ""; }
  if (d.state === "sub_millisecond") { return "under 1 ms"; }
  return (d.milliseconds ?? 0).toLocaleString("en-US") + " ms";
}

// Present-tense verbs agree with every outcome ("Run command · Running…").
const VERBS = {
  terminal_start: "Run command",
  terminal_write: "Send input",
  terminal_read: "Read output",
  terminal_signal: "Signal process",
  terminal_resize: "Resize terminal",
  terminal_close: "Close terminal",
  terminal_list: "List terminals",
  file_read: "Read files",
  file_list: "List files",
  file_search: "Search files",
  file_edit: "Edit file",
  file_receive: "Download file",
  file_receive_result: "Check download",
  file_receive_status: "Check download",
};

// verb names a call. file_write says whether it creates or replaces a
// file, when its recorded input is known: an expected_version replaces.
export function verb(tool, { inputRecorded = false, hasExpectedVersion = false } = {}) {
  if (tool === "file_write") { return !inputRecorded ? "Write file" : hasExpectedVersion ? "Replace file" : "Create file"; }
  return Object.hasOwn(VERBS, tool) ? VERBS[tool] : tool || "Unknown tool";
}

// Calls that observe work started elsewhere: their records say what they
// saw at that moment, never the current state.
const OBSERVING = new Set(["terminal_read", "file_receive_result", "file_receive_status"]);

// The server records a signal exit as Go's description of the signal.
const SIGNALS = {
  killed: "killed (SIGKILL)", terminated: "terminated (SIGTERM)", interrupt: "interrupted (SIGINT)", hangup: "hung up (SIGHUP)",
  quit: "quit (SIGQUIT)", aborted: "aborted (SIGABRT)", "abort trap": "aborted (SIGABRT)", "broken pipe": "broken pipe (SIGPIPE)", "segmentation fault": "crashed (SIGSEGV)",
};

// exitText names a recorded process exit; an exit without details reads
// "exited".
export function exitText(exit) {
  if (!exit || typeof exit !== "object") { return ""; }
  if (typeof exit.code === "number") { return "exited " + exit.code; }
  if (typeof exit.signal === "string" && exit.signal) {
    return Object.hasOwn(SIGNALS, exit.signal) ? SIGNALS[exit.signal] : "ended by signal: " + exit.signal;
  }
  if (typeof exit.error === "string" && exit.error) { return "exit error: " + exit.error; }
  return "exited";
}

// Recorded reason codes in plain words; an unknown code shows as recorded.
const REASONS = {
  schema_validation: "the arguments did not match the tool's schema",
  audit_record_too_large: "the input was too large to record in the audit",
  audit_input_unrepresentable: "the input could not be recorded in the audit",
  limit: "the result limit was reached",
  deadline: "the time limit was reached",
};
export function reasonText(code) { return typeof code === "string" && Object.hasOwn(REASONS, code) ? REASONS[code] : code || ""; }

const UNRECORDED = " (finish may not be recorded)";

// outcome answers {kind, label, detail} for an item. facts are values of
// the request's finish record (error, process, files_total, files_failed,
// truncated, search_matches, reason, bytes_written), as far as they were
// recorded; facts.error falls back to the effect's operation_finish error
// (a download's failure is recorded only there). For a command, facts.exit
// may carry an exit seen by a later call, and facts.seen the time (shown
// as given) of the newest observation its process state comes from.
// Kinds: done, failed, partial, running, started, checked, not_run,
// repeat, stopped, unknown. The server sets possibly_unrecorded only on
// open requests and in-flight effects.
export function outcome(item, facts = {}) {
  const a = item.action;
  if (!a) { return effectOutcome(item.effect, facts); }
  const e = item.effect; // The action's own effect, when it has one.
  const status = a.status || "";
  const err = typeof facts.error === "string" && facts.error ? facts.error : "";

  // 1. Not run.
  if (a.rejected || status === "rejected" || a.input === "not_recorded") { return { kind: "not_run", label: "Not run", detail: reasonText(a.reason) || err }; }
  // 2. A replay returns an earlier result and never runs again.
  if (a.replayed) {
    const was = { applied: "the original was done", completed: "the original download completed", failed: "the original failed",
      partial: "the original was partly done", running: "the original download was still running then", unknown: "the original's outcome is unknown" }[status];
    return { kind: "repeat", label: "Repeat of an earlier call, not run again", detail: was || "" };
  }
  // Polls describe what they saw.
  if (OBSERVING.has(a.tool) && a.state === "finished" && (status === "ok" || status === "running" || status === "completed")) {
    return checked(a, status, facts);
  }
  // 3. The action's own effect decides.
  if (e) {
    const suffix = e.possibly_unrecorded ? UNRECORDED : "";
    if (e.state === "finished") {
      const stopped = status === "canceled" || status === "deadline" ? " (the model stopped waiting)" : "";
      if (a.tool === "terminal_start" && (e.status === "applied" || e.status === "ok")) { return started(facts, stopped + suffix); }
      switch (e.status) {
        case "applied": case "completed": case "ok":
          // A request error with an applied effect: the effect happened.
          return { kind: "done", label: "Done" + stopped + suffix, detail: status === "error" && err ? "the request reported: " + err : "" };
        case "failed": return { kind: "failed", label: "Failed" + suffix, detail: err };
        case "partial": return { kind: "partial", label: "Partly done" + suffix, detail: partialDetail(a, facts) };
        default: return { kind: "unknown", label: "Outcome unknown" + suffix, detail: "" };
      }
    }
    if (e.state === "in_flight") { return { kind: "running", label: "Running…" + suffix, detail: "" }; }
    return { kind: "unknown", label: "Outcome unknown" + suffix, detail: "" };
  }
  // 4. Failed.
  if (status === "failed" || status === "error" || a.outcome === "failed") {
    return { kind: "failed", label: "Failed", detail: err || (a.outcome === "failed" ? failedFiles(facts) : "") };
  }
  // 5. Partly done.
  if (a.tool === "file_search" && (a.outcome === "partial" || facts.truncated === true)) {
    const count = typeof facts.search_matches === "number" ? plural(facts.search_matches, "match", "matches") + ", " : "";
    return { kind: "done", label: "Done", detail: count + "results cut" + (facts.reason ? ": " + reasonText(facts.reason) : "") };
  }
  if (status === "partial" || a.outcome === "partial") { return { kind: "partial", label: "Partly done", detail: partialDetail(a, facts) }; }
  // 6. Running. A finished call that saw a running download, without its
  // effect loaded, is history: what it saw is not a live state.
  if (a.state === "finished" && status === "running") {
    return { kind: "unknown", label: "Outcome unknown", detail: "still downloading when the call returned" };
  }
  if (a.state === "open") {
    return { kind: "running", label: "Running…" + (a.possibly_unrecorded ? UNRECORDED : ""), detail: "" };
  }
  // 7. Unknown, or a read the model stopped waiting for.
  if (status === "canceled" || status === "deadline") {
    if (a.outcome === "unknown") { return { kind: "unknown", label: "Outcome unknown", detail: "the model stopped waiting" }; }
    return { kind: "stopped", label: status === "canceled" ? "Canceled" : "Timed out", detail: "the model stopped waiting" };
  }
  if (a.state === "unknown" || status === "unknown" || a.outcome === "unknown" || a.possibly_unrecorded) {
    return { kind: "unknown", label: "Outcome unknown", detail: "" };
  }
  // 8. Done, for the statuses that mean it; any other recorded status is
  // not guessed at.
  if (status && !["ok", "applied", "completed"].includes(status)) {
    return { kind: "unknown", label: "Outcome unknown", detail: "status " + status };
  }
  return { kind: "done", label: "Done", detail: a.tool === "file_search" && typeof facts.search_matches === "number" ? plural(facts.search_matches, "match", "matches") : "" };
}

// started says what was last seen of a command's process, and when;
// never a live "still running".
function started(facts, suffix) {
  const seen = facts.seen ? " (seen " + facts.seen + ")" : "";
  const ex = facts.exit || (facts.process && facts.process.exit);
  const exit = exitText(ex);
  // warn: the program ended other than with exit code 0.
  if (exit) { return { kind: "started", label: "Started, " + exit + seen + suffix, detail: "", warn: !(ex && ex.code === 0) && exit !== "exited" }; }
  const state = facts.process && typeof facts.process.state === "string" ? facts.process.state : "";
  if (state === "running" && seen) { return { kind: "started", label: "Started, running" + seen + suffix, detail: "" }; }
  if (state === "exited") { return { kind: "started", label: "Started, exited" + seen + suffix, detail: "" }; }
  return { kind: "started", label: "Started" + suffix, detail: "" };
}

function checked(a, status, facts) {
  if (a.tool === "terminal_read") {
    const exit = exitText(facts.process && facts.process.exit);
    const state = facts.process && typeof facts.process.state === "string" ? facts.process.state : "";
    return { kind: "checked", label: "Checked: " + (exit || state || "output read"), detail: "" };
  }
  if (status === "completed") { return { kind: "checked", label: "Checked: download completed", detail: "" }; }
  return { kind: "checked", label: "Checked: still downloading", detail: "" };
}

function effectOutcome(e, facts) {
  if (!e) { return { kind: "unknown", label: "Outcome unknown", detail: "" }; }
  return outcome({ action: { tool: e.tool, state: "finished", status: "ok" }, effect: e }, facts);
}

function partialDetail(a, facts) {
  if (typeof facts.files_failed === "number" && typeof facts.files_total === "number") {
    return facts.files_failed + " of " + facts.files_total + " files failed";
  }
  if (typeof facts.bytes_written === "number") { return plural(facts.bytes_written, "byte", "bytes") + " written"; }
  return "";
}

function failedFiles(facts) {
  return typeof facts.files_total === "number" ? "all " + facts.files_total + " files failed" : "";
}

function plural(n, one, many) { return n.toLocaleString("en-US") + " " + (n === 1 ? one : many); }

// headDuration is the one duration the header shows, with a title that
// says what it measures.
export function headDuration(item, sinceText = (t) => t) {
  const a = item.action;
  const e = item.effect;
  if (e && e.state === "in_flight") {
    const t = e.start && e.start.time;
    return t ? { text: "since " + sinceText(t), title: "Still running" } : { text: "", title: "" };
  }
  if (a && a.tool === "terminal_start" && e && e.state === "finished" && durationText(e.duration)) {
    return { text: "started in " + durationText(e.duration), title: "The process launch; the program's runtime is not recorded" };
  }
  if (e && e.state === "finished" && !(a && a.replayed) && durationText(e.duration)) {
    return { text: durationText(e.duration), title: "The action itself" };
  }
  return { text: a ? durationText(a.duration) : "", title: "The MCP request" };
}

// timing explains every recorded duration in words, for Details.
export function timing(item) {
  const a = item.action;
  const e = item.effect;
  const parts = [];
  if (a && a.finish) { const d = durationText(a.duration); if (d) { parts.push("Answered in " + d + "."); } }
  if (e) {
    if (e.state === "in_flight") { parts.push("The action is still running."); }
    const d = durationText(e.duration);
    if (d && e.state === "finished") {
      parts.push(a && a.tool === "terminal_start" ? "The process launched in " + d + "; the program's runtime is not recorded." : "The action took " + d + ".");
    }
  }
  return parts.join(" ");
}
