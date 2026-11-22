# Codex CLI 接入指南

日常工作使用 Codex 和项目 CI。在 `~/.codex/AGENTS.md` 或项目的 `AGENTS.md` 中引用 [makewand-orchestrator skill](../skills/makewand-orchestrator/SKILL.md)。

独立审查从 `makewand route review normal --self codex --not <实现方>` 开始，高风险使用 `hard --risk high`。沿用同一请求 ID、`--caller codex` 和请求总上限，核对实际模型家族并按只读命令派发。按 skill 核对完整候选与 nonce 后记账。被 makewand 派发的 Codex 直接完成子任务。
