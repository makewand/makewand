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
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Sequence, Set, Tuple, Union
import atexit
import contextlib
import hashlib
import re
from contextlib import contextmanager

from makewand.providers.base import run_subprocess


class SandboxConfigError(RuntimeError):
    """A safe bubblewrap command line cannot be constructed; callers must fail closed."""


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
#   "credentials"  auth/token files; after the run a changed copy is written
#                  back to the host (regular file only, no symlinks, atomic
#                  replace, skipped when the host copy changed concurrently)
#   "seed"         harmless caches/ids the CLI expects; never written back
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
            (".gemini", "flow_auth.json"),
            (".gemini", "antigravity-cli/antigravity-oauth-token"),
        ],
        "seed": [
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
# fails closed for writable sessions.
SCAN_ENTRY_LIMIT = 300_000
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
def _scan_tree(
    roots: Iterable[str],
    *,
    max_depth: Optional[int],
    entry_limit: int,
    collect_git: bool,
    skip_dirs: frozenset = SCAN_SKIP_DIRS,
    skip_paths: Iterable[str] = (),
    descend_git: bool = False,
) -> Tuple[Set[str], Set[str], bool]:
    """
    Iteratively scans roots without following symlinks.
    Returns (git_entries, socket_paths, truncated). `.git` entries are recorded;
    a `.git` directory is descended into (for sockets only, e.g. the fsmonitor
    daemon socket) when descend_git is set; directories named in skip_dirs are
    skipped; the total number of entries visited is capped by entry_limit.
    """
    git_paths: Set[str] = set()
    sockets: Set[str] = set()
    skip_abs = {_norm(p) for p in skip_paths}
    seen = 0
    truncated = False
    # (path, depth, inside_git): .git entries below a .git directory are not recorded
    stack: List[Tuple[str, int, bool]] = [(_norm(r), 0, False) for r in roots if r and os.path.isdir(r)]
    while stack:
        cur, depth, in_git = stack.pop()
        try:
            it = os.scandir(cur)
        except OSError:
            continue
        with it:
            for entry in it:
                seen += 1
                if seen > entry_limit:
                    truncated = True
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
                        if max_depth is None or depth + 1 < max_depth:
                            stack.append((path, depth + 1, in_git or is_git))
                        continue
                    if entry.is_file(follow_symlinks=False) or entry.is_symlink():
                        continue
                    st = entry.stat(follow_symlinks=False)
                    if stat.S_ISSOCK(st.st_mode):
                        sockets.add(path)
                except OSError:
                    continue
        if truncated:
            break
    return git_paths, sockets, truncated


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


def _private_home_base() -> str:
    """0700 per-user directory holding per-run private provider homes."""
    candidates: List[str] = []
    runtime = os.environ.get("XDG_RUNTIME_DIR", "")
    if runtime and os.path.isabs(runtime):
        candidates.append(os.path.join(runtime, PRIVATE_HOME_SUBDIR))
    cache = os.environ.get("XDG_CACHE_HOME", "")
    if not (cache and os.path.isabs(cache)):
        cache = os.path.join(str(Path.home()), ".cache")
    candidates.append(os.path.join(cache, PRIVATE_HOME_SUBDIR))
    for base in candidates:
        try:
            os.makedirs(base, mode=0o700, exist_ok=True)
            st = os.lstat(base)
            if not stat.S_ISDIR(st.st_mode) or (hasattr(os, "geteuid") and st.st_uid != os.geteuid()):
                continue
            if stat.S_IMODE(st.st_mode) & 0o077:
                os.chmod(base, 0o700)
            return base
        except OSError:
            continue
    raise SandboxConfigError("无法创建私有 provider 状态目录（XDG_RUNTIME_DIR / 用户缓存目录均不可用），拒绝挂载 provider 状态")


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


def _read_host_regular(path: str) -> Optional[Tuple[bytes, int]]:
    """(content, mode) of a host file (dotfile symlinks followed), None when absent/not a bounded regular file."""
    try:
        real = os.path.realpath(path)
        fd = os.open(real, os.O_RDONLY | _O_NOFOLLOW | _O_CLOEXEC | getattr(os, "O_NONBLOCK", 0))
    except OSError:
        return None
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode) or st.st_size > CREDENTIAL_MAX_BYTES:
            return None
        chunks = []
        while True:
            b = os.read(fd, 65536)
            if not b:
                break
            chunks.append(b)
        return b"".join(chunks), stat.S_IMODE(st.st_mode)
    except OSError:
        return None
    finally:
        os.close(fd)


