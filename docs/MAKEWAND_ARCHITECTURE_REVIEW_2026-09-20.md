**Makewand 独立架构与红队代码评估｜2026-09-20**

**执行结论**

Makewand 的产品方向可行：利用现有 CLI 做任务编排、故障切换和交叉审查，能够降低接入成本。但当前 Python v3 应定位为实验性编排工具，不宜按“安全、可信、无人值守的自动交付引擎”验收。主要风险不是模型不够多，而是执行权限、候选基线、审查判定、失败状态和进程生命周期没有形成可靠契约。

Go 资产值得保留，已有实质性的隔离、审批、进程组清理和测试积累；这些保护没有自动传递给 Python。建议保持 Python 编排，抽出 Go 执行与验证后端，避免同时维护两套安全策略。先实现一个模型、一份候选、一次可信验证的闭环，再扩展 Race 和自动组队。

建议排序：建议 1 的安全止血为 P0、完整集成为 P1；建议 2 的搜索与快照预算为 P1；建议 3 的自动组队为 P2；建议 4 的失效测试修复为 P1 快速项，不能据此直接改写生产窗口算法。

**一、审查边界与证据等级**

- 基线：HEAD `b036a1f4327ce69113416fc58a8daca3c4c817d9` 加当前工作区内容。
- 审查开始时，`makewand/cli.py` 和四个 Provider 文件已有未提交修改，另有未跟踪的 `security_best_practices_report.md`。这些内容全部保留；本报告不以既有报告为结论依据。
- 范围：Python 编排全链路，以及与问题直接相关的 Go quota、runner、审批、SQLite 和发布测试路径。Server 包执行了选定测试，但本次不是全部 HTTP 端点、依赖或前端的完整安全认证。
- “实测”指现有测试或临时目录中的受控复现；“静态确认”指可由调用路径直接确定；“设计风险”指需进一步故障注入或真实 CLI 契约验证的问题。
- 没有发起真实模型推理，没有验证订阅套餐、真实模型版本、服务质量或费用。文案中的产品归属、模型名与“零额外 Token 计费”不作为已验证事实。
- 本次只新增本报告，没有实施产品代码修复。

**二、现有测试与受控复现**

| 检查 | 结果 | 能证明什么 |
|---|---|---|
| `python3 -m unittest discover tests -v` | 10/10 通过 | 字符串解析、档位判断、基础 Git 行为通过；不覆盖 pipeline、race、进程取消和并发缓存 |
| 9 个 Go 包普通测试 | 8 包通过；router 因 `TestCodexBothWindowsResets` 失败 | 已检查 Go 资产可编译；不能把历史“无法编译”结论沿用到当前树 |
| router、engine 选定 `-race` 测试 | engine 通过；router 同一时间断言失败，未出现 race detector 报告 | 已执行路径没有检出的数据竞争；不等于跨进程缓存正确 |
| Go overlay 只把该用例两个 reset 改为相对未来时间 | `TestCodexBothWindowsResets` 通过，生产代码和仓库测试文件未改 | 支持“过期 fixture”这一失败根因，而非需要更改窗口解析算法 |
| `TestEvaluateCandidateFiles_LiveBwrapIsolation` | 实际运行通过，未 skip | 本机 Go 候选隔离路径有有效的 live smoke 证据，不代表 Python 已受保护 |
| 静默子进程，stream=True，timeout=0.1 秒 | 约 0.744 秒后 exit=0、无 timeout | 超时检测可被阻塞读取绕过 |
| 父进程 timeout=0.15 秒，孙进程延迟写临时标记 | 父进程超时后标记仍产生 | Python 超时没有清理整个进程树 |
| race 副本不做修改 | 原文件仍出现在 diff 中 | 缺少任务起点基线 |
| race 副本删除唯一原文件 | diff 为空 | 删除可以完全漏审 |
| 副本内嵌指向临时工作区外文件的 symlink | 外部内容被复制为普通文件 | 复制不保持 symlink 语义，也不限制读取范围 |
| 两个缓存写者从同一旧版本读入，分别更新 Claude/Codex | 后写者使 Claude limited 退回 unknown | 可确定复现丢更新；不需要依赖随机竞态 |
| 写入 2020 年已到期 limited 状态后正常读取 | 仍 limited，未探测 | reset 字段没有参与恢复 |
| `AssertionError: expected 429, got 200` | 两个 quota parser 均判为限流 | 业务输出与 Provider 额度错误混淆 |
| `[P0] arbitrary host command execution` | 缺陷判断为 False | 严重级别漏报 |
| `LGTM; race condition in worker` | 缺陷判断为 False | 通过关键词可以压过部分缺陷词 |
| 模拟实现成功、两个审查者均失败 | 函数正常返回且打印全链路完成 | 审查不可用时放行 |
| Claude/Codex/Muse limited，仅 AGY 可用 | AGY 实现和审查各调用一次 | 跨模型不变量不成立 |
| `makewand quota --probe` | 实际调用 `force_probe=False` | CLI 分支覆盖了用户参数 |

