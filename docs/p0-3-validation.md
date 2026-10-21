# P0-3 离线验证与验收说明

3a：任务、项目自然日、全机自然日（本地时区，或 `MAKEWAND_BUDGET_TIMEZONE`；原为上海时间）分别保有上限和计数；在同一个账本文件的独立 scope map 中存放，以文件锁和原子替换作为跨 Go/Python 的事务边界。只传任务上限的入口使用稳定共享路径，daemon/重试/修复/审查继承任务身份。旧账本迁移保留旧生命周期硬上限，旧已花次数保守约束各 scope，不会借迁移清零。

3b：在同一准入事务中检查供应商、实际账号和订阅额度池并发；5 秒心跳、30 秒租期；启动前写不确定性标记，启动后记录进程身份。租期过期但 owner、worker 或进程组成员仍存活时不能重复派发。已启动的失败、取消、未知结果不会退款；重复结算不改写首次结果；已释放的预留不能重新启动。

## 修改文件

- 预算与租约核心：`execution/{context,ledger,layered,leases,lease_process_linux,lease_process_other}.go`；`makewand/{call_budget,layered_budget,account_leases}.py`。
- 入口与生命周期接线：`cmd/makewand/execution_budget.go`，`makewand/{cli,execution_runtime,windows_process}.py`，`makewand/providers/base.py`，`router/{execution,cli}.go`。
- 新增测试：`execution/{layered,leases}_test.go`、两平台租约进程测试辅助文件，`router/account_lease_test.go`，`tests/test_layered_budget.py`、`tests/test_account_leases.py`。
- 修改测试：`cmd/makewand/execution_budget_test.go`、`tests/test_workflow_scheduling.py`、`tests/test_daemon_contract.py`；入口断言从“独立文件”改为“共享文件”，新增真实 daemon IPC 的假 provider 拒绝验证。
- 配置与限制说明：`docs/layered-call-admission.md`。

## 验证结果

- `gofmt -l .`：空；`git diff --check`：通过。
- `go build ./...`：通过；`GOOS=linux/windows/darwin go vet ./...`：三平台均通过。
- `go test -race ./execution ./cmd/makewand ./cmd/versus`：三包均通过，包含跨语言多进程预算/租约抢占和自动心跳测试。
- `go test ./router -run '^TestLayeredBudgetAndAccountRefusalNeverStartCLI$'`：通过，两个子用例分别验证全机预算拒绝和账号容量拒绝时假 CLI 未启动。
- Python：12 个受影响文件共 **185 passed、5 skipped、107 subtests passed**。跳过项为当前平台不适用的原生 Windows 测试。
- router 全包复跑的基线失败见下表；未出现本项新增失败。

Go 命令固定 `GOCACHE=/tmp/makewand-p0-tmp/gocache`、`GOTMPDIR=/tmp/makewand-p0-tmp/gotmp`、`TMPDIR=/tmp`、`GOPROXY=off`、`GOTOOLCHAIN=local`。所有测试清掉 COMMON 指定的大小写代理变量。Python 使用 `python3 -I -m pytest -q -p no:cacheprovider` 和 `tests/_isolation.py`，进程与 IPC 均为假 CLI/静态样本。

Python 文件：`test_account_leases.py`、`test_layered_budget.py`、`test_call_budget.py`、`test_execution_runtime.py`、`test_call_budget_path_security.py`、`test_provider_runner_resources.py`、`test_native_windows_pipeline.py`、`test_provider_outcomes.py`、`test_daemon_contract.py`、`test_workflow_scheduling.py`、`test_protected_workflow.py`、`test_api_execution_contract.py`（均在 `tests/`）。

## 基线也失败的 router 测试

在仓库内部展开修改前的 `git archive`，使用同样的离线环境复跑整个 router 包；以下 9 个顶层测试在基线、3a 和 3b 均出现相同失败。没有将它们标成通过。

