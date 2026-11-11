import { test } from "node:test";
import assert from "node:assert/strict";
import { outcome, verb, exitLabel, headDuration, timing, durationText } from "../static/outcome.js";

const act = (tool, over = {}) => ({ tool, state: "finished", status: "ok", duration: { state: "measured", milliseconds: 49 }, finish: { time: "t" }, ...over });
const eff = (over = {}) => ({ tool: "x", state: "finished", status: "applied", duration: { state: "measured", milliseconds: 15 }, start: { time: "2026-11-10T10:00:00Z" }, ...over });

test("every recorded status and outcome maps to one plain result", () => {
  const rows = [
    // [what, item, facts, kind, label]
    ["rejected", { action: act("file_write", { rejected: { time: "t" }, reason: "audit_record_too_large", status: "rejected" }) }, {}, "not_run", "Not run"],
    ["input not recorded", { action: act("file_write", { input: "not_recorded", reason: "audit_record_too_large", status: "" }) }, {}, "not_run", "Not run"],
    ["schema rejection", { action: act("terminal_start", { status: "rejected", reason: "schema_validation" }) }, {}, "not_run", "Not run"],
    ["replay of an applied write", { action: act("file_write", { replayed: true, status: "applied" }) }, {}, "repeat", "Repeat of an earlier call, not run again"],
    ["write applied", { action: act("file_write", { status: "applied" }), effect: eff() }, {}, "done", "Done"],
    ["write failed", { action: act("file_write", { status: "failed" }), effect: eff({ status: "failed" }) }, { error: "version mismatch" }, "failed", "Failed"],
    ["partial terminal write", { action: act("terminal_write", { status: "partial" }), effect: eff({ status: "partial" }) }, { bytes_written: 3 }, "partial", "Partly done"],
    ["effect unknown", { action: act("file_edit", { status: "ok" }), effect: eff({ status: "unknown" }) }, {}, "unknown", "Outcome unknown"],
    ["effect in flight", { action: act("file_receive", { status: "running" }), effect: eff({ state: "in_flight", status: "" }) }, {}, "running", "Running…"],
    ["effect state unknown", { action: act("file_write"), effect: eff({ state: "unknown", status: "" }) }, {}, "unknown", "Outcome unknown"],
    ["applied after the model stopped waiting", { action: act("file_write", { status: "canceled", outcome: "unknown" }), effect: eff() }, {}, "done", "Done", "the model stopped waiting"],
    ["applied after a deadline", { action: act("file_write", { status: "deadline", outcome: "unknown" }), effect: eff() }, {}, "done", "Done", "the model stopped waiting"],
    ["effect may be unrecorded", { action: act("file_receive", { status: "running" }), effect: eff({ state: "in_flight", status: "", possibly_unrecorded: true }) }, {}, "running", "Running…", "finish may not be recorded"],
    ["command running at launch", { action: act("terminal_start", { status: "applied" }), effect: eff() }, { process: { state: "running" }, seen: "10:00:01" }, "observed", "Last seen running", "seen 10:00:01"],
    ["command running, no time", { action: act("terminal_start", { status: "applied" }), effect: eff() }, { process: { state: "running" } }, "started", "Started", "Exit not recorded in the loaded history"],
    ["command exited within the wait", { action: act("terminal_start", { status: "applied" }), effect: eff() }, { process: { state: "exited", exit: { code: 0 } }, seen: "10:00:01" }, "exited_ok", "Exited · 0", "exit seen 10:00:01"],
    ["command exited non-zero", { action: act("terminal_start", { status: "applied" }), effect: eff() }, { process: { state: "exited", exit: { code: 1 } }, seen: "10:00:01" }, "exited_bad", "Exited · 1", "exit seen 10:00:01"],
    ["command exit seen later", { action: act("terminal_start", { status: "applied" }), effect: eff() }, { exit: { signal: "killed" }, seen: "10:05:00" }, "signaled", "Killed · SIGKILL", "exit seen 10:05:00"],
    ["command ended by another signal", { action: act("terminal_start", { status: "applied" }), effect: eff() }, { exit: { signal: "user defined signal 1" }, seen: "10:05:00" }, "signaled", "Ended by signal · user defined signal 1", "exit seen 10:05:00"],
    ["command exit error", { action: act("terminal_start", { status: "applied" }), effect: eff() }, { process: { state: "exited", exit: { error: "wait failed" } }, seen: "10:05:00" }, "unknown", "Outcome unknown", "exit error: wait failed"],
    ["command exited without details", { action: act("terminal_start", { status: "applied" }), effect: eff() }, { process: { state: "exited", exit: {} } }, "exited_unknown", "Exited", ""],
    ["command exited, no exit object", { action: act("terminal_start", { status: "applied" }), effect: eff() }, { process: { state: "exited" }, seen: "10:00:02" }, "exited_unknown", "Exited", "exit seen 10:00:02"],
    ["command, later exit not loaded", { action: act("terminal_start", { status: "applied" }), effect: eff() }, {}, "started", "Started", "Exit not recorded in the loaded history"],
    ["command the model stopped waiting for", { action: act("terminal_start", { status: "canceled", outcome: "unknown" }), effect: eff() }, {}, "started", "Started", "Exit not recorded in the loaded history; the model stopped waiting"],
    ["command launch failed", { action: act("terminal_start", { status: "failed" }), effect: eff({ status: "failed" }) }, { error: "no such file" }, "failed", "Failed"],
    ["legacy row", { action: act("terminal_list", { legacy: true, status: "ok" }) }, {}, "done", "Done"],
    ["download completed", { action: act("file_receive", { status: "running" }), effect: eff({ status: "applied" }) }, {}, "done", "Done"],
    ["download failed", { action: act("file_receive", { status: "running" }), effect: eff({ status: "failed" }) }, { error: "HTTP 404" }, "failed", "Failed"],
    ["old download, effect not loaded", { action: act("file_receive", { status: "running" }) }, {}, "unknown", "Outcome unknown"],
    ["replay of a running download", { action: act("file_receive", { replayed: true, status: "running" }) }, {}, "repeat", "Repeat of an earlier call, not run again"],
    ["receipt unknown without an effect", { action: act("terminal_close", { status: "unknown" }) }, {}, "unknown", "Outcome unknown"],
    ["effect-only failure", { effect: eff({ tool: "file_receive", status: "failed" }) }, { error: "HTTP 500" }, "failed", "Failed"],
    ["request error", { action: act("terminal_write", { status: "error" }) }, { error: "invalid session_id" }, "failed", "Failed"],
    ["file_receive failed", { action: act("file_receive_result", { status: "failed" }) }, {}, "failed", "Failed"],
    ["all files failed", { action: act("file_read", { outcome: "failed" }) }, { files_total: 2, files_failed: 2 }, "failed", "Failed"],
    ["some files failed", { action: act("file_read", { outcome: "partial" }) }, { files_total: 3, files_failed: 1 }, "partial", "Partly done"],
    ["search cut", { action: act("file_search", { outcome: "partial" }) }, { truncated: true, reason: "deadline", search_matches: 500 }, "done", "Done"],
    ["search complete", { action: act("file_search", { outcome: "ok" }) }, { search_matches: 1 }, "done", "Done"],
    ["request open", { action: act("terminal_list", { state: "open", status: "", finish: null }) }, {}, "running", "Running…"],
    ["open, may be unrecorded", { action: act("terminal_list", { state: "open", status: "", possibly_unrecorded: true }) }, {}, "running", "Running…", "finish may not be recorded"],
    ["request state unknown", { action: act("terminal_list", { state: "unknown", status: "" }) }, {}, "unknown", "Outcome unknown"],
    ["mutation canceled without an effect", { action: act("file_write", { status: "canceled", outcome: "unknown" }) }, {}, "unknown", "Outcome unknown"],
    ["read canceled", { action: act("file_read", { status: "canceled" }) }, {}, "stopped", "Canceled"],
    ["read timed out", { action: act("file_search", { status: "deadline" }) }, {}, "stopped", "Timed out"],
    ["finished but possibly unrecorded", { action: act("terminal_list", { possibly_unrecorded: true }) }, {}, "unknown", "Outcome unknown"],
    ["poll of a running process", { action: act("terminal_read") }, { process: { state: "running" } }, "checked", "Checked: running"],
    ["poll that saw the exit", { action: act("terminal_read") }, { process: { state: "exited", exit: { code: 2 } } }, "checked", "Checked: exited · 2"],
    ["poll that saw a signal", { action: act("terminal_read") }, { process: { state: "exited", exit: { signal: "terminated" } } }, "checked", "Checked: terminated · SIGTERM"],
    ["poll of a running download", { action: act("file_receive_result", { status: "running" }) }, {}, "checked", "Checked: still downloading"],
    ["poll of a completed download", { action: act("file_receive_result", { status: "completed" }) }, {}, "checked", "Checked: download completed"],
    ["poll that errored", { action: act("terminal_read", { status: "error" }) }, { error: "invalid session_id" }, "failed", "Failed"],
    ["poll outcome unknown", { action: act("file_receive_result", { status: "unknown" }) }, {}, "unknown", "Outcome unknown"],
    ["plain ok", { action: act("terminal_list") }, {}, "done", "Done"],
    ["effect-only applied", { effect: eff({ tool: "file_write" }) }, {}, "done", "Done"],
    ["effect-only running", { effect: eff({ tool: "file_receive", state: "in_flight", status: "" }) }, {}, "running", "Running…"],
  ];
  for (const [what, item, facts, kind, label, secondary] of rows) {
    const o = outcome(item, facts);
    assert.deepEqual([o.kind, o.label], [kind, label], what);
    if (secondary !== undefined) { assert.equal(o.secondary, secondary, what); }
  }
});

