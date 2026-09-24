# Makewand 技术评估报告

评估日期：2026-09-23。范围：/mnt/data/makewand 当前工作区，基准提交 c0287b3 及评估开始时已有的 18 项未提交文件变更；不是对仅该提交或某个发布版本的评估。

**综合评分：68/100。定位：有明显工程投入、适合可信单用户环境受控试用的多模型开发工具；尚不能据此认定为安全边界完整、策略统一的生产级联合调度平台。** 主要扣分来自已复现的宿主执行绕过、信任参数遗漏，以及两套实现的策略分叉，而非功能数量不足。

评估采用源码追踪、全量测试、Go 并发检测、静态检查，以及仅使用临时目录、伪造凭据、mock 模型和无害标记的边界复现。没有修改实现代码。未对真实订阅服务做端到端兼容性认证、吞吐压测、故障演练或长期运行验证；不把仓库中的模型名称、套餐描述视为外部供应商事实。

## 一、架构设计与跨模型编排评析

### 1. 实际架构：两套执行与决策体系共用命令入口

Python 负责 run/review/race、候选应用、健康缓存与会话观察；Go 负责原生 TUI、HTTP 转发、另一套候选验证流水线、鉴权、账本及组织项目管理。两者通过子进程双向转交部分子命令，并非共享一个调度内核。

| 维度 | Python 路径 | Go 原生路径 |
|---|---|---|
| 模式 | fast / standard / deep | fast / balanced / power |
| 路由 | 关键词、静态偏好、健康过滤、本地调用次数惩罚 | 访问类型、故障率、配额分段、Beta/Thompson Sampling |
| 用量 | usage_window.json 调用事件 | 订阅快照、routing_stats、token/团队账本 |
| untrusted | CLI 只读与禁网沙箱，但有漏传/绕过 | capability 检查，排除不安全的本地 CLI |
| 审查失败 | run 最终拒绝交付 | 原生 BuildPipeline 的 review 是非阻塞阶段 |
| 候选验证 | race 没有调用本地测试门禁 | 独立副本验证，保留基线测试，强度分级 |

这种拆分有利于 Python 快速适配 CLI、Go 承担长驻服务，但语义重复已经形成实质性风险：同一个产品的不同入口，对安全、预算和成功状态给出不同保证。
依据：[Go→Python 入口](/mnt/data/makewand/cmd/makewand/main.go:81)、[Python→Go 入口](/mnt/data/makewand/makewand/cli.py:303)。

### 2. 意图与路由算法

Python 将输入划分为 identity/explain/review/code，默认 explain，显式“不要修改”等约束优先于 force_code。英文领域词采用边界匹配，避免 ui 命中 building 等误判。这些是合理的可解释性与安全默认值。

但它仍是人工词表启发式：中文子串、否定范围、复合任务和词义歧义没有统一语法或置信度。显式 run 默认强制 code，顶层自然语言入口则经过分类，用户可见行为需要清楚说明。

Python 评分可概括为：
score = 基础偏好 + 领域加权 + 档位加权 + 调用窗口惩罚，再过滤部分健康状态。
权重可解释，但没有实验结果证明当前数值具有最优性。问答、独立 review、race 另有固定选择逻辑，并未统一使用该算法。无候选时硬回退 agy，也可能重新引入已不可用的 provider。

Go 确实实现了 Beta 后验抽样和质量反馈，而不是仅在文档中宣称自适应；排序还受订阅/API 访问优先级、故障率、配额分段和冷启动静态顺序约束。其价值在于可以利用实际验证结果调整偏好。局限是质量信号主要来自测试与裁判选择，仍会受任务分布、模型升级、裁判偏差影响；未见统一的跨语言反馈账本或完整的任务条件化质量模型。
依据：[意图分类](/mnt/data/makewand/makewand/orchestrator.py:463)、[Python 评分](/mnt/data/makewand/makewand/orchestrator.py:629)、[Go 排序](/mnt/data/makewand/router/strategy.go:416)、[后验抽样](/mnt/data/makewand/router/strategy.go:307)。

### 3. 滑动窗口配额与防限流

Python 的实现值得肯定：记录事件的完整读改写周期使用 flock，临时文件 fsync 后 replace，避免常见的并发丢记录问题；运行中捕捉部分限流信息后写入共享状态。

