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

## Development invariants

Favor idiomatic Go, minimal sufficient complexity, explicit errors, reproducible
tests (including races and failures), and no unrequested architectural layers.
A running standalone server is distinct from the development binary; source
changes, Git push, and installed-server updates are separate operations.
Use the current `server_id` and unique `operation_id` for MCP mutations.
Never silently alter production processes, delete user data, or discard audit
history during testing or cleanup.
