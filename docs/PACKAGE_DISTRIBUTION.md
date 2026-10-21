# Package Distribution (Homebrew + Scoop)

> **Status (2026-10-21):** the tap and bucket serve **v3.2.0**, published by
> the Release workflow on 2026-10-09 (the 3.x tags before it failed in the
> workflow's `test` job, so v0.1.10 was the previous package). Changes merged
> since v3.2.0 reach `brew` and `scoop` users with the next release (v3.3.0):
> among them the Homebrew wrapper that no longer writes the build-time `PATH`
> into `bin/makewand` (#80) and the sandbox hardening listed under
> [Release status in SECURITY.md](../SECURITY.md#release-status--发布状态).
> Until then, install from source to get them
> (`curl -fsSL https://makewand.org/install.sh | bash`, needs the go.mod Go toolchain).

The release workflow now auto-generates package manifests on every `v*` tag:

- `dist/homebrew/Formula/makewand.rb`
- `dist/scoop/makewand.json`

These files are also attached to the GitHub release assets.

## Optional auto-publish to package repos

If configured, release workflow can also push updates to:

- Homebrew tap repo (default: `makewand/homebrew-makewand`)
- Scoop bucket repo (default: `makewand/scoop-makewand`)

### 1) Create target repos

1. Create `homebrew-makewand` repository with `Formula/` directory.
2. Create `scoop-makewand` repository for manifests (`makewand.json` at root).

### 2) Configure this repository secrets/variables

1. Add repository secret `PACKAGE_REPO_TOKEN`:
   - fine-grained PAT with `Contents: Read and write` on both target repos
2. (Optional) set repo variables:
   - `HOMEBREW_TAP_REPO` (override default tap repo)
   - `SCOOP_BUCKET_REPO` (override default bucket repo)

## End-user install commands

Both packages install the Go CLI together with its bundled Python engine
(`lib/`) and depend on Python 3.9+ (`python@3.12` on Homebrew, `main/python` on
Scoop).

Homebrew:

```bash
brew tap makewand/makewand
brew install makewand/makewand/makewand
```

Scoop:

```powershell
scoop bucket add makewand https://github.com/makewand/scoop-makewand
scoop install makewand
```

## Implementation references

- release workflow: [release.yml](../.github/workflows/release.yml)
- manifest generator: [gen_package_manifests.sh](../scripts/gen_package_manifests.sh)
- tap publisher: [publish_homebrew_tap.sh](../scripts/publish_homebrew_tap.sh)
- bucket publisher: [publish_scoop_bucket.sh](../scripts/publish_scoop_bucket.sh)