| 测试 | 原因 |
| --- | --- |
| `TestCLIProcessOversizedLineCannotBlockWait` | 假进程就绪文件未写出。 |
| `TestCLIProcessStreamingQuietCancellationAndLeaderExit`（quiet、leader） | 假子进程就绪超时。 |
| `TestCustomCLIRefusalIsRetried_R4` | counter 假 CLI 无法写状态文件，退出 93。 |
| `TestRemoteOriginCLIHonorsExplicitWorkDir` | record-codex 假 CLI 无法创建/写调用记录，退出 94。 |
| `TestCLIProvider_ChatStream_EmitsTimeoutErrorOnDeadline`（cancel_after_real_readiness） | 假进程就绪超时。 |
| `TestCLIProvider_Chat_RetriesExplicitQuotaRejection` | counter 假 CLI 退出 93。 |
| `TestCLIProvider_Chat_DoesNotRetryNonTransientError` | counter 假 CLI 退出 93。 |
| `TestCLIRetryEachRealProcessConsumesBudgetAndUnknownDoesNotRetry` | counter 假 CLI 退出 93。 |
| `TestHTTPFacadeReviewNeverRunsCodexReviewUncommittedInServerCwd` | 6 个子用例中 record-codex 假 CLI 退出 94。 |

93/94 的含义核对了 `internal/testfixture/cli.go`；这些文件在当前沙箱运行环境中不能按测试预期写出。没有修改与本项无关的 sandbox bind/测试夹具。

额外核对了显式全局设置临时 `CODEX_HOME/CLAUDE_CONFIG_DIR` 的运行方式。该方式与部分既有夹具自行选择的登录目录不一致，导致以下 8 个测试额外失败；同参数的未修改基线也全部复现，因此最终 router 全包比对保留夹具原有的目录设置：

- `TestAuditFix_ProviderStateNeverBoundWritable`
- `TestWrapCLICommandWithSandbox_CodexStateShielding`
- `TestCLIStreamEndedByCancellationWritesBackFirst_Final2`
- `TestRouterCLIStreamEndedByCancellationWritesBackFirst_Final2`
- `TestCLIStreamCancelledRefreshSurvivesProcessExit_Final2`
- `TestWritebackSyncFinal_ChatReturnsAfterTheWriteBack`
- `TestWritebackSyncFinal_StreamEndsAfterTheWriteBack`
- `TestWritebackSyncFinal_HealthProbeReturnsAfterTheWriteBack`

上述失败来自夹具路径/凭据写回断言。账号租约新增测试只使用显式静态假账号引用与临时假身份文件；原有无租约入口也不新增身份读取。

## 风险与 Claude 验收重点

- 全机计数的边界是**共同使用的账本**：不同 OS 用户/显式独立文件/直接供应商 CLI 无法自动合账。项目 worktree 需统一 `MAKEWAND_PROJECT_ROOT`。这不是受保护的委派授权存储。
- 无法证明进程结束时保留租约，优先避免超发，代价是名额可能需要人工核对回收；macOS/Windows 崩溃回收也使用这一保守策略。脱离已跟踪进程组的后代需要更强 OS 监督，不能宣称已完全自动回收任意后台树。
- 日期桶按本地时区（或 `MAKEWAND_BUDGET_TIMEZONE`）的午夜划分。时钟回拨跨日不拒绝准入，继续计入已见过的最新一天；在实际日期到达那一天之前，项目与机器的每日上限按从今天到那一天的各日期桶（含预留）合计检查。回拨不清零、不复活已过期预留。详见 `docs/layered-call-admission.md`。账本保留历史，16 MiB 上限达到时拒绝，归档需确认无活动派发。
- 重点审查：Go/Python 对同一账本最后一个次数/账号名额的竞争；账号未知与同账号多目录；启动标记和进程登记之间的崩溃；取消结算时 worker 仍活着；跨午夜审查预留；旧账本迁移的保守计数；daemon 环境快照中的稳定账本与任务身份。
