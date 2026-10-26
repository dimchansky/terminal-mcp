# Logging and audit redesign — target contract

> **Status: stages 0–7 implemented and validated in source; Windows native runtime checks remain a release gate.**
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
| **2 — source complete** | Separate MCP request and independent operation lifecycles | Cancellation, replay, conflict, actual completion, panic, shutdown and update fence tests |
| **3 — source complete** | Audit all supplied tool arguments, including rejected calls | Full `file_write`/`file_edit` input and unknown rejected fields retained; no implicit truncation; oversized call denied before effect |
| **4 — source complete** | Enforce audit-before-action and predictable degradation | Failed start/appends refuse new effects; preserved actual receipts; no unsafe retry; status/stop still usable |
| **5 — source complete** | Stable split diagnostic logs and audit path, plus migration | stdout/stderr captured separately, stable after update/rollback, old archives preserved, no simultaneous old/new audit writers |
| **6 — source complete** | Standard `slog`, status, and explicit retention policy | stderr diagnostics, usable status, no silently removed audit history, safe diagnostic log rotation plan |
| **7 — source complete** | End-to-end validation and documentation finalization | macOS/Linux race suites, Windows cross-checks, isolated update/rollback, failure injection, `make check`, explicit Windows native release gate |

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


## 9. Stage 2 implementation and verification (2026-10-26)

**Source implementation complete; not deployed to the running MCP instance.**

- Added `schema_version: 2` and explicit phases `request_start`,
  `request_finish`, `operation_start`, `operation_finish`. Legacy JSONL
  remains valid and is not rewritten.
- An accepted MCP call receives a fresh `request_id`, including a retry. A
  new ledger mutation receives exactly one effect pair, correlated with
  `operation_id` and the originating `request_id`. SDK schema rejections
  receive one request pair and no effect events; raw rejected arguments
  remain deferred to stage 3.
- Introduced a minimal per-request `mutation.Observer` connected through
  context. Notifications are sent by the **effect goroutine** at the real
  execution and completion boundaries, never by a request's `defer`.
  Successful replay, conflicting `operation_id`, and rejection before
  admission do not create a second effect pair.
- The observer receives the final bounded ledger receipt, including
  `unknown` after recovery from a panic. An MCP cancellation or deadline
  may end the request with an `unknown` effect outcome; the effect can still
  complete and log a later `operation_finish`.
- `Ledger.StopAndWait` freezes new admissions and waits on a standard
  `sync.WaitGroup` for all already accepted effect callbacks and audit
  notifications. The session manager waits for these before tearing down
  managed sessions; in stdio mode it also waits before closing the auditor.
  An expired updater fence cannot reopen a closed manager.
- Preserved existing update `Freeze` semantics (an in-flight effect,
  including its final audit hook, blocks update). Replays of completed
  operations continue to share the original receipt.
- `operation_finish` contains a normal bounded receipt. This may include
  a `file_edit` preview with fragments of old/new text; the request-input
  projection still omits full edit/write contents pending stage 3.

**Verification, local macOS:**

- `make check`: **PASS**, including race-enabled package tests, Go module
  verification, `go vet`, and build.
- `make audit-feedback`: **PASS**.
- `go test -race -shuffle=on -count=30 -timeout=180s -run '^(TestObserver|TestStopAndWait|TestFreezeWaitsForOperationAuditFinish|TestManagerCloseWaitsForCanceledRequestEffectAndAuditFinish|TestMCPRequestCancellationDoesNotMisreportCompletedMutation)' ./internal/mutation ./internal/session ./internal/mcpserver`:
  **PASS**, thirty repeated randomized-order runs.
- `make lint`: **PASS**, zero issues.
- Windows amd64 application cross-compile with `GOOS=windows`,
  `GOARCH=amd64`, `CGO_ENABLED=0`: **PASS**, not a native Windows runtime test.
- `git diff --check`: **PASS**.

**Intentionally pending:** capture of *complete* `file_write`/`file_edit`
arguments and rejected raw tool arguments (stage 3); audit-before-effect
enforcement, fatal enabled-audit startup errors, and sticky degradation
(stage 4); unified stable diagnostic/audit locations and migration (stage 5);
diagnostic formatting and lossless retention policy (stage 6).

## 10. Stage 3 implementation and verification (2026-10-26)

**Source implementation complete; not deployed to the running MCP instance.**

