# Release readiness — MCP audit and logging redesign

Status: **stage 7 source review and verification complete; no production deployment**. This file records the evidence and explicit remaining gates before any actual installation, update, push, or public release. Completing a local commit does **not** deploy or activate code on the owner's computer.

## Scope of this review

The audit contract is to show the computer's owner the *complete decoded* input supplied through MCP tools, including rejected tool arguments, and to distinguish a completed MCP request from a mutation that can continue after request cancellation. Auditing is intentionally **not** a secret-redaction system or a sandbox.

The reviewed architecture uses a synchronous JSONL writer, private directories and advisory locks, two distinct request/effect lifecycle pairs, fail-closed pre-effect admission, a sticky degraded state, separate persistent HTTP server stdout/stderr, safe cold migration, and numbered audit archives without silent eviction. All mutating operations use the shared idempotency ledger.

## Stage 7 independent findings

1. **HIGH — stderr stall during an audit failure:** The original degradation diagnostic was emitted while the audit sink mutex remained locked. A blocked stderr could therefore indefinitely delay other admissions or clean shutdown. Fixed by atomically recording degradation *under* the mutex and emitting its one best-effort stderr message *after* unlocking. Regression tests hold a diagnostic handler blocked and verify immediate degraded status, denied later admissions, and independent `Close`.
2. **HIGH — panic after a mutation effect:** A panic in the effect-completion observer could have terminated the entire MCP process even though the effect had already happened. The ledger now recovers such observer panics and retains the actual, replayable receipt. The MCP audit observer additionally degrades audit health if its completion path itself panics; further new actions are then refused. Regression tests confirm no duplicate execution and that shutdown/update fences can complete.
3. **MEDIUM — Linux zombie process identification:** The updater/standalone shutdown path previously relied on `kill(pid, 0)` returning ESRCH. On Linux, an exited-but-unreaped zombie still responds to this probe; the controller could report a false stop timeout. The Linux implementation now also recognizes zombie/dead process states in `/proc/<pid>/stat`, parsing `comm` safely even when it contains spaces or parentheses. Missing procfs still conservatively reports the process as possibly alive; macOS retains its platform-specific behavior. The zombie regression runs as a native Linux process inside Docker.
4. **TEST ENVIRONMENT — privilege-dependent activation fault:** The legacy rollback test simulates installation failure with restrictive directory permissions. It is not meaningful when tests run as root, which can override that restriction. The complete Linux race suite was therefore run as an unprivileged container user, preserving this fault-injection coverage. Under root, the particular permission-injection test now explicitly skips itself rather than returning a false failure; a separate bad-restart regression still tests rollback. The root-only skip was checked in Docker. This is a test-fixture constraint, not evidence of a successful unauthorized update.

The stage-7 fixes are deliberately narrow; no protocol schema, historical audit record, deployment mechanism, or Go module dependency was replaced.

## Verification gates

| Scope | Command or method | Evidence |
| --- | --- | --- |
| macOS arm64, Go 1.27.3 | `make check`: all eight packages under `-race`, `go vet`, module verification and binary build | **PASS** |
| macOS audit contract | `make audit-feedback` | **PASS** |
| macOS static analysis | `make lint` | **PASS**, zero issues |
| macOS concurrency failures | `go test -race -shuffle=on -count=20` focused on blocked diagnostics, panic recovery, shutdown and update fencing | PASS |
| Linux/amd64 userland under Docker emulation on an ARM64 Linux host | Go 1.26.5, no network, source mounted read-only, unprivileged UID 65534, `go test -race -count=1 -timeout=240s ./...` | **PASS**, all eight packages; cmd integration suite 173.5 s |
| Linux zombie regression | Actual Linux process left unreaped during `waitForExit`, then reaped in cleanup | **PASS** |
| Linux amd64 static checks | Cross-compile test binary and `GOOS=linux go vet ./...` | **PASS** |
| Windows amd64 | Cross-compile application, audit and session test executables; `GOOS=windows go vet ./...` | **PASS** (compile/static only) |
| Native Windows | Run tests on a real Windows host, including ACL, process lifecycle, named files and stderr paths | **Not performed** |
| Fault injection | Incomplete JSONL, archive conflicts, hard storage limit, canceled requests, degraded audit, stalled stderr, panic after mutation, idempotency replay | **PASS**, macOS and Linux suites |
| Update/rollback | Isolated real-process candidate preflight, activation, migration and rollback; actual installed server not touched | **PASS**, macOS and unprivileged Linux |

## Non-negotiable release and rollout considerations

- **No automatic deployment:** Do not push, publish a release, overwrite the installed binary, or restart the user's running server without explicit authorization. Tests launch only isolated fixture instances; the live `server_id` is intentionally unchanged.
- **Windows release gate:** Source and tests compile on Windows, but compile-only verification cannot establish ACL correctness, process/terminal semantics, or standalone stop behavior. Run the real Windows test and smoke matrix before claiming a fully validated Windows release.
- **Durability semantics:** A successful audit append is *not* a per-record `fsync`; power loss or abrupt termination can leave unpaired starts. An unpaired start means **unknown**, not a proof of failure or success.
- **Audit coverage:** Decoded `tools/call` arguments and action receipts are covered, not malformed or unauthenticated transport traffic rejected before the SDK middleware. The owner explicitly requests full inputs and no secret redaction.
- **Tamper boundary:** The local OS user and programs running with the same privileges may modify audit files. The journal is not cryptographically tamper-proof or remotely attestable.
- **Storage exhaustion:** Total audit history is unlimited by default and archives are never automatically deleted. New MCP work fails closed whenever its required record cannot be appended; after transient filesystem exhaustion, the next successful append automatically restores health. An optional explicit `--audit-max-bytes=N` cap requires manual adjustment/restart if exhausted. Preserve historical gaps from failed completion writes; recovery cannot reconstruct them.
- **Diagnostic file rotation:** There is no lossy copy-truncate or unsafe rename of live stdout/stderr. Stop, confirm the old process and updater are not writing, archive the *closed* files, then restart. Audit archives have no automated pruning.
- **Rollback:** Preserve the original audit journal, archives, migration manifest and previous executable. An update/rollback must keep the explicitly selected audit path and must not silently create an independent history.
- **No permanent supervisor:** A crashed detached server is not automatically restarted. Availability monitoring is an operational decision separate from the audit design.

## Controlled installation checklist (not executed)

1. Review the final diff, tests and this checklist. Confirm there are no active managed sessions or pending mutations on the live instance and that the user has approved changing it.
2. Save immutable copies of the current executable, legacy audit and numbered archives, new audit family (if already present), and existing stdout/stderr. Do not delete either journal or its lock.
3. Use the standalone updater's explicit preflight/checks (or its supported manual equivalent), without overriding the audit path or `audit-max-bytes`. Stop the old process cleanly; an already-active writer must prevent a second audit process from starting.
4. Verify the activated instance with authenticated `status`: new `server_id`, correct binary hash, `audit.state: ok`, correct audit path, `bytes_used` and optional `bytes_limit`, and separate stdout/stderr descriptors. Confirm a harmless `terminal_list` request produces a request-start/request-finish pair.
5. Verify recovery guidance by observing controlled stop and checked-error reporting. If activation fails, inspect the existing migration marker before rollback so an old binary does not switch back to a different journal. Preserve all historical files.

**Decision:** Local source verification can support a controlled macOS/Linux rollout after all final suites pass and the owner explicitly authorizes deployment. Native Windows verification remains a separate release gate. There is no claim of power-loss durability, tamper-proof logging, or automatic recovery from a crashed detached server.
