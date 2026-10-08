"""
Git workspace resilience and isolated worktree management.
"""

import os
import sys
import stat
import time
import errno
import shutil
import hashlib
import tempfile
import subprocess
import json
import shlex
from contextvars import ContextVar
from datetime import datetime
from pathlib import Path
from typing import Optional, Dict, Any, List, Tuple, Union
import makewand.config as config
from makewand.config import c, COLOR_YELLOW, COLOR_RED, ensure_private_dir
from makewand.windows_paths import filesystem_path, windows_git_directory

# Every git subprocess shares one generous, configurable timeout. A timeout is
# reported as rc=-1; callers that establish baselines or restore state must
# treat any non-zero rc as a hard failure (see HostWorkspaceTransaction).
DEFAULT_GIT_TIMEOUT = 300.0
_diff_index = ContextVar("makewand_diff_index", default=None)


def get_git_timeout() -> float:
    raw = os.environ.get("MAKEWAND_GIT_TIMEOUT", "").strip()
    try:
        value = float(raw) if raw else DEFAULT_GIT_TIMEOUT
    except ValueError:
        value = DEFAULT_GIT_TIMEOUT
    return value if value > 0 else DEFAULT_GIT_TIMEOUT

SAFE_GIT_SECURITY_FLAGS = [
    "--no-replace-objects",
    "-c", "diff.tool=",
    "-c", "core.fsmonitor=",
    "-c", "core.hooksPath=/dev/null",
    "-c", "core.attributesFile=/dev/null",
    # Delivery is bound to the exact reviewed bytes, including CRLF files.
    # A user's global Windows Git setting must not normalize those blobs.
    "-c", "core.autocrlf=false",
    # A command-local setting supports deep private state on Windows without
    # changing a user's repository, global Git config or system long-path policy.
    "-c", "core.longpaths=true",
    "-c", "core.pager=cat",
    "-c", "commit.gpgsign=false",
]

_DANGEROUS_GIT_ENVS = {
    "GIT_DIR",
    "GIT_WORK_TREE",
    "GIT_INDEX_FILE",
    "GIT_OBJECT_DIRECTORY",
    "GIT_ALTERNATE_OBJECT_DIRECTORIES",
    "GIT_COMMON_DIR",
    "GIT_HOOKS_PATH",
    "GIT_EXEC_PATH",
    "GIT_EXTERNAL_DIFF",
    "GIT_DIFF_OPTS",
    "GIT_PAGER",
    "GIT_EDITOR",
    "GIT_SEQUENCE_EDITOR",
    "GIT_SSH",
    "GIT_SSH_COMMAND",
    "GIT_ASKPASS",
}


def _fs_path(path):
    """Filesystem-call representation, never a saved workspace identity."""
    return Path(filesystem_path(path))


def _is_reparse_path(path):
    if os.name != "nt":
        return False
    try:
        return bool(getattr(_fs_path(path).lstat(), "st_file_attributes", 0) & 0x400)
    except FileNotFoundError:
        return False


def _walk_filesystem(root):
    api_root = _fs_path(root)
    for current, dirs, files in os.walk(api_root, followlinks=False):
        # scandir/realpath may return the extended API representation. Keep
        # relative paths, symlink policy and metadata in the original namespace.
        yield Path(root) / Path(current).relative_to(api_root), dirs, files


def _sanitize_git_env(env: Optional[Dict[str, str]] = None) -> Dict[str, str]:
    """Sanitize environment variables for safe git execution."""
    src = os.environ if env is None else env
    clean = {}
    for k, v in src.items():
        if k in _DANGEROUS_GIT_ENVS or k.startswith("GIT_CONFIG_") or k == "GIT_OPTIONAL_LOCKS":
            continue
        clean[k] = v
    clean["GIT_OPTIONAL_LOCKS"] = "0"
    clean["NoDefaultCurrentDirectoryInExePath"] = "1"
    return clean


_RESOLVED_GIT_BINARY: Optional[str] = None


def resolve_safe_git_binary(cwd: Optional[Union[str, Path]] = None) -> str:
    """Resolve the git binary securely, rejecting cwd or relative paths to prevent binary hijacking."""
    global _RESOLVED_GIT_BINARY
    if _RESOLVED_GIT_BINARY:
        if cwd is not None:
            try:
                if Path(_RESOLVED_GIT_BINARY).resolve().is_relative_to(Path(cwd).resolve()):
                    raise PermissionError(f"Refusing to execute git binary found inside workspace cwd: {_RESOLVED_GIT_BINARY}")
            except (ValueError, RuntimeError):
                pass
        return _RESOLVED_GIT_BINARY

    path_entries = os.environ.get("PATH", "").split(os.pathsep)
    clean_entries = []
    cwd_resolved = Path(cwd).resolve() if cwd else None
    for entry in path_entries:
        if not entry or entry == ".":
            continue
        try:
            entry_p = Path(entry).resolve()
            if cwd_resolved and entry_p.is_relative_to(cwd_resolved):
                continue
        except (ValueError, RuntimeError):
            pass
        clean_entries.append(entry)

    clean_path = os.pathsep.join(clean_entries)
    found = shutil.which("git", path=clean_path)
    if not found:
        found = shutil.which("git")
    if not found:
        return "git"

    found_path = Path(found).resolve()
    if cwd_resolved:
        try:
            if found_path.is_relative_to(cwd_resolved):
                raise PermissionError(f"Refusing to execute git binary found inside workspace cwd: {found}")
        except (ValueError, RuntimeError):
            pass

    _RESOLVED_GIT_BINARY = str(found_path)
    return _RESOLVED_GIT_BINARY


def _get_git_info_attributes_paths(cwd: Optional[Union[str, Path]]) -> List[Path]:
    paths: List[Path] = []
    seen_dirs = set()
    try:
        p = Path(cwd).resolve() if cwd else Path.cwd().resolve()
        for cur in [p] + list(p.parents):
            gp = cur / ".git"
            git_dirs: List[Path] = []
            if _fs_path(gp).is_dir():
                git_dirs.append(gp)
            elif _fs_path(gp).is_file():
                try:
                    txt = _fs_path(gp).read_text(encoding="utf-8").strip()
                    if txt.startswith("gitdir:"):
                        gd = Path(txt[7:].strip())
                        if not gd.is_absolute():
                            gd = (gp.parent / gd).resolve()
                        git_dirs.append(gd)
                        commondir_file = gd / "commondir"
                        if _fs_path(commondir_file).exists():
                            cd_txt = _fs_path(commondir_file).read_text(encoding="utf-8").strip()
                            cd_path = Path(cd_txt)
                            if not cd_path.is_absolute():
                                cd_path = (gd / cd_path).resolve()
                            git_dirs.append(cd_path)
                except Exception:
                    pass
            for gdir in git_dirs:
                try:
                    resolved_dir = gdir.resolve()
                except Exception:
                    resolved_dir = gdir
                if resolved_dir not in seen_dirs:
                    seen_dirs.add(resolved_dir)
                    info_dir = _fs_path(resolved_dir / "info")
                    ia = info_dir / "attributes"
                    gate_file = info_dir / "attributes.mw_gate"
                    needs_shield = False
                    if ia.exists() or gate_file.exists():
                        needs_shield = True
                    elif info_dir.is_dir():
                        try:
                            if any(info_dir.glob("attributes.*shield_*")):
                                needs_shield = True
                        except Exception:
                            pass
                    if needs_shield:
                        paths.append(ia)
            if _fs_path(gp).exists():
                break
    except Exception:
        pass
    return paths

def run_git_cmd(cmd, cwd=None, input_data=None, binary=False, safe=True, timeout=None):
    shielded_infos: List[Tuple[Any, ...]] = []
    if timeout is None:
        timeout = get_git_timeout()
    try:
        is_bytes = isinstance(input_data, bytes) or binary
        if safe and isinstance(cmd, str) and "&&" in cmd:
            subcmds = [s.strip() for s in cmd.split("&&") if s.strip()]
            last_rc, last_out, last_err = 0, "" if not is_bytes else b"", "" if not is_bytes else b""
            for sc in subcmds:
                last_rc, last_out, last_err = run_git_cmd(sc, cwd=cwd, input_data=input_data, binary=binary, safe=safe, timeout=timeout)
                if last_rc != 0:
                    return last_rc, last_out, last_err
            return last_rc, last_out, last_err

        use_shell = False
        if isinstance(cmd, str):
            if safe:
                if any(op in cmd for op in ["||", ";", "|", "`", "$("]):
                    raise ValueError(f"Unsafe shell metacharacter detected in git command: {cmd}")
                cmd = shlex.split(cmd)
            else:
                if any(op in cmd for op in ["&&", "||", ";", "|", "`", "$("]):
                    exec_cmd = cmd
                    use_shell = True
                else:
                    cmd = shlex.split(cmd)

        process_cwd = filesystem_path(cwd) if cwd is not None else None
        resolve_safe_git_binary(cwd=process_cwd)
        if isinstance(cmd, list) and len(cmd) > 0 and cmd[0] == "git" and safe:
            subcmd = cmd[1] if len(cmd) > 1 else ""
            extra_global = ["--no-pager"]
            if subcmd not in ["apply", "clone"]:
                extra_global.append("--attr-source=4b825dc642cb6eb9a060e54bf8d69288fbee4904")
            exec_cmd = [cmd[0]] + extra_global + SAFE_GIT_SECURITY_FLAGS + cmd[1:]
            if subcmd == "diff":
                diff_idx = exec_cmd.index("diff")
                if "--no-ext-diff" not in exec_cmd:
                    exec_cmd.insert(diff_idx + 1, "--no-ext-diff")
                if "--no-textconv" not in exec_cmd:
                    exec_cmd.insert(diff_idx + 2, "--no-textconv")
            use_shell = False
        elif not use_shell:
            exec_cmd = cmd
            use_shell = False

        if (os.name == "nt" and cwd is not None and isinstance(exec_cmd, list)
                and exec_cmd and exec_cmd[0] == "git"):
            # Avoid CreateProcess's extended-CWD limit and Git's fixed getcwd
            # buffers. Check before shielding attributes or launching Git.
            process_cwd = windows_git_directory(cwd)
        git_env = _sanitize_git_env() if safe else os.environ.copy()
        if safe:
            if _diff_index.get() is not None:
                git_env["GIT_INDEX_FILE"] = _diff_index.get()

            # S01: Temporarily shield .git/info/attributes across both primary and linked worktrees.
            # Use deterministic sorting and attributes.lock file locking to avoid multi-session collisions.
            ia_targets = sorted(_get_git_info_attributes_paths(cwd), key=lambda p: str(p.resolve()))
            for ia_target in ia_targets:
                info_dir = ia_target.parent
                try:
                    info_dir.mkdir(parents=True, exist_ok=True)
                except Exception:
                    pass
                lock_file = info_dir / "attributes.mw_gate"
                lock_handle = None
                try:
                    lock_handle = open(lock_file, "a+", encoding="utf-8")
                    from makewand import filelock
                    start_t = time.monotonic()
                    lock_timeout = timeout if timeout is not None else get_git_timeout()
                    acquired = False
                    while time.monotonic() - start_t < lock_timeout:
                        try:
                            filelock.flock(lock_handle, filelock.LOCK_EX | filelock.LOCK_NB)
                            acquired = True
                            break
                        except (OSError, BlockingIOError):
                            time.sleep(0.01)
                    if not acquired:
                        raise OSError("timed out acquiring attributes lock")
                except Exception as exc:
                    if lock_handle is not None:
                        try:
                            lock_handle.close()
                        except Exception:
                            pass
                    raise OSError(f"Cannot shield Git attributes at {ia_target}: {exc}") from exc

                # Under lock: self-heal any orphaned shield files from an aborted previous run
                try:
                    orphans = sorted(
                        list(info_dir.glob(ia_target.name + ".makewand_shield_*"))
                        + list(info_dir.glob(ia_target.name + ".mw_shield_*"))
                    )
                    if orphans:
                        if not ia_target.exists():
                            orphans[0].rename(ia_target)
                            for extra_orphan in orphans[1:]:
                                extra_orphan.unlink(missing_ok=True)
                        else:
                            for stale_orphan in orphans:
                                stale_orphan.unlink(missing_ok=True)
                except Exception:
                    pass

                # If the attributes file is present, rename it to mask clean/smudge filters
                if ia_target.exists():
                    try:
                        shield_file = info_dir / (ia_target.name + f".makewand_shield_{os.getpid()}_{len(shielded_infos)}")
                        ia_target.rename(shield_file)
                        shielded_infos.append((ia_target, shield_file, lock_handle))
                    except Exception as exc:
                        try:
                            from makewand import filelock
                            filelock.flock(lock_handle, filelock.LOCK_UN)
                        except Exception:
                            pass
                        try:
                            lock_handle.close()
                        except Exception:
                            pass
                        raise OSError(f"Cannot shield Git attributes at {ia_target}: {exc}") from exc
                else:
                    # No attributes file exists; release lock immediately so operations on repositories
                    # without attributes do not block each other during git execution.
                    try:
                        from makewand import filelock
                        filelock.flock(lock_handle, filelock.LOCK_UN)
                    except Exception:
                        pass
                    try:
                        lock_handle.close()
                    except Exception:
                        pass

        # Git emits UTF-8 text independently of the Windows ANSI code page.
        # Preserve undecodable bytes rather than crashing a pipe reader; binary
        # object reads remain byte-for-byte and never use a text wrapper.
        text_options = {} if is_bytes else {"encoding": "utf-8", "errors": "surrogateescape"}
        res = subprocess.run(
            exec_cmd,
            input=input_data,
            shell=use_shell,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=not is_bytes,
            **text_options,
            cwd=process_cwd,
            timeout=timeout,
            env=git_env
        )
        return res.returncode, res.stdout, res.stderr
    except Exception as e:
        empty = b"" if (isinstance(input_data, bytes) or binary) else ""
        return -1, empty, str(e)
    finally:
        for item in reversed(shielded_infos):
            orig_ia = item[0]
            shield_ia = item[1]
            lock_handle = item[2] if len(item) > 2 else None
            if shield_ia is not None:
                try:
                    if shield_ia.exists():
                        shield_ia.rename(orig_ia)
                except Exception:
                    pass
            if lock_handle is not None:
                try:
                    from makewand import filelock
                    filelock.flock(lock_handle, filelock.LOCK_UN)
                except Exception:
                    pass
                try:
                    lock_handle.close()
                except Exception:
                    pass

