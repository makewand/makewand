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
- 同一改动（同一 PR 或同一候选及其修复版）最多审 3 轮，第 4 轮起要用户点头（2026-11-27 站主定，替代 11-12 的按收益续审）。首轮完整审查；复审只核对上一轮“必须改”的问题是否修好、修复有没有引入会造成错误行为的回归，提示词写明这个范围并附上一轮的问题清单，不重新全面审查。判 pass 就停；3 轮后仍有核实属实的“必须改”，或同一问题修过两次仍被指出，停下把没解决的问题、已修内容和证据交给用户决定。用户此前对某个具体改动明确给过的追加授权只对那个改动有效。
- 审查意见要分级。提示词里要求审查方给每条意见标上 `[必须改]`（这次改动会造成错误行为、数据丢失、安全漏洞或发布失败，附文件行号和会出错的具体场景）、`[应该改]`、`[细节]` 或 `[原有问题]`（改动之前就存在），并且只有存在 `[必须改]` 时才判 FAIL。
- 不照单全收。逐条对着代码核实：核实属实的 `[必须改]` 才修并算打回；不属实的写明理由驳回；`[应该改]`、`[细节]` 自行决定，修了也不再送审。修复只做解决该问题所需的最小改动，不借机新增大段校验或状态机；修复新加的代码又被指出问题时，优先缩小或撤回那次修复。
- 审查派发失败时自动改派，不为此停下来问用户（2026-11-27 站主定，长期授权）：供应商当场拒绝（429、额度用完）、超时（16）或结果未知（17）时，在本请求剩余的调用次数内换另一个审查方，不重派同一个；429 和额度用完照常 `makewand mark`，改派的 route 和派发命令带 `--retry-approved`。顺序：原定 `claude-fable-5-2` 时先换 `claude-opus-5-5`（参数不变），否则按 `makewand route review <难度> --self <你> --not <实现方>` 的顺序取下一个。交付说明里写明原定审查方、失败原因和实际用了谁。只有 3 次调用用完仍没有有效裁决，或所有候选都失败时，才向用户说明并等指示。
- 复审轮（第 2、3 轮）本机任何已登录的模型都可以用（Claude、Codex、agy 里的各家模型、Muse、Grok；2026-11-27 站主定）：实现方不是 Claude 家族时优先 `claude-opus-5-5` 高推理档（比 Fable 快），其次与实现方不同家族的模型，都不行时同家族的也可以。复审用 `--readonly` 和预算参数即可，不必带 `--risk high`（带上会按高风险规则排除 Grok 和同家族）。复审轮查候选用 `makewand route review <难度> --rereview`，加了这个参数才会把 Grok 列进候选；首轮审查仍不派 Grok。

## 请求预算与路由

每个用户请求默认最多 3 次实际模型调用，包含失败、裁判、重试和修复；要超过先问用户，续审也不例外。新子任务、独立 shell、换模型、换工作树都不重置本请求预算。

第一次确实需要派发时生成一个随机请求 ID，保存在当前会话；同一用户请求的所有 CLI 始终带同一个 `--request-id <ID>` 和总上限 `--max-model-calls <N>`，并用 `--caller <claude|codex|agy|grok|muse>` 标注发起工具。也可继承 `MAKEWAND_TASK_ID`、`MAKEWAND_MAX_MODEL_CALLS`、`MAKEWAND_CALLER`。不要用项目名或长期会话 ID 合并互不相关的用户请求，不要把 ID 当秘密。

先运行（按风险选档位）：

```bash
# 安全、并发、数据丢失、资金、生产发布：强档
makewand route review hard --risk high --self <你> --not <实现方> --request-id <ID> --caller <你> --max-model-calls <N>
# 其他审查用中档：Codex 中档给出结论、打回的比例和高档相近，快约 1 分钟
makewand route review normal --self <你> --not <实现方> --request-id <ID> --caller <你> --max-model-calls <N>
```

高风险审查先看 `makewand models --json` 和带同一请求约束的 `route --json`。实现方经代理调用时，用 `--review-author <实现方的实际模型 ID>` 替换 `--not <实现方>`。站主要求排除的家族用 `--exclude-review-family <家族>`，包含经 AGY 代理的同家族型号。照抄候选中的精确型号、实际推理档和审查约束。只有用户明确批准同家族回退才加 `--allow-same-family-review`。

改用独立子代理时也先核对该入口当前可用的型号和推理档。高风险选当前强档并使用独立只读上下文。同家族的不同代际仍属同一家族；获准回退时也选择当前强档。交付时记录派发型号、推理档和回退依据，并核对完整候选哈希与 nonce。实际执行型号以可取得的运行证据为准。

类型可为 review/implement/test/research/doc，难度为 mechanical/normal/hard。先选够用的最便宜档，验收失败再升档；高风险直接强档，审查尽量跨模型家族。route 不调用模型，会避开暂停、官方当前读数显示已用完的额度及本请求已记录的操作失败；额度标记和以前的拒绝记录只是提醒（2026-11-27）：对应的工具或模型排在后面并注明原因，照样可以派，直接派发时也只打印一行提醒，能不能用以供应商当场的答复为准；首轮审查不派 Grok，复审轮可以用任何模型（`makewand review` 也不再默认用它）；agy credits 保持禁用。

用户指定最新型号时，先用 `makewand models --json` 核对当前账号的可见模型，再看 `route --json` 中符合档位、额度和家族约束的候选。路由顺序考虑档位、额度和历史验收。显式的 `--model` 会固定型号，复用旧命令前先核对这一项。AGY 型号用 CLI 列出的完整 ID；必要时用 `makewand models --refresh` 更新 AGY 列表。

