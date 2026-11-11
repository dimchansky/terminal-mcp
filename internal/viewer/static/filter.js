// Local search and filters over the loaded history. Pure: it reads only
// what the page already holds (records, the server's summary hints) and
// never loads anything for a search. Filters change the view, never the
// model; the caller computes process observations on the full timeline.

import { field } from "./json.js";
import { fieldClass, knownTool } from "./fields.js";
import { commandLine, interpreterScript } from "./shell.js";
import { verb } from "./outcome.js";
import { itemOutcome, startState, hasWarning, fieldPlacement, rowText } from "./cards.js";
import { fold } from "./lanes.js";

// Outcome kinds that need the owner's attention.
const ATTENTION = new Set(["failed", "not_run", "partial", "unknown"]);

// active says whether any filter is set.
export function active(f) {
  return !!(f && ((f.query && fold(f.query).trim()) || (f.tools && f.tools.size) || f.attention));
}

// alwaysShown: items that are the history's own warnings stay visible
// under every filter: unreadable rows, server restarts, effects whose
// request is not loaded, and items with a warning badge.
export function alwaysShown(item) {
  if (!item.action) { return true; } // A diagnostic, a server start or an effect without its request.
  return hasWarning(item);
}

// strings collects every text of a JSON node: keys, string values and
// number texts, at any depth.
function strings(node, out) {
  if (!node) { return out; }
  switch (node.kind) {
    case "string": out.push(node.value); break;
    case "number": out.push(node.text); break;
    case "bool": out.push(String(node.value)); break;
    case "array": for (const it of node.items) { strings(it, out); } break;
    case "object": for (const e of node.entries) { out.push(e.key); strings(e.value, out); } break;
  }
  return out;
}

// haystacks returns what a text search looks in, folded: {main, details,
// raw, hint, loaded}. main is what the card shows at a glance (fields it
// shows inline, the verb it shows, a command as argv joined with spaces
// and in its display notation where the card shows it); details what it
// folds into Details (fields that only steer the call, a script's full
// command line); raw the loaded text of a record the page cannot represent
// exactly; hint the server's summary for a record that is not loaded.
// Records never change, so the result is cached by their bytes.
const cache = new WeakMap();

export function haystacks(item, ctx) {
  const a = item.action;
  const out = { main: [], details: [], raw: "", hint: "", loaded: false };
  if (!a) { return out; }
  const rs = a.start ? startState(item, ctx) : null;
  if (!rs || rs.state !== "loaded") {
    out.main.push(fold(a.tool || ""), fold(verb(a.tool)));
    if (item.summary && typeof item.summary.text === "string") { out.hint = fold(item.summary.text); }
    return out;
  }
  const key = [a.tool, a.status, a.outcome, !!a.rejected, !!a.legacy, ctx.serverOS || ""].join("\u0000");
  let per = cache.get(rs.bytes);
  if (!per) { per = new Map(); cache.set(rs.bytes, per); }
  if (per.has(key)) { return per.get(key); }
  out.loaded = true;
  if (rs.parsed.issues.length || !rs.parsed.value) {
    out.main.push(fold(a.tool || ""), fold(verb(a.tool)));
    out.raw = fold(rs.parsed.text);
    per.set(key, out);
    return out;
  }
  const args = field(rs.parsed.value, "args");
  const replaces = !!args && args.kind === "object" && (() => { const v = field(args, "expected_version"); return !!v && v.kind === "string" && v.value !== ""; })();
  out.main.push(a.tool || "", verb(a.tool, { inputRecorded: !a.legacy, hasExpectedVersion: replaces }));
  if (args && args.kind === "object") {
    const place = fieldPlacement(item, args);
    for (const e of args.entries) {
      const where = place(e) === "details" ? out.details : out.main;
      where.push(e.key);
      strings(e.value, where);
    }
    if (a.tool === "terminal_start") {
      const argv = field(args, "argv");
      const env = field(args, "env");
      if (argv && argv.kind === "array" && argv.items.every((i) => i.kind === "string")) {
        const words = argv.items.map((i) => i.value);
        out.main.push(words.join(" "));
        // The POSIX notation only where the card shows it (not on Windows);
        // a script's full line is in Details.
        if (ctx.serverOS !== "windows" && (!env || (env.kind === "object" && env.entries.every((x) => x.value.kind === "string")))) {
          const pairs = env ? env.entries.map((x) => [x.key, x.value.value]) : [];
          const cmd = commandLine(pairs, words);
          const script = interpreterScript(words);
          if (cmd.exact && script) {
            out.main.push(commandLine(pairs, words.slice(0, script.scriptIndex)).line);
            out.details.push(cmd.line);
          } else {
            out.main.push(cmd.line);
          }
        }
      }
    }
  } else if (args) {
    strings(args, out.main);
  }
  out.main = out.main.map(fold);
  out.details = out.details.map(fold);
  per.set(key, out);
  return out;
}

// textMatch says where a query matches an item: "main", "details", "raw",
// "hint" (the server's summary only), or "" (no match); searchable is
// false when nothing of the item is loaded to search in.
export function textMatch(item, query, ctx) {
  const q = fold(query).trim();
  const h = haystacks(item, ctx);
  const searchable = h.loaded || !!h.hint;
  const has = (list) => list.some((s) => s.includes(q));
  let where = has(h.main) ? "main" : has(h.details) ? "details" : h.raw.includes(q) ? "raw" : h.hint.includes(q) ? "hint" : "";
  // A compact row shows only its cut main values: a match elsewhere in
  // its card is behind the row, as if in Details.
  if (where === "main") {
    const row = rowText(item, ctx);
    if (row !== null && !fold(row).includes(q)) { where = "details"; }
  }
  return { where, searchable };
}

// needsAttention: the outcome failed, did not run, was partly done or is
// unknown, or a command or poll ended badly (a non-zero exit, a signal, a
// stopped call).
export function needsAttention(item, ctx) {
  // A command's known result needs the full timeline's observations.
  if (!ctx.processes) { throw new Error("needsAttention: ctx.processes is not set (run timeline() on the full lane first)"); }
  const o = itemOutcome(item, ctx);
  return ATTENTION.has(o.kind) || !!o.warn || o.kind === "stopped";
}

// matches checks one item against the filters: {match, where, searchable}.
// Items that are the history's warnings are shown regardless (keep: true)
// but do not count as matches unless they match.
export function matches(item, f, ctx) {
  const keep = alwaysShown(item);
  if (!item.action) { return { match: false, keep, where: "", searchable: false }; }
  if (f.tools && f.tools.size && !f.tools.has(item.action.tool)) { return { match: false, keep, where: "", searchable: true }; }
  if (f.attention && !needsAttention(item, ctx)) { return { match: false, keep, where: "", searchable: true }; }
  const q = f.query ? fold(f.query).trim() : "";
  if (!q) { return { match: true, keep, where: "", searchable: true }; }
  const t = textMatch(item, q, ctx);
  return { match: !!t.where, keep, where: t.where, searchable: t.searchable };
}
