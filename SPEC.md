# Local Terminal MCP — Behavior Specification

**Current interface:** 12 MCP tools. The server reports MCP implementation version `0.2.0`.

**Platforms:** macOS and Linux. **Access model:** one trusted local service account.

This document describes the current implementation. Use [README.md](README.md) for setup instructions. Use [DEVELOPMENT.md](docs/DEVELOPMENT.md) for operational procedures.

The terms `server_id`, `session_id`, `operation_id`, `receipt`, and `observation` are protocol terms. Use these exact terms.

## 1. Purpose and limits

The server lets an MCP client control real local processes. The client can start a process, read output, send input, and stop it.

An active process does not depend on an individual HTTP request or MCP client connection.

The server is not a sandbox. A process has the permissions of the service user. The service does not use a command allowlist.

The server does not provide:

- Windows or ConPTY support.
- Per-user authorization or an OAuth authorization server.
- A browser-based terminal.
- Native SSH or serial-device APIs.
- Restoration of live sessions after a server crash.
- Full process-tree containment.
- A terminal screen-diff protocol.
- Unsolicited MCP notifications or MCP Tasks.

You can run an SSH client as a local process. The server cannot control arbitrary remote SSH processes by local signal.

## 2. Process and service architecture

The Go executable provides HTTP or stdio transport. It contains the session manager and the MCP tools.

Startup uses a dedicated `flag.FlagSet`. The server accepts an explicit
`context.Context` for cancellation; the executable subscribes to SIGINT and
SIGTERM. HTTP shutdown stops managed processes before it drains HTTP requests
within a bounded deadline. If the drain deadline expires, the server closes
remaining connections and reports the error.

```text
MCP client
    |
    +-- HTTP /mcp (default) or stdio
             |
             v
       MCP tool handlers
             |
       +-----+------------------+
       |                        |
       v                        v
  Session manager          File manager
       |                   (Go os.Root)
       |
  +----+--------------------+
  |                         |
  v                         v
Pipes                      PTY
stdin, stdout, stderr      input, screen, raw events
  |                         |
  +-----------+-------------+
              v
        Output journal

Independent macOS controller:
Makefile -> scripts/service.py -> launchd worker
                                   |
                                   v
                           release and restart
```

The session manager stores sessions and mutation receipts in memory. The audit logger stores tool-call events on disk.

The `terminal` package uses `creack/pty` and `vt10x`. The MCP Go SDK handles message transport and tool schemas.

### HTTP transport

HTTP is the default mode. The server listens on `127.0.0.1:8090` unless an operator changes the launch arguments.

The MCP endpoint is `/mcp`. The service also provides an authenticated `/admin/maintenance` endpoint.

The HTTP transport is stateless at the protocol level. The process manager keeps sessions between requests.

A tunnel can reconnect while the Go server continues to run. A server restart removes its session state.

### Stdio transport

Use `--transport=stdio` when an MCP host starts the server directly. Stdio provides the same 12 tools.

Stdio uses stdout for MCP data and stderr for diagnostics. It does not require the HTTP token.

When the stdio server process exits, it closes its managed sessions.

## 3. IDs and action receipts

Each server process creates a random `server_id`. Each managed process gets a random `session_id`.

Call `terminal_list` to get the current `server_id`. All mutating tools require this ID and an `operation_id`.

Use a new `operation_id` for a new action. To retry an accepted action, use the same ID and the same effect arguments.

The server rejects an old `server_id`. Do not use the new `server_id` to replay an old action automatically.

A mutation returns a `receipt`. The status can be `applied`, `failed`, `partial`, or `unknown`.

A receipt describes action submission. It does not prove that a child program completed its work.

The server can return the same receipt for a repeated `operation_id`. The action does not run a second time in that server instance.

The retry ledger has a finite limit. At capacity, it refuses new mutations instead of losing the old receipts.

**Guarantee:** at-most-once mutation submission in one server instance. This is not exactly-once external execution.

A restart removes the ledger. A response lost during restart can have an unknown outcome. Inspect the external effect before you submit a new action.

## 4. Process lifecycle

A session owns one directly started process and its I/O handles.

The public process states are `running` and `exited`:

- `running` means the server has not observed process exit.
- `exited` means the server has observed process exit.

A stopped process can still have state `running`. A process can also wait for input without using CPU.

The `exit` data contains an exit code or a terminating signal. It can also contain an error.

`output_done` is separate from process exit. The server reads remaining output after exit.

A child process can keep a pipe open after its parent exits. The server uses a bounded drain period. If the drain fails, it sets `output_incomplete`.

The server does not claim that an incomplete drain is a complete transcript.

### Close behavior

Use `close_on_exit: true` to release I/O automatically after process exit and output drain.

