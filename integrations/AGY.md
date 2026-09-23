# Antigravity (AGY) 接入指南

Antigravity 搭载 Google AI Pro 订阅（Gemini 3.8 Flash & Pro），具备 100 万+ Token 超长上下文与强大的架构设计和思维推理能力。

## 在 Antigravity 中使用 Makewand
Antigravity 已全局配置 `makewand-orchestrator` Skill 与全局行为准则：
- 当遇到复杂多文件重构、高难度算法、并发死锁排查或跨模型独立盲审时，Antigravity 可直接调用 `makewand` CLI。
- 在提示词中提及多模型协作或直接执行 `makewand run` 即可激活联动流水线。
