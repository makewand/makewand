#!/usr/bin/env bash
# Validate the executable exactly as an unpacked release, outside the checkout.
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BINARY="${1:-$ROOT_DIR/build/makewand}"
VERSION_TAG="${2:-}"
BINARY="$(python3 -I -c 'from pathlib import Path; import sys; print(Path(sys.argv[1]).resolve().as_posix())' "$BINARY")"
[[ -f "$BINARY" ]] || { echo "Binary not found: $BINARY" >&2; exit 1; }
for required in bin/makewand makewand/__init__.py makewand/cli.py makewand/VERSION; do
    [[ -f "$(dirname "$BINARY")/lib/makewand/python/$required" ]] || {
        echo "Incomplete release: bundled Python engine file $required is required." >&2; exit 1;
    }
done
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT
mkdir -p "$WORK_DIR/makewand" "$WORK_DIR/config"
printf 'raise RuntimeError("untrusted cwd imported")\n' > "$WORK_DIR/makewand/__init__.py"
cd "$WORK_DIR"
unset MAKEWAND_HOME PYTHONPATH PYTHONHOME
export MAKEWAND_CONFIG_DIR="$WORK_DIR/config"
export MAKEWAND_USAGE_FILE="$WORK_DIR/usage.json"
VERSION_OUTPUT="$("$BINARY" --version)"
[[ "$VERSION_OUTPUT" == 'makewand version '* ]] || { echo "Invalid version: $VERSION_OUTPUT" >&2; exit 1; }
BIN_VERSION="$(printf '%s\n' "$VERSION_OUTPUT" | awk '{print $3}')"
if [[ -n "$VERSION_TAG" && "$BIN_VERSION" != "$VERSION_TAG" ]]; then
    echo "Version mismatch: $BIN_VERSION != $VERSION_TAG" >&2; exit 1
fi
PY_VERSION="$(python3 -I "$(dirname "$BINARY")/lib/makewand/python/bin/makewand" --version)"
[[ "$PY_VERSION" == "makewand $BIN_VERSION" ]] || { echo "Engine version mismatch: $PY_VERSION / $VERSION_OUTPUT" >&2; exit 1; }
"$BINARY" --help >/dev/null
for command in new chat serve run review race status probe quota models observe candidates inspect apply discard; do
    "$BINARY" "$command" --help >/dev/null
done
"$BINARY" review --repo-trust untrusted --help >/dev/null
python3 -I "$ROOT_DIR/scripts/check_cli_examples.py" "$BINARY"
echo "Release contract passed: complete engines, matching version, public commands and cwd isolation."
