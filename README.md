# Local Terminal MCP

A small Go service that gives an MCP client live local processes and terminal
sessions, rather than a single blocking `execute(command)` call.

**Version 0.2:** terminal sessions, version-guarded file tools and supervised
macOS updates. See [Development tools and service operations](docs/DEVELOPMENT.md)
for the new API and safe self-update workflow. The original delivery report remains
historical; run `make check` for the current revision.

## What is included

- Twelve tools: seven terminal tools plus file_read, file_edit, file_write, file_search, file_list.
- Auto-close after exit and bounded completion waits for one-shot commands.
- Version-checked atomic single-file edits, batch line reads, and bounded search.
- Optional macOS LaunchAgent with preflight, independent restart worker and rollback.
- Pipes with separate stdout/stderr, or a real PTY with a rendered text screen.
- Continuous bounded output capture, independent byte cursors, explicit gaps.
- Bounded observation and input waits; signals never wait behind a reading call.
- Retry keys and server-instance fencing; retries do not duplicate accepted input.
- Loopback HTTP with token/Host/Origin checks, plus optional stdio.
- An English architecture/contract document: [SPEC.md](SPEC.md).

**Platform:** macOS or Linux. **Compiler:** Go 1.25 or newer. Direct dependencies
are pinned in `go.mod`: the official MCP Go SDK, `creack/pty`, `vt10x`, and `x/sys`.

There is intentionally no command allowlist. Processes run with the service user's
permissions. This is not a sandbox. Do not give an untrusted model/client access to
an account containing secrets or systems it must not control.

## Build and verify

On a machine with Go 1.25+ and access to the Go module proxy:

```sh
go mod tidy
make check
```

`make check` checks Go formatting, verifies modules, runs Go race tests and
`go vet`, runs the Python controller tests, and builds `bin/terminal-mcp`.
Use `make fmt-check`, `make test-go`, `make test-service`, or `make check-core`
(three race-test repetitions for the original process/security packages) when
only one check is needed. These targets do not install or restart the service.
A native C toolchain is needed for `-race`; Python 3 remains a dependency of the
LaunchAgent controller and its tests. Only `file_search` requires ripgrep (`rg`).

The repository contains the verified go.sum generated during native builds.
Keep checksum verification enabled. Dependencies are pinned, not vendored.

For a plain build after dependency resolution:

```sh
go build -o bin/terminal-mcp ./cmd/terminal-mcp
```

## macOS: Makefile and LaunchAgent

For an independent background service, use these targets from the project root:

```sh
make check         # Tests, go vet and build
make install       # Register and start the user LaunchAgent; repeat is harmless
make status        # Service health and last update result
make update        # Validate and roll out changes with rollback support
make stop          # Intentionally stop, keeping registration
make start         # Restart a registered service
make uninstall     # Stop and remove LaunchAgent registration, retaining data
make logs          # Follow detailed MCP tool audit events
make logs-service  # Follow server diagnostics
```

`make build` only builds `bin/terminal-mcp`; it never replaces the live deployed
binary. `make install` is idempotent for an already loaded agent. On a fresh
machine it requires an existing token file (generated below), runs full checks,
and preflights the candidate before registration. After `make uninstall`, another
`make install` restores the saved registration and starts the service. Neither
command deletes the bearer token, stored releases, configuration or logs.

If another server is already listening on port 8090 and has not been registered,
`make install` refuses to take it over. Follow the explicit one-time adoption
procedure in [Development tools and service operations](docs/DEVELOPMENT.md).
Do not run `make uninstall` from a session hosted by the service being removed.
This LaunchAgent manages terminal-mcp only; keep `tunnel-client` running separately.

## Audit trail and file-root configuration

The service records MCP **tool invocations** (start/finish, tool name, complete
terminal command arguments, session context, status, duration and generated
request ID) to a private, size-rotated JSONL file:

`~/Library/Application Support/LocalTerminalMCP/audit.jsonl`

Use `make logs` to follow events (`make logs-service` for operational errors).
The log has 4 MiB per file plus three archives, with 0600 permissions in the
private 0700 state directory. All arguments of terminal_start, including argv,
cwd, env and execution options, are recorded. terminal_write records exactly what
was sent (text, keys or base64 data); read, signal, resize and close records include
session_id and options. File operations record path(s), search queries, line ranges,
create/replace mode, expected version and content/edit byte counts, but not file
contents or edit fragments. The audit does not duplicate stdout/stderr.

