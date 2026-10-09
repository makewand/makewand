# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

本项目所有重要变更记录于此。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

## [3.2.0] - 2026-10-09

### Security & Hardening

- **Bubblewrap 物理沙箱全线加固与跨平台支持**：
  - 解决 Codex 0.162+ 在 `readonly=True` 审查模式下写 SQLite WAL/SHM 锁文件的只读文件系统报错（`os error 30 EROFS`），引入 tmpfs 内存写层并安全只读绑定宿主配置凭据。
  - 影子工作树使用 `git clone --shared --dissociate` 解耦克隆，消除宿主仓库根目录挂载泄露风险。
  - 非信任仓库更新子模块时严格封锁 `protocol.file.allow=always`，阻断本地文件窃取风险（CVE-2022-39253）。
  - 宿主工作区事务备份区分纯依赖缓存与构建产物，感知 gitignore 状态，确保未被忽略的代码文件在事务回滚中完整保全。
  - 家目录逃逸防护：`find_git_root` 终止于用户家目录和文件系统根边界；提供多账号凭据变体（如 `.codex-2`, `.codex-3` 等）动态掩码屏蔽。
  - 环境隔离与宿主脱敏：自动剥离桌面 GUI/DBus 环境变量，防止进程通过 IPC 逃逸；支持 `MAKEWAND_DISABLE_UNSHARE_NET` 便携适配无 CAP_NET_ADMIN 容器环境。
  - macOS 与无 bwrap 宿主友好降级：提供安全执行提示与 `--approval` 显式授权门禁。
- **发布与 CI 流水线解封**：
  - 修复 GitHub Actions runner 限制（relax `kernel.apparmor_restrict_unprivileged_userns`），打通端到端 live sandbox smoke 验证。
  - 升级 Go 工具链指令至 `1.26.9`，全面消除 Go 标准库已知 CVE 漏洞。

### Fixed

- Native Go and Python dispatches now share execution outcomes, task identity,
  deadlines, an atomic call budget and expiring workflow capacity holds. Unknown
  results stop automatic fallback and replay; race reserves judge calls and time in advance.
- Conservative workflow selection retains independent tests and sealed review.
  Opt-in stage events report timing without logging prompts or credentials;
  unknown token, cost and memory measurements remain null. A fixed 12-task
  repeated offline suite verifies independent acceptance and early-exit refusal.
  Its 72 stub trials validate the evaluator; live workflow/model effectiveness
  was not remeasured. This phase adds zero live calls to the exhausted 12/12 budget.

- Python daemon isolates every request in a worker, preserves argv/stdin/client
  policies, bounds IPC resources, and never replays a sent request after losing
  its result. Cancellation and versioned request status have regression coverage.
- Hybrid candidates retain complete three-way changes, file modes and deletions;
  tests and independent review bind to the sealed baseline and artifact. Git
  replacement refs and moving HEAD cannot hide changes from approval.
- Race stages, composite test suites and test fallbacks share their remaining
  time budget. Verification does not write workspace playbook state or Python
  bytecode; isolated copies retain tracked and deliverable cache-directory
  source while respecting ignored files in nested repositories.
- Subscription task admission uses a locked cross-process budget; failures,
  review follow-ups and live health probes count. Benchmarks independently verify
  child results, reject early exits, bound output and record evidence provenance.
- Login source/account/global limits and a shared password-hash concurrency cap
  bound resource use; JSON user writes serialize across processes. SQL usage
  aggregation, indexed paging, asynchronous webhook delivery, readiness and
  request/database metrics replace unbounded reads and blocking notification.
- Source installation freezes both engines and embedded assets, verifies matching
  versions and both public entry points, then switches atomically. Failed upgrades
  preserve a runnable previous version. A shared command registry prevents
  delegation drift, including global working-directory arguments.
- Go candidate fan-out, timeout and cost thresholds are configurable; workspace
  copies use safe filesystem-aware reflinks with ordinary-copy fallback. Output
  limits drain discarded bytes without turning successful commands into failures.


