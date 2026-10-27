# Running Local Terminal MCP on Windows

Windows support is tested on the native `windows-latest` GitHub Actions runner
with Go 1.25. The application is **not** a Windows Service: run it as the
ordinary account that should own the terminal commands and files. Do not run
it as `LocalSystem` or elevate it unless you intentionally want the MCP
client to receive those privileges. Local Terminal MCP executes arbitrary
commands from authenticated clients and is not a sandbox.

## Build and start

Open PowerShell in the repository:

```powershell
go test -race -count=1 ./...
go vet ./...
go build -trimpath -o terminal-mcp.exe ./cmd/terminal-mcp

.\terminal-mcp.exe start
.\terminal-mcp.exe status
```

On the **first** start, this creates a private standalone installation at
`%APPDATA%\terminal-mcp\standalone\terminal-mcp.exe` and listens at
`http://127.0.0.1:8090/mcp`. Later starts use the existing installed
version and refuse to overwrite a different executable implicitly.
The downloaded CLI can always run `status` or `stop`, regardless of its
own binary version. Status shows the running executable, SHA-256 and
embedded Go revision when available. Offline Windows updates still require
the documented stop/backup/replace/restart sequence. No Python, launchd,
systemd or Windows SCM registration is needed.

The first background start creates a random private bearer token and its
directory at `%APPDATA%\terminal-mcp\token`. Files are protected with
Windows access-control lists (ACLs), not POSIX `chmod 0600`. The program
checks token, audit file and state directory ownership and ACLs and refuses
unsafe paths. Preserve the token to keep the existing client configuration.

The `start` command defaults the `file_root` to the user's home directory.
To select another existing root:

```powershell
.\terminal-mcp.exe start --file-root="$HOME"
```

Do not point the root at a publicly writable directory or grant the tunnel
to untrusted users. The file root limits the `file_*` tools, **not** the
commands launched through terminal tools.

The detached server retains the absolute working directory selected on
its first start; use `start --working-directory="C:\path\to\project"`
to change it intentionally (only while stopped). A missing directory is an
error. It inherits the current start command's environment; old `path`
fields are ignored instead of restoring a stale `PATH`. Save only
intentional overrides with repeatable `start --env KEY=VALUE` and remove
them with `start --clear-env`. The Windows standalone config stores
`env_overrides`, not an automatic copy of `PATH`. Values passed on the
command line may appear in process listings and shell history; avoid
sensitive values in CLI flags.

Any copy of the CLI can be used for `status` or `stop`; the following
uses the installed path as a stable example:

```powershell
& "$env:APPDATA\terminal-mcp\standalone\terminal-mcp.exe" status
& "$env:APPDATA\terminal-mcp\standalone\terminal-mcp.exe" stop
& "$env:APPDATA\terminal-mcp\standalone\terminal-mcp.exe" start
```

For foreground operation, use `.\terminal-mcp.exe serve` and stop with
Ctrl+C. The HTTP port can be changed with `--listen=127.0.0.1:8092`.
The optional `file_search` tool requires `rg.exe` (ripgrep) on `PATH`;
the other tools do not depend on ripgrep.

## PTY and processes

`pipes` mode keeps stdout/stderr separate. `pty` mode uses Windows
ConPTY (Windows 10 version 1809 or later) and the same screen emulator as
macOS/Linux. Managed child processes are attached to per-session Windows
Job Objects so closing a session attempts to terminate its child tree.

Windows has no direct equivalent to Unix `HUP`, `CONT`, `STOP` or
foreground-process-group signals. The `terminal_signal` tool supports
`TERM` and `KILL` on Windows; both terminate the requested process or
managed Job Object without Unix grace-period semantics. The `foreground`
target is not available. Because attaching an already-started process to a
Job Object is not completely atomic, a very short-lived process may create
descendants before assignment. Do not interpret Job Objects as a security
sandbox or as a perfect process-tree containment guarantee.

## Logs

The default Windows log paths are:

| Data | Location |
| --- | --- |
| MCP audit | `%APPDATA%\terminal-mcp\logs\audit.jsonl` |
| Server stdout after `start` | `%APPDATA%\terminal-mcp\logs\server.stdout.log` |
| Server stderr after `start` | `%APPDATA%\terminal-mcp\logs\server.stderr.log` |
| Child command stdout/stderr | Bounded in-memory session journal; **not** persisted to disk |
| Standalone configuration | `%APPDATA%\terminal-mcp\standalone\config.json` |

The audit rotates its active journal at 4 MiB and retains **all** numbered archives. The total audit history is unlimited by default; an optional positive `--audit-max-bytes=N` cap can be configured on `start` or `serve`, and `start --audit-max-bytes=0` clears it. New actions fail closed on any audit append failure, but transient disk errors automatically recover on the next successful pre-effect append without restart. It records tool calls and arguments, not subprocess stdout/stderr. Server stderr diagnostics use standard-library `log/slog`, with separate private stdout and stderr files. Those diagnostics are not automatically rotated: stop the server and confirm all writer handles have closed before archiving/renaming the files and restarting. Old `standalone/server.log` remains as
historical evidence. On the first new default start, the old
`%APPDATA%\terminal-mcp\audit.jsonl` and all numbered archives are
copied cold after the former writer releases its lock; originals remain.
The legacy writer lock is retained until the new server shuts down.

## Updating Windows

The independent Go self-update worker currently supports macOS/Linux only.
`terminal-mcp.exe update` returns an explicit unsupported-operation error on
Windows; it will not attempt to overwrite a running `.exe` or claim that an
unverified rollback has succeeded.

For the initial Windows port, replace the installed binary only **after
stopping** the standalone process. In an ordinary PowerShell window:

```powershell
$installed = Join-Path $env:APPDATA 'terminal-mcp\standalone\terminal-mcp.exe'
& $installed stop
Copy-Item $installed "$installed.previous" -Force
go build -trimpath -o terminal-mcp.next.exe ./cmd/terminal-mcp
Copy-Item .\terminal-mcp.next.exe $installed -Force
& $installed start
& $installed status
```

Use `"$installed.previous"` as your known-working backup if a new version
fails. Do not replace the running executable from an MCP terminal hosted by
that same process. Native Windows self-update with preflight and rollback
remains a separate task.

The standalone process does not automatically start after reboot or recover
from unrelated crashes; no Windows Service is installed.
