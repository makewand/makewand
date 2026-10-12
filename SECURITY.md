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
- **Private per-run provider home (`PROVIDER_PROFILES`)**: the host provider directory (e.g. `~/.claude`, `~/.codex`, `~/.gemini`, `~/.config/muse`, `~/.grok`) is never bound writable. Each launch gets a private `0700` directory (under `$XDG_RUNTIME_DIR` or the cache directory) bound at the provider's path:
  - Only the allowlisted credential and seed files are copied in. Agent instructions, MCP configurations, hooks, plugins, skills and binary paths (e.g. `CLAUDE.md`, `GEMINI.md`, `settings.json`, `hooks.json`, `skills/`, `commands/`, `agents/`, `bin/`) that exist on the host are bound **read-only**; writes to missing ones land only in the discarded private copy.
  - After the run, changed credential files (for example a refreshed OAuth token) are written back to the host atomically — only regular files, never through symlinks, and only if the host copy did not change during the run. This also happens for read-only reviews so token refresh keeps working. Session history and other provider state are discarded.
  - Other providers' credentials, host SSH keys (`~/.ssh`), GPG keys (`~/.gnupg`), cloud tokens (`~/.aws`, `~/.kube`) and host Unix domain sockets — including sockets anywhere in the workspace such as under `node_modules` — are masked. In the Go launcher, `.git` metadata (including linked-worktree `gitdir`/`commondir` and `modules`) is read-only, so agents cannot plant hooks or rewrite repository configuration.
  **每次运行的私有 Provider 家目录**：宿主上的 provider 目录（如 `~/.claude`、`~/.codex`、`~/.gemini`、`~/.config/muse`、`~/.grok`）不再可写挂载。每次调用都在 `$XDG_RUNTIME_DIR` 或缓存目录下创建 `0700` 私有目录，挂载到 provider 的路径上：
  - 只复制白名单内的凭据与种子文件。宿主上已存在的 Agent 指令、MCP 配置、hooks、插件、技能和可执行路径（如 `CLAUDE.md`、`GEMINI.md`、`settings.json`、`hooks.json`、`skills/`、`commands/`、`agents/`、`bin/`）只读挂载；对不存在条目的写入只落在用完即弃的私有副本中。
  - 运行结束后，发生变化的凭据文件（例如刷新后的 OAuth token）原子地写回宿主：只写普通文件、不跟随符号链接，且宿主副本在运行期间未被修改。只读审查同样会写回，以保证 token 刷新可用。会话历史与其他 provider 状态全部丢弃。
  - 其他 Provider 凭据、宿主 SSH 密钥（`~/.ssh`）、GPG（`~/.gnupg`）、云凭据（`~/.aws`、`~/.kube`）以及宿主 Unix domain socket（包括工作区内任何位置、如 `node_modules` 下的套接字）完全屏蔽。Go 启动器中，`.git` 元数据（含链接工作树的 `gitdir`/`commondir` 与 `modules`）只读，Agent 无法植入 hooks 或改写仓库配置。
- **Launcher integrity, seccomp and limits**: `bwrap` (and `systemd-run`/`prlimit`) are resolved only from root-owned, non-writable system directories, never from `$PATH`; set `MAKEWAND_BWRAP_PATH` to an absolute path for other installs. A seccomp filter blocking `ptrace`, `bpf`, `keyctl`, `process_vm_readv/writev`, `userfaultfd` and the x32 ABI is **required** on x86-64 and arm64 (`MAKEWAND_ALLOW_NO_SECCOMP=1` opts out with a warning). Sandboxed launches run under a memory and task cap (`MAKEWAND_SANDBOX_MEMORY_MAX`, default `8G`; `MAKEWAND_SANDBOX_TASKS_MAX`, default `512`; never `RLIMIT_AS`). Writable workspaces at `/`, `/tmp`, `/home`, system roots, `$HOME` or an ancestor of `$HOME` are refused, and a writable workspace that cannot be scanned completely (more than 1,000,000 entries by default, `MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT`; or an unreadable directory you own) fails closed; another user's directory you cannot enter is skipped because the sandbox cannot reach it either.
  **启动器完整性、seccomp 与资源上限**：`bwrap`（以及 `systemd-run`/`prlimit`）只从 root 拥有、不可写的系统目录解析，绝不经 `$PATH`；装在其他位置时用 `MAKEWAND_BWRAP_PATH` 指定绝对路径。x86-64 与 arm64 上**强制**启用 seccomp 过滤器，屏蔽 `ptrace`、`bpf`、`keyctl`、`process_vm_readv/writev`、`userfaultfd` 与 x32 ABI（`MAKEWAND_ALLOW_NO_SECCOMP=1` 可显式关闭并告警）。沙箱内进程受内存与进程数上限约束（`MAKEWAND_SANDBOX_MEMORY_MAX` 默认 `8G`，`MAKEWAND_SANDBOX_TASKS_MAX` 默认 `512`，不使用 `RLIMIT_AS`）。拒绝以 `/`、`/tmp`、`/home`、系统根目录、`$HOME` 或其上级目录作为可写工作区；可写工作区无法完整扫描时（默认超过 1,000,000 个条目，可用 `MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT` 调整；或含有当前用户自己的不可读目录）fail-closed；其他用户的、无法进入的目录会被跳过，因为沙箱同样无法访问。
