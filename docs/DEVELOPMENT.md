# Development and Standalone Operation

This guide describes file tools, terminal workflows, and standalone management
on macOS/Linux. See the [Windows guide](WINDOWS.md) for native Windows
commands, permissions and process-model differences.
Read [README.md](../README.md) for setup and tunnel connection steps, and
[SPEC.md](../SPEC.md) for behavior guarantees. Older LaunchAgent users can
consult the [legacy migration guide](STANDALONE_MIGRATION.md).

Use the names of MCP fields exactly as the tool schemas define them.

## 1. Check the source

From the repository root, run:

```sh
make check
```

The target checks formatting, verifies modules and `go mod tidy -diff`,
runs Go tests under the race detector, executes `go vet`, then builds
`bin/terminal-mcp`. It does not restart the running installed binary.

To repeat the core race tests three times, run:

```sh
make check-core
```

To check MCP responses against JSONL audit events, run `make audit-feedback`. This isolated Go integration test starts a temporary stdio server and file root. It verifies outcomes, error messages, `server_id`, schema rejections, and log records. It inspects the synchronously appended events and closes the server before its final JSONL check.

These targets do not restart the service. GitHub Actions uses `make check` on macOS and Ubuntu with Go 1.25. It also runs linting and a dependency vulnerability scan in a separate Ubuntu quality job with Go 1.27.

Run `make lint` for the focused checks in `.golangci.yml` (`errcheck`, `bodyclose`, `govet`, `ineffassign`, `staticcheck`, and `unused`). Install `golangci-lint` v2.15.0 to use this target. Run `make vulncheck` to scan reachable vulnerabilities with a pinned `govulncheck` v1.9.0 via `go run`.

Keep these optional tools separate from `make check` so normal builds
do not require a vulnerability scanner or extra tooling. No Python is needed
to build or operate the server.

