# 长期架构优化实施记录 · 2026-10-01

本轮落实[上一轮评估](ASSESSMENT_FOLLOWUP_2026-10-01.md)明确列出的三项长期工作，并修复实现时确认的应用恢复和持久预算缺口。[7 月方案](MAKEWAND_OPTIMIZATION_PLAN.md)保留其历史基线与发布观察要求；本记录说明现在已经存在的代码、操作入口和持续验证方式。

| 工作 | 已实现的行为 | 持续验证 |
|---|---|---|
| 独立可信验收 | Go `stdio-v1` 父进程持有预期结果，以固定的受信运行时执行候选黑盒行为；只读工作区、隐藏预期、PID/IPC/网络隔离；记录绑定规范、运行时、完整基线、候选和交付摘要，父进程私有 MAC 拒绝导入证书；TUI 候选 autopilot 自动应用必须持有有效记录并再次验证 | `internal/engine/trusted_acceptance_test.go` 的真实 bubblewrap 对抗用例，TUI 自动与人工应用回归；CI 强制要求可运行的 bwrap |
| Windows 原生流程 | Python 固定目录/文件句柄拒绝 junction、reparse、ADS 和歧义名称；合法 8.3 根目录别名按文件身份识别；生成使用独立副本，实际测试、封存、复审后应用；原子替换/删除/恢复保留 DACL 和 readonly 属性；Job Object 管理启动、进程树、输出、时间、进程数与内存 | `Architecture runtime` 的 `windows-latest` 实机作业运行 native 后端、Git/非 Git 完整流程、硬中断恢复、Go 文件身份/事务/共享预算 |
| 负载与恢复演练 | `cmd/server-drill` 发起实际 HTTP 多租户竞争，测量延迟与吞吐，核查 Router 身份隔离、取消、未知消费、在途 SIGKILL 与重启、SIGTERM 流式排空；真实 WAL 热备份、损坏归档与 schema 检查、多组件恢复中断 | 每次 push/PR 执行短 profile 并保存 JSON；`make drill-long` 执行 10,000 请求及 100,000 Router 调用 |
| 持久预算 | SQLite 整数微美元准入与余额汇总；独立进程共享 reservation，使用与结算同事务提交、重复回调幂等；确认无消费可退款，未知或中断消费保守保留预留 | 多个独立 SQLite 对象并发争抢预算、重启、失败用量、重复结算与真实 HTTP 演练 |
| Provider 并发限制 | 每个 API provider 默认最多 8 个并发，本地/订阅 CLI 默认 1 个；请求视图共享额度，流式调用持有许可至结束或取消；排队超时与取消不触发模型、不扣调用预算 | 并发上限、共享视图、取消竞争、流式许可、半开熔断探针与 race 回归 |
| Go 进程生命周期 | 普通命令、Router CLI、原始 CLI 与 preview 使用统一进程清理；Windows 挂起启动后加入 Job；stdout/stderr 合计 10 MiB，超限标记 UNKNOWN 并停止进程树，leader 退出不会遗留持管道后代 | 真实输出洪泛、静默超时、leader 退出、后代逃逸与 preview 提前结束回归；Windows 实机 Job 门禁 |
| 候选应用恢复 | Go TUI 使用受锁保护的持久 preimage/postimage 日志、登记的 sibling 临时文件、固定根目录和提交标记；Python 在每次写入前持久化恢复计划；打开的根目录句柄绑定原目录身份，Windows 日志绑定应用前后 DACL；全部恢复输入先检查，内容或权限冲突保留现场，恢复可再次中断 | Go 真 kill/reopen、目录交换与临时文件/外部编辑冲突回归；Python 真实子进程硬退出及恢复重放；Windows ACL 整批预检与原权限恢复 |
| Go 待审批恢复 | 项目外私有 SQLite 保存候选 payload、完整基线、固定目录身份、审批状态与候选尝试审计；新进程恢复单次人工审阅，旧父进程验收授权不导入；已完整提交的 postimage 不重复应用，过期异步完成不能结束新审批 | 真实进程退出后 NewApp 恢复、重新批准/拒绝、记录篡改、依赖编辑、两实例撤销/覆盖、旧完成消息与 Windows ACL 回归 |
| Server 恢复事务 | 归档路径/类型/数量/大小/hash 与 DB integrity、FK、schema 全部预检；数据库、sidecar、auth、session 和 admin secret 由单写者持久恢复日志覆盖；启动恢复在打开服务存储前执行 | 恢复期间实际 SIGKILL、幂等回滚、旧 schema 重试、截断/篡改/路径穿越拒绝 |

