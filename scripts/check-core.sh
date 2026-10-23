#!/bin/sh
# Deliberately tests only standard-library packages; not an MCP/PTY build substitute.
set -eu
cd "$(dirname "$0")/.."
root=$(pwd)
for package in session httpguard; do
  (
    cd "$root/internal/$package"
    GO111MODULE=off GOTOOLCHAIN=local go test -race -count=3 -timeout=90s -v
    GO111MODULE=off GOTOOLCHAIN=local go vet
  )
done
