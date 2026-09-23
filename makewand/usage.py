"""
Makewand Usage Tracker and Sliding Window Burn Rate Estimator.
Maintains persistent rolling usage window for Claude, Codex, AGY, and Muse subscriptions.
"""

import os
import json
import fcntl
from datetime import datetime, timedelta
from typing import Dict, Any, List, Tuple, Optional
from pathlib import Path
from makewand.config import CONFIG_DIR, ensure_config_dir

USAGE_WINDOW_FILE = CONFIG_DIR / "usage_window.json"

# Window thresholds for burn-rate protection:
# Codex: rolling 3~4h window has quota refresh; warn/penalize above 20 calls/3h
# Claude: rolling 24h/7d budget; penalize above 35 calls/24h or 120 calls/7d to save weekly tokens
# AGY: unlimited Pro subscription anchor (0 penalty)
CODEX_WARN_3H = 20
CODEX_LIMIT_3H = 35

CLAUDE_WARN_24H = 35
CLAUDE_LIMIT_24H = 60
CLAUDE_WARN_7D = 120
CLAUDE_LIMIT_7D = 200

def _get_lock_file() -> Path:
    ensure_config_dir()
    return USAGE_WINDOW_FILE.parent / f".{USAGE_WINDOW_FILE.stem}.lock"

def _load_raw_usage_records(max_age_days: float = 7.0) -> List[Dict[str, Any]]:
    ensure_config_dir()
    if not USAGE_WINDOW_FILE.exists():
        return []

    lock_file = _get_lock_file()
    data = []
    try:
        with open(lock_file, "a+", encoding="utf-8") as lock_f:
            fcntl.flock(lock_f.fileno(), fcntl.LOCK_SH)
            try:
                if USAGE_WINDOW_FILE.exists():
                    with open(USAGE_WINDOW_FILE, "r", encoding="utf-8") as f:
                        data = json.load(f)
            finally:
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_UN)
    except Exception:
        try:
            if USAGE_WINDOW_FILE.exists():
                with open(USAGE_WINDOW_FILE, "r", encoding="utf-8") as f:
                    data = json.load(f)
        except Exception:
            return []

    if not isinstance(data, list):
        return []

    cutoff = datetime.now() - timedelta(days=max_age_days)
    valid_records = []
    for r in data:
        if not isinstance(r, dict) or "timestamp" not in r or "engine" not in r:
            continue
        try:
            ts = datetime.fromisoformat(r["timestamp"])
            if ts >= cutoff:
                valid_records.append(r)
        except Exception:
            continue

    return valid_records

def _save_raw_usage_records(records: List[Dict[str, Any]]) -> None:
    ensure_config_dir()
    lock_file = _get_lock_file()
    tmp_file = USAGE_WINDOW_FILE.parent / f".{USAGE_WINDOW_FILE.stem}_{os.getpid()}_{datetime.now().timestamp()}.tmp"
    try:
        with open(lock_file, "a+", encoding="utf-8") as lock_f:
            fcntl.flock(lock_f.fileno(), fcntl.LOCK_EX)
            try:
                with open(tmp_file, "w", encoding="utf-8") as f:
                    json.dump(records, f, ensure_ascii=False, indent=2)
                    f.flush()
                    os.fsync(f.fileno())
                os.replace(tmp_file, USAGE_WINDOW_FILE)
            finally:
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_UN)
    except Exception:
        if tmp_file.exists():
            try:
                tmp_file.unlink()
            except Exception:
                pass

def record_engine_usage(
    engine: str,
    tier: str = "standard",
    success: bool = True,
    task: str = ""
) -> None:
    """
    Atomically records an invocation event of an engine into the rolling usage window.
    Acquires an exclusive transactional flock across the entire Read-Modify-Write cycle.
    """
    engine_name = engine.lower().strip()
    record = {
        "timestamp": datetime.now().isoformat(),
        "engine": engine_name,
        "tier": tier,
        "success": success,
        "task": (task[:100] if task else "")
    }

    ensure_config_dir()
    lock_file = _get_lock_file()
    tmp_file = USAGE_WINDOW_FILE.parent / f".{USAGE_WINDOW_FILE.stem}_{os.getpid()}_{datetime.now().timestamp()}.tmp"
    try:
        with open(lock_file, "a+", encoding="utf-8") as lock_f:
            fcntl.flock(lock_f.fileno(), fcntl.LOCK_EX)
            try:
                # 1. Read existing records under exclusive lock
                records = []
                if USAGE_WINDOW_FILE.exists():
                    try:
                        with open(USAGE_WINDOW_FILE, "r", encoding="utf-8") as f:
                            data = json.load(f)
                            if isinstance(data, list):
                                cutoff = datetime.now() - timedelta(days=7.0)
                                for r in data:
                                    if isinstance(r, dict) and "timestamp" in r and "engine" in r:
                                        try:
                                            ts = datetime.fromisoformat(r["timestamp"])
                                            if ts >= cutoff:
                                                records.append(r)
                                        except Exception:
                                            continue
                    except Exception:
                        records = []

                # 2. Append new record
                records.append(record)

                # 3. Write via unique tmp file and atomic rename
                with open(tmp_file, "w", encoding="utf-8") as f:
                    json.dump(records, f, ensure_ascii=False, indent=2)
                    f.flush()
                    os.fsync(f.fileno())
                os.replace(tmp_file, USAGE_WINDOW_FILE)
            finally:
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_UN)
    except Exception:
        if tmp_file.exists():
            try:
                tmp_file.unlink()
            except Exception:
                pass