Commands or PTY input that themselves contain secrets or embedded file content
will be recorded verbatim: this is intentional for this owner-controlled server.
It records dispatched tools, not every unauthenticated or malformed HTTP request.
A shared
bearer token cannot identify which remote person called a tool. If the audit
cannot record the *start* of a tool call, the server refuses to execute it;
an end-of-call write failure is reported in diagnostics rather than turning an
already executed mutation into a misleading retryable failure. The log is neither
tamper-proof nor fsync-per-event.

**Current service configuration** is stored in:

`~/Library/Application Support/LocalTerminalMCP/config.json`

Keys: `project` (LaunchAgent working directory), `listen` (HTTP endpoint),
`token_file` (a *path*, not the token itself), and `path` (child executable PATH).
For this personal service, `file_root` defaults to `/` at installation, allowing
file tools to address any accessible absolute path. The configured value can be
changed to another **absolute** directory; the server's fallback if the key is
missing is the working directory (`project`). An explicit `--file-root` CLI
argument overrides it. To apply a new
`file_root`, edit `config.json` (preserving valid JSON and file permissions) and
run `make update` from the project. The controller preflights the new root before
switching the managed server. The tool schemas/descriptions may change when the
root changes; click Refresh tools if the update status requests it. If validation
fails, restore the previous config setting and retry. This is not a sandbox for
shell commands; the root applies only to `file_*` tools.

`--audit-file=/absolute/path` overrides the audit location for direct launches;
`--audit-file=off` disables it (stdio defaults to no audit). The supervised
LaunchAgent uses the default state directory. Do not hand-edit the generated
LaunchAgent plist: its configuration is managed by `service.py`.

## Run: HTTP manually (alternative to LaunchAgent)

Start the service independently of your MCP client/tunnel:

```sh
umask 077
mkdir -p "$HOME/.config/terminal-mcp"
# Run this generation step once; do not overwrite a token in use.
test -e "$HOME/.config/terminal-mcp/token" || \
  openssl rand -hex 32 > "$HOME/.config/terminal-mcp/token"
chmod 600 "$HOME/.config/terminal-mcp/token"

./bin/terminal-mcp \
  --listen=127.0.0.1:8090 \
  --token-file="$HOME/.config/terminal-mcp/token"
```

Endpoint: `http://127.0.0.1:8090/mcp`. Clients must send:

```text
Authorization: Bearer <contents of the token file>
```

The endpoint deliberately rejects browser Origin headers, requests with a different
Host, non-loopback binding, and requests over 1 MiB. There is no CORS or built-in
OAuth authorization server. Use the **same literal IP and port** in the client URL;
`localhost` is not interchangeable with the configured Host check.

Sessions survive MCP client disconnects and tunnel restarts while this process is
alive. Keep it running in a terminal or under your normal launchd/systemd setup.
Graceful SIGINT/SIGTERM shuts down sessions. Stopping the model's read call does
**not** stop its child process. Use `terminal_signal` or `terminal_close` instead.

### Verify the MCP endpoint with curl

You can use `curl` to verify that the HTTP server is running, authentication works, and MCP tools are available — without configuring an MCP client or tunnel.

The following request lists the tools exposed by the server:

```sh
curl -sS -N \
  'http://127.0.0.1:8090/mcp' \
  -H "Authorization: Bearer $(cat "$HOME/.config/terminal-mcp/token")" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/list",
    "params": {}
  }'
```

A successful response contains a JSON-RPC result with a `tools` array describing the available tools and their input schemas.

The request connects directly to the local HTTP endpoint; no tunnel credentials or external services are required.

### OpenAI Secure MCP Tunnel

This connects **this repository's `terminal-mcp` server** to ChatGPT Web without
publishing its HTTP endpoint on the internet. You do not need `uvx`, a Python
shell server, a public IP address, or router port forwarding.

There are two local processes; keep both running:

```text
ChatGPT Web
    |
OpenAI-hosted Secure MCP Tunnel
    |
    | tunnel-client establishes the outbound connection to OpenAI
    v
Your computer: tunnel-client                  [terminal B]
    |
    | HTTP + local Authorization header
    v
Your computer: http://127.0.0.1:8090/mcp       [terminal A]
    |
terminal-mcp -> local processes / PTY sessions
```

The steps below use environment variables rather than a YAML profile. No
`tunnel-client init` command is necessary for this setup.

#### 1. Build and start the local MCP server

