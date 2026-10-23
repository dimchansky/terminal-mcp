# Development tools and supervised updates (0.2)

## One-shot processes without manual cleanup

Obtain `server_id` with `terminal_list`, then use `terminal_start` with
`close_on_exit: true`, `wait_for: "completion"`, and a bounded `wait_ms`.
Defaults remain false and `output` for existing clients.

```json
{"server_id":"<current>","operation_id":"test-1","argv":["go","test","./..."],"cwd":"/absolute/project","close_on_exit":true,"wait_for":"completion","wait_ms":1000}
```

Completion waits for process exit AND output drain, plus automatic cleanup when
requested. Output limits, gaps and the absolute deadline can return earlier;
continue `terminal_read` with `next_cursor` and `wait_for: "completion"`.
Inspect exit status, `has_more`, `gap` and `output_incomplete`. A deadline or
cancelled read never kills the process. Auto-close preserves retained output and
exit status, does not require a reader, and does not consume another mutation ID.
For PTY, exit means the launched shell exits, not that one command inside it ends.

## Listing sessions

`terminal_list` with `{}` returns only unclosed sessions and counts of all
retained sessions (`running`, `exited_unclosed`, `closed_retained`). Pass
`{"include_closed":true}` to inspect closed retained sessions. Filtering does
not delete or shorten their stored output; `terminal_read` by ID still works.
The supervised updater requests the full list before a restart so that it cannot
miss active sessions during cleanup.

## Files: five synchronous tools, no terminal session

The executable exposes twelve tools. `--file-root` defaults to the server working
directory. File paths must be absolute and within this root; Go `os.Root` enforces
traversal-safe access. This does not sandbox the unrestricted shell.

### file_read

```json
{"files":[{"path":"/absolute/project/main.go","start_line":10,"end_line":80}],"max_bytes":16384}
```

Read 1..8 file ranges with one-based inclusive line numbers. Default 200 lines,
maximum 1000 per range; shared returned text budget defaults to 16 KiB, max 64 KiB.
Responses include numbered lines, `version: "sha256:..."` for the WHOLE file,
`next_line`, explicit truncation, and per-file errors. Text retains original line
endings. Supported files are ordinary UTF-8, no NUL, up to 2 MiB. Binary files,
devices, FIFOs and oversized files are refused. A line too large for the remaining
budget returns an explicit error; use the terminal for unusual/minified files.

### file_edit

```json
{"server_id":"<current>","operation_id":"edit-1","path":"/absolute/project/main.go","expected_version":"sha256:<from-read>","edits":[{"old_text":"exact old fragment","new_text":"replacement"}]}
```

Require the observed whole-file version. Each of 1..32 exact replacements must
match once, without overlap, against the same original snapshot. No fuzzy or
whitespace-normalized matching. A mismatch rejects the entire edit. Combined
replacement input is limited to 64 KiB. The receipt reports old/new versions and
a bounded human-readable preview, not an applyable unified diff.

### file_write

Arguments: `server_id`, `operation_id`, `path`, `content`, optional
`expected_version`. Omitting the version means CREATE ONLY; an existing file is
never silently overwritten. Replacing an existing file requires its version.
Content is UTF-8 up to 64 KiB; parent directories must exist. No newline conversion.

Publication is atomic for ONE file. Normal permissions are retained, but ACLs and
extended attributes are NOT preserved; do not edit files relying on them through
this API. Read-only, non-owned, symlink and hard-linked targets are refused.
There is no cross-file transaction. Version checks detect stale edits but are not
an OS-wide compare-and-swap against an editor saving concurrently. Coordinate
writers or use separate worktrees.

File mutations share the server-instance fence and bounded retry ledger with
terminal mutations. After a lost response, reuse the same key and arguments.
A changed server ID requires investigation, not automatic replay.

### file_list and file_search

`file_list`: directory `path`, optional basename `glob` (Go filepath.Match),
`limit` default 200/max 1000. One directory, including hidden/ignored entries;
symlinks are shown without traversal. Narrow the query when truncated.

`file_search`: directory `path`, `pattern`, optional `regex`, `glob`, `hidden`,
`max_results` default 100/max 500. Literal per-line matching by default; regex
uses Go syntax. Installed `rg` provides ignore-aware path discovery, while file
content is opened through `os.Root`. No shell interpolation. Search is bounded
by 5 seconds and 64 KiB returned text; skipped files and truncation are reported.
Only search requires ripgrep (`brew install ripgrep` on macOS).

## macOS: a server independent of the Terminal window

`scripts/service.py` uses Python 3 standard library and per-user launchd. No root
privileges are needed. It manages terminal-mcp, NOT tunnel-client: keep the tunnel
running independently. The initial installation adopts an already working server
at 127.0.0.1:8090 using ~/.config/terminal-mcp/token and this repository as file root.

### Service configuration and file access root

User-managed configuration lives at:

`~/Library/Application Support/LocalTerminalMCP/config.json`

`project` sets the launchd working directory and remains the default file root;
`listen`, `token_file` and `path` control HTTP binding, authentication-file
location and subprocess PATH. For this personal MCP, new installations set
`"file_root": "/"` so file_* tools can address all accessible absolute paths.
The current owner's service uses the same setting. Change it to another absolute
path in config.json to restrict the file tools again.
The repository root, not the chosen file_root, remains the service working
directory. Run `make update` after editing; preflight validates the new setting
before switching the live process. A path error prevents the rollout. Do not
change the managed plist directly. This is a filesystem tool boundary only;
unrestricted shell execution uses the service account privileges regardless.

