"""
Makewand Search Guardrail: Fast, budgeted search avoiding cold archives, databases, and heavy binaries.
"""

import os
import stat
import re
import fnmatch
import json
import shutil
import subprocess
import time
import select
import codecs
from pathlib import Path
from typing import List, Dict, Optional, Any

from makewand.constants import PROJECT_IGNORE_DIRS

DEFAULT_EXCLUDE_DIRS = set(PROJECT_IGNORE_DIRS) | {
    ".git", ".svn", ".hg",
    "__pycache__", ".venv-dbs", "runtime-venvs",
    "data", "data2", "data_dbs",
    "models", "logs",
    "cold_archive", "basin_canonical_slice_cold_archive",
    "historical-retired", "snapshots", "backups",
    ".claude", ".gemini"
}

DEFAULT_EXCLUDE_EXTS = {
    ".sqlite", ".db", ".gpkg", ".pbf", ".dump", ".bin",
    ".tar", ".gz", ".zst", ".zip", ".7z", ".bz2",
    ".pyc", ".pyo", ".so", ".dylib", ".dll", ".exe",
    ".png", ".jpg", ".jpeg", ".gif", ".webp", ".pdf"
}

MAX_FILE_SIZE_BYTES = 2 * 1024 * 1024  # 2MB per text file
MAX_RG_TIME_SECONDS = 15.0
MAX_WALK_TIME_SECONDS = 15.0
MAX_WALK_VISITED_FILES = 10000
MAX_WALK_VISITED_BYTES = 100 * 1024 * 1024  # 100MB


def find_ripgrep() -> Optional[str]:
    """Finds available ripgrep binary, preferring /usr/bin/rg."""
    for candidate in ("/usr/bin/rg", shutil.which("rg")):
        if candidate and os.path.isfile(candidate) and os.access(candidate, os.X_OK):
            return candidate
    return None


def _safe_search_rg(
    rg_path: str,
    pattern: str,
    root: Path,
    max_results: int = 150,
    max_depth: int = 6,
    ignore_case: bool = True
) -> Optional[List[Dict[str, Any]]]:
    """Runs rg --json and returns matching results, or None on failure."""
    cmd = [
        rg_path,
        "--json",
        "--hidden",
        "--no-ignore-vcs",
        "--max-count", str(max_results),
        "--max-filesize", f"{MAX_FILE_SIZE_BYTES}",
        "--max-depth", str(max_depth),
        "--no-follow",
        "--no-messages",
    ]
    if ignore_case:
        cmd.append("-i")
    else:
        cmd.append("-s")

    for d in DEFAULT_EXCLUDE_DIRS:
        cmd.extend(["-g", f"!{d}/**", "-g", f"!{d}"])

    for ext in DEFAULT_EXCLUDE_EXTS:
        cmd.extend(["-g", f"!*{ext}"])

    # Test if pattern compiles as valid regex
    try:
        re.compile(pattern)
        cmd.extend(["--", pattern, str(root)])
    except re.error:
        cmd.extend(["-F", "--", pattern, str(root)])

    proc = None
    try:
        proc = subprocess.Popen(
            cmd,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
            bufsize=1,
        )
        results = []
        start_time = time.monotonic()
        if proc.stdout is not None:
            has_native_fd = False
            if hasattr(proc.stdout, "fileno"):
                try:
                    fd = proc.stdout.fileno()
                    if isinstance(fd, int) and fd >= 0:
                        has_native_fd = True
                except Exception:
                    has_native_fd = False

            def _handle_line(line_str: str) -> bool:
                nonlocal results
                line_str = line_str.strip()
                if not line_str:
                    return False
                try:
                    item = json.loads(line_str)
                except Exception:
                    return False
                if item.get("type") == "match":
                    data = item.get("data", {})
                    path_obj = data.get("path", {})
                    raw_path = path_obj.get("text")
                    if not raw_path and "bytes" in path_obj:
                        try:
                            import base64
                            raw_path = base64.b64decode(path_obj["bytes"]).decode("utf-8", errors="replace")
                        except Exception:
                            raw_path = None
                    if not raw_path:
                        return False
                    try:
                        rel_p = os.path.relpath(raw_path, str(root))
                    except Exception:
                        rel_p = raw_path
                    line_num = data.get("line_number", 0)
                    lines_obj = data.get("lines", {})
                    line_text = lines_obj.get("text")
                    if line_text is None and "bytes" in lines_obj:
                        try:
                            import base64
                            line_text = base64.b64decode(lines_obj["bytes"]).decode("utf-8", errors="replace")
                        except Exception:
                            line_text = ""
                    line_text = line_text or ""
                    results.append({
                        "file": rel_p,
                        "line_num": line_num,
                        "content": line_text.strip()[:200]
                    })
                    if len(results) >= max_results:
                        return True
                return False

            if has_native_fd:
                buf = ""
                decoder = codecs.getincrementaldecoder("utf-8")("replace")
                fd = proc.stdout.fileno()
                while True:
                    remaining = MAX_RG_TIME_SECONDS - (time.monotonic() - start_time)
                    if remaining <= 0:
                        try:
                            proc.terminate()
                            proc.wait(timeout=0.2)
                        except Exception:
                            try:
                                proc.kill()
                                proc.wait(timeout=0.2)
                            except Exception:
                                pass
                        break
                    rlist, _, _ = select.select([fd], [], [], max(0.0, remaining))
                    if not rlist:
                        try:
                            proc.terminate()
                            proc.wait(timeout=0.2)
                        except Exception:
                            try:
                                proc.kill()
                                proc.wait(timeout=0.2)
                            except Exception:
                                pass
                        break
                    try:
                        chunk = os.read(fd, 65536)
                    except OSError:
                        break
                    if not chunk:
                        # EOF
                        break
                    buf += decoder.decode(chunk)
                    stop = False
                    while "\n" in buf:
                        line, buf = buf.split("\n", 1)
                        if _handle_line(line):
                            stop = True
                            break
                    if stop or len(results) >= max_results:
                        break
                if buf and len(results) < max_results:
                    buf += decoder.decode(b"", final=True)
                    if buf:
                        _handle_line(buf)
            else:
                for line in proc.stdout:
                    if time.monotonic() - start_time > MAX_RG_TIME_SECONDS:
                        try:
                            proc.terminate()
                            proc.wait(timeout=0.2)
                        except Exception:
                            try:
                                proc.kill()
                                proc.wait(timeout=0.2)
                            except Exception:
                                pass
                        break
                    if _handle_line(line):
                        break
        return results
    except Exception:
        return None
    finally:
        if proc is not None:
            if proc.stdout is not None:
                try:
                    proc.stdout.close()
                except Exception:
                    pass
            try:
                proc.terminate()
                proc.wait(timeout=0.2)
            except Exception:
                try:
                    proc.kill()
                    proc.wait(timeout=0.2)
                except Exception:
                    pass


