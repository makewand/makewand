"""
Bubblewrap (bwrap) physical process sandbox bridge for Makewand.

Security model (see SECURITY.md):
  * The host root is mounted read-only, but user data prefixes (/home, /root,
    /mnt, /media, /srv) and host IPC runtimes (/run, /var/run, /var/tmp, /tmp)
    are hidden behind empty tmpfs mounts. Only the workspace, repo_root, a small
    set of toolchain directories and the active provider's own state directory
    are mounted back in.
  * The active provider's state directory stays writable (sessions, OAuth token
    refresh), but every instruction / extension / configuration path inside it
    (CLAUDE.md, commands/, skills/, plugins/, hooks/, settings.json, config.toml,
    AGENTS.md, GEMINI.md, extensions/, grok bin/, ...) is re-mounted read-only so
    a sandboxed model cannot persist behaviour into later host sessions.
  * Live per-session IPC state of host sessions (shell snapshots, session keys,
    daemon control files, runtime sockets) is replaced by an empty tmpfs.
  * Pre-existing pathname AF_UNIX sockets under mounted trees are masked.
  * Host execution without bubblewrap requires MAKEWAND_UNSAFE_HOST_EXEC=1 plus
    the same one-time, host-bound acknowledgment the Go engine records, and is
    audited to unsafe_exec_audit.jsonl for every execution.

Known boundaries (not enforced here, documented in SECURITY.md): with network
allowed the sandbox shares the host network namespace, so abstract AF_UNIX
sockets and loopback services stay reachable; sockets created after the sandbox
starts inside a writable workspace are not masked.
"""

import json
import logging
import os
import platform
import signal
import socket
import stat
import struct
import sys
import shutil
import tempfile
import threading
import time
from collections import deque
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Dict, Iterable, List, Optional, Sequence, Set, Tuple, Union
import atexit
import contextlib
import errno
import hashlib
import re
from contextlib import contextmanager

try:
    import fcntl
except ImportError:  # Windows: no bubblewrap, so no private provider homes
    fcntl = None

from makewand.providers.base import run_subprocess


class SandboxConfigError(RuntimeError):
    """A safe bubblewrap command line cannot be constructed; callers must fail closed."""


class ProviderStartupError(SandboxConfigError):
    """Trusted prelaunch evidence: the provider has not been started."""

    def as_result(self, engine, readonly):
        from makewand.execution_contract import ExecutionResult
        return ExecutionResult(False, None, str(self), status="UNVERIFIED",
                               engine=engine, readonly=readonly,
                               error_kind="startup_environment", outcome_known=True)


# System and generic root directories that must never be bound writable in sandbox (VUL-01/02/06)
FORBIDDEN_WRITABLE_ROOTS = (
    "/", "/home", "/etc", "/usr", "/var", "/tmp", "/root", "/boot", "/opt", "/sys", "/dev", "/proc"
)

# Host prefixes that hold user data (other projects, other users' homes, data
# disks). They are hidden behind an empty tmpfs; required paths are re-bound.
MASKED_HOST_ROOTS = ("/home", "/root", "/mnt", "/media", "/srv")

# Host IPC runtime directories hidden behind tmpfs (S05).
SOCKET_RUNTIME_DIRS = ("/run", "/var/tmp", "/var/run")

# Credential / secret paths below $HOME. HOME is normally an empty tmpfs inside
# the sandbox, so these only matter when a workspace or repo_root mount makes
# HOME itself visible again (e.g. a read-only review whose workspace is HOME):
# they are then re-masked after the workspace mount.
SENSITIVE_HOME_DIRS = [
    ".ssh",
    ".aws",
    ".gnupg",
    ".config/gcloud",
    ".azure",
    ".kube",
    ".claude",
    ".claude-2",
    ".claude-3",
    ".claude.json",
    ".codex",
    ".codex-2",
    ".codex-3",
    ".gemini",
    ".anthropic",
    ".openai",
    ".config/gh",
    ".config/hub",
    ".config/git/credentials",
    ".config/makewand",
    ".config/rclone",
    ".gitconfig",
    ".git-credentials",
    ".cargo/credentials.toml",
    ".cargo/credentials",
    ".cargo/config.toml",
    ".npmrc",
    ".yarnrc",
    ".pypirc",
    ".netrc",
    ".docker",
    ".config/muse",
    ".local/share/muse",
    ".local/share/keyrings",
    ".password-store",
    ".grok",
    ".aider",
    ".aider.conf.yml",
    ".vault-token",
    ".terraform.d",
    ".s3cfg",
    ".boto",
    ".oci",
    ".m2/settings.xml",
    ".gradle/gradle.properties",
    ".pgpass",
    ".agy",
    ".antigravity",
    ".bash_history",
    ".zsh_history",
    ".histfile",
    ".python_history",
    ".node_repl_history",
    ".psql_history",
    ".mysql_history",
    ".sqlite_history",
    ".lesshst",
    ".viminfo",
    ".local/share/credentials",
    ".local/share/makewand",
    ".local/share/fish",
    ".local/state",
    "server_auth.json",
    "state.db",
    "state.db-wal",
    "state.db-shm",
    "admin_session_secret",
    "users.json",
    "audit.jsonl",
    "usage.jsonl",
    ".mozilla",
    ".config/google-chrome",
    ".config/chromium",
    ".pki",
]

# Toolchain directories below $HOME that are safe to expose read-only (never
# parents of credential files).
SAFE_HOME_BIN_DIRS = [
    ".cargo/bin",
    ".local/bin",
    ".nvm",
    ".pyenv/shims",
    ".pyenv/versions",
    ".rustup/toolchains",
    ".nix-profile/bin",
]

# ---------------------------------------------------------------------------
# Provider state-directory policy (per-run private provider home, H3).
#
# The host provider state directory (~/.claude, ~/.codex or $CODEX_HOME,
# ~/.gemini -> its real path, ~/.grok, ~/.config/muse, ~/.local/share/muse,
# ~/.aider) is NEVER bound writable. Every sandboxed provider run gets a fresh
# private directory (0700, below $XDG_RUNTIME_DIR or the user cache dir) that is
# bound writable at the provider's expected path. Into it we copy only:
#   "credentials"  auth/token files; after the run a changed copy is merged
#                  into the host file (regular file only, no symlinks, atomic
#                  replace under makewand's credential lock, skipped when the
#                  host copy changed concurrently, re-checked right before the
#                  rename; only the token-refresh fields of a same-account
#                  refresh, see _credential_writeback_merge and
#                  _atomic_write_host)
#   "seed"         harmless caches/ids the CLI expects; never written back
# A root missing on the host that holds credential files gets an empty private
# directory at its path inside the sandbox (nothing is created on the host), so
# a login made in the sandbox is reported after the run instead of vanishing;
# it is never written back.
# "protected" entries (root, relative path, kind, placeholder) — instruction,
# extension, configuration and executable paths — are read-only binds of the
# host originals (a symlinked entry binds its resolved target). Missing
# protected paths get no mount: anything the model creates there lands in the
# private directory and is deleted after the run, so nothing is ever created on
# or removed from the host. (placeholder is only used by the legacy
# sandbox_lifecycle helper.)
# Session transcripts, history, sockets and daemon state of host sessions are
# simply absent from the private directory.
# ---------------------------------------------------------------------------
PROVIDER_PROFILES: Dict[str, dict] = {
    "claude": {
        "roots": [".claude"],
        "home_ro_files": [".claude.json"],
        "credentials": [(".claude", ".credentials.json")],
        "seed": [],
        "protected": [
            (".claude", "CLAUDE.md", "file", ""),
            (".claude", "settings.json", "file", "{}\n"),
            (".claude", "settings.local.json", "file", "{}\n"),
            (".claude", "commands", "dir", True),
            (".claude", "agents", "dir", True),
            (".claude", "skills", "dir", True),
            (".claude", "plugins", "dir", True),
            (".claude", "hooks", "dir", True),
            (".claude", "output-styles", "dir", True),
            (".claude", "rules", "dir", True),
            (".claude", "workflows", "dir", True),
            (".claude", "themes", "dir", None),
            (".claude", "keybindings.json", "file", None),
            (".claude", "remote-settings.json", "file", None),
            (".claude", "policy-limits.json", "file", None),
            (".claude", "chrome", "dir", True),
            # npm "local" installation of the claude binary
            (".claude", "local", "dir", None),
        ],
        # auto-memory: ~/.claude/projects/<key>/memory/*.md (read-only for the
        # current project)
        "project_memory_root": (".claude", "projects"),
    },
    "codex": {
        "roots": [".codex"],
        "credentials": [(".codex", "auth.json")],
        "seed": [
            (".codex", "version.json"),
            (".codex", "models_cache.json"),
            (".codex", "installation_id"),
        ],
        "protected": [
            (".codex", "config.toml", "file", ""),
            (".codex", "AGENTS.md", "file", ""),
            (".codex", "prompts", "dir", True),
            (".codex", "skills", "dir", True),
            (".codex", "rules", "dir", True),
            (".codex", "hooks", "dir", True),
            (".codex", "AGENTS.override.md", "file", None),
            (".codex", "hooks.json", "file", None),
            (".codex", "policy", "dir", None),
            (".codex", "plugins", "dir", None),
            (".codex", "packages", "dir", None),
            (".codex", "memories", "dir", None),
        ],
    },
    "agy": {
        "roots": [".gemini"],
        "credentials": [
            (".gemini", "oauth_creds.json"),
            (".gemini", "google_accounts.json"),
            (".gemini", "antigravity-cli/antigravity-oauth-token"),
        ],
        "seed": [
            # A browser cookie jar (a JSON array of name/value/domain/path/
            # expires/... cookies): its values are opaque session cookies that
            # cannot be tied to the host account, and no provider CLI refreshes
            # it, so it is copied in only.
            (".gemini", "flow_auth.json"),
            (".gemini", "installation_id"),
            (".gemini", "antigravity/installation_id"),
            (".gemini", "antigravity-cli/installation_id"),
        ],
        "protected": [
            (".gemini", "settings.json", "file", "{}\n"),
            (".gemini", "GEMINI.md", "file", ""),
            (".gemini", "commands", "dir", True),
            (".gemini", "extensions", "dir", True),
            (".gemini", "trustedFolders.json", "file", None),
            (".gemini", "policies", "dir", None),
            (".gemini", "skills", "dir", None),
            (".gemini", "config", "dir", None),
            (".gemini", "antigravity/mcp_config.json", "file", None),
            (".gemini", "antigravity/browserAllowlist.txt", "file", None),
            (".gemini", "antigravity/user_settings.pb", "file", None),
            (".gemini", "antigravity-cli/bin", "dir", None),
            (".gemini", "antigravity-cli/builtin", "dir", None),
            (".gemini", "antigravity-cli/updater", "dir", None),
            (".gemini", "antigravity-cli/knowledge", "dir", None),
            (".gemini", "antigravity-cli/hooks.json", "file", None),
            (".gemini", "antigravity-cli/settings.json", "file", None),
            (".gemini", "antigravity-cli/mcp_config.json", "file", None),
        ],
    },
    "grok": {
        "roots": [".grok"],
        "credentials": [(".grok", "auth.json")],
        "seed": [
            (".grok", "agent_id"),
            (".grok", "version.json"),
            (".grok", "models_cache.json"),
            (".grok", ".metadata_version"),
        ],
        "protected": [
            (".grok", "config.toml", "file", ""),
            # bin/ must stay read-only: replacing ~/.grok/bin/grok = host RCE
            (".grok", "bin", "dir", None),
            (".grok", "hooks", "dir", True),
            (".grok", "hooks-paths", "file", None),
            (".grok", "installed-plugins", "dir", None),
            (".grok", "bundled", "dir", None),
            (".grok", "vendor", "dir", None),
            (".grok", "completions", "dir", None),
            (".grok", "marketplace-cache", "dir", None),
            (".grok", "memory-v2", "dir", None),
            (".grok", "sandbox.toml", "file", None),
            (".grok", "trusted_folders.toml", "file", None),
        ],
    },
    "muse": {
        "roots": [".config/muse", ".local/share/muse"],
        "credentials": [(".config/muse", "auth.json")],
        # muse mints the api_key of its auth.json itself from the OAuth login
        # (its "startup mint" and the "401 re-mint lane"), so a changed api_key
        # of a record in that login shape is a re-mint that is written back
        # (_credential_writeback_merge, _cred_mint_shape); a key saved with
        # `muse auth set --api-key-stdin` or pasted stays protected.
        "minted_keys": {(".config/muse", "auth.json"): ("api_key",)},
        "seed": [],
        "protected": [
            (".config/muse", "settings.json", "file", None),
            (".config/muse", "env", "file", None),
            (".config/muse", "trust.json", "file", None),
            (".config/muse", "approval-policy.json", "file", None),
            (".local/share/muse", "plugins", "dir", None),
            (".local/share/muse", "skills", "dir", None),
            (".local/share/muse", "feature-config", "dir", None),
        ],
    },
    "aider": {
        "roots": [".aider"],
        "home_ro_files": [".aider.conf.yml"],
        "credentials": [],
        "seed": [
            (".aider", "analytics.json"),
            (".aider", "installs.json"),
        ],
        "protected": [],
    },
}

# Provider-specific toolchain directories below $HOME (read-only).
PROVIDER_HOME_BIN_DIRS = {
    "claude": [".local/share/claude"],
    "muse": [".local/libexec"],
}

# Bounds for build-time tree scans (eng-delivery#7). The workspace socket scan
# walks everything (node_modules, .git internals included, H5); a truncated scan
# fails closed for writable sessions and masks unscanned subtrees with a
# read-only tmpfs in read-only sessions.
SCAN_ENTRY_LIMIT = 1_000_000  # MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT overrides
READONLY_SCAN_ENTRY_LIMIT = SCAN_ENTRY_LIMIT  # MAKEWAND_SANDBOX_READONLY_SCAN_ENTRY_LIMIT overrides
READONLY_SCAN_TIMEOUT_SECONDS = 15.0  # MAKEWAND_SANDBOX_SCAN_TIMEOUT_SECONDS overrides
SANDBOX_MASKED_CACHE_DIRS: frozenset = frozenset({"roi-image-cache"})
SCAN_DEFERRED_BULK_DIRS: frozenset = frozenset({
    "node_modules", ".venv", "venv", ".cache", ".worktrees",
})
MAX_UNSCANNED_DIR_MASKS = 512
BROAD_SCAN_DEPTH = 3
PROVIDER_SCAN_DEPTH = 3
PROVIDER_SCAN_ENTRY_LIMIT = 20_000
PROJECT_MEMORY_LIMIT = 2_000
SCAN_SKIP_DIRS: frozenset = frozenset()
HOST_VAR_ROOT = "/var"
HOST_VAR_SCAN_DEPTH = 8
HOST_VAR_SCAN_ENTRY_LIMIT = 100_000
HOST_VAR_SCAN_TTL = 30.0
_host_var_cache: Dict[str, object] = {"at": 0.0, "masks": None}

# Claude project key length after which Claude Code switches to a hashed key.
_CLAUDE_PROJECT_KEY_MAX = 200


def generate_seccomp_bpf_filter() -> Optional[bytes]:
    """
    Constructs a strict seccomp-bpf filter program blocking high-risk Linux syscalls
    (ptrace, bpf, keyctl, process_vm_readv, process_vm_writev, userfaultfd).
    Returns raw compiled BPF bytecode matching the host machine architecture,
    or None if architecture is unsupported or BPF generation fails.
    """
    try:
        BPF_LD, BPF_W, BPF_ABS = 0x00, 0x00, 0x20
        BPF_JMP, BPF_JEQ, BPF_K = 0x05, 0x10, 0x00
        BPF_RET = 0x06
        SECCOMP_RET_ERRNO = 0x00050000
        SECCOMP_RET_ALLOW = 0x7fff0000
        EPERM = 1

        AUDIT_ARCH_X86_64 = 0xc000003e
        AUDIT_ARCH_AARCH64 = 0xc00000b7

        machine = platform.machine().lower()
        if machine in ("x86_64", "amd64"):
            expected_arch = AUDIT_ARCH_X86_64
            blocked_syscalls = [
                101,  # ptrace
                321,  # bpf
                250,  # keyctl
                310,  # process_vm_readv
                311,  # process_vm_writev
                323,  # userfaultfd
            ]
        elif machine in ("aarch64", "arm64"):
            expected_arch = AUDIT_ARCH_AARCH64
            blocked_syscalls = [
                117,  # ptrace
                280,  # bpf
                219,  # keyctl
                270,  # process_vm_readv
                271,  # process_vm_writev
                282,  # userfaultfd
            ]
        else:
            return None

        instructions = []

        def stmt(code: int, k: int):
            instructions.append(struct.pack("<HBBI", code, 0, 0, k))

        def jmp(code: int, k: int, jt: int, jf: int):
            instructions.append(struct.pack("<HBBI", code, jt, jf, k))

        # 1. Load architecture: [BPF_LD | BPF_W | BPF_ABS, offset 4]
        stmt(BPF_LD | BPF_W | BPF_ABS, 4)
        # 2. Check architecture matches expected_arch, otherwise jump to errno
        jmp(BPF_JMP | BPF_JEQ | BPF_K, expected_arch, 1, 0)
        stmt(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM)

        # 3. Load syscall number: [BPF_LD | BPF_W | BPF_ABS, offset 0]
        stmt(BPF_LD | BPF_W | BPF_ABS, 0)

        n = len(blocked_syscalls)
        # Block X32 ABI syscalls (nr >= 0x40000000) on x86_64 to prevent seccomp filter bypass
        if machine in ("x86_64", "amd64"):
            BPF_JGE = 0x30
            X32_SYSCALL_BIT = 0x40000000
            jmp(BPF_JMP | BPF_JGE | BPF_K, X32_SYSCALL_BIT, n + 1, 0)

        # 4. Filter blocked syscalls: jump to errno block if matched
        for i, sc in enumerate(blocked_syscalls):
            remaining = n - 1 - i
            jmp(BPF_JMP | BPF_JEQ | BPF_K, sc, remaining + 1, 0)

        # 5. Default allow
        stmt(BPF_RET | BPF_K, SECCOMP_RET_ALLOW)
        # 6. Blocked errno (EPERM = 1)
        stmt(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM)

        return b"".join(instructions)
    except Exception:
        return None


SECCOMP_REQUIRE_ENV = "MAKEWAND_REQUIRE_SECCOMP"
SECCOMP_OPT_OUT_ENV = "MAKEWAND_ALLOW_NO_SECCOMP"
_seccomp_warned: Dict[str, bool] = {"opt_out": False}


@contextlib.contextmanager
def seccomp_filter_context(enabled: bool = True, required: bool = False):
    """
    Context manager that prepares a seccomp BPF filter pipe if enabled.
    Yields (seccomp_fd, pass_fds) tuple for passing into wrap_bwrap and run_subprocess.
    Guarantees the read descriptor is closed after process launch.

    enabled=True means a bwrap launch is being prepared: the filter is then
    mandatory (supported: Linux x86_64/aarch64) and any failure raises
    SandboxConfigError. MAKEWAND_ALLOW_NO_SECCOMP=1 is the only opt-out (warned,
    the filter is skipped). required=True / MAKEWAND_REQUIRE_SECCOMP=1 also
    refuse enabled=False and override the opt-out.
    """
    explicit = required or (os.environ.get(SECCOMP_REQUIRE_ENV) == "1")
    if not enabled:
        if explicit:
            raise SandboxConfigError("Seccomp-BPF is strictly required but was disabled")
        yield None, ()
        return
    if not explicit and os.environ.get(SECCOMP_OPT_OUT_ENV) == "1":
        if not _seccomp_warned["opt_out"]:
            _seccomp_warned["opt_out"] = True
            _warn(f"警告：{SECCOMP_OPT_OUT_ENV}=1——沙箱进程将不加载 seccomp 过滤器"
                  "（ptrace/bpf/keyctl/process_vm_*/userfaultfd 未被拦截）。")
        yield None, ()
        return
    bpf = generate_seccomp_bpf_filter()
    if not bpf:
        raise SandboxConfigError(
            f"无法为当前架构 ({platform.system()} {platform.machine()}) 生成 Seccomp-BPF 过滤器，"
            f"拒绝启动沙箱 (fail closed)。仅支持 Linux x86_64/aarch64；"
            f"如确需在无 seccomp 的情况下运行，请显式设置 {SECCOMP_OPT_OUT_ENV}=1。"
        )
    r, w = None, None
    try:
        r, w = os.pipe()
        os.write(w, bpf)
        os.close(w)
        w = None
    except Exception as exc:
        if w is not None:
            try:
                os.close(w)
            except Exception:
                pass
        if r is not None:
            try:
                os.close(r)
            except Exception:
                pass
        raise SandboxConfigError(
            f"Seccomp-BPF pipe setup failed: {exc} (fail closed; opt out with {SECCOMP_OPT_OUT_ENV}=1)"
        ) from exc

    try:
        yield r, (r,)
    finally:
        try:
            os.close(r)
        except Exception:
            pass


