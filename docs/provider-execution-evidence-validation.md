# E：provider 执行证据持久化验收报告

本项只实现 `.p0-tasks/P1-next.md` 的 E 节；基线为
`8e2e5d4`（`feat/unified-dispatch-p0`）。未执行 makewand 派发命令，未调用真实模型，
未读取或修改真实凭据。所有账号、CLI 输出和用量数据均为临时目录里的假文件或静态样本。

## 改动文件与行为

| 文件 | 变化 |
| --- | --- |
| `execution/evidence.go`、`execution/ledger.go` | Go 按 attempt 原子保存证据；执行前保存 UNKNOWN，完成时补全状态；实际 token 计数进入原有结算。 |
| `router/cli_evidence.go` | 白名单投影私有账号、实际模型、额度窗口、用量；提取结果写入独立证据记录。 |
| `router/cli.go`、`router/cli_sandbox.go`、`router/cli_sandbox_private_home.go` | 绑定本次 attempt；私有 home 删除前保存；普通调用和流式调用记录真实进程退出码。 |
| `router/execution.go` | 普通非零 CLI 退出无法证明远端结果，归为 UNKNOWN，阻止自动重派；明确的额度拒绝保留既有处理。 |
| `router/quota.go` | Codex 额度读取合并持久证据；仍要求账号/额度池匹配并检查原始观察时间与重置时间。 |
| `makewand/execution_evidence.py`、`makewand/execution_runtime.py` | Python 同一记录格式与生命周期；保存失败阻止派发或交付；已观察 token 数进入结果与预算账本。 |
| `makewand/sandbox.py` | 私有 home 清理前投影证据并持久化，保留既有凭据刷新回写行为。 |
| `makewand/providers/base.py`、`makewand/providers/codex.py` | 仅明确的 JSON 协议输出可提供证据；非流式 Codex 使用 `--json`，向调用者返回 agent message。兼容 Windows runner。 |
| `makewand/providers/api_client.py` | API 调用及重试不能冒认订阅登录账号或共享额度池。 |
| `makewand/health.py` | Python 额度读取消费同一持久证据，拒绝其他账号、其他池、陈旧和未来观察及过期窗口。 |
| `execution/evidence_test.go`、`router/cli_evidence_test.go`、`tests/test_execution_evidence.py` | 新增纯离线证据生命周期与回归测试。 |
| `docs/provider-execution-evidence.md`、本报告 | 记录存储位置、schema、结算关联方式、验证结果与限制。 |

默认存储为平台用户缓存目录下的 `makewand/execution-evidence/<attempt_id>.json`；
可用 `MAKEWAND_EXECUTION_EVIDENCE_DIR` 指定目录。不依赖调用预算和可选遥测开关。
记录仅包含引用、模型 ID、数字额度/用量、来源/时间、真实退出码和结果状态，
不复制令牌、提示词正文、回答正文、原始错误、凭据文档或私有 home 路径。
账号引用沿用 P0-2 对实际明文账号标识进行哈希的规则；请求模型不冒充实际模型。

## 测试与检查

所有测试命令均清除了 COMMON.md 指定的八个代理变量，并使用规定的
`GOCACHE`、`GOTMPDIR`、`TMPDIR`。`GOPROXY=off`，没有下载依赖。
Go 测试额外使用临时 HOME/缓存及假 CLI；Python 使用仓库已有隔离机制。

- `gofmt -l .`：空；`git diff --check`：通过。
- `go build ./...`：通过。
- `GOOS=linux go vet ./...`、`GOOS=windows go vet ./...`、`GOOS=darwin go vet ./...`：通过。
- `MAKEWAND_TEST_SIMULATE_NO_BWRAP=1 go test ./router ./execution`：两个包全量通过。
- `go test -race ./execution ./router -run 'Test(ProviderEvidence|.*ExecutionEvidence)'`：通过。
- Python 受影响测试：使用 `python3 -I -m pytest -q -p no:cacheprovider`；
  最终合并运行 **329 passed、4 skipped、151 subtests passed**。覆盖新增证据测试，以及执行运行时、账号绑定、额度新鲜度、
  sandbox、provider outcome/runner、API 接力、Windows、分层预算、租约和调用预算回归。

