# P1-T 假 CLI 故障场景验证

基线：`276dbd1`。本轮只处理 `.p0-tasks/P1-next.md` 的 T 节，增加测试与验证记录，未改变生产代码。

## 文件与测试边界

- `tests/fixtures/p1_fault_cli.py`：固定行为的 Python 假 CLI。只访问父测试创建的临时工作区、调用计数及场景文件；输出由本轮提示中的 nonce 绑定的裁决，或模拟缺行、修改文件、退出 7、自杀及挂起。使用当前 Python 的绝对路径启动，不搜索已安装的模型 CLI。
- `tests/test_p1_fault_scenarios.py`：13 项真实子进程测试。复用 `_isolation` 隔离 HOME、配置、模型 CLI 与网络；使用真实 Codex/Claude provider 适配器、Git、`run_subprocess`、执行账本/证据记录、裁决解析、确定性本地测试、影子交付及应用后核验。
- 路由固定为假环境中的候选；provider 沙箱启动传输只启动上述假 CLI，订阅可用性与模型目录由静态数据替代；本地测试传输只执行静态测试。退出码到执行结果的转换由真实 provider 适配器完成。未调用任何真实模型、makewand 派发命令或真实凭据。

## 场景与断言

| 要求 | 测试 | 必须观察到的结果 |
| --- | --- | --- |
| 缺少 VERDICT 行 | `test_missing_verdict_preserves_patch_after_exactly_one_followup` | 恰好一次同审查者追问，最终 UNVERIFIED/11，保存完整候选补丁，不进入修复或交付。 |
| 伪造“测试通过”文字 | `test_forged_test_success_cannot_override_local_failure` | 假编码器持续声称通过、审查者持续明确批准，但真实静态测试始终失败；初始和两轮修复都不能交付，最终 FAILED/10。 |
| 超时导致结果未知 | `test_coder_timeout_does_not_replay_partial_patch`、`test_reviewer_timeout_cannot_authorize_delivery_with_partial_pass` | 子进程先写补丁或输出有效 pass 行，再挂起；真实 runner 超时后为 TIMEOUT/16，账本和证据均为 outcome_known=false，不切换备用、不追问、不修复。 |
| 被杀导致结果未知 | `test_killed_coder_does_not_replay_partial_patch`、`test_killed_reviewer_cannot_authorize_delivery_with_partial_pass` | 子进程先写补丁或输出有效 pass 行，再实际 SIGKILL；最终 UNKNOWN/17，账本/证据保持未知，不重派。 |
| 审查后补丁变化 | `test_changed_patch_after_approval_is_not_published` | 假审查者批准旧 diff 后同步写入新版本，真实输入快照核验拒绝发布，最终 FAILED/10。无依赖调度时序的竞态。 |
| 没有补丁却声称完成 | `test_no_patch_despite_completion_claim_is_not_delivered` | 编码器退出 0 并声称完成但没有写补丁；不得发布空交付，最终 FAILED/10。 |
| 影子补丁未应用却声称成功 | `test_fake_apply_success_without_writing_patch_is_rejected` | 已验证的交付脚本调用假 git；假 git 退出 0、声称已应用却不写文件，真实后像核验拒绝成功，宿主内容不变。调用标记证明实际经过假 apply。 |
| 非零退出与通过输出矛盾 | `test_nonzero_coder_exit_overrides_success_prose`、`test_nonzero_reviewer_exit_overrides_valid_pass_line` | 退出 7 优先于通过文字或有效 pass 行，最终 UNKNOWN/17，不能重派或交付。 |
| 零退出与失败裁决矛盾 | `test_zero_exit_cannot_override_structured_failed_verdict` | 每个子进程都退出 0，但失败裁决始终阻止交付，修复上限后为 FAILED/10。 |
| 正常对照 | `test_success_control_requires_explicit_application` | 正常生成、实际测试和 nonce 审查均通过；影子结果可以封存，宿主仍保持基线。显式运行交付脚本并完成后像核验后，宿主才出现批准内容。 |

拒绝交付的流水线场景同时检查：工作流状态与退出码、宿主完整基线不变、没有已发布影子分支、没有可应用交付脚本。未知结果额外检查真实调用次数、预算账本及持久化证据，避免只凭控制台文字判断。

## 验证结果

所有测试清除大小写代理变量，Go 使用 COMMON.md 指定的缓存与临时目录，并设置 `GOPROXY=off`、`GOSUMDB=off`、`GOTOOLCHAIN=local`，不下载依赖。