普通审查已有其他符合要求的候选，或可以稍后派发时，若已安装版本支持，在 `route` 加 `--no-capacity-wait`。名额已满会立即报告容量占用，模型尚未启动。确认同一任务是否已经在运行，或选择其他符合要求的候选。UNKNOWN/超时后重跑同一子任务仍需用户授权。

带请求 ID 时，route 输出保留同一 ID、发起工具和请求**总上限**，不是每条命令各一份上限。默认单任务输出中的 `--max-model-calls 1` 不能另建 ID 来绕过总预算。旧版没有 `--request-id` 时，用每条命令显式传同一个 `MAKEWAND_TASK_ID` 和同一总上限；不能依赖一次 shell 的 export 跨工具调用持久化。

`makewand budget request --request-id <ID> --json` 查看已用计数。只有用户明确同意扩大额度后，才可用 `makewand budget request --request-id <ID> --set-limit <N> --max-model-calls <N>` 调整原上限，每次只加用户同意的次数。加之前先用 `makewand outcome <审查方> <档> review pass --candidate-verdict fail --request-id <ID> --candidate-hash <SHA256>`（另带 `--model`、`--effort` 和实测 `--seconds`/`--calls`）记下上一轮：审查工作记 pass 表示你核实了它找到的缺陷，候选记 fail；没有这条记录不加次数。之后的 route 和派发命令都带新总额 `--max-model-calls <N>`，带旧总额会把上限压回去；保留 ID 和已用计数，不退款、不另建请求。

始终指定 `--cwd`，沿用 route 的 timeout、total-timeout 和仓库信任级别，审查保持 readonly。review/test/research/doc 三档为 900/1800/5400 秒，implement 为 1200/3600/10800 秒。Codex 审查默认最多 1800 秒，显式总时限优先。只给 total-timeout 时单次跟随，都不给时默认 300 秒。按实际模型和推理深度选够用的档位。已有同一任务在运行时，后台等它完成。同项目或账号已占满时，避免重复排队。

## 判定与记账

`review` 的 0/10/11 分别是通过或无改动/发现缺陷/未验证。自动化调用核对 JSON 的 `verdict_status` 和 `reviewed_sha256`。直接工具 CLI 的 0 只表示执行完成，必须检查真实回答中的 nonce 裁决与完整覆盖。裁决缺失、截断或工具拒绝均未验证。代码合并门禁独立于调度成绩。

审查找到了经核实的真实缺陷：**审查工作 pass，候选代码 fail**。只有裁决缺失、覆盖不足、无效输出或结论未被验收时才给审查工具记 fail；不要因代码被打回惩罚准确的审查者。保留历史记录，不猜测和重写旧成绩。

验收后记录实际耗时、模型调用数、发起方、请求 ID 和完整候选差异 SHA256。例如：

```bash
makewand outcome codex 3 review pass --candidate-verdict fail --candidate-hash <SHA256> --model <模型> --effort <深度> --seconds <实际秒数> --calls 1 --caller <你> --request-id <ID> --max-model-calls <N>
```

带请求 ID、这个工具在本请求里只调用过一次、且还没记过成绩时，可以不填 `--seconds`/`--calls`，makewand 从调用账本补上实测值（不含排队）；手填的调用次数不是 1 或对不上时仍要自己填。验收对不上派发调用（没有 `--request-id`/`MAKEWAND_TASK_ID`，也无法唯一自动关联）时照常记账但会警告，请带上派发用的同一个请求 ID。review 记录不带 `--candidate-verdict` 时照常记账但会警告：没有实测耗时和实际调用数就照实不填，不要为消除警告编数字。用 `--call-budget-file` 指定非默认账本时，任务上限照常生效，但整机/项目日计数另算，`makewand observe` 会列出这些账本。

实现/测试任务的 pass/fail 仍是其结果是否通过验收。操作失败另带 `--failure-kind startup|auth|quota|timeout|unknown|invalid_output|verification`；只有 startup/auth/quota 在本请求冷却 5 分钟，unknown/timeout 要用户批准重试（审查派发改派另一个审查方已有长期授权，见上）。候选测试失败或有效审查拒绝不触发提供方冷却。429/额度耗尽还应 `makewand mark <工具> <剩余时长>`；只是某个模型被拒时加 `--model <模型>`：Agy 的 Gemini、Claude、GPT-OSS 额度分开算，Gemini 用完不要标记整个 agy。不要猜测失败原因。

退出码 12/16/17 分别为取消/超时/未知。审查派发遇到 16、17 按上面“审查派发失败时自动改派”处理（换另一个审查方已有长期授权）；其他任务遇到这类结果，告知用户并等待重试授权，UNKNOWN 可能已产生花费，换工具重跑同一子任务也需要授权。授权范围明确后，在 route 使用 `--retry-approved`。如果已安装版本的实际派发命令也支持此参数，则显式带上，让账本保留调用方声明。13 是预算耗尽，14 是应用冲突，15 是沙箱不可用。健康检查用 `status`，`status --probe` 可能产生真实模型调用。

## 权限与交付

用户授权和偏好优先；不要为已授权动作反复确认。`MAKEWAND_UNSAFE_HOST_EXEC`、`--retry-approved` 和提高请求额度只可在用户授权范围内使用，不绕过沙箱或额度保护。不可信仓库用 `--repo-trust untrusted`。

最后一句说明用了谁、档位、实际调用次数、裁决及父方核实情况。

如果 `MAKEWAND_PROVIDER_WORKER=1` 或自身任务来自 Makewand：直接完成任务，禁止再次派发或调用管理命令。只允许 worker 白名单内的静态查询；不要移除环境变量、换路径或绕过祖先进程检查。