Use `terminal_close` to close a session explicitly. A close can send TERM and CONT. It can later send KILL after a bounded grace period.

The server does not send Ctrl+D automatically.

A closed session can remain in memory for inspection. Closing a session does not immediately delete retained output.

### Session lists and retention

By default, `terminal_list({})` shows unclosed sessions only. Set `include_closed: true` to include retained closed sessions.

The `counts` object covers all retained sessions:

| Count | Meaning |
| --- | --- |
| `running` | The server has not observed process exit. |
| `exited_unclosed` | The process exited, but the session is not closed. |
| `closed_retained` | The session is closed and still retained. |

A session that is closing can appear in both `running` and `closed_retained`.

The default completed-session limits are one hour and 128 sessions. The manager removes expired sessions during normal access.

Active sessions are not removed by retention limits. A restart removes all in-memory sessions.

## 5. Process modes

### Pipes

`mode: "pipes"` is the default. Use this mode for commands that do not need a terminal.

The server records stdout and stderr as separate streams. Cross-stream order is the order in which the server reads data.

The server does not claim the original order of writes to both OS streams.

### PTY

`mode: "pty"` creates a controlling terminal. It supports interactive text input, terminal keys, resize, and screen output.

A PTY has one combined `terminal` output stream. The server cannot separate its stdout and stderr.

The screen view is a text rendering. Raw output events keep control bytes.

The screen view includes terminal dimensions, cursor position, and alternate-screen state. It does not show all modern terminal graphics.

The PTY implementation uses nonblocking writes and bounded waits. Failed terminal protocol replies appear in diagnostics.

### Program arguments

`terminal_start` runs the supplied `argv` directly. It does not interpret shell characters unless `argv` starts a shell.

Executable lookup uses the effective child `PATH`. Relative paths use the requested working directory.

The server inherits its environment and applies `env` overrides. It removes its HTTP authentication environment variable from the default child environment.

The server does not add `sudo` or elevate privileges.

## 6. Terminal tool contract

### `terminal_list`

**Input:** optional `include_closed`.

**Output:** `server_id`, `limits`, `counts`, `sessions`, supported modes and keys, mutation count, and drain state.

Session summaries exclude `argv` and `cwd`. Use `terminal_read` to get retained launch metadata.

### `terminal_start`

**Required:** `server_id`, `operation_id`, `argv`.

**Optional:** `cwd`, `env`, `mode`, `rows`, `cols`, `wait_ms`, `max_bytes`, `max_runtime_ms`, `close_on_exit`, `wait_for`.

The server starts one process and returns a receipt. It also tries to return an initial observation.

`max_runtime_ms` is a process lifetime limit. Zero disables this limit.

`wait_ms` is an observation wait. It does not limit the process lifetime.

Set `wait_for: "completion"` to wait for exit and output drain. If `close_on_exit` is true, this wait also checks automatic close.

A deadline, output cap, or journal gap can end the wait early.

### `terminal_read`

**Required:** `session_id`.

**Optional:** `cursor`, `view`, `wait_ms`, `max_bytes`, `until`, `wait_for`.

Reads do not consume output. An omitted cursor starts at position zero.

Cursors belong to one session. The server rejects an invalid cursor or a cursor beyond available output.

`view: "events"` is the default. `view: "screen"` returns a PTY screen.

`until` searches for a literal substring in the bounded result. It is not a regular expression.

Do not combine `until` with `wait_for: "completion"`.

A canceled read does not stop the process.

### `terminal_write`

**Required:** `server_id`, `operation_id`, `session_id`.

**Optional:** `input`, `close_stdin`, `cursor`, `wait_ms`, `max_bytes`.

Each input item contains one of `text`, `bytes_base64`, or `key`. The server sends text without adding Enter.

Use `close_stdin` with pipes. Use named terminal keys with PTY sessions.

The receipt reports accepted bytes. Accepted bytes do not prove that the child program processed them.

A replay returns the original action receipt. It does not send a missing suffix after a partial write.

### `terminal_signal`

**Required:** `server_id`, `operation_id`, `session_id`, `signal`.

**Optional:** `target`.

Supported signals are INT, TERM, KILL, HUP, CONT, and STOP.

The default target is `group`. You can select `process`, or `foreground` for a PTY.

The signal target is always local. A `CTRL_C` key is not the same action as an OS signal in every terminal mode.

### `terminal_resize`

**Required:** `server_id`, `operation_id`, `session_id`, `rows`, `cols`.

The server changes the PTY size and the rendered screen size. Pipes do not support resize.

### `terminal_close`

**Required:** `server_id`, `operation_id`, `session_id`.

**Optional:** `grace_ms`.

The maximum grace period is 2,000 ms. A value of zero requests immediate escalation.

