# Makewand 深度红队架构与代码评审

审查日期：2026-09-21。代码基线：`7348ddb`。审查对象：`/path/to/workspace/makewand` 的 Go 网关、TUI/执行引擎、Python 调度器和 Web 管理界面。

## 执行结论

**Go 网关已具备较成熟的请求治理基础，但 Python 调度器的权限、隔离、进程生命周期与质量门禁尚未形成可靠闭环。当前实现适合由可信操作者监督的个人工具；不足以据此宣称可以安全地自动处理不可信仓库、任意模型指令或高并发工程任务。**

最需要优先处理的不是增加模型或更复杂的提示词，而是：将权限策略绑定执行能力、修复真实超时和进程回收、让审查失败阻止成功交付，并统一两套执行路径的任务契约。

没有确认到无需认证的公网远程代码执行，本报告不标注 P0。P1 表示可能越过产品承诺的权限边界、失去执行控制或错误通过交付门禁，P2 表示重要可靠性、资源或状态一致性问题。严重程度针对对应触发条件，不把本地 CLI 缺陷自动推定成互联网可利用漏洞。

## 证据与部署核验范围

- 本机 `makewand` 解析到 `/path/to/workspace/.local/bin/makewand`，实际链接目标为仓库中的 `bin/makewand`，该入口导入 Python `makewand.cli`。
- `systemctl show makewand` 返回 `ActiveState=inactive`、`MainPID=0`，未得到该名称服务的运行实例。其他服务名、容器、反向代理或外部机器部署仍可能存在；不能据此断言线上服务停止，也不能确认浏览器访问的服务就是该提交。
- 仓库内没有找到四小时执行 `observe` 的 timer/cron 配置或内部调度循环。观察器支持单次采样；外部调度是否生效仍需核验。
- 未调用真实模型完成任务，未对线上服务压测，未读取真实凭据。复现使用短生命周期假 CLI、临时目录、合成标记及隔离的状态文件；仅新增本报告，未修改实现。
- Python `unittest discover -s tests -v`：**21 项通过**。
- Go `router`、`cmd/makewand`、`serverui`、`internal/engine`、`internal/tui`、`serverauth`、`serveradmin`、`internal/remotesession`：**8 个包测试通过**，其中部分使用 Go 测试缓存。
- `go test -race ./router ./serverauth ./internal/remotesession`：**通过**，其中 `serverauth` 使用缓存。该结果不覆盖 Python 进程间状态覆盖，也不证明不存在阻塞或资源泄漏。

## 架构合理性与协同效率

语言分工本身合理：Go 适合 HTTP、鉴权、流式传输和并发资源治理；Python 适合快速组合不同 CLI、任务阶段和巡检逻辑。但当前两者主要是**并列执行系统**，还没有形成可验证的统一控制面。

| 维度 | Go 路径 | Python 路径 | 实际影响 |
|---|---|---|---|
| 意图分类 | `router.ClassifyTask`，并有 TUI 构建阶段 | `classify_prompt_intent` 四类分类 | 再加 Web JS，共三份规则，优先级和能力语义不一致 |
| 模型执行 | 独立 CLI/API 适配器 | 独立 CLI 适配器 | 同一供应商在两条路径上的参数、权限、模型档位可能不同 |
| 不可信输入 | `RepoTrustUntrusted` 拒绝不安全 provider | 没有对应信任门禁 | Go 的防护不会自动覆盖 Python CLI |
| 运行隔离 | 候选验证默认要求隔离，缺失时拒绝 | `/sandbox` 可裸机降级；普通 provider 不经过它 | “项目有沙箱”无法证明任一任务实际受沙箱保护 |
| 健康与额度 | 配额探测、预留、断路器、用量审计 | 全量 JSON 状态快照与关键词解析 | 同一订阅缺少共同的准入和状态所有者 |
| 工程验证 | 候选验证强度与实际测试结果 | 模型文本审查与关键词判定 | Python 的“通过”语义明显弱于 Go 引擎 |

