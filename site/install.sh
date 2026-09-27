#!/usr/bin/env bash
# Official source installer: curl -fsSL https://makewand.org/install.sh | bash
set -euo pipefail

command -v git >/dev/null || { echo "Git is required." >&2; exit 1; }
command -v python3 >/dev/null || { echo "Python 3.9+ is required." >&2; exit 1; }
command -v go >/dev/null || { echo "Source installation requires the Go toolchain specified in go.mod; prebuilt releases require Python 3.9+." >&2; exit 1; }
INSTALL_DIR="${MAKEWAND_INSTALL_ROOT:-$HOME/.local/share/makewand}"
mkdir -p "$(dirname "$INSTALL_DIR")"
if [ -d "$INSTALL_DIR/.git" ]; then
    git -C "$INSTALL_DIR" pull --ff-only
else
    git clone --depth 1 https://github.com/makewand/makewand.git "$INSTALL_DIR"
fi

# Both installers share the isolated, fixed-path launcher and atomic upgrade.
bash "$INSTALL_DIR/scripts/install.sh"

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