def get_engine_usage_stats(window_hours: float = 4.0) -> Dict[str, Any]:
    """
    Returns call counts and breakdown across all engines within window_hours.
    """
    records = _load_raw_usage_records(max_age_days=max(7.0, window_hours / 24.0))
    cutoff = datetime.now() - timedelta(hours=window_hours)

    stats = {
        "claude": {"total": 0, "success": 0, "failed": 0},
        "codex": {"total": 0, "success": 0, "failed": 0},
        "agy": {"total": 0, "success": 0, "failed": 0},
        "muse": {"total": 0, "success": 0, "failed": 0},
    }

    for r in records:
        try:
            ts = datetime.fromisoformat(r["timestamp"])
            if ts >= cutoff:
                eng = r["engine"]
                if eng not in stats:
                    stats[eng] = {"total": 0, "success": 0, "failed": 0}
                stats[eng]["total"] += 1
                if r.get("success", True):
                    stats[eng]["success"] += 1
                else:
                    stats[eng]["failed"] += 1
        except Exception:
            continue

    return stats

def get_burn_rate_penalty(engine: str) -> Tuple[float, Optional[str]]:
    """
    Computes dynamic burn-rate penalty score for routing:
    Returns (penalty, reason) where penalty <= 0.0.
    """
    eng = engine.lower().strip()
    records = _load_raw_usage_records(max_age_days=7.0)
    now = datetime.now()

    if eng == "codex":
        # 3-hour rolling window check
        cutoff_3h = now - timedelta(hours=3.0)
        c_3h = sum(1 for r in records if r["engine"] == "codex" and datetime.fromisoformat(r["timestamp"]) >= cutoff_3h)
        if c_3h >= CODEX_LIMIT_3H:
            return -1.8, f"Codex 过去 3 小时高频调用已达 {c_3h} 次，触发配额窗口削峰熔断保护 (-1.8)"
        elif c_3h >= CODEX_WARN_3H:
            return -0.8, f"Codex 过去 3 小时调用已达 {c_3h} 次，触发滚动窗口削峰保护 (-0.8)"
        return 0.0, None

    elif eng == "claude":
        # 24-hour and 7-day rolling window check
        cutoff_24h = now - timedelta(hours=24.0)
        cutoff_7d = now - timedelta(days=7.0)
        c_24h = sum(1 for r in records if r["engine"] == "claude" and datetime.fromisoformat(r["timestamp"]) >= cutoff_24h)
        c_7d = sum(1 for r in records if r["engine"] == "claude" and datetime.fromisoformat(r["timestamp"]) >= cutoff_7d)

        if c_7d >= CLAUDE_LIMIT_7D:
            return -2.5, f"Claude 过去 7 天总调用已达 {c_7d} 次，触发周预算强保护 (-2.5)"
        elif c_7d >= CLAUDE_WARN_7D:
            return -1.2, f"Claude 过去 7 天总调用已达 {c_7d} 次，触发周预算防御保护 (-1.2)"
        elif c_24h >= CLAUDE_LIMIT_24H:
            return -1.5, f"Claude 过去 24 小时调用已达 {c_24h} 次，触发日预算高压保护 (-1.5)"
        elif c_24h >= CLAUDE_WARN_24H:
            return -0.8, f"Claude 过去 24 小时调用已达 {c_24h} 次，触发日预算平滑保护 (-0.8)"
        return 0.0, None

    elif eng == "agy":
        # Google AI Pro has unlimited Pro capacity; 0 penalty
        return 0.0, None

    elif eng == "muse":
        return 0.0, None

    return 0.0, None