# bwrap is resolved only from fixed system directories, never $PATH: a
# repository-controlled PATH entry (activated .venv, node_modules/.bin, direnv)
# could otherwise shadow it and run on the host before confinement exists.
# Mirrors internal/trustedbin on the Go side.
TRUSTED_BWRAP_DIRS = (
    "/usr/bin",
    "/bin",
    "/usr/local/bin",
    "/usr/sbin",
    "/sbin",
    "/run/current-system/sw/bin",  # NixOS system profile (symlinks into /nix/store)
)
BWRAP_OVERRIDE_ENV = "MAKEWAND_BWRAP_PATH"
# Marks provider CLI children; makewand/cli.py refuses model dispatch under it.
PROVIDER_WORKER_ENV = "MAKEWAND_PROVIDER_WORKER"


def _trusted_launcher_ok(path: str, allow_current_user: bool) -> bool:
    try:
        st = os.stat(path)
    except OSError:
        return False
    if stat.S_IMODE(st.st_mode) & 0o022:
        return False
    if st.st_uid == 0:
        return True
    # Inside a user namespace (nested bwrap, containers, sandboxed agent shells)
    # root-owned host files appear as the kernel overflow uid; such unmapped
    # owners are equally out of the current user's reach.
    if st.st_uid == _overflow_uid() and st.st_uid != os.geteuid() and _in_user_namespace():
        return True
    return allow_current_user and st.st_uid == os.geteuid()


def _in_user_namespace() -> bool:
    """False for the initial namespace, whose uid_map is the identity map."""
    try:
        with open("/proc/self/uid_map", encoding="ascii") as fh:
            return fh.read().split() != ["0", "0", "4294967295"]
    except OSError:
        return False


def _overflow_uid() -> int:
    try:
        with open("/proc/sys/kernel/overflowuid", encoding="ascii") as fh:
            return int(fh.read().strip())
    except (OSError, ValueError):
        return 65534


def resolve_trusted_bwrap() -> Optional[str]:
    """Return the resolved path of a trusted bwrap, or None.

    Accepts only root-owned, non-group/world-writable files in root-owned,
    non-group/world-writable directories. MAKEWAND_BWRAP_PATH may name an
    absolute path also owned by the current user. The returned path is
    symlink-free so the probe and the real launch execute the same file.
    """
    if not hasattr(os, "geteuid"):
        return None
    override = os.environ.get(BWRAP_OVERRIDE_ENV, "").strip()
    if override:
        candidates, allow_user = [override] if os.path.isabs(override) else [], True
    else:
        candidates, allow_user = [os.path.join(d, "bwrap") for d in TRUSTED_BWRAP_DIRS], False
    for candidate in candidates:
        if not os.path.lexists(candidate):
            continue
        real = os.path.realpath(candidate)
        if not os.path.isfile(real) or not os.access(real, os.X_OK):
            continue
        if _trusted_launcher_ok(real, allow_user) and _trusted_launcher_ok(os.path.dirname(real), allow_user):
            return real
    return None


def is_bwrap_available() -> bool:
    bwrap = resolve_trusted_bwrap()
    if not bwrap:
        return False
    # Quick self-test
    ret, _, _, _ = run_subprocess([bwrap, "--ro-bind", "/", "/", "true"], timeout=3)
    return ret == 0


# ---------------------------------------------------------------------------
# path helpers
# ---------------------------------------------------------------------------
def _norm(p: str) -> str:
    return os.path.normpath(os.path.abspath(p))


def _is_within(path: str, root: str) -> bool:
    """True when path == root or path is below root (both normalized, no symlink resolution)."""
    path = _norm(path)
    root = _norm(root)
    if root == "/":
        return True
    return path == root or path.startswith(root + os.sep)


def _effective_masked_roots() -> List[str]:
    roots: List[str] = []
    for r in MASKED_HOST_ROOTS:
        try:
            if not os.path.isdir(r):
                continue
            real = os.path.realpath(r)
            if real == "/" or not os.path.isdir(real):
                continue
            if real not in roots:
                roots.append(real)
            if r not in roots and not os.path.islink(r):
                roots.append(r)
        except OSError:
            continue
    return roots


def _hidden_on_host_view(path: str, user_home: str, masked_roots: Sequence[str]) -> bool:
    """True when path is hidden by the HOME tmpfs or one of the masked host roots."""
    return _is_within(path, user_home) or any(_is_within(path, r) for r in masked_roots)


def _broad_workspace(path: str, user_home: str) -> bool:
    """/, /tmp, /var/tmp, HOME itself or an ancestor of HOME (e.g. /home)."""
    p = _norm(path)
    if p in ("/", "/tmp", "/var/tmp"):
        return True
    try:
        rp = os.path.realpath(p)
        if rp in ("/", os.path.realpath("/tmp"), os.path.realpath("/var/tmp")):
            return True
    except OSError:
        pass
    return _is_within(user_home, p)


def _home_exposed(path: str, user_home: str) -> bool:
    """True when mounting `path` makes HOME itself visible (path is HOME or an ancestor)."""
    return _is_within(user_home, path)


def _claude_project_key(path: str) -> Optional[str]:
    key = "".join(ch if (ch.isascii() and ch.isalnum()) else "-" for ch in _norm(path))
    if len(key) > _CLAUDE_PROJECT_KEY_MAX:
        return None
    return key


def _warn(msg: str) -> None:
    try:
        print(f"[Makewand Sandbox] {msg}", file=sys.stderr)
    except Exception:
        pass


# ---------------------------------------------------------------------------
# bounded scans
# ---------------------------------------------------------------------------
def _untraversable_foreign_dir(path: str) -> bool:
    """A directory another user owns that the current user cannot enter."""
    try:
        st = os.lstat(path)
    except OSError:
        return False
    if not stat.S_ISDIR(st.st_mode) or not hasattr(os, "geteuid") or st.st_uid == os.geteuid():
        return False
    return not os.access(path, os.X_OK)


def _workspace_scan_limit(readonly: bool = False) -> int:
    raw = os.environ.get("MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT", "").strip()
    if raw.isdigit() and int(raw) > 0:
        return int(raw)
    if readonly:
        ro_raw = os.environ.get("MAKEWAND_SANDBOX_READONLY_SCAN_ENTRY_LIMIT", "").strip()
        if ro_raw.isdigit() and int(ro_raw) > 0:
            return min(SCAN_ENTRY_LIMIT, int(ro_raw))
        if READONLY_SCAN_ENTRY_LIMIT != 1_000_000:
            return min(SCAN_ENTRY_LIMIT, READONLY_SCAN_ENTRY_LIMIT)
    return SCAN_ENTRY_LIMIT


def _workspace_scan_timeout(readonly: bool = False) -> Optional[float]:
    raw = os.environ.get("MAKEWAND_SANDBOX_SCAN_TIMEOUT_SECONDS", "").strip()
    if raw:
        try:
            val = float(raw)
            return val if val > 0 else None
        except ValueError:
            pass
    if readonly:
        return READONLY_SCAN_TIMEOUT_SECONDS
    return None


def _workspace_masked_cache_dirs() -> frozenset:
    raw = os.environ.get("MAKEWAND_SANDBOX_MASKED_DIRS", "").strip()
    if not raw:
        return SANDBOX_MASKED_CACHE_DIRS
    extra = {part.strip() for part in raw.split(",") if part.strip() and os.sep not in part}
    return SANDBOX_MASKED_CACHE_DIRS | frozenset(extra)


def _norm_is_within(path: str, root: str) -> bool:
    """Fast _is_within when both path and root are already _norm()-ed."""
    if root == "/":
        return True
    return path == root or path.startswith(root + os.sep)


def _covered_by_norm_set(path: str, roots: Set[str]) -> bool:
    if not roots:
        return False
    cur = path
    while cur and cur != "/":
        if cur in roots:
            return True
        nxt = os.path.dirname(cur)
        if nxt == cur:
            break
        cur = nxt
    return "/" in roots


def _dedup_norm_ancestors(paths: Set[str]) -> Set[str]:
    kept: Set[str] = set()
    for d in paths:
        cur = os.path.dirname(d)
        has_anc = False
        while cur and cur != "/" and cur != d:
            if cur in paths:
                has_anc = True
                break
            nxt = os.path.dirname(cur)
            if nxt == cur:
                break
            cur = nxt
        if not has_anc:
            kept.add(d)
    return kept


def _collapse_unscanned_dirs(
    unscanned_dirs: Set[str],
    protected_paths: Sequence[str],
    max_masks: int = MAX_UNSCANNED_DIR_MASKS,
) -> Optional[List[str]]:
    """
    Normalizes and bounds a set of unscanned directory paths so each can be
    masked with a read-only tmpfs. Returns None if any unscanned directory
    contains a protected path (e.g. the workspace root itself) or cannot be
    collapsed within max_masks.
    """
    if not unscanned_dirs:
        return []
    prot = [_norm(p) for p in protected_paths if p and p != "/"]
    normed = {_norm(d) for d in unscanned_dirs if d}
    if any(d == "/" or any(_norm_is_within(p, d) for p in prot) for d in normed):
        return None
    kept = _dedup_norm_ancestors(normed)
    while len(kept) > max_masks:
        by_parent: Dict[str, List[str]] = {}
        for d in kept:
            parent = os.path.dirname(d)
            if parent in (d, "/", "") or any(_norm_is_within(p, parent) for p in prot):
                continue
            by_parent.setdefault(parent, []).append(d)
        if not by_parent:
            return None
        for parent, children in sorted(
            by_parent.items(),
            key=lambda kv: (kv[0].count(os.sep), len(kv[1]), kv[0]),
            reverse=True,
        ):
            for c in children:
                kept.discard(c)
            kept.add(parent)
            if len(kept) <= max_masks:
                break
        kept = _dedup_norm_ancestors(kept)
    return sorted(kept)


def _scan_tree(
    roots: Iterable[str],
    *,
    max_depth: Optional[int],
    entry_limit: int,
    collect_git: bool,
    skip_dirs: frozenset = SCAN_SKIP_DIRS,
    skip_paths: Iterable[str] = (),
    descend_git: bool = False,
    timeout_seconds: Optional[float] = None,
    mask_dirs: frozenset = frozenset(),
    defer_dirs: frozenset = SCAN_DEFERRED_BULK_DIRS,
    priority_paths: Iterable[str] = (),
    unbounded_paths: Iterable[str] = (),
    unscanned_dirs: Optional[Set[str]] = None,
    masked_cache_dirs: Optional[Set[str]] = None,
) -> Tuple[Set[str], Set[str], bool]:
    """
    Iteratively scans roots in breadth-first order without following symlinks.
    Returns (git_entries, socket_paths, truncated). `.git` entries are recorded;
    a `.git` directory is descended into (for sockets only, e.g. the fsmonitor
    daemon socket) when descend_git is set; directories named in skip_dirs are
    skipped; directories named in mask_dirs are recorded in masked_cache_dirs
    for read-only tmpfs masking; directories in defer_dirs are queued behind
    primary workspace directories; any subdirectories not entered due to
    max_depth, entry_limit, timeout_seconds, or OSError are recorded in
    unscanned_dirs.
    """
    git_paths: Set[str] = set()
    sockets: Set[str] = set()
    skip_abs = {_norm(p) for p in skip_paths}
    priority_abs = [_norm(p) for p in priority_paths if p]
    unbounded_abs = [_norm(p) for p in unbounded_paths if p]
    seen = 0
    truncated = False   # entry or time budget exhausted: stop walking non-spine subtrees
    incomplete = False  # an unreadable/errored directory was encountered
    deadline: Optional[float] = (
        time.monotonic() + timeout_seconds
        if timeout_seconds is not None and timeout_seconds > 0
        else None
    )
    dir_hard_limit = max(entry_limit * 10, 1_000_000)
    # (path, depth, inside_git, in_bulk): .git entries below a .git directory are not recorded
    queue = deque((_norm(r), 0, False, False) for r in roots if r)
    bulk_queue: deque = deque()
    while queue or bulk_queue:
        if seen > 0 and deadline is not None and time.monotonic() >= deadline:
            truncated = True
        cur, depth, in_git, in_bulk = queue.popleft() if queue else bulk_queue.popleft()
        cur_on_spine = any(_norm_is_within(p, cur) for p in priority_abs)
        if truncated and not cur_on_spine:
            if unscanned_dirs is not None:
                unscanned_dirs.add(cur)
            continue
        try:
            it = os.scandir(cur)
        except PermissionError:
            # Another user's directory we cannot enter is out of the sandbox's
            # reach too; one we own could be chmod-ed back from inside.
            if not _untraversable_foreign_dir(cur):
                incomplete = True
                if unscanned_dirs is not None:
                    unscanned_dirs.add(cur)
            continue
        except OSError:
            incomplete = True
            if unscanned_dirs is not None:
                unscanned_dirs.add(cur)
            continue
        cur_dirs: List[Tuple[str, int, bool, bool]] = []
        cur_failed = False
        cur_aborted = False
        dir_seen = 0
        try:
            with it:
                for entry in it:
                    seen += 1
                    dir_seen += 1
                    if not truncated and (
                        seen > entry_limit
                        or (deadline is not None and (seen & 0x3F) == 0 and time.monotonic() >= deadline)
                    ):
                        truncated = True
                        if not cur_on_spine:
                            cur_aborted = True
                            break
                    if dir_seen > dir_hard_limit:
                        incomplete = True
                        cur_failed = True
                        break
                    name = entry.name
                    path = entry.path
                    is_git = name == ".git" and not in_git
                    if is_git and collect_git:
                        git_paths.add(path)
                    if is_git and not descend_git:
                        continue
                    try:
                        if entry.is_dir(follow_symlinks=False):
                            if name in skip_dirs or path in skip_abs:
                                continue
                            on_path_spine = any(_norm_is_within(p, path) for p in priority_abs)
                            in_unbounded = any(_norm_is_within(path, u) for u in unbounded_abs)
                            if (
                                not in_git
                                and not is_git
                                and name in mask_dirs
                                and masked_cache_dirs is not None
                                and not on_path_spine
                            ):
                                masked_cache_dirs.add(path)
                                continue
                            if (
                                max_depth is None
                                or depth + 1 < max_depth
                                or on_path_spine
                                or in_unbounded
                            ):
                                in_priority_ws = not priority_abs or on_path_spine or any(
                                    _norm_is_within(path, p) for p in priority_abs
                                )
                                is_bulk = in_bulk or (not in_priority_ws) or (
                                    name in defer_dirs and not on_path_spine
                                )
                                cur_dirs.append((path, depth + 1, in_git or is_git, is_bulk))
                            elif unscanned_dirs is not None:
                                unscanned_dirs.add(path)
                            continue
                        if entry.is_file(follow_symlinks=False) or entry.is_symlink():
                            continue
                        st = entry.stat(follow_symlinks=False)
                        if stat.S_ISSOCK(st.st_mode):
                            sockets.add(path)
                    except OSError:
                        incomplete = True
                        cur_failed = True
                        continue
        except OSError:
            incomplete = True
            cur_failed = True
        if cur_failed or cur_aborted:
            if unscanned_dirs is not None:
                unscanned_dirs.add(cur)
            if truncated:
                kept_spine = deque()
                for q in (queue, bulk_queue):
                    while q:
                        q_path, q_depth, q_git, q_bulk = q.popleft()
                        if any(_norm_is_within(p, q_path) for p in priority_abs):
                            kept_spine.append((q_path, q_depth, q_git, q_bulk))
                        elif unscanned_dirs is not None:
                            unscanned_dirs.add(q_path)
                queue = kept_spine
                bulk_queue.clear()
            continue
        for d_path, d_depth, d_git, d_bulk in cur_dirs:
            on_spine = any(_norm_is_within(p, d_path) for p in priority_abs)
            if truncated and not on_spine:
                if unscanned_dirs is not None:
                    unscanned_dirs.add(d_path)
            elif on_spine:
                queue.appendleft((d_path, d_depth, d_git, False))
            elif d_bulk:
                bulk_queue.append((d_path, d_depth, d_git, True))
            else:
                queue.append((d_path, d_depth, d_git, False))
        if truncated:
            kept_spine: deque = deque()
            for q in (queue, bulk_queue):
                while q:
                    q_path, q_depth, q_git, q_bulk = q.popleft()
                    if any(_norm_is_within(p, q_path) for p in priority_abs):
                        kept_spine.append((q_path, q_depth, q_git, q_bulk))
                    elif unscanned_dirs is not None:
                        unscanned_dirs.add(q_path)
            queue = kept_spine
            bulk_queue.clear()
    return git_paths, sockets, truncated or incomplete


def _host_var_socket_masks() -> List[str]:
    """
    Paths below /var that hide pathname AF_UNIX sockets (libvirt/qemu monitors,
    snap daemons, postfix, ...). /var stays visible through the read-only root
    and a read-only mount does not stop connect(), so the stable
    /var/<a>/<b> ancestor of each deeper socket is hidden behind a tmpfs (a
    socket directly at /var/<a>/<sock> is covered by /dev/null). Cached for a
    short TTL; bounded by depth and entry count.
    """
    now = time.monotonic()
    cached = _host_var_cache.get("masks")
    if cached is not None and now - float(_host_var_cache.get("at") or 0.0) < HOST_VAR_SCAN_TTL:
        return list(cached)  # type: ignore[arg-type]
    masks: Set[str] = set()
    var_root = _norm(HOST_VAR_ROOT)
    if os.path.isdir(var_root) and not os.path.islink(var_root):
        skip = [os.path.join(var_root, d) for d in ("tmp", "run", "lock")]
        _, socks, _ = _scan_tree(
            [var_root], max_depth=HOST_VAR_SCAN_DEPTH, entry_limit=HOST_VAR_SCAN_ENTRY_LIMIT,
            collect_git=False, skip_dirs=frozenset(), skip_paths=skip,
        )
        for s in socks:
            rel = os.path.relpath(s, var_root).split(os.sep)
            if len(rel) > 2:
                anc = os.path.join(var_root, rel[0], rel[1])
                if os.path.isdir(anc) and not os.path.islink(anc):
                    masks.add(anc)
                    continue
            masks.add(s)
    # drop masks nested below another mask
    result = sorted(m for m in masks if not any(m != o and _is_within(m, o) for o in masks))
    _host_var_cache["masks"] = result
    _host_var_cache["at"] = now
    return list(result)


# ---------------------------------------------------------------------------
# provider state mounts
# ---------------------------------------------------------------------------
def _create_placeholder(path: str, kind: str, placeholder) -> None:
    """Creates an empty host placeholder so it can be mounted read-only (see PROVIDER_PROFILES)."""
    try:
        if kind == "dir":
            os.makedirs(path, mode=0o700, exist_ok=True)
        else:
            parent = os.path.dirname(path)
            if not os.path.isdir(parent):
                os.makedirs(parent, mode=0o700, exist_ok=True)
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            try:
                os.write(fd, str(placeholder).encode("utf-8"))
            finally:
                os.close(fd)
    except FileExistsError:
        return
    except OSError as exc:
        raise SandboxConfigError(
            f"无法在宿主创建受保护路径占位 {path} ({exc})，拒绝以可写方式挂载 provider 凭据目录"
        ) from exc


def _mask_mount_args(path: str) -> List[str]:
    """Hides an existing path: tmpfs for directories, /dev/null for anything else."""
    try:
        if os.path.isdir(path):
            return ["--tmpfs", path]
    except OSError:
        pass
    return ["--ro-bind", "/dev/null", path]


PRIVATE_HOME_SUBDIR = os.path.join("makewand", "provider-homes")
CREDENTIAL_MAX_BYTES = 4 * 1024 * 1024
_O_NOFOLLOW = getattr(os, "O_NOFOLLOW", 0)
_O_CLOEXEC = getattr(os, "O_CLOEXEC", 0)
_O_DIRECTORY = getattr(os, "O_DIRECTORY", 0)


