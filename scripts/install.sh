#!/usr/bin/env bash
# Install from an immutable staged version; existing engines survive failures.
set -euo pipefail
command -v python3 >/dev/null || { echo 'Python 3.9+ is required.' >&2; exit 1; }
python3 -I -c 'import sys; sys.exit(0 if sys.version_info >= (3, 9) else 1)' || {
    echo 'Python 3.9+ is required.' >&2; exit 1;
}
command -v go >/dev/null || { echo 'The Go toolchain specified in go.mod is required.' >&2; exit 1; }
SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec python3 -I "$SCRIPT_ROOT/scripts/install_source.py" "$SCRIPT_ROOT"
