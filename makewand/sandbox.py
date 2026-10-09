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
from contextlib import contextmanager

from makewand.providers.base import run_subprocess


class SandboxConfigError(RuntimeError):
    """A safe bubblewrap command line cannot be constructed; callers must fail closed."""


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
    ".bash_history",
    ".zsh_history",
    ".python_history",
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
# Provider state-directory policy.
#
# "protected" entries are (root, relative path, kind, placeholder):
#   kind        "file" or "dir"
#   placeholder None  -> re-mount read-only only when it already exists
#               str   -> file content of a host placeholder created when missing
#               True  -> create an empty host directory when missing
# A protected path that does not exist on the host could otherwise be created
# by the sandboxed model inside the writable state directory and would then be
# loaded by the next host session (persistent instruction / hook / extension
# injection). A read-only bind needs an existing mount point, so for paths whose
# empty form is equivalent to "absent" (empty CLAUDE.md, "{}" settings, empty
# directory) we first create that empty placeholder on the host and then mount
# it read-only. Paths whose empty form is NOT known to be equivalent to absence
# (e.g. codex AGENTS.override.md, which would shadow AGENTS.md) are only
# protected when they already exist; mounting /dev/null over an uncreated path
# inside a writable bind mount causes Bubblewrap to creat(0444) the file on the host.
#
# "ephemeral" entries are live per-session IPC / execution state of concurrently
# running host sessions (shell snapshots that host sessions source, session
# keys, daemon control keys, runtime sockets). The sandbox gets an empty tmpfs
# instead, so it can neither read nor poison them.
#
# "readonly_state" lists the state entries a *read-only* task may still write
# (e.g. session logs, credential caches). When present and readonly=True, wrap_bwrap
# mounts the entire provider state root read-only and re-binds only these whitelisted
# subpaths writable. This ensures that uncreated sensitive files (like AGENTS.override.md,
# hooks.json, policy/) cannot be created by a sandboxed process, eliminating the need
# for dangerous post-execution cleanup during read-only tasks.
# ---------------------------------------------------------------------------
PROVIDER_PROFILES: Dict[str, dict] = {
    "claude": {
        "roots": [".claude"],
        "home_ro_files": [".claude.json"],
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
        "ephemeral": [
            (".claude", "shell-snapshots"),
            (".claude", "session-env"),
            (".claude", "sessions"),
            (".claude", "ide"),
            (".claude", "daemon"),
            (".claude", "jobs"),
            (".claude", "bridge-spawn"),
        ],
        "readonly_state": [
            (".claude", ".credentials.json"),
            (".claude", "active-time.json"),
            (".claude", "backups"),
            (".claude", "cache"),
            (".claude", "debug"),
            (".claude", "file-history"),
            (".claude", "history.jsonl"),
            (".claude", "image-cache"),
            (".claude", "logs"),
            (".claude", "mcp-needs-auth-cache.json"),
            (".claude", "paste-cache"),
            (".claude", "plans"),
            (".claude", "startup-perf"),
            (".claude", "stats-cache.json"),
            (".claude", "statsig"),
            (".claude", "state"),
            (".claude", "tasks"),
            (".claude", "telemetry"),
            (".claude", "todos"),
            (".claude", "traces"),
            (".claude", "usage-data"),
        ],
        # auto-memory: ~/.claude/projects/<key>/memory/*.md is injected into
        # later sessions of that project.
        "project_memory_root": (".claude", "projects"),
    },
    "codex": {
        "roots": [".codex"],
        "tmpfs_readonly_root": True,
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
        "ephemeral": [
            (".codex", "shell_snapshots"),
            (".codex", "app-server-control"),
            (".codex", "app-server-daemon"),
        ],
        "readonly_state": [
            (".codex", "sessions"),
            (".codex", "history.jsonl"),
            (".codex", "log"),
            (".codex", "logs"),
            (".codex", "cache"),
            (".codex", "tmp"),
            (".codex", ".tmp"),
            (".codex", "version.json"),
            (".codex", "session_index.jsonl"),
            (".codex", "models_cache.json"),
            (".codex", "archived_sessions"),
        ],
    },
    "agy": {
        "roots": [".gemini"],
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
        "ephemeral": [],
        "readonly_state": [
            (".gemini", "history"),
            (".gemini", "tmp"),
            (".gemini", "state.json"),
            (".gemini", "projects.json"),
            (".gemini", "antigravity-cli/brain"),
            (".gemini", "antigravity-cli/scratch"),
        ],
    },
    "grok": {
        "roots": [".grok"],
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
        "ephemeral": [],
    },
    "muse": {
        "roots": [".config/muse", ".local/share/muse"],
        "protected": [
            (".config/muse", "settings.json", "file", None),
            (".config/muse", "env", "file", None),
            (".config/muse", "trust.json", "file", None),
            (".local/share/muse", "plugins", "dir", None),
            (".local/share/muse", "skills", "dir", None),
            (".local/share/muse", "feature-config", "dir", None),
        ],
        "ephemeral": [
            (".local/share/muse", "runtime"),
        ],
        "readonly_state": [
            (".config/muse", "cache"),
            (".local/share/muse", "logs"),
            (".local/share/muse", "cache"),
        ],
    },
    "aider": {
        "roots": [".aider"],
        "home_ro_files": [".aider.conf.yml"],
        "protected": [],
        "ephemeral": [],
        "readonly_state": [
            (".aider", "caches"),
            (".aider", "analytics.json"),
        ],
    },
}