def _is_git_marker(marker: Path) -> bool:
    """True for a real repository marker: a .git directory with HEAD or a gitdir file.

    A stray empty ``.git`` directory (e.g. /tmp/.git) is not a repository for git
    and must not be treated as one either.
    """
    try:
        marker = _fs_path(marker)
        if marker.is_dir():
            return (marker / "HEAD").is_file()
        if marker.is_file():
            with open(marker, "r", encoding="utf-8", errors="replace") as handle:
                return handle.read(8).startswith("gitdir:")
    except OSError:
        return False
    return False


def find_git_root(path: Union[str, Path]) -> Optional[str]:
    """
    Traverses upward from path to find the enclosing git repository root.
    """
    if not path:
        return None
    try:
        curr = Path(path).resolve()
        while curr != curr.parent:
            if _is_git_marker(curr / ".git"):
                return str(curr)
            curr = curr.parent
    except Exception:
        pass
    return None

EPHEMERAL_GIT_MARKER = "makewand-ephemeral"
EPHEMERAL_BASELINE_SUBJECT = "Makewand baseline snapshot"


def _refused_init_roots() -> List[Path]:
    roots = [Path("/"), Path("/tmp")]
    try:
        roots += [Path.home(), Path.home().resolve()]
    except Exception:
        pass
    return roots


def _remove_git_dir(git_dir: Path) -> Optional[str]:
    """Removes a .git directory Makewand created; never follows a symlink."""
    try:
        info = os.lstat(git_dir)
    except FileNotFoundError:
        return None
    except OSError as exc:
        return f"无法检查 {git_dir}: {exc}"
    if not stat.S_ISDIR(info.st_mode):
        return f"{git_dir} 不是目录（可能已被替换为链接或文件），拒绝删除"
    errors = _rmtree_collect(git_dir)
    if errors or os.path.lexists(git_dir):
        return "无法完全删除 Makewand 创建的 .git: " + "; ".join(errors[:3])
    return None


def _rmtree_collect(path: Path) -> List[str]:
    """shutil.rmtree that reports every failure instead of hiding it."""
    errors: List[str] = []
    if sys.version_info >= (3, 12):
        shutil.rmtree(path, onexc=lambda func, p, exc: errors.append(f"{p}: {exc}"))
    else:  # pragma: no cover - older interpreters
        shutil.rmtree(path, onerror=lambda func, p, exc: errors.append(f"{p}: {exc[1]}"))
    return errors


def init_git_baseline(cwd: str, ephemeral: bool = False) -> Tuple[bool, Optional[str]]:
    """Ensures cwd is inside a git work tree, initializing one when there is none.

    Returns (created, error). Every git step is checked: when init/config/add/
    commit fails, the freshly created .git is removed again and an error is
    returned, so callers never continue from an empty or partial baseline.
    An existing repository whose state git cannot read is an error, never a
    reason to create a nested repository.
    """
    if not cwd:
        cwd = os.getcwd()
    resolved = Path(cwd).resolve()
    if resolved in _refused_init_roots():
        return False, f"拒绝在系统目录或用户主目录 ({resolved}) 自动初始化 Git"
    if find_git_root(resolved):
        code, out, err = run_git_cmd(["git", "rev-parse", "--is-inside-work-tree"], cwd=str(resolved))
        if code == 0 and out.strip() == "true":
            return False, None
        return False, f"无法读取已有 Git 仓库状态 (rc={code}): {(err or '').strip()[:200]}"

    git_dir = resolved / ".git"
    print(c(f"[Makewand Git] {resolved} 不是 Git 仓库，建立临时 Git 基线（任务结束后自动移除）...", COLOR_YELLOW), file=sys.stderr)
    steps = [
        (["git", "init", "-q"], "git init"),
        (["git", "config", "user.name", "Makewand"], "git config user.name"),
        (["git", "config", "user.email", "makewand@local"], "git config user.email"),
    ]
    for cmd, label in steps:
        code, _, err = run_git_cmd(cmd, cwd=str(resolved))
        if code != 0:
            cleanup_err = _remove_git_dir(git_dir)
            return False, f"{label} 失败 (rc={code}): {(err or '').strip()[:200]}" + (f"；{cleanup_err}" if cleanup_err else "")
    if ephemeral:
        try:
            (git_dir / EPHEMERAL_GIT_MARKER).write_text(json.dumps({"pid": os.getpid(), "created": datetime.now().isoformat()}), encoding="utf-8")
        except OSError as exc:
            cleanup_err = _remove_git_dir(git_dir)
            return False, f"无法写入临时仓库标记: {exc}" + (f"；{cleanup_err}" if cleanup_err else "")
    for cmd, label in [
        (["git", "add", "-A"], "git add -A"),
        (["git", "commit", "-q", "--no-verify", "-m", EPHEMERAL_BASELINE_SUBJECT, "--allow-empty"], "git commit"),
    ]:
        code, _, err = run_git_cmd(cmd, cwd=str(resolved))
        if code != 0:
            cleanup_err = _remove_git_dir(git_dir)
            return False, (f"{label} 失败 (rc={code})，无法建立完整基线: {(err or '').strip()[:300]}"
                           + (f"；{cleanup_err}" if cleanup_err else ""))
    return True, None


def ensure_git_worktree(cwd: str) -> bool:
    """
    Ensures cwd is inside a git repository, initializing a baseline repository
    when it is not. Returns True only when a repository was created. Failures
    are reported and leave no partially initialized .git behind.
    Protects root system directories (/, /tmp, ~) from accidental init.
    """
    created, error = init_git_baseline(cwd)
    if error:
        print(c(f"❌ [Makewand Git] {error}", COLOR_RED), file=sys.stderr)
    return created

def get_submodule_paths(repo_dir: str) -> List[str]:
    """
    Returns an ordered list of submodule relative paths, sorted descending by path depth
    (deepest submodules first). Correctly handles submodule paths containing spaces.
    """
    r_path = Path(repo_dir)
    gitmodules = r_path / ".gitmodules"
    if not gitmodules.exists():
        return []

    paths = []
    # 1. Read .gitmodules paths via git config (preserves spaces in path values)
    code, out, error = run_git_cmd(["git", "config", "--file", ".gitmodules", "--get-regexp", r"^submodule\..*\.path$"], cwd=str(r_path))
    if code not in (0, 1):
        raise OSError(f"Cannot read Git submodule configuration: {error}")
    if code == 0 and out:
        for line in out.splitlines():
            line = line.strip()
            if not line:
                continue
            parts = line.split(None, 1)
            if len(parts) == 2:
                paths.append(parts[1].strip())

    # 2. Also parse git submodule status --recursive with regex to capture nested submodules
    st_code, st_out, error = run_git_cmd(["git", "submodule", "status", "--recursive"], cwd=str(r_path))
    if st_code != 0:
        raise OSError(f"Cannot read Git submodule status: {error}")
    if st_code == 0 and st_out:
        import re
        for line in st_out.splitlines():
            m = re.match(r"^[-+ U]?[0-9a-fA-F]+\s+(.*?)(?:\s+\([^\)]*\))?$", line)
            if m:
                paths.append(m.group(1).strip())

    # Sort descending by directory depth so nested submodules appear first
    return sorted(list(dict.fromkeys(paths)), key=lambda s: len(Path(s).parts), reverse=True)

def _resolve_diff_head(cwd):
    code, head, error = run_git_cmd(["git", "rev-parse", "--verify", "HEAD^{commit}"], cwd=cwd)
    if code == 0 and head.strip():
        return head.strip(), None
    # An unborn branch has a symbolic HEAD whose ref does not exist. A broken
    # object, detached HEAD or unreadable ref must never become an empty tree.
    sym_code, branch, _ = run_git_cmd(["git", "symbolic-ref", "-q", "HEAD"], cwd=cwd)
    if sym_code == 0 and branch.strip():
        ref_code, _, _ = run_git_cmd(["git", "show-ref", "--verify", "--quiet", branch.strip()], cwd=cwd)
        if ref_code == 1:
            return None, None
    return None, f"Cannot resolve Git HEAD: {error.strip()}"


def _read_workspace_diff(cwd, base_rev=None, arguments=(), binary=False):
    """Read working-tree changes using a private index and a fixed commit.

    Neither review nor candidate inspection stages files into the user's index.
    Every index construction and diff error is returned, including in submodules.
    """
    empty = b"" if binary else ""
    code, inside, error = run_git_cmd(["git", "rev-parse", "--is-inside-work-tree"], cwd=cwd)
    if code != 0 or inside.strip() != "true":
        return empty, f"Not inside a valid git working tree ({error.strip()})"
    head, error = _resolve_diff_head(cwd)
    if error:
        return empty, error
    ref = head
    if base_rev:
        code, resolved, error = run_git_cmd(
            ["git", "rev-parse", "--verify", "--end-of-options", f"{base_rev}^{{commit}}"], cwd=cwd)
        if code != 0 or not resolved.strip():
            return empty, f"Cannot resolve Git diff baseline: {error.strip()}"
        ref = resolved.strip()
    try:
        with tempfile.TemporaryDirectory(prefix="makewand-diff-index-") as directory:
            token = _diff_index.set(str(Path(directory) / "index"))
            try:
                seed = ["git", "read-tree", head] if head else ["git", "read-tree", "--empty"]
                code, _, error = run_git_cmd(seed, cwd=cwd)
                if code != 0:
                    return empty, f"git read-tree failed with exit code {code}: {error.strip()}"
                add = ["git", "add", "-A"]
                if ref:
                    add.append("--intent-to-add")
                code, _, error = run_git_cmd(add, cwd=cwd)
                if code != 0:
                    return empty, f"git add failed with exit code {code}: {error.strip()}"
                command = ["git", "diff", *arguments]
                if ref:
                    command.append(ref)
                else:
                    command.extend(["--cached", "4b825dc642cb6eb9a060e54bf8d69288fbee4904"])
                code, output, error = run_git_cmd(command, cwd=cwd, binary=binary)
                if code != 0:
                    return empty, f"git diff failed with exit code {code}: {error.strip()}"
                return output, None
            finally:
                _diff_index.reset(token)
    except OSError as exc:
        return empty, f"Cannot construct private Git diff index: {exc}"


def get_git_diff_status(cwd: str, base_rev: Optional[str] = None, sub_baselines: Optional[Dict[str, str]] = None) -> Tuple[str, Optional[str]]:
    """
    Extracts git diff for the workspace, including newly added, modified, and deleted files.
    Returns (diff: str, error: Optional[str]).
    Enforces fail-closed security: if git command errors (e.g. invalid repo, exit 128),
    returns the explicit error instead of disguising as an empty diff.
    """
    if not cwd:
        cwd = os.getcwd()

    diff_out, error = _read_workspace_diff(cwd, base_rev)
    if error:
        return "", error

    main_diff = diff_out.strip() if diff_out else ""

    # Recursively extract actual submodule diffs against sub_baselines
    sub_diffs = []
    try:
        sub_paths = get_submodule_paths(cwd)
    except OSError as exc:
        return "", str(exc)
    for sub_rel in sub_paths:
        sub_p = Path(cwd) / sub_rel
        if sub_p.exists() and (sub_p / ".git").exists():
            sub_base = sub_baselines.get(sub_rel) if sub_baselines else None
            if not sub_base:
                rev_code, gitlink_out, _ = run_git_cmd(["git", "rev-parse", f"HEAD:{sub_rel}"], cwd=cwd)
                if rev_code == 0 and gitlink_out and gitlink_out.strip():
                    sub_base = gitlink_out.strip()
            sub_ref = sub_base if sub_base else "HEAD"
            s_diff, s_err = _read_workspace_diff(str(sub_p), sub_ref, arguments=("--binary",))
            if not s_err and s_diff and s_diff.strip():
                sub_diffs.append(f"\n--- [Submodule: {sub_rel}] (diff against {sub_ref}) ---\n{s_diff.strip()}")
            elif s_err:
                return "", f"Submodule {sub_rel} diff extraction failed: {s_err}"

    full_diff = main_diff
    if sub_diffs:
        full_diff = (full_diff + "\n" if full_diff else "") + "\n".join(sub_diffs)

    return full_diff.strip(), None

def get_git_diff(cwd: str, base_rev: Optional[str] = None, sub_baselines: Optional[Dict[str, str]] = None) -> str:
    diff_text, error = get_git_diff_status(cwd, base_rev=base_rev, sub_baselines=sub_baselines)
    if error:
        raise OSError(error)
    return diff_text


def get_dirty_files(cwd: Optional[Union[str, Path]] = None) -> List[str]:
    """
    Returns relative paths of all dirty files (modified, untracked, staged, renamed)
    in the git repository. Returns candidate files if not inside a git repository.
    """
    if not cwd:
        cwd = os.getcwd()
    cwd_str = str(cwd)

    # 1. Try git status --porcelain -uall
    code, out, _ = run_git_cmd(["git", "status", "--porcelain", "-uall"], cwd=cwd_str)
    if code == 0 and out is not None:
        dirty: List[str] = []
        for line in out.splitlines():
            line = line.strip()
            if len(line) < 3:
                continue
            path_part = line[2:].strip()
            if " -> " in path_part:
                path_part = path_part.split(" -> ")[1].strip()
            path_part = path_part.strip('"\'')
            if path_part and path_part not in dirty:
                dirty.append(path_part)
        return dirty

    # 2. Fallback for non-git directories: collect candidate source files in cwd
    try:
        from makewand.repomap import _collect_candidate_files
        return _collect_candidate_files(Path(cwd_str))
    except Exception:
        fallback_files: List[str] = []
        for root, dirs, files in os.walk(cwd_str):
            dirs[:] = [d for d in dirs if not d.startswith(".") and d not in ("node_modules", "vendor", "__pycache__", "venv")]
            for f in files:
                rel = os.path.relpath(os.path.join(root, f), cwd_str)
                fallback_files.append(rel)
        return fallback_files[:100]


