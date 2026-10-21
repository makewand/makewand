# Release Strategy (Including npm Decision)

## Decision Table

| Option | What users run | Pros | Cons | Recommended now |
| --- | --- | --- | --- | --- |
| GitHub Release archives (prebuilt) | download `makewand_vX.Y.Z_<os>_<arch>` from the release, verify `checksums.txt`, extract | Native Go CLI + bundled Python engine, no Go toolchain needed | Need GitHub release discipline | **Yes** |
| Source install script | `curl -fsSL https://makewand.org/install.sh \| bash` (clones `master`, then runs `scripts/install.sh` from the clone) | Always tracks `master` | Requires the go.mod Go toolchain + git | For contributors |
| npm package as primary channel | `npx makewand` / `npm i -g` | Familiar JS developer UX | Wraps native binary, extra package/release complexity | Not yet |
| Homebrew + Scoop (auto-generated manifests) | `brew install ...` / `scoop install ...` | Better native package manager UX | Requires tap/bucket repo maintenance | **Optional now** |

## Why not npm as primary now

- makewand is a Go CLI, not a Node runtime app.
- npm channel introduces an extra distribution layer (binary download wrapper, postinstall behavior, platform edge cases).
- current priority should be release reliability and support cost control.

## Recommended rollout

1. **Now**: GitHub Release artifacts + checksums + signature/provenance (release notes show the prebuilt install; the one-line installer is the source path).
2. **Now (optional)**: Homebrew/Scoop manifest auto-generation and optional push to tap/bucket repos.
3. **Later**: Optional npm wrapper package for discovery (not primary install path).

## Implemented in this repository

- PR/push CI gate:
  - [ci.yml](../.github/workflows/ci.yml)
- Security static analysis:
  - [codeql.yml](../.github/workflows/codeql.yml)
- Tag-triggered GitHub release workflow:
  - [release.yml](../.github/workflows/release.yml): its `test` job calls
    [ci.yml](../.github/workflows/ci.yml) (`workflow_call`), and publication
    also waits for `package-smoke` and the native Windows gate
  - [native-windows.yml](../.github/workflows/native-windows.yml) (reusable;
    also run on every push and pull request by
    [architecture.yml](../.github/workflows/architecture.yml))
- Website deployment (`site/`, including the `install.sh` one-liner):
  - [deploy-site.yml](../.github/workflows/deploy-site.yml), see
    [CLOUDFLARE_WEBSITE_DEPLOYMENT.md](CLOUDFLARE_WEBSITE_DEPLOYMENT.md)
- Dependency update automation:
  - [dependabot.yml](../.github/dependabot.yml)
- Installer scripts:
  - [site/install.sh](../site/install.sh) (served as `https://makewand.org/install.sh`)
    clones `master` and runs [scripts/install.sh](../scripts/install.sh)
- Security policy:
  - [SECURITY.md](../SECURITY.md)
- Support policy:
  - [SUPPORT.md](../SUPPORT.md)
- Pre-launch quality gate (`make prelaunch`, see [PRELAUNCH.md](PRELAUNCH.md)):
  - [Makefile](../Makefile) targets `check-secrets`, `test-scripts`, `check-shell`, `fmt-check`, `lint`, `race`, `vuln`
  - [prelaunch_gate.sh](../scripts/prelaunch_gate.sh) (tests, vet, build, `doctor --strict`)
  - [check_version.sh](../scripts/check_version.sh) (version copies and tag agree with `makewand.__version__`)
  - [check_secrets.sh](../scripts/check_secrets.sh) + [test_check_secrets.sh](../scripts/test_check_secrets.sh)
- GitHub hardening baseline:
  - [GITHUB_HARDENING.md](GITHUB_HARDENING.md)
- Package distribution:
  - [PACKAGE_DISTRIBUTION.md](PACKAGE_DISTRIBUTION.md)

## Release operator checklist

1. Bump `__version__` in `makewand/__init__.py`, the README title, `site/`, the
   interactive welcome card and add a `## [X.Y.Z] - date` section to
   `CHANGELOG.md`; `bash scripts/check_version.sh --tag vX.Y.Z` must pass.
2. Run `make prelaunch` (see [PRELAUNCH.md](PRELAUNCH.md)).
3. (Recommended) run live probe: `MAKEWAND_LIVE_SMOKE=1 MAKEWAND_DOCTOR_MODES=balanced,power make prelaunch`.
4. Confirm the CI run for the release commit is green, and the
   `native-windows / verify` check of the Architecture runtime workflow too.
   Branch protection on `master` requires only the `verify` check, does not
   enforce it for admins and does not require pull requests, so a red CI does
   **not** block a direct push, and nothing requires the native Windows gate
   that Release needs — check both by hand (`gh run list --workflow CI -L 5`,
   `gh run list --workflow "Architecture runtime" -L 5`).
5. Tag and push: `git tag vX.Y.Z && git push origin vX.Y.Z`.
6. Watch the Release workflow (`gh run watch`). Its `test` job is CI's `verify`
   job run on the tag's commit, which also requires the tag to equal
   `v` + `makewand.__version__`; `package-smoke` runs the unpacked bundle on
   Linux, macOS and Windows; `native-windows` runs the same-commit native
   Windows gate (all Windows Go packages under `-race`, up to 75 minutes).
   `release` only runs after all three pass.
7. Verify the GitHub release has all archives, `checksums.txt`, the cosign
   signature/certificate and the provenance attestation, and that the Homebrew
   tap and Scoop bucket were bumped (`PACKAGE_REPO_TOKEN` must be configured).
   A release without assets means the workflow failed: never publish one by hand.
