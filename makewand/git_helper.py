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
import fnmatch
import itertools
from contextvars import ContextVar
from datetime import datetime
from pathlib import Path
from typing import Optional, Dict, Any, Callable, List, NamedTuple, Tuple, Union
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

# What makewand reads from ``git diff`` - the review diff, the --raw/--name-status
# listings parsed from it and every exported patch - never depends on the user's
# diff configuration: whole-repository, root-relative paths (diff.relative), no
# colour codes (color.ui=always), a/ b/ prefixes (diff.noprefix, diff.mnemonicPrefix,
# diff.srcPrefix), submodule commits as "Subproject commit" lines that git apply
# understands (diff.submodule=log) and git's own diff machinery (diff.external,
# textconv drivers). An option the caller passes itself comes later and wins.
DIFF_FORMAT_FLAGS = ("--no-ext-diff", "--no-textconv", "--no-color", "--no-relative",
                     "--src-prefix=a/", "--dst-prefix=b/", "--submodule=short")
# An exported patch applies with plain ``git apply`` whatever diff.context says.
PATCH_EXPORT_FLAGS = ("--binary", "--full-index", "--unified=3")
# The review diff (get_git_diff_status) shows three lines of context around every
# change whatever diff.context says, like an exported patch: diff.context=0 would
# leave reviewers with bare hunks. Not in DIFF_FORMAT_FLAGS, because --unified
# implies a patch, which a --raw or --name-status listing must not grow.
REVIEW_DIFF_FLAGS = ("--unified=3",)
# Applying the user's own changes reproduces them byte for byte: apply.whitespace=fix
# would rewrite them and apply.whitespace=error would refuse them.
PATCH_APPLY_FLAGS = ("--binary", "--whitespace=nowarn")
# Attributes read from the empty tree: no .gitattributes in the work tree or index
# selects a filter, diff or merge driver (info/attributes is shielded separately).
_EMPTY_TREE_ATTR_SOURCE = "--attr-source=4b825dc642cb6eb9a060e54bf8d69288fbee4904"
# git apply modes that work through the index, the only ones that accept an attribute source.
_APPLY_INDEX_OPTIONS = frozenset({"--index", "--cached", "--3way", "-3"})
# Settings that leave a filter driver no command to run and make git not insist on it.
_DISABLED_FILTER_DRIVER = (("clean", ""), ("smudge", ""), ("process", ""), ("required", "false"))
# Global git options whose value is a separate argument (``git -C dir diff``).
_GIT_GLOBAL_OPTIONS_WITH_VALUE = frozenset({"-C", "-c", "--git-dir", "--work-tree", "--namespace"})


def _git_subcommand_index(cmd: List[str]) -> Optional[int]:
    """Index of the subcommand in a ["git", <global options>..., <subcommand>, ...] list."""
    index = 1
    while index < len(cmd):
        argument = str(cmd[index])
        if argument in _GIT_GLOBAL_OPTIONS_WITH_VALUE:
            index += 2
        elif argument.startswith("-"):
            index += 1
        else:
            return index
    return None


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
    return _is_reparse_api_path(_fs_path(path)) if os.name == "nt" else False


def _is_reparse_api_path(path):
    """Inspect an API path already converted by _fs_path exactly once."""
    if os.name != "nt":
        return False
    try:
        return bool(getattr(path.lstat(), "st_file_attributes", 0) & 0x400)
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
            cwd_resolved = Path(cwd).resolve()
            bin_path = Path(_RESOLVED_GIT_BINARY).resolve()
            if os.name == "nt":
                cwd_s = os.path.normcase(str(cwd_resolved))
                bin_s = os.path.normcase(str(bin_path))
                if bin_s == cwd_s or bin_s.startswith(cwd_s + os.sep):
                    raise PermissionError(f"Refusing to execute git binary found inside workspace cwd: {_RESOLVED_GIT_BINARY}")
            else:
                try:
                    if bin_path.is_relative_to(cwd_resolved):
                        raise PermissionError(f"Refusing to execute git binary found inside workspace cwd: {_RESOLVED_GIT_BINARY}")
                except (ValueError, RuntimeError):
                    pass
        return _RESOLVED_GIT_BINARY

    path_entries = os.environ.get("PATH", "").split(os.pathsep)
    clean_entries = []
    cwd_resolved = Path(cwd).resolve() if cwd else None
    for entry in path_entries:
        if not entry or not os.path.isabs(entry):
            continue
        try:
            entry_p = Path(entry).resolve()
            if cwd_resolved:
                if os.name == "nt":
                    cwd_s = os.path.normcase(str(cwd_resolved))
                    entry_s = os.path.normcase(str(entry_p))
                    if entry_s == cwd_s or entry_s.startswith(cwd_s + os.sep):
                        continue
                else:
                    if entry_p.is_relative_to(cwd_resolved):
                        continue
        except (ValueError, RuntimeError):
            continue
        clean_entries.append(entry)

    clean_path = os.pathsep.join(clean_entries)
    found = shutil.which("git", path=clean_path)
    if not found:
        if cwd_resolved:
            raw_found = shutil.which("git")
            if raw_found:
                raw_path = Path(raw_found).resolve()
                if os.name == "nt":
                    cwd_s = os.path.normcase(str(cwd_resolved))
                    raw_s = os.path.normcase(str(raw_path))
                    if raw_s == cwd_s or raw_s.startswith(cwd_s + os.sep):
                        raise PermissionError(f"Refusing to execute git binary found inside workspace cwd: {raw_found}")
                else:
                    try:
                        if raw_path.is_relative_to(cwd_resolved):
                            raise PermissionError(f"Refusing to execute git binary found inside workspace cwd: {raw_found}")
                    except (ValueError, RuntimeError):
                        pass
        raise FileNotFoundError("Safe git binary not found in PATH")

    found_path = Path(found).resolve()
    if cwd_resolved:
        if os.name == "nt":
            cwd_s = os.path.normcase(str(cwd_resolved))
            found_s = os.path.normcase(str(found_path))
            if found_s == cwd_s or found_s.startswith(cwd_s + os.sep):
                raise PermissionError(f"Refusing to execute git binary found inside workspace cwd: {found}")
        else:
            try:
                if found_path.is_relative_to(cwd_resolved):
                    raise PermissionError(f"Refusing to execute git binary found inside workspace cwd: {found}")
            except (ValueError, RuntimeError):
                pass

    _RESOLVED_GIT_BINARY = str(found_path)
    return _RESOLVED_GIT_BINARY


# git refuses a .git file larger than this (read_gitfile_gently); commondir holds a path.
_GIT_POINTER_MAX_BYTES = 1 << 20
_HEX_DIGITS = frozenset(b"0123456789abcdefABCDEF")


def _read_git_pointer(path: Path) -> bytes:
    """A .git file or commondir, read as git reads it; OSError where git's read fails too."""
    descriptor = os.open(_fs_path(path), os.O_RDONLY | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_BINARY", 0)
                         | getattr(os, "O_CLOEXEC", 0))
    with os.fdopen(descriptor, "rb") as handle:
        # git cannot read a directory and would wait forever on a FIFO.
        if not stat.S_ISREG(os.fstat(handle.fileno()).st_mode):
            raise OSError(errno.EINVAL, "not a regular file", str(path))
        data = handle.read(_GIT_POINTER_MAX_BYTES + 1)
    if len(data) > _GIT_POINTER_MAX_BYTES:
        raise OSError(errno.EFBIG, "file too large", str(path))
    return data


def _gitfile_target(dot_git: Path) -> Optional[Path]:
    """The git directory a ``.git`` file names, parsed exactly as git parses it; None when git rejects the file."""
    data = _read_git_pointer(dot_git)
    if not data.startswith(b"gitdir: "):
        return None
    data = data.rstrip(b"\r\n")
    if len(data) < 9:
        return None
    target = os.fsdecode(data[8:].split(b"\0", 1)[0])
    return Path(os.path.realpath(os.path.join(dot_git.parent, target)))


def _git_common_dir(git_dir: Path) -> Path:
    """The directory git reads config and info/attributes from for ``git_dir``.

    git honours a ``commondir`` file in any git directory, not only in a linked
    worktree's: its content names the common directory, relative to ``git_dir``
    unless absolute, with trailing line ends dropped and nothing after a NUL. A
    commondir git cannot read makes git die, and makes this raise.
    """
    pointer = git_dir / "commondir"
    if not os.path.lexists(_fs_path(pointer)):
        return git_dir
    data = _read_git_pointer(pointer)
    if not data:
        raise OSError(errno.EINVAL, "empty commondir", str(pointer))
    target = os.fsdecode(data.rstrip(b"\r\n").split(b"\0", 1)[0])
    return Path(os.path.realpath(os.path.join(git_dir, target)))


def _valid_git_head(head: Path) -> bool:
    """git's validate_headref: a symbolic ref into refs/, as file content or symlink, or an object id."""
    path = _fs_path(head)
    try:
        mode = os.lstat(path).st_mode
        if stat.S_ISLNK(mode):
            return os.fsencode(os.readlink(path)).startswith(b"refs/")
        if not stat.S_ISREG(mode):
            return False
        with open(path, "rb") as handle:
            data = handle.read(255)
    except OSError:
        return False
    if data.startswith(b"ref:") and data[4:].lstrip(b" \t\n\r").startswith(b"refs/"):
        return True
    return len(data) >= 40 and all(byte in _HEX_DIGITS for byte in data[:40])


def _git_metadata_dirs(cwd: Optional[Union[str, Path]]) -> List[Path]:
    """Every git directory and common directory git may use for a command run in ``cwd``.

    Mirrors git's discovery by reading files, never by running git with the
    repository's configuration: at ``cwd`` and then each parent, a ``.git`` file
    names the repository (git uses it or refuses to run), a valid ``.git``
    directory is the repository, and otherwise the directory itself may be an
    implicit bare repository; each with the common directory its ``commondir``
    names. A candidate git would pass over may be listed, one git would use never
    is missed. Raises OSError where git itself cannot read the layout (a .git file
    or commondir it cannot read): the caller refuses to run git.
    """
    start = Path(os.path.realpath(cwd if cwd else os.getcwd()))
    found: List[Path] = []
    for level in (start, *start.parents):
        dot_git = level / ".git"
        try:
            mode = os.stat(_fs_path(dot_git)).st_mode
        except OSError:
            mode = 0  # git treats every stat failure as no .git at this level
        if stat.S_ISREG(mode):
            target = _gitfile_target(dot_git)
            if target is not None:
                found += [target, _git_common_dir(target)]
            return found
        candidates = [level]
        if stat.S_ISDIR(mode):
            found.append(dot_git)
            candidates.insert(0, dot_git)
        for git_dir in candidates:
            if _valid_git_head(git_dir / "HEAD"):
                common_dir = _git_common_dir(git_dir)
                found += [git_dir, common_dir]
                if all(os.access(_fs_path(common_dir / name), os.X_OK) for name in ("objects", "refs")):
                    return found
    return found


def _get_git_info_attributes_paths(cwd: Optional[Union[str, Path]]) -> List[Path]:
    """The info/attributes files to shield for a git command run in ``cwd``.

    --attr-source does not override them. Raises OSError when the repository
    layout cannot be read; run_git_cmd then refuses the command.
    """
    paths: List[Path] = []
    seen_dirs = set()
    for gdir in _git_metadata_dirs(cwd):
        try:
            resolved_dir = gdir.resolve()
        except (OSError, RuntimeError):
            resolved_dir = gdir
        if resolved_dir in seen_dirs:
            continue
        seen_dirs.add(resolved_dir)
        info_dir = _fs_path(resolved_dir / "info")
        ia = info_dir / "attributes"
        gate_file = info_dir / "attributes.mw_gate"
        try:
            needs_shield = (ia.exists() or gate_file.exists()
                            or (info_dir.is_dir() and any(info_dir.glob("attributes.*shield_*"))))
        except OSError:
            needs_shield = False  # a file makewand cannot look up, git cannot read either
        if needs_shield:
            paths.append(ia)
    return paths


def _disabled_filter_drivers(cwd, timeout) -> Dict[str, str]:
    """Command-scope configuration disabling every filter driver git's configuration defines for ``cwd``.

    Given as GIT_CONFIG_COUNT/KEY/VALUE because ``-c`` cannot carry a driver name
    containing ``=``; an empty name is a driver too (``filter=``). Raises OSError
    when git cannot list the configuration; the caller refuses the command.
    """
    code, listing, error = run_git_cmd(["git", "config", "--null", "--name-only", "--get-regexp", r"^filter\."],
                                       cwd=cwd, binary=True, timeout=timeout)
    if code == 1:  # no filter setting at all
        return {}
    if code != 0:
        raise OSError(f"Cannot list Git filter drivers: {os.fsdecode(error).strip()}")
    names: Dict[str, None] = {}
    for key in listing.split(b"\0"):
        name, dot, _ = key[len(b"filter."):].rpartition(b".")
        if key.startswith(b"filter.") and dot:
            names[os.fsdecode(name)] = None
    settings = [(f"filter.{name}.{variable}", value) for name in names for variable, value in _DISABLED_FILTER_DRIVER]
    config = {"GIT_CONFIG_COUNT": str(len(settings))} if settings else {}
    for index, (key, value) in enumerate(settings):
        config[f"GIT_CONFIG_KEY_{index}"] = key
        config[f"GIT_CONFIG_VALUE_{index}"] = value
    return config


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
        safe_git = resolve_safe_git_binary(cwd=process_cwd)
        command_config: Dict[str, str] = {}
        if isinstance(cmd, list) and len(cmd) > 0 and cmd[0] == "git" and safe:
            sub_index = _git_subcommand_index(cmd)
            subcommand = cmd[sub_index] if sub_index is not None else None
            extra_global = ["--no-pager"]
            if subcommand == "apply":
                # git (2.55) segfaults on worktree-mode apply, --check included, under
                # any attribute source: it reads the preimage through convert_to_git
                # without an index. Outside a repository --attr-source makes every
                # apply die. Only the index modes take one; every apply runs with each
                # configured filter driver disabled, whichever attribute selects it.
                if _APPLY_INDEX_OPTIONS.intersection(cmd[sub_index + 1:]):
                    extra_global.append(_EMPTY_TREE_ATTR_SOURCE)
                command_config = _disabled_filter_drivers(cwd, timeout)
            elif cmd[1:2] != ["clone"]:
                extra_global.append(_EMPTY_TREE_ATTR_SOURCE)
            exec_cmd = [safe_git] + extra_global + SAFE_GIT_SECURITY_FLAGS + cmd[1:]
            if subcommand == "diff":
                # Also after global options (``git --no-replace-objects diff``).
                position = len(exec_cmd) - len(cmd) + sub_index + 1
                exec_cmd[position:position] = [flag for flag in DIFF_FORMAT_FLAGS if flag not in cmd]
            use_shell = False
        elif not use_shell:
            exec_cmd = cmd
            if isinstance(exec_cmd, list) and len(exec_cmd) > 0 and exec_cmd[0] == "git":
                exec_cmd = [safe_git] + exec_cmd[1:]
            use_shell = False

        if (os.name == "nt" and cwd is not None and isinstance(exec_cmd, list)
                and exec_cmd and (exec_cmd[0] in ("git", safe_git) or exec_cmd[0].lower().endswith(("git", "git.exe")))):
            # Avoid CreateProcess's extended-CWD limit and Git's fixed getcwd
            # buffers. Check before shielding attributes or launching Git.
            process_cwd = windows_git_directory(cwd)
        git_env = _sanitize_git_env() if safe else os.environ.copy()
        if safe:
            git_env.update(command_config)
            if _diff_index.get() is not None:
                git_env["GIT_INDEX_FILE"] = _diff_index.get()

            # S01: Temporarily shield info/attributes in every git and common directory git may use
            # (any layout, commondir included). Use deterministic sorting and attributes.lock file
            # locking to avoid multi-session collisions.
            try:
                ia_targets = sorted(_get_git_info_attributes_paths(cwd), key=lambda p: str(p.resolve()))
            except (OSError, RuntimeError, ValueError) as exc:
                raise OSError(f"Cannot shield Git attributes: {exc}") from exc
            for ia_target in ia_targets:
                info_dir = ia_target.parent
                try:
                    info_dir.mkdir(parents=True, exist_ok=True)
                except Exception:
                    pass
                lock_file = info_dir / "attributes.mw_gate"
                lock_handle = None
                try:
                    # Never created or opened through a link (an init.templateDir can supply one).
                    gate_flags = (os.O_RDWR | os.O_CREAT | os.O_APPEND | getattr(os, "O_NOFOLLOW", 0)
                                  | getattr(os, "O_CLOEXEC", 0))
                    lock_handle = os.fdopen(os.open(lock_file, gate_flags, 0o666), "a+", encoding="utf-8")
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
    Stops before escaping past $HOME or filesystem root boundary, and refuses
    to return $HOME, /, or /tmp as a repository root.
    """
    if not path:
        return None
    try:
        curr = Path(path).resolve()
        refused = set(_refused_init_roots())
        home = None
        try:
            home = Path.home().resolve()
            refused.add(home)
        except Exception:
            pass

        while curr != curr.parent:
            if curr in refused:
                break
            if _is_git_marker(curr / ".git"):
                return str(curr)
            if home and curr == home:
                break
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
    print(c(_printable(f"[Makewand Git] {resolved} 不是 Git 仓库，建立临时 Git 基线（任务结束后自动移除）..."), COLOR_YELLOW), file=sys.stderr)
    steps = [
        (["git", "init", "-q"], "git init"),
        (["git", "config", "user.name", "Makewand"], "git config user.name"),
        (["git", "config", "user.email", "makewand@local"], "git config user.email"),
    ]
    for cmd, label in steps:
        code, _, err = run_git_cmd(cmd, cwd=str(resolved))
        if code == 0 and cmd[1] == "init":
            # A config linked from init.templateDir must not receive makewand's identity.
            try:
                _own_repository_file(git_dir, "config")
                _own_repository_directory(git_dir, "info")
            except OSError as exc:
                code, err = 1, f"无法确保仓库配置为本仓库的普通文件: {exc}"
        if code != 0:
            cleanup_err = _remove_git_dir(git_dir)
            return False, f"{label} 失败 (rc={code}): {(err or '').strip()[:200]}" + (f"；{cleanup_err}" if cleanup_err else "")
    if ephemeral:
        try:
            write_repository_file(git_dir, EPHEMERAL_GIT_MARKER,
                                  json.dumps({"pid": os.getpid(), "created": datetime.now().isoformat()}).encode("utf-8"))
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
        print(c(_printable(f"❌ [Makewand Git] {error}"), COLOR_RED), file=sys.stderr)
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
    Every index construction and diff error is returned, including in submodules;
    git's messages may quote file names with undecodable bytes, so every error is
    returned printable (_printable).
    """
    empty = b"" if binary else ""
    code, inside, error = run_git_cmd(["git", "rev-parse", "--is-inside-work-tree"], cwd=cwd)
    if code != 0 or inside.strip() != "true":
        return empty, _printable(f"Not inside a valid git working tree ({error.strip()})")
    head, error = _resolve_diff_head(cwd)
    if error:
        return empty, _printable(error)
    ref = head
    if base_rev:
        code, resolved, error = run_git_cmd(
            ["git", "rev-parse", "--verify", "--end-of-options", f"{base_rev}^{{commit}}"], cwd=cwd)
        if code != 0 or not resolved.strip():
            return empty, _printable(f"Cannot resolve Git diff baseline: {error.strip()}")
        ref = resolved.strip()
    try:
        with tempfile.TemporaryDirectory(prefix="makewand-diff-index-") as directory:
            token = _diff_index.set(str(Path(directory) / "index"))
            try:
                seed = ["git", "read-tree", head] if head else ["git", "read-tree", "--empty"]
                code, _, error = run_git_cmd(seed, cwd=cwd)
                if code != 0:
                    return empty, _printable(f"git read-tree failed with exit code {code}: {error.strip()}")
                add = ["git", "add", "-A"]
                if ref:
                    add.append("--intent-to-add")
                code, _, error = run_git_cmd(add, cwd=cwd)
                if code != 0:
                    return empty, _printable(f"git add failed with exit code {code}: {error.strip()}")
                command = ["git", "diff", *arguments]
                if ref:
                    command.append(ref)
                else:
                    command.extend(["--cached", "4b825dc642cb6eb9a060e54bf8d69288fbee4904"])
                code, output, error = run_git_cmd(command, cwd=cwd, binary=binary)
                if code != 0:
                    if isinstance(error, bytes):
                        error = os.fsdecode(error)
                    return empty, _printable(f"git diff failed with exit code {code}: {error.strip()}")
                return output, None
            finally:
                _diff_index.reset(token)
    except OSError as exc:
        return empty, _printable(f"Cannot construct private Git diff index: {exc}")


