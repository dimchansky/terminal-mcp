# Instructions for AI development agents: terminal-mcp

This file records non-negotiable project requirements. Read it before changing
MCP tools, the audit middleware, logging, or security-related behavior.

## Owner-first threat model: complete MCP action audit

**The audit protects the owner of the computer from actions requested by LLMs
and other MCP clients. It does NOT protect the MCP client from the owner.**

The owner must be able to inspect exactly what the model asked the server to do.

- Preserve **all actual arguments and values** supplied in every authenticated,
  parseable MCP `tools/call`, captured **before tool-schema validation**.
  This includes fields later rejected by validation, unknown fields, and nested
  objects.
- **Do not redact, censor, mask, filter, hash, omit, summarize, or shorten
  values because they are sensitive.** This explicitly includes passwords,
  access tokens, API keys, signed/presigned URLs, credentials, private file
  content, `argv`, `env` overrides, `file_write.content`,
  `file_edit.edits`, `file_receive.file.download_url`, and terminal input.
  The more sensitive a model-supplied input is, the more important it is for
  the owner to see it in the audit.
- Preserve what was supplied; do not silently replace full arguments with
  metadata, sizes, hashes, or `[REDACTED]`. Normalize JSON whitespace or key
  ordering only as permitted by the audit format.
- If the **entire** audit input cannot be represented or durably appended
  within the supported resource limits, **reject the action before any effect**
  (fail-closed). Explicitly record why it was rejected; do not claim a full
  audit when only a partial record exists.
- Keep distinct `request_*` and `operation_*` lifecycle events and protect
  completed mutation receipts. Never cause a model action to run without the
  required pre-effect audit.
- Temporary sink failures (for example ENOSPC) must not permanently disable
  the server: retry the next required pre-effect write and admit work only
  after that complete write succeeds. Repair an incomplete JSONL tail before
  any subsequent append. Invariant failures remain fail-closed; recovery must
  not invent missing completion events or erase historical audit records.
- The audit files are private from *other operating-system users* via
  permissions/ACLs. This is not an excuse to hide entries from their owner.
  The audit is not tamper-proof against local code with the owner's privileges.
- Scope: tool-call input audit begins at authenticated, parseable
  `tools/call` requests. It is not a raw HTTP packet capture, an inherited
  process-environment snapshot, or a copy of child stdout/stderr, which have
  their own streams. Never imply otherwise.

**Do not apply generic secret-redaction advice to this audit.** If a new
feature includes any field that might be sensitive, explicitly test that the
exact value survives in the owner's audit, including on schema rejection.
Changing this policy requires an explicit decision by the project owner, not
an autonomous LLM security judgment.

See `SPEC.md` (Audit log) and `docs/LOGGING_AND_AUDIT_DESIGN.md` (Purpose,
scope, and trust boundary) for the full behavioral contract.

## Provider-neutral network capability

This MCP server already permits arbitrary process execution with the owner's
OS permissions. It is **not a sandbox**. In particular, `file_receive` must
accept HTTP and HTTPS URLs from **any** host, network, or provider, including
localhost, private addresses, nonstandard ports and redirects. Do not introduce
domain/IP allowlists, OpenAI-only restrictions or HTTPS-only policies based
on generic SSRF advice. Such restrictions do not establish a meaningful trust
boundary for a client that can use `terminal_start`; they only break valid
user workflows. Introduce any new source restriction only at the owner's
explicit request. Keep necessary transport validation, bounded resource use,
atomic file publication, retry semantics and the **full owner-visible audit**.

## Async downloads and lifecycle invariants

`file_receive` must begin downloads independently of request lifetime after
the required pre-effect audit. Both `file_receive` and `file_receive_result`
return the same progress or final receipt. Optional `wait_ms` governs only
observation: default 2000 for receive, 20000 for result, 0 for an immediate
snapshot, maximum 20000. A progress update must not wake a result wait;
only effect completion or the observation deadline does. Varying `wait_ms`
never changes retry identity or starts another download. Keep snapshots
only in the current server's memory; do not persist them across restarts.
Never invent total size or percentage when Content-Length is unknown.
Do not impose an arbitrary overall download timeout or hard-coded size cap.

When an optional embedded Secure MCP Tunnel is enabled, every remote tool
call must traverse the *same* owner-visible pre-effect audit, shared session
manager and admission gate as local HTTP. Never let updater preflight start
another tunnel client with a production tunnel ID. Treat credentials as
privileged user data, but do not misrepresent same-user shell access as a
sandbox. The local HTTP MCP must work when the remote tunnel is offline.

Normal `stop`, `update`, SIGINT and SIGTERM must first atomically close
admission for new work, then wait for already accepted mutations, downloads,
managed processes and audit completion. Maintain status/controls for existing
sessions during draining. Only an explicit operator-requested `stop --force`
may abort active transfers or managed processes. Any updater must work with
the installed previous revision, not just with itself.

## Development invariants

Favor idiomatic Go, minimal sufficient complexity, explicit errors, reproducible
tests (including races and failures), and no unrequested architectural layers.
A running standalone server is distinct from the development binary; source
changes, Git push, and installed-server updates are separate operations.
Use the current `server_id` and unique `operation_id` for MCP mutations.
Never silently alter production processes, delete user data, or discard audit
history during testing or cleanup.
