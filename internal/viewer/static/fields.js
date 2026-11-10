// How a card shows each input field of each tool. "main" fields are the
// substance of the call and always visible; "protocol" fields steer the
// call (retry identity, waits, limits) and sit in the card's Details.
// Paths name nested fields (file.download_url, edits[].old_text); maps
// such as env are leaves. A field no tool schema defines is never listed
// here and stays visible.
//
// Deliberate cases: file_receive_result's operation_id is main (it names
// the download being checked); terminal_start's max_runtime_ms is main,
// and the card shows a supplied zero (no limit) in Details instead;
// expected_version is protocol, and its presence shows in the verb
// (Create or Replace file).
//
// The block between the markers is strict JSON: a Go test in
// internal/mcpserver compares it with the tools' input schemas, so every
// schema field is classified exactly once.

export const FIELDS = /*BEGIN-FIELDS*/{
  "file_edit": {
    "main": ["path", "edits", "edits[].old_text", "edits[].new_text"],
    "protocol": ["expected_version", "operation_id", "server_id"]
  },
  "file_list": { "main": ["path", "glob"], "protocol": ["limit"] },
  "file_read": {
    "main": ["files", "files[].path", "files[].start_line", "files[].end_line"],
    "protocol": ["max_bytes"]
  },
  "file_receive": {
    "main": ["path", "file", "file.download_url", "file.file_id", "file.file_name", "file.mime_type"],
    "protocol": ["operation_id", "server_id", "wait_ms"]
  },
  "file_receive_result": { "main": ["operation_id"], "protocol": ["server_id", "wait_ms"] },
  "file_search": { "main": ["path", "pattern", "regex", "glob", "hidden"], "protocol": ["max_results"] },
  "file_write": { "main": ["path", "content"], "protocol": ["expected_version", "operation_id", "server_id"] },
  "terminal_close": { "main": ["session_id", "grace_ms"], "protocol": ["operation_id", "server_id"] },
  "terminal_list": { "main": ["include_closed"], "protocol": [] },
  "terminal_read": {
    "main": ["session_id"],
    "protocol": ["cursor", "max_bytes", "until", "view", "wait_for", "wait_ms"]
  },
  "terminal_resize": { "main": ["session_id", "rows", "cols"], "protocol": ["operation_id", "server_id"] },
  "terminal_signal": { "main": ["session_id", "signal", "target"], "protocol": ["operation_id", "server_id"] },
  "terminal_start": {
    "main": ["argv", "env", "cwd", "mode", "max_runtime_ms"],
    "protocol": ["close_on_exit", "cols", "rows", "max_bytes", "operation_id", "server_id", "wait_for", "wait_ms"]
  },
  "terminal_write": {
    "main": ["session_id", "input", "input[].text", "input[].key", "input[].bytes_base64", "close_stdin"],
    "protocol": ["cursor", "max_bytes", "operation_id", "server_id", "wait_ms"]
  }
}/*END-FIELDS*/;

// fieldClass answers "main", "protocol", or "" (not in this server
// version's schema, or a tool not listed) for a tool's field path.
export function fieldClass(tool, path) {
  if (!Object.hasOwn(FIELDS, tool)) { return ""; }
  const f = FIELDS[tool];
  if (f.main.includes(path)) { return "main"; }
  if (f.protocol.includes(path)) { return "protocol"; }
  return "";
}

// hasChildren reports whether the schema defines fields inside a path (an
// object such as file, or array items such as edits[]); maps such as env
// do not, so their keys are values, not fields.
export function hasChildren(tool, path) {
  if (!Object.hasOwn(FIELDS, tool)) { return false; }
  const f = FIELDS[tool];
  return [...f.main, ...f.protocol].some((p) => p.startsWith(path + ".") || p.startsWith(path + "[]"));
}

// knownTool reports whether a tool's fields are classified.
export function knownTool(tool) { return Object.hasOwn(FIELDS, tool); }
