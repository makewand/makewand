"""Frozen task file constraints, checked through POSIX no-follow descriptors.

Nonempty protection requires POSIX directory-relative opens and O_NOFOLLOW.
Only content and complete permission mode are policy inputs; no ACLs or other
permission attributes are captured. Empty protection is a portable no-op.
"""
import base64
import hashlib
import json
import os
import re
import stat
import time
from dataclasses import dataclass
from pathlib import PureWindowsPath

MAX_PROTECTED_FILES = 256
MAX_PATH_BYTES = 4096
MAX_PATH_COMPONENTS = 64
MAX_METADATA_BYTES = 1024 * 1024
HASH_CHUNK_BYTES = 65536


class ProtectionError(ValueError):
    def __init__(self, message, *, status="UNVERIFIED"):
        super().__init__(message)
        self.status = status


def _platform():
    if (os.name != "posix" or not hasattr(os, "O_NOFOLLOW")
            or not hasattr(os, "O_DIRECTORY") or os.open not in os.supports_dir_fd):
        raise ProtectionError("Protected files require POSIX no-follow directory descriptors")


def _path(value):
    if not isinstance(value, str) or not value or len(value) > MAX_PATH_BYTES:
        raise ProtectionError("Protected paths must be nonempty relative strings")
    try:
        encoded = value.encode("utf-8")
    except UnicodeError as error:
        raise ProtectionError("Protected path is not valid UTF-8") from error
    parts = value.split("/")
    if (len(encoded) > MAX_PATH_BYTES or "\0" in value or value.startswith("/")
            or PureWindowsPath(value).drive or value.startswith("\\")
            or len(parts) > MAX_PATH_COMPONENTS
            or any(part in ("", ".", "..") or part.casefold() == ".git" for part in parts)):
        raise ProtectionError("Invalid protected relative path: " + repr(value))
    return value


def _base(cwd):
    try:
        value = os.fspath(cwd)
    except TypeError as error:
        raise ProtectionError("Protected workspace must be a filesystem path") from error
    if not isinstance(value, str) or not value or "\0" in value:
        raise ProtectionError("Protected workspace must be a nonempty text path")
    return os.path.abspath(value)


def _deadline():
    from makewand.execution_runtime import current_context
    deadline = current_context().get("_deadline_monotonic")
    if deadline is not None and time.monotonic() >= deadline:
        raise ProtectionError("Protected file verification exceeded the task deadline", status="TIMEOUT")


def _signature(info):
    return (info.st_dev, info.st_ino, info.st_mode, info.st_size,
            info.st_mtime_ns, info.st_ctime_ns)


def _bound(directories, bindings, file_fd, leaf):
    for parent, name, child in bindings:
        actual = os.stat(name, dir_fd=parent, follow_symlinks=False)
        opened = os.fstat(child)
        if (not stat.S_ISDIR(actual.st_mode)
                or (actual.st_dev, actual.st_ino) != (opened.st_dev, opened.st_ino)):
            raise ProtectionError("Protected directory changed during verification")
    actual = os.stat(leaf, dir_fd=directories[-1], follow_symlinks=False)
    opened = os.fstat(file_fd)
    if (not stat.S_ISREG(actual.st_mode)
            or (actual.st_dev, actual.st_ino) != (opened.st_dev, opened.st_ino)):
        raise ProtectionError("Protected file changed during verification")


