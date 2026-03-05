# Release Strategy (Including npm Decision)

## Decision Table

| Option | What users run | Pros | Cons | Recommended now |
| --- | --- | --- | --- | --- |
| GitHub Release + install script | `curl .../install.sh \| bash` | Native Go binary, fastest startup, simple support | Need GitHub release discipline | **Yes** |
| npm package as primary channel | `npx makewand` / `npm i -g` | Familiar JS developer UX | Wraps native binary, extra package/release complexity | Not yet |
| Homebrew + Scoop (auto-generated manifests) | `brew install ...` / `scoop install ...` | Better native package manager UX | Requires tap/bucket repo maintenance | **Optional now** |

## Why not npm as primary now

- makewand is a Go CLI, not a Node runtime app.
- npm channel introduces an extra distribution layer (binary download wrapper, postinstall behavior, platform edge cases).
- current priority should be release reliability and support cost control.

## Recommended rollout

1. **Now**: GitHub Release artifacts + checksums + signature/provenance + one-line installer.
2. **Now (optional)**: Homebrew/Scoop manifest auto-generation and optional push to tap/bucket repos.
3. **Later**: Optional npm wrapper package for discovery (not primary install path).

## Implemented in this repository

- PR/push CI gate:
  - [ci.yml](../.github/workflows/ci.yml)
- Security static analysis:
  - [codeql.yml](../.github/workflows/codeql.yml)
- Tag-triggered GitHub release workflow:
  - [release.yml](../.github/workflows/release.yml)
- Dependency update automation:
  - [dependabot.yml](../.github/dependabot.yml)
- Installer script:
  - [install.sh](../scripts/install.sh)
- Security policy:
  - [SECURITY.md](../SECURITY.md)
- Support policy:
  - [SUPPORT.md](../SUPPORT.md)
- Pre-launch quality gate:
  - [prelaunch_gate.sh](../scripts/prelaunch_gate.sh)
- GitHub hardening baseline:
  - [GITHUB_HARDENING.md](GITHUB_HARDENING.md)
- Package distribution:
  - [PACKAGE_DISTRIBUTION.md](PACKAGE_DISTRIBUTION.md)

## Release operator checklist

1. Run `make prelaunch`.
2. (Recommended) run live probe: `MAKEWAND_LIVE_SMOKE=1 MAKEWAND_DOCTOR_MODES=balanced,power make prelaunch`.
3. Tag and push: `git tag vX.Y.Z && git push origin vX.Y.Z`.
4. Verify GitHub release assets, checksums, signatures, and provenance attestation.
