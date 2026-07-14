#!/usr/bin/env bash
set -euo pipefail

state_dir="${1:-$HOME/.config/makewand/server}"
out_dir="${2:-$PWD/backups}"
timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
archive="$out_dir/makewand-backup-$timestamp.tar.gz"

mkdir -p "$out_dir"
tar -C "$state_dir" -czf "$archive" \
  state.db \
  server_auth.json \
  audit.jsonl \
  usage.jsonl \
  2>/dev/null || tar -C "$state_dir" -czf "$archive" .

printf 'Created backup: %s\n' "$archive"
