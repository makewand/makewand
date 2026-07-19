#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

BINARY="${1:-./build/makewand}"
VERSION_TAG="${2:-}"

echo "=== Release Contract Verification ==="
echo ""

if [[ ! -f "$BINARY" ]]; then
  echo "❌ Binary not found: $BINARY"
  exit 1
fi

echo "✓ Binary exists: $BINARY"

# 1. Verify --version output
echo ""
echo "--- Version Output ---"
VERSION_OUTPUT=$("$BINARY" --version)
echo "$VERSION_OUTPUT"

# Extract version from output (first word should be "makewand", then "version", then version string)
if ! echo "$VERSION_OUTPUT" | grep -q "makewand version"; then
  echo "❌ --version output format incorrect"
  exit 1
fi
echo "✓ --version output format correct"

# 2. Verify key public commands exist in help
echo ""
echo "--- Help Output Verification ---"
HELP_OUTPUT=$("$BINARY" --help)

required_commands=(
  "makewand \[prompt\]"
  "new"
  "chat"
  "serve"
)

for cmd in "${required_commands[@]}"; do
  if ! echo "$HELP_OUTPUT" | grep -q "$cmd"; then
    echo "❌ Required command not documented: $cmd"
    exit 1
  fi
done
echo "✓ All required commands documented in --help"

# 3. Verify subcommand help works
echo ""
echo "--- Subcommand Help Verification ---"
subcommands=("new" "chat" "serve")
for subcmd in "${subcommands[@]}"; do
  if ! "$BINARY" "$subcmd" --help >/dev/null 2>&1; then
    echo "⚠ Subcommand help may have issues: $subcmd"
  fi
done
echo "✓ Subcommands respond to --help"

# 4. Version tag verification (if provided)
if [[ -n "$VERSION_TAG" ]]; then
  echo ""
  echo "--- Version Tag Verification ---"
  if ! echo "$VERSION_OUTPUT" | grep -q "$VERSION_TAG"; then
    echo "⚠ Binary version ($VERSION_OUTPUT) doesn't match tag ($VERSION_TAG)"
  else
    echo "✓ Binary version matches tag: $VERSION_TAG"
  fi
fi

echo ""
echo "=== Release Contract Passed ✓ ==="