- 新增测试单文件：13 passed。
- 下列 9 个 Python 文件联合回归：167 passed、50 subtests passed。
- `gofmt -l .`：空输出；`git diff --check`：通过。
- `go build ./...`：通过。
- `GOOS=linux go vet ./...`、`GOOS=windows go vet ./...`、`GOOS=darwin go vet ./...`：全部通过。

Python 回归范围（以 `python3 -I -m pytest -q -p no:cacheprovider` 运行）：

```text
tests/test_p1_fault_scenarios.py
tests/test_delivery_binding.py
tests/test_execution_evidence.py
tests/test_provider_runner_resources.py
tests/test_g2_review_routing_verdict.py
tests/test_fix_final2_typed_apply.py
tests/test_fix_orch_race_review.py
tests/test_hybrid_integrity.py
tests/test_review_verdict.py
```

联合回归时，隔离器拒绝了已有测试对不可达本地端点 `127.0.0.1:9` 的探测，测试仍通过，未连接真实模型或外网。

## Go 回归与基线失败

额外运行 `go test -json ./execution ./router ./internal/engine`，使用临时 HOME、配置及 AI CLI 拦截桩，未访问真实凭据。`execution` 通过；`internal/engine` 除下述 Cargo 环境问题外通过。首跑 Cargo 测试 `TestCargoProjectWithoutLockfileVerifiesLive_R3` 因临时 HOME 没有 rustup 默认工具链失败，指定本机已有的 `RUSTUP_HOME`、`RUSTUP_TOOLCHAIN`、临时 `CARGO_HOME` 和 `CARGO_NET_OFFLINE=true` 后单项复跑通过，未下载工具链或依赖。

`router` 的以下失败均在 `git archive 276dbd1` 的基线副本中，用同一隔离环境及测试名筛选重新复现。本轮没有修改 Go 文件、go.mod 或 go.sum，不修复这些既有问题。

| 失败测试 | 原因 |
| --- | --- |
| `TestCLIProcessOversizedLineCannotBlockWait` | 假子进程未生成 `oversized-line.ready`，未进入超长行验证。 |
| `TestCLIProcessStreamingQuietCancellationAndLeaderExit`（`quiet`、`leader`） | 等待假子进程 readiness 超时。 |
| `TestCLIProvider_ChatStream_EmitsTimeoutErrorOnDeadline`（`cancel_after_real_readiness`） | 未观察到真实测试子进程就绪，context 超时。 |
| `TestCLIProvider_Chat_RetriesExplicitQuotaRejection` | 假 CLI 退出 93，被判定 UNKNOWN，未达到原测试预期的重试成功。 |
| `TestCLIProvider_Chat_DoesNotRetryNonTransientError` | 假 CLI 退出 93 后返回 UNKNOWN，原测试预期直接暴露 stderr。 |
| `TestCLIRetryEachRealProcessConsumesBudgetAndUnknownDoesNotRetry` | 假 CLI 退出 93 后停止重试，未达到预期重试预算。 |
| `TestCustomCLIRefusalIsRetried_R4` | 假 CLI 退出 93 后返回 UNKNOWN，原测试预期收到重试答案。 |
| `TestRemoteOriginCLIHonorsExplicitWorkDir` | 假 Codex CLI 退出 94，执行层返回 UNKNOWN。 |
| `TestHTTPFacadeReviewNeverRunsCodexReviewUncommittedInServerCwd`（`auto-classified_review`、`openai_client_default_model_alias_with_/review`、`power_mode_chinese_review_with_explicit_codex`、`power_mode_ensemble_without_explicit_model`、`responses_api_review`、`streaming_review`） | 假 Codex CLI 退出 94，HTTP 场景响应 503；power ensemble 场景未记录成功的 Codex 尝试。 |

## 风险与 Claude 验收重点

- 本轮验证的是现有交付门禁，没有修改生产行为；生产 CLI 参数、真实模型协议及真实沙箱隔离仍需要各自的独立验证。
- 该测试类使用 POSIX 的 SIGKILL 与 Bash 交付脚本，Windows 会跳过；三平台 vet 不等于 Windows/macOS 上的运行测试。
- 超时测试要求实际收到部分成功输出，防止“子进程尚未启动就超时”造成假通过；没有采用按睡眠时序竞争修改补丁的办法。
- 影子封存通过与宿主应用完成是两个阶段。正常对照明确检查应用前后内容；假 apply 测试必须在后像核验失败时返回非零，且不能打印最终应用成功提示。
- 验收时重点看未知结果是否在首轮与审查阶段都停止备用派发，以及伪造测试文字是否确实被运行失败的静态测试覆盖。
