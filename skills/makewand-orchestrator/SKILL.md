---
name: makewand-orchestrator
description: Unified multi-model joint orchestration system leveraging local Antigravity, Claude Code, Codex CLI, and Muse Code subscriptions. Use this skill automatically when executing complex coding tasks, multi-file refactoring, independent red-team code reviews, or when multi-model cross-verification is requested.
---

# Makewand Orchestrator (多模型联合调度核心)

Makewand 是部署于本机的**零成本多模型 AI 订阅统一调度中枢**。它将本机拥有的四大主流 AI 订阅打造成协同作战体系：
1. **Antigravity (`agy`)**：Google AI Pro / Gemini 3.8 Flash & Pro，拥有 100 万+ Token 超长上下文与原生思维推理，担任**总指挥、架构师、竞速裁判与兜底闭环**。
2. **Claude Code (`claude`)**：Anthropic 订阅，在代码逻辑生成、单测编写、架构重构中表现优异，担任**主力实现者**。
3. **Codex CLI (`codex`)**：OpenAI 订阅，搭载 `gpt-6-astra` 强力推理模型，擅长底层字节码排查、指令死锁逆向与边界用例爆破，担任**独立红队审查官**。
4. **Muse Code (`muse`)**：Meta AI 订阅 / Muse 代码助手，支持 OS 沙箱隔离与 Meta 开源代际模型，担任**辅助实现与异构验证引擎**。

---

## 核心能力与工作机制

### 1. 额度自适应与动态降级 (Quota-Aware Fallback)
Makewand 具备实时的配额感知与解析能力。任何模型触发使用额度超限（如 Claude 达到周限制、Codex 触发短时限流、Muse 未授权）时，系统自动识别并平滑切换：
- **黄金四角流水线**（全员健康）：`agy` 架构规划 -> `claude` 编码实现 -> `codex` 红队盲审 -> `agy` 最终交付。
- **降级模式 A**（Claude 限流）：`codex` 接管代码实现，`agy` 接管独立深度代码审查。
- **降级模式 B**（Codex 限流）：`claude` 实现代码，`agy` 开启 High Reasoning 自审。
- **降级模式 C**（Muse 介入）：启用 `muse` 作为辅助编码引擎，由 `agy` 实施质量验收。
- **自愈兜底模式**（外部订阅均受限）：全流程由 `agy` (Google AI Pro) 独立闭环完成。

### 2. 独立红队代码审查与自动自愈回环 (Auto-Fix Loop)
- **跨模型审查强制隔离**：系统强制执行“编码方不得担任审查方”规则。若由 Codex 编写代码，审查阶段自动派发给 Antigravity，彻底消除模型自身的盲区。
- **Auto-Fix 循环**：红队审查如果检测到 `[P1]`（高危）、`[P2]`（中危）、并发死锁或资源泄露缺陷，Makewand 自动提取审计反馈并回传给编码引擎，重新修复并复审，直至达到 `LGTM / 审核通过`（默认最多自动闭环 2 轮）。

### 3. 双模型并发竞速模式 (Race Mode)
- 执行 `makewand race "<任务描述>"`，系统会自动在 `/tmp` 创建相互隔离的临时工作区，并发派发给两组模型并行生成。
- 收集双方耗时、代码 Diff 大小及质量指标后，由 Antigravity 担任主裁判，生成包含 6 大工程维度的对比矩阵报告并给出最终采纳裁决。

### 4. 非 Git 目录韧性与无头权限穿透
- **非 Git 容灾**：在未初始化的文件夹中执行任务时，自动建立轻量级影子 Git 跟踪树，彻底避免 `fatal: not a git repository` 故障。
- **无头权限穿透**：底层注入 `--dangerously-skip-permissions`、`--dangerously-bypass-approvals-and-sandbox`、`--yolo` 等参数，自动化流转全程无需人工确认。
- **实时流式输出**：支持 `--stream` 参数，带色彩前缀逐行流式输出。

---

## 常用 CLI 命令速查

```bash
# 1. 查看当前三大/四大模型订阅健康状态、额度限流与重置时间
makewand status
makewand quota

# 2. 强制即刻向底层 CLI 发起探活
makewand probe

# 3. 动态发现当前可用的最新模型矩阵 (零版本硬编码)
makewand models

# 4. 执行全自动多模型编码流水线 (包含自愈回环)
makewand run "在当前目录编写一个支持重试与熔断机制的 HTTP Client"

# 5. 开启实时流式管道并指定目标目录
makewand run "重构用户认证模块" --cwd /path/to/project --stream

# 6. 对当前工作区未提交的 git diff 执行深度红队审查
makewand review

# 7. 并发双模型竞速对比与裁判裁决
makewand race "实现一个快速排序函数 quick_sort(arr) 并写测试用例验证"

# 8. 带预算的安全文件搜索 (自动避开冷归档、SQLite、大文件与虚拟环境)
makewand search "def safe_search" --cwd /path/to/project

# 9. 物理隔离沙箱执行 (Bubblewrap 容器级只读根目录与凭据脱敏)
makewand sandbox python3 -m pytest tests/
makewand sandbox --no-net go test ./...

# 10. 直接调用底层订阅单次执行 (透传)
makewand agy "总结当前项目的架构特点"
makewand codex "排查这段代码的死锁漏洞"
makewand claude "编写测试套件"
makewand muse "检查配置项"
```
