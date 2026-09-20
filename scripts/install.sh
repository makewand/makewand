#!/usr/bin/env bash
# Makewand System-wide Installer & Symlink Setup
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="$HOME/.local/bin"
SKILLS_DIR="$HOME/.gemini/config/skills"

echo "=== Installing Makewand (v3.0.0) ==="
mkdir -p "$BIN_DIR"

# 1. Install makewand executable
TARGET_BIN="$BIN_DIR/makewand"
echo "Installing $TARGET_BIN -> $SCRIPT_DIR/bin/makewand"
ln -sf "$SCRIPT_DIR/bin/makewand" "$TARGET_BIN"
chmod +x "$TARGET_BIN"

# 2. Maintain backwards compatibility with trio command
TRIO_BIN="$BIN_DIR/trio"
echo "Creating compatibility symlink $TRIO_BIN -> $TARGET_BIN"
ln -sf "$TARGET_BIN" "$TRIO_BIN"

# 3. Install Makewand global skill for Antigravity & AI agents
echo "Installing AI Skills..."
mkdir -p "$SKILLS_DIR/makewand-orchestrator"
cp -r "$SCRIPT_DIR/skills/makewand-orchestrator/"* "$SKILLS_DIR/makewand-orchestrator/"

# Also maintain trio-orchestrator skill compatibility
mkdir -p "$SKILLS_DIR/trio-orchestrator"
cp -r "$SCRIPT_DIR/skills/makewand-orchestrator/"* "$SKILLS_DIR/trio-orchestrator/"

echo "=== Installation Complete ==="
echo "You can now run 'makewand status' or 'makewand run <prompt>' from any directory."
echo "Backward-compatible 'trio' command is also linked to 'makewand'."
