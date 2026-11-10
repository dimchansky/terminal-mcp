// The fixed tool classes that decide auto-switching and unread counting.

export const MUTATION = "mutation";
export const LAUNCH = "launch";
export const READ = "read";
export const POLLING = "polling";
export const UNKNOWN = "unknown";

const CLASSES = {
  file_edit: MUTATION, file_write: MUTATION, file_receive: MUTATION, terminal_write: MUTATION,
  terminal_signal: MUTATION, terminal_resize: MUTATION, terminal_close: MUTATION,
  terminal_start: LAUNCH,
  file_read: READ, file_list: READ, file_search: READ, terminal_list: READ,
  terminal_read: POLLING, file_receive_result: POLLING, file_receive_status: POLLING, // The last is legacy.
};

// Tool names are recorded text: never let one reach the object prototype.
export function toolClass(tool) { return Object.hasOwn(CLASSES, tool) ? CLASSES[tool] : UNKNOWN; }

// mayAutoSwitch: a new live start of this tool may move the selection.
export function mayAutoSwitch(tool) {
  const c = toolClass(tool);
  return c === MUTATION || c === LAUNCH || c === READ;
}

// isPolling: calls of this tool collapse into poll groups and count at most
// once while unseen.
export function isPolling(tool) { return toolClass(tool) === POLLING; }
