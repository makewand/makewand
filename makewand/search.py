"""
Makewand Search Guardrail: Fast, budgeted search avoiding cold archives, databases, and heavy binaries.
"""

import os
import re
import fnmatch
from pathlib import Path
from typing import List, Dict, Optional

DEFAULT_EXCLUDE_DIRS = {
    ".git", ".svn", ".hg",
    "__pycache__", ".pytest_cache", ".mypy_cache",
    ".venv", "venv", ".venv-dbs", "runtime-venvs",
    "node_modules",
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

def safe_search(
    pattern: str,
    root_path: Optional[str] = None,
    max_results: int = 150,
    max_depth: int = 6,
    ignore_case: bool = True
) -> List[Dict[str, any]]:
    """
    Performs a budgeted, safe text search that strictly avoids deep data archives and binaries.
    """
    root = Path(root_path or os.getcwd()).resolve()
    results = []
    flags = re.IGNORECASE if ignore_case else 0
    try:
        regex = re.compile(pattern, flags)
    except re.error as e:
        regex = re.compile(re.escape(pattern), flags)

    root_depth = len(root.parts)

    for dirpath, dirnames, filenames in os.walk(str(root), followlinks=False):
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
            ext = os.path.splitext(fname)[1].lower()
            if ext in DEFAULT_EXCLUDE_EXTS:
                continue

            full_path = os.path.join(dirpath, fname)
            try:
                # Check size before opening
                st = os.stat(full_path, follow_symlinks=False)
                if st.st_size > MAX_FILE_SIZE_BYTES:
                    continue

                with open(full_path, "r", encoding="utf-8", errors="ignore") as f:
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

    return results
