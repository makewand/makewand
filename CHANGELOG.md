# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

本项目所有重要变更记录于此。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Security

修复 2026-10-10 安全审计的全部未闭环项（Python 与 Go 两个引擎）：

- **启动器完整性**：`bwrap`、`systemd-run`、`prlimit` 只从 root 拥有、不可写的系统目录解析，不再经 `$PATH`（仓库内的 `.venv/bin`、`node_modules/.bin` 无法再冒充 bwrap）。
- **私有 provider 家目录**：宿主 `~/.claude`、`~/.codex`、`~/.gemini` 等不再可写挂载；每次调用使用私有副本，只把变化的凭据文件经校验后写回。
- **无沙箱时一律需要确认**：没有 bwrap 时，只读审查也需要 `MAKEWAND_UNSAFE_HOST_EXEC=1` 加确认并写入审计日志（Python 与 Go 一致）。
- **seccomp 默认强制**，Go 的 engine 启动器也已接入；Go 沙箱内 `.git` 元数据只读；工作区内的 Unix 套接字（含 `node_modules` 下）全部屏蔽；扫描不完整时可写会话 fail-closed。
- **应用与审查完整性**：候选应用改用 `renameat2` 交换/不覆盖，并发保存不再被覆盖；审查的 diff 原样传入并与交付绑定 sha256；AST 合并拒绝冲突的 import 绑定；交付指纹覆盖 `.makewand_*` 与依赖目录；任务产生的 `__pycache__`/`.pyc` 不再进入交付补丁。
- **计费与服务端**：组合调用计入失败尝试的花费；可选开启 `MAKEWAND_SCALED_BUDGET_RESERVATION=1`，让 HTTP 预算预留按定价、输出上限和扇出放大，并在流式输出超出预留时停止（默认仍为固定预留）；SSE 事件总量有上限；SQLite 默认 `synchronous=FULL` 与 `_txlock=immediate`。

2026-10-12 P1 修复批次（Python 与 Go 两个引擎；行为细节与残余风险见 SECURITY.md、docs/VERIFICATION_CONTRACT.md 与 docs/EXECUTION_CONTRACT.md）：