普通 Go 检查命令：

```bash
go test ./router ./internal/engine ./cmd/makewand ./internal/tui \
  ./serverdb ./serverauth ./serveradmin ./serverui ./serverusage -count=1
go test -race ./router ./internal/engine \
  -run 'Quota|Isolation|Exec|Approval|CodexBothWindows' -count=1
go test ./internal/engine \
  -run '^TestEvaluateCandidateFiles_LiveBwrapIsolation$' -count=1 -v
```

受控复现使用 mock Provider、临时配置文件和临时工作目录，避免把审计实验变成真实编码任务。临时复现脚本位于 `/tmp/makewand_arch_review_repro.py`；普通 Go 和 race 日志分别位于 `/tmp/makewand-review-go-tests.log`、`/tmp/makewand-review-race-tests.log`，它们不是永久交付依赖。

**三、维度一：整体功能与架构**

**3.1 Python 与 Go 的真实关系**

`scripts/install.sh:15` 把命令链接到 `bin/makewand`；后者导入 Python `makewand.cli.main`。Python 调用图是 CLI → orchestrator → 四个 Provider → subprocess。没有调用 Go `RunVerificationPlan`、TUI approval 或 SQLite run ledger。

Go 入口在 `cmd/makewand`，仍有自己的 Router、TUI、Server、配置和测试体系。当前是同仓两个产品运行面，而不是已经完成集成的“Python 控制面 + Go 安全执行面”。Go 的测试数量和沙箱能力不能替 Python v3 背书。

适配器共用 runner、使用 argv 列表调用模型、签名大体一致，是合理的 MVP 起点。正常非流式路径使用 `subprocess.run`，会通过 communicate 消费 stdout/stderr，不能笼统断言这条路径必然发生双管道死锁。真正缺失的是结构化执行契约：结果目前只有 `(bool, output, error)`，没有实际 Provider/model、权限策略、错误类别、配额窗口、候选 ID、验证证据和取消状态。

`--model` 被同样传给不同厂商，fallback 时可能把一家模型名交给另一家；修复轮又不保留该 override。Codex 的日志固定显示 gpt-6-astra，但未指定 model 时实际采用 CLI 本地默认；`discovery.py:10` 的展示也包含静态默认值，且 discovery 不控制执行。档位和底层参数是否生效，需要按支持的 CLI 版本做契约测试，不能从打印文案推断。

“零额外 Token 计费”也不是现有代码能保证的不变量：runner 继承宿主环境与 CLI 配置，没有验证实际认证/计费通道，没有禁止按量 API 路径或记录实际消费。更准确的产品表述应是“优先复用订阅 CLI，实际额度和计费由认证模式、套餐及供应商决定”。本结论针对缺失的代码约束，不推断任何厂商当前价格或政策。

**3.2 P0：执行与交付完整性问题**

P0 表示应阻断当前默认无人值守使用，不表示已经证明存在远程零点击利用。

**F-01｜默认绕过权限，审查与探活也缺少独立边界。静态确认。**