test("details carry the error, counts and replay origin", () => {
  assert.equal(outcome({ action: act("terminal_write", { status: "error" }) }, { error: "invalid session_id" }).detail, "invalid session_id");
  assert.equal(outcome({ action: act("file_read", { outcome: "partial" }) }, { files_total: 3, files_failed: 1 }).detail, "1 of 3 files failed");
  assert.equal(outcome({ action: act("file_read", { outcome: "failed" }) }, { files_total: 2, files_failed: 2 }).detail, "all 2 files failed");
  assert.equal(outcome({ action: act("file_search", { outcome: "partial" }) }, { truncated: true, reason: "deadline" }).detail, "results cut: the time limit was reached");
  assert.equal(outcome({ action: act("file_search", { outcome: "partial" }) }, { truncated: true, reason: "something_new" }).detail, "results cut: something_new");
  assert.equal(outcome({ action: act("file_receive", { replayed: true, status: "completed" }) }).detail, "the original download completed");
  assert.equal(outcome({ action: act("file_receive", { status: "running" }) }).secondary, "still downloading when the call returned");
  assert.equal(outcome({ effect: eff({ tool: "file_receive", status: "failed" }) }, { error: "HTTP 500" }).detail, "HTTP 500");
  assert.equal(outcome({ action: act("file_search") }, { search_matches: 1 }).detail, "1 match");
  assert.equal(outcome({ action: act("file_write", { replayed: true, status: "failed" }) }).detail, "the original failed");
  assert.equal(outcome({ action: act("terminal_write", { status: "partial" }), effect: eff({ status: "partial" }) }, { bytes_written: 3 }).detail, "3 bytes written");
  assert.equal(outcome({ action: act("file_write", { rejected: { time: "t" }, reason: "audit_record_too_large" }) }).detail, "the input was too large to record in the audit");
  assert.equal(outcome({ action: act("terminal_start", { status: "rejected", reason: "schema_validation" }) }).detail, "the arguments did not match the tool's schema");
});

