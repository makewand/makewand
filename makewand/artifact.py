"""Snapshots binding tests and reviews to workspace inputs, including modes."""

import hashlib
import os
import stat
import time
from pathlib import Path

# Tool caches are not source inputs or candidate deliverables. Existing source
# files elsewhere, new files, deletions, links and mode changes are all checked.
CACHE_DIRS = {".git", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", "node_modules", ".makewand", ".venv", "venv", "target"}


def workspace_snapshot(workspace):
    if os.name == "nt":
        from makewand.native_windows import manifest
        return manifest(workspace, input_snapshot=True)
    root = Path(workspace)
    result = {}
    paths_to_check = set()
    started = time.monotonic()
    from makewand.git_helper import run_git_cmd
    code, tracked, _ = run_git_cmd(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "."], cwd=str(root), binary=True)
    for directory, dirs, files in os.walk(root, followlinks=False):
        if time.monotonic() - started > 10:
            raise OSError("workspace snapshot exceeded its 10 second budget")
        dirs[:] = sorted(d for d in dirs if d not in (CACHE_DIRS if code == 0 else {".git"}))
        paths = files + [d for d in dirs if (Path(directory) / d).is_symlink()]
        paths_to_check.update(Path(directory) / name for name in paths)
        if len(paths_to_check) > 100000:
            raise OSError("workspace snapshot exceeded 100000 files")
    # Both tracked AND deliverable untracked files are inputs, even inside a
    # cache-like directory. Only ignored generated artifacts may be excluded.
    if code == 0:
        for name in tracked.split(b"\0"):
            if name:
                relative = Path(os.fsdecode(name))
                if relative.is_absolute() or ".." in relative.parts:
                    raise OSError("invalid tracked input path")
                path = root / relative
                if path.exists() or path.is_symlink():
                    paths_to_check.add(path)
                if len(paths_to_check) > 100000:
                    raise OSError("workspace snapshot exceeded 100000 files")
    total_bytes = 0
    for path in sorted(paths_to_check):
        if time.monotonic() - started > 10 or total_bytes > 512 * 1024 * 1024:
            raise OSError("workspace snapshot exceeded its input budget")
        rel = path.relative_to(root).as_posix()
        info = path.lstat()
        mode = stat.S_IMODE(info.st_mode)
        if stat.S_ISLNK(info.st_mode):
            result[rel] = ("link", os.readlink(path), mode)
        elif stat.S_ISREG(info.st_mode):
            digest = hashlib.sha256()
            with path.open("rb") as f:
                while chunk := f.read(65536):
                    total_bytes += len(chunk)
                    if time.monotonic() - started > 10 or total_bytes > 512 * 1024 * 1024:
                        raise OSError("workspace snapshot exceeded its input budget")
                    digest.update(chunk)
            result[rel] = ("file", digest.hexdigest(), mode)
        else:
            result[rel] = ("special", stat.S_IFMT(info.st_mode), mode)
    return result


def changed_inputs(before, after):
    return sorted(path for path in before.keys() | after.keys() if before.get(path) != after.get(path))
