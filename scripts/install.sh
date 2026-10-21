#!/usr/bin/env bash
# Install from an immutable staged version; existing engines survive failures.
set -euo pipefail
# This installs the checkout the script belongs to. Read from a pipe
# (curl ... | bash) or a /dev/fd path there is no checkout beside it.
SCRIPT_ROOT=""
if [ -n "${BASH_SOURCE[0]:-}" ]; then
    SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fi
if [ -z "$SCRIPT_ROOT" ] || [ ! -f "$SCRIPT_ROOT/scripts/install_source.py" ]; then
    echo 'scripts/install.sh installs the makewand checkout it is part of: run it from a clone, not through a pipe.' >&2
    echo 'To install from source without a clone: curl -fsSL https://makewand.org/install.sh | bash' >&2
    exit 1
fi
command -v python3 >/dev/null || { echo 'Python 3.9+ is required.' >&2; exit 1; }
python3 -I -c 'import sys; sys.exit(0 if sys.version_info >= (3, 9) else 1)' || {
    echo 'Python 3.9+ is required.' >&2; exit 1;
}
command -v go >/dev/null || { echo 'The Go toolchain specified in go.mod is required.' >&2; exit 1; }
exec python3 -I "$SCRIPT_ROOT/scripts/install_source.py" "$SCRIPT_ROOT"
