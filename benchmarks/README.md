# 可复验的对照评测

目前没有足够的完整对照数据证明 Makewand 优于单模型。此目录提供固定任务、独立验收和自动化执行器；CI 使用桩验证执行器，不进行模型调用，也不把桩结果作为产品质量数据。

## 已纳入版本控制的内容

固定矩阵含 12 个 Python 任务，风险标签在 `metadata.json` 中预先登记，每个风险组各 4 个。风险表示任务的输入与恢复约束，不由某次模型结果推断。

| 任务 | 类别 | 风险 | 独立验收重点 |
| --- | --- | --- | --- |
| intervals | 函数修复 | low | 闭区间、排序、非法端点、输入不变 |
| slugs | 函数修复 | low | Unicode、天然数字后缀、全局冲突 |
| lower-bound | 函数修复 | low | 重复值、未命中、空列表、非法排序 |
| run-length | 函数修复 | low | 连续分组、Unicode、空值、迭代器 |
| moving-averages | 输入边界 | medium | 完整窗口、窗口不足、bool/非整数宽度 |
| query-values | 输入边界 | medium | 重复键、空值、百分号与 UTF-8 错误 |
| deep-config | 多文件重构 | medium | 两个公开接口、递归合并、可变对象脱离输入 |
| log-levels | 多文件重构 | medium | 解析与汇总接口、非法行、大小写与空白 |
| safe-path | 输入边界 | high | 路径越界、盘符、NUL、词法规范化 |
| invoice | 多文件重构 | high | Decimal 舍入、两层接口、非法价格与数量 |
| retry-sequence | 异常恢复 | high | 瞬态/永久失败、退避、次数上限、不实际睡眠 |
| record-recovery | 异常恢复 | high | 坏记录后继续、重复 ID、错误索引、输入不变 |

每个案例有初始源码、完整提示词、外部 `accept.py` 和风险元数据。所有 arm 从相同源码开始，由工作区外的可信父进程比较候选子进程返回值；提前退出、伪造成功文字、输入篡改和超时都不能通过。本阶段扩充矩阵时，保留 intervals/slugs 已有源码、提示词与验收脚本；旧结果文件不被补写或改记。

- `fixtures/*/offline_solution/` 与 `fixtures/offline_stub.py` 仅用于测试验收器，提供正确、错误、提前退出、输入篡改和超时桩。这些内容不会复制到候选 seed。
- `prompts/` 保留历史探索任务；它们不参与本自动验收矩阵，不作为已验证结果。

### 验收协议 v4：deep-config 的可变键

原任务只要求 `base` / `override` 为字典，没有限定键必须是字符串。v4 保留原来的 JSON 用例，并为两个公开接口分别增加三个独立子进程用例：同一身份键的递归合并、同一身份键的整体替换，以及相等但非同一对象的可变可哈希键合并。键能正常深复制，修改其 `notes` 不影响哈希；验收同时检查结果只有一个匹配条目、键状态保留，以及键和嵌套可变值在两个方向上与输入隔离。两次调用返回的独立身份键不要求是同一个对象。

可信父进程只向 worker 传递固定用例 ID；worker 在导入候选前构造探针类型并绑定观察器，校验类型/方法及观察序列化定义未被篡改，父进程比较固定 schema 4 的原始观察值。候选返回“通过”布尔值或伪造观察字典不会成为验收结论。旧 JSON 观察协议仍为 schema 1；其他 fixture 的验收规则不变。reference 先按原始键匹配并完成合并，再一次深复制最终结构，避免先复制键后使用原始身份键更新结果。

此扩展验证确定的可复制键和有限容器结构，不证明任意自定义 Python 对象、循环合并或恶意代码隔离。候选与观察器仍在同一 worker 中执行，进程/resource 限制不构成防任意 `inspect`、底层系统调用或自造协议输出的安全沙箱。新增离线回归记录为 v4，不是新增模型质量数据；此前归档的 pilot-v1、main-v2、postfix-v3 保留原登记结果，不能用新验收事后更改通过数或与新协议结果池化。

## 执行

需要 Python 3.9+。默认仅打印计划，不调用模型：

