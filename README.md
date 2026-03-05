# makewand

[![Release](https://img.shields.io/github/v/release/makewand/makewand)](https://github.com/makewand/makewand/releases)
[![CI](https://github.com/makewand/makewand/actions/workflows/ci.yml/badge.svg)](https://github.com/makewand/makewand/actions/workflows/ci.yml)
[![Release Workflow](https://github.com/makewand/makewand/actions/workflows/release.yml/badge.svg)](https://github.com/makewand/makewand/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://github.com/makewand/makewand/blob/master/LICENSE)
[![Security Policy](https://img.shields.io/badge/Security-Policy-blue)](https://github.com/makewand/makewand/blob/master/SECURITY.md)

AI coding assistant CLI (Go).

An adaptive multi-provider coding assistant for CLI workflows, with
mode-based routing (`free/economy/balanced/power`) and production-style release
integrity checks.

## Install

### Linux / macOS (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/makewand/makewand/master/scripts/install.sh | bash
```

The installer verifies downloaded binaries against release `checksums.txt` before installing.

Optional variables:

- `MAKEWAND_VERSION=v0.1.0` (default: latest)
- `MAKEWAND_INSTALL_DIR=$HOME/.local/bin`
- `MAKEWAND_REPO=makewand/makewand`

### From source

```bash
go build -trimpath -o build/makewand ./cmd/makewand
```

### Homebrew / Scoop (auto-published, optional)

When package distribution repos are configured, each tag release auto-updates:

- Homebrew tap formula (`makewand/homebrew-makewand`)
- Scoop bucket manifest (`makewand/scoop-makewand`)

See [docs/PACKAGE_DISTRIBUTION.md](docs/PACKAGE_DISTRIBUTION.md) for setup.

## Release integrity

Each GitHub release includes:

- platform binaries
- `checksums.txt`
- `checksums.txt.sig` (keyless cosign signature)
- `checksums.txt.pem` (signing certificate)

## Security

- Vulnerability reporting policy: [SECURITY.md](SECURITY.md)
- Version support policy: [SUPPORT.md](SUPPORT.md)

## Contributing

- Contribution guide: [CONTRIBUTING.md](CONTRIBUTING.md)
- Code of Conduct: [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)

## First run

```bash
makewand setup
makewand doctor --strict --modes balanced,power
```

## Release

- strategy: [docs/RELEASE_STRATEGY.md](docs/RELEASE_STRATEGY.md)
- prelaunch checklist: [docs/PRELAUNCH.md](docs/PRELAUNCH.md)
- package distribution: [docs/PACKAGE_DISTRIBUTION.md](docs/PACKAGE_DISTRIBUTION.md)
- GitHub hardening baseline: [docs/GITHUB_HARDENING.md](docs/GITHUB_HARDENING.md)
- hardening script: [scripts/github_hardening.sh](scripts/github_hardening.sh)
- CI workflow: [.github/workflows/ci.yml](.github/workflows/ci.yml)

## License

MIT. See [LICENSE](LICENSE).
