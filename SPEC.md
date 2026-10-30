# Local Terminal MCP — Behavior Specification

**Current interface:** 14 MCP tools. The server reports MCP implementation version `0.2.0`.

**Platforms:** macOS, Linux and Windows. **Access model:** one trusted local service account.

This document describes the current implementation. Use [README.md](README.md) for setup instructions. Use [DEVELOPMENT.md](docs/DEVELOPMENT.md) for operational procedures.

The terms `server_id`, `session_id`, `operation_id`, `receipt`, and `observation` are protocol terms. Use these exact terms.

## 1. Purpose and limits

The server lets an MCP client control real local processes. The client can start a process, read output, send input, and stop it.

An active process does not depend on an individual HTTP request or MCP client connection.

The server is not a sandbox. A process has the permissions of the service user. The service does not use a command allowlist.

The server does not provide:

- Per-user authorization or an OAuth authorization server.
- A browser-based terminal.
- Native SSH or serial-device APIs.
- Restoration of live sessions after a server crash.
- Full process-tree containment.
- A terminal screen-diff protocol.
- Unsolicited MCP notifications or MCP Tasks.

You can run an SSH client as a local process. The server cannot control arbitrary remote SSH processes by local signal.

## 2. Process and service architecture

The Go executable provides HTTP or stdio transport.
On Windows, the server runs natively with ConPTY and Job Objects; the Go
self-update worker is currently available on macOS/Linux only. It contains the session manager and the MCP tools.

Startup uses a dedicated `flag.FlagSet`. The server accepts an explicit
`context.Context` for cancellation; the executable subscribes to SIGINT and
SIGTERM. For ordinary cancellation, the service first enters draining:
new work is rejected, while already accepted operations and managed
processes are allowed to finish without a fixed deadline. While draining,
the HTTP interface remains available for status and control of existing
sessions. Once idle, HTTP shutdown waits for admitted requests to finish
before the audit is closed and the service exits. Explicit `stop --force`
may abort work. A bounded HTTP-close mode exists only in isolated tests;
it is not the default production shutdown contract.

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

Standalone background control:
terminal-mcp start -> independent server
terminal-mcp update -> detached worker (macOS/Linux)
                            |
                            v
                     verify and restart