Go 的已有优点应保留：HTTP 请求体有 10 MiB 上限；`serve` 设置读取、写入和空闲超时并要求认证；断路器具有互斥保护、半开探测及探测过期恢复；流转发具有 30 秒背压超时；存在配额预留和严格用量记录选项；不可信仓库在执行路径再次检查 provider 能力。参见 [HTTP 限制](/path/to/workspace/makewand/router/http.go:152)、[服务配置](/path/to/workspace/makewand/cmd/makewand/serve.go:258)、[断路器](/path/to/workspace/makewand/router/circuit_breaker.go:58)、[流转发](/path/to/workspace/makewand/router/execute.go:425)、[信任检查](/path/to/workspace/makewand/router/execute.go:69)。这些是实现与选项层面的优点，不代表所有部署均启用所有保护。

Web 的 `task/review/explain` 标签最终均调用 `/v1/chat/completions`，发送 `mode: balanced` 与消息历史，没有调用 Python `run_pipeline`，也没有传递 JS 分类结果。后端重新分类后走 Go 路由。因此界面的“工程任务流水线”描述与真实调用链存在偏差。[Web 请求](/path/to/workspace/makewand/serverui/static/app.js:709)、[Go 再分类](/path/to/workspace/makewand/router/http.go:1112)

效率方面，Python 每个候选、每个审查/修复阶段各自获得完整 `timeout`，默认 300 秒不是任务总预算；失败回退和多轮修复可累加到数十分钟。`/race` 等待两者完成，再调用裁判，并非自动取消落后者的最低延迟竞速。工作区采用目录递归复制，缺少文件数、总大小预算，可能复制虚拟环境、数据及有效符号链接指向的内容。模型调用、复制与等待的放大成本需要共同计量。[阶段回退](/path/to/workspace/makewand/makewand/orchestrator.py:246)、[竞速等待](/path/to/workspace/makewand/makewand/orchestrator.py:504)、[复制实现](/path/to/workspace/makewand/makewand/git_helper.py:61)

“零成本”在实现中主要表示某些订阅调用的边际货币记账为零，不能代替订阅额度、CPU、内存、磁盘、进程数和等待时间预算。`health.probe_model` 的若干探测本身会执行模型 CLI 推理，不能视作不消耗订阅资源的纯元数据查询。[探测](/path/to/workspace/makewand/makewand/health.py:122)

## P1：优先修复

### R1：问答、审查与竞速没有执行权限边界

**影响：被误分类的正常提问，或仓库/模型上下文中的恶意指令，可以进入具有宿主写入、命令执行能力的模型进程。**触发不要求突破 Bubblewrap，因为这些调用根本没有经过它。

Python `explain` 和 `review` 复用编码 provider；Claude/AGY 使用跳过权限参数，Codex 明确传入 `--dangerously-bypass-approvals-and-sandbox`，Muse 使用 `--yolo`，随后直接调用 `run_subprocess`。`/race` 也调用同一组函数，`cwd` 只改变当前目录，无法限制访问原仓库或另一选手目录。[问答调用](/path/to/workspace/makewand/makewand/orchestrator.py:200)、[Codex 参数](/path/to/workspace/makewand/makewand/providers/codex.py:37)、[Claude 参数](/path/to/workspace/makewand/makewand/providers/claude.py:37)、[竞速调用](/path/to/workspace/makewand/makewand/orchestrator.py:482)

意图误触发并未彻底解决。直接运行分类函数得到：

| 输入 | 实际输出 | 应保持的权限 |
|---|---|---|
| `解释如何修复这个 bug，不要修改代码` | `code` | 只读解释 |
| `请审查实现是否安全，不要修改代码` | `code` | 只读审查 |
| `What is a patch?` | `code` | 普通知识问答 |

根因是任何命中编码关键词的子串都优先判为 `code`，未处理否定、引用或疑问语义。更严重的是，即使分类正确为 `explain`，底层能力也未减权。[分类规则](/path/to/workspace/makewand/makewand/orchestrator.py:140)

整改：分类只负责选择流程；权限由独立执行策略强制落实。`identity/explain/review` 默认禁止写入和任意 shell 工具，`code` 只能写指定候选工作区。自然语言分类不能授予宿主权限；前端标签也不能作为授权依据。

### R2：Python 沙箱失败后执行宿主命令，且只读根目录不隔离机密读取

