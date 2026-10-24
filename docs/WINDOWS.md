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

This creates a private standalone installation at
`%APPDATA%\terminal-mcp\standalone\terminal-mcp.exe` and listens at
`http://127.0.0.1:8090/mcp`. No Python, launchd, systemd or Windows SCM
registration is needed.

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

Use the installed executable for subsequent control:

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
| MCP audit | `%APPDATA%\terminal-mcp\audit.jsonl` |
| Server stdout and stderr after `start` | `%APPDATA%\terminal-mcp\standalone\server.log` |
| Child command stdout/stderr | Bounded in-memory session journal; **not** persisted to disk |
| Standalone configuration | `%APPDATA%\terminal-mcp\standalone\config.json` |

The audit rotates at 4 MiB and retains three rotated files. It records tool
calls and arguments, not subprocess stdout/stderr. The diagnostic log combines
the **server's** stdout and stderr; it is not automatically rotated.

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