CLONE_EXCLUDED_DIRS = {
    ".git", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache",
    "node_modules", ".venv", "venv", "env", "benchmarks", ".tox", "dist", "build", ".cache"
}


def list_workspace_copy_paths(src_dir: Union[str, Path]) -> List[str]:
    """Relative paths of tracked and untracked-but-not-ignored entries under src_dir.

    .gitignore rules are honoured for non-git directories too, through a
    throw-away git directory outside the workspace. Raises OSError when the
    rules cannot be evaluated, so callers never fall back to copying secrets.
    """
    if os.name == "nt":
        windows_git_directory(src_dir)
    src = Path(src_dir).resolve()
    if find_git_root(src):
        code, out, err = run_git_cmd(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"], cwd=str(src), binary=True)
    else:
        probe = tempfile.mkdtemp(prefix="makewand-ignore-probe-")
        try:
            code, _, err = run_git_cmd(["git", "init", "-q", probe])
            if code == 0:
                code, out, err = run_git_cmd([
                    "git", f"--git-dir={os.path.join(probe, '.git')}", f"--work-tree={src}",
                    "ls-files", "-z", "--others", "--exclude-standard",
                ], cwd=str(src), binary=True)
        finally:
            shutil.rmtree(probe, ignore_errors=True)
    if code != 0:
        detail = os.fsdecode(err).strip() if isinstance(err, bytes) else str(err or "").strip()
        raise OSError(f"无法按 .gitignore 规则枚举工作区文件 (rc={code}): {detail[:200]}")
    return sorted({os.fsdecode(name) for name in out.split(b"\0") if name})


_REFLINK_SUPPORTED: Optional[bool] = None


def _is_reflink_supported() -> bool:
    """Checks whether kernel copy_file_range is supported on this platform."""
    global _REFLINK_SUPPORTED
    if _REFLINK_SUPPORTED is not None:
        return _REFLINK_SUPPORTED
    if os.name == "nt":
        _REFLINK_SUPPORTED = False
        return False
    _REFLINK_SUPPORTED = hasattr(os, "copy_file_range")
    return _REFLINK_SUPPORTED


def _copy_file_range(src_item: Path, dst_item: Path) -> bool:
    """
    Attempts zero-overhead kernel copy using os.copy_file_range.
    Preserves file permissions and metadata via shutil.copystat.
    Returns True if copy succeeded, False otherwise.
    """
    if not hasattr(os, "copy_file_range") or os.name == "nt":
        return False
    src_fd = None
    dst_fd = None
    try:
        src_fd = os.open(str(src_item), os.O_RDONLY)
        st = os.fstat(src_fd)
        if not stat.S_ISREG(st.st_mode):
            return False
        total_bytes = st.st_size
        # Ensure user write permission during copy; copystat restores original mode and metadata later
        dst_fd = os.open(str(dst_item), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, (st.st_mode & 0o777) | 0o600)
        copied = 0
        while copied < total_bytes:
            chunk = min(total_bytes - copied, 1 << 30)  # 1GB chunk
            n = os.copy_file_range(src_fd, dst_fd, chunk)
            if n == 0:
                break
            copied += n
        if copied != total_bytes:
            if dst_fd is not None:
                try:
                    os.close(dst_fd)
                except Exception:
                    pass
                dst_fd = None
            try:
                dst_item.unlink(missing_ok=True)
            except Exception:
                pass
            return False
        # Close write descriptor before copying metadata and timestamps
        if dst_fd is not None:
            try:
                os.close(dst_fd)
            except Exception:
                pass
            dst_fd = None
        try:
            shutil.copystat(src_item, dst_item, follow_symlinks=False)
        except Exception:
            pass
        return True
    except Exception:
        if dst_fd is not None:
            try:
                os.close(dst_fd)
            except Exception:
                pass
            dst_fd = None
        try:
            dst_item.unlink(missing_ok=True)
        except Exception:
            pass
        return False
    finally:
        if dst_fd is not None:
            try:
                os.close(dst_fd)
            except Exception:
                pass
        if src_fd is not None:
            try:
                os.close(src_fd)
            except Exception:
                pass


def _copy_file_with_reflink(src_item: Path, dst_item: Path):
    """
    Copies a regular file using kernel copy_file_range where supported to avoid
    subprocess overhead and physical copy duplication, falling back to shutil.copy2.
    """
    if _copy_file_range(src_item, dst_item):
        return
    shutil.copy2(filesystem_path(src_item), filesystem_path(dst_item), follow_symlinks=False)


def clone_isolated_worktree(src_dir: str, target_dir: Path):
    """
    Safely copies a workspace into an isolated directory for race or testing.
    Only tracked and untracked-but-not-ignored files are copied: files matched
    by .gitignore (.env, data, logs, ...) never leave the user's workspace.
    Skips system sockets, fifos, .git and untracked dependency/build directories.
    Cache names never exclude tracked files or deliverable source files.
    Preserves internal symlinks safely remapped to target_dir.
    Enforces fail-closed protection against write-through external symlinks when bwrap is unavailable.
    Raises OSError when the copy or its git baseline cannot be established.
    """
    if os.name == "nt":
        # Refuse unsupported roots before target creation, ignore evaluation or
        # copying any workspace bytes. Long nested file paths remain supported.
        windows_git_directory(src_dir)
        windows_git_directory(target_dir)
    target_dir = Path(target_dir)
    if _fs_path(target_dir).is_symlink() or _is_reparse_path(target_dir):
        raise OSError("isolated workspace target must not be a symlink")
    _fs_path(target_dir).mkdir(mode=0o700, parents=True, exist_ok=True)
    resolved = Path(src_dir).resolve()
    from makewand.sandbox import is_bwrap_available
    has_bwrap = is_bwrap_available()

    copy_errors: List[str] = []
    copied_paths = set()
    # A cache directory name is not evidence that its contents are disposable.
    # Preserve its deliverable files, and preserve tracked files everywhere.
    excluded = CLONE_EXCLUDED_DIRS - {"__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".cache"}
    def copy_inputs(source_root: Path, destination_root: Path):
        tracked = set()
        if find_git_root(source_root):
            code, names, detail = run_git_cmd(["git", "ls-files", "--cached", "-z", "--", "."], cwd=str(source_root), binary=True)
            if code != 0:
                raise OSError(f"无法核对隔离副本的已跟踪文件: {os.fsdecode(detail)}")
            tracked = {os.fsdecode(name) for name in names.split(b"\0") if name}
        for rel in list_workspace_copy_paths(source_root):
            rel = rel.rstrip("/")
            parts = Path(rel).parts
            if not parts or ".git" in parts or (rel not in tracked and any(part in excluded for part in parts)):
                continue
            if Path(rel).is_absolute() or ".." in parts:
                raise OSError(f"invalid workspace path from git: {rel}")
            src_item = source_root / rel
            dst_item = destination_root / rel
            try:
                # A cached Git path can survive replacement of its parent with
                # a symlink. Do not read outside the source or write through a
                # copied directory link while materializing that tracked path.
                for root_path, item in ((source_root, src_item), (target_dir, dst_item)):
                    parent = item.parent
                    while parent != root_path:
                        if _fs_path(parent).is_symlink() or _is_reparse_path(parent):
                            raise OSError(f"workspace path crosses directory symlink: {item}")
                        parent = parent.parent
                if not os.path.lexists(filesystem_path(src_item)):
                    continue
                dst_api = _fs_path(dst_item)
                src_api = _fs_path(src_item)
                dst_api.parent.mkdir(parents=True, exist_ok=True)
                if os.name == "nt" and bool(getattr(src_api.lstat(), "st_file_attributes", 0) & 0x400):
                    raise OSError("Windows isolated workspaces cannot contain reparse points")
                if src_api.is_symlink():
                    os.symlink(os.readlink(src_api), dst_api)
                    copied_paths.add(dst_item.relative_to(target_dir).as_posix())
                elif src_api.is_dir():
                    # Gitlinks and nested repositories have their own ignore
                    # rules. Re-enumerate there; never copy the directory whole.
                    dst_api.mkdir(parents=True, exist_ok=True)
                    copy_inputs(src_item, dst_item)
                elif src_api.is_file():
                    _copy_file_with_reflink(src_item, dst_item)
                    copied_paths.add(dst_item.relative_to(target_dir).as_posix())
            except OSError as exc:
                copy_errors.append(f"{src_item.relative_to(resolved)}: {exc}")
    if resolved not in _refused_init_roots():
        copy_inputs(resolved, target_dir)
    if copy_errors:
        raise OSError(f"隔离副本复制不完整: {'; '.join(copy_errors[:3])}")

    # Sanitize symlinks across target_dir
    sanitize_shadow_symlinks(target_dir, resolved)

    # Fail-closed check: remove any lingering external symlinks if bwrap is not available
    if not has_bwrap:
        ext_links = find_external_symlinks(target_dir, resolved, allow_repo_root=False)
        for link_path, _ in ext_links:
            try:
                link_path.unlink()
            except Exception:
                pass

    from makewand.telemetry import stage
    with stage("prepare", engine="git-baseline"):
        # Initialize isolated git baseline in target_dir so all existing files are committed
        for cmd in (["git", "init", "-q"],
                    ["git", "config", "user.name", "Makewand"],
                    ["git", "config", "user.email", "makewand@local"],
                    ["git", "add", "-A"]):
            code, _, err = run_git_cmd(cmd, cwd=str(target_dir))
            if code != 0:
                raise OSError(f"隔离副本基线建立失败 ({' '.join(cmd[1:3])}, rc={code}): {(err or '').strip()[:200]}")
        # Only force-add the exact safely enumerated files. A source file that was
        # tracked before a later .gitignore rule must remain tracked in the clone.
        # Literal pathspecs prevent a copied filename containing '*' from admitting
        # other ignored files. Sanitized, removed links are not staged.
        copied = sorted(path for path in copied_paths if os.path.lexists(filesystem_path(target_dir / path)))
        for offset in range(0, len(copied), 256):
            code, _, err = run_git_cmd(["git", "--literal-pathspecs", "add", "-f", "--", *copied[offset:offset + 256]], cwd=str(target_dir))
            if code != 0:
                raise OSError(f"隔离副本安全基线暂存失败: {(err or '').strip()[:200]}")
        code, _, err = run_git_cmd(["git", "commit", "-q", "--no-verify", "-m", "Makewand isolated baseline", "--allow-empty"], cwd=str(target_dir))
        if code != 0:
            raise OSError(f"隔离副本基线提交失败: {(err or '').strip()[:200]}")

def get_active_interactive_working_trees():
    """
    Returns a mapping of canonical working directory paths to session details
    for all active interactive AI sessions (tmux panes + external terminals).
    """
    active_trees = {}
    try:
        from makewand.observer import get_external_ai_sessions, get_active_tmux_sessions, get_session_cwd
        # 1. Tmux sessions
        for s in get_active_tmux_sessions():
            cwd = get_session_cwd(s)
            if cwd and os.path.exists(cwd):
                canon = str(Path(cwd).resolve())
                active_trees[canon] = {
                    "source": "tmux",
                    "session_name": s,
                    "cwd": canon
                }

        # 2. External AI sessions
        for ext in get_external_ai_sessions():
            cwd = ext.get("cwd")
            if cwd and os.path.exists(cwd):
                canon = str(Path(cwd).resolve())
                if canon not in active_trees:
                    active_trees[canon] = {
                        "source": "external_terminal",
                        "tty": ext.get("tty"),
                        "pid": ext.get("pid"),
                        "ai_type": ext.get("ai_type"),
                        "cwd": canon
                    }
    except Exception:
        pass
    return active_trees

P920_PROTECTED_SUBSTRINGS = [
    "/release-candidates/",
    "/static-releases/",
    "/runtime-venvs/",
]

def get_protected_paths() -> List[Path]:
    """
    Returns user-configured forbidden production paths.
    Loaded dynamically from MAKEWAND_PROTECTED_PATHS environment variable
    or ~/.config/makewand/protected_paths.json.
    """
    paths = []
    env_val = os.environ.get("MAKEWAND_PROTECTED_PATHS", "")
    if env_val:
        for p in env_val.split(os.pathsep):
            if p.strip():
                try:
                    paths.append(Path(p.strip()).expanduser().resolve())
                except Exception:
                    pass

    try:
        from makewand.config import CONFIG_DIR
        prot_file = CONFIG_DIR / "protected_paths.json"
        if prot_file.exists():
            with open(prot_file, "r", encoding="utf-8") as f:
                data = json.load(f)
                if isinstance(data, list):
                    for p in data:
                        paths.append(Path(str(p)).expanduser().resolve())
    except Exception:
        pass

    return paths

def is_protected_production_path(path: Union[str, Path]) -> Tuple[bool, Optional[str]]:
    """
    Checks whether the path is a forbidden production / release-candidate tree.
    """
    if not path:
        return False, None
    try:
        resolved = Path(path).expanduser().resolve()
        resolved_str = str(resolved)
        for p in get_protected_paths():
            if resolved == p or p in resolved.parents:
                return True, f"目标路径 {resolved} 位于受保护生产封印目录 ({p}) 下，严禁直接热写"
        for sub in P920_PROTECTED_SUBSTRINGS:
            if sub in resolved_str:
                return True, f"目标路径 {resolved} 包含受保护生产封印模式 ({sub})，严禁直接热写"
    except Exception:
        pass
    return False, None

# Backward compatibility alias
is_p920_protected_path = is_protected_production_path

def check_working_tree_isolation(target_dir: str):
    """
    Checks if target_dir overlaps with any active interactive AI session
    or is a protected production directory.
    Returns (is_safe, conflict_warning_message).
    """
    if not target_dir:
        return True, None
    try:
        target_path = Path(target_dir).resolve()

        # 1. Production Tree Guard
        is_prod, prod_msg = is_protected_production_path(target_path)
        if is_prod:
            return False, f"生产保护警报: {prod_msg}"

        # 2. Multi-Session Overlap Detection (bidirectional)
        active = get_active_interactive_working_trees()
        for active_cwd, info in active.items():
            active_path = Path(active_cwd).resolve()
            if target_path == active_path or active_path in target_path.parents or target_path in active_path.parents:
                src_desc = f"tmux 会话 [{info['session_name']}]" if info.get("source") == "tmux" else f"外部独立终端 [{info.get('tty')} · PID {info.get('pid')} · {info.get('ai_type')}]"
                return False, f"工作区 {target_dir} 与活跃交互会话 ({src_desc}) 存在工作树重叠冲突"

        # 3. Dirty Working Tree Guard: if working tree has any uncommitted changes,
        # enforce shadow worktree to protect user WIP from accidental rollback.
        # A non-git directory is handled by the host transaction; a git repository
        # whose status cannot be read is never assumed clean.
        if find_git_root(target_path):
            code, status_out, status_err = run_git_cmd(["git", "status", "--porcelain"], cwd=str(target_path))
            if code != 0:
                return False, f"无法读取工作区 {target_dir} 的 git 状态 (rc={code}: {(status_err or '').strip()[:120]})，按不安全处理"
            if status_out and status_out.strip():
                return False, f"工作区 {target_dir} 存在未提交的代码修改 (Dirty Working Tree)"
    except Exception as exc:
        return False, f"工作区隔离检查异常 ({exc})，按不安全处理"
    return True, None

class ShadowWorktreeResult(tuple):
    """
    Backward-compatible 3-tuple (effective_dir, branch_name, cleanup)
    augmented with delivery isolation metadata.
    """
    def __new__(cls, effective_dir, branch_name, cleanup, baseline_commit=None, repo_head=None, repo_root=None, worktree_root=None, sub_baselines=None):
        return super().__new__(cls, (effective_dir, branch_name, cleanup))

    def __init__(self, effective_dir, branch_name, cleanup, baseline_commit=None, repo_head=None, repo_root=None, worktree_root=None, sub_baselines=None):
        self.effective_dir = effective_dir
        self.branch_name = branch_name
        self.cleanup = cleanup
        self.baseline_commit = baseline_commit
        self.repo_head = repo_head
        self.repo_root = repo_root
        self.worktree_root = worktree_root
        self.sub_baselines = sub_baselines or {}

def find_external_symlinks(worktree_dir: Path, repo_root: Optional[Path] = None, allow_repo_root: bool = False) -> List[Tuple[Path, str]]:
    """
    Finds all symlinks within worktree_dir that point outside worktree_dir (or repo_root if allow_repo_root is True).
    Returns list of (symlink_path, target_string).
    """
    external = []
    try:
        wt_resolved = worktree_dir.resolve()
        repo_resolved = repo_root.resolve() if repo_root else None
    except Exception:
        return []

    for root, dirs, files in _walk_filesystem(worktree_dir):
        items = [(f, False) for f in files] + [(d, True) for d in dirs]
        for name, is_dir in items:
            p = Path(root) / name
            if _fs_path(p).is_symlink():
                try:
                    raw_target = os.readlink(_fs_path(p))
                    resolved = p.resolve()
                    is_in_wt = resolved.is_relative_to(wt_resolved)
                    is_in_repo = (repo_resolved is not None and resolved.is_relative_to(repo_resolved))
                    if is_in_wt or (allow_repo_root and is_in_repo):
                        continue
                    external.append((p, raw_target))
                except Exception:
                    pass
    return external

def sanitize_shadow_symlinks(worktree_dir: Path, repo_root: Path):
    """
    Audit and sanitize all symlinks within worktree_dir.
    - When bwrap sandbox is available:
      Internal symlinks resolve safely because sandbox mounts host root read-only.
    - When bwrap sandbox is not available:
      Rewrites ALL symlinks (both absolute and relative) that resolve to repo_root so they resolve
      strictly within worktree_dir via relative paths.
    """
    try:
        wt_resolved = worktree_dir.resolve()
        repo_resolved = repo_root.resolve()
    except Exception:
        return

    from makewand.sandbox import is_bwrap_available
    has_bwrap = is_bwrap_available()

    if has_bwrap:
        return

    for root, dirs, files in _walk_filesystem(worktree_dir):
        items = [(f, False) for f in files] + [(d, True) for d in dirs]
        for name, is_dir in items:
            p = Path(root) / name
            if _fs_path(p).is_symlink():
                try:
                    resolved = p.resolve()
                    # If it resolves inside repo_root and NOT inside worktree_dir, remap to worktree_dir
                    if resolved.is_relative_to(repo_resolved) and not resolved.is_relative_to(wt_resolved):
                        rel = resolved.relative_to(repo_resolved)
                        new_target = wt_resolved / rel
                        rel_target = os.path.relpath(new_target, p.parent)
                        _fs_path(p).unlink()
                        os.symlink(rel_target, _fs_path(p))
                    elif resolved.is_relative_to(wt_resolved):
                        raw_target = os.readlink(_fs_path(p))
                        if Path(raw_target).is_absolute():
                            rel_target = os.path.relpath(resolved, p.parent)
                            _fs_path(p).unlink()
                            os.symlink(rel_target, _fs_path(p))
                except Exception:
                    pass

def create_ephemeral_shadow_worktree(base_dir: str, prefix: str = "shadow"):
    """
    Creates an isolated ephemeral git worktree or shadow copy for base_dir.
    Carries forward uncommitted tracked and untracked changes into a clean baseline commit
    without touching the shared .git/config or altering original files.
    Preserves relative subdirectories when called from inside a repository.
    Returns: (effective_worktree_dir, branch_name, cleanup_callback) as ShadowWorktreeResult.
    """
    import sys
    import uuid
    from datetime import datetime

    base_path = Path(base_dir).resolve()
    timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
    rand_id = uuid.uuid4().hex[:6]
    try:
        # Private (0700), unpredictable directory; never a shared fixed /tmp path.
        worktree_dir = create_private_shadow_dir(base_path.name)
    except OSError as exc:
        print(c(f"❌ [Makewand Guard] 无法创建私有影子工作树目录 ({exc})。", COLOR_RED), file=sys.stderr)
        return None, None, None
    branch_name = f"makewand/{prefix}_{timestamp}_{rand_id}"

    def discard_worktree_dir():
        errors = _rmtree_collect(worktree_dir) if worktree_dir.exists() else []
        if errors:
            print(c(f"⚠ [Makewand Guard] 影子工作树残留清理失败: {errors[0]}", COLOR_YELLOW), file=sys.stderr)

    # Check if base_dir is inside a git repository
    code, is_inside, is_inside_err = run_git_cmd(["git", "rev-parse", "--is-inside-work-tree"], cwd=str(base_path))
    if code != 0 and find_git_root(base_path):
        print(c(f"❌ [Makewand Guard] 无法读取仓库状态 (rc={code}: {(is_inside_err or '').strip()[:120]})，拒绝以非 Git 副本代替影子工作树。", COLOR_RED), file=sys.stderr)
        discard_worktree_dir()
        return None, None, None
    if code == 0 and "true" in is_inside.strip().lower():
        code, repo_root_str, _ = run_git_cmd(["git", "rev-parse", "--show-toplevel"], cwd=str(base_path))
        repo_root = Path(repo_root_str.strip()).resolve() if code == 0 and repo_root_str.strip() else base_path

        # Record original repository HEAD commit hash before dirty changes
        _, head_out, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(repo_root))
        repo_head_hash = head_out.strip() if head_out else None

        try:
            rel_sub = base_path.relative_to(repo_root)
        except ValueError:
            rel_sub = Path(".")

        # Create lightweight independent clone with shared objects (decoupled git metadata for sandbox writeability)
        cmd = ["git", "-c", "protocol.file.allow=always", "clone", "--shared", str(repo_root), str(worktree_dir)]
        wt_code, wt_out, wt_err = run_git_cmd(cmd)
        if wt_code != 0:
            # A failed or timed-out clone leaves a partial directory; never fall
            # back to copying into it.
            print(c(f"❌ [Makewand Guard] 影子工作树克隆失败 (rc={wt_code}: {(wt_err or '').strip()[:160]})，拒绝使用残缺副本。", COLOR_RED), file=sys.stderr)
            discard_worktree_dir()
            return None, None, None
        if wt_code == 0:
            def cleanup():
                shutil.rmtree(worktree_dir, ignore_errors=True)

            co_target = repo_head_hash if repo_head_hash else "HEAD"
            co_code, _, co_err = run_git_cmd(["git", "checkout", "-q", "-b", branch_name, co_target], cwd=str(worktree_dir))
            if co_code != 0:
                print(c(f"❌ [Makewand Guard] 影子分支检出失败 (rc={co_code}: {(co_err or '').strip()[:160]})，拒绝残缺快照。", COLOR_RED), file=sys.stderr)
                cleanup()
                return None, None, None

            # 0. Submodule recursion & dirty state forwarding
            sub_baselines = {}
            if (repo_root / ".gitmodules").exists():
                sub_code, _, sub_err = run_git_cmd(["git", "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive"], cwd=str(worktree_dir))
                if sub_code != 0:
                    import sys
                    print(c(f"❌ [Makewand Guard] 影子工作树子模块初始化失败 ({sub_err})，拒绝以残缺快照作为基线。", COLOR_RED), file=sys.stderr)
                    cleanup()
                    return None, None, None

                # Check and synchronize actual checkout commits and dirty state for all submodules
                sorted_subs = get_submodule_paths(str(repo_root))
                for sub_rel in sorted_subs:
                    src_sub = repo_root / sub_rel
                    dst_sub = worktree_dir / sub_rel
                    if src_sub.exists() and dst_sub.exists() and (src_sub / ".git").exists():
                        # 1. Sync exact checked-out commit from src_sub to dst_sub
                        sc_code, sc_out, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(src_sub))
                        if sc_code == 0 and sc_out.strip():
                            target_sub_commit = sc_out.strip()
                            run_git_cmd(["git", "fetch", str(src_sub.resolve()), target_sub_commit], cwd=str(dst_sub))
                            co_code, _, co_err = run_git_cmd(["git", "checkout", target_sub_commit], cwd=str(dst_sub))
                            if co_code != 0:
                                import sys
                                print(c(f"❌ [Makewand Guard] 子模块 {sub_rel} 检出失败 ({co_err})，拒绝残缺快照。", COLOR_RED), file=sys.stderr)
                                cleanup()
                                return None, None, None
                            _, dst_head, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(dst_sub))
                            if dst_head.strip() != target_sub_commit:
                                import sys
                                print(c(f"❌ [Makewand Guard] 子模块 {sub_rel} 版本校验不一致，拒绝残缺快照。", COLOR_RED), file=sys.stderr)
                                cleanup()
                                return None, None, None

                        # 2. Forward submodule dirty diffs with full index
                        sd_code, sd_diff, _ = run_git_cmd(["git", "diff", "--binary", "--full-index", "HEAD"], cwd=str(src_sub), binary=True)
                        if sd_code == 0 and sd_diff and len(sd_diff.strip()) > 0:
                            sa_code, _, sa_err = run_git_cmd(["git", "apply", "--binary", "-"], cwd=str(dst_sub), input_data=sd_diff)
                            if sa_code != 0:
                                import sys
                                print(c(f"❌ [Makewand Guard] 子模块 {sub_rel} 改动应用失败 ({sa_err})，拒绝残缺快照。", COLOR_RED), file=sys.stderr)
                                cleanup()
                                return None, None, None

                        # 3. Forward submodule untracked files and symlinks with traversal defense
                        su_code, su_out_b, su_err = run_git_cmd(["git", "ls-files", "-z", "--others", "--exclude-standard"], cwd=str(src_sub), binary=True)
                        if su_code != 0:
                            import sys
                            print(c(f"❌ [Makewand Guard] 列举子模块 {sub_rel} 未跟踪文件失败 ({su_err})，拒绝残缺快照。", COLOR_RED), file=sys.stderr)
                            cleanup()
                            return None, None, None

                        if su_out_b:
                            dst_sub_resolved = dst_sub.resolve()
                            src_sub_resolved = src_sub.resolve()
                            for su_b in su_out_b.split(b"\0"):
                                if not su_b:
                                    continue
                                try:
                                    su_path = Path(os.fsdecode(su_b))
                                    s_src = src_sub / su_path
                                    s_dst = dst_sub / su_path

                                    # Prevent symlink directory traversal escape
                                    dst_parent_resolved = s_dst.parent.resolve()
                                    if not dst_parent_resolved.is_relative_to(dst_sub_resolved):
                                        cleanup()
                                        return None, None, None

                                    if s_src.is_symlink():
                                        s_dst.parent.mkdir(parents=True, exist_ok=True)
                                        if s_dst.exists() or s_dst.is_symlink():
                                            s_dst.unlink()
                                        raw_target = os.readlink(s_src)
                                        raw_path = Path(raw_target)
                                        if raw_path.is_absolute():
                                            resolved_target = raw_path.resolve()
                                            if resolved_target.is_relative_to(src_sub_resolved):
                                                from makewand.sandbox import is_bwrap_available
                                                if is_bwrap_available():
                                                    os.symlink(raw_target, s_dst)
                                                else:
                                                    new_target = dst_sub_resolved / resolved_target.relative_to(src_sub_resolved)
                                                    rel_target = os.path.relpath(new_target, s_dst.parent)
                                                    os.symlink(rel_target, s_dst)
                                            else:
                                                # External symlink: preserve link without copying external content
                                                os.symlink(raw_target, s_dst)
                                        else:
                                            os.symlink(raw_target, s_dst)
                                    elif s_src.is_file():
                                        s_dst.parent.mkdir(parents=True, exist_ok=True)
                                        if s_dst.is_symlink():
                                            s_dst.unlink()
                                        shutil.copy2(s_src, s_dst, follow_symlinks=False)
                                except Exception as e:
                                    import sys
                                    print(c(f"❌ [Makewand Guard] 复制子模块 {sub_rel} 未跟踪文件失败 ({e})，中止基线建立。", COLOR_RED), file=sys.stderr)
                                    cleanup()
                                    return None, None, None

                        # Sanitize symlinks within submodule
                        sanitize_shadow_symlinks(dst_sub, src_sub)

                        # 4. Commit submodule dirty baseline inside dst_sub for all submodules
                        a_sub_code, _, a_sub_err = run_git_cmd(["git", "add", "-A"], cwd=str(dst_sub))
                        if a_sub_code != 0:
                            import sys
                            print(c(f"❌ [Makewand Guard] 子模块 {sub_rel} 暂存失败 ({a_sub_err})，拒绝残缺快照。", COLOR_RED), file=sys.stderr)
                            cleanup()
                            return None, None, None

                        c_sub_code, _, c_sub_err = run_git_cmd([
                            "git",
                            "-c", "user.name=Makewand Guard",
                            "-c", "user.email=guard@makewand.local",
                            "commit", "-m", "makewand: submodule dirty baseline",
                            "--allow-empty"
                        ], cwd=str(dst_sub))
                        if c_sub_code != 0:
                            import sys
                            print(c(f"❌ [Makewand Guard] 子模块 {sub_rel} 提交失败 ({c_sub_err})，拒绝残缺快照。", COLOR_RED), file=sys.stderr)
                            cleanup()
                            return None, None, None

                        _, s_base_out, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(dst_sub))
                        sub_baselines[sub_rel] = s_base_out.strip() if s_base_out else None

            # 1. Forward modified binary and text diff FIRST using raw bytes
            diff_code, diff_out_b, diff_err = run_git_cmd(["git", "diff", "--binary", "HEAD"], cwd=str(repo_root), binary=True)
            if diff_code != 0:
                cleanup()
                return None, None, None

            if diff_out_b and len(diff_out_b.strip()) > 0:
                apply_code, _, apply_err = run_git_cmd(
                    ["git", "apply", "--binary", "-"],
                    cwd=str(worktree_dir),
                    input_data=diff_out_b
                )
                if apply_code != 0:
                    import sys
                    print(c(f"❌ [Makewand Guard] 影子工作树应用改动失败 ({apply_err})，拒绝以不完整副本作为基线。", COLOR_RED), file=sys.stderr)
                    cleanup()
                    return None, None, None

            # 2. Forward untracked files with NUL delimiter and symlink traversal defense
            u_code, u_out_b, _ = run_git_cmd(["git", "ls-files", "-z", "--others", "--exclude-standard"], cwd=str(repo_root), binary=True)
            if u_code != 0:
                cleanup()
                return None, None, None

            if u_out_b:
                wt_resolved = worktree_dir.resolve()
                repo_resolved = repo_root.resolve()
                for rel_b in u_out_b.split(b"\0"):
                    if not rel_b:
                        continue
                    try:
                        rel_path = Path(os.fsdecode(rel_b))
                        src_f = repo_root / rel_path
                        dst_f = worktree_dir / rel_path

                        # Prevent symlink directory traversal escape
                        dst_parent_resolved = dst_f.parent.resolve()
                        if not dst_parent_resolved.is_relative_to(wt_resolved):
                            cleanup()
                            return None, None, None

                        if src_f.is_symlink():
                            dst_f.parent.mkdir(parents=True, exist_ok=True)
                            if dst_f.exists() or dst_f.is_symlink():
                                dst_f.unlink()
                            raw_target = os.readlink(src_f)
                            raw_path = Path(raw_target)
                            if raw_path.is_absolute():
                                resolved_target = raw_path.resolve()
                                if resolved_target.is_relative_to(repo_resolved):
                                    from makewand.sandbox import is_bwrap_available
                                    if is_bwrap_available():
                                        os.symlink(raw_target, dst_f)
                                    else:
                                        new_target = wt_resolved / resolved_target.relative_to(repo_resolved)
                                        rel_target = os.path.relpath(new_target, dst_f.parent)
                                        os.symlink(rel_target, dst_f)
                                else:
                                    # External symlink: preserve link without copying external content
                                    os.symlink(raw_target, dst_f)
                            else:
                                os.symlink(raw_target, dst_f)
                        elif src_f.is_file():
                            dst_f.parent.mkdir(parents=True, exist_ok=True)
                            if dst_f.is_symlink():
                                dst_f.unlink()
                            shutil.copy2(src_f, dst_f, follow_symlinks=False)
                    except Exception as e:
                        import sys
                        print(c(f"❌ [Makewand Guard] 复制未跟踪文件失败 ({e})，中止基线建立。", COLOR_RED), file=sys.stderr)
                        cleanup()
                        return None, None, None

            # Sanitize all symlinks across shadow worktree
            sanitize_shadow_symlinks(worktree_dir, repo_root)

            # Fail-closed defense: if external symlinks exist and physical sandbox is unavailable, abort
            from makewand.sandbox import is_bwrap_available
            ext_symlinks = find_external_symlinks(worktree_dir, repo_root, allow_repo_root=is_bwrap_available())
            if ext_symlinks and not is_bwrap_available():
                import sys
                ext_desc = f"{ext_symlinks[0][0].name} -> {ext_symlinks[0][1]}"
                print(c(f"❌ [Makewand Guard] 物理沙箱不可用且检测到越界外部符号链接 ({ext_desc})，为防止写穿宿主机阻断任务。", COLOR_RED), file=sys.stderr)
                cleanup()
                return None, None, None

            # 3. Stage and record baseline snapshot without modifying shared .git/config
            add_code, _, _ = run_git_cmd(["git", "add", "-A"], cwd=str(worktree_dir))
            if add_code != 0:
                cleanup()
                return None, None, None

            c_code, _, _ = run_git_cmd([
                "git",
                "-c", "user.name=Makewand Guard",
                "-c", "user.email=guard@makewand.local",
                "commit", "-m", "makewand: forward active session dirty baseline",
                "--allow-empty"
            ], cwd=str(worktree_dir))
            if c_code != 0:
                cleanup()
                return None, None, None

            _, base_commit_out, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(worktree_dir))
            baseline_commit_hash = base_commit_out.strip() if base_commit_out else None

            effective_dir = worktree_dir / rel_sub
            effective_dir.mkdir(parents=True, exist_ok=True)

            return ShadowWorktreeResult(
                str(effective_dir),
                branch_name,
                cleanup,
                baseline_commit=baseline_commit_hash,
                repo_head=repo_head_hash,
                repo_root=str(repo_root),
                worktree_root=str(worktree_dir),
                sub_baselines=sub_baselines
            )

    # Fallback to standalone isolated copy (no git branch) for non-git directories
    try:
        clone_isolated_worktree(str(base_path), worktree_dir)
        baseline_code, baseline, baseline_error = run_git_cmd(
            ["git", "rev-parse", "HEAD"], cwd=str(worktree_dir))
        if baseline_code or not baseline.strip():
            raise OSError(f"cannot identify isolated copy baseline: {baseline_error}")
        def clone_cleanup():
            shutil.rmtree(worktree_dir, ignore_errors=True)

        return ShadowWorktreeResult(
            str(worktree_dir),
            None,
            clone_cleanup,
            baseline_commit=baseline.strip(),
            repo_head=None,
            repo_root=str(base_path),
            worktree_root=str(worktree_dir)
        )
    except Exception as exc:
        print(c(f"❌ [Makewand Guard] 隔离副本建立失败 ({exc})。", COLOR_RED), file=sys.stderr)
        discard_worktree_dir()
        return None, None, None


