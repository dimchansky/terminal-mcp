.DEFAULT_GOAL := help

GO ?= go
GOFMT ?= gofmt
# Integration tests build, activate, and roll back real binaries. This timeout
# bounds the complete package suite, not each individual test.
TEST_TIMEOUT ?= 300s
GOLANGCI_LINT ?= golangci-lint
GOVULNCHECK ?= $(GO) run golang.org/x/vuln/cmd/govulncheck@v1.9.0

BIN ?= ./bin/terminal-mcp

AUDIT_LOG ?= $(HOME)/.config/terminal-mcp/logs/audit.jsonl
STDERR_LOG ?= $(HOME)/.config/terminal-mcp/logs/server.stderr.log

.PHONY: help build check check-core audit-feedback fmt-check test-go lint vulncheck start stop status update update-status cleanup cleanup-apply logs logs-service

help:
	@printf '%s\n' 'make build         Build the Go binary (does not restart the server)' 'make check         Run all checks and build' 'make check-core    Repeat core Go race tests three times' 'make audit-feedback Verify MCP responses against JSONL audit' 'make lint          Run configured Go linters (requires golangci-lint v2)' 'make vulncheck     Scan known reachable Go vulnerabilities' 'make start         Start installed server; first run installs this build' 'make stop          Stop the standalone server when idle' 'make status        Query installed server and its revision' 'make update        Check current source build and update installed server' 'make update-status Show latest self-update result' 'make cleanup       Preview safe cleanup of completed update artifacts' 'make cleanup-apply  Remove safe completed update binaries (never audit/logs/backups)' 'make logs          Follow the MCP audit journal' 'make logs-service  Follow the stable standalone server stderr (including updates)'

build:
	@mkdir -p bin
	$(GO) build -trimpath -o $(BIN) ./cmd/terminal-mcp

fmt-check:
	@files="$$(find cmd internal -type f -name '*.go' -exec $(GOFMT) -l {} +)" || exit 1; \
	if [ -n "$$files" ]; then printf 'Unformatted Go files:\n%s\n' "$$files" >&2; exit 1; fi

test-go:
	$(GO) mod verify
	$(GO) mod tidy -diff
	$(GO) test -race -count=1 -timeout=$(TEST_TIMEOUT) ./...
	$(GO) vet ./...

lint:
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run ./...

vulncheck:
	$(GOVULNCHECK) ./...

audit-feedback:
	$(GO) test -race -count=1 -run '^TestMCPAuditFeedbackLoop$$' ./cmd/terminal-mcp

check-core:
	$(GO) test -race -count=3 -timeout=120s ./internal/session ./internal/httpguard ./internal/filesystem ./internal/mutation
	$(GO) vet ./internal/session ./internal/httpguard ./internal/filesystem ./internal/mutation

check:
	+@$(MAKE) fmt-check
	+@$(MAKE) test-go
	+@$(MAKE) build

$(BIN):
	@$(MAKE) build

start: $(BIN)
	"$(BIN)" start

stop: $(BIN)
	"$(BIN)" stop

status: $(BIN)
	"$(BIN)" status

update: check
	"$(BIN)" update --candidate="$(abspath $(BIN))"

update-status: $(BIN)
	"$(BIN)" update-status

cleanup: $(BIN)
	"$(BIN)" cleanup --dry-run

cleanup-apply: $(BIN)
	"$(BIN)" cleanup --apply

logs:
	@tail -n 100 -F "$(AUDIT_LOG)"

logs-service:
	@tail -n 100 -F "$(STDERR_LOG)"