影响：模型执行项目中的不可信指令或生成错误命令时，可以以启动用户的权限影响工作区外的文件与资源。

证据：`providers/codex.py:37`、`claude.py:37`、`agy.py:27`、`muse.py:35` 默认加入 bypass/yolo；`providers/base.py:28`、`:40` 未建立外部隔离，也未显式裁剪环境。`orchestrator.py:151` 使用同一可写 Provider 执行审查；`health.py:107` 的 Muse 探活同样 yolo，并继承当前目录。Git helper 对根目录的限制只影响 Git 初始化，pipeline 忽略返回值，不能阻止模型在危险工作目录执行。

`cwd` 只改变工作目录，不限制访问范围；独立线程、独立 CLI 进程、复制目录均不提供权限隔离。argv 列表降低了 wrapper 层 shell 注入风险，但不能限制模型内部发起的工具命令。

处置：默认关闭无外部保护的 bypass；审查使用冻结候选的只读视图；统一执行策略覆盖 run、review、race、probe 和修复阶段。权限不足时返回明确的不可执行状态。

**F-02｜超时、信号和进程树清理不成立。实测 + 静态确认。**

影响：任务超时或外层退出后，Agent 后代仍可能继续修改文件，后续 fallback、审查或清理会与其重叠。

证据：`providers/base.py:52` 先阻塞 `readline()`，到 `:62` 才检查时钟；若静默退出，`:53` 又会在检查超时前直接 break。stream 超时时只 kill 主进程，不 wait；异常路径缺少 finally。非流式 `subprocess.run` 会杀死并回收直接子进程，但不保证清理孙进程。两个路径均没有独立会话/进程组取消协议，`KeyboardInterrupt` 也不被 `except Exception` 捕获。

其他边界：输出全部保存在内存，单条超长无换行输出会扩大内存与等待；stream 分支忽略 input_text；stdout 写入被阻塞也影响时限；pipeline 没有总 deadline。Race 的 `Future.result()` 无独立时限，executor 退出通常仍等待运行中的任务；取消 Future 本身不能终止模型进程。裁判调用还没有透传 race 的 timeout（`orchestrator.py:341`）。

处置：统一监督器使用 monotonic deadline、异步读或 selectors、有界输出；取消时 TERM → 宽限期 → KILL → wait/reap，并处理 SIGINT/SIGTERM。进程组是基础，针对自行 setsid 的后代需要 cgroup 等更强的生命周期边界。输出泵退出后才能冻结候选。

