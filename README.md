# makewand

[![Release](https://img.shields.io/github/v/release/makewand/makewand)](https://github.com/makewand/makewand/releases)
[![CI](https://github.com/makewand/makewand/actions/workflows/ci.yml/badge.svg)](https://github.com/makewand/makewand/actions/workflows/ci.yml)
[![CodeQL](https://github.com/makewand/makewand/actions/workflows/codeql.yml/badge.svg)](https://github.com/makewand/makewand/actions/workflows/codeql.yml)
[![Release Workflow](https://github.com/makewand/makewand/actions/workflows/release.yml/badge.svg)](https://github.com/makewand/makewand/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://github.com/makewand/makewand/blob/master/LICENSE)
[![Security Policy](https://img.shields.io/badge/Security-Policy-blue)](https://github.com/makewand/makewand/blob/master/SECURITY.md)

Adaptive multi-provider AI routing library and coding assistant CLI for terminal makers.
面向终端开发者的自适应多模型 AI 路由库与编码助手 CLI。

Orchestrates Claude, Gemini, and Codex through adaptive mode-based routing
(`fast/balanced/power`) with Thompson Sampling, circuit breakers, and
cost-aware provider selection. Works both as an interactive CLI tool and as
a standalone Go routing library with an OpenAI-compatible HTTP API.
通过 Thompson Sampling、熔断器和成本感知的 Provider 选择，编排 Claude、Gemini
和 Codex，支持自适应模式路由（`fast/balanced/power`）。既可作为交互式 CLI 工具使用，
也可作为独立的 Go 路由库使用，并提供 OpenAI 兼容的 HTTP API。

## Features / 特性

- **Three usage modes / 三种使用模式** — Fast (low latency), Balanced (quality/cost), Power (ensemble + judge)
- **Adaptive routing / 自适应路由** — Thompson Sampling learns provider quality per task type
- **Circuit breaker / 熔断器** — auto-excludes failing providers with cooldown recovery
- **Power ensemble / 强劲集成** — parallel multi-provider generation with cross-model evaluation
- **OpenAI-compatible HTTP API** — expose the router as a `/v1/chat/completions` endpoint
- **Strategy hot-reload / 策略热重载** — customize routing via `routing.json` with live polling
- **Cost tracking / 成本追踪** — per-request estimated costs and budget awareness
- **Diff/patch engine / 差异引擎** — search-and-replace + unified diff for code modifications
- **Repository context / 仓库上下文** — `.makewand/rules.md`, symbol caching, file hints
- **Headless mode / 无头模式** — single-prompt execution for CI/scripting (`--print`)
- **i18n / 国际化** — English and Chinese interface

## Install / 安装

### Linux / macOS (recommended / 推荐)

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

### Homebrew / Scoop (optional / 可选)

When package distribution repos are configured, each tag release auto-updates:
当包分发仓库已配置后，每次 tag 发布会自动更新：

- Homebrew tap formula (`makewand/homebrew-makewand`)
- Scoop bucket manifest (`makewand/scoop-makewand`)

See [docs/PACKAGE_DISTRIBUTION.md](docs/PACKAGE_DISTRIBUTION.md) for setup.
配置说明见 [docs/PACKAGE_DISTRIBUTION.md](docs/PACKAGE_DISTRIBUTION.md)。

## Quick Start / 快速开始

```bash
# First-time setup / 首次配置
makewand setup

# Health check / 健康检查
makewand doctor --strict --modes balanced,power

# Interactive chat / 交互式对话
makewand chat .

# Create new project / 创建新项目
makewand new

# One-shot prompt for CI / CI 单次执行
makewand --print "Explain this error" --mode fast
```

## Usage Modes / 使用模式

| Mode / 模式 | Tier | Behavior / 行为 | Typical Models / 典型模型 |
|------|------|----------|----------------|
| `fast` | Cheap | Lowest latency and cost / 最低延迟和成本 | Gemini Flash, Claude Haiku |
| `balanced` | Mid | Good quality/cost ratio / 质量/成本均衡 | Claude Sonnet, Gemini Flash |
| `power` | Premium | Parallel ensemble + judge / 并行集成 + 评判 | Claude Opus, Gemini Pro |

Switch modes at any time / 随时切换模式：

```bash
makewand chat --mode power .   # CLI flag
/mode balanced                 # In-session command / 会话内命令
```

## Providers / 服务商

All three providers are supported as subscription CLIs or via API keys:
三个 Provider 均支持订阅制 CLI 或 API Key 方式接入：

| Provider | CLI | API Key Env |
|----------|-----|-------------|
| Claude | `claude` (Claude Code) | `ANTHROPIC_API_KEY` |
| Gemini | `gemini` (Gemini CLI) | `GEMINI_API_KEY` |
| Codex | `codex` (Codex CLI) | `OPENAI_API_KEY` |

Subscription CLIs are auto-detected and preferred. Custom command-line providers are also supported.
订阅制 CLI 会被自动检测并优先使用。也支持自定义命令行 Provider。

### Custom Providers / 自定义 Provider

Custom command providers support three prompt delivery modes:
自定义命令 Provider 支持三种 prompt 传递方式：

- `prompt_mode: "stdin"`: recommended and safer / 推荐，更安全
- `prompt_mode: "arg"`: pass prompt as the last argv / 作为最后一个参数传递
- empty / omitted: legacy `{{prompt}}` or argv append behavior / 旧版行为

If you wrap a provider with `sh -c`, `bash -c`, `cmd /c`, or similar shell
adapters, prefer `prompt_mode: "stdin"`. `makewand setup` and `makewand doctor`
will warn on legacy or shell-based adapters.
如果用 `sh -c`、`bash -c`、`cmd /c` 之类的 shell 适配层包装 Provider，建议使用
`prompt_mode: "stdin"`。`makewand setup` 和 `makewand doctor` 会对 legacy 或
shell 适配器给出警告。

## Configuration / 配置

Config file / 配置文件: `~/.config/makewand/config.json`

```json
{
  "default_model": "claude",
  "usage_mode": "balanced",
  "claude_access": "subscription",
  "language": "en",
  "theme": "dark"
}
```

### Strategy Customization / 策略定制

Place `routing.json` in `~/.config/makewand/` to override default routing:
在 `~/.config/makewand/` 放置 `routing.json` 覆盖默认路由策略：

```json
{
  "strategies": {
    "balanced": {
      "code": { "tier": "mid", "providers": ["claude", "gemini"] }
    }
  }
}
```

Merges non-destructively — omitted fields retain defaults. Changes are picked
up automatically via hot-reload (30s polling).
非破坏性合并 — 未指定的字段保留默认值。修改会通过热重载自动生效（30 秒轮询）。

## Library Usage / 库使用

The `router` package is a standalone Go library:
`router` 包是独立的 Go 路由库：

```go
import "github.com/makewand/makewand/router"

r, err := router.NewRouterFromConfig(router.RouterConfig{
    Providers: map[string]router.ProviderEntry{
        "claude": {Provider: myProvider, Access: router.AccessSubscription},
    },
    UsageMode: "balanced",
    ConfigDir: "/path/to/config",  // optional: loads routing.json overrides
})

content, usage, result, err := r.Chat(ctx, router.TaskCode, messages, system)
```

### HTTP Facade

Expose the router as an OpenAI-compatible API:
将路由器暴露为 OpenAI 兼容 API：

```go
http.ListenAndServe(":8080", r.HTTPHandler())
```

| Endpoint | Description |
|----------|-------------|
| `POST /v1/chat/completions` | Chat completions / 聊天补全 |
| `GET /v1/models` | List available providers / 列出可用 Provider |
| `GET /health` | Health check / 健康检查 |

See [`router/README.md`](router/README.md) for full library documentation.
完整库文档见 [`router/README.md`](router/README.md)。

## CLI Reference / CLI 参考

```
makewand [prompt]              Interactive or one-shot prompt / 交互或单次执行
makewand chat [path]           Chat about a project / 对话
makewand new                   Create new project / 创建项目
makewand preview [path]        Start preview server / 启动预览服务
makewand setup                 Configure providers / 配置 Provider
makewand doctor                Health check / 健康诊断

Flags:
  --mode <fast|balanced|power>  Usage mode / 使用模式
  --print                       One-shot mode / 单次模式
  --timeout <duration>          Timeout for --print (default: 4m)
  --debug                       Enable trace logging / 启用追踪日志
```

## Debugging / 调试

Enable structured tracing to inspect routing decisions:
启用结构化追踪以检查路由决策：

```bash
makewand --debug "your prompt"
# Trace output → ~/.config/makewand/trace.jsonl
```

## Release Integrity / 发布完整性

Each GitHub release includes platform binaries, `checksums.txt`, and
keyless cosign signatures (`checksums.txt.sig` + `checksums.txt.pem`).
每个 GitHub Release 包含平台二进制文件、`checksums.txt` 和无密钥 cosign 签名。

## Project Structure / 项目结构

```
cmd/makewand/      CLI entry point / CLI 入口
router/            Standalone routing library / 独立路由库
internal/
  model/           CLI adapter layer / CLI 适配层
  config/          JSON config / 配置加载
  tui/             Bubble Tea UI / 交互界面
  engine/          Project management, diff/patch / 项目管理、差异引擎
  i18n/            Translations (en/zh) / 翻译
  diag/            Diagnostics / 诊断
```

## Security / 安全

- Vulnerability reporting: [SECURITY.md](SECURITY.md)
- Version support: [SUPPORT.md](SUPPORT.md)

## Contributing / 贡献

- Contribution guide: [CONTRIBUTING.md](CONTRIBUTING.md)
- Code of Conduct: [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)

## Release / 发布

- Strategy: [docs/RELEASE_STRATEGY.md](docs/RELEASE_STRATEGY.md)
- Prelaunch checklist: [docs/PRELAUNCH.md](docs/PRELAUNCH.md)
- Package distribution: [docs/PACKAGE_DISTRIBUTION.md](docs/PACKAGE_DISTRIBUTION.md)
- GitHub hardening: [docs/GITHUB_HARDENING.md](docs/GITHUB_HARDENING.md)

## License / 许可证

MIT. See [LICENSE](LICENSE).
