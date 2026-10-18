# R：第二轮审读剩余问题的修复与验收

范围：`.p0-tasks/P1-next.md` 的 R 节；对照 `.p0-tasks/FINAL2-REVIEW.md`，在 `f184e6a`（P0-1/P0-2/P0-3a/P0-3b/P1-proxy 之后）修复全部 5 条 P2、10 条 P3。没有跳过或拒绝修复的条目；未实施 E、C、T。

## 逐条对应

| 条目 | 最终行为与主要文件 | 回归证据 |
| --- | --- | --- |
| P2-1 / P3-4 接力重复 | `providers/base.py` 的 `ProviderAPIRelay` 统一五个订阅适配器的 API 策略、拒绝说明和 `api` 路线。只有 key 和允许付费的策略同时成立才接力；默认策略下 needs_auth 仍运行 CLI，limited/缺 CLI 直接失败。每次 CLI 启动报告 `cli`；CLI 限额后接力使用已规范化的 cwd。 | `test_fix_final2_api_policy_relay.py` 覆盖五个适配器、两种策略、缺 key/CLI、缓存状态、运行时限额和 CLI→API 路线；`test_fix_health_engine_usable_r3.py` 的一致性矩阵扩到五个适配器；更新 `test_fix_health_quota_noise.py`，分别断言有 key 但策略禁止和显式允许付费的行为。 |
| P2-2 JSON | `orchestrator.py` 的十处 JSON 分支共用 `_print_json`，通过 `_json_text` 转义代理字符并直接用 builtin print 输出。 | `test_p1_r_remaining.py`：严格 UTF-8 输出流中的非 UTF-8 diff 错误、假审查结果中的非 UTF-8 文件名、saved hybrid 的非 UTF-8 race ID 均可 JSON 往返。 |
| P2-3 race 冻结 | 两次宿主 manifest 使用 `host=True`、共享 `ManifestCache` 和各自的 `ManifestBudget`，初次哈希也在错误处理内。只约束会复制的输入；超限、哈希失败和跟踪文件变化在派发前中止。 | `test_p1_r_remaining.py`：复制期间忽略日志变化不误中止，跟踪文件变化仍中止，哈希异常和快照大小超限不派发；检查预算及缓存共享。 |
| P2-4 wildmatch | 保留 Git 的匹配与 abort 语义，通过缓存状态和显式生成器帧消除 Python 递归；每条候选路径共用最多 100,000 个工作步骤，超限保留测试输入。 | `test_fix_final2_gitignore_fallback.py`：Git oracle 样本增加 6/8 段 `**/` 和 1100 段 `*x`；深路径、长模式、包含否定规则时的预算耗尽测试。 |
| P2-5 Windows 解码 | `.gitignore` 自始至终以字节分行并匹配，不再经 Windows fsdecode 解码。 | 同一 fallback 模块模拟 fsdecode 拒绝非 UTF-8；`test_windows_runtime_regression.py` 增加实际非 UTF-8 注释用例，纳入现有原生 Windows gate 的源码清单。 |
| P3-1 forced engine | `engine_selection.py` 在派发前拒绝未知、占位和缺少 CLI/允许 API 的引擎；`--local-only` 使用忽略外部 provider 后的 `forced` 值执行无可用提供方检查。 | `test_p1_r_remaining.py` 的未知/占位/无缓存安装检查与 `test_fix_final2_forced_engine.py` 的 local-only 回归；原有跨工作流测试继续覆盖拒绝。 |
| P3-2 API-only 提示 | `health.py` 在 CLI 检查前单独处理纯 API 引擎，明确报告缺 key 或被 API 策略阻止；删除不可达分支。 | `test_p1_r_remaining.py` 遍历全部纯 API 引擎的缺 key、策略禁止、允许付费三种情况，断言不检查 CLI。 |
| P3-3 race 提示 | 非 agentic 聊天引擎说明无法生成竞速改动，不建议 `--provider deepseek` 等无效操作。 | 更新 `test_fix_final2_race_pairing.py` 的拒绝原因断言。 |
| P3-4 其他重复 | WTA 审查复用带 `exclude` 的独立裁判选择器；`API_ONLY_ENGINES` 归于 `config.py`，路由、健康、直接命令和配置检查共用。 | 原有 WTA、race、路由测试，加上述 API-only 矩阵。 |
| P3-5 无效开关 | 删除七个模块中仅作初始化用的 STRICT_NONCE 补丁；`_scan_verdict_lines` 默认 `allow_legacy=False`。 | 保留明确验证“旧环境开关无效”的测试；DAG、race、whole-prompt 模块继续运行。 |
| P3-6 目录流规则 | Python 目录加载器拒绝 `lines` 选择读取回答流，含 selection 缺省时；校验流名称。 | `test_p1_r_remaining.py` 覆盖 merged/apart 及显式/缺省 lines；现有消息目录与 Go parity 语料继续运行。 |
| P3-7 模板 info | `git_helper.py` 在隔离副本/临时基线 `git init` 后将模板链接的 info 换为空的本仓库目录，后续属性屏蔽不进入共享模板。 | `test_fix_final2_symlink_exclude.py` 同时覆盖隔离副本和临时基线，比较共享目录全部名称和内容未变。 |
| P3-8 流收尾 | CLI 的流等待覆盖进程退出/管道收尾（原有的默认管道等待界限通过 `processjob.DefaultWaitDelay` 共用，值为 1 秒）加完整回写等待，默认总上限为 16 秒；API 流仍不等待。 | 更新 `stream_cancel_drain_final2_test.go` 的界限断言，新增 teardown 长于单独回写界限的假 provider 取消测试。当前实现直接杀进程组，所需前置等待来自 WaitDelay，而非另加 SIGTERM 重试流程。 |
| P3-9 注释 | 修正 `internal/engine/sandbox.go` 与 CHANGELOG：直接生成文件受路径保护，测试/自动修复进程仍以沙箱 HOME 运行并可写缓存。 | 注释修正；受影响 Go 包正常验证。 |
| P3-10 junction | Windows 元数据根目录拒绝重解析点；子目录/目标文件处的 junction 移除自身再创建本仓库条目，不写入目标。 | `test_windows_runtime_regression.py` 新增真实 junction 根目录/父目录/文件位置三例，进入原生 Windows gate。Linux 上按平台条件跳过。 |

