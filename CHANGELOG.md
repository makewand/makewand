# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

本项目所有重要变更记录于此。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

- 健康探测在慢主机上启动时就已超过 4 秒时限时（例如新二进制正被杀毒扫描），按与“启动后卡住”相同的超时策略处理：订阅 CLI 软放行、自定义探测仍判不可用。此前 Windows CI 上 gemini 会被误判为不可用。
- 模型发现写出的 `discovered.json` 改为参考性输入：其中本构建无法定价的 API 模型条目被丢弃（该档位保留内置模型），合并或校验仍失败的整个文件被忽略；此前一个未定价的发现模型（如 agy 的 `gemini-4-argon` 写进 gemini 档位）会让 TUI、headless 与 serve 全部启动失败。用户自己的 `routing.json` 仍然严格校验。
- 有界派发改用 Go/Python 共享的机器账本 `call-budget.json`；任务上限之外，项目每日默认 200 次、机器每日默认 1000 次，按 Asia/Shanghai 午夜重置。已结算记录保留 48 小时，写入时清理；未结束记录、活动租约和预留所引用的计数保留，旧 lifetime 上限不因清理退款。详见 [预算与租约](docs/layered-call-admission.md)。
- 账号与订阅池默认并发 1，provider 默认并发 64；无可用回退时最多等待 30 秒（`MAKEWAND_LEASE_WAIT_SECONDS`）。busy 不启动 provider、不计费、不降低可靠性，返回 FAILED/退出码 10；真实预算耗尽返回退出码 13。某并发键没有活动租约后可重新配置并发上限。
- quota cache v2 按账号及订阅池绑定；未知账号不作为独立池。Claude 登录绑定仅投影 `.claude.json` 的账号 ID，正常项目历史/计数写入不使绑定失效；启动前账号校验拒绝不计费。Python Codex 使用 `codex exec --json` 提取协议证据。
- provider worker 采用严格命令白名单，仅允许 `search`、`repomap`、静态 `help`、`completion <shell>` 和单独的帮助/版本参数；`quota`、`status`、`route`、`mark`、`outcome` 等管理命令均拒绝。详见 [worker 策略](docs/PROVIDER_WORKER_GUARD.md)。
自动派发与统一派发（#81–#85）：