- Moved `request_start` recording to an SDK receiving middleware that
  accesses `CallToolParamsRaw.Arguments` **before** schema validation or
  typed Go unmarshaling. Valid inputs and rejected inputs share this
  mechanism, with complete JSON argument values including unknown fields.
  Request IDs are passed through context to the typed request handler;
  the handler is no longer allowed to silently bypass middleware.
- The input now includes exact `file_write.content`, complete
  `file_edit.edits` (both `old_text` and `new_text`), supplied
  `terminal_start.argv` and `env`, and exact text, key and base64 terminal
  inputs. The SDK's recognized `_meta`, `inputResponses` and
  `requestState` fields are also recorded if provided.
- Existing SDK schema rejections keep their correlated
  `request_start`/`request_finish` pairs, but the start event now carries
  their full raw arguments. The previous fragile post-hoc projection of
  typed file-tool values has been removed. No redaction or masking occurs.
- `audit.Write` now distinguishes `ErrEventTooLarge` and
  `ErrInvalidEvent`. A tool call whose complete encoded request event
  exceeds the 4 MiB JSONL record limit, or cannot be serialized, is
  **refused before dispatch** with a clear MCP error. Only a small
  `request_rejected` event (with the reason and no misleading partial
  arguments) is written. No mutation is admitted in this path.
  Ordinary filesystem append/rotation failures remain fail-open until
  stage 4; this exception does not implement the full fail-closed policy.
- A syntactically invalid/unparseable JSON-RPC message rejected before
  `CallToolParamsRaw`, or unauthenticated HTTP traffic, is outside this
  tool-action audit. Serialization can normalize insignificant whitespace,
  key ordering and escapes; all decoded input **values** are retained.
  Original byte-for-byte wire framing is not promised.
- Existing event and operation lifecycles remain unchanged. Historical
  JSONL records are preserved. No new Go module dependency was added.

**Verification, local macOS:**

- `make check`: **PASS**, all eight race-enabled package suites, module
  verification, `go vet`, and binary build.
- `make audit-feedback`: **PASS**, checking a real MCP client/server audit
  loop with invalid schema input.
- `go test -race -shuffle=on -count=12 -timeout=180s -run
  '^(TestAuditFullFileInputsAndRejectedRawArguments|TestOversizedAuditedRequestIsRejectedBeforeEffect|TestAuditKeepsTerminalEnvironmentAndExactInput|TestMCPRequestCancellationDoesNotMisreportCompletedMutation)$'
  ./internal/mcpserver`: **PASS**, twelve repeated randomized-order runs.
- Integration tests check complete real file mutations, rejected raw
  arguments with unknown nested fields, JSON and Unicode escaping, call
  metadata, inherited invocation correlation, exact terminal env and
  input/base64, and an oversize request refused before execution.
  A near-limit (but representable) request is executed and recorded
  without truncation. Audit-package tests verify typed error sentinels.
- `make lint`: **PASS**, zero issues.
- Windows amd64 application cross-compilation: **PASS** (not native
  Windows runtime validation).
- `git diff --check`: **PASS**.

**Intentionally pending:** fail-closed behavior for write failures and
initialization, sticky audit degradation (stage 4); stable log path and
controlled data migration (stage 5); diagnostic logger and explicit
non-evicting audit retention policy (stage 6).


## 11. Stage 4 implementation and verification (2026-10-26)

**Source implementation complete; not deployed to the running MCP instance.**

- `audit.Log.Start` synchronously appends pre-dispatch/pre-effect events,
  checking sticky health while holding the **same sink mutex** as writes.
  New requests never invoke their handlers without a successful
  `request_start`. Already admitted requests continue to use `Write`
  to attempt their true finish/receipt events even after degradation.
- The first sink failure or a failed post-effect event sets a permanent
  instance-scoped `degraded` state. An atomic, nonblocking health snapshot
  (`ok`, `degraded`, `disabled`, path and first error) remains accessible
  even if an audit writer is stalled. The first error emits **one**
  best-effort stderr diagnostic outside the sink; no recursive logging,
  background queues, or new dependencies are required.
- The audit-enabled server **refuses startup** if its directory, permissions,
  exclusive lock, audit file open, or required `server_start` write fails.
  Explicit `--audit-file=off` and the default stdio opt-out remain valid.
