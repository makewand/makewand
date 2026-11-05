"""
Git workspace resilience and isolated worktree management.
"""

import os
import stat
import time
import errno
import shutil
import tempfile
import subprocess
import json
import shlex
from contextvars import ContextVar
from pathlib import Path, PurePosixPath
from typing import Optional, Dict, Any, List, Tuple, Union
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
# textconv drivers). --submodule=short pairs with --ignore-submodules=dirty so git
# shows a submodule's committed pointer without running `git status` inside it to
# decide "-dirty": that child reads the nested repository's own config and
# info/attributes - which neither --attr-source nor the shield reaches, since git
# spawns it without them - and would run a filter driver on the host. Submodule
# content is reviewed separately (get_git_diff_status), with the shield in force.
# An option the caller passes itself comes later and wins.
DIFF_FORMAT_FLAGS = ("--no-ext-diff", "--no-textconv", "--no-color", "--no-relative",
                     "--src-prefix=a/", "--dst-prefix=b/", "--submodule=short",
                     "--ignore-submodules=dirty")
# git status never enters a nested repository either (see DIFF_FORMAT_FLAGS): unlike
# diff.ignoreSubmodules (_SUBMODULE_BARRIER), the option outranks a submodule.<name>.ignore
# that .gitmodules, a work-tree file, sets. dirty_submodule_paths asks each registered
# submodule itself.
STATUS_FLAGS = ("--ignore-submodules=dirty",)
# A clone never checks files out: its new repository takes its object format and refs from
# the source, so no attribute source can be named for it in advance, and the source's
# .gitattributes would select a filter the user defines globally. Callers check the files
# out afterwards, through run_git_cmd.
_CLONE_FLAGS = ("--no-checkout",)
_SUBCOMMAND_FLAGS = {"diff": DIFF_FORMAT_FLAGS, "status": STATUS_FLAGS, "clone": _CLONE_FLAGS}
# Command-scope configuration of every hardened git command. GIT_CONFIG_COUNT reaches each
# git it starts, in a submodule too, so none of these enters a nested repository on its own:
# diff.ignoreSubmodules=dirty spares diff and status the `git status` they would run there
# (a .gitmodules "ignore" overrides it, hence also STATUS_FLAGS and DIFF_FORMAT_FLAGS;
# git add reads none of it, hence add_all), submodule.recurse=false keeps checkout, reset
# and read-tree out of a submodule, where they would check files out through its own filters,
# and fetch and push do not recurse. makewand visits registered submodules itself, each
# through run_git_cmd (get_git_diff_status, dirty_submodule_paths).
_SUBMODULE_BARRIER = (("diff.ignoreSubmodules", "dirty"), ("submodule.recurse", "false"),
                      ("fetch.recurseSubmodules", "false"), ("push.recurseSubmodules", "no"))
# The review diff (get_git_diff_status) shows three lines of context around every
# change whatever diff.context says, like an exported patch: diff.context=0 would
# leave reviewers with bare hunks. Not in DIFF_FORMAT_FLAGS, because --unified
# implies a patch, which a --raw or --name-status listing must not grow.
REVIEW_DIFF_FLAGS = ("--unified=3",)
# The empty tree of each object format. Attributes are read from it (--attr-source): no
# .gitattributes in the work tree or index selects a filter, diff or merge driver
# (info/attributes is shielded separately). Always the full id of the repository's own
# format: git resolves a hex string of another length as a ref name, and a branch named
# like the SHA-1 id would hand a SHA-256 repository's attributes to its tree.
_EMPTY_TREE_OIDS = {"sha1": "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
                    "sha256": "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321"}
# git apply modes that work through the index, the only ones that accept an attribute source.
_APPLY_INDEX_OPTIONS = frozenset({"--index", "--cached", "--3way", "-3"})
# Settings that leave a filter driver no command to run and make git not insist on it.
_DISABLED_FILTER_DRIVER = (("clean", ""), ("smudge", ""), ("process", ""), ("required", "false"))
# Global git options whose value is a separate argument (``git -C dir diff``), as git.c reads them.
_GIT_GLOBAL_OPTIONS_WITH_VALUE = frozenset({"-C", "-c", "--git-dir", "--work-tree", "--namespace",
                                            "--config-env", "--shallow-file", "--attr-source"})
