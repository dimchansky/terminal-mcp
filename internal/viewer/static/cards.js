// Timeline cards. The model's input is the main content, rendered from the
// original start record; tool, times and states are context. Every value is
// inert text with visible escapes, unknown fields are always shown, and a
// record the reader cannot represent exactly is shown raw first.

import { h } from "./view.js";
import { segments } from "./text.js";
import { parseRecord, field, str } from "./json.js";
import { commandLine, interpreterScript } from "./shell.js";
import { lineDiff } from "./diff.js";
import { isPolling } from "./classes.js";
import { refOccurrence } from "./model.js";

// parsed caches parsed records by their bytes (records are immutable).
const parsed = new WeakMap();
function parseCached(bytes) {
  let p = parsed.get(bytes);
  if (!p) { p = parseRecord(bytes); parsed.set(bytes, p); }
  return p;
}

// text renders a string with visible escapes.
export function text(s, multiline = false, invalidUTF8 = false) {
  return segments(s, multiline, invalidUTF8).map((seg) =>
    seg.escape !== undefined ? h("span", { class: "esc", title: "hidden or control character" }, seg.escape) : seg.text);
}

// value renders a JSON node: strings with escapes, numbers by their text.
export function value(node) {
  if (!node) { return h("span", { class: "absent" }, "absent"); }
  switch (node.kind) {
    case "string": return h("span", { class: "str" + (node.value.includes("\n") ? " multiline" : "") }, text(node.value, node.value.includes("\n")));
    case "number": return h("span", { class: "num" + (node.unsafe ? " unsafe" : "") }, node.text);
    case "bool": return h("span", { class: "lit" }, String(node.value));
    case "null": return h("span", { class: "lit" }, "null");
    case "array":
      if (node.items.length === 0) { return h("span", { class: "lit" }, "[]"); }
      return h("ol", { class: "arr" }, node.items.map((it) => h("li", {}, value(it))));
    case "object":
      if (node.entries.length === 0) { return h("span", { class: "lit" }, "{}"); }
      return h("dl", { class: "obj" }, node.entries.map((e) => [h("dt", {}, text(e.key)), h("dd", {}, value(e.value))]));
  }
  return h("span", {}, "?");
}

// fields renders the entries of an object node not in `used`.
function otherFields(args, used, title = "Other supplied fields") {
  if (!args || args.kind !== "object") { return null; }
  const rest = args.entries.filter((e) => !used.includes(e.key));
  if (rest.length === 0) { return null; }
  return h("div", { class: "other" }, h("div", { class: "label" }, title),
    h("dl", { class: "obj" }, rest.map((e) => [h("dt", {}, text(e.key)), h("dd", {}, value(e.value))])));
}

function line(label, node) {
  return node ? h("div", { class: "kv" }, h("span", { class: "label" }, label), " ", value(node)) : null;
}

// stringArray returns the strings of an array node, or null.
function stringArray(node) {
  if (!node || node.kind !== "array" || !node.items.every((i) => i.kind === "string")) { return null; }
  return node.items.map((i) => i.value);
}

// stringMap returns [name, value] pairs of an object of strings, or null.
function stringMap(node) {
  if (!node) { return []; }
  if (node.kind !== "object" || !node.entries.every((e) => e.value.kind === "string")) { return null; }
  return node.entries.map((e) => [e.key, e.value.value]);
}

