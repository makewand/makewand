"""
Makewand Candidate Lifecycle Manager: inspect, apply, and discard race candidates.
"""

import os
import sys
import json
import shutil
import time
from makewand import filelock as fcntl
import uuid
import stat
import tempfile
import errno
import contextlib
from pathlib import Path
from datetime import datetime
from typing import Dict, Any, List, Optional, Tuple, Union

import hashlib
import makewand.config as config
from makewand.config import (
    ensure_config_dir,
    ensure_private_dir,
    c,
    COLOR_YELLOW,
)
from makewand.git_helper import (
    run_git_cmd,
    get_git_diff,
    _read_workspace_diff,
    WorkspaceLock,
    WorkspaceLockError,
    workspace_lock_root,
)
from makewand.protected_files import ProtectedFiles, ProtectionError
from makewand.windows_paths import filesystem_path

_NO_SECURITY_EXPECTATION = object()
# Default for callers that do not bind a target preimage (sealing/restore).
# ``None`` explicitly means "the target must not exist".
_PREIMAGE_UNCHECKED = object()
_RENAME_NOREPLACE = 1
_RENAME_EXCHANGE = 2
_RENAMEAT2 = []


def _fs_path(path):
    """An API call path; keep journal and workspace identities ordinary."""
    return Path(filesystem_path(path))


def get_scoped_apply_lock_path(base_cwd: Optional[Union[str, Path]] = None) -> Path:
    """Returns the lock file path scoped per workspace/git repository."""
    ensure_config_dir()
    if not base_cwd:
        return config.CONFIG_DIR / "apply.lock"
    try:
        root = workspace_lock_root(base_cwd)
    except Exception:
        root = os.path.realpath(str(base_cwd))
    key = hashlib.sha256(os.fsencode(root)).hexdigest()[:16]
    return config.CONFIG_DIR / f"apply_{key}.lock"


@contextlib.contextmanager
def scoped_apply_lock(base_cwd: Optional[Union[str, Path]] = None):
    """
    Context manager acquiring the apply lock.
    Scopes the exclusive lock per workspace/git repository so operations across
    different repositories run concurrently without blocking each other.
    """
    ensure_config_dir()
    global_lock_file = config.CONFIG_DIR / "apply.lock"
    scoped_lock_file = get_scoped_apply_lock_path(base_cwd)

    global_fd = open(global_lock_file, "a")
    scoped_fd = None
    try:
        if base_cwd:
            fcntl.flock(global_fd, fcntl.LOCK_SH)
            scoped_fd = open(scoped_lock_file, "a")
            fcntl.flock(scoped_fd, fcntl.LOCK_EX)
        else:
            fcntl.flock(global_fd, fcntl.LOCK_EX)
        yield
    finally:
        if scoped_fd is not None:
            try:
                fcntl.flock(scoped_fd, fcntl.LOCK_UN)
            except Exception:
                pass
            try:
                scoped_fd.close()
            except Exception:
                pass
        try:
            fcntl.flock(global_fd, fcntl.LOCK_UN)
        except Exception:
            pass
        try:
            global_fd.close()
        except Exception:
            pass


def _race_protection(race):
    # Presence is authoritative: malformed declarations cannot become legacy.
    return ProtectedFiles.from_dict(race["protected_files"]) if "protected_files" in race else ProtectedFiles()


class CandidateMessage(str):
    """Add an outcome category without changing the legacy apply tuple."""
    def __new__(cls, message, status):
        value = str.__new__(cls, message)
        value.status = status
        return value

    def __getnewargs__(self):
        # copy and pickle rebuild the message through __new__, status included.
        return (str(self), self.status)


class ApplyResult(tuple):
    """What CandidateManager.apply_candidate returns: the (ok, files, message) tuple every
    caller unpacks, plus status, the outcome as an execution status (STATUS_CODES) that a
    caller maps to an exit status without reading the message:

    PASSED - applied, or a dry run, or nothing to apply;
    APPLY_CONFLICT - the workspace no longer matches what the candidate was built on
    (uncommitted or later changes, an entry where the candidate adds one) or holds an
    entry apply never writes, also with --force; files lists those paths;
    TIMEOUT - the execution deadline ran out before the application;
    UNVERIFIED - the test harness refuses the workspace, or protected files could not
    be verified; UNKNOWN - whether the application was committed cannot be established;
    FAILED - anything else (no such candidate, not approved, integrity or write failure).

    The status comes from the message when it carries one (CandidateMessage)."""

    def __new__(cls, ok: bool, files: List[str], message: str, status: Optional[str] = None):
        from makewand.execution_contract import STATUS_CODES
        result = tuple.__new__(cls, (ok, files, message))
        if ok:
            status = "PASSED"
        elif status is None:
            status = getattr(message, "status", None)
        result.status = status if ok or (status in STATUS_CODES and status != "PASSED") else "FAILED"
        return result

    def __getnewargs__(self):
        # copy and pickle rebuild the result through __new__, status included.
        return (self[0], self[1], self[2], self.status)

    @property
    def ok(self) -> bool:
        return self[0]

    @property
    def files(self) -> List[str]:
        return self[1]

    @property
    def message(self) -> str:
        return self[2]


class ApplicationTargetChanged(ValueError):
    """A target of the application no longer matches the preimage apply froze for it."""

    def __init__(self, message: str, path: str):
        super().__init__(message)
        self.path = path


class CandidateDeadlineExceeded(TimeoutError):
    pass


def _check_apply_deadline():
    from makewand.execution_runtime import current_context
    deadline = current_context().get("_deadline_monotonic")
    if deadline is not None and time.monotonic() >= deadline:
        raise CandidateDeadlineExceeded("候选应用的总执行期限已耗尽，保留封存候选")
    return deadline


def harness_source_tree_conflict(path: Union[str, Path]) -> Optional[str]:
    """Under the test harness, a candidate is never applied to makewand's own source tree.

    The rule is git_helper.harness_source_tree_conflict, the one definition, for
    every caller of apply (the CLI, a race, a multi-task DAG): a test that forgot
    its temporary workspace would write candidate files into the checkout under
    test. The source tree is git_helper._makewand_source_root(), the one seam tests
    replace with a stand-in checkout. Outside the harness
    (MAKEWAND_TEST_ISOLATION_ROOT unset) makewand may apply to its own repository
    like to any other.
    """
    from makewand.git_helper import harness_source_tree_conflict as guard
    return guard(path, "把候选应用到工作区")

def file_sha256(path: Path) -> Optional[str]:
    """Computes SHA-256 hex digest of a regular file. Returns None for links/missing."""
    if os.name == "nt":
        from makewand.native_windows import inspect_file
        try:
            return inspect_file(path)["sha256"]
        except (OSError, ValueError):
            return None
    if not path.is_file() or os.path.islink(path):
        return None
    try:
        h = hashlib.sha256()
        with open(path, "rb") as f:
            while chunk := f.read(65536):
                h.update(chunk)
        return h.hexdigest()
    except Exception:
        return None

def file_record(path: Path) -> Optional[Dict[str, Any]]:
    if os.name == "nt":
        from makewand.native_windows import inspect_file
        try:
            return inspect_file(path)
        except FileNotFoundError:
            return None
    digest = file_sha256(path)
    if digest is None:
        return None
    return {"sha256": digest, "mode": stat.S_IMODE(path.stat().st_mode)}


# Directory names never part of a delivered project, whatever Git reports:
# repository metadata and makewand's own sandbox scratch home.
_NEVER_PROJECT_DIRS = frozenset({".git", ".makewand_sandbox_home"})
# Records sealed with this scope bind project files below PROJECT_IGNORE_DIRS
# names (see build_manifest); older records are verified the way they were sealed.
MANIFEST_SCOPE = "project-files"


def _uncopied_output_names() -> frozenset:
    """Directory names whose untracked files never reach an isolated copy.

    git_helper.clone_isolated_worktree skips untracked files below these
    dependency and build output names (it keeps the deliverable files of the
    cache directories among CLONE_EXCLUDED_DIRS): git_helper.CLONE_UNCOPIED_DIRS,
    the one definition both use.
    """
    from makewand.git_helper import CLONE_UNCOPIED_DIRS
    return CLONE_UNCOPIED_DIRS


def _project_files_below(dir_path: Path, directories: List[str], host: bool = False) -> List[str]:
    """Paths below directories that Git tracks or would add (untracked and not ignored).

    Ignored means what `git status` and the isolated copies honour: .gitignore,
    .git/info/exclude and the user's global excludes file (a node_modules/ ignored
    only there is never hashed). With host, untracked files no isolated copy
    receives (below a _uncopied_output_names() directory such as an un-ignored
    node_modules/) are left out: Git does not list the untracked files of such a
    skipped directory at all. Empty outside a Git work tree or when Git cannot
    list them: the directories then stay skipped as dependency or build output.
    """
    from makewand.git_helper import find_git_root
    if not directories or not find_git_root(dir_path):
        return []
    uncopied = _uncopied_output_names() if host else frozenset()
    if host:
        listings = [(("--cached",), directories),
                    (("--others", "--exclude-standard"),
                     [directory for directory in directories if directory.rsplit("/", 1)[-1] not in uncopied])]
    else:
        listings = [(("--cached", "--others", "--exclude-standard"), directories)]
    found = []
    for options, selected in listings:
        for offset in range(0, len(selected), 256):
            code, out, _ = run_git_cmd(["git", "--literal-pathspecs", "ls-files", "-z", *options, "--",
                                        *selected[offset:offset + 256]], cwd=str(dir_path), binary=True)
            if code != 0:
                return []
            names = (os.fsdecode(name) for name in out.split(b"\0") if name)
            if "--others" in options and uncopied:
                # Also below a nested name (vendor/pkg/node_modules/), as the copy skips it.
                names = (name for name in names if uncopied.isdisjoint(name.split("/")))
            found.extend(names)
    return found


def _project_file_stat(dir_path: Path, relative: str) -> Optional[os.stat_result]:
    """lstat of a path Git listed, never through a symlinked or missing directory."""
    parts = relative.split("/")
    if any(part in ("", ".", "..") or part in _NEVER_PROJECT_DIRS for part in parts):
        return None
    parent = dir_path
    for part in parts[:-1]:
        parent = parent / part
        try:
            if not stat.S_ISDIR(os.lstat(parent).st_mode):
                return None
        except OSError:
            return None
    try:
        return os.lstat(parent / parts[-1])
    except OSError:
        return None


class ManifestLimitExceeded(OSError):
    """build_manifest stopped at a ManifestBudget limit: the tree is too large to bind."""


class ManifestBudget:
    """The workspace snapshot limits applied to one build_manifest call: at most
    MAKEWAND_WORKSPACE_SNAPSHOT_MAX_FILES files and _MAX_BYTES bytes bound - counted
    whether or not a digest is cached, so that the limit is a property of the tree -
    and _TIMEOUT_SECS seconds (artifact._get_snapshot_limits, the limits
    workspace_snapshot applies). The files and bytes are counted before anything is
    hashed: a tree over the limits fails closed at once, with a message naming the
    limit (ManifestLimitExceeded), instead of hashing for minutes. what names the tree."""

    HINT = "；可用 .gitignore 忽略数据、依赖与构建输出，或调高该上限"

    def __init__(self, what: str, clock=time.monotonic):
        from makewand.artifact import _get_snapshot_limits
        self.max_bytes, self.seconds, self.max_files = _get_snapshot_limits()
        self.what = what
        self.clock = clock
        self.files = self.bytes = 0
        self.deadline = clock() + self.seconds

    def count(self, size: int) -> None:
        self.files += 1
        self.bytes += size
        if self.files > self.max_files:
            raise ManifestLimitExceeded(f"{self.what}中需要计算摘要的文件超过 {self.max_files} 个 "
                                        f"(MAKEWAND_WORKSPACE_SNAPSHOT_MAX_FILES){self.HINT}")
        if self.bytes > self.max_bytes:
            raise ManifestLimitExceeded(f"{self.what}中需要计算摘要的文件超过 {self.max_bytes} 字节 "
                                        f"(MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES){self.HINT}")

    def check_time(self) -> None:
        if self.clock() > self.deadline:
            raise ManifestLimitExceeded(f"为{self.what}计算摘要超过 {self.seconds:g} 秒 "
                                        f"(MAKEWAND_WORKSPACE_SNAPSHOT_TIMEOUT_SECS){self.HINT}")


class ManifestCache:
    """The file records of one multi-stage run (a DAG), which binds the same trees again
    and again: a file's sha256 keyed by its stat identity (st_dev, st_ino, st_size,
    st_mtime_ns, st_ctime_ns), its mode always taken from the current stat. Writing a
    file, replacing it or changing its mode or links moves its inode change time, which
    only the kernel sets, so a digest is reused only for the very same, unchanged inode.
    A file whose timestamps were recent when it was read (within settle_ns, SETTLE_NS by
    default) is not cached: a write within the same timestamp tick right after the read
    would leave all five values unchanged. Holds at most MAX_ENTRIES digests."""

    SETTLE_NS = 2 * 10 ** 9
    MAX_ENTRIES = 200000

    def __init__(self, settle_ns: int = SETTLE_NS):
        self.settle_ns = settle_ns
        self._digests: Dict[Tuple[int, int, int, int, int], str] = {}

    @staticmethod
    def _key(info: os.stat_result) -> Tuple[int, int, int, int, int]:
        return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)

    def get(self, info: os.stat_result) -> Optional[str]:
        return self._digests.get(self._key(info))

    def put(self, info: os.stat_result, digest: str, read_started_ns: int) -> None:
        if (max(info.st_mtime_ns, info.st_ctime_ns) < read_started_ns - self.settle_ns
                and len(self._digests) < self.MAX_ENTRIES):
            self._digests[self._key(info)] = digest

    def __len__(self) -> int:
        return len(self._digests)


