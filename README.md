# Local Terminal MCP

Local Terminal MCP is a Go service. It lets an MCP client start, read, and control local processes. It also gives clients access to local text files.

**This service is not a sandbox.** Tools run with the permissions of the service user. Give access only to trusted clients.

## Start here

- [Build and check](#build-and-check) — Build the server and run tests.
- [Windows](docs/WINDOWS.md) — Native Windows installation, ConPTY, logs and current limitations.
- [Run as an application](#run-as-an-application) — Run, start in the background, stop, and update without a service manager.
- [Migrate a legacy macOS LaunchAgent](docs/STANDALONE_MIGRATION.md) — One-time migration for older installations.
- [Connect through Secure MCP Tunnel](#connect-through-secure-mcp-tunnel) — Connect ChatGPT to the local server.
- [Use the tools](#use-the-tools) — Start a process and read its output.
- [Configuration and logs](#configuration-and-logs) — Set the file root and inspect MCP actions.
- [Specification](SPEC.md) — Read the behavior and limits of each tool.
- [Development guide](docs/DEVELOPMENT.md) — Read the complete file-tool and update procedures.
- [Validation report](TEST_REPORT.md) — See the verified checks and their limits.
- [Logging and audit redesign](docs/LOGGING_AND_AUDIT_DESIGN.md) — Accepted target contracts, staged progress, and remaining work.

The server supports macOS, Linux and Windows. It needs Go 1.25 or later.
On Windows, use the [native setup guide](docs/WINDOWS.md); the Makefile is
intended for Unix platforms.

Running the server and updater requires no Python or OS service manager. The `file_search` tool needs `rg` (ripgrep).

## What the server does

The server provides 12 MCP tools.

| Tool | Function |
| --- | --- |
| `terminal_list` | Get the current `server_id`, session counts, and unclosed sessions. |
| `terminal_start` | Start a process and get a `session_id`. |
| `terminal_read` | Read process output without consuming it. |
| `terminal_write` | Send text, bytes, or keys to a process. |
| `terminal_signal` | Send a signal to a local process or group. |
| `terminal_resize` | Change the size of a PTY. |
| `terminal_close` | Close a session. |
| `file_read` | Read UTF-8 text with line numbers and a file version. |
| `file_edit` | Replace exact text after a version check. |
| `file_write` | Create a file or replace a file with a version check. |
| `file_search` | Search file content with bounded results. |
| `file_list` | List the entries in one directory. |

The server supports two process modes:

- `pipes` keeps stdout and stderr separate. Use this mode for builds and tests.
- `pty` provides a terminal, interactive input, and a text screen (Unix PTY or Windows ConPTY).

A process can continue after the MCP client disconnects. The server requests process termination when it closes a session. A restart removes in-memory session state.

Each mutating action uses an `operation_id`. The server uses this ID to prevent duplicate execution in one server instance.

## Build and check

From the repository root, run:

```sh
make check
```

`make check` checks Go source formatting, verifies modules and `go mod tidy -diff`, runs race-enabled Go tests and `go vet`, and builds `bin/terminal-mcp`.

Use one target when you do not need a full check:

```sh
make fmt-check
make test-go
make build
make check-core
make audit-feedback
make lint
make vulncheck
```

`make check-core` repeats selected Go race tests three times. It also runs `go vet` on those packages. `make audit-feedback` checks MCP responses against a temporary JSONL audit log using a real SDK client and isolated server.

`make lint` uses the checks in `.golangci.yml` and needs `golangci-lint` v2.15.0 installed. `make vulncheck` uses a pinned `govulncheck` v1.9.0 through `go run`. They also run in GitHub Actions. Neither tool is required for `make check` or the standalone updater.

`go mod tidy -diff` reports module-file changes without editing `go.mod` or `go.sum`. Use `go mod tidy` explicitly when you intend to update them.

These targets do not stop or restart the MCP server. A native C toolchain is necessary for the Go race detector.

## Run as an application

After [building](#build-and-check), launch the server directly:

```sh
./bin/terminal-mcp serve
```

This runs in the foreground on `http://127.0.0.1:8090/mcp` until interrupted.
For a background process that survives closing the terminal, run:

```sh
./bin/terminal-mcp start
./bin/terminal-mcp status
./bin/terminal-mcp stop
```

`start` copies its executable outside the development directory into
`~/.config/terminal-mcp/standalone/`, starts it in a separate process session,
and saves its working directory, PATH, token file and file root for subsequent
starts. This means rebuilding `bin/terminal-mcp` cannot overwrite the running
binary. Background operation does not provide automatic login/reboot startup
or crash recovery.

No Python, `launchd`, or service registration is required for standalone
mode. The `serve` subcommand is optional:
`./bin/terminal-mcp` continues to work in the foreground.

Older installations that still use a macOS LaunchAgent should follow the
[legacy migration procedure](docs/STANDALONE_MIGRATION.md) before starting a
standalone server on the same port.

On the first HTTP launch without `--token-file` or `TERMINAL_MCP_TOKEN`, the
server generates a random bearer token at `~/.config/terminal-mcp/token`.
The directory and token are private (`0700` and `0600`). Subsequent starts
reuse the token, so a previously configured tunnel continues to work.
**Keep the token private:** it grants the connected client local user privileges.
To configure a tunnel, read the token locally; do not paste it into logs or chat.

The existing `--token-file` flag and `TERMINAL_MCP_TOKEN` take precedence
and do not create a default token. To avoid port conflicts with an existing
server, use a different loopback port and audit path, for example
`./bin/terminal-mcp serve --listen=127.0.0.1:18090 --audit-file=off`.
Two instances cannot write to the same audit journal.
For interactive use, Ctrl+C gracefully stops the server and its managed sessions.

### Update a running standalone server

From the repository root, use:

```sh
make update
make update-status
```

`make update` runs Go checks, builds a candidate into
`bin/terminal-mcp.next` and submits it to the running installed updater.
It does **not** replace the installed executable before the updater saves
the rollback image. The corresponding direct CLI commands are:

```sh
go build -trimpath -o bin/terminal-mcp.next ./cmd/terminal-mcp
"$HOME/.config/terminal-mcp/standalone/terminal-mcp" update --candidate=bin/terminal-mcp.next
"$HOME/.config/terminal-mcp/standalone/terminal-mcp" update-status
```

Never build directly over the installed running executable; this invalidates
the rollback image.

On macOS/Linux, the `update` command returns a queued job and status-file
path. The Windows port currently requires
[manual updates while stopped](docs/WINDOWS.md#updating-windows). It may also
be launched through the running server's `terminal_start` tool. A temporary,
independent copy of the binary performs the update after the initiating command
finishes. It checks the candidate using an authenticated MCP connection on an
ephemeral port (including a real process launch and file listing), waits for
managed sessions to finish, acquires the existing maintenance fence, and asks
the old instance to stop itself. It then atomically installs and starts the
new binary, verifies the new instance and MCP metadata, and attempts rollback
if the new version fails.

`update-status` reports `applied`, `failed`, or `rolled_back` when finished.
Logs and saved binaries remain in `~/.config/terminal-mcp/updates/<job-id>/`.
After a successful update, the new instance runs independently of the original
terminal, with diagnostics in `server.log`. The old terminal command will exit.
The connection drops briefly; obtain the new `server_id` and refresh ChatGPT's
tools if the status reports `refresh_tools_required`.

Both commands accept `--state-dir` for a private alternative update directory;
`update` also accepts `--listen` and `--token-file` for a non-default server.
On macOS/Linux, only a server explicitly started with `serve`
(including through `start`) can be updated this way. The old LaunchAgent
is not eligible.

Self-update requires an on-disk token; servers using only the
`TERMINAL_MCP_TOKEN` environment variable must first be restarted with a token
file. If the old binary was overwritten after starting, the updater refuses
the operation rather than guess at a rollback. It also refuses to interrupt
active managed processes; the wait is bounded. If the shutdown acknowledgement
is lost, the worker does not assume permission to restart a deliberately stopped
server; manual recovery may be needed. A crash of the updater can leave a pending
marker requiring manual inspection. There is no permanent supervisor,
so an unexpected crash after a successful update is not automatically restarted.

## Configuration and logs

On macOS/Linux, the background controller stores configuration in
`~/.config/terminal-mcp/standalone/config.json`. On Windows, see
[Windows configuration and logging](docs/WINDOWS.md#logs). It includes the listen
address, token file, file root, working directory, PATH and optional audit
location. Use flags on `terminal-mcp start` to override saved settings
after stopping the running server. On a fresh installation the macOS/Linux default file root is
the working directory and the Windows default is the user's home directory;
a direct `--file-root` argument overrides either. Files outside that root are not accessible to the five `file_*`
tools, but terminal commands retain the user's OS permissions.

On macOS/Linux, a new HTTP start creates a private token at
`~/.config/terminal-mcp/token` if no explicit token or environment token
was supplied. On Windows the token is stored under
`%APPDATA%\terminal-mcp\token` with a private Windows ACL. Protect this token: clients using it can run commands as
the logged-in user. No launchd, Python or privileged installation is required.

Use `make status` or `terminal-mcp status` for the running instance.
`make logs-service` follows the first standalone startup log at
`~/.config/terminal-mcp/standalone/server.log`. After an update, the new
process instead writes to its update job's `server.log` under
`~/.config/terminal-mcp/updates/<job-id>/`.

### Audit log


The default macOS audit path is:

```text
~/Library/Application Support/LocalTerminalMCP/audit.jsonl
```

The Linux default is `$XDG_STATE_HOME/terminal-mcp/audit.jsonl`
(or `~/.local/state/terminal-mcp/audit.jsonl`); Windows uses
`%APPDATA%\terminal-mcp\audit.jsonl`. Unix users can follow the
audit using `make logs`.

The audit records:

- The tool name, timestamps, result, `request_id`, and instance-specific `server_id`.
- Complete `terminal_start` arguments, including `argv`, `cwd`, `env`, and execution options.
- `terminal_write` input, including text, keys, and base64 data.
- `session_id` and options for terminal read, signal, resize, and close operations.
- The complete JSON arguments of **every decoded `tools/call`**, including `file_write.content`, all `file_edit.edits`, `argv`, supplied `env` overrides, terminal input, and values rejected by SDK schema validation (including unknown fields).
- Recognized tool-call `_meta`, `inputResponses`, and `requestState` fields, when supplied.
- Outcomes of file reads/searches, mutation receipts and SDK schema rejections.

New events have `schema_version: 2`. Each tool request has `request_start` and `request_finish`, linked by `request_id`. A newly admitted mutation additionally has `operation_start` and `operation_finish`, correlated with the originating request and `operation_id`. A canceled request can have an unknown outcome even when its mutation later finishes successfully; a replay creates a new request pair, not another operation pair. The operation receipt describes the requested action, **not the eventual exit of a child process**. Schema-invalid requests retain the exact argument values in the rejected request pair; unknown fields are not discarded. Older audit records with `start`, `finish`, or `rejected` remain readable.

Raw inputs are captured **before** schema validation or conversion into Go structs. JSON whitespace and key ordering may be normalized, but argument values are not redacted or truncated. Each audit event has a 4 MiB file-size limit. If a complete request-start record cannot fit or be serialized, the MCP tool is rejected **before execution**, with an explicit error and a small `request_rejected` event (not a misleading incomplete request pair). This differs from ordinary disk I/O failures, which still use temporary fail-open behavior until stage 4. Source file bodies returned by `file_read` and child stdout/stderr are not copied into the audit; `operation_finish` may include the usual bounded edit preview. This is a tool-action audit, not a byte-for-byte recording of malformed or unauthenticated protocol traffic.

**Commands can contain secrets.** The audit records command arguments and terminal input exactly as supplied.

The auditor timestamps each event before writing it. Each `Write` serializes a complete JSONL record synchronously under a mutex. Concurrent callers wait for the sink rather than enqueueing or dropping events. A successful `Write` means that the OS reported the whole record written, **not** that it was fsynced or would survive power loss. Write, rotation, and serialization errors are returned immediately and logged by MCP callers. On shutdown, `Close` syncs and closes the file and releases the lock, reporting errors from those steps. **For now, MCP calls still execute when auditing fails**, including if the audit cannot open on startup; fail-closed enforcement is a later stage. A slow disk can delay a tool call or shutdown.

The audit files and persistent `<audit path>.lock` file have mode `0600`; the state directory has private permissions. The server uses an exclusive advisory lock to reject a second concurrent writer of the same journal. The active file is limited to 4 MiB. The server keeps up to three rotated files. On startup, the auditor removes a partial final JSONL line left by an interrupted write.

The audit records tool callbacks. It does not identify a specific person when callers share one bearer token. It is not a tamper-proof record. If the audit cannot open on startup, the server continues without it and reports the failure in diagnostics. A normal shutdown can wait for a slow disk because accepted events must be processed.

Standalone diagnostics are written to the startup or update job logs described above.

## Run HTTP manually

Use this method when you want a foreground server without background management.

The shortest command is `./bin/terminal-mcp serve` as described in
[Run as an application](#run-as-an-application). To use an existing token
explicitly instead:

```sh
./bin/terminal-mcp serve --token-file="$HOME/.config/terminal-mcp/token"
```

The MCP endpoint is `http://127.0.0.1:8090/mcp`.

The server accepts only a loopback bind address. It checks the HTTP Host, Origin, and bearer token. Do not remove these checks to repair a connection problem.

Use this command to check local tool discovery:

```sh
curl -sS -N 'http://127.0.0.1:8090/mcp' \
  -H "Authorization: Bearer $(cat "$HOME/.config/terminal-mcp/token")" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
```

The response must contain a JSON-RPC result with a `tools` array.

For direct launches, `--file-root` sets the file-tool root. The `--audit-file` option sets the audit path. Use `--audit-file=off` to disable this audit.

## Connect through Secure MCP Tunnel

The local server does not need a public IP address or an open router port. The separate `tunnel-client` establishes an outbound connection to OpenAI.

```text
ChatGPT
   |
OpenAI Secure MCP Tunnel
   |
tunnel-client (your computer)
   |
http://127.0.0.1:8090/mcp
   |
terminal-mcp (your computer)
```

### 1. Install the tunnel client

On macOS, use Homebrew:

```sh
brew install openai/tools/tunnel-client
tunnel-client --version
```

For Linux, get the correct bundle from the [official tunnel-client releases](https://github.com/openai/tunnel-client/releases/latest). Keep the files from the bundle together.

### 2. Create the tunnel and a runtime key

Open [OpenAI Platform Tunnels](https://platform.openai.com/settings/organization/tunnels). Select the correct organization and ChatGPT workspace.

Create a tunnel. Keep the complete tunnel ID, including any namespace.

The runtime key and its owner need **Tunnels Read + Use** permissions. Create a restricted runtime key from the [Platform API keys page](https://platform.openai.com/settings/organization/api-keys).

Keep these values separate:

| Value | Use |
| --- | --- |
| Tunnel ID | Identify the tunnel for the client and ChatGPT. |
| Runtime API key | Authenticate `tunnel-client` to OpenAI. |
| Local bearer token | Authenticate `tunnel-client` to `terminal-mcp`. |

Do not put these secrets in the repository or in a model prompt.

### 3. Start the tunnel client

Open a separate local terminal. Enter the restricted runtime API key without putting it in shell history:

```sh
set +x
printf 'Runtime API key: '
read -r -s CONTROL_PLANE_API_KEY
printf '\n'
export CONTROL_PLANE_API_KEY
```

Set the remaining environment variables:

```sh
export CONTROL_PLANE_TUNNEL_ID='YOUR_COMPLETE_TUNNEL_ID'
export MCP_SERVER_URL='http://127.0.0.1:8090/mcp'
export TERMINAL_MCP_LOCAL_AUTH="Bearer $(cat "$HOME/.config/terminal-mcp/token")"
export MCP_EXTRA_HEADERS='Authorization: env:TERMINAL_MCP_LOCAL_AUTH'
export HEALTH_LISTEN_ADDR='127.0.0.1:8091'

tunnel-client doctor --explain
tunnel-client run
```

The `MCP_EXTRA_HEADERS` value is an environment-variable reference. Do not replace it with the literal token.

Port 8090 belongs to `terminal-mcp`. Port 8091 belongs to the tunnel client's local health listener.

In another terminal, run:

```sh
curl -fsS http://127.0.0.1:8091/healthz
curl -fsS http://127.0.0.1:8091/readyz
```

You can open the [local tunnel client UI](http://127.0.0.1:8091/ui) to examine connection status.

### 4. Add the connection in ChatGPT

In the applicable ChatGPT workspace, add a custom MCP server through the tunnel. Select the tunnel ID from step 2.

For this setup, choose **No authentication** for the custom MCP server. The tunnel client already supplies the local bearer token. Do not put your runtime key in the ChatGPT server-authentication field.

Review and enable the 12 MCP tools. The UI can change. Refer to the [custom MCP server guide](https://developers.openai.com/api/docs/guides/custom-mcp-server) for current steps.

**A healthy tunnel does not prove that ChatGPT can use the tools.** Make a real tool call to confirm the connection.

### 5. Test from ChatGPT

Ask ChatGPT to call `terminal_list` with `{}`. Check the returned `server_id` and session counts.

Then start a harmless process:

```json
{
  "server_id": "<server_id>",
  "operation_id": "first-command-1",
  "argv": ["/bin/pwd"],
  "mode": "pipes",
  "close_on_exit": true,
  "wait_for": "completion",
  "wait_ms": 1000
}
```

Check `receipt.status` and the process exit code. Use `terminal_read` if the process has not finished.

If the tools change, select **Refresh tools** in ChatGPT. If the local endpoint works but the plugin fails, check tunnel permissions and workspace association.

For more information, see [Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels) and the [tunnel-client configuration guide](https://github.com/openai/tunnel-client/blob/master/docs/configuration.md).

## Use the tools

### Start a process

Call `terminal_list` first. Copy its current `server_id`.

Use a new `operation_id` for each mutating action:

```json
{
  "server_id": "<server_id>",
  "operation_id": "go-tests-1",
  "argv": ["go", "test", "./..."],
  "cwd": "/absolute/project",
  "mode": "pipes",
  "close_on_exit": true,
  "wait_for": "completion",
  "wait_ms": 1000
}
```

`terminal_start` returns a receipt and, when available, an observation. A wait timeout does not stop the process.

### Read the result

Use the `session_id` from the receipt. Pass `next_cursor` to the next read:

```json
{
  "session_id": "<session_id>",
  "cursor": "<previous_next_cursor>",
  "wait_for": "completion",
  "wait_ms": 1000,
  "max_bytes": 16384
}
```

Check `exit`, `output_done`, `has_more`, and `gap`. A gap means that some output is no longer available.

If a mutation response is lost, use the **same** `operation_id` and the **same** effect arguments. Do not replay it with a new `server_id`.

### Use an interactive shell

Start a PTY with `argv: ["/bin/zsh", "-i"]`, `mode: "pty"`, `rows: 24`, and `cols: 120`.

Then send input:

```json
{
  "server_id": "<server_id>",
  "operation_id": "input-1",
  "session_id": "<session_id>",
  "input": [{"text": "pwd"}, {"key": "ENTER"}],
  "wait_ms": 500
}
```

Send `CTRL_C` as a terminal key, or use `terminal_signal` with `signal: "INT"`. These actions are not equivalent in every terminal mode.

Use `terminal_read` with `view: "screen"` to get a text screen. The screen is not an image. An interactive shell can remain active after a command finishes.

### Work with files

Use `file_read` to obtain the file version. Use that version in `file_edit` or a guarded `file_write`.

Omit `expected_version` only when you want to create a new file. `file_read` returns up to 256 KiB of file text (16 KiB by default). File tools enforce size and text-format limits. See the [development guide](docs/DEVELOPMENT.md#3-file-tools) for exact limits.

## Limits and recovery

`terminal_list` reports the current limits. Default values include:

| Resource | Default |
| --- | --- |
| Active sessions | 32 |
| Retained completed sessions | 128, for up to 1 hour |
| Output journal | 1 MiB and 2,048 events per session |
| Default and maximum terminal output read size | 16 KiB and 64 KiB |
| Maximum input size | 64 KiB |
| Maximum observation wait | 10 seconds |
| Maximum PTY size | 80 rows by 200 columns |
| Mutation receipts | 16,384 per server instance |

A terminal session is not a complete process-tree container. A child process can survive after its leader exits.

The server keeps sessions and mutation receipts in memory. A restart loses this state. The audit log remains on disk, but it cannot restore processes.

## Optional stdio mode

Use stdio when an MCP host starts and owns the server process:

```sh
./bin/terminal-mcp --transport=stdio
```

Stdio uses the same tools. It does not need an HTTP token. When the server process exits, it also closes its managed sessions.

## Source layout

```text
Makefile                 Build, tests, and standalone commands
cmd/terminal-mcp/        CLI and transport
internal/session/        Process lifecycle, output, and mutation receipts
internal/terminal/       PTY, keys, and text screen
internal/filesystem/     File operations and root restrictions
internal/mcpserver/      MCP tools and integration tests
internal/httpguard/      HTTP authentication and request checks
internal/audit/          Tool-call audit events and rotation
internal/winsecure/      Windows ACL and ownership checks
SPEC.md                  Behavior specification
docs/DEVELOPMENT.md      Detailed operational procedures
TEST_REPORT.md           Validation report
.github/workflows/       macOS and Linux CI checks
```

The project does not include a browser UI or live-session recovery after a crash.
Windows lacks the automatic standalone update and rollback workflow currently
available on macOS and Linux.