test("exits read as short labels, from the values the server records", () => {
  const signals = { killed: "Killed · SIGKILL", terminated: "Terminated · SIGTERM", interrupt: "Interrupted · SIGINT", hangup: "Hung up · SIGHUP",
    quit: "Quit · SIGQUIT", aborted: "Aborted · SIGABRT", "abort trap": "Aborted · SIGABRT", "broken pipe": "Broken pipe · SIGPIPE",
    "segmentation fault": "Crashed · SIGSEGV", constructor: "Ended by signal · constructor" };
  for (const [sig, label] of Object.entries(signals)) {
    assert.deepEqual(exitLabel({ signal: sig }), { kind: "signaled", label, warn: true }, sig);
  }
  assert.deepEqual(exitLabel({ code: 0 }), { kind: "exited_ok", label: "Exited · 0", warn: false });
  assert.deepEqual(exitLabel({ code: 137 }), { kind: "exited_bad", label: "Exited · 137", warn: true });
  assert.deepEqual(exitLabel({ code: "0" }), { kind: "exited_unknown", label: "Exited", warn: false }, "a code that is not a number is not trusted");
  assert.equal(exitLabel({ error: "wait failed" }), null, "an exit error leaves the exit unknown");
  assert.deepEqual(exitLabel({}), { kind: "exited_unknown", label: "Exited", warn: false });
  assert.equal(exitLabel(null), null);
});