```

The session manager stores sessions and mutation receipts in memory. The audit logger stores tool-call events on disk.

The `terminal` package uses `creack/pty` on Unix, ConPTY on Windows
(via `charmbracelet/x/conpty`) and `vt10x` for both platforms. The MCP Go SDK handles message transport and tool schemas.

### HTTP transport

HTTP is the default mode. The server listens on `127.0.0.1:8090` unless an operator changes the launch arguments.

The MCP endpoint is `/mcp`. All HTTP modes expose an authenticated
`/admin/maintenance` endpoint. An explicitly launched standalone `serve`
instance additionally exposes authenticated `GET /admin/runtime` and a
fenced maintenance shutdown action to support self-update. These update
features are unavailable in other transport modes.

The HTTP transport is stateless at the protocol level. The process manager keeps sessions between requests.

A tunnel can reconnect while the Go server continues to run. A server restart removes its session state.

### Optional embedded OpenAI Secure MCP Tunnel

HTTP serve/start supports `--tunnel` (default false), `--tunnel-id` (or `CONTROL_PLANE_TUNNEL_ID`) and `--tunnel-key-file`. The official pinned Go SDK polls the OpenAI control plane in the same process; it uses the existing authenticated 127.0.0.1 HTTP `/mcp` endpoint with a loopback-only, pinned RoundTripper that replaces forwarded Authorization with the local bearer and strips Origin/forwarded routing headers. The /admin routes cannot be used through the tunnel. Stdio mode rejects `--tunnel`. One embedded client provides the main MCP channel only, with no generic routing of additional MCP servers.

Standalone `start --tunnel` requires a private pre-created runtime key file; its configuration persists only the enable flag, tunnel ID and file path. The key is never inserted into argv or saved config. Existing standalone config readers ignore the new optional fields. Direct foreground `serve --tunnel` also accepts the runtime key via `CONTROL_PLANE_API_KEY` when no file is given. The key is filtered from automatically inherited managed-process environments; explicitly supplied and fully audited tool overrides remain supported. No isolation from local commands running with the same user's privileges is claimed.

Tunnel traffic enters the same MCP server, tool middleware, session manager and mandatory pre-effect audit. Local HTTP startup is not gated on a successful external control-plane poll; temporary OpenAI failures do not stop local HTTP. The SDK `Ready` latch only indicates its first successful poll and is not a live connectivity check. An unexpected SDK shutdown request leaves local HTTP available and is surfaced as `shutdown_requested`; a failed local SDK initialization reports `start_failed`. Neither automatically restarts a second runtime, and neither is confused with a temporary control-plane polling outage.

Graceful stop/update preserves remote access to existing-session controls during manager draining, then stops tunnel polling after active MCP/HTTP requests have drained and before closing session manager and audit. SDK stop cleanup may be bounded, but already accepted terminal processes and downloads have no automatic drain timeout. Updater candidate preflight always omits tunnel flags to avoid a duplicate control-plane client. Successful activation starts the new tunnel only after the old process exits;
rollback to a binary without embedded tunnel support removes unsupported
tunnel flags, restores local HTTP/audit, and leaves the tunnel disabled.

### Stdio transport

Use `--transport=stdio` when an MCP host starts the server directly. Stdio provides the same 14 tools.

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

The seven `file_*` tools use Go `os.Root`. File paths must be absolute and
inside the configured `file_root`.

A fresh standalone installation uses the working directory of the first
`start` as its file root on macOS/Linux. On Windows, it uses the user's home
directory. Saved settings keep the selected root across restarts and updates.
A foreground server without `--file-root` uses its working directory.
An explicit `--file-root` overrides the default.

These restrictions apply only to file tools. They do not restrict arbitrary child commands.

### `file_read`

**Input:** one to eight file ranges, with optional line boundaries and `max_bytes`.

The server reads regular UTF-8 text files of at most 2 MiB. It refuses binary data and NUL bytes.

Lines have one-based numbers. A read returns a SHA-256 version for the whole file.

The default line window has 200 lines. The maximum window has 1,000 lines.

The total file-read text budget defaults to 16 KiB and cannot exceed 256 KiB. The `file_read` input JSON Schema enforces `max_bytes` from 0 to 262144; zero selects the default. It also limits the `files` array to 1–8 entries and requires nonnegative line numbers. A truncated response provides `next_line` when possible.

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

### `file_receive` (provider-neutral HTTP(S) file download)

**Required:** `server_id`, `operation_id`, `path`, `file`.

The `file` object has `download_url` and `file_id` (required), `mime_type`
and `file_name` (optional). Any MCP client can supply these fields. The optional
ChatGPT integration declares `_meta["openai/fileParams"]: ["file"]` to permit
automatic file binding; it does not limit the allowed sources.

The server downloads exact HTTP response bytes (without automatic
decompression) from **any absolute HTTP or HTTPS URL**, without restrictions
on hostname, IP address (including loopback and private networks), provider or
port. Standard Go HTTP redirects are followed, including across hosts and
between HTTP/HTTPS (up to 10 redirects). There is no URL-source allowlist:
this service already exposes arbitrary process execution and is not a sandbox.
The command accepts a transfer into the shared mutation ledger after the
pre-effect audit succeeds. Optional `wait_ms` is **0..20000 milliseconds**,
default **2000**; an explicit zero returns immediately. The call waits up
to that long for the actual transfer to complete, then returns a final receipt
if ready, or `running` with the latest progress. Observation timeout or client
disconnect never cancels an admitted transfer. The initial response has the
same result fields as `file_receive_result`, plus `replayed` (always present).
`wait_ms` is excluded from the effect fingerprint: retrying the same action
with a different observation wait never starts a second download. There is
no fixed overall transfer timeout or hardcoded maximum total size. Network,
disk and server lifetime limits still apply.

The destination must be a new regular file path inside `file_root` whose
parent already exists. On Unix, downloaded bytes stream into a private
mode-0600 temporary file. On Windows, temporary and final files inherit the
destination directory ACL; the caller must choose a suitably restricted
directory when confidentiality is required. After all bytes arrive, the file is synchronized and
atomically linked into place, never overwriting existing content. Temporary
files are removed on handled failure. A successful receipt reports byte count
and SHA-256. This command does not upload local files to an MCP client.

### `file_receive_result` (read-only wait for completion or progress)

**Required:** `server_id`, `operation_id`.

**Optional:** `wait_ms` (**0..20000 milliseconds**, default **20000**).
An explicit zero returns the current snapshot. If the operation is running
and `wait_ms` is positive, wait for **completion** up to that duration, then
return its latest progress if still running. Changes to byte progress do not
wake the waiting request; neither the observation deadline nor a disconnected
client terminates the underlying download. The HTTP write timeout is 30 seconds,
so maximum observation waits are capped at 20 seconds.

Both `file_receive` and `file_receive_result` return a common result shape
with `operation_id`, `status`, `stage`, `path`, `bytes_downloaded`,
`total_bytes`, `percent`, `started_at`, optionally `completed_at` and
`receipt`. Only `file_receive` additionally includes the `replayed` boolean.

A running operation can report `queued`, `connecting`, `downloading`, or
`finalizing`. Unknown Content-Length uses null `total_bytes` and `percent`;
no fabricated percentage is reported. Completed transfers return
`completed`, 100%, exact bytes and SHA-256 receipt, while failures report
`failed` and the error receipt. An unclassifiable outcome reports `unknown`.

Repeated result calls never trigger a new transfer. Final results remain
available during the current server instance's lifetime even if the source
URL expires or the file is later modified. No operation snapshots persist
across process restarts; an old `server_id` cannot retrieve them. Graceful
stop/update waits for accepted downloads and completion audit, while explicit
`stop --force` may cancel them.

The owner's audit preserves all submitted inputs, including the full signed
`file.download_url`, malformed and unknown fields. A schema-invalid call
is still fully audited before validation. An independent accepted transfer
adds one `operation_start` and one `operation_finish`, even if the
original `request_finish` preceded completion. Progress updates are not
separate audit events, so the audit does not balloon with every byte.

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

**Threat model (owner-first, non-negotiable):** this audit is for the local
computer's owner to examine actions requested by language models and other
MCP clients. It is **not** intended to conceal sensitive client-supplied
values from the computer owner. The more sensitive the supplied value (password,
token, signed URL, file content, environment override, terminal input), the
more important its exact presence in the audit. Never add implicit masking,
redaction or selective omissions to protect an LLM/MCP client from its owner.
Any change to this contract requires the owner's explicit approval. This
policy is also recorded in [AGENTS.md](AGENTS.md).

In HTTP mode, the server opens a JSONL audit log in a private user-state directory.

The current event schema is `schema_version: 2`. For each decoded `tools/call` reaching receiving middleware, `request_start` records the **full raw JSON arguments** before SDK schema validation or conversion into a typed Go struct. Every supplied argument field, including unknown and invalid fields, is preserved. A normal `request_finish` records the result of that **MCP request**, not necessarily the final outcome of an independently running mutation. The `request_id` links both events; every request gets its own ID, including a retry with the same `operation_id`. Read-only calls have no mutation events. A special oversized/unrepresentable input rejection is described below.

For a new mutating operation admitted by the retry ledger, `operation_start` is recorded immediately before its effect callback and `operation_finish` after the callback actually returns or a panic is recovered. They carry the originating `request_id`, the effect's `operation_id`, its status and receipt. The same operation is **not executed or recorded as a new operation** on replay; conflicting operation IDs and operations denied before admission produce no operation pair. An `operation_finish` for `terminal_start` describes process submission, **not the eventual child exit**. As with existing mutation receipts, `failed` is not a general guarantee that no external state was affected.

When a request is canceled or reaches its deadline, `request_finish` can report `canceled` or `deadline` with `outcome: unknown` for a mutation. Its `operation_finish` may arrive afterwards with the actual outcome. An unmatched start after a crash signifies uncertainty, not success or failure. A `session_id` links terminal actions on one session. Every event includes the instance-specific `server_id`. The server emits a `server_start` event on startup. No server stop event is promised after a crash. Older unversioned `start`/`finish`/`rejected` entries remain readable without rewriting them.

The audit records all supplied `terminal_start` arguments (`argv`, `cwd`, `env` overrides and execution options), exact `terminal_write` text/keys/base64 values, **complete `file_write.content` and `file_edit.edits`**, and all other tool arguments. The SDK's recognized `_meta`, `inputResponses`, and `requestState` fields are also recorded when present. Supplied `env` means the values explicitly provided in MCP arguments, not an automatic dump of every inherited environment variable. JSON whitespace, key order, and escaping may be normalized on journal serialization; original JSON values, including unknown nested fields, are preserved without masking or truncation. Unknown top-level JSON-RPC fields already discarded by the SDK and messages rejected before the tool-call middleware are not covered; this is not a wire-level traffic recorder.

For `file_read`, `request_finish` records `outcome`, `files_total`, and `files_failed`. For `file_search`, it records `search_matches`, `truncated`, and `reason`. Mutation `operation_finish` also records the normal receipt, which may contain a bounded `file_edit` preview. Only requested input, not returned source-file bodies or child stdout/stderr, is copied into the audit.

**Audit inputs intentionally include any secrets or file content supplied by the model.** No redaction or filtering of model-supplied arguments is performed. Protect the journal using the private file permissions described below.

Tool calls rejected by SDK input validation generate a `request_start` with **complete raw arguments** and a `request_finish` with `status: rejected` and `reason: schema_validation`, sharing one request ID and with no operation events. An unrepresentable or oversized request-start JSONL event instead causes an explicit MCP error **before invoking the tool**. Since a complete input record cannot be stored, the journal records one small `request_rejected` event with reason `audit_record_too_large` or `audit_input_unrepresentable`, and does **not** claim to have saved the full arguments. The per-record limit remains 4 MiB of encoded JSONL, including the newline. The audit does not claim coverage for unauthenticated or malformed HTTP/JSON rejected before the SDK makes `CallToolParamsRaw` available.

The caller timestamps each event before writing it. `Write` encodes the entire event to JSONL and serializes its disk append and any rotation under one mutex, synchronously with the call. No background worker, queue, or queue-overflow drop policy remains. Concurrent callers wait for the sink. A successful `Write` confirms a complete OS-reported append, not an `fsync` or physical durability. All failures are reported immediately; no background audit queue exists.

**Fail-closed behavior for new work:** HTTP auditing is enabled by default. An error opening the required audit file, obtaining the exclusive lock, preparing its directory, or writing the `server_start` event prevents startup. An explicitly disabled `--audit-file=off` (or stdio's default disabled audit) remains an intentional opt-out. For enabled audit, every new request requires a successful `request_start`; otherwise no tool runs. A new mutation additionally requires a successful `operation_start` at the ledger's real execution boundary. Failed pre-effect recording returns `operation_not_executed`, removes the unused retry key and does not run the effect. Failed sink writes and rotation temporarily mark health `degraded`, but the next request retries its own pre-effect append under the same lock. Only a successful complete append permits that request to run and restores `ok` automatically; the affected failed attempt never executes. A successful independent completion append can also restore health. Unrecoverable audit invariant failures, including an unrepresentable post-effect completion record, still reject new work until a restart. An oversized or unrepresentable request is refused before dispatch without marking an otherwise healthy auditor degraded, provided its small `request_rejected` event can be recorded.

**Already accepted operations:** If `operation_finish` or `request_finish` fails after an effect, the server does not alter its actual mutation receipt or falsely report that the effect never happened. The journal attempts to append completion records even after degradation. Each transition to a degraded state is reported best-effort through stderr; a successful recovery is logged separately. The nonblocking health snapshot allows authenticated administrative diagnosis while the audit file is blocked. Previously failed or unrecordable completion events are not retroactively reconstructed, even after recovery. Standalone `status` exposes `audit.state` (`ok`, `degraded`, `disabled`), `audit.path`, and `audit.last_error`; authenticated `/admin/runtime` and safe maintenance operations remain accessible. A manual stop still refuses to interrupt active sessions or effects; degraded audit status does not disable that safeguard. Every archive is retained under an increasing numeric suffix rather than evicted. Total audit history is **unlimited by default**. Operators can opt into a hard budget with `--audit-max-bytes=N` (positive N), retained across standalone starts; `--audit-max-bytes=0` clears it. Exceeding an explicit budget returns `audit_history_limit`, refuses new work without deleting history, and cannot succeed on retry until that budget is increased. `audit.bytes_used` remains queryable, and `audit.bytes_limit` is present only when a cap is configured.

During ordinary server shutdown, the mutation ledger rejects new effects and waits for already accepted independent effects **and their audit completion callbacks**, even after a client has canceled the original request. Only then is the audit file closed. This also applies to stdio shutdown. The updater's normal `Freeze` still refuses to stop a server with in-flight mutations. `Close()` rejects new writes, waits for any currently active write by acquiring the same mutex, then syncs and closes the active file and releases the exclusive lock. It returns close/sync errors (not previously returned `Write` errors), consistently on repeated calls. A stuck effect or filesystem can delay shutdown.

The audit keeps one active file and **all** numbered historical archives. Each has a 4 MiB write-size limit and private permissions. Rotation renames only the active file to its next never-before-used numeric suffix; earlier archives are neither overwritten nor removed. On startup, all existing numeric archives are inventoried and their sizes counted. When an explicit nonzero budget is configured, existing history above it refuses startup; a full disk also prevents required writes. An already admitted operation's completion append may fail if the disk or an explicit budget is exhausted, but its actual effect receipt is preserved and the error reported separately. A failed partial write is repaired before the next append where possible, including after transient disk exhaustion. Before accessing the log, the server acquires a nonblocking advisory lock on the private `<audit path>.lock` file. Concurrent audit writers using this implementation cannot open the same journal. The lock file is intentionally kept across restarts to avoid inode-replacement lock races; it is not part of rotation. A partial write is truncated to the previous complete record when possible. The next startup also removes an incomplete final JSONL line.

The audit is not tamper-proof. A crash or power loss can lose recently written but unsynced events. There is no `fsync` for each event.

In stdio mode, auditing is off by default. A direct `--audit-file` argument can set a path or disable auditing with `off`. The `--audit-max-bytes` flag accepts a non-negative integer: 0 (the default) means unlimited; a positive value configures an optional total history budget. For `start`, omission retains the saved setting and an explicit zero clears it. The selected setting persists across standalone starts and updates. Server diagnostics use standard-library `log/slog` to stderr. Runtime status includes real redirected diagnostic paths when detected; stdout remains MCP protocol in stdio mode. Detached stdout/stderr logs do not rotate automatically while their writer retains an open descriptor; rotation is a controlled offline operation only, after verified server shutdown.

### Read-only journal history (internal Go API)

`internal/audit.NewReader(writer)` reads the selected writer's JSONL and
numbered archives without acquiring the writer lock, repairing files, or
creating another authoritative store. These Go methods are **not** an
implemented dashboard, HTTP route, or authorization boundary.

- `Log.Observe()` publishes an immutable active/sealed segment snapshot,
  its current-instance acknowledged byte ranges and baselines, a `Closed`
  flag, and a coalesced `Changed` channel. A successful complete OS append
  advances/wakes; a failed append or rename alone does not. `Close` wakes
  terminal waiters. OS acknowledgement does **not** mean per-record `fsync`
  or survival of a crash or power loss.
- `Reader.FrontierCursor(ctx)` returns a **Forward** checkpoint at the
  latest complete boundary within an observed historical baseline or
  acknowledged writer range (empty for empty history). `Reader.ReadPage(ctx, cursor, Forward|Backward, budget)`
  returns bounded, oldest-first or newest-first source records and an
  opaque `Next` checkpoint. Continue only in the **same direction**.
  `HasMore` means more records remain in that page's captured history.
- `Reader.WaitNextPage(ctx, after, budget)` takes a **Forward** cursor.
  It returns existing records immediately, otherwise waits for a
  successful append, terminal close, or context cancellation. The
  frozen scan and its wake channel refer to the same observation, and
  wake-up causes replay from `after`. Only `WaitNextPage` sets
  `AtLiveEdge`, `Closed`, and `Changed`; `Closed=true` means the writer
  has closed **and** the observed backlog is exhausted, with a nil
  `Changed` channel. Check `Closed` before waiting on `Changed`; a nil
  channel never wakes. Ordinary `ReadPage` leaves these fields unset.
- For the **history-to-live handoff**, capture `FrontierCursor` **before**
  fetching backward history (`ReadPage(ctx, "", Backward, ...)`), then
  start `WaitNextPage` from the previously captured frontier. This
  permits duplicates across the handoff without silently missing
  admitted appends; loss of a referenced source still yields an error.
  A Forward cursor cannot resume Backward pages.
- `ReadBudget` defaults to 100 records, 1 MiB of inline original
  bytes, and 8 MiB counted scanned-record bytes; maxima are 1,000,
  8 MiB, and 32 MiB respectively. Nonpositive values select defaults;
  values over the caps are clamped.
  `MaxScanBytes` is **soft**: processing one complete row may exceed it
  to allow cursor progress. It does not include fingerprint, boundary
  proof, directory or candidate-open I/O; it is **not** a total I/O cap.
  A damaged row exceeding the 16 MiB corrupt-scan limit can be an
  explicit non-resumable barrier.
- Valid `HistoryRecord` entries always include a `Ref` plus exact
  newline-ended `Data`, or `Deferred=true` and `Data=nil` when their size
  exceeds the page byte budget. `Reader.ReadRecord(ctx, ref)` verifies source identity,
  acknowledged bounds, line boundaries, and raw-byte digest, and
  returns exact original JSONL bytes (up to 4 MiB) plus a `historical`
  flag. It **does not** guarantee that JSON is valid: the
  `HistoryRecord.Problem` classification exists only on the page row,
  not in the `ReadRecord` return value. Complete malformed rows up to
  4 MiB have `Problem` and a verifiable `Ref` but no inline `Data` or
  `Deferred`; oversized damaged rows lack `Ref`. An incomplete
  historical tail is a non-skippable explicit error. Any physical row
  over 4 MiB is outside the supported record-detail API.
- `Historical` means the bytes preceded the *current writer's*
  acknowledged baseline, including old archives and bytes that already
  existed when this writer opened or reopened the journal. Appends
  after the new baseline are not historical; earlier admission cannot
  be inferred.

`Cursor` and page-provided `RecordRef` can survive normal rotation and
restart while the relevant source bytes remain available and unchanged. Invalid/stale
proofs or boundaries return `ErrInvalidCursor`; a **known missing**
referenced/observed segment returns `ErrHistoryGap`. A gap in
pre-existing archive suffixes alone is **not** evidence of deletion:
removing an archive not referenced by a cursor may remain
undetectable after a fresh inventory. `ErrHistoryDisabled` means no
file-backed audit enabled for this writer; `ErrHistoryScanLimit` denotes
a bounded navigation failure. `ErrCorruptRecord`, `ErrRecordTooLarge`, and
`ErrRecordBoundary` report damaged or invalid line boundaries,
sometimes as `HistoryRecord.Problem`. Cancellation and disk I/O errors
are kept distinct. Do not silently continue after a detected gap;
start a new bounded history snapshot.

`RecordPos` is an equality-comparable byte-derived position that
survives normal rename, **not** a globally unique event ID. Distinct
archives with identical first-line bytes and identical target-record
bytes at the same offset may have equal `RecordPos`. A page-provided
`RecordRef` also carries an eventual archive suffix hint, stable on
ordinary rotation and useful for distinguishing such occurrences.
This hint is a locator, **not** tamper-proof identity. Consumers must
retain source/ordering context rather than deduplicating by `RecordPos`
alone. Cursor proofs detect supported changes but are not tamper-proof
authentication: for a complete damaged row over 4 MiB, the proof uses
the row's length and last 64 KiB rather than a full-row digest.

## 12. Standalone background operation and self-update

`terminal-mcp serve` runs a foreground HTTP server. The **first**
`terminal-mcp start` installs a copy in the stable private user path; later
`start` invokes that installed binary only and rejects a downloaded/development
binary with different bytes (never silently upgrades or downgrades).
`status` and `stop` use authenticated local HTTP and can be called through
any copy of the CLI without installing that copy. Status exposes the real
executable path, its exact SHA-256, descriptive embedded Go revision when
available, PID, listening address, audit health, drain state, active work counts
and working directory. A manual `stop` immediately blocks admission of new work
and returns `draining` if existing operations or managed processes are active;
the server shuts down itself once all such work and its audit finish.
`stop --force` explicitly aborts transfers and managed processes instead. Background mode does not automatically
start at login or restart after an unexpected crash.

Standalone settings live at
`~/.config/terminal-mcp/standalone/config.json`. The controller retains the
token file, file root and a stable, absolute working directory across starts.
The initial cwd comes from the first `start` invocation and may be changed
explicitly with `start --working-directory=...`; it is never replaced
by the updater's working directory. The server inherits the environment of
each launcher; `PATH` and other inherited variables are **not** saved
automatically. Only `start --env KEY=VALUE` values persist in
`env_overrides`, and these override the environment inherited during later
starts, updater preflight, candidate activation and rollback. The updater
pins them in its worker's environment instead of copying them to its
manifest. `start --clear-env` clears the saved overrides. The bearer-token
and tunnel runtime-key environment variables are intentionally filtered and
cannot be set as explicit overrides. The standalone CLI refuses to overwrite
a running installed binary and rejects duplicate starts. A fresh standalone
configuration uses the first `start` working directory as its file root on
macOS/Linux and the user's home directory on Windows. Only the modern saved
standalone configuration is read on later starts; old controller settings are
not imported.

### Independent self-update

`terminal-mcp update --candidate=<path>` queues a short-lived detached
worker built from a private copy of the same executable. `update-status`
reads the saved status. The worker:

1. Verifies that the running server and original executable have not changed,
   and saves a checksum-verified rollback binary.
2. Copies and preflights a separately built candidate on a temporary loopback
   port, exercising real MCP calls, a process launch, and file access.
3. Atomically establishes the authenticated maintenance drain fence **before**
   waiting for managed processes and accepted mutations to complete. Newly
   created work is refused, but existing-session controls and read-only status
   requests remain usable. Once idle, a final fence prevents more mutations.
4. Requests shutdown of the exact current server instance and waits for
   the old process to exit. It does not kill an arbitrary numeric PID.
5. Atomically installs the candidate and checks the new instance, executable
   checksum and tool metadata on the original port.
6. On a failed activation or readiness check, attempts to restore and start
   the previously saved binary.
7. Retains one verified rollback executable in a private dedicated slot
   before stopping the old server. Once activation or rollback succeeds,
   removes the update job's transient `candidate`, `previous` and `worker`
   executables, but retains status, manifest and diagnostic logs.
   Failed/ambiguous jobs retain binary recovery evidence. `cleanup --dry-run`
   lists completed historical job payloads, and explicit `cleanup --apply`
   prunes only those files under an OS lock also used by update queueing.
   No installed executable, audit, diagnostics, backups or unrecognized
   candidate file is automatically deleted.

The update briefly disconnects MCP clients and loses in-memory process
sessions and mutation receipts. There is no time limit on graceful draining;
the updater runs independently of the requesting MCP call. If it crashes while
waiting, the server remains draining until explicitly recovered rather than
silently reopening admission. The recorded outcome is `applied`, `failed` or `rolled_back`;
`queued` alone is not confirmation of success.

An interrupted shutdown acknowledgment or a crashed worker may require
manual recovery. Completed worker records, logs, and binaries remain in
the private update directory. If tool metadata changes, the job reports
`refresh_tools_required`. The update worker is not a continuously running
supervisor and does not provide automatic crash recovery.

## 13. Failure and recovery limits

The server does not restore sessions after a crash. The audit log and saved release files do not restore them.

On Unix, each session has a local process group. A child can escape the
group or outlive its leader. On Windows, each session uses a Job Object that
attempts to terminate descendants when closed, but process creation and Job
Object assignment are not atomic and some descendants can escape.

After the server observes leader exit, it does not send a signal to a
possibly reused numeric Unix process group. On Windows only `TERM` and
`KILL` are supported; both terminate the process or Job Object, not POSIX
signals. Windows ConPTY does not support the foreground target.

No native SSH process tracking, complete Unicode terminal rendering, or process-container guarantee is provided.

The response time budget of a hosted MCP client is not a child-process lifetime limit.

## 14. Checks and references

On macOS/Linux, run `make check`. On Windows, use
`go test -race ./...`, `go vet ./...`, and `go build` as described in the
[Windows guide](docs/WINDOWS.md). Use `make check-core` on Unix for repeated
checks of selected core packages.

GitHub Actions runs `make check` on macOS and Ubuntu, Go tests on
native Windows, and also runs static analysis and `govulncheck` in an
Ubuntu quality job. The lint and vulnerability tools are separate from the default `make check` target. `make check` verifies that `go mod tidy` would not change module files.

The [historical validation report (2026-10-24)](TEST_REPORT.md) records
earlier checks and their limits. Use the current CI run for current results.

References:

- [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
- [Go process API](https://pkg.go.dev/os/exec)
- [Go filesystem root](https://pkg.go.dev/os#Root)
- [Unix PTY package](https://github.com/creack/pty)
- [Windows ConPTY package](https://pkg.go.dev/github.com/charmbracelet/x/conpty)
- [Windows Job Objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects)
- [Virtual terminal package](https://pkg.go.dev/github.com/hinshun/vt10x)
- [MCP specification](https://modelcontextprotocol.io/specification)
- [Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels)