但“防限流”需要降格为“启发式削峰”：
- Codex 的 3 小时 20/35 次、Claude 的 24 小时 35/60 次及 7 天 120/200 次是代码内置阈值，不是供应商真实剩余 token。
- 统计调用次数，未按上下文、输出、推理开销、子代理、重试成本加权。
- 惩罚只影响排序；领域加权可以使达到阈值的 provider 仍然首选，不是硬熔断。
- 问答、独立 review、race、直接 provider 命令绕过 dispatch_task，缺少统一计数。mock 验证问答成功时 record_engine_usage 调用次数为 0。
- 审查者排序没有同等滑窗惩罚；agy/muse/grok 为零惩罚，不代表真实无上限。
- 普通 get_or_update_status 不主动刷新；agy 探测只检查版本，不能证明登录、额度或实际可调用性。超时、认证失败、限流的分类也不完全一致。
- 多进程可以同时读取尚未计入在途请求的窗口，锁住写账并不等于原子准入。

Go 通过 Claude OAuth 用量源、Codex 会话日志、AGY 登录状态构造快照；预测额度用于软排序，实际 429 用于执行边界封禁，并结合最早重置时间和熔断器。设计比 Python 更完整，但数据仍有陈旧、缺失、来源变化的风险，也未覆盖所有 Python provider。
依据：[Python usage](/mnt/data/makewand/makewand/usage.py:90)、[记录入口](/mnt/data/makewand/makewand/orchestrator.py:597)、[健康探测](/mnt/data/makewand/makewand/health.py:209)、[Go 配额门禁](/mnt/data/makewand/router/quota_gate.go:1)。

### 4. 双模型竞速与独立审查

Python race 为两个候选创建独立目录，保留 diff、耗时、基线与 manifest，再提供 inspect/apply/discard，交付可审查性较好。

然而当前更接近“并行生成后比较”，不能承诺首个合格结果立即返回：
- 等待两边结果，没有取消输家；高负载改为串行。
- 不使用上述统一动态评分；降级时 A、B 都可能是 agy。
- 裁判固定 agy，可能与参赛者同引擎；候选名称直接传给裁判，不构成真正盲审。
- 每份 diff 只给裁判前 3000 字符；未运行 run_local_tests，也不要求与 run 相同的审查通过状态。
- winner 依赖自然语言正则；裁判失败但仅一个候选执行成功时，仍可能推荐该候选。

Go CandidateSelection 则支持验证强度、基线测试保护、达到强度门槛后取消其他工作，并继续收集已开始尝试的用量，避免漏账。这是应当复用的较成熟实现。即便 provider 名称不同，也不能保证底层模型或供应商不同，尤其在聚合 CLI 可切换底层模型时。
依据：[Python race](/mnt/data/makewand/makewand/orchestrator.py:1594)、[裁判输入](/mnt/data/makewand/makewand/orchestrator.py:1714)、[Go 候选选择](/mnt/data/makewand/internal/engine/candidate_service.go:136)。

### 5. Auto-Fix 与质量门禁

Python run 默认最多修复两轮，复审排除实际修复引擎，存在独立的最终测试硬门禁；结构化 verdict 对空输出、格式错误、缺陷与 pass 矛盾进行防御，方向正确。

边界仍需明确：
- 测试不存在返回通过；模型能改动原有测试，没有 Go 路径的基线测试保护。
- 初次 diff 在运行可写测试前生成，测试可能改变后续代码；缺少把测试、审查和最终交付绑定到同一内容哈希的机制。
- verdict 仍接受自由文本 LGTM 等兼容判定，不能替代确定性验收。
- 普通 run 直接改当前工作区，失败返回不自动回滚此前修改。“拒绝交付”不等于“工作区未变”。
- 全局预算使用 time.time；测试固定自己的超时，不接收剩余预算，独立 review 的多个回退及 race 裁判也各自使用完整 timeout。
- Go BuildPipeline 在 ReviewError、ReviewNoFixFiles 以及单 provider 时可继续，修复后也不统一强制复审。这属于另一套产品策略，不能套用 Python 的严格闭环宣传。

另一个已复现的问题：让 Git helper 返回退出码 128 后，独立 run_review 仍返回 exit 0、pass=true。原因是 Git 错误被变成空 diff，再被解释成“无修改”。
依据：[Python 最终门禁](/mnt/data/makewand/makewand/orchestrator.py:1210)、[测试发现](/mnt/data/makewand/makewand/orchestrator.py:224)、[Go 非阻塞审查](/mnt/data/makewand/internal/engine/buildpipeline.go:214)、[空 diff 通过](/mnt/data/makewand/makewand/orchestrator.py:1501)。