// The tool-specific input renderers: (args, ctx) → [view, used field names].
const INPUT = {
  terminal_start(args, ctx) {
    const argv = stringArray(field(args, "argv"));
    const env = stringMap(field(args, "env"));
    if (!argv || !env) { return [null, []]; } // Not representable: the field list shows everything.
    const used = ["argv", "env", "cwd"];
    const out = [];
    if (ctx.serverOS === "windows") {
      out.push(h("div", { class: "label" }, "Program and arguments (Windows; exact tokens, no shell notation)"));
      out.push(h("ol", { class: "tokens" }, argv.map((a) => h("li", {}, h("code", {}, text(a))))));
      if (env.length) {
        out.push(h("div", { class: "label" }, "Environment overrides"));
        out.push(h("dl", { class: "obj" }, env.map(([k, v]) => [h("dt", {}, text(k)), h("dd", {}, h("code", {}, text(v)))])));
      }
    } else {
      const cmd = commandLine(env, argv);
      out.push(h("pre", { class: "command" }, h("code", {}, text(cmd.line)))); // Tabs and newlines visible.
      out.push(h("div", { class: "note" }, "Display notation of argv and environment overrides, not a shell invocation."));
      if (cmd.exact) {
        out.push(h("button", { type: "button", class: "copy", "data-action": "copy-command", "data-key": ctx.key }, "Copy command"));
      } else {
        out.push(h("div", { class: "warning" }, "Not copyable: ", text(cmd.problems.join("; ")), ". Exact tokens:"));
        out.push(h("ol", { class: "tokens" }, argv.map((a) => h("li", {}, h("code", {}, text(a))))));
        if (env.length) { out.push(h("dl", { class: "obj" }, env.map(([k, v]) => [h("dt", {}, text(k)), h("dd", {}, h("code", {}, text(v)))]))); }
      }
      const script = interpreterScript(argv);
      if (script) {
        out.push(h("div", { class: "label" }, "Script passed to ", text(argv[0]), " ", text(argv[script.flagIndex])));
        out.push(h("pre", { class: "script" }, h("code", {}, text(script.script, true))));
      }
    }
    const cwd = field(args, "cwd");
    if (cwd) { out.push(line("Working directory", cwd)); }
    return [h("div", { class: "launch" }, out), used];
  },

  terminal_write(args) {
    const input = field(args, "input");
    if (!input || input.kind !== "array") { return [null, []]; }
    const items = input.items.map((it) => {
      const t = str(field(it, "text"));
      const key = str(field(it, "key"));
      const b = str(field(it, "bytes_base64"));
      let shown;
      let main = null;
      if (t !== null) { main = "text"; shown = [h("span", { class: "label" }, "text "), h("pre", { class: "inline" }, h("code", {}, text(t)))]; }
      else if (key !== null) { main = "key"; shown = [h("span", { class: "label" }, "key "), h("kbd", {}, text(key))]; }
      else if (b !== null) { main = "bytes_base64"; shown = [h("span", { class: "label" }, "bytes (base64) "), h("code", {}, text(b)), decodedBytes(b)]; }
      else { return h("li", {}, value(it)); }
      return h("li", {}, shown, otherFields(it, [main], "Other fields of this input"));
    });
    return [h("div", {}, line("Session", field(args, "session_id")), h("ul", { class: "inputs" }, items)), ["input", "session_id"]];
  },

  file_edit(args) {
    const edits = field(args, "edits");
    if (!edits || edits.kind !== "array") { return [null, []]; }
    const out = [line("File", field(args, "path")), line("Expected version", field(args, "expected_version"))];
    edits.items.forEach((e, i) => {
      const o = str(field(e, "old_text"));
      const n = str(field(e, "new_text"));
      const head = h("div", { class: "label" }, "Requested replacement " + (i + 1) + " of " + edits.items.length);
      if (o === null || n === null) { out.push(head, value(e)); return; }
      const d = lineDiff(o, n);
      if (d === null) {
        out.push(head, h("div", { class: "label" }, "Before"), h("pre", { class: "before" }, h("code", {}, text(o, true))),
          h("div", { class: "label" }, "After"), h("pre", { class: "after" }, h("code", {}, text(n, true))));
      } else {
        out.push(head, h("pre", { class: "diff" }, d.ops.map((op) =>
          h("div", { class: op.op === "-" ? "del" : op.op === "+" ? "ins" : "ctx" }, op.op + " ", diffLine(op.text)))));
        if (d.oldEndsWithNewline !== d.newEndsWithNewline) {
          out.push(h("div", { class: "note" }, d.newEndsWithNewline ? "Adds a final newline." : "Removes the final newline."));
        }
      }
      const extra = otherFields(e, ["old_text", "new_text"], "Other fields of this replacement");
      if (extra) { out.push(extra); }
    });
    out.push(h("div", { class: "note" }, "The requested change, not the file's result; no file line numbers are recorded."));
    return [h("div", { class: "edit" }, out), ["edits", "path", "expected_version"]];
  },

  file_write(args) {
    const content = field(args, "content");
    if (!content || content.kind !== "string") { return [null, []]; }
    return [h("div", {}, line("File", field(args, "path")), line("Expected version", field(args, "expected_version")),
      h("pre", { class: "content" }, h("code", {}, text(content.value, true)))), ["content", "path", "expected_version"]];
  },
};

