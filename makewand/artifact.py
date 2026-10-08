"""Snapshots binding tests and reviews to workspace inputs, including modes."""

import hashlib
import os
import stat
import time
from pathlib import Path

# Tool caches are not source inputs or candidate deliverables. Existing source
# files elsewhere, new files, deletions, links and mode changes are all checked.
CACHE_DIRS = {
    ".git", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache",
    "node_modules", ".makewand", ".venv", "venv", "target", ".hypothesis",
    ".tox", ".nox", ".nyc_output", "htmlcov",
}

try:
    from makewand.constants import PROJECT_IGNORE_DIRS
    ALL_IGNORE_DIRS = CACHE_DIRS | PROJECT_IGNORE_DIRS
except ImportError:
    ALL_IGNORE_DIRS = CACHE_DIRS | {
        ".git", "node_modules", "vendor", "target", "dist", "build",
        ".pytest_cache", ".mypy_cache", ".ruff_cache", ".venv", "venv",
        "env", ".coverage", ".tox", ".idea", ".vscode", "site-packages",
        ".makewand_sandbox_home", ".hypothesis", ".nox", ".nyc_output", "htmlcov",
    }

# Stat cache to avoid re-reading and re-hashing unmodified files across repeated snapshots:
# (st_dev, st_ino, st_size, st_mtime_ns) -> (sha256_hex, mode)
_STAT_CACHE = {}
_MAX_STAT_CACHE_SIZE = 200000


def _get_snapshot_limits():
    max_bytes_env = os.environ.get("MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES") or os.environ.get("MAKEWAND_DELIVERY_MAX_BYTES")
    try:
        max_bytes = int(max_bytes_env) if max_bytes_env else 512 * 1024 * 1024
    except ValueError:
        max_bytes = 512 * 1024 * 1024

    timeout_env = os.environ.get("MAKEWAND_WORKSPACE_SNAPSHOT_TIMEOUT_SECS")
    try:
        timeout_secs = float(timeout_env) if timeout_env else 10.0
    except ValueError:
        timeout_secs = 10.0

    max_files_env = os.environ.get("MAKEWAND_WORKSPACE_SNAPSHOT_MAX_FILES")
    try:
        max_files = int(max_files_env) if max_files_env else 100000
    except ValueError:
        max_files = 100000

    return max_bytes, timeout_secs, max_files


def clear_stat_cache():
    """Clear internal stat cache (useful in tests)."""
    _STAT_CACHE.clear()