- `mutation.Observer.Started` now returns an error. The actual mutation
  goroutine refuses execution on a failed pre-effect audit, reports
  `operation_not_executed`, and releases the unused `operation_id` for
  a later safe retry. It never does blocking audit I/O with the ledger
  mutex held. Existing in-flight mutation receipts are unchanged if
  `operation_finish` fails after the action was applied; the audit becomes
  degraded and rejects subsequent new MCP requests.
- Standalone authenticated `/admin/runtime` and CLI `status` expose
  the live audit snapshot. Administrative `begin`/`shutdown` remain
  accessible even when the audit is degraded, still honoring existing
  active-session and mutation-fencing safeguards. Status is read without
  waiting for file I/O.
- Full raw MCP inputs, operation lifecycle events, JSONL format, rotation,
  existing audit path and old journals are retained. No automated migration
  or release of the running server was performed.

**Verification, local macOS:**

- `make check`: **PASS**, including race tests for all Go packages, module
  verification, `go vet`, and application build.
- `make audit-feedback`: **PASS**.
- `go test -race -shuffle=on -count=20 -timeout=180s` with the focused
  stage-4 failure/startup/status patterns in `internal/audit`,
  `internal/mutation`, `internal/mcpserver`, and
  `cmd/terminal-mcp`: **PASS**, 20 randomized repetitions.
- `make lint`: **PASS**, zero issues.
- Windows amd64 application cross-compile: **PASS** (no native Windows run).
- `git diff --check`: **PASS**.
- Fault tests exercise immediate disk-error propagation, sticky failure
  even after a successful later write, failed rotation and recovery,
  concurrent status during a stalled sink, pre-effect veto without
  reservation or execution, a panicking pre-effect observer, preserved
  `applied` receipt when operation-finish audit is too large,
  invalid startup paths, exclusive-lock conflicts, failed server-start
  audit writes, and authenticated status/maintenance during degradation.

**Remaining limitations:** a successful append is not an `fsync`;
power failure or crashes may lose recently written events. The existing
three-archive rotation can evict old audit history (stage 6). The
self-managed stdout/stderr file paths and update/rollback migration
remain stage 5. A process with the local user's own privileges can still
tamper with the audit, as documented in the original trust boundary.


## 12. Stage 5 implementation and verification (2026-10-26)

**Source implementation complete; not deployed to the running MCP instance.**

- Unified default on macOS/Linux: `~/.config/terminal-mcp/logs/`; on
  Windows: `%APPDATA%\\terminal-mcp\\logs\\`. Files:
  `audit.jsonl`, its numbered archives and persistent lock,
  `server.stdout.log`, and `server.stderr.log`.
  Detached server stdout and stderr now append to **separate** private
  files across ordinary standalone starts, successful updates and rollbacks.
  The stdio MCP protocol continues to own stdout, unchanged.
- Added verified **cold migration** for the former macOS, Linux and Windows
  default audit locations. Migration obtains both legacy/new writer locks,
  refuses a still-active old writer, copies the original audit and **every
  numbered archive**, verifies SHA-256 digests, and persists a private marker
  with all source hashes. The originals are never moved, overwritten or
  removed. The new running process retains the legacy lock until shutdown
  to fence cooperating older writers. Interrupted copying can resume only
  if existing targets agree with originals. Missing active history, an
  independent destination archive, or changed source history produces an
  explicit startup refusal rather than silently merging journals.
- Explicitly configured nondefault audit paths remain unchanged and are
  **not** migrated. Explicit `--audit-file=off` stays disabled; stdio still
  disables auditing by default. The resolved default path is persisted in
  standalone launch settings.
- The updater now explicitly pins the selected audit path in both candidate
  activation and rollback arguments. It reads the running server's audit
  health/path when available, and handles old binaries that only expose
  the original command-line arguments. After a candidate has already
  completed migration, rollback restarts the restored executable against
  the **migrated path** rather than creating a second audit in the old
  location. If migration did not complete, rollback uses the original
  path. Readiness checks verify audit location and health.
- Worker and preflight diagnostics remain job-specific; ordinary server
  stdout/stderr use the stable `logs/` paths, including rollback. Old
  `standalone/server.log`, previous update-job `server.log` /
  `rollback.log`, and the source audit journal are preserved untouched.
  No remote push, live service update or process restart was performed.

**Verification (macOS, isolated temporary homes and test servers):**

- `make check`: **PASS**, all Go package suites under `-race`,
  `go vet`, dependency checks, and a binary build.