- **Release and CI pipelines unblocked on GitHub-hosted runners / 发布链与 CI 解锁
  (eng-delivery#1, #2).** Every 3.x tag failed in the `test` job, so no 3.x
  artifacts or Homebrew/Scoop updates were ever published: Ubuntu 24.04 runners
  set `kernel.apparmor_restrict_unprivileged_userns=1`, which strips bubblewrap's
  capabilities inside its user namespace (`bwrap: loopback: Failed RTM_NEWADDR:
  Operation not permitted`). CI and Release now relax that sysctl right after
  installing bubblewrap and run a `bwrap --unshare-net` diagnostic with a
  readable error; the `MAKEWAND_REQUIRE_BWRAP=1` hard gate is kept. All jobs have
  `timeout-minutes`.
- **Secret scanner / 密钥扫描 (eng-delivery#4).** `scripts/check_secrets.sh`
  passes every pattern with `git grep -e` (the PEM rule used to be parsed as an
  option and silently skipped), fails closed on scanner errors, ships built-in
  rules for current Anthropic/OpenAI/AWS/GitHub/GitLab/Slack/Google key formats,
  JWTs, private keys, private-network IPv4 hosts (CIDR ranges ignored) and
  workstation home paths without needing `~/.config`, and redacts findings.
  Regression corpus: `scripts/test_check_secrets.sh` (in `make test` and CI).
- **Version consistency / 版本一致性 (eng-delivery#9).** `scripts/check_version.sh`
  checks README, website, welcome card, CHANGELOG and the Go `dev` default against
  `makewand.__version__`; the Release workflow also requires the tag to equal
  `v` + `__version__` and stamps binaries without the `v`, so source and release
  installs print the same `makewand version X.Y.Z`.
- **Release docs and local gates / 发布文档与本地门禁 (eng-delivery#10, #11, #12).**
  Release notes install the prebuilt archive instead of the Go source installer;
  `make lint` / `make vuln` run golangci-lint and govulncheck under the go.mod
  toolchain; `make prelaunch` now matches docs/PRELAUNCH.md; the release-metadata
  workflow no longer hardcodes a personal account and defaults to a dry run.

### Changed (merged after 3.1.0, 2026-09-24 – 2026-09-27)

- Interactive console redesigned after Claude Code: minimalist prompt, multiline
  input, path completion, terminal Markdown highlighting, quota bars in `status`.
- Portable multi-model `dispatch/ai-dispatch` layer with a Muse egress guard.
- Dynamic quota pacing and adaptive effort modulation; `--max-fix` flag; diffs
  against the empty tree in empty repositories.
- Project website (`site/`) with Cloudflare/Vercel deployment configs.
- Security and reliability remediation from the 2026-09-27 assessments (F01–F06,
  hardlink and fork-bomb P0s, deadlocks, candidate symlink pruning, readline
  history, sanitized absolute paths).

## [3.1.0] - 2026-09-24

### Added

- **Universal tool adaptation / 主流工具自适应接入.** Provider adapters for Claude
  Code, Codex CLI, Antigravity (`agy`), Grok Build, Muse Code and Aider; OpenAI-
  compatible cloud APIs (DeepSeek, Qwen/DashScope, GLM, Kimi/Moonshot, OpenRouter,
  SiliconFlow) and self-hosted models (Ollama, vLLM, LocalAI, llama.cpp).
- **Dynamic runtime topology.** Detects the active tool pool at start-up: two or
  more tools run a cross-model red-team review pipeline, a single tool runs a
  shadow-worktree self-critique pipeline, none prints setup guidance.
- **Usage governance.** 4h/24h/7d sliding-window burn-rate tracking, soft
  anti-burst penalties and host concurrency back-pressure.
- `status` dashboard listing the active tool pool and not-yet-configured tools
  with one-line activation hints.

### Security

- Bubblewrap isolation enforced for local test runs, non-bypassable unit-test
  quality gate, transactional rollback and collision-free shadow worktrees.

> No prebuilt artifacts were published for this tag: the Release workflow failed
> at the bubblewrap gate (fixed under Unreleased). The GitHub release is empty.

## [3.0.2] - 2026-09-23

### Security

- `run_local_tests()` runs inside the bubblewrap sandbox with networking
  disabled, a tmpfs `HOME` and provider credentials masked.
- The test-delivery quality gate can no longer be overridden by a plain-text
  "LGTM" review.
- Go → Python delegation no longer imports modules from the current directory
  (cwd hijack), and `--repo-trust` is validated before delegation.
- Explicit read-only constraints survive the natural-language CLI entry point.

> `makewand.__version__` still reported 3.0.0 in this tag (now caught by
> `scripts/check_version.sh --tag`). No prebuilt artifacts were published.

## [3.0.1] - 2026-09-23

### Fixed

- Unified execution safety across providers, strict review-verdict gates, a
  single local test runner, and a dual-stack Go/Python CLI bridge.

> `makewand.__version__` still reported 3.0.0 in this tag. No prebuilt artifacts
> were published.

## [3.0.0] - 2026-09-23

### Added

- **Makewand v3 unified multi-model orchestrator (Python engine).** Autonomous
  planning, code generation and cross-model red-team review; quota-window
  burn-rate routing with hard circuit breakers; bubblewrap sandbox with a
  transparent repository mount; multi-session worktree isolation guard with
  ephemeral shadow branches; open-source sanitization pre-push gate
  (`scripts/check_secrets.sh`).
- **Quota-aware routing / 配额感知路由.** Routing now reads each subscription's
  remaining usage (5-hour session window + weekly cap) and steers work toward the
  pool with headroom before a cap is hit.
  - Predicted headroom is a *soft* signal — a near-limit pool is deprioritized in
    candidate ranking (new `QuotaBand` sort key, orthogonal to the Thompson
    quality score) but never removed, so quota prediction alone cannot cause a
    routing failure.
  - Confirmed exhaustion (a real quota/429 error) *hard-blocks* the pool at the
    execution boundary until its window resets.
  - `makewand quota` (with `--json`) shows the current picture per provider.
  - Data is read locally or via already-stored credentials (Claude OAuth usage
    endpoint, Codex session logs, agy login state); nothing is uploaded, and an
    unreadable source degrades to neutral.
  - Quota reads are cached to disk (`~/.cache/makewand/quota-snapshot.json`,
    120s TTL) and shared across processes, so repeated invocations don't re-hit
    the rate-limited usage endpoints; a stale cache still serves as last-good
    when a source is momentarily unavailable. 429-seals are never persisted.
  - Disabled by default for library embedders: `NewRouterFromConfig` is quota-free
    unless `RouterConfig.Quota` is set.
- **Antigravity CLI (`agy`) support.** Personal Gemini subscriptions now route
  through `agy` (the successor to the retired individual `gemini` CLI OAuth). When
  installed, `agy` takes the Gemini provider slot automatically; `MAKEWAND_AGY_MODEL`
  overrides the model.
- **Untrusted-repository mode (`--repo-trust=trusted|untrusted`).** For cloned
  third-party repositories you do not control, `--repo-trust=untrusted` routes
  generation only to direct API providers (never a local repo-aware CLI) and fails
  closed if none is configured, stops injecting the repo's `.makewand/rules.md` as
  trusted instructions, and skips local-CLI health/quota probes in the repo's cwd.
  The flag is validated globally and honored by `serve`, `doctor`, and `quota`.
  Default is `trusted` (unchanged behavior). See SECURITY.md for the boundary.

### Security

- **Candidate verification is sandboxed and fail-closed.** Test/build/dependency
  commands for generated candidates run inside a bubblewrap sandbox (read-only
  root, writable workspace, cleared environment, `--unshare-net` for test/build
  steps); when strong isolation is unavailable, verification does not execute
  unless `MAKEWAND_UNSAFE_HOST_EXEC=1` is set. Baseline `*_test.go` and npm test
  scripts are restored before judging, "no tests" cannot reach a passing strength,
  and `.git`/CI config/scripts are protected write paths. Static preview serves via
  `python3 -I` so a repo-local `sitecustomize.py`/`http` package cannot execute.
- **Router instance isolation.** Strategy/pricing tables and provider factories are
  per-`Router` (no shared mutable package state); overrides deep-merge at field
  granularity with strict validation, and hot-reload recomputes from immutable
  defaults.
- **Multi-user server authorization.** Remote sessions are namespaced per
  user/org/project; delegated tokens cannot widen scopes/allowlists/quota beyond
  their issuer; self-registration creates inactive accounts in a single write;
  login rate-limiting ignores forwarded-IP headers unless a `--trusted-proxy` is
  configured. Public registration is a separate opt-in (`--enable-registration`).
- Trust boundary documented in SECURITY.md (verification/preview are sandboxed;
  generation runs provider CLIs on the host), with a one-time runtime notice on
  first local-CLI generation.

### Changed

- The metered `gemini -p` path is now a fallback used only when `agy` is not
  installed, avoiding accidental pay-per-token charges on personal subscriptions.

> No prebuilt artifacts were published for 3.0.0 (Release workflow failed at
> the bubblewrap gate).

## Earlier releases

0.1.x releases (last: v0.1.10, 2026-03-06) are described in their
[GitHub release notes](https://github.com/makewand/makewand/releases).

[Unreleased]: https://github.com/makewand/makewand/compare/v3.2.0...HEAD
[3.2.0]: https://github.com/makewand/makewand/compare/v3.1.0...v3.2.0
[3.1.0]: https://github.com/makewand/makewand/compare/v3.0.2...v3.1.0
[3.0.2]: https://github.com/makewand/makewand/compare/v3.0.1...v3.0.2
[3.0.1]: https://github.com/makewand/makewand/compare/v3.0.0...v3.0.1
[3.0.0]: https://github.com/makewand/makewand/compare/v0.1.10...v3.0.0
