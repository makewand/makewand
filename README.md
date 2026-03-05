# makewand

[![Release](https://img.shields.io/github/v/release/makewand/makewand)](https://github.com/makewand/makewand/releases)
[![CI](https://github.com/makewand/makewand/actions/workflows/ci.yml/badge.svg)](https://github.com/makewand/makewand/actions/workflows/ci.yml)
[![CodeQL](https://github.com/makewand/makewand/actions/workflows/codeql.yml/badge.svg)](https://github.com/makewand/makewand/actions/workflows/codeql.yml)
[![Release Workflow](https://github.com/makewand/makewand/actions/workflows/release.yml/badge.svg)](https://github.com/makewand/makewand/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://github.com/makewand/makewand/blob/master/LICENSE)
[![Security Policy](https://img.shields.io/badge/Security-Policy-blue)](https://github.com/makewand/makewand/blob/master/SECURITY.md)

AI coding assistant CLI (Go).  
AI 编码助手命令行工具（Go）。

An adaptive multi-provider coding assistant for CLI workflows, with
mode-based routing (`free/economy/balanced/power`) and release integrity checks
(checksums, cosign signatures, and provenance).  
面向 CLI 工作流的自适应多提供商编码助手，支持模式路由（`free/economy/balanced/power`）和发布完整性校验
（校验和、cosign 签名与 provenance 证明）。

## Install / 安装

### Linux / macOS (recommended) / Linux / macOS（推荐）

```bash
curl -fsSL https://raw.githubusercontent.com/makewand/makewand/master/scripts/install.sh | bash
```

The installer verifies downloaded binaries against release `checksums.txt` before installing.  
安装脚本会先根据发布包中的 `checksums.txt` 校验二进制文件，再执行安装。

Optional variables / 可选变量：

- `MAKEWAND_VERSION=vX.Y.Z` (default: latest)
- `MAKEWAND_INSTALL_DIR=$HOME/.local/bin`
- `MAKEWAND_REPO=makewand/makewand`

### From source / 从源码构建

```bash
go build -trimpath -o build/makewand ./cmd/makewand
```

### Homebrew / Scoop (optional for maintainers with package repos configured)  
### Homebrew / Scoop（可选，适用于已配置包仓库的维护者）

When package distribution repos are configured, each tag release auto-updates:
当包分发仓库已配置后，每次 tag 发布会自动更新：

- Homebrew tap formula (`makewand/homebrew-makewand`)
- Scoop bucket manifest (`makewand/scoop-makewand`)

See [docs/PACKAGE_DISTRIBUTION.md](docs/PACKAGE_DISTRIBUTION.md) for setup.  
配置说明见 [docs/PACKAGE_DISTRIBUTION.md](docs/PACKAGE_DISTRIBUTION.md)。

## Release integrity / 发布完整性

Each GitHub release includes:
每个 GitHub Release 包含：

- platform binaries
- `checksums.txt`
- `checksums.txt.sig` (keyless cosign signature)
- `checksums.txt.pem` (signing certificate)

## Security / 安全

- Vulnerability reporting policy: [SECURITY.md](SECURITY.md)
- Version support policy: [SUPPORT.md](SUPPORT.md)

## Contributing / 贡献

- Contribution guide: [CONTRIBUTING.md](CONTRIBUTING.md)
- Code of Conduct: [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)

## First run / 首次使用

```bash
makewand setup
makewand doctor --strict --modes balanced,power
```

## Release / 发布

- strategy: [docs/RELEASE_STRATEGY.md](docs/RELEASE_STRATEGY.md)
- prelaunch checklist: [docs/PRELAUNCH.md](docs/PRELAUNCH.md)
- package distribution: [docs/PACKAGE_DISTRIBUTION.md](docs/PACKAGE_DISTRIBUTION.md)
- GitHub hardening baseline: [docs/GITHUB_HARDENING.md](docs/GITHUB_HARDENING.md)
- hardening script: [scripts/github_hardening.sh](scripts/github_hardening.sh)
- CI workflow: [.github/workflows/ci.yml](.github/workflows/ci.yml)
- CodeQL workflow: [.github/workflows/codeql.yml](.github/workflows/codeql.yml)

## License / 许可证

MIT. See [LICENSE](LICENSE).
