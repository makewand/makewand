# Package Distribution (Homebrew + Scoop)

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
