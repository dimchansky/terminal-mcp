.DEFAULT_GOAL := help

GO ?= go
GOFMT ?= gofmt
GOLANGCI_LINT ?= golangci-lint
GOVULNCHECK ?= $(GO) run golang.org/x/vuln/cmd/govulncheck@v1.9.0

STANDALONE ?= $(HOME)/.config/terminal-mcp/standalone/terminal-mcp

AUDIT_LOG ?= $(HOME)/.config/terminal-mcp/logs/audit.jsonl
STDERR_LOG ?= $(HOME)/.config/terminal-mcp/logs/server.stderr.log

.PHONY: help build check check-core audit-feedback fmt-check test-go lint vulncheck start stop status update update-status logs logs-service

help:
	@printf '%s\n' 'make build         Build the Go binary (does not restart the server)' 'make check         Run all checks and build' 'make check-core    Repeat core Go race tests three times' 'make audit-feedback Verify MCP responses against JSONL audit' 'make lint          Run configured Go linters (requires golangci-lint v2)' 'make vulncheck     Scan known reachable Go vulnerabilities' 'make start         Build and start the standalone server in the background' 'make stop          Stop the standalone server when idle' 'make status        Query the standalone server' 'make update        Test, build a separate candidate, and queue self-update' 'make update-status Show latest self-update result' 'make logs          Follow the MCP audit journal' 'make logs-service  Follow the stable standalone server stderr (including updates)'

build:
	@mkdir -p bin
	$(GO) build -trimpath -o bin/terminal-mcp ./cmd/terminal-mcp

fmt-check:
	@files="$$(find cmd internal -type f -name '*.go' -exec $(GOFMT) -l {} +)" || exit 1; \
	if [ -n "$$files" ]; then printf 'Unformatted Go files:\n%s\n' "$$files" >&2; exit 1; fi

test-go:
	$(GO) mod verify
	$(GO) mod tidy -diff
	$(GO) test -race -count=1 -timeout=120s ./...
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

start: build
	./bin/terminal-mcp start

stop:
	"$(STANDALONE)" stop

status:
	"$(STANDALONE)" status

update: check
	$(GO) build -trimpath -o bin/terminal-mcp.next ./cmd/terminal-mcp
	"$(STANDALONE)" update --candidate="$(CURDIR)/bin/terminal-mcp.next"

update-status:
	"$(STANDALONE)" update-status

logs:
	@tail -n 100 -F "$(AUDIT_LOG)"

logs-service:
	@tail -n 100 -F "$(STDERR_LOG)"
