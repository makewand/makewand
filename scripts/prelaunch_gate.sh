#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

if [[ -z "${GOCACHE:-}" ]]; then
  # Go's private per-user cache, exported so test processes that move HOME
  # reuse it. A fixed /tmp path is shared with every local user, who could
  # plant build results there before the gate runs.
  GOCACHE="$(go env GOCACHE)"
  export GOCACHE
fi

echo "[prelaunch] test gate"
bash ./scripts/test_gate.sh

echo "[prelaunch] Python, dispatch, installation contracts"
# pytest runs the modules of module-level tests that the unittest gate cannot
# collect. python3 -I ignores the user site: install pytest into the interpreter.
if ! python3 -I -c 'import pytest' >/dev/null 2>&1; then
  echo "[prelaunch] pytest is not importable by python3 -I; install it with: python3 -m pip install pytest==9.0.2 (pytest 9 needs Python 3.10+)" >&2
  exit 1
fi
python3 -I -m pytest -q -p no:cacheprovider tests
bash dispatch/test.sh
python3 -I scripts/test_installation.py
python3 -I benchmarks/test_runner.py
python3 -I benchmarks/test_deep_config_acceptance_v4.py
python3 -I benchmarks/test_analyze_results.py

echo "[prelaunch] go vet ./..."
go vet ./...

echo "[prelaunch] go build ./cmd/makewand"
mkdir -p build
go build -trimpath -o build/makewand ./cmd/makewand
build/makewand --version

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
