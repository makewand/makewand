# GitHub Hardening Baseline

This repository includes an automation script to apply a practical release baseline in GitHub settings.

## What it configures

1. Repository settings:
   - auto-delete branch on merge
   - disable merge-commit, keep squash/rebase
   - enable auto-merge
   - require signoff for web-based commits
2. Security:
   - enable Dependabot vulnerability alerts
3. Default branch protection:
   - require status check `verify`
   - require pull request before merge
   - auto-select profile based on maintainer count:
     - `team`: require 1 approval + CODEOWNERS review
     - `solo`: no mandatory approval count
   - dismiss stale approvals on new commits
   - require conversation resolution
   - require linear history
   - block force-push and deletion
   - enforce for admins

## Run

```bash
# Auto-detect owner/repo and default branch
./scripts/github_hardening.sh

# Or explicitly:
./scripts/github_hardening.sh makewand/makewand master

# Force team profile
MAKEWAND_HARDENING_PROFILE=team ./scripts/github_hardening.sh

# Force solo profile
MAKEWAND_HARDENING_PROFILE=solo ./scripts/github_hardening.sh
```

Prerequisites:

- `gh` installed and authenticated (`gh auth login`)
- admin permission on the target repository

## Notes

- The status check context is set to `verify` (from `.github/workflows/ci.yml`).
- If CI job names change, update `scripts/github_hardening.sh` accordingly.
- `CODEOWNERS` is defined at `.github/CODEOWNERS`.
- Default profile is `auto`.