// recordState describes one occurrence's record for a card.
function recordState(ctx, id) {
  const r = ctx.records.get(id);
  if (!r) {
    return refOccurrence(id) ? { state: "loading" }
      : { state: "unavailable", problem: "a damaged row without a reader reference cannot be loaded", retryable: false };
  }
  if (r.bytes) { return { state: "loaded", parsed: parseCached(r.bytes), bytes: r.bytes }; }
  if (r.unavailable) { return { state: "unavailable", problem: r.problem, retryable: r.retryable }; }
  if (r.released) { return { state: "released", size: r.size }; }
  if (r.deferred && r.size > 0) { return { state: "large", size: r.size }; }
  return { state: "loading" };
}

function controls(id, rs) {
  if (rs.state === "released") {
    return h("div", { class: "deferred" }, "Not kept in memory (" + rs.size.toLocaleString("en") + " bytes). ",
      h("button", { type: "button", "data-action": "load-record", "data-key": id }, "Load again"));
  }
  if (rs.state === "large") {
    return h("div", { class: "deferred" }, "Large record (" + rs.size.toLocaleString("en") + " bytes). ",
      h("button", { type: "button", "data-action": "load-record", "data-key": id }, "Load complete original"));
  }
  if (rs.state === "unavailable") {
    return h("div", { class: "warning" }, "Record unavailable: " + (rs.problem || "unknown reason") + ". ",
      rs.retryable ? h("button", { type: "button", "data-action": "load-record", "data-key": id }, "Retry") : null);
  }
  return h("div", { class: "loading" }, "Loading the record…");
}

// inputView renders the model's input from the start record.
function inputView(item, ctx) {
  const a = item.action;
  if (a.input === "not_recorded") {
    return h("div", { class: "not-recorded" }, "Input not recorded · tool not executed",
      a.reason ? h("div", {}, "Reason: ", text(a.reason)) : null);
  }
  // Other reasons (for example why a search stopped) are in the recorded result.
  const reason = a.reason && a.rejected ? h("div", { class: "warning" }, "Rejected, not executed. Reason: ", text(a.reason)) : null;
  if (!a.start) { return h("div", { class: "note" }, "The start of this call is outside the loaded history."); }
  const rs = recordState(ctx, a.start.primary);
  if (rs.state !== "loaded") {
    return h("div", {}, item.summary ? h("div", { class: "summary" }, h("span", { class: "label" }, "Summary (may be incomplete): "), text(item.summary.text),
      item.summary.truncated ? "…" : "") : null, controls(a.start.primary, rs));
  }
  const p = rs.parsed;
  const legacy = a.input === "legacy" || a.legacy ? h("div", { class: "warning" }, "Legacy record: the input may be incomplete.") : null;
  if (p.issues.length > 0) {
    return h("div", { class: "lossy" }, legacy, reason, h("div", { class: "warning" }, "Shown as recorded text: ", text(p.issues.join("; ")), "."),
      h("pre", { class: "raw" }, h("code", {}, text(p.text, true, !p.validUTF8))));
  }
  const args = field(p.value, "args");
  const out = [reason];
  out.push(legacy);
  if (Object.hasOwn(NOT_RECORDED_OUTPUT, a.tool)) { out.push(h("div", { class: "note" }, NOT_RECORDED_OUTPUT[a.tool])); }
  const render = Object.hasOwn(INPUT, a.tool) ? INPUT[a.tool] : null;
  let used = [];
  if (render && args) {
    const [view, u] = render(args, ctx);
    if (view) { out.push(view); used = u; }
  }
  if (args && args.kind === "object") {
    const rest = otherFields(args, used, used.length ? "Other supplied fields" : "Supplied fields");
    if (rest) { out.push(rest); }
  } else if (args) {
    out.push(h("div", { class: "label" }, "Supplied arguments"), value(args));
  } else {
    out.push(h("div", { class: "note" }, "No arguments were recorded."));
  }
  return h("div", { class: "input" }, out);
}

function callMeta(item, ctx) {
  const a = item.action;
  if (!a || !a.start) { return null; }
  const rs = recordState(ctx, a.start.primary);
  if (rs.state !== "loaded" || !rs.parsed.value || rs.parsed.issues.length) { return null; } // Lossy: see the raw record.
  const rows = ["call_meta", "input_responses", "request_state"].map((k) => line(k, field(rs.parsed.value, k))).filter(Boolean);
  return rows.length ? h("details", { class: "meta" }, h("summary", {}, "Call metadata (client-claimed)"), rows) : null;
}

