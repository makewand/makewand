# makewand

AI coding assistant CLI (Go).

## Install

### Linux / macOS (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/bnumsn/makewand/master/scripts/install.sh | bash
```

Optional variables:

- `MAKEWAND_VERSION=v0.1.0` (default: latest)
- `MAKEWAND_INSTALL_DIR=$HOME/.local/bin`
- `MAKEWAND_REPO=bnumsn/makewand`

### From source

```bash
go build -trimpath -o build/makewand ./cmd/makewand
```

## First run

```bash
makewand setup
makewand doctor --strict --modes balanced,power
```

## Release

- strategy: [docs/RELEASE_STRATEGY.md](/mnt/data/makewand/docs/RELEASE_STRATEGY.md)
- prelaunch checklist: [docs/PRELAUNCH.md](/mnt/data/makewand/docs/PRELAUNCH.md)
