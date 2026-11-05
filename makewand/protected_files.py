"""Frozen task constraints, verified through native non-following handles.

POSIX captures content and complete permission mode. Native Windows verifies
stable file identities and its supported readonly permission bits; candidate
replacement and rollback preserve Windows DACLs separately. ACLs are not inputs
to this frozen policy. Empty protection is a portable no-op.
"""
import json
import os
import re
import time
from pathlib import PureWindowsPath

MAX_PROTECTED_FILES = 256
MAX_PATH_BYTES = 4096
MAX_PATH_COMPONENTS = 64
MAX_METADATA_BYTES = 1024 * 1024


class ProtectionError(ValueError):
    def __init__(self, message, *, status="UNVERIFIED"):
        super().__init__(message)
        self.status = status


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