def _manifest_digest(path: str, budget: Optional[ManifestBudget]
                     ) -> Optional[Tuple[str, os.stat_result, os.stat_result, int]]:
    """(sha256, stat before and after reading, wall-clock ns when reading began) of the
    regular file path, read through a no-follow handle (the stats are the opened
    file's); None for a file that is no longer a regular file or cannot be read.
    budget's time limit is checked while reading (ManifestLimitExceeded)."""
    started = time.time_ns()
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0)
                     | getattr(os, "O_CLOEXEC", 0))
    except OSError:
        return None
    try:
        before = os.fstat(fd)
        if not stat.S_ISREG(before.st_mode):
            return None
        digest = hashlib.sha256()
        while True:
            if budget is not None:
                budget.check_time()
            chunk = os.read(fd, 1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
        after = os.fstat(fd)
    except ManifestLimitExceeded:
        raise
    except OSError:
        return None
    finally:
        os.close(fd)
    return digest.hexdigest(), before, after, started


def _manifest_record(path: str, info: os.stat_result, cache: Optional[ManifestCache],
                     budget: Optional[ManifestBudget]) -> Optional[Dict[str, Any]]:
    """file_record of the regular file path (info: its lstat), reusing cache's digest of
    the same unchanged inode. A file that changed while it was read is not cached."""
    if cache is not None:
        digest = cache.get(info)
        if digest is not None:
            return {"sha256": digest, "mode": stat.S_IMODE(info.st_mode)}
    if budget is not None:
        budget.check_time()
    result = _manifest_digest(path, budget)
    if result is None:
        return None
    digest, before, after, started = result
    if cache is not None and ManifestCache._key(before) == ManifestCache._key(after):
        cache.put(after, digest, started)
    return {"sha256": digest, "mode": stat.S_IMODE(after.st_mode)}


def build_manifest(dir_path: Path, project_files: bool = True, host: bool = False,
                   cache: Optional[ManifestCache] = None, budget: Optional[ManifestBudget] = None) -> Dict[str, Any]:
    """Bind regular file content AND permissions; old hash-only manifests fail closed.

    Directories named in PROJECT_IGNORE_DIRS (dependency, build and tool output)
    are skipped only where they are not part of the project: inside a Git work
    tree the files Git tracks or would add below them (a committed dist/ bundle,
    a shared .vscode/settings.json) are bound like any other file, so a candidate
    changing them can be sealed and applied. project_files=False reproduces the
    manifests of records sealed before MANIFEST_SCOPE, which skipped those names.

    host=True is for the user's workspace that isolated copies are cloned from
    (a freeze check): what no copy receives is left out - untracked files Git
    ignores (data, logs, local secrets: git_helper.list_workspace_ignored_paths)
    and untracked files below dependency or build output names (an un-ignored
    node_modules/ or dist/) - since no candidate can modify or delete them and
    hashing such a tree is most of the cost. Tracked files stay bound wherever they
    are. A candidate copy's manifest (the default) binds the files a stage created
    below those names.

    A multi-stage run passes a ManifestCache it keeps across its stages and a
    ManifestBudget per call: the snapshot limits, checked before anything is
    hashed. Records are the same with or without them.
    """
    dir_path = Path(dir_path)
    if os.name == "nt":
        from makewand.native_windows import manifest
        return manifest(dir_path)
    manifest = {}
    if not dir_path.exists():
        return manifest
    from makewand.constants import PROJECT_IGNORE_DIRS
    ignored = frozenset()
    if host:
        from makewand.git_helper import list_workspace_ignored_paths
        ignored = frozenset(list_workspace_ignored_paths(dir_path) or ())
    skipped = []
    found: List[Tuple[str, str, os.stat_result]] = []

    def bind(relative: str, path: str, info: Optional[os.stat_result]) -> None:
        if info is not None and stat.S_ISREG(info.st_mode):
            if budget is not None:
                budget.count(info.st_size)
            found.append((relative, path, info))

    for root, directories, files in os.walk(str(dir_path)):
        rel_dir = Path(root).relative_to(dir_path)
        kept = []
        for directory in directories:
            if directory in _NEVER_PROJECT_DIRS:
                continue
            relative = (rel_dir / directory).as_posix()
            if directory in PROJECT_IGNORE_DIRS:
                skipped.append(relative)
                continue
            if relative + "/" in ignored:
                continue
            kept.append(directory)
        directories[:] = kept
        for f in files:
            relative = (rel_dir / f).as_posix()
            if relative in ignored:
                continue
            path = os.path.join(root, f)
            try:
                info = os.lstat(path)
            except OSError:
                continue
            bind(relative, path, info)  # never a link: only regular files are bound
    if project_files:
        for relative in _project_files_below(dir_path, skipped, host=host):
            bind(relative, str(dir_path / relative), _project_file_stat(dir_path, relative))
    for relative, path, info in found:
        record = _manifest_record(path, info, cache, budget)
        if record is not None:
            manifest[relative] = record
    return manifest


def _binds_project_files(race: Dict[str, Any]) -> bool:
    """Whether a saved record's manifests were built with build_manifest's project files."""
    return race.get("manifest_scope") == MANIFEST_SCOPE


def remove_new_generated_bytecode(workspace: Union[str, Path], baseline_manifest: Dict[str, Any]) -> List[str]:
    """Remove reproducible, untracked Python caches before the first seal.

    Existing baseline files, tracked files and arbitrary cache contents remain
    inputs. Never use this during approval or application of a sealed candidate.
    """
    import importlib.util
    import marshal
    root = Path(workspace).resolve()
    code, output, error = run_git_cmd(["git", "ls-files", "--cached", "-z", "--", "."], cwd=str(root), binary=True)
    if code != 0:
        raise OSError(f"cannot protect tracked files before cache cleanup: {os.fsdecode(error)}")
    protected = set(baseline_manifest) | {os.fsdecode(name) for name in output.split(b"\0") if name}
    removed = []
    for directory, directories, files in os.walk(root, followlinks=False):
        directories[:] = [name for name in directories if name != ".git" and not (Path(directory) / name).is_symlink()]
        if Path(directory).name != "__pycache__":
            continue
        for name in files:
            path = Path(directory) / name
            relative = path.relative_to(root).as_posix()
            if relative in protected or path.is_symlink() or not path.is_file() or not name.endswith(".pyc"):
                continue
            try:
                source = Path(importlib.util.source_from_cache(str(path)))
                _verify_safe_target_path(root, source.relative_to(root).as_posix())
                if source.is_symlink() or not source.is_file() or source.stat().st_size > 4 * 1024 * 1024:
                    continue
                tag = f"{source.stem}.{sys.implementation.cache_tag}"
                optimization = next((level for level in (0, 1, 2)
                                     if name == tag + (f".opt-{level}" if level else "") + ".pyc"), None)
                if optimization is None or path.stat().st_size > 16 * 1024 * 1024:
                    continue
                data = path.read_bytes()
                if len(data) < 16 or data[:4] != importlib.util.MAGIC_NUMBER:
                    continue
                source_bytes = source.read_bytes()
                matches = False
                for filename in {str(source), source.name, source.relative_to(root).as_posix()}:
                    # Keep the trusted code object referenced while serializing,
                    # matching CPython's pyc writer. Never deserialize candidate
                    # marshal data, whose length fields are untrusted.
                    compiled = compile(source_bytes, filename, "exec", dont_inherit=True, optimize=optimization)
                    if marshal.dumps(compiled) == data[16:]:
                        matches = True
                        break
                if not matches:
                    continue
                _atomic_remove(str(root), relative)
                removed.append(relative)
                # Remove only the now-empty generated directory; preserve any
                # neighboring source, tracked data, links or unknown artifacts.
                try:
                    path.parent.rmdir()
                except OSError:
                    pass
            except (OSError, ValueError, SyntaxError, EOFError, TypeError, MemoryError):
                continue
    return sorted(removed)


def _renameat2_function():
    """Linux renameat2 through libc, or None where the platform lacks it."""
    if not _RENAMEAT2:
        function = None
        if sys.platform.startswith("linux"):
            try:
                import ctypes
                libc = ctypes.CDLL(None, use_errno=True)
                function = libc.renameat2
                function.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
                function.restype = ctypes.c_int
            except (OSError, AttributeError):
                function = None
        _RENAMEAT2.append(function)
    return _RENAMEAT2[0]


def _renameat2(directory, source, target, flags):
    """Rename within one directory handle; False means "unsupported here".

    ENOSYS (old kernel) and EINVAL/EOPNOTSUPP (filesystem without the flag)
    leave both names untouched, so callers may fall back safely.
    """
    function = _renameat2_function()
    if function is None:
        return False
    import ctypes
    if function(directory, os.fsencode(source), directory, os.fsencode(target), flags) == 0:
        return True
    error = ctypes.get_errno()
    if error in (errno.ENOSYS, errno.EINVAL, errno.EOPNOTSUPP):
        return False
    raise OSError(error, os.strerror(error), target)


def _record_at(directory, name):
    """file_record for a name under an open directory handle, without links."""
    fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=directory)
    with os.fdopen(fd, "rb") as stream:
        opened = os.fstat(stream.fileno())
        if not stat.S_ISREG(opened.st_mode):
            return {"type": "non-regular"}
        digest = hashlib.sha256()
        while chunk := stream.read(65536):
            digest.update(chunk)
    return {"sha256": digest.hexdigest(), "mode": stat.S_IMODE(opened.st_mode)}


def _fsync_directory(path):
    """Durably publish directory entries; only platforms without O_DIRECTORY skip."""
    if not hasattr(os, "O_DIRECTORY"):
        return
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def _atomic_copy(workspace: str, rel_path: str, source: Path, expected=None, *, temporary_name=None,
                 workspace_identity=None, before_replace=None, expected_preimage=_PREIMAGE_UNCHECKED):
    """Write through directory handles and atomically replace, preserving mode.

    The source is checked while copying, so a changed candidate cannot win the
    gap between manifest validation and application. No shared inode is edited.

    With ``expected_preimage`` the publication itself is the check on Linux:
    a new file uses RENAME_NOREPLACE, and an existing one is exchanged and the
    displaced file verified afterwards; a mismatch exchanges back, leaving the
    newer user file in place and the candidate at a reported conflict path.
    """
    if os.name == "nt":
        from makewand.native_windows import atomic_copy
        return atomic_copy(workspace, rel_path, source, expected, temporary_name=temporary_name,
                           before_replace=before_replace)
    parts = Path(rel_path).parts
    if not parts or Path(rel_path).is_absolute() or any(x in (".", "..") for x in parts):
        raise ValueError("invalid workspace-relative path")
    directory_flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    directory = os.open(workspace, directory_flags)
    temp_name = temporary_name if temporary_name is not None else ".makewand-" + uuid.uuid4().hex
    if (not isinstance(temp_name, str) or len(temp_name) != len(".makewand-") + 32
            or not temp_name.startswith(".makewand-")
            or any(character not in "0123456789abcdef" for character in temp_name[len(".makewand-"):])):
        os.close(directory)
        raise ValueError("invalid registered application temporary name")
    created = False
    try:
        if workspace_identity is not None:
            opened = os.fstat(directory)
            if [opened.st_dev, opened.st_ino] != workspace_identity:
                raise ValueError("Application opened workspace identity changed")
        for component in parts[:-1]:
            try:
                os.mkdir(component, 0o755, dir_fd=directory)
                os.fsync(directory)
            except FileExistsError:
                pass
            child = os.open(component, directory_flags, dir_fd=directory)
            os.close(directory)
            directory = child
        source_fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(source_fd, "rb") as src:
            source_stat = os.fstat(src.fileno())
            if not stat.S_ISREG(source_stat.st_mode):
                raise ValueError("source must be a regular file")
            mode = stat.S_IMODE(source_stat.st_mode)
            if expected is not None and mode & 0o7000:
                raise ValueError("candidate cannot introduce special permission bits")
            fd = os.open(temp_name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory)
            created = True
            with os.fdopen(fd, "wb") as dst:
                digest = hashlib.sha256()
                while chunk := src.read(65536):
                    digest.update(chunk)
                    dst.write(chunk)
                if expected is not None and expected != {"sha256": digest.hexdigest(), "mode": mode}:
                    raise ValueError("candidate changed during apply")
                dst.flush()
                os.fchmod(dst.fileno(), mode)
                os.fsync(dst.fileno())
        if before_replace is not None:
            before_replace()
        conflict = "Application target changed concurrently before atomic replacement: " + rel_path
        published = False
        if expected_preimage is None:
            try:
                published = _renameat2(directory, temp_name, parts[-1], _RENAME_NOREPLACE)
            except FileExistsError:
                raise ValueError(conflict) from None
        elif expected_preimage is not _PREIMAGE_UNCHECKED:
            try:
                published = _renameat2(directory, temp_name, parts[-1], _RENAME_EXCHANGE)
            except FileNotFoundError:
                raise ValueError(conflict) from None
            if published:
                # temp_name now holds the displaced target: never unlink it
                # unless it is exactly the frozen (and backed-up) preimage.
                created = False
                try:
                    displaced = _record_at(directory, temp_name)
                except OSError:
                    displaced = None
                if displaced != expected_preimage:
                    _renameat2(directory, temp_name, parts[-1], _RENAME_EXCHANGE)
                    conflict_name = ".makewand-conflict-" + uuid.uuid4().hex
                    os.rename(temp_name, conflict_name, src_dir_fd=directory, dst_dir_fd=directory)
                    os.fsync(directory)
                    conflict_path = Path(rel_path).parent / conflict_name
                    raise ValueError(conflict + "; newer workspace content kept, candidate preserved at "
                                     + conflict_path.as_posix())
                os.unlink(temp_name, dir_fd=directory)
        if not published:
            # No renameat2 here: best-effort verify-then-replace.
            if expected_preimage is not _PREIMAGE_UNCHECKED:
                if _application_target_record(workspace, rel_path) != expected_preimage:
                    raise ValueError(conflict)
            os.replace(temp_name, parts[-1], src_dir_fd=directory, dst_dir_fd=directory)
        created = False
        os.fsync(directory)
    finally:
        if created:
            os.unlink(temp_name, dir_fd=directory)
        os.close(directory)

def _metadata_text(data: Dict[str, Any]) -> str:
    """Candidate metadata as JSON text that UTF-8 can hold. A file name with undecodable
    bytes (a surrogate escape from os.fsdecode or git) cannot be written as UTF-8, so
    then every non-ASCII character is escaped instead: json.load gives back the very
    same names (git_helper._json_text), which apply turns into the original bytes."""
    from makewand.git_helper import _json_text
    return _json_text(data)


