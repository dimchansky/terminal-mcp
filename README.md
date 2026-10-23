# Local Terminal MCP

Local Terminal MCP is a Go service. It lets an MCP client start, read, and control local processes. It also gives clients access to local text files.

**This service is not a sandbox.** Tools run with the permissions of the service user. Give access only to trusted clients.

## Start here

- [Build and check](#build-and-check) — Build the server and run tests.
- [Run as a macOS service](#run-as-a-macos-service) — Use a LaunchAgent to keep the server running.
- [Connect through Secure MCP Tunnel](#connect-through-secure-mcp-tunnel) — Connect ChatGPT to the local server.
- [Use the tools](#use-the-tools) — Start a process and read its output.
- [Configuration and logs](#configuration-and-logs) — Set the file root and inspect MCP actions.
- [Specification](SPEC.md) — Read the behavior and limits of each tool.
- [Development guide](docs/DEVELOPMENT.md) — Read the complete file-tool and update procedures.
- [Validation report](TEST_REPORT.md) — See the verified checks and their limits.

The server supports macOS and Linux. It needs Go 1.25 or later.

The macOS service controller needs Python 3 and `launchd`. The core Go server does not use Python. The `file_search` tool needs `rg` (ripgrep).

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
- `pty` provides a terminal, interactive input, and a text screen.

A process can continue after the MCP client disconnects. The server requests process termination when it closes a session. A restart removes in-memory session state.

Each mutating action uses an `operation_id`. The server uses this ID to prevent duplicate execution in one server instance.

## Build and check

From the repository root, run:

```sh
make check
```

`make check` runs four targets in this order:

1. `fmt-check` checks Go source formatting.
2. `test-go` verifies modules, checks `go mod tidy -diff`, runs Go race tests, and runs `go vet`.
3. `test-service` runs Python tests for the macOS service controller.
4. `build` creates `bin/terminal-mcp`.

Use one target when you do not need a full check:

```sh
make fmt-check
make test-go
make test-service
make build
make check-core
make audit-feedback
make lint
make vulncheck
```

`make check-core` repeats selected Go race tests three times. It also runs `go vet` on those packages. `make audit-feedback` checks MCP responses against a temporary JSONL audit log using a real SDK client and isolated server.

`make lint` uses the checks in `.golangci.yml` and needs `golangci-lint` v2.15.0 installed. `make vulncheck` uses a pinned `govulncheck` v1.9.0 through `go run`. They also run in GitHub Actions. Neither tool is required for `make check` or for managed service updates.

`go mod tidy -diff` reports module-file changes without editing `go.mod` or `go.sum`. Use `go mod tidy` explicitly when you intend to update them.

These targets do not stop or restart the MCP server. A native C toolchain is necessary for the Go race detector.

## Run as a macOS service

A user LaunchAgent can keep the server running without an open Terminal window. The service controller manages the server, not `tunnel-client`.

### 1. Create the local token

Run these commands once:

```sh
umask 077
mkdir -p "$HOME/.config/terminal-mcp"
test -e "$HOME/.config/terminal-mcp/token" || \
  openssl rand -hex 32 > "$HOME/.config/terminal-mcp/token"
chmod 600 "$HOME/.config/terminal-mcp/token"
```

Do not replace a token that a running client uses.

### 2. Install and check the service

From the repository root, run:

```sh
make install
make status
```

The installer checks the code, builds a binary, and tests that binary before it registers the LaunchAgent.

If the LaunchAgent already runs, `make install` does not restart it. If the service has a manual stop marker, use `make start` to start it.

**Do not use `make install` to take control of an unrelated process on port 8090.** If a manual server already runs, use the [one-time adoption procedure](docs/DEVELOPMENT.md#one-time-adoption-of-a-manual-server).

### 3. Control the service

| Command | Result |
| --- | --- |
| `make status` | Show registration, health, and the last update result. |
| `make update` | Check, build, test, and update the running server. |
| `make stop` | Stop the server. Keep the registration and stop marker. |
| `make start` | Start a registered service. |
| `make uninstall` | Stop the server and remove its LaunchAgent registration. |
| `make install` | Register the service again from retained state. |
| `make logs` | Follow detailed MCP tool actions. |
| `make logs-service` | Follow server diagnostics. |

`make uninstall` keeps the configuration, token, logs, and stored releases. Do not run it from a terminal session hosted by this server.

The updater runs in a separate launchd job. It waits for active commands to finish. It also checks the new server after the restart.

**An update causes a short connection interruption.** It does not transfer live terminal sessions to the new server.

If the update fails after activation, the controller tries to restore the previous binary. A manual stop takes priority over automatic recovery.

## Configuration and logs

### Service configuration

The macOS controller reads this file:

```text
~/Library/Application Support/LocalTerminalMCP/config.json
```

The configuration can contain these keys:

| Key | Function |
| --- | --- |
| `project` | Set the service working directory. |
| `listen` | Set the local HTTP address. The installed default is `127.0.0.1:8090`. |
| `token_file` | Give the path to the local bearer token. |
| `path` | Set the PATH environment variable for the server and child processes. |
| `file_root` | Set the root directory for `file_*` tools. |

For a new macOS installation, the controller sets `file_root` to `/`. This permits file tools to access any path allowed by the user's OS permissions.

If `file_root` is absent, the Go server uses its working directory. A direct `--file-root` argument overrides the configured value.

To change `file_root`:

1. Open `config.json`.
2. Set `file_root` to an absolute directory path.
3. Save valid JSON.
4. Run `make update`.
5. Run `make status`.

The updater checks the new root before it restarts the service. If tool metadata changes, the update status tells you to select **Refresh tools** in ChatGPT.

**Do not edit the generated LaunchAgent plist.** The controller does not provide a general live-edit function for `listen`, `project`, or `token_file`.

File-root limits apply to the five `file_*` tools. They do not restrict shell commands.

### Audit log

The server writes MCP tool-call events to:

```text
~/Library/Application Support/LocalTerminalMCP/audit.jsonl
```

Run `make logs` to read new events.

The audit records:

- The tool name, timestamps, result, `request_id`, and instance-specific `server_id`.
- Complete `terminal_start` arguments, including `argv`, `cwd`, `env`, and execution options.
- `terminal_write` input, including text, keys, and base64 data.
- `session_id` and options for terminal read, signal, resize, and close operations.
- File paths, line ranges, search terms, action types, versions, and byte counts for file operations.
- Counts of successful/failed file reads, search result counts, truncation reasons, and SDK schema rejections.

The file tools do not add file content or edit text to the audit. The audit does not copy stdout or stderr.

**Commands can contain secrets.** The audit records command arguments and terminal input exactly as supplied.

The auditor makes each timestamp before it queues an event. One worker writes queued JSONL events in order. The queue holds at most 1,024 events and 8 MiB of encoded data. MCP tool calls do not wait for disk writes. If the queue fills, new audit events can be lost; the worker reports the total in diagnostics. On shutdown, the auditor processes all accepted events, syncs the file, and closes it. A disk error does not stop any MCP tool call.

The audit files have mode `0600`. The state directory has private permissions. The active file is limited to 4 MiB. The server keeps up to three rotated files. On startup, the auditor removes a partial final JSONL line left by an interrupted write.

The audit records tool callbacks. It does not identify a specific person when callers share one bearer token. It is not a tamper-proof record. If the audit cannot open on startup, the server continues without it and reports the failure in diagnostics. A normal shutdown can wait for a slow disk because accepted events must be processed.

The server writes diagnostic messages to `stderr.log`. Use `make logs-service` to read them.

## Run HTTP manually

Use this method when you do not want a LaunchAgent.

First, build the binary and create the local token as shown above. Then run:

```sh
./bin/terminal-mcp \
  --listen=127.0.0.1:8090 \
  --token-file="$HOME/.config/terminal-mcp/token"
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
Makefile                 Build, tests, and service commands
cmd/terminal-mcp/        CLI and transport
internal/session/        Process lifecycle, output, and mutation receipts
internal/terminal/       PTY, keys, and text screen
internal/filesystem/     File operations and root restrictions
internal/mcpserver/      MCP tools and integration tests
internal/httpguard/      HTTP authentication and request checks
internal/audit/          Tool-call audit events and rotation
scripts/service.py       macOS service and update controller
scripts/test_*.py        Controller tests
SPEC.md                  Behavior specification
docs/DEVELOPMENT.md      Detailed operational procedures
TEST_REPORT.md           Validation report
.github/workflows/       macOS and Linux CI checks
```

The project does not include a browser UI, Windows support, or live-session recovery after a crash.
