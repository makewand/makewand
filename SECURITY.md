# Security Policy

## Security Model / 安全模型

makewand runs AI coding work in two distinct stages with **different trust boundaries**. Read this before pointing makewand at a repository you do not fully trust.
makewand 的 AI 编码流程分两个阶段，二者的**信任边界不同**。在让 makewand 处理你不完全信任的仓库前，请先读这一节。

### Verification / preview is sandboxed / 验证与预览是沙箱隔离的

When makewand runs a candidate's tests, builds, dependency installs, or a preview server, those commands execute inside a **bubblewrap sandbox** (read-only root, writable workspace bind, cleared environment, ephemeral HOME, and `--unshare-net` for test/build steps). On a host where strong isolation is unavailable, this stage **fails closed** — nothing runs unless you explicitly set `MAKEWAND_UNSAFE_HOST_EXEC=1` **and** complete a one-time interactive acknowledgment. The environment variable alone never enables host execution: the acknowledgment is recorded in your config (bound to this machine and to the current risk-statement version, so copied configs and materially changed risks require re-acknowledging), non-interactive runs without it refuse host execution, and every host execution under the opt-in is appended to an audit log (`unsafe_exec_audit.jsonl` in the config directory).
当 makewand 运行候选代码的测试、构建、依赖安装或预览服务时，这些命令在 **bubblewrap 沙箱**内执行（只读根、可写 workspace、清空环境、临时 HOME，测试/构建步骤 `--unshare-net`）。在无法强隔离的主机上，该阶段**失败即停**——除非你显式设置 `MAKEWAND_UNSAFE_HOST_EXEC=1` **并**完成一次性交互确认。仅设置环境变量永远不会启用宿主执行：确认会记录在配置中（绑定本机与当前风险声明版本，配置拷到别机或风险声明实质变更都需重新确认），未确认的非交互运行会拒绝宿主执行，且该选项下的每一次宿主执行都会追加写入审计日志（配置目录下的 `unsafe_exec_audit.jsonl`）。

### Generation is sandboxed with provider isolation / 生成阶段通过 Provider 沙箱物理隔离

In makewand v3.1+, the **generation** stage is strongly sandboxed. When a subscription CLI provider (`claude`, `codex`, `gemini`/`agy`, `grok`, `muse`, or `aider`) produces or reviews code, makewand physically isolates that CLI process via `wrap_bwrap(is_provider=True)` applying tailored `PROVIDER_PROFILES`:
在 makewand v3.1+ 中，**生成**阶段同样受到强物理沙箱隔离保护。当由订阅制 CLI provider（`claude`、`codex`、`gemini`/`agy`、`grok`、`muse` 或 `aider`）生成或审查代码时，makewand 通过 `wrap_bwrap(is_provider=True)` 并在定制的 `PROVIDER_PROFILES` 规则下物理隔离运行该 CLI：

- **System & Root Isolation**: System binaries and utilities (`/usr`, `/bin`, `/lib`, `/etc`, etc.) are mounted strictly read-only (`--ro-bind`). Installed system software cannot be tampered with.
  **系统只读与环境隔离**：系统二进制与底层工具（`/usr`、`/bin`、`/lib`、`/etc` 等）以严格只读方式挂载，无法篡改系统软件与全局配置。
- **Ephemeral & Masked HOME**: User home directories and sensitive root paths (`/home`, `/root`, `/mnt`, `/media`, `/srv`) are hidden and masked. An isolated, empty `tmpfs` is mounted as `$HOME`, preventing access to the host's private user files or unrelated repositories.
  **临时 HOME 与敏感根屏蔽**：敏感宿主目录（`/home`、`/root`、`/mnt`、`/media`、`/srv`）被物理隐藏。沙箱内以全新空白 `tmpfs` 挂载为 `$HOME`，阻断对宿主机个人文件和跨项目代码的读取。