- `make audit-feedback`: **PASS**.
- `make lint`: **PASS**, zero issues.
- Windows amd64 application cross-compilation: **PASS** (not native Windows
  runtime testing).
- `go test -race -count=1 -run '^TestMigration' ./internal/audit`:
  **PASS**, including archive copies, partial copy recovery, lock conflicts,
  changed history, symlink refusal, unexpected independent destination
  archives, and missing active file with a marker.
- Isolated end-to-end updater tests:
  `TestUpdaterMigratesLegacyAuditAndKeepsStableDiagnosticLogs` and
  `TestUpdaterRollbackAfterSuccessfulAuditMigration`: **PASS**.
  The latter injected a real failure after migration and verified the
  rollback binary, journal path, legacy lock and complete history.
- `git diff --check`: **PASS**.

**Intentionally pending stage 6:** stdout/stderr files currently append
without automatic rotation. Renaming a file whose child process retains an
open descriptor is not a valid rotation strategy. Audit files still rotate
at 4 MiB with only three archives, so older events may be deleted by the
existing retention policy. A successful append is not a per-event `fsync`,
and the local owner or arbitrary code under their UID can still tamper with
unprotected files. Do not represent this stage as lossless lifetime retention
or tamper-proof history.


## 13. Stage 6 implementation and verification (2026-10-26)

**Source implementation complete; not deployed to the running MCP instance.**

- Replaced repository-owned runtime `log.Printf` and ad-hoc stderr
  diagnostics with the standard `log/slog` API. Default `slog` writes
  structured text diagnostics directly to stderr, which detached HTTP
  servers already persist in `server.stderr.log`. MCP stdio stdout remains
  reserved for the protocol. CLI data output (JSON) stays on stdout.
  There is no new logging framework or dependency.
- The active audit journal remains bounded to 4 MiB. On rotation it is
  renamed only to the **next increasing archive suffix**, preserving every
  existing `audit.jsonl.N` untouched, including archives copied from the
  historical journal. No automatic renaming/deleting of older archives.
  Old-style `.1`/`.2`/`.3` archives are preserved as-is; newly created
  suffixes continue beyond the highest existing one.
- Inventories and sums all numbered archives and the active journal on
  opening, validating regular-file and private permissions. The total
  history has a **1 GiB default hard budget**. `audit.OpenWithLimit` and
  `audit.OpenWithMigrationLimit` accept an explicit positive budget.
  `--audit-max-bytes=N` is available for `serve`, `start`, and Windows
  `start`; standalone saves the configured value and passes it to the
  server process, including subsequent updates/rollbacks via its recorded
  arguments. Existing history exceeding the budget refuses startup.
- When an append would exceed the budget, it fails with the distinct
  `audit_history_limit` error **before** rotation or writing, latches
  audit degradation, and prevents future new MCP calls. It never removes
  history to make room. The real outcome of already accepted mutations
  remains intact if their finish record can no longer fit. The budget
  covers the *new* audit family; retained duplicate old copies are not
  included. Increasing `--audit-max-bytes` after ensuring sufficient
  space and restarting provides controlled recovery without deletion.
- Live authenticated `/admin/runtime` and CLI `status` include the
  nonblocking `audit.bytes_used`/`audit.bytes_limit` alongside
  `ok/degraded/disabled` and the first error. Status also reports
  `stdout_log`/`stderr_log` only when the current process's descriptors
  actually refer to the stable diagnostic files; foreground processes
  without that redirection do not advertise misleading paths.
- **Diagnostic rotation policy:** stdout/stderr remain append-only while
  a server runs. Never rename open files expecting the writer to follow,
  and never use lossy copy-truncate. The supported operator procedure is
  to stop the service gracefully, verify that no server/update worker
  retains the old descriptors, rename/archive the now-closed files, then
  start again. No automatic diagnostic rotation is claimed. Historical
  logs and audit records are never silently pruned.

**Validation (isolated local macOS tests):**

- `make check`: **PASS** including all eight Go packages under `-race`,
  `go vet`, module verification, and binary build.
- `make audit-feedback`: **PASS**.
- `make lint`: **PASS**, zero issues.
- Windows amd64 application cross-compilation: **PASS**; native Windows
  runtime/ACL behavior is not validated by this macOS run.