# ---------------------------------------------------------------------------
# Private artifacts (shared contract 1): 0700 directories, 0600 files, names
# from tempfile.mkdtemp (unpredictable), writes never follow symlinks, and a
# bounded retention so state does not grow without limit.
# ---------------------------------------------------------------------------

ARTIFACT_PREFIXES = ("delivery_", "rejected_", "txn_", "baseline_")
DEFAULT_ARTIFACT_RETENTION = 50
DEFAULT_SHADOW_RETENTION = 20
# Rollback backups and shadow worktrees may belong to a task that is still
# running; they are never pruned while young.
PROTECTED_YOUNG_SECONDS = 24 * 3600


def _positive_int_env(name: str, default: int) -> int:
    try:
        value = int(os.environ.get(name, "").strip() or default)
    except ValueError:
        return default
    return value if value > 0 else default


def _prune_old_entries(root: Path, keep: int, prefixes: Tuple[str, ...], young_protected: Tuple[str, ...]) -> List[str]:
    """Keeps the newest ``keep`` entries with the given prefixes; returns removed names."""
    entries = []
    with os.scandir(filesystem_path(root)) as iterator:
        for entry in iterator:
            if entry.name.startswith(prefixes):
                try:
                    entries.append((entry.stat(follow_symlinks=False).st_mtime, entry.name, entry))
                except OSError:
                    continue
    entries.sort(reverse=True)
    removed = []
    now = time.time()
    for mtime, name, entry in entries[keep:]:
        if name.startswith(young_protected) and now - mtime < PROTECTED_YOUNG_SECONDS:
            continue
        if entry.is_dir(follow_symlinks=False):
            errors = _rmtree_collect(Path(entry.path))
            if errors:
                print(c(f"⚠ [Makewand Artifacts] 清理过期产物 {name} 失败: {errors[0]}", COLOR_YELLOW), file=sys.stderr)
                continue
        else:
            os.unlink(entry.path)
        removed.append(name)
    return removed


