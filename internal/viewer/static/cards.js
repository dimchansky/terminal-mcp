// Timeline cards. The model's input is the main content, rendered from the
// original start record; tool, times and states are context. Every value is
// inert text with visible escapes, unknown fields are always shown, and a
// record the reader cannot represent exactly is shown raw first.

import { h } from "./view.js";
import { segments } from "./text.js";
import { parseRecord, field, str } from "./json.js";
import { commandLine, interpreterScript, quote } from "./shell.js";
import { lineDiff } from "./diff.js";
import { isPolling } from "./classes.js";
import { refOccurrence } from "./model.js";
import { outcome, verb, headDuration, timing } from "./outcome.js";
import { fieldClass, knownTool, hasChildren } from "./fields.js";

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

// schemaValue renders a field's value and marks any key inside it that the
// tool's schema does not define (file.x, files[].x), at any depth.
function schemaValue(tool, path, node) {
  if (!node || !hasChildren(tool, path)) { return value(node); }
  if (node.kind === "array") {
    if (node.items.length === 0) { return value(node); }
    return h("ol", { class: "arr" }, node.items.map((it) => h("li", {}, schemaValue(tool, path + "[]", it))));
  }
  if (node.kind === "object" && node.entries.length) {
    return h("dl", { class: "obj" }, node.entries.map((e) => {
      const p = path + "." + e.key;
      const unknown = fieldClass(tool, p) === "";
      return [h("dt", unknown ? { class: "undefined-key", title: "Not in this server version's schema" } : {}, text(e.key), unknown ? " (not in schema)" : ""),
        h("dd", {}, schemaValue(tool, p, e.value))];
    }));
  }
  return value(node);
}

// nestedFields shows the other keys of a nested item (an edit, an input):
// schema fields under the title, others marked as not in the schema.
function nestedFields(tool, path, node, shown, title) {
  if (!node || node.kind !== "object") { return null; }
  const rest = node.entries.filter((e) => !shown.includes(e.key));
  const known = rest.filter((e) => fieldClass(tool, path + "." + e.key) !== "");
  const unknown = rest.filter((e) => fieldClass(tool, path + "." + e.key) === "");
  return [known.length ? fieldList(known, title) : null, unknown.length ? fieldList(unknown, "Not in this server version's schema", "undefined-fields") : null];
}

