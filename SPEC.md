# Local Terminal MCP — Specification and Architecture

Status: implementation contract for the accompanying source, version 0.1.0.
Target: Linux and macOS. Single local OS user / single trusted principal.

## 1. Goal and boundary

Give an MCP client reliable, incremental control over real processes and interactive
terminal sessions: start, observe, type, interrupt, resize, and terminate. A process
must not depend on the lifetime of an individual RPC or HTTP connection.

This is a process manager, not a shell-command allowlist, an SSH replacement, or a
browser terminal UI. Commands run with the server user's full privileges. MCP is
only the control interface. Neither tool annotations nor authentication sandbox a
process. Output is untrusted program data, not instructions to the model.

The minimum complete implementation includes a real PTY, a rendered text screen,
separate pipe streams, non-consuming cursor reads, bounded output, bounded waits,
input retry protection, and independent interruption. Omitting any of these would
leave an important failure mode in the proposed workflow unresolved.

Deliberately excluded: Windows/ConPTY, multi-tenant authorization, OAuth issuance,
persistent disk logging, restoring live terminals after the server dies, native
SSH/serial backends, a browser UI, screen diffs, shell-specific command tracking,
MCP Tasks, progress-dependent correctness, and unsolicited event delivery. Native
`ssh` can run inside a PTY, but signal scope remains local. These are not required
to make the local control loop work, and each adds its own lifecycle or trust model.

## 2. Transport decision

**Default: independently launched HTTP server bound to a literal loopback IP.**
The official Go SDK supplies MCP Streamable HTTP at `/mcp`. The handler is stateless
at the MCP transport level; the manager retains application sessions independently.

Why HTTP: a tunnel or client can restart without taking ownership of the terminal
server process. This is a lifecycle advantage, not a richer terminal protocol.
The service must be kept running by the user, launchd, or systemd; it does not
self-daemonize. No custom relay or custom JSON-RPC stack is necessary.

An optional `--transport=stdio` uses the same tools and manager. It is convenient
for desktop MCP hosts, but EOF/cancellation of that server process shuts down its
sessions. It does **not** promise survival of a tunnel that launches it and then
exits. Server diagnostics go only to stderr in this mode.

There is no guarantee that a particular ChatGPT plan exposes the tools, that a
client renders notifications, or that a model can issue a concurrent interruption.
Those are client capabilities. All essential behavior uses ordinary, bounded tool
calls. A local operator can always stop this server outside the MCP client.

## 3. Runtime layout

One executable, not a collection of microservices:

```
MCP HTTP or stdio adapter
          |
          v
Session manager + mutation ledger
          |
          +-- pipes: stdin, stdout, stderr; direct process exit status
          |
          +-- PTY: terminal byte stream; keyboard; size; text screen
                       |
                 persistent reader(s)
                       |
                bounded output journal
```

The `session` package owns lifecycle, buffers, retry semantics and waits using only
Go's standard library. The `terminal` package implements the actual PTY/screen
boundary using `creack/pty` and `vt10x`. A small interface is justified by the two
real execution modes, not hypothetical future platforms. The official MCP SDK
handles wire protocol negotiation and schema generation.

## 4. Identity, lifecycle and guarantees

Each server start generates a cryptographically random `server_id`. Each session
has a random `session_id`. `terminal_list` returns the server ID, session summaries
and limits. Mutation calls require the current server ID. A stale ID is rejected
**before** any effect. Clients must not automatically replace a stale server ID
and retry an old operation: its previous outcome may be unknowable.

A session owns the directly spawned process and its output handles. Pipe processes
start in their own process group. PTYs start with a controlling terminal in their
own session. The application session is independent of every MCP connection.

Public process states are `running` and `exited`. `running` means that termination
has not been observed; it does not prove CPU activity, progress, or absence of a
job-control stop. A stopped process remains controllable with `CONT`. `exit`
contains either an exit code or a terminating signal. The shell's exit code is
not the exit code of the last command entered into an interactive shell.

`output_done` is separate from process exit. Readers drain remaining output after
exit. Descendants can inherit descriptors; after a bounded drain grace the server
closes them and marks `output_incomplete`. It never claims that a timed-out drain
was a complete transcript. `terminal_close` closes the session's remaining I/O.

