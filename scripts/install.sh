#!/usr/bin/env bash
# Install the complete source distribution; no Python third-party dependencies.
set -euo pipefail

BIN_DIR="${MAKEWAND_BIN_DIR:-$HOME/.local/bin}"
SKILLS_DIR="${MAKEWAND_SKILLS_DIR:-$HOME/.gemini/config/skills}"
INSTALL_ROOT="${MAKEWAND_INSTALL_ROOT:-$HOME/.local/share/makewand}"
REPO_URL="https://github.com/makewand/makewand.git"

command -v python3 >/dev/null || { echo "Python 3.9+ is required." >&2; exit 1; }
python3 -I -c 'import sys; sys.exit(0 if sys.version_info >= (3, 9) else 1)' || {
    echo "Python 3.9+ is required." >&2; exit 1;
}
command -v go >/dev/null || { echo "The Go toolchain specified in go.mod is required for source installation. Prebuilt release bundles require only Python 3.9+." >&2; exit 1; }

SCRIPT_DIR=""
if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
    CANDIDATE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
    if [ -f "$CANDIDATE_DIR/bin/makewand" ]; then
        SCRIPT_DIR="$CANDIDATE_DIR"
    fi
fi
if [ -z "$SCRIPT_DIR" ]; then
    command -v git >/dev/null || { echo "Git is required." >&2; exit 1; }
    mkdir -p "$(dirname "$INSTALL_ROOT")"
    if [ -d "$INSTALL_ROOT/.git" ]; then
        git -C "$INSTALL_ROOT" pull --ff-only
    else
        git clone "$REPO_URL" "$INSTALL_ROOT"
    fi
    SCRIPT_DIR="$INSTALL_ROOT"
fi

# Compile before replacing the existing command, so failed upgrades remain usable.
BUILD_TMP="$(mktemp "$SCRIPT_DIR/bin/.makewand-server.XXXXXX")"
WRAPPER_TMP=""
trap 'rm -f "$BUILD_TMP" "${WRAPPER_TMP:-}"' EXIT
SOURCE_VERSION="$(python3 -I -c 'import sys; sys.path.insert(0, sys.argv[1]); import makewand; print(makewand.__version__)' "$SCRIPT_DIR")"
(cd "$SCRIPT_DIR" && go build -trimpath -ldflags "-X github.com/makewand/makewand/internal/buildinfo.Version=$SOURCE_VERSION" -o "$BUILD_TMP" ./cmd/makewand)
chmod 755 "$BUILD_TMP"
python3 -I -c 'import os,sys; os.replace(sys.argv[1], sys.argv[2])' "$BUILD_TMP" "$SCRIPT_DIR/bin/makewand-server"

mkdir -p "$BIN_DIR"
WRAPPER_TMP="$(mktemp "$BIN_DIR/.makewand.XXXXXX")"
{
    printf '#!/usr/bin/env bash\nset -euo pipefail\nexec python3 -I '
    printf '%q' "$SCRIPT_DIR/bin/makewand"
    printf ' "$@"\n'
} > "$WRAPPER_TMP"
chmod 755 "$WRAPPER_TMP"
# os.replace replaces a symlink itself; it never truncates its source target.
python3 -I -c 'import os,sys; os.replace(sys.argv[1], sys.argv[2])' "$WRAPPER_TMP" "$BIN_DIR/makewand"
python3 -I - "$BIN_DIR" <<'PY'
import os, pathlib, sys, tempfile
root = pathlib.Path(sys.argv[1])
fd, name = tempfile.mkstemp(prefix=".trio.", dir=root)
os.close(fd)
os.unlink(name)
try:
    os.symlink("makewand", name)
    os.replace(name, root / "trio")
finally:
    if os.path.lexists(name):
        os.unlink(name)
PY

if [ -d "$SCRIPT_DIR/skills/makewand-orchestrator" ]; then
    for skill in makewand-orchestrator trio-orchestrator; do
        mkdir -p "$SKILLS_DIR/$skill"
        cp -R "$SCRIPT_DIR/skills/makewand-orchestrator/." "$SKILLS_DIR/$skill/"
    done
fi
"$BIN_DIR/makewand" --version
"$BIN_DIR/makewand" run --help >/dev/null
"$BIN_DIR/makewand" serve --help >/dev/null
echo "Installed complete Makewand in $BIN_DIR. Add this directory to PATH if needed."