def workspace_snapshot(workspace):
    if os.name == "nt":
        from makewand.native_windows import manifest
        return manifest(workspace, input_snapshot=True)
    root = Path(workspace)
    result = {}
    paths_to_check = set()
    started = time.monotonic()
    max_bytes, timeout_secs, max_files = _get_snapshot_limits()

    from makewand.git_helper import run_git_cmd, get_submodule_paths
    code, tracked, _ = run_git_cmd(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-per-directory=.gitignore", "--", "."],
        cwd=str(root),
        binary=True
    )

    if code == 0:
        # In a git repository, git ls-files respecting .gitignore gives all tracked
        # source files and deliverable untracked files while excluding project-level
        # ignored build outputs and datasets (dist/, data/, build/, weights, etc.).
        # Note: --exclude-per-directory=.gitignore explicitly ensures internal metadata
        # exclusions (.git/info/exclude) cannot maliciously hide uncommitted files.
        for name in tracked.split(b"\0"):
            if not name:
                continue
            relative = Path(os.fsdecode(name))
            if relative.is_absolute() or ".." in relative.parts:
                raise OSError("invalid tracked input path")
            path = root / relative
            if path.exists() or path.is_symlink():
                paths_to_check.add(path)
            if len(paths_to_check) > max_files:
                raise OSError(f"workspace snapshot exceeded {max_files} files")

        # Handle submodules: re-enumerate tracked and deliverable files within each submodule
        try:
            submodules = get_submodule_paths(str(root))
            for sub in submodules:
                sub_root = root / sub
                if not sub_root.is_dir():
                    continue
                sub_code, sub_tracked, _ = run_git_cmd(
                    ["git", "ls-files", "-z", "--cached", "--others", "--exclude-per-directory=.gitignore", "--", "."],
                    cwd=str(sub_root),
                    binary=True
                )
                if sub_code == 0:
                    for sub_name in sub_tracked.split(b"\0"):
                        if not sub_name:
                            continue
                        sub_rel = Path(sub) / os.fsdecode(sub_name)
                        if sub_rel.is_absolute() or ".." in sub_rel.parts:
                            continue
                        sub_path = root / sub_rel
                        if sub_path.exists() or sub_path.is_symlink():
                            paths_to_check.add(sub_path)
                        if len(paths_to_check) > max_files:
                            raise OSError(f"workspace snapshot exceeded {max_files} files")
        except Exception:
            pass
    else:
        # Fallback for non-git workspaces: walk directory tree while pruning ignored dirs
        for directory, dirs, files in os.walk(root, followlinks=False):
            if time.monotonic() - started > timeout_secs:
                raise OSError(f"workspace snapshot exceeded its {timeout_secs} second budget")
            dirs[:] = sorted(d for d in dirs if d not in ALL_IGNORE_DIRS)
            paths = files + [d for d in dirs if (Path(directory) / d).is_symlink()]
            paths_to_check.update(Path(directory) / name for name in paths)
            if len(paths_to_check) > max_files:
                raise OSError(f"workspace snapshot exceeded {max_files} files")

    total_bytes = 0
    for path in sorted(paths_to_check):
        if time.monotonic() - started > timeout_secs:
            raise OSError(f"workspace snapshot exceeded its {timeout_secs} second budget")
        if total_bytes > max_bytes:
            raise OSError(f"workspace snapshot exceeded size budget ({total_bytes} > {max_bytes} bytes). Configure MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES or check .gitignore.")
        rel = path.relative_to(root).as_posix()
        info = path.lstat()
        mode = stat.S_IMODE(info.st_mode)
        if stat.S_ISLNK(info.st_mode):
            result[rel] = ("link", os.readlink(path), mode)
        elif stat.S_ISREG(info.st_mode):
            mtime_ns = getattr(info, "st_mtime_ns", int(info.st_mtime * 1e9))
            stat_key = (info.st_dev, info.st_ino, info.st_size, mtime_ns)
            if stat_key in _STAT_CACHE:
                cached_digest, _ = _STAT_CACHE[stat_key]
                result[rel] = ("file", cached_digest, mode)
                total_bytes += info.st_size
                continue

            digest = hashlib.sha256()
            with path.open("rb") as f:
                while chunk := f.read(65536):
                    total_bytes += len(chunk)
                    if time.monotonic() - started > timeout_secs:
                        raise OSError(f"workspace snapshot exceeded its {timeout_secs} second budget")
                    if total_bytes > max_bytes:
                        raise OSError(f"workspace snapshot exceeded size budget ({total_bytes} > {max_bytes} bytes). Configure MAKEWAND_WORKSPACE_SNAPSHOT_MAX_BYTES or check .gitignore.")
                    digest.update(chunk)
            hexdigest = digest.hexdigest()
            if len(_STAT_CACHE) < _MAX_STAT_CACHE_SIZE:
                _STAT_CACHE[stat_key] = (hexdigest, mode)
            result[rel] = ("file", hexdigest, mode)
        else:
            result[rel] = ("special", stat.S_IFMT(info.st_mode), mode)
    return result


def changed_inputs(before, after, ignore_names=None):
    changed = sorted(path for path in before.keys() | after.keys() if before.get(path) != after.get(path))
    if ignore_names:
        filtered = []
        for p in changed:
            parts = Path(p).parts
            if not any(ign in parts or Path(p).name.startswith(ign) for ign in ignore_names):
                filtered.append(p)
        return filtered
    return changed
