# Muse Code 接入指南

Muse Code 是 Meta 生态的终端编码助手，支持 OS 沙箱隔离与 Meta 开源大模型。

## 在 Muse Code 中使用 Makewand
1. **凭据配置**：
   运行 `muse login` 完成 OAuth 授权，或者在 `~/.config/muse/env` 中配置 `export META_API_KEY="..."`。
2. **在 Muse 中调用 Makewand**：
   使用 Bash 运行 `makewand status`、`makewand review` 或 `makewand run` 即可与 Antigravity、Claude、Codex 联合协作。