- **Provider State Isolation & Credential Protection (`PROVIDER_PROFILES`)**: Only the active provider's specific configuration directory (e.g. `~/.claude`, `~/.codex`, `~/.gemini`, `~/.config/muse`, `~/.grok`) is mounted into the container. Within this directory:
  - Agent instructions, MCP configurations, hooks, plugins, skills, and binary paths (e.g. `CLAUDE.md`, `GEMINI.md`, `settings.json`, `hooks.json`, `skills/`, `commands/`, `agents/`, `bin/`) are mounted **read-only** (or shadowed with clean templates) to prevent prompt injection attacks from poisoning host configs or establishing persistence.
  - Ephemeral runtime state (e.g. `shell-snapshots`, `session-env`, `daemon`, `ide`) is placed on private, isolated tmpfs mounts.
  - Read-only state (subscription tokens, history, logs) is mounted read-only for authentication while strictly preventing modification or overwriting.
  - Other providers' credentials, host SSH keys (`~/.ssh`), GPG keys (`~/.gnupg`), cloud tokens (`~/.aws`, `~/.kube`), and host Unix domain sockets (Docker, ssh-agent, systemd) are completely masked and unexposed.
  **Provider 状态隔离与凭据防护**：沙箱内仅选择性挂载当前所调度的 Provider 自身状态目录（如 `~/.claude`、`~/.codex`、`~/.gemini`、`~/.config/muse`、`~/.grok`）。在此目录中：
  - Agent 指令、MCP 配置、hooks、插件、技能和可执行路径（如 `CLAUDE.md`、`GEMINI.md`、`settings.json`、`hooks.json`、`skills/`、`commands/`、`agents/`、`bin/`）严格**只读挂载**（或以安全模板遮盖），杜绝 Prompt 注入篡改宿主配置或驻留后门。
  - 临时会话环境（如 `shell-snapshots`、`session-env`、`daemon`、`ide`）隔离在专属临时 tmpfs 中。
  - 认证凭据与日志只读提供订阅鉴权，禁止写入或覆盖篡改。
  - 其他 Provider 凭据、宿主 SSH 密钥（`~/.ssh`）、GPG（`~/.gnupg`）、云凭据（`~/.aws`、`~/.kube`）以及 Unix domain socket（Docker、ssh-agent）完全屏蔽不可见。
- **Network Namespace & Socket Boundaries (`--unshare-net`)**: In trusted mode (the default), subscription CLI providers require outbound network access to communicate with cloud model endpoints, and therefore operate within the host network namespace. Filepath sockets under `/tmp`, `/var`, `/run`, and `$HOME` are masked, and GUI/D-Bus environment variables (`DISPLAY`, `XAUTHORITY`, `WAYLAND_DISPLAY`, `DBUS_*`) are stripped. Under Linux kernel architecture, abstract AF_UNIX sockets (`@/tmp/.X11-unix/X0`) and loopback services (`127.0.0.1`, `::1`) are scoped to the network namespace rather than filesystem mounts. In `--repo-trust=untrusted` mode (or when `MAKEWAND_SANDBOX_UNSHARE_NET=1` is configured), `--unshare-net` is strictly enforced to isolate the network namespace, completely blocking access to all host abstract sockets and loopback ports.
  **网络命名空间与套接字隔离边界（`--unshare-net`）**：在默认可信模式下，订阅制 CLI 需要访问外部模型 API，因此在宿主网络命名空间中运行。沙箱屏蔽了 `/tmp`、`/var`、`/run`、`$HOME` 下的文件路径套接字，并剔除了 GUI 与 D-Bus 环境变量（`DISPLAY`、`XAUTHORITY`、`WAYLAND_DISPLAY`、`DBUS_*`）。但由于 Linux 内核将抽象 AF_UNIX 套接字（如 `@/tmp/.X11-unix/X0`）和回环服务（`127.0.0.1`）绑定在网络命名空间而非文件系统挂载点，在允许联网时它们保持可达。在不可信仓库模式（`--repo-trust=untrusted` 或设置 `MAKEWAND_SANDBOX_UNSHARE_NET=1`）下，沙箱强制启用 `--unshare-net`，建立全新网络命名空间，彻底阻断对宿主抽象套接字与本地回环服务的连接。