## 验证条件

全部测试使用临时 HOME/配置和假 AI CLI，清除代理变量，不运行真实模型或 makewand 派发命令，不读取真实凭据。Go 测试也通过现有 `tests/_isolation.py` 启用隔离；假凭据仅位于测试临时目录。

环境为 `GOCACHE=/tmp/makewand-p0-tmp/gocache`、`GOTMPDIR=/tmp/makewand-p0-tmp/gotmp`、`TMPDIR=/tmp`、`GOPROXY=off`、`GOSUMDB=off`。使用本机已缓存的 Go 1.26.9；Rust 使用已安装的 stable 工具链，`CARGO_HOME` 为测试临时目录且 `CARGO_NET_OFFLINE=true`。未下载依赖。

## 检查结果

- `git diff --check`、全部受跟踪 Go 文件的 `gofmt -l`：无输出。
- `go build ./...`：Go 1.26.9 与宿主 Go 1.27.2 均通过。
- `GOOS=linux|windows|darwin go vet ./...`：两个工具链的三个目标均通过。
- 定向 Python 回归：82 passed、4 skipped、165 subtests passed。新增 junction 测试及一个已有 Windows DACL 测试在 Linux 上跳过。
- API 接力 cwd 复核后补跑 `test_p1_r_remaining`、五适配器 API 策略/一致性、agy 判定、Windows runner、provider outcome、API client 与 API execution contract：152 passed、295 subtests passed。
- 完整 Python `python3 -I -m pytest -q -p no:cacheprovider tests`：2224 passed、45 skipped、3966 subtests passed，发现 3 个旧 Grok 接力子断言和下述 1 个基线隔离检查失败。旧断言已更新为显式区分 API 策略并复跑。
- 最后一轮受影响 Python 模块（quota noise、API policy、R 回归、gitignore、forced engine、模板链接、引擎一致性及 Windows runtime）：90 passed、4 skipped、580 subtests passed；本次引起的失败均已修复。测试命令均为 `python3 -I -m pytest -q -p no:cacheprovider <文件>`。
- Go 1.26.9：`go test -count=1 -p 2 ./router ./internal/engine ./internal/processjob` 中 engine（117.905 秒）、processjob（0.258 秒）通过；router 完成运行，但有下述 9 项基线失败（190.443 秒），本次新增流等待用例没有失败。
- `go test -race -count=1 ./router -run 'TestCancelled.*Stream|TestCancelledCLIWrapper|TestCLIProviderStreamEndWait'`：通过（2.003 秒）。

### 已在改动前 HEAD 复现的失败

用 `git archive f184e6a` 的独立临时副本、相同环境和假 CLI 复跑下列 9 个 router 测试，Go 1.26.9 下仍全部失败（51.620 秒）；宿主 Go 1.27.2 也复现相同结果。原因是这两个工具链的 `testing.TempDir` 均以 GOTMPDIR 为根，而既有 `cliTestTempDirs` 仅按启动时/当前 os.TempDir 枚举夹具根。本次要求的 GOTMPDIR 与 TMPDIR 不同，导致私有沙箱看不到 Test* 标记、计数或记录路径。R 不改这套无关的夹具目录发现逻辑。

