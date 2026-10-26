# Logging and audit redesign — target contract

> **Status: staged implementation; stage 1 completed in source, stages 2–7 pending.**
> This document is the agreed **target** contract, not a claim that every
> requirement below is implemented. [SPEC.md](../SPEC.md) and
> [README.md](../README.md) describe the current checked-out implementation.
> A new source commit is not a live-server update.
>
> Baseline: `main` at `bee1cfa184437e795c7b8e242a202766d97cb827` (2026-10-26).
> No production-server restart or update is part of stage 0.

## 1. Purpose, scope, and trust boundary

The primary purpose of the audit is to let the computer's owner investigate **what
an MCP client (including a language model) asked the server to do, whether the
server accepted the work, and what actually happened**. The owner explicitly
wants complete inputs, not secret redaction.

- Preserve **all supplied fields and values** in authenticated, syntactically
  valid `tools/call` requests, including `argv`, supplied `env` overrides,
  `cwd`, exact `terminal_write` input, `file_write.content`,
  `file_edit.edits`, other file-operation arguments, and arguments rejected
  by tool-schema validation. Do not mask secrets, omit user content, silently
  truncate values, or substitute only lengths/hashes for contents.
- Preserve the JSON data faithfully, including unknown fields in a rejected
  call. Re-encoding JSON may normalize whitespace or object-key order; the
  requirement is the complete **values supplied**, not a byte-for-byte HTTP
  transcript.
- If a complete accepted event cannot be represented or stored within an
  explicit resource limit, reject the request **before any tool effect** with a
  visible error. Never claim to have fully audited a partially retained input.
- The boundary begins with authorized `tools/call` requests that the MCP
  transport can parse. Malformed HTTP/JSON rejected before that point,
  rejected authentication, `initialize`, and `tools/list` are **not**
  promised as part of the tool-action audit. This is not full wire-traffic
  recording. The current HTTP guard and stdio line limits remain in force.
- Supplied `env` means variables passed as MCP arguments; it does not imply
  a snapshot of the entire effective environment inherited from the host.
- Tool invocation audit and **child process stdout/stderr** are different
  streams. Child output remains in the existing bounded session journal;
  copying it into the audit is not a requirement.
- The shared HTTP bearer token does not reliably identify which language
  model or individual initiated a call. Never invent that identity.
- Keep private directory/file permissions (Unix 0700/0600; Windows ACLs) to
  protect the owner's data from other OS users. This is **not protection from
  the owner** or from code executed with the owner's own privileges.
- **Not tamper-proof**: an authorized client can run arbitrary commands with
  the server account's privileges and may be able to modify/delete audit files.
  External tamper resistance, sandboxing, and non-repudiation are out of scope.

## 2. Distinct streams and paths

Do not conflate these concerns:

| Stream | Purpose | Target behavior |
| --- | --- | --- |
| HTTP background server stdout | Preserve otherwise unexpected process output | Separate persistent `server.stdout.log` |
| HTTP background server stderr | Operational diagnostics, runtime messages, failures | `log/slog` to stderr, persisted in `server.stderr.log` |
| MCP audit | Full accepted tool inputs, rejections, request and effect outcomes | Synchronous JSONL writer, checked errors |
| Updater worker and preflight | Technical evidence for a particular update job | Private job-specific `worker.log`, `preflight.log`, `status.json` |
| Child terminal processes | Cursor-addressable output and process observations | Existing in-memory session journal, unchanged |

Target directory layout:

```text
macOS and Linux:
  ~/.config/terminal-mcp/logs/
    server.stdout.log
    server.stderr.log
    audit.jsonl
    audit.jsonl.lock
    audit archives (see retention policy)
  ~/.config/terminal-mcp/standalone/
    config.json
    installed binary and controller state
  ~/.config/terminal-mcp/updates/<job-id>/
    manifest, status, worker/preflight logs, rollback material

Windows:
  %APPDATA%\terminal-mcp\logs\
    server.stdout.log, server.stderr.log, audit.jsonl, lock, archives
  %APPDATA%\terminal-mcp\standalone\
  %APPDATA%\terminal-mcp\updates\ (where supported)
```

- In `stdio` transport, **stdout belongs exclusively to the MCP protocol**;
  do not redirect it to a diagnostic file. Use stderr for diagnostics. When
  configured, the same full tool-audit contract applies to stdio; explicit
  `--audit-file=off` remains an intentional opt-out.