def _create_private_home(provider: str) -> str:
    """Create a writable private home under the first usable allowed base."""
    candidates: List[str] = []
    runtime = os.environ.get("XDG_RUNTIME_DIR", "")
    if runtime and os.path.isabs(runtime):
        candidates.append(os.path.join(runtime, PRIVATE_HOME_SUBDIR))
    cache = os.environ.get("XDG_CACHE_HOME", "")
    if not (cache and os.path.isabs(cache)):
        cache = os.path.join(str(Path.home()), ".cache")
    candidates.append(os.path.join(cache, PRIVATE_HOME_SUBDIR))
    last_error = None
    for base in candidates:
        private = None
        try:
            os.makedirs(base, mode=0o700, exist_ok=True)
            st = os.lstat(base)
            if not stat.S_ISDIR(st.st_mode) or (hasattr(os, "geteuid") and st.st_uid != os.geteuid()):
                continue
            if stat.S_IMODE(st.st_mode) & 0o077:
                os.chmod(base, 0o700)
            private = tempfile.mkdtemp(prefix=f"{os.getpid()}-{provider}-", dir=base)
            os.chmod(private, 0o700)
            # Existing 0700 directories can still be on a read-only/full mount.
            # Exercise the actual private home before copying any credentials.
            with tempfile.TemporaryFile(dir=private) as probe:
                probe.write(b"\0")
                probe.flush()
            _sweep_stale_private_homes(base)
            return private
        except OSError as error:
            last_error = error
            if private is not None:
                _robust_force_remove(private)
            continue
    detail = f"；最后的 I/O 错误 [{last_error.errno}] {last_error.strerror}" if last_error is not None else ""
    raise ProviderStartupError(
        "启动环境检查失败：无法创建可写的私有 provider 状态目录"
        f"（XDG_RUNTIME_DIR / 用户缓存目录均不可用）{detail}，拒绝挂载 provider 状态"
    ) from last_error


def _sweep_stale_private_homes(base: str) -> None:
    """Removes private homes left behind by makewand processes that no longer exist."""
    try:
        names = os.listdir(base)
    except OSError:
        return
    for name in names:
        pid_s = name.split("-", 1)[0]
        if not pid_s.isdigit() or int(pid_s) == os.getpid():
            continue
        try:
            os.kill(int(pid_s), 0)
            continue
        except ProcessLookupError:
            pass
        except OSError:
            continue
        _robust_force_remove(os.path.join(base, name))


def _read_host_state(path: str) -> Optional[Tuple[bytes, int, tuple]]:
    """
    (content, mode, identity) of the bounded regular file at path (a symlink
    there is not followed), None otherwise. identity is (st_dev, st_ino,
    st_size, st_mtime_ns, sha256 of the content).
    """
    try:
        fd = os.open(path, os.O_RDONLY | _O_NOFOLLOW | _O_CLOEXEC | getattr(os, "O_NONBLOCK", 0))
    except OSError:
        return None
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode) or st.st_size > CREDENTIAL_MAX_BYTES:
            return None
        chunks = []
        size = 0
        while True:
            b = os.read(fd, min(65536, CREDENTIAL_MAX_BYTES + 1 - size))
            if not b:
                break
            size += len(b)
            if size > CREDENTIAL_MAX_BYTES:
                return None
            chunks.append(b)
        data = b"".join(chunks)
        return data, stat.S_IMODE(st.st_mode), (st.st_dev, st.st_ino, st.st_size, st.st_mtime_ns, _digest(data))
    except OSError:
        return None
    finally:
        os.close(fd)


def _read_host_regular(path: str) -> Optional[Tuple[bytes, int]]:
    """(content, mode) of a host file (dotfile symlinks followed), None when absent/not a bounded regular file."""
    state = _read_host_state(os.path.realpath(path))
    return None if state is None else state[:2]


def _read_private_nofollow(base: str, rel: str) -> Tuple[Optional[bytes], str]:
    """
    Reads base/rel without following any symlink below base. Returns (content,
    "") for a bounded regular file, else (None, why): "missing", "not-regular"
    (a symlink, FIFO, directory, ... at the file or on its path), "oversized" or
    "unreadable" (_PRIVATE_COPY_PROBLEM_TEXT).
    """
    parts = [p for p in rel.split("/") if p]
    if not parts or any(p in (".", "..") for p in parts):
        return None, "unreadable"
    fds: List[int] = []
    try:
        fds.append(os.open(base, os.O_RDONLY | _O_DIRECTORY | _O_NOFOLLOW | _O_CLOEXEC))
        for comp in parts[:-1]:
            fds.append(os.open(comp, os.O_RDONLY | _O_DIRECTORY | _O_NOFOLLOW | _O_CLOEXEC, dir_fd=fds[-1]))
        fds.append(os.open(parts[-1], os.O_RDONLY | _O_NOFOLLOW | _O_CLOEXEC | getattr(os, "O_NONBLOCK", 0),
                           dir_fd=fds[-1]))
        st = os.fstat(fds[-1])
        if not stat.S_ISREG(st.st_mode):
            return None, "not-regular"
        if st.st_size > CREDENTIAL_MAX_BYTES:
            return None, "oversized"
        chunks, size = [], 0
        while True:
            b = os.read(fds[-1], 65536)
            if not b:
                break
            chunks.append(b)
            size += len(b)
            if size > CREDENTIAL_MAX_BYTES:
                return None, "oversized"
        return b"".join(chunks), ""
    except FileNotFoundError:
        return None, "missing"
    except OSError as exc:
        # ELOOP: a symlink; ENOTDIR: a non-directory on the path; ENXIO: a socket.
        return None, "not-regular" if exc.errno in (errno.ELOOP, errno.ENOTDIR, errno.ENXIO) else "unreadable"
    finally:
        for fd in reversed(fds):
            try:
                os.close(fd)
            except OSError:
                pass


# Why a private credential copy cannot be read at write-back (_read_private_nofollow).
_PRIVATE_COPY_PROBLEM_TEXT = {
    "missing": "已被删除",
    "not-regular": "是符号链接、FIFO、目录等非普通文件",
    "oversized": f"超过 {CREDENTIAL_MAX_BYTES >> 20} MiB 上限",
    "unreadable": "无法读取",
}


# Seconds a write-back waits for another makewand write-back of the same host
# credential file (_credential_lock).
_CRED_LOCK_WAIT = 10.0


def _credential_lock_path(path: str) -> str:
    """Sidecar lock file of the host credential file path, never renamed or
    removed (the Go engine uses the same one, cliCredentialLockPath)."""
    return os.path.join(os.path.dirname(path), "." + os.path.basename(path) + ".makewand-lock")


@contextmanager
def _credential_lock(path: str):
    """
    Serializes makewand's own write-backs of the host credential file path
    across processes and both engines: an exclusive flock(2) on its sidecar
    lock file, which the Go engine takes as well (cliLockCredential). Yields
    False when another write-back held it for _CRED_LOCK_WAIT seconds. Where no
    lock can be taken (no fcntl, a filesystem without flock) it yields True
    unlocked; the identity re-check before the rename still applies.
    """
    fd = None
    if fcntl is not None:
        try:
            fd = os.open(_credential_lock_path(path), os.O_RDWR | os.O_CREAT | _O_NOFOLLOW | _O_CLOEXEC, 0o600)
        except OSError:
            fd = None
    acquired = True
    if fd is not None:
        deadline = time.monotonic() + _CRED_LOCK_WAIT
        while True:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    acquired = False
                    break
                time.sleep(0.05)
            except OSError:
                break  # this filesystem cannot lock
    try:
        yield acquired
    finally:
        if fd is not None:
            os.close(fd)  # releases the lock


def _atomic_write_host(path: str, data: bytes, mode: int, expect: tuple) -> str:
    """
    Atomically replaces the host file path with data, unless it is no longer
    the file the merge read. The data is written and synced to a temporary
    file first; then, holding the credential lock (_credential_lock), the host
    file's identity (_read_host_state: device, inode, size, mtime_ns, digest)
    is compared with expect immediately before the rename. Returns "written",
    "changed" (replaced or rewritten meanwhile) or "locked" (another makewand
    write-back held the lock); nothing is written unless "written".
    """
    parent = os.path.dirname(path)
    fd, tmp = tempfile.mkstemp(prefix=".makewand-wb-", dir=parent)
    status = "changed"
    try:
        with os.fdopen(fd, "wb") as fh:
            fh.write(data)
            fh.flush()
            os.fsync(fh.fileno())
        os.chmod(tmp, mode & 0o777 or 0o600)
        with _credential_lock(path) as locked:
            current = _read_host_state(path) if locked else None
            if not locked:
                status = "locked"
            elif current is not None and current[2] == expect:
                os.replace(tmp, path)
                status = "written"
    finally:
        if status != "written":
            try:
                os.unlink(tmp)
            except OSError:
                pass
    return status


def _digest(data: Optional[bytes]) -> Optional[str]:
    return None if data is None else hashlib.sha256(data).hexdigest()


# Credential write-back merge (_credential_writeback_merge).
_CRED_JSON_MAX_DEPTH = 64
_CRED_MISSING = object()
_CRED_ASCII_LOWER = str.maketrans("ABCDEFGHIJKLMNOPQRSTUVWXYZ", "abcdefghijklmnopqrstuvwxyz")
# Keys are compared in ASCII lower case without "_" and "-" (expires_at == expiresAt).
_CRED_KEY_NORM = str.maketrans("ABCDEFGHIJKLMNOPQRSTUVWXYZ", "abcdefghijklmnopqrstuvwxyz", "_-")
# Normalized keys of fields a token refresh never changes: e-mails, API keys and
# other secrets, credentials outside the refresh allowlist, endpoints, auth modes
# and account identifiers.
_CRED_PROTECTED_KEY = re.compile(
    r"email|apikey|secret|passw|privatekey|clientid|\Akey\Z|token|cookie|credential"
    r"|url\Z|uri\Z|issuer|endpoint|audience|\A(?:iss|aud|host|hostname|domain)\Z"
    r"|mode\Z|method\Z|mechanism|obtainedvia|granttype|authtype"
    r"|\A(?:sub|subject|uid|id|uuid|user|account|org|organization|team|tenant|principal|owner)\Z"
    r"|(?:account|user|org|organization|team|tenant|principal|owner|subject)(?:id|uuid|guid)\Z")
# Metadata that is never protected, even where its name says token or URL:
# scopes, profile pictures, times.
_CRED_BENIGN_KEY = re.compile(r"scope|avatar|image|picture|photo|icon|expir|lastrefresh")
_CRED_EMAIL_VALUE = re.compile(r"[^@\x00-\x20]+@[^@\x00-\x20]+\.[^@\x00-\x20]+")
_CRED_URL_VALUE = re.compile(r"[A-Za-z][A-Za-z0-9+.-]*://[^\x00-\x20]+")
_CRED_JWT_VALUE = re.compile(r"[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*")
_CRED_SURROGATE = re.compile("[\ud800-\udfff]")
_CRED_RFC3339 = re.compile(r"([0-9]{4})-([0-9]{2})-([0-9]{2})[Tt]([0-9]{2}):([0-9]{2}):([0-9]{2})"
                           r"(?:\.([0-9]{1,9}))?(?:[Zz]|([+-])([0-9]{2}):([0-9]{2}))")
# JWT claims naming the account, compared at any depth of the claims (e.g.
# inside the https://api.openai.com/auth object).
_CRED_ACCOUNT_CLAIMS = frozenset({"sub", "email", "user_id", "principal_id", "team_id", "account_id",
                                  "chatgpt_account_id", "chatgpt_user_id", "chatgpt_account_user_id"})
# Refresh tokens a refresh never rotates: Google's (golang.org/x/oauth2 and
# google-auth-library keep the old one), so a different one comes from a new login.
_CRED_STATIC_REFRESH_PREFIXES = ("1//",)
# Refusal codes -> warning text ({0}: field path, {1}: detail).
_CRED_PROBLEM_TEXT = {
    "absent": "启动时宿主没有该凭据，沙箱内新建的登录凭据不回写（请在宿主直接登录）",
    "host-not-object": "宿主版本不是 JSON 对象，无法核验是否同一账号",
    "sandbox-not-object": "沙箱内版本不是有效的 JSON 对象",
    "remove": "沙箱内版本删除了 {0}（疑似登出/篡改）",
    "clear": "沙箱内版本清空或改变了 {0} 的类型（疑似登出/篡改）",
    "protected": "沙箱内版本更改了令牌刷新从不改变的字段 {0}（疑似账号替换/重定向）",
    "jwt-shape": "沙箱内版本把 {0} 在 JWT 与不透明令牌之间替换",
    "jwt-account": "沙箱内版本的 {0} 属于其他账号（声明 {1} 不同）",
    "token-type": "沙箱内版本更改了令牌类型 {0}",
    "bad-expiry": "沙箱内版本的时间 {0} 无法解析",
    "backwards": "沙箱内版本把 {0} 改到更早的时间（疑似旧凭据回放）",
    "refresh-only": "沙箱内版本只更换了刷新令牌 {0}，没有新的访问令牌（疑似令牌替换）",
    "no-refresh-token": "沙箱内版本更换了访问令牌 {0}，但该凭据没有刷新令牌（这是新登录而不是刷新）",
    "google-refresh": "沙箱内版本更换了 Google 刷新令牌 {0}，刷新从不轮换它（这是新登录）",
}


class _JsonNumber(str):
    """A JSON number kept as its literal text, so identifiers compare exactly."""


def _cred_json_object(pairs):
    obj = {}
    for key, value in pairs:
        if key in obj:
            raise ValueError(f"duplicate object key {key!r}")
        obj[key] = value
    return obj


def _cred_json_constant(name):
    raise ValueError(f"non-standard JSON constant {name}")


def _cred_load_json(data: bytes):
    """
    One strict JSON value: UTF-8, no duplicate keys, no NaN/Infinity, no lone
    surrogate escapes, bounded depth. Raises ValueError.
    """
    try:
        value = json.loads(data.decode("utf-8"), object_pairs_hook=_cred_json_object,
                           parse_int=_JsonNumber, parse_float=_JsonNumber, parse_constant=_cred_json_constant)
    except RecursionError as exc:
        raise ValueError("JSON nesting too deep") from exc
    stack = [(value, 0)]
    while stack:
        node, depth = stack.pop()
        if depth > _CRED_JSON_MAX_DEPTH:
            raise ValueError("JSON nesting too deep")
        # json reads "\ud800" without its pair as a lone surrogate, which no
        # UTF-8 encoder accepts (and Go's decoder reads as U+FFFD).
        texts = node.keys() if isinstance(node, dict) else (node,) if isinstance(node, str) else ()
        if any(_CRED_SURROGATE.search(text) for text in texts):
            raise ValueError("lone surrogate escape")
        children = node.values() if isinstance(node, dict) else node if isinstance(node, list) else ()
        stack.extend((child, depth + 1) for child in children)
    return value


def _cred_load_object(data: bytes) -> Optional[dict]:
    try:
        value = _cred_load_json(data)
    except ValueError:
        return None
    return value if isinstance(value, dict) else None


def _cred_jwt_claims(value: str) -> Optional[dict]:
    """Claims of a JWT-shaped string. The signature is NOT verified: the claims
    are only compared with the host copy."""
    if not _CRED_JWT_VALUE.fullmatch(value):
        return None
    import base64
    header_seg, payload_seg, _sig = value.split(".")
    try:
        header, claims = (_cred_load_json(base64.urlsafe_b64decode(seg + "=" * (-len(seg) % 4)))
                          for seg in (header_seg, payload_seg))
    except ValueError:  # binascii.Error and UnicodeDecodeError included
        return None
    if not isinstance(header, dict) or "alg" not in header or not isinstance(claims, dict):
        return None
    return claims


def _cred_same(a, b) -> bool:
    """Decoded JSON values equal in type and value."""
    if type(a) is not type(b):
        return False
    if isinstance(a, dict):
        return a.keys() == b.keys() and all(_cred_same(a[k], b[k]) for k in a)
    if isinstance(a, list):
        return len(a) == len(b) and all(_cred_same(x, y) for x, y in zip(a, b))
    return a == b


def _cred_text(value) -> bool:
    """A non-empty JSON string."""
    return isinstance(value, str) and not isinstance(value, _JsonNumber) and value != ""


def _cred_kind(key: str, value) -> Optional[str]:
    """The token-refresh role of a host leaf: "access", "refresh", "id", "type"
    (token type) or "time" (expiry or last refresh), None for every other field."""
    nk = key.translate(_CRED_KEY_NORM)
    if nk in ("expiry", "expirydate", "lastrefresh") or nk.endswith("expiresat"):
        return "time" if isinstance(value, _JsonNumber) or _cred_text(value) else None
    if not _cred_text(value):
        return None
    if nk == "accesstoken" or (nk == "key" and _cred_jwt_claims(value) is not None):
        return "access"
    return {"refreshtoken": "refresh", "idtoken": "id", "tokentype": "type"}.get(nk)


def _cred_protected(key: str, *values) -> bool:
    """Whether a field holds something a token refresh never changes
    (_CRED_PROTECTED_KEY); e-mail and URL values and nested protected fields
    count as well."""
    nk = key.translate(_CRED_KEY_NORM)
    if _CRED_BENIGN_KEY.search(nk):
        return False
    return bool(_CRED_PROTECTED_KEY.search(nk)) or any(_cred_protected_value(v) for v in values)


def _cred_protected_value(value) -> bool:
    if isinstance(value, dict):
        return any(_cred_protected(k, v) for k, v in value.items())
    if isinstance(value, list):
        return any(_cred_protected_value(v) for v in value)
    return _cred_text(value) and bool(_CRED_EMAIL_VALUE.fullmatch(value) or _CRED_URL_VALUE.fullmatch(value))


def _cred_holds_tokens(obj: dict) -> bool:
    """Whether a host object holds an access or refresh token at any depth."""
    return any(_cred_kind(k, v) in ("access", "refresh") or (isinstance(v, dict) and _cred_holds_tokens(v))
               for k, v in obj.items())


def _cred_mint_shape(obj: dict) -> bool:
    """
    Whether the host object obj is in the login shape in which the provider
    CLI mints its keys itself (muse's OAuth record): it holds an OAuth access
    token, and neither its mechanism nor obtained_via names a key. A key saved
    with `muse auth set --api-key-stdin` or pasted in the pasted_key lane
    (mechanism api_key, obtained_via pasted_key) is the user's own and stays
    protected.
    """
    has_access = False
    for key, value in obj.items():
        nk = key.translate(_CRED_KEY_NORM)
        if nk == "accesstoken":
            has_access = has_access or _cred_text(value)
        elif nk in ("mechanism", "obtainedvia") and (not _cred_text(value) or "key" in value.translate(_CRED_KEY_NORM)):
            return False
    return has_access


def _cred_account_mismatch(old: dict, new: dict, prefix: str = "") -> Optional[str]:
    """The first account claim of old (at any depth) that new lacks or changes, None for the same account."""
    for key in sorted(old):
        value = old[key]
        if key in _CRED_ACCOUNT_CLAIMS:
            if key not in new or not _cred_same(value, new[key]):
                return prefix + key
        elif isinstance(value, dict):
            nested = new.get(key)
            claim = _cred_account_mismatch(value, nested if isinstance(nested, dict) else {}, prefix + key + ".")
            if claim:
                return claim
    return None


def _cred_instant(value) -> Optional[tuple]:
    """A comparable expiry or last-refresh time: a JSON number, or an RFC 3339
    string (with "T", at most nanosecond precision, "Z" or a numeric offset)."""
    if isinstance(value, _JsonNumber):
        number = float(value)
        return None if number in (float("inf"), float("-inf")) else (number,)
    match = _CRED_RFC3339.fullmatch(value)
    if not match:
        return None
    year, month, day, hour, minute, second = (int(g) for g in match.groups()[:6])
    leap = year % 4 == 0 and (year % 100 != 0 or year % 400 == 0)
    month_days = (31, 29 if leap else 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31)
    if not (year >= 1 and 1 <= month <= 12 and 1 <= day <= month_days[month - 1]
            and hour <= 23 and minute <= 59 and second <= 59):
        return None
    offset = 0
    if match.group(8):
        if int(match.group(9)) > 23 or int(match.group(10)) > 59:
            return None
        offset = (int(match.group(9)) * 60 + int(match.group(10))) * 60 * (-1 if match.group(8) == "-" else 1)
    # Days since 1970-01-01 (H. Hinnant's days_from_civil).
    y, mp = (year - 1, month + 9) if month <= 2 else (year, month - 3)
    era = y // 400
    yoe = y - era * 400
    days = era * 146097 + yoe * 365 + yoe // 4 - yoe // 100 + (153 * mp + 2) // 5 + day - 1 - 719468
    nanos = int((match.group(7) or "").ljust(9, "0"))
    return days * 86400 + hour * 3600 + minute * 60 + second - offset, nanos


