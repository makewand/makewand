# Antigravity (AGY) 接入指南

源码安装将 [makewand-orchestrator skill](../skills/makewand-orchestrator/SKILL.md) 复制到 `~/.gemini/config/skills/makewand-orchestrator/`，可用 `MAKEWAND_SKILLS_DIR` 指定目录。日常工作使用 Antigravity 和项目 CI。

独立审查从 `makewand route review normal --self agy --not <实现方>` 开始，高风险使用 `hard --risk high`。沿用同一请求 ID、`--caller agy` 和请求总上限，核对代理模型的实际家族。单个模型额度受限时，按已知重置时间用 `mark --model` 标记。AGY credits 保持关闭。被 makewand 派发的 AGY 直接完成子任务。
