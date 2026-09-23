"""
Makewand Auto-Fix Pattern Memory.
Persists past failure patterns, review findings, and successful remediation lessons.
Automatically retrieves relevant hints for new tasks to avoid repeated pitfalls.
"""

import os
import time
import json
import fcntl
from datetime import datetime
from pathlib import Path
from typing import List, Dict, Any, Optional
from makewand.config import CONFIG_DIR, ensure_config_dir

PATTERNS_FILE = CONFIG_DIR / "autofix_patterns.json"
PATTERNS_LOCK = CONFIG_DIR / "autofix_patterns.lock"

DEFAULT_PATTERNS = [
    {
        "keywords": ["mock", "patch", "unittest.mock"],
        "issue": "Mock return value unpack failure: Mock objects without explicit return_value raise ValueError on tuple unpacking.",
        "lesson": "When mocking functions returning tuples (e.g. (success, out, err)), always provide a mock tuple or use safe tuple length fallback."
    },
    {
        "keywords": ["tempfile", "temporarydirectory", "tmpdir"],
        "issue": "TemporaryDirectory used inside test functions without top-level import or clean exit.",
        "lesson": "Ensure tempfile.TemporaryDirectory or tempfile.mkdtemp is imported at module top and cleaned up via try/finally or tearDown."
    },
    {
        "keywords": ["postgres", "lock", "deadlock", "flock", "concurrency"],
        "issue": "Long running database queries holding exclusive locks block CI runners.",
        "lesson": "Use non-blocking locking (flock -n) and ephemeral test databases with explicit query timeout and statement_timeout."
    },
    {
        "keywords": ["worktree", "git status", "concurrent"],
        "issue": "Multiple sessions modifying the same working tree causing index collision and dirty overwrites.",
        "lesson": "Strictly create independent git worktrees (git worktree add) under /tmp/makewand-shadow-worktrees."
    }
]

def _load_patterns_unlocked() -> List[Dict[str, Any]]:
    """
    Loads patterns from disk without acquiring PATTERNS_LOCK.
    Assumes caller either holds PATTERNS_LOCK or is performing an unlocked read.
    """
    if not PATTERNS_FILE.exists():
        return list(DEFAULT_PATTERNS)
    try:
        with open(PATTERNS_FILE, "r", encoding="utf-8") as f:
            data = json.load(f)
        if isinstance(data, list):
            return data
    except Exception:
        pass
    return list(DEFAULT_PATTERNS)

def _save_patterns_unlocked(patterns: List[Dict[str, Any]]) -> bool:
    """
    Saves patterns atomically via temporary file without acquiring PATTERNS_LOCK.
    Returns True on success, False on failure.
    """
    ensure_config_dir()
    tmp = PATTERNS_FILE.with_suffix(f".tmp_{os.getpid()}_{time.time_ns()}")
    try:
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(patterns, f, ensure_ascii=False, indent=2)
        os.replace(tmp, PATTERNS_FILE)
        return True
    except Exception:
        if tmp.exists():
            try:
                tmp.unlink()
            except Exception:
                pass
        return False

def _ensure_initialized() -> None:
    ensure_config_dir()
    if not PATTERNS_FILE.exists():
        with open(PATTERNS_LOCK, "w") as lock_f:
            fcntl.flock(lock_f, fcntl.LOCK_EX)
            try:
                if not PATTERNS_FILE.exists():
                    _save_patterns_unlocked(DEFAULT_PATTERNS)
            finally:
                fcntl.flock(lock_f, fcntl.LOCK_UN)

def _load_patterns() -> List[Dict[str, Any]]:
    ensure_config_dir()
    if not PATTERNS_FILE.exists():
        _ensure_initialized()
    return _load_patterns_unlocked()

def _save_patterns(patterns: List[Dict[str, Any]]) -> bool:
    ensure_config_dir()
    with open(PATTERNS_LOCK, "w") as lock_f:
        fcntl.flock(lock_f, fcntl.LOCK_EX)
        try:
            return _save_patterns_unlocked(patterns)
        finally:
            fcntl.flock(lock_f, fcntl.LOCK_UN)

def record_autofix_lesson(keywords: List[str], issue: str, lesson: str) -> bool:
    """
    Persist a newly learned lesson into pattern memory with single transactional lock protection.
    Guaranteed no re-entrant lock acquisition or self-deadlock.
    """
    clean_k = [k.lower().strip() for k in keywords if k.strip()]
    if not clean_k or not issue or not lesson:
        return False

    ensure_config_dir()
    with open(PATTERNS_LOCK, "w") as lock_f:
        fcntl.flock(lock_f, fcntl.LOCK_EX)
        try:
            patterns = _load_patterns_unlocked()
            # Check for duplicate
            for p in patterns:
                if p.get("issue") == issue:
                    p["keywords"] = list(set(p.get("keywords", []) + clean_k))
                    p["lesson"] = lesson
                    p["updated_at"] = datetime.now().isoformat()
                    return _save_patterns_unlocked(patterns)

            patterns.append({
                "keywords": clean_k,
                "issue": issue[:200],
                "lesson": lesson[:300],
                "created_at": datetime.now().isoformat()
            })
            return _save_patterns_unlocked(patterns)
        finally:
            fcntl.flock(lock_f, fcntl.LOCK_UN)

def get_relevant_hints(prompt: str, max_hints: int = 3) -> List[Dict[str, str]]:
    """
    Find relevant cautionary hints for a given prompt based on past patterns.
    """
    p_lower = prompt.lower()
    patterns = _load_patterns()
    matched = []

    for p in patterns:
        keys = p.get("keywords", [])
        if any(k in p_lower for k in keys):
            matched.append({
                "issue": p.get("issue", ""),
                "lesson": p.get("lesson", "")
            })
            if len(matched) >= max_hints:
                break

    return matched

def format_memory_hints_for_prompt(prompt: str) -> str:
    """
    Formats matched hints as clean instruction block to append to implementation prompt.
    """
    hints = get_relevant_hints(prompt)
    if not hints:
        return ""

    lines = ["\n【历史避坑与质量规范提示 (Makewand Pattern Memory)】"]
    for i, h in enumerate(hints, 1):
        lines.append(f"{i}. 避坑点: {h['issue']}")
        lines.append(f"   防范准则: {h['lesson']}")
    return "\n".join(lines) + "\n"
