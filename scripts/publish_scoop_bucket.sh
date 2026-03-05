#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 3 ]]; then
  echo "usage: $0 <bucket-repo> <manifest-path> <tag>" >&2
  exit 1
fi

if [[ -z "${GH_TOKEN:-}" ]]; then
  echo "GH_TOKEN is required for publishing to scoop bucket repo" >&2
  exit 1
fi

BUCKET_REPO="$1"
MANIFEST_PATH="$2"
TAG="$3"

if [[ ! -f "${MANIFEST_PATH}" ]]; then
  echo "manifest file not found: ${MANIFEST_PATH}" >&2
  exit 1
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

git clone "https://github.com/${BUCKET_REPO}.git" "${tmp_dir}"
git -C "${tmp_dir}" remote set-url origin "https://x-access-token:${GH_TOKEN}@github.com/${BUCKET_REPO}.git"

default_branch="$(git -C "${tmp_dir}" remote show origin | sed -n '/HEAD branch/s/.*: //p' | head -n 1)"
if [[ -z "${default_branch}" ]]; then
  default_branch="main"
fi

cp "${MANIFEST_PATH}" "${tmp_dir}/makewand.json"

git -C "${tmp_dir}" config user.name "github-actions[bot]"
git -C "${tmp_dir}" config user.email "github-actions[bot]@users.noreply.github.com"

if [[ -z "$(git -C "${tmp_dir}" status --porcelain -- makewand.json)" ]]; then
  echo "Scoop manifest unchanged; skipping commit."
  exit 0
fi

git -C "${tmp_dir}" add makewand.json
git -C "${tmp_dir}" commit -m "makewand ${TAG}"
git -C "${tmp_dir}" push origin "HEAD:${default_branch}"

echo "Published Scoop manifest to ${BUCKET_REPO}@${default_branch}"