def get_git_diff_status(cwd: str, base_rev: Optional[str] = None, sub_baselines: Optional[Dict[str, str]] = None) -> Tuple[str, Optional[str]]:
    """
    Extracts git diff for the workspace, including newly added, modified, and deleted files.
    Returns (diff: str, error: Optional[str]).
    Enforces fail-closed security: if git command errors (e.g. invalid repo, exit 128),
    returns the explicit error instead of disguising as an empty diff. The error is
    printable (_printable) even when git quotes a file name with undecodable bytes.
    Every hunk carries three lines of context (REVIEW_DIFF_FLAGS): this is the review
    diff, and a verdict is bound to the digest of exactly this text.
    """
    if not cwd:
        cwd = os.getcwd()

    diff_out, error = _read_workspace_diff(cwd, base_rev, arguments=REVIEW_DIFF_FLAGS)
    if error:
        return "", error

    main_diff = diff_out.strip() if diff_out else ""

    # Recursively extract actual submodule diffs against sub_baselines. Like the main
    # diff (--no-relative) they cover the whole repository from any of its directories:
    # submodules come from the top level's .gitmodules, with root-relative paths.
    code, top, _ = run_git_cmd(["git", "rev-parse", "--show-toplevel"], cwd=cwd)
    repo_root = cwd
    if code == 0 and top.strip() and (os.path.normcase(os.path.realpath(top.strip()))
                                      != os.path.normcase(os.path.realpath(cwd))):
        repo_root = top.strip()  # launched from a subdirectory
    sub_diffs = []
    try:
        sub_paths = get_submodule_paths(repo_root)
    except OSError as exc:
        return "", _printable(str(exc))
    for sub_rel in sub_paths:
        sub_p = Path(repo_root) / sub_rel
        if sub_p.exists() and (sub_p / ".git").exists():
            sub_base = sub_baselines.get(sub_rel) if sub_baselines else None
            if not sub_base:
                rev_code, gitlink_out, _ = run_git_cmd(["git", "rev-parse", f"HEAD:{sub_rel}"], cwd=repo_root)
                if rev_code == 0 and gitlink_out and gitlink_out.strip():
                    sub_base = gitlink_out.strip()
            sub_ref = sub_base if sub_base else "HEAD"
            s_diff, s_err = _read_workspace_diff(str(sub_p), sub_ref, arguments=("--binary", *REVIEW_DIFF_FLAGS))
            if not s_err and s_diff and s_diff.strip():
                sub_diffs.append(f"\n--- [Submodule: {sub_rel}] (diff against {sub_ref}) ---\n{s_diff.strip()}")
            elif s_err:
                return "", _printable(f"Submodule {sub_rel} diff extraction failed: {s_err}")

    full_diff = main_diff
    if sub_diffs:
        full_diff = (full_diff + "\n" if full_diff else "") + "\n".join(sub_diffs)

    return full_diff.strip(), None

def get_git_diff(cwd: str, base_rev: Optional[str] = None, sub_baselines: Optional[Dict[str, str]] = None) -> str:
    diff_text, error = get_git_diff_status(cwd, base_rev=base_rev, sub_baselines=sub_baselines)
    if error:
        raise OSError(error)
    return diff_text


def git_status_paths(cwd: Union[str, Path]) -> Optional[List[str]]:
    """The paths `git status` reports in cwd (modified, staged, deleted, untracked - each
    untracked file on its own), in git's order; None when git status cannot run there
    (outside a work tree). The one parser of the user's dirty files: NUL-separated entries
    are never quoted (core.quotePath) and, without rename pairs (--no-renames: a rename is
    its deletion and its addition), each one is "XY <path>"; names are taken byte-exact
    (os.fsdecode), spaces and undecodable bytes included."""
    code, out, _ = run_git_cmd(["git", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames"],
                               cwd=str(cwd), binary=True)
    if code != 0 or out is None:
        return None
    return list(dict.fromkeys(os.fsdecode(token[3:]) for token in out.split(b"\0") if len(token) > 3))


def get_dirty_files(cwd: Optional[Union[str, Path]] = None) -> List[str]:
    """
    Returns relative paths of all dirty files (modified, untracked, staged, renamed)
    in the git repository. Returns candidate files if not inside a git repository.
    """
    if not cwd:
        cwd = os.getcwd()
    cwd_str = str(cwd)

    # 1. git status (git_status_paths).
    dirty = git_status_paths(cwd_str)
    if dirty is not None:
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
# The directory names below which clone_isolated_worktree copies no untracked file:
# CLONE_EXCLUDED_DIRS without the cache names, whose deliverable files it keeps (a
# cache name is no evidence that the contents are disposable). The one definition:
# candidate.build_manifest(host=True) leaves out exactly the files no copy receives.
CLONE_UNCOPIED_DIRS = frozenset(CLONE_EXCLUDED_DIRS - {"__pycache__", ".pytest_cache", ".mypy_cache",
                                                       ".ruff_cache", ".cache"})


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
        raise OSError(_printable(f"无法按 .gitignore 规则枚举工作区文件 (rc={code}): {detail[:200]}"))
    return sorted({os.fsdecode(name) for name in out.split(b"\0") if name})


def list_workspace_ignored_paths(src_dir: Union[str, Path]) -> Optional[set]:
    """The untracked entries below src_dir that the ignore rules exclude - exactly what
    list_workspace_copy_paths, and so every isolated copy, never receives: a directory
    everything below which is ignored as "dir/", otherwise each ignored file (paths
    relative to src_dir). The rules are those list_workspace_copy_paths honours
    (.gitignore, $GIT_DIR/info/exclude and the global excludes file inside a work tree,
    the .gitignore files elsewhere). None when Git cannot evaluate them."""
    src = Path(src_dir).resolve()
    listing = ["ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory"]
    if find_git_root(src):
        code, out, _ = run_git_cmd(["git", *listing], cwd=str(src), binary=True)
    else:
        probe = tempfile.mkdtemp(prefix="makewand-ignore-probe-")
        try:
            code, _, _ = run_git_cmd(["git", "init", "-q", probe])
            if code == 0:
                code, out, _ = run_git_cmd(["git", f"--git-dir={os.path.join(probe, '.git')}", f"--work-tree={src}",
                                            *listing], cwd=str(src), binary=True)
        finally:
            shutil.rmtree(probe, ignore_errors=True)
    if code != 0:
        return None
    return {os.fsdecode(name) for name in out.split(b"\0") if name}


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
    excluded = CLONE_UNCOPIED_DIRS
    def copy_inputs(source_root: Path, destination_root: Path):
        tracked = set()
        if find_git_root(source_root):
            code, names, detail = run_git_cmd(["git", "ls-files", "--cached", "-z", "--", "."], cwd=str(source_root), binary=True)
            if code != 0:
                raise OSError(_printable(f"无法核对隔离副本的已跟踪文件: {os.fsdecode(detail or b'').strip()[:200]}"))
            tracked = {os.fsdecode(name) for name in names.split(b"\0") if name}
        for rel in list_workspace_copy_paths(source_root):
            rel = rel.rstrip("/")
            parts = Path(rel).parts
            if not parts or ".git" in parts or (rel not in tracked and any(part in excluded for part in parts)):
                continue
            if Path(rel).is_absolute() or ".." in parts:
                raise OSError(_printable(f"invalid workspace path from git: {rel}"))
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
                            raise OSError(_printable(f"workspace path crosses directory symlink: {item}"))
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
                copy_errors.append(_printable(f"{src_item.relative_to(resolved)}: {exc}"))
    if resolved not in _refused_init_roots():
        copy_inputs(resolved, target_dir)
    if copy_errors:
        raise OSError(_printable(f"隔离副本复制不完整: {'; '.join(copy_errors[:3])}"))

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
                raise OSError(_printable(f"隔离副本基线建立失败 ({' '.join(cmd[1:3])}, rc={code}): {(err or '').strip()[:200]}"))
            if cmd[1] == "init":
                # A config linked from init.templateDir must not receive the copy's settings.
                try:
                    _own_repository_file(Path(target_dir) / ".git", "config")
                    _own_repository_directory(Path(target_dir) / ".git", "info")
                except OSError as exc:
                    raise OSError(_printable(f"隔离副本基线建立失败 (无法确保仓库配置为本仓库的普通文件): {exc}")) from exc
        # Only force-add the exact safely enumerated files. A source file that was
        # tracked before a later .gitignore rule must remain tracked in the clone.
        # Literal pathspecs prevent a copied filename containing '*' from admitting
        # other ignored files. Sanitized, removed links are not staged.
        copied = sorted(path for path in copied_paths if os.path.lexists(filesystem_path(target_dir / path)))
        for offset in range(0, len(copied), 256):
            code, _, err = run_git_cmd(["git", "--literal-pathspecs", "add", "-f", "--", *copied[offset:offset + 256]], cwd=str(target_dir))
            if code != 0:
                raise OSError(_printable(f"隔离副本安全基线暂存失败: {(err or '').strip()[:200]}"))
        code, _, err = run_git_cmd(["git", "commit", "-q", "--no-verify", "-m", "Makewand isolated baseline", "--allow-empty"], cwd=str(target_dir))
        if code != 0:
            raise OSError(_printable(f"隔离副本基线提交失败: {(err or '').strip()[:200]}"))

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
            return False, _printable(f"生产保护警报: {prod_msg}")

        # 2. Multi-Session Overlap Detection (bidirectional)
        active = get_active_interactive_working_trees()
        for active_cwd, info in active.items():
            active_path = Path(active_cwd).resolve()
            if target_path == active_path or active_path in target_path.parents or target_path in active_path.parents:
                src_desc = f"tmux 会话 [{info['session_name']}]" if info.get("source") == "tmux" else f"外部独立终端 [{info.get('tty')} · PID {info.get('pid')} · {info.get('ai_type')}]"
                return False, _printable(f"工作区 {target_dir} 与活跃交互会话 ({src_desc}) 存在工作树重叠冲突")

        # 3. Dirty Working Tree Guard: if working tree has any uncommitted changes,
        # enforce shadow worktree to protect user WIP from accidental rollback.
        # A non-git directory is handled by the host transaction; a git repository
        # whose status cannot be read is never assumed clean.
        if find_git_root(target_path):
            # Untracked files count whatever status.showUntrackedFiles says.
            code, status_out, status_err = run_git_cmd(["git", "status", "--porcelain", "--untracked-files=normal"],
                                                       cwd=str(target_path))
            if code != 0:
                return False, _printable(f"无法读取工作区 {target_dir} 的 git 状态 (rc={code}: {(status_err or '').strip()[:120]})，按不安全处理")
            if status_out and status_out.strip():
                return False, _printable(f"工作区 {target_dir} 存在未提交的代码修改 (Dirty Working Tree)")
    except Exception as exc:
        return False, _printable(f"工作区隔离检查异常 ({exc})，按不安全处理")
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
                    # Guard against modifying files outside worktree_dir (e.g., if p.parent traverses a symlink into readonly repo_root)
                    try:
                        p_parent_resolved = p.parent.resolve()
                        if not p_parent_resolved.is_relative_to(wt_resolved):
                            continue
                    except Exception:
                        continue

                    resolved = p.resolve()
                    # If it resolves inside repo_root and NOT inside worktree_dir, remap to worktree_dir
                    if resolved.is_relative_to(repo_resolved) and not resolved.is_relative_to(wt_resolved):
                        rel = resolved.relative_to(repo_resolved)
                        new_target = wt_resolved / rel
                        rel_target = os.path.relpath(new_target, p.parent)
                        tmp_link = p.parent / f".tmp_link_{os.getpid()}_{name}"
                        try:
                            os.symlink(rel_target, _fs_path(tmp_link))
                            os.replace(_fs_path(tmp_link), _fs_path(p))
                        except OSError as e:
                            if tmp_link.exists() or tmp_link.is_symlink():
                                try:
                                    tmp_link.unlink()
                                except OSError:
                                    pass
                            if getattr(e, "errno", None) == errno.EROFS:
                                continue
                            raise
                    elif resolved.is_relative_to(wt_resolved):
                        raw_target = os.readlink(_fs_path(p))
                        if Path(raw_target).is_absolute():
                            rel_target = os.path.relpath(resolved, p.parent)
                            tmp_link = p.parent / f".tmp_link_{os.getpid()}_{name}"
                            try:
                                os.symlink(rel_target, _fs_path(tmp_link))
                                os.replace(_fs_path(tmp_link), _fs_path(p))
                            except OSError as e:
                                if tmp_link.exists() or tmp_link.is_symlink():
                                    try:
                                        tmp_link.unlink()
                                    except OSError:
                                        pass
                                if getattr(e, "errno", None) == errno.EROFS:
                                    continue
                                raise
                except Exception:
                    pass

def create_ephemeral_shadow_worktree(base_dir: str, prefix: str = "shadow", repo_trust: str = "trusted"):
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
        print(c(_printable(f"❌ [Makewand Guard] 无法创建私有影子工作树目录 ({exc})。"), COLOR_RED), file=sys.stderr)
        return None, None, None
    branch_name = f"makewand/{prefix}_{timestamp}_{rand_id}"

    def discard_worktree_dir():
        errors = _rmtree_collect(worktree_dir) if worktree_dir.exists() else []
        if errors:
            print(c(_printable(f"⚠ [Makewand Guard] 影子工作树残留清理失败: {errors[0]}"), COLOR_YELLOW), file=sys.stderr)

    # Check if base_dir is inside a git repository
    code, is_inside, is_inside_err = run_git_cmd(["git", "rev-parse", "--is-inside-work-tree"], cwd=str(base_path))
    if code != 0 and find_git_root(base_path):
        print(c(_printable(f"❌ [Makewand Guard] 无法读取仓库状态 (rc={code}: {(is_inside_err or '').strip()[:120]})，拒绝以非 Git 副本代替影子工作树。"), COLOR_RED), file=sys.stderr)
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

        # Create lightweight independent clone with dissociated objects (self-contained, decoupled git metadata)
        cmd = ["git", "clone", "--shared", "--dissociate", str(repo_root), str(worktree_dir)]
        if repo_trust != "untrusted":
            cmd = ["git", "-c", "protocol.file.allow=always", "clone", "--shared", "--dissociate", str(repo_root), str(worktree_dir)]
        wt_code, wt_out, wt_err = run_git_cmd(cmd)
        if wt_code != 0:
            # A failed or timed-out clone leaves a partial directory; never fall
            # back to copying into it.
            print(c(_printable(f"❌ [Makewand Guard] 影子工作树克隆失败 (rc={wt_code}: {(wt_err or '').strip()[:160]})，拒绝使用残缺副本。"), COLOR_RED), file=sys.stderr)
            discard_worktree_dir()
            return None, None, None
        if wt_code == 0:
            def cleanup():
                shutil.rmtree(worktree_dir, ignore_errors=True)

            co_target = repo_head_hash if repo_head_hash else "HEAD"
            co_code, _, co_err = run_git_cmd(["git", "checkout", "-q", "-b", branch_name, co_target], cwd=str(worktree_dir))
            if co_code != 0:
                print(c(_printable(f"❌ [Makewand Guard] 影子分支检出失败 (rc={co_code}: {(co_err or '').strip()[:160]})，拒绝残缺快照。"), COLOR_RED), file=sys.stderr)
                cleanup()
                return None, None, None

            # 0. Submodule recursion & dirty state forwarding
            sub_baselines = {}
            if (repo_root / ".gitmodules").exists():
                sub_cmd = ["git", "submodule", "update", "--init", "--recursive"]
                if repo_trust != "untrusted":
                    sub_cmd = ["git", "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive"]
                sub_code, _, sub_err = run_git_cmd(sub_cmd, cwd=str(worktree_dir))
                if sub_code != 0:
                    import sys
                    print(c(_printable(f"❌ [Makewand Guard] 影子工作树子模块初始化失败 ({sub_err})，拒绝以残缺快照作为基线。"), COLOR_RED), file=sys.stderr)
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
                                print(c(_printable(f"❌ [Makewand Guard] 子模块 {sub_rel} 检出失败 ({co_err})，拒绝残缺快照。"), COLOR_RED), file=sys.stderr)
                                cleanup()
                                return None, None, None
                            _, dst_head, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(dst_sub))
                            if dst_head.strip() != target_sub_commit:
                                import sys
                                print(c(_printable(f"❌ [Makewand Guard] 子模块 {sub_rel} 版本校验不一致，拒绝残缺快照。"), COLOR_RED), file=sys.stderr)
                                cleanup()
                                return None, None, None

                        # 2. Forward submodule dirty diffs with full index
                        sd_code, sd_diff, _ = run_git_cmd(["git", "diff", *PATCH_EXPORT_FLAGS, "HEAD"], cwd=str(src_sub), binary=True)
                        if sd_code == 0 and sd_diff and len(sd_diff.strip()) > 0:
                            sa_code, _, sa_err = run_git_cmd(["git", "apply", *PATCH_APPLY_FLAGS, "-"], cwd=str(dst_sub), input_data=sd_diff)
                            if sa_code != 0:
                                import sys
                                print(c(_printable(f"❌ [Makewand Guard] 子模块 {sub_rel} 改动应用失败 ({sa_err})，拒绝残缺快照。"), COLOR_RED), file=sys.stderr)
                                cleanup()
                                return None, None, None

                        # 3. Forward submodule untracked files and symlinks with traversal defense
                        su_code, su_out_b, su_err = run_git_cmd(["git", "ls-files", "-z", "--others", "--exclude-standard"], cwd=str(src_sub), binary=True)
                        if su_code != 0:
                            import sys
                            print(c(_printable(f"❌ [Makewand Guard] 列举子模块 {sub_rel} 未跟踪文件失败 ({su_err})，拒绝残缺快照。"), COLOR_RED), file=sys.stderr)
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
                                    print(c(_printable(f"❌ [Makewand Guard] 复制子模块 {sub_rel} 未跟踪文件失败 ({e})，中止基线建立。"), COLOR_RED), file=sys.stderr)
                                    cleanup()
                                    return None, None, None

                        # Sanitize symlinks within submodule
                        sanitize_shadow_symlinks(dst_sub, src_sub)

                        # 4. Commit submodule dirty baseline inside dst_sub for all submodules
                        a_sub_code, _, a_sub_err = run_git_cmd(["git", "add", "-A"], cwd=str(dst_sub))
                        if a_sub_code != 0:
                            import sys
                            print(c(_printable(f"❌ [Makewand Guard] 子模块 {sub_rel} 暂存失败 ({a_sub_err})，拒绝残缺快照。"), COLOR_RED), file=sys.stderr)
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
                            print(c(_printable(f"❌ [Makewand Guard] 子模块 {sub_rel} 提交失败 ({c_sub_err})，拒绝残缺快照。"), COLOR_RED), file=sys.stderr)
                            cleanup()
                            return None, None, None

                        _, s_base_out, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(dst_sub))
                        sub_baselines[sub_rel] = s_base_out.strip() if s_base_out else None

            # 1. Forward modified binary and text diff FIRST using raw bytes
            diff_code, diff_out_b, diff_err = run_git_cmd(["git", "diff", *PATCH_EXPORT_FLAGS, "HEAD"], cwd=str(repo_root), binary=True)
            if diff_code != 0:
                cleanup()
                return None, None, None

            if diff_out_b and len(diff_out_b.strip()) > 0:
                apply_code, _, apply_err = run_git_cmd(
                    ["git", "apply", *PATCH_APPLY_FLAGS, "-"],
                    cwd=str(worktree_dir),
                    input_data=diff_out_b
                )
                if apply_code != 0:
                    import sys
                    print(c(_printable(f"❌ [Makewand Guard] 影子工作树应用改动失败 ({apply_err})，拒绝以不完整副本作为基线。"), COLOR_RED), file=sys.stderr)
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
                        print(c(_printable(f"❌ [Makewand Guard] 复制未跟踪文件失败 ({e})，中止基线建立。"), COLOR_RED), file=sys.stderr)
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
                print(c(_printable(f"❌ [Makewand Guard] 物理沙箱不可用且检测到越界外部符号链接 ({ext_desc})，为防止写穿宿主机阻断任务。"), COLOR_RED), file=sys.stderr)
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
        print(c(_printable(f"❌ [Makewand Guard] 隔离副本建立失败 ({exc})。"), COLOR_RED), file=sys.stderr)
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
SHADOW_YOUNG_SECONDS = 1800


