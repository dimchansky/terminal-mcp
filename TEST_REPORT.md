# Validation Report

> **Historical report (2026-10-24).** The results below describe the earlier implementation, not the completed October 26 audit/logging redesign. In particular, references to an audit queue/worker, bounded three-archive retention, omitted file content in the audit, and earlier CI runs are not claims about the current source. See the [stage 7 release-readiness and cross-platform validation report](docs/RELEASE_READINESS.md) and [current specification](SPEC.md). A local source test is not a deployed-server test.

| Item | Value |
| --- | --- |
| Date | 2026-10-24 |
| Project | Local Terminal MCP |
| Platforms | macOS and Linux |
| Status | Historical checks passed on their stated dates; consult the stage 7 report for current results. |

This report describes checks that were executed. It separates verified behavior from behavior that has not received a full test.

## 1. Test commands

Run all source checks with:

```sh
make check
```

This command runs the following checks in order:

| Target | Check |
| --- | --- |
| `fmt-check` | Check Go formatting. |
| `test-go` | Verify modules and `go mod tidy -diff`. Run all Go tests with `-race`. Run `go vet`. |
| `build` | Build `bin/terminal-mcp`. |

For repeated core checks, run:

```sh
make check-core
```

This target runs selected Go race tests three times. It also runs `go vet` on the selected packages.

`make lint` runs the configured Go linters. `make vulncheck` runs pinned
`govulncheck v1.9.0`. These are separate from `make check` so the standalone
updater does not require extra tools.

The Makefile is the standard entry point for builds and tests. The previous `scripts/check.sh` and `scripts/check-core.sh` files were removed.

The macOS Python/launchd controller was retired after successful migration
to standalone mode. `make check` now has no Python dependency. The old
controller and its tests remain available in Git history for reference.

## 2. Verified source and CI checks

On 2026-10-23, the full `make check` command passed on a local macOS machine.

At that time the local check included Go module verification, race-enabled
Go tests, `go vet`, 22 historical Python controller tests, and a Go build.
The Python tests were removed after the transition to standalone.

The same machine also passed `make check-core`. It ran the selected race tests three times.

The retry ledger now has direct tests in `internal/mutation/ledger_test.go`. On
2026-10-23, its race-enabled suite passed 100 repetitions with test-order
randomization (`go test -race -shuffle=on -count=100 ./internal/mutation`).
Its measured statement coverage was 98.4% (`go test -cover ./internal/mutation`).
The tests cover concurrent duplicate calls, effect and method conflicts,
context cancellation, panic recovery, capacity limits, and `Freeze`/`Resume`.
They found no behavior defect, so production ledger code was not changed.

The CLI and server-lifecycle tests now exercise independent flag parsing,
configuration precedence, startup failures, HTTP bearer authentication, real
HTTP and stdio MCP calls, managed-process shutdown, and SIGTERM in a separate
OS process. These tests use loopback ephemeral ports and isolated temporary
files. Twelve repeated race-enabled CLI test runs with random test order passed
on macOS. The measured statement coverage of `cmd/terminal-mcp` increased from
13.3% to 71.1% (`go test -cover ./cmd/terminal-mcp`).

On 2026-10-24, tests first reproduced a cancelled in-flight MCP request on HTTP
shutdown. The server now drains admitted HTTP requests before closing managed
processes. A real-listener test covers successful drain; another covers forced
close when the drain deadline expires. HTTP handler tests use
`httptest.NewUnstartedServer`. A regression test confirms the authorization
`RoundTripper` does not modify the caller's request.

The SIGTERM E2E test now builds and runs the actual `terminal-mcp` executable,
not a copied signal handler inside a Go test helper. It uses a loopback port
assigned by the OS, discovers the address from the service log, starts a managed
child, sends SIGTERM, verifies the child exits, and checks complete JSONL audit
records after process shutdown. Its cleanup also handles premature test failure.
Ten repeated race-enabled runs passed on local macOS.

The file-tool regression tests now cover external changes before a no-op replacement, external inode replacement, concurrent file creation, cleanup of temporary files after a conflict, symlinks outside the configured root, and operations after manager shutdown. They also check that `file_search` does not report nonexistent empty lines in empty files or after a trailing newline. Both defects were reproduced before the fixes.

The MCP audit feedback test starts a temporary server and issues a real MCP command, a missing-session read, partial file reads, an ambiguous edit, limited search, a 115000-byte read, and an oversized schema-rejected read. After each call, it waits for the corresponding JSONL event. On shutdown, it checks event pairs by request ID and verifies that file bodies were not written to the audit. Run it with `make audit-feedback`; it is also part of `make check`.

The audit package now owns an exclusive advisory lock for the journal and has a separate worker-owned file sink. Package tests verify same-process and cross-process lock exclusion, aliased paths, lock retention during rotation, byte-budget backpressure including the in-flight record, concurrent Write/Close operations, and retry of failures in partial writes, truncation, Close, Rename, Remove, and Sync. Tests inject I/O errors into the sink before launching the queue worker, without mutating a running worker. In the local macOS run, audit package statement coverage increased from 75.2% to 85.3%, with appendLine coverage from 42.9% to 81.0% and rotate coverage to 92.9%.

