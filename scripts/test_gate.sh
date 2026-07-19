#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

# Use go list ./... as the single source of truth for all packages.
all_pkgs="$(go list ./...)"
cmd_pkg="github.com/makewand/makewand/cmd/makewand"

echo "[test-gate] All packages to test:"
echo "${all_pkgs}" | sort

echo ""
echo "[test-gate] go test ./cmd/makewand -run ^TestE2E"
go test ./cmd/makewand -count=1 -run '^TestE2E' -v

# All packages except cmd/makewand (which is tested separately for E2E)
other_pkgs="$(echo "${all_pkgs}" | grep -Fxv "${cmd_pkg}" || true)"

non_e2e_tests="$(
  go test ./cmd/makewand -list '^Test' 2>/dev/null \
    | awk '/^Test/ && $0 !~ /^TestE2E/ { print }' \
    | paste -sd'|' -
)"

echo "[test-gate] go test all packages (except cmd/makewand)"
if [[ -n "${other_pkgs}" ]]; then
  go test ${other_pkgs}
fi

if [[ -n "${non_e2e_tests}" ]]; then
  echo "[test-gate] go test ./cmd/makewand (non-E2E)"
  go test ./cmd/makewand -count=1 -run "^(${non_e2e_tests})$"
fi

echo ""
echo "[test-gate] go vet ./..."
go vet ./...

echo ""
echo "[test-gate] All checks passed ✓"