- Audit retention and migration fault tests under `-race`: **PASS**,
  including preservation of more than three numbered archives; byte-exact
  budget accounting after restart and migration; explicit hard limit;
  refusal before rotation; rejection of symlinked archives; and previous
  short-write, truncate, lock and rotation recovery tests.
- An isolated detached server with an explicit custom history budget:
  **PASS**. It persisted the limit, reported live bytes in status,
  identified actual redirected stdout/stderr descriptors, and retained
  the journal on shutdown.
- End-to-end background start/update/rollback regressions in the full
  test suite: **PASS**, including migration and rollback after activation.
- Additional `-race` integration tests: **PASS**, validating that reaching
  the hard cap rejects a new MCP mutation, migrated archives count against
  the cap, and a customized audit budget persists through real isolated
  binary activation without changing the selected journal path.
- `git diff --cached --check`: **PASS**.

**Explicit limitations:** A successful JSONL append is **not** a per-event
`fsync` or guarantee of survival after a crash/power loss. The hard
budget cannot promise that finish records of already in-flight mutations
will fit once other activity exhausts storage; the actual receipt is not
rewritten as an execution failure. Local processes running as the same
OS user can modify files; external tamper-proof storage is out of scope.
Diagnostic files have no background rotation or automatic truncation.
Stage 7 is reserved for final cross-platform integration/release checks.


## 14. Stage 7 independent review and final integration checks (2026-10-26)

**Stages 0–7 completed and committed in source, subject to the platform release gates described below. This is not an installed-server update or a published release.** The final risk assessment, exact test matrix, and controlled rollout checklist are in [RELEASE_READINESS.md](RELEASE_READINESS.md).

An independent end-to-end review exposed and corrected three important failure modes:

1. **Audit failure with blocked stderr:** The first degradation diagnostic had been emitted while holding the audit sink mutex. The failure is now latched atomically before releasing the mutex, but its one best-effort `slog` diagnostic is written **after** unlock. Tests block the diagnostic handler and verify that health, refusal of new effects, `Close`, and repeated `Close` remain available.
2. **Completion observer panic after a real mutation:** The ledger now recovers a panic in `Observer.Finished` without crashing the MCP server, losing the true receipt, or duplicating an effect during replay. The MCP audit observer also latches `degraded` when its finish path unexpectedly panics, so further new tool calls are refused.
3. **Linux exited-process detection:** On Linux, `kill(pid, 0)` succeeds for an unreaped zombie even though the process has exited and released file descriptors. The updater/standalone shutdown logic now additionally detects Linux `/proc/PID/stat` zombie/dead states. A native-Linux regression test keeps a real child unreaped to reproduce this case. A chmod-based rollback fault test was also made explicitly conditional on running unprivileged, because root can override its injected permissions; an independent bad-restart rollback test still covers recovery as root.

**Validated in isolated environments:**

- macOS arm64 (Go 1.27.3): `make check` **PASS** (all eight packages under the race detector, modules, `go vet`, binary build); `make lint` **PASS** (zero issues); `make audit-feedback` **PASS**.
- macOS focused hardening: `go test -race -shuffle=on -count=20` for blocked audit diagnostics, completion-observer panic, update fences and shutdown **PASS**.
- Linux/amd64 userland under Docker emulation on an ARM64 host (Go 1.26.5, offline, source read-only, UID 65534): `go test -race -count=1 -timeout=240s ./...` **PASS** in all eight packages, including real-process updater and rollback tests. Root-run chmod injection was separately checked as a reported skip, not a false failure.
- Linux/amd64 and Windows/amd64 cross-compilation of relevant test binaries: **PASS**. `GOOS=linux go vet ./...` and `GOOS=windows go vet ./...`: **PASS**. Windows native runtime/ACL tests **not executed** on this Mac; they remain an explicit Windows release gate.
- Audit failure injection includes disk stalls, partial writes, retention budget exhaustion, replay/cancellation, migration conflicts, panic, blocked stderr and schema rejections. No Go module or external logging framework was added.

**Release recommendation:** The tested source is a candidate for a **controlled macOS/Linux rollout only with the owner's explicit permission**. Do not infer native Windows runtime readiness from cross-compilation. Do not push, publish or deploy automatically. The live MCP server's `server_id` is not changed by these stage commits. Expected residual limits remain: no per-event `fsync`/power-loss guarantee, no tamper-proof protection against the OS user, no automatic detached-process supervision, a fail-closed hard history budget, and offline-only diagnostic log rotation.
