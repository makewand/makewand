# Security Policy

## Security Model / 安全模型

makewand hands work to coding CLIs that are signed in on this machine. Read this before pointing makewand at a repository you do not fully trust.
makewand 把工作交给本机已登录的编码 CLI。在让 makewand 处理你不完全信任的仓库前，请先读这一节。

### Release status / 发布状态

This document describes `v4.0.0` and `master`. v4.0.0 removed the Go TUI and server, the Go engine and the Python pipeline; they stay in the tag `archive/pre-trim-20261105`, whose copy of this file describes them.
本文描述 `v4.0.0` 与 `master`。v4.0.0 移除了 Go 的 TUI、server、引擎和 Python 流水线；它们保留在 tag `archive/pre-trim-20261105`，该 tag 里的本文件描述的是它们。


### Provider CLIs run in a sandbox / 提供方 CLI 在沙箱内运行

When a subscription CLI provider (`claude`, `codex`, `gemini`/`agy`, `grok`, `muse`, or `aider`) produces or reviews code, makewand physically isolates that CLI process via `wrap_bwrap(is_provider=True)` applying tailored `PROVIDER_PROFILES`:
当由订阅制 CLI provider（`claude`、`codex`、`gemini`/`agy`、`grok`、`muse` 或 `aider`）生成或审查代码时，makewand 通过 `wrap_bwrap(is_provider=True)` 并在定制的 `PROVIDER_PROFILES` 规则下物理隔离运行该 CLI：

- **System & Root Isolation**: System binaries and utilities (`/usr`, `/bin`, `/lib`, `/etc`, etc.) are mounted strictly read-only (`--ro-bind`). Installed system software cannot be tampered with.
  **系统只读与环境隔离**：系统二进制与底层工具（`/usr`、`/bin`、`/lib`、`/etc` 等）以严格只读方式挂载，无法篡改系统软件与全局配置。
- **Ephemeral & Masked HOME**: User home directories and sensitive root paths (`/home`, `/root`, `/mnt`, `/media`, `/srv`) are hidden and masked. An isolated, empty `tmpfs` is mounted as `$HOME`, preventing access to the host's private user files or unrelated repositories.
  **临时 HOME 与敏感根屏蔽**：敏感宿主目录（`/home`、`/root`、`/mnt`、`/media`、`/srv`）被物理隐藏。沙箱内以全新空白 `tmpfs` 挂载为 `$HOME`，阻断对宿主机个人文件和跨项目代码的读取。