# Global options that would make git use another repository, work tree, namespace, attribute
# source or set of git programs than the ones run_git_cmd shields for its cwd: a caller passes
# cwd instead. --config-env and include/includeIf settings would load configuration the filter
# driver listing does not read in the same way.
_REDIRECTING_GLOBAL_OPTIONS = frozenset({"-C", "--git-dir", "--work-tree", "--namespace", "--bare",
                                         "--attr-source", "--exec-path", "--config-env"})
_INCLUDE_SECTIONS = frozenset({"include", "includeif"})


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


def _refuse_redirecting_options(cmd: List[str], sub_index: Optional[int]) -> None:
    """Raises ValueError for a global option (before the subcommand) in _REDIRECTING_GLOBAL_OPTIONS,
    or a ``-c`` setting in an include or includeIf section (any case, as git compares them)."""
    end = len(cmd) if sub_index is None else sub_index
    index = 1
    while index < end:
        argument = str(cmd[index])
        if argument.split("=", 1)[0] in _REDIRECTING_GLOBAL_OPTIONS:
            raise ValueError(f"Refusing git option {argument!r}: run_git_cmd shields the repository of its cwd; "
                             "pass cwd instead")
        if argument == "-c" and index + 1 < end:
            setting = str(cmd[index + 1])
            if setting.split("=", 1)[0].strip().split(".", 1)[0].lower() in _INCLUDE_SECTIONS:
                raise ValueError(f"Refusing git option -c {setting!r}: run_git_cmd lists the configuration of "
                                 "its cwd; pass cwd instead")
        index += 2 if argument in _GIT_GLOBAL_OPTIONS_WITH_VALUE else 1


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
    # git config reads only this file (like --file) and every other git command ignores it:
    # the filter driver listing for git apply would miss the repository's own drivers, and
    # makewand's own `git config` writes would land in the caller's file.
    "GIT_CONFIG",
    # An attribute source of the caller's (git 2.55 also segfaults on a work-tree apply under
    # one), a repository discovery other than the one run_git_cmd reproduces by reading files,
    # and a namespace, like the global options _refuse_redirecting_options refuses.
    "GIT_ATTR_SOURCE",
    "GIT_CEILING_DIRECTORIES",
    "GIT_DISCOVERY_ACROSS_FILESYSTEM",
    "GIT_NAMESPACE",
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
# A throw-away Git repository may inspect a different, untrusted work tree.
# Keep that tree excluded for every subprocess, including cached resolutions.
_git_binary_exclusions = ContextVar("makewand_git_binary_exclusions", default=())


def _git_workspace_root(cwd: Optional[Union[str, Path]]) -> Path:
    """Exclude the whole enclosing workspace, including parent/nested repositories."""
    start = Path(cwd if cwd is not None else os.getcwd()).resolve()
    boundary = start
    refused = set(_refused_init_roots())
    for level in (start, *start.parents):
        if level in refused:
            break
        bare = all((level / name).is_dir() for name in ("objects", "refs")) and _valid_git_head(level / "HEAD")
        if _is_git_marker(level / ".git") or bare:
            boundary = level
    return boundary