def _safe_search_walk(
    pattern: str,
    root: Path,
    max_results: int = 150,
    max_depth: int = 6,
    ignore_case: bool = True
) -> List[Dict[str, Any]]:
    """Fallback directory walker using pure Python os.walk with time budget and file/byte guards."""
    results = []
    flags = re.IGNORECASE if ignore_case else 0
    try:
        regex = re.compile(pattern, flags)
    except re.error:
        regex = re.compile(re.escape(pattern), flags)

    root_depth = len(root.parts)
    start_time = time.monotonic()
    visited_files = 0
    visited_bytes = 0

    for dirpath, dirnames, filenames in os.walk(str(root), followlinks=False):
        if time.monotonic() - start_time > MAX_WALK_TIME_SECONDS:
            break
        current_depth = len(Path(dirpath).parts) - root_depth
        if current_depth >= max_depth:
            dirnames.clear()
            continue

        # Prune excluded directories in-place
        dirnames[:] = [
            d for d in dirnames
            if not any(fnmatch.fnmatch(d.lower(), pat.lower()) for pat in DEFAULT_EXCLUDE_DIRS)
        ]

        for fname in filenames:
            if time.monotonic() - start_time > MAX_WALK_TIME_SECONDS:
                return results
            if visited_files >= MAX_WALK_VISITED_FILES or visited_bytes >= MAX_WALK_VISITED_BYTES:
                return results

            ext = os.path.splitext(fname)[1].lower()
            if ext in DEFAULT_EXCLUDE_EXTS:
                continue

            full_path = os.path.join(dirpath, fname)
            fd = None
            try:
                open_flags = os.O_RDONLY | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
                fd = os.open(full_path, open_flags)
                st = os.fstat(fd)
                if not stat.S_ISREG(st.st_mode) or st.st_size > MAX_FILE_SIZE_BYTES:
                    os.close(fd)
                    fd = None
                    continue

                visited_files += 1
                visited_bytes += st.st_size

                with open(fd, "r", encoding="utf-8", errors="ignore", closefd=True) as f:
                    fd = None  # fd ownership transferred to file object
                    for line_num, line in enumerate(f, 1):
                        if regex.search(line):
                            rel_path = os.path.relpath(full_path, str(root))
                            results.append({
                                "file": rel_path,
                                "line_num": line_num,
                                "content": line.strip()[:200]
                            })
                            if len(results) >= max_results:
                                return results
            except (PermissionError, FileNotFoundError, OSError):
                continue
            finally:
                if fd is not None:
                    try:
                        os.close(fd)
                    except OSError:
                        pass

    return results


def safe_search(
    pattern: str,
    root_path: Optional[str] = None,
    max_results: int = 150,
    max_depth: int = 6,
    ignore_case: bool = True
) -> List[Dict[str, Any]]:
    """
    Performs a budgeted, safe text search that strictly avoids deep data archives and binaries.
    Prefers /usr/bin/rg --json when available for 10-50x faster code search, gracefully falling back
    to Python os.walk.
    """
    root = Path(root_path or os.getcwd()).resolve()

    rg_bin = find_ripgrep()
    if rg_bin:
        rg_results = _safe_search_rg(rg_bin, pattern, root, max_results, max_depth, ignore_case)
        if rg_results is not None:
            return rg_results

    return _safe_search_walk(pattern, root, max_results, max_depth, ignore_case)