```bash
./benchmarks/run.sh --repeats 3 --seed 20260927
python3 -I benchmarks/test_runner.py
```

用 `--cases intervals slugs` 可重现原两任务范围；用 `--risk high` 可在执行前选择已登记的风险组。选择、任务标签、超时与随机顺序都会写入计划。每个风险组对所有 arm 使用同一个生成与验收预算，不按观测结果调整预算。

复制 `arms.example.json` 到一个自定义文件，为单模型基线加入已在本机验证的命令 argv 列表。每个参数支持 `{prompt}`、`{prompt_file}`、`{workspace}`、`{timeout}`、`{case}`、`{risk}`、`{adapter}`（仓库内 `model_arm.py` 的绝对路径）；不经 shell 展开。风险感知 arm 可显式使用 `{risk}` 传递已登记标签。供应商 CLI 参数因版本而异，执行前记录各 CLI/模型版本，并确保命令退出前已经把源码写入工作区。示例中的三个 Makewand 档位不是单模型基线。

完整离线矩阵不需要模型 CLI、账号或额度。以下命令仅运行本仓库桩，输出目录应选新的路径：

```bash
python3 - <<'PY'
import json
import sys
from pathlib import Path
stub = str(Path('benchmarks/fixtures/offline_stub.py').resolve())
arms = {f'offline-{mode}': [sys.executable, '-I', stub, '--case', '{case}', '--mode', mode]
        for mode in ('correct', 'wrong')}
Path('/tmp/makewand-fixed12-offline-arms.json').write_text(json.dumps(arms))
PY
./benchmarks/run.sh --arms /tmp/makewand-fixed12-offline-arms.json \
  --repeats 1 --timeout 30 --evidence-kind offline \
  --output /tmp/makewand-fixed12-offline-results --execute
```

真实模型 arm 的显式执行可能消耗订阅额度或按配置产生 API 费用：

```bash
./benchmarks/run.sh --arms /path/to/arms.json --timeout 600 --repeats 3 \
  --seed 20260927 --output /tmp/makewand-benchmark-results \
  --record-cli-versions claude codex --evidence-kind live --execute
```

所有 arm 使用相同提示词、初始源码、墙钟预算和验收脚本，按固定种子随机排序。超时会终止进程树；Linux 还按每次运行的环境标记定位脱离会话或已失去父进程的后代。主命令退出后仍有后代持有输出管道，会记为 `incomplete_output` 并取消后代，不能算作成功。每个捕获流最多保留 16 MiB，额外字节数单独记录。输出目录必须不存在，避免覆盖旧证据。候选与验收均可执行代码，应只在用于评测的隔离环境中运行；本执行器不是恶意代码安全边界。子进程主动删除标记或平台不支持进程枚举时，取消范围存在限制。

`--record-cli-versions` 在显式执行时仅运行列出的 CLI 的 `--version`；默认和 dry run 均不启动版本命令。`--evidence-kind offline` 用于桩验证，`live` 用于真实模型评测，默认 `unclassified`；该标签只是实验来源说明，不自动证明 arm 的身份或能力。

### Daemon 与调用预算

正式比较应固定是否启用 daemon。使用 `MAKEWAND_NO_DAEMON=1` 可让所有 Makewand arm 从独立 CLI 开始计时；若评测 daemon，另存一个对照矩阵并记录 daemon 的启停与 IPC 协议版本。当前 daemon 为每次请求启动独立 worker，耗时包含 worker 启动，没有共享任务解释器或 `<15ms` 性能保证。失去最终 IPC 结果应计为基础设施失败，不能把同一任务自动重跑后得到的结果替换原始记录。

`--budget-file /tmp/unique-call-budget.json --max-model-calls 12` 可为支持 Makewand 执行契约的 Go/Python arm 设置共享派发预算；失败尝试也计数。使用新的账本路径，保留账本和各次 `result.json`。未接入契约的供应商 CLI 命令不受该账本约束；即使账本记录 12 次派发，供应商 CLI 的内部多轮请求与 Token 数仍需单独测量。首次小样本可只运行两个 fixture、两个单模型基线和一个联合流水线，每个 arm 重复一次；其质量结果只作探索，不足以证明普遍优势。