- **凭据回写只合并同账号的令牌刷新**：两引擎使用同一套规则和共享测试向量（`_credential_writeback_merge` / `cliMergeCredential`），以宿主文档为基础只拷入令牌刷新字段。登出、换账号或重定向（删除/清空令牌，改动邮箱、API key、端点、认证方式、账号 ID 等受保护字段）、JWT 换成他人账号或不透明令牌、更换 Google refresh token、时间倒退、令牌类型除大小写外的变化、非严格 JSON（含孤立的 UTF-16 代理转义）都整份拒绝，并提示一次（指明文件和字段，不显示值）。muse 只有 OAuth 登录记录中的 `api_key` 视为其自行铸造、可单独写回，保存或粘贴的 key 受保护；`flow_auth.json` 只拷入。写回在宿主文件旁的 `.<文件名>.makewand-lock` 上取 `flock`（最多等 10 秒）并在重命名前复核宿主文件；副本被删除/替换/超过 4 MiB/不可读、沙箱内新建登录（含宿主缺少整个 provider 目录）都会提示，不再静默跳过。Go 的审查与远端（`makewand serve`）会话也使用私有副本并经同一合并写回，`Chat` 与正常结束的流式调用等写回完成才返回（最多 15 秒）。不透明令牌无法离线绑定账号、JWT 签名不校验、登出无法防止等残余风险见 SECURITY.md。
- **依赖安装网络**：Go 候选验证的每个步骤都在独立网络命名空间中运行。依赖安装（及 Rust 检查前的 `cargo fetch`）在有可信的 `slirp4netns`、`nsenter`、`ip` 时获得只能访问公网的用户态网络：私网、回环、链路本地（云元数据）、CGNAT、组播、保留网段与本机地址在命令运行前即被路由屏蔽，不启用 IPv6，可选的 `tc`/`nft` 再丢弃组播与 IPv6；网络配置失败时沙箱在命令运行前被终止，步骤改为仅回环重跑。安装失败只有经 makewand 确认才算环境问题：联网时由 makewand 自身可执行文件在全新沙箱中探测默认注册表（go 按 go 命令的 `,`/`|` 规则遍历 `GOPROXY` 与校验和数据库），离线时须由项目文件证明确有未安装的注册表依赖（go 依据宿主工具链 `go mod edit -json` 解析的 go.mod、go.sum、imports 与两处模块缓存）。候选代码打印的文字不能再伪造环境错误。
- **宿主模式交付**：只核查本次任务新建、修改或删除的路径（按类型、权限与内容判断，单纯 ctime/inode 变化不算修改）。审查 diff 之外的改动——被忽略文件的新建或改动、二进制/可执行产物、`.env*`/`.envrc`/`*.pth`/`sitecustomize.py`/`.husky`/`.githooks`、子模块或嵌套仓库中的文件、本检出及其子模块私有 hooks 目录中的新 hook——阻断交付并回滚。新 hook 移入 `rejected_*/quarantine/git-hooks/` 并附 `mv` 恢复命令，不再删除；链接工作树或仓库外 `core.hooksPath` 等共享 hooks 目录只报告、不阻断。用户的 diff 配置（外部 diff 与 textconv、颜色、`diff.relative`、路径前缀、`diff.context` 等）不再影响审查 diff、交付核查与导出补丁；回滚改用字面路径（`[ab].txt` 不再连带恢复 `a.txt`）。未备份的既有文件按 SHA-256 摘要与抽样指纹核对，内容无法确认时按“无法确认”阻断。
- **审查完整性**：竞速裁决行绑定本任务 nonce（`MAKEWAND_RACE_VERDICT_<nonce>`），候选 diff 中回显的裁决行无效；Winner-Take-All 胜者以及裁判选中的与其同一提供方的候选，都须经其他提供方只读复审通过，并优先选择独立于双方选手的裁判。只采纳收到完整提示的裁决：超过 31 KiB 的提示不交给 agy（流水线改由下一位可用审查方接替），占位引擎（cursor、copilot）从不审查，本地模型的只读回答须由 token 计数证明读到了完整提示，OpenRouter 请求关闭提示压缩（`transforms: []`）。
- **测试不再改动源码树**：设置 `MAKEWAND_TEST_ISOLATION_ROOT`（测试环境）时，宿主事务、流水线、竞速、DAG（含单任务）与 `apply` 一律拒绝以 makewand 源码树、其子目录或上级目录为工作区（单一定义 `git_helper.harness_source_tree_conflict`）；测试只能连接自身绑定的回环端口，代理变量被清除，pytest 结束时列出被拦截的连接。

### Added

- 递归保护：makewand 启动的 provider CLI 带 `MAKEWAND_PROVIDER_WORKER=1`，在其中再调用会派发模型的 makewand 命令会被拒绝。
- 新环境变量：`MAKEWAND_BWRAP_PATH`（非系统目录安装的 bwrap）、`MAKEWAND_ALLOW_NO_SECCOMP=1`（显式关闭 seccomp）、`MAKEWAND_SANDBOX_MEMORY_MAX`（默认 `8G`）、`MAKEWAND_SANDBOX_TASKS_MAX`（默认 `512`）、`MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT`（工作区扫描上限，默认 1,000,000）、`MAKEWAND_SQLITE_SYNCHRONOUS`（`FULL`/`EXTRA`/`NORMAL`）、`MAKEWAND_SCALED_BUDGET_RESERVATION=1`（`makewand serve` 按定价放大预算预留）。
- 新环境变量（2026-10-12 批次）：`MAKEWAND_REVIEW_DIFF_MAX_BYTES`（复审 diff 上限，默认 262144 字节，原固定为 64 KiB；无效值回退默认并警告一次）；`MAKEWAND_FINGERPRINT_FILE_LIMIT`、`MAKEWAND_FINGERPRINT_TOTAL_LIMIT`、`MAKEWAND_FINGERPRINT_SECONDS`（宿主模式摘要与抽样指纹上限，默认 64 MiB、512 MiB、20 秒）；`MAKEWAND_SLIRP4NETNS_PATH`、`MAKEWAND_NSENTER_PATH`、`MAKEWAND_IP_PATH`、`MAKEWAND_TC_PATH`、`MAKEWAND_NFT_PATH`（依赖安装网络工具不在 root 拥有的系统目录时，指定可信的绝对路径）。
- `makewand/cli_message_catalog.json`：各 CLI 自己打印的限额、限流、过载、登录与地区消息目录；Go 路由器内嵌字节相同的 `router/cli_message_catalog.json`，两侧共用对照语料 `tests/testdata/cli_message_parity.json`。
- Go SDK `ExecResult.Note`：makewand 自己的说明（例如输出疑似注册表故障但未被确认的原因），也附在候选验证的 `DepsError`/`QuickCheckError` 之后。
- 使用账本记录 `route`（`cli`/`api`），API 路由的成功不再清除 CLI 的失败记录。