def create_private_artifact_dir(kind: str) -> Path:
    """Creates <ARTIFACTS_DIR>/<kind>_<timestamp>_<random> as a private 0700 directory."""
    root = ensure_private_dir(config.ARTIFACTS_DIR)
    try:
        _prune_old_entries(root, _positive_int_env("MAKEWAND_ARTIFACTS_KEEP", DEFAULT_ARTIFACT_RETENTION),
                           ARTIFACT_PREFIXES, ("txn_",))
    except OSError as exc:
        print(c(f"⚠ [Makewand Artifacts] 产物保留策略执行失败: {exc}", COLOR_YELLOW), file=sys.stderr)
    stamp = datetime.now().strftime("%Y%m%d_%H%M%S")
    path = Path(tempfile.mkdtemp(prefix=f"{kind}_{stamp}_", dir=str(root)))
    os.chmod(path, 0o700)
    return path


def create_private_shadow_dir(base_name: str) -> Path:
    """Creates an empty private directory for a shadow worktree under SHADOW_WORKTREES_DIR."""
    root = ensure_private_dir(config.SHADOW_WORKTREES_DIR)
    try:
        _prune_old_entries(root, _positive_int_env("MAKEWAND_SHADOW_KEEP", DEFAULT_SHADOW_RETENTION), ("wt_",), ("wt_",))
    except OSError as exc:
        print(c(f"⚠ [Makewand Shadow] 影子工作树保留策略执行失败: {exc}", COLOR_YELLOW), file=sys.stderr)
    # The logical repository name is metadata, not part of a filesystem budget.
    # An unpredictable short leaf leaves room for .git/objects and nested inputs.
    created = Path(tempfile.mkdtemp(prefix="wt_", dir=filesystem_path(root)))
    path = root / created.name
    os.chmod(filesystem_path(path), 0o700)
    return path


def write_private_file(path: Union[str, Path], data: Union[str, bytes], mode: int = 0o600) -> Path:
    """Creates a new file exclusively (never follows or reuses an existing path) with ``mode``."""
    path = Path(path)
    payload = data.encode("utf-8") if isinstance(data, str) else bytes(data)
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    fd = os.open(path, flags, 0o600)
    with os.fdopen(fd, "wb") as handle:
        if hasattr(os, "fchmod"):
            os.fchmod(handle.fileno(), mode)
        handle.write(payload)
        handle.flush()
    return path


# ---------------------------------------------------------------------------
# Per-repository workspace lock: one makewand code task per repository.
# The lock file lives in the private state directory, never in the user repo.
# ---------------------------------------------------------------------------

class WorkspaceLockError(RuntimeError):
    pass


def workspace_lock_root(cwd: Union[str, Path]) -> str:
    real = os.path.realpath(str(cwd))
    git_root = find_git_root(real)
    if git_root:
        code, out, _ = run_git_cmd(["git", "rev-parse", "--show-toplevel"], cwd=real)
        if code == 0 and out and out.strip():
            return os.path.realpath(out.strip())
        # A git failure must never disable locking: fall back to the filesystem root.
        return os.path.realpath(git_root)
    return real


class WorkspaceLock:
    def __init__(self, cwd: Union[str, Path]):
        self.root = workspace_lock_root(cwd)
        self.path: Optional[Path] = None
        self._handle = None

    def acquire(self) -> "WorkspaceLock":
        from makewand import filelock
        ensure_private_dir(config.ARTIFACTS_DIR)
        lock_dir = ensure_private_dir(Path(config.ARTIFACTS_DIR) / ".locks")
        key = hashlib.sha256(os.fsencode(self.root)).hexdigest()[:32]
        self.path = lock_dir / f"{key}.lock"
        flags = os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
        handle = os.fdopen(os.open(self.path, flags, 0o600), "r+", encoding="utf-8")
        try:
            filelock.flock(handle, filelock.LOCK_EX | filelock.LOCK_NB)
        except OSError:
            detail = ""
            try:
                info = json.loads(handle.read(4096) or "{}")
                if info.get("pid"):
                    detail = f" (PID {info.get('pid')}，开始于 {info.get('started', '?')})"
            except (OSError, ValueError):
                pass
            handle.close()
            raise WorkspaceLockError(f"另一个 makewand 任务正在此目录运行: {self.root}{detail}。请等待该任务结束后再试。")
        handle.seek(0)
        handle.truncate()
        handle.write(json.dumps({"pid": os.getpid(), "root": self.root, "started": datetime.now().isoformat(timespec="seconds")}))
        handle.flush()
        self._handle = handle
        return self

    def release(self) -> None:
        handle, self._handle = self._handle, None
        if handle is None:
            return
        from makewand import filelock
        try:
            handle.seek(0)
            handle.truncate()
            filelock.flock(handle, filelock.LOCK_UN)
        finally:
            handle.close()

    def __enter__(self) -> "WorkspaceLock":
        return self.acquire()

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.release()