- 新增 `makewand route`、`makewand mark`、`makewand outcome`（#81）：route 为子任务在完整的模型 × 推理深度目录中从够用的最便宜档位起选工具、模型与推理深度，跳过暂停中、`mark` 标记期内与官方周额度过线的工具，按 `outcome` 记录的成绩做 Thompson 采样排序，打印可照抄的派活命令，不调用模型；mark 记录一家工具额度受限到何时，outcome 记录一次派活的验收成绩。Go 入口把这三个命令交给 Python 引擎。用法见 README。
- agy 从不花 G1 credits（#81）：两个沙箱的私有家目录里，agy 的两个设置文件（`~/.gemini/settings.json`、`~/.gemini/antigravity-cli/settings.json`）一律写成 `useG1Credits=false`；没有 bwrap 时，只有宿主这两个文件都证明 `useG1Credits` 为 false 才运行 agy，否则 fail-closed 并说明。
- agy 在 makewand 沙箱中能执行命令（#82）：agy 每次 run_command 前要写 `~/.gemini/antigravity-cli/bin/agentapi`，两个沙箱此前把该目录只读挂载；现在它是 tmpfs，宿主自带的文件只读绑定回去，不向宿主写回任何内容。
- 派活时限（#83）：provider 子命令（`makewand codex|claude|agy|grok|muse ...`）与 review、run、race、ensemble 未给 `--timeout` 时单次时限跟随 `--total-timeout`，两者都给取较小值，都不给时默认值不变（300 秒，ensemble 900 秒）。此前照抄 route 的建议（不带 `--timeout`）会让重活在 5 分钟时以退出码 16 结束。route 按任务类型与难度打印 `--timeout`/`--total-timeout`（review/test/research/doc 为 900/1800/5400 秒，implement 为 1200/3600/10800 秒），review 加 `--readonly`，`--json` 同样包含。守护进程跟随请求的期限（不再封顶 600 秒，安全上限 6 小时），拒绝长预算的旧守护进程改为独立运行；观察器只在任务自身的截止时间之后才判定挂起；交互会话重新标记为派发方；`--total-timeout` 为 0 或负数时明确报错。
- 有界派发改用 Go/Python 共享的机器账本 `call-budget.json`；任务上限之外，项目每日默认 200 次、机器每日默认 1000 次，按 Asia/Shanghai 午夜重置。已结算记录保留 48 小时，写入时清理；未结束记录、活动租约和预留所引用的计数保留，旧 lifetime 上限不因清理退款。详见 [预算与租约](docs/layered-call-admission.md)。（#84）
- 账号与订阅池默认并发 1，provider 默认并发 64；无可用回退时最多等待 30 秒（`MAKEWAND_LEASE_WAIT_SECONDS`）。busy 不启动 provider、不计费、不降低可靠性，返回 FAILED/退出码 10；真实预算耗尽返回退出码 13。某并发键没有活动租约后可重新配置并发上限。（#84）
- quota cache v2 按账号及订阅池绑定；未知账号不作为独立池。Claude 登录绑定仅投影 `.claude.json` 的账号 ID，正常项目历史/计数写入不使绑定失效；启动前账号校验拒绝不计费。Python Codex 使用 `codex exec --json` 提取协议证据。（#84）
- provider worker 采用严格命令白名单，仅允许 `search`、`repomap`、静态 `help`、`completion <shell>` 和单独的帮助/版本参数；`quota`、`status`、`route`、`mark`、`outcome` 等管理命令均拒绝。详见 [worker 策略](docs/PROVIDER_WORKER_GUARD.md)。（#84）
- 其他账号的 `CODEX_HOME`/`CLAUDE_CONFIG_DIR` 在 provider 沙箱中被屏蔽：不存在的外部根目录只有在可证明位于所有可写挂载之外时才跳过，首段之后含 `..` 的值视为有歧义而拒绝（两引擎一致）；运行期间切换登录时结果记为 UNKNOWN，不记到错误的账号；无 bwrap 直接运行的 codex 保留用户的 `CODEX_HOME`（配置、技能、hooks），并在运行前后核对账号绑定（#84）。
- Go/Python 统一支持 `proxy.default` / `proxy.no_proxy` 和 `MAKEWAND_DEFAULT_PROXY` / `MAKEWAND_DEFAULT_NO_PROXY`：缺少调用方代理变量时供所有 provider CLI 与 API 路由使用，不内置代理地址；Gemini 仅显式开关绕过代理，移除 muse guard 的固定端口，状态安全显示代理来源。（#84）
- 执行证据写入改用账本结算的写入时限（5 秒，原为固定 1 秒）：证据是持久的记账，写入失败会把已结算的结果变成 UNKNOWN；CI 中依赖时间窗的测试改为事件驱动（#85）。

### Security

- Go 工具链指令升级到 `1.26.10`（含 GO-2026-6809：crypto/x509 不再信任被攻破的中间证书）；Docker 构建镜像同步。

修复 2026-10-10 安全审计的全部未闭环项（Python 与 Go 两个引擎）：

- **启动器完整性**：`bwrap`、`systemd-run`、`prlimit` 只从 root 拥有、不可写的系统目录解析，不再经 `$PATH`（仓库内的 `.venv/bin`、`node_modules/.bin` 无法再冒充 bwrap）。
- **私有 provider 家目录**：宿主 `~/.claude`、`~/.codex`、`~/.gemini` 等不再可写挂载；每次调用使用私有副本，只把变化的凭据文件经校验后写回。
- **无沙箱时一律需要确认**：没有 bwrap 时，只读审查也需要 `MAKEWAND_UNSAFE_HOST_EXEC=1` 加确认并写入审计日志（Python 与 Go 一致）。
- **seccomp 默认强制**，Go 的 engine 启动器也已接入；Go 沙箱内 `.git` 元数据只读；工作区内的 Unix 套接字（含 `node_modules` 下）全部屏蔽；扫描不完整时可写会话 fail-closed。
- **应用与审查完整性**：候选应用改用 `renameat2` 交换/不覆盖，并发保存不再被覆盖；审查的 diff 原样传入并与交付绑定 sha256；AST 合并拒绝冲突的 import 绑定；交付指纹覆盖 `.makewand_*` 与依赖目录；任务产生的 `__pycache__`/`.pyc` 不再进入交付补丁。
- **计费与服务端**：组合调用计入失败尝试的花费；可选开启 `MAKEWAND_SCALED_BUDGET_RESERVATION=1`，让 HTTP 预算预留按定价、输出上限和扇出放大，并在流式输出超出预留时停止（默认仍为固定预留）；SSE 事件总量有上限；SQLite 默认 `synchronous=FULL` 与 `_txlock=immediate`。

