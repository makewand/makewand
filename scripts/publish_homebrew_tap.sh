#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 3 ]]; then
  echo "usage: $0 <tap-repo> <formula-path> <tag>" >&2
  exit 1
fi

if [[ -z "${GH_TOKEN:-}" ]]; then
  echo "GH_TOKEN is required for publishing to tap repo" >&2
  exit 1
fi

TAP_REPO="$1"
FORMULA_PATH="$2"
TAG="$3"

if [[ ! -f "${FORMULA_PATH}" ]]; then
  echo "formula file not found: ${FORMULA_PATH}" >&2
  exit 1
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

git clone "https://github.com/${TAP_REPO}.git" "${tmp_dir}"
git -C "${tmp_dir}" remote set-url origin "https://x-access-token:${GH_TOKEN}@github.com/${TAP_REPO}.git"

default_branch="$(git -C "${tmp_dir}" remote show origin | sed -n '/HEAD branch/s/.*: //p' | head -n 1)"
if [[ -z "${default_branch}" ]]; then
  default_branch="main"
fi

mkdir -p "${tmp_dir}/Formula"
cp "${FORMULA_PATH}" "${tmp_dir}/Formula/makewand.rb"

git -C "${tmp_dir}" config user.name "github-actions[bot]"
git -C "${tmp_dir}" config user.email "github-actions[bot]@users.noreply.github.com"

if git -C "${tmp_dir}" diff --quiet -- Formula/makewand.rb; then
  echo "Homebrew formula unchanged; skipping commit."
  exit 0
fi

git -C "${tmp_dir}" add Formula/makewand.rb
git -C "${tmp_dir}" commit -m "makewand ${TAG}"
git -C "${tmp_dir}" push origin "HEAD:${default_branch}"

echo "Published Homebrew formula to ${TAP_REPO}@${default_branch}"