# Provider-specific toolchain directories below $HOME (read-only).
PROVIDER_HOME_BIN_DIRS = {
    "claude": [".local/share/claude"],
    "muse": [".local/libexec"],
}

# Bounds for build-time tree scans (eng-delivery#7).
SCAN_ENTRY_LIMIT = 100_000
BROAD_SCAN_DEPTH = 3
PROVIDER_SCAN_DEPTH = 3
PROVIDER_SCAN_ENTRY_LIMIT = 20_000
PROJECT_MEMORY_LIMIT = 2_000
SCAN_SKIP_DIRS = frozenset({".git", "node_modules"})
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

        # 4. Filter blocked syscalls: jump to errno block if matched
        n = len(blocked_syscalls)
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


def is_bwrap_available() -> bool:
    bwrap = shutil.which("bwrap")
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
) -> Tuple[Set[str], Set[str], bool]:
    """
    Iteratively scans roots without following symlinks.
    Returns (git_entries, socket_paths, truncated). `.git` entries are recorded
    but never descended into; directories named in skip_dirs are skipped; the
    total number of entries visited is capped by entry_limit.
    """
    git_paths: Set[str] = set()
    sockets: Set[str] = set()
    skip_abs = {_norm(p) for p in skip_paths}
    seen = 0
    truncated = False
    stack: List[Tuple[str, int]] = [(_norm(r), 0) for r in roots if r and os.path.isdir(r)]
    while stack:
        cur, depth = stack.pop()
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
                if name == ".git":
                    if collect_git:
                        git_paths.add(path)
                    continue
                try:
                    if entry.is_dir(follow_symlinks=False):
                        if name in skip_dirs or path in skip_abs:
                            continue
                        if max_depth is None or depth + 1 < max_depth:
                            stack.append((path, depth + 1))
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