- **Private per-run provider home (`PROVIDER_PROFILES`)**: the host provider directory (e.g. `~/.claude`, `~/.codex`, `~/.gemini`, `~/.config/muse`, `~/.grok`) is never bound writable. Each launch gets a private `0700` directory (under `$XDG_RUNTIME_DIR` or the cache directory) bound at the provider's path:
  - Only the allowlisted credential and seed files are copied in. Agent instructions, MCP configurations, hooks, plugins, skills and binary paths (e.g. `CLAUDE.md`, `GEMINI.md`, `settings.json`, `hooks.json`, `skills/`, `commands/`, `agents/`, `bin/`) that exist on the host are bound **read-only**; writes to missing ones land only in the discarded private copy.
  - After the run, only a same-account token refresh is merged into each host credential file: the access, refresh and id tokens of objects that hold tokens (grok's JWT `key` included) and, as part of such a refresh, the token type and the expiry and last-refresh times; every other field keeps its host value. Anything else refuses the whole file and is reported once, naming the file and the field, never a value: a host file that was absent at launch, or a host or sandbox document that is not a strict JSON object (duplicate keys, NaN, lone UTF-16 surrogate escapes, depth over 64); a token or token-holding object removed, emptied or retyped; a changed protected field (e-mail, API key, secret, client id, endpoint/issuer/URL, auth mode or mechanism, account/user/org/team id, e-mail- or URL-shaped values); a JWT replaced by an opaque token or carrying other account claims; a refresh token changed without its access token; an access token changed in an object without a refresh token (a new login); a changed Google refresh token (`1//…`, which a refresh never rotates). Protected fields the sandbox adds or removes are not written (reported) while the refresh is. The write is atomic, only to regular files, never through symlinks, under an exclusive `flock` on the sidecar `.<name>.makewand-lock` created next to the file (10 s wait, then skipped with a warning; the lock file is not opened through a symlink, nor on Windows through a reparse point, and when it cannot be opened the write-back goes on without the lock), and only if the host file still has its launch content and is still the file the merge read. Read-only reviews write back too. `flow_auth.json` is copied in only; session history and other provider state are discarded. See [Credential write-back residuals](#credential-write-back-residuals--凭据回写的残余风险).
  - agy ≥ 1.5.5 keeps its login in the desktop keyring, which the sandbox cannot reach, and reads the file token `~/.gemini/antigravity-cli/antigravity-oauth-token` instead; a host agy run deletes that file whenever it refreshes the keyring login. makewand therefore keeps its own copy at `~/.config/makewand/credential-backups/agy/antigravity-oauth-token` (`0600`, directories `0700`): each launch copies the host file there when they differ, and while the host file is gone the copy is copied in and receives the run's write-back under the same rules. The copy is hidden in every sandbox (`$HOME` is a tmpfs, `~/.config/makewand` is re-masked, and the directory is masked where a workspace below `$HOME` contains it). Signing out of agy on the host does not remove it: delete that file to stop makewand's agy runs from using the login.
  - Other providers' credentials, host SSH keys (`~/.ssh`), GPG keys (`~/.gnupg`), cloud tokens (`~/.aws`, `~/.kube`) and host Unix domain sockets — including sockets anywhere in the workspace such as under `node_modules` — are masked. In a writable sandbox, `.git` metadata (including linked-worktree `gitdir`/`commondir` and `modules`) is read-only, so agents cannot plant hooks or rewrite repository configuration.
  **每次运行的私有 Provider 家目录**：宿主上的 provider 目录（如 `~/.claude`、`~/.codex`、`~/.gemini`、`~/.config/muse`、`~/.grok`）不再可写挂载。每次调用都在 `$XDG_RUNTIME_DIR` 或缓存目录下创建 `0700` 私有目录，挂载到 provider 的路径上：
  - 只复制白名单内的凭据与种子文件。宿主上已存在的 Agent 指令、MCP 配置、hooks、插件、技能和可执行路径（如 `CLAUDE.md`、`GEMINI.md`、`settings.json`、`hooks.json`、`skills/`、`commands/`、`agents/`、`bin/`）只读挂载；对不存在条目的写入只落在用完即弃的私有副本中。
  - 运行结束后，只有同一账号的令牌刷新会合并进宿主凭据文件：含令牌对象的 access、refresh、id token（含 grok 的 JWT `key`），以及作为这次刷新一部分的令牌类型与过期/最近刷新时间；其他字段一律保留宿主值。其余改动整份拒绝并提示一次（指明文件和字段，绝不显示值）：启动时宿主没有该文件，或宿主/沙箱任一方不是严格的 JSON 对象（重复键、NaN、孤立的 UTF-16 代理转义、深度超过 64）；令牌或含令牌的对象被删除、清空或改变类型；受保护字段被改值（邮箱、API key、secret、client id、端点/issuer/URL、认证方式、账号/用户/组织/团队 ID，以及邮箱或 URL 形态的值）；JWT 被换成不透明令牌或带有其他账号的声明；只换 refresh token 而不换对应的 access token；在没有 refresh token 的对象中更换 access token（这是新登录）；更换 Google refresh token（`1//…`，刷新从不轮换）。沙箱新增或删除的受保护字段不写回（会提示），刷新照常写回。写回是原子的，只写普通文件、不跟随符号链接，在文件旁创建的 `.<文件名>.makewand-lock` 上持有排他 `flock`（最多等 10 秒，超时跳过并提示；不经符号链接打开该锁文件，Windows 上也不经重解析点，无法打开时不加锁继续写回），且宿主文件内容仍与启动时相同、仍是合并时读取的那个文件。只读审查也会写回。`flow_auth.json` 只拷入；会话历史与其他 provider 状态全部丢弃。见[凭据回写的残余风险](#credential-write-back-residuals--凭据回写的残余风险)。
  - agy 1.5.5 起把登录存在桌面密钥环里，沙箱连不到，这时 agy 改读文件令牌 `~/.gemini/antigravity-cli/antigravity-oauth-token`；宿主上的 agy 每次刷新密钥环登录都会删掉这个文件。所以 makewand 自己保管一份 `~/.config/makewand/credential-backups/agy/antigravity-oauth-token`（文件 `0600`，目录 `0700`）：每次启动时宿主文件和它不同就复制过去；宿主文件不在时，拷入的是这份副本，运行后的写回也按同样的规则写进它。这份副本在所有沙箱里都看不到（`$HOME` 是 tmpfs，`~/.config/makewand` 会重新屏蔽，工作区在 `$HOME` 下且包含它时也会屏蔽这个目录）。在宿主退出 agy 登录不会删掉它：要让 makewand 的 agy 不再用这个登录，请删除这个文件。
  - 其他 Provider 凭据、宿主 SSH 密钥（`~/.ssh`）、GPG（`~/.gnupg`）、云凭据（`~/.aws`、`~/.kube`）以及宿主 Unix domain socket（包括工作区内任何位置、如 `node_modules` 下的套接字）完全屏蔽。可写沙箱中，`.git` 元数据（含链接工作树的 `gitdir`/`commondir` 与 `modules`）只读，Agent 无法植入 hooks 或改写仓库配置。
- **Launcher integrity, seccomp and limits**: `bwrap` (and `systemd-run`/`prlimit`) are resolved only from root-owned, non-writable system directories, never from `$PATH`; set `MAKEWAND_BWRAP_PATH` to an absolute path for other installs. A seccomp filter blocking `ptrace`, `bpf`, `keyctl`, `process_vm_readv/writev`, `userfaultfd` and the x32 ABI is **required** on x86-64 and arm64 (`MAKEWAND_ALLOW_NO_SECCOMP=1` opts out with a warning). Where a systemd user manager is reachable, sandboxed launches run in a transient user scope with a memory cap (`MAKEWAND_SANDBOX_MEMORY_MAX`, default `8G`; swap off; never `RLIMIT_AS`); the scope also caps tasks (`MAKEWAND_SANDBOX_TASKS_MAX`, default `512`). Without a user manager, makewand falls back to `prlimit` limits on open files and file size only, and with neither the launch has no resource cap (a warning is printed once). Writable workspaces at `/`, `/tmp`, `/home`, system roots, `$HOME` or an ancestor of `$HOME` are refused, and a writable workspace that cannot be scanned completely (more than 1,000,000 entries by default, `MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT`; or an unreadable directory you own) fails closed; another user's directory you cannot enter is skipped because the sandbox cannot reach it either.
  **启动器完整性、seccomp 与资源上限**：`bwrap`（以及 `systemd-run`/`prlimit`）只从 root 拥有、不可写的系统目录解析，绝不经 `$PATH`；装在其他位置时用 `MAKEWAND_BWRAP_PATH` 指定绝对路径。x86-64 与 arm64 上**强制**启用 seccomp 过滤器，屏蔽 `ptrace`、`bpf`、`keyctl`、`process_vm_readv/writev`、`userfaultfd` 与 x32 ABI（`MAKEWAND_ALLOW_NO_SECCOMP=1` 可显式关闭并告警）。能连上 systemd 用户管理器时，沙箱进程运行在临时用户 scope 中，受内存上限约束（`MAKEWAND_SANDBOX_MEMORY_MAX` 默认 `8G`，禁用 swap，不使用 `RLIMIT_AS`）；scope 还限制进程数（`MAKEWAND_SANDBOX_TASKS_MAX` 默认 `512`）。连不上时退回到 `prlimit`，只限制打开文件数与文件大小；两者都不可用时没有资源上限（只提示一次）。拒绝以 `/`、`/tmp`、`/home`、系统根目录、`$HOME` 或其上级目录作为可写工作区；可写工作区无法完整扫描时（默认超过 1,000,000 个条目，可用 `MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT` 调整；或含有当前用户自己的不可读目录）fail-closed；其他用户的、无法进入的目录会被跳过，因为沙箱同样无法访问。
- **No unsandboxed launch without acknowledgement**: without bubblewrap, *every* provider launch — read-only reviews included — requires `MAKEWAND_UNSAFE_HOST_EXEC=1` plus the recorded acknowledgement and is audited. Provider CLIs launched by makewand carry `MAKEWAND_PROVIDER_WORKER=1`, and makewand allows only `search`, `repomap` and static help/version under it (administrative commands are refused), so nested dispatch cannot recurse.
  **无确认不在沙箱外启动**：没有 bubblewrap 时，**所有** provider 调用（包括只读审查）都需要 `MAKEWAND_UNSAFE_HOST_EXEC=1` 加已记录的确认，并写入审计日志。makewand 启动的 provider CLI 带有 `MAKEWAND_PROVIDER_WORKER=1`，在此环境下 makewand 仅允许 `search`、`repomap` 和静态帮助/版本，管理命令也被拒绝，嵌套调用不会形成递归。
- **Network Namespace & Socket Boundaries (`--unshare-net`)**: In trusted mode (the default), subscription CLI providers require outbound network access to communicate with cloud model endpoints, and therefore operate within the host network namespace. Filepath sockets under `/tmp`, `/var`, `/run`, and `$HOME` are masked, and GUI/D-Bus environment variables (`DISPLAY`, `XAUTHORITY`, `WAYLAND_DISPLAY`, `DBUS_*`) are stripped. Under Linux kernel architecture, abstract AF_UNIX sockets (`@/tmp/.X11-unix/X0`) and loopback services (`127.0.0.1`, `::1`) are scoped to the network namespace rather than filesystem mounts. In `--repo-trust=untrusted` mode (or when `MAKEWAND_SANDBOX_UNSHARE_NET=1` is configured), `--unshare-net` is strictly enforced to isolate the network namespace, completely blocking access to all host abstract sockets and loopback ports.
  **网络命名空间与套接字隔离边界（`--unshare-net`）**：在默认可信模式下，订阅制 CLI 需要访问外部模型 API，因此在宿主网络命名空间中运行。沙箱屏蔽了 `/tmp`、`/var`、`/run`、`$HOME` 下的文件路径套接字，并剔除了 GUI 与 D-Bus 环境变量（`DISPLAY`、`XAUTHORITY`、`WAYLAND_DISPLAY`、`DBUS_*`）。但由于 Linux 内核将抽象 AF_UNIX 套接字（如 `@/tmp/.X11-unix/X0`）和回环服务（`127.0.0.1`）绑定在网络命名空间而非文件系统挂载点，在允许联网时它们保持可达。在不可信仓库模式（`--repo-trust=untrusted` 或设置 `MAKEWAND_SANDBOX_UNSHARE_NET=1`）下，沙箱强制启用 `--unshare-net`，建立全新网络命名空间，彻底阻断对宿主抽象套接字与本地回环服务的连接。
- **Fail-Closed Sandbox Enforcement**: For writable generation and code review tasks, bubblewrap sandbox isolation is mandatory. If `bwrap` is missing or probe fails, makewand fails closed and refuses execution unless `MAKEWAND_UNSAFE_HOST_EXEC=1` is explicitly set AND authenticated via a recorded one-time host acknowledgment. When authorized to fall back to the host, the process environment is strictly sanitized of foreign credentials/tokens and all host executions are audited to `unsafe_exec_audit.jsonl`.
  **失败即停沙箱契约**：对于代码生成与审查任务，Bubblewrap 沙箱物理隔离均为强制约束。若系统无可用 `bwrap` 或探测失败，除非显式设置 `MAKEWAND_UNSAFE_HOST_EXEC=1` 且已完成记录的一次性本机授权确认，否则一律失败即停（fail-closed）拒绝执行。即使经授权降级至宿主执行，也对环境变量执行严格凭据脱敏剥离，并将命令写入 `unsafe_exec_audit.jsonl` 审计日志。

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
- **Timing**: the write-back finishes before the provider launch returns. Concurrent runs of one provider copy the same host file at launch; the later write-back finds the host changed and keeps it (reported), losing its own refresh. A process killed during a run never writes back.
  **时序**：写回在 provider 启动调用返回前完成。同一 provider 的并发运行在启动时复制同一宿主文件，较晚的写回发现宿主已变化就保留宿主版本（会提示），自身的刷新因此丢失。运行中被杀死的进程不会写回。
- **The lock window**: a writer that does not take makewand's lock (the provider CLI on the host, the user, another tool) can still change the host file between the final re-check and the rename; that write is lost. Where `flock` is unavailable, or the lock file cannot be opened (a symlink planted at its path is not followed), the re-check alone applies.
  **锁窗口**：不使用 makewand 锁的写入者（宿主上的 provider CLI、用户或其他工具）仍可能在最后一次复核与重命名之间修改宿主文件，那次写入会丢失。不支持 `flock`、或锁文件无法打开（不跟随放在该路径上的符号链接）的地方只做复核。


### Guidance / 使用建议

- **Your own code**: the trusted default is appropriate. The provider CLI runs within the bubblewrap provider sandbox on your repository. If your host runs sensitive loopback services without authentication (e.g. unauthenticated databases, local Ollama, or X11 servers), ensure they require authentication, run with `--repo-trust=untrusted`, or execute makewand in an isolated container/VM.
  **你自己的代码**：默认的可信模式即可，Provider CLI 在面向该仓库的 bubblewrap 沙箱中运行。若宿主机在本地回环上运行无认证的敏感服务（如无密码数据库、本地 Ollama 或 X11 服务），请为其配置鉴权，或使用 `--repo-trust=untrusted`，或在独立容器/虚拟机中运行 makewand。
- **Untrusted third-party code** (a cloned repo you do not control): pass `--repo-trust=untrusted`. In that mode writable operations are denied and network access inside the sandbox is disabled (`--unshare-net`). A local CLI is allowed only for read-only work inside the sandbox and is refused without bubblewrap. For genuinely hostile code, still run makewand inside a VM, container, or a separate low-privilege user. makewand does **not** claim to safely execute arbitrary hostile third-party code.
  **不可信的第三方代码**（你无法掌控的克隆仓库）：请加 `--repo-trust=untrusted`。该模式下禁止可写操作，沙箱内断网（`--unshare-net`）。只允许本地 CLI 在沙箱内做只读工作，没有 bubblewrap 时直接拒绝。处理真正敌对的代码，仍请在 VM、容器或独立低权限用户下运行。makewand **不**声称能安全执行任意敌对的第三方代码。

### Review verdicts and billing / 审查裁决与费用边界

`makewand review` counts a verdict only when it carries the task's nonce (an
un-nonced verdict line never counts, and no setting re-enables it) and comes from a
reviewer that received the whole prompt with the complete diff: diffs over
`MAKEWAND_REVIEW_DIFF_MAX_BYTES` (256 KiB by default, measured as dispatched) are
never reviewed truncated, agy is never given a review prompt over 31 KiB,
placeholder engines (cursor, copilot) never review, and a read-only local model
answer counts only when its token counts show the whole prompt was processed. A
server that cuts a prompt yet reports at least one token per 6 bytes cannot be
detected. See [reviewer capacity](docs/EXECUTION_CONTRACT.md#reviewer-capacity).
`makewand review` 的裁决只有带本任务 nonce（不带 nonce 的裁决行一律无效，没有任何设置能恢复）、且来自收到完整提示和完整 diff 的审查方时才有效：超过 `MAKEWAND_REVIEW_DIFF_MAX_BYTES`（默认 256 KiB，按实际派发的文本计量）的 diff 从不截断审查，超过 31 KiB 的审查提示从不交给 agy，占位引擎（cursor、copilot）从不审查，本地模型的只读回答只有在 token 计数表明处理了完整提示时才有效；截断提示却仍报告每 6 字节至少 1 个 token 的服务端无法被发现。

A direct `makewand <tool>` dispatch checks no verdict: the caller accepts or rejects
the result and records that with `makewand outcome`.
直接 `makewand <工具>` 派发不检查裁决：由调用方验收，并用 `makewand outcome` 记录。

Direct cloud APIs and API fallback are disabled by default, including in untrusted
repository mode. Explicitly set `MAKEWAND_API_POLICY=allow_paid` or `api_policy` to
`allow_paid` to enable them. Third-party CLIs retain their own billing policies. A
configured key alone neither authorizes a call nor makes an engine usable: under
`subscription_only` the claude, codex, agy, grok and muse adapters never relay a
limited, logged-out or missing CLI to the API (a logged-out CLI still runs; the other
cases fail, naming the policy), and `status --probe` reports an engine whose CLI is
missing and whose key the policy blocks as missing. All five adapters share the same
policy and dispatch-route reporting helper.
环境中存在 API key 本身不再授权 Makewand 发起云 API 调用，也不会让引擎变为可用：`subscription_only` 下，claude、codex、agy、grok、muse 的适配器从不把限额、登录失效或未安装的 CLI 转给 API（登录失效时仍运行 CLI，其余情况直接失败并说明被 API 策略阻止），`status --probe` 把未安装 CLI、key 被策略阻止的引擎报告为 missing。五个适配器共用同一策略与派发路线记录入口。

Windows code remains in the package but no CI gate runs it since v4.0.0, and native
Windows execution has never had the Linux sandbox's file, credential or network isolation.
Windows 代码仍在包内，但 v4.0.0 起没有 CI 门禁运行它；原生 Windows 执行本来也没有 Linux 沙箱的文件、凭据和网络隔离。

## Supported Versions

| Version | Supported | Notes |
| --- | --- | --- |
| `master` (source install) | Yes | Has every protection this document describes |
| `v4.0.x` | Yes | Has every protection this document describes |
| `< v4.0.0` | No | Includes the archived Go server and pipeline, which get no further fixes |

v4.0.x 与 `master` 源码安装包含本文描述的全部保护；更早的版本含已归档的 Go server 和流水线，不再修复。

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
