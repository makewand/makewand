#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "[test-gate] go test ./..."
go test -count=1 ./...

echo ""
echo "[test-gate] go vet ./..."
go vet ./...