A connection loss, cancellation of a read, or client restart does not stop the
process. A graceful server shutdown terminates managed sessions. A server crash or
machine reboot loses in-memory state and may leave descendants running. This is
not crash recovery, a cgroup supervisor, or a universal process-tree killer.
Processes that create new sessions/groups can escape the original group. Even a
descendant in the original group may survive if it ignores TERM and outlives the
leader: the close operation is bounded best-effort cleanup, not containment. The
server will not send signals to remembered numeric groups after it has observed
the leader exit; PID/process-group identity is not a durable capability.

Completed sessions are retained subject to a time and count limit; active sessions
are never silently evicted. A retry of an accepted mutation still returns its original receipt even if the
session has expired. In particular, a successful start must never spawn again.
An expired observation is reported separately from the retained receipt.

## 5. Execution modes

### 5.1 Pipes

`mode: "pipes"` is the default for builds, tests and scripts. stdout and stderr are
separate labeled channels. Their cross-stream order is the order in which the
server observed chunks, not a claim about the program's exact write chronology.
A pipe is not a terminal; interactive programs may buffer or refuse to run.

### 5.2 PTY

`mode: "pty"` provides one `terminal` output channel, a controlling terminal, named
keys, resize, and a virtual terminal screen. It does not falsely split terminal
stdout/stderr. Text snapshots expose rows, columns, cursor position and alternate
screen state. Raw events retain control bytes.

A text snapshot is a rendering, not ANSI-stripped logs. Repositioning, erasure and
alternate-screen operations change the virtual screen. Screen snapshots are not
an image protocol, a complete modern terminal conformance claim, or support for
Kitty graphics, sixel, every Unicode grapheme, or all mouse/keyboard extensions.
The terminal emulator is reused rather than implemented from scratch. Input can
always send explicit UTF-8 text or exact base64 bytes when a named key is absent.

The PTY master is wrapped as a nonblocking, poller-owned descriptor. This is
necessary for write deadlines and interruptible close. Resize uses `SyscallConn`
and an ioctl rather than a helper that can call `File.Fd` and restore blocking mode.
The screen decoder handles split UTF-8; a recoverable emulator failure is visible
as a screen error/diagnostic while raw events remain authoritative.

Terminal-generated protocol replies use a bounded queue and the same serialized
input path as user input. They must not block the output reader indefinitely.
Dropped/failed replies appear as diagnostics rather than silently hanging a reader.

### 5.3 Program invocation and environment

The server executes `argv` without interpreting shell syntax. To use a shell,
explicitly start it, for example `["/bin/sh", "-lc", "make && make test"]`.
Executable lookup uses the effective child PATH. Relative executable paths and
relative PATH entries are resolved against the requested working directory.

The server environment is inherited with explicit `env` overrides. Its own HTTP
authentication variable is removed from the default child environment to reduce
accidental leakage, not as a security boundary. There is no automatic `sudo`.

## 6. Tool contract

All tools validate lengths, modes, limits and combinations of fields before acting.
Unknown IDs do not act. Runtime action validation failures are recorded in receipts;
malformed requests and stale server IDs can fail before reservation. Action failures return structured receipts;
a failed program itself is represented by `exit`, not an MCP transport error.

### `terminal_start`

Fields: `server_id`, `operation_id`, `argv`, optional `cwd`, `env`, `mode`, `rows`,
`cols`, `wait_ms`, `max_bytes`, `max_runtime_ms`.

Creates the process once and returns a mutation receipt plus an initial observation.
The process is not created with the RPC's context. `max_runtime_ms` is an optional
independent process lifetime limit. Zero means no automatic lifetime limit.

### `terminal_read`

Fields: `session_id`, optional `cursor`, `view` (`events` or `screen`), `wait_ms`,
`max_bytes`, `until` (literal substring).

Reads retained output without consuming it. An omitted cursor means the beginning
of the journal, so any evicted prefix is reported. Returned cursors are opaque and
bound to the session. A cursor beyond the journal end or for another session is
rejected rather than silently clamped.

With no `until`, return as soon as output, an exit, or a gap is observable. With
`until`, wait until the literal appears in the bounded returned window, output is
full, a gap/EOF is encountered, or the absolute deadline expires. The explicit
`return_reason` prevents treating a full window as a successful match. Literal
matching avoids adding regex complexity or matching arbitrarily large logs.
In screen mode matching is against the rendered screen; it does not advance the
raw event cursor. Screen snapshots return the journal's separate current `end_cursor`.