- The background HTTP server opens both output logs at stable paths for the
  complete life of the server, across successful updates and rollbacks.
  Distinct stdout/stderr files preserve evidence, but exact ordering between
  independent streams cannot be reconstructed from them.
- Keep update-job logs for the worker and candidate preflight. An activated
  or rolled-back **normal server** writes to the shared server logs, not an
  update job's `server.log` or `rollback.log`.
- Continue to separate updater's machine-readable status from its free-form
  diagnostics. Do not add a permanent supervisor.
- The mechanism for rotating raw redirected stdout/stderr **must account for
  open file descriptors**. Renaming an open file alone does not redirect its
  writer. Do not implement lossy copy-truncate and claim no gaps.

## 3. Audit event semantics

Every auditable tool request has a fresh `request_id`, even when it repeats
a previous `operation_id`. Each event has a timestamp, `server_id`, tool
name, and an explicit phase. Introduce a schema version for new events;
older `start`, `finish`, `rejected` JSONL must remain readable as history
without destructive rewriting.

**Request lifecycle (including invalid tool arguments):**

1. `request_start`: complete received `tools/call.arguments`, tool name,
   request ID, server ID. Must be successfully appended before dispatch or
   schema validation that might lead to effects.
2. `request_finish`: outcome of this **MCP request**, including schema
   rejection, ordinary result, transport/context cancellation, or uncertainty
   when no actual operation result was returned. It **does not** assert that
   an independently executing mutation has finished.

A request rejected by schema validation has complete input evidence and
a `request_finish` with a rejection status, but no operation events.

**Effect lifecycle (new mutating operation only):**

1. `operation_start`: `server_id`, `operation_id`, originating
   `request_id` and tool name; append successfully at the actual
   **new-effect admission/execution boundary** before an irreversible effect.
2. `operation_finish`: actual operation receipt/status (including partial
   or unknown on recovered panic) emitted when the independent mutation
   finishes, even if the initiating MCP request was canceled.

A replay of the same `operation_id` adds its own request events but **does
not create another operation start/finish or execute the effect twice**. A
conflicting `operation_id` and a denied new operation are recorded as request
errors, not as successful operations. Preserve the existing instance-scoped,
at-most-once ledger behavior; a restart does not create exactly-once execution.

If the process crashes, an unmatched `request_start` or `operation_start`
indicates **unknown outcome**, not a successful or failed effect. If an
operation outlives a canceled request, `request_finish` may precede the
corresponding `operation_finish`. A successful process-launch receipt does
not imply that the child command has exited successfully.

**Implementation constraint:** current
`internal/mutation/ledger.go:45-100` runs effects in an independent
goroutine and can return on context cancellation first. A request-handler
`defer` cannot truthfully produce `operation_finish`. Place that event
at the real ledger execution/completion boundary; prevent a failed pre-effect
audit from accidentally reserving a retry key for an effect that never ran.
Do not perform blocking audit I/O while holding the ledger's state mutex.
Preserve the ledger's `Freeze`/updater fence and panic semantics.

## 4. Write, failure, and durability contract

Preferred implementation: `encoding/json` + `sync.Mutex` + minimal existing
private JSONL file sink. No queue, asynchronous worker, silent queue overflow,
or standalone telemetry framework. Keep proven privacy checks, interprocess
lock, partial-write recovery, and an explicitly tested rotation mechanism.

- **Successful `Write`:** the entire serialized JSONL record was reported
  written by the OS without an observed write/rotation error. It does **not**
  mean `fsync` happened or that data will survive loss of power.
- **Failed `Write`:** return the error immediately. A partially written
  record must not be treated as a successful event. Detect or repair an
  incomplete final JSONL line before writing further complete records.
- **Shutdown:** attempt `Sync`, `Close`, and release the interprocess
  lock; return/check errors. Neither a crash nor power loss guarantees a
  matching `finish` or preservation of the last record.
- **Concurrency:** serialize appends and file rotation; no audit rejection
  merely because concurrent callers filled an application queue. File I/O may
  block if the file system stalls. Do not introduce goroutine-per-write timeouts
  that leave unknown writes running in the background.
- **Startup:** when auditing is enabled, failure to initialize its file,
  directory, permissions, or exclusive lock **prevents the MCP service from
  accepting tool calls**. The explicit `--audit-file=off` option remains an
  operator choice; it must never be selected implicitly after a failure.