2026-10-12 P1 修复批次（Python 与 Go 两个引擎；行为细节与残余风险见 SECURITY.md、docs/VERIFICATION_CONTRACT.md 与 docs/EXECUTION_CONTRACT.md）：

- **凭据回写只合并同账号的令牌刷新**：两引擎使用同一套规则和共享测试向量（`_credential_writeback_merge` / `cliMergeCredential`），以宿主文档为基础只拷入令牌刷新字段。登出、换账号或重定向（删除/清空令牌，改动邮箱、API key、端点、认证方式、账号 ID 等受保护字段）、JWT 换成他人账号或不透明令牌、更换 Google refresh token、时间倒退、令牌类型除大小写外的变化、非严格 JSON（含孤立的 UTF-16 代理转义）都整份拒绝，并提示一次（指明文件和字段，不显示值）。muse 只有 OAuth 登录记录中的 `api_key` 视为其自行铸造、可单独写回，保存或粘贴的 key 受保护；`flow_auth.json` 只拷入。写回在宿主文件旁的 `.<文件名>.makewand-lock` 上取 `flock`（最多等 10 秒；两引擎都不经符号链接打开该锁文件，Windows 上也不经重解析点，无法打开时不加锁、只靠复核）并在重命名前复核宿主文件；副本被删除/替换/超过 4 MiB/不可读、沙箱内新建登录（含宿主缺少整个 provider 目录）都会提示，不再静默跳过。Go 的审查与远端（`makewand serve`）会话也使用私有副本并经同一合并写回；`Chat` 与流式调用（包括因取消或超过期限而结束的）都等写回完成才结束（最多 15 秒），Router 的流式调用另预留 1 秒的进程退出/管道收尾时间（总等待上限 16 秒），尝试超时后的回退要等超时的流结束才开始；API provider 的流式调用不等待。不透明令牌无法离线绑定账号、JWT 签名不校验、登出无法防止等残余风险见 SECURITY.md。
- **依赖安装网络**：Go 候选验证的每个步骤都在独立网络命名空间中运行。依赖安装（及 Rust 检查前的 `cargo fetch`）在有可信的 `slirp4netns`、`nsenter`、`ip` 时获得只能访问公网的用户态网络：私网、回环、链路本地（云元数据）、CGNAT、组播、保留网段与本机地址在命令运行前即被路由屏蔽，不启用 IPv6，可选的 `tc`/`nft` 再丢弃组播与 IPv6；网络配置失败时沙箱在命令运行前被终止，步骤改为仅回环重跑。安装失败只有经 makewand 确认才算环境问题：联网时由 makewand 自身可执行文件在全新沙箱中探测默认注册表（go 按 go 命令的 `,`/`|` 规则遍历 `GOPROXY` 与校验和数据库：遇到 `direct` 时直连数据库，遇到 `off` 或列表结束时只有此前每个代理都回答 404/410 才直连，因此 `|` 越过一次失败之后数据库与 go 一样不可用，探针也不再访问它），离线时须由项目文件证明确有未安装的注册表依赖（go 依据宿主工具链 `go mod edit -json` 解析的 go.mod、go.sum、imports 与两处模块缓存）。候选代码打印的文字不能再伪造环境错误：go 的项目字符串取自项目中所有 go.mod、go.work 与 Go 文件，包括任何 `.makewand` 目录下的，只跳过 `.git` 与 makewand 的沙箱 HOME `.makewand/sandbox-home`，直接生成文件的写入受路径保护限制，测试与自动修复进程仍使用后者作为 HOME，并可写入缓存。
- **宿主模式交付**：只核查本次任务新建、修改或删除的路径（按类型、权限与内容判断，单纯 ctime/inode 变化不算修改）。审查 diff 之外的改动——被忽略文件的新建或改动、二进制/可执行产物、`.env*`/`.envrc`/`*.pth`/`sitecustomize.py`/`.husky`/`.githooks`、子模块或嵌套仓库中的文件、本检出及其子模块私有 hooks 目录中的新 hook——阻断交付并回滚。新 hook 移入 `rejected_*/quarantine/git-hooks/` 并附 `mv` 恢复命令，不再删除；链接工作树或仓库外 `core.hooksPath` 等共享 hooks 目录只报告、不阻断。用户的 diff 配置（外部 diff 与 textconv、颜色、`diff.relative`、路径前缀、`diff.context` 等）不再影响审查 diff、交付核查与导出补丁；回滚改用字面路径（`[ab].txt` 不再连带恢复 `a.txt`）。未备份的既有文件按 SHA-256 摘要与抽样指纹核对，内容无法确认时按“无法确认”阻断。
- **审查完整性**：竞速裁决行绑定本任务 nonce（`MAKEWAND_RACE_VERDICT_<nonce>`），候选 diff 中回显的裁决行无效；审查与竞速只认带本任务 nonce 的裁决行，不带 nonce 的行在任何环境下都不算，3.2.0 中未写入文档的 `MAKEWAND_ALLOW_LEGACY_VERDICT`、`MAKEWAND_STRICT_NONCE` 开关与测试环境（`MAKEWAND_TEST_ISOLATION_ROOT`）豁免均已移除。Winner-Take-All 胜者以及裁判选中的与其同一提供方的候选，都须经其他提供方只读复审通过；裁判与这位复审方按同一规则选择，都优先选独立于双方选手的提供方，复审方没有这样的选择时由另一位选手担任，从不由胜者自己的提供方担任。只采纳收到完整提示的裁决：超过 31 KiB 的提示不交给 agy（流水线改由下一位可用审查方接替），占位引擎（cursor、copilot）从不审查，本地模型的只读回答须由 token 计数证明读到了完整提示，OpenRouter 请求关闭提示压缩（`transforms: []`）。
- **测试不再改动源码树**：设置 `MAKEWAND_TEST_ISOLATION_ROOT`（测试环境）时，宿主事务、流水线、竞速、DAG（含单任务）与 `apply` 一律拒绝以 makewand 源码树、其子目录或上级目录为工作区（单一定义 `git_helper.harness_source_tree_conflict`）；测试只能连接自身绑定的回环端口，代理变量被清除，pytest 结束时列出被拦截的连接。
- **不经模板链接写仓库元数据**：makewand 建立的隔离副本（竞速、DAG 暂存区）与临时基线仓库在 `git init` 之后先把从 `init.templateDir` 复制来的 `config` 链接换成本仓库的普通文件，再写入 makewand 的 `user.name`/`user.email`（DAG 还有 `core.excludesFile`）；DAG 暂存区的 `info/exclude` 与临时仓库标记总写成新的普通文件，从不写穿符号链接或硬链接，makewand 自己不再改写共享的模板文件。`.git/info/attributes.mw_gate` 不经链接打开，那里有链接时 makewand 的 git 命令 fail-closed（`Cannot shield Git attributes`）。`info` 目录本身是链接时也先换成本仓库的空目录，属性屏蔽锁与临时文件不会写入共享模板；原生 Windows 的重解析点（含 junction）同样拒绝作为元数据根目录，并在子路径处替换。`git init` 自身仍会经链接写入其 core 设置，见 docs/VERIFICATION_CONTRACT.md。