def _read_private_nofollow(base: str, rel: str) -> Optional[bytes]:
    """Reads base/rel without following any symlink below base; None unless a bounded regular file."""
    parts = [p for p in rel.split("/") if p]
    if not parts or any(p in (".", "..") for p in parts):
        return None
    fds: List[int] = []
    try:
        fds.append(os.open(base, os.O_RDONLY | _O_DIRECTORY | _O_NOFOLLOW | _O_CLOEXEC))
        for comp in parts[:-1]:
            fds.append(os.open(comp, os.O_RDONLY | _O_DIRECTORY | _O_NOFOLLOW | _O_CLOEXEC, dir_fd=fds[-1]))
        fds.append(os.open(parts[-1], os.O_RDONLY | _O_NOFOLLOW | _O_CLOEXEC | getattr(os, "O_NONBLOCK", 0),
                           dir_fd=fds[-1]))
        st = os.fstat(fds[-1])
        if not stat.S_ISREG(st.st_mode) or st.st_size > CREDENTIAL_MAX_BYTES:
            return None
        chunks = []
        while True:
            b = os.read(fds[-1], 65536)
            if not b:
                break
            chunks.append(b)
        return b"".join(chunks)
    except OSError:
        return None
    finally:
        for fd in reversed(fds):
            try:
                os.close(fd)
            except OSError:
                pass


def _atomic_write_host(path: str, data: bytes, mode: int) -> None:
    parent = os.path.dirname(path)
    fd, tmp = tempfile.mkstemp(prefix=".makewand-wb-", dir=parent)
    try:
        with os.fdopen(fd, "wb") as fh:
            fh.write(data)
            fh.flush()
            os.fsync(fh.fileno())
        os.chmod(tmp, mode & 0o777 or 0o600)
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def _digest(data: Optional[bytes]) -> Optional[str]:
    return None if data is None else hashlib.sha256(data).hexdigest()


@dataclass
class _CredentialCopy:
    private_root: str
    rel: str
    host_path: str
    initial_digest: Optional[str]
    mode: int = 0o600


def _resolve_provider_profile(p_name: str, user_home: str, project_paths: Sequence[str]) -> Optional[dict]:
    profile = PROVIDER_PROFILES.get(p_name)
    if not profile:
        return None
    if p_name != "codex":
        return profile
    # A configured Codex home selects one account. Never silently switch
    # accounts after --clearenv, or expose another account's state as well.
    selected = _norm(os.path.expanduser(os.environ.get("CODEX_HOME") or
                                       os.path.join(user_home, ".codex")))
    real_selected = os.path.realpath(selected)
    if (_broad_workspace(real_selected, user_home) or _home_exposed(real_selected, user_home)
            or any(_is_within(os.path.realpath(path), real_selected) or
                   _is_within(real_selected, os.path.realpath(path)) for path in project_paths if path)):
        raise SandboxConfigError("CODEX_HOME must be a dedicated provider state directory")
    if os.environ.get("CODEX_HOME") and not os.path.isdir(selected):
        raise SandboxConfigError("configured CODEX_HOME does not exist; refusing account fallback")
    profile = dict(profile)
    profile["roots"] = [selected]
    for category in ("protected", "credentials", "seed"):
        profile[category] = [(selected, *entry[1:]) for entry in profile.get(category, [])
                             if entry[0] == ".codex"]
    return profile


