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
a standalone Go routing library with an OpenAI-compatible subset HTTP API.
通过 Thompson Sampling、熔断器和成本感知的 Provider 选择，编排 Claude、Gemini
和 Codex，支持自适应模式路由（`fast/balanced/power`）。既可作为交互式 CLI 工具使用，
也可作为独立的 Go 路由库使用，并提供 OpenAI 兼容子集 HTTP API。

## Features / 特性

- **Three usage modes / 三种使用模式** — Fast (low latency), Balanced (quality/cost), Power (ensemble + judge)
- **Adaptive routing / 自适应路由** — Thompson Sampling learns provider quality per task type
- **Quota-aware routing / 配额感知路由** — reads each subscription's remaining 5h/weekly usage and steers work toward the pool with headroom *before* you hit a cap (`makewand quota`)
- **Circuit breaker / 熔断器** — auto-excludes failing providers with cooldown recovery
- **Power ensemble / 强劲集成** — parallel multi-provider generation with cross-model evaluation
- **OpenAI-compatible subset HTTP API** — expose the router as a `/v1/chat/completions` endpoint
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
# First-time setup; persists balanced routing by default
# 首次配置；默认保存 balanced 路由模式
makewand setup

# Optionally persist a different routing mode / 可选：保存其他路由模式
makewand setup --mode fast

# Health check / 健康检查
makewand doctor --strict --modes balanced,power

# Interactive chat / 交互式对话
makewand chat .

# Create new project / 创建新项目
makewand new

# One-shot prompt for CI / CI 单次执行
makewand --print "Explain this error" --mode fast
```

### Remote Server (Alpha)

⚠️ **ALPHA STATUS**: The server component is in alpha and provided without production support. For details on limitations, security considerations, and setup instructions, see [docs/SERVER_ALPHA.md](docs/SERVER_ALPHA.md).

**Quick summary**: Makewand can run as a server for centralized session storage and remote access via SSH tunnel or TLS-terminating reverse proxy. Use `makewand serve --listen 127.0.0.1:8080 --enable-users` on your main machine and point remote clients to it via `MAKEWAND_REMOTE_URL=http://localhost:8080` (over SSH tunnel, with `MAKEWAND_REMOTE_TOKEN` set to a server-issued token).

**Security**: The server listens on loopback by default and only accepts remote connections over SSH tunnel or TLS-based reverse proxy. Plaintext remote connections are explicitly blocked unless overridden with the `--unsafe-no-tls` flag.

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
| Gemini | `agy` (Antigravity CLI, preferred) or `gemini` (Gemini CLI) | `GEMINI_API_KEY` |
| Codex | `codex` (Codex CLI) | `OPENAI_API_KEY` |

Subscription CLIs are auto-detected and preferred. Custom command-line providers are also supported.
订阅制 CLI 会被自动检测并优先使用。也支持自定义命令行 Provider。

Since June 2026, personal Gemini subscriptions flow through the **Antigravity CLI
(`agy`)** rather than the old `gemini` CLI. When `agy` is installed it takes the
Gemini provider slot automatically; the metered `gemini -p` path is used only as
a fallback when `agy` is absent. Force a specific agy model with `MAKEWAND_AGY_MODEL`.
2026 年 6 月起，个人版 Gemini 订阅改由 **Antigravity CLI（`agy`）** 承载，取代旧的
`gemini` CLI。检测到 `agy` 时它自动占据 Gemini 槽位；只有在没有 `agy` 时才回退到
按量计费的 `gemini -p`。用 `MAKEWAND_AGY_MODEL` 可指定 agy 模型。

### Quota-Aware Routing / 配额感知路由

makewand reads each subscription's remaining usage and routes around pools that
are running low — *before* they hit a cap and start refusing requests or charging
metered overflow prices. Run `makewand quota` (add `--json` for scripting) to see
the current picture:
makewand 读取各订阅的剩余用量，在某个池子撞顶（开始拒绝请求或按量计费）**之前**就把
任务引开。运行 `makewand quota`（加 `--json` 便于脚本处理）查看当前状态：

```
$ makewand quota
── claude
   5h window   19% used
   weekly      64% used  (resets 07-19 20:00)
── codex
   weekly      86% used  (resets 07-21 06:08)
   → getting low — routing will deprioritize this pool
── gemini
   subscription: signed in (agy)
```

How it steers routing / 如何影响路由：

- **Predicted headroom** (usage %) is a *soft* signal: a pool nearing its cap is
  tried later, but never removed — quota prediction alone can't cause a routing
  failure. Warn/critical thresholds default to 70%/90%.
- **Confirmed exhaustion** (a real quota/429 error) *hard-blocks* that pool until
  its window resets, so retries route elsewhere.
- Quota data is read locally or via your own stored credentials (Claude's OAuth
  usage endpoint, Codex session logs, agy login state) — **nothing is uploaded**.
  A source that can't be read simply drops out, and routing falls back to its
  normal quality/circuit-breaker signals.

- **预测余量**（用量百分比）是*软*信号：接近上限的池子会被排后，但不会被移除——仅凭预测
  不会导致路由失败。warn/critical 阈值默认 70%/90%。