- **Fail-Closed Sandbox Enforcement**: For writable generation and code review tasks across both Python and Go router entrypoints (`makewand [prompt]`, `chat`, `--print`, `new`), bubblewrap sandbox isolation is mandatory. If `bwrap` is missing or probe fails, makewand fails closed and refuses execution unless `MAKEWAND_UNSAFE_HOST_EXEC=1` is explicitly set AND authenticated via a recorded one-time host acknowledgment. When authorized to fall back to the host, the process environment is strictly sanitized of foreign credentials/tokens and all host executions are audited to `unsafe_exec_audit.jsonl`.
  **失败即停沙箱契约**：对于代码生成与审查任务，无论 Python 调度引擎还是 Go 路由入口（`makewand [prompt]`、`chat`、`--print`、`new`），Bubblewrap 沙箱物理隔离均为强制约束。若系统无可用 `bwrap` 或探测失败，除非显式设置 `MAKEWAND_UNSAFE_HOST_EXEC=1` 且已完成记录的一次性本机授权确认，否则一律失败即停（fail-closed）拒绝执行。即使经授权降级至宿主执行，也对环境变量执行严格凭据脱敏剥离，并将命令写入 `unsafe_exec_audit.jsonl` 审计日志。

### Guidance / 使用建议

- **Your own code**: the trusted default is appropriate. The provider CLI runs within the bubblewrap provider sandbox on your repository. If your host runs sensitive loopback services without authentication (e.g. unauthenticated databases, local Ollama, or X11 servers), ensure they require authentication, run with `--repo-trust=untrusted`, or execute makewand in an isolated container/VM.
  **你自己的代码**：默认的可信模式即可，Provider CLI 在面向该仓库的 bubblewrap 沙箱中运行。若宿主机在本地回环上运行无认证的敏感服务（如无密码数据库、本地 Ollama 或 X11 服务），请为其配置鉴权，或使用 `--repo-trust=untrusted`，或在独立容器/虚拟机中运行 makewand。
- **Untrusted third-party code** (a cloned repo you do not control): pass `--repo-trust=untrusted`. In that mode makewand routes generation only to direct API providers or a remote makewand server (never a local repo-aware CLI) and fails closed if none is configured, network access is strictly disabled (`--unshare-net`), and writable operations are denied. For genuinely hostile code, still run makewand inside a VM, container, or a separate low-privilege user. makewand does **not** claim to safely execute arbitrary hostile third-party code.
  **不可信的第三方代码**（你无法掌控的克隆仓库）：请加 `--repo-trust=untrusted`。该模式下网络连接被切断（`--unshare-net`），生成只路由到直接 API provider（绝不用会读取仓库的本地 CLI），未配置则 fail-closed，禁止本地可写生成。处理真正敌对的代码，仍请在 VM、容器或独立低权限用户下运行。makewand **不**声称能安全执行任意敌对的第三方代码。

### Acceptance and billing boundaries / 验收与费用边界

Local test output is diagnostic evidence, not an independent acceptance certificate.
Go local checks alone do not grant Strength 2 or authorize automatic application;
a person must approve the sealed candidate unless configured independent trusted
acceptance passes for that same artifact and grants Strength 2. See
[the verification contract](docs/VERIFICATION_CONTRACT.md)
and [server credential invalidation](docs/SERVER_AUTHORIZATION.md) for the exact guarantees.
本地测试结果不能证明候选进程没有伪造输出；Go 仅通过本地验证时仍需人工批准封存产物。配置的独立可信验收通过并达到 Strength 2 后，autopilot 才可自动应用。

Direct cloud APIs and API fallback are disabled by default, including in untrusted
repository mode. Explicitly set `MAKEWAND_API_POLICY=allow_paid` or `api_policy` to
`allow_paid` to enable them. A remote server and third-party CLIs retain their own
billing policies. 环境中存在 API key 本身不再授权 Makewand 发起云 API 调用。

Python candidate application uses fixed POSIX directory handles or native Windows
handles for replacement and recovery. Native Windows execution requires explicit
unsafe-host consent and cannot grant Strength 2; Windows Job Objects manage
resources and process trees without providing Linux sandbox isolation. See
[the native Windows contract](docs/VERIFICATION_CONTRACT.md#python-pipeline-and-race-candidates).
Python 候选应用支持 POSIX 和原生 Windows 固定句柄。Windows 执行生成代码仍需显式宿主执行授权，不能达到 Strength 2；Job Object 不提供 Linux 沙箱的文件、凭据或网络隔离。

## Supported Versions

| Version | Supported |
| --- | --- |
| `v3.1.x` | Yes |
| `< v3.1.0` | No |

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