function duration(d) {
  if (!d || d.state === "unknown") { return "unknown"; }
  if (d.state === "sub_millisecond") { return "under 1 ms"; }
  return d.milliseconds.toLocaleString("en") + " ms";
}

// localTime renders a recorded time in local time, with UTC on hover.
export function localTime(t) {
  if (!t) { return null; }
  const d = new Date(t);
  if (Number.isNaN(d.getTime())) { return h("time", {}, text(t)); }
  return h("time", { title: d.toISOString() + " (writer clock)" }, d.toLocaleString());
}

// recordsOf lists an item's records with their roles.
function recordsOf(item) {
  const out = [];
  const add = (role, p) => { if (p) { for (const id of (p.occurrences && p.occurrences.length ? p.occurrences : [p.primary])) { out.push([role, id]); } } };
  if (item.action) { add("request start", item.action.start); add("request finish", item.action.finish); add("request rejected", item.action.rejected); }
  if (item.effect) { add("operation start", item.effect.start); add("operation finish", item.effect.finish); }
  if (item.diagnostic) { out.push(["row", item.diagnostic.key]); }
  if (item.server_start) { out.push(["server start", item.server_start.key]); }
  return out;
}

// originals renders every record of the item as exact text with Copy.
function originals(item, ctx) {
  const key = item.key + ":originals";
  const open = !!(ctx.open && ctx.open.has(key));
  if (!open) { // The body is built only when opened.
    return h("details", { class: "originals", "data-key": key }, h("summary", {}, "Original audit JSONL"));
  }
  const rows = recordsOf(item).map(([role, id]) => {
    const rs = recordState(ctx, id);
    if (rs.state !== "loaded") { return h("div", { class: "original" }, h("div", { class: "label" }, role), controls(id, rs)); }
    const valid = rs.parsed.validUTF8;
    return h("div", { class: "original" }, h("div", { class: "label" }, role, " · ", rs.bytes.length.toLocaleString("en"), " bytes"),
      h("pre", { class: "raw" }, h("code", {}, text(rs.parsed.text, true, !valid))),
      h("button", { type: "button", class: "copy", "data-action": valid ? "copy-record" : "copy-record-base64", "data-key": id },
        valid ? "Copy record" : "Copy as base64 (not valid UTF-8)"));
  });
  return h("details", { class: "originals", "data-key": key, open: true }, h("summary", {}, "Original audit JSONL"), rows);
}

// Envelope fields every record has; result() shows all the others.
const ENVELOPE = ["schema_version", "time", "server_id", "request_id", "tool", "phase", "operation_id", "status", "duration_ms"];

// The server cuts recorded error text at this many bytes and appends "...".
const ERROR_LIMIT = 2048;

// result shows what a finish, rejection or effect finish record says
// beyond its envelope: errors, the writer's truncation marker, counts,
// process details, reasons, receipts.
function result(title, ctx, phase, requestFinish = false) {
  if (!phase) { return null; }
  const rs = recordState(ctx, phase.primary);
  if (rs.state !== "loaded" || !rs.parsed.value || rs.parsed.value.kind !== "object" || rs.parsed.issues.length) { return null; }
  const rest = rs.parsed.value.entries.filter((e) => !ENVELOPE.includes(e.key));
  if (rest.length === 0) { return null; }
  const truncated = field(rs.parsed.value, "truncated");
  const error = str(field(rs.parsed.value, "error"));
  return h("div", { class: "result" }, h("div", { class: "label" }, title),
    truncated && truncated.kind === "bool" && truncated.value
      ? h("div", { class: "note" }, "The tool's result to the model was truncated; this record is complete.") : null,
    // Only request_finish errors are cut: to 2048 bytes plus "...", where a
    // split character may have become U+FFFD (up to 2 more bytes).
    requestFinish && error !== null && error.endsWith("...") &&
      Math.abs(new TextEncoder().encode(error).length - (ERROR_LIMIT + 3)) <= 4
      ? h("div", { class: "warning" }, "The error text was likely cut by the server at " + ERROR_LIMIT + " bytes.") : null,
    h("dl", { class: "obj" }, rest.map((e) => [h("dt", {}, text(e.key)), h("dd", {}, value(e.value))])));
}