The audit log uses `~/Library/Application Support/LocalTerminalMCP/audit.jsonl`
under the private state directory (0600; 4 MiB plus three archives). It records
start/finish events, exact terminal_start argv/cwd/env/options and terminal_write
input, plus session IDs and outcomes. File tools log targeted paths, ranges,
search parameters, action types and byte counts, excluding file contents, edit
strings and output. The audit cannot attribute calls to a person while all callers use one
bearer token. It is not a tamper-proof journal; disk failure after an action
cannot be undone. `make logs` follows the rotated filename and
`make logs-service` shows runtime diagnostics. Preflight instances explicitly
disable audit output to avoid test noise in the production file.

### Makefile shortcuts and first installation

Use `make help` for available targets. `make check` tests and builds; `make build`
only builds the binary without touching the running service. `make update` uses
the same tested supervisor workflow as `python3 scripts/service.py update`;
`make status`, `make start`, and `make stop` delegate to the controller.
`make logs` follows the bounded JSONL MCP tool audit; `make logs-service` tails
stderr diagnostics. Terminal arguments are recorded verbatim, including values
that may be sensitive if sent as command arguments; the journal is local-only.

For a fresh installation (no existing server on 127.0.0.1:8090), first create
the HTTP token according to README, then run `make install`. It tests, builds,
preflights and registers the user LaunchAgent. Repeating `make install` while
registered and running does not restart it. If intentionally stopped, it stays
stopped; use `make start`. Following `make uninstall`, `make install` restores
registration from retained configuration and binary releases without rebuilding.

An existing manually launched server cannot be silently adopted by `make install`.
For the initial transition only, save its known-working binary BEFORE rebuilding.
Find the old listener PID with `lsof -nP -iTCP:8090 -sTCP:LISTEN`. Finish active
managed jobs, then run from this repository:

```sh
test -e bin/terminal-mcp.previous || cp -p bin/terminal-mcp bin/terminal-mcp.previous
python3 scripts/service.py install --adopt-pid <LISTENER_PID> --previous-binary bin/terminal-mcp.previous
```

The saved binary is preflighted on a separate loopback port and its tool metadata
must match the running server. The controller then runs the full check script,
stages a new immutable binary, and preflights process/auto-cleanup, file listing
and discovery. Only then is an independent launchd update job submitted.
The first handover from version 0.1 has no maintenance fence: use an idle window
and avoid concurrent commands. The old PID is checked again before signalling it.

### Update from a tool call on the server being updated

```sh
python3 scripts/service.py update
python3 scripts/service.py status
```

The update command returns a job ID and persistent status-file path. It is NOT
success until status is `applied` and the MCP endpoint is healthy. The worker is
owned by launchd, not the old MCP process group. It waits for the requesting
command and other managed jobs to finish. Then it acquires an authenticated
maintenance fence, preventing new mutations while changing the service.
Busy updates abort without stopping jobs. A 60-second generation-fenced lease
unfreezes the old server if the updater dies before restarting it.

The worker atomically selects an immutable release, restarts the agent, checks a
new server ID and matching metadata, and records success. Readiness/metadata
failure triggers restoration of the previous release. Builds never overwrite the
live deployed executable. A manual stop marker overrides restart and rollback.

This is a supervised restart with a BRIEF OUTAGE, NOT zero-downtime migration.
Live PTYs, retained output, retry receipts and session IDs do not migrate between
processes. Get the new server ID; never silently replay old actions against it.
Prepare output you need before requesting an update.

`refresh_tools_required` compares actual schemas, descriptions, annotations and
server instructions; binary version alone is ignored. When true, click Refresh
tools in the ChatGPT plugin after deployment. Implementation-only updates with
unchanged metadata need no refresh. Until refreshed, the model may still see old
signatures even though the server has the new implementation.

### Intentional stop and registration removal

```sh
make stop       # Stop without removing registration
make start      # Start a registered service
make uninstall  # Stop and remove LaunchAgent registration
make install    # Register again using retained state
```

`make stop` records an explicit marker, disables and unloads the agent. KeepAlive
and updates will not override it. `make start` explicitly reenables a registered
service; if unregistered, use `make install`. A plain kill is considered a crash
and launchd restarts it.

`make uninstall` refuses a pending update. It records an explicit stop, unloads
the agent, verifies launchd no longer has the job, then deletes ONLY its plist
registration (not tokens, configuration, immutable releases, logs or update jobs).
It is idempotent; a failed bootout preserves the plist for recovery. Never invoke
uninstall from the same service's MCP terminal session: the connection will be
interrupted. The tunnel-client runs independently and is not uninstalled.

A user LaunchAgent requires the logged-in GUI domain, not a system boot daemon,
and it does not keep sessions alive across logout or a machine crash.

State: ~/Library/Application Support/LocalTerminalMCP (private permissions).
Agent: ~/Library/LaunchAgents/com.dimchansky.terminal-mcp.plist.
The live executable is current/terminal-mcp inside the state directory, selected
from releases/<sha256>/. Logs are stdout.log, stderr.log and jobs/<id>/worker.log.
Token values are not placed in plists or command arguments. Completed update
records/releases remain available for diagnosis; no automatic pruning is claimed.
If the updater itself is killed, inspect the job and pending marker locally before
starting another update. Never clear pending while its worker may still be alive.