def _provider_mounts(
    p_name: str,
    user_home: str,
    readonly: bool,
    project_paths: Sequence[str],
) -> Tuple[List[str], List[str], List[str]]:
    """
    Builds the provider state-directory mounts.
    Returns (bwrap_args, scan_roots, scan_skip_paths).
    """
    profile = PROVIDER_PROFILES.get(p_name)
    if not profile:
        return [], [], []
    if p_name == "codex":
        # A configured Codex home selects one account. Never silently switch
        # accounts after --clearenv, or expose another account's state as well.
        selected = _norm(os.path.expanduser(os.environ.get("CODEX_HOME") or
                                           os.path.join(user_home, ".codex")))
        real_selected = os.path.realpath(selected)
        if (_broad_workspace(real_selected, user_home) or _home_exposed(real_selected, user_home)
                or any(_is_within(os.path.realpath(path), real_selected) or
                       _is_within(real_selected, os.path.realpath(path)) for path in project_paths)):
            raise SandboxConfigError("CODEX_HOME must be a dedicated provider state directory")
        if os.environ.get("CODEX_HOME") and not os.path.isdir(selected):
            raise SandboxConfigError("configured CODEX_HOME does not exist; refusing account fallback")
        profile = dict(profile)
        profile["roots"] = [selected]
        for category in ("protected", "ephemeral", "readonly_state"):
            profile[category] = [(selected, *entry[1:]) for entry in profile.get(category, [])
                                 if entry[0] == ".codex"]
    args: List[str] = []
    scan_roots: List[str] = []
    scan_skip: List[str] = []

    for rel in profile.get("home_ro_files", []):
        p = os.path.join(user_home, rel)
        if os.path.exists(p):
            args.extend(["--ro-bind", p, p])

    tmpfs_root_ro = readonly and bool(profile.get("tmpfs_readonly_root"))
    whole_root_ro = readonly and bool(profile.get("readonly_state")) and not tmpfs_root_ro

    real_roots: Dict[str, str] = {}
    for root_rel in profile.get("roots", []):
        root_path = os.path.join(user_home, root_rel)
        if not os.path.isdir(root_path):
            continue
        real = os.path.realpath(root_path)
        real_roots[root_rel] = real
        if tmpfs_root_ro:
            args.extend(["--tmpfs", real])
        elif whole_root_ro:
            args.extend(["--ro-bind", real, real])
        else:
            args.extend(["--bind", real, real])
        if real != _norm(root_path):
            # Keep the host layout: ~/.gemini -> /mnt/.../gemini. Only the real
            # directory is mounted, so every protection below applies once.
            args.extend(["--symlink", real, root_path])
        scan_roots.append(real)

    if tmpfs_root_ro:
        for root_rel in profile.get("roots", []):
            real = real_roots.get(root_rel)
            if not real:
                continue
            for sub in ("auth.json", "config.toml", "version.json", "models_cache.json"):
                p = os.path.join(real, sub)
                if os.path.exists(p):
                    args.extend(["--ro-bind", p, p])
        for root_rel, sub, kind, placeholder in profile.get("protected", []):
            real = real_roots.get(root_rel)
            if not real:
                continue
            p = os.path.join(real, sub)
            if os.path.islink(p):
                target = os.path.realpath(p)
                if os.path.exists(target):
                    args.extend(["--ro-bind", target, p])
            elif os.path.exists(p):
                args.extend(["--ro-bind", p, p])
    elif whole_root_ro:
        for root_rel, sub in profile.get("readonly_state", []):
            real = real_roots.get(root_rel)
            if not real:
                continue
            p = os.path.join(real, sub)
            if os.path.lexists(p) and not os.path.islink(p):
                args.extend(["--bind", p, p])
    else:
        for root_rel, sub, kind, placeholder in profile.get("protected", []):
            real = real_roots.get(root_rel)
            if not real:
                continue
            p = os.path.join(real, sub)
            if os.path.islink(p):
                # A dotfile-managed symlink: a bind on it would land on its
                # target anyway, so expose the target read-only at its real
                # location (the link itself stays replaceable, see SECURITY.md).
                target = os.path.realpath(p)
                if os.path.exists(target):
                    args.extend(["--ro-bind", target, target])
                else:
                    _warn(f"受保护路径 {p} 是悬空符号链接，未能只读挂载")
                continue
            if not os.path.lexists(p):
                if placeholder is None:
                    continue
                _create_placeholder(p, kind, placeholder)
            args.extend(["--ro-bind", p, p])

        pm = profile.get("project_memory_root")
        if pm and real_roots.get(pm[0]):
            projects_dir = os.path.join(real_roots[pm[0]], pm[1])
            args.extend(_claude_project_memory_mounts(projects_dir, project_paths))

    for root_rel, sub in profile.get("ephemeral", []):
        real = real_roots.get(root_rel)
        if not real:
            continue
        p = os.path.join(real, sub)
        if os.path.islink(p):
            target = os.path.realpath(p)
            if os.path.isdir(target):
                args.extend(["--tmpfs", target])
                scan_skip.append(target)
            continue
        if whole_root_ro and not os.path.lexists(p):
            continue  # cannot create a mount point inside a read-only root
        if os.path.lexists(p) and not os.path.isdir(p):
            args.extend(["--ro-bind", "/dev/null", p])
        else:
            args.extend(["--tmpfs", p])
        scan_skip.append(p)

    return args, scan_roots, scan_skip


def _claude_project_memory_mounts(projects_dir: str, project_paths: Sequence[str]) -> List[str]:
    """Read-only auto-memory directories for every known Claude project (+ the current one)."""
    args: List[str] = []
    candidates: List[str] = []
    try:
        if os.path.isdir(projects_dir):
            with os.scandir(projects_dir) as it:
                for entry in it:
                    if entry.is_dir(follow_symlinks=False):
                        candidates.append(entry.path)
    except OSError:
        pass
    for p in project_paths:
        key = _claude_project_key(p) if p else None
        if key:
            cand = os.path.join(projects_dir, key)
            if cand not in candidates:
                candidates.append(cand)
    if len(candidates) > PROJECT_MEMORY_LIMIT:
        _warn(f"Claude 项目目录超过 {PROJECT_MEMORY_LIMIT} 个，仅保护前 {PROJECT_MEMORY_LIMIT} 个项目的 auto-memory")
        candidates = candidates[:PROJECT_MEMORY_LIMIT]
    for proj in sorted(candidates):
        mem = os.path.join(proj, "memory")
        if os.path.islink(mem):
            continue
        if not os.path.lexists(mem):
            _create_placeholder(mem, "dir", True)
        args.extend(["--ro-bind", mem, mem])
    return args


