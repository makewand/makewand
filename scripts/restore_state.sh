#!/usr/bin/env bash
# Thin wrapper around `makewand state restore`.
#
# Restore verifies every file against the archive manifest checksum, then
# atomically installs each component. STOP THE SERVER FIRST: restoring a live
# state database can corrupt it. Prefer calling `makewand state restore`.
#
# Usage: restore_state.sh <archive.tar.gz> [data-dir]
#   MAKEWAND_BIN          override the makewand binary (default: makewand on PATH)
#   MAKEWAND_AUTH_CONFIG  restore the auth config to this path
set -euo pipefail

archive="${1:?usage: restore_state.sh <archive.tar.gz> [data-dir]}"
data_dir="${2:-$HOME/.config/makewand/server}"
bin="${MAKEWAND_BIN:-makewand}"

echo "warning: ensure the makewand server is stopped before restoring" >&2

args=(state restore "$archive" --data-dir "$data_dir" --force)
if [ -n "${MAKEWAND_AUTH_CONFIG:-}" ]; then
  args+=(--auth-config "$MAKEWAND_AUTH_CONFIG")
fi

"$bin" "${args[@]}"