### `terminal_write`

Fields: `server_id`, `operation_id`, `session_id`, optional `input` (ordered items),
`close_stdin`, `cursor`, `wait_ms`, `max_bytes`.

Each input item has exactly one of `text`, `bytes_base64`, or `key`. Empty input is
allowed only with `close_stdin`. Text is sent verbatim; no Enter is appended.
Named keys are for PTY sessions. Closing stdin is for pipe sessions; Ctrl+D is a
terminal character, not a general promise of EOF.

The write is bounded independently of read wait. A receipt records bytes accepted
by the OS and any partial-write error. Accepted bytes are not proof that the
application consumed or executed them. A retry never replays the remaining suffix.
The caller must inspect the receipt before deciding on a new operation.

A write may include a short observation wait, eliminating a redundant round trip
without inventing a separate `interact` tool. The observation cursor defaults to
the position captured before the first attempt's input. A replay can return a
fresh observation but the original mutation receipt.

### `terminal_signal`

Fields: `server_id`, `operation_id`, `session_id`, `signal`, optional `target`.

Signals: `INT`, `TERM`, `KILL`, `HUP`, `CONT`, `STOP`. Targets: `process`, `group`
(default), or `foreground` (PTY only). The target is always local. Foreground
signaling obtains the current foreground group from the terminal; it is not an
arbitrary caller-supplied PID. Signal acceptance is reported independently of
whether the process has actually exited.

A named `CTRL_C` key writes a terminal character; a signal explicitly asks the OS
to signal a managed target. They are not interchangeable in raw mode or across SSH.

### `terminal_resize`

Fields: `server_id`, `operation_id`, `session_id`, `rows`, `cols`.

Changes PTY dimensions and emulator dimensions. Non-PTY sessions reject it.
This is an explicit assignment; retries do not apply a stale resize after a newer
one because mutations are deduplicated by operation ID.

### `terminal_list`

No required arguments. By default (`include_closed: false`), returns server
identity, limits, counts for all retained sessions, and summaries of unclosed
sessions. Set `include_closed: true` to include retained closed sessions for
inspection. This filter does not delete data or change expiration; closed
sessions remain readable by session_id within retention limits. `counts.running`
counts processes without observed exit (including closure in progress),
`counts.exited_unclosed` counts exited sessions not manually/automatically closed,
and `counts.closed_retained` counts retained closed sessions. Counts describe all
retained sessions regardless of the filter; while closure is in progress, running
and closed-retained categories can overlap. It is the bootstrap/recovery operation,
not a destructive attach.
Summaries omit argv/cwd to bound the list response even when processes were started
with large arguments. A read returns full launch metadata; CWD and argv describe
the launched process, not dynamically discovered shell state.

### `terminal_close`

Fields: `server_id`, `operation_id`, `session_id`, optional `grace_ms`.

Stops accepting input, requests TERM for managed groups (including the current
PTY foreground group where applicable), requests CONT so stopped jobs can react,
waits a bounded grace, then uses KILL if necessary. It closes remaining handles.
No implicit Ctrl+D is sent: in a raw application that can be ordinary data.

Closing a completed session is harmless. Closing retains bounded output and exit
status; it does not mean "forget". The result reports actual observed state,
including failure to observe termination within the bounded wait. Graceful close
can leave descendants behind if the leader exits before escalation, as described
in the process-group limitations above.

## 7. Output journal and observations

Readers run continuously, independently of tool calls. Retained chunks are bounded
by both total bytes and event count, so one-byte writes cannot bypass the memory
budget through per-event overhead. Positions are monotonically increasing byte
positions across observed stream chunks. Each returned event includes stream,
position, and either valid UTF-8 `text` or exact `bytes_base64`.

Cursor slicing is byte-exact. A UTF-8 sequence split across a cursor or I/O chunk
may be returned as base64; no bytes are silently replaced in the authoritative
journal. This is preferable to pretending that every arbitrary byte boundary is a
Unicode boundary. The separate screen decoder carries incomplete UTF-8 sequences
across PTY reads.

