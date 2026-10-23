#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
files=$(find cmd internal -type f -name '*.go' -exec gofmt -l {} +)
if [ -n "$files" ]; then
  printf 'Unformatted Go files:\n%s\n' "$files" >&2
  exit 1
fi
go mod verify
go test -race -count=1 -timeout=120s ./...
go vet ./...
python3 -m unittest discover -s scripts -p "test_*.py"
mkdir -p bin
go build -trimpath -o bin/terminal-mcp ./cmd/terminal-mcp
