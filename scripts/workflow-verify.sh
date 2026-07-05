#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

echo "== workflow verify: build =="
go build ./...

echo "== workflow verify: vet =="
go vet ./...

echo "== workflow verify: test =="
go test ./...

echo "== workflow verify: race (workflow packages) =="
go test -race ./internal/workflow/... ./cmd/...

echo "workflow verify passed"