function receipt(item, ctx) {
  const a = item.action;
  return [a ? result(a.rejected ? "Recorded rejection" : "Recorded result", ctx, a.rejected || a.finish, !a.rejected) : null,
    item.effect ? result("Recorded effect result", ctx, item.effect.finish) : null];
}

// Output the audit does not record, stated where it matters.
const NOT_RECORDED_OUTPUT = {
  file_read: "The audit records the request, not the file contents returned.",
  terminal_read: "The audit records the request, not the process output returned.",
  terminal_start: "The audit records the launch, not the program's output.",
};

// decodedBytes shows base64 input bytes as escaped text.
function decodedBytes(b64) {
  try {
    const bin = atob(b64);
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
    let s;
    let invalid = false;
    try { s = new TextDecoder("utf-8", { fatal: true }).decode(bytes); } catch (e) { s = new TextDecoder("utf-8").decode(bytes); invalid = true; }
    return h("div", { class: "note" }, "decoded: ", h("code", {}, text(s, false, invalid)));
  } catch (e) {
    return h("div", { class: "warning" }, "not valid base64");
  }
}

// diffLine shows a diff line with trailing whitespace made visible.
function diffLine(s) {
  const m = /[ \t]+$/.exec(s);
  if (!m) { return text(s, true); }
  return [text(s.slice(0, m.index), true), h("span", { class: "esc", title: "trailing whitespace" }, m[0].replaceAll(" ", "·").replaceAll("\t", "→"))];
}

// stateBadge names a request or effect state.
function badge(cls, label) { return h("span", { class: "badge " + cls }, label); }

function actionHeader(item) {
  const a = item.action;
  const e = item.effect;
  const time = (a.start || a.rejected || a.finish || {}).time;
  const parts = [h("strong", { class: "tool" }, text(a.tool || "unknown tool")), " ", localTime(time)];
  parts.push(" ", a.rejected ? badge("status-rejected", "rejected · not executed")
    : badge("state-" + a.state, a.state === "open" ? "running" : a.state === "unknown" ? "outcome unknown" : "finished"));
  if (a.status) { parts.push(" ", badge("status-" + a.status, a.status)); }
  if (a.outcome) { parts.push(" ", h("span", { class: "outcome" }, text(a.outcome))); }
  if (a.finish) { parts.push(" ", h("span", { class: "dur", title: "MCP request time, including waiting and auditing" }, "request " + duration(a.duration))); }
  if (e) {
    parts.push(" ", badge("effect-" + e.state, e.state === "in_flight" ? "effect running" : e.state === "unknown" ? "effect outcome unknown" : "effect finished"));
    if (e.status) { parts.push(" ", badge("status-" + e.status, "effect " + e.status)); }
    if (e.finish) {
      parts.push(" ", h("span", { class: "dur", title: a.tool === "terminal_start" ? "Process launch, not the program's runtime" : "Admitted mutation" },
        (a.tool === "terminal_start" ? "launch " : "effect ") + duration(e.duration)));
    }
  }
  if (a.replayed) {
    parts.push(" ", h("span", { class: "badge replay" }, "replay of operation ", text(a.effect ? a.effect.effect.operation_id : "?"),
      a.effect && !a.effect.loaded ? " (its effect is outside the loaded history)" : " (no second effect)"));
  }
  if (a.process_creator) { parts.push(" ", h("span", { class: "note" }, "process started by request ", text(a.process_creator.request_id))); }
  if (a.possibly_unrecorded || (e && e.possibly_unrecorded)) { parts.push(" ", badge("warn", "finish may be unrecorded (audit was degraded)")); }
  const phases = [a.start, a.finish, a.rejected, e && e.start, e && e.finish].filter(Boolean);
  if (phases.some((p) => p.conflict) || (e && e.origin_conflict)) { parts.push(" ", badge("warn", "Ambiguous: journal copies differ")); }
  if (phases.some((p) => p.more > 0)) { parts.push(" ", badge("warn", "more journal copies than shown")); }
  if (phases.some((p) => (p.occurrences || [p.primary]).some((id) => id.startsWith("p:")))) { parts.push(" ", badge("warn", "pending row identity")); }
  if (a.identity && a.identity.metadataInvalid) { parts.push(" ", badge("warn", "unusable client metadata")); }
  if (a.effect && a.effect.kind === "observe") { parts.push(" ", h("span", { class: "note" }, "observes operation ", text(a.effect.effect.operation_id))); }
  return h("div", { class: "card-head" }, parts);
}

