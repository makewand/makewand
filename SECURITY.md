# Security Policy

## Security Model / 安全模型

makewand runs AI coding work in two distinct stages with **different trust boundaries**. Read this before pointing makewand at a repository you do not fully trust.
makewand 的 AI 编码流程分两个阶段，二者的**信任边界不同**。在让 makewand 处理你不完全信任的仓库前，请先读这一节。

### Verification / preview is sandboxed / 验证与预览是沙箱隔离的

When makewand runs a candidate's tests, builds, dependency installs, or a preview server, those commands execute inside a **bubblewrap sandbox** (read-only root, writable workspace bind, cleared environment, ephemeral HOME, and `--unshare-net` for test/build steps). On a host where strong isolation is unavailable, this stage **fails closed** — nothing runs unless you explicitly set `MAKEWAND_UNSAFE_HOST_EXEC=1`.
当 makewand 运行候选代码的测试、构建、依赖安装或预览服务时，这些命令在 **bubblewrap 沙箱**内执行（只读根、可写 workspace、清空环境、临时 HOME，测试/构建步骤 `--unshare-net`）。在无法强隔离的主机上，该阶段**失败即停**——除非你显式设置 `MAKEWAND_UNSAFE_HOST_EXEC=1`，否则什么都不会运行。

### Generation runs provider CLIs on the host / 生成阶段在宿主机运行 provider CLI

The **generation** stage is different. When a subscription CLI provider (`claude`, `codex`, `gemini`/`agy`, or a custom command provider) produces the candidate, makewand runs that CLI **directly on the host**, with the repository directory as its working directory and your normal environment and credentials. This stage is **not** sandboxed.
**生成**阶段则不同。当由订阅制 CLI provider（`claude`、`codex`、`gemini`/`agy` 或自定义命令 provider）生成候选时，makewand 会以仓库目录为工作目录、带着你的常规环境与凭据，**直接在宿主机上**运行该 CLI。这个阶段**没有**沙箱。

Concretely, this means:
具体而言：

- A local CLI provider has roughly the **same host privileges as running that agent CLI yourself** in that directory — it can read/write files, use the network, and invoke whatever tools (shell, MCP servers, browsers) the CLI is configured to use.
  本地 CLI provider 拥有与你**自己在该目录直接运行那个 agent CLI 近似的宿主权限**——可读写文件、使用网络，并调用该 CLI 配置的任何工具（shell、MCP server、浏览器）。
- Repository content is an **untrusted input to that agent**. Instruction files (`CLAUDE.md`, `GEMINI.md`, `AGENTS.md`, `.makewand/rules.md`), project-level agent config (`.mcp.json`, hooks, plugins), and even prompt injection embedded in ordinary source/README/tests can steer the host-privileged agent into acting as a *confused deputy* with your tools and credentials.
  仓库内容是**该 agent 的不可信输入**。指令文件（`CLAUDE.md`、`GEMINI.md`、`AGENTS.md`、`.makewand/rules.md`）、项目级 agent 配置（`.mcp.json`、hooks、plugins），乃至藏在普通源码/README/测试里的 prompt injection，都可能把有宿主权限的 agent 变成 *confused deputy*，借用你的工具和凭据行事。
- The "verification is sandboxed" guarantee above does **not** extend to this stage. Do not read it as "the whole AI flow is isolated."
  上面"验证已沙箱化"的保证**不覆盖**这个阶段。不要把它理解成"整个 AI 流程都被隔离了"。

### Guidance / 使用建议

- **Your own code**: the trusted default is appropriate. The risk is comparable to running the underlying CLI on your own project.
  **你自己的代码**：默认的可信模式即可，风险与你在自己项目上跑该 CLI 相当。
- **Untrusted third-party code** (a cloned repo you do not control): treat generation as running those provider CLIs directly on hostile input. Until a dedicated untrusted-repo mode exists, run makewand inside a VM, container, or a separate low-privilege user for such repositories. makewand does **not** currently claim to safely process hostile third-party repositories.
  **不可信的第三方代码**（你无法掌控的克隆仓库）：请把生成阶段视为在敌对输入上直接运行这些 provider CLI。在专门的不可信仓库模式落地前，处理这类仓库请在 VM、容器或独立的低权限用户下运行 makewand。makewand 目前**不**声称能安全处理敌对的第三方仓库。

## Supported Versions

| Version | Supported |
| --- | --- |
| `v0.1.x` | Yes |
| `< v0.1.0` | No |

## Reporting a Vulnerability

Please do not open public issues for security vulnerabilities.

Use GitHub Private Vulnerability Reporting for this repository:

- Go to `Security` tab in this repo
- Click `Report a vulnerability`

Include:

- Affected version/tag
- Reproduction steps or proof of concept
- Impact assessment
- Suggested fix (if available)

## Response Targets

- Initial triage response: within 3 business days
- Status update cadence: at least every 7 days until resolution
- Coordinated disclosure: after fix is released and users can upgrade
