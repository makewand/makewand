# Claude Code 接入指南

Claude Code 基于 Anthropic 订阅运行，在代码实现、单测编写与业务逻辑梳理中表现优异。

## 在 Claude Code 中使用 Makewand
可在 `~/.claude/CLAUDE.md` 或项目级 `CLAUDE.md` 中添加以下指引：

```markdown
## 多模型联合调度 (Makewand)
当需要对刚刚编写的代码进行独立红队审查，或需要借助 OpenAI Codex / Google Antigravity 进行交叉验证时，可直接通过 Bash 运行：
- `makewand review`：由 Codex 盲审当前 git diff
- `makewand status`：查看各订阅健康度与限流重置时间
- `makewand race "<任务>"`：与 Codex 开启并发实现竞速
```