// info is a small note behind an "i" the owner can open (on touch too);
// its text stays in the card.
function info(note, ctx, n) {
  const id = "tm-note-" + (ctx && ctx.key ? ctx.key : "x").replace(/[^\w:-]/g, "_") + "-" + n;
  return h("details", { class: "info" }, h("summary", { "aria-label": "Note", "aria-describedby": id, title: note }, "i"), h("span", { id }, note));
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
    let details = null;
    if (ctx.serverOS === "windows") {
      out.push(h("div", { class: "label" }, "Program and arguments (Windows; exact tokens, no shell notation)"));
      out.push(h("ol", { class: "tokens" }, argv.map((a) => h("li", {}, h("code", {}, text(a))))));
      if (env.length) {
        out.push(h("div", { class: "label" }, "Environment overrides"));
        out.push(h("dl", { class: "obj" }, env.map(([k, v]) => [h("dt", {}, text(k)), h("dd", {}, h("code", {}, text(v)))])));
      }
    } else {
      const cmd = commandLine(env, argv);
      const script = interpreterScript(argv);
      const copy = h("button", { type: "button", class: "copy", "data-action": "copy-command", "data-key": ctx.key, title: "Copy the full command line" }, "Copy command line");
      if (cmd.exact && script) {
        // A shell script: the script itself, with every other token of the
        // command (env overrides, the interpreter and its options, the
        // operands after the script) shown around it; the full line, which
        // they rebuild exactly, is in Details.
        const via = commandLine(env, argv.slice(0, script.scriptIndex));
        out.push(h("div", { class: "via" }, "via ", h("code", {}, text(via.line))));
        out.push(script.script === "" ? h("div", { class: "warning" }, "(empty script)")
          : h("pre", { class: "command script" }, h("code", {}, text(script.script, true))));
        const operands = argv.slice(script.scriptIndex + 1);
        if (operands.length) {
          // Quoted as in the full line, so empty and space-padded tokens show.
          out.push(h("div", { class: "label" }, "Arguments after the script ($0 first)"));
          out.push(h("ol", { class: "tokens" }, operands.map((a) => h("li", {}, h("code", {}, text(quote(a)))))));
        }
        out.push(copy, info("The script and the tokens around it are argv and environment overrides in display notation, not proof that a shell ran.", ctx, 1));
        details = [h("div", { class: "label" }, "Full command line"), h("pre", { class: "command" }, h("code", {}, text(cmd.line)))];
      } else {
        out.push(h("pre", { class: "command" }, h("code", {}, text(cmd.line)))); // Tabs and newlines visible.
        if (cmd.exact) {
          out.push(copy, info("Display notation of argv and environment overrides, not a shell invocation.", ctx, 1));
        } else {
          out.push(info("Display notation of argv and environment overrides, not a shell invocation.", ctx, 1));
          out.push(h("div", { class: "warning" }, "Not copyable: ", text(cmd.problems.join("; ")), ". Exact tokens:"));
          out.push(h("ol", { class: "tokens" }, argv.map((a) => h("li", {}, h("code", {}, text(a))))));
          if (env.length) { out.push(h("dl", { class: "obj" }, env.map(([k, v]) => [h("dt", {}, text(k)), h("dd", {}, h("code", {}, text(v)))]))); }
          if (script) {
            out.push(h("div", { class: "label" }, "Script passed to ", text(argv[0]), " ", text(argv[script.flagIndex])));
            out.push(h("pre", { class: "script" }, h("code", {}, text(script.script, true))));
          }
        }
      }
    }
    const cwd = field(args, "cwd");
    if (cwd) { out.push(line("Working directory", cwd)); }
    return [h("div", { class: "launch" }, out), used, details];
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
      return h("li", {}, shown, nestedFields("terminal_write", "input[]", it, [main], "Other fields of this input"));
    });
    return [h("div", {}, line("Session", field(args, "session_id")), h("ul", { class: "inputs" }, items)), ["input", "session_id"]];
  },

  file_edit(args, ctx) {
    const edits = field(args, "edits");
    if (!edits || edits.kind !== "array") { return [null, []]; }
    const out = [line("File", field(args, "path"))];
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
      out.push(nestedFields("file_edit", "edits[]", e, ["old_text", "new_text"], "Other fields of this replacement"));
    });
    out.push(info("The requested change, not the file's result; no file line numbers are recorded.", ctx, 1));
    return [h("div", { class: "edit" }, out), ["edits", "path"]];
  },

  file_write(args) {
    const content = field(args, "content");
    if (!content || content.kind !== "string") { return [null, []]; }
    return [h("div", {}, line("File", field(args, "path")),
      h("pre", { class: "content" }, h("code", {}, text(content.value, true)))), ["content", "path"]];
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

// inputView renders what the model asked for: [the visible view, the
// protocol fields for Details (or null)].
function inputView(item, ctx) {
  const a = item.action;
  if (a.input === "not_recorded") {
    return [h("div", { class: "not-recorded" }, "Input not recorded · tool not executed"), null];
  }
  if (!a.start) { return [h("div", { class: "note" }, "The start of this call is outside the loaded history."), null]; }
  const rs = recordState(ctx, a.start.primary);
  if (rs.state !== "loaded") {
    return [h("div", {}, item.summary ? h("div", { class: "summary" }, h("span", { class: "label" }, "Summary (may be incomplete): "), text(item.summary.text),
      item.summary.truncated ? "…" : "") : null, controls(a.start.primary, rs)), null];
  }
  const p = rs.parsed;
  const legacy = a.input === "legacy" || a.legacy ? h("div", { class: "warning" }, "Legacy record: the input may be incomplete.") : null;
  if (p.issues.length > 0) {
    return [h("div", { class: "lossy" }, legacy, h("div", { class: "warning" }, "Shown as recorded text: ", text(p.issues.join("; ")), "."),
      h("pre", { class: "raw" }, h("code", {}, text(p.text, true, !p.validUTF8)))), null];
  }
  const args = field(p.value, "args");
  const out = [legacy];
  if (Object.hasOwn(NOT_RECORDED_OUTPUT, a.tool)) { out.push(info(NOT_RECORDED_OUTPUT[a.tool], ctx, 0)); }
  const render = Object.hasOwn(INPUT, a.tool) ? INPUT[a.tool] : null;
  let used = [];
  let rendered = !render;
  let extra = null; // What a renderer moves to Details (a script's full command line).
  if (render && args) {
    const [view, u, more] = render(args, ctx);
    if (view) { out.push(view); used = u; rendered = true; extra = more || null; }
  }
  let protocol = extra;
  if (args && args.kind === "object") {
    const rest = args.entries.filter((e) => !used.includes(e.key));
    // Everything stays visible when the split could mislead: a rejected or
    // failed call (the bad value may be a protocol field), input a
    // renderer could not show, or a tool this page does not know.
    const failed = ["rejected", "failed", "error"].includes(a.status) || a.outcome === "failed";
    const split = rendered && knownTool(a.tool) && !a.rejected && !failed;
    const cls = (e) => (split ? fieldClass(a.tool, e.key) : "main");
    const zeroLimit = (e) => split && a.tool === "terminal_start" && e.key === "max_runtime_ms" && e.value.kind === "number" && e.value.value === 0;
    const main = rest.filter((e) => cls(e) === "main" && !zeroLimit(e));
    const undefinedFields = rest.filter((e) => cls(e) === "");
    const folded = rest.filter((e) => cls(e) === "protocol" || (cls(e) === "main" && zeroLimit(e)));
    if (main.length) { out.push(fieldList(main, used.length ? "Also supplied" : "Supplied fields", "other", knownTool(a.tool) ? a.tool : "")); }
    if (undefinedFields.length) { out.push(fieldList(undefinedFields, "Not in this server version's schema", "undefined-fields")); }
    if (folded.length) { protocol = [extra, fieldList(folded, "Retry identity, waits and limits")]; }
  } else if (args) {
    out.push(h("div", { class: "label" }, "Supplied arguments"), value(args));
  } else {
    out.push(h("div", { class: "note" }, "No arguments were recorded."));
  }
  return [h("div", { class: "input" }, out), protocol];
}

// fieldList lists fields; with a tool, keys inside them that its schema
// does not define are marked.
function fieldList(entries, title, cls = "other", tool = "") {
  return h("div", { class: cls }, h("div", { class: "label" }, title),
    h("dl", { class: "obj" }, entries.map((e) => [h("dt", {}, text(e.key)), h("dd", {}, tool ? schemaValue(tool, e.key, e.value) : value(e.value))])));
}

// moreDetails is the card's one Details section: what steers the call,
// timing, the full recorded result, call metadata and the original
// records. Built eagerly (except the records' text), so nothing in it is
// lost from the card.
function moreDetails(item, ctx, parts) {
  const key = item.key + ":details";
  const attrs = { class: "more", "data-key": key };
  if (ctx.open && ctx.open.has(key)) { attrs.open = true; }
  return h("details", attrs, h("summary", {}, "Details"), parts);
}

function callMeta(item, ctx) {
  const a = item.action;
  if (!a || !a.start) { return null; }
  const rs = recordState(ctx, a.start.primary);
  if (rs.state !== "loaded" || !rs.parsed.value || rs.parsed.issues.length) { return null; } // Lossy: see the raw record.
  const rows = ["call_meta", "input_responses", "request_state"].map((k) => line(k, field(rs.parsed.value, k))).filter(Boolean);
  const key = item.key + ":meta";
  const attrs = { class: "meta", "data-key": key };
  if (ctx.open && ctx.open.has(key)) { attrs.open = true; }
  return rows.length ? h("details", attrs, h("summary", {}, "Call metadata (client-claimed)"), rows) : null;
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

// plain turns a parsed JSON node into a JS value (numbers that are not
// exact stay text); only for reading recorded facts, never for display.
function plain(node) {
  if (!node) { return undefined; }
  switch (node.kind) {
    case "object": { const o = Object.create(null); for (const e of node.entries) { if (!Object.hasOwn(o, e.key)) { o[e.key] = plain(e.value); } } return o; }
    case "array": return node.items.map(plain);
    case "string": return node.value;
    case "number": return node.unsafe ? node.text : node.value;
    case "bool": return node.value;
    default: return null;
  }
}

// recorded answers the plain value of a loaded, lossless record, or null.
function recorded(ctx, phase) {
  if (!phase) { return null; }
  const rs = recordState(ctx, phase.primary);
  return rs.state === "loaded" && rs.parsed.value && !rs.parsed.issues.length ? plain(rs.parsed.value) : null;
}

// processKey names a terminal process across the calls that touch it.
export function processKey(item) {
  const p = item.action && item.action.process;
  return p ? p.server_id + "\u0000" + p.session_id : "";
}

// processSeen collects, per process, the newest observation recorded by
// any loaded call of the timeline: {state, exit, time}. A command's card
// shows it, so its signature depends on it (see app.js entrySig).
export function processSeen(items, ctx) {
  const out = new Map();
  for (const item of items) {
    const key = processKey(item);
    if (!key || !item.action.finish) { continue; }
    const fin = recorded(ctx, item.action.finish);
    if (!fin || !fin.process || typeof fin.process !== "object") { continue; }
    const time = item.action.finish.time || "";
    const prev = out.get(key);
    if (!prev || time >= prev.time) { out.set(key, { state: fin.process.state, exit: fin.process.exit, time }); }
  }
  return out;
}

// facts gathers what the outcome model reads from the records.
function factsOf(item, ctx) {
  const a = item.action;
  const fin = a ? recorded(ctx, a.finish) : null;
  const op = item.effect ? recorded(ctx, item.effect.finish) : null;
  const receipt = op && op.receipt && typeof op.receipt === "object" ? op.receipt : null;
  // Result values only of an applied action: a failed receipt may name an
  // intended path or version that does not exist.
  const applied = receipt && (receipt.status === "applied" || receipt.status === "partial");
  const data = applied && receipt.data && typeof receipt.data === "object" ? receipt.data : null;
  const f = {};
  if (fin) {
    for (const k of ["error", "process", "files_total", "files_failed", "truncated", "search_matches", "reason"]) {
      if (fin[k] !== undefined) { f[k] = fin[k]; }
    }
  }
  if (!f.error && op && typeof op.error === "string") { f.error = op.error; }
  if (data && typeof data.bytes_written === "number") { f.bytes_written = data.bytes_written; }
  if (a && a.tool === "terminal_start") {
    const seen = ctx.processes && ctx.processes.get(processKey(item));
    if (seen) {
      f.process = { state: seen.state, exit: seen.exit };
      f.seen = localTimeText(seen.time);
    } else if (f.process && a.finish) {
      f.seen = localTimeText(a.finish.time);
    }
  }
  f.data = data;
  return f;
}

function localTimeText(t) {
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleTimeString();
}

// essentials shows the few result values that matter, inline.
function essentials(item, f) {
  const a = item.action;
  const d = f.data;
  const parts = [];
  if (d && typeof d.path === "string" && a && a.tool === "file_receive") { parts.push(["Saved to ", h("code", {}, text(d.path))]); }
  if (d && typeof d.version === "string") { parts.push(["version ", h("code", {}, text(d.version))]); }
  if (d && typeof d.bytes === "number") { parts.push(d.bytes.toLocaleString("en-US") + (d.bytes === 1 ? " byte" : " bytes")); }
  if (a && a.tool === "file_read" && typeof f.files_total === "number") {
    parts.push(f.files_total + (f.files_total === 1 ? " file" : " files") + (f.files_failed ? ", " + f.files_failed + " failed" : "") + (f.truncated === true ? ", cut to the size limit" : ""));
  }
  return parts.length ? h("div", { class: "essentials" }, parts.flatMap((x, i) => (i ? [" · ", x] : [x]))) : null;
}

const KIND_CLASS = { done: "ok", started: "ok", checked: "neutral", failed: "bad", not_run: "bad", partial: "warn", stopped: "warn", unknown: "warn", running: "live", repeat: "neutral" };

function actionHeader(item, ctx, f) {
  const a = item.action;
  const e = item.effect;
  const time = (a.start || a.rejected || a.finish || {}).time;
  const start = recorded(ctx, a.start);
  const args = start && start.args && typeof start.args === "object" ? start.args : null;
  const o = outcome(item, f);
  const dur = headDuration(item, localTimeText);
  // The server treats an empty expected_version as "create".
  const replaces = !!args && typeof args.expected_version === "string" && args.expected_version !== "";
  const parts = [h("strong", { class: "verb", title: a.tool || "" }, text(verb(a.tool, { inputRecorded: !!args && !a.legacy, hasExpectedVersion: replaces }))),
    " ", h("span", { class: "outcome-chip " + chipClass(o) }, text(o.label)),
    " ", localTime(time)];
  if (dur.text) { parts.push(" ", h("span", { class: "dur", title: dur.title }, text(dur.text))); }
  if (a.replayed) {
    parts.push(" ", h("span", { class: "badge replay" }, "of operation ", text(a.effect ? a.effect.effect.operation_id : "?"),
      a.effect && !a.effect.loaded ? " (its effect is outside the loaded history)" : ""));
  }
  const creator = a.process_creator;
  if (creator && !(creator.server_id === a.key.server_id && creator.request_id === a.key.request_id)) {
    parts.push(" ", h("span", { class: "note" }, "process started by request ", text(creator.request_id)));
  }
  if (a.possibly_unrecorded || (e && e.possibly_unrecorded)) { parts.push(" ", badge("warn", "finish may be unrecorded (audit was degraded)")); }
  const phases = [a.start, a.finish, a.rejected, e && e.start, e && e.finish].filter(Boolean);
  if (phases.some((p) => p.conflict) || (e && e.origin_conflict)) { parts.push(" ", badge("warn", "Ambiguous: journal copies differ")); }
  if (phases.some((p) => p.more > 0)) { parts.push(" ", badge("warn", "more journal copies than shown")); }
  if (phases.some((p) => (p.occurrences || [p.primary]).some((id) => id.startsWith("p:")))) { parts.push(" ", badge("warn", "pending row identity")); }
  if (a.identity && a.identity.metadataInvalid) { parts.push(" ", badge("warn", "unusable client metadata")); }
  if (a.effect && a.effect.kind === "observe") { parts.push(" ", h("span", { class: "note" }, "checks operation ", text(a.effect.effect.operation_id))); }
  const out = [h("div", { class: "card-head" }, parts)];
  // The outcome's detail: an error in full, counts, or why it did not run.
  if (o.detail) { out.push(h("div", { class: o.kind === "failed" || o.kind === "not_run" ? "outcome-detail bad" : "outcome-detail" }, text(o.detail, true))); }
  const ess = essentials(item, f);
  if (ess) { out.push(ess); }
  return out;
}

// timingLine spells out every recorded duration and status, for Details.
function timingLine(item) {
  const a = item.action;
  const e = item.effect;
  const recordedStatus = [a && a.status ? "request " + a.status : "", a && a.outcome ? "outcome " + a.outcome : "", e && e.status ? "action " + e.status : ""].filter(Boolean).join(", ");
  const t = [timing(item), recordedStatus ? "Recorded status: " + recordedStatus + "." : ""].filter(Boolean).join(" ");
  return t ? h("div", { class: "timing note" }, text(t)) : null;
}

// chipClass colours an outcome: a command that exited non-zero or by a
// signal is a warning, not a success.
function chipClass(o) {
  if (o.warn) { return "warn"; }
  return KIND_CLASS[o.kind] || "neutral";
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
    const o = outcome(item, factsOf(item, c));
    return h("div", { class: "card effect-only", "data-key": item.key },
      h("div", { class: "card-head" }, h("strong", { class: "verb", title: e.tool || "" }, text(verb(e.tool))), " ",
        h("span", { class: "outcome-chip " + chipClass(o) }, text(o.label)), " ", localTime((e.start || e.finish || {}).time),
        " operation ", text(e.key.operation_id)),
      o.detail ? h("div", { class: o.kind === "failed" ? "outcome-detail bad" : "outcome-detail" }, text(o.detail, true)) : null,
      h("div", { class: "note" }, "The request that admitted this effect is outside the loaded history."),
      moreDetails(item, c, [timingLine(item), receipt(item, c), originals(item, c)]));
  }
  const [input, protocol] = inputView(item, c);
  return h("div", { class: "card", "data-key": item.key }, actionHeader(item, c, factsOf(item, c)), input,
    moreDetails(item, c, [protocol, timingLine(item), callMeta(item, c), receipt(item, c), originals(item, c)]));
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
  ctx.processes = processSeen(items, ctx);
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
