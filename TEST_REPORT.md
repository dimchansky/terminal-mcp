# Validation Report

| Item | Value |
| --- | --- |
| Date | 2026-10-24 |
| Project | Local Terminal MCP |
| Platforms | macOS and Linux |
| Status | Local checks and the last verified CI run passed. |

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
| `test-service` | Run Python tests for the macOS service controller. |
| `build` | Build `bin/terminal-mcp`. |

For repeated core checks, run:

```sh
make check-core
```

This target runs selected Go race tests three times. It also runs `go vet` on the selected packages.

`make lint` runs the configured Go linters. `make vulncheck` runs pinned `govulncheck v1.9.0`. These are separate from `make check` so the managed updater does not need extra tools.

The Makefile is the standard entry point for builds and tests. The previous `scripts/check.sh` and `scripts/check-core.sh` files were removed.

Python is necessary for the current macOS service controller and its tests. The Go server has no Python runtime dependency.

## 2. Verified source and CI checks

On 2026-10-23, the full `make check` command passed on a local macOS machine.

The local check included:

- Go module verification.
- All Go tests with the race detector.
- `go vet`.
- 22 Python controller tests.
- A successful Go build.

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

The file-tool regression tests now cover external changes before a no-op replacement, external inode replacement, concurrent file creation, cleanup of temporary files after a conflict, symlinks outside the configured root, and operations after manager shutdown. They also check that `file_search` does not report nonexistent empty lines in empty files or after a trailing newline. Both defects were reproduced before the fixes.

On 2026-10-24, `golangci-lint v2.15.0` and `staticcheck` reported no issues on the local macOS checkout. The lint configuration limits checks to relevant correctness, resource handling, and error-handling rules. The local `govulncheck v1.9.0` scan reported no vulnerabilities after upgrading `golang.org/x/sys` to v0.44.0. This upgrade also removes a previously reported, unreachable Windows-only advisory (GO-2026-5024) without raising the Go 1.25 minimum version.

A separate `gosec v2.30.0` review reported 11 potential issues. These concern
variable paths and commands that the personal MCP intentionally accepts, and
integer conversions that use constrained sizes or nonnegative counts. They are
not confirmed vulnerabilities. `gosec` is not a blocking CI check because these
intended operations produce warnings; dependency scanning and the focused
linters are blocking checks.

GitHub Actions runs `make check` on macOS and Ubuntu. A separate Ubuntu quality job checks linting and reachable vulnerabilities.

The last CI run verified before this documentation update was:

| Item | Result |
| --- | --- |
| Commit | `0ba50dc` |
| Workflow run | [40024600724](https://github.com/dimchansky/terminal-mcp/actions/runs/40024600724) |
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

A managed `make update` also succeeded from an MCP terminal session.

The controller reported `status: "applied"` and a healthy new server ID. This test confirms a supervised restart, not zero-downtime migration.

The unit tests cover additional results, including mutation retries, cancellation, terminal I/O, version checks, audit rotation, and update rollback.

A unit test result is not proof of every real-world failure condition.

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

A passing CI run also does not prove that the LaunchAgent can start on every macOS configuration.

## 5. Historical report

The initial source delivery had different test results. Its environment used Go 1.23.2 and could not fetch a required newer toolchain.

The initial report is available in the repository's Git history. It is historical evidence, not the current project status.

The source, tests, and CI setup changed after that delivery. Use the current Makefile and CI runs to check the present implementation.

## 6. Before a new release

1. Run `make check`.
2. Confirm that the Git worktree contains only the intended changes.
3. Check the macOS and Ubuntu CI results.
4. If the Go server changed, use `make update` on the local service.
5. Check `make status` after the update.
6. Check `refresh_tools_required`. Refresh the ChatGPT tools when it is true.
7. Review `make logs` and `make logs-service` if a test or update fails.

For design limits, read [SPEC.md](SPEC.md). For operating procedures, read [DEVELOPMENT.md](docs/DEVELOPMENT.md).