## 二、安全防护与沙箱隔离评析

### 1. 已落实的保护

Python 使用只读宿主根、工作区 bind、tmpfs /tmp、清空环境、PID/IPC/UTS 隔离和可选禁网；普通测试隐藏 HOME，provider 按名称选择凭据目录；写任务缺少 bwrap 时拒绝。沙箱确定挂载范围时没有执行不可信 git rev-parse，这能防止 core.worktree 扩大挂载范围的特定攻击。

Go 将生成和验证分开；untrusted 对执行及回退路径进行 capability 检查，测试默认禁网，unsafe host execution 要求环境变量与一次性确认。候选验证保存原始测试，避免通过削弱测试伪造成功。

Bubblewrap 提供 Linux 内核级进程隔离，不是虚拟机或独立内核。“物理沙箱”不应被理解为硬件隔离。上游明确说明：安全策略取决于调用方传入的参数，bwrap 本身不是现成的完整安全策略。[Bubblewrap 官方说明](https://github.com/containers/bubblewrap#sandbox-security)

### 2. 主要安全发现

**S01｜高危，发布阻断优先级｜不可信 Git 配置触发宿主代码执行，已真实复现。**

run_review 先在宿主调用 get_git_diff，后调用 provider；git_helper.run_git_cmd 直接 subprocess.run，继承仓库配置，未沙箱化，也未统一关闭 external diff、textconv 等可执行扩展。在临时仓库配置无害 diff.external 后，untrusted review 在工作区外创建了标记文件。

影响：能影响该仓库 Git 配置或相关扩展的内容，可能以 Makewand 用户身份执行宿主命令；这也关系到模型可以写 .git 的候选目录在返回宿主处理时的边界。普通远端 Git clone 不自动继承攻击者的本地 .git/config，因此不能描述成“克隆任意仓库立即执行”；本次证明的是处理已带该配置的工作区确实越过沙箱。
修复：仓库 Git 操作使用统一隔离执行器，过滤环境与配置、禁用可执行扩展；不要仅加 --no-ext-diff 而遗漏 textconv/filter/fsmonitor/hooks 等不同路径。
依据：[宿主 Git runner](/mnt/data/makewand/makewand/git_helper.py:13)、[差异提取](/mnt/data/makewand/makewand/git_helper.py:108)、[review 调用顺序](/mnt/data/makewand/makewand/orchestrator.py:1501)。

**S02｜高危｜只读直接调用在缺少 cwd 时跳过 bwrap，mock 执行器已验证。**

CLI 的 --cwd 默认 None。provider untrusted 分支检查 bwrap 存在、拒绝写模式，但真正包装要求 repo_root and cwd。execute_claude_task(..., readonly=True, repo_trust="untrusted") 不给 cwd 时，最终 runner 收到的是 claude 本体，cwd=None，而非 bwrap。相同包装模式存在于其他适配器。
影响：显式要求 untrusted 的调用仍可能继承宿主环境和网络；只读工具参数不是进程隔离。普通直接写调用则会因缺少 cwd 被拒绝，与 README 用法也存在偏差。
修复：在入口归一化 cwd，再构造不可降级的执行计划；对所有 provider 做“真实包装发生”的参数化测试。
依据：[Claude 检查与包装](/mnt/data/makewand/makewand/providers/claude.py:43)、[CLI 参数转交](/mnt/data/makewand/makewand/cli.py:530)。

**S03｜高危｜race 裁判丢失 repo_trust，mock 已验证。**

race 选手收到 untrusted，裁判 execute_agy_task 没有 repo_trust 参数，恢复默认 trusted、allow_network=True。即使两位选手都失败，代码仍调用裁判；mock 复现得到一次默认可信裁判调用，之后 race 才返回失败。
修复：trust/network/readonly 使用不可变 ExecutionPolicy，贯穿实现、修复、探活、裁判、Git 和测试；untrusted 禁止写的任务应在任何仓库初始化前整体拒绝。
依据：[裁判调用](/mnt/data/makewand/makewand/orchestrator.py:1723)。交互入口也未携带 repo_trust，应纳入同一矩阵测试：[交互入口](/mnt/data/makewand/makewand/cli.py:477)。

**S04｜高危边界缺口｜凭据隔离不完整，部分已复现。**

provider 模式统一透传多家 API_KEY；伪造环境实验确认 Claude 包装同时包含 ANTHROPIC_API_KEY、OPENAI_API_KEY、XAI_API_KEY，跨供应商凭据隔离不成立。当前 provider 的配置目录实际使用可写 --bind，和函数注释中的“只读授权凭据”不一致；可读整个 HOME 配合黑名单也会遗漏其他敏感文件。Muse 还跳过 PID 隔离，并开放运行时目录，不能沿用其他 provider 的隔离强度描述。
影响：被注入的 agent 或其子进程可接触额外凭据与持久配置；本次仅验证伪造变量，没有读取或外传真实凭据。
修复：按 provider 最小化环境；凭据与可写缓存拆分；认证尽可能移到受控 broker；为兼容性例外制定独立能力声明。
依据：[认证目录挂载](/mnt/data/makewand/makewand/sandbox.py:138)、[环境透传](/mnt/data/makewand/makewand/sandbox.py:249)、[Muse 例外](/mnt/data/makewand/makewand/sandbox.py:125)。

**S05｜条件性高危｜只读根和禁网不能阻断可见 Unix socket，已真实复现。**

在 /var/tmp 创建自有 Unix socket，Python run_in_sandbox(readonly=True, allow_network=False) 仍成功连接并收到无害标记。ro-bind / / 保留宿主可读文件树和未遮蔽 socket；--unshare-net 不阻断所有基于路径的本地 socket。
影响取决于宿主是否暴露当前用户有权限连接的代理、D-Bus、容器等服务；本次没有连接这些真实服务，不能据此声称已获取 root。Go 也采用宽根挂载，值得用相同矩阵单独验证。
修复：改为最小 rootfs 挂载白名单，遮蔽 /run、/var/run、/var/tmp 等非必需路径，只显式代理必要 IPC。
依据：[Python 挂载](/mnt/data/makewand/makewand/sandbox.py:120)、[Go 挂载](/mnt/data/makewand/internal/engine/verify_isolation.go:150)、[Linux Unix socket 说明](https://man7.org/linux/man-pages/man7/unix.7.html)。

### 3. 其他安全与可用性边界

- Python untrusted 在正确进入沙箱时完全禁网，云端 CLI 通常也失去模型连接。应将联网推理与无凭据本地工具执行分离；不能简单开放全部网络解决。
- health.py 的主动探测直接在宿主运行 CLI，其中 Muse 使用 --yolo，且没有 repo_trust 参数。不能将探活视为天然无副作用。
- Go 验证路径在 workspace 位于 HOME 内时采用敏感目录列表，该列表没有 Python 中完整的 .codex/.claude 等条目；默认临时候选路径常在 /tmp，会隐藏整个 HOME，但直接 HOME 下验证需要额外检查。
- 无 cgroup CPU/内存/进程数硬限制；nice 和超时不能构成资源耗尽防御。
- Go trusted 生成明确允许宿主 CLI；SECURITY.md 已承认生成与验证的不同边界，应统一 README、Python 与 Go 的说明。没有必要把该已说明的策略本身误报为新漏洞。

## 三、代码质量与工程落地评析

### 1. 实测测试与检查结果

环境：Python 3.12.3、Go 1.27.1、Linux，bwrap 可运行。go.mod 声明 Go 1.26.0，因此没有验证最低声明版本。

| 检查 | 结果 |
|---|---|
| python3 -m unittest discover tests -v | 99/99 通过，15.300 秒 |
| GOPROXY=off GOTOOLCHAIN=local go test -count=1 ./... | 全仓通过 |
| go test -race -cover -count=1 ./...（同样禁用依赖下载） | 全仓通过，未报告数据竞争 |
| go vet ./... | 通过 |
| MAKEWAND_REQUIRE_BWRAP=1 的 Go live isolation 测试 | 实际执行通过，7.35 秒，未跳过 |

静态统计：Python 实现 22 个文件、6384 行，11 个测试文件、99 个测试方法；Go 实现 124 个文件、126 个测试文件、800 个顶层 Test 函数和 3 个 fuzz 入口。800 是源码定义数，不是精确运行子用例数。常规 go test 也不等于持续 fuzz。

部分 Go 包语句覆盖率：
| 包 | 覆盖率 |
|---|---:|
| router | 77.5% |
| internal/engine | 76.4% |
| internal/tui | 65.2% |
| cmd/makewand | 41.4% |
| serverauth | 69.7% |
| serveradmin | 61.7% |
| serverusage | 60.2% |
| serverui | 94.7% |

这些是包级语句覆盖率，不是分支覆盖率；serverui 的 Go 覆盖率不表示浏览器 JavaScript 已被全面测试。没有测量 Python 行/分支覆盖率。

CI 已包含真实 bwrap smoke、全仓测试、race、vet、govulncheck、lint 和构建，是明显优点。本次没有执行在线 govulncheck 或 golangci-lint，不能把 CI 配置当成当前运行结果。Makefile 的 test-go 仅列出部分包，和 CI 全仓门禁不完全一致。
依据：[CI](/mnt/data/makewand/.github/workflows/ci.yml:1)、[Makefile](/mnt/data/makewand/Makefile:11)。

### 2. 并发与可靠性

做得较好的部分：
- Python subprocess 使用新进程组、超时 TERM/KILL 清理和非阻塞流式读取，避免无换行输出卡住读取。
- usage 的事务锁和原子替换正确覆盖主要写入过程。
- 候选应用包含 SHA-256 完整性、冲突检测、符号链接与路径边界检查、备份、单文件原子替换。
- Go 使用实例级路由表、原子快照、锁、context 取消、半开只允许一个探测的熔断器。
- Go HTTP 有 scope、provider/mode allowlist、组织项目归属、限流、预算预留、审计和用量账本；SQLite 每连接设置 busy_timeout/foreign_keys，WAL；管理会话使用 HMAC、CSRF 校验与 HttpOnly cookie。

仍不足的部分：
- apply 的 flock 异常被忽略；该锁也只能约束合作的 Makewand 进程，不能“消除所有 TOCTOU”。
- journal 在全部文件修改后才落盘，异常回滚不等于宕机事务恢复；os.replace 仅保证单文件原子性。
- Python 非流式 communicate 和 Go CLI bytes.Buffer 未统一限制输出，10 MB 防线不覆盖全部执行路径。
- 多处 except Exception: pass 把缓存、配置或探测错误静默转为默认状态，不利于诊断。
- Python orchestrator.py 1796 行，混合分类、调度、提示词、测试、Git 交付脚本和 UI 输出；provider 之间大量复制安全逻辑。
- observer 基于 tmux 文本、ps、load average 输出启发式建议；不是死锁证明或分布式追踪。sample_lines 和命令参数可能保存敏感内容，缺少统一脱敏。discovery 是本地配置解析加静态列表，不是实时能力协商。
- Python 测试部分路径调用实际全局用量记录，测试输出受真实窗口统计影响；应使用隔离配置根，避免测试污染用户状态及路由不确定性。

依据：[进程管理](/mnt/data/makewand/makewand/providers/base.py:64)、[应用锁](/mnt/data/makewand/makewand/candidate.py:277)、[journal](/mnt/data/makewand/makewand/candidate.py:428)、[Go breaker](/mnt/data/makewand/router/circuit_breaker.go:61)、[服务配置](/mnt/data/makewand/cmd/makewand/serve.go:273)、[observer 存储](/mnt/data/makewand/makewand/observer.py:454)。

### 3. 服务端生产边界

Go 服务并非简单转发壳，已有可用的管理与多租户基础。但预算预留和部分计数依赖进程内状态，不能自动扩展为多副本一致性保证；固定每请求默认 0.01 USD 预留不是最大请求成本上界。strict-accounting 默认关闭且不覆盖流式响应；进程崩溃与日志失败仍需恢复策略。

HTTP 明确只提供兼容子集，max_tokens/temperature 被接受但忽略；客户端可能误以为预算或采样参数已生效。应声明 capability 或明确拒绝不支持字段。

“零额外 token 费用”只能是符合特定配置的使用方式，不能概括整个系统：Go 包含计费 API 路由，Python 也透传 API 凭据。实际账单、供应商规则与模型版本未在本次实测。
依据：[预算预留](/mnt/data/makewand/router/http.go:1316)、[strict accounting 默认值](/mnt/data/makewand/cmd/makewand/serve.go:407)、[兼容参数](/mnt/data/makewand/router/http.go:548)。

## 四、核心优势与现存不足/潜在隐患

核心优势：
1. 从“调用模型”延伸到代码差异、测试、审查、自愈、候选封存与应用，具备实际开发闭环。
2. Go 路由有真实的质量反馈、熔断、配额快照和取消机制。
3. 沙箱、基线测试保护、哈希与路径检查具有实质工程价值。
4. Go 服务端具备身份、权限、组织项目、预算、审计、可观测性基础。
5. 全量测试、race 和真实 bwrap 门禁齐备，修复后具备建立回归保障的条件。

按优先级处理不足：
| 优先级 | 内容 | 证据强度 |
|---|---|---|
| P0 | 宿主 Git 执行边界 S01；untrusted 缺 cwd 绕过 S02 | 真实无害复现 / mock 执行器验证 |
| P0 | race 裁判信任漏传 S03；统一信任策略入口 | mock 验证及源码 |
| P1 | 跨 provider 凭据、持久配置写权限、可见 Unix socket | 伪造环境与真实 socket 复现 |
| P1 | Git 失败误报 pass；race 缺少测试门禁；两套 review 成功语义不同 | mock 复现及源码 |
| P1 | 统一用量入口、在途预留、总 deadline 和测试状态 | mock 与源码 |
| P2 | apply 崩溃恢复、不可变产物、隔离测试状态 | 源码确认设计缺口 |
| P2 | 多副本账本一致性、兼容字段、文档与能力声明 | 当前架构边界，未做部署压测 |

P0 表示优先级/发布阻断，不表示已证明 root 提权或所有部署都可远程利用。

## 五、综合评分与下一代演进建议

| 维度 | 权重 | 得分 | 主要判断 |
|---|---:|---:|---|
| 架构与编排 | 25 | 20 | 功能闭环和 Go 自适应路由扎实，双实现分叉明显 |
| 安全与隔离 | 25 | 10 | 有真实隔离措施，但存在已证实的边界绕过 |
| 工程质量与可靠性 | 20 | 16 | Go 工程较成熟，Python 状态与异常处理偏脆弱 |
| 测试与验证 | 15 | 13 | 全量/race/live 均通过，缺少关键负向场景和完整覆盖证据 |
| 生产运维与一致性 | 15 | 9 | 有账本与管理能力，仍缺跨进程一致性及真实服务验证 |
| 合计 | 100 | **68** | 有价值的工程基础，安全收敛应先于扩 provider |

该分数是依据本次工作区与证据的工程判断，不是漏洞认证或真实生产 SLO 的统计测量；关键安全问题不因其他维度得分高而被豁免。

建议按顺序演进：

**第一阶段：封闭执行边界。**
建立唯一 ExecutionPolicy 与 ExecutionPlan，入口统一解析 cwd、trust、readonly、network、deadline；所有 Git、probe、provider、judge、测试通过同一策略执行。先加入 S01–S05 与 Git failure→UNVERIFIED 回归；只读、禁网、失败返回都应验证实际效果。收紧 rootfs 与凭据可见性，取消安全异常的静默降级。

**第二阶段：统一调度事实与结果状态。**
保留双语言时，以有版本的 JSON 协议或本地 RPC 明确控制面边界；共享 ProviderCapability、真实 model_id、QuotaSnapshot、UsageEvent、Verdict schema。统一所有调用的记账与限流；分别表达 SUCCESS、FAILED、UNVERIFIED、CANCELLED、BUDGET_EXHAUSTED，区分进程成功、测试通过、审查通过与可应用。

**第三阶段：把“自愈”变成可验证交付。**
始终在候选副本修改，保护基线测试；同一内容哈希绑定生成、测试、审查、批准与应用。完整 diff 分片评审或验证完整读取，禁止无声明截断。race 用“首个达到质量门槛的候选”策略，可取消输家并结清用量；修复轮数、额度与时间共同限制。apply 采用提前持久化日志、恢复状态机和应用前再次校验。

**第四阶段：用数据验证智能与生产能力。**
构建固定任务集，记录质量通过率、人工返工率、p50/p95 延迟、429 率、每个合格交付的调用与费用；比较静态路由与 Thompson 路由。建立 provider CLI 版本/参数/JSON 输出契约测试；模型升级后重置或衰减旧后验。加入取消、断流、磁盘满、日志失败、崩溃恢复、多进程争用与恶意仓库矩阵。多副本部署前使用共享原子预留账本与幂等 request_id，验证流式和失败请求计费。

验收顺序应为：安全边界可证明 → 两套入口语义一致 → 质量与成本可量化 → 扩充模型和并行度。
