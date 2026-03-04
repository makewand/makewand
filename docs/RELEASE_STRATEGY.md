# Release Strategy (Including npm Decision)

## Decision Table

| Option | What users run | Pros | Cons | Recommended now |
| --- | --- | --- | --- | --- |
| GitHub Release + install script | `curl .../install.sh \| bash` | Native Go binary, fastest startup, simple support | Need GitHub release discipline | **Yes** |
| npm package as primary channel | `npx makewand` / `npm i -g` | Familiar JS developer UX | Wraps native binary, extra package/release complexity | Not yet |
| Homebrew tap | `brew install ...` | Great for macOS users | Ongoing tap maintenance | Later |

## Why not npm as primary now

- makewand is a Go CLI, not a Node runtime app.
- npm channel introduces an extra distribution layer (binary download wrapper, postinstall behavior, platform edge cases).
- current priority should be release reliability and support cost control.

## Recommended rollout

1. **Now**: GitHub Release artifacts + checksums + one-line installer.
2. **Next**: Homebrew tap (if macOS demand is high).
3. **Then**: Optional npm wrapper package for discovery (not primary install path).

## Implemented in this repository

- Tag-triggered GitHub release workflow:
  - [release.yml](/mnt/data/makewand/.github/workflows/release.yml)
- Installer script:
  - [install.sh](/mnt/data/makewand/scripts/install.sh)
- Pre-launch quality gate:
  - [prelaunch_gate.sh](/mnt/data/makewand/scripts/prelaunch_gate.sh)

## Release operator checklist

1. Run `make prelaunch`.
2. (Recommended) run live probe: `MAKEWAND_LIVE_SMOKE=1 MAKEWAND_DOCTOR_MODES=balanced,power make prelaunch`.
3. Tag and push: `git tag vX.Y.Z && git push origin vX.Y.Z`.
4. Verify GitHub release assets and checksums.