def _write_private_json(path: Path, data: Dict[str, Any]) -> None:
    """Publish complete private metadata atomically, without following links."""
    if path.is_symlink():
        raise ValueError("candidate metadata must not be a symlink")
    text = _metadata_text(data)
    fd, temporary = tempfile.mkstemp(prefix=".meta-", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write(text)
            handle.flush()
            os.fsync(handle.fileno())
        if os.name == "nt":
            from makewand.native_windows import replace_file
            replace_file(temporary, path)
        else:
            os.replace(temporary, path)
        if os.name == "posix":
            directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


IN_PROGRESS_LOCK_NAME = ".in-progress.lock"


class CandidateClaim:
    """Marks a candidate directory as still being produced until released.

    Pruning and ``discard --all`` skip a directory whose owner holds this lock.
    The lock ends with the owning process, so a directory abandoned by a crash
    is pruned like any other unapplied candidate.
    """

    def __init__(self, directory: Union[str, Path]):
        self.path = Path(directory) / IN_PROGRESS_LOCK_NAME
        fd = os.open(_fs_path(self.path), os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0), 0o600)
        self._handle = os.fdopen(fd, "r+b")
        try:
            fcntl.flock(self._handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BaseException:
            self._handle.close()
            raise

    def release(self) -> None:
        """Hand the directory back to normal candidate management (idempotent)."""
        handle, self._handle = self._handle, None
        if handle is None:
            return
        try:
            # Unlink while still locked: a released directory carries no lock file.
            os.unlink(_fs_path(self.path))
        except OSError:
            pass
        handle.close()


def candidate_in_progress(directory: Union[str, Path]) -> bool:
    """True while a CandidateClaim holds the directory.

    A lock that cannot be tested counts as held: never delete what may be in use.
    """
    path = Path(directory) / IN_PROGRESS_LOCK_NAME
    try:
        fd = os.open(_fs_path(path), os.O_RDWR | getattr(os, "O_NOFOLLOW", 0))
    except FileNotFoundError:
        return False
    except OSError:
        return True
    with os.fdopen(fd, "r+b") as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            return True
        fcntl.flock(handle, fcntl.LOCK_UN)
    return False


def _write_application_journal(path, data):
    if len(_metadata_text(data).encode("utf-8")) > 16 * 1024 * 1024:
        raise ValueError("Application journal exceeds its recovery size limit")
    _write_private_json(path, data)


def _application_workspace_identity(root, expected):
    identity = os.stat(root, follow_symlinks=False)
    if not stat.S_ISDIR(identity.st_mode) or [identity.st_dev, identity.st_ino] != expected:
        raise ValueError("Interrupted application workspace identity changed")


def _application_target_record(root, relative):
    target = _verify_safe_target_path(root, relative)
    if not os.path.lexists(_fs_path(target)):
        return None
    record = file_record(target)
    if record is None:
        raise ValueError("Application target is not a regular file: " + relative)
    return record


def _application_target_security(root, relative):
    """Host transaction metadata; Windows ACLs never enter model manifests."""
    if os.name != "nt":
        return None
    target = _verify_safe_target_path(root, relative)
    if not os.path.lexists(_fs_path(target)):
        return None
    from makewand.native_windows import application_security
    return application_security(target)


def _validate_application_security(root, item, current):
    if os.name != "nt":
        return
    if "before_security" not in item or "after_security" not in item:
        raise ValueError("Interrupted Windows application lacks frozen file security")
    for key in ("before_security", "after_security"):
        value = item[key]
        if value is not None and (not isinstance(value, str) or not value or len(value) > 16384):
            raise ValueError("Invalid interrupted application file security")
    if item["before"] is not None and (item["before_security"] is None
            or not isinstance(item.get("before_security_descriptor"), str)
            or not item["before_security_descriptor"] or len(item["before_security_descriptor"]) > 1024 * 1024):
        raise ValueError("Interrupted Windows application lacks original file security")
    if item["before"] is not None:
        from makewand.native_windows import validate_application_security_descriptor
        validate_application_security_descriptor(item["before_security_descriptor"], item["before_security"])
    allowed = []
    if current == item["before"]:
        allowed.append(item["before_security"])
    if current == item["after"]:
        allowed.append(item["after_security"])
    if _application_target_security(root, item["path"]) not in allowed:
        raise ValueError("Interrupted apply conflicts with later file security changes: " + item["path"])


def _validate_application_temp(root, item, folder):
    relative = item.get("temp")
    if relative is None:  # Older journals did not register sibling scratch files.
        return None
    if not isinstance(relative, str):
        raise ValueError("Invalid interrupted application temporary path")
    temp = Path(relative)
    name = temp.name
    if (relative != temp.as_posix() or temp.is_absolute()
            or temp.parent != Path(item["path"]).parent
            or len(name) != len(".makewand-") + 32 or not name.startswith(".makewand-")
            or any(character not in "0123456789abcdef" for character in name[len(".makewand-"):])):
        raise ValueError("Invalid interrupted application temporary path")
    target = _verify_safe_target_path(root, relative)
    api_target = _fs_path(target)
    if not os.path.lexists(api_target):
        return target
    if api_target.is_symlink() or not api_target.is_file() or api_target.stat().st_size > 512 * 1024 * 1024:
        raise ValueError("Interrupted application temporary path changed")
    size = api_target.stat().st_size
    current = file_sha256(target)
    for bucket, record in (("preimages", item.get("before")), ("postimages", item.get("after"))):
        if record is None:
            continue
        source = folder / bucket / item["path"]
        if not _fs_path(source).exists():
            continue
        digest = hashlib.sha256()
        remaining = size
        if os.name == "nt":
            from makewand.native_windows import regular_reader
            source_reader = regular_reader(source)
        else:
            source_reader = os.fdopen(os.open(source, os.O_RDONLY | os.O_NOFOLLOW), "rb")
        with source_reader as stream:
            while remaining:
                chunk = stream.read(min(65536, remaining))
                if not chunk:
                    break
                digest.update(chunk)
                remaining -= len(chunk)
        if remaining == 0 and digest.hexdigest() == current:
            return target
    raise ValueError("Interrupted application temporary content changed")


def _remove_application_temp(root, item, folder, *, workspace_identity=None):
    target = _validate_application_temp(root, item, folder)
    if target is not None and os.path.lexists(_fs_path(target)):
        _atomic_remove(root, item["temp"], workspace_identity=workspace_identity)


def _missing_application_dirs(root, relative):
    result = []
    parent = Path(relative).parent
    while parent != Path("."):
        if os.path.lexists(_fs_path(Path(root) / parent)):
            break
        result.append(parent.as_posix())
        parent = parent.parent
    return result


def _remove_application_dirs(root, directories, identity):
    for relative in directories:
        _application_workspace_identity(root, identity)
        if os.name == "nt":
            from makewand.native_windows import atomic_remove
            try:
                atomic_remove(root, relative, directory=True)
            except FileNotFoundError:
                continue
            except OSError as error:
                if error.errno in (errno.ENOTEMPTY, errno.EEXIST) or getattr(error, "winerror", None) == 145:
                    break
                raise
        else:
            flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
            directory = os.open(root, flags)
            try:
                opened = os.fstat(directory)
                if [opened.st_dev, opened.st_ino] != identity:
                    raise ValueError("Interrupted application workspace identity changed")
                parts = Path(relative).parts
                for component in parts[:-1]:
                    child = os.open(component, flags, dir_fd=directory)
                    os.close(directory)
                    directory = child
                os.rmdir(parts[-1], dir_fd=directory)
                os.fsync(directory)
            except FileNotFoundError:
                continue
            except OSError as error:
                if error.errno in (errno.ENOTEMPTY, errno.EEXIST):
                    break
                raise
            finally:
                os.close(directory)


def _restore_application_entry(root, item, folder, *, workspace_identity=None):
    if item["action"] == "restore":
        backup = folder / "preimages" / item["path"]
        if os.name == "nt":
            from makewand.native_windows import atomic_copy
            def validate_restoration(security):
                current = _application_target_record(root, item["path"])
                if current not in (item["before"], item["after"]):
                    raise ValueError("Interrupted apply conflicts with later workspace changes: " + item["path"])
                _validate_application_security(root, item, current)
                if security != item["before_security"]:
                    raise ValueError("Interrupted application original file security changed")
            atomic_copy(root, item["path"], backup, item["before"], restore_acl=True,
                        temporary_name=Path(item["temp"]).name if item.get("temp") else None,
                        restore_security=item["before_security_descriptor"], before_replace=validate_restoration)
        else:
            _atomic_copy(root, item["path"], backup, item["before"],
                         temporary_name=Path(item["temp"]).name if item.get("temp") else None,
                         workspace_identity=workspace_identity)
    else:
        _atomic_remove(root, item["path"], workspace_identity=workspace_identity,
                       expected_security=item.get("after_security") if os.name == "nt" else _NO_SECURITY_EXPECTATION)


def _verify_restored_application(root, entries, identity):
    """A completed restore is authority only after its actual preimages match."""
    _application_workspace_identity(root, identity)
    for item in entries:
        _application_workspace_identity(root, identity)
        if _application_target_record(root, item["path"]) != item["before"]:
            raise ValueError("Interrupted apply rollback conflicts with restored preimage changes: " + item["path"])
        if os.name == "nt" and _application_target_security(root, item["path"]) != item["before_security"]:
            raise ValueError("Interrupted apply rollback conflicts with restored file security changes: " + item["path"])
    _application_workspace_identity(root, identity)


def _recover_application_journals(base_cwd):
    """Recover durable preimages while refusing later user modifications.

    The caller holds the workspace and apply locks. Every interrupted mutation
    must match its frozen preimage or postimage before *any* rollback begins.
    """
    root = os.path.realpath(base_cwd)
    if not config.BACKUPS_DIR.exists():
        return []
    recovered = []
    plans = []
    for folder in sorted(config.BACKUPS_DIR.iterdir()):
        if folder.is_symlink() or not folder.is_dir():
            continue
        path = folder / "journal.json"
        if not path.exists():
            continue
        if path.is_symlink() or path.stat().st_size > 16 * 1024 * 1024:
            raise ValueError("Invalid interrupted application journal")
        with path.open(encoding="utf-8") as stream:
            frozen = json.load(stream)
        # Historical list journals were written only after successful apply.
        if isinstance(frozen, list):
            continue
        if not isinstance(frozen, dict) or frozen.get("workspace") != root:
            continue
        if frozen.get("state") in ("committed", "rolled_back"):
            continue
        _application_workspace_identity(root, frozen.get("workspace_identity"))
        if frozen.get("schema") != 1 or frozen.get("state") not in ("prepared", "failed") or not isinstance(frozen.get("entries"), list):
            raise ValueError("Invalid interrupted application journal schema")
        entries = frozen["entries"]
        if os.name == "nt" and entries and frozen.get("security_schema") != 1:
            raise ValueError("Interrupted Windows application lacks frozen file security; preserve evidence for manual review")
        pending = []
        seen = set()
        for item in entries:
            if not isinstance(item, dict) or item.get("path") in seen:
                raise ValueError("Invalid interrupted application journal entry")
            relative = item.get("path")
            if not isinstance(relative, str):
                raise ValueError("Invalid interrupted application journal path")
            parts = relative.split("/")
            if any(not part or part in (".", "..", ".git") for part in parts) or Path(relative).is_absolute():
                raise ValueError("Unsafe interrupted application journal path")
            seen.add(relative)
            for directory in item.get("created_dirs", []):
                if (not isinstance(directory, str) or Path(directory).is_absolute()
                        or Path(directory).as_posix() != directory
                        or any(part in (".", "..", ".git") for part in Path(directory).parts)
                        or not relative.startswith(directory + "/")):
                    raise ValueError("Invalid interrupted application directory")
            current = _application_target_record(root, relative)
            before, after = item.get("before"), item.get("after")
            if current not in (before, after):
                raise ValueError(f"Interrupted apply conflicts with later workspace changes: {relative}")
            _validate_application_security(root, item, current)
            if item.get("action") == "restore":
                backup = (folder / "preimages").joinpath(*parts)
                _verify_safe_target_path(folder / "preimages", relative)
                if before is None or file_record(backup) != before:
                    raise ValueError(f"Interrupted apply backup changed: {relative}")
            elif item.get("action") != "delete" or before is not None:
                raise ValueError("Invalid interrupted application journal action")
            if "postimage" in item:
                if item["postimage"] != "postimages/" + relative:
                    raise ValueError("Invalid interrupted application postimage path")
                postimage = _verify_safe_target_path(folder / "postimages", relative)
                if after is None or file_record(postimage) != after:
                    raise ValueError("Interrupted application postimage changed: " + relative)
            _validate_application_temp(root, item, folder)
            pending.append(item)
        plans.append((folder, path, frozen, pending))
    # Validate all journals and their preimages before changing even one file.
    for folder, path, frozen, pending in plans:
        for item in reversed(pending):
            _application_workspace_identity(root, frozen["workspace_identity"])
            current = _application_target_record(root, item["path"])
            if current not in (item["before"], item["after"]):
                raise ValueError("Interrupted apply conflicts with later workspace changes: " + item["path"])
            _validate_application_security(root, item, current)
            _remove_application_temp(root, item, folder, workspace_identity=frozen["workspace_identity"])
            if current != item["before"]:
                _restore_application_entry(root, item, folder, workspace_identity=frozen["workspace_identity"])
            _remove_application_dirs(root, item.get("created_dirs", []), frozen["workspace_identity"])
        _verify_restored_application(root, pending, frozen["workspace_identity"])
        frozen["state"] = "rolled_back"
        _write_application_journal(path, frozen)
        recovered.append(str(folder))
    return recovered


def _input_manifest(directory: Path) -> Dict[str, Any]:
    from makewand.artifact import workspace_snapshot
    # Lists round-trip through metadata JSON; tuples would compare unequal.
    return {path: list(record) for path, record in workspace_snapshot(directory).items()}


def _export_baseline(source: Path, revision: str, destination: Path) -> bool:
    """Read fixed Git blobs, never checkout mutable HEAD or invoke filters."""
    if not isinstance(revision, str) or len(revision) not in (40, 64) or any(character not in "0123456789abcdefABCDEF" for character in revision):
        return False
    code, tree, _ = run_git_cmd(["git", "--no-replace-objects", "ls-tree", "-r", "-z", revision],
                              cwd=str(source), binary=True)
    if code != 0:
        return False
    destination.mkdir(mode=0o700, parents=True, exist_ok=True)
    for entry in tree.split(b"\0"):
        if not entry:
            continue
        header, name = entry.split(b"\t", 1)
        mode, kind, oid = header.split()
        relative = os.fsdecode(name)
        parts = Path(relative).parts
        if Path(relative).is_absolute() or not parts or any(part in ("..", ".git") for part in parts):
            raise ValueError("invalid frozen baseline path")
        if kind != b"blob" or mode not in (b"100644", b"100755"):
            raise ValueError("hybrid baseline contains unsupported links or submodules")
        code, content, error = run_git_cmd(["git", "--no-replace-objects", "cat-file", "blob", os.fsdecode(oid)],
                                         cwd=str(source), binary=True)
        if code != 0:
            raise OSError(f"cannot export frozen baseline: {error}")
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(content)
        target.chmod(0o755 if mode == b"100755" else 0o644)
    return True


def _candidate_seal_error_unchecked(agent: Dict[str, Any], race: Dict[str, Any]) -> Optional[str]:
    path = Path(agent.get("path") or "")
    if not agent.get("path") or not path.is_dir() or path.is_symlink():
        return "候选工作区不存在或为符号链接"
    expected = agent.get("manifest")
    if not isinstance(expected, dict) or build_manifest(path, project_files=_binds_project_files(race)) != expected:
        return "候选文件哈希或权限自封存后发生变化"
    if "input_manifest" in agent and _input_manifest(path) != agent["input_manifest"]:
        return "候选完整输入自封存后发生变化"
    changes = agent.get("changes")
    if not isinstance(changes, dict) or changes != get_candidate_files_changed(
            path, agent.get("baseline_commit") or race.get("baseline_commit")):
        return "候选 Git 变更计划自封存后发生变化"
    for relative, status in changes.items():
        parts = Path(relative).parts
        if not parts or Path(relative).is_absolute() or any(part in ("..", ".git") for part in parts):
            return "候选变更计划包含非法路径"
        _verify_safe_target_path(path, relative)
        if status not in ("A", "M", "D") or (status == "D") != (relative not in expected):
            return "候选变更计划与已封存文件不一致"
    return None


def _unreviewed_winner_take_all_approval(race: Dict[str, Any], label: str) -> bool:
    """Approvals written by older versions' Winner-Take-All without any review call.

    Those versions recorded review_passed together with a synthesized verdict
    line followed by the Winner-Take-All note; current races record approval only
    after an independent read-only review.
    """
    report = race.get("judge_report")
    synthesized = (f'MAKEWAND_RACE_VERDICT: {{"pass": true, "winner": "{label}", "defects": []}}\n'
                   f"[Winner-Take-All] 选手 {label} ")
    return isinstance(report, str) and race.get("winner") == label and report.startswith(synthesized)


def describe_race_outcome(race: Dict[str, Any]) -> Tuple[str, str]:
    """What actually selected a saved record's result, for listings: (kind, label).

    kind is "dag" (a multi-stage DAG's cumulative result), "judge" (A/B judge
    approval), "winner_take_all" (first finisher approved by an independent
    read-only review), "legacy_winner_take_all" (an older version's Winner-Take-All
    approval without any review, refused by apply without --force) or "undecided".
    """
    from makewand.review_contract import REVIEW_PASSED
    candidates = race.get("candidates") if isinstance(race.get("candidates"), dict) else {}
    report = race.get("judge_report") if isinstance(race.get("judge_report"), str) else ""
    winner = race.get("winner")
    dag = race.get("dag")
    if isinstance(dag, dict) or str(race.get("race_id") or "").startswith("dg_"):
        dag = dag if isinstance(dag, dict) else {}
        stages, total = dag.get("stages") or [], dag.get("total")
        complete = dag.get("complete") if "complete" in dag else not str(race.get("prompt") or "").startswith("[部分结果")
        parts = [f"多阶段 DAG {'完整' if complete else '部分'}累积结果"
                 + (f" ({len(stages)}/{total} 个子任务)" if stages and total else "")]
        if dag.get("blocked"):
            what = ("makewand apply 无法写入的条目" if dag["blocked"].get("reason") == "unwritable"
                    else "符号链接/子模块条目")
            parts.append(f"含{what} ({', '.join(dag['blocked'].get('paths') or [])})，"
                         "makewand apply 无法交付，需从暂存区人工恢复")
        elif (candidates.get("A") or {}).get("review_passed") is not True:
            parts.append("累积改动未获审查批准 (apply 需人工确认 --force)")
        else:
            parts.append("各代码子任务均已通过本地测试与独立审查")
        if race.get("applied_at"):
            parts.append("已应用")
        return "dag", "，".join(parts)
    hybrid = candidates.get("M")
    if isinstance(hybrid, dict):
        hybrid_note = "；混合候选 M " + ("已获独立复审批准" if hybrid.get("review_passed") is True else "待独立复审")
    else:
        hybrid_note = ""
    if winner in ("A", "B"):
        if _unreviewed_winner_take_all_approval(race, winner):
            return ("legacy_winner_take_all",
                    f"旧版 Winner-Take-All 记录: 选手 {winner} 未经任何独立复审直接判胜 (apply 需人工确认 --force){hybrid_note}")
        if (candidates.get(winner) or {}).get("review_passed") is not True:
            return "undecided", f"选手 {winner} (记录未获批准，apply 需人工确认 --force){hybrid_note}"
        if report.startswith(f"[Winner-Take-All] 选手 {winner} "):
            if f"结论: {REVIEW_PASSED}" in report.splitlines()[0]:
                return "winner_take_all", f"Winner-Take-All 胜出: 选手 {winner} (率先通过本地测试，并已通过独立只读复审){hybrid_note}"
            return "winner_take_all", f"Winner-Take-All 胜出: 选手 {winner} (记录中未见独立复审通过结论){hybrid_note}"
        return "judge", f"主裁推荐: 选手 {winner}{hybrid_note}"
    if report.startswith("[Winner-Take-All] 选手 "):
        side = report[len("[Winner-Take-All] 选手 "):][:1]
        return "undecided", f"未决 (Winner-Take-All 选手 {side} 未通过独立复审){hybrid_note}"
    return "undecided", f"未决{hybrid_note}"


def record_application_obstacles(race: Dict[str, Any], label: str = "A") -> Dict[str, str]:
    """application_obstacles of a saved record's candidate against its workspace as it is now.

    Empty when the record carries no change plan or its workspace is gone; apply
    itself reports those cases.
    """
    candidate = (race.get("candidates") or {}).get(label)
    changes = candidate.get("changes") if isinstance(candidate, dict) else None
    base_cwd = race.get("base_cwd")
    if not isinstance(changes, dict) or not isinstance(base_cwd, str) or not os.path.isdir(base_cwd):
        return {}
    try:
        return application_obstacles(base_cwd, {path: status for path, status in changes.items()
                                                if isinstance(path, str) and path})
    except (OSError, ValueError):
        return {}


def _candidate_seal_error(agent: Dict[str, Any], race: Dict[str, Any]) -> Optional[str]:
    try:
        _race_protection(race).verify(agent.get("path"))
        return _candidate_seal_error_unchecked(agent, race)
    except ProtectionError as exc:
        return CandidateMessage(f"候选受保护文件校验失败: {exc}", exc.status)
    except (OSError, ValueError, KeyError, TypeError) as exc:
        return f"候选完整性校验失败: {exc}"


def _verify_safe_target_path(base_cwd: Union[str, Path], rel_path: str) -> Path:
    if os.name == "nt":
        from makewand.native_windows import validate_target
        return validate_target(base_cwd, rel_path)
    canonical_base = os.path.realpath(base_cwd)
    cur = Path(base_cwd)
    for part in Path(rel_path).parts:
        cur = cur / part
        if os.path.islink(cur) or cur.is_symlink():
            raise ValueError(f"安全越界风险: 路径组件 {part} 包含符号链接")
        if cur.exists():
            resolved = os.path.realpath(cur)
            if not (resolved == canonical_base or resolved.startswith(canonical_base + os.sep)):
                raise ValueError(f"安全越界风险: 路径组件 {part} 逃逸出工作区 ({resolved})")
    return cur


def _atomic_remove(workspace: str, rel_path: str, *, workspace_identity=None,
                   expected_security=_NO_SECURITY_EXPECTATION):
    """Remove a workspace entry without following mutable parent symlinks."""
    if os.name == "nt":
        from makewand.native_windows import atomic_remove
        if expected_security is _NO_SECURITY_EXPECTATION:
            return atomic_remove(workspace, rel_path)
        return atomic_remove(workspace, rel_path, expected_security=expected_security)
    parts = Path(rel_path).parts
    if not parts or Path(rel_path).is_absolute() or any(x in (".", "..") for x in parts):
        raise ValueError("invalid workspace-relative path")
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    directory = os.open(workspace, flags)
    try:
        if workspace_identity is not None:
            opened = os.fstat(directory)
            if [opened.st_dev, opened.st_ino] != workspace_identity:
                raise ValueError("Application opened workspace identity changed")
        for component in parts[:-1]:
            child = os.open(component, flags, dir_fd=directory)
            os.close(directory)
            directory = child
        try:
            os.unlink(parts[-1], dir_fd=directory)
        except FileNotFoundError:
            pass
        os.fsync(directory)
    finally:
        os.close(directory)

def get_candidate_files_changed(candidate_dir: Path, baseline_commit: Optional[str] = None) -> Dict[str, str]:
    """
    Returns a dict mapping relative file path to change status ('M' modified, 'A' added, 'D' deleted).
    Captures BOTH uncommitted changes AND commits made by candidate since baseline_commit.
    Uses NUL-delimited parsing (--no-renames --name-status -z) and os.fsdecode to safely handle
    spaces, tabs, renames, and binary paths.
    """
    changes = {}

    # 1. Compare directly against baseline_commit (covers both committed changes and working tree changes)
    out_b, error = _read_workspace_diff(str(candidate_dir), baseline_commit,
                                      arguments=("--no-renames", "--name-status", "-z"), binary=True)
    if error:
        raise OSError(error)

    if out_b:
        tokens = out_b.split(b"\0")
        i = 0
        while i < len(tokens) - 1:
            st_b = tokens[i]
            if not st_b:
                i += 1
                continue
            path_b = tokens[i + 1]
            i += 2
            if not path_b:
                continue
            st = os.fsdecode(st_b).strip()
            path = os.fsdecode(path_b)
            if st.startswith("D"):
                changes[path] = "D"
            elif st.startswith("A"):
                changes[path] = "A"
            else:
                changes[path] = "M"

    # 2. Also incorporate uncommitted worktree changes with safe 2-token rename parsing
    code, out_b, error = run_git_cmd(
        ["git", "status", "-z", "--porcelain", "--untracked-files=all"],
        cwd=str(candidate_dir), binary=True,
    )
    if code != 0:
        raise OSError(f"Cannot read candidate Git status: {error}")
    if code == 0 and out_b:
        tokens = [t for t in out_b.split(b"\0") if t]
        idx = 0
        while idx < len(tokens):
            token = tokens[idx]
            if len(token) < 3:
                idx += 1
                continue
            st = os.fsdecode(token[:2]).strip()
            path = os.fsdecode(token[3:])
            idx += 1
            if st.startswith("R") or st.startswith("C"):
                # git status -z provides: <status> <new_path>\0<old_path>\0
                orig_path = os.fsdecode(tokens[idx]) if idx < len(tokens) else ""
                idx += 1
                if orig_path and orig_path not in changes:
                    changes[orig_path] = "D"
                if path and path not in changes:
                    changes[path] = "A"
                continue

            if path and path not in changes:
                if st in ("M", "MM", "AM"):
                    changes[path] = "M"
                elif st in ("A", "??"):
                    changes[path] = "A"
                elif st == "D":
                    changes[path] = "D"
                else:
                    changes[path] = "M"

    return changes

def workspace_dirty_files(base_cwd: Union[str, Path]) -> set:
    """Paths `git status` reports as changed in base_cwd (empty outside Git).

    git_helper.git_status_paths, the parser get_dirty_files uses too: both sides of
    a rename, every untracked file, and names byte-exact (leading or trailing spaces,
    undecodable bytes) as get_candidate_files_changed reports them.
    """
    from makewand.git_helper import git_status_paths
    return set(git_status_paths(base_cwd) or ())


def committable_work_in_progress(base_cwd: Union[str, Path], conflicts: List[str],
                                 baseline_manifest: Optional[Dict[str, Any]]) -> List[str]:
    """Conflicting files that are dirty only with the exact state the candidate was built on.

    The candidate already contains that work in progress: once the user commits
    it, the file is clean and still matches the recorded preimage, so apply no
    longer reports it (stashing it instead changes the file and keeps the conflict).
    The state may be an uncommitted deletion: a file absent from the preimage and
    still absent, which the candidate re-creates (see uncommitted_deletions).
    """
    if not isinstance(baseline_manifest, dict):
        return []
    dirty = workspace_dirty_files(base_cwd)
    committable = []
    for path in conflicts:
        if path not in dirty:
            continue
        if path in baseline_manifest:
            if file_record(Path(base_cwd) / path) == baseline_manifest[path]:
                committable.append(path)
        elif not os.path.lexists(_fs_path(Path(base_cwd) / path)):
            committable.append(path)
    return committable


def uncommitted_deletions(committable: List[str], baseline_manifest: Optional[Dict[str, Any]]) -> List[str]:
    """The committable_work_in_progress entries that are uncommitted deletions."""
    if not isinstance(baseline_manifest, dict):
        return []
    return [path for path in committable if path not in baseline_manifest]


def _application_obstacle(base_cwd: Union[str, Path], relative: str, status: Optional[str]) -> Optional[str]:
    """Why makewand apply can never write (or delete) relative in base_cwd, also with --force; None if it can.

    The checks apply makes: no symlink anywhere on the path
    (_verify_safe_target_path), a target that is absent or a readable regular file
    (_application_target_record), directories (or nothing yet) above a file it
    writes, and a directory it can write into.
    """
    parts = relative.split("/")
    if any(part in ("", ".", "..") for part in parts):
        return "路径无效"
    current = directory = Path(base_cwd)
    exists = False
    for index, part in enumerate(parts):
        current = current / part
        name = "/".join(parts[:index + 1])
        try:
            info = os.lstat(_fs_path(current))
        except FileNotFoundError:
            break  # absent: apply creates it, and any missing directory above it
        except OSError as error:
            return f"无法访问 {name}: {error.strerror}"
        last = index == len(parts) - 1
        if stat.S_ISLNK(info.st_mode):
            return "符号链接" if last else f"上级路径 {name} 是符号链接"
        if not last:
            if not stat.S_ISDIR(info.st_mode):
                # Nothing to delete below a file, but a written file needs a directory here.
                return None if status == "D" else f"上级路径 {name} 不是目录"
            directory = current
            continue
        if stat.S_ISDIR(info.st_mode):
            return "目录"
        if not stat.S_ISREG(info.st_mode):
            return "特殊文件"
        try:
            os.close(os.open(_fs_path(current), os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)))
        except OSError:
            return "不可读的文件"
        exists = True
    if (exists or status != "D") and not os.access(_fs_path(directory), os.W_OK | os.X_OK):
        below = directory.relative_to(base_cwd).as_posix()
        return "工作区根目录不可写" if below == "." else f"所在目录 {below} 不可写"
    if os.name == "nt":
        try:
            _application_target_record(base_cwd, relative)
        except (OSError, ValueError) as error:
            return str(error)
    return None


