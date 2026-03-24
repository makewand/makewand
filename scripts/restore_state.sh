#!/usr/bin/env bash
set -euo pipefail

archive="${1:?usage: restore_state.sh <archive.tar.gz> [target-dir]}"
target_dir="${2:-$HOME/.config/makewand/server}"

mkdir -p "$target_dir"
tar -C "$target_dir" -xzf "$archive"
printf 'Restored backup into: %s\n' "$target_dir"