- **Before dispatch:** if `request_start` cannot be stored, reject the tool
  call without invoking it. If `operation_start` cannot be stored, refuse
  the **new effect** at the real ledger execution boundary.
- **After an effect:** failed `operation_finish` or `request_finish` must
  not falsify or roll back an effect that already occurred. Preserve the
  actual receipt/outcome and surface audit degradation separately.
- **Degradation:** expose a sticky audit-error state and immediately attempt
  a direct stderr diagnostic, outside the audit sink (no recursive logging).
  Subsequent new auditable work is refused until a controlled recovery such
  as repair and restart. Avoid infinitely repeated identical diagnostics.
  Local authenticated administration such as `status` and `stop` must
  remain usable to diagnose/recover a broken audit.
- **Storage limits:** never silently discard old audit archives to keep
  accepting unaudited work. Rotate files for manageability, with a documented
  configurable total-history budget/retention policy. On a hard audit storage
  limit, report the error and refuse new effects rather than quietly delete
  history. Exact default limits and safe implementation belong to the
  retention stage; do not misrepresent existing 4 MiB + three archives as
  lossless historical storage.
- **Diagnostics are best effort:** if stdout/stderr and audit share a full
  or inaccessible filesystem, a stderr write can fail too. The system must
  not promise an infallible secondary output channel.

## 5. Safe update and migration requirements

- The legacy live service keeps running until a tested release is explicitly
  applied. Intermediate stage commits do **not** trigger self-update.
- Existing default audit files and archives are on macOS under
  `~/Library/Application Support/LocalTerminalMCP/`; on Linux under
  `$XDG_STATE_HOME/terminal-mcp/` (or the home fallback); on Windows under
  `%APPDATA%\terminal-mcp\`. Existing `standalone/server.log` and
  update-job logs are historical evidence, not disposable temp files.
- Plan a cold, verified migration of the entire audit family (including
  numbered archives and the required lock semantics). Do not move/delete
  a journal while a live process owns it. Verify contents before changing
  the configured active path. Preserve originals for rollback.
- Explicitly propagate the selected audit path through `start`, saved
  configuration, running process arguments, candidate activation, and
  rollback. A previous binary must not silently resume writing the old
  default while the new binary writes to the new default.
- Validate both normal update and failure/rollback paths in isolated tests
  before touching the live instance. The preflight test instance can
  explicitly use `--audit-file=off`; its internal probes are not model calls.
- `status` should expose current diagnostics paths, audit location,
  `ok/degraded/disabled` state, and last audit error without requiring
  successful writing to the failed audit file.

## 6. Baseline observations (stage 0)

At the pinned baseline:

- `internal/audit/audit.go`: asynchronous 1,024-entry / 8 MiB queue;
  nonblocking submission can return `ErrQueueFull`; disk-write errors
  surface to the recorder only at shutdown.
- `internal/audit/sink.go`, `sink_unix.go`, `sink_windows.go`: JSONL,
  partial-tail repair, size-based rotation (4 MiB with three archives),
  privacy checks and an exclusive writer lock.
- `internal/mcpserver/server.go`: request callback `start` and `finish`
  events; audit errors do not block tool execution.
- `internal/mcpserver/auditargs.go`: full `terminal_start` and
  `terminal_write` inputs, but not full file-write/edit contents.
- `internal/mcpserver/audit_validation.go`: rejected arguments are omitted.
- `internal/mutation/ledger.go`: execution can outlive a canceled MCP
  request; existing request `finish` is not an operation completion event.
- `cmd/terminal-mcp/main.go`: HTTP audit may fail open at startup.
- `cmd/terminal-mcp/standalone.go` and `standalone_windows.go`:
  stdout/stderr share an unrotated `server.log`.
- `cmd/terminal-mcp/update_worker.go`: activated server diagnostics
  move into the update-job directory (and rollback diagnostics into
  `rollback.log`).
- `internal/session/journal.go`: a separate bounded, in-memory child
  output journal; it must not be converted into the audit sink.
- `internal/httpguard/guard.go`: HTTP request body currently limited to
  1 MiB; stdio transport uses the same maximum line length.

The baseline suites include concurrency/race, error injection, schema
rejections, request cancellation, standalone lifecycle, and updater tests.
Keep these tests as regression evidence, but adjust tests that explicitly
assert the **old** loss/fail-open/redaction-like omission behavior.

### Stage 0 baseline validation (macOS, 2026-10-26)

- Initial `git status --short --branch`: `## main...origin/main` (clean tree).
- `make check`: **PASS**, exit code 0. It ran formatting checks,
  `go mod verify`, `go mod tidy -diff`, `go test -race -count=1 -timeout=120s ./...`,
  `go vet ./...`, and a separate binary build. All eight Go package tests passed.