Every observation includes `next_cursor`, `end_cursor`, `has_more`, optional `gap`,
process state, `output_done`, `output_incomplete`, diagnostics, and `return_reason`.
A gap gives the exact unavailable byte range. Two readers can use independent
cursors without interfering. Repeated reads of retained byte ranges reproduce the
same bytes (though process metadata can advance).

Screen observations use a monotonically increasing revision. Screen rendering does
not consume raw output. No diff protocol or hidden reader cursor exists in v0.1.

## 8. Time, concurrency and cancellation

Three distinct clocks exist: RPC observation wait, optional process runtime, and
completed-session retention. Lack of polling is not an idle-process kill policy.

Observation waits have a fixed absolute deadline: more output never extends it.
Returning because the wait ended does not imply command completion. Events wake
readers through a change-notification channel; no lock is held while waiting.

Input serializes within a session. A blocked input operation has its own short
write deadline. Signals and close do not acquire the input lock or wait for a
reader. Session state locks protect short copies/updates, not external I/O.

RPC cancellation stops waiting for an observation or mutation receipt. Once a
mutation is accepted, its bounded action runs independently so that a retry can
recover the receipt. A client losing an answer must reuse the same operation ID.

## 9. Retry semantics and finite resources

Every mutation requires a bounded, nonempty `operation_id`. The server reserves
that ID before performing the action. Its fingerprint includes the method and
normalized effect arguments, excluding purely observational wait/cursor fields.
Concurrent duplicates share one action. A reused ID with different effect arguments
is an error. Failed and partial actions are recorded too.

This is **at-most-once mutation submission per server instance**, not exactly-once
business execution. External effects, crashes and client abandonment cannot be
rolled back. A crash invalidates the server ID; the server does not guess outcomes.

The ledger stores small receipts, not full logs or input payloads. Stored error
messages are capped at 1024 bytes plus an explicit truncation marker, and session
IDs are validated before reserving mutations; a failed action cannot retain an
arbitrarily large error or ID per ledger entry. Input payloads
are hashed and released after the action completes. Ledger entries are not silently
evicted: at capacity, new mutations fail before acting. This finite bound trades
unlimited uptime for an honest retry guarantee. Read/list remain available; the
operator can stop the server locally. Perform a controlled restart after closing
sessions to reset the ledger.

Defaults and hard limits are returned by `terminal_list` and defined centrally in
source: bounded active/retained session counts, a per-session output budget, an
event-count cap, input and response sizes, terminal dimensions, waits, and ledger
entries. No unbounded disk spool or unlimited audit history is hidden behind a
claim of bounded memory.

## 10. HTTP security

The HTTP mode requires a long bearer token, supplied via a protected token file or
`TERMINAL_MCP_TOKEN`. It rejects non-loopback bind addresses, unexpected Host values,
all Origin headers (this is not a browser-facing API), missing/incorrect tokens,
and oversized request bodies. No CORS is enabled. A tunnel/proxy must authenticate
to the local endpoint and preserve the expected local Host behavior.

One credential represents one fully trusted local principal; every authorized
client can control every managed session. IDs are not authorization secrets. This
is intentionally not a multi-user service. Never publish this endpoint through a
public tunnel without an authenticated upstream access policy. A bearer token is
not an OAuth authorization server.

Tool annotations describe actual effects: reads/list are read-only; start, write,
signal, resize and close are not. Local privileges and client policy remain
separate. No command-content policy pretends to sandbox a general-purpose shell.

## 11. Shutdown and operational behavior

HTTP server and session manager lifetimes are independent from requests. On process
SIGINT/SIGTERM the manager rejects new starts, completes accepted starts, then
closes sessions with bounded parallel cleanup. HTTP shutdown also has a deadline.
The command does not intercept user-program control characters as server signals.

The stdio server exits when its transport ends and performs the same cleanup.
Diagnostics do not contain input, output, tokens, or full environment values.
In HTTP mode a bounded metadata-only audit is created automatically under a
private user state directory. Stdout/stderr are not persisted by this audit;
program output can contain secrets.

### Audit log: operational guarantees and boundaries