def resolve_safe_git_binary(cwd: Optional[Union[str, Path]] = None) -> str:
    """Resolve git outside the entire workspace; never execute one of its paths."""
    global _RESOLVED_GIT_BINARY
    workspaces = (_git_workspace_root(cwd), *_git_binary_exclusions.get())

    def inside(path):
        candidate = Path(path).resolve()
        for workspace in workspaces:
            try:
                if os.path.commonpath((os.path.normcase(str(workspace)),
                                       os.path.normcase(str(candidate)))) == os.path.normcase(str(workspace)):
                    return True
            except ValueError:  # different Windows volumes
                continue
        return False

    if _RESOLVED_GIT_BINARY:
        if inside(_RESOLVED_GIT_BINARY):
            raise PermissionError(f"Refusing to execute git binary found inside workspace cwd: {_RESOLVED_GIT_BINARY}")
        return _RESOLVED_GIT_BINARY

    clean_entries = []
    for entry in os.environ.get("PATH", "").split(os.pathsep):
        if not entry or not os.path.isabs(entry):
            continue
        try:
            if inside(entry):
                continue
        except (OSError, ValueError, RuntimeError):
            continue
        clean_entries.append(entry)
    found = shutil.which("git", path=os.pathsep.join(clean_entries))
    if not found:
        raw_found = shutil.which("git")
        if raw_found and inside(raw_found):
            raise PermissionError(f"Refusing to execute git binary found inside workspace cwd: {raw_found}")
        raise FileNotFoundError("Safe git binary not found in PATH")
    found_path = Path(found).resolve()
    if inside(found_path):
        raise PermissionError(f"Refusing to execute git binary found inside workspace cwd: {found}")
    _RESOLVED_GIT_BINARY = str(found_path)
    return _RESOLVED_GIT_BINARY


# git refuses a .git file larger than this (read_gitfile_gently); commondir holds a path.
_GIT_POINTER_MAX_BYTES = 1 << 20
# A repository configuration makewand reads for its object format; larger ones are refused.
_GIT_CONFIG_MAX_BYTES = 16 << 20
_HEX_DIGITS = frozenset(b"0123456789abcdefABCDEF")


def _read_git_pointer(path: Path, limit: int = _GIT_POINTER_MAX_BYTES) -> bytes:
    """A .git file, commondir or config, read as git reads it; OSError where git's read fails too."""
    descriptor = os.open(_fs_path(path), os.O_RDONLY | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_BINARY", 0)
                         | getattr(os, "O_CLOEXEC", 0))
    with os.fdopen(descriptor, "rb") as handle:
        # git cannot read a directory and would wait forever on a FIFO.
        if not stat.S_ISREG(os.fstat(handle.fileno()).st_mode):
            raise OSError(errno.EINVAL, "not a regular file", str(path))
        data = handle.read(limit + 1)
    if len(data) > limit:
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
        # Open without blocking and confirm by fstat, like _read_git_pointer: a FIFO
        # swapped in after the lstat must not hang makewand before git starts, where
        # MAKEWAND_GIT_TIMEOUT could not cover the wait.
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_CLOEXEC", 0))
        with os.fdopen(descriptor, "rb") as handle:
            if not stat.S_ISREG(os.fstat(handle.fileno()).st_mode):
                return False
            data = handle.read(255)
    except OSError:
        return False
    if data.startswith(b"ref:") and data[4:].lstrip(b" \t\n\r").startswith(b"refs/"):
        return True
    return len(data) >= 40 and all(byte in _HEX_DIGITS for byte in data[:40])


