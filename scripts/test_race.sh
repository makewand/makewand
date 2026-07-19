#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "[race] Testing all packages with the race detector"
echo ""

# Use go list ./... as the single source of truth so concurrency-heavy
# packages (router, server*, internal/remotesession, ...) can never silently
# drop out of race coverage when packages are added or renamed.
mapfile -t pkgs < <(go list ./...)

echo "[race] Packages under test:"
printf '  %s\n' "${pkgs[@]}"
echo ""

echo "[race] go test -race ${#pkgs[@]} packages"
go test -race "${pkgs[@]}"

echo ""
echo "[race] All race tests passed ✓"
