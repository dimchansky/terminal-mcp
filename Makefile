.DEFAULT_GOAL := help

PYTHON ?= python3
GO ?= go

.PHONY: help build check install uninstall update status start stop logs logs-service

help:
	@printf '%s\n' 'make build        Build the Go binary (does not restart the service)' 'make check        Run all checks and build' 'make install      Register/start LaunchAgent (idempotent)' 'make uninstall    Unregister/stop LaunchAgent; preserve state and logs' 'make update       Verify, build, and safely update the running service' 'make status       Show service and update status' 'make start        Resume a registered, stopped service' 'make stop         Stop service without automatic restart' 'make logs        Follow detailed MCP tool audit events' 'make logs-service Tail service diagnostics'

build:
	@mkdir -p bin
	$(GO) build -trimpath -o bin/terminal-mcp ./cmd/terminal-mcp

check:
	./scripts/check.sh

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
