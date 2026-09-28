"""
Makewand Kibitzer Sidecar Memory & Auto-Fix Pattern Engine (inspired by OmO Kibitzer).
Persists past failure patterns, review findings, and successful remediation lessons.
Automatically retrieves relevant quality hints and real-time nudges for new tasks.
"""

import os
import time
import json
import re
from datetime import datetime
from pathlib import Path
from typing import List, Dict, Any, Optional

from makewand import filelock as fcntl
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
    },
    {
        "keywords": ["goroutine", "unbuffered channel", "channel deadlock", "waitgroup", "context cancellation"],
        "issue": "Go goroutine leaks or unbuffered channel deadlocks when receivers exit prematurely.",
        "lesson": "Use buffered channels or context cancellation (ctx.Done()), and always call wg.Done() inside defer."
    },
    {
        "keywords": ["rust", "unwrap()", "panic", "borrow checker"],
        "issue": "Rust unwrap() or expect() in production execution paths triggering panics on unexpected inputs.",
        "lesson": "Propagate Result/Option using '?' or explicit match with defensive fallback instead of unwrap()."
    },
    {
        "keywords": ["bwrap", "sandbox escape", "symlink escape", "path traversal"],
        "issue": "Path traversal or symlink escape out of sandboxed workspace into host directories.",
        "lesson": "Always resolve symlinks and verify canonical paths are strictly contained within base workspace directory."
    },
    {
        "keywords": ["patch parsimony", "bloated diff", "diff churn", "excessive changes"],
        "issue": "Sprawling unrequested refactoring causing high diff churn and regression risks.",
        "lesson": "Keep patches parsimonious and focused on the exact bug or feature requested without touching unrelated files."
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

def record_failure_pattern(issue: str, lesson: str, keywords: Optional[List[str]] = None) -> bool:
    """
    Records a historical failure pattern or quality rule into pattern memory (OmO Kibitzer style).
    If keywords is omitted, extracts key terms from issue and lesson.
    """
    if not issue or not lesson:
        return False

    if not keywords:
        raw_words = re.findall(r"\b[a-zA-Z0-9_-]{4,}\b", (issue + " " + lesson).lower())
        stop_words = {
            "this", "that", "with", "from", "have", "test", "tests", "fail",
            "failed", "when", "always", "must", "should", "using", "into"
        }
        keywords = [w for w in raw_words if w not in stop_words][:6]

    clean_k = [k.lower().strip() for k in keywords if k.strip()]
    if not clean_k:
        clean_k = ["general"]

    return record_autofix_lesson(keywords=clean_k, issue=issue, lesson=lesson)

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

def get_kibitzer_nudges(prompt: str, stage: str = "implementation", max_nudges: int = 4) -> List[Dict[str, str]]:
    """
    Finds relevant quality rules and failure pitfalls for prompt context (inspired by OmO Kibitzer).
    Stage can be 'implementation' (for coders) or 'review' (for auditors/referees).
    """
    p_lower = prompt.lower()
    patterns = _load_patterns()
    matched = []

    domain_map = {
        "go": ["goroutine", "channel", "deadlock"],
        "golang": ["goroutine", "channel", "deadlock"],
        "concurrency": ["goroutine", "channel", "deadlock", "postgres", "lock", "concurrency"],
        "async": ["goroutine", "channel", "deadlock"],
        "test": ["mock", "patch", "tempfile"],
        "mock": ["mock", "patch"],
        "sandbox": ["bwrap", "sandbox", "symlink", "traversal"],
        "security": ["bwrap", "sandbox", "symlink", "traversal"],
        "rust": ["rust", "unwrap", "panic"],
        "refactor": ["parsimony", "diff", "churn", "refactor"],
        "minimal": ["parsimony", "diff", "churn"],
    }

    active_keys = set()
    for word, mapped in domain_map.items():
        if word in p_lower:
            active_keys.update(mapped)

    for p in patterns:
        keys = p.get("keywords", [])
        if any(k in p_lower for k in keys) or any(k in active_keys for k in keys):
            if stage == "review":
                matched.append({
                    "focus": f"核验隐患: {p.get('issue', '')}",
                    "guidance": f"验收准则: {p.get('lesson', '')}"
                })
            else:
                matched.append({
                    "focus": p.get("issue", ""),
                    "guidance": p.get("lesson", "")
                })
            if len(matched) >= max_nudges:
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

def format_kibitzer_guidance(prompt: str, stage: str = "implementation") -> str:
    """
    Formats matched Kibitzer quality nudges as an instruction block (inspired by OmO Kibitzer).
    Stage can be 'implementation' or 'review'.
    """
    nudges = get_kibitzer_nudges(prompt, stage=stage)
    if not nudges:
        return ""

    if stage == "review":
        lines = ["\n【Makewand Kibitzer 独立审计核查要点】"]
        for i, n in enumerate(nudges, 1):
            lines.append(f"{i}. {n['focus']}")
            lines.append(f"   {n['guidance']}")
    else:
        lines = ["\n【Makewand Kibitzer 实时工程质量与避坑护航】"]
        for i, n in enumerate(nudges, 1):
            lines.append(f"{i}. 避坑要点: {n['focus']}")
            lines.append(f"   质量准则: {n['guidance']}")
    return "\n".join(lines) + "\n"


# =========================================================================
# Workspace Playbook & Repo Knowledge Persistence (inspired by Devin / Claude Code)
# =========================================================================

import hashlib

def _get_playbook_path(cwd: str) -> Path:
    clean_cwd = Path(os.path.realpath(os.path.abspath(cwd)))
    repo_playbook = clean_cwd / ".makewand" / "playbook.json"
    if repo_playbook.parent.exists() or (clean_cwd / ".git").exists():
        return repo_playbook
    # fallback to CONFIG_DIR / "playbooks" / <hash>.json
    h = hashlib.sha256(str(clean_cwd).encode("utf-8")).hexdigest()[:16]
    return CONFIG_DIR / "playbooks" / f"{h}.json"

def load_workspace_playbook(cwd: str) -> Dict[str, Any]:
    """
    Loads persistent workspace playbook containing verified build/test commands
    and repo-specific architectural conventions.
    """
    p = _get_playbook_path(cwd)
    default_pb = {
        "verified_test_commands": [],
        "verified_build_commands": [],
        "verified_lint_commands": [],
        "project_conventions": []
    }
    if not p.exists():
        return default_pb
    try:
        with open(p, "r", encoding="utf-8") as f:
            data = json.load(f)
            if isinstance(data, dict):
                for k in default_pb:
                    if k not in data:
                        data[k] = []
                return data
    except Exception:
        pass
    return default_pb

def save_workspace_playbook(cwd: str, playbook: Dict[str, Any]) -> bool:
    """
    Saves workspace playbook atomically.
    """
    p = _get_playbook_path(cwd)
    try:
        p.parent.mkdir(parents=True, exist_ok=True)
        tmp = p.with_suffix(f".tmp_{os.getpid()}_{time.time_ns()}")
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(playbook, f, ensure_ascii=False, indent=2)
        os.replace(tmp, p)
        return True
    except Exception:
        return False

def record_verified_command(cwd: str, category: str, command: str) -> None:
    """
    Records a verified working test, build, or lint command for this repository.
    """
    if not command or not command.strip():
        return
    cmd = command.strip()
    key_map = {
        "test": "verified_test_commands",
        "build": "verified_build_commands",
        "lint": "verified_lint_commands",
    }
    key = key_map.get(category, "verified_test_commands")
    data = load_workspace_playbook(cwd)
    cmd_list = data.get(key, [])
    if cmd not in cmd_list:
        cmd_list.append(cmd)
        data[key] = cmd_list[-5:]  # keep top 5
        save_workspace_playbook(cwd, data)

def record_project_convention(cwd: str, convention: str) -> None:
    """
    Records a repository-specific architectural convention or quirk learned across sessions.
    """
    if not convention or not convention.strip():
        return
    conv = convention.strip()
    data = load_workspace_playbook(cwd)
    conv_list = data.get("project_conventions", [])
    if conv not in conv_list:
        conv_list.append(conv)
        data["project_conventions"] = conv_list[-10:]
        save_workspace_playbook(cwd, data)

def format_playbook_for_prompt(cwd: str) -> str:
    """
    Formats the workspace playbook as a prompt instruction section.
    """
    data = load_workspace_playbook(cwd)
    tests = data.get("verified_test_commands", [])
    builds = data.get("verified_build_commands", [])
    lints = data.get("verified_lint_commands", [])
    convs = data.get("project_conventions", [])

    if not tests and not builds and not lints and not convs:
        return ""

    lines = ["\n【工程专属构建与测试指南 (Workspace Playbook)】"]
    if tests:
        lines.append(f"• 已验证测试指令: {', '.join(tests)}")
    if builds:
        lines.append(f"• 已验证构建指令: {', '.join(builds)}")
    if lints:
        lines.append(f"• 已验证代码规范/Lint: {', '.join(lints)}")
    if convs:
        lines.append("• 本工程专属经验与规约:")
        for c_rule in convs:
            lines.append(f"  - {c_rule}")
    return "\n".join(lines) + "\n"

