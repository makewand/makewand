#!/usr/bin/env bash
# Official source installer: curl -fsSL https://makewand.org/install.sh | bash
set -euo pipefail

command -v git >/dev/null || { echo "Git is required." >&2; exit 1; }
command -v python3 >/dev/null || { echo "Python 3.9+ is required." >&2; exit 1; }
command -v go >/dev/null || { echo "Source installation requires the Go toolchain specified in go.mod; prebuilt releases require Python 3.9+." >&2; exit 1; }
INSTALL_DIR="${MAKEWAND_INSTALL_ROOT:-$HOME/.local/share/makewand}"
SOURCE_DIR="${MAKEWAND_SOURCE_DIR:-}"
FETCH_DIR=""
trap 'if [ -n "$FETCH_DIR" ]; then rm -rf "$FETCH_DIR"; fi' EXIT
if [ -z "$SOURCE_DIR" ]; then
    FETCH_DIR="$(mktemp -d)"
    git clone --depth 1 https://github.com/makewand/makewand.git "$FETCH_DIR/source"
    SOURCE_DIR="$FETCH_DIR/source"
fi
# Never pull into an active installation. Both engines are built and tested in
# a new version directory before the stable current pointer is replaced.
export MAKEWAND_INSTALL_ROOT="$INSTALL_DIR"
bash "$SOURCE_DIR/scripts/install.sh"

CONFIG_DIR="${MAKEWAND_CONFIG_DIR:-$HOME/.config/makewand}"
mkdir -p "$CONFIG_DIR"
python3 -I - "$CONFIG_DIR/config.json" <<'PY'
import json, os, sys
try:
    fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
except FileExistsError:
    pass
else:
    with os.fdopen(fd, "w", encoding="utf-8") as config:
        json.dump({"enabled_providers": {"local": False}}, config, indent=2)
        config.write("\n")
PY
echo 'Installation complete. Run makewand status to inspect available providers.'