The standalone updater is built into the Go executable:
`make update` checks and builds a separate candidate, then queues a detached
worker. Follow with `make update-status`; a queued update is not a successful
update. See [README.md](../README.md#update-a-running-standalone-server).
Never build directly over the installed running executable: the updater needs
its unchanged original bytes for reliable rollback.

## 2. Terminal workflow

### Start a short command

1. Call `terminal_list({})`.
2. Copy its `server_id`.
3. Call `terminal_start` with a new `operation_id`.

For example:

```json
{
  "server_id": "<current>",
  "operation_id": "test-1",
  "argv": ["go", "test", "./..."],
  "cwd": "/absolute/project",
  "mode": "pipes",
  "close_on_exit": true,
  "wait_for": "completion",
  "wait_ms": 1000
}
```

The receipt reports the submission result. The observation reports output and process state.

`wait_ms` limits observation time only. A timeout does not stop the process.

`wait_for: "completion"` waits for process exit and output drain. With `close_on_exit: true`, it also waits for session cleanup.

The observation can return early because of a deadline, journal gap, or output limit.

If the process is still active, call `terminal_read`:

```json
{
  "session_id": "<session_id>",
  "cursor": "<previous_next_cursor>",
  "wait_for": "completion",
  "wait_ms": 1000,
  "max_bytes": 16384
}
```

Inspect `exit`, `output_done`, `output_incomplete`, `has_more`, and `gap`. A completed process can have incomplete output.

### Inspect sessions

`terminal_list({})` shows unclosed sessions. It also gives counts for all retained sessions.

Use `{"include_closed": true}` to see closed sessions. This does not change retention or remove output.

A closed session remains readable by `session_id` until its retention period ends.

### Run an interactive shell

Use `mode: "pty"` and start an interactive shell such as `/bin/zsh -i`.

Set `rows` and `cols`. To send text, use `terminal_write`:

```json
{
  "server_id": "<current>",
  "operation_id": "input-1",
  "session_id": "<session_id>",
  "input": [{"text": "pwd"}, {"key": "ENTER"}],
  "wait_ms": 500
}
```

A PTY screen is a text view, not an image. Request `view: "screen"` with `terminal_read`.

The shell can stay active after a command finishes. The shell exit code does not report every command's exit code.

### Retry an action

Every mutation needs a unique `operation_id`. If a response is lost, retry with the same ID and effect arguments.

Do not replace an old `server_id` automatically. First check whether the earlier action had an effect.

A partial input write reports bytes accepted by the operating system. It does not guarantee that the child program processed them.

## 3. File tools

The five file tools use Go `os.Root`. Each file path must be absolute and inside the configured `file_root`.

On a new macOS installation, the controller sets `file_root` to `/`. The tools can access paths allowed by the service user's OS permissions.

If `file_root` is absent, the Go server uses its working directory. A direct `--file-root` argument takes priority.

**The file root is not a shell sandbox.** Commands started through `terminal_start` use the service user's OS permissions.

### Read text with `file_read`

Use one to eight `files` entries:

```json
{
  "files": [
    {"path": "/absolute/project/main.go", "start_line": 10, "end_line": 80}
  ],
  "max_bytes": 16384
}
```

Line numbers start at one. Start and end lines are inclusive.

The default line window has 200 lines. The maximum has 1,000 lines per file.

The response gives each line number, a file version, and a `next_line` value when needed.

The `version` value is a SHA-256 digest of the complete file. It is not a digest of the selected lines.

The file must contain UTF-8 text without NUL bytes. The maximum file size is 2 MiB.

The default file-read text budget is 16 KiB. The maximum is 256 KiB. Use `max_bytes: 0` to select the default. The JSON Schema publishes this maximum and the array and line-range bounds. Larger results can use more MCP client memory and response bandwidth.

The tool refuses binary, special, and oversized files. Use a terminal command when the file tools cannot handle a file.

### Edit text with `file_edit`

First, call `file_read` to get the current version.

Then submit exact replacements:

```json
{
  "server_id": "<current>",
  "operation_id": "edit-1",
  "path": "/absolute/project/main.go",
  "expected_version": "sha256:<from-file-read>",
  "edits": [
    {"old_text": "exact old text", "new_text": "new text"}
  ]
}
```

The server applies all edits to one original snapshot.

Each `old_text` value must match exactly once. Edits must not overlap. A version mismatch or ambiguous match stops the operation before a write.

The maximum is 32 edits. Combined edit text cannot exceed 64 KiB.

The receipt gives the old and new versions. It also includes a bounded preview.

### Create or replace a file with `file_write`

To create a new file, omit `expected_version`:

```json
{
  "server_id": "<current>",
  "operation_id": "create-1",
  "path": "/absolute/project/new.txt",
  "content": "example\n"
}
```

If the file exists, this request fails.

To replace an existing file, provide its current `expected_version`.

The maximum content size is 64 KiB. The parent directory must exist.

The server writes one file atomically. It preserves normal permission bits. It does not preserve ACLs or extended attributes.

The server refuses special, non-owned, read-only, symlink, and hard-linked targets for mutation.

The version check detects an out-of-date file. The server checks the original snapshot again before returning a successful no-op edit or publishing a replacement. Concurrent external changes detected at this check cause a version conflict. This is not an OS-wide transaction: a writer can still modify the target after the final check.

### List files with `file_list`

Provide the directory path:

```json
{"path": "/absolute/project", "glob": "*.go", "limit": 200}
```

The tool lists one directory. It includes hidden files unless the glob excludes them.

The default limit is 200 entries. The maximum is 1,000.

The tool reports symlinks without following them.

### Search text with `file_search`

Provide a directory and search pattern:

```json
{
  "path": "/absolute/project",
  "pattern": "CloseOnExit",
  "glob": "*.go",
  "max_results": 100
}
```

The default search is literal text. Set `regex: true` to use Go regular expressions on each line.

The tool uses `rg` to find candidate files. It opens file content through `os.Root`.

The search has a five-second time limit. It returns bounded output and reports truncated or skipped results. Empty files have no lines. A final newline does not create a phantom empty line for regular-expression matches.

The default result limit is 100. The maximum is 500. Install ripgrep before you use this tool.

### File mutation limits

File mutations use the same `server_id` and mutation ledger as terminal mutations.

Use the same `operation_id` for an identical retry. Do not ignore a version conflict or a changed server ID.

File writes are atomic for one file only. There is no multi-file transaction.

## 4. Standalone startup and update

`terminal-mcp serve` runs a foreground process. `make start` builds the CLI
and launches a detached standalone server with the current user's permissions.
`make stop` and `make status` contact the installed process over the
authenticated loopback endpoint.

A new installation stores its configuration in
`~/.config/terminal-mcp/standalone/config.json`. The settings include:
`listen` (loopback address), `token_file` (private bearer token),
`file_root` (root for file tools), `working_directory`, `path` (PATH for
child processes), and optionally `audit_file`. If no token exists, a random
private token is created at `~/.config/terminal-mcp/token`.

The background binary is installed at
`~/.config/terminal-mcp/standalone/terminal-mcp`. Builds in the repository
do not overwrite it. The controller refuses concurrent starts or taking over
another listener. The process can exit after user logout or OS termination;
this is not a system service and no automatic restart is promised.

```sh
make check
make start
make status
```

To change settings, stop the server from an independent terminal, then start
with explicit flags; this does not require editing generated OS service files.

### Update from the repository or the MCP connection

```sh
make update
make update-status
make status
```

The Go updater preserves the original binary, tests a candidate on another
loopback port, waits until managed commands are idle, acquires the existing
maintenance fence and restarts a new standalone instance. A failed readiness
check triggers a rollback attempt. The connection interruption loses live
session state but not the preserved token or on-disk audit.

The updater can also be invoked directly using the installed executable and
`update --candidate=<separately built binary>`. It is safe for an MCP tool
to queue that command because the worker runs as a detached process outside
the MCP session's process group. Never overwrite the installed executable
with `go build` or `make build` before preserving its rollback image.

If an update fails, inspect `make update-status` and the worker logs in the
private update directory. A lost shutdown acknowledgment or a crashed worker
may require manual intervention. Do not delete a pending update marker
without first checking whether its worker is still running.

## 5. Audit and diagnostics

Use:

```sh
make logs
make logs-service
```

`make logs` follows the MCP tool-call audit. `make logs-service` follows server diagnostic messages.

The audit file is:

```text
~/Library/Application Support/LocalTerminalMCP/audit.jsonl
```

The active file is at most 4 MiB. The server keeps up to three rotated files. Each `audit.Write` serializes a complete JSONL record and appends it under one mutex; there is no worker or queue. Concurrent calls wait for the same sink instead of dropping events under load. A private `<audit path>.lock` file enforces one cooperating writer process. Do not unlink the lock file: advisory locks refer to an inode, and removing it while in use could allow two processes to acquire different locks.

Event timestamps are set before the synchronous write. New events use schema version 2 and carry a `server_id`. The receiving middleware now logs **raw JSON tool arguments before SDK schema validation**; SDK argument rejections have a request pair with complete invalid inputs, including unknown fields, and no operation pair. Every normally representable MCP tool call has one `request_start`/`request_finish` pair, with a unique `request_id`. An independently accepted mutating `operation_id` has exactly one `operation_start`/`operation_finish` pair, even after the original request is canceled or retried. The SDK's supported `_meta`, `inputResponses`, and `requestState` fields are captured too. An input that cannot fit in the 4 MiB JSONL record or cannot be serialized is rejected before the tool runs: the audit contains one small `request_rejected` event with no arguments, and the client receives a clear error. Every successful dispatch now requires a successfully appended `request_start`. Newly admitted mutations also require `operation_start`; failure releases the unused retry key without executing the effect. If an operational write fails, audit health remains `degraded` until restart and all subsequent MCP requests are refused, while already accepted effects retain their actual receipts.

On service shutdown, the session manager first prevents new ledger mutations and waits for all accepted independent mutations and their `operation_finish` notifications, including those whose MCP request was canceled. Only afterwards does `audit.Close()` wait for an active synchronous `Write`, sync and close the file, then release the process lock. A slow disk or stuck effect can delay shutdown. `Close()` returns errors from sync/close; errors from earlier `Write` calls have already been returned to their callers. An incomplete JSONL tail is removed when the log opens. If audit initialization or the `server_start` write fails, an audit-enabled server refuses to start. Intentional `--audit-file=off` still works. The first error is logged best-effort to stderr without recursion. In background mode `terminal-mcp status` and authenticated `/admin/runtime` expose a nonblocking `audit` health snapshot (`state`, `path`, `last_error`) even after sink failures. Controlled maintenance/stop stays available, subject to the existing active-session protection.

The audit records exact `terminal_start` arguments and `terminal_write` input. It also records `session_id` for actions on an existing session.

All validly decoded tool-call argument values are now preserved without redaction: complete file-write contents, old/new edit text, terminal commands, supplied `env` overrides, exact text and base64 terminal inputs, and raw invalid/unknown fields rejected by schemas. Whitespace, JSON escaping, and key ordering may be normalized; the journal does not claim byte-for-byte transport framing or coverage for malformed/unauthenticated requests rejected before middleware. `file_read` reports total and failed file counts and `outcome`; `file_search` reports matches, truncation and reason. Mutation `operation_finish` retains the ordinary receipt, which can include a bounded edit preview. Source file bodies returned by reads and child stdout/stderr are not copied into the audit.

The audit does not save stdout or stderr. It can still contain secrets if a model sends them as command arguments or terminal input.

The audit is not a tamper-proof record. It does not identify individual callers that use one shared bearer token.

The updater disables auditing for temporary preflight instances so
the candidate's probe calls do not pollute the real audit journal.

## 6. Standalone files

| Path | Purpose |
| --- | --- |
| `~/.config/terminal-mcp/token` | Private default bearer token. |
| `~/.config/terminal-mcp/standalone/config.json` | Saved standalone launch configuration. |
| `~/.config/terminal-mcp/standalone/terminal-mcp` | Installed executable, independent of development builds. |
| `~/.config/terminal-mcp/standalone/server.log` | Diagnostic log from the first background start. |
| `~/.config/terminal-mcp/updates/<job-id>/` | Independent update worker, manifest, backups, status and logs. |

The macOS default audit journal remains
`~/Library/Application Support/LocalTerminalMCP/audit.jsonl` for
compatibility. On Linux, the default is
`$XDG_STATE_HOME/terminal-mcp/audit.jsonl` (or
`~/.local/state/terminal-mcp/audit.jsonl` if unset).

Completed update records and releases are retained; there is currently
no automatic pruning. The legacy macOS controller is no longer part of the
source tree and no longer required to run the server.

## 7. Validation

Run `make check` before a change, `make update` to queue a verified
candidate, and `make update-status` plus `make status` to confirm the
result. After changing tool schemas or descriptions, refresh ChatGPT's MCP
tools when `refresh_tools_required` is true.

Use [TEST_REPORT.md](../TEST_REPORT.md) for verified checks and their limits.
