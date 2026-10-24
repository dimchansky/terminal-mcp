# Development and Service Procedures

This guide describes file tools, terminal workflows, and macOS service operations. Read [README.md](../README.md) for setup and tunnel connection steps. Read [SPEC.md](../SPEC.md) for behavior guarantees.

Use the names of MCP fields exactly as the tool schemas define them.

## 1. Check the source

From the repository root, run:

```sh
make check
```

The target runs these steps in order:

1. `make fmt-check` checks Go formatting.
2. `make test-go` verifies modules and `go mod tidy -diff`, then runs race tests and `go vet`.
3. `make test-service` runs the Python service-controller tests.
4. `make build` creates `bin/terminal-mcp`.

To repeat the core race tests three times, run:

```sh
make check-core
```

To check MCP responses against JSONL audit events, run `make audit-feedback`. This isolated Go integration test starts a temporary stdio server and file root. It verifies outcomes, error messages, `server_id`, schema rejections, and log records. It waits briefly for asynchronous events and closes the server before its final JSONL check.

These targets do not restart the service. GitHub Actions uses `make check` on macOS and Ubuntu with Go 1.25. It also runs linting and a dependency vulnerability scan in a separate Ubuntu quality job with Go 1.27.

Run `make lint` for the focused checks in `.golangci.yml` (`errcheck`, `bodyclose`, `govet`, `ineffassign`, `staticcheck`, and `unused`). Install `golangci-lint` v2.15.0 to use this target. Run `make vulncheck` to scan reachable vulnerabilities with a pinned `govulncheck` v1.9.0 via `go run`.

Keep these targets separate from `make check`: the service controller must be able to verify and install its own binary without requiring optional development tools or downloading a security scanner.

The Go server does not need Python at runtime. The macOS service controller uses Python 3 and standard-library modules.

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

## 4. macOS service configuration

The user LaunchAgent runs independently of the Terminal window. It manages `terminal-mcp` only.

The controller is `scripts/service.py`. Use Makefile targets for normal operations.

### State and settings

The configuration file is:

```text
~/Library/Application Support/LocalTerminalMCP/config.json
```

| Key | Purpose |
| --- | --- |
| `project` | Working directory for the managed process. |
| `listen` | Local HTTP address. |
| `token_file` | Path to the bearer token file. |
| `path` | Executable search path for the service. |
| `file_root` | Root for the file tools. |

The new-installation value of `file_root` is `/`.

To change this value, edit `config.json` and set an absolute path. Then run `make update`.

The updater tests the candidate and its file root before it changes the active binary. A path error stops the update.

Do not edit the generated LaunchAgent plist. The controller does not provide general live reconfiguration for `project`, `listen`, or `token_file`.

The state directory is private. The controller does not include the bearer-token value in launch arguments or the plist.

### Install the service