A close keeps retained output available within the retention limits.

## 7. Output journal and observations

The session reader works independently of MCP read requests.

Each journal has a byte limit and an event-count limit. Each event records a stream and an offset.

A returned event contains valid UTF-8 `text` or exact `bytes_base64`.

The server can return base64 if a byte range splits a UTF-8 sequence. It does not replace or remove bytes from the authoritative stream.

A journal cursor is a byte position. `next_cursor` tells the client where to continue.

`has_more` means the bounded response does not contain all available data.

A `gap` gives the range of output that is no longer available. A gap is not an empty output result.

Two clients can use independent cursors. One client's read does not change the other client's cursor.

A PTY screen has a revision number. Screen reads do not advance a raw-output cursor.

## 8. Time, cancellation, and resource limits

Three time limits have different purposes:

1. Observation waits limit one read or the observation part of a tool call.
2. `max_runtime_ms` can limit the lifetime of a child process.
3. Completed-session retention limits how long old session data remains in memory.

An observation wait uses an absolute deadline. New output does not extend that deadline.

Input writes have a separate short deadline. A signal does not wait for an output reader.

Once the server accepts a mutation, it processes the mutation independently of the canceled observation wait.

The default limits are:

| Resource | Default or maximum |
| --- | --- |
| Active sessions | 32 |
| Retained completed sessions | 128 |
| Completed-session retention | 3,600 seconds |
| Output journal | 1 MiB and 2,048 events per session |
| Maximum events in one response | 256 |
| Default read size | 16 KiB |
| Maximum read size | 64 KiB |
| Maximum input size | 64 KiB |
| Maximum observation wait | 10,000 ms |
| Input write deadline | 1,000 ms |
| Mutation receipts | 16,384 |
| Maximum PTY size | 80 rows and 200 columns |

Use `terminal_list` to read the limits of the running instance.

## 9. File tool contract

The five file tools use Go `os.Root`. All file paths must be absolute and inside the configured `file_root`.

The macOS controller sets `file_root` to `/` on a new installation. This allows access to paths allowed by the service user's OS permissions.

If the configuration has no `file_root`, the Go server uses the working directory. `--file-root` overrides the configured root.

These restrictions apply only to file tools. They do not restrict arbitrary child commands.

### `file_read`

**Input:** one to eight file ranges, with optional line boundaries and `max_bytes`.

The server reads regular UTF-8 text files of at most 2 MiB. It refuses binary data and NUL bytes.

Lines have one-based numbers. A read returns a SHA-256 version for the whole file.

The default line window has 200 lines. The maximum window has 1,000 lines.

The total response-text budget defaults to 16 KiB and cannot exceed 64 KiB. A truncated response provides `next_line` when possible.

### `file_edit`

**Required:** `server_id`, `operation_id`, `path`, `expected_version`, `edits`.

Each edit specifies `old_text` and `new_text`. The old text must have exactly one match in the original snapshot.

All edits must be non-overlapping. The server refuses the entire action when validation fails.

The input has at most 32 edits and a combined text budget of 64 KiB.

The server writes one file with an atomic replacement. It returns the new version and a bounded preview.

### `file_write`

**Required:** `server_id`, `operation_id`, `path`, `content`.

**Optional:** `expected_version`.

Without `expected_version`, the server only creates a new file. To replace an existing file, supply its observed version.

The content must be UTF-8 and cannot exceed 64 KiB. The parent directory must exist.

### Common edit and write limits

File writes preserve normal permission bits. They do not preserve ACLs or extended attributes.

The server refuses unsupported targets, such as linked, special, non-owned, and read-only files.

A version check detects an old snapshot. The server checks the snapshot again before returning success for a no-op replacement. It also checks before publishing a changed file. An external writer can still change a file after this final check; there is no OS-wide compare-and-swap operation.

One-file publication is atomic. There is no transaction across several files.

File mutations use the same instance ID and retry ledger as terminal mutations.

### `file_list`

**Required:** directory `path`.

**Optional:** `glob`, `limit`.

The server lists one directory without following symlinks. It includes hidden entries.

The default limit is 200 entries. The maximum is 1,000 entries.

### `file_search`

**Required:** directory `path` and search `pattern`.

**Optional:** `regex`, `glob`, `hidden`, `max_results`.

The server uses `rg` for file discovery. It reads matching file content through `os.Root`.

The default is literal text search. `regex: true` uses Go regular expressions on individual lines.

The default result limit is 100. The maximum is 500. The search has a five-second time limit and a bounded output budget. An empty file has no searchable lines. A trailing newline does not create an extra empty line.

## 10. Local HTTP security

HTTP mode requires a bearer token. The token comes from a protected file or the `TERMINAL_MCP_TOKEN` environment variable.