def application_obstacles(base_cwd: Union[str, Path], changes: Dict[str, str]) -> Dict[str, str]:
    """Workspace entries that make every makewand apply of these changes fail, also with --force.

    apply writes and deletes regular files only and never through a symlink: a
    directory, symlink, special or unreadable file at a changed path, a symlink
    or file above it, or a directory it cannot write into stops the whole apply
    until the user moves or fixes that entry. Returns {path: why}, so that no
    caller offers an apply (or a --force) that cannot succeed.
    """
    obstacles = {}
    for relative, status in changes.items():
        why = _application_obstacle(base_cwd, relative, status)
        if why:
            obstacles[relative] = why
    return obstacles


def existing_targets_of_additions(base_cwd: Union[str, Path], conflicts: List[str],
                                  changes: Dict[str, str]) -> List[str]:
    """Conflicting files the candidate adds although the workspace already has a file there.

    The candidate's copy lacked them (never copied into it, e.g. ignored or untracked
    dependency/build output, or created later), so applying with --force would
    overwrite a file the candidate never saw. An entry apply cannot replace at all
    (a directory, symlink or special file: application_obstacles) is not one of them.
    """
    existing = {path: "A" for path in conflicts
                if changes.get(path) == "A" and os.path.lexists(_fs_path(Path(base_cwd) / path))}
    obstacles = application_obstacles(base_cwd, existing)
    return [path for path in existing if path not in obstacles]


