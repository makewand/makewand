#!/usr/bin/env bash
# Thin wrapper around `makewand state backup`.
#
# It snapshots the SQLite state database with VACUUM INTO — a transaction-
# consistent copy that is safe to take while the server is running, unlike a
# plain tar of the live file — plus the auth config and JSONL ledgers. Prefer
# calling `makewand state backup` directly; this wrapper only fills defaults.
#
# Usage: backup_state.sh [data-dir] [out-dir]
#   MAKEWAND_BIN          override the makewand binary (default: makewand on PATH)
#   MAKEWAND_AUTH_CONFIG  include this auth config (e.g. /etc/makewand/server_auth.json)
set -euo pipefail

data_dir="${1:-$HOME/.config/makewand/server}"
out_dir="${2:-$PWD/backups}"
bin="${MAKEWAND_BIN:-makewand}"

mkdir -p "$out_dir"
timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
archive="$out_dir/makewand-backup-$timestamp.tar.gz"

args=(state backup "$archive" --data-dir "$data_dir")
if [ -n "${MAKEWAND_AUTH_CONFIG:-}" ]; then
  args+=(--auth-config "$MAKEWAND_AUTH_CONFIG")
fi

"$bin" "${args[@]}"