### Added

- 递归保护：makewand 启动的 provider CLI 带 `MAKEWAND_PROVIDER_WORKER=1`，在其中仅允许 search、repomap、静态帮助/版本和 completion，管理命令也被拒绝。
- 新环境变量：`MAKEWAND_BWRAP_PATH`（非系统目录安装的 bwrap）、`MAKEWAND_ALLOW_NO_SECCOMP=1`（显式关闭 seccomp）、`MAKEWAND_SANDBOX_MEMORY_MAX`（默认 `8G`）、`MAKEWAND_SANDBOX_TASKS_MAX`（默认 `512`）、`MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT`（工作区扫描上限，默认 1,000,000）、`MAKEWAND_SQLITE_SYNCHRONOUS`（`FULL`/`EXTRA`/`NORMAL`）、`MAKEWAND_SCALED_BUDGET_RESERVATION=1`（`makewand serve` 按定价放大预算预留）。
- 新环境变量（2026-10-12 批次）：`MAKEWAND_REVIEW_DIFF_MAX_BYTES`（复审 diff 上限，默认 262144 字节，原固定为 64 KiB；无效值回退默认并警告一次）；`MAKEWAND_FINGERPRINT_FILE_LIMIT`、`MAKEWAND_FINGERPRINT_TOTAL_LIMIT`、`MAKEWAND_FINGERPRINT_SECONDS`（宿主模式摘要与抽样指纹上限，默认 64 MiB、512 MiB、20 秒）；`MAKEWAND_SLIRP4NETNS_PATH`、`MAKEWAND_NSENTER_PATH`、`MAKEWAND_IP_PATH`、`MAKEWAND_TC_PATH`、`MAKEWAND_NFT_PATH`（依赖安装网络工具不在 root 拥有的系统目录时，指定可信的绝对路径）。
- `makewand/cli_message_catalog.json`：各 CLI 自己打印的限额、限流、过载、登录与地区消息目录；Go 路由器内嵌字节相同的 `router/cli_message_catalog.json`，两侧共用对照语料 `tests/testdata/cli_message_parity.json`。Go 加载目录时校验各条目的流：会读到模型回答（`answer_streams`）的捕获不得使用逐行（`lines`）选择，否则拒绝加载。
- Go SDK `ExecResult.Note`：makewand 自己的说明（例如输出疑似注册表故障但未被确认的原因），也附在候选验证的 `DepsError`/`QuickCheckError` 之后。
- 使用账本按每次派发记录 `route`：claude、codex、agy、grok、muse 由适配器报告实际走订阅 CLI（`cli`）还是转用 API（`api`，包括 CLI 触发限额后再转用的），纯 API 引擎记为 `api`，直接命令（`makewand claude` 等）按同样规则记录；aider 的适配器尚不报告路线，只在配置不允许转用 API 时记为 `cli`，否则不记录（本地模型不记录）。经 API 完成的成功不会清除 CLI 的失败记录；配置了 API key 时，只有记为 `cli` 的成功才清除 needs_auth 记录。
- Python SDK：`CandidateManager.apply_candidate` 返回 `ApplyResult`，仍可按 `(ok, files, message)` 解包，`status` 给出类型化结果（`PASSED`、`APPLY_CONFLICT`、`TIMEOUT`、`UNVERIFIED`、`UNKNOWN`、`FAILED`）。
- 源码安装器完成后，若 `makewand` 命令找到的不是这次安装（安装目录，默认 `~/.local/bin`，不在 `PATH` 中，或 `PATH` 上更早的位置有另一个 makewand），打印要加入 shell 配置的确切 `export PATH=...` 行；官方安装脚本（makewand.org/install.sh）的最后一句随之改为先加 PATH 再运行 `makewand status`。

