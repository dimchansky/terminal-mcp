# Migrate an older macOS LaunchAgent to standalone

This is a one-time procedure for installations created with the **old**
Python/launchd controller (through commit `84fef9a`). The current project no
longer ships or requires that controller. Users already running
`terminal-mcp start` can skip this guide.

Run the cutover from an **ordinary macOS Terminal**, not from an MCP session
owned by the server being stopped. The MCP connection will be interrupted.

## 1. Test a standalone instance beside the old service

From the repository root, while the old LaunchAgent still runs:

```sh
make check
./bin/terminal-mcp start --listen=127.0.0.1:18090 --audit-file=off
./bin/terminal-mcp status
./bin/terminal-mcp stop
```

The standalone controller reads the existing macOS configuration if no
standalone configuration exists, preserving its working directory, PATH,
token file and file root. It saves an independent copy at
`~/.config/terminal-mcp/standalone/config.json`.

Use any unused loopback port for the canary. Port 8091 is reserved for the
`tunnel-client` health listener in the documented tunnel setup. The
`--audit-file=off` flag prevents the test process from competing for the
old server's audit journal.

## 2. Remove the old registration and start standalone

The last commit containing the old Python controller is `84fef9a`. If you
still have that checkout, `make uninstall` there unregisters the
LaunchAgent. Otherwise, from the current Git repository, retrieve **only
the exact historical version of the controller** for this one-time cleanup:

```sh
git show 84fef9a:scripts/service.py | python3 - uninstall
```

Confirm that the old registration is gone before starting on port 8090.
If the first uninstall reports that the launchd job is still loaded, inspect
the actual service state and retry only when it is still the expected old
registration; do not forcibly kill an unrelated listener.

```sh
./bin/terminal-mcp start --listen=127.0.0.1:8090 --audit-file=default
./bin/terminal-mcp status
```

The standalone executable is installed outside the repository at
`~/.config/terminal-mcp/standalone/terminal-mcp`. The canary's temporary
audit-disable flag is cleared, and the existing private bearer token,
working directory, PATH, and file root are retained. The tunnel continues
to use port 8090; refresh the MCP connection after the new process starts.

## 3. Recovery

If the standalone instance fails, inspect
`~/.config/terminal-mcp/logs/server.stderr.log` (and the separate
`server.stdout.log`) and confirm port 8090 is free. Legacy
`standalone/server.log` remains available as history. The old default
audit and its archives are copied into `logs/` only after the old audit
writer releases its lock. If migration refuses to proceed due to an
occupied lock or differing history, stop and inspect the precise old/new
paths; never delete the old files to force the migration through. Stop an identifiable standalone instance, then use the preserved
historical controller to restore the previous LaunchAgent from its retained
local configuration and binary releases:

```sh
./bin/terminal-mcp stop
git show 84fef9a:scripts/service.py | python3 - install
```

**Do not run the historical controller against an unknown or unrelated
listener.** An installed legacy configuration and its saved binary releases
are required for this fallback. The current Go project does not remove
those user files automatically.

## 4. Normal operation after migration

```sh
make status
make stop
make start
make update
make update-status
```

These targets now operate on standalone, not launchd. `make update`
checks the Go source, builds a separate candidate and queues a detached
updater. `make update-status` is required to establish whether the update
was applied, failed, or rolled back.

The standalone process is not an OS service: it is not guaranteed to start
after login/reboot or recover from an unexpected crash.