- **No unsandboxed launch without acknowledgement**: without bubblewrap, *every* provider launch — read-only reviews included — requires `MAKEWAND_UNSAFE_HOST_EXEC=1` plus the recorded acknowledgement and is audited. Provider CLIs launched by makewand carry `MAKEWAND_PROVIDER_WORKER=1`, and makewand refuses model-dispatching commands under it, so nested dispatch cannot recurse.
  **无确认不在沙箱外启动**：没有 bubblewrap 时，**所有** provider 调用（包括只读审查）都需要 `MAKEWAND_UNSAFE_HOST_EXEC=1` 加已记录的确认，并写入审计日志。makewand 启动的 provider CLI 带有 `MAKEWAND_PROVIDER_WORKER=1`，在此环境下 makewand 拒绝会派发模型的命令，嵌套调用不会形成递归。
- **Network Namespace & Socket Boundaries (`--unshare-net`)**: In trusted mode (the default), subscription CLI providers require outbound network access to communicate with cloud model endpoints, and therefore operate within the host network namespace. Filepath sockets under `/tmp`, `/var`, `/run`, and `$HOME` are masked, and GUI/D-Bus environment variables (`DISPLAY`, `XAUTHORITY`, `WAYLAND_DISPLAY`, `DBUS_*`) are stripped. Under Linux kernel architecture, abstract AF_UNIX sockets (`@/tmp/.X11-unix/X0`) and loopback services (`127.0.0.1`, `::1`) are scoped to the network namespace rather than filesystem mounts. In `--repo-trust=untrusted` mode (or when `MAKEWAND_SANDBOX_UNSHARE_NET=1` is configured), `--unshare-net` is strictly enforced to isolate the network namespace, completely blocking access to all host abstract sockets and loopback ports.
  **网络命名空间与套接字隔离边界（`--unshare-net`）**：在默认可信模式下，订阅制 CLI 需要访问外部模型 API，因此在宿主网络命名空间中运行。沙箱屏蔽了 `/tmp`、`/var`、`/run`、`$HOME` 下的文件路径套接字，并剔除了 GUI 与 D-Bus 环境变量（`DISPLAY`、`XAUTHORITY`、`WAYLAND_DISPLAY`、`DBUS_*`）。但由于 Linux 内核将抽象 AF_UNIX 套接字（如 `@/tmp/.X11-unix/X0`）和回环服务（`127.0.0.1`）绑定在网络命名空间而非文件系统挂载点，在允许联网时它们保持可达。在不可信仓库模式（`--repo-trust=untrusted` 或设置 `MAKEWAND_SANDBOX_UNSHARE_NET=1`）下，沙箱强制启用 `--unshare-net`，建立全新网络命名空间，彻底阻断对宿主抽象套接字与本地回环服务的连接。
- **Fail-Closed Sandbox Enforcement**: For writable generation and code review tasks across both Python and Go router entrypoints (`makewand [prompt]`, `chat`, `--print`, `new`), bubblewrap sandbox isolation is mandatory. If `bwrap` is missing or probe fails, makewand fails closed and refuses execution unless `MAKEWAND_UNSAFE_HOST_EXEC=1` is explicitly set AND authenticated via a recorded one-time host acknowledgment. When authorized to fall back to the host, the process environment is strictly sanitized of foreign credentials/tokens and all host executions are audited to `unsafe_exec_audit.jsonl`.
  **失败即停沙箱契约**：对于代码生成与审查任务，无论 Python 调度引擎还是 Go 路由入口（`makewand [prompt]`、`chat`、`--print`、`new`），Bubblewrap 沙箱物理隔离均为强制约束。若系统无可用 `bwrap` 或探测失败，除非显式设置 `MAKEWAND_UNSAFE_HOST_EXEC=1` 且已完成记录的一次性本机授权确认，否则一律失败即停（fail-closed）拒绝执行。即使经授权降级至宿主执行，也对环境变量执行严格凭据脱敏剥离，并将命令写入 `unsafe_exec_audit.jsonl` 审计日志。

### Guidance / 使用建议

- **Your own code**: the trusted default is appropriate. The provider CLI runs within the bubblewrap provider sandbox on your repository. If your host runs sensitive loopback services without authentication (e.g. unauthenticated databases, local Ollama, or X11 servers), ensure they require authentication, run with `--repo-trust=untrusted`, or execute makewand in an isolated container/VM.
  **你自己的代码**：默认的可信模式即可，Provider CLI 在面向该仓库的 bubblewrap 沙箱中运行。若宿主机在本地回环上运行无认证的敏感服务（如无密码数据库、本地 Ollama 或 X11 服务），请为其配置鉴权，或使用 `--repo-trust=untrusted`，或在独立容器/虚拟机中运行 makewand。
- **Untrusted third-party code** (a cloned repo you do not control): pass `--repo-trust=untrusted`. In that mode writable operations are denied and network access inside the sandbox is disabled (`--unshare-net`). The Go engine routes generation only to direct API providers or a remote makewand server and fails closed if none is configured; the Python engine allows a local CLI only for read-only work inside the sandbox and refuses it without bubblewrap. For genuinely hostile code, still run makewand inside a VM, container, or a separate low-privilege user. makewand does **not** claim to safely execute arbitrary hostile third-party code.
  **不可信的第三方代码**（你无法掌控的克隆仓库）：请加 `--repo-trust=untrusted`。该模式下禁止可写操作，沙箱内断网（`--unshare-net`）。Go 引擎只把生成路由到直接 API provider 或远端 makewand 服务，未配置则 fail-closed；Python 引擎只允许本地 CLI 在沙箱内做只读工作，没有 bubblewrap 时直接拒绝。处理真正敌对的代码，仍请在 VM、容器或独立低权限用户下运行。makewand **不**声称能安全执行任意敌对的第三方代码。

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
| `v3.2.x` | Yes |
| `< v3.2.0` | No |

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