// card renders one timeline item.
export function card(item, ctx) {
  const c = { ...ctx, key: item.key };
  if (item.kind === "server_start") {
    return h("div", { class: "marker", "data-key": item.key }, "Server restarted ", localTimeOf(item, c), " (", text(item.server_start.server_id), ")");
  }
  if (item.kind === "diagnostic") {
    return h("div", { class: "card diagnostic", "data-key": item.key },
      h("div", { class: "card-head" }, badge("warn", "Journal row that could not be read"),
        item.diagnostic.possible_repeat ? [" ", badge("warn", "possible repeat")] : null),
      h("div", {}, text(item.diagnostic.problem)), originals(item, c));
  }
  if (item.kind === "effect" && !item.action) {
    const e = item.effect;
    return h("div", { class: "card effect-only", "data-key": item.key },
      h("div", { class: "card-head" }, h("strong", { class: "tool" }, text(e.tool)), " ", localTime((e.start || e.finish || {}).time), " ",
        badge("effect-" + e.state, e.state === "in_flight" ? "effect running" : e.state), e.status ? [" ", badge("status-" + e.status, e.status)] : null,
        " operation ", text(e.key.operation_id)),
      h("div", { class: "note" }, "The request that admitted this effect is outside the loaded history."),
      receipt(item, c), originals(item, c));
  }
  return h("div", { class: "card", "data-key": item.key }, actionHeader(item), inputView(item, c), callMeta(item, c),
    receipt(item, c), originals(item, c));
}

function localTimeOf(item, ctx) {
  const rs = recordState(ctx, item.server_start.key);
  if (rs.state === "loaded" && rs.parsed.value) { return localTime(str(field(rs.parsed.value, "time"))); }
  return null;
}

// pollTarget returns the grouping key of a polling call, or null.
export function pollTarget(item, ctx) {
  const a = item.action;
  if (!a || !isPolling(a.tool) || !a.start) { return null; }
  const rs = recordState(ctx, a.start.primary);
  if (rs.state !== "loaded" || !rs.parsed.value || rs.parsed.issues.length) { return null; }
  const args = field(rs.parsed.value, "args");
  if (a.tool === "terminal_read") {
    const s = str(field(args, "session_id"));
    return s === null ? null : a.tool + "\u0000" + a.key.server_id + "\u0000" + s;
  }
  const op = str(field(args, "operation_id"));
  const server = str(field(args, "server_id")) ?? a.key.server_id;
  return op === null ? null : a.tool + "\u0000" + server + "\u0000" + op;
}

// timeline groups a lane's items (oldest first): consecutive polls of the
// same target become one group.
export function timeline(items, ctx) {
  const out = [];
  for (const item of items) {
    const target = pollTarget(item, ctx);
    const last = out[out.length - 1];
    if (target && last && last.group === target) { last.items.push(item); continue; }
    out.push(target ? { group: target, items: [item] } : { item });
  }
  return out;
}

// entryKey is unique per timeline: a card's item key, or a group's target
// with its first member (several groups may poll the same target).
export function entryKey(e) {
  if (e.item) { return e.item.key; }
  return e.items.length === 1 ? e.items[0].key : "group:" + e.group + "\u0000" + e.items[0].key;
}

// entry renders a timeline entry: a card or a poll group.
export function entry(e, ctx) {
  if (e.item) { return card(e.item, ctx); }
  if (e.items.length === 1) { return card(e.items[0], ctx); }
  const last = e.items[e.items.length - 1];
  const key = entryKey(e);
  const open = ctx.expanded && ctx.expanded.has(key);
  const failed = e.items.filter((it) => it.action.rejected || (it.action.status && it.action.status !== "ok")).length;
  return h("div", { class: "group", "data-key": key },
    h("button", { type: "button", class: "group-head", "data-action": "toggle-group", "data-key": key, "aria-expanded": !!open },
      text(last.action.tool), " × " + e.items.length + " (polling)" + (failed ? ", " + failed + " not ok" : "") + " · last ", localTime((last.action.start || {}).time)),
    open ? e.items.map((it) => card(it, ctx)) : card(last, ctx));
}
