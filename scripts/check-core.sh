#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
go test -race -count=3 -timeout=120s ./internal/session ./internal/httpguard ./internal/filesystem ./internal/mutation
go vet ./internal/session ./internal/httpguard ./internal/filesystem ./internal/mutation