执行前用工具的版本和认证状态命令检查安装与登录方式，避免用 `makewand probe` 或 `status --probe` 预检，因为部分探针会消耗真实模型额度。执行器设置 `subscription_only` 以禁止 Makewand 自己调用付费云 API；官方 CLI 自身仍可能根据 API key 或账号配置计费。订阅对照应在评测子进程内移除相关按量 API key，并确认使用订阅认证，不修改用户的认证文件。

## 结果与解释

`plan.json` 保留命令模板、随机顺序、两阶段超时、预算、风险元数据、fixture 内容摘要、Git revision/dirty 状态、可信验收支持文件及执行契约的哈希，以及 fixture/arm/repeat 数量和分位数算法。显式请求时还记录实际安装的 CLI 路径和版本输出；CLI 版本不代表模型部署版本。dirty 工作区应另存代码 patch 或完整源码。每次运行保存生成/验收的退出码、时延、stdout/stderr、产物摘要和文件模式。生成命令成功但独立验收失败，验收过程中修改产物，或登记后的可信输入发生变化，均不计为通过。`summary.json` 汇总验收数、基础设施失败和 P50/P95 生成时延；少量重复运行的分位数只描述这些样本。

### 可选阶段事件与每次交付成本

`--execution-events` 开启阶段分析，例如：

```bash
./benchmarks/run.sh --arms /path/to/arms.json --cases intervals slugs \
  --execution-events --repeats 1 --output /tmp/new-stage-results
```

上述命令仍为 dry run。显式执行时，每个 trial 向支持事件的 arm 提供独立 `MAKEWAND_EXECUTION_EVENTS_FILE`、`MAKEWAND_TASK_ID` 和 `MAKEWAND_BENCHMARK_RUN_ID`；不继承另一 trial 或外层进程的事件文件。`events.jsonl` 使用统一 schema 1，只记录阶段、状态、时长、尝试与产物标识，不记录提示词、工作目录、原始错误或凭据。验收子进程不继承事件与模型账本上下文。

执行器验证契约字段、trial 绑定以及同一 `event_id` 的 start/end 对后，分别汇总 prepare/copy/generation/verification/review/merge/apply/workflow/provider 等阶段。`stage_end_status_counts` 统计各阶段已结束 span 的状态，`unfinished_spans_by_stage` 记录缺少 end 的数量，缺失测量保持 `null`；provider 的 PASSED 不代表最终交付通过。未结束 span 的时长保持 `null`。阶段可以嵌套或并行，因此各阶段时长不能相加当作总耗时；`total_seconds` 独立测量生成、验收及中间检查的墙钟时间。

汇总中的 `dispatches_per_accepted_delivery` 是全部 trial 的派发数除以成功验收数，包含失败尝试与失败运行的派发；`seconds_per_accepted_delivery` 同样包含失败运行耗时。`accepted_mean_total_seconds` 仅平均成功 trial 的总耗时。没有成功交付时三者均为 `null`。调用数优先采用显式账本；没有账本时，仅从经过验证、具有唯一 `attempt_id` 的 provider span 统计。如果某次调用、事件文件或阶段没有测量，相关汇总保持 `null`；无效事件保留测量错误，独立质量验收仍单独记录。事件属于执行器自报测量，不代表模型效果，也不替代调用预算门禁。

供应商调用次数、Token 与金额默认为 `null`，不能将未测量的成本写成零。指定调用账本后，各次结果按运行 ID 记录尝试、全局已预留数量和未完成尝试；失败也计数。账本缺失或无效会报告测量错误，调用数保持未知，该次运行不能计为通过。墙钟预算对所有 arm 一致；支持共享账本的 Go/Python arm 还可固定派发上限，未接入契约的 arm 与 Token 预算需另外实施并记录。订阅额度、本机资源和人工修订时间也应计入正式实验。

修复后重测或超时后的独立复审属于新的阶段，应保留原始失败结果和原预算消耗。后续复审通过并应用候选，也不能将原先超时的运行改记为在时限内通过。各阶段的代码版本与结果应分别记录，再给出受样本规模限制的结论。

### 离线工作区准备 profiling

无需模型、凭据或网络，可单独测量竞速使用的真实隔离复制、Git 基线和清单准备：

