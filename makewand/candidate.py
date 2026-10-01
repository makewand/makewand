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

import makewand.config as config
from makewand.config import (
    ensure_config_dir,
    ensure_private_dir,
    c,
    COLOR_YELLOW,
)
import hashlib
from makewand.git_helper import run_git_cmd, get_git_diff, _read_workspace_diff, WorkspaceLock, WorkspaceLockError
from makewand.protected_files import ProtectedFiles, ProtectionError

_NO_SECURITY_EXPECTATION = object()


def _race_protection(race):
    # Presence is authoritative: malformed declarations cannot become legacy.
    return ProtectedFiles.from_dict(race["protected_files"]) if "protected_files" in race else ProtectedFiles()


class CandidateMessage(str):
    """Add an outcome category without changing the legacy apply tuple."""
    def __new__(cls, message, status):
        value = str.__new__(cls, message)
        value.status = status
        return value


class CandidateDeadlineExceeded(TimeoutError):
    pass


def _check_apply_deadline():
    from makewand.execution_runtime import current_context
    deadline = current_context().get("_deadline_monotonic")
    if deadline is not None and time.monotonic() >= deadline:
        raise CandidateDeadlineExceeded("候选应用的总执行期限已耗尽，保留封存候选")
    return deadline

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


def build_manifest(dir_path: Path) -> Dict[str, Any]:
    """Bind regular file content AND permissions; old hash-only manifests fail closed."""
    dir_path = Path(dir_path)
    if os.name == "nt":
        from makewand.native_windows import manifest
        return manifest(dir_path)
    manifest = {}
    if not dir_path.exists():
        return manifest
    for root, directories, files in os.walk(str(dir_path)):
        if Path(root) == dir_path:
            directories[:] = [directory for directory in directories if directory != ".git"]
        for f in files:
            p = Path(root) / f
            if not os.path.islink(p):
                rel = p.relative_to(dir_path).as_posix()
                parts = Path(rel).parts
                if parts and parts[0] == ".git":
                    continue
                record = file_record(p)
                if record is not None:
                    manifest[rel] = record
    return manifest


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


