import { test } from "node:test";
import assert from "node:assert/strict";
import { outcome, verb, exitText, headDuration, timing, durationText } from "../static/outcome.js";

const act = (tool, over = {}) => ({ tool, state: "finished", status: "ok", duration: { state: "measured", milliseconds: 49 }, finish: { time: "t" }, ...over });
const eff = (over = {}) => ({ tool: "x", state: "finished", status: "applied", duration: { state: "measured", milliseconds: 15 }, start: { time: "2026-11-10T10:00:00Z" }, ...over });

test("every recorded status and outcome maps to one plain result", () => {
  const rows = [
    // [what, item, facts, kind, label]
    ["rejected", { action: act("file_write", { rejected: { time: "t" }, reason: "audit_record_too_large", status: "rejected" }) }, {}, "not_run", "Not run"],
    ["schema rejection", { action: act("terminal_start", { status: "rejected", reason: "schema_validation" }) }, {}, "not_run", "Not run"],
    ["replay of an applied write", { action: act("file_write", { replayed: true, status: "applied" }) }, {}, "repeat", "Repeat of an earlier call, not run again"],
    ["write applied", { action: act("file_write", { status: "applied" }), effect: eff() }, {}, "done", "Done"],
    ["write failed", { action: act("file_write", { status: "failed" }), effect: eff({ status: "failed" }) }, { error: "version mismatch" }, "failed", "Failed"],
    ["partial terminal write", { action: act("terminal_write", { status: "partial" }), effect: eff({ status: "partial" }) }, { bytes_written: 3 }, "partial", "Partly done"],
    ["effect unknown", { action: act("file_edit", { status: "ok" }), effect: eff({ status: "unknown" }) }, {}, "unknown", "Outcome unknown"],
    ["effect in flight", { action: act("file_receive", { status: "running" }), effect: eff({ state: "in_flight", status: "" }) }, {}, "running", "Running…"],
    ["effect state unknown", { action: act("file_write"), effect: eff({ state: "unknown", status: "" }) }, {}, "unknown", "Outcome unknown"],
    ["applied after the model stopped waiting", { action: act("file_write", { status: "canceled", outcome: "unknown" }), effect: eff() }, {}, "done", "Done (the model stopped waiting)"],
    ["applied after a deadline", { action: act("file_write", { status: "deadline", outcome: "unknown" }), effect: eff() }, {}, "done", "Done (the model stopped waiting)"],
    ["effect may be unrecorded", { action: act("file_receive", { status: "running" }), effect: eff({ state: "in_flight", status: "", possibly_unrecorded: true }) }, {}, "running", "Running… (finish may not be recorded)"],
    ["command running at launch", { action: act("terminal_start", { status: "applied" }), effect: eff() }, { process: { state: "running" }, seen: "10:00:01" }, "started", "Started, running (seen 10:00:01)"],
    ["command exited within the wait", { action: act("terminal_start", { status: "applied" }), effect: eff() }, { process: { state: "exited", exit: { code: 0 } }, seen: "10:00:01" }, "started", "Started, exited 0 (seen 10:00:01)"],
    ["command exit seen later", { action: act("terminal_start", { status: "applied" }), effect: eff() }, { exit: { signal: "killed" }, seen: "10:05:00" }, "started", "Started, killed (SIGKILL) (seen 10:05:00)"],
    ["command exited without details", { action: act("terminal_start", { status: "applied" }), effect: eff() }, { process: { state: "exited", exit: {} } }, "started", "Started, exited"],
    ["command, later exit not loaded", { action: act("terminal_start", { status: "applied" }), effect: eff() }, {}, "started", "Started"],
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
    ["open, may be unrecorded", { action: act("terminal_list", { state: "open", status: "", possibly_unrecorded: true }) }, {}, "running", "Running… (finish may not be recorded)"],
    ["request state unknown", { action: act("terminal_list", { state: "unknown", status: "" }) }, {}, "unknown", "Outcome unknown"],
    ["mutation canceled without an effect", { action: act("file_write", { status: "canceled", outcome: "unknown" }) }, {}, "unknown", "Outcome unknown"],
    ["read canceled", { action: act("file_read", { status: "canceled" }) }, {}, "stopped", "Canceled"],
    ["read timed out", { action: act("file_search", { status: "deadline" }) }, {}, "stopped", "Timed out"],
    ["finished but possibly unrecorded", { action: act("terminal_list", { possibly_unrecorded: true }) }, {}, "unknown", "Outcome unknown"],
    ["poll of a running process", { action: act("terminal_read") }, { process: { state: "running" } }, "checked", "Checked: running"],
    ["poll that saw the exit", { action: act("terminal_read") }, { process: { state: "exited", exit: { code: 2 } } }, "checked", "Checked: exited 2"],
    ["poll of a running download", { action: act("file_receive_result", { status: "running" }) }, {}, "checked", "Checked: still downloading"],
    ["poll of a completed download", { action: act("file_receive_result", { status: "completed" }) }, {}, "checked", "Checked: download completed"],
    ["poll that errored", { action: act("terminal_read", { status: "error" }) }, { error: "invalid session_id" }, "failed", "Failed"],
    ["poll outcome unknown", { action: act("file_receive_result", { status: "unknown" }) }, {}, "unknown", "Outcome unknown"],
    ["plain ok", { action: act("terminal_list") }, {}, "done", "Done"],
    ["effect-only applied", { effect: eff({ tool: "file_write" }) }, {}, "done", "Done"],
    ["effect-only running", { effect: eff({ tool: "file_receive", state: "in_flight", status: "" }) }, {}, "running", "Running…"],
  ];
  for (const [what, item, facts, kind, label] of rows) {
    const o = outcome(item, facts);
    assert.deepEqual([o.kind, o.label], [kind, label], what);
  }
});

