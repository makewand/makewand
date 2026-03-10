#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

if [[ -z "${GOCACHE:-}" ]]; then
  export GOCACHE="/tmp/makewand-gocache"
fi
mkdir -p "$GOCACHE"

echo "[prelaunch] test gate"
bash ./scripts/test_gate.sh

echo "[prelaunch] go vet ./..."
go vet ./...

echo "[prelaunch] go build ./cmd/makewand"
mkdir -p build
go build -trimpath -o build/makewand ./cmd/makewand

MODES="${MAKEWAND_DOCTOR_MODES:-fast,balanced,power}"

echo "[prelaunch] doctor static checks"
build/makewand doctor --strict --modes "${MODES}"

if [[ "${MAKEWAND_LIVE_SMOKE:-0}" == "1" ]]; then
  PROBE_TIMEOUT="${MAKEWAND_PROBE_TIMEOUT:-45s}"
  PROBE_RETRIES="${MAKEWAND_PROBE_RETRIES:-1}"
  echo "[prelaunch] doctor live probe (modes=${MODES}, timeout=${PROBE_TIMEOUT}, retries=${PROBE_RETRIES})"
  build/makewand doctor --strict --probe --modes "${MODES}" --probe-timeout "${PROBE_TIMEOUT}" --probe-retries "${PROBE_RETRIES}"
else
  echo "[prelaunch] live probe skipped (set MAKEWAND_LIVE_SMOKE=1 to enable)"
fi

echo "[prelaunch] PASS"
