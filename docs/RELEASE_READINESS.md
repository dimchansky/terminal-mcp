# Release readiness — dated reviews

> **Dated records:** the dashboard readiness of #10 (2026-11-10) and the
> historical stage 7 review (2026-10-26). Each describes its checks at that
> time, **not** the current release
> state, the present CI matrix or a deployment procedure. The application
> was installed and subsequently changed after the stage 7 review, including the
> removal of cold audit migration. Use [README.md](../README.md) for setup,
> [SPEC.md](../SPEC.md) for behavior, and current GitHub Actions for test
> results. Do not follow the old rollout checklist as instructions.

## Dashboard readiness (#10, 2026-11-10)

This section records the end-to-end checks of the owner-only activity
dashboard (#2, stages #3–#10) on the date above. Like the rest of this file,
it is a dated record; current results are in GitHub Actions.

### Automated checks

| Scope | Where | Evidence |
| --- | --- | --- |
| All page modules, and a boot smoke test of `app.js` | `make test-js` (`internal/viewer/webtest/*.test.js`) | Node 22, Linux, macOS, Windows CI |
| Page client against the real server | `TestPage*` in `cmd/terminal-mcp` (Node driver `journey.mjs` with the page's own modules) | Required in CI on all three platforms (`TERMINAL_MCP_REQUIRE_NODE=1`) |
| What the model asked for | `TestPageShowsWhatTheModelAskedFor` | Real MCP calls: readable commands with env first, copy only on exact round trip (exact tokens on Windows); terminal input with visible escapes; full `file_write`; red/green edits; unknown and nested fields; duplicate keys and inexact numbers flagged; a replay from another chat; a near-4-MiB record deferred and loaded byte for byte; an unrecordable input rejected before any effect, in its chat; no invented file contents or process output; every record byte-equal to its journal line; nothing in browser storage or URLs |
| Request budget | `TestPageFirstScreenRequestBudget` | First screen: one window request, no record reads; mid-size records inline across pages; a large record read once, on Load |
| History and archive | `TestPageHistoryJourney` | A chat only beyond the live window is found through Discover and the archive view; the live selection survives |
| Unread and auto-switch | `TestPageUnreadAndAutoSwitch` | Logical actions per chat, a poll storm once; no new unread for rows delivered again (reconnect, earlier cursor) or a rebuilt window (new generation, possible unseen activity); auto-switch only on new non-polling starts in another claimed chat, outside the hold, waiting while reading, never when off; separate request and effect durations |
| Lifecycle | `TestPageEndsOnStopAndNeedsANewCode`, `TestDashboardStream*`, `TestShutdownFencesAViewer…`, `TestForceStopEndsViewersFirst` | Stop and update end open pages with `server_stopping`, also with queued record reads, and no request follows; a restart needs a new code; force stop ends viewers first |
| Isolation and security | `TestTunnelCannotReachViewerRoutes`, `TestBrowserNeverReceivesTheMCPBearer`, asset bans, `TestThePageOffersNoPause` | The tunnel reaches only `/mcp`; the bearer never reaches the browser; inert rendering; no Pause |
| Rotation, slow viewers | `TestDashboardStreamCompleteAcrossRotation`, `TestDashboardStreamSlowConsumer`, Windows reader proofs | No lost rows across rotation; a slow viewer never blocks MCP writes |
| Older installed server | `TestDashboardExplainsServerProblems` | `terminal-mcp dashboard` says the server does not support the dashboard |
| MCP tool schemas | `tools/list` of a build of bbf28ac (before the dashboard) and of HEAD, over stdio with the audit off | All 14 tool schemas byte-identical |

### Manual check

Chromium (the desktop app's built-in browser) against an isolated
foreground server built from HEAD: its own port, token, audit file and file
root in a temporary directory, no tunnel. Two chats, a poll group, an
unattributed call, a live call raising "1 new" in another chat, and a
normal stop showing "The server is stopping or restarting". The fixture
checks of #9 (follow and Jump, deferral while reading, archive view,
keyboard, aliases) were repeated during #10's fixes. **Safari: not checked**
(owner decision: Chromium is enough).

### Baseline

Apple M1 Max, macOS, Go 1.27.3, Node 22; one run, not a guarantee.

| Measure | Result |
| --- | --- |
| `BenchmarkRecentAtCapacity` (one Recent request, window of 50,000 occurrences) | about 14 ms, 14.6 MB, 9,900 allocations |
| First screen over 50,000 recorded rows (`TestPageBaseline`) | about 45 ms after sign-in: one window request and the stream |
| Discovering 50 chats in that journal | about 9 s in 63 requests |
| `tools/call terminal_list`, median / p95, without a dashboard | 0.58 ms / 0.90 ms |
| The same with an open dashboard (live window and stream) | 0.38 ms / 0.71 ms (no measurable overhead; the difference is noise) |

### Defects found and fixed in #10

- An archive scan ignored updates that did not fit; a rebuilt archive window failed on every Earlier.
- A window that kept ending was reopened in a tight loop; the header said only "Offline".
- A record deferred for its size ended its page, so ordinary cards needed Earlier; now only records whose base64 form exceeds 1 MiB are deferred with their size.
- An oversized-input rejection lost its call metadata and showed as unattributed (owner decision: keep `call_meta`).
- The manual fixture command could print a stale code from a cached test result.

### Known limits

- Unread counts and a waiting auto-switch are per tab and in memory (the on/off setting is kept in localStorage); activity arriving only through a new snapshot is not counted (the page says so).
- One record read per request; a page reads at most 8 MiB of over-budget records (base64), plus one record.
- The embedded tunnel with real ChatGPT chats is checked by the owner after `make update` (owner decision); its result will be recorded in #10.

## Stage 7 review (2026-10-26)

The sections below record the stage 7 source review.

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

## Historical stage 7 release considerations

- **No automatic deployment:** Do not push, publish a release, overwrite the installed binary, or restart the user's running server without explicit authorization. Tests launch only isolated fixture instances; the live `server_id` is intentionally unchanged.
- **Windows release gate:** Source and tests compile on Windows, but compile-only verification cannot establish ACL correctness, process/terminal semantics, or standalone stop behavior. Run the real Windows test and smoke matrix before claiming a fully validated Windows release.
- **Durability semantics:** A successful audit append is *not* a per-record `fsync`; power loss or abrupt termination can leave unpaired starts. An unpaired start means **unknown**, not a proof of failure or success.
- **Audit coverage:** Decoded `tools/call` arguments and action receipts are covered, not malformed or unauthenticated transport traffic rejected before the SDK middleware. The owner explicitly requests full inputs and no secret redaction.
- **Tamper boundary:** The local OS user and programs running with the same privileges may modify audit files. The journal is not cryptographically tamper-proof or remotely attestable.
- **Storage exhaustion:** Total audit history is unlimited by default and archives are never automatically deleted. New MCP work fails closed whenever its required record cannot be appended; after transient filesystem exhaustion, the next successful append automatically restores health. An optional explicit `--audit-max-bytes=N` cap requires manual adjustment/restart if exhausted. Preserve historical gaps from failed completion writes; recovery cannot reconstruct them.
- **Diagnostic file rotation:** There is no lossy copy-truncate or unsafe rename of live stdout/stderr. Stop, confirm the old process and updater are not writing, archive the *closed* files, then restart. Audit archives have no automated pruning.
- **Rollback (at stage 7):** The original rollout required preserving the
  old audit journal, migration manifest and previous executable. This is
  historical evidence, not the current update procedure. The current updater
  pins one audit path for activation and rollback.
- **No permanent supervisor:** A crashed detached server is not automatically restarted. Availability monitoring is an operational decision separate from the audit design.

## Historical decision

At stage 7, the source verification supported a controlled macOS/Linux
installation after its remaining gates. That decision is a dated record,
not an instruction to copy legacy journals, inspect migration markers, or
preserve obsolete binaries.

For current operation, use the standalone updater described in
[README.md](../README.md#stop-and-update). The updater pins one explicit audit
path for activation and rollback. The normal stop and audit rules still apply.
There is no guarantee of per-event power-loss durability or automatic
recovery after a detached server crash.