test("details carry the error, counts and replay origin", () => {
  assert.equal(outcome({ action: act("terminal_write", { status: "error" }) }, { error: "invalid session_id" }).detail, "invalid session_id");
  assert.equal(outcome({ action: act("file_read", { outcome: "partial" }) }, { files_total: 3, files_failed: 1 }).detail, "1 of 3 files failed");
  assert.equal(outcome({ action: act("file_read", { outcome: "failed" }) }, { files_total: 2, files_failed: 2 }).detail, "all 2 files failed");
  assert.equal(outcome({ action: act("file_search", { outcome: "partial" }) }, { truncated: true, reason: "deadline" }).detail, "results cut: the time limit was reached");
  assert.equal(outcome({ action: act("file_search", { outcome: "partial" }) }, { truncated: true, reason: "something_new" }).detail, "results cut: something_new");
  assert.equal(outcome({ action: act("file_receive", { replayed: true, status: "completed" }) }).detail, "the original download completed");
  assert.equal(outcome({ action: act("file_receive", { status: "running" }) }).detail, "still downloading when the call returned");
  assert.equal(outcome({ effect: eff({ tool: "file_receive", status: "failed" }) }, { error: "HTTP 500" }).detail, "HTTP 500");
  assert.equal(outcome({ action: act("file_search") }, { search_matches: 1 }).detail, "1 match");
  assert.equal(outcome({ action: act("file_write", { replayed: true, status: "failed" }) }).detail, "the original failed");
  assert.equal(outcome({ action: act("terminal_write", { status: "partial" }), effect: eff({ status: "partial" }) }, { bytes_written: 3 }).detail, "3 bytes written");
  assert.equal(outcome({ action: act("file_write", { rejected: { time: "t" }, reason: "audit_record_too_large" }) }).detail, "the input was too large to record in the audit");
  assert.equal(outcome({ action: act("terminal_start", { status: "rejected", reason: "schema_validation" }) }).detail, "the arguments did not match the tool's schema");
});

test("exits read as words, from the values the server records", () => {
  assert.equal(exitText({ code: 0 }), "exited 0");
  assert.equal(exitText({ signal: "killed" }), "killed (SIGKILL)");
  assert.equal(exitText({ signal: "terminated" }), "terminated (SIGTERM)");
  assert.equal(exitText({ signal: "interrupt" }), "interrupted (SIGINT)");
  assert.equal(exitText({ signal: "hangup" }), "hung up (SIGHUP)");
  assert.equal(exitText({ signal: "user defined signal 1" }), "ended by signal: user defined signal 1");
  assert.equal(exitText({ error: "wait failed" }), "exit error: wait failed");
  assert.equal(exitText({ code: "0" }), "exited");
  assert.equal(exitText({}), "exited");
  assert.equal(exitText(null), "");
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
