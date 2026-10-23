# Validation report

Prepared on 2026-10-23 for this source archive.

## Release status

**The complete application has not been compiled or integration-tested in the
creation environment.** This is a source delivery, not a verified binary release.
The process core and HTTP trust-boundary packages were genuinely built and tested;
that does not validate the third-party adapters.

The available compiler was `go1.23.2 linux/amd64`. The pinned official MCP SDK
requires Go 1.25. Attempts to fetch the newer toolchain/dependencies failed because
outbound network/DNS access from the build container was unavailable. No dependency
source was replaced with a fake implementation, and no successful full-build or
full-suite result is claimed.

## Checks actually executed

| Check | Scope | Result |
|---|---|---|
| Native tests with the race detector, three repetitions | `internal/session`, standard library only | PASS |
| Native tests with the race detector, three repetitions | `internal/httpguard`, standard library only | PASS |
| `go vet` | Those same two packages | PASS |
| Compile-only tests for Linux arm64 | Those same two packages | PASS |
| Compile-only tests for Darwin amd64 | Those same two packages | PASS |
| Compile-only tests for Darwin arm64 | Those same two packages | PASS |
| `gofmt -l` | All supplied Go source/tests | No unformatted files |
| `sh -n` | Both check scripts | PASS |
| Full `go test ./...` with the installed compiler | Entire module | BLOCKED: Go 1.25 required |

There are 32 top-level test functions in `internal/session` (including the subprocess
helper entry point) and 4 in `internal/httpguard`. All were run three times by
`scripts/check-core.sh`. Its actual verbose output is in `CORE_TEST_RESULTS.txt`.
Compiler/platform and compile-only results are in `BUILD_STATUS.txt`.

The native process tests start real OS subprocesses, not just mocks. They exercise
separate output streams, exit status, retries/concurrent duplicates, server epoch
rejection, cancellation without termination, read deadlines during continuous
output, interruption while reads wait, backpressured/partial stdin writes,
stdin EOF, runtime limits, buffer gaps, inherited-descriptor drain deadlines,
environment/PATH/cwd behavior, active/retained limits, expired receipt replay,
large-argument list bounds, and invalid cursor/input/session-ID rejection.

HTTP boundary tests exercise loopback validation, Host/Origin rejection,
authentication including duplicate headers, body-size bounds, and token file
permissions. These are tests of the real middleware, not of ChatGPT.

## Tests supplied but NOT executed here

- Five PTY/emulator tests: screen overwrite and split UTF-8, application cursor keys,
  alternate screen and terminal replies, bounded reply storage, real PTY sizing and
  Ctrl+C, and interruptible PTY read closure. Several behaviors share one test.
- Two official-SDK tests: in-memory discovery/call/schema behavior, and HTTP client
  disconnect/reconnect while retaining a process, using the authentication wrapper.
- The full `scripts/check.sh` path (module verification, all race tests, full vet,
  and application build).
- The included GitHub Actions Linux/macOS workflow. It is configuration, not a
  workflow that has already run.

No live macOS test, real ChatGPT/Secure MCP Tunnel test, stdio-host end-to-end test,
long-duration soak, terminal conformance suite, or vulnerability scan was performed.
No Windows support is claimed.

## Required next verification on a networked machine

```sh
go mod tidy
./scripts/check.sh
```

Use Go 1.25 or newer and a native C toolchain for the race detector. `go mod tidy`
must generate real module sums; none were invented or copied from an unrelated
build. The direct dependency versions are pinned, but the resolved graph/checksum
file was not produced in this environment. Review and commit that result before
publishing a release.

Then test the actual ChatGPT/tunnel route, including a multi-minute command,
incremental output, input, interruption, and tunnel restart. A passing local SDK
test would still not establish hosted-client behavior.

## Known implementation boundaries, not test claims

- Sessions and receipts are memory-only. Restarting the daemon invalidates its
  server ID; stale mutations are rejected, not recovered.
- Cleanup is bounded best effort. Descendants can survive an early leader exit or
  escape its process group; no universal process-tree containment is provided.
- Terminal snapshots support conventional text-terminal behavior, not all modern
  graphics/Unicode/keyboard extensions.
- Mutation ledger capacity is finite and does not silently evict old keys.
- Arbitrary commands have the service user's full permissions; authentication is
  not a sandbox.