# ---------------------------------------------------------------------------
# Host-mode transaction: the task-start snapshot is taken before any git init,
# rollback only removes paths that did not exist before the task, restores
# tracked files from the baseline commit and pre-existing untracked/ignored
# files from private backups, and reports success only after verification.
# ---------------------------------------------------------------------------

EMPTY_TREE_HASH = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
# Large dependency/build directories: contents are recorded as metadata only.
HEAVY_DIR_NAMES = frozenset({
    "node_modules", "venv", ".venv", "env", ".tox", "__pycache__", ".mypy_cache", ".pytest_cache",
    ".ruff_cache", "target", "dist", "build", ".cache", ".gradle", ".next", ".nuxt", "bower_components",
    ".terraform",
})
# Regenerable caches that tests create; not worth a delivery warning.
REGENERABLE_CACHE_NAMES = frozenset({"__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache"})
DEFAULT_BACKUP_FILE_LIMIT = 1024 * 1024
DEFAULT_BACKUP_TOTAL_LIMIT = 50 * 1024 * 1024
DEFAULT_SNAPSHOT_MAX_ENTRIES = 300000
DEFAULT_SNAPSHOT_SECONDS = 120


class SnapshotError(RuntimeError):
    pass


class FsEntry(tuple):
    """(kind, size, mtime_ns, mode, dev, ino, link, atime_ns) for one path."""
    __slots__ = ()
    kind = property(lambda self: self[0])
    size = property(lambda self: self[1])
    mtime_ns = property(lambda self: self[2])
    mode = property(lambda self: self[3])
    dev = property(lambda self: self[4])
    ino = property(lambda self: self[5])
    link = property(lambda self: self[6])
    atime_ns = property(lambda self: self[7])


def _fs_entry(path: str, info: os.stat_result, link: Optional[str] = None) -> FsEntry:
    mode = info.st_mode
    if stat.S_ISLNK(mode):
        kind = "link"
        link = link if link is not None else os.readlink(path)
    elif stat.S_ISDIR(mode):
        kind = "dir"
    elif stat.S_ISREG(mode):
        kind = "file"
    else:
        kind = "other"
    if kind != "link":
        link = None
    size = info.st_size if kind == "file" else 0
    return FsEntry((kind, size, info.st_mtime_ns, stat.S_IMODE(mode), info.st_dev, info.st_ino, link, info.st_atime_ns))


def _same_entry(current: Optional[FsEntry], expected: FsEntry) -> bool:
    if current is None or current.kind != expected.kind:
        return False
    if expected.kind == "file":
        return (current.size, current.mtime_ns, current.mode) == (expected.size, expected.mtime_ns, expected.mode)
    if expected.kind == "link":
        return current.link == expected.link
    return True


def _makewand_state_paths() -> set:
    """Makewand's own state directories are never part of a workspace snapshot."""
    paths = set()
    for candidate_path in (getattr(config, "ARTIFACTS_DIR", None), getattr(config, "SHADOW_WORKTREES_DIR", None),
                           getattr(config, "CONFIG_DIR", None)):
        if candidate_path:
            paths.add(os.path.realpath(str(candidate_path)))
    return paths


def scan_workspace_tree(root: Union[str, Path], strict: bool = True, errors: Optional[List[str]] = None,
                        max_entries: Optional[int] = None, time_budget: Optional[float] = None) -> Dict[str, FsEntry]:
    """Records every path under root (never following symlinks), skipping .git entries
    and makewand's own state directories.

    strict=True raises SnapshotError on any unreadable path or budget overrun,
    because an incomplete task-start snapshot could later misclassify a
    pre-existing file as task-created.
    """
    root = str(root)
    excluded = _makewand_state_paths()
    limit = max_entries or _positive_int_env("MAKEWAND_SNAPSHOT_MAX_ENTRIES", DEFAULT_SNAPSHOT_MAX_ENTRIES)
    deadline = time.monotonic() + (time_budget or _positive_int_env("MAKEWAND_SNAPSHOT_SECONDS", DEFAULT_SNAPSHOT_SECONDS))
    entries: Dict[str, FsEntry] = {}

    def problem(message: str) -> None:
        if strict:
            raise SnapshotError(message)
        if errors is not None:
            errors.append(message)

    stack = [""]
    while stack:
        rel_dir = stack.pop()
        abs_dir = os.path.join(root, rel_dir) if rel_dir else root
        try:
            with os.scandir(abs_dir) as iterator:
                children = list(iterator)
        except OSError as exc:
            problem(f"无法读取目录 {rel_dir or '.'}: {exc.strerror or exc}")
            continue
        for child in children:
            if child.name == ".git" or child.path in excluded:
                continue
            rel = f"{rel_dir}/{child.name}" if rel_dir else child.name
            try:
                entry = _fs_entry(child.path, os.lstat(child.path))
            except OSError as exc:
                problem(f"无法读取 {rel}: {exc.strerror or exc}")
                continue
            entries[rel] = entry
            if entry.kind == "dir":
                stack.append(rel)
        if len(entries) > limit:
            problem(f"工作区条目超过 {limit} 个 (MAKEWAND_SNAPSHOT_MAX_ENTRIES)")
            break
        if time.monotonic() > deadline:
            problem("工作区快照超出时间预算 (MAKEWAND_SNAPSHOT_SECONDS)")
            break
    return entries


# The host transaction restores through directory handles so a symlink planted
# by the task can never redirect a delete or restore outside the workspace.
HOST_TRANSACTION_SUPPORTED = os.name == "posix" and all(
    fn in os.supports_dir_fd for fn in (os.open, os.stat, os.unlink, os.rmdir, os.rename, os.mkdir, os.readlink, os.symlink))

_NOFOLLOW_DIR_FLAGS = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)


def _open_parent(root: str, rel: str, create: bool = False) -> Tuple[int, str]:
    """Opens the parent directory of rel without following any symlink component."""
    parts = rel.split("/")
    fd = os.open(root, _NOFOLLOW_DIR_FLAGS)
    try:
        for part in parts[:-1]:
            if create:
                try:
                    os.mkdir(part, 0o777, dir_fd=fd)
                except FileExistsError:
                    pass
            child = os.open(part, _NOFOLLOW_DIR_FLAGS, dir_fd=fd)
            os.close(fd)
            fd = child
    except BaseException:
        os.close(fd)
        raise
    return fd, parts[-1]


def _lstat_at(root: str, rel: str) -> Optional[FsEntry]:
    try:
        fd, name = _open_parent(root, rel)
    except OSError:
        return None
    try:
        info = os.stat(name, dir_fd=fd, follow_symlinks=False)
        link = os.readlink(name, dir_fd=fd) if stat.S_ISLNK(info.st_mode) else None
    except OSError:
        return None
    finally:
        os.close(fd)
    return _fs_entry(name, info, link)


def _unlink_at(root: str, rel: str) -> None:
    fd, name = _open_parent(root, rel)
    try:
        os.unlink(name, dir_fd=fd)
    finally:
        os.close(fd)


def _rmdir_at(root: str, rel: str) -> None:
    fd, name = _open_parent(root, rel)
    try:
        os.rmdir(name, dir_fd=fd)
    finally:
        os.close(fd)


def _rename_at(root: str, src_rel: str, dst_rel: str) -> None:
    src_fd, src_name = _open_parent(root, src_rel)
    try:
        dst_fd, dst_name = _open_parent(root, dst_rel, create=True)
        try:
            os.rename(src_name, dst_name, src_dir_fd=src_fd, dst_dir_fd=dst_fd)
        finally:
            os.close(dst_fd)
    finally:
        os.close(src_fd)


def _copy_regular_nofollow(source: str, destination: Path) -> Tuple[str, int]:
    """Copies a regular file without following links; returns (sha256, size)."""
    src_fd = os.open(source, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_CLOEXEC", 0))
    with os.fdopen(src_fd, "rb") as src:
        if not stat.S_ISREG(os.fstat(src.fileno()).st_mode):
            raise OSError(errno.EINVAL, "not a regular file")
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
        digest = hashlib.sha256()
        size = 0
        with os.fdopen(os.open(destination, flags, 0o600), "wb") as dst:
            while chunk := src.read(1024 * 1024):
                digest.update(chunk)
                size += len(chunk)
                dst.write(chunk)
    return digest.hexdigest(), size


def _file_sha256_at(root: str, rel: str) -> Optional[str]:
    try:
        fd, name = _open_parent(root, rel)
    except OSError:
        return None
    try:
        file_fd = os.open(name, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0), dir_fd=fd)
    except OSError:
        return None
    finally:
        os.close(fd)
    digest = hashlib.sha256()
    with os.fdopen(file_fd, "rb") as handle:
        if not stat.S_ISREG(os.fstat(handle.fileno()).st_mode):
            return None
        while chunk := handle.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def _summarize_paths(paths: List[str], limit: int = 20) -> str:
    shown = ", ".join(paths[:limit])
    return shown + (f" …（另有 {len(paths) - limit} 项）" if len(paths) > limit else "")


def _is_regenerable_cache(rel: str) -> bool:
    parts = rel.split("/")
    return any(part in REGENERABLE_CACHE_NAMES for part in parts[:-1]) or rel.endswith((".pyc", ".pyo"))