### Changed

- 行为变化：拒绝以 `/`、`/tmp`、`/home`、系统根目录、`$HOME` 或其上级目录作为可写工作区；provider 的会话历史不再持久化到宿主；Go 沙箱内的 agent 不能再修改 `.git`。
- 重写 agent skill `makewand-orchestrator`：按 CLI 实际行为说明命令、退出码 0–17、影子工作树交付与递归规则，收紧触发条件。
- 宿主模式交付被阻断时返回 UNVERIFIED（退出码 11，原为 10），失败原因列出未能恢复的文件；有文件无法复核时，回滚标题不再声称已恢复基线。
- 复审 diff 上限提高到 256 KiB；超出时不截断审查，结论为 UNVERIFIED，完整补丁保存为可直接 `git apply` 的 `unverified.patch`（`--binary --full-index --unified=3`，子模块补丁列入 `manifest.json`）。审查 diff 与导出补丁总带三行上下文。
- 测试产物只有一个定义（`git_helper.is_test_output_path`），测试关卡与交付核查共用：`coverage/`、`playwright-report/` 只在仓库根、任务目录或包根计，被 Git 跟踪的文件永远是测试输入。`run_local_tests` 删除本次运行在默认位置生成的测试产物（不进入审查与交付）；可作为代码执行的产物或链接使关卡失败并写明原因；测试前已存在的目录（含空目录）保留，被忽略的目录即使只含被测试删除的被忽略文件也保留（它们只记录、不展开，其下的子目录在这种情况下仍可能被删除）；被重写的既有测试产物只在是纯数据（含 Playwright 的 `.zip`/`.webm`）或字节不变时保留。
- 多阶段 DAG（`makewand plan`）在一份私有暂存副本中逐阶段执行，累积结果作为候选 `dg_…` 的 `A` 只交付一次；只读子任务审查此前的累积改动，其写入一律丢弃；原生 Windows 上多阶段 DAG 拒绝执行（请用 WSL2）。暂存区绑定的每份清单都受 `MAKEWAND_WORKSPACE_SNAPSHOT_MAX_FILES`、`MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES`、`MAKEWAND_WORKSPACE_SNAPSHOT_TIMEOUT_SECS`（默认 100,000 个文件、512 MiB、每份 10 秒）约束：文件数与字节数在计算摘要前统计，超限的仓库在复制任何内容前即被拒绝，不执行任何子任务，结果为 UNVERIFIED（退出码 11），消息指明超出的上限（如 `无法建立 DAG 私有暂存工作区…: 工作区 <根目录> 中需要计算摘要的文件超过 536870912 字节 (MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES)；可用 .gitignore 忽略数据、依赖与构建输出，或调高该上限`；慢盘上第一次读取可能超过时间上限）。冻结检查不再绑定被 gitignore 的未跟踪文件（被忽略目录中正在写入的日志不再中止 DAG），未改动文件的摘要在同一次运行的各阶段复用。
- 候选封存清单覆盖 Git 跟踪或会添加的全部文件（含依赖/构建目录名下的文件），旧记录仍可应用。候选新建而工作区已有的条目视为冲突（确认后才用 `--force`）；apply 无法写入的工作区条目（目录、符号链接、特殊文件等）即使加 `--force` 也报告冲突（退出码 14）。apply 的冲突检查把未跟踪目录中的每个文件都算作未提交的改动（与顶层的未跟踪文件一样，按提示先提交或确认后用 `--force`），文件名按原字节比较（首尾空格、非 UTF-8 文件名、暂存重命名的两侧都不再丢失）。`makewand candidates`/`inspect` 按实际情况标注主裁推荐、Winner-Take-All 胜出、旧版记录与 DAG 结果，只列出可行的操作。
- `makewand apply` 的退出码取自类型化结果，不再看消息里有没有“冲突”二字：测试环境拒绝与受保护文件校验失败为 11 UNVERIFIED（原 10），提交结果无法确定为 17 UNKNOWN（原 10），冲突检查之后、写入之前目标被改动为 14 APPLY_CONFLICT（原 10），`--merge` 合成失败（如语义合并冲突）为 10 FAILED（原 14）；每种失败都记录类型化结果。多阶段 DAG 的交付报告同样的状态：结果未知与受保护文件校验失败不再记为 FAILED，消息里只是出现“冲突”二字也不再算作冲突。
- 没有可用引擎时，流水线、解释/问答、REPL 与 `ensemble` 都不派发任何任务并逐个列出原因，结果为 UNVERIFIED（退出码 11，错误为 `No usable execution provider: <引擎>: <原因>; …`）。
- API 转用需要允许的 API 策略：限额、登录失效（needs_auth）或未安装的 claude/codex/agy/grok/muse 只在 `MAKEWAND_API_POLICY=allow_paid`（或 `api_policy` 为 `allow_paid`）时转用已配置的 API key；默认的 `subscription_only` 下从不转用：登录失效时照常运行 CLI，其余情况直接失败，配置了 key 时说明它被 API 策略阻止。路由、审查方选择与 `ensemble` 只在允许转用时才把这类引擎算作可用，只有被策略禁止的 key 时按不可用处理（全部不可用时退出码 11）；`status --probe` 把未安装 CLI、key 被策略阻止的引擎报告为 missing，不再报告为纯 API 模式健康。五个适配器共用同一策略与路线记录入口。
- `--provider X`（`run` 与自然语言入口；`run --workflow race --provider X` 中为选手 A）在工作区准备、路由与任何派发之前判定，解释/问答、流水线、审查与竞速路径一致：X 须满足路由对任何引擎的要求（已启用、没有生效的失败记录，除非适配器可把该状态转给允许的 API；纯 API 引擎须有 API 策略允许使用的 key）。不可用时不派发，结果为 UNVERIFIED（退出码 11，错误为 `Requested engine X is not usable: <原因>`）；可用时总会运行，即使其他引擎都不可用（此时作为唯一工具，按单工具的审查规则）；在审查路径上它是唯一的审查方。`auto` 不算指定。未知或占位引擎直接拒绝；没有健康记录的 CLI 引擎仍须通过安装或允许的 API key 检查；交互式 REPL 中 `/model X` 之后的问答仍不经这一检查直接派发给 X。
- `makewand race` 让 claude、codex、grok、muse、local、agy 中任意两个不同的已启用且健康的引擎组成一对选手（如 codex 与 muse、grok 与 muse；agy 只要不处于已知失败状态），每个位置按自己的偏好顺序选取，此前 muse 与 codex 不能担任选手 B；无法组队时列出每个可参赛引擎（含被禁用的）以及其他已接入的引擎，各附原因。
- `status --probe`、`probe`、`quota --probe` 在 agy 有未过期的失败记录或额度限制时做一次真实的 `agy -p "echo ok" --mode plan` 调用，只有成功或新的明确结论才替换旧记录。
- 限额与登录判定只依据各 CLI 自己的消息（上述目录）：两引擎读取相同的输出流与行，跳过 glog、代码块、缩进行、codex 的提示回显与转发的命令输出；流式模式（含原生 Windows）把 stderr 与回答分开捕获。Go 只读 stderr 末尾 8192 行/1 MiB；目录中没有的自定义 CLI 使用通用锚定模式，`token limit exceeded` 之类的措辞也可能把它封停 30 分钟。
- 依赖安装：没有锁文件的 npm 加 `--no-package-lock`；含 `pyproject.toml` 或 `setup.py` 的 pip 安装加 `--use-pep517`，egg-info 写到工作树外；有 `Cargo.lock` 时所有 cargo 步骤带 `--locked`，没有时删除 cargo 生成的 `Cargo.lock`；声明了依赖的 Rust 项目先 `cargo fetch`。TUI 在依赖无法下载时跳过测试，不扣分、不自动修复。
- 安装器只安装一份 `makewand-orchestrator` 技能；仍声明同名的旧 `trio-orchestrator` 副本会保持更新并提示删除。
- 文档：SECURITY.md 与 README 写明 v3.2.0 之后加入的保护（#75、#80–#85）在 `master` 源码中、随 v3.3.0 发布，v3.2.x 构建（Release 压缩包、Homebrew、Scoop）没有；支持版本表与“v3.1+”的说法相应更正。SECURITY.md 也写明候选的测试与自动修复轮次以 `.makewand/sandbox-home` 为 HOME，会写入其中。
- CI/发布：Release 的 test 作业改为调用 CI 的 verify 作业（`workflow_call`），未经 CI 的标签也要通过 PR 必须通过的全部检查（gofmt、golangci-lint、shell 语法、契约与 trusted acceptance 等），标签须等于 `v` + `__version__`；govulncheck 固定为 v1.9.0（漏洞库仍实时获取）；网站部署后在同一个 3 分钟期限内核对 makewand.org、www.makewand.org、makewand.com、www.makewand.com 与 makewand.pages.dev 上的 install.sh；`make prelaunch` 改用 Go 的私有用户缓存（原为任何本机用户都能预先写入的 `/tmp/makewand-gocache`），并与 CI 一样运行全部三个离线基准脚本。
- 文档：README、`makewand-orchestrator` 技能与网站文档补上 route/mark/outcome；PACKAGE_DISTRIBUTION（tap 与 bucket 已是 v3.2.0）、Cloudflare 部署手册（实际发布路径是 deploy-site.yml）、RELEASE_STRATEGY（Release 还等待原生 Windows 关卡）与 PRELAUNCH 按实际情况更正；GITHUB_HARDENING 区分已生效的分支保护与 `scripts/github_hardening.sh` 的目标基线，脚本中的 CodeQL 检查名更正为实际上报的 `Analyze (go)`，并把 Release 需要的 `native-windows / verify` 列入目标基线。

