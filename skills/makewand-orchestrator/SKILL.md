---
name: makewand-orchestrator
description: Dispatch bounded implementation work or independent reviews through the local makewand orchestrator. Use when delegation saves time, independent judgment is needed, or changes affect security, concurrency, data loss, money or production releases. Skip routine edits with sufficient local verification, pure Q&A and read-only analysis.
---

# Makewand：有界多模型调度

makewand 将工作交给已登录的编码工具。由调用方负责验收；审查结论必须绑定完整候选与任务 nonce。

## 何时派发

- 安全、并发、数据丢失、资金和生产发布：交付前必须独立审查。
- 普通改动：按行为复杂度、影响范围和现有验证是否充分决定；不按行数触发。可本地充分验证的文案、格式、机械修正和小范围修复自行完成。
- 大任务：只有边界明确、委派能减少总时间时才拆 worker，自己验收。拿不准且本地证据不足时可请求独立意见。
- 纯问答、只读代码分析、状态查询不用模型。先集中解决已知问题、通过定向检查，再对冻结候选审查；复用同一源码范围及哈希绑定的有效结果。避免重复全套本机测试与完整 CI。

## 请求预算与路由

每个用户请求默认最多 3 次实际模型调用，包含失败、裁判、重试和修复。每个用户请求默认最多 3 次模型调用，要超过先问用户。新子任务、独立 shell、换模型、换工作树都不重置本请求预算。

第一次确实需要派发时生成一个随机请求 ID，保存在当前会话；同一用户请求的所有 CLI 始终带同一个 `--request-id <ID>` 和总上限 `--max-model-calls <N>`，并用 `--caller <claude|codex|agy|grok|muse>` 标注发起工具。也可继承 `MAKEWAND_TASK_ID`、`MAKEWAND_MAX_MODEL_CALLS`、`MAKEWAND_CALLER`。不要用项目名或长期会话 ID 合并互不相关的用户请求，不要把 ID 当秘密。

先运行：

```bash
makewand route review hard --risk high --self <你> --not <实现方> --request-id <ID> --caller <你> --max-model-calls <N>
```

类型可为 review/implement/test/research/doc，难度为 mechanical/normal/hard。先选够用的最便宜档，验收失败再升档；高风险直接强档，审查尽量跨模型家族。route 不调用模型，会避开暂停、额度过线、429 标记及本请求已记录的操作失败；agy credits 保持禁用。

带请求 ID 时，route 输出保留同一 ID、发起工具和请求**总上限**，不是每条命令各一份上限。默认单任务输出中的 `--max-model-calls 1` 不能另建 ID 来绕过总预算。旧版没有 `--request-id` 时，用每条命令显式传同一个 `MAKEWAND_TASK_ID` 和同一总上限；不能依赖一次 shell 的 export 跨工具调用持久化。

`makewand budget request --request-id <ID> --json` 查看已用计数。用户明确扩大额度后，才可用 `makewand budget request --request-id <ID> --set-limit <N> --max-model-calls <N>` 调整原上限，并在后续调用沿用新总额；保留 ID 和已用计数，不退款、不另建请求。

始终指定 `--cwd`；照抄 route 的 timeout/total-timeout，审查保持 readonly。review/test/research/doc 三档为 900/1800/5400 秒，implement 为 1200/3600/10800 秒。只给 total-timeout 时单次跟随；都不给时默认 300 秒（ensemble 900）。后台等待完成，不重复派同一任务。同一仓库只跑一个写任务；同一账号并发 1 时，额外审查只会排队。

## 判定与记账

`review` 的 0/10/11 分别是通过/发现缺陷/未验证；直接工具 CLI 的 0 只表示执行完成，必须检查真实回答中的 nonce 裁决与完整覆盖，不能采纳回显提示中的示例。裁决缺失、截断或工具拒绝均未验证。代码合并门禁独立于调度成绩。

审查找到了经核实的真实缺陷：**审查工作 pass，候选代码 fail**。只有裁决缺失、覆盖不足、无效输出或结论未被验收时才给审查工具记 fail；不要因代码被打回惩罚准确的审查者。保留历史记录，不猜测和重写旧成绩。

验收后记录实际耗时、模型调用数、发起方、请求 ID 和完整候选差异 SHA256。例如：

```bash
makewand outcome codex 3 review pass --candidate-verdict fail --candidate-hash <SHA256> --model <模型> --effort <深度> --seconds <实际秒数> --calls 1 --caller <你> --request-id <ID> --max-model-calls <N>
```

实现/测试任务的 pass/fail 仍是其结果是否通过验收。操作失败另带 `--failure-kind startup|auth|quota|timeout|unknown|invalid_output|verification`；只有 startup/auth/quota 在本请求冷却 5 分钟，unknown/timeout 要用户批准重试。候选测试失败或有效审查拒绝不触发提供方冷却。429/额度耗尽还应 `makewand mark <工具> <剩余时长>`，不要猜测失败原因。

退出码 12/16/17 分别为取消/超时/未知：告知用户，未经授权不自动重试，UNKNOWN 可能已产生花费。13 是预算耗尽，14 是应用冲突，15 是沙箱不可用。模型恢复、冷却时间经过或预选推荐项都不构成重试授权；用户已批准后才使用 route 的 `--retry-approved`。`status --probe` 可能产生真实模型调用，不能当廉价健康检查。

## 权限与交付

用户授权和偏好优先；不要为已授权动作反复确认。`MAKEWAND_UNSAFE_HOST_EXEC`、`apply --force`、`--allow-collision`、`--boost`、`--retry-approved` 和提高请求额度只可在用户授权范围内使用，不绕过沙箱或额度保护。不可信仓库用 `--repo-trust untrusted`。

若报告隔离交付，只生成了补丁，应明确尚未应用；应用属于用户既有授权时继续完成，否则展示具体补丁后再询问。race 选择不明确时展示候选再请用户决定。最后一句说明用了谁、档位、实际调用次数、裁决及父方核实情况。

如果 `MAKEWAND_PROVIDER_WORKER=1` 或自身任务来自 Makewand：直接完成任务，禁止再次派发或调用管理命令。只允许 worker 白名单内的静态查询；不要移除环境变量、换路径或绕过祖先进程检查。
