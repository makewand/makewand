# makewand

AI coding assistant CLI (Go).

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

## Release integrity

Each GitHub release includes:

- platform binaries
- `checksums.txt`
- `checksums.txt.sig` (keyless cosign signature)
- `checksums.txt.pem` (signing certificate)

## Security

- Vulnerability reporting policy: [SECURITY.md](/mnt/data/makewand/SECURITY.md)

## First run

```bash
makewand setup
makewand doctor --strict --modes balanced,power
```

## Release

- strategy: [docs/RELEASE_STRATEGY.md](/mnt/data/makewand/docs/RELEASE_STRATEGY.md)
- prelaunch checklist: [docs/PRELAUNCH.md](/mnt/data/makewand/docs/PRELAUNCH.md)

## License

MIT. See [LICENSE](/mnt/data/makewand/LICENSE).
