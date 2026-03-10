# makewand

[![Release](https://img.shields.io/github/v/release/makewand/makewand)](https://github.com/makewand/makewand/releases)
[![CI](https://github.com/makewand/makewand/actions/workflows/ci.yml/badge.svg)](https://github.com/makewand/makewand/actions/workflows/ci.yml)
[![CodeQL](https://github.com/makewand/makewand/actions/workflows/codeql.yml/badge.svg)](https://github.com/makewand/makewand/actions/workflows/codeql.yml)
[![Release Workflow](https://github.com/makewand/makewand/actions/workflows/release.yml/badge.svg)](https://github.com/makewand/makewand/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://github.com/makewand/makewand/blob/master/LICENSE)
[![Security Policy](https://img.shields.io/badge/Security-Policy-blue)](https://github.com/makewand/makewand/blob/master/SECURITY.md)

Multi-provider coding router for terminal makers (Go).
面向终端开发者的多模型编码路由器（Go）。

Orchestrates Claude, Gemini, and Codex through adaptive
mode-based routing (`fast/balanced/power`) with Thompson Sampling,
circuit breakers, and cost-aware provider selection.
通过 Thompson Sampling、熔断器和成本感知的 Provider 选择，编排 Claude、Gemini
和 Codex，支持自适应模式路由（`fast/balanced/power`）。

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

## Daily usage / 日常使用

Start directly (Codex/Claude Code style):
直接启动（类似 Codex/Claude Code 交互）：

```bash
makewand
```

In-session commands:
会话内命令：

- `/mode fast|balanced|power`
- `/help`
- `/exit` (or `Ctrl+C`)

## Custom Providers / 自定义 Provider

Custom command providers support three prompt delivery modes:
自定义命令 Provider 支持三种 prompt 传递方式：

- `prompt_mode: "stdin"`: recommended and safer
- `prompt_mode: "arg"`: pass prompt as the last argv
- empty / omitted: legacy `{{prompt}}` or argv append behavior

If you wrap a provider with `sh -c`, `bash -c`, `cmd /c`, or similar shell
adapters, prefer `prompt_mode: "stdin"`. `makewand setup` and `makewand doctor`
will warn on legacy or shell-based adapters.
如果用 `sh -c`、`bash -c`、`cmd /c` 之类的 shell 适配层包装 Provider，建议使用
`prompt_mode: "stdin"`。`makewand setup` 和 `makewand doctor` 会对 legacy 或
shell 适配器给出警告。

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