def _positive_int_env(name: str, default: int) -> int:
    try:
        value = int(os.environ.get(name, "").strip() or default)
    except ValueError:
        return default
    return value if value > 0 else default


def _prune_old_entries(
    root: Path,
    keep: int,
    prefixes: Tuple[str, ...],
    young_protected: Tuple[str, ...],
    young_seconds: Optional[int] = None,
) -> List[str]:
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
    barrier = PROTECTED_YOUNG_SECONDS if young_seconds is None else young_seconds
    for mtime, name, entry in entries[keep:]:
        if name.startswith(young_protected) and now - mtime < barrier:
            continue
        if entry.is_dir(follow_symlinks=False):
            errors = _rmtree_collect(Path(entry.path))
            if errors:
                print(c(_printable(f"⚠ [Makewand Artifacts] 清理过期产物 {name} 失败: {errors[0]}"), COLOR_YELLOW), file=sys.stderr)
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
        print(c(_printable(f"⚠ [Makewand Artifacts] 产物保留策略执行失败: {exc}"), COLOR_YELLOW), file=sys.stderr)
    stamp = datetime.now().strftime("%Y%m%d_%H%M%S")
    path = Path(tempfile.mkdtemp(prefix=f"{kind}_{stamp}_", dir=str(root)))
    os.chmod(path, 0o700)
    return path


def create_private_shadow_dir(base_name: str) -> Path:
    """Creates an empty private directory for a shadow worktree under SHADOW_WORKTREES_DIR."""
    root = ensure_private_dir(config.SHADOW_WORKTREES_DIR)
    shadow_keep = _positive_int_env("MAKEWAND_SHADOW_KEEP", DEFAULT_SHADOW_RETENTION)
    shadow_young = _positive_int_env("MAKEWAND_SHADOW_YOUNG_SECONDS", SHADOW_YOUNG_SECONDS)
    try:
        _prune_old_entries(root, shadow_keep, ("wt_",), ("wt_",), young_seconds=shadow_young)
    except OSError as exc:
        print(c(_printable(f"⚠ [Makewand Shadow] 影子工作树保留策略执行失败: {exc}"), COLOR_YELLOW), file=sys.stderr)
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
            root_path = os.path.realpath(out.strip())
        else:
            # A git failure must never disable locking: fall back to the filesystem root.
            root_path = os.path.realpath(git_root)
    else:
        root_path = real
    if sys.platform == "win32":
        root_path = os.path.normcase(root_path)
    return root_path


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
            raise WorkspaceLockError(_printable(f"另一个 makewand 任务正在此目录运行: {self.root}{detail}。请等待该任务结束后再试。"))
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
# Pure dependency and regenerable cache directories: contents are skipped from file backup
PURE_DEPENDENCY_OR_CACHE_DIRS = frozenset({
    "node_modules", "venv", ".venv", ".tox", "__pycache__", ".mypy_cache", ".pytest_cache",
    ".ruff_cache", ".cache", ".gradle", "bower_components", ".terraform",
})
# Build and artifact directories: only skipped from backup if genuinely ignored by git
BUILD_OR_ENV_DIRS = frozenset({
    "target", "dist", "build", "env", ".next", ".nuxt",
})
HEAVY_DIR_NAMES = PURE_DEPENDENCY_OR_CACHE_DIRS | BUILD_OR_ENV_DIRS
# Regenerable caches that tests create; not worth a delivery warning.
REGENERABLE_CACHE_NAMES = frozenset({".pytest_cache", ".mypy_cache", ".ruff_cache"})
DEFAULT_BACKUP_FILE_LIMIT = 1024 * 1024
DEFAULT_BACKUP_TOTAL_LIMIT = 50 * 1024 * 1024
# Content digests (no copies) of pre-existing files that are not backed up.
DEFAULT_FINGERPRINT_FILE_LIMIT = 64 * 1024 * 1024
DEFAULT_FINGERPRINT_TOTAL_LIMIT = 512 * 1024 * 1024
DEFAULT_FINGERPRINT_SECONDS = 20
DEFAULT_SNAPSHOT_MAX_ENTRIES = 300000
DEFAULT_SNAPSHOT_SECONDS = 120


class SnapshotError(RuntimeError):
    pass


class FsEntry(tuple):
    """(kind, size, mtime_ns, mode, dev, ino, link, atime_ns, ctime_ns, nlink) for one path."""
    __slots__ = ()
    kind = property(lambda self: self[0])
    size = property(lambda self: self[1])
    mtime_ns = property(lambda self: self[2])
    mode = property(lambda self: self[3])
    dev = property(lambda self: self[4])
    ino = property(lambda self: self[5])
    link = property(lambda self: self[6])
    atime_ns = property(lambda self: self[7])
    ctime_ns = property(lambda self: self[8])
    nlink = property(lambda self: self[9])


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
    return FsEntry((kind, size, info.st_mtime_ns, stat.S_IMODE(mode), info.st_dev, info.st_ino, link, info.st_atime_ns,
                    info.st_ctime_ns, info.st_nlink))


def _same_entry(current: Optional[FsEntry], expected: FsEntry) -> bool:
    if current is None or current.kind != expected.kind:
        return False
    if expected.kind == "file":
        return (current.size, current.mtime_ns, current.mode) == (expected.size, expected.mtime_ns, expected.mode)
    if expected.kind == "link":
        return current.link == expected.link
    return True


def _unchanged_since_snapshot(current: Optional[FsEntry], before: FsEntry) -> bool:
    """True only while current is still the object the task-start snapshot recorded.

    _same_entry() deliberately accepts a restored copy (rollback verification).
    Deciding whether the task created or modified a path also compares the inode
    and its change time: a file's owner can reset mtime with utime() or rename a
    look-alike into place, but cannot reset ctime.
    """
    if current is None or current.kind != before.kind:
        return False
    if before.kind == "dir":
        return True
    if before.kind == "link":
        return current.link == before.link
    return ((current.size, current.mtime_ns, current.ctime_ns, current.mode, current.dev, current.ino)
            == (before.size, before.mtime_ns, before.ctime_ns, before.mode, before.dev, before.ino))


def _changed_since_snapshot(current: Optional[FsEntry], before: FsEntry,
                            content_changed: Callable[[], Optional[bool]]) -> Optional[bool]:
    """Whether a path differs from its task-start state in type, mode or content:
    True, False, or None when that cannot be decided.

    Size and mtime alone can be faked with utime(), so when only the inode or the
    change time differs the content decides: content_changed() compares the file
    with what was recorded before the task (None when nothing decides). ctime alone
    is no evidence either way: chmod to the same mode, an extended attribute or a
    hard link added elsewhere changes it, and so does an in-place rewrite whose
    mtime was put back. An undecided change is None: never reported as a
    modification, and never taken for no change (whether a changed link count
    explains it is for content_changed() to say: HostWorkspaceTransaction._content_changed).
    """
    if current is None or current.kind != before.kind:
        return True
    if before.kind != "file":
        return not _unchanged_since_snapshot(current, before)
    if (current.size, current.mtime_ns, current.mode) != (before.size, before.mtime_ns, before.mode):
        return True
    if (current.ctime_ns, current.dev, current.ino) == (before.ctime_ns, before.dev, before.ino):
        return False
    return content_changed()


def _makewand_state_paths() -> set:
    """Makewand's own state directories are never part of a workspace snapshot."""
    paths = set()
    for candidate_path in (getattr(config, "ARTIFACTS_DIR", None), getattr(config, "SHADOW_WORKTREES_DIR", None),
                           getattr(config, "CONFIG_DIR", None)):
        if candidate_path:
            paths.add(os.path.realpath(str(candidate_path)))
    return paths


def scan_workspace_tree(root: Union[str, Path], strict: bool = True, errors: Optional[List[str]] = None,
                        max_entries: Optional[int] = None, time_budget: Optional[float] = None,
                        git_markers: Optional[Dict[str, FsEntry]] = None) -> Dict[str, FsEntry]:
    """Records every path under root (never following symlinks), skipping .git entries
    and makewand's own state directories.

    strict=True raises SnapshotError on any unreadable path or budget overrun,
    because an incomplete task-start snapshot could later misclassify a
    pre-existing file as task-created.

    git_markers, when given, receives every ``.git`` entry itself (the root's and
    nested repositories'), which is never descended into.
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
            if child.path in excluded:
                continue
            rel = f"{rel_dir}/{child.name}" if rel_dir else child.name
            if child.name == ".git":
                if git_markers is not None:
                    try:
                        git_markers[rel] = _fs_entry(child.path, os.lstat(child.path))
                    except OSError as exc:
                        problem(f"无法读取 {rel}: {exc.strerror or exc}")
                continue
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


def _open_metadata_directory(parent_fd: int, name: str) -> int:
    """Opens directory name in parent_fd (a new descriptor) to write repository metadata
    below it. A link there is replaced by a new, empty directory, never followed."""
    try:
        info = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
    except FileNotFoundError:
        info = None
    if info is not None and stat.S_ISLNK(info.st_mode):
        os.unlink(name, dir_fd=parent_fd)
        info = None
    if info is None:
        try:
            os.mkdir(name, 0o755, dir_fd=parent_fd)
        except FileExistsError:
            pass
    return os.open(name, _NOFOLLOW_DIR_FLAGS, dir_fd=parent_fd)


def _unlink_repository_link(path: Path) -> None:
    """Remove a metadata link itself, including a Windows directory junction."""
    if _is_reparse_api_path(path) and path.is_dir() and not path.is_symlink():
        path.rmdir()
    else:
        path.unlink()


def _own_repository_directory(git_dir: Union[str, Path], name: str) -> None:
    """Detach a template-linked metadata directory before any later git command."""
    if HOST_TRANSACTION_SUPPORTED:
        fd = os.open(str(git_dir), _NOFOLLOW_DIR_FLAGS)
        try:
            child = _open_metadata_directory(fd, name)
            os.close(child)
        finally:
            os.close(fd)
        return
    base = _fs_path(git_dir)
    if base.is_symlink() or _is_reparse_api_path(base) or not base.is_dir():
        raise OSError(errno.ENOTDIR, "repository metadata root must be a real directory")
    target = base / name
    if target.is_symlink() or _is_reparse_api_path(target):
        _unlink_repository_link(target)
    target.mkdir(exist_ok=True)


def write_repository_file(git_dir: Union[str, Path], rel: str, data: bytes, mode: int = 0o644) -> None:
    """Writes data to rel below git_dir - a file of a repository's own metadata, such as
    info/exclude - as a regular file of its own, never through a link.

    git init copies init.templateDir as it is, symbolic links included: a template's
    info/exclude (or config) linked to a shared file arrives in every new repository as
    that link, and writing through it would overwrite the shared file. A link (or a
    hard-linked file) at rel is replaced by a new regular file and a link at a directory
    between git_dir and rel by a real directory; no link target is ever written. git_dir
    itself must be a directory, not a link (OSError otherwise)."""
    parts = rel.split("/")
    if any(part in ("", ".", "..") for part in parts):
        raise OSError(errno.EINVAL, _printable(f"invalid repository metadata path: {rel}"))
    if not HOST_TRANSACTION_SUPPORTED:
        # No directory handles (native Windows): the same rule by path.
        base = _fs_path(git_dir)
        if base.is_symlink() or _is_reparse_api_path(base) or not base.is_dir():
            raise OSError(errno.ENOTDIR, _printable(f"repository metadata directory is not a directory: {base}"))
        parent = base
        for part in parts[:-1]:
            parent = parent / part
            if parent.is_symlink() or _is_reparse_api_path(parent):
                _unlink_repository_link(parent)
            parent.mkdir(exist_ok=True)
        target = parent / parts[-1]
        if target.is_symlink() or _is_reparse_api_path(target) or (target.is_file() and target.stat().st_nlink > 1):
            _unlink_repository_link(target)
        with open(target, "wb") as stream:
            stream.write(data)
        return
    fd = os.open(str(git_dir), _NOFOLLOW_DIR_FLAGS)
    try:
        for part in parts[:-1]:
            child = _open_metadata_directory(fd, part)
            os.close(fd)
            fd = child
        name = parts[-1]
        try:
            info = os.stat(name, dir_fd=fd, follow_symlinks=False)
        except FileNotFoundError:
            info = None
        if info is not None and (not stat.S_ISREG(info.st_mode) or info.st_nlink > 1):
            if stat.S_ISDIR(info.st_mode):
                raise IsADirectoryError(errno.EISDIR, _printable(f"repository metadata path is a directory: {rel}"))
            os.unlink(name, dir_fd=fd)
        flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
        with os.fdopen(os.open(name, flags, mode, dir_fd=fd), "wb") as stream:
            stream.write(data)
    finally:
        os.close(fd)


def _own_repository_file(git_dir: Union[str, Path], rel: str) -> None:
    """Makes rel below git_dir a regular file of this repository before anything writes it:
    a link git init copied from init.templateDir (or a hard-linked file) becomes a regular
    copy of the content it led to, so that later writes (git config) stay in this
    repository instead of rewriting the shared file. Nothing happens to a regular file or
    a missing one."""
    path = _fs_path(Path(git_dir).joinpath(*rel.split("/")))
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return
    if stat.S_ISREG(info.st_mode) and info.st_nlink == 1:
        return
    data = b""
    try:
        if stat.S_ISREG(os.stat(path).st_mode):  # the content a link or hard link leads to
            with open(path, "rb") as stream:
                data = stream.read()
    except FileNotFoundError:
        pass  # a dangling link: git saw no content there either
    write_repository_file(git_dir, rel, data)


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


# Sampled fingerprint of a pre-existing file that gets no full digest (over
# MAKEWAND_FINGERPRINT_FILE_LIMIT, past the total or time budget): its size and
# FINGERPRINT_SAMPLE_CHUNKS chunks at fixed offsets spread over the whole file, each
# byte read once and at most FINGERPRINT_SAMPLE_BYTES per file (_sample_ranges). For a
# file of at most FINGERPRINT_SAMPLE_BYTES the chunks cover every byte, so the sample
# decides like a full digest. For a larger one it can prove that the file was rewritten
# (another build, a truncated or replaced copy), never that it is intact - a targeted
# patch between the chunks keeps it - so a matching sample only corroborates what the
# stat change itself shows (HostWorkspaceTransaction._content_changed).
FINGERPRINT_SAMPLE_CHUNK = 64 * 1024
FINGERPRINT_SAMPLE_CHUNKS = 16
FINGERPRINT_SAMPLE_BYTES = FINGERPRINT_SAMPLE_CHUNK * FINGERPRINT_SAMPLE_CHUNKS


def _sample_ranges(size: int) -> List[Tuple[int, int]]:
    """(offset, length) of the bytes the sampled fingerprint of a size-byte file reads:
    FINGERPRINT_SAMPLE_CHUNKS chunks at fixed offsets, overlapping or adjacent chunks
    merged, so a file of at most FINGERPRINT_SAMPLE_BYTES is one range: all of it."""
    span = max(size - FINGERPRINT_SAMPLE_CHUNK, 0)
    ranges: List[List[int]] = []
    for index in range(FINGERPRINT_SAMPLE_CHUNKS):
        start = span * index // (FINGERPRINT_SAMPLE_CHUNKS - 1)
        end = min(start + FINGERPRINT_SAMPLE_CHUNK, size)
        if ranges and start <= ranges[-1][1]:
            ranges[-1][1] = max(ranges[-1][1], end)
        else:
            ranges.append([start, end])
    return [(start, end - start) for start, end in ranges]


class _DigestBudget:
    """How much content one phase may hash: MAKEWAND_FINGERPRINT_FILE_LIMIT per file and
    MAKEWAND_FINGERPRINT_TOTAL_LIMIT / _SECONDS in all. The task-start fingerprints, the
    delivery check and the rollback verification each get the same limits, so neither
    check re-hashes more, or for longer, than the fingerprinting did. Sampled
    fingerprints are counted apart (_SampleBudget)."""

    def __init__(self):
        self.file_limit = _positive_int_env("MAKEWAND_FINGERPRINT_FILE_LIMIT", DEFAULT_FINGERPRINT_FILE_LIMIT)
        self.total_limit = _positive_int_env("MAKEWAND_FINGERPRINT_TOTAL_LIMIT", DEFAULT_FINGERPRINT_TOTAL_LIMIT)
        self.seconds = _positive_int_env("MAKEWAND_FINGERPRINT_SECONDS", DEFAULT_FINGERPRINT_SECONDS)
        self.deadline = time.monotonic() + self.seconds
        self.total = 0

    def over_limit(self, size: int) -> Optional[str]:
        """Why another size bytes do not fit (None: they fit and are counted)."""
        if size > self.file_limit:
            return f"超过单文件摘要上限 MAKEWAND_FINGERPRINT_FILE_LIMIT={self.file_limit} 字节"
        if self.total + size > self.total_limit:
            return f"超过摘要总量上限 MAKEWAND_FINGERPRINT_TOTAL_LIMIT={self.total_limit} 字节"
        self.total += size
        return None

    def expired(self) -> Optional[str]:
        """Why hashing has to stop now (None while the time budget lasts)."""
        if time.monotonic() > self.deadline:
            return f"超出摘要时间预算 MAKEWAND_FINGERPRINT_SECONDS={self.seconds} 秒"
        return None


class _SampleBudget(_DigestBudget):
    """The sampled fingerprints' own budget in a phase: the same total and time limits as
    _DigestBudget, counted apart, so that hashing whole files never uses up what sampling
    the files without a full digest needs. A sample costs its read bytes - at most
    FINGERPRINT_SAMPLE_BYTES - and has no per-file limit; its time is counted from the
    budget's creation, which at task start follows the full digests."""

    def over_limit(self, size: int) -> Optional[str]:
        cost = min(size, FINGERPRINT_SAMPLE_BYTES)
        if self.total + cost > self.total_limit:
            return f"超过抽样指纹总量上限 MAKEWAND_FINGERPRINT_TOTAL_LIMIT={self.total_limit} 字节"
        self.total += cost
        return None

    def expired(self) -> Optional[str]:
        if time.monotonic() > self.deadline:
            return f"超出抽样指纹时间预算 MAKEWAND_FINGERPRINT_SECONDS={self.seconds} 秒"
        return None