def _atomic_copy(workspace: str, rel_path: str, source: Path, expected=None, *, temporary_name=None,
                 workspace_identity=None, before_replace=None):
    """Write through directory handles and atomically replace, preserving mode.

    The source is checked while copying, so a changed candidate cannot win the
    gap between manifest validation and application. No shared inode is edited.
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
        os.replace(temp_name, parts[-1], src_dir_fd=directory, dst_dir_fd=directory)
        created = False
        os.fsync(directory)
    finally:
        if created:
            os.unlink(temp_name, dir_fd=directory)
        os.close(directory)

def _write_private_json(path: Path, data: Dict[str, Any]) -> None:
    """Publish complete private metadata atomically, without following links."""
    if path.is_symlink():
        raise ValueError("candidate metadata must not be a symlink")
    fd, temporary = tempfile.mkstemp(prefix=".meta-", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(data, handle, indent=2, ensure_ascii=False)
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


def _write_application_journal(path, data):
    if len(json.dumps(data, indent=2, ensure_ascii=False).encode("utf-8")) > 16 * 1024 * 1024:
        raise ValueError("Application journal exceeds its recovery size limit")
    _write_private_json(path, data)


def _application_workspace_identity(root, expected):
    identity = os.stat(root, follow_symlinks=False)
    if not stat.S_ISDIR(identity.st_mode) or [identity.st_dev, identity.st_ino] != expected:
        raise ValueError("Interrupted application workspace identity changed")


def _application_target_record(root, relative):
    target = _verify_safe_target_path(root, relative)
    if not os.path.lexists(target):
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
    if not os.path.lexists(target):
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
    if not os.path.lexists(target):
        return target
    if target.is_symlink() or not target.is_file() or target.stat().st_size > 512 * 1024 * 1024:
        raise ValueError("Interrupted application temporary path changed")
    size = target.stat().st_size
    current = file_sha256(target)
    for bucket, record in (("preimages", item.get("before")), ("postimages", item.get("after"))):
        if record is None:
            continue
        source = folder / bucket / item["path"]
        if not source.exists():
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
    if target is not None and os.path.lexists(target):
        _atomic_remove(root, item["temp"], workspace_identity=workspace_identity)


def _missing_application_dirs(root, relative):
    result = []
    parent = Path(relative).parent
    while parent != Path("."):
        if os.path.lexists(Path(root) / parent):
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
    if not isinstance(expected, dict) or build_manifest(path) != expected:
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
        try:
            with open(config.CONFIG_DIR / "apply.lock", "a") as lock:
                fcntl.flock(lock, fcntl.LOCK_EX)
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
                    fcntl.flock(lock, fcntl.LOCK_UN)
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
    ) -> Path:
        ensure_config_dir()
        with open(config.CONFIG_DIR / "apply.lock", "a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            try:
                return CandidateManager._save_race_locked(
                    race_id, prompt, base_cwd, baseline_commit, agent_a, agent_b,
                    judge_report, winner, baseline_dir, baseline_manifest, frozen_baseline_manifest, protected_files)
            finally:
                fcntl.flock(lock, fcntl.LOCK_UN)

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
        with open(config.CONFIG_DIR / "apply.lock", "a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            try:
                return CandidateManager._prune_old_candidates_locked(max_candidates)
            finally:
                fcntl.flock(lock, fcntl.LOCK_UN)

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
        code, diff_out, _ = run_git_cmd(["git", "status", "-z", "--porcelain"], cwd=base_cwd, binary=True)
        base_dirty_files = set()
        if code == 0 and diff_out:
            for token in diff_out.split(b"\0"):
                if len(token) >= 3:
                    fpath = token[3:].decode("utf-8", errors="replace").strip()
                    if fpath:
                        base_dirty_files.add(fpath)

        for changed_file in candidate_changes:
            if changed_file in base_dirty_files and changed_file not in conflicts:
                conflicts.append(changed_file)

        # 2. Baseline manifest hash comparison (preimage check)
        if baseline_manifest:
            for changed_file in candidate_changes:
                target = Path(base_cwd) / changed_file
                cur_hash = file_record(target) if target.exists() else None
                base_hash = baseline_manifest.get(changed_file)
                if cur_hash != base_hash and changed_file not in conflicts:
                    conflicts.append(changed_file)

        # 3. Git commit divergence check if baseline_commit was recorded
        if baseline_commit:
            c_code, c_out, _ = run_git_cmd(["git", "diff", "--no-renames", "--name-only", "-z", baseline_commit, "HEAD"], cwd=base_cwd, binary=True)
            if c_code == 0 and c_out:
                for token in c_out.split(b"\0"):
                    f = token.decode("utf-8", errors="replace").strip()
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
            with open(config.CONFIG_DIR / "apply.lock", "a") as lock:
                fcntl.flock(lock, fcntl.LOCK_EX)
                try:
                    return CandidateManager._create_hybrid_candidate_locked(race_id, deadline=deadline)
                finally:
                    fcntl.flock(lock, fcntl.LOCK_UN)
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
        try:
            protection.verify(base_path)
            if base_path.is_symlink() or not base_path.is_dir() or build_manifest(base_path) != expected_base or _input_manifest(base_path) != expected_inputs:
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
            from makewand.orchestrator import run_local_tests, compute_patch_parsimony
            race_dir = config.CANDIDATES_DIR / r_id
            cand_m_dir = race_dir / ("candidate_M_" + uuid.uuid4().hex)
            # A failed safe clone is an error. Never fall back to an unrestricted
            # directory walk that could copy ignored credentials or data.
            clone_isolated_worktree(str(base_path), cand_m_dir)
            protection.prepare_workspace(cand_m_dir)
            if build_manifest(cand_m_dir) != expected_base:
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
            sealed_manifest = build_manifest(cand_m_dir)
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
                and getattr(test_detail, "execution_status", None) != "UNVERIFIED"
            )
            tests_passed = test_ok if test_evidence_available else None
            if (build_manifest(cand_m_dir) != sealed_manifest or _input_manifest(cand_m_dir) != sealed_inputs
                    or get_candidate_files_changed(cand_m_dir, baseline_commit) != merged_changes):
                return False, None, "合并测试改变了已封存内容、权限或变更计划，拒绝保存候选 M"
            if build_manifest(base_path) != expected_base or _input_manifest(base_path) != expected_inputs:
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
        with open(config.CONFIG_DIR / "apply.lock", "a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            try:
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
            finally:
                fcntl.flock(lock, fcntl.LOCK_UN)

    @staticmethod
    def apply_candidate(
        race_id: Optional[str] = None,
        candidate_label: Optional[str] = None,
        dry_run: bool = False,
        force: bool = False,
        merge: bool = False,
        protected_paths=None,
    ) -> Tuple[bool, List[str], str]:
        """
        Safely applies candidate changes to base_cwd with conflict detection and rollback journal.
        Guarded with a file lock to serialize Makewand apply operations.
        Returns (success, applied_files, message).
        """
        if os.name not in ("posix", "nt"):
            return False, [], "当前平台缺少安全候选应用后端。"
        ensure_config_dir()
        lock_file = config.CONFIG_DIR / "apply.lock"
        lock_fd = None
        try:
            deadline = _check_apply_deadline()
            lock_fd = open(lock_file, "a")
            try:
                if deadline is None:
                    fcntl.flock(lock_fd, fcntl.LOCK_EX)
                else:
                    while True:
                        _check_apply_deadline()
                        try:
                            fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
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
            race = CandidateManager.get_race(race_id)
            base_cwd = race.get("base_cwd") if race else None
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
            if lock_fd:
                try:
                    fcntl.flock(lock_fd, fcntl.LOCK_UN)
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
        current_manifest = build_manifest(candidate_dir)
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
                msg = f"检测到工作区冲突: 以下文件在基线后已被修改，已阻止覆盖: {', '.join(conflicts)}"
                return False, conflicts, msg

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
                        raise ValueError("Application target changed while freezing file security: " + relative)
            baseline = race.get("baseline_manifest")
            if not force and isinstance(baseline, dict):
                for relative, current in preimages.items():
                    if current != baseline.get(relative):
                        raise ValueError("Application target changed since its baseline: " + relative)
        except (OSError, ValueError) as error:
            return False, [], str(error)

        # Create Backup Journal (holds copies of workspace files: private 0700)
        ensure_config_dir()
        ensure_private_dir(config.BACKUPS_DIR)
        ts = datetime.now().strftime("%Y%m%d_%H%M%S")
        backup_dir = config.BACKUPS_DIR / f"{r_id}_{ts}_{uuid.uuid4().hex[:8]}"
        backup_dir.mkdir(mode=0o700, parents=True, exist_ok=True)

        journal = []
        durable_journal = {"schema": 1, "workspace": os.path.realpath(base_cwd), "race_id": r_id,
                           "workspace_identity": workspace_identity,
                           "candidate": label, "state": "prepared", "entries": journal}
        if os.name == "nt":
            durable_journal["security_schema"] = 1
        journal_path = backup_dir / "journal.json"
        applied_files = []

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
                if target_file.exists():
                    if os.path.islink(target_file) or target_file.is_symlink():
                        raise ValueError(f"安全越界风险: 目标文件 {rel_path} 为符号链接")
                    bak_file = backup_dir / "preimages" / rel_path
                    bak_file.parent.mkdir(parents=True, exist_ok=True)
                    if os.name == "nt":
                        from makewand.native_windows import copy_backup
                        copy_backup(target_file, bak_file)
                    else:
                        shutil.copy2(target_file, bak_file)
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
                    target_file.parent.mkdir(parents=True, exist_ok=True)
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
                                 workspace_identity=workspace_identity, before_replace=before_replace)
                    applied_files.append(f"A/M {rel_path}")
                elif status == "D":
                    if target_file.exists():
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
        with open(config.CONFIG_DIR / "apply.lock", "a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            try:
                return CandidateManager._discard_race_locked(race_id, all_races)
            finally:
                fcntl.flock(lock, fcntl.LOCK_UN)

    @staticmethod
    def _discard_race_locked(race_id: Optional[str] = None, all_races: bool = False) -> Tuple[bool, str]:
        ensure_config_dir()
        if all_races:
            if config.CANDIDATES_DIR.exists():
                shutil.rmtree(config.CANDIDATES_DIR, ignore_errors=True)
                config.CANDIDATES_DIR.mkdir(parents=True, exist_ok=True)
            return True, "已清理所有已保存的候选工作区"

        race = CandidateManager.get_race(race_id)
        if not race:
            return False, "未找到指定的候选记录"

        r_id = race.get("race_id", "")
        target_dir = config.CANDIDATES_DIR / r_id
        if target_dir.exists():
            shutil.rmtree(target_dir, ignore_errors=True)
        return True, f"已清理候选记录: {r_id}"
