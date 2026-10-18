# P1-C 模型目录元数据验证记录

基线：`3c1c363`（本次仅处理 `.p0-tasks/P1-next.md` 的 C 节）。

## 修改范围

- `makewand/discovery.py`：保留 agy 外部目录；记录候选工具、账号引用、家族、实际模型（未执行时未知）、额度池、目录来源、缓存时间及调用 ID 验证状态。未验证外部 ID 不参与自动选择；原生 Gemini 保留已有路由。目录存在不代表执行验证成功。
- `makewand/engine_selection.py`、`makewand/orchestrator.py`：按模型家族过滤审查者，覆盖首轮、备用、修复后复审和 race；考虑实现档位、auto 的所有可能档位及显式模型覆盖。未知家族不能证明独立性。
- `makewand/cli.py`、`makewand/health.py`：agy 额度显示未知，附来源与状态观察时间；补充 `models --json` 元数据和 Claude/Opus 排名解释；已有 `--tier`、`--model` 覆盖继续有效。
- `tests/test_p1_c_model_metadata.py`：新增 13 项静态样本/假执行器测试；`tests/test_pacing.py`：把 agy 的额度预期由 100 改为未知。
- `docs/model-catalog-metadata.md`：记录字段语义、自动选择规则和使用方法。

## 检查结果

所有测试命令均清除大小写代理变量；Go 使用 COMMON.md 指定的 GOCACHE、GOTMPDIR、TMPDIR，并设置 `GOPROXY=off GOTOOLCHAIN=local`，未下载依赖。

- `gofmt -l .`：空输出；`git diff --check`：通过。
- `go build ./...`：通过。
- `GOOS=linux go vet ./...`、`GOOS=windows go vet ./...`、`GOOS=darwin go vet ./...`：均通过。
- `python3 -I -m pytest -q -p no:cacheprovider` 对下列 28 个受影响文件：362 passed、1169 subtests passed。随后补充的两项测试及其同文件测试再次运行：13 passed。最终共有 364 个不同测试通过。

```text
tests/test_p1_c_model_metadata.py
tests/test_discovery_provenance.py
tests/test_fix_health_discovery.py
tests/test_model_discovery_root_fix.py
tests/test_pacing.py
tests/test_routing_sync.py
tests/test_fix_orch_review_stand_in.py
tests/test_fix_r4_orch_review_stand_in.py
tests/test_fix_orch_race_review.py
tests/test_workflow_scheduling.py
tests/test_g2_review_routing_engines.py
tests/test_g2_review_routing_intent.py
tests/test_g2_review_routing_verdict.py
tests/test_g3_reliability_routing.py
tests/test_cli_message_parity.py
tests/test_health.py
tests/test_hybrid_routing.py
tests/test_multi_model_optimization.py
tests/test_p1_r_remaining.py
tests/test_fix_final2_forced_engine.py
tests/test_fix_final2_race_pairing.py
tests/test_fix_final2_single_seam.py
tests/test_fix_health_engine_usable_r3.py
tests/test_execution_evidence.py
tests/test_autofix_early_stop.py
tests/test_race_winner_take_all.py
tests/test_quota_freshness.py
tests/test_quota_reserve_setting.py
```

隔离器阻止了部分已有测试对 `127.0.0.1:9` 的不可达本地模型探测，测试通过，未发生外网或真实模型调用。

## Go 包的既有失败

`go test ./router ./cmd/makewand` 未通过。本次没有修改任何 Go 文件、go.mod 或 go.sum。另用 `git archive 3c1c363` 在 `/tmp/makewand-p0-tmp/c-baseline` 建立静态基线副本，复跑同一组 Go 测试核对失败。临时副本首次构建缺少 VCS 元数据；对 CLI 包以 `GOFLAGS=-buildvcs=false` 重跑，排除这个副本因素后仍复现已知失败。详细名单见下表。

| 包 / 失败测试 | 原因 |
| --- | --- |
| `router` / `TestCLIProcessOversizedLineCannotBlockWait` | 假子进程没有生成 oversized-line.ready，测试无法进入原有超长行验证。 |
| `router` / `TestCLIProcessStreamingQuietCancellationAndLeaderExit`；子场景：`quiet`, `leader` | quiet、leader 两种假子进程的 readiness 均未出现，等待 context 超时。 |
| `router` / `TestCLIProvider_ChatStream_EmitsTimeoutErrorOnDeadline`；子场景：`cancel_after_real_readiness` | cancel_after_real_readiness 场景未观察到真实测试子进程就绪，context 超时。 |
| `router` / `TestCLIProvider_Chat_DoesNotRetryNonTransientError` | 错误被封装为结果未知，原测试预期直接暴露 stderr。 |
| `router` / `TestCLIProvider_Chat_RetriesExplicitQuotaRejection` | 假 CLI 退出 93，被判定结果未知，未按原测试预期重试成功。 |
| `router` / `TestCLIRetryEachRealProcessConsumesBudgetAndUnknownDoesNotRetry` | 假 CLI 退出 93 后停止于结果未知，未达到测试预期的重试额度上限。 |
| `router` / `TestCustomCLIRefusalIsRetried_R4` | 假 CLI 退出 93，被判定结果未知并停止重试，测试预期收到重试答案。 |
| `router` / `TestHTTPFacadeReviewNeverRunsCodexReviewUncommittedInServerCwd`；子场景：`auto-classified_review`, `openai_client_default_model_alias_with_/review`, `power_mode_chinese_review_with_explicit_codex`, `power_mode_ensemble_without_explicit_model`, `responses_api_review`, `streaming_review` | 假 Codex CLI 退出 94 后响应 503；power ensemble 子场景也没有成功记录 Codex 尝试。 |
| `router` / `TestRemoteOriginCLIHonorsExplicitWorkDir` | 假 Codex CLI 退出 94，执行层返回结果未知。 |
| `cmd/makewand` / `TestE2ENativeKnownProviderFailureExit10`；子场景：`offline_quota_exceeded`, `synthetic_provider_refusal` | 两个已知失败场景均返回 17（UNKNOWN），原测试要求退出码 10。 |

## 风险与 Claude 验收重点

- 本轮不验证真实 Argon/外部模型调用 ID；没有成功执行证据时继续显示未验证且默认不选。已有原生 Gemini 路由仍可自动选择，界面不会把它冒充已执行验证。
- 账号/额度池无法绑定、未来候选尚未执行时，字段保持未知；历史 ID 验证不证明本次实际模型。实际模型仍看单次执行证据。
- 家族识别依据模型 ID；未知别名不能证明独立审查。auto 涉及多个可能家族时采取保守过滤，可能减少可用审查者。
- Claude 重点核对：agy 的 Claude 与 Claude Code 是否始终被视为同家族；备用和修复后复审是否绕过过滤；race 实现档位与审查档位是否混淆；Opus 显式覆盖和排名解释是否一致；agy 文本/JSON 额度是否均保持未知。
- 本次不修复 R/E/T 范围的已有问题，不调用真实模型、不读取或改写真实凭据、不运行 makewand 派发命令、不推送。