# Directory handles HostWorkspaceTransaction._hash_pre_files keeps open at once.
_HASH_DIRECTORY_HANDLES = 64


def _fair_sample_order(sizes: Dict[str, int]) -> List[str]:
    """The paths of sizes ({path: size}) in the order the sampled fingerprints claim the
    _SampleBudget: in turns across size classes (powers of two), the smallest file of
    each class first, so that every size gets its share of the budget - a large file
    (whose sample is the only check of its content) is never crowded out by hundreds of
    smaller ones, nor the small ones by a few large ones."""
    classes: Dict[int, List[str]] = {}
    for rel in sorted(sizes, key=lambda path: (sizes[path], path)):
        classes.setdefault(sizes[rel].bit_length(), []).append(rel)
    turns = itertools.zip_longest(*(classes[size_class] for size_class in sorted(classes)))
    return [rel for turn in turns for rel in turn if rel is not None]


def _same_identity(info: os.stat_result, entry: FsEntry) -> bool:
    return ((info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)
            == (entry.dev, entry.ino, entry.size, entry.mtime_ns, entry.ctime_ns))


def _entry_digest(dir_fd: int, name: str, expected: Optional[FsEntry] = None, sampled: bool = False) -> Optional[str]:
    """sha256 of regular file name in dir_fd (sampled: its sampled fingerprint), never
    following a link; None when it cannot be read. With expected, also None unless the
    file is still the inode, size, mtime and ctime the snapshot recorded, before and
    after it was read."""
    try:
        file_fd = os.open(name, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0)
                          | getattr(os, "O_CLOEXEC", 0), dir_fd=dir_fd)
    except OSError:
        return None
    try:
        info = os.fstat(file_fd)
        if not stat.S_ISREG(info.st_mode) or (expected is not None and not _same_identity(info, expected)):
            return None
        digest = hashlib.sha256()
        if sampled:
            digest.update(b"%d\0" % info.st_size)
            for offset, length in _sample_ranges(info.st_size):
                digest.update(os.pread(file_fd, length, offset))
        else:
            while chunk := os.read(file_fd, 1024 * 1024):
                digest.update(chunk)
        if expected is not None and not _same_identity(os.fstat(file_fd), expected):
            return None
        return digest.hexdigest()
    except OSError:
        return None
    finally:
        os.close(file_fd)


def _file_sha256_at(root: str, rel: str, sampled: bool = False) -> Optional[str]:
    try:
        fd, name = _open_parent(root, rel)
    except OSError:
        return None
    try:
        return _entry_digest(fd, name, sampled=sampled)
    finally:
        os.close(fd)


def _printable(text: str) -> str:
    """text with the undecodable bytes of a file name (surrogate escapes from os.fsdecode
    or git output read with errors='surrogateescape') shown as \\xNN, so that printing
    or storing it never raises UnicodeEncodeError."""
    try:
        return text.encode("utf-8", "surrogateescape").decode("utf-8", "backslashreplace")
    except UnicodeEncodeError:  # a lone surrogate that no file name produced
        return text.encode("utf-8", "backslashreplace").decode("utf-8")


def _json_text(data: Any) -> str:
    """JSON for a private manifest. A file name with undecodable bytes cannot be written as
    UTF-8, so then every non-ASCII character is escaped (\\udcNN round-trips exactly)."""
    text = json.dumps(data, ensure_ascii=False, indent=2)
    try:
        text.encode("utf-8")
    except UnicodeEncodeError:
        return json.dumps(data, ensure_ascii=True, indent=2)
    return text


def _shell_quote(path: Union[str, Path]) -> str:
    """A shell word for path; a name with undecodable bytes becomes a $'...' word with \\xNN escapes."""
    text = str(path)
    if text == _printable(text):
        return shlex.quote(text)
    escaped = "".join(chr(byte) if 32 <= byte < 127 and byte not in (39, 92) else f"\\x{byte:02x}"
                      for byte in os.fsencode(text))
    return f"$'{escaped}'"


def _summarize_paths(paths: List[str], limit: int = 20) -> str:
    shown = ", ".join(paths[:limit])
    return _printable(shown + (f" …（另有 {len(paths) - limit} 项）" if len(paths) > limit else ""))


EXECUTABLE_OR_BINARY_EXTS = frozenset({".pyc", ".pyo", ".pyd", ".so", ".dylib", ".dll", ".exe", ".bin"})


def _is_regenerable_cache(rel: str, root: Optional[str] = None) -> bool:
    """
    Determines whether rel is a legitimate regenerable compiler/test cache.
    .pyc / .pyo files are ONLY considered regenerable cache if:
      1. They reside directly inside a __pycache__ directory, AND
      2. There is a matching .py source file in the directory containing __pycache__.
    Loose .pyc files outside __pycache__ or orphan .pyc files with no matching source
    are NOT regenerable cache and represent potential payload injection (#py-orch-2).
    """
    rel = rel.replace("\\", "/")
    py_rel = _bytecode_source(rel)
    if py_rel is not None:
        if root:
            py_path = os.path.join(root, py_rel)
            return os.path.isfile(py_path)
        return True
    return any(part in REGENERABLE_CACHE_NAMES for part in rel.split("/")[:-1])


def _bytecode_source(rel: str) -> Optional[str]:
    """The source of bytecode cached in __pycache__ (pkg/__pycache__/mod.cpython-312.pyc -> pkg/mod.py)."""
    parts = rel.replace("\\", "/").split("/")
    if len(parts) < 2 or parts[-2] != "__pycache__" or not parts[-1].endswith((".pyc", ".pyo")):
        return None
    return "/".join(parts[:-2] + [parts[-1].split(".", 1)[0] + ".py"])


# Test output: reports, coverage data and tool caches that a project's own test
# setup writes at the tools' default locations (pytest --junitxml, pytest-cov and
# coverage.py html/xml/json/lcov, hypothesis, tox/nox, jest, vitest, c8, nyc,
# istanbul, Playwright). is_test_output_path() is the one definition of it, for the
# test gate (orchestrator._filter_non_deliverable_test_changes: such changes are not
# tested inputs; untracked_test_output() also leaves out what git tracks) and for
# the host-mode delivery check (_is_inert_test_artifact), which removes test output
# the run created instead of blocking, unless it could run as code (an executable
# bit, Python source, a security-sensitive name) or is a link (a symlink, or a hard
# link that reaches another file's bytes). Test output a test run creates is removed
# by run_local_tests itself (orchestrator._discard_created_test_output), which names
# what it cannot remove for those reasons, so neither the pipeline's snapshot check
# nor the delivery sees the rest. The gate judges paths from the directory the test
# run used, the delivery from that directory and from the repository root alike
# (HostWorkspaceTransaction._report_roots). The two checks still differ: the
# gate also accepts every other path .gitignore covers, which the delivery check
# blocks when the task created it - a report written to a custom location
# (--junitxml=results/pytest-junit.xml with results/ ignored) passes the gate and
# then blocks the delivery as UNVERIFIED - and a pre-existing file the run rewrote
# is kept only when that is clearly safe (_unreviewed_changes).
TEST_OUTPUT_NAMES = frozenset({
    ".coverage", "coverage.xml", "coverage.json", "coverage.lcov", "lcov.info",
    "pytest.log", "test.log", "test-results", "test-reports", "test_reports",
    "junit.xml", "report.xml", ".hypothesis", "htmlcov", "cov_html",
    ".pytest_cache", ".tox", ".nox", ".mypy_cache", ".ruff_cache",
    ".nyc_output", ".jest-cache", ".vitest-cache",
})
# Report directories of static reports (HTML, CSS and JS assets, JSON, XML, LCOV,
# images), matched only as a directory: a file named "coverage" may be a console
# script. coverage/ is the default output of jest, vitest, c8, nyc and istanbul (and
# c8's raw coverage/tmp/), playwright-report/ the Playwright HTML report (its
# test-results/ is in TEST_OUTPUT_NAMES). They count only where those tools write
# them: at the root (of the repository, or of the directory the paths are relative
# to) and at the root of a package with a manifest of its own
# (TEST_REPORT_PACKAGE_MARKERS) - src/coverage/ is source code. Inside installed
# packages (TEST_REPORT_FOREIGN_DIRS) such a directory belongs to the package, not to a test run.
TEST_REPORT_DIRS = frozenset({"coverage", "playwright-report"})
TEST_REPORT_PACKAGE_MARKERS = ("package.json", "pyproject.toml", "setup.py")
TEST_REPORT_FOREIGN_DIRS = frozenset({"node_modules", "site-packages", "dist-packages", "bower_components", "vendor"})
TEST_OUTPUT_PREFIXES = (".coverage", "coverage.", "pytest.", "test-results", "test_results", "test-report", "test_report")
TEST_OUTPUT_PATTERNS = ("junit*.xml",)
TEST_OUTPUT_SUFFIXES = (".log", ".tmp", ".coverage", ".pyc", ".pyo", ".cache")
# Configuration that test tools read (coverage.py plugins, pytest pythonpath and
# addopts) is never test output, although the names match a prefix above.
TEST_TOOL_CONFIG_NAMES = frozenset({".coveragerc", "pytest.ini", "pytest.toml"})
# Test output that a later run loads as code: tox/nox virtualenvs (activate scripts,
# installed packages) and jest's transform cache. The delivery check removes such
# files the task created, but never keeps a pre-existing one that the run changed.
# Other transform caches (babel-loader's node_modules/.cache, vite's
# node_modules/.vite) are not test output at all.
TEST_OUTPUT_CODE_DIRS = frozenset({".tox", ".nox", ".jest-cache"})
# Test output that is plain data, which no interpreter, build tool or shell on the
# host loads as code by itself: JSON (require() parses it as data), XML, LCOV, logs,
# the HTML, CSS and images of a report, coverage.py's SQLite data files, and the
# suffixless data files of tool directories that hold nothing else (pytest's cache
# values, hypothesis' example database, mypy's and ruff's caches). A pre-existing
# file the run rewrote is kept only as such data or with the bytes it had - never
# e.g. a .js file that require() would load (_unreviewed_changes).
TEST_OUTPUT_DATA_SUFFIXES = frozenset({".json", ".xml", ".lcov", ".info", ".txt", ".log", ".csv", ".md", ".coverage",
                                       ".html", ".htm", ".css", ".svg", ".png", ".gif", ".jpg", ".jpeg", ".ico"})
TEST_OUTPUT_DATA_DIRS = frozenset({".pytest_cache", ".hypothesis", ".mypy_cache", ".ruff_cache"})
# Playwright's own trace archives and videos - test-results/<test>/trace.zip and
# video.webm, copied into its HTML report as playwright-report/data/<sha1>.zip and
# .webm - are report data too, but only in those two directories where Playwright
# writes them (_is_playwright_artifact): only its trace viewer or a browser opens
# them, and every run with trace or video 'on' rewrites them. Its screenshots are
# images (TEST_OUTPUT_DATA_SUFFIXES).
PLAYWRIGHT_OUTPUT_DIRS = frozenset({"test-results", "playwright-report"})
PLAYWRIGHT_ARTIFACT_SUFFIXES = frozenset({".zip", ".webm"})


def _is_vitest_results(parts: List[str]) -> bool:
    """vitest's run-order cache, plain JSON: node_modules/.vite/vitest/<hash>/results.json
    (older releases: node_modules/.vitest/results.json). The rest of .vite is vite's
    dependency transform cache and stays outside the test output definition."""
    if parts[-1] != "results.json":
        return False
    return ".vitest" in parts[:-1] or any(parts[index] == ".vite" and parts[index + 1] == "vitest"
                                          for index in range(len(parts) - 2))


def _in_report_dir(original: List[str], parents: List[str], root: Optional[Union[str, Path]],
                   directories: frozenset = TEST_REPORT_DIRS) -> bool:
    """Whether a report directory (one of directories, TEST_REPORT_DIRS by default)
    holds the path made of the components original (parents: its directory
    components, lower-cased): one at the top of the path or, when root is given to
    look the manifest up in, at the root of a package that has a
    TEST_REPORT_PACKAGE_MARKERS file. Never inside an installed package."""
    if any(part in TEST_REPORT_FOREIGN_DIRS for part in parents):
        return False
    for index, part in enumerate(parents):
        if part not in directories:
            continue
        if index == 0 or (root is not None and any(
                os.path.isfile(os.path.join(str(root), *original[:index], marker)) for marker in TEST_REPORT_PACKAGE_MARKERS)):
            return True
    return False


def is_test_output_path(rel: str, root: Optional[Union[str, Path]] = None) -> bool:
    """True for a path that a test run writes as a report, coverage data or cache.

    rel is relative to root - the repository, or the directory the test run used -
    '/' or '\\' separated; case is ignored. Everything inside a tool's own directory
    (TEST_OUTPUT_NAMES: .pytest_cache, .hypothesis, .tox, htmlcov, ...) is test
    output. Elsewhere Python source never is, whatever its name or the report
    directory around it says (coverage.py, coverage/conftest.py), and test tool
    configuration never is. A report directory (TEST_REPORT_DIRS) counts at the top
    of rel and, when root is given to look for the manifest, at the root of a package
    inside it. Whether a path git tracks is test output is answered by
    untracked_test_output(): never. Whether a delivery may drop such a path without
    review also depends on what it is (_is_inert_test_artifact).
    """
    original = rel.replace("\\", "/").split("/")
    parts = [part.lower() for part in original]
    name, parents = parts[-1], parts[:-1]
    if name in TEST_TOOL_CONFIG_NAMES:
        return False
    if any(part in TEST_OUTPUT_NAMES for part in parents):
        return True
    if name.endswith((".py", ".pyw")):
        return False
    return (name in TEST_OUTPUT_NAMES or _in_report_dir(original, parents, root)
            or name.startswith(TEST_OUTPUT_PREFIXES) or name.endswith(TEST_OUTPUT_SUFFIXES)
            or any(fnmatch.fnmatchcase(name, pattern) for pattern in TEST_OUTPUT_PATTERNS)
            or _is_vitest_results(parts))


def tracked_paths(root: Union[str, Path]) -> set:
    """The files git tracks in the repository holding the directory root and in its
    submodules, relative to root ('/' separated); empty outside a git work tree.
    Raises OSError when git cannot list them."""
    if find_git_root(root) is None:
        return set()
    code, listed, err = run_git_cmd(["git", "ls-files", "-z", "--cached", "--recurse-submodules"], cwd=str(root), binary=True)
    if code != 0:
        raise OSError(_printable(f"无法列出被跟踪的文件 (rc={code}): {os.fsdecode(err or b'').strip()[:200]}"))
    return {os.fsdecode(name) for name in listed.split(b"\0") if name}