def _cred_copy(value):
    if isinstance(value, dict):
        return {k: _cred_copy(v) for k, v in value.items()}
    if isinstance(value, list):
        return [_cred_copy(v) for v in value]
    return value


def _cred_dump(node, indent: Optional[str], level: int = 0) -> str:
    """JSON text of a decoded credential document (numbers keep their literal)."""
    if isinstance(node, (dict, list)):
        if isinstance(node, dict):
            sep = ": " if indent else ":"
            items = [json.dumps(k, ensure_ascii=False) + sep + _cred_dump(v, indent, level + 1)
                     for k, v in node.items()]
            brackets = "{}"
        else:
            items = [_cred_dump(v, indent, level + 1) for v in node]
            brackets = "[]"
        if not items:
            return brackets
        if indent is None:
            return brackets[0] + ",".join(items) + brackets[1]
        pad = "\n" + indent * (level + 1)
        return brackets[0] + pad + ("," + pad).join(items) + "\n" + indent * level + brackets[1]
    if isinstance(node, _JsonNumber):
        return str(node)
    if isinstance(node, bool) or node is None:
        return {True: "true", False: "false", None: "null"}[node]
    return json.dumps(node, ensure_ascii=False)


def _cred_field_list(fields: List[str]) -> str:
    """At most five field paths for a warning."""
    return ", ".join(fields[:5]) + (", ..." if len(fields) > 5 else "")


class _CredMerge:
    """Outcome of _credential_writeback_merge."""

    def __init__(self) -> None:
        self.data: Optional[bytes] = None  # bytes to write to the host; None: nothing to write
        self.problems: List[Tuple[str, str, str]] = []  # (field path, code, detail): nothing may be written
        self.ignored: List[str] = []  # protected fields added or removed in the sandbox copy

    def problem_text(self) -> str:
        texts = [_CRED_PROBLEM_TEXT[code].format(path, detail) for path, code, detail in self.problems[:5]]
        return "；".join(texts) + ("；……" if len(self.problems) > 5 else "")


class _CredWalk:
    """Differences between the host and sandbox credential documents."""

    def __init__(self, minted_keys: Iterable[str] = ()) -> None:
        self.minted = frozenset(k.translate(_CRED_KEY_NORM) for k in minted_keys)
        self.changes: list = []  # (host object path, key, kind, host value, sandbox value or _CRED_MISSING)
        self.groups: Dict[tuple, dict] = {}  # host objects holding access or refresh tokens
        self.problems: List[Tuple[str, str, str]] = []
        self.ignored: List[str] = []

    def problem(self, path, code: str, detail: str = "") -> None:
        self.problems.append((path if isinstance(path, str) else ".".join(path), code, detail))

    def walk(self, old: dict, new: dict, path: tuple) -> None:
        for key in sorted(old):
            value, current, p = old[key], new.get(key, _CRED_MISSING), path + (key,)
            kind = _cred_kind(key, value)
            if (kind is None and _cred_text(value) and key.translate(_CRED_KEY_NORM) in self.minted
                    and _cred_mint_shape(old)):
                kind = "minted"
            if kind is None:
                if isinstance(value, dict) and isinstance(current, dict):
                    self.walk(value, current, p)
                elif isinstance(value, dict) and _cred_holds_tokens(value):
                    self.problem(p, "remove")
                elif current is _CRED_MISSING:
                    if _cred_protected(key, value):
                        self.ignored.append(".".join(p))
                elif not _cred_same(value, current) and _cred_protected(key, value, current):
                    self.problem(p, "protected")
                continue
            changed = not _cred_same(value, current)
            if kind in ("access", "refresh"):
                group = self.groups.setdefault(path, {"refresh": False, "static": False,
                                                      "access_changed": "", "refresh_changed": ""})
                group["refresh"] = group["refresh"] or kind == "refresh"
                if changed:
                    group[kind + "_changed"] = group[kind + "_changed"] or ".".join(p)
                    if kind == "refresh" and value.startswith(_CRED_STATIC_REFRESH_PREFIXES):
                        group["static"] = True
            if changed:
                self.changes.append((path, key, kind, value, current))
        for key in sorted(new):
            if key not in old and _cred_protected(key, new[key]):
                self.ignored.append(".".join(path + (key,)))

    def check(self, path: tuple, kind: str, value, current) -> None:
        """Records the problems of one changed token-refresh leaf."""
        if current is _CRED_MISSING and kind not in ("access", "refresh"):
            return  # dropped: the host value is kept
        if kind == "id" and (current is None or current == ""):
            return  # a refresh without an id token keeps the host's
        if kind == "time":
            if type(current) is not type(value) or current == "":
                self.problem(path, "clear")
                return
            before, after = _cred_instant(value), _cred_instant(current)
            if before is None or after is None:
                self.problem(path, "bad-expiry")
            elif after < before:
                self.problem(path, "backwards")
            return
        if not _cred_text(current):
            self.problem(path, "clear")
        elif kind == "type":
            if current.translate(_CRED_ASCII_LOWER) != value.translate(_CRED_ASCII_LOWER):
                self.problem(path, "token-type")
        else:
            old_claims, new_claims = _cred_jwt_claims(value), _cred_jwt_claims(current)
            if (old_claims is None) != (new_claims is None):
                self.problem(path, "jwt-shape")
            elif old_claims is not None:
                claim = _cred_account_mismatch(old_claims, new_claims)
                if claim:
                    self.problem(path, "jwt-account", claim)


def _credential_writeback_merge(old: Optional[bytes], new: bytes, minted_keys: Iterable[str] = ()) -> _CredMerge:
    """
    Merges the sandbox copy `new` of a credential into the host document `old`
    (None: absent at launch). Only token-refresh leaves of host objects that hold
    tokens are copied: access, refresh and id tokens (grok's access JWT "key"
    included) and, as part of a refresh of their object or of an object below
    it, the token type and the expiry and last-refresh times. The merge has
    problems, and nothing is written, unless the change is a same-account refresh:
      * either document is not a strict JSON object, or the host had none;
      * a token or an object holding tokens is removed, emptied or retyped;
      * a protected field (_CRED_PROTECTED_KEY: e-mail, key, endpoint, auth mode,
        account id; e-mail and URL values) changes value;
      * a JWT-shaped token becomes opaque or vice versa, or a JWT's account
        claims (_CRED_ACCOUNT_CLAIMS) change;
      * a time moves backwards or is unreadable, or the token type changes;
      * a refresh token changes without the access token next to it, an access
        token changes in an object without a refresh token (a new login), or a
        Google refresh token, which a refresh never rotates, changes.
    minted_keys names keys whose value the provider CLI mints itself from the
    login (muse's api_key, see PROVIDER_PROFILES). Only inside a host object in
    the login shape (_cred_mint_shape: an OAuth access token, no key mechanism)
    is such a key minted: a changed one is a re-mint and is copied on its own,
    under the token rules (a non-empty string, same JWT shape and account
    claims); a removed one keeps the host value. Anywhere else (a saved or
    pasted API key) it stays a protected field.
    Other fields keep their host value: benign metadata (plan, rate tier,
    profile, scopes) silently, protected fields the sandbox adds or removes are
    listed in `ignored`. Opaque tokens cannot be tied to an account offline.
    The Go engine implements the same rules (cliMergeCredential).
    """
    result = _CredMerge()
    old_doc = _cred_load_object(old) if old is not None else None
    new_doc = _cred_load_object(new)
    if old is None or old_doc is None or new_doc is None:
        code = "absent" if old is None else "host-not-object" if old_doc is None else "sandbox-not-object"
        result.problems.append(("", code, ""))
        return result
    walk = _CredWalk(minted_keys)
    walk.walk(old_doc, new_doc, ())
    for path, key, kind, value, current in walk.changes:
        walk.check(path + (key,), kind, value, current)
    in_refresh = set()  # objects refreshed, or above a refreshed one
    for path, group in walk.groups.items():
        if group["refresh_changed"] and not group["access_changed"]:
            walk.problem(group["refresh_changed"], "refresh-only")
        if group["access_changed"] and not group["refresh"]:
            walk.problem(group["access_changed"], "no-refresh-token")
        if group["static"]:
            walk.problem(group["refresh_changed"], "google-refresh")
        if group["access_changed"]:
            in_refresh.update(path[:i] for i in range(len(path) + 1))
    result.ignored = sorted(set(walk.ignored))
    if walk.problems:
        result.problems = sorted(walk.problems)
        return result
    merged = _cred_copy(old_doc)
    for path, key, kind, value, current in walk.changes:
        if kind in ("access", "refresh") or (current is not _CRED_MISSING and (
                kind == "minted" or (path in in_refresh and (kind != "id" or _cred_text(current))))):
            node = merged
            for k in path:
                node = node[k]
            node[key] = current
    if _cred_same(merged, old_doc):
        return result
    if _cred_same(merged, new_doc):
        result.data = new  # keep the provider's own formatting
    else:
        indent = "  " if b"\n" in old.rstrip(b" \t\r\n") else None
        result.data = _cred_dump(merged, indent).encode("utf-8") + (b"\n" if old.endswith(b"\n") else b"")
    return result


@dataclass
class _CredentialCopy:
    private_root: str
    rel: str
    host_path: str
    initial_digest: Optional[str]
    mode: int = 0o600
    minted: Tuple[str, ...] = ()  # minted keys of this file (_credential_writeback_merge)


ACCOUNT_DIR_VARS = {"codex": "CODEX_HOME", "claude": "CLAUDE_CONFIG_DIR"}


def _unambiguous_account_path(path: str) -> bool:
    """Allow leading parents, but never erase a possibly retargetable entry."""
    seen_directory = False
    for component in path.replace(os.sep, "/").split("/"):
        if component == ".." and seen_directory:
            return False
        if component not in ("", ".", ".."):
            seen_directory = True
    return True


def _resolve_provider_profile(p_name: str, user_home: str, project_paths: Sequence[str]) -> Optional[dict]:
    profile = PROVIDER_PROFILES.get(p_name)
    if not profile:
        return None
    if p_name not in ("codex", "claude"):
        return profile
    variable = ACCOUNT_DIR_VARS[p_name]
    value = os.environ.get(variable)
    if value and not _unambiguous_account_path(value):
        raise SandboxConfigError(f"configured {variable} has ambiguous path traversal; refusing account fallback")
    default_root = "." + p_name
    if p_name == "claude" and not os.environ.get(variable):
        return profile
    selected = _norm(os.path.expanduser(os.environ.get(variable) or
                                       os.path.join(user_home, default_root)))
    real_selected = os.path.realpath(selected)
    if (_broad_workspace(real_selected, user_home) or _home_exposed(real_selected, user_home)
            or any(_is_within(os.path.realpath(path), real_selected) or
                   _is_within(real_selected, os.path.realpath(path)) for path in project_paths if path)):
        raise SandboxConfigError(f"{variable} must be a dedicated provider state directory")
    if os.environ.get(variable) and not os.path.isdir(selected):
        raise SandboxConfigError(f"configured {variable} does not exist; refusing account fallback")
    profile = dict(profile)
    profile["roots"] = [selected]
    for category in ("protected", "credentials", "seed"):
        profile[category] = [(selected, *entry[1:]) for entry in profile.get(category, [])
                             if entry[0] == default_root]
    if p_name == "claude":
        is_default = real_selected == os.path.realpath(os.path.join(user_home, ".claude"))
        profile["home_ro_files"] = [".claude.json"] if is_default else []
        profile["seed"] = [] if is_default else [(selected, ".claude.json")]
        profile["project_memory_root"] = (selected, "projects")
        profile["minted_keys"] = {(selected, entry[1]): value for entry, value in
                                  profile.get("minted_keys", {}).items() if entry[0] == default_root}
    return profile



# agy must never spend G1 credits: every agy process makewand launches reads
# its settings from the private provider home (see PROVIDER_PROFILES["agy"]),
# where both settings files are forced to useG1Credits=false. The host files
# (~/.gemini/settings.json and ~/.gemini/antigravity-cli/settings.json, the
# latter being the one the Antigravity CLI actually reads) are only read,
# never written.
AGY_SETTINGS_RELS = ("settings.json", "antigravity-cli/settings.json")


def _sanitized_agy_settings_bytes(host_path) -> bytes:
    """Host agy settings content with useG1Credits forced to false.

    Preserves every other key; a missing, unreadable or non-object file
    yields a minimal {"useG1Credits": false} document.
    """
    data = {}
    if host_path:
        found = _read_host_regular(str(host_path))
        if found is not None:
            try:
                parsed = json.loads(found[0].decode("utf-8"))
            except Exception:
                parsed = None
            if isinstance(parsed, dict):
                data = dict(parsed)
    data["useG1Credits"] = False
    return (json.dumps(data, ensure_ascii=False, indent=2) + "\n").encode("utf-8")


# Variables that could point agy at settings other than the ~/.gemini files
# checked below (discovery honours the first two).
AGY_CONFIG_DIR_ENVS = ("GEMINI_CONFIG_DIR", "AGY_CONFIG_DIR", "ANTIGRAVITY_CONFIG_DIR")

# agy writes a helper script below this directory before every run_command
# (BIN/agentapi, `exec ".../agy" agentapi "$@"`). It was protected read-only
# like every other executable path, so every sandboxed agy command failed with
# "read-only file system". The directory is therefore overlaid with a tmpfs
# (see _agy_bin_mount_args): shipped host files stay visible read-only, the
# agentapi entry is left for agy itself, and everything vanishes with the
# sandbox — the host directory is never written.
AGY_BIN_REL = "antigravity-cli/bin"
AGY_BIN_WRITABLE_ENTRY = "agentapi"


def _agy_bin_mount_args(host_bin: str, sandbox_bin: str) -> List[str]:
    """bwrap mounts for agy's writable helper-script directory.

    Always overlays sandbox_bin with a tmpfs, then re-binds every regular
    host file inside it read-only — except AGY_BIN_WRITABLE_ENTRY, which agy
    creates itself. Symlinks, directories and other non-regular entries are
    never re-bound, so nothing outside the host directory can be pulled in.
    A missing host directory yields an empty writable tmpfs. A host path
    that is a symlink or not a directory keeps the previous read-only bind.
    """
    if os.path.islink(host_bin):
        # A dangling link gets no mount (as before); its target is bound
        # read-only otherwise.
        target = os.path.realpath(host_bin)
        if os.path.exists(target):
            return ["--ro-bind", target, sandbox_bin]
        return []
    if os.path.lexists(host_bin) and not os.path.isdir(host_bin):
        return ["--ro-bind", host_bin, sandbox_bin]
    args = ["--tmpfs", sandbox_bin]
    try:
        names = sorted(os.listdir(host_bin))
    except OSError:
        return args
    for name in names:
        if name == AGY_BIN_WRITABLE_ENTRY:
            continue
        full = os.path.join(host_bin, name)
        try:
            st = os.lstat(full)
        except OSError:
            continue
        if not stat.S_ISREG(st.st_mode):
            continue
        args.extend(["--ro-bind", full, os.path.join(sandbox_bin, name)])
    return args


def _agy_host_credits_disabled(user_home=None) -> bool:
    """True only when both host agy settings files prove useG1Credits is false.

    A missing, unreadable, non-object or otherwise unproven file does not
    count: without a sandbox agy would read the host settings (or its own
    default-true behaviour) and could spend G1 credits once quota runs out.
    A set config-dir variable does not count either: agy could read other
    settings than the ones checked here.
    """
    if any(os.environ.get(name) for name in AGY_CONFIG_DIR_ENVS):
        return False
    home = _norm(str(user_home)) if user_home else _norm(str(Path.home()))
    gemini_root = os.path.join(home, ".gemini")
    for rel in AGY_SETTINGS_RELS:
        found = _read_host_regular(os.path.join(gemini_root, rel))
        if found is None:
            return False
        try:
            parsed = json.loads(found[0].decode("utf-8"))
        except Exception:
            return False
        if not isinstance(parsed, dict) or parsed.get("useG1Credits") is not False:
            return False
    return True


def _agy_no_sandbox_credits_message() -> str:
    return (
        "❌ [Agy 沙箱不可用] 没有可信 Bubblewrap (bwrap) 沙箱时，拒绝在宿主直接运行 agy："
        "宿主 ~/.gemini/settings.json 与 ~/.gemini/antigravity-cli/settings.json 的 "
        "\"useG1Credits\" 未能证明均为 false，agy 在订阅额度用完后可能扣 G1 credits (fail closed)。\n"
        "解决建议：\n"
        "  1. [安装沙箱] 在 Linux 上安装 Bubblewrap 并启用非特权用户命名空间，让 agy 读取私有 home 里的净化设置；\n"
        "  2. [关闭 credits 后直跑] 把上面两个宿主设置文件里的 \"useG1Credits\" 都明确设为 false，"
        "并取消 GEMINI_CONFIG_DIR / AGY_CONFIG_DIR / ANTIGRAVITY_CONFIG_DIR（它们会让 agy 改读别处的设置），"
        "再按无沙箱宿主执行的授权流程运行。"
    )


def _is_agy_provider(provider) -> bool:
    return (provider or "").lower().strip() in ("agy", "gemini")


def _write_agy_sanitized_settings(priv_root: str, host_real_root: str) -> list:
    """Write sanitized agy settings into a private root; return the dst paths.

    Covers both settings locations agy may read. Existing private files are
    replaced (they are inside the per-run private directory, never the host).
    """
    written = []
    for rel in AGY_SETTINGS_RELS:
        host_p = os.path.join(host_real_root, rel) if host_real_root else None
        content = _sanitized_agy_settings_bytes(host_p)
        dst = os.path.join(priv_root, rel)
        try:
            os.makedirs(os.path.dirname(dst), mode=0o700, exist_ok=True)
            fd = os.open(dst, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | _O_NOFOLLOW | _O_CLOEXEC, 0o600)
            try:
                os.write(fd, content)
            finally:
                os.close(fd)
            try:
                os.chmod(dst, 0o600)
            except OSError:
                pass
        except OSError as exc:
            raise SandboxConfigError(f"无法写私有 agy 设置 ({exc})，拒绝启动 (fail closed)") from exc
        written.append(dst)
    return written