- **确认耗尽**（真实的配额/429 错误）会把该池*硬性封锁*到窗口重置，重试自动转向别处。
- 配额数据在本地读取或经你自己已存的凭据获取（Claude OAuth usage 接口、Codex 会话日志、
  agy 登录态）——**不上传任何内容**。读不到的源直接退出，路由回落到常规的质量/熔断信号。

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
if err != nil {
    return err
}

content, usage, result, err := r.Chat(ctx, router.TaskCode, messages, system)
```

### HTTP Facade

Expose the router as an OpenAI-compatible subset API:
将路由器暴露为 OpenAI 兼容子集 API：

```go
http.ListenAndServe(":8080", r.HTTPHandler())
```

A request may set `model` to a provider name returned by `/v1/models` to force
that provider. The facade also accepts alias families such as `gpt-*`, `o1*`,
`o3*`, and `o4*` for Codex-backed routing, plus `claude*`, `gemini*`, and
`codex*`.
请求可将 `model` 设置为 `/v1/models` 返回的 Provider 名称以强制选择该 Provider。
同时也支持常见 alias 家族：例如把 `gpt-*`、`o1*`、`o3*`、`o4*` 映射到 Codex，
以及把 `claude*`、`gemini*`、`codex*` 映射到对应 Provider。

`stream=true` is supported; `max_tokens` and `temperature` are accepted but
currently ignored for compatibility. `response_format` supports `json_object`
and a pragmatic subset of `json_schema`. Basic function-style tool calling is
also supported through `tools` and `tool_choice`.
支持 `stream=true`；`max_tokens`、`temperature` 会被接受，但当前仅作为兼容字段忽略。
`response_format` 支持 `json_object` 和实用型 `json_schema` 子集；也支持通过
`tools` 与 `tool_choice` 进行基础版函数式工具调用。
`makewand serve` also attaches `X-Request-Id` to each response and exposes a
Prometheus-style `/metrics` endpoint when started as a server.
`makewand serve` 还会在响应中附带 `X-Request-Id`，并暴露 Prometheus 风格的
`/metrics` 端点。

When a scoped token is associated with an organization or project and the
corresponding monthly budget is configured in the server team store, requests
are now rejected before routing once that budget is exhausted.
当 scoped token 关联了 organization 或 project，且服务端团队存储里配置了对应的
月预算后，预算耗尽时会在真正路由前直接拒绝请求。

A basic OpenAI-style `POST /v1/responses` subset is also available for
request/response clients, including SSE streaming mode.
同时也提供基础版的 `POST /v1/responses` 兼容入口，并支持 SSE streaming 模式。

| Endpoint | Description |
|----------|-------------|
| `POST /v1/chat/completions` | Chat completions (`stream=true` supported) / 支持 `stream=true` 的聊天补全 |
| `POST /v1/responses` | Responses API subset (`stream=true`, `response_format`, `tools`) / 支持 `stream=true`、`response_format`、`tools` 的 Responses API 子集 |
| `GET /v1/models` | List available providers / 列出可用 Provider |
| `GET /health` | Health check / 健康检查 |

See [`router/README.md`](router/README.md) for full library documentation.
完整库文档见 [`router/README.md`](router/README.md)。

## CLI Reference / CLI 参考

```
makewand [prompt]              Interactive or one-shot prompt / 交互或单次执行
makewand chat [path]           Chat about a project / 对话
makewand new                   Create new project / 创建项目
makewand serve                 Start personal remote server / 启动个人远程服务
makewand token                 Manage remote auth config / 管理远程鉴权配置
makewand audit                 Inspect server audit log / 查看服务端审计日志
makewand usage                 Inspect structured usage log / 查看结构化用量日志
makewand quota                 Show remaining subscription quota / 查看订阅剩余配额 (--json)
makewand user                  Manage registered server users / 管理注册用户
makewand preview [path]        Start preview server / 启动预览服务
makewand setup [--mode MODE]   Inspect providers and save routing mode (default: balanced)
                               检查 Provider 并保存路由模式（默认 balanced）
makewand doctor                Health check / 健康诊断

Flags:
  --mode <fast|balanced|power>  Usage mode / 使用模式
  --print                       One-shot mode / 单次模式
  --timeout <duration>          Timeout for --print (default: 4m)
  --debug                       Enable trace logging / 启用追踪日志
  --repo-trust <trusted|untrusted>  Repository trust level (default: trusted).
                                untrusted: only direct API providers may generate
                                (fail closed), and .makewand/rules.md is not trusted.
                                不可信仓库模式：仅允许直连 API Provider 生成（失败即拒），
                                且不信任 .makewand/rules.md
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

- **Trust model / 信任模型**: verification and preview run in a bubblewrap sandbox, but the **generation** stage runs provider CLIs directly on the host with your environment and credentials. Repository content is untrusted input to that host-privileged agent — for untrusted third-party repos, run makewand in a VM/container/low-privilege user. See [SECURITY.md](SECURITY.md#security-model--安全模型).
  验证与预览在 bubblewrap 沙箱内运行，但**生成**阶段会带着你的环境与凭据在宿主机上直接运行 provider CLI。仓库内容是该宿主权限 agent 的不可信输入——处理不可信的第三方仓库请在 VM/容器/低权限用户下运行。详见 [SECURITY.md](SECURITY.md#security-model--安全模型)。
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
