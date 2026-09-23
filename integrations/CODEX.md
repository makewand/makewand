# Codex CLI 接入指南

Codex CLI 搭载 OpenAI 订阅（gpt-6-astra 模型），具备出色的底层指令排查、并发死锁分析与安全审计能力。

## 在 Codex CLI 中使用 Makewand
可在 `~/.codex/rules/default.rules` 或项目 `AGENTS.md` 中集成：

```markdown
## 多模型联合调度工具 (Makewand)
当需要发起跨模型联合编码或方案对比时：
- 使用 `makewand status` 查看各模型健康度与限流状态。
- 使用 `makewand run "<任务>"` 自动编排 Antigravity 与 Claude 协同开发。
- 使用 `makewand race "<任务>"` 进行方案并发竞速对比。
```