def _inspect(base, relative, expected=None, *, check_mode=True, restore_mode=False):
    _platform()
    _deadline()
    directories, bindings = [], []
    file_fd = None
    try:
        flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)
        directories.append(os.open("/", flags))
        components = [part for part in base.split("/") if part] + relative.split("/")
        for name in components[:-1]:
            _deadline()
            parent = directories[-1]
            child = os.open(name, flags, dir_fd=parent)
            bindings.append((parent, name, child))
            directories.append(child)
        leaf = components[-1]
        file_fd = os.open(leaf, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK
                          | getattr(os, "O_CLOEXEC", 0), dir_fd=directories[-1])
        before = os.fstat(file_fd)
        if not stat.S_ISREG(before.st_mode):
            raise ProtectionError("Protected path is not a regular file: " + repr(relative))
        _bound(directories, bindings, file_fd, leaf)
        digest = hashlib.sha256()
        while True:
            _deadline()
            chunk = os.read(file_fd, HASH_CHUNK_BYTES)
            _deadline()
            if not chunk:
                break
            digest.update(chunk)
        after = os.fstat(file_fd)
        _bound(directories, bindings, file_fd, leaf)
        if _signature(before) != _signature(after):
            raise ProtectionError("Protected file changed while hashing: " + repr(relative))
        record = {"sha256": digest.hexdigest(), "mode": stat.S_IMODE(after.st_mode)}
        if expected is not None:
            if record["sha256"] != expected["sha256"]:
                raise ProtectionError("Protected file content changed: " + repr(relative))
            if restore_mode and record["mode"] != expected["mode"]:
                # Fresh workspace copies must not chmod a shared external inode.
                if after.st_nlink != 1:
                    raise ProtectionError("Cannot restore mode of a multiply linked protected file")
                _deadline()
                _bound(directories, bindings, file_fd, leaf)
                os.fchmod(file_fd, expected["mode"])
                _bound(directories, bindings, file_fd, leaf)
                record["mode"] = stat.S_IMODE(os.fstat(file_fd).st_mode)
            if check_mode and record["mode"] != expected["mode"]:
                raise ProtectionError("Protected file mode changed: " + repr(relative))
        _deadline()
        return record
    except OSError as error:
        raise ProtectionError("Cannot safely inspect protected file " + repr(relative) + ": " + str(error)) from error
    finally:
        if file_fd is not None:
            os.close(file_fd)
        for descriptor in reversed(directories):
            os.close(descriptor)


def _metadata(data):
    if (type(data) is not dict or len(data) != 2 or set(data) != {"schema", "files"}
            or type(data["schema"]) is not int or data["schema"] != 1
            or type(data["files"]) is not dict or len(data["files"]) > MAX_PROTECTED_FILES):
        raise ProtectionError("Invalid protected-file metadata schema")
    entries = []
    for name, record in data["files"].items():
        name = _path(name)
        if (type(record) is not dict or len(record) != 2 or set(record) != {"sha256", "mode"}
                or not isinstance(record["sha256"], str)
                or re.fullmatch(r"[0-9a-f]{64}", record["sha256"]) is None
                or type(record["mode"]) is not int or not 0 <= record["mode"] <= 0o7777):
            raise ProtectionError("Invalid protected-file record: " + repr(name))
        entries.append((name, record["sha256"], record["mode"]))
    if len(json.dumps(data, ensure_ascii=False, separators=(",", ":")).encode()) > MAX_METADATA_BYTES:
        raise ProtectionError("Protected-file metadata exceeds its size limit")
    return tuple(sorted(entries))


@dataclass(frozen=True)
class ProtectedFiles:
    _entries: tuple = ()

    @classmethod
    def capture(cls, cwd, paths=None):
        if paths is None:
            encoded = os.environ.get("MAKEWAND_TASK_PROTECTED_PATHS")
            if encoded is None:
                paths = []
            else:
                if len(encoded) > MAX_METADATA_BYTES:
                    raise ProtectionError("Protected-path environment exceeds its size limit")
                try:
                    paths = json.loads(encoded)
                except (ValueError, TypeError) as error:
                    raise ProtectionError("Protected-path environment must contain a JSON array") from error
        if not isinstance(paths, (list, tuple)) or len(paths) > MAX_PROTECTED_FILES:
            raise ProtectionError("Protected paths must be a bounded list")
        names = sorted({_path(value) for value in paths})
        if not names:
            return cls()
        # Apply the complete metadata bound before opening potentially large files.
        _metadata({"schema": 1, "files": {name: {"sha256": "0" * 64, "mode": 0}
                                           for name in names}})
        base = _base(cwd)
        return cls.from_dict({"schema": 1, "files": {name: _inspect(base, name) for name in names}})

    @classmethod
    def from_dict(cls, data):
        return cls(_metadata(data))

    @property
    def paths(self):
        return tuple(name for name, _, _ in self._entries)

    def to_dict(self):
        return {"schema": 1, "files": {name: {"sha256": digest, "mode": mode}
                                       for name, digest, mode in self._entries}}

    def verify(self, workspace):
        if not self._entries:
            return True
        base = _base(workspace)
        for name, digest, mode in self._entries:
            _inspect(base, name, {"sha256": digest, "mode": mode})
        return True

    def prepare_workspace(self, workspace):
        if not self._entries:
            return True
        base = _base(workspace)
        records = self.to_dict()["files"]
        # Validate every file's content before restoring any copy's mode.
        for name, record in records.items():
            _inspect(base, name, record, check_mode=False)
        for name, record in records.items():
            _inspect(base, name, record, restore_mode=True)
        return self.verify(workspace)

    def shell_guard(self, base_cwd, *, rollback_on_error=False):
        if type(rollback_on_error) is not bool:
            raise ProtectionError("Protected preflight rollback option must be boolean")
        if not self._entries:
            return ": # No protected files\n"
        _platform()
        payload = base64.b64encode(json.dumps({"workspace": _base(base_cwd),
                                              "snapshot": self.to_dict()}, ensure_ascii=True).encode()).decode()
        failure = "rollback" if rollback_on_error else 'exit "$?"'
        return ("python3 -I - <<'MAKEWAND_PROTECTED_PREFLIGHT' || " + failure + "\n"
                + _SHELL_SOURCE + "\nfrozen = json.loads(base64.b64decode(" + repr(payload) + "))\n"
                + "try:\n    check(frozen)\nexcept (OSError, ValueError) as error:\n"
                + "    print('Protected files preflight failed: ' + str(error), file=sys.stderr)\n    sys.exit(1)\n"
                + "MAKEWAND_PROTECTED_PREFLIGHT\n")