### Changed

- 行为变化：拒绝以 `/`、`/tmp`、`/home`、系统根目录、`$HOME` 或其上级目录作为可写工作区；provider 的会话历史不再持久化到宿主；Go 沙箱内的 agent 不能再修改 `.git`。
- 重写 agent skill `makewand-orchestrator`：按 CLI 实际行为说明命令、退出码 0–17、影子工作树交付与递归规则，收紧触发条件。
- 宿主模式交付被阻断时返回 UNVERIFIED（退出码 11，原为 10），失败原因列出未能恢复的文件；有文件无法复核时，回滚标题不再声称已恢复基线。
- 复审 diff 上限提高到 256 KiB；超出时不截断审查，结论为 UNVERIFIED，完整补丁保存为可直接 `git apply` 的 `unverified.patch`（`--binary --full-index --unified=3`，子模块补丁列入 `manifest.json`）。审查 diff 与导出补丁总带三行上下文。
- 测试产物只有一个定义（`git_helper.is_test_output_path`），测试关卡与交付核查共用：`coverage/`、`playwright-report/` 只在仓库根、任务目录或包根计，被 Git 跟踪的文件永远是测试输入。`run_local_tests` 删除本次运行在默认位置生成的测试产物（不进入审查与交付）；可作为代码执行的产物或链接使关卡失败并写明原因；测试前已存在的目录（含空目录）保留；被重写的既有测试产物只在是纯数据（含 Playwright 的 `.zip`/`.webm`）或字节不变时保留。
- 多阶段 DAG（`makewand plan`）在一份私有暂存副本中逐阶段执行，累积结果作为候选 `dg_…` 的 `A` 只交付一次；只读子任务审查此前的累积改动，其写入一律丢弃；原生 Windows 上多阶段 DAG 拒绝执行（请用 WSL2）。
- 候选封存清单覆盖 Git 跟踪或会添加的全部文件（含依赖/构建目录名下的文件），旧记录仍可应用。候选新建而工作区已有的条目视为冲突（确认后才用 `--force`）；apply 无法写入的工作区条目（目录、符号链接、特殊文件等）即使加 `--force` 也报告冲突（退出码 14）。`makewand candidates`/`inspect` 按实际情况标注主裁推荐、Winner-Take-All 胜出、旧版记录与 DAG 结果，只列出可行的操作。
- 没有可用引擎时，流水线、解释/问答、REPL 与 `ensemble` 都不派发任何任务并逐个列出原因，结果为 UNVERIFIED（退出码 11，错误为 `No usable execution provider: <引擎>: <原因>; …`）；`makewand race` 列出每个引擎的状态。claude/codex 登录失效（needs_auth）时与限额一样可转已配置的 API key（仍受 `MAKEWAND_API_POLICY` 约束）。
- `status --probe`、`probe`、`quota --probe` 在 agy 有未过期的失败记录或额度限制时做一次真实的 `agy -p "echo ok" --mode plan` 调用，只有成功或新的明确结论才替换旧记录。
- 限额与登录判定只依据各 CLI 自己的消息（上述目录）：两引擎读取相同的输出流与行，跳过 glog、代码块、缩进行、codex 的提示回显与转发的命令输出；流式模式（含原生 Windows）把 stderr 与回答分开捕获。Go 只读 stderr 末尾 8192 行/1 MiB；目录中没有的自定义 CLI 使用通用锚定模式，`token limit exceeded` 之类的措辞也可能把它封停 30 分钟。
- 依赖安装：没有锁文件的 npm 加 `--no-package-lock`；含 `pyproject.toml` 或 `setup.py` 的 pip 安装加 `--use-pep517`，egg-info 写到工作树外；有 `Cargo.lock` 时所有 cargo 步骤带 `--locked`，没有时删除 cargo 生成的 `Cargo.lock`；声明了依赖的 Rust 项目先 `cargo fetch`。TUI 在依赖无法下载时跳过测试，不扣分、不自动修复。
- 安装器只安装一份 `makewand-orchestrator` 技能；仍声明同名的旧 `trio-orchestrator` 副本会保持更新并提示删除。

