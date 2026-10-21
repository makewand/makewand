# GitHub Hardening Baseline

`scripts/github_hardening.sh` applies a release baseline to the repository
settings. Only part of it is in force on `makewand/makewand`: branch protection
on `master` is weaker than the script's target. This page lists both; the
script changes settings only when an admin runs it.

## Applied today

Read back with `gh api` on 2026-10-21:

1. Repository settings: branches auto-deleted on merge; merge commits disabled,
   squash and rebase allowed; auto-merge enabled; signoff required for
   web-based commits.
2. Security: Dependabot vulnerability alerts and security updates, secret
   scanning and push protection enabled.
3. Branch protection on `master`:
   - one required status check, `verify` (CI), with branches required to be up
     to date;
   - not enforced for admins;
   - no pull request or review required;
   - linear history required; force-push and deletion blocked;
   - conversation resolution not required.

The Release workflow also needs the native Windows gate (`native-windows /
verify`, which the Architecture runtime workflow runs on every push and pull
request). Branch protection does not require it, so a green `master` can still
fail a release: check it before tagging, see
[the release checklist](RELEASE_STRATEGY.md#release-operator-checklist).

## Target baseline (recommended, not applied)

Running the script sets the repository settings above and replaces the branch
protection with:

- required status checks `verify` (CI), `Analyze (go)` (CodeQL) and
  `native-windows / verify` (Architecture runtime), branches up to date;
- pull request required before merge, with a profile chosen by maintainer count:
  - `team`: 1 approval + CODEOWNERS review
  - `solo`: no mandatory approval count
- stale approvals dismissed on new commits;
- conversation resolution required;
- linear history required; force-push and deletion blocked;
- enforced for admins.

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

- Required checks must match the names GitHub reports exactly, or merges wait
  forever. CodeQL's job is `Analyze (${{ matrix.language }})`, reported as
  `Analyze (go)` and `Analyze (python)`; a job that calls a reusable workflow is
  reported as `<caller job> / <job>`, e.g. `native-windows / verify`. CI's
  `verify` therefore stays a plain job of `ci.yml`, which the Release workflow
  calls.
- If CI job names change, update `scripts/github_hardening.sh` accordingly.
- `CODEOWNERS` is defined at `.github/CODEOWNERS`.
- Default profile is `auto`.