def _git_discovery(cwd: Optional[Union[str, Path]]) -> Tuple[List[Path], Optional[Path]]:
    """(every git directory and common directory git may use for a command run in ``cwd``,
    the common directory of the repository git uses there, None outside one).

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
            if target is None:
                return found, None
            common_dir = _git_common_dir(target)
            return found + [target, common_dir], common_dir
        candidates = [level]
        if stat.S_ISDIR(mode):
            found.append(dot_git)
            candidates.insert(0, dot_git)
        for git_dir in candidates:
            if _valid_git_head(git_dir / "HEAD"):
                common_dir = _git_common_dir(git_dir)
                found += [git_dir, common_dir]
                if all(os.access(_fs_path(common_dir / name), os.X_OK) for name in ("objects", "refs")):
                    return found, common_dir
    return found, None


# git's own ctype (git-compat-util.h): only these four bytes are space.
_GIT_SPACE = frozenset(b" \t\n\r")
_UTF8_BOM = b"\xef\xbb\xbf"


def _git_config_entries(data: bytes) -> List[Tuple[bytes, Optional[bytes]]]:
    """(name, value) of each entry of a git config file, as git's parser reads it (config.c,
    git_parse_source): section and key lower-cased, a quoted subsection kept as written, value
    None for a key without "=". Raises ValueError where git reports a bad config line."""
    position = 0
    at_end = False

    def char() -> int:  # get_next_char: CRLF reads as LF, the end of the file as a final LF
        nonlocal position, at_end
        if position >= len(data):
            at_end = True
            return 0x0A
        byte = data[position]
        position += 1
        if byte == 0x0D and data[position:position + 1] == b"\n":
            position += 1
            return 0x0A
        return byte

    def is_alpha(byte: int) -> bool:
        return 0x41 <= byte <= 0x5A or 0x61 <= byte <= 0x7A

    def is_key(byte: int) -> bool:
        return is_alpha(byte) or 0x30 <= byte <= 0x39 or byte == 0x2D

    def lower(byte: int) -> int:
        return byte | 0x20 if 0x41 <= byte <= 0x5A else byte

    def section() -> bytes:  # get_base_var and get_extended_base_var
        name = bytearray()
        while True:
            byte = char()
            if at_end:
                raise ValueError("unterminated section header")
            if byte == 0x5D:  # ]
                return bytes(name)
            if byte in _GIT_SPACE:
                break
            if not (is_key(byte) or byte == 0x2E):
                raise ValueError("bad section name")
            name.append(lower(byte))
        while byte in _GIT_SPACE:
            if byte == 0x0A:
                raise ValueError("incomplete section header")
            byte = char()
        if byte != 0x22:  # "
            raise ValueError("bad section header")
        name.append(0x2E)
        while True:
            byte = char()
            if byte == 0x0A:
                raise ValueError("incomplete section header")
            if byte == 0x22:
                break
            if byte == 0x5C:  # \
                byte = char()
                if byte == 0x0A:
                    raise ValueError("incomplete section header")
            name.append(byte)
        if char() != 0x5D:
            raise ValueError("bad section header")
        return bytes(name)

    def value() -> bytes:  # parse_value
        quoted = comment = False
        trim = 0
        result = bytearray()
        while True:
            byte = char()
            if byte == 0x0A:
                if quoted:
                    raise ValueError("unterminated quote")
                return bytes(result[:trim] if trim else result)
            if comment:
                continue
            if byte in _GIT_SPACE and not quoted:
                trim = trim or len(result)
                if result:
                    result.append(byte)
                continue
            if not quoted and byte in (0x23, 0x3B):  # # ;
                comment = True
                continue
            trim = 0
            if byte == 0x5C:
                byte = {0x0A: None, 0x74: 0x09, 0x62: 0x08, 0x6E: 0x0A, 0x5C: 0x5C, 0x22: 0x22}.get(char(), -1)
                if byte == -1:
                    raise ValueError("bad escape")
                if byte is not None:
                    result.append(byte)
                continue
            if byte == 0x22:
                quoted = not quoted
                continue
            result.append(byte)

    entries: List[Tuple[bytes, Optional[bytes]]] = []
    prefix = b""
    bom: Optional[int] = 0
    comment = False
    while True:
        byte = char()
        if bom is not None and bom < len(_UTF8_BOM):
            if not at_end and byte == _UTF8_BOM[bom]:
                bom += 1
                continue
            if bom:
                raise ValueError("partial byte order mark")
            bom = None
        if byte == 0x0A:
            if at_end:
                return entries
            comment = False
            continue
        if comment or byte in _GIT_SPACE:
            continue
        if byte in (0x23, 0x3B):
            comment = True
            continue
        if byte == 0x5B:  # [
            name = section()
            if not name:
                raise ValueError("empty section name")
            prefix = name + b"."
            continue
        if not is_alpha(byte):
            raise ValueError("bad key")
        key = bytearray([lower(byte)])
        while True:  # get_value
            byte = char()
            if at_end or not is_key(byte):
                break
            key.append(lower(byte))
        while byte in (0x20, 0x09):
            byte = char()
        if byte == 0x0A:
            entries.append((prefix + bytes(key), None))
        elif byte == 0x3D:  # =
            entries.append((prefix + bytes(key), value()))
        else:
            raise ValueError("bad key")


def _object_format(common_dir: Path) -> str:
    """The object format of the repository whose common directory is ``common_dir``.

    Read as git's repository-format check reads it: extensions.objectFormat of the
    common directory's config (no include is followed), sha1 without one. Raises
    OSError where git refuses the repository: a config it cannot read or parse, or
    a value that names no object format (a NUL ends it, as git's C string does).
    """
    config_file = common_dir / "config"
    try:
        data = _read_git_pointer(config_file, _GIT_CONFIG_MAX_BYTES)
    except FileNotFoundError:
        return "sha1"
    try:
        values = [entry for name, entry in _git_config_entries(data) if name == b"extensions.objectformat"]
    except ValueError as exc:
        raise OSError(errno.EINVAL, f"bad config ({exc})", str(config_file)) from None
    formats = [entry.split(b"\0", 1)[0].decode("ascii", "replace") if entry is not None else None for entry in values]
    if any(name not in _EMPTY_TREE_OIDS for name in formats):
        raise OSError(errno.EINVAL, "extensions.objectFormat names no object format", str(config_file))
    return formats[-1] if formats else "sha1"


def _empty_tree(common_dir: Optional[Path]) -> str:
    """The empty tree's id in the repository of ``common_dir`` (SHA-1 outside a repository)."""
    return _EMPTY_TREE_OIDS[_object_format(common_dir) if common_dir is not None else "sha1"]


def empty_tree_oid(cwd: Optional[Union[str, Path]]) -> str:
    """The empty tree's object id in the repository git uses for a command run in ``cwd``.

    Read from files (_git_discovery, _object_format), never by running git; raises
    OSError where git cannot read that repository either.
    """
    return _empty_tree(_git_discovery(cwd)[1])


def _attributes_to_shield(metadata_dirs: List[Path]) -> List[Path]:
    """The info/attributes file of each of metadata_dirs (_git_discovery) there is to shield."""
    paths: List[Path] = []
    seen_dirs = set()
    for gdir in metadata_dirs:
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


def _disabled_filter_drivers(git_command: List[str], cwd, env: Dict[str, str], timeout) -> List[Tuple[str, str]]:
    """Command-scope settings disabling every filter driver git's configuration defines for a
    command that starts with git_command (git and its global options) in cwd with env.

    run_git_cmd lists them with the attributes shield already in place, right before git
    starts. A process that writes the repository's configuration at that very moment can still
    add a driver between this listing and git's own read: git has no setting that disables
    every filter driver whatever its name. Raises OSError when git cannot list the
    configuration; the caller refuses the command.
    """
    try:
        listing = subprocess.run([*git_command, "config", "--null", "--name-only", "--get-regexp", r"^(filter|merge)\."],
                                 cwd=cwd, env=env, capture_output=True, timeout=timeout)
    except subprocess.SubprocessError as exc:
        raise OSError(f"Cannot list Git filter drivers: {exc}") from exc
    if listing.returncode == 1:  # no filter or merge setting at all
        return _filter_driver_settings(b"")
    if listing.returncode != 0:
        raise OSError(f"Cannot list Git filter drivers: {os.fsdecode(listing.stderr).strip()}")
    return _filter_driver_settings(listing.stdout)


def _filter_driver_settings(listing: bytes) -> List[Tuple[str, str]]:
    """The settings disabling each driver a NUL-separated ``git config --name-only`` listing of
    filter/merge keys names; an empty filter name is a driver too (``filter=``).
    The default merge name is chosen outside all configured driver names so it
    selects Git's built-in text merge. delivery.py builds the same standalone."""
    names: Dict[str, None] = {}
    for key in listing.split(b"\0"):
        name, dot, _ = key[len(b"filter."):].rpartition(b".")
        if key.startswith(b"filter.") and dot:
            names[os.fsdecode(name)] = None
    settings = [(f"filter.{name}.{variable}", value) for name in names for variable, value in _DISABLED_FILTER_DRIVER]
    merge_names = set()
    for key in listing.split(b"\0"):
        name, dot, _ = key[len(b"merge."):].rpartition(b".")
        if key.startswith(b"merge.") and dot:
            merge_names.add(os.fsdecode(name))
    # User drivers shadow even "text" and "binary". An unconfigured name
    # falls back to Git's built-in text merge (merge-ll.c:find_ll_merge_driver).
    for index in range(len(merge_names) + 1):
        default = "makewand_builtin_text_" + str(index)
        if default not in merge_names:
            break
    return settings + [("merge.default", default)]


def _config_environment(settings: List[Tuple[str, str]]) -> Dict[str, str]:
    """GIT_CONFIG_COUNT/KEY/VALUE for settings: unlike ``-c``, they carry a key holding ``=``
    (a driver named "a=b"), and git hands them on to every git it starts."""
    environment = {"GIT_CONFIG_COUNT": str(len(settings))}
    for index, (key, value) in enumerate(settings):
        environment[f"GIT_CONFIG_KEY_{index}"] = key
        environment[f"GIT_CONFIG_VALUE_{index}"] = value
    return environment


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
        # The resolver walks logical workspace paths. process_cwd is already
        # an extended Win32 API path and must never be converted a second time.
        safe_git = resolve_safe_git_binary(cwd=cwd)
        hardened = isinstance(cmd, list) and len(cmd) > 0 and cmd[0] == "git" and safe
        sub_index = _git_subcommand_index(cmd) if hardened else None
        subcommand = cmd[sub_index] if sub_index is not None else None
        if hardened:
            _refuse_redirecting_options(cmd, sub_index)
        if safe:
            # S01: one discovery, by reading files, names every git and common directory whose
            # info/attributes is shielded (any layout, commondir included) and the object format
            # of the repository git uses, which the empty-tree attribute source must match.
            try:
                metadata_dirs, common_dir = _git_discovery(cwd)
                ia_targets = sorted(_attributes_to_shield(metadata_dirs), key=lambda p: str(p.resolve()))
                attr_source = "--attr-source=" + _empty_tree(common_dir) if hardened else ""
            except (OSError, RuntimeError, ValueError) as exc:
                raise OSError(f"Cannot shield Git attributes: {exc}") from exc
        if hardened:
            extra_global = ["--no-pager"]
            if subcommand == "apply":
                # git (2.55) segfaults on worktree-mode apply, --check included, under
                # any attribute source: it reads the preimage through convert_to_git
                # without an index. Outside a repository --attr-source makes every
                # apply die. Only the index modes take one; every apply runs with each
                # configured filter driver disabled, whichever attribute selects it.
                if _APPLY_INDEX_OPTIONS.intersection(cmd[sub_index + 1:]):
                    extra_global.append(attr_source)
            elif subcommand != "clone":
                extra_global.append(attr_source)
            exec_cmd = [safe_git] + extra_global + SAFE_GIT_SECURITY_FLAGS + cmd[1:]
            subcommand_flags = _SUBCOMMAND_FLAGS.get(subcommand, ())
            if subcommand_flags:
                # Also after global options (``git --no-replace-objects diff``).
                position = len(exec_cmd) - len(cmd) + sub_index + 1
                exec_cmd[position:position] = [flag for flag in subcommand_flags if flag not in cmd]
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
            if _diff_index.get() is not None:
                git_env["GIT_INDEX_FILE"] = _diff_index.get()

            # Shield in a deterministic order, with attributes.mw_gate file locks so that
            # concurrent sessions do not collide.
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
            if hardened:
                settings = list(_SUBMODULE_BARRIER)
                if subcommand == "apply":
                    # Listed under the shield, in the same context and as late as possible.
                    settings += _disabled_filter_drivers([safe_git, "--no-pager", *SAFE_GIT_SECURITY_FLAGS,
                                                          *cmd[1:sub_index]], process_cwd, git_env, timeout)
                git_env.update(_config_environment(settings))

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


def _refused_init_roots() -> List[Path]:
    roots = [Path("/"), Path("/tmp")]
    try:
        roots += [Path.home(), Path.home().resolve()]
    except Exception:
        pass
    return roots


def get_submodule_paths(repo_dir: str) -> List[str]:
    """
    Returns an ordered list of submodule relative paths, sorted descending by path depth
    (deepest submodules first). Correctly handles submodule paths containing spaces.
    """
    r_path = Path(repo_dir)
    gitmodules = r_path / ".gitmodules"
    if not gitmodules.exists():
        return []

    # 1. Read .gitmodules paths via git config (preserves spaces in path values)
    paths = _gitmodules_paths(r_path)

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


def _gitmodules_paths(repo_dir: Union[str, Path]) -> List[str]:
    """The submodule paths repo_dir/.gitmodules registers, as written there (git config --file,
    which follows no include). Raises OSError when git cannot read the file."""
    # -z: a record is "key\nvalue\0", and a submodule name or path may hold any whitespace.
    code, out, error = run_git_cmd(["git", "config", "-z", "--file", ".gitmodules", "--get-regexp",
                                    r"^submodule\..*\.path$"], cwd=str(repo_dir), binary=True)
    if code not in (0, 1):
        raise OSError(f"Cannot read Git submodule configuration: {_text(error)}")
    paths = []
    for record in out.split(b"\0") if code == 0 else ():
        _, newline, value = record.partition(b"\n")
        if newline and value:
            paths.append(os.fsdecode(value))
    return paths


def _text(output: Union[str, bytes]) -> str:
    """run_git_cmd output as its text mode decodes it (a bytes input_data makes it bytes)."""
    return output.decode("utf-8", "surrogateescape") if isinstance(output, bytes) else output


def _index_gitlink_entries(cwd: Union[str, Path]) -> Tuple[int, Dict[bytes, bool], str]:
    """(code, {root-relative path: flagged} of the gitlinks in the index git uses in cwd, error),
    flagged when the entry is marked assume-unchanged or skip-worktree: the private diff index
    inside _read_workspace_diff."""
    code, listing, error = run_git_cmd(["git", "ls-files", "-z", "-v", "--stage", "--full-name", "--", ":(top)"],
                                       cwd=str(cwd), binary=True)
    if code != 0:
        return code, {}, _text(error)
    gitlinks: Dict[bytes, bool] = {}
    for record in listing.split(b"\0"):
        meta, tab, name = record.partition(b"\t")
        tag, _, stage = meta.partition(b" ")
        if tab and stage.startswith(b"160000 "):
            # ls-files -v: a lower-case tag is assume-unchanged, S skip-worktree.
            gitlinks[name] = gitlinks.get(name, False) or tag.islower() or tag.upper() == b"S"
    return 0, gitlinks, ""


def _behind_symlink(root: Union[str, Path], name: bytes) -> bool:
    """Whether a directory above root-relative name is a symlink or a junction, checked from
    the top down so that no check follows one."""
    parts = PurePosixPath(os.fsdecode(name)).parts[:-1]
    for depth in range(1, len(parts) + 1):
        parent = Path(root, *parts[:depth])
        if os.path.islink(parent) or _is_reparse_path(parent):
            return True
    return False


def add_all(cwd: Union[str, Path], intent_to_add: bool = False) -> Tuple[int, str, str]:
    """``git add -A`` (``--intent-to-add`` for a diff index) that never starts a command inside a
    nested repository; returns (code, stdout, stderr) like run_git_cmd.

    For a gitlink whose submodule HEAD matches the index, git add -A runs ``git status`` in the
    nested repository to learn whether it is dirty: a child that reads that repository's own
    config and info/attributes and can run a filter driver on the host, and that no setting
    keeps away (git add reads no diff.ignoreSubmodules). So the gitlinks are marked
    assume-unchanged for the add, which then never looks at them, and unmarked after it.
    Exclude pathspecs would do the same, but git add refuses them once a directory above a
    gitlink is ignored or a symlink, where plain git add -A succeeds.

    Unless intent_to_add, the gitlinks are then brought up to date by ``git update-index``,
    which reads only each nested HEAD: a moved HEAD is staged, a removed path drops out and a
    path that is now a file is staged as one, as git add -A would; one behind a symlink is
    dropped without being looked at, as git add -A drops it. A diff index keeps them as they
    are: git diff compares a gitlink with its nested HEAD itself. A gitlink the user marked
    assume-unchanged or skip-worktree is left alone, as git add -A leaves it.
    """
    add = ["git", "add", "-A"] + (["--intent-to-add"] if intent_to_add else [])
    code, entries, error = _index_gitlink_entries(cwd)
    if code != 0:
        return code, "", error
    gitlinks = [name for name, flagged in entries.items() if not flagged]
    if not gitlinks:
        return run_git_cmd(add, cwd=str(cwd))
    # update-index takes paths, not pathspecs: from the top of the work tree.
    code, top, error = run_git_cmd(["git", "rev-parse", "--show-toplevel"], cwd=str(cwd))
    if code != 0:
        return code, "", error
    root = top.rstrip("\n")

    def update_index(options, names):
        if not names:
            return 0, b"", b""
        return run_git_cmd(["git", "update-index", *options, "-z", "--stdin"], cwd=root,
                           input_data=b"".join(name + b"\0" for name in names))

    code, out, error = update_index(["--assume-unchanged"], gitlinks)
    if code != 0:
        return code, _text(out), _text(error)
    present: List[bytes] = []
    try:
        code, out, error = run_git_cmd(add, cwd=str(cwd))
    finally:
        # The add may have replaced a gitlink (a directory above it is now a file or a link).
        list_code, after, list_error = _index_gitlink_entries(root)
        if list_code == 0:
            present = [name for name in gitlinks if name in after]
            list_code, list_out, list_error = update_index(["--no-assume-unchanged"], present)
        if list_code != 0 and code == 0:
            code, out, error = list_code, "", list_error
    if code == 0 and not intent_to_add:
        behind = [name for name in present if _behind_symlink(root, name)]
        code, out, error = update_index(["--force-remove"], behind)
        if code == 0:
            code, out, error = update_index(["--remove"], [name for name in present if name not in behind])
    return code, _text(out), _text(error)


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
                code, _, error = add_all(cwd, intent_to_add=bool(ref))
                if code != 0:
                    return empty, _printable(f"git add failed with exit code {code}: {error.strip()}")
                command = ["git", "diff", *arguments]
                if ref:
                    command.append(ref)
                else:
                    command.extend(["--cached", empty_tree_oid(cwd)])
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


_REFLINK_SUPPORTED: Optional[bool] = None


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


# ---------------------------------------------------------------------------
# Private artifacts (shared contract 1): 0700 directories, 0600 files, names
# from tempfile.mkdtemp (unpredictable), writes never follow symlinks, and a
# bounded retention so state does not grow without limit.
# ---------------------------------------------------------------------------


# ---------------------------------------------------------------------------
# Per-repository workspace lock: one makewand code task per repository.
# The lock file lives in the private state directory, never in the user repo.
# ---------------------------------------------------------------------------


# ---------------------------------------------------------------------------
# Host-mode transaction: the task-start snapshot is taken before any git init,
# rollback only removes paths that did not exist before the task, restores
# tracked files from the baseline commit and pre-existing untracked/ignored
# files from private backups, and reports success only after verification.
# ---------------------------------------------------------------------------


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


def _is_within(path: str, directory: str) -> bool:
    return path == directory or path.startswith(directory.rstrip(os.sep) + os.sep)