test("a poll that saw a bad exit is a warning", () => {
  assert.equal(outcome({ action: act("terminal_read") }, { process: { state: "exited", exit: { code: 2 } } }).warn, true);
  assert.equal(outcome({ action: act("terminal_read") }, { process: { state: "exited", exit: { code: 0 } } }).warn, false);
  assert.equal(outcome({ action: act("terminal_read") }, { process: { state: "exited", exit: { signal: "killed" } } }).warn, true);
  const err = outcome({ action: act("terminal_read") }, { process: { state: "exited", exit: { error: "wait failed" } } });
  assert.deepEqual([err.label, err.secondary, err.warn], ["Checked: exit error", "wait failed", true]);
});

test("verbs are present tense; file_write says create or replace", () => {
  assert.equal(verb("terminal_start"), "Run command");
  assert.equal(verb("file_write", { inputRecorded: true }), "Create file");
  assert.equal(verb("file_write", { inputRecorded: true, hasExpectedVersion: true }), "Replace file");
  assert.equal(verb("file_write"), "Write file", "input not recorded");
  assert.equal(verb("brand_new_tool"), "brand_new_tool");
  assert.equal(verb("constructor"), "constructor", "names from records are never object keys");
  assert.equal(verb(""), "Unknown tool");
});

test("one header duration, the breakdown in words", () => {
  const start = { action: act("terminal_start"), effect: eff() };
  assert.deepEqual(headDuration(start).text, "started in 15 ms");
  assert.equal(timing(start), "Answered in 49 ms. The process launched in 15 ms; the program's runtime is not recorded.");
  const download = { action: act("file_receive", { duration: { state: "measured", milliseconds: 2 } }), effect: eff({ state: "in_flight", duration: { state: "unknown" } }) };
  assert.equal(headDuration(download, () => "10:00:00").text, "since 10:00:00");
  const done = { action: act("file_receive", { duration: { state: "measured", milliseconds: 2 } }), effect: eff({ duration: { state: "measured", milliseconds: 9000 } }) };
  assert.equal(headDuration(done).text, "9,000 ms");
  assert.equal(headDuration({ action: act("file_write", { replayed: true }) }).text, "49 ms", "a replay has no own effect");
  assert.equal(headDuration({ action: act("terminal_list", { duration: { state: "sub_millisecond" } }) }).text, "under 1 ms");
  assert.equal(headDuration({ action: act("file_write"), effect: eff({ duration: { state: "unknown" } }) }).text, "49 ms", "an unknown effect time falls back to the request");
  assert.equal(durationText({ state: "unknown" }), "");
});

test("an unknown status is not guessed at; a cut search keeps its count", () => {
  const o = outcome({ action: act("terminal_list", { status: "brand_new" }) });
  assert.deepEqual([o.kind, o.label, o.secondary], ["unknown", "Outcome unknown", "status brand_new"]);
  assert.equal(outcome({ action: act("file_search", { outcome: "partial" }) }, { truncated: true, reason: "limit", search_matches: 12 }).detail,
    "12 matches, results cut: the result limit was reached");
  const err = outcome({ action: act("file_write", { status: "error" }), effect: eff() }, { error: "connection reset" });
  assert.deepEqual([err.label, err.detail], ["Done", "the request reported: connection reset"]);
});
