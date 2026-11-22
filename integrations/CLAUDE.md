# Claude Code 接入指南

日常工作使用 Claude Code 和项目 CI。在 `~/.claude/CLAUDE.md` 中引用 [makewand-orchestrator skill](../skills/makewand-orchestrator/SKILL.md)。

独立审查从 `makewand route review normal --self claude --not <实现方>` 开始，高风险使用 `hard --risk high`。沿用同一请求 ID、`--caller claude` 和请求总上限。实现方是 Claude 系列时，排除经其他工具提供的 Claude 模型。被 makewand 派发的 Claude 直接完成子任务。
