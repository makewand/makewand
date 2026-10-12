# Codex CLI 接入指南

Codex 在这套体系里主要是被 makewand 派发的实现者和审查者，不需要安装完整 skill。只在你自己用 Codex 工作、偶尔想要跨模型审查时，在 `~/.codex/AGENTS.md`（或项目的 `AGENTS.md`）加入下面这段。注意不要写进 `~/.codex/rules/`，那里是 Codex 的命令执行策略文件，不是说明文档。

```markdown
## Makewand（多模型调度，仅在用户明确要求时使用）
- 只有当用户明确提出"用 makewand""跨模型审查""让别的模型看看"时才调用，不要自行发起。
- 审查当前未提交的改动：`makewand review`（只读，耗时数分钟，请在后台运行并等待结束）。退出码：0=通过，10=发现缺陷（见 `MAKEWAND_VERDICT_*` 行），11=未能验证。
- 查看各订阅的额度与健康状态：`makewand status`。
- 如果环境变量 `MAKEWAND_PROVIDER_WORKER=1`，说明你正被 makewand 派发执行子任务：不要调用 makewand，直接完成分配的任务。
```