class ProviderHome:
    """
    Per-run private provider home (H3). Created lazily by wrap_bwrap via
    materialize(); finalize() merges same-account token refreshes from changed
    credential files into the host files (see write_back) and deletes the
    private directory. Use provider_home_session().
    """

    def __init__(self, provider: str, user_home: Optional[str] = None, project_paths: Sequence[str] = ()):
        p = (provider or "").lower().strip()
        self.provider = "agy" if p == "gemini" else p
        self.user_home = _norm(user_home or str(Path.home()))
        self.project_paths = [p for p in project_paths if p]
        self.private_dir: Optional[str] = None
        self.bwrap_args: List[str] = []
        self.state_roots: List[str] = []
        self.scan_roots: List[str] = []
        self.credentials: List[_CredentialCopy] = []
        self.identity_file: Optional[Tuple[str, str]] = None
        self._materialized = False
        self._lock = threading.Lock()
        self._private_roots: Dict[str, str] = {}

    def materialize(self, project_paths: Sequence[str] = (),
                    absent_root_ok: Optional[Callable[[str], bool]] = None) -> List[str]:
        """
        Builds the private home and returns its bwrap arguments (cached).
        absent_root_ok(path) tells whether a provider root missing on the host
        may get a private directory mounted at path (_bind_absent_root); None
        allows it everywhere.
        """
        with self._lock:
            if self._materialized:
                return list(self.bwrap_args)
            from makewand.execution_runtime import current_context
            from makewand.account_identity import credential_binding, refuse_credential_launch
            binding = current_context().get("_credential_binding")
            if binding is not None and binding != credential_binding(self.provider):
                refuse_credential_launch("selected login changed after admission; refusing account switch")
            paths = list(self.project_paths) + [p for p in project_paths if p]
            profile = _resolve_provider_profile(self.provider, self.user_home, paths)
            if not profile:
                self._materialized = True
                return []
            try:
                self._build(profile, paths, absent_root_ok)
            except BaseException:
                self._remove_private_dir()
                raise
            self._materialized = True
            return list(self.bwrap_args)

    def _ensure_private_dir(self) -> str:
        if self.private_dir is None:
            self.private_dir = _create_private_home(self.provider)
        return self.private_dir

    def _copy_in(self, src: str, priv_root: str, rel: str) -> Optional[Tuple[bytes, int]]:
        found = _read_host_regular(src)
        from makewand.account_identity import verify_credential_copy
        verify_credential_copy(src, found[0] if found else None)
        if (self.provider == "claude" and rel == ".claude.json" or
                self.provider == "codex" and rel == "auth.json"):
            self.identity_file = (priv_root, rel)
        if found is None:
            return None
        dst = os.path.join(priv_root, rel)
        try:
            os.makedirs(os.path.dirname(dst), mode=0o700, exist_ok=True)
            fd = os.open(dst, os.O_WRONLY | os.O_CREAT | os.O_EXCL | _O_NOFOLLOW | _O_CLOEXEC, 0o600)
            try:
                os.write(fd, found[0])
            finally:
                os.close(fd)
        except OSError as exc:
            raise SandboxConfigError(f"无法复制 provider 凭据到私有目录 ({exc})，拒绝启动 (fail closed)") from exc
        return found

    def _private_root(self, idx: int) -> str:
        priv = os.path.join(self._ensure_private_dir(), f"root{idx}")
        try:
            os.mkdir(priv, 0o700)
        except OSError as exc:
            raise SandboxConfigError(f"无法创建私有 provider 状态目录 ({exc})，拒绝启动 (fail closed)") from exc
        return priv

    def _bind_absent_root(self, profile: dict, idx: int, root_rel: str, root_path: str, args: List[str]) -> None:
        """
        A provider root missing on the host that holds credential files gets an
        empty private directory at its path inside the sandbox, where HOME is an
        empty tmpfs: nothing is created on the host, and a login made inside the
        sandbox lands in the private copy, so write_back reports it instead of it
        vanishing with the tmpfs (it is never written back: there is no host
        account to verify it against).
        """
        rels = [rel for e_root, rel in profile.get("credentials", []) if e_root == root_rel]
        if not rels and self.provider != "agy":
            return
        priv = self._private_root(idx)
        self._private_roots[root_rel] = priv
        if self.provider == "codex":
            # Also observe a login created in an initially empty private root.
            self.identity_file = (priv, "auth.json")
        args.extend(["--bind", priv, root_path])
        if self.provider == "agy":
            # No host settings exist: the private home must still present
            # useG1Credits=false so a default-true CLI cannot spend credits.
            for _dst in _write_agy_sanitized_settings(priv, root_path):
                pass
            # Bind each sanitized file read-only at its expected location.
            for rel in AGY_SETTINGS_RELS:
                dst = os.path.join(priv, rel)
                args.extend(["--ro-bind", dst, os.path.join(root_path, rel)])
            # agy still needs a writable helper-script directory: an empty
            # tmpfs at the bin path (nothing exists on the host to seed it).
            args.extend(["--tmpfs", os.path.join(root_path, AGY_BIN_REL)])
        minted = profile.get("minted_keys", {})
        for rel in rels:
            self.credentials.append(_CredentialCopy(private_root=priv, rel=rel, host_path=os.path.join(root_path, rel),
                                                    initial_digest=None, minted=tuple(minted.get((root_rel, rel), ()))))

    def _build(self, profile: dict, project_paths: Sequence[str],
               absent_root_ok: Optional[Callable[[str], bool]] = None) -> None:
        args: List[str] = []
        for rel in profile.get("home_ro_files", []):
            p = os.path.join(self.user_home, rel)
            if not os.path.exists(p):
                from makewand.account_identity import verify_credential_copy
                verify_credential_copy(p, None)
                continue
            private = self._ensure_private_dir()
            if self._copy_in(p, private, rel) is not None:
                args.extend(["--ro-bind", os.path.join(private, rel), p])

        real_roots: Dict[str, str] = {}
        for idx, root_rel in enumerate(profile.get("roots", [])):
            root_path = _norm(os.path.join(self.user_home, root_rel))
            if not os.path.isdir(root_path):
                # The root can disappear after materialize's admission check.
                from makewand.account_identity import verify_credential_copy
                for kind in ("credentials", "seed"):
                    for e_root, rel in profile.get(kind, []):
                        if e_root == root_rel:
                            verify_credential_copy(os.path.join(root_path, rel), None)
                # Never create or expose a missing provider root on the host.
                if absent_root_ok is None or absent_root_ok(root_path):
                    self._bind_absent_root(profile, idx, root_rel, root_path, args)
                continue
            real = os.path.realpath(root_path)
            priv = self._private_root(idx)
            self._private_roots[root_rel] = priv
            real_roots[root_rel] = real
            self.state_roots.append(real)
            args.extend(["--bind", priv, real])
            if real != root_path:
                # Keep the host layout: ~/.gemini -> /mnt/.../gemini.
                args.extend(["--symlink", real, root_path])

            for kind in ("credentials", "seed"):
                for e_root, rel in profile.get(kind, []):
                    if e_root != root_rel:
                        continue
                    host_p = os.path.join(real, rel)
                    found = self._copy_in(host_p, priv, rel)
                    if kind == "credentials":
                        self.credentials.append(_CredentialCopy(
                            private_root=priv, rel=rel, host_path=host_p,
                            initial_digest=_digest(found[0]) if found else None,
                            mode=found[1] if found else 0o600,
                            minted=tuple(profile.get("minted_keys", {}).get((e_root, rel), ())),
                        ))

            # An explicit CLAUDE_CONFIG_DIR makes Claude read its metadata
            # inside that directory, even when it names the default root.
            if (self.provider == "claude" and os.environ.get("CLAUDE_CONFIG_DIR")
                    and real == os.path.realpath(os.path.join(self.user_home, ".claude"))):
                self._copy_in(os.path.join(self.user_home, ".claude.json"), priv, ".claude.json")

        for root_rel, sub, _kind, _placeholder in profile.get("protected", []):
            real = real_roots.get(root_rel)
            if not real:
                continue
            p = os.path.join(real, sub)
            if self.provider == "agy" and sub in AGY_SETTINGS_RELS:
                # Force useG1Credits=false in the private home: write a
                # sanitized copy (host file only read) and bind it read-only
                # at the path agy reads. The host original is first bound
                # read-only (the protected-file invariant other providers
                # rely on) and then shadowed by the sanitized copy, so the
                # file agy actually sees is the sanitized one and the host
                # file is never writable or modified.
                priv = self._private_roots.get(root_rel)
                if priv is None:
                    continue
                if os.path.islink(p):
                    _target = os.path.realpath(p)
                    if os.path.exists(_target):
                        args.extend(["--ro-bind", _target, p])
                elif os.path.exists(p):
                    args.extend(["--ro-bind", p, p])
                _write_agy_sanitized_settings(priv, real)
                dst = os.path.join(priv, sub)
                args.extend(["--ro-bind", dst, p])
                continue
            if self.provider == "agy" and sub == AGY_BIN_REL:
                # Writable overlay for agy's helper-script directory (both
                # read-only and writable runs use this private home): a tmpfs
                # at the directory, shipped host files re-bound read-only,
                # agentapi left for agy itself. The tmpfs dies with the
                # sandbox, so the host directory keeps its bytes and modes
                # and agentapi is never written back.
                priv = self._private_roots.get(root_rel)
                if priv is not None:
                    try:
                        os.makedirs(os.path.join(priv, os.path.dirname(sub)), mode=0o700, exist_ok=True)
                    except OSError:
                        pass
                args.extend(_agy_bin_mount_args(p, p))
                if os.path.isdir(p) and not os.path.islink(p):
                    self.scan_roots.append(p)
                continue
            if os.path.islink(p):
                # dotfile-managed symlink: bind its resolved target read-only at
                # the expected path (the link itself is not exposed)
                target = os.path.realpath(p)
                if os.path.exists(target):
                    args.extend(["--ro-bind", target, p])
                    if os.path.isdir(target):
                        self.scan_roots.append(target)
                continue
            if os.path.exists(p):
                args.extend(["--ro-bind", p, p])
                if os.path.isdir(p):
                    self.scan_roots.append(p)

        pm = profile.get("project_memory_root")
        if pm and real_roots.get(pm[0]):
            projects_dir = os.path.join(real_roots[pm[0]], pm[1])
            for proj in project_paths:
                key = _claude_project_key(proj)
                if not key:
                    continue
                mem = os.path.join(projects_dir, key, "memory")
                if os.path.isdir(mem) and not os.path.islink(mem):
                    pair = ["--ro-bind", mem, mem]
                    if mem not in args:
                        args.extend(pair)
        self.bwrap_args = args

    def write_back(self) -> List[str]:
        """
        Merges changed credential copies into the host files: only the
        token-refresh fields of a same-account refresh are written
        (_credential_writeback_merge), so a sandboxed agent can neither swap the
        host account nor redirect it or log it out. A failure for one file is
        reported and never stops the others or escapes finalize(). Returns the
        written host paths.
        """
        written: List[str] = []
        for cred in self.credentials:
            try:
                target = self._write_back_one(cred)
            except Exception as exc:  # noqa: BLE001 - see docstring
                _warn(f"凭据回写失败 {cred.host_path}: {type(exc).__name__}，保留宿主版本")
                continue
            if target:
                written.append(target)
        return written

    def _write_back_one(self, cred: _CredentialCopy) -> Optional[str]:
        """Merges one changed credential copy into its host file; returns the written host path."""
        if self.provider == "agy" and cred.rel in AGY_SETTINGS_RELS:
            # The sanitized agy settings in the private home are generated by
            # makewand itself (useG1Credits=false): never a user login, never
            # written back to the host, never warned about.
            return None
        data, why = _read_private_nofollow(cred.private_root, cred.rel)
        if data is None:
            # The host copy stays untouched. A copy that existed at launch and
            # is gone, replaced or unreadable may have held a rotated refresh
            # token, so the user learns why the host login can stop working.
            problem = _PRIVATE_COPY_PROBLEM_TEXT[why]
            if cred.initial_digest is not None:
                _warn(f"沙箱内的凭据副本 {cred.host_path} 在运行结束时{problem}，无法核对其中的令牌刷新，保留宿主版本"
                      "（若提供方已在沙箱内轮换令牌，宿主登录可能已失效，请在宿主重新登录）")
            elif why != "missing":
                _warn(f"沙箱内新建的凭据 {cred.host_path} {problem}，不回写（请在宿主直接登录）")
            return None
        if _digest(data) == cred.initial_digest:
            return None
        target = cred.host_path
        if os.path.islink(target):
            target = os.path.realpath(target)
        if os.path.lexists(target) and not os.path.isfile(target):
            _warn(f"宿主凭据 {cred.host_path} 不是普通文件，跳过回写")
            return None
        changed = f"宿主凭据 {cred.host_path} 在运行期间被其他会话修改，保留宿主版本，跳过回写"
        state = _read_host_state(target)
        current = state[0] if state else None
        if _digest(current) != cred.initial_digest:
            _warn(changed)
            return None
        # The host copy still equals the launch snapshot, so it is the original.
        merge = _credential_writeback_merge(current, data, cred.minted)
        if merge.problems:
            _warn(f"宿主凭据 {cred.host_path} 未回写：{merge.problem_text()}，保留宿主版本")
            return None
        outcome, written = "保留宿主版本", None
        if merge.data is not None:
            try:
                status = _atomic_write_host(target, merge.data, cred.mode, state[2])
            except OSError as exc:
                _warn(f"凭据回写失败 {cred.host_path}: {exc}")
            else:
                if status == "written":
                    outcome, written = "已回写刷新后的令牌", target
                elif status == "locked":
                    _warn(f"宿主凭据 {cred.host_path} 正由另一个 makewand 进程回写（等待锁超时），保留宿主版本，跳过回写")
                else:
                    _warn(changed)
        if merge.ignored:
            _warn(f"宿主凭据 {cred.host_path}：沙箱内新增或删除的字段 {_cred_field_list(merge.ignored)} "
                  f"未回写（只回写令牌刷新），{outcome}")
        return written

    def _remove_private_dir(self) -> None:
        if self.private_dir:
            _robust_force_remove(self.private_dir)
            self.private_dir = None

    def finalize(self, write_back: bool = True) -> None:
        with self._lock:
            try:
                if self._materialized:
                    from makewand.execution_runtime import current_context
                    evidence = current_context().get("_evidence")
                    if evidence is not None:
                        from makewand import execution_evidence
                        try:
                            execution_evidence.private_home(evidence, self)
                            execution_evidence.save(evidence)
                        except (OSError, ValueError, RuntimeError):
                            evidence["persistence_failed"] = True
                if write_back and self._materialized:
                    self.write_back()
            finally:
                self._remove_private_dir()


_ORPHAN_HOMES: List[ProviderHome] = []
_ORPHAN_LOCK = threading.Lock()


def _cleanup_orphan_homes() -> None:
    with _ORPHAN_LOCK:
        homes = list(_ORPHAN_HOMES)
        _ORPHAN_HOMES.clear()
    for home in homes:
        try:
            home.finalize(write_back=False)
        except Exception:
            pass


atexit.register(_cleanup_orphan_homes)


def _orphan_provider_home(p_name: str, user_home: str) -> ProviderHome:
    """wrap_bwrap without a session: private home without credential write-back, removed at exit."""
    home = ProviderHome(p_name, user_home)
    with _ORPHAN_LOCK:
        _ORPHAN_HOMES.append(home)
    return home


@contextmanager
def provider_home_session(provider_name: str, project_paths: Sequence[str] = ()):
    """Yields a ProviderHome for one sandboxed provider run (pass it to wrap_bwrap)."""
    home = ProviderHome(provider_name, None, project_paths)
    try:
        yield home
    finally:
        home.finalize()


def _provider_mounts(
    p_name: str,
    user_home: str,
    readonly: bool,
    project_paths: Sequence[str],
    provider_home: Optional[ProviderHome] = None,
    absent_root_ok: Optional[Callable[[str], bool]] = None,
) -> Tuple[List[str], List[str], List[str]]:
    """
    Builds the provider state mounts (private home, see PROVIDER_PROFILES).
    Returns (bwrap_args, state_roots, socket_scan_roots). readonly is accepted for
    compatibility: read-only and writable runs both get a private home.
    absent_root_ok: see ProviderHome.materialize.
    """
    if p_name not in PROVIDER_PROFILES:
        return [], [], []
    home = provider_home or _orphan_provider_home(p_name, user_home)
    args = home.materialize(project_paths, absent_root_ok)
    return args, list(home.state_roots), list(home.scan_roots)


def _mount_point_creatable(bwrap_args: Sequence[str], path: str) -> bool:
    """
    Whether bubblewrap can create the missing mount point path after the
    mounts in bwrap_args: the last of them covering its parent directory is a
    tmpfs (the empty sandbox HOME), not a read-only or host bind (a HOME made
    visible by the workspace, a toolchain directory such as ~/.local).
    """
    parent = os.path.dirname(_norm(path))
    covering = ""
    i = 0
    while i < len(bwrap_args):
        op = bwrap_args[i]
        if op in ("--bind", "--ro-bind", "--dev-bind", "--symlink", "--setenv") and i + 2 < len(bwrap_args):
            if op != "--setenv" and _is_within(parent, bwrap_args[i + 2]):
                covering = op
            i += 3
        elif op in ("--tmpfs", "--proc", "--dev") and i + 1 < len(bwrap_args):
            if _is_within(parent, bwrap_args[i + 1]):
                covering = op
            i += 2
        else:
            i += 1
    return covering == "--tmpfs"


# ---------------------------------------------------------------------------
# command builder
# ---------------------------------------------------------------------------
def _detect_provider(provider_name: Optional[str], command_args: Optional[Sequence]) -> str:
    """Provider from the explicit name, else from the first argv binary name."""
    p_name = (provider_name or "").lower().strip()
    if p_name == "gemini":
        p_name = "agy"
    if not p_name and command_args:
        first_bin = os.path.basename(str(command_args[0])).lower()
        if first_bin == "gemini" or first_bin.startswith("gemini-") or first_bin.startswith("gemini."):
            p_name = "agy"
        else:
            for candidate in ["claude", "codex", "agy", "muse", "grok", "aider"]:
                if first_bin == candidate or first_bin.startswith(candidate + "-") or first_bin.startswith(candidate + "."):
                    p_name = candidate
                    break
    return p_name