### Fixed

- Go 路由：#84 起普通 CLI 退出一律算结果未知，回退停止，Power 模式丢弃已有回答。现按证据区分：CLI 自己打印、由消息目录识别的拒绝（未登录或凭据被拒，agy 拒绝参数）是结果已知的失败（状态 FAILED，退出码 10），照常回退并计入熔断（熔断后的半开探测失败也会回退，冷却时间逐次加倍）；可能写入工作区的运行只有在 stdout 没有回答、且拒绝是 CLI 输出的第一行时才算拒绝，运行途中登录失效仍为未知并停止重派。只读运行（沙箱内的审查、未指定 WorkDir 的 `serve` 远端请求）的未识别退出仍记为未知，但可回退到下一个 provider；宿主机上的审查、指定了 WorkDir 的远端请求以及其他可写运行的未识别退出仍为未知并停止重派。Power 模式某个生成方或裁判结果未知时返回其他生成方的回答，未知尝试保留在 trace 与用量中；有生成方被预算或记账拒绝时仍结束请求。
- Go 路由：凭据准入快照改在拿到账号租约之后拍摄；已知账号（codex `account_id`、Claude `accountUuid`）按账号比对，不再比对令牌字节，持有租约的运行刷新同一账号的令牌不再让排队的运行报 "selected login changed after admission"；等待期间换成其他账号仍拒绝，并在启动前释放预留。
- Go 配额缓存按来源逐个读穿：仍新鲜的条目直接使用，只读取缺失或过期的来源；"无证据"的读取（原装 Codex 日志、未安装 agy、未知 Claude 账号）沿用 10 分钟，缓存新鲜时 `makewand quota`、TUI 与 `serve` 启动不再全量读取；运行中的后台刷新每次都重新读取自己上一次读过的有证据来源，其他进程此后的读取仍直接使用；其他账号的缓存条目从不使用。
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
- 测试关卡的 `.gitignore` 回退（`git check-ignore` 无法运行时使用，如工作树外或子模块内的路径）改为按 git 自己的规则逐字节匹配：`*`、`?` 不匹配 `/`，`**/`、`/**`、`/**/` 跨目录，字符集支持范围、取反与 `[:class:]`，`\` 转义，尾随空格、CRLF 与 UTF-8 BOM 按 git 的方式处理，被忽略目录中的内容不能被 `!` 重新包含（原先的 fnmatch 近似常与 git 的结论不同）。含大量 `**/` 或 `*` 段的病态模式遇到很深或很长的路径时，回退可能非常慢，或以内部错误结束运行。
- 宿主模式指纹：任务开始时的摘要与抽样指纹按大小预算接纳的顺序读取（摘要从小到大，抽样按大小分组轮流），时间预算用尽时丢下的文件与字节预算用尽时相同，大文件不再只因路径排序靠后而拿不到抽样。
- 复审 diff 上限、提示完整性上限与本地模型的上下文窗口统一按实际派发的文本计量（`review_contract.dispatched_size`：无法解码的文件名字节按其 4 字节的 `\xNN` 转义计算），此前三处的计法各不相同。
- `status --probe` 的帮助写明 agy 做真实调用的两种情形：额度限制尚未重置，或真实派发、此前的真实探活记录的登录/地区失败仍然有效。
- TUI：因依赖无法下载而跳过测试时，进度步骤的说明按界面语言显示（中文为“已跳过（依赖无法下载）”）。
- Python 测试关卡（CI、Release、`make prelaunch`，以及装有 pytest 时的 `make test-py`）改为 `python3 -I -m pytest -q -p no:cacheprovider tests`，pytest 固定为 9.0.2：原先的 unittest 发现收集不到只有模块级 pytest 测试的 10 个模块（96 个函数、416 个用例，多为 #83/#84 的回归测试），它们从未在任何关卡运行。只用标准库的 `scripts/test_python.py` 遇到 unittest 跑不到的 pytest 风格测试、没有测试的指名模块或一个测试也没收集到时以失败退出，不再静默通过。
- 每次发布说明中的源码安装命令改为 `curl -fsSL https://makewand.org/install.sh | bash`：原先的 `curl …/scripts/install.sh | bash` 从管道读取时 `BASH_SOURCE[0]` 未设置，安装根变成 `/`，什么也装不上。`scripts/install.sh` 经管道、`bash -s` 或 `/dev/fd` 运行时先于任何工具检查退出，并给出这条命令。

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