class ProviderHome:
    """
    Per-run private provider home (H3). Created lazily by wrap_bwrap via
    materialize(); finalize() writes changed credential files back to the host
    and deletes the private directory. Use provider_home_session().
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
        self._materialized = False
        self._lock = threading.Lock()

    def materialize(self, project_paths: Sequence[str] = ()) -> List[str]:
        with self._lock:
            if self._materialized:
                return list(self.bwrap_args)
            paths = list(self.project_paths) + [p for p in project_paths if p]
            profile = _resolve_provider_profile(self.provider, self.user_home, paths)
            if not profile:
                self._materialized = True
                return []
            try:
                self._build(profile, paths)
            except BaseException:
                self._remove_private_dir()
                raise
            self._materialized = True
            return list(self.bwrap_args)

    def _ensure_private_dir(self) -> str:
        if self.private_dir is None:
            base = _private_home_base()
            _sweep_stale_private_homes(base)
            try:
                self.private_dir = tempfile.mkdtemp(prefix=f"{os.getpid()}-{self.provider}-", dir=base)
                os.chmod(self.private_dir, 0o700)
            except OSError as exc:
                raise SandboxConfigError(f"无法创建私有 provider 状态目录 ({exc})，拒绝启动 (fail closed)") from exc
        return self.private_dir

    def _copy_in(self, src: str, priv_root: str, rel: str) -> Optional[Tuple[bytes, int]]:
        found = _read_host_regular(src)
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

    def _build(self, profile: dict, project_paths: Sequence[str]) -> None:
        args: List[str] = []
        for rel in profile.get("home_ro_files", []):
            p = os.path.join(self.user_home, rel)
            if os.path.exists(p):
                args.extend(["--ro-bind", p, p])

        real_roots: Dict[str, str] = {}
        for idx, root_rel in enumerate(profile.get("roots", [])):
            root_path = _norm(os.path.join(self.user_home, root_rel))
            if not os.path.isdir(root_path):
                continue  # never create or expose a missing provider root
            real = os.path.realpath(root_path)
            priv = os.path.join(self._ensure_private_dir(), f"root{idx}")
            try:
                os.mkdir(priv, 0o700)
            except OSError as exc:
                raise SandboxConfigError(f"无法创建私有 provider 状态目录 ({exc})，拒绝启动 (fail closed)") from exc
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
                        ))

        for root_rel, sub, _kind, _placeholder in profile.get("protected", []):
            real = real_roots.get(root_rel)
            if not real:
                continue
            p = os.path.join(real, sub)
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
        """Writes changed credential copies back to the host. Returns the written host paths."""
        written: List[str] = []
        for cred in self.credentials:
            data = _read_private_nofollow(cred.private_root, cred.rel)
            if data is None:
                continue  # deleted / replaced by a non-regular file: host copy untouched
            if _digest(data) == cred.initial_digest:
                continue
            target = cred.host_path
            if os.path.islink(target):
                target = os.path.realpath(target)
            if os.path.lexists(target) and not os.path.isfile(target):
                _warn(f"宿主凭据 {cred.host_path} 不是普通文件，跳过回写")
                continue
            current = _read_host_regular(target)
            if _digest(current[0] if current else None) != cred.initial_digest:
                _warn(f"宿主凭据 {cred.host_path} 在运行期间被其他会话修改，保留宿主版本，跳过回写")
                continue
            try:
                _atomic_write_host(target, data, cred.mode)
                written.append(target)
            except OSError as exc:
                _warn(f"凭据回写失败 {cred.host_path}: {exc}")
        return written

    def _remove_private_dir(self) -> None:
        if self.private_dir:
            _robust_force_remove(self.private_dir)
            self.private_dir = None

    def finalize(self, write_back: bool = True) -> None:
        with self._lock:
            try:
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
) -> Tuple[List[str], List[str], List[str]]:
    """
    Builds the provider state mounts (private home, see PROVIDER_PROFILES).
    Returns (bwrap_args, state_roots, socket_scan_roots). readonly is accepted for
    compatibility: read-only and writable runs both get a private home.
    """
    if p_name not in PROVIDER_PROFILES:
        return [], [], []
    home = provider_home or _orphan_provider_home(p_name, user_home)
    args = home.materialize(project_paths)
    return args, list(home.state_roots), list(home.scan_roots)


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
        "--disable-userns",
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
    scan_roots = []
    for r in sorted({mount_root, ws}, key=len):
        if r == "/":
            continue
        if not any(_is_within(r, s) for s in scan_roots):
            scan_roots.append(r)
    broad = any(_broad_workspace(r, user_home) for r in scan_roots)
    git_paths, ws_sockets, truncated = _scan_tree(
        scan_roots,
        max_depth=BROAD_SCAN_DEPTH if broad else None,
        entry_limit=SCAN_ENTRY_LIMIT,
        collect_git=not readonly,
        skip_dirs=frozenset(),
        descend_git=True,
    )
    if truncated:
        if not readonly:
            raise SandboxConfigError(
                f"工作区条目超过 {SCAN_ENTRY_LIMIT}，.git 保护与套接字遮蔽扫描无法完成，拒绝可写沙箱 (fail closed)。"
                "请在更小的项目目录中运行或清理工作区。"
            )
        _warn(f"工作区条目超过 {SCAN_ENTRY_LIMIT}，套接字遮蔽扫描已截断（只读会话，更深处的套接字未被遮蔽）")

    # In writable sessions, protect all root .git directories AND submodule .git gitfiles from tampering
    if not readonly:
        for gp_str in sorted(git_paths):
            bwrap_cmd.extend(["--ro-bind", gp_str, gp_str])

    # If repo_root is provided and distinct from mount_root, ensure host repo_root is strictly read-only
    if repo_needed and not repo_first:
        bwrap_cmd.extend(["--ro-bind", repo_abs, repo_abs])

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
            p_name, user_home, readonly, [ws, mount_root], provider_home=provider_home
        )
        bwrap_cmd.extend(p_args)

    # Mask pre-existing pathname AF_UNIX sockets in workspace / provider state to prevent host breakout
    sockets = set(ws_sockets)
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

    for var in SAFE_PASSTHROUGH_ENVS:
        if var in os.environ:
            bwrap_cmd.extend(["--setenv", var, os.environ[var]])

    # Strip any D-Bus and GUI display environment variables to prevent host escape / privilege escalation
    for k in list(os.environ.keys()):
        if "DBUS" in k or k in ("DISPLAY", "XAUTHORITY", "WAYLAND_DISPLAY"):
            bwrap_cmd.extend(["--unsetenv", k])

    if extra_env:
        for k, v in extra_env.items():
            bwrap_cmd.extend(["--setenv", str(k), str(v)])

    if os.environ.get("MAKEWAND_SANDBOX_UNSHARE_NET", "").lower() in ("1", "true", "yes"):
        allow_network = False

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
            "在 macOS 上继续使用 Makewand 的建议途径：\n"
            "  1. [只读审查与设计] 使用只读模式运行代码审查或方案设计：makewand review 或在提示词中显式指定只读分析；\n"
            "  2. [Linux 容器/远端运行] 在 Linux Docker 容器、虚拟机或指定远端服务中运行：--remote-url <URL>；\n"
            "  3. [受权宿主执行（自担风险）] 若当前工作区完全受信任且知晓安全风险，可显式授权直接在宿主机执行：\n"
            "     配置 MAKEWAND_UNSAFE_HOST_EXEC=1 并通过终端交互输入 'yes' 授权确认（或预置 config.json 授权）；\n"
            "     所有无沙箱宿主执行操作均将被永久记入 ~/.config/makewand/unsafe_exec_audit.jsonl 审计日志。"
        )
    else:
        return (
            f"❌ [{prov_label}沙箱未就绪] {prov_label}写入任务强制要求 Bubblewrap (bwrap) 沙箱隔离，系统未检测到可用 bwrap 环境 (fail closed)。\n"
            "解决建议：\n"
            "  1. [安装沙箱] 安装 Bubblewrap 并启用非特权用户命名空间 (例如: sudo apt install bubblewrap，sysctl kernel.unprivileged_userns_clone=1)；\n"
            "  2. [只读审查] 使用只读模式运行代码审查与方案设计：makewand review；\n"
            "  3. [受权宿主执行（自担风险）] 若当前工作区受信任且知晓安全风险，可配置 MAKEWAND_UNSAFE_HOST_EXEC=1 并通过交互确认授权（所有执行将记入审计日志）。"
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
        "  2. [API 模式] 配置对应 provider 的 API Key，由无工具的 API 调用完成审查；\n"
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


def _user_process_count() -> int:
    uid = os.getuid() if hasattr(os, "getuid") else -1
    n = 0
    try:
        for name in os.listdir("/proc"):
            if name.isdigit():
                try:
                    if os.stat(f"/proc/{name}").st_uid == uid:
                        n += 1
                except OSError:
                    continue
    except OSError:
        pass
    return n


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
    prlimit --nproc/--nofile/--fsize (never --as: torch/BLAS reserve huge
    virtual ranges), else nothing (warned once).
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
            if tasks != "infinity":
                # RLIMIT_NPROC counts every process of the uid: budget = current + TasksMax
                budget = _user_process_count() + int(tasks)
                prefix.append(f"--nproc={_rlimit_arg(resource, resource.RLIMIT_NPROC, budget)}")
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
    if not is_bwrap_available():
        source = authorize_unsandboxed_launch(provider, cmd, cwd, readonly, repo_trust, host_auth_source)
        yield ProviderLaunch(["env", f"{PROVIDER_WORKER_ENV}=1", *cmd], (), False, source)
        return
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
    """Recursively removes a file or directory, checking dev/ino and adjusting permissions."""
    if not os.path.lexists(path):
        return
    try:
        st = os.lstat(path)
    except OSError:
        return
    if expected_dev_ino is not None and (st.st_dev, st.st_ino) != expected_dev_ino:
        return
    if os.path.islink(path):
        try:
            os.unlink(path)
        except OSError:
            pass
        return
    try:
        os.chmod(path, stat.S_IRWXU)
    except OSError:
        pass
    if os.path.isdir(path):
        try:
            entries = os.listdir(path)
        except OSError:
            entries = []
        for entry in entries:
            _robust_force_remove_inode_tracked(os.path.join(path, entry), None)
        try:
            os.rmdir(path)
        except OSError:
            pass
    else:
        try:
            os.unlink(path)
        except OSError:
            pass


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
