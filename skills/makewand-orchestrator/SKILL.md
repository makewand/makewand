---
name: makewand-orchestrator
description: Dispatch coding work to the local makewand multi-model orchestrator (Claude Code, Codex, Antigravity, Grok, Muse subscriptions) for independent cross-model review, delegated implementation with review, two-candidate races, or multi-model opinion panels. Use when the user asks for makewand, a second-model review, cross-verification or a race, or when a change is high-risk (security, concurrency, data loss, money) and deserves review by a different model. Do not use for routine edits you can finish and verify yourself.
---

# Makewand：多模型调度

makewand 是本机的命令行调度器：把任务派给已登录的编码 CLI，自动挑选实现方和**另一家模型**做只读审查，测试与审查都通过后才交付。分工由 makewand 按额度、成功率和任务类型动态决定，调用方不需要也不应该指定"谁当总指挥"。

## 何时使用

| 场景 | 命令 | 成本 |
|---|---|---|
| 你刚写完/改完代码，想让另一家模型审一遍 | `makewand review` | 低：1 次只读调用 |
| 把一个实现任务整体委托出去（实现 → 测试 → 跨模型审查 → 自动修复） | `makewand run "<任务>"` | 中：2–6 次调用 |
| 想要两个独立实现择优 | `makewand race "<任务>"` | 高：两组实现 + 裁判 |
| 方案、架构、论文等需要多方意见（只读，不改文件） | `makewand ensemble "<问题>"` | 高：每个活跃模型各 1 次 |

不要用于：你自己就能完成并验证的常规修改；纯问答；只需要读代码的分析（自己读更快更省）。

## 调用规范

1. **先看额度再派重活**：`run`/`race`/`ensemble` 之前可运行 `makewand status`。多个订阅显示限流或削峰时，告诉用户并建议改用 `review` 或稍后再试。
2. **始终指定目录**：`--cwd <项目目录>`。
3. **后台运行并等待结束**：这些命令常需数分钟到十余分钟。不要因为等待而中断，也不要重复派发同一任务。`run` 默认总时限 300 秒，大任务用 `--timeout <秒>` 放宽。
4. **控制花费**：可用 `--max-model-calls <N>` 给本次任务设调用上限。
5. **同一仓库同时只跑一个** makewand 写任务。
6. **以下开关只能由用户决定，你不要自行添加**：`MAKEWAND_UNSAFE_HOST_EXEC`（无沙箱宿主执行）、`apply --force`、`--allow-collision`、`--boost`（无视削峰保护）。处理不可信的第三方仓库时加 `--repo-trust untrusted`。

## 读结果：以退出码为准

| 退出码 | 含义 | 你该做什么 |
|---|---|---|
| 0 | 测试与审查通过 | 按下文"交付"处理 |
| 10 | 审查或测试未通过 | 读 `MAKEWAND_VERDICT_<随机串>:` 行里的 `defects`，向用户说明；`run` 已在修复轮数内自动修过 |
| 11 | 未能验证（如审查裁决缺失、沙箱未就绪） | 不交付；把输出中的原因和建议转告用户 |
| 12 / 16 | 已取消 / 超时 | 告诉用户；是否重试由用户决定 |
| 13 | 调用预算耗尽 | 告诉用户 |
| 14 | 应用补丁时发生冲突 | 用户的文件在此期间被改过；不要强制覆盖，请用户处理 |
| 15 | 沙箱不可用 | 转告输出中的建议；不要自行开启宿主执行 |
| 17 | 结果未知 | **不要自动重试**（可能已产生花费），告诉用户 |
| 1 / 2 | 内部错误 / 用法错误 | 检查命令参数 |

不要只凭文字摘要判断成败。`review --json` 输出机器可读结果。

## 交付：改动不一定已经落到用户的工作区

- 以下任一情况，makewand 会在影子工作树里开发和审查，**不修改当前工作区**，只生成补丁：工作区有未提交改动；检测到其他交互式 AI 会话也在这个目录工作（**从 AI 工具的交互界面调用时，调用方自己通常就会被算进去**，所以这是常态）；目标位于受保护的生产目录。
- 这时输出末尾会出现"隔离交付报告"，给出 `apply_delivery.sh` 和 `git apply` 命令。你必须告诉用户"改动尚未应用到工作区"，经用户同意后再运行应用命令。不要声称文件已经改好。
- 只有没有出现"隔离交付报告"时，通过审查的改动才已直接写入工作区。
- `race` 默认保留 A/B 两个候选：`makewand candidates` 列出，`makewand apply <race_id> --candidate A|B` 应用（可先加 `--dry-run`），`makewand discard <race_id>` 丢弃。选哪个由用户决定。

## 常见失败

- **某个引擎报认证失败或限流**：makewand 会自动换其他引擎。只有用户需要重新登录时（例如提示运行 `agy` 或 `claude login`），才把提示转告用户。
- **所有引擎都不可用**：运行 `makewand status` 查看原因，转告用户。
- **沙箱相关报错**：按输出建议处理，或转告用户；不要绕过。

## 禁止递归

如果环境变量 `MAKEWAND_PROVIDER_WORKER=1`，或者你的任务本身来自 makewand 流水线，说明你是被 makewand 派发的子进程：直接完成分配给你的任务，**不要再调用 makewand**。makewand 也会拒绝这种调用。

## 命令速查

```bash
makewand status                                    # 各订阅健康、额度与削峰状态
makewand review --cwd <dir>                        # 只读审查当前未提交的改动
makewand review --cwd <dir> --json                 # 同上，机器可读输出
makewand run "<任务>" --cwd <dir>                  # 实现 → 测试 → 跨模型审查 → 自动修复
makewand run "<任务>" --cwd <dir> --effort high --timeout 900 --max-model-calls 6
makewand race "<任务>" --cwd <dir>                 # 两组并行实现 + 裁判
makewand candidates --cwd <dir>                    # 列出 race 候选
makewand apply <race_id> --candidate A --dry-run   # 预览应用候选
makewand ensemble "<问题>" --cwd <dir> --providers claude,codex,agy --output-dir ./reviews
makewand sandbox --cwd <dir> -- python3 -m pytest  # 在隔离沙箱中运行命令
makewand clean                                     # 清理残留的临时工作区
```