## 使用入口

独立验收规范放在项目外，用 `MAKEWAND_TRUSTED_ACCEPTANCE_FILE` 指定；SDK 可用 `ContextWithTrustedAcceptance`。规范示例和等级定义见[验收契约](VERIFICATION_CONTRACT.md)。仅通过候选可控制的本地测试仍为 Strength 1；没有配置受信验收时仍需人工接受。

Go SDK 的多文件交付使用 `Project.ApplyFilesTransactional`，或显式调用 `RecoverInterruptedApply`。TUI 已使用事务入口，项目打开会自动恢复。低层 `WriteFile`/`WriteFiles` 仍是直接写入 API，`CheckpointFiles` 是进程内检查点。

TUI 待审批状态独立存放在配置目录的 `pending-approvals/`，原聊天会话格式保留。SDK 可用 `SavePendingApproval`、`LoadPendingApproval` 和 `RecordCandidateAttempts` 管理恢复证据；记录不构成执行授权。恢复后的文件、依赖与测试审批都需重新人工确认，单次完成后停止旧命令链。完整记录上限为 16 MiB，基线最多 100,000 文件、512 MiB；超过限制或基线冲突会停止交付并保留诊断。

Python 下次应用会自动检查未完成日志；SDK 也可调用 `CandidateManager.recover_interrupted_applications(base_cwd)`。后续用户修改和损坏备份会停止恢复并保留证据；成功提交或恢复后也保留 Python 备份记录。

Server 可以用 `--api-concurrency`、`--cli-concurrency`、`--provider-queue-timeout` 调整 provider 的并发与排队上限；SDK 使用 `RouterConfig` 或首次准入前调用 `ConfigureConcurrency`。流式请求不会在刚返回通道时提前释放许可。

```bash
# Linux：可信验收、应用恢复和短服务演练
make test-architecture

# 较长的本地 HTTP/Router/恢复 profile；不调用真实模型
make drill-long

# Windows runner / 原生 Windows 主机：实际执行而非交叉编译
python -I scripts/test_python.py test_native_windows test_native_windows_pipeline test_native_delivery test_candidate_recovery
```

报告默认写入 `benchmarks/results/architecture/`。本轮完整回归、最终构建的长 profile、源代码绑定信息和远端作业记录独立保存在 `benchmarks/results/architecture-v7/`，不替换历史模型评测或上一轮 v6 证据。

## 能力范围

受信验收证明明示的有限行为用例通过，不能证明任意程序安全或覆盖所有需求。证书仅在签发它的父进程内有效，重启后重新验收。当前 Strength 2 要求 Linux bubblewrap；Windows Job Object 是资源管理机制，不提供文件、凭据或网络沙箱，执行生成代码仍遵守现有显式宿主授权。

Windows 实机流程使用离线模型响应夹具，文件、Git、测试进程、封存、应用和恢复执行真实实现；这不等于认证所有第三方模型 CLI 或所有 Windows 文件系统。Go 的 Windows 实机门禁覆盖其指定的身份、事务及预算 API。

多文件事务提供进程中断后的可恢复语义，不保证所有文件同时可见，也不认证硬件断电和网络文件系统。外部编辑器不参与 makewand 的应用锁；冲突时停止恢复而不覆盖编辑。

预算预留仍是每次消费的估计。未知真实价格或实际费用高于预留时，不能宣称供应商账单严格受该金额上限约束。故障演练测量这台机器的 RPO/RTO，不能代替实际部署的 SLA。Server 保持 Alpha；原方案的连续三个真实版本发布证据窗口需由真实发布累积。