新增测试覆盖：删除私有 home 后仍可查询账号/模型/额度；删除前已有 UNKNOWN 快照；
同一账号的跨运行读取；其他账号不能借用证据；私有账号切换后按实际登录绑定；
符号链接拒读；模型正文中的 JSON 不作为协议证据；普通文本流不作为 JSON 协议；
过期额度不授权；正退出码故障、超时、中断不自动重试；证据写入失败阻止交付；
无调用预算时仍持久化；并发 attempt 不互相覆盖；实际 token 数结算；Claude 模型投影；
Codex JSON 返回文本兼容；API 账号未知。

### 默认 bubblewrap 模式的基线失败

规定环境中，默认 `go test ./router ./execution` 的 router 存在以下 9 组既有失败。
已从 `8e2e5d4` 导出干净基线，在相同环境完整复跑确认；execution 包通过。
这些问题属于假 CLI 的挂载/临时目录可见性，本项未修改其测试夹具。

| 测试 | 基线上复现的原因 |
| --- | --- |
| `TestCLIProcessOversizedLineCannotBlockWait` | 真实假进程的 `.ready` 文件不可见。 |
| `TestCLIProcessStreamingQuietCancellationAndLeaderExit`（quiet、leader） | 假子进程未能产生可见就绪标记，等待超时。 |
| `TestCustomCLIRefusalIsRetried_R4` | 假 CLI 计数文件不可用，夹具退出 93。 |
| `TestRemoteOriginCLIHonorsExplicitWorkDir` | 假 Codex 无法创建调用日志文件，夹具退出 94。 |
| `TestCLIProvider_ChatStream_EmitsTimeoutErrorOnDeadline/cancel_after_real_readiness` | 假子进程未能到达可见就绪状态。 |
| `TestCLIProvider_Chat_RetriesExplicitQuotaRejection` | 假 CLI 计数文件不可用，夹具退出 93。 |
| `TestCLIProvider_Chat_DoesNotRetryNonTransientError` | 同上，未运行到预期 stderr 分支。 |
| `TestCLIRetryEachRealProcessConsumesBudgetAndUnknownDoesNotRetry` | 同上，未运行到预期额度拒绝分支。 |
| `TestHTTPFacadeReviewNeverRunsCodexReviewUncommittedInServerCwd` | 六个子场景均受假 Codex 调用日志不可写（94）影响，返回 503 或缺少预期追踪。 |

无 bubblewrap 模式通过完整 router 测试，使用的是仓库原有的测试授权路径和假 CLI，
没有运行真实 provider。首次扩大 Python 回归时曾出现 Muse guard/socket 权限失败；
随后当前代码与干净基线的对照重跑均通过，最终结果不把这些瞬时失败列为持续故障。

## 剩余限制及 Claude 验收重点

- CLI 没有提供账号、实际模型、额度或用量时，保留未知；普通文本流和 Codex 本地
  `review` 的输出不会被猜成协议。其他 provider 暂无账号/额度白名单投影。
- 未以真实订阅调用验证厂商 CLI 格式；需重点验收 schema 与静态样本是否覆盖当前格式。
- 证据是本地 CLI 观察，不是厂商签名的计费回执。宿主被直接杀死时可能只留下初始 UNKNOWN，
  不能据此声称完成。结果未知不得自动重派，调用预算也不会因已派发失败而返还。
- 记录目前不自动清理；额度只读取最新 64 条记录，并保持 15 分钟 TTL。
  应重点检查长期保留量和后续额度观测覆盖策略。
- 请重点检查：私有 home 删除前保存的顺序、账号切换/池绑定、token 用量与账本按 attempt ID
  对应、JSON 正文不能污染证据、保存失败和超时的禁止重派行为，以及现有凭据刷新回写未被破坏。
