# Security Policy

## Security Model / 安全模型

makewand runs AI coding work in two distinct stages with **different trust boundaries**. Read this before pointing makewand at a repository you do not fully trust.
makewand 的 AI 编码流程分两个阶段，二者的**信任边界不同**。在让 makewand 处理你不完全信任的仓库前，请先读这一节。

### Release status / 发布状态

This document describes the source on `master`. The protections added after v3.2.0 (#75, #80–#85) are in source on `master` and ship in v3.3.0; v3.2.x builds (release archives, Homebrew and Scoop) do not have them. Among them: `bwrap`, `systemd-run` and `prlimit` resolved only from root-owned system directories (v3.2.0 resolves `bwrap` through `$PATH`); the host provider directory never bound writable (v3.2.0 binds it writable for writable sessions); required seccomp, and a memory cap (plus a task cap in the Python launcher) where a systemd user scope is available; fail-closed workspace scans; the recorded acknowledgement also for read-only reviews without bubblewrap; outbound-only networks for dependency installs; verdicts that count only with the task's nonce; the provider-worker command whitelist; and a Homebrew wrapper that no longer freezes `PATH`. Until v3.3.0 is released, install from source to get them: `curl -fsSL https://makewand.org/install.sh | bash`. Every change is listed under [Unreleased] in [CHANGELOG.md](CHANGELOG.md).
本文描述 `master` 上的源码。v3.2.0 之后加入的保护（#75、#80–#85）已在 `master` 源码中，随 v3.3.0 发布；v3.2.x 构建（Release 压缩包、Homebrew 与 Scoop）没有这些保护。其中包括：`bwrap`、`systemd-run`、`prlimit` 只从 root 拥有的系统目录解析（v3.2.0 经 `$PATH` 解析 `bwrap`）；宿主 provider 目录不再可写挂载（v3.2.0 在可写会话中可写挂载它）；强制 seccomp，以及有 systemd 用户 scope 时的内存上限（Python 启动器另有进程数上限）；工作区扫描不完整时 fail-closed；没有 bubblewrap 时只读审查也需已记录的确认；依赖安装只出不进的网络；只认带本任务 nonce 的裁决；provider worker 命令白名单；以及不再写死 `PATH` 的 Homebrew 包装脚本。v3.3.0 发布前，需要这些保护请从源码安装：`curl -fsSL https://makewand.org/install.sh | bash`。全部变更见 [CHANGELOG.md](CHANGELOG.md) 的 [Unreleased]。

### Verification / preview is sandboxed / 验证与预览是沙箱隔离的

When makewand runs a candidate's tests, builds, dependency installs, or a preview server, those commands execute inside a **bubblewrap sandbox** (read-only root, writable workspace bind, cleared environment, ephemeral HOME, and a fresh network namespace for every test, build and dependency-install step; see [Dependency installs](#dependency-installs--依赖安装)). On a host where strong isolation is unavailable, this stage **fails closed** — nothing runs unless you explicitly set `MAKEWAND_UNSAFE_HOST_EXEC=1` **and** complete a one-time interactive acknowledgment; with both, these commands run on the host, and so does the probe that picks the Python test runner (it only imports pytest, `python -B -c "import pytest"`, loading no pytest plugin or `conftest.py`). The environment variable alone never enables host execution: the acknowledgment is recorded in your config (bound to this machine and to the current risk-statement version, so copied configs and materially changed risks require re-acknowledging), non-interactive runs without it refuse host execution, and every host execution under the opt-in is appended to an audit log (`unsafe_exec_audit.jsonl` in the config directory).
当 makewand 运行候选代码的测试、构建、依赖安装或预览服务时，这些命令在 **bubblewrap 沙箱**内执行（只读根、可写 workspace、清空环境、临时 HOME，测试、构建与依赖安装的每个步骤都使用独立的网络命名空间，见[依赖安装](#dependency-installs--依赖安装)）。在无法强隔离的主机上，该阶段**失败即停**——除非你显式设置 `MAKEWAND_UNSAFE_HOST_EXEC=1` **并**完成一次性交互确认；两者齐备时这些命令在宿主上运行，选择 Python 测试运行器的探测也一样（它只导入 pytest，即 `python -B -c "import pytest"`，不加载 pytest 插件或 `conftest.py`）。仅设置环境变量永远不会启用宿主执行：确认会记录在配置中（绑定本机与当前风险声明版本，配置拷到别机或风险声明实质变更都需重新确认），未确认的非交互运行会拒绝宿主执行，且该选项下的每一次宿主执行都会追加写入审计日志（配置目录下的 `unsafe_exec_audit.jsonl`）。

### Dependency installs / 依赖安装

These rules apply to candidate verification by the Go engine (`internal/engine`).
以下规则适用于 Go 引擎（`internal/engine`）的候选验证。

- **Outbound-only network**: no verification step shares the host network namespace. A dependency install, and the `cargo fetch` that precedes Rust checks of a manifest that declares dependencies, gets user-mode networking through `slirp4netns` when trusted copies of `slirp4netns`, `nsenter` (util-linux) and `ip` (iproute2) are found in root-owned system directories or named by `MAKEWAND_SLIRP4NETNS_PATH`, `MAKEWAND_NSENTER_PATH` and `MAKEWAND_IP_PATH`. Before the held command runs, unreachable routes cover private, loopback, link-local (cloud metadata), CGNAT, multicast, reserved and documentation ranges and every host address (read when the step starts); inside slirp4netns' own `10.0.2.0/24` only its built-in DNS forwarder, which forwards to the host's resolver, stays reachable. `198.18.0.0/15` stays reachable for fake-IP proxies, IPv6 is never enabled, and slirp4netns runs with `--disable-host-loopback`, its own sandbox and seccomp. If the namespace cannot be identified, slirp4netns cannot attach or the routes cannot be installed, the sandbox is killed before its command runs and the step runs again loopback-only, with guidance naming the failed stage; without the tools every install runs loopback-only (npm retries off). Registries, mirrors and proxies on private addresses or host loopback are unreachable by design.
  **只出不进的网络**：任何验证步骤都不共享宿主网络命名空间。依赖安装（以及声明了依赖的 Rust 项目在检查前执行的 `cargo fetch`）在 root 拥有的系统目录中找到可信的 `slirp4netns`、`nsenter`（util-linux）和 `ip`（iproute2），或由 `MAKEWAND_SLIRP4NETNS_PATH`、`MAKEWAND_NSENTER_PATH`、`MAKEWAND_IP_PATH` 指定时，通过 `slirp4netns` 获得用户态网络。被挂起的命令运行前，不可达路由已覆盖私网、回环、链路本地（云元数据）、CGNAT、组播、保留与文档网段以及本机的每个地址（在步骤开始时读取）；slirp4netns 自身的 `10.0.2.0/24` 内只保留其内置的 DNS 转发地址（转发到宿主的解析器）。`198.18.0.0/15` 为 fake-IP 代理保留可达，从不启用 IPv6，slirp4netns 以 `--disable-host-loopback` 并在自身沙箱与 seccomp 下运行。若无法确认命名空间、slirp4netns 无法接入或路由无法安装，沙箱在命令运行前即被终止，该步骤改为仅回环重跑，并说明失败的环节；缺少这些工具时所有安装都只有回环网络（关闭 npm 重试）。私网地址或宿主回环上的注册表、镜像与代理按设计不可达。
- **Best-effort filters**: a trusted `tc` (`MAKEWAND_TC_PATH`) drops IPv4 multicast on the tap device, and a trusted `nft` (`MAKEWAND_NFT_PATH`) drops the blocked destinations and all IPv6 whatever interface a socket names. On a host with neither, or whose kernel cannot load the needed modules, a step that binds a UDP socket to the tap device can still send multicast with TTL 1 onto the host's LAN segment.
  **尽力而为的过滤**：可信的 `tc`（`MAKEWAND_TC_PATH`）在 tap 设备上丢弃 IPv4 组播，可信的 `nft`（`MAKEWAND_NFT_PATH`）无论套接字指定哪个接口都丢弃被屏蔽的目标与全部 IPv6。主机上两者都没有、或内核无法加载所需模块时，把 UDP 套接字绑定到 tap 设备的步骤仍能向宿主所在局域网段发送 TTL 为 1 的组播。
- **Outage verdicts**: a failed install is an environment problem (`ErrDependencyNetworkUnavailable`: no quality penalty or auto-fix, and the TUI skips the tests) only when makewand confirms it. The package manager's diagnostics only raise the suspicion, because candidate code (lifecycle scripts, build backends, `build.rs`) can print the same lines and project configuration can point the install at a host that does not answer. With network, makewand's own executable, started as a probe from an empty directory in a fresh sandbox with the same network (on the host under the unsafe opt-in), asks the default registry: `registry.npmjs.org`; `pypi.org` and `files.pythonhosted.org`; `index.crates.io` and `static.crates.io`; for go, the sandbox's `GOPROXY` (the host module cache, then the host's proxies without entries that carry credentials; `proxy.golang.org,direct` by default) walked with the go command's `,` and `|` rules, and its checksum database reached through `<proxy>/sumdb/<name>/supported` the way go reaches it: directly when a `direct` entry comes, and at an `off` entry or the end of the list only if every proxy before answered 404 or 410, so after a failure that `|` passed over the database is unavailable, as it is for go, and the probe does not ask it. Only an endpoint without a response (for the checksum database: without a usable answer) confirms the outage; a registry that answers, or a check that cannot run after two tries, leaves the failure the candidate's with a note (Go SDK `ExecResult.Note`; the TUI does not show it yet). Without network the verdict needs a fact printing cannot change: a declared registry dependency that is not installed (npm, pip, cargo) or, for `go mod tidy` and `go mod download`, a module that `go.mod` (parsed by the host toolchain's `go mod edit -json` on a copy), `go.sum`, the imports and both module caches show to be needed and missing; only a pip install of named requirements alone, where no project code runs, is judged from pip's own report. Go output that quotes project strings, and go's `errors parsing go.mod` report, never count; the project strings come from every go.mod, go.work and Go file of the project, also below a `.makewand` directory, skipping only `.git` and makewand's sandbox HOME `.makewand/sandbox-home`. Generated files cannot be written there directly, but the candidate's tests and auto-fix rounds run with `HOME` there and do write it (tool caches, and whatever their code writes); see What remains.
  **故障判定**：安装失败只有经 makewand 确认才算环境问题（`ErrDependencyNetworkUnavailable`：不扣质量分、不自动修复，TUI 跳过测试）。包管理器的诊断只会引起怀疑，因为候选代码（生命周期脚本、构建后端、`build.rs`）能打印相同的行，项目配置也能让安装指向不响应的主机。联网时，makewand 自身的可执行文件作为探针，在使用同类网络的全新沙箱中（不安全宿主模式下在宿主上）从空目录启动，询问默认注册表：`registry.npmjs.org`；`pypi.org` 与 `files.pythonhosted.org`；`index.crates.io` 与 `static.crates.io`；go 则按 go 命令的 `,`/`|` 规则遍历沙箱的 `GOPROXY`（先是宿主模块缓存，再是去掉含凭据条目的宿主代理，默认 `proxy.golang.org,direct`），并像 go 一样经 `<proxy>/sumdb/<name>/supported` 访问校验和数据库：遇到 `direct` 条目时直连；遇到 `off` 条目或列表结束时，只有此前每个代理都回答 404 或 410 才直连，因此 `|` 越过一次失败之后，数据库与 go 一样不可用，探针也不会访问它。只有端点得不到响应（校验和数据库为得不到可用应答）才确认故障；注册表有响应，或检查尝试两次仍无法运行，失败仍归候选，并附说明（Go SDK `ExecResult.Note`；TUI 暂不显示）。离线时必须有打印无法改变的事实：声明了未安装的注册表依赖（npm、pip、cargo），或对 `go mod tidy`、`go mod download` 而言，`go.mod`（由宿主工具链对副本运行 `go mod edit -json` 解析）、`go.sum`、imports 与两处模块缓存表明确实需要而缺失的模块；只有仅安装具名依赖、不运行项目代码的 pip 安装按 pip 自己的报告判定。引用项目字符串的 go 输出以及 go 的 `errors parsing go.mod` 报告一律不算；项目字符串取自项目中所有 go.mod、go.work 与 Go 文件，包括 `.makewand` 目录下的，只跳过 `.git` 与 makewand 的沙箱 HOME `.makewand/sandbox-home`。生成的文件不能直接写入后者，但候选的测试与自动修复轮次以它为 `HOME` 运行，确实会写入其中（工具缓存，以及其代码写入的任何内容），见“仍然存在的情况”。
- **What remains**: offline, a candidate that declares a registry dependency that is not installed gets the environment verdict even when its own code fails first; leftovers in a persistent sandbox HOME are not considered, and Go files that code run by an earlier step (the tests or an auto-fix round, which run with `HOME` there) leaves in it are not read for project strings; needs the files do not show (transitive npm dependencies, requirements of go dependencies), go workspace mode (a `go.work` above the project) and go projects beyond the scan limits (20,000 Go files, 4 MiB per file) leave the failure the candidate's. A project registry setting (`.npmrc`, `--index-url`, cargo `replace-with`) that points at an unreachable host, such as a private registry, is a candidate failure when the default registry answers. `GOPROXY=<proxy>|direct` with every proxy down is still confirmed as an outage (the check cannot name the version-control hosts a direct fetch uses).
  **仍然存在的情况**：离线时，候选声明了未安装的注册表依赖，即使其自身代码先失败也判为环境问题；不考虑持久化沙箱 HOME 中的遗留内容，此前步骤运行的代码（测试或自动修复轮次，它们以那里为 `HOME` 运行）写在其中的 Go 文件也不读取其项目字符串；文件无法表明的需求（npm 传递依赖、go 依赖的依赖）、go 工作区模式（项目上层有 `go.work`）以及超出扫描上限（20,000 个 Go 文件、单文件 4 MiB）的 go 项目，失败仍归候选。项目的注册表配置（`.npmrc`、`--index-url`、cargo `replace-with`）指向不可达的主机（如私有注册表）时，只要默认注册表有响应就算候选失败。`GOPROXY=<proxy>|direct` 且所有代理都不可达时仍会确认为故障（检查无法得知直接拉取会访问哪些版本库主机）。

### Generation is sandboxed with provider isolation / 生成阶段通过 Provider 沙箱物理隔离

The **generation** stage is strongly sandboxed too (since v3.1; the protections below are those of `master` and v3.3.0, and v3.2.x builds lack several of them, see [Release status](#release-status--发布状态)). When a subscription CLI provider (`claude`, `codex`, `gemini`/`agy`, `grok`, `muse`, or `aider`) produces or reviews code, makewand physically isolates that CLI process via `wrap_bwrap(is_provider=True)` applying tailored `PROVIDER_PROFILES`:
**生成**阶段同样受到强物理沙箱隔离保护（自 v3.1 起；下列保护以 `master` 与 v3.3.0 为准，v3.2.x 构建缺少其中多项，见[发布状态](#release-status--发布状态)）。当由订阅制 CLI provider（`claude`、`codex`、`gemini`/`agy`、`grok`、`muse` 或 `aider`）生成或审查代码时，makewand 通过 `wrap_bwrap(is_provider=True)` 并在定制的 `PROVIDER_PROFILES` 规则下物理隔离运行该 CLI：

- **System & Root Isolation**: System binaries and utilities (`/usr`, `/bin`, `/lib`, `/etc`, etc.) are mounted strictly read-only (`--ro-bind`). Installed system software cannot be tampered with.
  **系统只读与环境隔离**：系统二进制与底层工具（`/usr`、`/bin`、`/lib`、`/etc` 等）以严格只读方式挂载，无法篡改系统软件与全局配置。
- **Ephemeral & Masked HOME**: User home directories and sensitive root paths (`/home`, `/root`, `/mnt`, `/media`, `/srv`) are hidden and masked. An isolated, empty `tmpfs` is mounted as `$HOME`, preventing access to the host's private user files or unrelated repositories.
  **临时 HOME 与敏感根屏蔽**：敏感宿主目录（`/home`、`/root`、`/mnt`、`/media`、`/srv`）被物理隐藏。沙箱内以全新空白 `tmpfs` 挂载为 `$HOME`，阻断对宿主机个人文件和跨项目代码的读取。
- **Private per-run provider home (`PROVIDER_PROFILES`)**: the host provider directory (e.g. `~/.claude`, `~/.codex`, `~/.gemini`, `~/.config/muse`, `~/.grok`) is never bound writable. Each launch gets a private `0700` directory (under `$XDG_RUNTIME_DIR` or the cache directory) bound at the provider's path:
  - Only the allowlisted credential and seed files are copied in. Agent instructions, MCP configurations, hooks, plugins, skills and binary paths (e.g. `CLAUDE.md`, `GEMINI.md`, `settings.json`, `hooks.json`, `skills/`, `commands/`, `agents/`, `bin/`) that exist on the host are bound **read-only**; writes to missing ones land only in the discarded private copy.
  - After the run, only a same-account token refresh is merged into each host credential file, by the same rules in both engines: the access, refresh and id tokens of objects that hold tokens (grok's JWT `key` included) and, as part of such a refresh, the token type and the expiry and last-refresh times; every other field keeps its host value. Anything else refuses the whole file and is reported once, naming the file and the field, never a value: a host file that was absent at launch, or a host or sandbox document that is not a strict JSON object (duplicate keys, NaN, lone UTF-16 surrogate escapes, depth over 64); a token or token-holding object removed, emptied or retyped; a changed protected field (e-mail, API key, secret, client id, endpoint/issuer/URL, auth mode or mechanism, account/user/org/team id, e-mail- or URL-shaped values); a JWT replaced by an opaque token or carrying other account claims; a refresh token changed without its access token; an access token changed in an object without a refresh token (a new login); a changed Google refresh token (`1//…`, which a refresh never rotates). Protected fields the sandbox adds or removes are not written (reported) while the refresh is. The write is atomic, only to regular files, never through symlinks, under an exclusive `flock` on the sidecar `.<name>.makewand-lock` created next to the file (10 s wait, then skipped with a warning; neither engine opens that lock file through a symlink, nor on Windows through a reparse point, and when it cannot be opened the write-back goes on without the lock), and only if the host file still has its launch content and is still the file the merge read. Read-only reviews write back in both engines, and so do Go remote-origin (`makewand serve`) requests. `flow_auth.json` is copied in only; session history and other provider state are discarded. See [Credential write-back residuals](#credential-write-back-residuals--凭据回写的残余风险).
  - Other providers' credentials, host SSH keys (`~/.ssh`), GPG keys (`~/.gnupg`), cloud tokens (`~/.aws`, `~/.kube`) and host Unix domain sockets — including sockets anywhere in the workspace such as under `node_modules` — are masked. In the Go launcher, `.git` metadata (including linked-worktree `gitdir`/`commondir` and `modules`) is read-only, so agents cannot plant hooks or rewrite repository configuration.
  **每次运行的私有 Provider 家目录**：宿主上的 provider 目录（如 `~/.claude`、`~/.codex`、`~/.gemini`、`~/.config/muse`、`~/.grok`）不再可写挂载。每次调用都在 `$XDG_RUNTIME_DIR` 或缓存目录下创建 `0700` 私有目录，挂载到 provider 的路径上：
  - 只复制白名单内的凭据与种子文件。宿主上已存在的 Agent 指令、MCP 配置、hooks、插件、技能和可执行路径（如 `CLAUDE.md`、`GEMINI.md`、`settings.json`、`hooks.json`、`skills/`、`commands/`、`agents/`、`bin/`）只读挂载；对不存在条目的写入只落在用完即弃的私有副本中。
  - 运行结束后，只有同一账号的令牌刷新会合并进宿主凭据文件，两引擎规则相同：含令牌对象的 access、refresh、id token（含 grok 的 JWT `key`），以及作为这次刷新一部分的令牌类型与过期/最近刷新时间；其他字段一律保留宿主值。其余改动整份拒绝并提示一次（指明文件和字段，绝不显示值）：启动时宿主没有该文件，或宿主/沙箱任一方不是严格的 JSON 对象（重复键、NaN、孤立的 UTF-16 代理转义、深度超过 64）；令牌或含令牌的对象被删除、清空或改变类型；受保护字段被改值（邮箱、API key、secret、client id、端点/issuer/URL、认证方式、账号/用户/组织/团队 ID，以及邮箱或 URL 形态的值）；JWT 被换成不透明令牌或带有其他账号的声明；只换 refresh token 而不换对应的 access token；在没有 refresh token 的对象中更换 access token（这是新登录）；更换 Google refresh token（`1//…`，刷新从不轮换）。沙箱新增或删除的受保护字段不写回（会提示），刷新照常写回。写回是原子的，只写普通文件、不跟随符号链接，在文件旁创建的 `.<文件名>.makewand-lock` 上持有排他 `flock`（最多等 10 秒，超时跳过并提示；两引擎都不经符号链接打开该锁文件，Windows 上也不经重解析点，无法打开时不加锁继续写回），且宿主文件内容仍与启动时相同、仍是合并时读取的那个文件。两个引擎的只读审查都会写回，Go 的远端（`makewand serve`）请求也会。`flow_auth.json` 只拷入；会话历史与其他 provider 状态全部丢弃。见[凭据回写的残余风险](#credential-write-back-residuals--凭据回写的残余风险)。
  - 其他 Provider 凭据、宿主 SSH 密钥（`~/.ssh`）、GPG（`~/.gnupg`）、云凭据（`~/.aws`、`~/.kube`）以及宿主 Unix domain socket（包括工作区内任何位置、如 `node_modules` 下的套接字）完全屏蔽。Go 启动器中，`.git` 元数据（含链接工作树的 `gitdir`/`commondir` 与 `modules`）只读，Agent 无法植入 hooks 或改写仓库配置。
- **Launcher integrity, seccomp and limits**: `bwrap` (and `systemd-run`/`prlimit`) are resolved only from root-owned, non-writable system directories, never from `$PATH`; set `MAKEWAND_BWRAP_PATH` to an absolute path for other installs. A seccomp filter blocking `ptrace`, `bpf`, `keyctl`, `process_vm_readv/writev`, `userfaultfd` and the x32 ABI is **required** on x86-64 and arm64 (`MAKEWAND_ALLOW_NO_SECCOMP=1` opts out with a warning). Where a systemd user manager is reachable, sandboxed launches run in a transient user scope with a memory cap (`MAKEWAND_SANDBOX_MEMORY_MAX`, default `8G`; swap off; never `RLIMIT_AS`); the Python launcher's scope also caps tasks (`MAKEWAND_SANDBOX_TASKS_MAX`, default `512`), while the Go launcher sets no task cap on Linux (its Windows job object limits processes). Without a user manager, the Python launcher falls back to `prlimit` limits on open files and file size only, and with neither the launch has no resource cap (a warning is printed once); the Go launcher then runs uncapped and reports `MemoryEnforcement` `none`. Writable workspaces at `/`, `/tmp`, `/home`, system roots, `$HOME` or an ancestor of `$HOME` are refused, and a writable workspace that cannot be scanned completely (more than 1,000,000 entries by default, `MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT`; or an unreadable directory you own) fails closed; another user's directory you cannot enter is skipped because the sandbox cannot reach it either.
  **启动器完整性、seccomp 与资源上限**：`bwrap`（以及 `systemd-run`/`prlimit`）只从 root 拥有、不可写的系统目录解析，绝不经 `$PATH`；装在其他位置时用 `MAKEWAND_BWRAP_PATH` 指定绝对路径。x86-64 与 arm64 上**强制**启用 seccomp 过滤器，屏蔽 `ptrace`、`bpf`、`keyctl`、`process_vm_readv/writev`、`userfaultfd` 与 x32 ABI（`MAKEWAND_ALLOW_NO_SECCOMP=1` 可显式关闭并告警）。能连上 systemd 用户管理器时，沙箱进程运行在临时用户 scope 中，受内存上限约束（`MAKEWAND_SANDBOX_MEMORY_MAX` 默认 `8G`，禁用 swap，不使用 `RLIMIT_AS`）；Python 启动器的 scope 还限制进程数（`MAKEWAND_SANDBOX_TASKS_MAX` 默认 `512`），Go 启动器在 Linux 上不设进程数上限（Windows 上由作业对象限制进程数）。连不上时，Python 启动器退回到 `prlimit`，只限制打开文件数与文件大小；两者都不可用时没有资源上限（只提示一次）；Go 启动器此时不设上限，并报告 `MemoryEnforcement` 为 `none`。拒绝以 `/`、`/tmp`、`/home`、系统根目录、`$HOME` 或其上级目录作为可写工作区；可写工作区无法完整扫描时（默认超过 1,000,000 个条目，可用 `MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT` 调整；或含有当前用户自己的不可读目录）fail-closed；其他用户的、无法进入的目录会被跳过，因为沙箱同样无法访问。
- **No unsandboxed launch without acknowledgement**: without bubblewrap, *every* provider launch — read-only reviews included — requires `MAKEWAND_UNSAFE_HOST_EXEC=1` plus the recorded acknowledgement and is audited. Provider CLIs launched by makewand carry `MAKEWAND_PROVIDER_WORKER=1`, and makewand allows only `search`, `repomap`, static help/version and `completion <shell>` under it (administrative commands are refused), so nested dispatch cannot recurse.
  **无确认不在沙箱外启动**：没有 bubblewrap 时，**所有** provider 调用（包括只读审查）都需要 `MAKEWAND_UNSAFE_HOST_EXEC=1` 加已记录的确认，并写入审计日志。makewand 启动的 provider CLI 带有 `MAKEWAND_PROVIDER_WORKER=1`，在此环境下 makewand 仅允许 `search`、`repomap`、静态帮助/版本和 `completion <shell>`，管理命令也被拒绝，嵌套调用不会形成递归。
- **Network Namespace & Socket Boundaries (`--unshare-net`)**: In trusted mode (the default), subscription CLI providers require outbound network access to communicate with cloud model endpoints, and therefore operate within the host network namespace. Filepath sockets under `/tmp`, `/var`, `/run`, and `$HOME` are masked, and GUI/D-Bus environment variables (`DISPLAY`, `XAUTHORITY`, `WAYLAND_DISPLAY`, `DBUS_*`) are stripped. Under Linux kernel architecture, abstract AF_UNIX sockets (`@/tmp/.X11-unix/X0`) and loopback services (`127.0.0.1`, `::1`) are scoped to the network namespace rather than filesystem mounts. In `--repo-trust=untrusted` mode (or when `MAKEWAND_SANDBOX_UNSHARE_NET=1` is configured), `--unshare-net` is strictly enforced to isolate the network namespace, completely blocking access to all host abstract sockets and loopback ports.
  **网络命名空间与套接字隔离边界（`--unshare-net`）**：在默认可信模式下，订阅制 CLI 需要访问外部模型 API，因此在宿主网络命名空间中运行。沙箱屏蔽了 `/tmp`、`/var`、`/run`、`$HOME` 下的文件路径套接字，并剔除了 GUI 与 D-Bus 环境变量（`DISPLAY`、`XAUTHORITY`、`WAYLAND_DISPLAY`、`DBUS_*`）。但由于 Linux 内核将抽象 AF_UNIX 套接字（如 `@/tmp/.X11-unix/X0`）和回环服务（`127.0.0.1`）绑定在网络命名空间而非文件系统挂载点，在允许联网时它们保持可达。在不可信仓库模式（`--repo-trust=untrusted` 或设置 `MAKEWAND_SANDBOX_UNSHARE_NET=1`）下，沙箱强制启用 `--unshare-net`，建立全新网络命名空间，彻底阻断对宿主抽象套接字与本地回环服务的连接。
- **Fail-Closed Sandbox Enforcement**: For writable generation and code review tasks across both Python and Go router entrypoints (`makewand [prompt]`, `chat`, `--print`, `new`), bubblewrap sandbox isolation is mandatory. If `bwrap` is missing or probe fails, makewand fails closed and refuses execution unless `MAKEWAND_UNSAFE_HOST_EXEC=1` is explicitly set AND authenticated via a recorded one-time host acknowledgment. When authorized to fall back to the host, the process environment is strictly sanitized of foreign credentials/tokens and all host executions are audited to `unsafe_exec_audit.jsonl`.
  **失败即停沙箱契约**：对于代码生成与审查任务，无论 Python 调度引擎还是 Go 路由入口（`makewand [prompt]`、`chat`、`--print`、`new`），Bubblewrap 沙箱物理隔离均为强制约束。若系统无可用 `bwrap` 或探测失败，除非显式设置 `MAKEWAND_UNSAFE_HOST_EXEC=1` 且已完成记录的一次性本机授权确认，否则一律失败即停（fail-closed）拒绝执行。即使经授权降级至宿主执行，也对环境变量执行严格凭据脱敏剥离，并将命令写入 `unsafe_exec_audit.jsonl` 审计日志。

### Credential write-back residuals / 凭据回写的残余风险

Only a same-account refresh is written back, but which account a token belongs to can be checked offline only for JWTs, and the sandboxed provider process holds your tokens anyway. A sandboxed agent can still:
只有同一账号的刷新会写回，但只有 JWT 能离线核对令牌属于哪个账号，而且沙箱内的 provider 进程本来就持有你的令牌。沙箱内的 agent 仍能：

- **Substitute opaque tokens and unverified JWTs.** Claude's tokens, Google `ya29.` access tokens, codex and grok refresh tokens and muse's tokens are opaque and cannot be tied to an account offline. JWT signatures are not verified, and account claims (`sub`, `email`, `user_id`, `principal_id`, `team_id`, `account_id`, `chatgpt_account_id`, `chatgpt_user_id`, `chatgpt_account_user_id`, at any depth) are compared only where the host token has them: a host JWT without them can be replaced by any JWT, and claims can be added. So another account's tokens can be planted consistently: both Claude tokens; the Google access token until it expires (Google refresh tokens are pinned); and for codex and grok a forged access JWT carrying your claims plus another account's refresh token passes, and the host switches accounts at its next refresh.
  **替换不透明令牌、利用不校验签名的 JWT**：Claude 的令牌、Google `ya29.` access token、codex 与 grok 的 refresh token 以及 muse 的令牌都是不透明的，无法离线对应到账号。JWT 签名不校验，账号声明（`sub`、`email`、`user_id`、`principal_id`、`team_id`、`account_id`、`chatgpt_account_id`、`chatgpt_user_id`、`chatgpt_account_user_id`，任意深度）只在宿主令牌带有时才比较：宿主 JWT 不含这些声明时可被任意 JWT 替换，也可以新增声明。因此可以一致地植入其他账号的令牌：Claude 的两个令牌；Google access token（在其过期前有效，Google refresh token 已固定）；codex 与 grok 则可用带有你的账号声明的伪造 access JWT 加上其他账号的 refresh token 通过校验，宿主在下次刷新时切换到该账号。
- **Log you out; denial of service cannot be prevented.** A token can be replaced by any non-empty string: opaque tokens set to `" "` are written where a refresh could change them (Claude's tokens, muse's OAuth tokens in the shape that has a refresh token, muse's minted `api_key`) and log the host out; a JWT set to `" "` is refused, so for codex and grok it takes a forged JWT plus any refresh token. With a refresh, any later expiry (even the year 9999) is accepted. An agent can also use up a rotating refresh token inside the sandbox and then make the write-back refuse; the host keeps a token the server has invalidated. Any skipped write-back after a real rotation (a concurrent host change, a lock timeout, a write failure) loses the refresh the same way.
  **让宿主登出（无法防止拒绝服务）**：令牌可以被换成任意非空字符串：在刷新可能改变它们的位置，不透明令牌设为 `" "` 会被写回并使宿主登出（Claude 的令牌、muse 带 refresh token 的 OAuth 形态令牌、muse 铸造的 `api_key`）；JWT 设为 `" "` 会被拒绝，所以 codex 与 grok 需要伪造的 JWT 再配任意 refresh token。伴随刷新时，任何更晚的过期时间（甚至 9999 年）都会被接受。agent 还能在沙箱内用掉会轮换的 refresh token，再让写回被拒，宿主就留下服务端已作废的令牌。真实轮换之后任何被跳过的写回（宿主同时被修改、等锁超时、写入失败）同样会丢失这次刷新。
- **Replace muse's minted key.** muse mints `api_key` itself only in its OAuth login record (an object with an OAuth `access_token` whose `mechanism` and `obtained_via` do not name a key); a changed key there is written back alone as a re-mint if it stays a non-empty string with the host's JWT shape and claims, so it can be replaced by another value. A key saved with `muse auth set --api-key-stdin` or pasted (`pasted_key`), and any `api_key` without an OAuth access token beside it, at the top level or nested, is protected: a change is refused, a removal keeps the host key and is reported.
  **替换 muse 铸造的 key**：只有在 muse 的 OAuth 登录记录里（对象含 OAuth `access_token`，且 `mechanism`、`obtained_via` 都不指向 key），`api_key` 才算 muse 自行铸造；那里变化的 key 视为重新铸造单独写回，只要求保持非空字符串及与宿主相同的 JWT 形态和声明，因此可被换成别的值。用 `muse auth set --api-key-stdin` 保存或粘贴（`pasted_key`）的 key，以及旁边没有 OAuth access token 的任何 `api_key`（顶层或嵌套）都受保护：改动被拒绝，删除则保留宿主的 key 并提示。

Further limits:
其他限制：

- **Token type and times**: the token type may change only in letter case (`Bearer` → `bearer`); expiry and last-refresh times are written only with a new access token in the same object or one below it, and only forward; a backward, unreadable or retyped time refuses the file.
  **令牌类型与时间**：令牌类型只允许改变大小写（`Bearer` → `bearer`）；过期与最近刷新时间只在同一对象或其下层对象出现新 access token 时写回，且只能往后；往前、无法解析或改变类型的时间会使整个文件被拒绝。
- **Refused documents**: a host file that itself contains a lone surrogate escape (Node can write one) is refused on every run, with a warning. Field names in the warnings are printed as the sandbox copy spells them.
  **被拒绝的文档**：宿主文件本身含孤立代理转义时（Node 可能写出这种文件）每次运行都会被拒绝并提示。提示中的字段名按沙箱副本的写法原样打印。
- **Skipped write-backs**: a copy that existed at launch and is deleted, replaced by a symlink, FIFO or directory, larger than 4 MiB or unreadable at the end is not written back and is reported (log in again on the host if the provider rotated its tokens). A login made in the sandbox is reported and never written back, also when the provider's whole directory (e.g. `~/.gemini`) is missing on the host: it gets an empty private copy wherever bubblewrap can create the mount point; where the workspace shows `$HOME` read-only, a login there fails inside the sandbox. Without a warning: a credential absent at launch that stays absent, an unchanged copy, and changes without a new access token, which are dropped.
  **被跳过的写回**：启动时存在的副本在结束时被删除、换成符号链接/FIFO/目录、超过 4 MiB 或不可读时不回写，并会提示（若提供方已在沙箱内轮换令牌，请在宿主重新登录）。沙箱内新建的登录会提示且从不写回；provider 的整个目录（如 `~/.gemini`）在宿主不存在时也一样：bubblewrap 能创建挂载点的地方会给它一个空的私有副本，工作区让 `$HOME` 只读可见时，在那里登录会在沙箱内失败。不提示的只有：启动时不存在且一直不存在的凭据、未改动的副本，以及不带新 access token 的改动（直接丢弃）。
- **Timing**: Python finishes the write-back before the provider launch returns. Go's `Chat` and its streams, including a stream that a cancellation or deadline ends, finish only after it, waiting at most 15 s (the 10 s lock wait plus 5 s) and then warning with the private home's path; a Router stream adds the process teardown/pipe-drain allowance (1 s, for a total bound of 16 s), and a fallback after an attempt's timeout starts only once the timed-out stream has ended. Streams of API providers end at once. Concurrent runs of one provider copy the same host file at launch; the later write-back finds the host changed and keeps it (reported), losing its own refresh. A process killed during a run never writes back, and Go then leaves its private copies under the user cache directory (`makewand/cli-homes`).
  **时序**：Python 在 provider 启动调用返回前完成写回。Go 的 `Chat` 与流式调用（包括因取消或超过期限而结束的流）都要等写回完成才结束，最多等 15 秒（10 秒等锁加 5 秒），超时则提示私有目录路径；Router 的流式调用另预留 1 秒的进程退出/管道收尾时间（总等待上限 16 秒），尝试超时后的回退要等超时的流结束才开始。API provider 的流式调用立即结束。同一 provider 的并发运行在启动时复制同一宿主文件，较晚的写回发现宿主已变化就保留宿主版本（会提示），自身的刷新因此丢失。运行中被杀死的进程不会写回，Go 还会把私有副本留在用户缓存目录（`makewand/cli-homes`）下。
- **Remote callers and the lock window**: because Go remote-origin (`makewand serve`) requests write back too, all of the above applies to runs a remote caller triggers. A writer that does not take makewand's lock (the provider CLI on the host, the user, another tool) can still change the host file between the final re-check and the rename; that write is lost. Where `flock` is unavailable, or the lock file cannot be opened (neither engine follows a symlink planted at its path), the re-check alone applies.
  **远端调用方与锁窗口**：Go 的远端（`makewand serve`）请求同样会写回，以上各项也适用于远端调用方触发的运行。不使用 makewand 锁的写入者（宿主上的 provider CLI、用户或其他工具）仍可能在最后一次复核与重命名之间修改宿主文件，那次写入会丢失。不支持 `flock`、或锁文件无法打开（两引擎都不跟随放在该路径上的符号链接）的地方只做复核。

### Host-mode delivery / 宿主模式交付

In host mode a Python task edits your directory inside a transaction. Before delivery, every path the task created, modified or deleted is judged on type, mode and content against the reviewed diff; an unreviewed change blocks delivery, is rolled back and is reported as UNVERIFIED (exit 11): an ignored file created or changed, a binary or executable artifact or an executable bit the diff does not show, a security-sensitive file (`.env*`, `.envrc`, `*.pth`, `sitecustomize.py`, `usercustomize.py`, `.husky/`, `.githooks/`), a file in a submodule or nested repository, or a new hook in the hooks directory of this checkout or of a checked-out submodule, which is moved to `rejected_*/quarantine/git-hooks/` with an `mv` command to restore it. A hooks directory other checkouts share (every checkout of a repository with linked worktrees, a `core.hooksPath` outside the repository) is only reported: its changes cannot be attributed to the task, so a hook the task plants there, even after running `git worktree add` itself, is not blocked. `*.sample` hooks are ignored and `.git/config` is not compared. Test output at the tools' default report locations is removed instead of blocking. Pre-existing files without a backup are checked against SHA-256 digests and sampled fingerprints taken at task start within `MAKEWAND_FINGERPRINT_FILE_LIMIT`, `MAKEWAND_FINGERPRINT_TOTAL_LIMIT` and `MAKEWAND_FINGERPRINT_SECONDS`; a file whose content cannot be confirmed blocks as unconfirmed. An in-place patch of a large file placed between its sampled chunks, with its mtime restored and a hard link added outside the workspace, is not detected. See [the verification contract](docs/VERIFICATION_CONTRACT.md#python-host-mode-delivery).
宿主模式下，Python 任务在事务中修改你的目录。交付前，本次任务新建、修改或删除的每个路径都按类型、权限与内容与审查 diff 核对；未经审查的改动会阻止交付、回滚并以 UNVERIFIED（退出码 11）报告：新建或改动被忽略的文件、二进制或可执行产物以及 diff 中没有体现的可执行位、安全敏感文件（`.env*`、`.envrc`、`*.pth`、`sitecustomize.py`、`usercustomize.py`、`.husky/`、`.githooks/`）、子模块或嵌套仓库中的文件，以及本检出或已检出子模块的 hooks 目录中新增的 hook——它会被移入 `rejected_*/quarantine/git-hooks/` 并附 `mv` 恢复命令。被其他检出共享的 hooks 目录（有链接工作树的仓库的每个检出、仓库外的 `core.hooksPath`）只报告：其中的改动无法归因于本任务，所以任务在那里植入的 hook（即使任务自己先运行了 `git worktree add`）不会被阻断。`*.sample` 会被忽略，`.git/config` 不做比较。工具默认报告位置下的测试产物会被删除而不是阻断。没有备份的既有文件按任务开始时在 `MAKEWAND_FINGERPRINT_FILE_LIMIT`、`MAKEWAND_FINGERPRINT_TOTAL_LIMIT`、`MAKEWAND_FINGERPRINT_SECONDS` 限额内计算的 SHA-256 摘要与抽样指纹核对，内容无法确认的文件按“无法确认”阻断。对大文件的原地修改若恰好落在抽样块之间、恢复了 mtime 并在工作区外新增硬链接，则无法发现。详见[验证契约](docs/VERIFICATION_CONTRACT.md#python-host-mode-delivery)。

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

A review or race verdict counts only when it carries the task's nonce (an
un-nonced verdict line never counts, and no setting re-enables it) and comes from a
reviewer that received the whole prompt with the complete diff: diffs over
`MAKEWAND_REVIEW_DIFF_MAX_BYTES` (256 KiB by default, measured as dispatched) are
never reviewed truncated, agy is never given a review prompt over 31 KiB,
placeholder engines (cursor, copilot) never review, and a read-only local model
answer counts only when its token counts show the whole prompt was processed. A
server that cuts a prompt yet reports at least one token per 6 bytes cannot be
detected. A race win, Winner-Take-All included, needs a read-only review by a
provider other than the winner's; the judge and that reviewer prefer a provider
independent of both contestants, and the reviewer is otherwise the other
contestant. See [reviewer capacity](docs/EXECUTION_CONTRACT.md#reviewer-capacity).
审查与竞速裁决只有带本任务 nonce（不带 nonce 的裁决行一律无效，没有任何设置能恢复）、且来自收到完整提示和完整 diff 的审查方时才有效：超过 `MAKEWAND_REVIEW_DIFF_MAX_BYTES`（默认 256 KiB，按实际派发的文本计量）的 diff 从不截断审查，超过 31 KiB 的审查提示从不交给 agy，占位引擎（cursor、copilot）从不审查，本地模型的只读回答只有在 token 计数表明处理了完整提示时才有效；截断提示却仍报告每 6 字节至少 1 个 token 的服务端无法被发现。竞速胜出（含 Winner-Take-All）须经胜者以外提供方的只读复审；裁判与复审方都优先选择独立于双方选手的提供方，复审方没有这样的选择时由另一位选手担任。

Direct cloud APIs and API fallback are disabled by default, including in untrusted
repository mode. Explicitly set `MAKEWAND_API_POLICY=allow_paid` or `api_policy` to
`allow_paid` to enable them. A remote server and third-party CLIs retain their own
billing policies. A configured key alone neither authorizes a call nor makes an
engine usable: under `subscription_only` the claude, codex, agy, grok and muse adapters never relay
a limited, logged-out or missing CLI to the API (a logged-out CLI still runs; the
other cases fail, naming the policy), routing and `ensemble` count such an engine as
unusable, and `status --probe` reports an engine whose CLI is missing and whose key
the policy blocks as missing. All five adapters share the same policy and
dispatch-route reporting helper.
环境中存在 API key 本身不再授权 Makewand 发起云 API 调用，也不会让引擎变为可用：`subscription_only` 下，claude、codex、agy、grok、muse 的适配器从不把限额、登录失效或未安装的 CLI 转给 API（登录失效时仍运行 CLI，其余情况直接失败并说明被 API 策略阻止），路由与 `ensemble` 把这类引擎视为不可用，`status --probe` 把未安装 CLI、key 被策略阻止的引擎报告为 missing。五个适配器共用同一策略与派发路线记录入口。

`makewand serve` bills streamed API calls at the model the provider actually
calls, unpriced API models at a conservative $3/$15 per million tokens, and
records every unmeasured cost with `cost_estimated=1`; an estimate never exceeds
the call's `max_tokens` unless the text provably does, and a reply that reports
only part of its usage keeps its reported cost. With
`MAKEWAND_SCALED_BUDGET_RESERVATION=1` each request reserves at the highest price
of any API model routing can call, including a route built on demand such as an
API-key-only deployment's `codex` route, priced at the model it calls
(`OPENAI_MODEL`, not its table's `codex-cli` placeholder), and a stream is
stopped once a lower bound of its streamed tokens passes what the reservation
covers, so a reply within `max_tokens` is not cut, apart from a few
pathological ones (long runs of one letter or emoji, `../` repeated on
cl100k). The bound is calibrated on eleven published vocabularies (o200k_base,
cl100k_base, Llama 3.1, Qwen 2.5/3, DeepSeek-V3, Gemma 2/3, Mistral Nemo,
GLM-4.5, Grok-1), not on current Claude
vocabularies. An upstream that ignores `max_tokens` can therefore stream past the
reservation before the cut, in real tokens: ordinary words and emoji close to 1×,
CJK text about 4×, floods of one repeated character more (on GPT-4o: digits
about 3×, `●` 16×, `▍` about 32×, rare Unicode whitespace up to about 96×; other
vocabularies higher); alternating whitespace such as `" \n"` is never cut.
`makewand serve` 按 provider 实际调用的模型为流式 API 调用计价，没有价目的 API 模型按每百万 token $3/$15 的保守价计费，所有未测得的费用记为 `cost_estimated=1`；估算不超过该次调用的 `max_tokens`，除非文本可证明更长；只报告部分 usage 的回复保留其报告的费用。设置 `MAKEWAND_SCALED_BUDGET_RESERVATION=1` 时，每个请求按路由可调用的任一 API 模型（包括按需构建的路线，如只配 API key 部署的 `codex` 路线，按它实际调用的 `OPENAI_MODEL` 而非模型表中的 `codex-cli` 占位计价）的最高价预留，流式输出的 token 下界超过预留覆盖量时即停止，所以 `max_tokens` 之内的回复不会被截断，少数异常回复除外（同一字母或 emoji 的长串、cl100k 上重复的 `../`）。该下界按 11 个公开词表（o200k_base、cl100k_base、Llama 3.1、Qwen 2.5/3、DeepSeek-V3、Gemma 2/3、Mistral Nemo、GLM-4.5、Grok-1）校准，未覆盖当前的 Claude 词表。因此忽略 `max_tokens` 的上游在被截断前可能超出预留，按真实 token 计：普通单词与 emoji 接近 1 倍，CJK 文本约 4 倍，单个重复字符的洪流更多（GPT-4o 上数字约 3 倍、`●` 16 倍、`▍` 约 32 倍、少见的 Unicode 空白最多约 96 倍；其他词表更高）；交替的空白（如 `" \n"`）永远不会被截断。

Without a codex CLI, Power mode's `codex` route is the OpenAI API: it calls the
configured `OPENAI_MODEL` and is billed and budgeted as API spend, never as a
free `codex-cli` call. A client-named OpenAI model (`gpt-*`, `o1*`, `o3*`,
`o4*`, `codex-mini*`) is served only when it has a price or is the configured
model, and a scaled reservation covers its price, so a client cannot choose a
model whose spend the budgets cannot price. A token's allowed modes do not
narrow that choice; its cost budgets bound it. A stream is billed from an
estimate of the text it delivers, which leaves out the hidden reasoning tokens
of OpenAI's reasoning models (o-series, GPT-5), so a streamed request to one
of them can be billed below OpenAI's charge, by at most the price of the call's
output cap. Its upstream errors block or open the circuit of that model only,
never the shared `openai` route, so a client's choice of model cannot take the
route down for other clients. Remote session storage is capped per account
(1,000 sessions and 256 MiB by default) and refuses saves that would leave less
than 1 GiB free on the sessions disk, which holds the state database by
default, checking an account's concurrent saves one after another, so one
account cannot fill the disk and take the server down for everyone; with the
session cap removed, the byte cap alone does not bound the number of empty
sessions.
没有 codex CLI 时，Power 模式的 `codex` 路线就是 OpenAI API：调用配置的 `OPENAI_MODEL`，按 API 花费计费并计入预算，不再记为免费的 `codex-cli` 调用。客户端指名的 OpenAI 模型（`gpt-*`、`o1*`、`o3*`、`o4*`、`codex-mini*`）只有在有价目或就是配置的模型时才提供，按比例放大的预留也覆盖它的价格，所以客户端无法选用预算无法计价的模型。令牌允许的模式不限制这种选择，由令牌的费用预算约束。流式请求按其输出文本的估算计费，不含 OpenAI 推理模型（o 系列、GPT-5）隐藏的推理 token，所以对这些模型的流式请求可能低于 OpenAI 的实际收费，差额不超过该次调用输出上限的价格。其上游错误只阻断或熔断该模型自己，不影响共享的 `openai` 路线，所以客户端选用的模型无法让其他客户端的请求失败。远端会话存储按账号限额（默认 1,000 个会话、256 MiB），并拒绝会让会话所在磁盘（默认也存放状态数据库）剩余空间低于 1 GiB 的保存，同一账号的并发保存逐个检查，单个账号无法写满磁盘、拖垮整个服务；去掉会话数上限后，仅靠字节上限无法限制空会话的数量。

Usage and audit CSV exports (`makewand usage --csv`, `makewand audit --csv`,
`GET /v1/admin/{usage,audit}/events?format=csv`), and the team billing and
budget-alert CSV exports, prefix a single quote to every cell that begins with
`=`, `+`, `-`, `@`, a tab, a carriage return, a line feed, or a full-width `＝`,
`＋`, `－` or `＠`, plain ASCII numbers excepted, so a client-supplied value such
as a rejected model name opens in a spreadsheet as text, not as a formula. The
ledgers keep values as received.
用量与审计的 CSV 导出（`makewand usage --csv`、`makewand audit --csv`、`GET /v1/admin/{usage,audit}/events?format=csv`）以及团队账单与预算告警的 CSV 导出，在以 `=`、`+`、`-`、`@`、制表符、回车、换行或全角 `＝`、`＋`、`－`、`＠` 开头的单元格（纯 ASCII 数字除外）前加单引号，客户端提供的值（例如被拒绝的模型名）在电子表格中按文本显示，不会作为公式执行；账本仍按收到的原值记录。

Python candidate application uses fixed POSIX directory handles or native Windows
handles for replacement and recovery. Native Windows execution requires explicit
unsafe-host consent and cannot grant Strength 2; Windows Job Objects manage
resources and process trees without providing Linux sandbox isolation. See
[the native Windows contract](docs/VERIFICATION_CONTRACT.md#python-pipeline-and-race-candidates).
Python 候选应用支持 POSIX 和原生 Windows 固定句柄。Windows 执行生成代码仍需显式宿主执行授权，不能达到 Strength 2；Job Object 不提供 Linux 沙箱的文件、凭据或网络隔离。

## Supported Versions

| Version | Supported | Notes |
| --- | --- | --- |
| `master` (source install) | Yes | Has every protection this document describes; ships as v3.3.0 |
| `v3.2.x` | Yes | Lacks the protections added after v3.2.0 ([Release status](#release-status--发布状态)), which ship in v3.3.0 |
| `< v3.2.0` | No | |

v3.2.x 缺少 v3.2.0 之后加入的保护（见[发布状态](#release-status--发布状态)），这些保护随 v3.3.0 发布；`master` 源码安装已包含本文描述的全部保护。

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