def untracked_test_output(paths: List[str], root: Union[str, Path]) -> set:
    """The paths among paths - relative to the directory root, as is_test_output_path()
    takes them - that are test output and that git does not track, in the repository
    or in one of its submodules: a tracked file is a deliverable input whatever its
    name or directory says. Outside a git work tree nothing is tracked. Raises OSError
    when git cannot list the tracked files."""
    output = {rel for rel in paths if is_test_output_path(rel, root)}
    if not output:
        return output
    tracked = tracked_paths(root)
    return {rel for rel in output if rel.replace("\\", "/") not in tracked}


def _is_test_output_data(rel: str) -> bool:
    """True for test output that is plain data (TEST_OUTPUT_DATA_SUFFIXES, coverage.py's
    .coverage and .coverage.* data files, a suffixless file in one of the TEST_OUTPUT_DATA_DIRS)."""
    parts = rel.replace("\\", "/").lower().split("/")
    name = parts[-1]
    suffix = os.path.splitext(name)[1]
    if suffix in TEST_OUTPUT_DATA_SUFFIXES or name == ".coverage" or name.startswith(".coverage."):
        return True
    return not suffix and any(part in TEST_OUTPUT_DATA_DIRS for part in parts[:-1])


def _is_playwright_artifact(rel: str, root: Optional[Union[str, Path]] = None) -> bool:
    """True for a Playwright trace archive or video (PLAYWRIGHT_ARTIFACT_SUFFIXES) in its
    test-results/ or playwright-report/ directory at the top of rel (relative to root)
    or at the root of a package inside it, as _in_report_dir() places report directories."""
    original = rel.replace("\\", "/").split("/")
    parts = [part.lower() for part in original]
    if os.path.splitext(parts[-1])[1] not in PLAYWRIGHT_ARTIFACT_SUFFIXES:
        return False
    return _in_report_dir(original, parts[:-1], root, PLAYWRIGHT_OUTPUT_DIRS)


# Host-mode delivery check (HostWorkspaceTransaction.finalize_success). Only paths
# the task created, modified or deleted - judged on type, mode and content by
# _changed_since_snapshot() - are examined; pre-existing files the task left
# untouched (ignored caches, virtualenvs, tracked vendored binaries) never block.
#
# Security-sensitive changes always block when they are not part of the reviewed
# diff, wherever they are: binary or executable artifacts (EXECUTABLE_OR_BINARY_EXTS
# or a newly gained executable bit) and text files that a later shell, interpreter
# or git command on the host picks up by itself (direnv/dotenv files, Python
# start-up hooks and .pth files, git hook manager directories). Nested .git
# metadata created by the task is checked separately.
SECURITY_SENSITIVE_NAMES = frozenset({".envrc", "sitecustomize.py", "usercustomize.py"})
SECURITY_SENSITIVE_SUFFIXES = frozenset({".pth"})
GIT_HOOK_DIR_NAMES = frozenset({".husky", ".githooks"})


def _security_risk(rel: str, gained_exec: bool = False) -> Optional[str]:
    """'binary' or 'sensitive' for a path whose unreviewed change always blocks delivery."""
    name = rel.rsplit("/", 1)[-1].lower()
    suffix = os.path.splitext(name)[1]
    if gained_exec or suffix in EXECUTABLE_OR_BINARY_EXTS:
        return "binary"
    if (name.startswith(".env") or name in SECURITY_SENSITIVE_NAMES or suffix in SECURITY_SENSITIVE_SUFFIXES
            or any(part in GIT_HOOK_DIR_NAMES for part in rel.split("/")[:-1])):
        return "sensitive"
    return None


def _is_inert_test_artifact(rel: str, entry: Optional[FsEntry], exec_bits: bool = True,
                            root: Optional[str] = None) -> bool:
    """True for test output (is_test_output_path, relative to root) that nothing imports or runs.

    Such a change outside the reviewed diff does not block delivery: files the task
    created are removed again, pre-existing ones the run deleted are left so, and
    pre-existing ones it updated are left as they are when that is clearly safe
    (_unreviewed_changes). A path is never inert when it is not a regular file, has an
    executable bit (exec_bits=False: the filesystem sets one on every file, so it
    means nothing), is Python source, or is security-sensitive.
    """
    if entry is not None and (entry.kind != "file" or (exec_bits and entry.mode & 0o111)):
        return False
    name = rel.rsplit("/", 1)[-1].lower()
    if os.path.splitext(name)[1] in (".py", ".pyw") or _security_risk(rel):
        return False
    return is_test_output_path(rel, root)


def _fixed_exec_bits(directory: str) -> Optional[bool]:
    """True when a file created in directory with mode 0600 already reports an
    executable bit: the filesystem gives every file the same mode (WSL drvfs, CIFS
    file_mode, exFAT/NTFS fmask), so executable bits carry no information there.
    None when the probe cannot run. The probe file is removed at once."""
    path = os.path.join(directory, f".makewand-mode-probe-{os.getpid()}-{time.monotonic_ns()}")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(path, flags, 0o600)
    except OSError:
        return None
    try:
        return bool(os.fstat(fd).st_mode & 0o111)
    except OSError:
        return None
    finally:
        os.close(fd)
        try:
            os.unlink(path)
        except OSError:
            pass


def _move_aside(source: str, folder: Path) -> Path:
    """Moves source (a file, link or directory, never followed) into folder and
    returns its new path; across filesystems it is copied, then removed."""
    folder.mkdir(mode=0o700, parents=True, exist_ok=True)
    name = os.path.basename(source)
    destination = folder / name
    suffix = 1
    while os.path.lexists(destination):
        destination = folder / f"{name}.{suffix}"
        suffix += 1
    try:
        os.rename(source, destination)
        return destination
    except OSError as exc:
        if exc.errno != errno.EXDEV:
            raise
    info = os.lstat(source)
    if stat.S_ISLNK(info.st_mode):
        os.symlink(os.readlink(source), destination)
        os.unlink(source)
    elif stat.S_ISDIR(info.st_mode):
        shutil.copytree(source, destination, symlinks=True)
        errors = _rmtree_collect(Path(source))
        if errors:
            raise OSError(errno.EIO, errors[0])
    else:
        _copy_regular_nofollow(source, destination)
        os.chmod(destination, stat.S_IMODE(info.st_mode) & 0o700)
        os.unlink(source)
    return destination


def _parse_raw_diff(raw: bytes, prefix: str, shown: Dict[str, str], deleted: set) -> None:
    """Reads ``git diff --raw -z --no-renames`` output: the new git mode of every path
    whose current content the diff shows, and the paths it shows as deleted."""
    tokens = raw.split(b"\0")
    index = 0
    while index + 1 < len(tokens):
        meta, name = tokens[index], tokens[index + 1]
        index += 2
        fields = meta[1:].split() if meta.startswith(b":") else []
        if len(fields) < 5 or not name:
            raise ValueError(f"无法解析 git diff --raw 输出: {meta[:80]!r}")
        path = prefix + os.fsdecode(name)
        if fields[4][:1] == b"D":
            deleted.add(path)
        else:
            shown[path] = fields[1].decode("ascii", "replace")


def _export_workspace_patch(repo: str, base: Optional[str], target: Path,
                            include_untracked: bool = True) -> Tuple[bool, bool, Optional[str]]:
    """Writes the binary patch of every reviewable change in repo against base (the
    empty tree for an unborn baseline) to target: tracked edits, commits the task
    made and new untracked files git does not ignore. Like the review diff (see
    _read_workspace_diff) it uses a private intent-to-add index, so the user's
    index is never touched. git writes the file itself, so a large binary never
    has to fit in memory. include_untracked=False leaves new files out (a retry
    when one of them cannot be read).

    Returns (written, complete, detail): complete=False means new untracked files
    may be missing from a written patch (detail says why), or nothing was written.
    """
    head, error = _resolve_diff_head(repo)
    if error:
        return False, False, error
    try:
        with tempfile.TemporaryDirectory(prefix="makewand-rejected-index-") as directory:
            token = _diff_index.set(str(Path(directory) / "index"))
            try:
                code, _, err = run_git_cmd(["git", "read-tree", head] if head else ["git", "read-tree", "--empty"], cwd=repo)
                if code != 0:
                    return False, False, f"git read-tree 失败 (rc={code}): {err.strip()[:160]}"
                missing = "补丁未包含新建的未跟踪文件"
                if include_untracked:
                    add_code, _, add_err = run_git_cmd(["git", "add", "-A", "--intent-to-add"], cwd=repo)
                    missing = (f"git add 失败 (rc={add_code}: {add_err.strip()[:160]})，新建的未跟踪文件可能不在补丁中"
                               if add_code != 0 else None)
                code, _, err = run_git_cmd(["git", "diff", *PATCH_EXPORT_FLAGS, f"--output={target}",
                                            base or EMPTY_TREE_HASH], cwd=repo)
                if code != 0:
                    return False, False, f"git diff 失败 (rc={code}): {err.strip()[:160]}"
                return True, missing is None, missing
            finally:
                _diff_index.reset(token)
    except OSError as exc:
        return False, False, f"无法建立私有 Git 索引: {exc}"


class _DeliveryReview(NamedTuple):
    """What the host-mode delivery check found outside the reviewed diff."""
    blocks: List[str]  # security blocks: the delivery is rolled back as UNVERIFIED
    transient: List[str]  # bytecode the run generated or rewrote: purged on delivery
    inert_new: List[str]  # test output the task created: removed on delivery
    inert_kept: List[str]  # pre-existing test output the run updated: left as it is
    notes: List[str]
    unrestorable: set  # changed pre-existing files without a backup: a rollback cannot restore them
    unconfirmed: set  # pre-existing files whose content could not be confirmed (no digest decides)


class _HookDirectory(NamedTuple):
    """A directory git runs hook scripts from without any review, outside the scanned work tree."""
    path: str  # absolute, symlinks resolved
    shared: bool  # other checkouts or repositories used it at task start
    common_dir: Optional[str]  # the repository whose linked worktrees share it, re-checked later


def _is_within(path: str, directory: str) -> bool:
    return path == directory or path.startswith(directory.rstrip(os.sep) + os.sep)


def _has_linked_worktrees(common_dir: str) -> bool:
    """True when the repository whose shared git directory is common_dir has a linked
    worktree: an administrative entry in worktrees/ whose checkout still exists or is
    locked (on removable media). Entries left by a deleted checkout do not count."""
    admin = os.path.join(common_dir, "worktrees")
    try:
        names = os.listdir(admin)
    except OSError:
        return False
    for name in names:
        entry = os.path.join(admin, name)
        if os.path.lexists(os.path.join(entry, "locked")):
            return True
        try:
            with open(os.path.join(entry, "gitdir"), "rb") as handle:
                target = os.fsdecode(handle.read().strip())
        except OSError:
            continue
        if target and os.path.exists(os.path.join(entry, target)):
            return True
    return False


def _configured_hooks_path(repo: str) -> Optional[str]:
    """core.hooksPath as the user's own git commands see it (run_git_cmd overrides it on
    its command line, so that scope is skipped), relative to the work tree; None when unset."""
    code, out, _ = run_git_cmd(["git", "config", "--show-scope", "-z", "--type=path", "--get-all", "core.hooksPath"],
                               cwd=repo, binary=True)
    if code != 0:
        return None
    fields = out.split(b"\0")
    values = [os.fsdecode(fields[index + 1]) for index in range(0, len(fields) - 1, 2)
              if fields[index] != b"command" and fields[index + 1]]
    return values[-1] if values else None


def _makewand_source_root() -> Path:
    """The directory holding the makewand package (a source checkout's root).

    The one source root the test-harness guard judges against: every writing entry
    point (host transaction, pipeline, race, DAG, apply) asks
    harness_source_tree_conflict, which reads only this function, so a test stands a
    fixture checkout in for the source tree by replacing it here and nowhere else."""
    return Path(__file__).resolve().parent.parent