Python 官方文档对 `run(timeout=...)` 的保证是直接子进程被终止并等待，不应外推成完整进程树保证：[subprocess 文档](https://docs.python.org/3/library/subprocess.html)。

**F-03｜审查和 Auto-Fix 以默认成功结束。实测 + 静态确认。**

影响：未完成审查或仍带严重缺陷的变更可被上层自动化当作成功交付。

证据：`orchestrator.py:41` 的字符串判定遗漏 `[P0]`，将无关键字文本视为无缺陷；`:167` 审查失败后跳过；`:199` 修复失败后 break；`:232` 无条件打印成功。达到 max_fix 且缺陷未解决同样没有失败状态。程序没有执行确定性测试，只在 prompt 中要求模型“确保单测全通”。

处置：显式状态 `PASSED / FAILED / UNVERIFIED / CANCELLED / BUDGET_EXHAUSTED`；审查结果缺失、解析失败、复审失败和修复耗尽均不得成为 PASSED；状态映射到稳定退出码。结构化 finding schema 必须保留 severity、文件、证据和候选 ID，且机器测试与策略检查优先于模型意见。结构化输出可以改善解析，仍不能把模型声明当成事实。

**F-04｜Race 固定目录与真实工作区多写者冲突。静态确认。**

影响：两个 Makewand 实例可互删仍在使用的候选目录，产生丢失、交叉污染或错误裁决。

证据：`orchestrator.py:277` 固定使用系统临时目录下 `makewand_race_agent_a/b`，`:281` 无所有权检查删除。结束路径无清理/保留协议。普通 pipeline 也直接写用户目录，没有仓库级写锁；失败模型可能已经部分修改，fallback 继续在该混合状态上实现。

处置：每个 run 使用不可预测的私有临时目录和 owner/run manifest；停止全部相关进程后再按所有权清理；真实工作区一个写者，候选并发写入各自副本；失败 attempt 的部分结果默认不成为下一 attempt 的隐式基线。

**3.3 P1：调度、隔离语义与数据正确性**

**F-05｜“影子 Git”没有稳定基线，也不是恢复系统。实测 + 静态确认。**

`git_helper.py:42` 在原目录直接 `git init` 并修改 local config，`:55` 对原 index 执行 `git add -N`，没有独立 shadow git-dir、初始 commit、preimage 或恢复日志。初始化失败也可以返回 True。`get_git_diff` 把错误与无变更都折叠为字符串，不能可靠判定审查范围。

Race 副本排除 `.git` 后新建一个没有 HEAD 的仓库（`:61`），因此所有现存文件看起来都是新增；删掉原文件则可完全漏报。若 Agent 自己 commit，按当前 HEAD 取 diff 也可能看不到本次实现。既有仓库则把调用前用户的 dirty/staged 改动混入审查。

处置：记录明确的 BaseSnapshot，比较候选与该基线，覆盖 dirty、untracked、删除、权限和 symlink；审查读取不应改变用户 index。Git 可以作为差异工具，但应使用隔离 metadata，且先关闭/控制外部 diff 与 textconv 等可执行扩展。把“容灾”留给经过恢复演练的 journal/backup，而不是仅能初始化 Git 的 helper。[Git diff 官方语义](https://git-scm.com/docs/git-diff)。

**F-06｜副本构造可能全量复制大目录、越界读取并吞掉失败。实测 + 静态确认。**

`git_helper.py:69` 只过滤顶层三个名字；`copytree` 未提供递归 ignore，默认解引用内部 symlink，虚拟环境、归档、大库、模型权重和嵌套缓存都可能进入复制。顶层文件 symlink 也可能经 copy2 解引用。复制异常被吞掉，候选可能是不完整仓库；没有文件数、单文件、总字节或总时间限制。路径位于临时根或包含旧候选目录时还需防止自包含复制。

处置：安全快照遍历器显式处理 symlink、特殊文件、路径包含关系、总预算与排除清单；不完整快照必须报告，不能拿截断候选声称完整审查。

**F-07｜额度机制属于错误文本驱动的降级，不是完整的 Quota-Aware Routing。实测 + 静态确认。**

`codex.py:11`、`claude.py:11` 依赖非结构化错误关键词，裸 `429` 会把失败测试日志误判为额度错误；overloaded、频率限制与硬额度耗尽混用。reset 只保存人类文本，没有绝对时间、时区、窗口、来源或置信度。

`health.py:122` 只有 force_probe 才更新；Provider 在发现 cached limited 时直接返回，完全不检查 reset 是否已过去。这会把短期限制变成长期不可用。AGY 的 `--version` 成功被当成 healthy，但只能证明二进制可启动。Claude/Codex 探活本身调用模型，消耗额度并可能受当前仓库配置影响。四家状态判定不统一：普通 pipeline 对 Claude/Codex 仅排除 limited，而 Race 必须 healthy。`cli.py:211` 还使 quota --probe 不生效。

处置：区分 installation/auth、quota、rate、overload、network、timeout、task failure；优先结构化事件或可信用量来源；对未知状态保持 unknown。记录 UTC reset、source_at、next_probe_at；采用退避和单探针半开恢复。额度状态按账号/认证来源/模型作用域区分，不应只按四个固定名字缓存。调度开始时的一次快照也不能替代每个 attempt 的准入。

**F-08｜Python 缓存可损坏、丢更新；Go 跨进程缓存也有边界。实测 + 静态确认。**

`health.py:51` 直接以 w 截断主缓存并写 legacy 副本，无锁、原子替换、版本或 schema 校验，异常全部吞掉；读失败后恢复 unknown，会重新启用受限模型。原子 rename 只能防半写文件，不能单独解决两个旧快照的丢更新。`dict(DEFAULT_CACHE)` 是浅拷贝；当前主路径多为替换整条记录，暂未据此认定独立的运行故障，但共享嵌套默认值应消除。

Go 的 `QuotaSnapshotter` 已用 `refreshMu` 和 atomic snapshot 改善进程内同步；不过 `router/quota.go:524` 用固定 `path + ".tmp"`，多个进程同时写同一缓存仍可能互相截断、重命名或读取到不完整内容。此为静态确认的跨进程风险，本次未做崩溃竞争压力复现；Go race detector 不覆盖它。

处置：单写者状态服务，或锁住 read-modify-write 全事务并用独占创建的唯一临时文件 + replace；主状态与 legacy 投影分离；需要复用 SQLite 时用事务/upsert 和明确 schema，不要直接让 Python 操纵现有账本私有表。

**F-09｜“独立盲审”与质量竞赛的实际保证不足。实测 + 静态确认。**

初始分配只避免大部分 Codex 自审，AGY fallback 可同时实现和审查（`orchestrator.py:159`）；修复降级至 Codex 时，原 Codex reviewer 没有重新分配（`:192`、`:212`）。不同 CLI 也不自动等于不同底层模型、只读权限或独立证据。

pipeline diff 截断 4500 字符，review 截断 6000，race 每边 3000。尾部缺陷可能不进入输入，亦无完整覆盖清单。Race 裁判知道选手名字，可以与选手同为 AGY；候选失败状态和测试证据没有进入裁判输入。它统计时间和 diff 长度（Python len(str) 也不是真实字节数），没有客观代码质量评分或可靠应用胜者的事务路径。

处置：候选 ID + 匿名标签 + 完整文件清单/分块审查 + 只读 reviewer；每次修复按实际实现者重选审查者。没有独立 reviewer 时明确 UNVERIFIED。先用可重复的 build/test/性能门槛筛选，再让模型解释差异；被审代码与日志应视为不可信数据，不能赋予其改变审查规则的权限。

**F-10｜交付门禁与版本入口分裂。静态确认。**

`Makefile:8` 只跑 Python 10 个测试；现有 GitHub CI 和 test_gate 主要运行 Go，没有 Python 测试步骤。CI 的“拒绝本地绝对路径”步骤（`.github/workflows/ci.yml:25`）会命中当前 `README.md:62` 的本地安装路径，早于后面的测试阶段失败。这里是按当前文件重现匹配规则的结论，并未查询线上 Actions。

安装器指向可变源码目录，没有独立版本化 Python 包；CI 构建的是 Go 同名二进制。用户安装渠道不同，会得到不同命令语义、安全能力与配置。修复 Go 不会自动修复 Python 运行路径。

处置：明确唯一对外入口及 backend/version 握手；测试入口同时覆盖 Python 和 Go；发布验证覆盖安装产物而不只是源码测试。

**3.4 Go 资产的价值与限制**

- `internal/engine/sandbox.go:142` 有验证阶段的 fail-closed 门槛；unsafe host 路径需要环境请求和确认对象。
- `sandbox.go:314` 对输出做上限，`:343` 建立进程组，`:361` 超时杀进程组；优于 Python 当前实现。
- `internal/tui/approval.go:121` 把自动审批与隔离可用性联系起来；autopilot 写入还参考 verified 状态。这是可复用的策略基础，不是外部 Agent 每条工具指令的通用拦截器。
- `serverdb/sqlite.go:29` 按连接设置 busy_timeout、foreign_keys，并启用 WAL；相关包测试通过。Python 没有复用这些持久化契约。
- `verify_isolation.go:163` 仍使用只读挂载整个宿主根。只读不等于不可读；HOME 之外或未被掩蔽位置的可读数据仍可能暴露。依赖步骤允许网络；wrapper 没有显式加入 PID namespace、seccomp、CPU/内存/PID/磁盘配额。self-test 使用无 context 的 exec（`:31`），其自身也需要时限。

因此不能把现有 bwrap wrapper 原样套在任意 Agent 外面，就称为完整“容器级安全”。本机 live smoke 通过只覆盖该测试中的攻击面。bubblewrap 官方明确把安全性依赖于调用方的参数与策略设计：[bubblewrap 官方说明](https://github.com/containers/bubblewrap)。

**四、维度二：四项优化建议的批判性评审**

| 建议 | 必要性判断 | 可行性与复杂度 | 推荐优先级 |
|---|---|---|---|
| 1. Go 沙箱与审批接入 Python | 必要，是可信自动执行前置条件 | 可行；完整 Agent 网络/凭据/工具边界使其为高复杂度 | P0 止血，P1 集成 |
| 2. 全局 Shell/搜索护栏 | 必要，但“杜绝”承诺过强 | 有界搜索工具为中等复杂度；覆盖任意 Shell/内置读文件为高复杂度 | P1；失控资源硬上限纳入 P0 runner |
| 3. 自动拉 Codex 组队 | 有条件价值，不是基础设施修复 | 显式单次委派简单；可靠自动触发、共享预算与防递归为中高复杂度 | P2 |
| 4. QuotaRefresh 窗口修复 | 修复红测必要；需纠正故障归因 | 测试时钟修复低复杂度；窗口状态机完善中等复杂度 | P1 快速项 |

**4.1 建议 1：支持复用 Go 能力，不支持直接“加一层 bwrap”**

推荐边界：Python 负责选择、重试、review/fix 状态机；独立 Go runner 负责工作区策略、进程监管、隔离能力、冻结/验证与受控应用。通过版本化 JSON/NDJSON 的 stdio 协议交互即可，第一版不需要引入常驻网络服务或复杂 RPC 平台。不要让 Python 驱动 Bubbletea 界面来复用审批。

请求至少包含 run/attempt ID、角色、Provider argv、快照 ID、策略 ID、允许的工作区和时限预算；响应应返回实际身份、退出/取消原因、结果状态、输出截断情况、候选 hash 和证据引用。策略由可信控制面决定，模型不能通过改请求自行提升权限。

关键陷阱有五项：

1. 外部 CLI 要联网到模型服务，项目工具又不应任意外连。整个进程树共享网络时，无法靠一个 unshare-net 参数同时满足两者。长期应分离可信模型连接通道与受限工具执行；CLI 不支持分离时只能声明较弱保障，并降低其自动执行等级。
2. CLI 需要认证，但把整个 HOME、SSH agent、云凭据、Docker socket 或生产 env 挂入沙箱会扩大攻击面。最小 runtime 挂载、专用 HOME、受控凭据通道和出口策略需一起设计；同一 Agent 进程可读的凭据不能凭名称变成秘密。
3. 把可疑仓库测试放入沙箱仍须 CPU、内存、进程数、输出、时间与磁盘预算。bwrap 本身不替代 cgroup，也不是独立内核或虚拟机。
4. Go 审批目前面向它掌控的文件写入和验证计划；外部 CLI 内部 shell/file 工具不经过这些回调。没有工具协议或可靠外部边界时，“深度集成审批”只能约束整个 attempt，不能声称逐命令拦截。
5. 候选验证后还需进程已停止、artifact 已冻结、真实工作区未变化的证明；否则验证与应用之间存在 TOCTOU。跨平台能力不同，不能在 bwrap 不可用时悄悄降为 unrestricted。

建议重新明确模式语义：manual 对影响确认；safe 在受限候选环境内自动执行，验证后受控应用；autopilot 只扩大预授权范围，保持相同隔离与验证不变量。人工点击批准并不能修复一个缺失的隔离边界。

验收：受控逃逸 fixture 不能读宿主秘密、写候选之外路径或访问未授权出口；超时/取消后没有后代；禁用隔离能力时零受限任务启动；review 不能写候选；验收与应用 hash 一致。

**4.2 建议 2：把搜索当有预算的能力，不把 shell alias 当安全策略**

问题成立，而且当前最直接的 I/O 放大点包括 Python copytree、Git add/diff 和 Go 全量 session 日志扫描，不只是 grep。`quota_helpers.go:48` 遍历所有 JSONL 后才选最近 8 个；`quota.go:787` 每个日志整体 ReadFile，context 不能中断这些内部扫描步骤。额度探测本身也需要 I/O 预算。

推荐先实现 repo-scoped search：显式根目录、默认不跟随 symlink、默认跳过二进制和约定大目录；设置单文件/总字节、文件数量、输出数量和 wall-clock 上限；优先 rg --files 建立范围，再定向搜索。保存排除清单和截断原因，允许有记录的单次扩围；源代码被排除时必须影响审查完整性判定。ripgrep 原生忽略机制是良好默认，但不是不可绕过的权限边界：[ripgrep 官方指南](https://github.com/BurntSushi/ripgrep/blob/master/GUIDE.md)。

不建议全局替换 grep、仅修改 .bashrc 或仅加提示词：非交互 shell 不一定加载；绝对路径、rg -uuu、Python open/os.walk、Agent 原生搜索均可绕过。要做强约束，需要 tool broker 或文件系统可见范围和资源控制兜底。

排除也有误伤风险：SQL migrations、测试数据库 fixture、模型配置文本和虚拟环境诊断可能是任务对象。应按类型、大小和目录语义组合规则；“检查被跳过”与“检查通过”必须分开。SQLite 生产大库默认不扫描，但授权查询应使用受控的数据库工具或一致性副本。

**4.3 建议 3：先做受控专家委派，再讨论自动突击队**

第二模型对高难问题可能提供不同假设，但也会增加额度、延迟与协调成本；使用相同资料不保证错误独立。当前多个无约束模型并行写入只会放大 F-01～F-09。

可先提供显式的只读二次审查和隔离的单次修复任务：输入失败命令、精简日志、任务范围、基线 hash 和验收标准；输出 patch + 机器可验证结果，由主控制面应用。对各类外部业务仓库应先检查各自仓库约束、数据敏感范围和失败基线，不能推断其具体适配成本。

自动触发需要明确门槛，例如同一失败完成有限次独立修复仍无进展，或用户已启用架构二审策略；先过滤依赖缺失、测试不稳定和环境故障。设置最大深度、最大 fan-out、每 Provider 并发上限、全局 deadline/额度预算与去重 run ID。禁止 Codex → Makewand → Codex 无限递归，子任务权限不得超出父任务授权。

“自动拉取”应区分调用已安装且已登录的 CLI 与下载/安装新工具。前者可按既定策略准入；后者涉及版本和供应链，不应由模型自由执行安装脚本。上线前用固定任务集比较一次成功率、人工返工、总调用量与 p95 延迟，证实净收益再自动化。

**4.4 建议 4：失败主要来自过期 fixture，不是已证实的 QuotaRefresh 边缘窗口错误**

`router/quota_fix_test.go:129` 写死两个 reset：

- `1784600000` = 2026-07-21 02:13:20 UTC。
- `1785000000` = 2026-07-25 17:20:00 UTC。

在审查日期 2026-09-20，两者都已过去。`router/quota.go:110` 的 SoonestReset 明确只选未来窗口，所以返回零值符合当前契约。该测试先成功验证两个 reset 均已解析、短窗早于周窗，再错误地要求 SoonestReset 返回过去的短窗。测试直接调用 quota source 和 SoonestReset，并未进入后台 refresh 循环。

因此应该先注入 clock/now，分离“解析固定 fixture”与“相对时钟决策”；最低限度可用相对未来时间造 fixture，但不能把日期简单延后几年，也不能移除生产代码对过去时间的过滤。

补充验证：通过 Go `-overlay /tmp/makewand-review-quota-overlay.json` 只把两个 reset 替换为当前时间加 2 小时、加 5 天，原生产代码上的该用例立即通过；仓库源文件没有修改。这将故障定位进一步收敛到测试日期依赖。

生产策略仍有值得单独改进的窗口语义：`quota_gate.go:70` 在错误发生时总选最早未来 reset，这是不知道耗尽窗口时的早探测策略；周额度已耗尽时，短窗重置不等于恢复。已知多个约束均耗尽时，应保留各自 blocked-until，全部解除才能恢复；只知道模糊 429 时，用短退避和单个半开探针验证，而不是把最早 reset 当作一定健康。支持供应商明确的 retry/reset 信号，分别定义 source freshness 与缓存写入 freshness。

验收应覆盖：now 前/相等/后一瞬、仅短窗耗尽、仅周窗耗尽、双窗耗尽、missing/null/未知窗口长度、时区、旧日志、时钟跳变、refresh 与 reactive seal 交错、多进程状态更新。该 Go 修复不会自动改变 Python 的永久 limited 问题。

**五、对仓库已有优化方案的校准**

`docs/MAKEWAND_OPTIMIZATION_PLAN.md` 标记 Proposed、日期 2026-07-16。其“不变量”——完整快照、不可变验收资产、验证与应用一致、单工作区单写者、预算准入和 fail closed——仍然正确，且恰好击中 Python v3 的主要缺口。

但不能照搬其历史基线：当前所选 Go 包已经可编译；`scripts/test_gate.sh` 已用 go list ./... 纳入 server 包，race 脚本也已扩展；Go 已有真实 bwrap 路径。本次尚未证明历史 Server、账本等每条问题均已解决，应逐项重验。方案还以 Go v0.2/v0.3 为发布路线，与当前 Python v3 的产品口径不一致。

建议保留不变量，重写“当前状态”和交付顺序。先补可测试的执行/候选/结果契约，不把大规模包迁移、全功能 Server 或多 Agent DAG 作为起步依赖。

**六、优先演进路线图与退出条件**

| 优先级 | 工作包 | 完成标准 |
|---|---|---|
| P0-1 | 默认权限止血，统一覆盖 probe/review/run/race | 无外部隔离时不默认绕过；review 只读；工作目录策略不能仅依赖 Git helper |
| P0-2 | 进程监督器与总预算 | 静默/超长行/持续输出均按期限取消；SIGINT/SIGTERM 和 timeout 后清理后代；输出内存有界 |
| P0-3 | 真实结果状态与验证门禁 | review 缺失、P0、修复耗尽和失败测试均不会返回 PASSED；CLI 退出码与状态一致 |
| P0-4 | 竞速止损与单写者 | 私有 run 目录、不互删；稳定基线之前禁用自动质量裁决/采纳；fallback 不沿用未知部分写入 |
| P1-1 | BaseSnapshot、冻结候选、受控应用 | 增删改、dirty/untracked、mode/symlink 可重建；应用前检查 stale；中断后可恢复 |
| P1-2 | Go runner 的版本化协议与能力声明 | Python 调用经审计的 Go 后端；认证/网络边界明确；不具备能力的 Adapter 不自动升级权限 |
| P1-3 | 额度/缓存状态机 | reset 后单探针恢复；未知与耗尽分离；多写者不丢更新；按账号与窗口记录；修复 quota --probe |
| P1-4 | 搜索、快照、日志扫描护栏 | 统一范围与资源预算，排除/截断可见；大库和权重不会被默认全量读入 |
| P1-5 | Go 时钟红测与统一发布门禁 | 测试不依赖日期；Python/Go 同时入 CI；修复 README guard；安装入口/版本/模型身份可核验 |
| P2-1 | 受控 Codex 专家委派 | 最小权限、有限递归、共享预算、可验证 patch；固定任务集证明收益 |
| P2-2 | 重建 Race 与真正独立的审查 | 同基线、独立沙箱、匿名候选、确定性验收、实际身份记录和故障淘汰 |
| P2-3 | 扩展控制台与账本观测 | 先统一 run/attempt/candidate/evidence ID，再接 Web 与分析；不扩大未经验证的生产承诺 |

实施顺序建议：权限与失败语义 → 生命周期与工作区 → 完整候选与验证 → 配额/搜索/发布可靠性 → 专家委派与竞速收益评估。建议 4 可作为独立的小修复提前完成，但它不是整体风险最高的工作。