On 2026-10-24, `golangci-lint v2.15.0` and `staticcheck` reported no issues on the local macOS checkout. The lint configuration limits checks to relevant correctness, resource handling, and error-handling rules. The local `govulncheck v1.9.0` scan reported no vulnerabilities after upgrading `golang.org/x/sys` to v0.44.0. This upgrade also removes a previously reported, unreachable Windows-only advisory (GO-2026-5024) without raising the Go 1.25 minimum version.

A separate `gosec v2.30.0` review reported 11 potential issues. These concern
variable paths and commands that the personal MCP intentionally accepts, and
integer conversions that use constrained sizes or nonnegative counts. They are
not confirmed vulnerabilities. `gosec` is not a blocking CI check because these
intended operations produce warnings; dependency scanning and the focused
linters are blocking checks.

GitHub Actions runs `make check` on macOS and Ubuntu. A separate Ubuntu quality job checks linting and reachable vulnerabilities.

The last verified CI run before retiring the legacy Python controller was:

| Item | Result |
| --- | --- |
| Commit | `84fef9a` |
| Workflow run | [40088262291](https://github.com/dimchansky/terminal-mcp/actions/runs/40088262291) |
| macOS job | PASS |
| Ubuntu job | PASS |

This table is a dated result. Check the [GitHub Actions page](https://github.com/dimchansky/terminal-mcp/actions) for the status of later commits.

## 3. Verified local MCP behavior

Real tool calls on the local macOS server confirmed these behaviors:

- `terminal_list` returned the server ID and session counts.
- The default session list excluded closed sessions.
- `include_closed: true` returned retained closed sessions.
- `file_list` listed the root directory `/`.
- `file_read` read `/etc/hosts` with `file_root: "/"`.
- A short terminal process completed and closed its session.
- The audit recorded terminal command arguments and session input.
- The audit recorded file paths without saving file content.

Historically, a supervised `make update` succeeded from an MCP terminal
session through the Python controller. That result applies to the retired
implementation, not the current Go updater.

The unit tests cover additional results, including mutation retries, cancellation, terminal I/O, version checks, audit rotation, and update rollback.

A unit test result is not proof of every real-world failure condition.

### Standalone background operation and migration

On 2026-10-24, a real CLI test verified background start, authentication,
runtime status, safe rejection of a duplicate start on another port, and
graceful stop. Another test confirmed that active managed processes block a
manual stop. On macOS, tests also verified that existing launchd settings
(`file_root`, working directory, token and PATH) survive into an independent
standalone configuration, including after the legacy config file is deleted.

The standalone controller was also checked against an occupied simulated
legacy listener. The canary started on a separate port, exposed the configured
file root and PATH, then stopped without disturbing the simulated legacy
listener. Resetting `--audit-file=default` cleared the temporary canary
setting. These tests ran with the Go race detector and passed both native CI
jobs at the run listed above.

A non-disruptive canary test was subsequently run on the installed macOS
machine using port 18090. It verified all 12 MCP tools, successfully called
`file_list` at root `/`, and stopped cleanly. The pre-existing server on
8090 remained healthy, and `tunnel-client` on 8091 continued responding
to its health endpoint. Subsequently, on 2026-10-24 the operator migrated
the live server to standalone on port 8090. The new standalone process was
verified healthy with `file_root: "/"`, while the LaunchAgent registration
and plist were absent.

## 4. Limits of this evidence

The tests do not prove these behaviors:

- Restoration of live processes after a server crash. The server does not provide this feature.
- Complete termination of all descendant processes.
- Complete support for modern terminal graphics and all Unicode display cases.
- Durability of every audit event after power loss.
- A complete end-to-end test of every ChatGPT, tunnel, and authorization failure case.
- Long-duration operation under all workloads.
- Windows support.

A successful local MCP call does not prove that every remote client can use the service.

A passing CI run does not prove that standalone background execution will
survive every logout or machine configuration.

## 5. Historical report

The initial source delivery had different test results. Its environment used Go 1.23.2 and could not fetch a required newer toolchain.

The initial report is available in the repository's Git history. It is historical evidence, not the current project status.

The source, tests, and CI setup changed after that delivery. Use the current Makefile and CI runs to check the present implementation.

## 6. Before a new release

1. Run `make check`.
2. Confirm that the Git worktree contains only the intended changes.
3. Check the macOS and Ubuntu CI results.
4. Use `make update` to check and queue a separate candidate for standalone.
5. Check `make update-status` and `make status` for the final result and running instance.
6. Check `refresh_tools_required`. Refresh the ChatGPT tools when it is true.
7. Inspect the startup and update-job logs if an update fails.

For design limits, read [SPEC.md](SPEC.md). For operating procedures, read [DEVELOPMENT.md](docs/DEVELOPMENT.md).