The HTTP guard checks the bind address, Host, Origin, token, and request size.

It rejects non-loopback bind addresses and requests with invalid authentication. It does not enable CORS.

The maintenance endpoint uses the same HTTP guard as `/mcp`.

A shared bearer token identifies a trusted local service principal. It does not identify each individual client.

Do not expose the local HTTP endpoint through an unauthenticated public tunnel.

## 11. Audit log

In HTTP mode, the server opens a JSONL audit log in a private user-state directory.

The log records a `start` event before a tool callback runs. It records a `finish` event after the callback returns.

A `request_id` links the two events. A `session_id` links terminal actions that use one session.

The audit records all `terminal_start` arguments, including `argv`, `cwd`, `env`, and process options.

It also records the exact `terminal_write` input. This includes text, keys, and base64 values.

The audit records file paths, ranges, search terms, versions, edit counts, and content lengths. It does not store file content or replacement text from file tools.

**A command argument can contain a secret or file content.** If the model sends it as a command argument, the audit records it.

The audit does not copy stdout or stderr. It also does not record every rejected HTTP request or invalid MCP tool schema.

The caller sets the event timestamp before enqueue. Each tool sends a complete event to the auditor. The caller encodes the event and submits it to a bounded, nonblocking queue. One goroutine writes accepted events to JSONL in queue order. The queue has at most 1,024 events and 8 MiB of encoded data.

If the queue is full, the auditor drops the incoming event. It records the loss count in diagnostics. Audit errors never prevent tool execution. If audit initialization fails, the server starts without auditing and reports the error. The service must restart after the cause is fixed to enable audit logging again.

`Close()` stops new submissions, processes all accepted events, syncs the active file, and closes it. Disk errors are reported in diagnostics. A persistent disk stall can delay shutdown because `Close()` waits for the worker.

The audit keeps one active file and at most three rotated files. Each file has a 4 MiB size limit and private permissions. A partial write is truncated to the previous complete record when possible. The next startup also removes an incomplete final JSONL line.

The audit is not tamper-proof. A crash can lose queued or recently written events. There is no `fsync` for each event.

In stdio mode, auditing is off by default. A direct `--audit-file` argument can set a path or disable auditing with `off`.

## 12. macOS service and updates

The macOS controller is `scripts/service.py`. It uses Python 3, `launchctl`, and a user LaunchAgent.

Makefile targets provide the normal interface. For example, `make install`, `make update`, and `make status` call the controller.

The controller uses `make check` for checks and builds. It does not need separate shell check scripts.

### Managed update

The controller:

1. Runs `make check`.
2. Stores the candidate as an immutable release.
3. Tests the candidate on a separate local port.
4. Starts an independent launchd update job.
5. Waits for active managed processes to finish.
6. Acquires an authenticated maintenance fence.
7. Selects the candidate and restarts the LaunchAgent.
8. Checks the new server ID and tool metadata.
9. Reports success or attempts rollback.

The update can interrupt the MCP connection briefly. It does not move live PTYs or in-memory receipts to the new server.

A busy update must not stop an active job. The maintenance fence has a generation-based expiration time of 60 seconds.

If the new process fails its readiness check, the controller tries to restore the previous release.

A manual stop marker takes priority over an update or rollback. `make uninstall` removes LaunchAgent registration but keeps stored state.

The controller compares tool metadata and server instructions before and after the update. It reports `refresh_tools_required` when they change.

The one-time adoption procedure for a manually started server has separate checks. It requires a saved, known-working binary and an idle period.

## 13. Failure and recovery limits

The server does not restore sessions after a crash. The audit log and saved release files do not restore them.

A session has a local process group. A child can escape the group or outlive its leader.

After the server observes leader exit, it does not send a signal to an old numeric process group. The operating system can reuse that ID.

No native SSH process tracking, complete Unicode terminal rendering, or process-container guarantee is provided.

The response time budget of a hosted MCP client is not a child-process lifetime limit.

## 14. Checks and references

Run `make check` on a supported system. Use `make check-core` for repeated checks of selected core packages.

GitHub Actions runs `make check` on macOS and Ubuntu and also runs static analysis and `govulncheck` in an Ubuntu quality job. The lint and vulnerability tools are separate from the managed server update path. `make check` verifies that `go mod tidy` would not change module files.

The [validation report](TEST_REPORT.md) separates executed checks from untested scenarios.

References:

- [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
- [Go process API](https://pkg.go.dev/os/exec)
- [Go filesystem root](https://pkg.go.dev/os#Root)
- [PTY package](https://github.com/creack/pty)
- [Virtual terminal package](https://pkg.go.dev/github.com/hinshun/vt10x)
- [MCP specification](https://modelcontextprotocol.io/specification)
- [Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels)