All dispatched MCP tool callbacks write a start record before executing their
function, and a finish record with status and elapsed time afterward. A generated
request_id links the two. Tool name and (where available) generated session_id
are the only operation metadata: arguments, credentials, file contents and command
output are never serialized into audit events. Successful mutations whose *finish*
record fails are not changed into retryable errors. A *start* logging failure
rejects execution. This is not a forensic-grade tamper-proof log or an identity
system; rejected authentication, invalid tool schemas and MCP discovery requests
are outside this initial audit. A single shared bearer token identifies no
individual caller. JSONL writes are synchronous under a mutex, with up to four
4 MiB files (active and three rotated archives), mode 0600, and no fsync on each
call. Crash loss of the most recent entries remains possible. The preflight
candidate disables auditing so probe calls do not pollute the production log.

In macOS HTTP mode, the optional file_root key in the existing user-private
config.json sets the file-access root, with --file-root taking precedence. The
controller does not hand-edit the launchd plist; the new root is read on start,
validated during preflight, and activated by a managed update. It does not change
shell process permissions or sandbox them.

## 12. Verification requirements

Tests cover journal cursors/gaps/binary data, two independent readers, absolute
wait deadlines under continuous output, cancellation without process death,
start/input retry deduplication and conflict, stale server IDs, partial input,
foreground keyboard interruption, resize/snapshot behavior, process exit/drain,
resource limits, HTTP trust checks, and MCP discovery/calls with the official SDK.

Run `go test -race ./...`, `go vet ./...`, and compile for Linux/macOS. Tests requiring
a real PTY run only on those platforms. `TEST_REPORT.md` must distinguish actual
executed checks from tests merely supplied in the archive. A cross-compile is not
a macOS runtime test; an SDK transport test is not a ChatGPT/tunnel integration test.

## 13. Primary references

Dependency APIs are pinned in `go.mod`; updating them requires rerunning tests.

- Official MCP Go SDK: https://github.com/modelcontextprotocol/go-sdk
- Go process API: https://pkg.go.dev/os/exec
- PTY implementation: https://github.com/creack/pty
- Virtual terminal API: https://pkg.go.dev/github.com/hinshun/vt10x
- MCP transport specification: https://modelcontextprotocol.io/specification
- Secure MCP Tunnel limits: https://developers.openai.com/api/docs/guides/secure-mcp-tunnels

The last reference documents a 120-second request budget, not a process lifetime.
This design uses much shorter waits and does not depend on those limits remaining
unchanged. No particular hosted-client behavior is assumed by the core contract.


## 14. Version 0.2: files, completion and supervised updates

This section extends the earlier seven-terminal-tool contract. Detailed argument
limits and operations are in [DEVELOPMENT.md](docs/DEVELOPMENT.md).

`close_on_exit` defaults false and releases I/O after leader exit and bounded output
drain. It never discards retained output, sends stale-PID signals or consumes another
mutation ID. It shares the single-close primitive with manual close. The `closed`
flag is not proof of a resource leak: completed-session retention already worked
independently of it. `wait_for=completion` waits for exit, drained output and requested
auto-close; deadlines, gaps and size caps still return earlier. `until` cannot be
combined with completion. Wait mode is observational and excluded from retry identity;
auto-close changes lifetime and is included. No duplicate terminal_exec API is needed.

Five file tools use one os.Root and absolute paths, no mutable global cwd. Reads
return whole-file SHA-256 versions. Edits require exact unique, non-overlapping text
against that version. Writes are atomic per file, create-only without an expected
version, and guarded replacements otherwise. Normal permissions are preserved, not
ACLs or xattrs. Linked, non-owned, special and read-only targets are refused. This
is optimistic conflict detection, not a cross-file transaction or universal CAS.
A shared mutation ledger fences both process and file writes. Search uses rg for
ignore-aware discovery but opens content through os.Root with bounded output/time.

An independent launchd job owns deployment because a process cannot reliably finish
its own restart. Immutable releases and an atomic selection symlink separate builds
from the live executable. Preflight runs full tests and starts the candidate on a
separate port. An authenticated /admin/maintenance endpoint applies the same bearer,
Host and Origin checks as /mcp and freezes new mutations only when the manager is
idle. Generation-fenced 60-second expiry avoids permanent freeze after updater death.
A new server ID and exact candidate metadata are required before reporting success;
otherwise restore the verified previous release. Manual stop wins over restart.

This single-process design has a brief restart outage and loses in-memory session
and retry state; it does not migrate active PTYs. Busy updates must not terminate
useful work. Legacy adoption lacks the fence and requires one explicit idle window.
Tool metadata and instructions determine Refresh tools, not binary version alone.