def harness_source_tree_conflict(path: Union[str, Path], action: str = "执行会修改文件的任务") -> Optional[str]:
    """The test-harness guard, the one definition for every caller: under the test
    harness (MAKEWAND_TEST_ISOLATION_ROOT set) makewand never writes to, snapshots or
    rolls back its own source tree. A host transaction snapshots everything below its
    root and rolls it back, a shadow delivery publishes into it and an applied
    candidate writes into it: a test that forgot its temporary workspace would revert
    edits and delete files that other sessions create meanwhile in the checkout under
    test. path, and the root of the git repository holding it, conflict when they are
    the source tree (_makewand_source_root), lie inside it or contain it.

    Returns None, or why the target is refused, naming action (what the caller would
    do, e.g. "建立宿主模式事务"). Outside the harness makewand may work on its own
    repository like on any other."""
    if not os.environ.get("MAKEWAND_TEST_ISOLATION_ROOT"):
        return None
    source = Path(os.path.realpath(_makewand_source_root()))
    roots = [Path(os.path.realpath(path))]
    git_root = find_git_root(roots[0])
    if git_root:
        roots.append(Path(os.path.realpath(git_root)))
    for root in roots:
        if root == source or source in root.parents or root in source.parents:
            return (f"测试环境中拒绝{action}: 目标工作区 {root} 与 makewand 源码目录 {source} 重叠，"
                    "测试必须以临时仓库作为工作区")
    return None


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
        self.tracked_modes: Dict[str, str] = {}
        self.backup_dir: Optional[Path] = None
        self.backups: Dict[str, Tuple[Path, str]] = {}
        self.unbacked: Dict[str, str] = {}
        # Task-start sha256 of files that are not backed up (_fingerprint_unbacked_files),
        # sampled fingerprints of the largest ones, and why a file's content could not be
        # compared with a task-start digest (none was taken, or a check ran out of budget).
        self.fingerprints: Dict[str, str] = {}
        self.samples: Dict[str, str] = {}
        self.digest_gaps: Dict[str, str] = {}
        self.pre_git_markers: Dict[str, FsEntry] = {}
        # The hooks directories of this checkout and its submodules (_hook_directories)
        # and their task-start entries {label: (directory, name, entry)}.
        self.hook_dirs: List[_HookDirectory] = []
        self.pre_git_hooks: Optional[Dict[str, Tuple[_HookDirectory, str, FsEntry]]] = None
        self.pre_git_hook_digests: Dict[str, str] = {}
        self._reported_shared_hooks: set = set()
        self.sub_baselines: Dict[str, str] = {}
        self.state = "new"
        self.succeeded: Optional[bool] = None
        # A delivery hard block (finalize_success) is UNVERIFIED, not FAILED: the
        # reviewed change was rolled back and archived, never judged defective.
        self.outcome_status: Optional[str] = None
        self.failure_reason: Optional[str] = None
        # Pre-existing files the delivery check found changed that no backup covers, and
        # those whose content it could not confirm (only ctime or inode moved, no digest);
        # what the last rollback verification could not restore, or could not verify.
        self.unrestorable: set = set()
        self.unconfirmed: set = set()
        self.unrestored: List[str] = []
        self.unverified: List[str] = []
        self._exec_bits: Optional[bool] = None
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
        conflict = harness_source_tree_conflict(self.cwd, "建立宿主模式事务")
        if conflict:
            return _printable(conflict)
        if self.is_git_repo:
            code, top, err = run_git_cmd(["git", "rev-parse", "--show-toplevel"], cwd=self.cwd)
            if code != 0 or not top.strip():
                return _printable(f"无法定位 Git 仓库根目录 (rc={code}): {(err or '').strip()[:200]}")
            self.root = os.path.realpath(top.strip())
            conflict = harness_source_tree_conflict(self.root, "建立宿主模式事务")
            if conflict:
                return _printable(conflict)
            self._discard_stale_ephemeral_git()
        if not self.is_git_repo:
            self.root = self.cwd
            if Path(self.root) in _refused_init_roots():
                return _printable(f"拒绝在系统目录或用户主目录 ({self.root}) 中执行会修改文件的任务，请进入具体项目目录")
        conflict = harness_source_tree_conflict(self.root, "建立宿主模式事务")
        if conflict:
            return _printable(conflict)
        root_prefix = self.root.rstrip(os.sep) + os.sep
        for state_path in sorted(_makewand_state_paths()):
            if state_path == self.root or state_path.startswith(root_prefix):
                return _printable(f"makewand 状态目录 {state_path} 位于工作区内，不能在宿主模式下安全回滚")
        try:
            home = Path.home().resolve()
            if home == Path(self.root) or Path(self.root) in home.parents:
                # The user's home holds provider credentials and session state that
                # change during a task; never snapshot/roll back it in place.
                return _printable(f"工作区 {self.root} 包含用户主目录，不能在宿主模式下安全回滚")
        except (OSError, RuntimeError):
            pass
        try:
            markers: Dict[str, FsEntry] = {}
            self.pre = scan_workspace_tree(self.root, strict=True, git_markers=markers)
            self.pre_git_markers = markers
        except SnapshotError as exc:
            return _printable(f"无法建立任务前完整快照 ({exc})")
        if self.is_git_repo:
            self.hook_dirs = self._hook_directories()
            if self.hook_dirs:
                self.pre_git_hooks = self._git_hook_entries(self.pre_git_hook_digests)
        self.state = "snapshotted"
        return None

    def _hook_directories(self) -> List[_HookDirectory]:
        """The directories git runs hooks from for this checkout and its checked-out
        submodules: each repository's default <git-common-dir>/hooks and a core.hooksPath
        from the user's configuration that lies outside the scanned work tree.

        run_git_cmd itself forces core.hooksPath=/dev/null, but the user's own git
        commands run whatever is placed there. Such a directory is shared when other
        checkouts use it as well - every checkout of a repository that has linked
        worktrees (pre-commit install or git lfs install run in any of them writes
        there), or a hooks directory outside the repository (a global core.hooksPath)
        - and its changes cannot be attributed to this task.
        """
        repos = [self.root]
        if (Path(self.root) / ".gitmodules").exists():
            try:
                repos += [str(Path(self.root) / rel) for rel in get_submodule_paths(self.root)
                          if (Path(self.root) / rel / ".git").exists()]
            except OSError:
                pass
        directories: Dict[str, _HookDirectory] = {}
        for repo in repos:
            code, out, _ = run_git_cmd(["git", "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir"],
                                       cwd=repo)
            lines = [line.strip() for line in out.splitlines() if line.strip()] if code == 0 else []
            if len(lines) != 2:
                continue
            git_dir, common_dir = (os.path.realpath(line) for line in lines)
            linked = git_dir != common_dir or _has_linked_worktrees(common_dir)
            candidates = [os.path.join(common_dir, "hooks")]
            configured = _configured_hooks_path(repo)
            if configured:
                candidates.append(os.path.join(repo, configured))  # relative: to the work tree root
            for candidate in candidates:
                path = os.path.realpath(candidate)
                relative = os.path.relpath(path, self.root).split(os.sep)
                if path in directories or (relative[0] != os.pardir and ".git" not in relative):
                    continue  # already watched, or part of the scanned work tree
                if git_dir != common_dir and _is_within(path, git_dir):
                    directories[path] = _HookDirectory(path, False, None)  # this checkout's own
                elif _is_within(path, common_dir):
                    directories[path] = _HookDirectory(path, linked, common_dir)
                else:
                    directories[path] = _HookDirectory(path, True, None)
        return list(directories.values())

    def _hook_shared(self, directory: _HookDirectory) -> bool:
        """Whether other checkouts use directory now (a linked worktree added meanwhile counts)."""
        return directory.shared or (directory.common_dir is not None and _has_linked_worktrees(directory.common_dir))

    def _hook_label(self, directory: str, name: str) -> str:
        path = os.path.join(directory, name)
        relative = os.path.relpath(path, self.root)
        return path if relative.split(os.sep, 1)[0] == os.pardir else relative.replace(os.sep, "/")

    def _git_hook_entries(self, digests: Optional[Dict[str, str]] = None
                          ) -> Optional[Dict[str, Tuple[_HookDirectory, str, FsEntry]]]:
        """The entries of the watched hooks directories, which git runs without any review
        and which live outside the scanned work tree, keyed by display path; digests,
        when given, receives the sha256 of each regular file. *.sample files are left out:
        git never runs them, and git init (re)creates them from its template. (The
        config is shared with every linked worktree and legitimately rewritten by
        concurrent git commands, so it is not compared.) None when no hooks directory
        could be located: the check is then skipped."""
        if not self.hook_dirs:
            return None
        entries: Dict[str, Tuple[_HookDirectory, str, FsEntry]] = {}
        for directory in self.hook_dirs:
            try:
                with os.scandir(directory.path) as iterator:
                    children = list(iterator)
            except OSError:
                continue
            for child in children:
                if child.name.endswith(".sample"):
                    continue
                try:
                    entry = _fs_entry(child.path, os.lstat(child.path))
                except OSError:
                    continue
                label = self._hook_label(directory.path, child.name)
                entries[label] = (directory, child.name, entry)
                if digests is not None and entry.kind == "file" and entry.size <= DEFAULT_BACKUP_FILE_LIMIT:
                    digest = _file_sha256_at(directory.path, child.name)
                    if digest is not None:
                        digests[label] = digest
        return entries

    def _changed_git_hooks(self) -> Tuple[List[str], List[str], Dict[str, Tuple[_HookDirectory, str, FsEntry]]]:
        """Hooks added, removed, or changed in type, mode or content since the task
        started: (those in directories only this checkout uses, those in shared
        directories, the current entries)."""
        if self.pre_git_hooks is None:
            return [], [], {}
        now = self._git_hook_entries() or {}
        before = self.pre_git_hooks
        private: List[str] = []
        shared: List[str] = []
        for label in sorted(set(now) | set(before)):
            directory, name, _ = now.get(label) or before[label]
            if label in now and label in before:
                def content_changed(path=directory.path, name=name, digest=self.pre_git_hook_digests.get(label)):
                    return None if digest is None else _file_sha256_at(path, name) != digest

                if _changed_since_snapshot(now[label][2], before[label][2], content_changed) is False:
                    continue
            (shared if self._hook_shared(directory) else private).append(label)
        return private, shared, now

    def _note_shared_hooks(self, labels: List[str], now: Dict[str, Tuple[_HookDirectory, str, FsEntry]],
                           notes: List[str]) -> None:
        """Changes in a shared hooks directory are reported once and never acted on."""
        fresh = [label for label in labels if label not in self._reported_shared_hooks]
        if not fresh:
            return
        self._reported_shared_hooks.update(fresh)
        directories = sorted({(now.get(label) or self.pre_git_hooks[label])[0].path for label in fresh})
        notes.append(f"多个检出或仓库共用的 Git hooks 目录 {_summarize_paths(directories, 3)} 在任务期间发生变化"
                     f"（可能来自其他检出，未计入本次交付核验，也未移动或删除），请人工确认: {_summarize_paths(fresh)}")

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
                print(c(_printable(f"[Makewand Git] 已移除上次中断遗留的临时 .git ({self.root})"), COLOR_YELLOW))
                self.is_git_repo = find_git_root(self.cwd) is not None
                if self.is_git_repo:
                    code, top, _ = run_git_cmd(["git", "rev-parse", "--show-toplevel"], cwd=self.cwd)
                    self.root = os.path.realpath(top.strip()) if code == 0 and top.strip() else self.root
        else:
            print(c(_printable(f"[Makewand Git] {self.root}/.git 带有 makewand 临时标记但已有其他提交，按普通仓库处理"),
                    COLOR_YELLOW))

    # -- phase 2: immediately before the first model dispatch -------------
    def begin(self) -> Optional[str]:
        if self.state != "snapshotted":
            return "内部错误: 尚未完成任务前快照"
        try:
            if not self.is_git_repo:
                created, error = init_git_baseline(self.root, ephemeral=True)
                if error or not created:
                    return _printable(f"无法为非 Git 目录建立基线，已在派发任何模型前中止，未改动任何文件: {error or '未创建仓库'}")
                self.created_git = True
            error = self._capture_git_baseline() or self._backup_untracked_files()
            if not error:
                self.sub_baselines = self._capture_submodule_baselines()
        except Exception as exc:  # noqa: BLE001 - reported, then aborted
            error = f"建立任务基线时发生异常: {type(exc).__name__}: {exc}"
        if error:
            self._abort_begin()
            return _printable(error)
        self.state = "active"
        return None

    def _abort_begin(self) -> None:
        self.state = "aborted"
        if self.created_git:
            removal_error = _remove_git_dir(self.git_dir)
            if removal_error:
                self.keep_git = True
                print(c(_printable(f"⚠ [Makewand Transaction] {removal_error}"), COLOR_RED), file=sys.stderr)
        self._discard_backups()

    def _capture_git_baseline(self) -> Optional[str]:
        code, out, err = run_git_cmd(["git", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames"],
                                     cwd=self.root, binary=True)
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
        code, listed, err = run_git_cmd(["git", "ls-files", "-z", "--cached", "--stage"], cwd=self.root, binary=True)
        if code != 0:
            return f"git ls-files 失败 (rc={code})，无法确认被跟踪文件: {os.fsdecode(err or b'').strip()[:200]}"
        # "<mode> <object> <stage>\t<path>": the index mode of each tracked path.
        for record in listed.split(b"\0"):
            meta, tab, name = record.partition(b"\t")
            if tab and name:
                self.tracked_modes[os.fsdecode(name)] = meta.split(b" ", 1)[0].decode("ascii", "replace")
        self.tracked = set(self.tracked_modes)
        return None

    def _capture_submodule_baselines(self) -> Dict[str, str]:
        """Task-start commit of each checked-out submodule (root-relative paths): the
        baseline its review diff and its rejected-change archive are taken against."""
        baselines: Dict[str, str] = {}
        if not (Path(self.root) / ".gitmodules").exists():
            return baselines
        try:
            paths = get_submodule_paths(self.root)
        except OSError:
            return baselines
        for rel in paths:
            directory = Path(self.root) / rel
            if (directory / ".git").exists():
                code, head, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(directory))
                if code == 0 and head.strip():
                    baselines[rel] = head.strip()
        return baselines

    def _submodule_repos(self, discover_from: str) -> List[Tuple[str, str, str]]:
        """(root-relative path, directory, task-start commit) of each checked-out
        submodule listed in discover_from/.gitmodules, found the way
        get_git_diff_status() finds them. Raises OSError when they cannot be listed."""
        if not (Path(discover_from) / ".gitmodules").exists():
            return []
        repos = []
        for sub_rel in get_submodule_paths(discover_from):
            directory = Path(discover_from) / sub_rel
            if not (directory.exists() and (directory / ".git").exists()):
                continue
            rel = os.path.relpath(str(directory), self.root).replace(os.sep, "/")
            if rel == "." or rel.startswith("../"):
                continue
            base = self.sub_baselines.get(rel)
            if not base:
                code, out, _ = run_git_cmd(["git", "rev-parse", f"{self.baseline_commit or 'HEAD'}:{rel}"], cwd=self.root)
                base = out.strip() if code == 0 and out.strip() else "HEAD"
            repos.append((rel, str(directory), base))
        return repos

    def _backup_untracked_files(self) -> Optional[str]:
        per_file = _positive_int_env("MAKEWAND_BACKUP_FILE_LIMIT", DEFAULT_BACKUP_FILE_LIMIT)
        total_limit = _positive_int_env("MAKEWAND_BACKUP_TOTAL_LIMIT", DEFAULT_BACKUP_TOTAL_LIMIT)
        total = 0
        index = []

        # Collect git-ignored directories under root
        ignored_dirs = set()
        code, ig_out, _ = run_git_cmd(["git", "ls-files", "-z", "--others", "-i", "--exclude-standard", "--directory"], cwd=self.root, binary=True)
        if code == 0 and ig_out:
            for item in ig_out.split(b"\0"):
                if item:
                    decoded = os.fsdecode(item)
                    if decoded.endswith("/") and decoded != "/":
                        ignored_dirs.add(decoded.rstrip("/"))

        def _is_dir_ignored(rel_path: str) -> bool:
            return rel_path in ignored_dirs or any(d and rel_path.startswith(d + "/") for d in ignored_dirs)

        for rel, entry in sorted(self.pre.items()):
            if entry.kind != "file" or (rel in self.tracked and rel not in self.dirty_tracked):
                continue
            parent_parts = rel.split("/")[:-1]
            if any(part in PURE_DEPENDENCY_OR_CACHE_DIRS for part in parent_parts):
                self.unbacked[rel] = "位于依赖/缓存大目录，只记录元数据"
                continue
            if any(part in BUILD_OR_ENV_DIRS for part in parent_parts):
                dir_ignored = False
                cum = []
                for p in parent_parts:
                    cum.append(p)
                    cum_path = "/".join(cum)
                    if p in BUILD_OR_ENV_DIRS and _is_dir_ignored(cum_path):
                        dir_ignored = True
                        break
                if dir_ignored:
                    self.unbacked[rel] = "位于被忽略的构建大目录，只记录元数据"
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
            write_private_file(self.backup_dir / "index.json", _json_text(
                {"root": self.root, "files": index, "not_backed_up": self.unbacked}))
        self._fingerprint_unbacked_files()
        return None

    def _fingerprint_unbacked_files(self) -> None:
        """Records the sha256 of pre-existing files that are not backed up (dependency
        and cache directories, files over the backup limits), smallest first, within
        MAKEWAND_FINGERPRINT_FILE_LIMIT / _TOTAL_LIMIT / _SECONDS, and then a sampled
        fingerprint of every file that got no digest - over the file limit, past the
        total or time budget, or unreadable - within a budget of its own (_SampleBudget)
        that the digests cannot use up, taken in turns across file sizes
        (_fair_sample_order) so that many smaller files cannot crowd the largest ones
        out of it. Rollback cannot restore these files, but the
        delivery check and the rollback verification can tell an in-place rewrite that
        kept size and mtime (utime()) from a metadata-only change (chmod to the same
        mode, a hard link added elsewhere) by content. Why a file has no digest is kept
        for the messages (self.digest_gaps)."""
        budget = _DigestBudget()
        selected: List[str] = []
        for rel in sorted(self.unbacked, key=lambda path: (self.pre[path].size, path)):
            reason = budget.over_limit(self.pre[rel].size)
            if reason is None:
                selected.append(rel)
            else:
                self.digest_gaps[rel] = reason
        self.fingerprints.update(self._hash_pre_files(selected, budget, sampled=False))
        samples = _SampleBudget()
        undigested = {rel: self.pre[rel].size for rel in self.unbacked if rel not in self.fingerprints}
        chosen = [rel for rel in _fair_sample_order(undigested) if samples.over_limit(undigested[rel]) is None]
        self.samples.update(self._hash_pre_files(chosen, samples, sampled=True))

    def _hash_pre_files(self, paths: List[str], budget: _DigestBudget, sampled: bool) -> Dict[str, str]:
        """{path: sha256} of the pre-existing files paths (sampled: their sampled
        fingerprints) that are still exactly as the snapshot recorded them, until budget's
        time runs out. They are read in the order given - the order in which the caller's
        size budget admitted them: smallest first for digests, _fair_sample_order for
        samples - so that running out of time drops the same files running out of bytes
        would, never a large file only because its path sorts late. Each is read through
        a no-follow handle on its directory; the most recently used directory handles
        stay open. Why a full digest is missing goes to self.digest_gaps."""
        hashes: Dict[str, str] = {}
        parents: Dict[str, Optional[int]] = {}  # least recently used first
        try:
            for index, rel in enumerate(paths):
                expired = budget.expired()
                if expired:
                    if not sampled:
                        self.digest_gaps.update((rest, expired) for rest in paths[index:])
                    break
                parent, _, name = rel.rpartition("/")
                if parent in parents:
                    parent_fd = parents[parent] = parents.pop(parent)
                else:
                    try:
                        parent_fd = _open_parent(self.root, rel)[0]
                    except OSError:
                        parent_fd = None
                    parents[parent] = parent_fd
                    if len(parents) > _HASH_DIRECTORY_HANDLES:
                        evicted = parents.pop(next(iter(parents)))
                        if evicted is not None:
                            os.close(evicted)
                digest = None if parent_fd is None else _entry_digest(parent_fd, name, self.pre[rel], sampled=sampled)
                if digest is not None:
                    hashes[rel] = digest
                elif not sampled:
                    self.digest_gaps[rel] = "任务开始时无法读取，或读取期间发生变化"
        finally:
            for parent_fd in parents.values():
                if parent_fd is not None:
                    os.close(parent_fd)
        return hashes

    def _discard_backups(self) -> None:
        if self.backup_dir is not None and self.backup_dir.exists():
            errors = _rmtree_collect(self.backup_dir)
            if errors:
                print(c(_printable(f"⚠ [Makewand Transaction] 无法删除临时备份 {self.backup_dir}: {errors[0]}"), COLOR_YELLOW),
                      file=sys.stderr)
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
        scan_errors: List[str] = []
        markers: Dict[str, FsEntry] = {}
        current = scan_workspace_tree(root, strict=False, errors=scan_errors, git_markers=markers)
        problems.extend(f"回滚扫描: {msg}" for msg in scan_errors)
        new_repos = sorted(rel for rel in markers if rel != ".git" and rel not in self.pre_git_markers)
        archived = self._archive_rejected_changes(reason, notes, new_repos)
        # First of all: git runs a hook without review, even when the rollback stops early.
        self._quarantine_new_git_hooks(problems, notes)

        # Before any git reset: git deletes intent-to-add/indexed paths that are not
        # in the baseline, so relocated pre-existing files are put back first and
        # pre-existing untracked files the task added to the index are unstaged.
        self._recover_relocated(current, problems, notes)
        if not archived:
            # A reviewable new file is never deleted unless it is archived.
            unsaved = self._quarantine_unarchived_new_files(notes)
            if unsaved:
                problems.append(f"无法存档本次任务新建的文件 {_summarize_paths(unsaved)}；为避免丢失改动，未删除或恢复任何文件")
                return

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

        markers = {}
        current = scan_workspace_tree(root, strict=False, git_markers=markers)
        self._delete_new_git_markers(markers, problems)
        self._delete_new_paths(current, problems)
        self._restore_pre_entries(problems, current)
        self._purge_rewritten_bytecode(problems, notes)
        self._verify(problems, notes)

    def _quarantine_new_git_hooks(self, problems: List[str], notes: List[str]) -> None:
        """Hooks added to a hooks directory only this checkout uses (its own, or a
        submodule's) while the task ran are moved into the rejected-changes folder -
        restorable and reported, never deleted: git runs them without review, but they
        may also be the user's own (pre-commit install, git lfs install in another
        terminal). A hooks directory that other checkouts share is left alone and only
        reported. A changed or deleted pre-existing hook cannot be restored here;
        _verify() reports it."""
        private, shared, now = self._changed_git_hooks()
        self._note_shared_hooks(shared, now, notes)
        added = [label for label in private if label not in self.pre_git_hooks and label in now]
        folder = self._rejected_artifacts() / "quarantine" / "git-hooks" if added else None
        for label in added:
            directory, name, _ = now[label]
            source = os.path.join(directory.path, name)
            try:
                destination = _move_aside(source, folder)
            except FileNotFoundError:
                continue
            except OSError as exc:
                problems.append(_printable(f"无法隔离任务期间新增的 Git hook {label}: {exc.strerror or exc}"))
                continue
            notes.append(_printable(f"任务期间新增的 Git hook {label} 已移出隔离保存于 {destination}")
                         + f"（如为你本人安装，移回即可: mv {_shell_quote(destination)} {_shell_quote(source)}）")

    def _purge_rewritten_bytecode(self, problems: List[str], notes: List[str]) -> None:
        """Bytecode in __pycache__ that existed before the task and that the task
        rewrote (or may have: undecided counts) has no backup; it is removed rather than
        left (an unchecked-hash .pyc is used without looking at its source), and Python
        regenerates it."""
        purged = []
        for rel, entry in sorted(self.pre.items()):
            if entry.kind != "file" or _bytecode_source(rel) is None or not _is_regenerable_cache(rel, self.root):
                continue
            current = _lstat_at(self.root, rel)
            if current is None or self._changed_since_task_start(rel, current) is False:
                continue
            try:
                _unlink_at(self.root, rel)
                purged.append(rel)
            except OSError as exc:
                problems.append(f"无法删除任务改写的字节码缓存 {rel}: {exc.strerror or exc}")
        if purged:
            notes.append(f"已删除任务改写、无法恢复的字节码缓存（Python 会按源码重新生成）: {_summarize_paths(purged)}")

    def _archive_rejected_changes(self, reason: str, notes: List[str], new_repos: Optional[List[str]] = None) -> bool:
        """Saves the rejected change before rollback touches anything.

        rejected.patch holds every reviewable change of the repository against the
        task baseline, built in the same private intent-to-add index as the review
        diff: tracked edits, commits made by the task and new untracked files git
        does not ignore. Each checked-out submodule gets its own patch. Returns
        False when a change may be missing, so rollback preserves new files instead:
        so does every repository the task created (new_repos), whose files a patch
        of the enclosing repository records only as an embedded commit.
        """
        complete = True
        if new_repos:
            complete = False
            notes.append(f"本次任务新建的嵌套 Git 仓库中的文件无法写入补丁: {_summarize_paths(new_repos)}")
        repos = [("", self.root, self.baseline_commit)]
        try:
            repos += self._submodule_repos(self.root)
        except OSError as exc:
            complete = False
            notes.append(f"无法列出子模块，子模块中的改动未能存档: {exc}")
        folder = self._rejected_artifacts()
        saved: Dict[str, str] = {}
        for index, (rel, repo, base) in enumerate(repos):
            name = f"rejected.submodule-{index}.patch" if rel else "rejected.patch"
            target = folder / name
            written, whole, detail = _export_workspace_patch(repo, base, target)
            if not written:
                # e.g. a new file git cannot read: keep at least tracked edits and task commits.
                written = _export_workspace_patch(repo, base, target, include_untracked=False)[0]
            if not whole:
                complete = False
                notes.append(f"无法完整导出被拒改动补丁{f'（子模块 {rel}）' if rel else ''}: {detail}")
            try:
                if written and os.lstat(target).st_size:
                    os.chmod(target, 0o600)
                    saved[rel] = name
                else:
                    target.unlink()
            except FileNotFoundError:
                pass
        if not saved:
            _rmtree_collect(folder)
            self._rejected_dir = None
            return complete
        write_private_file(folder / "manifest.json", _json_text({
            "repo_root": self.root, "baseline_commit": self.baseline_commit, "reason": reason,
            "includes_untracked_files": True, "complete": complete,
            "patches": {rel or ".": name for rel, name in saved.items()},
            "submodule_baselines": {rel: base for rel, _, base in repos if rel},
        }))
        notes.append(f"被拒改动已存档: {folder / saved[''] if '' in saved else folder}")
        return complete

    def _quarantine_unarchived_new_files(self, notes: List[str]) -> List[str]:
        """Archive fallback: new regular files that git does not ignore are moved out of
        the workspace into the rejected-changes folder instead of being deleted.
        Returns the files that could not be preserved."""
        code, listed, _ = run_git_cmd(["git", "ls-files", "-z", "--others", "--ignored", "--exclude-standard"],
                                      cwd=self.root, binary=True)
        ignored = {os.fsdecode(name) for name in listed.split(b"\0") if name} if code == 0 else set()
        moved: Dict[str, str] = {}
        unsaved: List[str] = []
        for rel, entry in sorted(scan_workspace_tree(self.root, strict=False).items()):
            if rel in self.pre or entry.kind != "file" or rel in ignored:
                continue
            try:
                moved[self._quarantine(rel).name] = rel
            except OSError:
                unsaved.append(rel)
        if moved:
            folder = self._rejected_artifacts() / "quarantine"
            write_private_file(folder / f"index-{os.getpid()}-{time.monotonic_ns()}.json", _json_text(moved))
            notes.append(f"本次任务新建的 {len(moved)} 个文件未能写入补丁，已移出工作区保存: {folder}")
        return unsaved

    def _delete_new_git_markers(self, markers: Dict[str, FsEntry], problems: List[str]) -> None:
        """Removes nested .git entries the task created; the root's and pre-existing ones stay."""
        for rel in sorted(markers, key=lambda p: (p.count("/"), p), reverse=True):
            if rel == ".git" or rel in self.pre_git_markers:
                continue
            try:
                if markers[rel].kind == "dir":
                    errors = _rmtree_collect(Path(self.root, rel))
                    if errors:
                        problems.append(f"无法删除本次任务新建的 Git 元数据 {rel}: {errors[0]}")
                else:
                    _unlink_at(self.root, rel)
            except FileNotFoundError:
                continue
            except OSError as exc:
                problems.append(f"无法删除本次任务新建的 Git 元数据 {rel}: {exc.strerror or exc}")

    def _untrack_preexisting_paths(self) -> Optional[str]:
        """Unstages pre-existing untracked/ignored files the task added to the index."""
        code, listed, err = run_git_cmd(["git", "ls-files", "-z", "--cached"], cwd=self.root, binary=True)
        if code != 0:
            return f"无法读取当前暂存区 (rc={code})"
        indexed = {os.fsdecode(name) for name in listed.split(b"\0") if name}
        paths = sorted(rel for rel in indexed if rel in self.pre and rel not in self.tracked)
        if not paths:
            return None
        # Literal pathspecs: a file named "[ab].txt" must not also match a.txt and b.txt.
        code, _, err = run_git_cmd(["git", "--literal-pathspecs", "rm", "-r", "-q", "--cached", "--ignore-unmatch",
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

    def _restore_pre_entries(self, problems: List[str], scanned: Optional[Dict[str, FsEntry]] = None) -> None:
        """Puts every pre-existing path back. scanned, a scan taken after the git reset,
        lets entries that are still exactly as the snapshot recorded them be skipped
        without another walk down their path; anything else is re-read no-follow first."""
        root = self.root
        scanned = scanned or {}
        # Directories first (shallow to deep) so files and links have their parents.
        for rel, entry in sorted(((r, e) for r, e in self.pre.items() if e.kind == "dir"), key=lambda item: item[0].count("/")):
            seen = scanned.get(rel)
            if seen is not None and seen.kind == "dir" and seen.mode == entry.mode:
                continue
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
            if entry.kind == "dir":
                continue
            seen = scanned.get(rel)
            if seen is not None and (_unchanged_since_snapshot(seen, entry) or (
                    self._git_restores(rel) and seen.kind == entry.kind == "file" and seen.mode == entry.mode)):
                continue
            if self._git_restores(rel):
                # git leaves modes alone under core.fileMode=false: put the task-start mode back.
                self._restore_mode(rel, entry, problems)
                continue
            current = _lstat_at(root, rel)
            # A backed-up file is restored unless it is provably untouched: matching
            # size and mtime alone can be faked with utime().
            if _same_entry(current, entry) and (rel not in self.backups or _unchanged_since_snapshot(current, entry)):
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
            # Literal pathspecs: a glob-like file name must never restore (and overwrite) other files.
            code, _, err = run_git_cmd(["git", "--literal-pathspecs", "checkout", self.baseline_commit,
                                        "--pathspec-from-file=-", "--pathspec-file-nul"],
                                       cwd=root, input_data=b"\0".join(os.fsencode(p) for p in checkout_paths))
            if code != 0:
                problems.append(f"无法从基线提交恢复被跟踪文件 (rc={code}: {os.fsdecode(err or b'').strip()[:160]})")

    def _restore_mode(self, rel: str, entry: FsEntry, problems: List[str]) -> None:
        current = _lstat_at(self.root, rel)
        if entry.kind != "file" or current is None or current.kind != "file" or current.mode == entry.mode:
            return
        try:
            fd, name = _open_parent(self.root, rel)
            try:
                os.chmod(name, entry.mode, dir_fd=fd)
            finally:
                os.close(fd)
        except OSError as exc:
            problems.append(f"无法恢复 {rel} 的文件权限: {exc.strerror or exc}")

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

    def _verify(self, problems: List[str], notes: Optional[List[str]] = None) -> None:
        root = self.root
        scan_errors: List[str] = []
        markers: Dict[str, FsEntry] = {}
        current = scan_workspace_tree(root, strict=False, errors=scan_errors, git_markers=markers)
        problems.extend(f"核验扫描: {msg}" for msg in scan_errors)
        leftovers = sorted(rel for rel in current if rel not in self.pre)
        if leftovers:
            problems.append(f"本次任务新建的路径未能清除: {_summarize_paths(leftovers)}")
        stray_git = self._changed_git_markers(markers)
        if stray_git:
            problems.append(f"嵌套的 Git 元数据未能恢复到任务前状态（新建的未能清除，或任务前已存在的被替换、删除）: {_summarize_paths(stray_git)}")
        private_hooks, _, _ = self._changed_git_hooks()
        if private_hooks:
            problems.append(f"仓库自身的 Git hooks 被改动且无法自动恢复，请人工检查: {_summarize_paths(private_hooks)}")
        missing, changed, caches, unverified = [], [], [], []
        budget, samples = _DigestBudget(), _SampleBudget()
        for rel, entry in sorted(self.pre.items()):
            now = current.get(rel)
            if now is None:
                found = missing
            elif now.kind != entry.kind:
                found = changed
            elif entry.kind == "file" and not self._git_restores(rel) and not (rel in self.tracked and rel not in self.dirty_tracked):
                if rel in self.backups:
                    # The untouched original (same inode and ctime) or the copy restored from the backup.
                    if _unchanged_since_snapshot(now, entry) or (
                            _same_entry(now, entry) and _file_sha256_at(root, rel) == self.backups[rel][1]):
                        continue
                    found = changed
                else:
                    # Nothing restores a file without a backup. Its content decides - re-hashed
                    # only when its stat moved, within the fingerprinting limits - or the delivery
                    # check's finding that it changed (self.unrestorable).
                    state = True if rel in self.unrestorable else self._changed_since_task_start(rel, now, budget, samples)
                    if state is False:
                        continue
                    found = changed if state else unverified
            elif entry.kind == "link" and now.link != entry.link:
                found = changed
            else:
                continue
            (caches if _is_regenerable_cache(rel, root) else found).append(rel)
        self.unrestored = [f"{p}（未备份: {self.unbacked[p]}）" if p in self.unbacked else p for p in missing + changed]
        self.unverified = [f"{p}（{self.digest_gaps[p]}）" if p in self.digest_gaps else p for p in unverified]
        for label, paths in (("任务前已存在但现已丢失", missing), ("任务前已存在但未能恢复原内容", changed)):
            if paths:
                detail = [f"{p}（{self.unbacked[p]}）" if p in self.unbacked else p for p in paths]
                problems.append(f"{label}: {_summarize_paths(detail)}")
        if self.unverified and notes is not None:
            notes.append("以下任务前已存在、没有备份的文件在任务期间只有 ctime 或 inode 发生变化，没有可比对的内容摘要"
                         "（或超出摘要预算），无法核实其内容是否被改动；调高 MAKEWAND_FINGERPRINT_FILE_LIMIT / "
                         f"MAKEWAND_FINGERPRINT_TOTAL_LIMIT / MAKEWAND_FINGERPRINT_SECONDS 可在下次任务开始时为其计算摘要: "
                         f"{_summarize_paths(self.unverified)}")
        if caches and notes is not None:
            notes.append(f"可再生的缓存文件未恢复（相应工具会自动重建）: {_summarize_paths(caches)}")
        if self.baseline_commit:
            # Without rename pairs every NUL-separated entry is "XY <path>" (status.renames).
            code, status_out, err = run_git_cmd(["git", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames"],
                                                cwd=root, binary=True)
            if code != 0:
                problems.append(f"无法核验 git 状态 (rc={code})")
            else:
                # An untracked nested repository is listed as its directory ("tools/lib/").
                dirty = sorted({os.fsdecode(t[3:]).rstrip("/") for t in status_out.split(b"\0") if len(t) > 3})
                expected = self.dirty_tracked | {rel for rel in self.pre if rel not in self.tracked}
                unexpected = [p for p in dirty if p not in expected]
                if unexpected:
                    problems.append(f"被跟踪文件未能恢复到基线: {_summarize_paths(unexpected)}")
            code, head, _ = run_git_cmd(["git", "rev-parse", "--verify", "-q", "HEAD"], cwd=root)
            if code != 0 or head.strip() != self.baseline_commit:
                problems.append("HEAD 未能回到任务基线提交")

    def _changed_since_task_start(self, rel: str, current: Optional[FsEntry], budget: Optional[_DigestBudget] = None,
                                  samples: Optional[_SampleBudget] = None) -> Optional[bool]:
        """Whether the task changed pre-existing rel in type, mode or content
        (_changed_since_snapshot), its content judged by _content_changed. None when only
        its change time or inode moved and nothing decides."""
        return _changed_since_snapshot(current, self.pre[rel], lambda: self._content_changed(rel, current, budget, samples))

    def _content_changed(self, rel: str, current: Optional[FsEntry] = None, budget: Optional[_DigestBudget] = None,
                         samples: Optional[_SampleBudget] = None) -> Optional[bool]:
        """Whether pre-existing rel's bytes differ from the task start's: compared with its
        backup's or fingerprint's digest, or else with its sampled fingerprint, which
        decides on its own only where it covers the whole file. None when nothing decides:
        there is no digest or sample, or the phase's budget for it (budget for digests,
        samples for sampled fingerprints) is used up.

        A matching partial sample leaves room for a patch between its chunks, so it only
        corroborates what current's stat change shows: the same inode with a changed link
        count and nothing else but ctime moved is a hard link added elsewhere (uv and pnpm
        link cached packages into every environment that uses them), no edit. Without such
        corroboration - no digest or sample, or a digest that could not be checked - a
        changed link count proves nothing: a file can be patched behind an added link."""
        entry = self.pre[rel]
        digest = self.backups[rel][1] if rel in self.backups else self.fingerprints.get(rel)
        if digest is not None:
            refused = budget and (budget.expired() or budget.over_limit(entry.size))
            if refused:
                self.digest_gaps.setdefault(rel, f"核对时{refused}")
                return None
            return _file_sha256_at(self.root, rel) != digest
        sample = self.samples.get(rel)
        if sample is None:
            return None
        refused = samples and (samples.expired() or samples.over_limit(entry.size))
        if refused:
            self.digest_gaps.setdefault(rel, f"核对时{refused}")
            return None
        if _file_sha256_at(self.root, rel, sampled=True) != sample:
            return True
        if entry.size <= FINGERPRINT_SAMPLE_BYTES:
            return False  # the sample holds every byte of the file
        unmoved = current is not None and ((current.dev, current.ino, current.size, current.mtime_ns, current.mode)
                                           == (entry.dev, entry.ino, entry.size, entry.mtime_ns, entry.mode))
        return False if unmoved and current.nlink != entry.nlink else None

    def _exec_bits_meaningful(self) -> bool:
        """Whether an executable bit on a workspace file carries information. git probes
        the filesystem when it creates a repository and records the answer in
        core.fileMode. core.fileMode=false is also set by hand where the filesystem
        does keep the bit, so then the bit means nothing only when the filesystem sets
        it on every file. A file created now decides (_fixed_exec_bits): local modes of
        tracked files are history - copied from a FAT drive, or chmod -R +x - and say
        nothing about this filesystem. Only when that probe cannot run, the bit means
        nothing if every tracked file that the index records as non-executable already
        had one when the task started."""
        if self._exec_bits is None:
            code, out, _ = run_git_cmd(["git", "config", "--bool", "core.fileMode"], cwd=self.root)
            if code == 0 and out.strip() == "false":
                fixed = _fixed_exec_bits(self.root)
                if fixed is None:
                    plain = [self.pre[rel].mode for rel, mode in self.tracked_modes.items()
                             if mode == "100644" and rel in self.pre and self.pre[rel].kind == "file"]
                    fixed = bool(plain) and all(mode & 0o111 for mode in plain)
                self._exec_bits = not fixed
            else:
                self._exec_bits = True
        return self._exec_bits

    def _link_count(self, rel: str) -> int:
        try:
            fd, name = _open_parent(self.root, rel)
        except OSError:
            return 0
        try:
            return os.stat(name, dir_fd=fd, follow_symlinks=False).st_nlink
        except OSError:
            return 0
        finally:
            os.close(fd)

    def _is_transient_bytecode(self, rel: str) -> bool:
        """Bytecode in __pycache__ for a source that exists, or that existed at task
        start and is gone now (compiled, then deleted or renamed by the task). Python
        regenerates it, so bytecode the run generated or rewrote is purged once delivery
        is decided, never delivered (it can shadow its source). Loose bytecode and
        bytecode of a source that never existed are security blocks (#py-orch-2)."""
        source = _bytecode_source(rel)
        return source is not None and (_is_regenerable_cache(rel, self.root) or source in self.pre)

    def _report_roots(self) -> List[Tuple[str, str]]:
        """(prefix of root-relative paths, directory) of each root that test output is
        judged from: the repository root and, for a task run in a subdirectory of it
        (makewand run -C repo/sub), that directory too - where the test gate
        (orchestrator.run_local_tests, run in the task's directory) judges it from."""
        roots = [("", self.root or self.cwd)]
        if self.root:
            relative = os.path.relpath(self.cwd, self.root)
            if relative != os.curdir and relative != os.pardir and not relative.startswith(os.pardir + os.sep):
                roots.append((relative.replace(os.sep, "/") + "/", self.cwd))
        return roots

    def _inert_test_artifact(self, rel: str, entry: Optional[FsEntry], exec_bits: bool = True) -> bool:
        """_is_inert_test_artifact() for root-relative rel from any of the report roots."""
        return any(rel.startswith(prefix) and _is_inert_test_artifact(rel[len(prefix):], entry, exec_bits, directory)
                   for prefix, directory in self._report_roots())

    def _is_report_data(self, rel: str) -> bool:
        """Test output that is plain data (_is_test_output_data), or a Playwright trace or
        video in its output directories at one of the report roots (_is_playwright_artifact)."""
        return _is_test_output_data(rel) or any(rel.startswith(prefix) and _is_playwright_artifact(rel[len(prefix):], directory)
                                                for prefix, directory in self._report_roots())

    def _reviewed_scope(self) -> Tuple[Dict[str, str], set, set]:
        """Rebuilds what the review diff showed, the way get_git_diff_status() builds
        it in host mode: a private intent-to-add index against the task baseline,
        plus each checked-out submodule against its task-start commit. Like the
        review diff it covers the whole repository with root-relative paths from any
        working directory, whatever diff.relative says (DIFF_FORMAT_FLAGS).

        Returns ({root-relative path: new git mode} for every path whose current
        content the diff shows, the paths it shows as deleted, the paths tracked by
        a submodule's baseline). Raises OSError when it cannot be rebuilt.
        Submodules are found from the repository root, as get_git_diff_status() does
        from any directory of the repository.
        """
        shown: Dict[str, str] = {}
        deleted: set = set()
        sub_tracked: set = set()
        for rel, repo, base in [("", self.cwd, self.baseline_commit)] + self._submodule_repos(self.root):
            raw, error = _read_workspace_diff(repo, base, arguments=("--raw", "-z", "--no-renames"), binary=True)
            if error:
                raise OSError(error)
            prefix = f"{rel}/" if rel else ""
            try:
                _parse_raw_diff(raw, prefix, shown, deleted)
            except ValueError as exc:
                raise OSError(str(exc)) from exc
            if rel:
                code, listed, _ = run_git_cmd(["git", "ls-tree", "-r", "-z", "--name-only", "--full-tree", base],
                                              cwd=repo, binary=True)
                if code != 0:
                    raise OSError(f"无法读取子模块 {rel} 的基线文件列表 (rc={code})")
                sub_tracked.update(prefix + os.fsdecode(name) for name in listed.split(b"\0") if name)
        return shown, deleted, sub_tracked

    def _unreviewed_changes(self, current: Dict[str, FsEntry], markers: Dict[str, FsEntry]) -> _DeliveryReview:
        """Classifies what the task created, modified or deleted outside the reviewed diff.

        A pre-existing path the task left untouched (judged on type, mode and content
        by _changed_since_task_start) is never examined, whatever its type or ignore
        status; one whose content cannot be confirmed (only its ctime or inode moved
        and no digest decides) is reported as such, never as modified. Nothing is
        removed here: finalize_success() acts on the result.
        """
        review = _DeliveryReview([], [], [], [], [], set(), set())
        blocks = review.blocks
        changed_markers = self._changed_git_markers(markers)
        if changed_markers:
            blocks.append("安全阻断：本次任务新建、替换或删除了嵌套的 Git 元数据（.git 可携带会被 git 自动执行的 hooks 与配置，"
                          f"未纳入审查）: {_summarize_paths(changed_markers)}")
        private_hooks, shared_hooks, hooks_now = self._changed_git_hooks()
        # Every checkout using a shared hooks directory writes there; nothing ties the change to this task.
        self._note_shared_hooks(shared_hooks, hooks_now, review.notes)
        if private_hooks:
            blocks.append("安全阻断：本次任务改动了仓库或子模块自身的 Git hooks（会被 git 自动执行，未纳入审查）: "
                          + _summarize_paths(private_hooks))
        budget, samples = _DigestBudget(), _SampleBudget()
        touched: Dict[str, FsEntry] = {}
        for rel, entry in current.items():
            if entry.kind == "dir":
                continue
            changed = True if rel not in self.pre else self._changed_since_task_start(rel, entry, budget, samples)
            if changed is not False:
                touched[rel] = entry
                if changed is None:
                    review.unconfirmed.add(rel)
        missing = sorted(rel for rel, entry in self.pre.items()
                         if entry.kind != "dir" and (rel not in current or current[rel].kind == "dir"))
        # Submodules and other nested repositories: the review diff shows their tracked
        # changes (each checked-out submodule's own diff), never their other files.
        nested = {rel.rsplit("/", 1)[0] for rel in set(markers) | set(self.pre_git_markers) if rel != ".git"}

        def in_nested(rel: str) -> bool:
            parts = rel.split("/")
            return any("/".join(parts[:depth]) in nested for depth in range(1, len(parts)))

        for rel in sorted(touched):
            if touched[rel].kind == "file" and self._is_transient_bytecode(rel):
                del touched[rel]
                review.transient.append(rel)
        if not touched and not missing:
            return review
        try:
            shown, deleted, sub_tracked = self._reviewed_scope()
        except OSError as exc:
            blocks.append(f"安全阻断：无法重建审查 diff，无法核对本次任务的改动是否都经过审查: {str(exc)[:200]}")
            return review
        # Probed only when it matters: WSL drvfs, CIFS or exFAT mounts give every file an executable bit.
        exec_bits = (not any(entry.kind == "file" and entry.mode & 0o111 for entry in touched.values())
                     or self._exec_bits_meaningful())
        found: Dict[str, List[str]] = {"binary": [], "sensitive": [], "mode": [], "unconfirmed": [], "nested": [],
                                       "changed": [], "new": []}
        for rel, entry in sorted(touched.items()):
            before = self.pre.get(rel)
            if before is not None and before.kind == "dir":
                before = None
            gained_exec = bool(exec_bits and entry.kind == "file" and entry.mode & 0o111
                               and (before is None or before.kind != "file" or not before.mode & 0o111))
            if rel in shown:
                # Reviewed content; an executable bit the diff does not show (core.fileMode=false) was not.
                if gained_exec and shown[rel] != "100755":
                    found["mode"].append(rel)
                continue
            if rel not in deleted and (rel in self.tracked or rel in sub_tracked):
                # Rewritten with the baseline's bytes: the diff shows every content change of a tracked path.
                if gained_exec:
                    found["mode"].append(rel)
                continue
            risk = _security_risk(rel, gained_exec)
            # Test output counts from the repository root and from the task's directory alike (_report_roots).
            inert = risk is None and self._inert_test_artifact(rel, entry, exec_bits)
            if inert and before is not None:
                # A pre-existing file the run changed is kept only when that is clearly safe: not
                # in a nested repository (which may track it) nor in a tool directory a later run
                # loads as code, and either plain data (a Playwright trace or video included) or
                # rewritten with the very bytes it had (a report asset regenerated as it was).
                inert = (not in_nested(rel) and not any(part in TEST_OUTPUT_CODE_DIRS for part in rel.lower().split("/"))
                         and (self._is_report_data(rel) or self._content_changed(rel, None, budget, samples) is False))
            if inert and self._link_count(rel) == 1:  # a hard link would reach another file's bytes
                (review.inert_kept if before is not None else review.inert_new).append(rel)
                continue
            if rel in review.unconfirmed:
                found["unconfirmed"].append(rel)  # possibly unmodified: never reported as a change
            else:
                found[risk or ("nested" if in_nested(rel) else "changed" if before is not None else "new")].append(rel)
        for rel in missing:
            if rel not in deleted and not _is_regenerable_cache(rel) and (
                    in_nested(rel) or not self._inert_test_artifact(rel, None)):
                found["nested" if in_nested(rel) else "changed"].append(rel)
        review.unconfirmed.intersection_update(found["unconfirmed"])
        review.unrestorable.update(rel for key in ("binary", "sensitive", "nested", "changed") for rel in found[key]
                                   if rel in self.pre and rel not in self.backups)

        def detail(paths: List[str]) -> str:
            why = {p: [f"未备份: {self.unbacked[p]}"] if p in self.unbacked else [] for p in paths}
            for p in paths:
                if p in review.unconfirmed and p in self.digest_gaps:
                    why[p].append(f"未核对内容: {self.digest_gaps[p]}")
            return _summarize_paths([f"{p}（{'；'.join(why[p])}）" if why[p] else p for p in paths])

        for key, label in (
            ("binary", "安全阻断：本次任务新建或修改了未纳入审查的可执行或二进制产物"),
            ("sensitive", "安全阻断：本次任务改动了未纳入审查的安全敏感文件（.env*、.envrc、*.pth、sitecustomize.py、Git hooks 等）"),
            ("mode", "安全阻断：新增的可执行权限未体现在审查 diff 中"),
            ("unconfirmed", "安全阻断：无法确认任务前已存在的以下文件未被改动（只有 ctime 或 inode 变化，内容未能与任务前"
                            "摘要核对；调高括注的 MAKEWAND_FINGERPRINT_* 上限后重试即可核对）"),
            ("nested", "安全阻断：子模块或嵌套 Git 仓库中的文件被新建、修改或删除，但改动不在审查 diff 中"),
            ("changed", "安全阻断：任务前已存在但被 .gitignore 忽略的文件被篡改或删除（未纳入审查）"),
            ("new", "安全阻断：本次任务新建了被 .gitignore 忽略的文件（未纳入审查）"),
        ):
            if found[key]:
                blocks.append(f"{label}: {detail(found[key])}")
        return review

    def _changed_git_markers(self, markers: Dict[str, FsEntry]) -> List[str]:
        """Nested .git entries that differ from the task-start snapshot (new, replaced or gone)."""
        changed = {rel for rel, entry in markers.items() if rel != ".git" and (
            rel not in self.pre_git_markers or (entry.kind, entry.dev, entry.ino) != (
                self.pre_git_markers[rel].kind, self.pre_git_markers[rel].dev, self.pre_git_markers[rel].ino))}
        changed.update(rel for rel in self.pre_git_markers if rel != ".git" and rel not in markers)
        return sorted(changed)

    def _remove_task_files(self, paths: List[str], problems: List[str], failure: str) -> List[str]:
        removed: List[str] = []
        for rel in paths:
            try:
                _unlink_at(self.root, rel)
                removed.append(rel)
            except FileNotFoundError:
                continue
            except OSError as exc:
                problems.append(f"{failure} {rel}: {exc.strerror or exc}")
        return removed

    def _rollback_outcome(self, rollback_problems: List[str]) -> str:
        """How the rollback after a blocked delivery ended, for failure_reason. A file the
        rollback could not verify (self.unverified) is reported as such, not as lost."""
        if not rollback_problems:
            outcome = "改动已回滚"
        elif self.unrestored:
            outcome = f"改动回滚未能完全恢复，以下任务前已存在的文件无法恢复: {_summarize_paths(self.unrestored, 10)}"
        else:
            outcome = "改动回滚未能完全成功（见回滚提示）"
        if self.unverified:
            outcome += f"；以下任务前已存在的文件内容无法核实（未被确认改动，也无法确认未被改动）: {_summarize_paths(self.unverified, 10)}"
        return outcome

    def _remove_empty_new_parents(self, removed: List[str]) -> None:
        """Drops directories the task created that removing its transient files left empty."""
        parents = {"/".join(rel.split("/")[:depth]) for rel in removed for depth in range(1, rel.count("/") + 1)}
        for rel in sorted(parents, key=lambda p: (p.count("/"), p), reverse=True):
            if rel in self.pre:
                continue
            try:
                _rmdir_at(self.root, rel)
            except OSError:
                pass  # still holds other files: harmless

    # -- success path -------------------------------------------------------
    def finalize_success(self) -> None:
        if self.state != "active":
            return
        self.state = "finalizing"
        problems: List[str] = []
        notes: List[str] = []
        try:
            scan_errors: List[str] = []
            markers: Dict[str, FsEntry] = {}
            current = scan_workspace_tree(self.root, strict=False, errors=scan_errors, git_markers=markers)
            if scan_errors:
                # The task-start snapshot was complete, so anything unreadable now was hidden during the task.
                problems.append(f"安全阻断：交付核查扫描不完整，无法确认是否存在未审查的改动: {_summarize_paths(scan_errors, 5)}")

            removed: List[str] = []
            if not problems:
                review = self._unreviewed_changes(current, markers)
                problems.extend(review.blocks)
                notes.extend(review.notes)
                # A rollback restores none of these; _verify() reports them as not restored
                # (unrestorable) or as not verifiable (unconfirmed).
                self.unrestorable = review.unrestorable
                self.unconfirmed = review.unconfirmed
                if not problems:
                    # Only with delivery decided is anything removed: bytecode the run
                    # generated or rewrote (it can shadow its source) and test output
                    # the task created. Pre-existing files are never touched before.
                    removed = self._remove_task_files(review.transient, problems,
                                                      "安全阻断：无法清除任务生成的字节码缓存")
                    removed += self._remove_task_files(review.inert_new, problems,
                                                       "安全阻断：无法清理本次运行生成、未纳入审查的测试产物")
                    if review.inert_new:
                        notes.append(f"已清理本次运行生成、未纳入审查的测试产物: {_summarize_paths(review.inert_new)}")
                    if review.inert_kept:
                        notes.append("本次运行更新了任务前已存在的测试产物（不在审查 diff 中，按惰性测试产物放行）: "
                                     + _summarize_paths(review.inert_kept))
            self._remove_empty_new_parents(removed)

            if problems:
                self.outcome_status = "UNVERIFIED"
                self.state = "rolling_back"
                rollback_problems: List[str] = []
                try:
                    self._rollback_steps("安全阻断交付并回滚: " + "; ".join(problems), rollback_problems, notes)
                except Exception as exc:  # noqa: BLE001
                    rollback_problems.append(f"安全阻断回滚异常: {type(exc).__name__}: {exc}")
                summary = "；".join(p.replace("安全阻断：", "", 1) for p in problems)
                self.failure_reason = (f"交付核验触发安全阻断，结论未验证 (UNVERIFIED): {summary[:300]}；"
                                       + self._rollback_outcome(rollback_problems))
                if self._rejected_dir is not None:
                    self.failure_reason += f"；被拒改动已存档于 {self._rejected_dir}"
                self.failure_reason = _printable(self.failure_reason)
                problems.extend(rollback_problems)
                self._finish(False, problems, notes, rollback=True, rollback_problems=rollback_problems)
                return

            if self.created_git:
                self._save_nongit_delivery(problems, notes)
            self._discard_backups()
        except Exception as exc:  # noqa: BLE001 - reported below
            problems.append(f"交付收尾发生异常: {type(exc).__name__}: {exc}")
            self.state = "rolling_back"
            self.outcome_status = "UNVERIFIED"
            rollback_problems = []
            try:
                self._rollback_steps("交付异常中断并回滚: " + str(exc), rollback_problems, notes)
            except Exception as r_exc:  # noqa: BLE001
                rollback_problems.append(f"交付异常回滚出错: {type(r_exc).__name__}: {r_exc}")
            self.failure_reason = _printable(f"交付核验未能完成，结论未验证 (UNVERIFIED): {type(exc).__name__}: {str(exc)[:200]}；"
                                             + self._rollback_outcome(rollback_problems))
            problems.extend(rollback_problems)
            self._finish(False, problems, notes, rollback=True, rollback_problems=rollback_problems)
            return

        self._finish(not problems, problems, notes, rollback=False)

    def _save_nongit_delivery(self, problems: List[str], notes: List[str]) -> None:
        code, _, err = run_git_cmd(["git", "add", "-A", "--intent-to-add"], cwd=self.root)
        if code == 0:
            code, patch_bytes, err = run_git_cmd(["git", "diff", *PATCH_EXPORT_FLAGS, self.baseline_commit], cwd=self.root, binary=True)
        if code != 0:
            problems.append(f"无法导出交付补丁 (rc={code})")
            return
        delivery = create_private_artifact_dir("delivery")
        patch_file = write_private_file(delivery / "makewand_delivery.patch", patch_bytes or b"")
        write_private_file(delivery / "delivery_manifest.json", _json_text({
            "repo_root": self.root, "mode": "non-git-host", "baseline_commit": self.baseline_commit,
            "main_patch": str(patch_file), "sha256": hashlib.sha256(patch_bytes or b"").hexdigest(),
        }))
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
        notes.append(f"任务基线已导出为 git bundle，可用于人工恢复: git clone {_shell_quote(target)} <目录>")
        return True

    def _finish(self, ok: bool, problems: List[str], notes: List[str], rollback: bool,
                rollback_problems: Optional[List[str]] = None) -> None:
        """Reports the outcome. rollback_problems: what the rollback after a blocked or
        aborted delivery could not restore, while problems also holds why it rolled back;
        None when problems are all the rollback's own (rollback()). The title says whether
        the workspace is back at its baseline."""
        unrestored = list(problems if rollback_problems is None else rollback_problems)
        if self.created_git and os.path.lexists(self.git_dir):
            if ok or self._bundle_baseline(notes):
                removal_error = _remove_git_dir(self.git_dir)
                if removal_error:
                    problems.append(removal_error)
                    unrestored.append(removal_error)
                    self.keep_git = True
            else:
                self.keep_git = True
                kept = f"为保留恢复依据，暂未删除 makewand 创建的临时仓库 {self.git_dir}（核对后可手动删除）"
                problems.append(kept)
                unrestored.append(kept)
        if rollback:
            if not problems:
                self._discard_backups()
            elif self.backup_dir is not None:
                notes.append(f"任务前文件备份保存在: {self.backup_dir}")
        self.succeeded = not problems
        self.state = "rolled_back" if rollback else "committed"
        # Every line may carry a file name or git output with undecodable bytes (_printable).
        if rollback and not problems and self.unverified:
            # A file nothing could re-check (no digest, or its re-hash refused) may still differ.
            print(c(f"🛡️ [Makewand Transaction] 已回滚本次任务的改动并逐项核验，但有 {len(self.unverified)} 个任务前已存在的"
                    "文件内容无法核实（见下方说明），不能确认工作区已完全恢复基线。", COLOR_YELLOW))
        elif rollback and not problems:
            print(c("🛡️ [Makewand Transaction] 已回滚本次任务的全部改动并逐项核验，工作区已恢复基线。", COLOR_YELLOW))
        elif problems:
            if rollback:
                restored = "回滚未能完全恢复任务前状态" if unrestored else "改动已回滚"
                if rollback_problems is None:
                    title = restored
                elif any("安全阻断" in p for p in problems):
                    title = f"交付核验触发安全阻断，{restored}"
                else:
                    title = f"交付核验未能完成，{restored}"
                if self.outcome_status == "UNVERIFIED":
                    title += "（结论未验证 UNVERIFIED：审查通过的改动未交付）"
            else:
                title = "交付收尾存在问题"
            print(c(f"⚠️ [Makewand Transaction] {title}，请人工检查：", COLOR_RED))
            for line in problems[:30]:
                print(c(_printable(f"   • {line}"), COLOR_RED))
        for line in notes:
            print(c(_printable(f"   {line}"), COLOR_YELLOW))

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
                print(c(_printable(f"⚠ [Makewand Transaction] {removal_error}"), COLOR_RED))


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
            return _printable(str(exc))
        except OSError as exc:
            return _printable(f"无法获取工作区锁: {exc}")
        return None

    def close(self, result: Any, error: Optional[BaseException]) -> None:
        try:
            if self.txn is not None:
                reason = f"流水线异常中断: {type(error).__name__}: {error}" if error is not None else "流水线未通过"
                self.txn.close(succeeded=(error is None and result is True), reason=reason)
        except Exception as exc:  # noqa: BLE001 - must not mask the pipeline outcome
            print(c(_printable(f"⚠️ [Makewand Transaction] 收尾时发生异常，请人工检查工作区: {type(exc).__name__}: {exc}"),
                    COLOR_RED))
        finally:
            if self.lock is not None:
                try:
                    self.lock.release()
                except OSError as exc:
                    print(c(_printable(f"⚠ [Makewand Workspace Lock] 释放工作区锁失败: {exc}"), COLOR_YELLOW))
                self.lock = None
