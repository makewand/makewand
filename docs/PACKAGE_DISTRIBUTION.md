# Package Distribution (Homebrew + Scoop)

> **Status (2026-09-28):** the tap and bucket still serve **v0.1.10**
> (2026-03-06). Every 3.x tag (v3.0.0–v3.1.0) failed in the Release workflow's
> `test` job (bubblewrap gate on Ubuntu 24.04 runners, fixed in
> `.github/workflows/release.yml`), so no 3.x archives or manifests were ever
> published and `brew install` / `scoop install` currently install 0.1.10. They
> update automatically on the next tag whose Release workflow succeeds with
> `PACKAGE_REPO_TOKEN` configured. Until then, install 3.x from source
> (`scripts/install.sh`, needs the go.mod Go toolchain).

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