class HostWorkspaceTransaction:
    """Data-safety transaction for a task that edits the user's own directory."""

    def __init__(self, cwd: Union[str, Path]):
        self.cwd = os.path.realpath(str(cwd))
        self.is_git_repo = find_git_root(self.cwd) is not None
        self.root: Optional[str] = None
        self.pre: Dict[str, FsEntry] = {}
        self.created_git = False
        self.keep_git = False
        self.baseline_commit: Optional[str] = None
        self.head_ref: Optional[str] = None
        self.initial_dirty = False
        self.dirty_tracked: set = set()
        self.tracked: set = set()
        self.backup_dir: Optional[Path] = None
        self.backups: Dict[str, Tuple[Path, str]] = {}
        self.unbacked: Dict[str, str] = {}
        self.state = "new"
        self.succeeded: Optional[bool] = None
        self._rejected_dir: Optional[Path] = None

    @property
    def is_active(self) -> bool:
        return self.state == "active"

    @property
    def git_dir(self) -> Path:
        return Path(self.root or self.cwd) / ".git"

    # -- phase 1: before any git init ------------------------------------
    def capture_pre_snapshot(self) -> Optional[str]:
        if not HOST_TRANSACTION_SUPPORTED:
            return "当前平台缺少 POSIX 目录句柄，无法保证宿主模式安全回滚（Windows 请在 WSL2 中运行）"
        if self.is_git_repo:
            code, top, err = run_git_cmd(["git", "rev-parse", "--show-toplevel"], cwd=self.cwd)
            if code != 0 or not top.strip():
                return f"无法定位 Git 仓库根目录 (rc={code}): {(err or '').strip()[:200]}"
            self.root = os.path.realpath(top.strip())
            self._discard_stale_ephemeral_git()
        if not self.is_git_repo:
            self.root = self.cwd
            if Path(self.root) in _refused_init_roots():
                return f"拒绝在系统目录或用户主目录 ({self.root}) 中执行会修改文件的任务，请进入具体项目目录"
        root_prefix = self.root.rstrip(os.sep) + os.sep
        for state_path in sorted(_makewand_state_paths()):
            if state_path == self.root or state_path.startswith(root_prefix):
                return f"makewand 状态目录 {state_path} 位于工作区内，不能在宿主模式下安全回滚"
        try:
            home = Path.home().resolve()
            if home == Path(self.root) or Path(self.root) in home.parents:
                # The user's home holds provider credentials and session state that
                # change during a task; never snapshot/roll back it in place.
                return f"工作区 {self.root} 包含用户主目录，不能在宿主模式下安全回滚"
        except (OSError, RuntimeError):
            pass
        try:
            self.pre = scan_workspace_tree(self.root, strict=True)
        except SnapshotError as exc:
            return f"无法建立任务前完整快照 ({exc})"
        self.state = "snapshotted"
        return None

    def _discard_stale_ephemeral_git(self) -> None:
        """A temporary .git left by an interrupted makewand run is removed when untouched."""
        marker = Path(self.root) / ".git" / EPHEMERAL_GIT_MARKER
        if not marker.is_file() or Path(self.root, ".git").is_symlink():
            return
        try:
            owner = int(json.loads(marker.read_text(encoding="utf-8") or "{}").get("pid") or 0)
        except (OSError, ValueError, TypeError, AttributeError):
            owner = 0
        if owner and owner != os.getpid():
            try:
                os.kill(owner, 0)
                return  # the creating makewand process is still running
            except PermissionError:
                return
            except OSError:
                pass
        count_code, count, _ = run_git_cmd(["git", "rev-list", "--count", "--all"], cwd=self.root)
        subject_code, subject, _ = run_git_cmd(["git", "log", "-1", "--format=%s"], cwd=self.root)
        if count_code == 0 and subject_code == 0 and count.strip() == "1" and subject.strip() == EPHEMERAL_BASELINE_SUBJECT:
            error = _remove_git_dir(Path(self.root) / ".git")
            if error is None:
                print(c(f"[Makewand Git] 已移除上次中断遗留的临时 .git ({self.root})", COLOR_YELLOW))
                self.is_git_repo = find_git_root(self.cwd) is not None
                if self.is_git_repo:
                    code, top, _ = run_git_cmd(["git", "rev-parse", "--show-toplevel"], cwd=self.cwd)
                    self.root = os.path.realpath(top.strip()) if code == 0 and top.strip() else self.root
        else:
            print(c(f"[Makewand Git] {self.root}/.git 带有 makewand 临时标记但已有其他提交，按普通仓库处理", COLOR_YELLOW))

    # -- phase 2: immediately before the first model dispatch -------------
    def begin(self) -> Optional[str]:
        if self.state != "snapshotted":
            return "内部错误: 尚未完成任务前快照"
        try:
            if not self.is_git_repo:
                created, error = init_git_baseline(self.root, ephemeral=True)
                if error or not created:
                    return f"无法为非 Git 目录建立基线，已在派发任何模型前中止，未改动任何文件: {error or '未创建仓库'}"
                self.created_git = True
            error = self._capture_git_baseline() or self._backup_untracked_files()
        except Exception as exc:  # noqa: BLE001 - reported, then aborted
            error = f"建立任务基线时发生异常: {type(exc).__name__}: {exc}"
        if error:
            self._abort_begin()
            return error
        self.state = "active"
        return None

    def _abort_begin(self) -> None:
        self.state = "aborted"
        if self.created_git:
            removal_error = _remove_git_dir(self.git_dir)
            if removal_error:
                self.keep_git = True
                print(c(f"⚠ [Makewand Transaction] {removal_error}", COLOR_RED), file=sys.stderr)
        self._discard_backups()

    def _capture_git_baseline(self) -> Optional[str]:
        code, out, err = run_git_cmd(["git", "status", "--porcelain=v1", "-z", "--untracked-files=all"], cwd=self.root, binary=True)
        if code != 0:
            return f"git status 失败 (rc={code})，无法确认任务前基线，已在派发任何模型前中止: {os.fsdecode(err or b'').strip()[:200]}"
        tokens = [t for t in out.split(b"\0")]
        index = 0
        while index < len(tokens):
            token = tokens[index]
            index += 1
            if len(token) < 4:
                continue
            self.initial_dirty = True
            xy, path = token[:2], os.fsdecode(token[3:])
            if xy != b"??":
                self.dirty_tracked.add(path)
            if xy[:1] in (b"R", b"C") and index < len(tokens):
                self.dirty_tracked.add(os.fsdecode(tokens[index]))
                index += 1
        code, head, err = run_git_cmd(["git", "rev-parse", "--verify", "-q", "HEAD"], cwd=self.root)
        if code == 0 and head.strip():
            self.baseline_commit = head.strip()
        else:
            unborn_code, _, _ = run_git_cmd(["git", "symbolic-ref", "-q", "HEAD"], cwd=self.root)
            if unborn_code != 0:
                return f"无法读取基线提交 (rc={code}): {(err or '').strip()[:200]}"
        ref_code, ref, _ = run_git_cmd(["git", "symbolic-ref", "-q", "HEAD"], cwd=self.root)
        self.head_ref = ref.strip() if ref_code == 0 and ref.strip() else None
        code, listed, err = run_git_cmd(["git", "ls-files", "-z", "--cached"], cwd=self.root, binary=True)
        if code != 0:
            return f"git ls-files 失败 (rc={code})，无法确认被跟踪文件: {os.fsdecode(err or b'').strip()[:200]}"
        self.tracked = {os.fsdecode(name) for name in listed.split(b"\0") if name}
        return None

    def _backup_untracked_files(self) -> Optional[str]:
        per_file = _positive_int_env("MAKEWAND_BACKUP_FILE_LIMIT", DEFAULT_BACKUP_FILE_LIMIT)
        total_limit = _positive_int_env("MAKEWAND_BACKUP_TOTAL_LIMIT", DEFAULT_BACKUP_TOTAL_LIMIT)
        total = 0
        index = []
        for rel, entry in sorted(self.pre.items()):
            if entry.kind != "file" or (rel in self.tracked and rel not in self.dirty_tracked):
                continue
            if any(part in HEAVY_DIR_NAMES for part in rel.split("/")[:-1]):
                self.unbacked[rel] = "位于依赖/构建大目录，只记录元数据"
                continue
            if entry.size > per_file:
                self.unbacked[rel] = f"超过单文件备份上限 {per_file} 字节"
                continue
            if total + entry.size > total_limit:
                self.unbacked[rel] = f"超过备份总量上限 {total_limit} 字节"
                continue
            if self.backup_dir is None:
                self.backup_dir = create_private_artifact_dir("txn")
            target = self.backup_dir / f"{len(self.backups):06d}.bak"
            try:
                digest, size = _copy_regular_nofollow(os.path.join(self.root, rel), target)
            except OSError as exc:
                self.unbacked[rel] = f"无法读取备份 ({exc.strerror or exc})"
                continue
            if size != entry.size:
                self.unbacked[rel] = "备份期间文件发生变化"
                continue
            self.backups[rel] = (target, digest)
            total += size
            index.append({"path": rel, "backup": target.name, "sha256": digest, "mode": oct(entry.mode)})
        if self.backup_dir is not None:
            write_private_file(self.backup_dir / "index.json", json.dumps(
                {"root": self.root, "files": index, "not_backed_up": self.unbacked}, ensure_ascii=False, indent=2))
        return None

    def _discard_backups(self) -> None:
        if self.backup_dir is not None and self.backup_dir.exists():
            errors = _rmtree_collect(self.backup_dir)
            if errors:
                print(c(f"⚠ [Makewand Transaction] 无法删除临时备份 {self.backup_dir}: {errors[0]}", COLOR_YELLOW), file=sys.stderr)
        self.backup_dir = None

    def _git_restores(self, rel: str) -> bool:
        """True when a clean hard reset to the baseline commit is responsible for rel."""
        return bool(self.baseline_commit) and not self.initial_dirty and rel in self.tracked

    # -- failure path -------------------------------------------------------
    def rollback(self, reason: str = "") -> bool:
        if self.state != "active":
            return bool(self.succeeded)
        self.state = "rolling_back"
        problems: List[str] = []
        notes: List[str] = []
        try:
            self._rollback_steps(reason, problems, notes)
        except Exception as exc:  # noqa: BLE001 - never swallowed: reported below
            problems.append(f"回滚过程发生异常: {type(exc).__name__}: {exc}")
        self._finish(not problems, problems, notes, rollback=True)
        return not problems

    def _rejected_artifacts(self) -> Path:
        if self._rejected_dir is None:
            self._rejected_dir = create_private_artifact_dir("rejected")
        return self._rejected_dir

    def _rollback_steps(self, reason: str, problems: List[str], notes: List[str]) -> None:
        root = self.root
        base = self.baseline_commit or EMPTY_TREE_HASH
        code, diff_bytes, _ = run_git_cmd(["git", "diff", "--binary", base], cwd=root, binary=True)
        if code == 0 and diff_bytes and diff_bytes.strip():
            rejected = self._rejected_artifacts()
            write_private_file(rejected / "rejected.patch", diff_bytes)
            write_private_file(rejected / "manifest.json", json.dumps(
                {"repo_root": root, "baseline_commit": self.baseline_commit, "reason": reason}, ensure_ascii=False, indent=2))
            notes.append(f"被拒改动已存档: {rejected / 'rejected.patch'}")
        elif code != 0:
            notes.append(f"无法导出被拒改动补丁 (rc={code})")

        # Before any git reset: git deletes intent-to-add/indexed paths that are not
        # in the baseline, so relocated pre-existing files are put back first and
        # pre-existing untracked files the task added to the index are unstaged.
        scan_errors: List[str] = []
        current = scan_workspace_tree(root, strict=False, errors=scan_errors)
        problems.extend(f"回滚扫描: {msg}" for msg in scan_errors)
        self._recover_relocated(current, problems, notes)

        if self.baseline_commit and not self.initial_dirty:
            untrack_error = self._untrack_preexisting_paths()
            if untrack_error:
                problems.append(untrack_error + "；未删除或恢复任何文件")
                return
            if self.head_ref:
                ref_code, ref, _ = run_git_cmd(["git", "symbolic-ref", "-q", "HEAD"], cwd=root)
                if ref_code != 0 or ref.strip() != self.head_ref:
                    code, _, err = run_git_cmd(["git", "symbolic-ref", "HEAD", self.head_ref], cwd=root)
                    if code != 0:
                        problems.append(f"无法切回原分支 {self.head_ref} (rc={code}: {err.strip()[:120]})；未删除或恢复任何文件")
                        return
            code, _, err = run_git_cmd(["git", "reset", "--hard", "-q", self.baseline_commit], cwd=root)
            if code != 0:
                problems.append(f"git reset --hard 失败 (rc={code}: {err.strip()[:160]})；为避免在半恢复状态上继续操作，未删除或恢复任何文件")
                return
        elif self.baseline_commit:
            code, head, _ = run_git_cmd(["git", "rev-parse", "--verify", "-q", "HEAD"], cwd=root)
            if code != 0 or head.strip() != self.baseline_commit:
                code, _, err = run_git_cmd(["git", "reset", "--soft", "-q", self.baseline_commit], cwd=root)
                if code != 0:
                    problems.append(f"git reset --soft 失败 (rc={code}: {err.strip()[:160]})；未删除或恢复任何文件")
                    return
        else:
            code, _, _ = run_git_cmd(["git", "rev-parse", "--verify", "-q", "HEAD"], cwd=root)
            if code == 0:
                code, _, err = run_git_cmd(["git", "update-ref", "-d", "HEAD"], cwd=root)
                if code != 0:
                    problems.append(f"无法撤销任务在空仓库中创建的提交 (rc={code}: {err.strip()[:120]})；未删除或恢复任何文件")
                    return
            if not self.initial_dirty:
                code, _, err = run_git_cmd(["git", "read-tree", "--empty"], cwd=root)
                if code != 0:
                    problems.append(f"无法清空任务写入的暂存区 (rc={code})")

        current = scan_workspace_tree(root, strict=False)
        self._delete_new_paths(current, problems)
        self._restore_pre_entries(problems)
        self._verify(problems)

    def _untrack_preexisting_paths(self) -> Optional[str]:
        """Unstages pre-existing untracked/ignored files the task added to the index."""
        code, listed, err = run_git_cmd(["git", "ls-files", "-z", "--cached"], cwd=self.root, binary=True)
        if code != 0:
            return f"无法读取当前暂存区 (rc={code})"
        indexed = {os.fsdecode(name) for name in listed.split(b"\0") if name}
        paths = sorted(rel for rel in indexed if rel in self.pre and rel not in self.tracked)
        if not paths:
            return None
        code, _, err = run_git_cmd(["git", "rm", "-r", "-q", "--cached", "--ignore-unmatch",
                                    "--pathspec-from-file=-", "--pathspec-file-nul"],
                                   cwd=self.root, input_data=b"\0".join(os.fsencode(p) for p in paths))
        if code != 0:
            return f"无法把任务前已存在的未跟踪文件移出暂存区 (rc={code}: {os.fsdecode(err or b'').strip()[:160]})"
        return None

    def _recover_relocated(self, current: Dict[str, FsEntry], problems: List[str], notes: List[str]) -> None:
        """Pre-existing inodes found at new paths are moved back or quarantined, never deleted."""
        by_inode = {(e.dev, e.ino): rel for rel, e in self.pre.items() if e.kind == "file"}
        for rel, entry in sorted(current.items()):
            if rel in self.pre or entry.kind != "file":
                continue
            original = by_inode.get((entry.dev, entry.ino))
            if not original:
                continue
            expected = self.pre[original]
            now = _lstat_at(self.root, original)
            if now is not None and (now.dev, now.ino) == (entry.dev, entry.ino):
                continue  # an extra hard link: removing the new name keeps the original
            try:
                if (entry.size, entry.mtime_ns) == (expected.size, expected.mtime_ns) and (now is None or now.kind != "dir"):
                    _rename_at(self.root, rel, original)
                else:
                    # Either a moved-and-modified original or a new file reusing a freed
                    # inode: it is neither deleted nor left in the workspace.
                    destination = self._quarantine(rel)
                    notes.append(f"{rel} 与任务前文件 {original} 共用 inode 但内容已变，已移出工作区隔离保存: {destination}")
            except OSError as exc:
                problems.append(f"无法处理与任务前文件 {original} 共用 inode 的 {rel}: {exc.strerror or exc}")

    def _quarantine(self, rel: str) -> Path:
        folder = self._rejected_artifacts() / "quarantine"
        folder.mkdir(mode=0o700, exist_ok=True)
        destination = folder / rel.replace("/", "__")
        suffix = 1
        while os.path.lexists(destination):
            destination = folder / f"{rel.replace('/', '__')}.{suffix}"
            suffix += 1
        fd, name = _open_parent(self.root, rel)
        try:
            try:
                os.rename(name, str(destination), src_dir_fd=fd)
            except OSError as exc:
                if exc.errno != errno.EXDEV:
                    raise
                _copy_regular_nofollow(os.path.join(self.root, rel), destination)
                os.unlink(name, dir_fd=fd)
        finally:
            os.close(fd)
        return destination

    def _delete_new_paths(self, current: Dict[str, FsEntry], problems: List[str]) -> None:
        new_paths = [rel for rel in current if rel not in self.pre]
        for rel in sorted(new_paths, key=lambda p: (p.count("/"), p), reverse=True):
            entry = current[rel]
            try:
                if entry.kind == "dir":
                    try:
                        _rmdir_at(self.root, rel)
                    except OSError as exc:
                        if exc.errno not in (errno.ENOTEMPTY, errno.EEXIST):
                            raise
                        # A directory the task created may hold a nested .git it also created.
                        leftovers = os.listdir(os.path.join(self.root, rel))
                        if leftovers and all(name == ".git" for name in leftovers) and not os.path.islink(os.path.join(self.root, rel, ".git")):
                            nested = Path(self.root, rel, ".git")
                            if nested.is_dir():
                                _rmtree_collect(nested)
                            else:
                                nested.unlink()
                            _rmdir_at(self.root, rel)
                        else:
                            raise
                else:
                    _unlink_at(self.root, rel)
            except FileNotFoundError:
                continue
            except OSError as exc:
                problems.append(f"无法删除本次任务新建的 {rel}: {exc.strerror or exc}")

    def _restore_pre_entries(self, problems: List[str]) -> None:
        root = self.root
        # Directories first (shallow to deep) so files and links have their parents.
        for rel, entry in sorted(((r, e) for r, e in self.pre.items() if e.kind == "dir"), key=lambda item: item[0].count("/")):
            current = _lstat_at(root, rel)
            try:
                if current is not None and current.kind != "dir":
                    # Pre-existing files were already moved back by inode; whatever
                    # now occupies a pre-existing directory path was created by the task.
                    _unlink_at(root, rel)
                    current = None
                if current is None:
                    fd, name = _open_parent(root, rel, create=True)
                    try:
                        os.mkdir(name, 0o700, dir_fd=fd)
                    finally:
                        os.close(fd)
                    current = _lstat_at(root, rel)
                if current is not None and current.kind == "dir" and current.mode != entry.mode:
                    fd, name = _open_parent(root, rel)
                    try:
                        dir_fd = os.open(name, _NOFOLLOW_DIR_FLAGS, dir_fd=fd)
                        try:
                            os.fchmod(dir_fd, entry.mode)
                        finally:
                            os.close(dir_fd)
                    finally:
                        os.close(fd)
            except OSError as exc:
                problems.append(f"无法恢复目录 {rel}: {exc.strerror or exc}")

        checkout_paths: List[str] = []
        for rel, entry in sorted(self.pre.items()):
            if entry.kind == "dir" or self._git_restores(rel):
                continue
            current = _lstat_at(root, rel)
            if _same_entry(current, entry):
                continue
            if rel in self.tracked and rel not in self.dirty_tracked and self.baseline_commit:
                checkout_paths.append(rel)  # clean tracked file in a dirty-start tree
                continue
            try:
                if current is not None and current.kind == "dir":
                    _rmdir_at(root, rel)
                if entry.kind == "link":
                    if current is not None and current.kind != "dir":
                        _unlink_at(root, rel)
                    fd, name = _open_parent(root, rel, create=True)
                    try:
                        os.symlink(entry.link, name, dir_fd=fd)
                    finally:
                        os.close(fd)
                elif entry.kind == "file" and rel in self.backups:
                    self._restore_file(rel, entry)
            except OSError as exc:
                problems.append(f"无法恢复 {rel}: {exc.strerror or exc}")
        if checkout_paths:
            code, _, err = run_git_cmd(["git", "checkout", self.baseline_commit, "--pathspec-from-file=-", "--pathspec-file-nul"],
                                       cwd=root, input_data=b"\0".join(os.fsencode(p) for p in checkout_paths))
            if code != 0:
                problems.append(f"无法从基线提交恢复被跟踪文件 (rc={code}: {os.fsdecode(err or b'').strip()[:160]})")

    def _restore_file(self, rel: str, entry: FsEntry) -> None:
        backup, _ = self.backups[rel]
        fd, name = _open_parent(self.root, rel, create=True)
        temp_name = f".makewand-restore-{os.getpid()}-{time.monotonic_ns()}"
        created = False
        try:
            src_fd = os.open(backup, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
            with os.fdopen(src_fd, "rb") as src:
                flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
                with os.fdopen(os.open(temp_name, flags, 0o600, dir_fd=fd), "wb") as dst:
                    created = True
                    shutil.copyfileobj(src, dst, 1024 * 1024)
                    dst.flush()
                    os.fchmod(dst.fileno(), entry.mode)
                    os.fsync(dst.fileno())
                    os.utime(dst.fileno(), ns=(entry.atime_ns, entry.mtime_ns))
            os.replace(temp_name, name, src_dir_fd=fd, dst_dir_fd=fd)
            created = False
        finally:
            if created:
                try:
                    os.unlink(temp_name, dir_fd=fd)
                except OSError:
                    pass
            os.close(fd)

    def _verify(self, problems: List[str]) -> None:
        root = self.root
        scan_errors: List[str] = []
        current = scan_workspace_tree(root, strict=False, errors=scan_errors)
        problems.extend(f"核验扫描: {msg}" for msg in scan_errors)
        leftovers = sorted(rel for rel in current if rel not in self.pre)
        if leftovers:
            problems.append(f"本次任务新建的路径未能清除: {_summarize_paths(leftovers)}")
        missing, changed = [], []
        for rel, entry in sorted(self.pre.items()):
            now = current.get(rel)
            if now is None:
                missing.append(rel)
            elif now.kind != entry.kind:
                changed.append(rel)
            elif entry.kind == "file" and not self._git_restores(rel) and not (rel in self.tracked and rel not in self.dirty_tracked):
                if not _same_entry(now, entry):
                    changed.append(rel)
                elif rel in self.backups and _file_sha256_at(root, rel) != self.backups[rel][1]:
                    changed.append(rel)
            elif entry.kind == "link" and now.link != entry.link:
                changed.append(rel)
        for label, paths in (("任务前已存在但现已丢失", missing), ("任务前已存在但未能恢复原内容", changed)):
            if paths:
                detail = [f"{p}（{self.unbacked[p]}）" if p in self.unbacked else p for p in paths]
                problems.append(f"{label}: {_summarize_paths(detail)}")
        if self.baseline_commit:
            code, status_out, err = run_git_cmd(["git", "status", "--porcelain=v1", "-z", "--untracked-files=all"], cwd=root, binary=True)
            if code != 0:
                problems.append(f"无法核验 git 状态 (rc={code})")
            else:
                dirty = sorted({os.fsdecode(t[3:]) for t in status_out.split(b"\0") if len(t) > 3})
                expected = self.dirty_tracked | {rel for rel in self.pre if rel not in self.tracked}
                unexpected = [p for p in dirty if p not in expected]
                if unexpected:
                    problems.append(f"被跟踪文件未能恢复到基线: {_summarize_paths(unexpected)}")
            code, head, _ = run_git_cmd(["git", "rev-parse", "--verify", "-q", "HEAD"], cwd=root)
            if code != 0 or head.strip() != self.baseline_commit:
                problems.append("HEAD 未能回到任务基线提交")

    # -- success path -------------------------------------------------------
    def finalize_success(self) -> None:
        if self.state != "active":
            return
        self.state = "finalizing"
        problems: List[str] = []
        notes: List[str] = []
        warnings_out: List[str] = []
        try:
            scan_errors: List[str] = []
            current = scan_workspace_tree(self.root, strict=False, errors=scan_errors)
            notes.extend(f"交付核查扫描: {msg}" for msg in scan_errors)
            changed = [rel for rel, entry in sorted(self.pre.items())
                       if entry.kind != "dir" and rel not in self.tracked and not _is_regenerable_cache(rel)
                       and not _same_entry(current.get(rel), entry)]
            new_paths = [rel for rel, entry in current.items() if rel not in self.pre and entry.kind != "dir"]
            reviewed: set = set()
            if new_paths or changed:
                # Tracked files and untracked-but-not-ignored files are part of the reviewed diff.
                code, listed, _ = run_git_cmd(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"], cwd=self.root, binary=True)
                if code == 0:
                    reviewed = {os.fsdecode(name) for name in listed.split(b"\0") if name}
                else:
                    notes.append("无法判定文件是否被 .gitignore 忽略，以下文件均按未审查处理")
            changed = [rel for rel in changed if rel not in reviewed]
            new_ignored = sorted(rel for rel in new_paths if rel not in reviewed and not _is_regenerable_cache(rel))
            if changed:
                detail = [f"{p}（未备份: {self.unbacked[p]}）" if p in self.unbacked else p for p in changed]
                warnings_out.append(f"任务前已存在、被 .gitignore 忽略的文件被修改或删除: {_summarize_paths(detail)}")
            if new_ignored:
                warnings_out.append(f"本次任务新建了被 .gitignore 忽略的文件: {_summarize_paths(new_ignored)}")
            if self.created_git:
                self._save_nongit_delivery(problems, notes)
            keep_backups = any(p in self.backups for p in changed)
            if keep_backups:
                notes.append(f"这些文件任务前的原始内容备份在: {self.backup_dir}")
            else:
                self._discard_backups()
        except Exception as exc:  # noqa: BLE001 - reported below
            problems.append(f"交付收尾发生异常: {type(exc).__name__}: {exc}")
        if warnings_out:
            print(c("⚠️ [Makewand Transaction] 以下改动不在审查 diff 中（被 .gitignore 忽略），请人工核查：", COLOR_YELLOW))
            for line in warnings_out:
                print(c(f"   • {line}", COLOR_YELLOW))
        self._finish(not problems, problems, notes, rollback=False)

    def _save_nongit_delivery(self, problems: List[str], notes: List[str]) -> None:
        code, _, err = run_git_cmd(["git", "add", "-A", "--intent-to-add"], cwd=self.root)
        if code == 0:
            code, patch_bytes, err = run_git_cmd(["git", "diff", "--binary", "--full-index", self.baseline_commit], cwd=self.root, binary=True)
        if code != 0:
            problems.append(f"无法导出交付补丁 (rc={code})")
            return
        delivery = create_private_artifact_dir("delivery")
        patch_file = write_private_file(delivery / "makewand_delivery.patch", patch_bytes or b"")
        write_private_file(delivery / "delivery_manifest.json", json.dumps({
            "repo_root": self.root, "mode": "non-git-host", "baseline_commit": self.baseline_commit,
            "main_patch": str(patch_file), "sha256": hashlib.sha256(patch_bytes or b"").hexdigest(),
        }, ensure_ascii=False, indent=2))
        notes.append(f"交付补丁已存入私有产物目录: {patch_file}")

    def _bundle_baseline(self, notes: List[str]) -> bool:
        if not self.baseline_commit:
            return False
        target = create_private_artifact_dir("baseline") / "baseline.bundle"
        code, _, err = run_git_cmd(["git", "bundle", "create", str(target), self.baseline_commit], cwd=self.root)
        if code != 0:
            code, _, err = run_git_cmd(["git", "bundle", "create", str(target), "HEAD"], cwd=self.root)
        if code != 0:
            return False
        try:
            os.chmod(target, 0o600)
        except OSError:
            pass
        notes.append(f"任务基线已导出为 git bundle，可用于人工恢复: git clone {shlex.quote(str(target))} <目录>")
        return True

    def _finish(self, ok: bool, problems: List[str], notes: List[str], rollback: bool) -> None:
        if self.created_git and os.path.lexists(self.git_dir):
            if ok or self._bundle_baseline(notes):
                removal_error = _remove_git_dir(self.git_dir)
                if removal_error:
                    problems.append(removal_error)
                    self.keep_git = True
            else:
                self.keep_git = True
                problems.append(f"为保留恢复依据，暂未删除 makewand 创建的临时仓库 {self.git_dir}（核对后可手动删除）")
        if rollback:
            if not problems:
                self._discard_backups()
            elif self.backup_dir is not None:
                notes.append(f"任务前文件备份保存在: {self.backup_dir}")
        self.succeeded = not problems
        self.state = "rolled_back" if rollback else "committed"
        if rollback and not problems:
            print(c("🛡️ [Makewand Transaction] 已回滚本次任务的全部改动并逐项核验，工作区已恢复基线。", COLOR_YELLOW))
        elif problems:
            title = "回滚未能完全恢复任务前状态" if rollback else "交付收尾存在问题"
            print(c(f"⚠️ [Makewand Transaction] {title}，请人工检查：", COLOR_RED))
            for line in problems[:30]:
                print(c(f"   • {line}", COLOR_RED))
        for line in notes:
            print(c(f"   {line}", COLOR_YELLOW))

    def close(self, succeeded: bool, reason: str = "") -> None:
        """Safety net for every exit path, including exceptions and interrupts."""
        if self.state == "active":
            if succeeded:
                self.finalize_success()
            else:
                self.rollback(reason or "流水线异常中断")
        if self.created_git and not self.keep_git and os.path.lexists(self.git_dir):
            removal_error = _remove_git_dir(self.git_dir)
            if removal_error:
                print(c(f"⚠ [Makewand Transaction] {removal_error}", COLOR_RED))


class PipelineWorkspaceGuard:
    """Resources for one run_pipeline call: repository lock + host transaction."""

    def __init__(self):
        self.lock: Optional[WorkspaceLock] = None
        self.txn: Optional[HostWorkspaceTransaction] = None

    def acquire_workspace_lock(self, cwd: Union[str, Path]) -> Optional[str]:
        if self.lock is not None:
            return None
        try:
            self.lock = WorkspaceLock(cwd).acquire()
        except WorkspaceLockError as exc:
            return str(exc)
        except OSError as exc:
            return f"无法获取工作区锁: {exc}"
        return None

    def close(self, result: Any, error: Optional[BaseException]) -> None:
        try:
            if self.txn is not None:
                reason = f"流水线异常中断: {type(error).__name__}: {error}" if error is not None else "流水线未通过"
                self.txn.close(succeeded=(error is None and result is True), reason=reason)
        except Exception as exc:  # noqa: BLE001 - must not mask the pipeline outcome
            print(c(f"⚠️ [Makewand Transaction] 收尾时发生异常，请人工检查工作区: {type(exc).__name__}: {exc}", COLOR_RED))
        finally:
            if self.lock is not None:
                try:
                    self.lock.release()
                except OSError as exc:
                    print(c(f"⚠ [Makewand Workspace Lock] 释放工作区锁失败: {exc}", COLOR_YELLOW))
                self.lock = None