# ---------------------------------------------------------------------------
# command builder
# ---------------------------------------------------------------------------
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
    seccomp_fd: Optional[int] = None
) -> List[str]:
    """
    Wraps command with bubblewrap isolating host filesystem, IPC, PID, and credentials.
    Workspace (or entire worktree root if workspace is a subdirectory) is mounted.
    Workspace is mounted read-only if readonly=True, otherwise writable.
    If is_provider=True, only the active provider's state directory is mounted:
    writable for session/credential state, with instruction/extension/config
    paths read-only (see PROVIDER_PROFILES).
    HOME is always an empty tmpfs; /home, /root, /mnt, /media, /srv are hidden.
    Raises SandboxConfigError when no safe command line can be built (fail closed).
    """
    bwrap = shutil.which("bwrap") or "/usr/bin/bwrap"
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

    # A writable mount of / or of HOME (or an ancestor of HOME) would expose the
    # whole host / every credential read-write: refuse (fail closed).
    if not readonly:
        for cand in {ws, mount_root}:
            if cand == "/" or _home_exposed(cand, user_home):
                raise SandboxConfigError(
                    f"拒绝以 {cand} 作为可写沙箱工作区：它是 / 或 HOME（或 HOME 的上级目录），"
                    "可写挂载会暴露全部宿主文件与凭据。请在具体项目目录中运行。"
                )

    # Identify provider strictly from explicit provider_name or first command argument binary name
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
    if not is_provider:
        p_name = ""

    masked_roots = _effective_masked_roots()

    bwrap_cmd = [
        bwrap,
        "--die-with-parent",
        "--new-session",
        "--unshare-pid",
        "--unshare-ipc",
        "--unshare-uts",
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
    )
    if truncated:
        _warn(f"工作区条目超过 {SCAN_ENTRY_LIMIT}，.git 保护与套接字遮蔽扫描已截断（更深处的嵌套 .git/套接字未被保护）")

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
    provider_scan_skip: List[str] = []
    if is_provider and p_name:
        p_args, provider_scan_roots, provider_scan_skip = _provider_mounts(
            p_name, user_home, readonly, [ws, mount_root]
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
            skip_paths=provider_scan_skip,
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


def apply_posix_sandbox_rlimits():
    """Apply POSIX rlimits (RLIMIT_AS, RLIMIT_NPROC, RLIMIT_FSIZE) on child processes where supported."""
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
) -> Tuple[int, str, str, Optional[str]]:
    """
    Executes a command inside the bubblewrap sandbox with optional Seccomp-BPF filtering.
    Enforces fail-closed security: when bwrap is missing, host execution needs
    MAKEWAND_UNSAFE_HOST_EXEC=1 *and* the recorded one-time acknowledgment
    (shared with the Go engine); every such execution is audited.
    """
    exec_cmd = cmd
    seccomp_r = None
    pass_fds: tuple = ()
    with sandbox_lifecycle(is_provider=is_provider, provider_name=provider_name, cmd=cmd, readonly=readonly):
        try:
            if is_bwrap_available():
                if enable_seccomp:
                    bpf_filter = generate_seccomp_bpf_filter()
                    if bpf_filter:
                        try:
                            r, w = os.pipe()
                            os.write(w, bpf_filter)
                            os.close(w)
                            seccomp_r = r
                            pass_fds = (seccomp_r,)
                        except Exception:
                            seccomp_r = None
                            pass_fds = ()

                try:
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
                    )
                except SandboxConfigError as exc:
                    _warn(str(exc))
                    return -1, "", str(exc), "SandboxConfigError"
            else:
                authorized, source = resolve_unsafe_host_exec()
                if not authorized:
                    msg = (
                        "Bubblewrap (bwrap) sandbox is not available and unsafe host execution is not acknowledged "
                        "(MAKEWAND_UNSAFE_HOST_EXEC=1 alone never enables it). Execution blocked for security."
                    )
                    _warn(msg)
                    return -1, "", msg, "SandboxUnavailable"
                audit_unsafe_host_exec(audit_context, cmd, os.path.abspath(workspace), source)

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
                stream=stream,
                print_prefix=print_prefix,
                pass_fds=pass_fds,
                preexec_fn=apply_posix_sandbox_rlimits,
            )
        finally:
            if seccomp_r is not None:
                try:
                    os.close(seccomp_r)
                except Exception:
                    pass
                pass