def wrap_bwrap(
    command_args: List[str],
    workspace: str,
    allow_network: bool = True,
    readonly: bool = False,
    repo_root: Optional[str] = None,
    is_provider: bool = False,
    worktree_root: Optional[str] = None,
    extra_env: Optional[dict] = None,
    provider_name: Optional[str] = None,
    extra_ro_binds: Optional[List[str]] = None,
    seccomp_fd: Optional[int] = None,
    provider_home: Optional[ProviderHome] = None,
) -> List[str]:
    """
    Wraps command with bubblewrap isolating host filesystem, IPC, PID, and credentials.
    Workspace (or entire worktree root if workspace is a subdirectory) is mounted.
    Workspace is mounted read-only if readonly=True, otherwise writable.
    If is_provider=True, the active provider gets a private per-run home at its
    expected path (copied credentials, read-only instruction/extension/config
    binds, see PROVIDER_PROFILES); pass provider_home (provider_home_session)
    to get changed credentials written back after the run.
    HOME is always an empty tmpfs; /home, /root, /mnt, /media, /srv are hidden.
    Raises SandboxConfigError when no safe command line can be built (fail closed).
    """
    bwrap = resolve_trusted_bwrap()
    if not bwrap:
        raise SandboxConfigError("未找到可信的 Bubblewrap (bwrap)（仅接受系统目录中 root 所有且不可被组/其他用户写入的 bwrap），拒绝构建沙箱命令 (fail closed)")
    ws = _norm(workspace)
    user_home = _norm(str(Path.home()))

    # Determine worktree mount root to ensure full repo visibility when running from a subdirectory
    # NEVER execute git rev-parse inside untrusted workspaces, which can expand mounts via core.worktree
    mount_root = None
    if worktree_root:
        wt_abs = _norm(worktree_root)
        try:
            if Path(ws).resolve().is_relative_to(Path(wt_abs).resolve()):
                mount_root = wt_abs
        except Exception:
            pass

    if not mount_root:
        try:
            curr = Path(ws).resolve()
            boundaries = {
                Path.home().resolve(),
                Path("/tmp").resolve(),
                Path("/var/tmp").resolve(),
                Path("/").resolve(),
            }
            if (curr / ".git").exists():
                mount_root = str(curr)
            else:
                p = curr.parent
                while p != curr and p not in boundaries:
                    if (p / ".git").exists():
                        mount_root = str(p)
                        break
                    p = p.parent
        except Exception:
            pass

    if not mount_root:
        mount_root = ws
    mount_root = _norm(mount_root)
    repo_abs = _norm(repo_root) if repo_root else None

    # A writable mount of forbidden roots, / or of HOME (or an ancestor of HOME) would expose the
    # whole host / every credential read-write: refuse (fail closed).
    if not readonly:
        forbidden_candidates = set(FORBIDDEN_WRITABLE_ROOTS)
        for r in FORBIDDEN_WRITABLE_ROOTS:
            try:
                forbidden_candidates.add(_norm(os.path.realpath(r)))
            except Exception:
                pass
        for cand in {ws, mount_root}:
            cand_norm = _norm(cand)
            try:
                cand_real = _norm(os.path.realpath(cand))
            except Exception:
                cand_real = cand_norm
            if cand_norm in forbidden_candidates or cand_real in forbidden_candidates:
                raise SandboxConfigError(
                    f"拒绝以 {cand} 作为可写沙箱工作区：它是受保护的系统根目录或受限路径 ({cand_real})，"
                    "可写挂载会破坏宿主系统隔离。请在具体项目目录中运行。"
                )
            if _home_exposed(cand_norm, user_home) or _home_exposed(cand_real, user_home):
                raise SandboxConfigError(
                    f"拒绝以 {cand} 作为可写沙箱工作区：它是 HOME（或 HOME 的上级目录），"
                    "可写挂载会暴露全部宿主文件与凭据。请在具体项目目录中运行。"
                )

    # Identify provider strictly from explicit provider_name or first command argument binary name
    p_name = _detect_provider(provider_name, command_args) if is_provider else ""

    masked_roots = _effective_masked_roots()

    bwrap_cmd = [
        bwrap,
        "--die-with-parent",
        "--new-session",
        "--unshare-pid",
        "--unshare-ipc",
        "--unshare-uts",
        "--unshare-user",
        # No --disable-userns (bubblewrap >= 0.8 only; provider CLIs such as
        # codex also run their own bubblewrap sandbox inside this one).
        "--clearenv",
        # Read-only host root filesystem
        "--ro-bind", "/", "/",
        "--proc", "/proc",
        "--dev", "/dev",
        "--tmpfs", "/tmp",
    ]

    # Hardened /proc masking (kernel symbols, core dump triggers, sched debug)
    for proc_leaf in ("/proc/kallsyms", "/proc/kcore", "/proc/sysrq-trigger", "/proc/sched_debug"):
        if os.path.exists(proc_leaf):
            bwrap_cmd.extend(["--ro-bind", "/dev/null", proc_leaf])
    for proc_dir in ("/proc/acpi", "/proc/asound"):
        if os.path.isdir(proc_dir):
            bwrap_cmd.extend(["--tmpfs", proc_dir])

    if seccomp_fd is not None:
        bwrap_cmd.extend(["--seccomp", str(seccomp_fd)])

    # S05 defense: Mask system Unix domain sockets and host IPC runtimes
    for sock_runtime_dir in SOCKET_RUNTIME_DIRS:
        if os.path.exists(sock_runtime_dir) and not os.path.islink(sock_runtime_dir):
            bwrap_cmd.extend(["--tmpfs", sock_runtime_dir])

    # Empty tmpfs masks below the read-only root are remounted read-only once
    # every bind inside them is in place, so writes there fail like they did on
    # the read-only root instead of silently landing in RAM.
    readonly_masks: List[str] = []

    # F06: host sockets below /var that stay visible through the read-only root
    for var_mask in _host_var_socket_masks():
        mask_args = _mask_mount_args(var_mask)
        bwrap_cmd.extend(mask_args)
        if mask_args[0] == "--tmpfs":
            readonly_masks.append(var_mask)

    # py-security#4: hide user-data prefixes (other projects, other users, data disks)
    for masked in masked_roots:
        bwrap_cmd.extend(["--tmpfs", masked])
        readonly_masks.append(masked)

    # HOME isolation policy: Always isolate HOME with tmpfs to prevent credential, history and token leakage
    bwrap_cmd.extend(["--tmpfs", user_home])

    # Mount safe executable toolchain directories (never parent directories containing credential files)
    home_bins = list(SAFE_HOME_BIN_DIRS) + list(PROVIDER_HOME_BIN_DIRS.get(p_name, []))
    for bin_sub in home_bins:
        tp = os.path.join(user_home, bin_sub)
        if os.path.exists(tp):
            bwrap_cmd.extend(["--ro-bind", tp, tp])

    # Mount the active Python interpreter / venv when it lives in a hidden location
    mounted_ro: List[str] = []

    def _ro_rebind(path: str) -> None:
        path = _norm(path)
        if path in ("/", user_home) or path in masked_roots:
            return
        if any(_is_within(path, m) for m in mounted_ro):
            return
        if os.path.exists(path):
            bwrap_cmd.extend(["--ro-bind", path, path])
            mounted_ro.append(path)

    try:
        py_exe = os.path.abspath(sys.executable)
        py_dir = os.path.dirname(py_exe)
        py_env_root = os.path.dirname(py_dir)
        py_roots = [py_env_root, sys.prefix, sys.base_prefix]
        for cand in py_roots:
            if cand and _hidden_on_host_view(cand, user_home, masked_roots):
                if _norm(cand) in (user_home, "/") or _norm(cand) in masked_roots:
                    if _hidden_on_host_view(py_dir, user_home, masked_roots):
                        _ro_rebind(py_dir)
                    continue
                _ro_rebind(cand)
        real_exe_root = os.path.dirname(os.path.dirname(os.path.realpath(py_exe)))
        if _hidden_on_host_view(real_exe_root, user_home, masked_roots):
            _ro_rebind(real_exe_root)
    except Exception:
        pass

    # If the command executable lives in a hidden location, mount it read-only
    try:
        if command_args:
            cmd0 = str(command_args[0])
            resolved = cmd0 if os.path.isabs(cmd0) else shutil.which(cmd0)
            if resolved:
                resolved = _norm(resolved)
                if _is_within(resolved, user_home):
                    if os.path.isabs(cmd0) and os.path.exists(resolved):
                        _ro_rebind(resolved)
                elif _hidden_on_host_view(resolved, user_home, masked_roots) and os.path.exists(resolved):
                    _ro_rebind(resolved)
                    real_bin = os.path.realpath(resolved)
                    if real_bin != resolved and _hidden_on_host_view(real_bin, user_home, masked_roots) \
                            and not _is_within(real_bin, user_home):
                        _ro_rebind(real_bin)
    except Exception:
        pass

    if extra_ro_binds:
        for extra_path in extra_ro_binds:
            if os.path.exists(extra_path):
                _ro_rebind(extra_path)

    # Re-expose PATH toolchain directories that live under masked data prefixes
    # (never other users' homes or our own HOME, which follows the SAFE list).
    for entry in os.environ.get("PATH", "").split(os.pathsep):
        if not entry or not os.path.isabs(entry):
            continue
        e = _norm(entry)
        if _is_within(e, user_home) or _is_within(e, "/home"):
            continue
        if any(_is_within(e, m) for m in masked_roots) and os.path.isdir(e):
            _ro_rebind(e)

    # repo_root that is an ancestor of the workspace must be mounted first, or
    # its read-only bind would cover the writable workspace.
    repo_needed = bool(repo_abs) and repo_abs not in (mount_root, ws, "/")
    repo_first = repo_needed and _is_within(mount_root, repo_abs)
    if repo_first:
        bwrap_cmd.extend(["--ro-bind", repo_abs, repo_abs])

    # Mount workspace / worktree ("/" is already visible read-only; mounting it
    # again would undo every mask above)
    if mount_root != "/":
        bwrap_cmd.extend(["--ro-bind" if readonly else "--bind", mount_root, mount_root])

    # Bounded scan of the mounted workspace: .git entries (writable sessions) and pathname sockets
    scan_candidates = {mount_root, ws}
    if repo_needed and repo_abs:
        scan_candidates.add(repo_abs)
    scan_roots = []
    for r in sorted((c for c in scan_candidates if c), key=len):
        if not any(_is_within(r, s) for s in scan_roots):
            scan_roots.append(r)
    broad = any(_broad_workspace(r, user_home) for r in scan_roots)
    scan_skip_paths: Set[str] = set()
    if "/" in scan_roots:
        exposed_nonroot = [p for p in (mount_root, ws, repo_abs) if p and p != "/"]
        scan_skip_paths.update(["/proc", "/sys", "/dev"])
        for m in ("/tmp", *SOCKET_RUNTIME_DIRS, *masked_roots, user_home):
            if m and not any(_is_within(p, m) or _is_within(m, p) for p in exposed_nonroot):
                scan_skip_paths.add(m)
    if any(_home_exposed(p, user_home) for p in (mount_root, ws, repo_abs) if p and p != "/"):
        for rel in SENSITIVE_HOME_DIRS:
            scan_skip_paths.add(os.path.join(user_home, rel))
        try:
            for entry in os.listdir(user_home):
                if any(entry.startswith(pfx) for pfx in (".codex", ".claude", ".gemini", ".anthropic", ".openai")):
                    scan_skip_paths.add(os.path.join(user_home, entry))
        except OSError:
            pass
    priority_ws = tuple(
        dict.fromkeys(
            p
            for p in (
                ws,
                repo_abs if repo_needed else None,
                mount_root if not _broad_workspace(mount_root, user_home) else None,
            )
            if p
        )
    )
    unbounded_ws = tuple(
        p for p in dict.fromkeys((*priority_ws, mount_root))
        if p and not _broad_workspace(p, user_home)
    )
    protected_ws = tuple(
        dict.fromkeys(
            p for p in (ws, mount_root, repo_abs if repo_needed else None, user_home) if p
        )
    )
    scan_limit = _workspace_scan_limit(readonly=readonly)
    scan_timeout = _workspace_scan_timeout(readonly=readonly)
    unscanned_dirs: Set[str] = set()
    masked_cache_dirs: Set[str] = set()
    git_paths, ws_sockets, truncated = _scan_tree(
        scan_roots,
        max_depth=BROAD_SCAN_DEPTH if broad else None,
        entry_limit=scan_limit,
        collect_git=not readonly,
        skip_dirs=frozenset(),
        skip_paths=scan_skip_paths,
        descend_git=True,
        timeout_seconds=scan_timeout,
        mask_dirs=_workspace_masked_cache_dirs(),
        priority_paths=priority_ws,
        unbounded_paths=unbounded_ws,
        unscanned_dirs=unscanned_dirs,
        masked_cache_dirs=masked_cache_dirs,
    )
    if not readonly and (truncated or unscanned_dirs):
        raise SandboxConfigError(
            f"工作区扫描未能完成（条目超过 {scan_limit}，或含有当前用户自己的不可读目录），"
            ".git 保护与套接字遮蔽无法确认，拒绝可写沙箱 (fail closed)。"
            "可用 MAKEWAND_SANDBOX_SCAN_ENTRY_LIMIT 调高上限，或恢复相关目录的读取权限。"
        )
    if readonly:
        workspace_tmpfs_masks = _collapse_unscanned_dirs(
            masked_cache_dirs | unscanned_dirs, protected_ws
        )
        if workspace_tmpfs_masks is None:
            raise SandboxConfigError(
                f"工作区扫描未能完成（条目超过 {scan_limit}，或工作区根目录不可读/无法遮蔽未扫描子树），"
                "拒绝只读沙箱 (fail closed)。"
            )
        if truncated:
            _warn(f"工作区扫描未能完成（条目超过 {scan_limit} 或有不可读目录），未扫描子目录已由 tmpfs 遮蔽（只读会话）")
    else:
        collapsed_cache = _collapse_unscanned_dirs(masked_cache_dirs, protected_ws)
        if collapsed_cache is None:
            raise SandboxConfigError("工作区缓存目录遮蔽无法完成，拒绝构建沙箱 (fail closed)。")
        workspace_tmpfs_masks = collapsed_cache

    workspace_tmpfs_set = set(workspace_tmpfs_masks)

    # In writable sessions, protect all root .git directories AND submodule .git gitfiles from tampering
    if not readonly:
        for gp_str in sorted(git_paths):
            if not _covered_by_norm_set(gp_str, workspace_tmpfs_set):
                bwrap_cmd.extend(["--ro-bind", gp_str, gp_str])

    # If repo_root is provided and distinct from mount_root, ensure host repo_root is strictly read-only
    if repo_needed and not repo_first:
        bwrap_cmd.extend(["--ro-bind", repo_abs, repo_abs])

    for mdir in workspace_tmpfs_masks:
        bwrap_cmd.extend(["--tmpfs", mdir])
        readonly_masks.append(mdir)

    # SENSITIVE_HOME_DIRS: a workspace / repo_root mount that contains HOME made
    # it visible again, so re-mask every credential path after those mounts.
    if any(_home_exposed(p, user_home) for p in (mount_root, ws, repo_abs) if p and p != "/"):
        for rel in SENSITIVE_HOME_DIRS:
            sp = os.path.join(user_home, rel)
            if os.path.lexists(sp):
                bwrap_cmd.extend(_mask_mount_args(sp))
        try:
            for entry in os.listdir(user_home):
                if any(entry.startswith(pfx) for pfx in (".codex", ".claude", ".gemini", ".anthropic", ".openai")):
                    if entry not in SENSITIVE_HOME_DIRS:
                        sp = os.path.join(user_home, entry)
                        if os.path.lexists(sp):
                            bwrap_cmd.extend(_mask_mount_args(sp))
        except OSError:
            pass

    # Active provider's state directory (after the workspace so it always wins)
    provider_scan_roots: List[str] = []
    if is_provider and p_name:
        if provider_home is not None and provider_home.provider != p_name:
            raise SandboxConfigError(f"provider home 属于 {provider_home.provider}，不能用于 {p_name}")
        p_args, _, provider_scan_roots = _provider_mounts(
            p_name, user_home, readonly, [ws, mount_root], provider_home=provider_home,
            absent_root_ok=lambda path: _mount_point_creatable(bwrap_cmd, path),
        )
        bwrap_cmd.extend(p_args)

    # Missing foreign roots are safe only outside every writable bind. Check
    # both the spelling and the resolved nearest existing ancestor: an alias
    # into a writable workspace must not permit planting next-run config.
    writable_roots = [user_home]
    for index, argument in enumerate(bwrap_cmd[:-2]):
        if argument in ("--bind", "--bind-try"):
            writable_roots.append(bwrap_cmd[index + 2])

    def foreign_root_creatable(path):
        ancestor = path
        while True:
            try:
                os.stat(ancestor)
                break
            except OSError as error:
                if error.errno not in (errno.ENOENT, errno.ENOTDIR, errno.EACCES):
                    return True  # cannot prove isolation
                parent = os.path.dirname(ancestor)
                if parent == ancestor:
                    return True
                ancestor = parent
        return any(_is_within(candidate, root) for candidate in
                   (path, os.path.realpath(path), ancestor, os.path.realpath(ancestor))
                   for writable in writable_roots
                   for root in (writable, os.path.realpath(writable)))

    # The read-only root bind otherwise exposes other accounts stored outside
    # HOME / the masked prefixes. General sandbox commands need these masks too.
    active_root = None
    if p_name in ACCOUNT_DIR_VARS:
        active_root = _norm(os.path.realpath(os.path.expanduser(
            os.environ.get(ACCOUNT_DIR_VARS[p_name]) or os.path.join(user_home, "." + p_name))))
    for owner, variable in ACCOUNT_DIR_VARS.items():
        value = os.environ.get(variable)
        if owner == p_name or not value:
            continue
        if not _unambiguous_account_path(value):
            raise SandboxConfigError(f"configured {variable} has ambiguous path traversal; refusing account fallback")
        # The child starts in ws. Preserve components until realpath has
        # followed symlinks: abspath/normpath would erase a meaningful "..".
        selected = os.path.expanduser(value)
        if not os.path.isabs(selected):
            selected = os.path.join(ws, selected)
        real_selected = os.path.realpath(selected)
        protected = [ws, mount_root, repo_abs, active_root]
        for path in dict.fromkeys((selected, real_selected)):
            # Skip stale foreign roots only when a sandbox cannot plant state
            # there. Writable HOME/workspace paths remain fail-closed.
            try:
                os.stat(path)
            except OSError as error:
                if (error.errno in (errno.ENOENT, errno.ENOTDIR, errno.EACCES)
                        and not foreign_root_creatable(path)):
                    continue
                raise SandboxConfigError(f"cannot inspect configured {variable}: {error}") from error
            if (_broad_workspace(path, user_home) or
                    any(_is_within(path, os.path.realpath(root)) or
                        _is_within(os.path.realpath(root), path) for root in protected if root)):
                raise SandboxConfigError(f"{variable} must be a dedicated provider state directory")
            if _hidden_on_host_view(path, user_home, masked_roots):
                continue
            mask_args = _mask_mount_args(path)
            bwrap_cmd.extend(mask_args)
            if mask_args[0] == "--tmpfs":
                readonly_masks.append(path)

    # Mask pre-existing pathname AF_UNIX sockets in workspace / provider state to prevent host breakout
    sockets = {
        s for s in ws_sockets
        if not _covered_by_norm_set(s, workspace_tmpfs_set)
    }
    if provider_scan_roots:
        _, p_socks, _ = _scan_tree(
            provider_scan_roots,
            max_depth=PROVIDER_SCAN_DEPTH,
            entry_limit=PROVIDER_SCAN_ENTRY_LIMIT,
            collect_git=False,
            skip_dirs=frozenset(),
        )
        sockets |= p_socks
    for entry_path in sorted(sockets):
        bwrap_cmd.extend(["--ro-bind", "/dev/null", entry_path])

    bwrap_cmd.extend([
        "--chdir", ws,
        "--setenv", "HOME", user_home,
        "--setenv", "TMPDIR", "/tmp",
        "--setenv", "MAKEWAND_SANDBOX", "1",
        "--setenv", "PAGER", "cat",
        "--setenv", "CI", "1",
    ])

    if readonly:
        bwrap_cmd.extend(["--setenv", "MAKEWAND_READONLY", "1"])
    if is_provider:
        # Lets the CLI refuse makewand -> provider CLI -> makewand recursion.
        bwrap_cmd.extend(["--setenv", PROVIDER_WORKER_ENV, "1"])

    if os.environ.get("MAKEWAND_SANDBOX_UNSHARE_NET", "").lower() in ("1", "true", "yes"):
        allow_network = False

    # Strict environment whitelist: only pass safe system variables to general sandbox,
    # pass provider API variables ONLY tailored to the active provider when is_provider is True
    SAFE_PASSTHROUGH_ENVS = [
        "PATH", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "TZ",
        "NODE_PATH", "PYTHONPATH", "PAGER"
    ]
    if allow_network:
        SAFE_PASSTHROUGH_ENVS.extend([
            "http_proxy", "https_proxy", "all_proxy", "no_proxy",
            "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"
        ])
    if is_provider:
        if p_name == "claude":
            SAFE_PASSTHROUGH_ENVS.extend(["ANTHROPIC_API_KEY", "CLAUDE_API_KEY"])
            if os.environ.get("CLAUDE_CONFIG_DIR"):
                bwrap_cmd.extend(["--setenv", "CLAUDE_CONFIG_DIR",
                                  _norm(os.path.expanduser(os.environ["CLAUDE_CONFIG_DIR"]))])
        elif p_name == "codex":
            SAFE_PASSTHROUGH_ENVS.extend(["OPENAI_API_KEY", "CODEX_API_KEY"])
            if os.environ.get("CODEX_HOME"):
                bwrap_cmd.extend(["--setenv", "CODEX_HOME",
                                  _norm(os.path.expanduser(os.environ["CODEX_HOME"]))])
        elif p_name == "grok":
            SAFE_PASSTHROUGH_ENVS.extend(["XAI_API_KEY", "GROK_API_KEY", "GROK_AUTH_TOKEN", "GROK_WEB_FETCH_PROXY"])
        elif p_name == "muse":
            SAFE_PASSTHROUGH_ENVS.extend(["META_API_KEY"])
        elif p_name == "agy":
            SAFE_PASSTHROUGH_ENVS.extend(["GEMINI_API_KEY"])
        elif p_name == "aider":
            SAFE_PASSTHROUGH_ENVS.extend([
                "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY",
                "DEEPSEEK_API_KEY", "OPENROUTER_API_KEY", "AIDER_API_KEY", "AIDER_MODEL",
                "OPENAI_API_BASE", "OLLAMA_API_BASE"
            ])

    passthrough_env = os.environ
    if allow_network:
        from makewand.proxy import proxy_environment
        passthrough_env = proxy_environment(provider=p_name if is_provider else None)
    for var in SAFE_PASSTHROUGH_ENVS:
        if var in passthrough_env:
            bwrap_cmd.extend(["--setenv", var, passthrough_env[var]])

    # Strip any D-Bus and GUI display environment variables to prevent host escape / privilege escalation
    for k in list(os.environ.keys()):
        if "DBUS" in k or k in ("DISPLAY", "XAUTHORITY", "WAYLAND_DISPLAY"):
            bwrap_cmd.extend(["--unsetenv", k])

    if extra_env:
        for k, v in extra_env.items():
            bwrap_cmd.extend(["--setenv", str(k), str(v)])

    if not allow_network:
        bwrap_cmd.append("--unshare-net")

    # Universally mask user-configured protected production trees that lie OUTSIDE user_home
    # (Paths inside user_home are already completely absent thanks to user_home being an isolated tmpfs)
    try:
        from makewand.git_helper import get_protected_paths
        for prod_path in get_protected_paths():
            if prod_path.exists():
                real_prod = str(prod_path.resolve())
                if not real_prod.startswith(user_home) and not (mount_root.startswith(real_prod) or ws.startswith(real_prod)):
                    bwrap_cmd.extend(["--tmpfs", real_prod])
    except Exception:
        pass

    for mask in readonly_masks:
        bwrap_cmd.extend(["--remount-ro", mask])

    bwrap_cmd.extend(command_args)
    return bwrap_cmd


# ---------------------------------------------------------------------------
# MAKEWAND_UNSAFE_HOST_EXEC — aligned with cmd/makewand/hostexec.go
#
# The environment variable is only the *request*. Host execution additionally
# needs the one-time acknowledgment the Go engine records in the shared
# config.json (same keys, same risk-statement version, bound to this hostname),
# so acknowledging once in either frontend covers both. Without a valid
# acknowledgment a non-interactive process is refused; an interactive one gets
# the same responsibility prompt. Every host execution is appended to
# unsafe_exec_audit.jsonl in the config directory.
# ---------------------------------------------------------------------------
UNSAFE_HOST_EXEC_ENV = "MAKEWAND_UNSAFE_HOST_EXEC"
# Must equal internal/config.UnsafeHostExecAckCurrentVersion.
UNSAFE_HOST_EXEC_ACK_CURRENT_VERSION = 1
# Must equal cmd/makewand/hostexec.go unsafeHostExecAuditFile.
UNSAFE_HOST_EXEC_AUDIT_FILE = "unsafe_exec_audit.jsonl"
# JSON keys of internal/config.Config (json tags).
ACK_VERSION_KEY = "unsafe_host_exec_ack_version"
ACK_AT_KEY = "unsafe_host_exec_ack_at"
ACK_HOST_KEY = "unsafe_host_exec_ack_host"

