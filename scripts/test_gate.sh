#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

cmd_pkg="github.com/makewand/makewand/cmd/makewand"

echo "[test-gate] go test ./cmd/makewand -run ^TestE2E"
go test ./cmd/makewand -count=1 -run '^TestE2E' -v

other_pkgs="$(go list ./... | grep -Fxv "${cmd_pkg}" || true)"
non_e2e_tests="$(
  go test ./cmd/makewand -list '^Test' 2>/dev/null \
    | awk '/^Test/ && $0 !~ /^TestE2E/ { print }' \
    | paste -sd'|' -
)"

echo "[test-gate] go test other packages"
if [[ -n "${other_pkgs}" ]]; then
  go test ${other_pkgs}
fi

if [[ -n "${non_e2e_tests}" ]]; then
  echo "[test-gate] go test ./cmd/makewand (non-E2E)"
  go test ./cmd/makewand -count=1 -run "^(${non_e2e_tests})$"
fi