**影响：沙箱能力缺失时仍执行用户认为已经隔离的命令；沙箱可用时，工作区外可读数据仍可能被读取并经网络发送。**

`run_in_sandbox` 在 bwrap 自检失败后直接 `run_subprocess(cmd)`；`allow_network=False` 也随之失效，且没有向调用者返回隔离降级状态。模拟 bwrap 不可用后，测试命令成功改写了临时工作区外的合成文件。[降级分支](/path/to/workspace/makewand/makewand/sandbox.py:94)

实际 bwrap 使用 `--ro-bind / /`，仅遮蔽六类 HOME 子目录，没有遮蔽全部原始 HOME、其他工作区或 `.codex`、`.claude` 等模型状态目录。重新设置 `HOME` 并不能阻止绝对路径读取。在真实 bwrap、禁网条件下，成功读取了 `/var/tmp` 内工作区外的合成标记文件；未读取真实凭据。默认又允许网络，故保密边界并未建立。[挂载策略](/path/to/workspace/makewand/makewand/sandbox.py:46)

缺少 PID/IPC 隔离及资源上限也是待补强项；本次未将这些配置缺失夸大为已验证的沙箱逃逸。Bubblewrap 官方明确将安全策略责任交给调用者，工具存在本身并不构成完整安全保证。[Bubblewrap 官方说明](https://github.com/containers/bubblewrap)

整改：默认隔离失败即拒绝；以最小挂载白名单或完整 HOME 遮蔽建立读取边界，按需提供单一 provider 凭据。测试/构建默认禁网，联网模型进程只获得所需网络能力。Go 验证路径已有拒绝降级设计，可统一复用其授权语义，但也应继续审查其全根目录只读挂载暴露面。[Go 验证隔离](/path/to/workspace/makewand/internal/engine/verify_isolation.go:64)

### R3：两套运行器都有流式超时和进程回收缺口

**影响：任务超时后仍占用进程、管道或锁；并发重复触发会耗尽资源，Python 还可能把超时执行报告为成功。**

Python 在 selector 报告“有字节可读”后调用文本 `readline()`。子进程输出半行后不输出换行，读取仍可阻塞；进程退出后的迭代排空同样可能等待继承管道的后代。实测设 `timeout=0.15`，子进程输出 `partial` 后休眠 1.2 秒，调用约 **1.25 秒**后返回 `returncode=0, exception=None`。[读取循环](/path/to/workspace/makewand/makewand/providers/base.py:120)

`kill_process_tree` 发送进程组 SIGTERM 后，一旦直接子进程退出立即返回，不再对还活着的同组子孙 SIGKILL。实测父进程被回收，但忽略 SIGTERM 的测试子进程仍为 `S` 存活状态，随后由审查脚本清理。这里确认的是残留活进程，不是把它误称为僵尸进程。[提前返回](/path/to/workspace/makewand/makewand/providers/base.py:43)

Go `ChatStream` 的 `scanner.Scan()` 和 `<-stderrDone` 位于独立的阻塞等待中。默认 `CommandContext` 杀直接进程；代码里的组杀要等到循环重新获得控制权。假 CLI 的后代持有输出管道时，设 **150 毫秒**截止，provider 流约 **1.209 秒**才结束。外层 Router 可能先返回超时，但内部 goroutine、管道和后代清理仍会延后。[Go 流循环](/path/to/workspace/makewand/router/cli.go:546)、[等待 stderr](/path/to/workspace/makewand/router/cli.go:591)、[组杀实现](/path/to/workspace/makewand/router/process_unix.go:9)。该根因符合 Go 对默认 `CommandContext` 取消行为的说明。[Go 官方文档](https://pkg.go.dev/os/exec#CommandContext)

输出预算也不完整：Python 非流式 `communicate` 全量收集；流式 10 MiB 上限在读取整行后才判断，单条巨长行可以突破。Go 非流式 stdout/stderr 和流式 stderr 使用无界 `bytes.Buffer`。

整改：独立截止监护；Python 用非阻塞字节块读取及增量解码；Go 自定义 `Cmd.Cancel` 组杀、限制退出等待并关闭管道。最终以专属 cgroup/容器覆盖脱离原进程组的后代；超时后验证整个任务执行单元为空。统一 stdout、stderr、单记录及累计输出限制。

### R4：审查失败会通过质量门禁，审查目标也不完整

**影响：没有执行成功的审查，被报告为“通过红队审查”，自动化调用方会把未经验证的代码当成完成交付。**

主审与 AGY 回退均失败时，`review_output` 保持 `None`；最终仅在存在审查文本且命中缺陷词时返回失败，随后无条件打印通过并返回 `True`。通过 mock 令编码成功、两个审查 provider 均失败，已确认结果为 `True` 且输出包含“完成并通过红队审查”。[审查回退](/path/to/workspace/makewand/makewand/orchestrator.py:322)、[最终门禁](/path/to/workspace/makewand/makewand/orchestrator.py:410)

其他相连问题：独立 `review` 在 diff 为空时直接返回，不能满足“审查已提交的整个仓库”；`run_pipeline` 的 review 分支忽略执行状态并返回成功；diff 分别按 4500/6000 字符截断；`get_git_diff` 忽略 Git 失败，可把取证失败当作无改动；审查结论依靠自然语言关键词；AGY 既可能编码又可能审查，不保证独立模型。[独立审查](/path/to/workspace/makewand/makewand/orchestrator.py:417)、[成功传播](/path/to/workspace/makewand/makewand/orchestrator.py:229)、[Git 错误处理](/path/to/workspace/makewand/makewand/git_helper.py:47)

`/race` 无强制测试门禁，裁判只获得各自前 3000 字符 diff，之后 `finally` 删除全部候选目录，没有持久化可采纳补丁；初始复制又不是带基线提交的 Git worktree，可能把大量原有文件一并算入 diff。以上均来自代码路径，未启动真实竞赛。[裁判与清理](/path/to/workspace/makewand/makewand/orchestrator.py:519)

整改：建立 `passed/failed/not_run/error` 四态，只有审查完整且验证通过才允许成功；保存明确基线、完整 diff 与候选产物；拒绝将供应商退出码 0 或缺陷关键词缺失等同质量通过。

### R5：Web 自动签发聊天令牌后，登出路径跳过服务端会话注销

证据为确定性的静态调用链，未对线上账户执行登录/登出测试。

Cookie 登录后首次聊天会将自动签发的聊天令牌写入全局 `state.token` 和 `sessionStorage.mw_playground_token`。登出只有 `!state.token && authMode === "session"` 才调用服务端注销，因此完成一次聊天后，该条件不成立。随后 `clearSession()` 只清空内存，不清理该存储键，也不撤销签发令牌。刷新时 `restoreSession()` 又通过原 Cookie 恢复会话。

**影响：用户以为已退出，原服务器会话仍可能有效；同一浏览器标签切换账户时也可能复用前一账户聊天令牌。**只限有该登录/令牌签发权限的流程，不意味着匿名用户能签发令牌。[签发和复用](/path/to/workspace/makewand/serverui/static/app.js:685)、[清理函数](/path/to/workspace/makewand/serverui/static/app.js:107)、[登出条件](/path/to/workspace/makewand/serverui/static/app.js:771)、[恢复 Cookie 会话](/path/to/workspace/makewand/serverui/static/app.js:333)

整改：管理会话和聊天令牌分离；按会话实际来源注销服务端 Cookie 会话，清理/撤销临时聊天令牌并绑定账户。不要通过切换全局认证令牌实现聊天授权。

## P2：并发、预算与可观测性

### R6：缺少跨入口的并发准入与任务总截止

所审 HTTP 聊天执行链未见全局/每订阅活跃任务信号量；登录/注册限流、货币预算与断路器不能代替模型进程并发上限。Python `/race` 的 `max_workers=2` 只约束单次调用；多个终端、Web 请求与多个 race 可以累加。普通流水线还会直接操作同一工作区，没有项目级写锁。外部代理/服务是否另有限制未确认。

Python 两个 `Future.result()` 没有总超时，线程池退出也等待工作线程；stage timeout 无法覆盖复制目录、所有 Git 操作及整个流水线。当前 Go 断路器主要保护 Go 自身状态，无法阻止 Python 同时冲击同一个订阅。[竞速等待](/path/to/workspace/makewand/makewand/orchestrator.py:504)、[HTTP 执行入口](/path/to/workspace/makewand/router/http.go:476)

整改：按 provider、工作区与全局三层准入；有界队列；读任务并发、写任务排他；任务总 deadline 向下传播，回退只使用剩余时间；按调用数与执行资源限制订阅任务，而不只按美元计费。

### R7：健康缓存会丢失并发更新，跨日重置修复仍有反例

`save_status_cache` 的临时文件加 `os.replace` 能保证单次文件替换完整，不能使“读取旧状态—修改单个 provider—写回全部状态”成为事务。两个旧快照分别将 Claude/Codex 改为 limited，再依次保存，实测最终 Claude 回到 `unknown`，其限流信息丢失。[缓存读写](/path/to/workspace/makewand/makewand/health.py:78)

跨日逻辑算出次日重置时间但尚未到期时，没有返回 `False`，继续落入“今天该时刻”比较。固定时钟为 `2026-09-21 22:00`，`updated_at=2026-09-21 21:00`、`resets_at=8pm`，应等待次日 20:00，实际返回 `True`，随即将 provider 恢复为 healthy。[重置计算](/path/to/workspace/makewand/makewand/health.py:51)

另有 `短期恢复` 既不在四小时兜底分支内，也无法解析为时间，可能一直保持 limited，直到显式探测刷新。广泛吞掉持久化异常进一步削弱可观测性。

整改：统一事务状态存储，或单进程状态所有者加按 provider 更新；重置时间标准化成带时区绝对时间；到期状态转为允许一次探测，不直接断言 healthy；未知/短期限流使用有界退避。

### R8：“受限预算搜索”缺少时间、输入类型与读取边界预算

`followlinks=False` 只约束目录遍历，`os.stat(...follow_symlinks=False)` 检查链接本体后，`open` 仍跟随文件链接。实测通过 `workspace/link.txt` 搜到工作区外的合成标记。[打开文件](/path/to/workspace/makewand/makewand/search.py:69)

未检查普通文件类型，FIFO 会阻塞在 `open`；可输入任意 Python 正则，灾难性回溯没有截止。实测仅 28 个 `a` 加 `!` 配合 `(a+)+$`、以及一个无写入者 FIFO，分别超过外部 0.4 秒截止并被测试程序终止。`max_results` 只限制匹配数量，零匹配扫描仍可遍历大量文件；单文件大小也不构成全任务总读取预算。

整改：默认字面量或线性时间正则搜索；拒绝链接及非普通文件，使用安全打开和描述符检查防止检查/打开竞态；限制累计文件数、字节数、墙钟时间并支持取消。

### R9：观察器是启发式采样工具，不能作为实时死锁防线

tmux 的两处 `check_output` 没有 timeout；单个捕获挂起会拖住整轮。分类仅根据当前文本与是否持锁判断 `hung_anomaly`，没有任务持续时间、CPU 增量、PID 身份或进度心跳，不能支持“持续十分钟”“单核 100%”等确定结论。合法持锁同样可能被标为死锁。代码仅产生建议，没有执行调度背压。[采集](/path/to/workspace/makewand/makewand/observer.py:56)、[异常判断](/path/to/workspace/makewand/makewand/observer.py:103)

即使外部每四小时触发完全可靠，也无法及时回收分钟级卡死任务。报表直接保存 tmux 尾部内容，可能包含用户输入或敏感输出；应有脱敏、最小文件权限及保留期限，而不是把截图式文本长期当作运行事实。[报表保存](/path/to/workspace/makewand/makewand/observer.py:188)

整改：任务运行器负责秒级截止/回收；观察器负责趋势、状态核对与异常提示。引入带任务 ID 的心跳、进度时间和实际进程状态，并为单次采集、整轮巡检设置截止及防重入锁。

## 最终五项高价值优化建议

| 优先级 | 落地项 | 最小改造范围 | 验收标准 |
|---|---|---|---|
| 1，立即 | 建立统一执行权限与隔离策略 | 去除普通问答/审查的默认高权限旁路；所有工程执行调用同一执行器；bwrap 缺失拒绝 | 否定/解释型输入不写文件；两个 race 任务无法读取对方或宿主标记；缺少 bwrap 时零命令执行 |
| 2，立即 | 修复超时、进程树与输出预算 | Python 字节块读取；Go 独立取消和组杀；任务级 cgroup/资源上限；传播总 deadline | 半行输出、子进程继承管道、忽略 TERM、客户端断开均能在截止加清理宽限内结束；无残留任务进程/锁；输出不突破预算 |
| 3，立即 | 让质量门禁失败时停止交付 | 四态审查结果、Git 基线与错误传播、完整 diff、候选产物保存、强制基线测试 | 所有审查 provider 失败返回非零；空 diff 与 Git 失败可区分；未执行测试不宣称已验证；race 有可重放补丁 |
| 4，随后 | 统一任务协议、准入与状态所有者 | 保留 Go/Python 分工，用本地受控 IPC 定义 job ID、intent、capabilities、workspace、deadline、quota、events；集中 provider 并发和事务状态 | CLI/Web 同任务获得同能力策略；跨入口并发不超过上限；健康更新不丢失；四小时巡检只消费结构化事件 |
| 5，并行补齐 | 封住 Web 会话和搜索边界，并将复现纳入回归 | 修复聊天令牌与管理会话混用；搜索类型/时间/累计预算；巡检超时及脱敏；增加针对上述反例的测试 | 聊天后登出、刷新仍未登录；账户切换不复用令牌；FIFO/软链接/回溯正则不会越界或挂起；跨日重置反例通过 |

建议先完成 1—3，再扩展吞吐量和自动化程度。第 4 项不要求重写双核，也不需要引入重型分布式平台；单机 Unix socket 加事务存储即可先明确任务所有权与安全契约。

## 其他核查结论与局限

- Web 滚动实现包含 `overflow-y: auto`、上滚检测和按用户位置决定自动滚到底部；本次没有运行真实浏览器验证线上滚轮行为，因此不重复宣称该 UI 缺陷存在，也不把静态代码等同线上验证。[滚动逻辑](/path/to/workspace/makewand/serverui/static/app.js:595)
- Python `muse` 子命令缺少 `--timeout` 参数定义，但分发访问 `args.timeout`，会触发属性错误；这是直接的功能缺陷，优先级低于上述边界问题。[参数定义](/path/to/workspace/makewand/makewand/cli.py:254)、[分发](/path/to/workspace/makewand/makewand/cli.py:327)
- CLI 输出中的 `gpt-6-astra` 等固定文字不能证明底层实际模型身份；本报告未验证四个订阅的当前可用模型、额度或条款。应记录 provider 实际响应中的模型与版本信息。
- 尚未覆盖全部仓库文件、外部 CLI 自身漏洞、全部部署配置及公网暴露面。结论来自指定关键路径的深入审查与列明的复现，而非形式化安全证明。

## 复现记录摘要

| 本地隔离实验 | 结果 |
|---|---|
| Python 半行流式输出，截止 150 ms | 约 1.25 s 后返回成功 |
| Go 假 CLI 后代持有管道，截止 150 ms | 约 1.209 s 后 provider 流才结束 |
| Python 父进程响应 TERM，子进程忽略 TERM | 回收函数返回后子进程仍存活，随后人工测试清理 |
| bwrap 可用、禁网、读取工作区外合成标记 | 成功读取 |
| 模拟 bwrap 不可用、指定禁网、写工作区外标记 | 成功写入 |
| 编码成功、全部审查模型失败 | 流水线返回 True 并宣称通过 |
| 两个旧健康快照分别更新不同 provider | 前一次 provider 更新丢失 |
| 固定 22:00，21:00 记录次日 20:00 重置 | 错误返回已经重置 |
| 搜索指向根目录外文件的链接 | 返回根目录外合成内容 |
| FIFO 与小输入回溯正则 | 均超过外部测试截止，被测试进程终止 |

测试日志：`/tmp/makewand-review-python-tests.log`、`/tmp/makewand-review-go-tests.log`、`/tmp/makewand-review-go-race.log`。临时日志可能随系统清理，不作为长期审计存档。