_UNSAFE_ACK_PROMPT = (
    "检测到 MAKEWAND_UNSAFE_HOST_EXEC=1。\n\n"
    "这会关闭沙箱隔离：AI 生成的命令（依赖安装、测试、自动修复重试、预览脚本）将以你的用户账户和环境【直接在本机执行】。"
    "恶意或有缺陷的生成命令可以读写你的文件、使用你的凭据。你需要对这些命令的行为承担全部责任。\n\n"
    "本次一次性确认将记录在本机的 makewand 配置中（风险声明 v%d）。每一次宿主执行都会写入审计日志。\n\n"
    "输入 \"yes\" 接受，输入其他内容拒绝："
)
_UNSAFE_ACK_CONFIRMED = "已确认并记录不安全宿主执行授权。每次宿主执行都会被审计。"
_UNSAFE_ACK_DECLINED = "已拒绝确认。MAKEWAND_UNSAFE_HOST_EXEC=1 将被忽略；命令只会在沙箱隔离内执行（无沙箱时拒绝执行）。"
_UNSAFE_ACK_NON_INTERACTIVE = (
    "已设置 MAKEWAND_UNSAFE_HOST_EXEC=1，但本机尚未完成一次性宿主执行确认。已拒绝宿主执行（fail closed）。"
    "请交互式运行一次 makewand（如 `makewand setup`）完成确认。"
)
_UNSAFE_ACTIVE_WARNING = "警告：MAKEWAND_UNSAFE_HOST_EXEC=1——AI 生成的命令将不经沙箱隔离直接在本机执行（已于 %s 确认）。执行记录审计至 %s。"
_UNSAFE_ACK_SAVE_FAILED = "无法记录确认（%s）；宿主执行保持禁用（fail closed）。请修复配置后重试。"

_host_exec_session: Dict[str, bool] = {"warned": False, "declined": False}


def _makewand_config_dir() -> Path:
    from makewand import config as _cfg
    return Path(_cfg.CONFIG_DIR)


def unsafe_host_exec_audit_path() -> Path:
    return _makewand_config_dir() / UNSAFE_HOST_EXEC_AUDIT_FILE


def _local_hostname() -> Optional[str]:
    try:
        host = socket.gethostname()
    except OSError:
        return None
    return host or None


def _load_shared_config() -> Tuple[Optional[dict], Optional[str]]:
    """Reads the shared config.json. Returns (cfg, error); cfg is None on read/parse failure."""
    path = _makewand_config_dir() / "config.json"
    try:
        raw = path.read_text(encoding="utf-8")
    except FileNotFoundError:
        return {}, None
    except OSError as exc:
        return None, f"read config: {exc}"
    try:
        data = json.loads(raw)
    except ValueError as exc:
        return None, f"parse config: {exc}"
    if not isinstance(data, dict):
        return None, "parse config: not a JSON object"
    return data, None


def unsafe_host_exec_ack_valid(cfg: Optional[dict]) -> bool:
    """Mirror of internal/config Config.UnsafeHostExecAckValid (version + host-bound, fail closed)."""
    if not isinstance(cfg, dict):
        return False
    version = cfg.get(ACK_VERSION_KEY)
    if isinstance(version, bool) or not isinstance(version, int) or version < UNSAFE_HOST_EXEC_ACK_CURRENT_VERSION:
        return False
    host = cfg.get(ACK_HOST_KEY)
    if not isinstance(host, str) or not host.strip():
        return False
    local = _local_hostname()
    return local is not None and host == local


def _record_unsafe_host_exec_ack() -> Optional[str]:
    """
    Durably records the acknowledgment on a freshly loaded config.json (all
    other keys preserved, atomic replace). Returns an error string on any
    failure — the caller must then refuse host execution (fail closed).
    """
    host = _local_hostname()
    if not host:
        return "resolve hostname for unsafe host exec acknowledgment failed"
    cfg, err = _load_shared_config()
    if cfg is None:
        return err or "config unavailable"
    cfg[ACK_VERSION_KEY] = UNSAFE_HOST_EXEC_ACK_CURRENT_VERSION
    cfg[ACK_AT_KEY] = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    cfg[ACK_HOST_KEY] = host
    cfg_dir = _makewand_config_dir()
    try:
        cfg_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        fd, tmp = tempfile.mkstemp(prefix=".config.json.", dir=str(cfg_dir))
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as fh:
                json.dump(cfg, fh, ensure_ascii=False, indent=2)
                fh.write("\n")
                fh.flush()
                os.fsync(fh.fileno())
            os.chmod(tmp, 0o600)
            os.replace(tmp, cfg_dir / "config.json")
        except BaseException:
            try:
                os.unlink(tmp)
            except OSError:
                pass
            raise
    except OSError as exc:
        return f"save config: {exc}"
    return None


def _stdio_interactive() -> bool:
    try:
        return os.isatty(sys.stdin.fileno()) and os.isatty(sys.stdout.fileno())
    except (AttributeError, OSError, ValueError):
        return False


def resolve_unsafe_host_exec() -> Tuple[bool, Optional[str]]:
    """
    Turns the MAKEWAND_UNSAFE_HOST_EXEC=1 request into an authorization.
    Returns (authorized, source) where source is "config-ack" or "interactive-ack".
    The environment variable alone never authorizes host execution.
    """
    if os.environ.get(UNSAFE_HOST_EXEC_ENV) != "1":
        return False, None
    cfg, _ = _load_shared_config()
    if cfg is not None and unsafe_host_exec_ack_valid(cfg):
        if not _host_exec_session["warned"]:
            _host_exec_session["warned"] = True
            _warn(_UNSAFE_ACTIVE_WARNING % (cfg.get(ACK_AT_KEY, "?"), unsafe_host_exec_audit_path()))
        return True, "config-ack"
    if _host_exec_session["declined"]:
        return False, None
    if not _stdio_interactive():
        _warn(_UNSAFE_ACK_NON_INTERACTIVE)
        return False, None
    try:
        sys.stderr.write(_UNSAFE_ACK_PROMPT % UNSAFE_HOST_EXEC_ACK_CURRENT_VERSION)
        sys.stderr.flush()
        line = sys.stdin.readline()
    except Exception:
        line = ""
    if (line or "").strip().lower() != "yes":
        _host_exec_session["declined"] = True
        _warn(_UNSAFE_ACK_DECLINED)
        return False, None
    err = _record_unsafe_host_exec_ack()
    if err:
        _warn(_UNSAFE_ACK_SAVE_FAILED % err)
        return False, None
    _host_exec_session["warned"] = True
    _warn(_UNSAFE_ACK_CONFIRMED)
    return True, "interactive-ack"


def is_unsafe_host_exec_authorized() -> bool:
    """Gate helper for callers that only need the decision (e.g. untrusted-repo checks)."""
    return resolve_unsafe_host_exec()[0]


