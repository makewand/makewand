#!/usr/bin/env bash
# Makewand System-wide Installer & Symlink Setup
set -euo pipefail

BIN_DIR="$HOME/.local/bin"
SKILLS_DIR="$HOME/.gemini/config/skills"
REPO_URL="https://github.com/makewand/makewand.git"

# Detect if running from a local checkout or piped via curl ... | bash
IS_LOCAL=0
if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
    CANDIDATE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
    if [ -f "$CANDIDATE_DIR/bin/makewand" ]; then
        SCRIPT_DIR="$CANDIDATE_DIR"
        IS_LOCAL=1
    fi
fi

if [ "$IS_LOCAL" -eq 0 ]; then
    INSTALL_ROOT="$HOME/.local/share/makewand"
    echo "=== Downloading / Updating Makewand from repository ==="
    mkdir -p "$(dirname "$INSTALL_ROOT")"
    if [ -d "$INSTALL_ROOT/.git" ]; then
        echo "Updating existing clone in $INSTALL_ROOT..."
        git -C "$INSTALL_ROOT" pull --ff-only 2>/dev/null || true
    else
        echo "Cloning Makewand into $INSTALL_ROOT..."
        git clone "$REPO_URL" "$INSTALL_ROOT"
    fi
    SCRIPT_DIR="$INSTALL_ROOT"
fi

echo "=== Installing Makewand (v3.0.0) ==="
mkdir -p "$BIN_DIR"

# 1. Install makewand executable
TARGET_BIN="$BIN_DIR/makewand"
echo "Installing $TARGET_BIN -> $SCRIPT_DIR/bin/makewand"
ln -sf "$SCRIPT_DIR/bin/makewand" "$TARGET_BIN"
chmod +x "$TARGET_BIN"
chmod +x "$SCRIPT_DIR/bin/makewand"

# 1.5 Build native Go server/components if Go toolchain is available
if command -v go >/dev/null 2>&1; then
    echo "Building native Go server/components ($SCRIPT_DIR/bin/makewand-server)..."
    if (cd "$SCRIPT_DIR" && go build -trimpath -o "$SCRIPT_DIR/bin/makewand-server" ./cmd/makewand); then
        chmod +x "$SCRIPT_DIR/bin/makewand-server"
        echo "✔ Native Go server/CLI successfully compiled."
    else
        echo "⚠️ Warning: Failed to build native Go components. Server subcommands (serve/audit/token) may be unavailable." >&2
    fi
else
    echo "ℹ️ Go compiler not found. Server subcommands will require Go toolchain or precompiled binary."
fi

# 2. Maintain backwards compatibility with trio command
TRIO_BIN="$BIN_DIR/trio"
echo "Creating compatibility symlink $TRIO_BIN -> $TARGET_BIN"
ln -sf "$TARGET_BIN" "$TRIO_BIN"

# 3. Install Makewand global skill for Antigravity & AI agents
echo "Installing AI Skills..."
if [ -d "$SCRIPT_DIR/skills/makewand-orchestrator" ]; then
    mkdir -p "$SKILLS_DIR/makewand-orchestrator"
    cp -r "$SCRIPT_DIR/skills/makewand-orchestrator/"* "$SKILLS_DIR/makewand-orchestrator/"
    mkdir -p "$SKILLS_DIR/trio-orchestrator"
    cp -r "$SCRIPT_DIR/skills/makewand-orchestrator/"* "$SKILLS_DIR/trio-orchestrator/"
fi

echo "=== Installation Complete ==="
echo "You can now run 'makewand status' or 'makewand run <prompt>' from any directory."
echo "Backward-compatible 'trio' command is also linked to 'makewand'."