- This is a baseline of the **old implementation**, not evidence that the
  target contract above has been implemented or tested.
- No `start`, `stop`, `update`, or other live-server control command was issued.

## 7. Ordered implementation stages and acceptance

Each stage is separately reviewed, tested and committed. Do not proceed to
the next stage in the same change set. Update the current-behavior
documentation only as each corresponding implementation becomes true.

| Stage | Change | Minimum acceptance evidence |
| --- | --- | --- |
| **0** | Record target contracts and baseline | Clean initial worktree, documented semantic boundaries, baseline `make check`, docs-only commit |
| **1 — source complete** | Replace audit queue/worker with synchronous `Write` | No `ErrQueueFull` or background worker; concurrent calls and failures tested with `-race`; no queue drops |
| **2** | Separate MCP request and independent operation lifecycles | Cancellation, replay, conflict, actual completion, panic, shutdown and update fence tests |
| **3** | Audit all supplied tool arguments, including rejected calls | Full `file_write`/`file_edit` input and unknown rejected fields retained; no implicit truncation; oversized call denied before effect |
| **4** | Enforce audit-before-action and predictable degradation | Failed start/appends refuse new effects; preserved actual receipts; no unsafe retry; status/stop still usable |
| **5** | Stable split diagnostic logs and audit path, plus migration | stdout/stderr captured separately, stable after update/rollback, old archives preserved, no simultaneous old/new audit writers |
| **6** | Standard `slog`, status, and explicit retention policy | stderr diagnostics, usable status, no silently removed audit history, safe diagnostic log rotation plan |
| **7** | End-to-end validation and documentation finalization | macOS/Linux/Windows checks, isolated updater/rollback tests, failure injection, `make check`, release readiness review |

For all stages: keep changes minimal and idiomatic Go; prefer existing tested
components over new libraries when they do not remove real complexity. Do not
update the installed/live MCP server as a side effect of a test, build, or
commit.

## 8. Stage 1 implementation and verification (2026-10-26)

**Source implementation complete; not deployed to the running MCP instance.**

- `internal/audit/audit.go`: removed queue, asynchronous worker, in-flight
  accounting, overflow error, and deferred disk error reporting. `Write` now
  appends under one mutex and returns any append/rotation error immediately.
  Concurrent `Write` and `Close` serialize; repeated `Close` reports the
  same sink-close error, with no double close.
- Retained the original `fileSink`, JSONL encoding, private permissions,
  interprocess lock, size rotation, partial-tail repair, and existing event
  schema. No new dependency was added.
- MCP call sites now log every `Write` error, without special treatment for
  the removed `ErrQueueFull`.
- Replaced obsolete queue tests with backpressure, sink serialization,
  concurrent `Write`/`Close`, direct error propagation, no-truncation,
  and on-disk visibility-before-`Close` tests. The existing rotating
  concurrent stress test now counts every valid JSONL record across all
  retained files (the test fits within the retention window).
- Updated [README.md](../README.md), [SPEC.md](../SPEC.md), and
  [DEVELOPMENT.md](DEVELOPMENT.md) to document **current** behavior.

**Verification, local macOS:**

- `make check`: **PASS**, including full race-enabled tests,
  `go vet`, module verification, and Go build.
- `make audit-feedback`: **PASS**, exercising real MCP audit events.
- `go test -race -shuffle=on -count=6 -timeout=180s ./internal/audit`:
  **PASS**.
- `go test -race -count=20 -run '^(TestWriteIsVisibleOnDiskBeforeClose|TestCloseWaitsForInFlightSynchronousWrite|TestConcurrentWritesWaitForSlowSink)$' ./internal/audit`:
  **PASS**.
- `make lint`: **PASS**, zero issues.
- Windows amd64 application cross-compile with `CGO_ENABLED=0`:
  **PASS** (compilation only, no Windows runtime test).
- `git diff --check`: **PASS**.

**Intentionally unchanged until later stages:** the old request
`start`/`finish` semantics, omission of full file-write/edit input and
schema-rejected arguments, fail-open behavior on audit failure, pre-existing
audit and diagnostic paths, and three-archive rotation that evicts older
history. A successful `Write` still is not `fsync` or crash/power-loss
durability.