def audit_unsafe_host_exec(context: str, cmd, cwd: str, source: Optional[str]) -> None:
    """Appends one host-execution record (same fields as the Go audit log). Never blocks execution."""
    try:
        from makewand.config import ensure_private_dir
        cfg_dir = ensure_private_dir(_makewand_config_dir())
        if isinstance(cmd, (list, tuple)):
            command = str(cmd[0]) if cmd else ""
            args = [str(a) for a in cmd[1:]]
        else:
            command, args = str(cmd), []
        entry = {
            "time": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "context": context,
            "command": command,
        }
        if args:
            entry["args"] = args
        entry["dir"] = cwd
        entry["source"] = source or ""
        line = json.dumps(entry, ensure_ascii=False, separators=(",", ":")) + "\n"
        audit_path = cfg_dir / UNSAFE_HOST_EXEC_AUDIT_FILE
        if audit_path.is_symlink():
            raise PermissionError("refusing symlinked host execution audit file")
        flags = (os.O_APPEND | os.O_CREAT | os.O_WRONLY | getattr(os, "O_NOFOLLOW", 0)
                 | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_BINARY", 0))
        fd = os.open(str(audit_path), flags, 0o600)
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
                raise PermissionError("host execution audit must be a regular file with one link")
            if os.name == "nt":
                from makewand.native_windows import ensure_private_file_descriptor
                ensure_private_file_descriptor(fd)
            else:
                if hasattr(os, "getuid") and info.st_uid != os.getuid():
                    raise PermissionError("host execution audit file is owned by another user")
                if stat.S_IMODE(info.st_mode) != 0o600:
                    os.fchmod(fd, 0o600)
            payload = line.encode("utf-8")
            while payload:
                written = os.write(fd, payload)
                if written <= 0:
                    raise OSError("host execution audit write made no progress")
                payload = payload[written:]
        finally:
            os.close(fd)
    except Exception as exc:
        _warn(f"unsafe host exec audit write failed: {exc}")


def get_writable_sandbox_guidance(provider_name: str = "") -> str:
    """
    Returns platform-specific actionable guidance when a writable task cannot run in sandbox.
    """
    prov_label = f"{provider_name.capitalize()} " if provider_name else ""
    if sys.platform == "darwin":
        return (
            f"❌ [{prov_label}沙箱不可用] {prov_label}写入任务强制要求隔离沙箱，但 Bubblewrap (bwrap) 依赖 Linux 内核命名空间，macOS 无法运行 bwrap。\n"
            "在 macOS 上继续使用 Makewand 的建议途径（没有沙箱时，只读审查 makewand review 同样需要下面的授权）：\n"
            "  1. [Linux 容器/虚拟机] 在装有 Bubblewrap 的 Linux Docker 容器或虚拟机中运行 makewand；\n"
            "  2. [受权宿主执行（自担风险）] 若当前工作区完全受信任且知晓安全风险，可显式授权直接在宿主机执行：\n"
            "     配置 MAKEWAND_UNSAFE_HOST_EXEC=1 并通过终端交互输入 'yes' 授权确认（或预置 config.json 授权）；\n"
            "     所有无沙箱宿主执行操作均将被永久记入 ~/.config/makewand/unsafe_exec_audit.jsonl 审计日志。"
        )
    else:
        return (
            f"❌ [{prov_label}沙箱未就绪] {prov_label}写入任务强制要求 Bubblewrap (bwrap) 沙箱隔离，系统未检测到可用 bwrap 环境 (fail closed)。\n"
            "解决建议：\n"
            "  1. [安装沙箱] 安装 Bubblewrap 并启用非特权用户命名空间 (例如: sudo apt install bubblewrap，sysctl kernel.unprivileged_userns_clone=1)；"
            "bwrap 只从 root 拥有的系统目录解析，装在其他位置时用 MAKEWAND_BWRAP_PATH 指定绝对路径；\n"
            "  2. [受权宿主执行（自担风险）] 若当前工作区受信任且知晓安全风险，可配置 MAKEWAND_UNSAFE_HOST_EXEC=1 并通过交互确认授权（所有执行将记入审计日志；没有沙箱时只读审查同样需要此授权）。"
        )


def verify_writable_sandbox_or_authorized(
    provider_name: str = "",
    repo_trust: str = "trusted",
) -> Tuple[bool, Optional[str], Optional[str]]:
    """
    Validates whether a writable / code-modifying execution is permitted.
    Returns (ok, auth_source, error_message).
    - If bwrap is available: returns (True, None, None).
    - If untrusted repo: fail closed (returns False, None, error_message).
    - If bwrap unavailable on trusted repo:
      - If MAKEWAND_UNSAFE_HOST_EXEC=1 is authorized (config-ack or interactive-ack):
        returns (True, auth_source, None).
      - Otherwise:
        returns (False, None, get_writable_sandbox_guidance(provider_name)).
    """
    if is_bwrap_available():
        return True, None, None

    if repo_trust == "untrusted":
        return False, None, "不可信仓库 (--repo-trust=untrusted) 强制要求 Bubblewrap 物理沙箱隔离，未检测到 bwrap，拒绝执行"

    authorized, source = resolve_unsafe_host_exec()
    if authorized:
        return True, source, None

    return False, None, get_writable_sandbox_guidance(provider_name)


class SandboxUnavailableError(SandboxConfigError):
    """bwrap is unavailable and unsandboxed host execution is not authorized (fail closed)."""


_UNTRUSTED_NO_BWRAP = "不可信仓库 (--repo-trust=untrusted) 强制要求 Bubblewrap 物理沙箱隔离，未检测到 bwrap，拒绝执行"


def get_readonly_sandbox_guidance(provider_name: str = "") -> str:
    """Guidance when a read-only provider task cannot run in the sandbox."""
    prov_label = f"{provider_name.capitalize()} " if provider_name else ""
    reason = ("macOS 无法运行 Bubblewrap (bwrap)" if sys.platform == "darwin"
              else "系统未检测到可用 Bubblewrap (bwrap) 环境")
    return (
        f"❌ [{prov_label}沙箱未就绪] {prov_label}只读任务同样要求沙箱隔离：provider CLI 以宿主用户权限运行，"
        f"只读约束只是 CLI 自身的工具策略，{reason} (fail closed)。\n"
        "解决建议：\n"
        "  1. [安装沙箱] Linux 上安装 Bubblewrap 并启用非特权用户命名空间；\n"
        "  2. [API 模式] 配置对应 provider 的 API Key 并设置 MAKEWAND_API_POLICY=allow_paid（按量计费），由无工具的 API 调用完成审查；\n"
        "  3. [受权宿主执行（自担风险）] 配置 MAKEWAND_UNSAFE_HOST_EXEC=1 并通过交互确认授权（所有执行将记入审计日志）。"
    )


def authorize_unsandboxed_launch(
    provider: str,
    cmd,
    cwd: str,
    readonly: bool,
    repo_trust: str = "trusted",
    auth_source: Optional[str] = None,
    context: Optional[str] = None,
) -> Optional[str]:
    """
    Gate for EVERY launch that runs without bubblewrap, read-only included (H1).
    Requires the MAKEWAND_UNSAFE_HOST_EXEC=1 + one-time acknowledgment
    authorization (auth_source when the caller already resolved it) and audits
    the execution. Returns the auth source; raises SandboxUnavailableError.
    """
    if repo_trust == "untrusted":
        raise SandboxUnavailableError(_UNTRUSTED_NO_BWRAP)
    if not auth_source:
        authorized, auth_source = resolve_unsafe_host_exec()
        if not authorized:
            raise SandboxUnavailableError(
                get_readonly_sandbox_guidance(provider) if readonly else get_writable_sandbox_guidance(provider)
            )
    audit_unsafe_host_exec(context or (f"provider:{provider}" if provider else "sandbox"), cmd, cwd, auth_source)
    return auth_source


# ---------------------------------------------------------------------------
# resource limits for sandboxed launches (no preexec_fn: CON-01 fork safety)
# ---------------------------------------------------------------------------
SANDBOX_MEMORY_MAX_ENV = "MAKEWAND_SANDBOX_MEMORY_MAX"
SANDBOX_TASKS_MAX_ENV = "MAKEWAND_SANDBOX_TASKS_MAX"
DEFAULT_SANDBOX_MEMORY_MAX = "8G"
DEFAULT_SANDBOX_TASKS_MAX = "512"
DEFAULT_SANDBOX_NOFILE = 4096
TRUSTED_SYSTEM_BIN_DIRS = ("/usr/bin", "/bin", "/usr/local/bin", "/run/current-system/sw/bin")
_MEM_RE = re.compile(r"^(\d+[KMGT]?|infinity)$")
_TASKS_RE = re.compile(r"^(\d+|infinity)$")
_limits_probe: Dict[str, Any] = {"systemd": None, "warned": False}


def _trusted_system_binary(name: str) -> Optional[str]:
    """Root-owned system binary (never resolved from $PATH)."""
    for d in TRUSTED_SYSTEM_BIN_DIRS:
        cand = os.path.join(d, name)
        if not os.path.lexists(cand):
            continue
        real = os.path.realpath(cand)
        if os.path.isfile(real) and os.access(real, os.X_OK) and _trusted_launcher_ok(real, False) \
                and _trusted_launcher_ok(os.path.dirname(real), False):
            return real
    return None


def _env_limit(env: str, default: str, pattern) -> str:
    val = os.environ.get(env, "").strip() or default
    # Same contract as the Go engine (internal/processjob): 0/off/none mean no
    # cap, and 8g / 8GiB style sizes are accepted.
    if val.lower() in ("0", "off", "none", "infinity"):
        return "infinity"
    size = re.match(r"^(\d+)\s*([kmgt])(?:i?b)?$", val, re.IGNORECASE)
    if size:
        val = size.group(1) + size.group(2).upper()
    if not pattern.match(val):
        _warn(f"{env}={val!r} 格式无效，使用默认值 {default}")
        return default
    return val


def _systemd_scope_usable(systemd_run: str) -> bool:
    cached = _limits_probe.get("systemd")
    if cached is not None:
        return bool(cached)
    ok = False
    try:
        import subprocess
        res = subprocess.run(
            [systemd_run, "--user", "--scope", "--quiet", "--collect", "-p", "TasksMax=16", "--", "true"],
            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10,
        )
        ok = res.returncode == 0
    except Exception:
        ok = False
    _limits_probe["systemd"] = ok
    return ok


def _rlimit_arg(resource_mod, which: int, want: int) -> str:
    """soft:hard with hard kept at the current hard limit (an unprivileged prlimit cannot raise it)."""
    _soft, hard = resource_mod.getrlimit(which)
    unlimited = hard == resource_mod.RLIM_INFINITY or hard < 0
    cap = want if unlimited else min(want, hard)
    return f"{cap}:{'unlimited' if unlimited else hard}"


def sandbox_resource_limit_prefix() -> List[str]:
    """
    Launcher prefix that bounds a sandboxed process tree: a transient systemd
    user scope (MemoryMax / MemorySwapMax=0 / TasksMax) when available, else
    prlimit --nofile/--fsize (never --as: torch/BLAS reserve huge virtual
    ranges; never --nproc: RLIMIT_NPROC counts every thread of the uid, so
    any relative budget breaks bwrap on busy hosts), else nothing (warned once).
    """
    if os.name != "posix":
        return []
    mem = _env_limit(SANDBOX_MEMORY_MAX_ENV, DEFAULT_SANDBOX_MEMORY_MAX, _MEM_RE)
    tasks = _env_limit(SANDBOX_TASKS_MAX_ENV, DEFAULT_SANDBOX_TASKS_MAX, _TASKS_RE)
    systemd_run = _trusted_system_binary("systemd-run")
    if systemd_run and _systemd_scope_usable(systemd_run):
        return [systemd_run, "--user", "--scope", "--quiet", "--collect",
                "-p", f"MemoryMax={mem}", "-p", "MemorySwapMax=0", "-p", f"TasksMax={tasks}", "--"]
    if systemd_run:
        logging.getLogger(__name__).debug("systemd user scope unavailable; falling back to prlimit")
    prlimit = _trusted_system_binary("prlimit")
    if prlimit:
        try:
            import resource
            fsize = int(os.environ.get("MAKEWAND_SANDBOX_RLIMIT_FSIZE", 1024 * 1024 * 1024))
            prefix = [prlimit]
            prefix.append(f"--nofile={_rlimit_arg(resource, resource.RLIMIT_NOFILE, DEFAULT_SANDBOX_NOFILE)}")
            prefix.append(f"--fsize={_rlimit_arg(resource, resource.RLIMIT_FSIZE, fsize)}")
            return prefix + ["--"]
        except Exception as exc:
            logging.getLogger(__name__).debug("prlimit prefix unavailable: %s", exc)
    if not _limits_probe["warned"]:
        _limits_probe["warned"] = True
        _warn("systemd-run 与 prlimit 均不可用，沙箱进程未施加内存/进程数资源上限")
    return []


@dataclass
class ProviderLaunch:
    cmd: List[str]
    pass_fds: tuple
    sandboxed: bool
    auth_source: Optional[str] = None
    home: Optional["ProviderHome"] = None
    # Extra environment for the launched process (run_subprocess extra_env).
    env: Dict[str, str] = field(default_factory=dict)


class ProviderAccountChangedError(RuntimeError):
    """A dispatched host CLI changed login; its remote outcome is unknown."""


_SOCKET_PROBE = (
    "import socket,sys\n"
    "errors=[]\n"
    "for name in ('AF_INET','AF_INET6'):\n"
    " family=getattr(socket,name,None)\n"
    " if family is None: continue\n"
    " try:\n"
    "  with socket.socket(family,socket.SOCK_STREAM): pass\n"
    " except OSError as error: errors.append(name+': '+str(error))\n"
    " else: sys.exit(0)\n"
    "sys.stderr.write('network socket unavailable: '+'; '.join(errors)+'\\n')\n"
    "sys.exit(77)\n"
)


def _provider_socket_preflight(command, provider_command, *, cwd, allow_network,
                               pass_fds=(), seccomp_fd=None, extra_env=None):
    """Check TCP socket creation in the actual launch environment, without a model.

    No DNS lookup, connection, provider CLI or credential request is made.
    A fresh seccomp pipe is necessary: bubblewrap consumes its input pipe.
    The actual provider's filter must remain unread until its own launch.
    """
    if not allow_network or os.environ.get("MAKEWAND_SANDBOX_UNSHARE_NET", "").lower() in ("1", "true", "yes"):
        return
    if not provider_command or list(command[-len(provider_command):]) != list(provider_command):
        raise ProviderStartupError("启动环境检查失败：无法隔离 provider 命令，拒绝执行探针")
    from makewand.execution_runtime import auxiliary_process_scope, current_context
    remaining = current_context().get("_deadline_monotonic")
    timeout = min(3.0, max(0, remaining - time.monotonic())) if remaining is not None else 3.0
    if timeout <= 0:
        raise ProviderStartupError("启动环境检查失败：socket 检查前派发期限已到，模型尚未启动")
    interpreter = _trusted_system_binary("python3") or os.path.realpath(sys.executable)
    filter_scope = (seccomp_filter_context(enabled=True, required=True) if seccomp_fd is not None
                    else contextlib.nullcontext((None, ())))
    with contextlib.ExitStack() as stack:
        try:
            probe_fd, probe_fds = stack.enter_context(filter_scope)
        except SandboxConfigError as error:
            raise ProviderStartupError(f"启动环境检查失败：无法准备 socket 探针的 seccomp 隔离 ({error})") from error
        prefix = list(command[:-len(provider_command)])
        if seccomp_fd is not None:
            try:
                index = prefix.index("--seccomp")
                if prefix[index + 1] != str(seccomp_fd):
                    raise ValueError("different filter")
                prefix[index + 1] = str(probe_fd)
            except (ValueError, IndexError) as error:
                raise ProviderStartupError("启动环境检查失败：无法保留 seccomp 隔离，拒绝执行探针") from error
        probe = prefix + [interpreter, "-I", "-S", "-c", _SOCKET_PROBE]
        descriptors = tuple(fd for fd in pass_fds if fd != seccomp_fd) + tuple(probe_fds)
        with auxiliary_process_scope():
            code, out, err, error = run_subprocess(probe, timeout=timeout, cwd=cwd,
                                                   pass_fds=descriptors, extra_env=extra_env,
                                                   separate_stderr=True)
    if code != 0 or error is not None:
        detail = str(err or error or out or f"exit {code}").strip()[:512]
        raise ProviderStartupError(
            f"启动环境检查失败：socket 能力探针未通过，模型尚未启动 ({detail})"
        )


@contextmanager
def provider_sandbox_launch(
    provider: str,
    cmd: List[str],
    cwd: str,
    *,
    readonly: bool,
    repo_root: Optional[str] = None,
    repo_trust: str = "trusted",
    allow_network: bool = True,
    extra_ro_binds: Optional[List[str]] = None,
    extra_env: Optional[dict] = None,
    host_auth_source: Optional[str] = None,
    apply_limits: bool = True,
):
    """
    Single launch path for provider CLIs. With bwrap: mandatory seccomp, private
    provider home (credentials written back on exit) and resource limits.
    Without bwrap: authorize_unsandboxed_launch (read-only too) + audit.
    Yields ProviderLaunch; raises SandboxConfigError / SandboxUnavailableError.
    """
    from makewand.proxy import proxy_environment
    proxy_env = proxy_environment(provider=provider) if allow_network else dict(os.environ)
    launch_env = {key: value for key, value in proxy_env.items() if os.environ.get(key) != value}
    launch_env.update(extra_env or {})
    if not is_bwrap_available():
        if _is_agy_provider(provider) and not _agy_host_credits_disabled():
            raise SandboxUnavailableError(_agy_no_sandbox_credits_message())
        source = authorize_unsandboxed_launch(provider, cmd, cwd, readonly, repo_trust, host_auth_source)
        # Passed through the process environment rather than an `env` prefix,
        # which does not exist on Windows.
        if provider in ("claude", "codex"):
            # Keep the user's configuration, instructions and hooks (and
            # Claude's Keychain namespace). Check account attribution on exit.
            from makewand.account_identity import credential_binding, account_identity, refuse_credential_launch
            from makewand.execution_runtime import current_context
            binding = current_context().get("_credential_binding")
            selected_binding = credential_binding(provider)
            if binding is not None and binding != selected_binding:
                refuse_credential_launch("selected login changed after admission; refusing account switch")
            selected_account = account_identity(provider)
            variable = ACCOUNT_DIR_VARS[provider]
            child_home = launch_env.get("HOME") or str(Path.home())
            child_root = (launch_env.get(variable) if variable in launch_env else os.environ.get(variable))
            child_root = child_root or os.path.join(child_home, "." + provider)
            if not _unambiguous_account_path(child_root):
                refuse_credential_launch(f"configured {variable} has ambiguous path traversal; refusing account fallback")
            child_root = os.path.expanduser(child_root)
            if not os.path.isabs(child_root):
                child_root = os.path.join(cwd, child_root)
            if (os.path.realpath(child_root) != selected_account["credential_root"] or
                    os.path.realpath(child_home) != os.path.realpath(str(Path.home()))):
                refuse_credential_launch("credential source differs from admitted account")
            try:
                _provider_socket_preflight(cmd, cmd, cwd=cwd, allow_network=allow_network,
                                           extra_env=launch_env)
                yield ProviderLaunch(list(cmd), (), False, source,
                    env={**launch_env, PROVIDER_WORKER_ENV: "1"})
            finally:
                try:
                    changed = selected_account != account_identity(provider)
                    if not selected_account["identity_known"]:
                        changed = changed or selected_binding != credential_binding(provider)
                except (OSError, ValueError, RuntimeError):
                    changed = True
                if changed:
                    raise ProviderAccountChangedError("selected login changed during direct execution")
        else:
            _provider_socket_preflight(cmd, cmd, cwd=cwd, allow_network=allow_network,
                                       extra_env=launch_env)
            yield ProviderLaunch(list(cmd), (), False, source, env={**launch_env, PROVIDER_WORKER_ENV: "1"})
        return
    if _is_agy_provider(provider) and extra_env:
        # Never point agy away from the sanitized ~/.gemini settings.
        extra_env = {k: v for k, v in extra_env.items() if k.upper() not in AGY_CONFIG_DIR_ENVS}
    with seccomp_filter_context(enabled=True) as (seccomp_fd, pass_fds):
        with provider_home_session(provider, [cwd, repo_root or ""]) as home:
            exec_cmd = wrap_bwrap(
                cmd,
                workspace=cwd,
                allow_network=allow_network,
                readonly=readonly,
                repo_root=repo_root,
                is_provider=True,
                provider_name=provider,
                extra_ro_binds=extra_ro_binds,
                extra_env=extra_env,
                seccomp_fd=seccomp_fd,
                provider_home=home,
            )
            if apply_limits:
                exec_cmd = sandbox_resource_limit_prefix() + list(exec_cmd)
            _provider_socket_preflight(exec_cmd, cmd, cwd=cwd, allow_network=allow_network,
                                       pass_fds=pass_fds, seccomp_fd=seccomp_fd)
            yield ProviderLaunch(list(exec_cmd), pass_fds, True, None, home)


def apply_posix_sandbox_rlimits():
    """
    In-process POSIX rlimits (RLIMIT_AS, RLIMIT_NPROC, RLIMIT_FSIZE) for code
    already running inside a sandbox. Launchers must not call it via
    preexec_fn (fork safety, CON-01); they use sandbox_resource_limit_prefix().
    """
    if os.name != "posix":
        return
    try:
        import resource
    except ImportError:
        return

    # RLIMIT_FSIZE: Max file size writable by process (default: 1 GiB)
    try:
        fsize_bytes = int(os.environ.get("MAKEWAND_SANDBOX_RLIMIT_FSIZE", 1024 * 1024 * 1024))
        cur_soft, cur_hard = resource.getrlimit(resource.RLIMIT_FSIZE)
        if cur_hard == resource.RLIM_INFINITY or cur_hard < 0:
            soft = fsize_bytes
        else:
            soft = min(fsize_bytes, cur_hard)
        resource.setrlimit(resource.RLIMIT_FSIZE, (soft, cur_hard))
    except Exception:
        pass

    # RLIMIT_AS: Max virtual address space (default: 16 GiB)
    try:
        as_bytes = int(os.environ.get("MAKEWAND_SANDBOX_RLIMIT_AS", 16 * 1024 * 1024 * 1024))
        cur_soft, cur_hard = resource.getrlimit(resource.RLIMIT_AS)
        if cur_hard == resource.RLIM_INFINITY or cur_hard < 0:
            soft = as_bytes
        else:
            soft = min(as_bytes, cur_hard)
        resource.setrlimit(resource.RLIMIT_AS, (soft, cur_hard))
    except Exception:
        pass

    # RLIMIT_NPROC: Max number of processes/threads for UID (default: 65536)
    try:
        nproc_limit = int(os.environ.get("MAKEWAND_SANDBOX_RLIMIT_NPROC", 65536))
        cur_soft, cur_hard = resource.getrlimit(resource.RLIMIT_NPROC)
        effective_limit = min(nproc_limit, cur_soft) if cur_soft > 0 else nproc_limit
        if cur_hard == resource.RLIM_INFINITY or cur_hard < 0:
            soft = effective_limit
        else:
            soft = min(effective_limit, cur_hard)
        resource.setrlimit(resource.RLIMIT_NPROC, (soft, cur_hard))
    except Exception:
        pass


def _collect_uncreated_sensitive_files(
    is_provider: bool = False,
    provider_name: Optional[str] = None,
    cmd: Optional[List[str]] = None,
    readonly: bool = False,
) -> List[str]:
    """Find sensitive provider files that currently do not exist on the host."""
    user_home = _norm(str(Path.home()))
    targets: List[str] = []

    p_names: List[str] = []
    if is_provider:
        p = (provider_name or "").lower().strip()
        if p == "gemini":
            p = "agy"
        if not p and cmd:
            first_bin = os.path.basename(str(cmd[0])).lower()
            if first_bin == "gemini" or first_bin.startswith("gemini-") or first_bin.startswith("gemini."):
                p = "agy"
            else:
                for cand in ["claude", "codex", "agy", "muse", "grok", "aider"]:
                    if first_bin == cand or first_bin.startswith(cand + "-") or first_bin.startswith(cand + "."):
                        p = cand
                        break
        if p:
            p_names.append(p)

    for p_name in p_names:
        prof = PROVIDER_PROFILES.get(p_name)
        if not prof:
            continue
        if readonly and prof.get("tmpfs_readonly_root"):
            continue
        roots: List[Tuple[str, str]] = []
        if p_name == "codex":
            codex_home = os.environ.get("CODEX_HOME")
            selected = os.path.realpath(_norm(os.path.expanduser(codex_home or os.path.join(user_home, ".codex"))))
            if os.path.isdir(selected):
                roots.append((".codex", selected))
        else:
            for r in prof.get("roots", []):
                rp = os.path.realpath(os.path.join(user_home, r))
                if os.path.isdir(rp):
                    roots.append((r, rp))

        for root_rel, real_root in roots:
            for entry in prof.get("protected", []):
                e_root, sub, _, placeholder = entry[0], entry[1], entry[2], entry[3]
                if e_root == root_rel or (p_name == "codex" and e_root == ".codex"):
                    if placeholder is None:
                        p = os.path.join(real_root, sub)
                        if not os.path.lexists(p):
                            targets.append(p)

    return list(dict.fromkeys(targets))


@dataclass
class _SensitiveFileSnapshot:
    paths: List[str]
    initial_inodes: Dict[str, Optional[Tuple[int, int]]]
    start_time: float
    readonly: bool = False


def _create_sensitive_snapshot(
    paths: Sequence[str],
    readonly: bool = False,
) -> _SensitiveFileSnapshot:
    initial_inodes: Dict[str, Optional[Tuple[int, int]]] = {}
    for p in paths:
        if os.path.lexists(p):
            try:
                st = os.lstat(p)
                initial_inodes[p] = (st.st_dev, st.st_ino)
            except OSError:
                initial_inodes[p] = None
        else:
            initial_inodes[p] = None
    return _SensitiveFileSnapshot(
        paths=list(paths),
        initial_inodes=initial_inodes,
        start_time=time.time(),
        readonly=readonly,
    )


def _robust_force_remove_inode_tracked(
    path: str,
    expected_dev_ino: Optional[Tuple[int, int]] = None,
) -> None:
    """
    Removes a file or a directory tree, checking dev/ino of path and making
    directories accessible first; symlinks are removed, never followed. The
    walk is iterative: a tree nested deeper than the recursion limit (a
    sandboxed process can create one in its private home) cannot make the
    caller raise.
    """
    try:
        st = os.lstat(path)
    except OSError:
        return
    if expected_dev_ino is not None and (st.st_dev, st.st_ino) != expected_dev_ino:
        return
    stack = [(path, stat.S_ISDIR(st.st_mode), False)]  # (path, is a directory, children queued)
    while stack:
        p, is_dir, queued = stack.pop()
        if not is_dir:
            try:
                os.unlink(p)
            except OSError:
                pass
            continue
        if queued:
            try:
                os.rmdir(p)
            except OSError:
                pass
            continue
        try:
            os.chmod(p, stat.S_IRWXU)
        except OSError:
            pass
        stack.append((p, True, True))
        try:
            entries = os.listdir(p)
        except OSError:
            continue
        for entry in entries:
            child = os.path.join(p, entry)
            try:
                stack.append((child, stat.S_ISDIR(os.lstat(child).st_mode), False))
            except OSError:
                continue


def _robust_force_remove(path: str) -> None:
    """Recursively removes a file or directory, adjusting read-only permissions if needed."""
    _robust_force_remove_inode_tracked(path, None)


def _cleanup_uncreated_sensitive_files(
    target: Union[Sequence[str], _SensitiveFileSnapshot],
) -> None:
    """
    Removes sensitive files that were created during sandbox execution.
    Guards against deleting files that existed before sandbox execution, files created
    by concurrent host sessions in readonly tasks, or inodes replaced after creation.
    """
    if isinstance(target, _SensitiveFileSnapshot):
        snapshot = target
    else:
        snapshot = _create_sensitive_snapshot(target, readonly=False)

    # In readonly mode with whole-root ro-bind, Bubblewrap mounted the state root read-only,
    # so the sandboxed process could never create or write to any sensitive provider file.
    # Any sensitive path present on host was created by an external/concurrent host session;
    # never delete in readonly mode.
    if snapshot.readonly:
        return

    for p in snapshot.paths:
        # Pre-existing paths must NEVER be deleted
        if snapshot.initial_inodes.get(p) is not None:
            continue

        if not os.path.lexists(p):
            continue

        try:
            st = os.lstat(p)
        except OSError:
            continue

        # Files created before the sandbox started must not be deleted
        if snapshot.start_time > 0 and st.st_ctime < snapshot.start_time - 1.0:
            continue

        expected_dev_ino = (st.st_dev, st.st_ino)
        try:
            _robust_force_remove_inode_tracked(p, expected_dev_ino)
        except OSError:
            pass


@contextmanager
def sandbox_lifecycle(
    is_provider: bool = False,
    provider_name: Optional[str] = None,
    cmd: Optional[List[str]] = None,
    readonly: bool = False,
):
    """
    Manages safe provider sandbox lifecycle.
    Tracks uncreated sensitive provider files before execution and ensures any that
    are created during execution are cleanly removed on completion.
    Guards against concurrent host file deletion and abnormal signal termination.
    """
    tracked = _collect_uncreated_sensitive_files(
        is_provider=is_provider,
        provider_name=provider_name,
        cmd=cmd,
        readonly=readonly,
    )
    snapshot = _create_sensitive_snapshot(tracked, readonly=readonly)

    sig_handlers: Dict[int, Any] = {}
    sig_fired: List[int] = []

    def _sig_handler(signum: int, frame: Any) -> None:
        sig_fired.append(signum)
        raise KeyboardInterrupt(f"Sandbox terminated by signal {signum}")

    if threading.current_thread() is threading.main_thread():
        signals_to_catch = [sig for sig in (getattr(signal, "SIGTERM", None), getattr(signal, "SIGHUP", None)) if sig is not None]
        for sig in signals_to_catch:
            try:
                sig_handlers[sig] = signal.signal(sig, _sig_handler)
            except (ValueError, OSError):
                pass

    try:
        yield tracked
    finally:
        try:
            _cleanup_uncreated_sensitive_files(snapshot)
        finally:
            if threading.current_thread() is threading.main_thread():
                for sig, old_h in sig_handlers.items():
                    try:
                        signal.signal(sig, old_h)
                    except (ValueError, OSError):
                        pass


def run_in_sandbox(
    cmd: List[str],
    workspace: str,
    timeout: int = 120,
    allow_network: bool = True,
    readonly: bool = False,
    repo_root: Optional[str] = None,
    is_provider: bool = False,
    worktree_root: Optional[str] = None,
    stream: bool = False,
    print_prefix: str = "",
    extra_env: Optional[dict] = None,
    audit_context: str = "sandbox",
    extra_ro_binds: Optional[List[str]] = None,
    enable_seccomp: bool = True,
    provider_name: Optional[str] = None,
    input_text: Optional[str] = None,
) -> Tuple[int, str, str, Optional[str]]:
    """
    Executes a command inside the bubblewrap sandbox with optional Seccomp-BPF filtering.
    Enforces fail-closed security: when bwrap is missing, host execution needs
    MAKEWAND_UNSAFE_HOST_EXEC=1 *and* the recorded one-time acknowledgment
    (shared with the Go engine); every such execution is audited.
    """
    exec_cmd = cmd
    bwrap_avail = is_bwrap_available()
    req_seccomp = os.environ.get(SECCOMP_REQUIRE_ENV) == "1"
    with contextlib.ExitStack() as stack:
        if bwrap_avail:
            try:
                seccomp_r, pass_fds = stack.enter_context(
                    seccomp_filter_context(enabled=enable_seccomp, required=req_seccomp))
                home = None
                p_name = _detect_provider(provider_name, cmd) if is_provider else ""
                if p_name in PROVIDER_PROFILES:
                    home = stack.enter_context(provider_home_session(p_name, [workspace, repo_root or ""]))
                exec_cmd = wrap_bwrap(
                    cmd,
                    workspace=workspace,
                    allow_network=allow_network,
                    readonly=readonly,
                    repo_root=repo_root,
                    is_provider=is_provider,
                    worktree_root=worktree_root,
                    extra_env=extra_env,
                    provider_name=provider_name,
                    extra_ro_binds=extra_ro_binds,
                    seccomp_fd=seccomp_r,
                    provider_home=home,
                )
            except SandboxConfigError as exc:
                _warn(str(exc))
                return -1, "", str(exc), "SandboxConfigError"
            exec_cmd = sandbox_resource_limit_prefix() + list(exec_cmd)
        else:
            pass_fds = ()
            try:
                authorize_unsandboxed_launch(provider_name or "", cmd, os.path.abspath(workspace),
                                             readonly, context=audit_context)
            except SandboxUnavailableError:
                msg = (
                    "Bubblewrap (bwrap) sandbox is not available and unsafe host execution is not acknowledged "
                    "(MAKEWAND_UNSAFE_HOST_EXEC=1 alone never enables it). Execution blocked for security."
                )
                _warn(msg)
                return -1, "", msg, "SandboxUnavailable"

        # Dynamic backpressure: when host load is elevated, deprioritize background sandbox task
        try:
            load_1m = os.getloadavg()[0]
            if load_1m > 10.0:
                nice_bin = shutil.which("nice")
                if nice_bin:
                    nice_val = "15" if load_1m > 18.0 else "10"
                    exec_cmd = [nice_bin, "-n", nice_val] + exec_cmd
                ionice_bin = shutil.which("ionice")
                if ionice_bin and load_1m > 12.0:
                    exec_cmd = [ionice_bin, "-c2", "-n7"] + exec_cmd
        except Exception:
            pass

        return run_subprocess(
            exec_cmd,
            timeout=timeout,
            cwd=workspace,
            input_text=input_text,
            stream=stream,
            print_prefix=print_prefix,
            pass_fds=pass_fds,
        )
