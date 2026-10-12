# Antigravity (AGY) 接入指南

推荐把 Antigravity 作为调用 makewand 的中心工具：它的额度通常最充裕，可以把 Claude 与 Codex 的额度留给 makewand 派发的实际实现和审查。

## 安装 skill

源码安装会把 `skills/makewand-orchestrator/SKILL.md` 复制到 `~/.gemini/config/skills/makewand-orchestrator/`（可用 `MAKEWAND_SKILLS_DIR` 改目录）。手动更新时直接覆盖该文件即可。

skill 规定了何时调用、各命令的成本、如何按退出码判断结果、影子工作树交付需要征得用户同意后再应用补丁，以及禁止递归调用。agy 自己的全局准则（`GEMINI.md`）不要再写"优先激活 makewand"之类会让 agy 自动发起多模型任务的规则。

## 注意

- 在 agy 交互界面中调用时，agy 自身会被识别为同目录的交互式 AI 会话，`makewand run` 通常走影子工作树，只生成补丁。
- makewand 派发 agy 时会带 `MAKEWAND_PROVIDER_WORKER=1`；被派发的 agy 不能再调用 makewand。
