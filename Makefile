.DEFAULT_GOAL := help

PYTHON ?= python3
GO ?= go
GOFMT ?= gofmt

.PHONY: help build check check-core fmt-check test-go test-service install uninstall update status start stop logs logs-service

help:
	@printf '%s\n' 'make build        Build the Go binary (does not restart the service)' 'make check        Run all checks and build' 'make check-core  Repeat core Go race tests three times' 'make install      Register/start LaunchAgent (idempotent)' 'make uninstall    Unregister/stop LaunchAgent; preserve state and logs' 'make update       Verify, build, and safely update the running service' 'make status       Show service and update status' 'make start        Resume a registered, stopped service' 'make stop         Stop service without automatic restart' 'make logs        Follow detailed MCP tool audit events' 'make logs-service Tail service diagnostics'

build:
	@mkdir -p bin
	$(GO) build -trimpath -o bin/terminal-mcp ./cmd/terminal-mcp

fmt-check:
	@files="$$(find cmd internal -type f -name '*.go' -exec $(GOFMT) -l {} +)" || exit 1; \
	if [ -n "$$files" ]; then printf 'Unformatted Go files:\n%s\n' "$$files" >&2; exit 1; fi

test-go:
	$(GO) mod verify
	$(GO) test -race -count=1 -timeout=120s ./...
	$(GO) vet ./...

test-service:
	$(PYTHON) -m unittest discover -s scripts -p 'test_*.py'

check-core:
	$(GO) test -race -count=3 -timeout=120s ./internal/session ./internal/httpguard ./internal/filesystem ./internal/mutation
	$(GO) vet ./internal/session ./internal/httpguard ./internal/filesystem ./internal/mutation

check:
	+@$(MAKE) fmt-check
	+@$(MAKE) test-go
	+@$(MAKE) test-service
	+@$(MAKE) build

install:
	$(PYTHON) scripts/service.py install

uninstall:
	$(PYTHON) scripts/service.py uninstall

update:
	$(PYTHON) scripts/service.py update

status:
	$(PYTHON) scripts/service.py status

start:
	$(PYTHON) scripts/service.py start

stop:
	$(PYTHON) scripts/service.py stop

logs:
	@tail -n 100 -F "$$HOME/Library/Application Support/LocalTerminalMCP/audit.jsonl"

logs-service:
	@tail -n 100 -F "$$HOME/Library/Application Support/LocalTerminalMCP/stderr.log"