| 测试名称 | 失败表现 |
| --- | --- |
| `TestCLIProcessOversizedLineCannotBlockWait` | oversized-line.ready 不可见，子进程未进入已就绪状态。 |
| `TestCLIProcessStreamingQuietCancellationAndLeaderExit`（quiet、leader） | 子进程 readiness 等待超时。 |
| `TestCLIProvider_ChatStream_EmitsTimeoutErrorOnDeadline`（cancel_after_real_readiness） | 同样等待 readiness 超时。 |
| `TestCustomCLIRefusalIsRetried_R4` | 假 CLI 无法写计数文件，退出 93。 |
| `TestCLIProvider_Chat_RetriesExplicitQuotaRejection` | 假 CLI 无法写计数文件，退出 93。 |
| `TestCLIProvider_Chat_DoesNotRetryNonTransientError` | 假 CLI 无法写计数文件，退出 93。 |
| `TestCLIRetryEachRealProcessConsumesBudgetAndUnknownDoesNotRetry` | 假 CLI 无法写计数文件，退出 93。 |
| `TestRemoteOriginCLIHonorsExplicitWorkDir` | 假 Codex 无法建立调用记录，退出 94。 |
| `TestHTTPFacadeReviewNeverRunsCodexReviewUncommittedInServerCwd`（六个子场景） | 调用记录路径不可见，假 Codex 退出 94，HTTP 返回 503。 |

Python 还有一项基线失败：`tests/test_g5_tests_isolation.py::TestGuardPresence::test_every_owned_test_module_isolates_before_importing_makewand` 的 `test_proxy_defaults.py` 子项。该文件由 P1-proxy 提交加入，自身缺少 `_isolation` 导入，AST 检查因此失败；pytest 的 conftest 仍为其启用隔离。在同一 `git archive f184e6a` 副本上复跑该检查，结果为 1 failed、1 passed、153 subtests passed（2.20 秒），原因完全相同。本次未改这个无关文件。

初跑在完全隔离 HOME 下，`TestCargoProjectWithoutLockfileVerifiesLive_R3` 还因 rustup 找不到默认工具链而失败，改动前 HEAD 也复现。显式选择已安装的 Rust stable、临时 CARGO_HOME、离线模式后，该测试及整个 engine 包通过；没有安装工具或下载依赖。

## 尚存边界与 Claude 验收重点

- 当前机器只实际运行 Linux 测试；Windows/macOS 的 vet 不代表原生运行。请在 Windows gate 验收三个 junction 用例与非 UTF-8 gitignore 用例。
- wildmatch 达到预算时保守地保留文件，可能增加需要核验的输入，不会因超限放行被修改的输入。请关注长模式与否定规则的边界。
- 模板链接的 info 被替换为空目录；不会继续继承该共享目录内的模板规则。`git init` 本身仍可能在 makewand 有机会脱离 config 链接前修改其 core 设置，这是已有的 Git 行为，仍在 VERIFICATION_CONTRACT 中列为残余。
- 流结束等待有界：进程或文件系统真正卡住时，仍可能超时告警后先结束等待。请关注“进程收尾 + 回写”时间起点，而非只核对常量相加。
- API 策略和五个适配器是否一致、实际路线记录及限额后的 cwd、WTA 排除胜者且优先独立于双方的语义，是本次验收的主要行为边界。

## 改动文件清单

- `CHANGELOG.md`
- `SECURITY.md`
- `docs/VERIFICATION_CONTRACT.md`
- `docs/p1-r-validation.md`
- `internal/engine/sandbox.go`
- `internal/processjob/job.go`
- `makewand/cli.py`
- `makewand/config.py`
- `makewand/engine_selection.py`
- `makewand/git_helper.py`
- `makewand/health.py`
- `makewand/orchestrator.py`
- `makewand/providers/agy.py`
- `makewand/providers/base.py`
- `makewand/providers/claude.py`
- `makewand/providers/codex.py`
- `makewand/providers/grok.py`
- `makewand/providers/muse.py`
- `makewand/review_contract.py`
- `router/execution.go`
- `router/stream_cancel_drain_final2_test.go`
- `tests/test_fix_api_whole_prompt.py`
- `tests/test_fix_final2_api_policy_relay.py`
- `tests/test_fix_final2_forced_engine.py`
- `tests/test_fix_final2_gitignore_fallback.py`
- `tests/test_fix_final2_race_pairing.py`
- `tests/test_fix_final2_symlink_exclude.py`
- `tests/test_fix_health_engine_usable_r3.py`
- `tests/test_fix_health_quota_noise.py`
- `tests/test_fix_orch_dag.py`
- `tests/test_fix_orch_dag_r2.py`
- `tests/test_fix_orch_dag_r3.py`
- `tests/test_fix_orch_dag_r4.py`
- `tests/test_fix_orch_race_review.py`
- `tests/test_fix_r4_local_whole_prompt.py`
- `tests/test_p1_r_remaining.py`
- `tests/test_windows_runtime_regression.py`