def reverted_work_in_progress(base_cwd: Union[str, Path], conflicts: List[str],
                              baseline_manifest: Optional[Dict[str, Any]],
                              baseline_commit: Optional[str]) -> List[str]:
    """Clean conflicting files back at their baseline-commit content.

    The candidate was built on uncommitted changes to them that are gone now
    (stashed or reverted); restoring and committing that work makes them apply.
    """
    if not baseline_commit or not isinstance(baseline_manifest, dict):
        return []
    dirty = workspace_dirty_files(base_cwd)
    reverted = []
    for path in conflicts:
        if path in dirty or path not in baseline_manifest:
            continue
        code, committed, _ = run_git_cmd(["git", "rev-parse", "--verify", "--quiet", f"{baseline_commit}:./{path}"],
                                         cwd=str(base_cwd))
        current_code, current, _ = run_git_cmd(["git", "hash-object", "--no-filters", "--", path], cwd=str(base_cwd))
        if code == 0 and current_code == 0 and committed.strip() and committed.strip() == current.strip():
            reverted.append(path)
    return reverted


def _hybrid_tests_available(root: Path) -> bool:
    if (root / "pytest.ini").is_file() or list(root.glob("test_*.py")) or list(root.glob("*_test.py")):
        return True
    if (root / "tests").is_dir() and any((root / "tests").rglob("*test*.py")):
        return True
    if (root / "go.mod").is_file() and shutil.which("go"):
        return True
    if (root / "Cargo.toml").is_file() and shutil.which("cargo"):
        return True
    if (root / "package.json").is_file() and shutil.which("npm"):
        try:
            return bool(json.loads((root / "package.json").read_text()).get("scripts", {}).get("test"))
        except (OSError, ValueError):
            return False
    if (root / "pyproject.toml").is_file():
        try:
            return "[tool.pytest" in (root / "pyproject.toml").read_text()
        except OSError:
            pass
    return False