_SHELL_SOURCE = r'''import base64, hashlib, json, os, stat, sys

def signature(info):
    return (info.st_dev, info.st_ino, info.st_mode, info.st_size,
            info.st_mtime_ns, info.st_ctime_ns)

def bound(directories, bindings, file_fd, leaf):
    for parent, name, child in bindings:
        actual, opened = os.stat(name, dir_fd=parent, follow_symlinks=False), os.fstat(child)
        if not stat.S_ISDIR(actual.st_mode) or (actual.st_dev, actual.st_ino) != (opened.st_dev, opened.st_ino):
            raise ValueError('Protected directory changed')
    actual, opened = os.stat(leaf, dir_fd=directories[-1], follow_symlinks=False), os.fstat(file_fd)
    if not stat.S_ISREG(actual.st_mode) or (actual.st_dev, actual.st_ino) != (opened.st_dev, opened.st_ino):
        raise ValueError('Protected file changed')

def check(frozen):
    if os.name != 'posix' or not hasattr(os, 'O_NOFOLLOW') or not hasattr(os, 'O_DIRECTORY') or os.open not in os.supports_dir_fd:
        raise ValueError('POSIX no-follow descriptors are required')
    for name, expected in frozen['snapshot']['files'].items():
        directories, bindings, file_fd = [], [], None
        try:
            flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | getattr(os, 'O_CLOEXEC', 0)
            directories.append(os.open('/', flags))
            components = [part for part in frozen['workspace'].split('/') if part] + name.split('/')
            for component in components[:-1]:
                parent = directories[-1]
                child = os.open(component, flags, dir_fd=parent)
                bindings.append((parent, component, child))
                directories.append(child)
            leaf = components[-1]
            file_fd = os.open(leaf, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | getattr(os, 'O_CLOEXEC', 0), dir_fd=directories[-1])
            before = os.fstat(file_fd)
            if not stat.S_ISREG(before.st_mode):
                raise ValueError('Protected path is not a regular file: ' + repr(name))
            bound(directories, bindings, file_fd, leaf)
            digest = hashlib.sha256()
            while True:
                chunk = os.read(file_fd, 65536)
                if not chunk:
                    break
                digest.update(chunk)
            after = os.fstat(file_fd)
            bound(directories, bindings, file_fd, leaf)
            if signature(before) != signature(after):
                raise ValueError('Protected file changed while hashing: ' + repr(name))
            if digest.hexdigest() != expected['sha256'] or stat.S_IMODE(after.st_mode) != expected['mode']:
                raise ValueError('Protected file content or mode changed: ' + repr(name))
        finally:
            if file_fd is not None:
                os.close(file_fd)
            for descriptor in reversed(directories):
                os.close(descriptor)
'''