### Fixed

- agy：启动时的 glog 行 "not logged into Antigravity" 不再被误判为需要重新登录（此前 agy 任意一次失败后都会被停用 30 分钟）；深度档不再传 agy 不支持的 `--effort max`；参数被拒按"结果已知的失败"处理，审查会换下一个审查方。
- 无沙箱的宿主路径改用环境变量传递 provider 标记，Windows 上不再依赖不存在的 `env` 命令。
- 3.2.0 的说明中网络隔离开关应为 `MAKEWAND_SANDBOX_UNSHARE_NET=1`（并无 `MAKEWAND_DISABLE_UNSHARE_NET`）。
- 计费（`makewand serve`）：流式 API 调用按实际调用的模型计价（OpenAI、Claude、Gemini API provider 报告所调模型，SSE `model` 字段改为模型 ID）；没有价目的 API 模型按每百万 token $3/$15 的保守价计费，不再为 $0；不返回 usage 的非流式回复按估算计费；未测得的费用记为 `cost_estimated=1`；估算输出不超过该次调用的 `max_tokens`，除非文本可证明更长。`MAKEWAND_SCALED_BUDGET_RESERVATION=1` 的预留按所有已注册 API 模型中的最高价计算（固定 provider 的请求也可能回退到它们），流式守卫按 token 下界计数，`max_tokens` 之内的回复不再被截断；上游忽略 `max_tokens` 时的超支残余见 SECURITY.md。
- 健康状态：重置时间解析支持带时区的日期、周限额与时长；`error`/`needs_auth` 记录缺少有效时间戳（缺失、无法解析或超前 5 分钟以上）时立即过期，`ttl_seconds` 最多 24 小时；处于 error 状态的引擎不再被选为裁判或审查方。
- agy：地区封锁、登录失效与参数错误只按 agy 自己的输出判定（提示与警告取自其二进制中的格式串），退出码 0 却没有回答算失败；`agy models` 刷新失败后 1 小时内不重试，缓存目录不可写时不刷新，不含 gemini-/google- 模型的输出不覆盖旧目录。Muse 不再因回答中的通用 429 行被判限流；codex 自己的 ERROR 事件、退出时的致命错误与额度警告，Claude 的组织额度消息，以及 Gemini CLI 的错误报告都能被识别。
- 模型发现：grok 快速档不再默认 2 倍计价的 build-fast 变体，价格措辞只在价格语境中解析。
- 非 UTF-8 文件名：消息、审查提示与产物中显示为 `\xNN`，不再抛出异常；影子交付脚本与打印的 apply 命令使用 `$'\xNN'` 写法并按字节写出；含此类文件名的候选元数据可封存并按原字节应用。
- 从子目录启动时，子模块改动纳入审查与交付。
- 单个凭据文件写回出错只提示，不影响其他文件与本次运行；私有 provider 目录的清理不再因极深目录树抛出异常。
- Go：内存作用域包装器不再把含 `=` 的目标路径当作 `env` 赋值（此前退出码 127）。
- Homebrew 公式不再把构建时的 `PATH` 写死进 `bin/makewand`（下一次发布生成的公式生效）。

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