class CandidateManager:
    """Manages the lifecycle of race candidates."""

    @staticmethod
    def recover_interrupted_applications(base_cwd) -> Tuple[bool, List[str], str]:
        """Explicitly recover interrupted applies under both application locks."""
        ensure_config_dir()
        harness_conflict = harness_source_tree_conflict(base_cwd)
        if harness_conflict:
            return False, [], CandidateMessage(harness_conflict, "UNVERIFIED")
        try:
            with scoped_apply_lock(base_cwd):
                workspace_lock = None
                try:
                    workspace_lock = WorkspaceLock(base_cwd).acquire()
                    if os.name == "nt":
                        from makewand.native_windows import pinned_directory
                        boundary = pinned_directory(base_cwd)
                    else:
                        boundary = contextlib.nullcontext()
                    with boundary:
                        recovered = _recover_application_journals(base_cwd)
                    return True, recovered, f"已恢复 {len(recovered)} 项中断应用"
                finally:
                    if workspace_lock is not None:
                        workspace_lock.release()
        except (OSError, ValueError, WorkspaceLockError) as error:
            return False, [], f"中断应用恢复已停止，保留工作区与备份: {error}"

    @staticmethod
    def save_race(
        race_id: str,
        prompt: str,
        base_cwd: str,
        baseline_commit: str,
        agent_a: Dict[str, Any],
        agent_b: Dict[str, Any],
        judge_report: str = "",
        winner: Optional[str] = None,
        baseline_dir: Optional[Union[str, Path]] = None,
        baseline_manifest: Optional[Dict[str, Any]] = None,
        frozen_baseline_manifest: Optional[Dict[str, Any]] = None,
        protected_files: Optional[Dict[str, Any]] = None,
        dag: Optional[Dict[str, Any]] = None,
    ) -> Path:
        ensure_config_dir()
        with scoped_apply_lock(base_cwd):
            return CandidateManager._save_race_locked(
                race_id, prompt, base_cwd, baseline_commit, agent_a, agent_b,
                judge_report, winner, baseline_dir, baseline_manifest, frozen_baseline_manifest, protected_files,
                dag)

    @staticmethod
    def _save_race_locked(
        race_id: str,
        prompt: str,
        base_cwd: str,
        baseline_commit: str,
        agent_a: Dict[str, Any],
        agent_b: Dict[str, Any],
        judge_report: str = "",
        winner: Optional[str] = None,
        baseline_dir: Optional[Union[str, Path]] = None,
        baseline_manifest: Optional[Dict[str, Any]] = None,
        frozen_baseline_manifest: Optional[Dict[str, Any]] = None,
        protected_files: Optional[Dict[str, Any]] = None,
        dag: Optional[Dict[str, Any]] = None,
    ) -> Path:
        protection = ProtectedFiles.from_dict(protected_files) if protected_files is not None else None
        ensure_config_dir()
        # Candidates hold copies of workspace sources: private 0700 directories.
        ensure_private_dir(config.CANDIDATES_DIR)
        race_dir = ensure_private_dir(config.CANDIDATES_DIR / race_id)

        base_path = Path(base_cwd).resolve()
        baseline_manifest = baseline_manifest if baseline_manifest is not None else build_manifest(base_path)

        # Attach frozen candidate manifests and ensure test_passed is explicitly set
        for agent in (agent_a, agent_b):
            agent.setdefault("test_passed", None)
            agent.setdefault("review_passed", None)

        # Auto-populate patch parsimony metrics if missing
        for agent in (agent_a, agent_b):
            if "parsimony" not in agent and "diff" in agent:
                from makewand.orchestrator import compute_patch_parsimony
                agent["parsimony"] = compute_patch_parsimony(agent.get("diff", ""))
        if "path" in agent_a and os.path.exists(agent_a["path"]):
            current = build_manifest(Path(agent_a["path"]))
            if "manifest" in agent_a and agent_a["manifest"] != current:
                raise ValueError("candidate A changed after review")
            agent_a["manifest"] = current
            inputs = _input_manifest(Path(agent_a["path"]))
            if "input_manifest" in agent_a and agent_a["input_manifest"] != inputs:
                raise ValueError("candidate A inputs changed after review")
            agent_a["input_manifest"] = inputs
        if "path" in agent_b and os.path.exists(agent_b["path"]):
            current = build_manifest(Path(agent_b["path"]))
            if "manifest" in agent_b and agent_b["manifest"] != current:
                raise ValueError("candidate B changed after review")
            agent_b["manifest"] = current
            inputs = _input_manifest(Path(agent_b["path"]))
            if "input_manifest" in agent_b and agent_b["input_manifest"] != inputs:
                raise ValueError("candidate B inputs changed after review")
            agent_b["input_manifest"] = inputs

        # Freeze the complete application plan, including deletions, outside the
        # candidate's writable Git metadata. A caller that performed a review
        # supplies the pre-review plan and any later change fails closed.
        for label, agent in (("A", agent_a), ("B", agent_b)):
            if agent.get("path") and os.path.exists(agent["path"]):
                changes = get_candidate_files_changed(
                    Path(agent["path"]), agent.get("baseline_commit") or baseline_commit)
                if "changes" in agent and agent["changes"] != changes:
                    raise ValueError(f"candidate {label} application plan changed after review")
                agent["changes"] = changes

        # A hybrid must use the version that produced A/B, including the dirty
        # workspace snapshot captured before generation. Never reconstruct it
        # from the user's current working directory at merge time.
        frozen_baseline = None
        baseline_error = None
        try:
            if baseline_dir is not None:
                source = Path(baseline_dir)
                if source.is_symlink() or not source.is_dir():
                    raise ValueError("frozen baseline must be a real directory")
                frozen_baseline = source
                if frozen_baseline_manifest is not None and build_manifest(source) != frozen_baseline_manifest:
                    raise ValueError("frozen baseline changed during generation")
            else:
                exported = []
                for agent in (agent_a, agent_b):
                    revision = agent.get("baseline_commit")
                    if not revision or not agent.get("path"):
                        continue
                    target = race_dir / ("baseline-" + uuid.uuid4().hex)
                    if _export_baseline(Path(agent["path"]), revision, target):
                        exported.append(target)
                    else:
                        shutil.rmtree(target, ignore_errors=True)
                if exported:
                    frozen_baseline = exported[0]
                    try:
                        if any(build_manifest(path) != build_manifest(frozen_baseline) for path in exported[1:]):
                            raise ValueError("candidate A/B baselines differ")
                    finally:
                        for path in exported[1:]:
                            shutil.rmtree(path, ignore_errors=True)
            if protection is not None and frozen_baseline is not None:
                if baseline_dir is None:
                    protection.prepare_workspace(frozen_baseline)
                else:
                    protection.verify(frozen_baseline)
        except (OSError, ValueError) as exc:
            if isinstance(exc, ProtectionError) and exc.status == "TIMEOUT":
                raise
            frozen_baseline = None
            baseline_error = str(exc)

        meta = {
            "race_id": race_id,
            "prompt": prompt,
            "base_cwd": str(base_path),
            "baseline_commit": baseline_commit,
            "baseline_manifest": baseline_manifest,
            "baseline_dir": str(frozen_baseline) if frozen_baseline is not None else None,
            "frozen_baseline_manifest": build_manifest(frozen_baseline) if frozen_baseline is not None else None,
            "frozen_baseline_inputs": _input_manifest(frozen_baseline) if frozen_baseline is not None else None,
            "baseline_error": baseline_error,
            "manifest_scope": MANIFEST_SCOPE,
            "created_at": datetime.now().isoformat(),
            "status": "completed",
            "winner": winner,
            "judge_report": judge_report,
            "candidates": {
                "A": agent_a,
                "B": agent_b,
            }
        }
        if protection is not None:
            meta["protected_files"] = protection.to_dict()
        if dag is not None:
            # A multi-stage DAG's cumulative result: which stages produced it and
            # anything that keeps it from being applied (see task_dag.DagStaging).
            meta["dag"] = dag

        meta_file = race_dir / "meta.json"
        _write_private_json(meta_file, meta)

        # LRU eviction: keep only the latest candidates; unapplied ones are
        # evicted last and every such eviction is reported.
        try:
            CandidateManager._prune_old_candidates_locked(max_candidates=CandidateManager.DEFAULT_MAX_CANDIDATES)
        except Exception as exc:
            print(c(f"⚠ [Makewand Candidates] 候选清理失败: {exc}", COLOR_YELLOW), file=sys.stderr)

        return race_dir

    DEFAULT_MAX_CANDIDATES = 10

    @staticmethod
    def prune_old_candidates(max_candidates: int = DEFAULT_MAX_CANDIDATES) -> int:
        ensure_config_dir()
        with scoped_apply_lock():
            return CandidateManager._prune_old_candidates_locked(max_candidates)

    @staticmethod
    def _prune_old_candidates_locked(max_candidates: int = DEFAULT_MAX_CANDIDATES) -> int:
        """
        LRU eviction to keep only the latest candidates and clean up older
        candidate directories to prevent disk exhaustion. Candidates that were
        already applied are evicted first; evicting a candidate that was never
        applied is always reported, never silent.
        """
        ensure_config_dir()
        if not config.CANDIDATES_DIR.exists():
            return 0

        entries = []
        for entry in config.CANDIDATES_DIR.iterdir():
            if entry.is_dir() or entry.is_symlink():
                if not entry.is_symlink() and candidate_in_progress(entry):
                    # Still being produced (e.g. a running multi-stage DAG's staging
                    # copy, which has no meta.json yet): not a candidate to evict.
                    continue
                ts = 0.0
                applied = False
                meta_file = entry / "meta.json"
                if not entry.is_symlink() and meta_file.exists():
                    try:
                        with open(meta_file, "r", encoding="utf-8") as f:
                            data = json.load(f)
                            c_str = data.get("created_at")
                            if c_str:
                                ts = datetime.fromisoformat(c_str).timestamp()
                            applied = bool(data.get("applied_at"))
                    except Exception:
                        pass
                if ts <= 0.0:
                    try:
                        ts = entry.stat().st_mtime
                    except Exception:
                        ts = 0.0
                entries.append((entry, ts, applied))

        # Newest first; beyond the limit, applied candidates go before unapplied ones.
        entries.sort(key=lambda x: x[1], reverse=True)
        evicted = 0
        excess = len(entries) - max_candidates
        if excess > 0:
            older = entries[max_candidates:] + entries[:max_candidates]
            to_remove = sorted(older, key=lambda x: (not x[2], x[1]))[:excess]
            unapplied = []
            for entry, _, applied in to_remove:
                try:
                    if entry.is_symlink() or not entry.is_dir():
                        entry.unlink(missing_ok=True)
                    else:
                        shutil.rmtree(entry)
                    evicted += 1
                    if not applied and not entry.is_symlink():
                        unapplied.append(entry.name)
                except Exception as exc:
                    print(c(f"⚠ [Makewand Candidates] 无法清理候选 {entry.name}: {exc}", COLOR_YELLOW), file=sys.stderr)
            if unapplied:
                print(c(f"⚠ [Makewand Candidates] 候选数量超过上限 {max_candidates}，已清理以下从未应用的旧候选: "
                        f"{', '.join(unapplied)}", COLOR_YELLOW), file=sys.stderr)
        return evicted

    @staticmethod
    def list_races() -> List[Dict[str, Any]]:
        ensure_config_dir()
        races = []
        if not config.CANDIDATES_DIR.exists():
            return races

        for entry in config.CANDIDATES_DIR.iterdir():
            if entry.is_dir():
                meta_file = entry / "meta.json"
                if meta_file.exists():
                    try:
                        with open(meta_file, "r", encoding="utf-8") as f:
                            data = json.load(f)
                            races.append(data)
                    except Exception:
                        pass

        races.sort(key=lambda x: x.get("created_at", ""), reverse=True)
        return races

    @staticmethod
    def get_race(race_id: Optional[str] = None) -> Optional[Dict[str, Any]]:
        races = CandidateManager.list_races()
        if not races:
            return None
        if not race_id:
            return races[0]

        for race in races:
            if race.get("race_id") == race_id or race.get("race_id", "").startswith(race_id):
                return race
        return None

    @staticmethod
    def detect_conflicts(
        base_cwd: str,
        candidate_dir: Path,
        baseline_manifest: Optional[Dict[str, str]] = None,
        baseline_commit: Optional[str] = None,
        candidate_baseline_commit: Optional[str] = None
    ) -> List[str]:
        """
        Checks if any file modified by the candidate has also been modified
        in base_cwd since the race baseline (uncommitted or committed).
        """
        candidate_changes = get_candidate_files_changed(candidate_dir, baseline_commit=candidate_baseline_commit)
        conflicts = []

        # 1. Uncommitted changes check in base_cwd (NUL-delimited parsing)
        base_dirty_files = workspace_dirty_files(base_cwd)

        for changed_file in candidate_changes:
            if changed_file in base_dirty_files and changed_file not in conflicts:
                conflicts.append(changed_file)

        # 2. Baseline manifest hash comparison (preimage check). A file the
        # candidate adds was absent from the copy it was built in, whatever the
        # recorded workspace manifest says: an existing entry there (one the
        # candidate never saw) is a conflict.
        for changed_file, status in candidate_changes.items():
            if changed_file in conflicts:
                continue
            target = Path(base_cwd) / changed_file
            if status == "A":
                differs = os.path.lexists(_fs_path(target))
            elif baseline_manifest is not None:
                differs = file_record(target) != baseline_manifest.get(changed_file)
            else:
                continue
            if differs:
                conflicts.append(changed_file)

        # 3. Git commit divergence check if baseline_commit was recorded. With a
        # recorded preimage, check 2 already compared every candidate file: a
        # commit that left a file exactly as the candidate saw it (the user
        # committed the work in progress the candidate was built on) is no conflict.
        if baseline_commit and baseline_manifest is None:
            c_code, c_out, _ = run_git_cmd(["git", "diff", "--no-renames", "--name-only", "-z", baseline_commit, "HEAD"], cwd=base_cwd, binary=True)
            if c_code == 0 and c_out:
                for token in c_out.split(b"\0"):
                    f = os.fsdecode(token)  # byte-exact, as get_candidate_files_changed names it
                    if f and f in candidate_changes and f not in conflicts:
                        conflicts.append(f)

        return conflicts

    @staticmethod
    def create_hybrid_candidate(race_id: Optional[str] = None, test_timeout: Optional[float] = None) -> Tuple[bool, Optional[Dict[str, Any]], str]:
        """Serialize synthesis and application, preserving existing sealed M."""
        if os.name not in ("posix", "nt"):
            return False, None, "当前平台缺少安全目录句柄后端"
        deadline = None
        if test_timeout is not None:
            if isinstance(test_timeout, bool) or not isinstance(test_timeout, (int, float)) or not 0 < test_timeout < float("inf"):
                return False, None, "混合候选测试预算已耗尽或无效"
            deadline = time.monotonic() + test_timeout
        ensure_config_dir()
        try:
            race = CandidateManager.get_race(race_id)
            base_cwd = race.get("base_cwd") if race else None
            with scoped_apply_lock(base_cwd):
                return CandidateManager._create_hybrid_candidate_locked(race_id, deadline=deadline)
        except (OSError, ValueError) as exc:
            return False, None, f"无法合成候选 M: {exc}"

    @staticmethod
    def _create_hybrid_candidate_locked(race_id: Optional[str] = None, deadline: Optional[float] = None) -> Tuple[bool, Optional[Dict[str, Any]], str]:
        if deadline is not None and time.monotonic() >= deadline:
            return False, None, "混合候选测试预算已耗尽"
        race = CandidateManager.get_race(race_id)
        if not race:
            return False, None, "未找到指定的竞速记录"
        r_id = race.get("race_id", "")
        if not r_id or Path(r_id).name != r_id or r_id in (".", ".."):
            return False, None, "无效竞速记录路径"
        candidates = race.get("candidates", {})
        try:
            protection = _race_protection(race)
            protection.verify(race.get("base_cwd"))
        except ProtectionError as exc:
            return False, None, CandidateMessage(f"受保护文件校验失败: {exc}", exc.status)
        if "M" in candidates:
            error = _candidate_seal_error(candidates["M"], race)
            if error:
                detail = f"已封存候选 M 完整性校验失败: {error}"
                return False, None, CandidateMessage(detail, error.status) if isinstance(error, CandidateMessage) else detail
            return True, candidates["M"], "候选 M 已封存；保留原有验证与复审状态"
        base_path = Path(race.get("baseline_dir") or "")
        expected_base = race.get("frozen_baseline_manifest")
        expected_inputs = race.get("frozen_baseline_inputs")
        if not race.get("baseline_dir") or not isinstance(expected_base, dict) or expected_inputs is None:
            return False, None, "缺少竞速开始时的冻结基线，请重新生成候选后再合并"
        cand_m_dir = None
        registered = False
        # Every manifest here is compared with ones sealed in this record.
        scoped = _binds_project_files(race)
        try:
            protection.verify(base_path)
            if (base_path.is_symlink() or not base_path.is_dir()
                    or build_manifest(base_path, project_files=scoped) != expected_base
                    or _input_manifest(base_path) != expected_inputs):
                return False, None, "冻结基线内容或权限发生变化，拒绝合并"
            if any(record[0] != "file" for record in expected_inputs.values()):
                return False, None, "混合基线包含不支持的链接或特殊文件"
            for label in ("A", "B"):
                error = _candidate_seal_error(candidates.get(label, {}), race)
                if error:
                    detail = f"候选 {label} 完整性校验失败: {error}"
                    return False, None, CandidateMessage(detail, error.status) if isinstance(error, CandidateMessage) else detail
            cand_a, cand_b = candidates["A"], candidates["B"]
            from makewand.merger import semantic_merge_candidate_worktrees
            from makewand.git_helper import clone_isolated_worktree
            from makewand.orchestrator import LOCAL_TESTS_NO_EVIDENCE, run_local_tests, compute_patch_parsimony
            race_dir = config.CANDIDATES_DIR / r_id
            cand_m_dir = race_dir / ("candidate_M_" + uuid.uuid4().hex)
            # A failed safe clone is an error. Never fall back to an unrestricted
            # directory walk that could copy ignored credentials or data.
            clone_isolated_worktree(str(base_path), cand_m_dir)
            protection.prepare_workspace(cand_m_dir)
            if build_manifest(cand_m_dir, project_files=scoped) != expected_base:
                return False, None, "安全克隆未保留完整冻结基线，拒绝合并"
            code, baseline_commit, detail = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(cand_m_dir))
            if code != 0 or not baseline_commit.strip():
                return False, None, f"无法建立混合候选 Git 基线: {detail}"
            baseline_commit = baseline_commit.strip()
            ok, merged_changes, _, summary = semantic_merge_candidate_worktrees(
                str(base_path), Path(cand_a["path"]), Path(cand_b["path"]), cand_m_dir,
                changes_a=cand_a["changes"], changes_b=cand_b["changes"],
                manifest_a=cand_a["manifest"], manifest_b=cand_b["manifest"],
                baseline_manifest=expected_base)
            if not ok:
                return False, None, f"语义合并未通过: {summary}"
            protection.verify(cand_m_dir)
            if get_candidate_files_changed(cand_m_dir, baseline_commit) != merged_changes:
                return False, None, "混合候选变更计划与实际 Git 差异不一致"
            diff = get_git_diff(str(cand_m_dir), base_rev=baseline_commit)
            sealed_manifest = build_manifest(cand_m_dir, project_files=scoped)
            sealed_inputs = _input_manifest(cand_m_dir)
            if deadline is not None:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    return False, None, "混合候选测试预算已耗尽，不启动测试"
                test_result = run_local_tests(cwd=str(cand_m_dir), timeout=remaining)
                if time.monotonic() >= deadline:
                    return False, None, "混合候选测试超过剩余预算，拒绝保存"
            else:
                test_result = run_local_tests(cwd=str(cand_m_dir))
            protection.verify(cand_m_dir)
            # A tuple is always truthy, including (False, error). Neither a
            # missing suite nor a malformed result is a passing attestation.
            if isinstance(test_result, tuple) and len(test_result) == 2 and isinstance(test_result[0], bool):
                test_ok, test_detail = test_result
            else:
                test_ok, test_detail = False, "无效的本地测试结果协议"
            tests_available = _hybrid_tests_available(base_path)
            test_evidence_available = (
                tests_available and test_detail is not None
                and getattr(test_detail, "execution_status", None) not in LOCAL_TESTS_NO_EVIDENCE
            )
            tests_passed = test_ok if test_evidence_available else None
            if (build_manifest(cand_m_dir, project_files=scoped) != sealed_manifest
                    or _input_manifest(cand_m_dir) != sealed_inputs
                    or get_candidate_files_changed(cand_m_dir, baseline_commit) != merged_changes):
                return False, None, "合并测试改变了已封存内容、权限或变更计划，拒绝保存候选 M"
            if (build_manifest(base_path, project_files=scoped) != expected_base
                    or _input_manifest(base_path) != expected_inputs):
                return False, None, "合并期间冻结基线发生变化"
            for label in ("A", "B"):
                error = _candidate_seal_error(candidates[label], race)
                if error:
                    detail = f"合并期间候选 {label} 完整性失效: {error}"
                    return False, None, CandidateMessage(detail, error.status) if isinstance(error, CandidateMessage) else detail
            cand_m = {
                "label": "M", "model": f"{cand_a.get('model', 'A')}+{cand_b.get('model', 'B')}-hybrid",
                "path": str(cand_m_dir), "success": True,
                "test_passed": tests_passed, "test_details": test_detail,
                "review_passed": None, "diff": diff, "changes": merged_changes,
                "manifest": sealed_manifest, "input_manifest": sealed_inputs,
                "baseline_commit": baseline_commit,
                "parsimony": compute_patch_parsimony(diff), "merged_from": ["A", "B"],
                "created_at": datetime.now().isoformat(),
            }
            candidates["M"] = cand_m
            protection.verify(cand_m_dir)
            protection.verify(race.get("base_cwd"))
            _write_private_json(race_dir / "meta.json", race)
            registered = True
            state = "通过" if tests_passed is True else "未通过" if tests_passed is False else "无可执行测试"
            return True, cand_m, f"成功合成 Candidate M (Hybrid): 测试验证={state}，待独立复审"
        except ProtectionError as exc:
            return False, None, CandidateMessage(f"混合候选受保护文件校验失败: {exc}", exc.status)
        except (OSError, ValueError, KeyError) as exc:
            return False, None, f"混合候选完整性校验失败: {exc}"
        finally:
            if cand_m_dir is not None and not registered:
                shutil.rmtree(cand_m_dir, ignore_errors=True)

    @staticmethod
    def approve_hybrid_candidate(race_id: str, expected_manifest: Dict[str, Any],
                                 expected_changes: Dict[str, str], review_report: str = "") -> Tuple[bool, str]:
        """Bind an independently approved review to the exact sealed hybrid."""
        from makewand.review_contract import evaluate_review_verdict, REVIEW_PASSED
        if not isinstance(review_report, str) or evaluate_review_verdict(review_report)["status"] != REVIEW_PASSED:
            return False, "候选 M 复审缺少有效、明确通过的 MAKEWAND_VERDICT 裁决"
        ensure_config_dir()
        race = CandidateManager.get_race(race_id)
        base_cwd = race.get("base_cwd") if race else None
        with scoped_apply_lock(base_cwd):
            race = CandidateManager.get_race(race_id)
            hybrid = race.get("candidates", {}).get("M") if race else None
            if (not hybrid or hybrid.get("manifest") != expected_manifest
                    or hybrid.get("changes") != expected_changes):
                return False, "复审对象与已封存候选 M 不一致"
            try:
                protection = _race_protection(race)
                protection.verify(hybrid.get("path"))
                protection.verify(race.get("base_cwd"))
            except ProtectionError as exc:
                return False, CandidateMessage(f"候选 M 受保护文件校验失败: {exc}", exc.status)
            error = _candidate_seal_error(hybrid, race)
            if error:
                return False, error
            if hybrid.get("test_passed") is not True:
                return False, "候选 M 未完成测试验证，不能授予自动复审批准"
            hybrid["review_passed"] = True
            hybrid["review_report"] = review_report
            _write_private_json(config.CANDIDATES_DIR / race["race_id"] / "meta.json", race)
            return True, "候选 M 独立复审已绑定封存产物"

    @staticmethod
    def apply_candidate(
        race_id: Optional[str] = None,
        candidate_label: Optional[str] = None,
        dry_run: bool = False,
        force: bool = False,
        merge: bool = False,
        protected_paths=None,
    ) -> ApplyResult:
        """
        Safely applies candidate changes to base_cwd with conflict detection and rollback journal.
        Guarded with a file lock to serialize Makewand apply operations.
        Returns (success, applied_files, message) as an ApplyResult, whose status types
        the outcome (APPLY_CONFLICT, TIMEOUT, ...) for callers that map it to an exit status.
        """
        return ApplyResult(*CandidateManager._apply_candidate(
            race_id=race_id, candidate_label=candidate_label, dry_run=dry_run, force=force,
            merge=merge, protected_paths=protected_paths))

    @staticmethod
    def _apply_candidate(
        race_id: Optional[str] = None,
        candidate_label: Optional[str] = None,
        dry_run: bool = False,
        force: bool = False,
        merge: bool = False,
        protected_paths=None,
    ) -> Tuple[bool, List[str], str]:
        if os.name not in ("posix", "nt"):
            return False, [], "当前平台缺少安全候选应用后端。"
        ensure_config_dir()
        race = CandidateManager.get_race(race_id)
        base_cwd = race.get("base_cwd") if race else None
        # Before journal recovery, which may already write into base_cwd.
        harness_conflict = harness_source_tree_conflict(base_cwd) if base_cwd and not dry_run else None
        if harness_conflict:
            return False, [], CandidateMessage(harness_conflict, "UNVERIFIED")

        lock_file = config.CONFIG_DIR / "apply.lock"
        scoped_lock_file = get_scoped_apply_lock_path(base_cwd)
        lock_fd = None
        scoped_fd = None
        try:
            deadline = _check_apply_deadline()
            lock_fd = open(lock_file, "a")
            target_lock_op = fcntl.LOCK_SH if base_cwd else fcntl.LOCK_EX
            try:
                if deadline is None:
                    fcntl.flock(lock_fd, target_lock_op)
                    if base_cwd:
                        scoped_fd = open(scoped_lock_file, "a")
                        fcntl.flock(scoped_fd, fcntl.LOCK_EX)
                else:
                    while True:
                        _check_apply_deadline()
                        try:
                            fcntl.flock(lock_fd, target_lock_op | fcntl.LOCK_NB)
                            break
                        except OSError as error:
                            if error.errno not in (errno.EAGAIN, errno.EACCES, errno.EWOULDBLOCK):
                                raise
                            time.sleep(min(.01, max(0, deadline - time.monotonic())))
                    if base_cwd:
                        scoped_fd = open(scoped_lock_file, "a")
                        while True:
                            _check_apply_deadline()
                            try:
                                fcntl.flock(scoped_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                                break
                            except OSError as error:
                                if error.errno not in (errno.EAGAIN, errno.EACCES, errno.EWOULDBLOCK):
                                    raise
                                time.sleep(min(.01, max(0, deadline - time.monotonic())))
            except CandidateDeadlineExceeded:
                raise
            except Exception as e:
                return False, [], f"无法获取候选应用独占锁 (apply.lock): {e}"
            # Writing into the workspace must not race a makewand task running there.
            workspace_lock = None
            if not dry_run and base_cwd and os.path.isdir(base_cwd):
                try:
                    workspace_lock = WorkspaceLock(base_cwd).acquire()
                except WorkspaceLockError as e:
                    return False, [], str(e)
                except OSError as e:
                    return False, [], f"无法获取工作区锁: {e}"
            try:
                if os.name == "nt" and base_cwd:
                    from makewand.native_windows import pinned_directory
                    boundary = pinned_directory(base_cwd)
                else:
                    boundary = contextlib.nullcontext()
                with boundary:
                    if not dry_run and base_cwd:
                        _recover_application_journals(base_cwd)
                    return CandidateManager._do_apply_candidate(
                        race_id=race_id,
                        candidate_label=candidate_label,
                        dry_run=dry_run,
                        force=force,
                        merge=merge,
                        protected_paths=protected_paths,
                    )
            finally:
                if workspace_lock is not None:
                    workspace_lock.release()
        except CandidateDeadlineExceeded as e:
            return False, [], CandidateMessage(str(e), "TIMEOUT")
        except Exception as e:
            return False, [], f"打开候选应用锁失败: {e}"
        finally:
            if scoped_fd is not None:
                try:
                    fcntl.flock(scoped_fd, fcntl.LOCK_UN)
                except Exception:
                    pass
                try:
                    scoped_fd.close()
                except Exception:
                    pass
            if lock_fd is not None:
                try:
                    fcntl.flock(lock_fd, fcntl.LOCK_UN)
                except Exception:
                    pass
                try:
                    lock_fd.close()
                except Exception:
                    pass

    @staticmethod
    def _do_apply_candidate(
        race_id: Optional[str] = None,
        candidate_label: Optional[str] = None,
        dry_run: bool = False,
        force: bool = False,
        merge: bool = False,
        protected_paths=None,
    ) -> Tuple[bool, List[str], str]:
        try:
            _check_apply_deadline()
        except CandidateDeadlineExceeded as error:
            return False, [], CandidateMessage(str(error), "TIMEOUT")
        race = CandidateManager.get_race(race_id)
        if not race:
            return False, [], "未找到指定的候选竞速记录"

        r_id = race.get("race_id", "")
        base_cwd = race.get("base_cwd", "")
        if not isinstance(r_id, str) or not r_id or Path(r_id).name != r_id or r_id in (".", ".."):
            return False, [], "无效竞速记录路径"
        if not os.path.exists(base_cwd):
            return False, [], f"原始工作区不存在: {base_cwd}"
        try:
            protection = _race_protection(race)
            protection.verify(base_cwd)
            additional = ProtectedFiles.capture(base_cwd, protected_paths)
        except ProtectionError as exc:
            return False, [], CandidateMessage(f"受保护文件校验失败: {exc}", exc.status)

        # Choose candidate (require explicit candidate if no winner)
        if merge:
            label = "M"
        else:
            raw = (candidate_label or race.get("winner") or "").upper()
            if raw in ("M", "HYBRID", "MERGE", "MERGED"):
                label = "M"
            elif raw in ("A", "B"):
                label = raw
            elif not raw:
                return False, [], "竞速裁判未决出胜者，请显式指定待应用的候选方案: --candidate A, --candidate B, 或 --merge"
            else:
                return False, [], f"无效的候选方案标识: {raw}，仅支持 A、B 或 M (混合方案)"

        # If candidate M is requested but doesn't exist yet, synthesize it now
        if label == "M" and "M" not in race.get("candidates", {}):
            if dry_run:
                return False, [], "候选 M 尚未合成；dry-run 不执行测试或创建候选，请先显式合成"
            # The public apply path already holds apply.lock.
            ok_m, cand_m_meta, msg_m = CandidateManager._create_hybrid_candidate_locked(race_id=r_id)
            if not ok_m:
                detail = f"无法合成候选 M (Hybrid): {msg_m}"
                return False, [], CandidateMessage(detail, msg_m.status) if isinstance(msg_m, CandidateMessage) else detail
            race = CandidateManager.get_race(r_id)

        cand_info = race.get("candidates", {}).get(label, {})
        cand_path_str = cand_info.get("path")
        if not cand_path_str or not os.path.exists(cand_path_str):
            return False, [], f"候选选手 {label} 的工作区目录已丢失: {cand_path_str}"

        # Prevent applying failed candidate unless forced
        if cand_info.get("success") is not True and not force:
            return False, [], f"候选选手 {label} 任务执行状态为失败/未完成，已阻止应用未就绪的方案 (如需强制应用请使用 --force)"

        if cand_info.get("test_passed") is not True and not force:
            return False, [], f"候选选手 {label} 本地单元测试未通过或未完成测试验证 (test_passed != True)，已阻止应用存在缺陷的方案 (如需强制应用请使用 --force)"

        if label != "M" and cand_info.get("review_passed") is not True and not force:
            return False, [], f"候选选手 {label} 未获裁判批准，已阻止应用 (人工确认后可使用 --force)"
        if label != "M" and not force and _unreviewed_winner_take_all_approval(race, label):
            return False, [], (f"候选选手 {label} 由旧版 Winner-Take-All 在未经任何独立复审的情况下直接判定胜出，"
                               "该批准无效，已阻止应用 (请重新运行竞速；人工确认后可使用 --force)")
        if label == "M" and cand_info.get("review_passed") is not True and not force:
            return False, [], "候选 M 尚未获独立复审批准；测试通过不能代替复审 (人工确认后可使用 --force)"

        candidate_dir = Path(cand_path_str)
        try:
            protection.verify(candidate_dir)
            additional.verify(candidate_dir)
            additional.verify(base_cwd)
        except ProtectionError as exc:
            return False, [], CandidateMessage(f"受保护文件校验失败: {exc}", exc.status)

        # Integrity check: verify candidate files haven't been mutated after save
        expected_manifest = cand_info.get("manifest")
        if expected_manifest is None:
            return False, [], f"候选选手 {label} 缺少完整性清单 (manifest 缺失)，拒绝应用未审查内容"
        current_manifest = build_manifest(candidate_dir, project_files=_binds_project_files(race))
        if current_manifest != expected_manifest:
            return False, [], f"候选选手 {label} 的文件自封存后已被外部修改 (哈希校验不匹配)，拒绝应用未审查内容"
        if "input_manifest" in cand_info and _input_manifest(candidate_dir) != cand_info["input_manifest"]:
            return False, [], f"候选选手 {label} 完整输入自封存后发生变化，拒绝应用未审查内容"

        cand_baseline = cand_info.get("baseline_commit") or race.get("baseline_commit")
        changes = cand_info.get("changes")
        if not isinstance(changes, dict):
            return False, [], "候选缺少封存的变更计划，请重新运行竞速后再应用"
        if changes != get_candidate_files_changed(candidate_dir, baseline_commit=cand_baseline):
            return False, [], "候选的 Git 变更计划自复审后发生变化，拒绝应用未审查内容"
        if not changes:
            return True, [], f"候选选手 {label} 没有产生任何有效的文件变更"

        # Boundary & Symlink security checks (non-bypassable, evaluated before conflict detection)
        canonical_base = os.path.realpath(base_cwd)
        for rel_path in changes:
            target_file = Path(base_cwd) / rel_path
            src_file = candidate_dir / rel_path
            # Candidate file must not be a symlink
            if os.path.islink(src_file) or src_file.is_symlink():
                return False, [], f"安全风险: 候选文件 {rel_path} 为符号链接，已拒绝应用"

            if changes[rel_path] not in ("A", "M", "D"):
                return False, [], f"候选变更类型无效: {rel_path}"
            if changes[rel_path] == "D" and (rel_path in expected_manifest or src_file.exists()):
                return False, [], f"删除计划与已审核文件清单不一致: {rel_path}"
            if changes[rel_path] != "D" and rel_path not in expected_manifest:
                return False, [], f"候选文件 {rel_path} 未包含在已审核清单中"

            # Target file in workspace must not be a symlink
            if os.path.islink(target_file) or target_file.is_symlink():
                return False, [], f"安全风险: 目标文件 {rel_path} 为符号链接，已拒绝写入覆盖"

            # Resolve canonical path of target
            resolved_target = os.path.realpath(target_file)
            if not resolved_target.startswith(canonical_base + os.sep) and resolved_target != canonical_base:
                return False, [], f"安全越界风险: 目标文件 {rel_path} 解析落点位于工作区外部 ({resolved_target})，已拒绝写入"

            # Verify parent directories are not symlinks pointing outside
            parent = target_file.parent
            while parent != Path(base_cwd) and parent != parent.parent:
                if os.path.islink(parent):
                    resolved_parent = os.path.realpath(parent)
                    if not resolved_parent.startswith(canonical_base + os.sep) and resolved_parent != canonical_base:
                        return False, [], "安全越界风险: 目标父目录包含指向外部的符号链接，已拒绝写入"
                parent = parent.parent

        # A directory, symlink or special file where apply writes (or a file above
        # it) fails every apply, also with --force: never report it as an ordinary
        # conflict that --force would override.
        obstacles = application_obstacles(base_cwd, changes)
        if obstacles:
            listed = ", ".join(f"{path} ({why})" for path, why in sorted(obstacles.items()))
            return False, sorted(obstacles), CandidateMessage(
                f"检测到工作区冲突: makewand apply 无法写入以下工作区条目: {listed}。"
                "apply 只写入普通文件且从不经过符号链接，--force 也不例外；请先核对这些条目，"
                f"需要采用候选内容时请从候选工作区人工恢复: {candidate_dir}", "APPLY_CONFLICT")

        # Conflict Detection (dirty files + baseline manifest + baseline commit)
        if not force:
            conflicts = CandidateManager.detect_conflicts(
                base_cwd,
                candidate_dir,
                baseline_manifest=race.get("baseline_manifest"),
                baseline_commit=race.get("baseline_commit"),
                candidate_baseline_commit=cand_baseline
            )
            if conflicts:
                msg = f"检测到工作区冲突: 以下文件有未提交的改动或已不是候选生成时的状态，已阻止覆盖: {', '.join(conflicts)}"
                baseline = race.get("baseline_manifest")
                committable = committable_work_in_progress(base_cwd, conflicts, baseline)
                deleted = uncommitted_deletions(committable, baseline)
                edited = [path for path in committable if path not in deleted]
                if edited:
                    msg += (f"。其中 {', '.join(edited)} 只含候选生成时就已存在的未提交修改 (候选已包含这些修改)："
                            "提交 (git commit) 这些修改后即可应用，无需 --force；stash 会改变文件内容，不能解除冲突")
                if deleted:
                    msg += (f"。其中 {', '.join(deleted)} 在候选生成时就已被删除 (删除尚未提交)，而候选重新创建了这些文件："
                            "要采用候选中的版本，先提交这次删除 (git rm 这些文件后 git commit) 即可应用，无需 --force")
                others = [path for path in conflicts if path not in committable]
                existing = existing_targets_of_additions(base_cwd, others, changes)
                if existing:
                    msg += (f"。其中 {', '.join(existing)} 由候选新建，但工作区中已有同名条目 (候选生成时未复制进其隔离副本，"
                            "如被忽略或未跟踪的依赖/构建输出，或在此之后才创建)：确认用候选内容覆盖时才使用 --force")
                reverted = reverted_work_in_progress(base_cwd, [path for path in others if path not in existing],
                                                     baseline, race.get("baseline_commit"))
                if reverted:
                    msg += (f"。其中 {', '.join(reverted)} 已回到基线提交时的内容，而候选基于当时未提交的修改生成 "
                            "(这些修改可能已被 stash 或撤销)：恢复这些修改 (如 git stash pop) 并提交后即可应用，无需 --force")
                return False, conflicts, CandidateMessage(msg, "APPLY_CONFLICT")

        try:
            for guard in (protection, additional):
                guard.verify(candidate_dir)
                guard.verify(base_cwd)
        except ProtectionError as exc:
            return False, [], CandidateMessage(f"受保护文件校验失败: {exc}", exc.status)

        if dry_run:
            preview = [f"{status} {path}" for path, status in changes.items()]
            return True, preview, f"[Dry-run] 演练完成，共涉及 {len(changes)} 个文件的增删改"

        # Freeze every current target before the first mutation, including
        # absent additions. Force can accept prior edits, but never edits made
        # after this transaction starts.
        identity = os.stat(base_cwd, follow_symlinks=False)
        workspace_identity = [identity.st_dev, identity.st_ino]
        try:
            _application_workspace_identity(base_cwd, workspace_identity)
            preimages = {relative: _application_target_record(base_cwd, relative)
                         for relative in changes}
            before_security = {}
            before_security_descriptors = {}
            if os.name == "nt":
                from makewand.native_windows import application_security_descriptor, validate_application_security_descriptor
                for relative, current in preimages.items():
                    before_security[relative] = _application_target_security(base_cwd, relative)
                    before_security_descriptors[relative] = (application_security_descriptor(
                        _verify_safe_target_path(base_cwd, relative)) if current is not None else None)
                    if current is not None:
                        validate_application_security_descriptor(before_security_descriptors[relative], before_security[relative])
                    if (_application_target_record(base_cwd, relative) != current
                            or _application_target_security(base_cwd, relative) != before_security[relative]):
                        raise ApplicationTargetChanged(
                            "Application target changed while freezing file security: " + relative, relative)
            baseline = race.get("baseline_manifest")
            if not force:
                for relative, current in preimages.items():
                    # An added file was absent from the copy the candidate was built in.
                    if changes[relative] == "A":
                        expected = None
                    elif isinstance(baseline, dict):
                        expected = baseline.get(relative)
                    else:
                        continue
                    if current != expected:
                        raise ApplicationTargetChanged("Application target changed since its baseline: " + relative,
                                                       relative)
        except ApplicationTargetChanged as error:
            # The destination no longer matches its frozen baseline: nothing was written.
            return False, [error.path], CandidateMessage(str(error), "APPLY_CONFLICT")
        except (OSError, ValueError) as error:
            return False, [], str(error)

        # Create Backup Journal (holds copies of workspace files: private 0700)
        ensure_config_dir()
        ensure_private_dir(config.BACKUPS_DIR)
        ts = datetime.now().strftime("%Y%m%d_%H%M%S")
        backup_dir = config.BACKUPS_DIR / f"{r_id}_{ts}_{uuid.uuid4().hex[:8]}"
        backup_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        if os.name == "posix":
            _fsync_directory(str(config.BACKUPS_DIR))

        journal = []
        durable_journal = {"schema": 1, "workspace": os.path.realpath(base_cwd), "race_id": r_id,
                           "workspace_identity": workspace_identity,
                           "candidate": label, "state": "prepared", "entries": journal}
        if os.name == "nt":
            durable_journal["security_schema"] = 1
        journal_path = backup_dir / "journal.json"
        applied_files = []
        synced_backup_parents = set()

        try:
            _write_application_journal(journal_path, durable_journal)
            for rel_path, status in changes.items():
                _check_apply_deadline()
                _application_workspace_identity(base_cwd, workspace_identity)
                if _application_target_record(base_cwd, rel_path) != preimages[rel_path]:
                    raise ValueError("Application target changed before write: " + rel_path)
                if os.name == "nt" and _application_target_security(base_cwd, rel_path) != before_security[rel_path]:
                    raise ValueError("Application target file security changed before write: " + rel_path)
                for guard in (protection, additional):
                    guard.verify(candidate_dir)
                    guard.verify(base_cwd)
                target_file = _verify_safe_target_path(base_cwd, rel_path)
                src_file = candidate_dir / rel_path

                if os.path.islink(src_file) or src_file.is_symlink():
                    raise ValueError(f"安全越界风险: 候选文件 {rel_path} 为符号链接")

                postimage = None
                if status in ("M", "A"):
                    postimages = backup_dir / "postimages"
                    postimages.mkdir(mode=0o700, exist_ok=True)
                    _atomic_copy(str(postimages), rel_path, src_file, expected_manifest[rel_path])
                    if file_record(postimages / rel_path) != expected_manifest[rel_path]:
                        raise ValueError("Application postimage changed while sealing: " + rel_path)
                    postimage = "postimages/" + rel_path

                # Backup existing
                if _fs_path(target_file).exists():
                    if os.path.islink(target_file) or target_file.is_symlink():
                        raise ValueError(f"安全越界风险: 目标文件 {rel_path} 为符号链接")
                    bak_file = backup_dir / "preimages" / rel_path
                    created_backup_dirs = []
                    ancestor = bak_file.parent
                    while ancestor != backup_dir and not os.path.lexists(_fs_path(ancestor)):
                        created_backup_dirs.append(ancestor)
                        ancestor = ancestor.parent
                    _fs_path(bak_file.parent).mkdir(parents=True, exist_ok=True)
                    if os.name == "nt":
                        from makewand.native_windows import copy_backup
                        copy_backup(target_file, bak_file)
                    else:
                        shutil.copy2(target_file, bak_file)
                        with open(bak_file, "rb") as bf:
                            os.fsync(bf.fileno())
                        # The file entry, and every directory entry created for
                        # it up to the backup root, must be durable before the
                        # journal claims this backup can restore user data.
                        pending = [bak_file.parent] + [created.parent for created in created_backup_dirs]
                        for parent in pending:
                            parent_str = str(parent)
                            if parent_str not in synced_backup_parents:
                                _fsync_directory(parent_str)
                                synced_backup_parents.add(parent_str)
                    if file_record(bak_file) != preimages[rel_path]:
                        raise ValueError("Application preimage changed during backup: " + rel_path)
                    journal.append({"path": rel_path, "action": "restore", "bak": str(bak_file),
                                    "before": file_record(bak_file), "after": expected_manifest.get(rel_path)})
                else:
                    journal.append({"path": rel_path, "action": "delete", "before": None,
                                    "after": expected_manifest.get(rel_path)})
                entry = journal[-1]
                if os.name == "nt":
                    entry["before_security"] = before_security[rel_path]
                    entry["after_security"] = None if status == "D" else before_security[rel_path]
                    entry["before_security_descriptor"] = before_security_descriptors[rel_path]
                entry["temp"] = (Path(rel_path).parent / (".makewand-" + uuid.uuid4().hex)).as_posix()
                entry["created_dirs"] = _missing_application_dirs(base_cwd, rel_path)
                if postimage is not None:
                    entry["postimage"] = postimage

                # Complete and fsync the rollback plan before changing user data.
                _write_application_journal(journal_path, durable_journal)
                _application_workspace_identity(base_cwd, workspace_identity)
                if _application_target_record(base_cwd, rel_path) != preimages[rel_path]:
                    raise ValueError("Application target changed while preparing write: " + rel_path)
                if os.name == "nt" and _application_target_security(base_cwd, rel_path) != before_security[rel_path]:
                    raise ValueError("Application target file security changed while preparing write: " + rel_path)

                # Apply Change
                if status in ("M", "A"):
                    _fs_path(target_file.parent).mkdir(parents=True, exist_ok=True)
                    _verify_safe_target_path(base_cwd, rel_path)

                    before_replace = None
                    if os.name == "nt":
                        def record_after_security(security):
                            _application_workspace_identity(base_cwd, workspace_identity)
                            if (_application_target_record(base_cwd, rel_path) != preimages[rel_path]
                                    or _application_target_security(base_cwd, rel_path) != before_security[rel_path]):
                                raise ValueError("Application target changed before replacement: " + rel_path)
                            if preimages[rel_path] is not None and security != before_security[rel_path]:
                                raise ValueError("Application replacement file security changed: " + rel_path)
                            if not isinstance(security, str) or not security or len(security) > 16384:
                                raise ValueError("Invalid application replacement file security")
                            entry["after_security"] = security
                            _write_application_journal(journal_path, durable_journal)
                        before_replace = record_after_security
                    _atomic_copy(base_cwd, rel_path, src_file, expected_manifest.get(rel_path),
                                 temporary_name=Path(entry["temp"]).name,
                                 workspace_identity=workspace_identity, before_replace=before_replace,
                                 expected_preimage=preimages[rel_path])
                    applied_files.append(f"A/M {rel_path}")
                elif status == "D":
                    if _fs_path(target_file).exists():
                        _verify_safe_target_path(base_cwd, rel_path)
                        _atomic_remove(base_cwd, rel_path, workspace_identity=workspace_identity,
                                       expected_security=before_security[rel_path] if os.name == "nt" else _NO_SECURITY_EXPECTATION)
                        applied_files.append(f"D   {rel_path}")

                _check_apply_deadline()

            _check_apply_deadline()
            for guard in (protection, additional):
                guard.verify(candidate_dir)
                guard.verify(base_cwd)
            _application_workspace_identity(base_cwd, workspace_identity)
            for relative, status in changes.items():
                expected = None if status == "D" else expected_manifest[relative]
                if _application_target_record(base_cwd, relative) != expected:
                    raise ValueError("Application postimage changed before commit: " + relative)
            if os.name == "nt":
                for entry in journal:
                    if _application_target_security(base_cwd, entry["path"]) != entry["after_security"]:
                        raise ValueError("Application postimage file security changed before commit: " + entry["path"])
            durable_journal["state"] = "committed"
            _write_application_journal(journal_path, durable_journal)
            CandidateManager._mark_applied(r_id, label)
            return True, applied_files, f"成功应用候选方案 {label} ({len(applied_files)} 个变更已同步)"

        except Exception as e:
            # The same complete preflight used on restart protects user edits
            # during immediate failure rollback. A conflict preserves every
            # target and the private evidence instead of partly restoring first.
            rollback_errors = []
            try:
                # Publication may succeed before a metadata sync reports an
                # error. A committed marker must never be called rolled back.
                if journal_path.exists():
                    with journal_path.open(encoding="utf-8") as stream:
                        published = json.load(stream)
                    if published.get("state") == "committed":
                        return False, [], CandidateMessage(f"应用提交结果未确定，已保留提交日志: {e}；{backup_dir}", "UNKNOWN")
                _recover_application_journals(base_cwd)
                # A missing header must not make recovery silently skip this
                # transaction and authorize a false rolled-back marker.
                _verify_restored_application(base_cwd, journal, workspace_identity)
            except Exception as rollback_error:
                rollback_errors.append(str(rollback_error))

            if rollback_errors:
                durable_journal["state"] = "failed"
                try:
                    _write_application_journal(journal_path, durable_journal)
                except (OSError, ValueError):
                    pass
                detail = (f"应用失败: {e}；部分文件回滚失败: {'; '.join(rollback_errors)}。"
                          f"备份保留于 {backup_dir}")
                return False, [], CandidateMessage(detail, e.status) if isinstance(e, ProtectionError) else CandidateMessage(detail, "TIMEOUT") if isinstance(e, CandidateDeadlineExceeded) else detail
            durable_journal["state"] = "rolled_back"
            try:
                _write_application_journal(journal_path, durable_journal)
            except (OSError, ValueError):
                pass
            detail = f"应用过程中发生异常并已自动回滚: {str(e)}"
            return False, [], CandidateMessage(detail, e.status) if isinstance(e, ProtectionError) else CandidateMessage(detail, "TIMEOUT") if isinstance(e, CandidateDeadlineExceeded) else detail

    @staticmethod
    def _mark_applied(race_id: str, label: str) -> None:
        """Records the application so LRU eviction can prefer applied candidates."""
        meta_file = config.CANDIDATES_DIR / race_id / "meta.json"
        try:
            with open(meta_file, "r", encoding="utf-8") as f:
                meta = json.load(f)
            meta["applied_at"] = datetime.now().isoformat()
            meta["applied_candidate"] = label
            _write_private_json(meta_file, meta)
        except Exception as exc:
            print(c(f"⚠ [Makewand Candidates] 无法记录候选 {race_id} 的应用状态: {exc}", COLOR_YELLOW), file=sys.stderr)

    @staticmethod
    def discard_race(race_id: Optional[str] = None, all_races: bool = False) -> Tuple[bool, str]:
        ensure_config_dir()
        race = CandidateManager.get_race(race_id) if race_id else None
        base_cwd = race.get("base_cwd") if race else None
        with scoped_apply_lock(base_cwd):
            return CandidateManager._discard_race_locked(race_id, all_races)

    @staticmethod
    def _discard_race_locked(race_id: Optional[str] = None, all_races: bool = False) -> Tuple[bool, str]:
        ensure_config_dir()
        if all_races:
            in_progress = []
            if config.CANDIDATES_DIR.exists():
                for entry in config.CANDIDATES_DIR.iterdir():
                    if entry.is_symlink() or not entry.is_dir():
                        entry.unlink(missing_ok=True)
                    elif candidate_in_progress(entry):
                        in_progress.append(entry.name)
                    else:
                        shutil.rmtree(entry, ignore_errors=True)
            if in_progress:
                return True, f"已清理所有已保存的候选工作区；仍在生成中的工作区未清理: {', '.join(sorted(in_progress))}"
            return True, "已清理所有已保存的候选工作区"

        race = CandidateManager.get_race(race_id)
        if not race:
            return False, "未找到指定的候选记录"

        r_id = race.get("race_id", "")
        target_dir = config.CANDIDATES_DIR / r_id
        if target_dir.exists():
            shutil.rmtree(target_dir, ignore_errors=True)
        return True, f"已清理候选记录: {r_id}"