```bash
python3 benchmarks/profile_workspace.py --output /tmp/new-workspace-profile \
  --file-counts 8 128 --file-bytes 1024 65536 --repeats 3
```

每个样本使用独立 Python worker、私有 HOME 和合成文件，真实执行一次 baseline
复制和两次 candidate 复制，并核对三份副本的字节及模式。默认同时测试普通目录与已提交的
Git 目录。`first` 表示新建输入且没有显式预热；`repeated` 表示先对同一输入完成一次不计时的
三份复制。两者均不清空操作系统缓存，新写入的文件也可能已在缓存中，不能称为冷缓存基准。
worker 按顺序运行，每个最多 180 秒；该保护只用于离线测量，不改变 SDK 的任务期限或调用预算。

`profile.json` 保存每个样本的退出状态、准备总时长、实际文件数/字节数、源码摘要及 P50/P95。
每个样本另保留私有 `stdout.log`/`stderr.log` 和摘要，用于复查失败或超时；它们来自合成输入，
原始诊断可能含该样本的临时路径，结构化指标和阶段事件不会复制这些文本。
`events.jsonl` 复用 schema 1 的固定阶段/引擎标签，记录三次 `copy`、嵌套的
`prepare/git-baseline` 与三次 manifest/check，不记录路径、内容或原始错误。
copy 时长包含安全枚举、沙箱能力检查、符号链接处理及 Git 初始化；嵌套 Git span 不能与
copy span 相加作为总耗时。测量失败和缺失的指标保留未知，不参与成功样本分位数。

Linux/macOS 的 `worker_peak_rss_bytes` 是新 worker 整个生命周期的实际 `ru_maxrss`，包含
导入、输入准备和预热；`largest_terminated_child_peak_rss_bytes` 单独记录已结束子进程的
最大 RSS，包含 Git/沙箱检查，不能与 worker RSS 相加，也不代表整个进程树的同时峰值。
Linux 将 KiB 转为字节，macOS 原值为字节，其他平台未测量时为 `null`；Token 与费用为
`null`。小样本分位数只描述所测输入和本机，不能证明模型质量或完整 workflow 的性能提升。
输出目录必须不存在，以保留旧证据；此入口不读取用户工作区，也不运行生成、测试或审查模型。

这 12 个确定性 Python 任务只能验证所列代码行为，无法支持 UI、安全或大型工程优势主张。任务覆盖扩充与正确桩通过不等于真实模型效果提高。正式结论还需要真实、重复的完整单模型对照、模型版本及费用/额度数据。

2026-09-30 本阶段的 72 次离线验收检查来自 12 个任务、正确/错误/提前退出三个桩、每项重复两次；三个 arm 分别通过 24/0/0 次。它们验证验收器，不测真实 workflow 或模型成效；派发数、阶段时长等未测字段仍为 `null`。此前真实模型派发预算已用尽 12/12，本阶段新增真实调用为 0，原始结果保持原样。

### 任务文件保护与协议版本

当前 runner 的 `evaluation_protocol.schema=2` 登记 `protected_file_guard=task-file-v1`。
使用 `--seed-overlays` 时，每个 trial 自动向 adapter 提供公共测试文件的
`MAKEWAND_TASK_PROTECTED_PATHS` JSON 清单。adapter 额外的 `--protect PATH` 与该清单取并集。
流水线、竞速候选和单模型臂均在隔离副本生成，并在应用前检查原始文件与候选的内容及权限。
单模型臂仍只调用一次生成模型，不增加测试或审查模型，也不伪造相应通过记录；其原子应用
可跳过本来没有执行的质量门槛，但不能跳过冻结的文件保护或候选完整性检查。

配置合并的键隔离验收属于 v4，旧的 JSON 验收仍兼容其他 fixture。新代码、协议和离线回归
需要单独保存源码快照与记录。历史真实试验按其原验收版本保留，不重写通过率、不与新协议
合并统计；本轮 v4 修复验证没有新增真实模型调用。CI 和 `make test-benchmark` 均运行新键探针
回归。文件保护的 SDK/CLI 范围见 [执行约定](../docs/EXECUTION_CONTRACT.md#protected-task-files)。