Follow [Build and verify](#build-and-verify), then
[Run: HTTP is the recommended mode](#run-http-is-the-recommended-mode).
Keep `terminal-mcp` running in **terminal A**, with its token file at
`~/.config/terminal-mcp/token`.

Use the [curl check above](#verify-the-mcp-endpoint-with-curl) to confirm that the
local endpoint responds and lists the tools. Resolve local connection or token
errors before configuring the tunnel. The MCP URL for the rest of this guide is
exactly `http://127.0.0.1:8090/mcp`.

#### 2. Install the official `tunnel-client`

On **macOS with Homebrew**, use OpenAI's official tap:

```sh
brew install openai/tools/tunnel-client
```

If it is already installed, update it instead:

```sh
brew update
brew upgrade openai/tools/tunnel-client
```

OpenAI currently recommends Homebrew on macOS: manually downloaded release ZIPs
are not notarized and may be blocked by Gatekeeper. Do not disable Gatekeeper or
remove quarantine attributes to work around that; use the supported package.

For **Linux**, download the matching architecture's client bundle from the
[official releases](https://github.com/openai/tunnel-client/releases/latest), or
use the download offered on the Platform Tunnels page. Extract the bundle and
add the directory containing `tunnel-client` to your `PATH`. Keep any bundled
companion files beside it rather than copying only the executable. Select a
client binary bundle, not GitHub's source-code archive.

Verify the installation:

```sh
tunnel-client --version
tunnel-client help quickstart
```

The [upstream installation instructions](https://github.com/openai/tunnel-client#install-with-homebrew)
are the reference if packaging changes.

#### 3. Create a tunnel in OpenAI Platform

Open [Platform -> Organization -> Tunnels](https://platform.openai.com/settings/organization/tunnels).
Select the intended Platform organization. For a personal account, use that
account's personal Platform organization.

Create a tunnel with a recognizable name, such as `Local Terminal MCP`. Include
the Platform organization and associate the **ChatGPT workspace in which you
will use it**. Copy the complete returned `tunnel_id`; do not shorten it or
replace it with the tunnel's display name. Some IDs include a namespace; keep
that too.

Association with a Platform organization alone does not necessarily make the
tunnel visible in your ChatGPT workspace. Check the tunnel's associations before
continuing.

Tunnel permissions are organization-level, not just project permissions:

| Task | Required tunnel permissions |
| --- | --- |
| Create or edit a tunnel | **Read + Manage** |
| Run `tunnel-client` | **Read + Use**, for the runtime key's principal and the key itself |
| Select/use the tunnel in ChatGPT | **Read + Use**, for the operator |

If you see **Tunnels access required**, check the organization selector and ask
an owner or permissions administrator for the appropriate role. The relevant
pages are [Organization roles](https://platform.openai.com/settings/organization/people/roles)
and [Organization groups](https://platform.openai.com/settings/organization/people/groups).
Account/workspace policies still apply; installing the client does not grant
access. See the [official permissions guide](https://github.com/openai/tunnel-client/blob/master/docs/permissions.md).

#### 4. Create a restricted runtime API key

In the same Platform organization, open
[Runtime API keys](https://platform.openai.com/settings/organization/api-keys).
Create a **Restricted** key with Tunnels **Read + Use**. The user or service
account behind that key must also have those permissions for this tunnel.

Use this key for `CONTROL_PLANE_API_KEY`. Do **not** use an admin API key or a key
with unrestricted permissions for the long-running client. Creating the tunnel
through the Platform UI does not require you to create an `OPENAI_ADMIN_KEY`.

Keep these three values separate:

| Value | Where it comes from | Where it is used |
| --- | --- | --- |
| `CONTROL_PLANE_TUNNEL_ID` | Platform Tunnels page | Selects the same tunnel in the local client and ChatGPT; it is an identifier, not a password |
| `CONTROL_PLANE_API_KEY` | Platform Runtime API keys | Authenticates `tunnel-client` to OpenAI; it is a secret |
| Local token in `~/.config/terminal-mcp/token` | Generated in the HTTP startup instructions | Authenticates requests from `tunnel-client` to `terminal-mcp`; it is a different secret |

Do not paste either secret into ChatGPT, commit it, or put it in a command you
ask the model to run. Perform credential setup yourself, in a separate terminal
from the server. A full-access shell can still read files and secrets accessible
to its OS user; this credential separation does not turn the server into a sandbox.

#### 5. Configure and start the tunnel client

Open **terminal B** on the same computer, under an account that can read the
local token file. The following interactive input works in Bash or zsh. Enter
the runtime API key at the prompt, not as a literal in shell history:

```sh
# Disable shell tracing before handling credentials.
set +x
printf 'Paste the tunnel runtime API key (hidden), then press Enter: '
read -r -s CONTROL_PLANE_API_KEY
printf '\n'
export CONTROL_PLANE_API_KEY
```

Next, replace the tunnel-ID placeholder with the complete ID from step 3 and run:

```sh
export CONTROL_PLANE_TUNNEL_ID='YOUR_COMPLETE_TUNNEL_ID'
export MCP_SERVER_URL='http://127.0.0.1:8090/mcp'
export TERMINAL_MCP_LOCAL_AUTH="Bearer $(cat "$HOME/.config/terminal-mcp/token")"
export MCP_EXTRA_HEADERS='Authorization: env:TERMINAL_MCP_LOCAL_AUTH'
export HEALTH_LISTEN_ADDR='127.0.0.1:8091'

tunnel-client doctor --explain
```

If the checks succeed, start the client in that **same terminal**:

```sh
tunnel-client run
```

`MCP_SERVER_URL` selects HTTP mode and points to the server already running in
terminal A. It is equivalent to passing
`--mcp.server-url=http://127.0.0.1:8090/mcp`; do not also configure a stdio command
for the same channel.

`MCP_EXTRA_HEADERS` injects the local bearer credential on the hop to
`terminal-mcp`. The single-quoted `env:TERMINAL_MCP_LOCAL_AUTH` is intentional:
it is a secret reference that `tunnel-client` resolves, not the token itself.
This header is not the runtime API key used to authenticate to OpenAI.
Connector-forwarded headers take precedence over these local static headers;
do not configure a conflicting `Authorization` header in ChatGPT.

Port **8090** is the terminal server; port **8091** is the tunnel client's local
health/admin listener. In a **third terminal**, check the latter:

```sh
curl -fsS http://127.0.0.1:8091/healthz
curl -fsS http://127.0.0.1:8091/readyz
```

Open [the local tunnel admin UI](http://127.0.0.1:8091/ui) to inspect the connection.
Keep this listener on loopback. A healthy/ready client is a prerequisite, not
proof that ChatGPT can discover and call this server; perform the next steps.

These exports apply only to the current shell and its child processes. In a new
terminal, repeat the credential input and exports before restarting the client.
If you already use tunnel-client profiles/configuration, check for conflicting
settings; this recipe assumes no other MCP binding or header overrides.

#### 6. Add the tunnel in ChatGPT Web

Keep **both local processes running** during discovery and subsequent tool calls.
In the intended ChatGPT workspace:

1. Open **Plugins**, select **+**, then **Add custom MCP server**.
2. Name it `Local Terminal MCP`. A suitable description is
   `Manage local processes and interactive terminals on my computer.`
3. Under **Connection**, select **Tunnel**, then choose the tunnel from step 3
   or paste its complete `tunnel_id`.
4. For **Authentication**, select **No authentication** for this configuration.
   Do not choose OAuth: this server does not implement an OAuth authorization
   server. Do not enter your OpenAI runtime API key as a server credential.
5. Review the risk warning, select **I understand and want to continue**, and
   choose **Create as a plugin**.
6. Review the discovered tools, then install the resulting plugin from your
   personal plugins or workspace before using it in a conversation.

**Why "No authentication" does not disable local protection:** it means ChatGPT
will not perform a separate OAuth flow for this MCP server. OpenAI still checks
tunnel access, and the local `tunnel-client` supplies the bearer token required
by `terminal-mcp`. Leave the server's `--token-file` protection enabled. This is
one shared local service credential, not per-ChatGPT-user authorization; grant
access only to users who may control this computer.

Do not enter `http://127.0.0.1:8090/mcp` as a public Server URL in ChatGPT. That
address is used by the local tunnel client; ChatGPT identifies the connection
by its tunnel ID.

UI labels and availability can change. The current reference is
[Add custom MCP server](https://developers.openai.com/api/docs/guides/custom-mcp-server).
If the tunnel is missing, check its workspace association and your **Read + Use**
permissions before changing the local server.

#### 7. Verify a real tool call from ChatGPT

Open a new conversation, type `@`, and select the installed `Local Terminal MCP`
plugin. First test discovery and a read-only call:

```text
Use the Local Terminal MCP plugin only. Call terminal_list with {} and report
its server_id, unclosed sessions, and session counts. Do not run shell commands yet.
Use include_closed=true only when inspecting retained closed sessions.
```

Confirm that discovery exposes all twelve tools: `terminal_start`,
`terminal_read`, `terminal_write`, `terminal_signal`, `terminal_resize`,
`terminal_list`, `terminal_close`, `file_read`, `file_edit`, `file_write`,
`file_search`, and `file_list`.

Then perform a harmless process-execution check, approving the call if prompted:

```text
Using Local Terminal MCP, first call terminal_list to obtain the current
server_id. Start argv ["/bin/pwd"] in pipes mode with a fresh operation_id and
wait_ms 1000. Read that session as needed until the process has exited and
output_done is true. Report its working directory and exit status. Do not read
other files or run additional programs.
```

Only a successful tool call from ChatGPT verifies the complete path. If tools
are missing or disabled, inspect the plugin's tool settings and workspace
policy. After changing server metadata, refresh the connection and test in a
new conversation. See [Connect and test your plugin](https://developers.openai.com/plugins/deploy/connect-chatgpt).

#### Troubleshooting and daily use

| Symptom | What to check |
| --- | --- |
| Client cannot authenticate to OpenAI, or polling returns 401/403 | Runtime key, target organization/tunnel, and **Read + Use** on both the key and its principal. Do not substitute the local token or an admin key. |
| Local `/mcp` returns 401 | Match `TERMINAL_MCP_LOCAL_AUTH` to the token file used by the running server, including `Bearer `. Check `MCP_EXTRA_HEADERS` and conflicting forwarded/discovery headers. |
| Local `/mcp` rejects Host or Origin | Use exactly `127.0.0.1:8090`, not `localhost`; do not send an `Origin` header. Keep the server's checks enabled. |
| Connection refused, or `/readyz` fails | Check that both processes are still running, that the local URL/ports match, and inspect `doctor --explain`, client logs and `/ui`. |
| Tunnel exists in Platform but is absent in ChatGPT | Check the selected ChatGPT workspace, tunnel associations and operator permissions. New resources/permission changes can take time to propagate. |
| Plugin connects but only some tools can be called | Review discovered tool metadata, enablement/confirmation settings and workspace policy; refresh the connection and try a new conversation. |
| A downloaded macOS binary is blocked | Install with `brew install openai/tools/tunnel-client`; do not bypass Gatekeeper. |

For normal use, start `terminal-mcp` first, then `tunnel-client`; reuse the same
tunnel ID, runtime key and local token. Restarting only the tunnel does not stop
terminal sessions while `terminal-mcp` remains alive. Stopping `terminal-mcp`
itself shuts down its sessions. Keep the computer awake and connected while
ChatGPT is using it.

**Validation status:** this procedure is based on this project's HTTP setup and
the official documentation checked on 2026-10-23. It is **not an executed
ChatGPT/tunnel integration test**. Do not remove authentication to work around
an integration error.

Upstream references: [Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels),
[client onboarding](https://github.com/openai/tunnel-client/blob/master/docs/onboarding.md),
and [client configuration](https://github.com/openai/tunnel-client/blob/master/docs/configuration.md).

### Optional stdio

For a host that launches an MCP executable directly:

```sh
./bin/terminal-mcp --transport=stdio
```

Example local MCP host configuration (the host must support this schema):

```json
{
  "mcpServers": {
    "terminal": {
      "command": "/absolute/path/to/terminal-mcp",
      "args": ["--transport=stdio"]
    }
  }
}
```

The same twelve tools are exposed. No HTTP token is needed. Diagnostics stay on
stderr; stdout is reserved for MCP. This is simpler to launch but **not** the mode
for keeping sessions alive when the owning host/tunnel shuts down the server.

## Tools and workflow

These are MCP **tool argument objects**, not standalone HTTP requests. Let an MCP
client/the official SDK handle initialization, JSON-RPC framing, and protocol
versions. Do not turn the endpoint into ad-hoc REST routes.

1. Call `terminal_list` with `{}` and retain its `server_id`.
2. Start a process using a new operation ID.
3. Read using `next_cursor`; examine both process state and `output_done`.
4. Send exact input, explicit keys, or a signal as needed.
5. Prefer `close_on_exit: true` for one-shot commands; otherwise close your sessions explicitly. Output remains available within retention limits.

Start an ordinary build/test process:

```json
{
  "server_id": "<from terminal_list>",
  "operation_id": "tests-1",
  "argv": ["go", "test", "./...", "-v"],
  "cwd": "/absolute/path/to/project",
  "mode": "pipes",
  "wait_ms": 1000
}
```

`terminal_start` returns an action **receipt** and an optional **observation**.
Inspect `receipt.status` before interpreting the output. A response timeout is not
proof that a start/input action failed: repeat the *identical action and operation
ID*. A changed `server_id` means the previous outcome must be investigated, not
blindly resubmitted under the new ID.

Read incrementally:

```json
{
  "session_id": "<receipt.session_id>",
  "cursor": "<previous observation.next_cursor>",
  "wait_ms": 1000,
  "max_bytes": 16384
}
```

Each event has a stream name and either UTF-8 `text` or exact `bytes_base64`.
Advance the cursor even when `return_reason` is `limit`. `gap` is explicit data
loss from retention, not an empty result. `until` is an optional literal substring
searched in the bounded returned window; it is not a whole-log regular expression.

For a persistent interactive shell, start `argv: ["/bin/zsh", "-i"]`,
`mode: "pty"`, `rows: 24`, `cols: 120`. Then call `terminal_write`:

```json
{
  "server_id": "<current server_id>",
  "operation_id": "input-1",
  "session_id": "<session_id>",
  "input": [{"text": "pwd"}, {"key": "ENTER"}],
  "wait_ms": 500
}
```

Interrupt the foreground terminal job by writing `[{"key":"CTRL_C"}]`, or use
`terminal_signal` with `signal: "INT", target: "foreground"` for an explicit local
OS signal. For pipes use `target: "group"` or `"process"`. Ctrl+C and OS signals
are not identical in raw mode or when interacting with a remote SSH session.

For an interactive screen:

```json
{"session_id":"<session_id>","view":"screen","max_bytes":65536}
```

The snapshot includes text cells, dimensions, cursor, alternate-screen state and a
revision. It is not an image and does not advance the raw event cursor. Use the
observation's `session.end_cursor` when waiting for newly emitted terminal output.

`terminal_resize` assigns `rows`/`cols` and requires an operation ID.
`terminal_close` requires an operation ID and optionally `grace_ms` (0..2000).
Pipe stdin can be closed using `terminal_write` with `close_stdin: true`.

### What completion does and does not mean

A direct `go test` process has an observable exit status. An interactive shell can
stay alive after many commands; no prompt/quiet-period heuristic is claimed to be
an exact command-completion signal. `running` includes a process that is stopped
or waiting for input. `output_done` can precede exit if a program closes its output.

A session starts its own process group, not a universal process-tree sandbox.
Descendants that escape the group **or outlive an already-observed leader exit**
may survive termination. After observing leader exit, the service avoids sending
signals to a remembered numeric group that could later be reused. Use an external
OS supervisor/container when strict descendant containment is required.

## Limits and recovery

Defaults are returned by `terminal_list`: 32 active sessions, 128 retained completed
sessions (one-hour retention), 1 MiB/2048-event journals, 16 KiB default reads,
64 KiB read/input cap, 10-second observation limit, 1-second input deadline,
80x200 maximum terminal dimensions, and 16,384 mutation IDs per server lifetime.
Session lists are compact; `terminal_read` returns launch argv/cwd and detail.

At ledger capacity, new mutations are refused rather than forgetting retry keys
and risking duplicate commands. Read/list and existing-key retries continue to
work. Stop the service locally when a controlled restart is needed. The service
has no disk journal and cannot reconstruct live PTYs after a crash.

A partial write reports bytes accepted by the OS; it does not promise the target
program consumed them. Do not retry a partial write with a new key without deciding
what the target process actually needs.

## Repository layout

```text
SPEC.md                  Architecture, decisions, guarantees and limits
TEST_REPORT.md           Actual validation status of this archive
internal/session/        Process lifecycle, journal, retry ledger, tests
internal/terminal/       Real PTY, emulator, terminal keys, tests
internal/mcpserver/      Twelve typed tools and SDK integration tests
internal/httpguard/      Loopback/bearer/Host/Origin boundary and tests
internal/audit/          Bounded audit journal for MCP tool actions
cmd/terminal-mcp/        CLI and transport lifetime
Makefile                 Build, format check, test and service tasks
scripts/service.py       launchd deployment and rollback controller
scripts/test_*.py        Python controller tests
.github/workflows/       Native Linux/macOS verification when pushed to GitHub
```

No browser UI, Windows support, screen diffing, unsolicited notifications, shell
integration hooks or crash recovery are implemented. A bounded MCP audit log is
available, but active process sessions cannot be reconstructed after a crash. These are
explicit exclusions, not hidden TODOs in the core control loop.