Create the bearer token as shown in [README.md](../README.md#run-as-a-macos-service).

Then run:

```sh
make install
make status
```

A new installation runs `make check`, stores the tested binary, and registers the LaunchAgent.

If the agent already runs, `make install` makes no changes. If it has a manual stop marker, use `make start`.

### One-time adoption of a manual server

Use this procedure only when an older server already listens on port 8090.

1. Save the known-working binary before you rebuild it.
2. Find the listener PID.
3. Finish active managed jobs.
4. Run the adoption command.

```sh
test -e bin/terminal-mcp.previous || cp -p bin/terminal-mcp bin/terminal-mcp.previous
lsof -nP -iTCP:8090 -sTCP:LISTEN
python3 scripts/service.py install \
  --adopt-pid <LISTENER_PID> \
  --previous-binary bin/terminal-mcp.previous
```

Replace `<LISTENER_PID>` with the PID from the previous command.

The controller tests the saved binary on a separate port. Its tool metadata must match the running server.

The first adoption of a legacy server might not have a maintenance fence. Use an idle period for this step.

### Update the server from its own MCP connection

Run this command from the repository root:

```sh
make update
```

The command checks the code and submits an independent launchd update job. It returns a job ID and status-file path.

**A queued job is not a successful update.** Check the result:

```sh
make status
```

The update is complete when the status is `applied` and the MCP endpoint is healthy.

The update worker:

1. Waits for active managed jobs to finish.
2. Acquires a maintenance fence. This blocks new mutations.
3. Selects the tested binary and restarts the LaunchAgent.
4. Checks the new server ID and expected tool metadata.
5. Reports success or attempts rollback.

The maintenance fence expires after 60 seconds if the update process fails before restart.

If an active session prevents the update, the controller reports failure without stopping that session.

The update causes a short connection interruption. It does not move live PTYs, output journals, or mutation receipts to the new process.

Get the new `server_id` after restart. Never repeat an old mutation with the new ID without first checking its outcome.

The controller sets `refresh_tools_required` when tool schemas, descriptions, annotations, or server instructions change.

If that value is true, select **Refresh tools** in ChatGPT after the successful update.

### Stop or remove registration

Use:

```sh
make stop
make start
make uninstall
make install
```

`make stop` records a manual stop and unloads the service. Launchd does not override this stop.

`make start` resumes a registered service.

`make uninstall` stops the server and removes its LaunchAgent registration. It keeps the configuration, bearer token, releases, logs, and update records.

Do not run `make uninstall` from a terminal session on the server you want to remove.

`make uninstall` refuses a pending update. If launchd cannot unload the job, the controller keeps its plist for recovery.

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

The active file is at most 4 MiB. The server keeps up to three rotated files. A bounded queue holds up to 1,024 waiting events and 8 MiB, counting its in-flight record. One worker owns a private file sink (append, repair, rotation, sync) while concurrent producers only enqueue encoded events. A private `<audit path>.lock` file enforces one cooperating writer process. Do not unlink the lock file: advisory locks refer to an inode, and removing it while in use could allow two processes to acquire different locks.

Event timestamps are set before enqueue. Every event has a `server_id`. A `server_start` event identifies a newly started instance. Accepted tool calls have a `request_id` and `start`/`finish` phases. Input rejected by the SDK has one `rejected` event without input contents. Submission does not wait for disk I/O. If the queue is full, the new event is lost; the worker logs a drop count. The MCP call continues even if the auditor fails.

When the service closes, `audit.Close()` waits for all accepted events to be processed. It syncs and closes the file and releases the process lock. A slow disk can delay shutdown. The returned error includes the first write failure plus a count of failed records and a separate count of queue rejections; a successful retry does not erase evidence of earlier loss. An incomplete JSONL tail is removed when the log opens. If audit initialization fails, including because another writer holds the lock, the server continues without auditing and reports the failure. Restart the service after fixing the cause.

The audit records exact `terminal_start` arguments and `terminal_write` input. It also records `session_id` for actions on an existing session.

For file tools, it records paths, ranges, search values, action types, and byte counts. `file_read` also reports total and failed file counts and `outcome`. `file_search` reports matches, truncation, and reason. It does not record file content or edit strings.

The audit does not save stdout or stderr. It can still contain secrets if a model sends them as command arguments or terminal input.

The audit is not a tamper-proof record. It does not identify individual callers that use one shared bearer token.

The controller disables auditing for temporary preflight instances. This keeps test calls out of the service audit.

## 6. Files used by launchd

The main paths are:

| Path | Purpose |
| --- | --- |
| `~/Library/LaunchAgents/com.dimchansky.terminal-mcp.plist` | LaunchAgent registration. |
| `~/Library/Application Support/LocalTerminalMCP/config.json` | Saved service configuration. |
| `~/Library/Application Support/LocalTerminalMCP/current` | Link to the selected binary release. |
| `~/Library/Application Support/LocalTerminalMCP/releases/` | Immutable release directories. |
| `~/Library/Application Support/LocalTerminalMCP/audit.jsonl` | MCP tool audit. |
| `~/Library/Application Support/LocalTerminalMCP/stderr.log` | Server diagnostics. |
| `~/Library/Application Support/LocalTerminalMCP/jobs/<id>/status.json` | Update result. |
| `~/Library/Application Support/LocalTerminalMCP/jobs/<id>/worker.log` | Update-worker diagnostics. |

The controller retains completed job records and releases. It does not automatically remove old records.

If an update worker stops unexpectedly, inspect the status and pending marker. Do not delete `pending` while a worker can still run.

A user LaunchAgent operates in the logged-in GUI domain. It is not a system boot daemon.

## 7. Validation

Run `make check` before a change. Run `make update` to test and install that change.

Check `make status` after an update. Inspect `make logs-service` if the server does not become healthy.

Use [TEST_REPORT.md](../TEST_REPORT.md) for the verification evidence and remaining limits.
