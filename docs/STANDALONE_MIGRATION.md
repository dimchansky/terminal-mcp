# Move from the macOS LaunchAgent to standalone

The new standalone mode does not register an OS service. It runs in the
background until stopped, the user logs out, or the OS terminates the process.
The Go updater can subsequently restart it independently. No Python or
`launchd` is required **after** migration. This procedure leaves the old
installation available for rollback.

**The cutover commands must run in an ordinary macOS Terminal, not in an MCP
terminal session served by the instance being removed.** Otherwise that session
will disappear midway through the procedure.

## 1. Prepare and test while launchd is still running

From the project checkout:

```sh
make check
./bin/terminal-mcp start --listen=127.0.0.1:8091 --audit-file=off
./bin/terminal-mcp status
```

The standalone controller creates a stable executable at
`~/.config/terminal-mcp/standalone/terminal-mcp`. It does not execute directly
from the development `bin/` directory, which `make check` overwrites.

On macOS, the first start reads the existing LaunchAgent configuration and
preserves its working directory, file root, token-file path and PATH.
It stores these settings independently in
`~/.config/terminal-mcp/standalone/config.json`. The `--listen` and
`--audit-file` flags above deliberately create a canary on port 8091 without
contending for the old server's port or audit journal.

Verify that `status` reports `running` and the expected file root. The
existing ChatGPT connection remains attached to the old service on port 8090.
Do not reconfigure your tunnel for this canary.

Stop only the canary, and verify the old service still responds:

```sh
./bin/terminal-mcp stop
make status
```

If port 8091 is occupied, choose another unused loopback port. When these
checks pass, proceed to the cutover from an **ordinary Terminal window**.

## 2. Switch on the original port

Use the same checkout and already-tested binary:

```sh
make uninstall
./bin/terminal-mcp start --listen=127.0.0.1:8090 --audit-file=default
./bin/terminal-mcp status
```

`make uninstall` unregisters the old LaunchAgent and stops its process.
The connection from ChatGPT will briefly disappear. The Python controller
preserves the legacy configuration, token file, logs and binary releases.

`--audit-file=default` clears the canary's temporary `off` setting. The
standalone server resumes the normal private audit journal. Both instances
use the same local bearer token and port, so the existing tunnel configuration
does not need to change.

Confirm that `status` reports `running`, the desired file root, and a new
`server_id`. Reconnect or refresh ChatGPT's MCP tools if necessary.

After cutover, use the **binary** to control standalone mode:

```sh
./bin/terminal-mcp status
./bin/terminal-mcp stop
./bin/terminal-mcp start
```

`make start`, `make stop`, `make update` and `make status` still refer to the
*legacy Python/launchd controller* until that code is removed in a separate
cleanup. Do not use them to manage a standalone instance.

## 3. Roll back if the new installation fails

From the same ordinary Terminal window, first ensure the standalone process
is stopped, then register the preserved LaunchAgent again:

```sh
./bin/terminal-mcp stop
make install
make status
```

`stop` is safe to repeat. If the new process is not responding, inspect
`~/.config/terminal-mcp/standalone/server.log` and ensure port 8090 is free
before `make install`. Do not try to run two servers on the same port.

Do not delete the Python controller, legacy binary releases or launchd
configuration until standalone operation has been verified.

## 4. Updating standalone after the cutover

The installed executable is separate from the build output:

```sh
go test -race ./...
go vet ./...
go build -trimpath -o bin/terminal-mcp.next ./cmd/terminal-mcp
"$HOME/.config/terminal-mcp/standalone/terminal-mcp" update --candidate=bin/terminal-mcp.next
"$HOME/.config/terminal-mcp/standalone/terminal-mcp" update-status
```

The updater runs a detached copy of itself, waits for the managed sessions to
finish, restarts the standalone instance, and attempts rollback if the new
version fails its readiness check. A queued update is not yet a successful one.

After a successful update, the new process logs to the corresponding
`~/.config/terminal-mcp/updates/<job-id>/server.log`, while the initial
background start logs to
`~/.config/terminal-mcp/standalone/server.log`.

A standalone background process is *not* a system service: it will not
automatically start after login or reboot and does not automatically recover
from an unrelated crash. This is intentional for now.
