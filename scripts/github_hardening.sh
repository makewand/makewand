#!/usr/bin/env bash
set -euo pipefail

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required command: $1" >&2
    exit 1
  }
}

detect_repo() {
  local url
  url="$(git config --get remote.origin.url || true)"
  if [[ -z "${url}" ]]; then
    return 1
  fi

  case "${url}" in
    git@github.com:*.git)
      echo "${url#git@github.com:}" | sed 's/\.git$//'
      ;;
    https://github.com/*.git)
      echo "${url#https://github.com/}" | sed 's/\.git$//'
      ;;
    https://github.com/*)
      echo "${url#https://github.com/}"
      ;;
    *)
      return 1
      ;;
  esac
}

detect_default_branch() {
  local ref
  ref="$(git symbolic-ref --short refs/remotes/origin/HEAD 2>/dev/null || true)"
  if [[ -n "${ref}" ]]; then
    echo "${ref#origin/}"
    return
  fi
  echo "master"
}

need_cmd gh
need_cmd git

PROFILE="${MAKEWAND_HARDENING_PROFILE:-team}"
APPROVAL_COUNT=1
CODEOWNER_REVIEWS=true
case "${PROFILE}" in
  team)
    APPROVAL_COUNT=1
    CODEOWNER_REVIEWS=true
    ;;
  solo)
    APPROVAL_COUNT=0
    CODEOWNER_REVIEWS=false
    ;;
  *)
    echo "invalid MAKEWAND_HARDENING_PROFILE: ${PROFILE} (expected: team or solo)" >&2
    exit 1
    ;;
esac

REPO="${1:-}"
if [[ -z "${REPO}" ]]; then
  REPO="$(detect_repo || true)"
fi
if [[ -z "${REPO}" ]]; then
  echo "usage: $0 [owner/repo] [branch]" >&2
  echo "could not infer owner/repo from git remote" >&2
  exit 1
fi

BRANCH="${2:-$(detect_default_branch)}"

if ! gh auth status >/dev/null 2>&1; then
  echo "gh is not authenticated; run: gh auth login" >&2
  exit 1
fi

echo "[hardening] repo: ${REPO}"
echo "[hardening] branch: ${BRANCH}"
echo "[hardening] profile: ${PROFILE} (approvals=${APPROVAL_COUNT}, codeowners=${CODEOWNER_REVIEWS})"

echo "[hardening] applying repository baseline settings"
gh api --method PATCH "repos/${REPO}" \
  -H "Accept: application/vnd.github+json" \
  -f delete_branch_on_merge=true \
  -f allow_merge_commit=false \
  -f allow_squash_merge=true \
  -f allow_rebase_merge=true \
  -f allow_auto_merge=true \
  -f web_commit_signoff_required=true >/dev/null

echo "[hardening] enabling vulnerability alerts"
gh api --method PUT "repos/${REPO}/vulnerability-alerts" \
  -H "Accept: application/vnd.github+json" >/dev/null

payload="$(mktemp)"
trap 'rm -f "${payload}"' EXIT
cat >"${payload}" <<JSON
{
  "required_status_checks": {
    "strict": true,
    "contexts": [
      "verify"
    ]
  },
  "enforce_admins": true,
  "required_pull_request_reviews": {
    "dismiss_stale_reviews": true,
    "require_code_owner_reviews": ${CODEOWNER_REVIEWS},
    "required_approving_review_count": ${APPROVAL_COUNT}
  },
  "restrictions": null,
  "required_linear_history": true,
  "allow_force_pushes": false,
  "allow_deletions": false,
  "block_creations": false,
  "required_conversation_resolution": true,
  "lock_branch": false,
  "allow_fork_syncing": true
}
JSON

echo "[hardening] applying branch protection"
gh api --method PUT "repos/${REPO}/branches/${BRANCH}/protection" \
  -H "Accept: application/vnd.github+json" \
  --input "${payload}" >/dev/null

echo "[hardening] current branch protection snapshot"
gh api "repos/${REPO}/branches/${BRANCH}/protection" \
  --jq '{
    required_status_checks: .required_status_checks.contexts,
    require_code_owner_reviews: .required_pull_request_reviews.require_code_owner_reviews,
    required_approving_review_count: .required_pull_request_reviews.required_approving_review_count,
    required_linear_history: .required_linear_history.enabled,
    required_conversation_resolution: .required_conversation_resolution.enabled,
    allow_force_pushes: .allow_force_pushes.enabled,
    allow_deletions: .allow_deletions.enabled,
    enforce_admins: .enforce_admins.enabled
  }'

echo "[hardening] done"
