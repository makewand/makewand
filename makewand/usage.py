"""
Makewand Usage Tracker and Sliding Window Burn Rate Estimator.
Maintains persistent rolling usage window for Claude, Codex, AGY, and Muse subscriptions.
"""

import os
import sys
import json
import fcntl
from datetime import datetime, timedelta
from typing import Dict, Any, List, Tuple, Optional
from pathlib import Path
import uuid
from makewand.config import CONFIG_DIR, ensure_config_dir

USAGE_WINDOW_FILE = CONFIG_DIR / "usage_window.json"

# Window thresholds for burn-rate protection:
# Codex: rolling 3h rate limit + 24h & 7d weekly budget (save weekly subscription tokens)
CODEX_WARN_3H = 20
CODEX_LIMIT_3H = 35
CODEX_WARN_24H = 50
CODEX_LIMIT_24H = 100
CODEX_WARN_7D = 180
CODEX_LIMIT_7D = 350

# Claude: rolling 24h/7d budget
CLAUDE_WARN_24H = 35
CLAUDE_LIMIT_24H = 60
CLAUDE_WARN_7D = 120
CLAUDE_LIMIT_7D = 200

# Grok: rolling 24h/7d budget (prevent day-one burnout of monthly/daily allowance)
GROK_WARN_24H = 20
GROK_LIMIT_24H = 40
GROK_WARN_7D = 80
GROK_LIMIT_7D = 160

# Muse: rolling 24h/7d budget
MUSE_WARN_24H = 30
MUSE_LIMIT_24H = 60
MUSE_WARN_7D = 100
MUSE_LIMIT_7D = 200

def _get_active_usage_file() -> Path:
    test_env_file = os.environ.get("MAKEWAND_USAGE_FILE")
    if test_env_file:
        return Path(test_env_file)
    if USAGE_WINDOW_FILE != CONFIG_DIR / "usage_window.json":
        return Path(USAGE_WINDOW_FILE)
    if "unittest" in sys.modules or os.environ.get("PYTEST_CURRENT_TEST") or os.environ.get("MAKEWAND_TEST_MODE") == "1":
        import tempfile
        return Path(tempfile.gettempdir()) / "makewand_test_usage.json"
    return USAGE_WINDOW_FILE

def _get_lock_file() -> Path:
    ensure_config_dir()
    uf = _get_active_usage_file()
    return uf.parent / f".{uf.stem}.lock"

def _load_raw_usage_records(max_age_days: float = 7.0) -> List[Dict[str, Any]]:
    ensure_config_dir()
    uf = _get_active_usage_file()
    if not uf.exists():
        return []

    lock_file = _get_lock_file()
    data = []
    try:
        with open(lock_file, "a+", encoding="utf-8") as lock_f:
            fcntl.flock(lock_f.fileno(), fcntl.LOCK_SH)
            try:
                if uf.exists():
                    with open(uf, "r", encoding="utf-8") as f:
                        data = json.load(f)
            finally:
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_UN)
    except Exception:
        try:
            if uf.exists():
                with open(uf, "r", encoding="utf-8") as f:
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
    uf = _get_active_usage_file()
    lock_file = _get_lock_file()
    tmp_file = uf.parent / f".{uf.stem}_{os.getpid()}_{datetime.now().timestamp()}_{uuid.uuid4().hex[:8]}.tmp"
    try:
        with open(lock_file, "a+", encoding="utf-8") as lock_f:
            fcntl.flock(lock_f.fileno(), fcntl.LOCK_EX)
            try:
                with open(tmp_file, "w", encoding="utf-8") as f:
                    json.dump(records, f, ensure_ascii=False, indent=2)
                    f.flush()
                    os.fsync(f.fileno())
                os.replace(tmp_file, uf)
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
    uf = _get_active_usage_file()
    lock_file = _get_lock_file()
    tmp_file = uf.parent / f".{uf.stem}_{os.getpid()}_{datetime.now().timestamp()}_{uuid.uuid4().hex[:8]}.tmp"
    try:
        with open(lock_file, "a+", encoding="utf-8") as lock_f:
            fcntl.flock(lock_f.fileno(), fcntl.LOCK_EX)
            try:
                # 1. Read existing records under exclusive lock
                records = []
                if uf.exists():
                    try:
                        with open(uf, "r", encoding="utf-8") as f:
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
                os.replace(tmp_file, uf)
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
        "grok": {"total": 0, "success": 0, "failed": 0},
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

TIER_WEIGHTS = {
    "fast": 0.5,
    "standard": 1.0,
    "deep": 2.0
}

def _calc_weighted_counts(records: List[Dict[str, Any]], engine: str) -> Tuple[float, float, float]:
    now = datetime.now()
    cutoff_3h = now - timedelta(hours=3.0)
    cutoff_24h = now - timedelta(hours=24.0)
    cutoff_7d = now - timedelta(days=7.0)

    c_3h = 0.0
    c_24h = 0.0
    c_7d = 0.0

    for r in records:
        if not isinstance(r, dict) or r.get("engine") != engine:
            continue
        ts_str = r.get("timestamp")
        if not ts_str:
            continue
        try:
            clean_ts = ts_str.replace("Z", "+00:00") if ts_str.endswith("Z") else ts_str
            ts = datetime.fromisoformat(clean_ts)
            tier = r.get("tier", "standard")
            weight = TIER_WEIGHTS.get(tier, 1.0)
            if ts >= cutoff_3h:
                c_3h += weight
            if ts >= cutoff_24h:
                c_24h += weight
            if ts >= cutoff_7d:
                c_7d += weight
        except Exception:
            continue

    return round(c_3h, 1), round(c_24h, 1), round(c_7d, 1)

def _calc_continuous_penalty(
    count: float,
    warn: float,
    limit: float,
    warn_pen: float,
    limit_pen: float,
    name: str,
    window_name: str
) -> Optional[Tuple[float, str]]:
    # 15% hysteresis band below warn threshold
    if count < warn * 0.85:
        return None

    if count < warn:
        ratio = (count - (warn * 0.85)) / max(1.0, (warn * 0.15))
        pen = round(warn_pen * 0.5 * ratio, 2)
        return pen, f"{name} 过去 {window_name} 调用已达 {count:.1f} 加权当量，触发日预算平滑保护 ({pen})"

    if count < limit:
        ratio = (count - warn) / max(1.0, (limit - warn))
        pen = round(warn_pen + (limit_pen - warn_pen) * (ratio ** 1.2), 2)
        return pen, f"{name} 过去 {window_name} 调用已达 {count:.1f} 加权当量，触发滚动窗口削峰保护 ({pen})"

    overflow = min(1.0, (count - limit) / max(1.0, limit * 0.5))
    pen = round(limit_pen - 1.0 * overflow, 2)
    return pen, f"{name} 过去 {window_name} 高频调用已达 {count:.1f} 加权当量，触发配额窗口削峰熔断保护 ({pen})"

def get_burn_rate_penalty(engine: str) -> Tuple[float, Optional[str]]:
    """
    Computes dynamic burn-rate penalty score for routing:
    Returns (penalty, reason) where penalty <= 0.0.
    Uses continuous mathematical flow curves and 15% hysteresis to eliminate abrupt step cliffs.
    """
    eng = engine.lower().strip()
    if eng == "agy":
        # Google AI Pro anchor - continuous high capacity
        return 0.0, None

    records = _load_raw_usage_records(max_age_days=7.0)
    c_3h, c_24h, c_7d = _calc_weighted_counts(records, eng)

    if eng == "codex":
        penalties = []
        p_7d = _calc_continuous_penalty(c_7d, CODEX_WARN_7D, CODEX_LIMIT_7D, -1.5, -3.0, "Codex", "7 天")
        if p_7d: penalties.append(p_7d)

        p_24h = _calc_continuous_penalty(c_24h, CODEX_WARN_24H, CODEX_LIMIT_24H, -1.0, -2.0, "Codex", "24 小时")
        if p_24h: penalties.append(p_24h)

        p_3h = _calc_continuous_penalty(c_3h, CODEX_WARN_3H, CODEX_LIMIT_3H, -0.8, -1.8, "Codex", "3 小时")
        if p_3h: penalties.append(p_3h)

        if penalties:
            penalties.sort(key=lambda x: x[0])
            return penalties[0]
        return 0.0, None

    elif eng == "claude":
        penalties = []
        p_7d = _calc_continuous_penalty(c_7d, CLAUDE_WARN_7D, CLAUDE_LIMIT_7D, -1.2, -2.5, "Claude", "7 天")
        if p_7d: penalties.append(p_7d)

        p_24h = _calc_continuous_penalty(c_24h, CLAUDE_WARN_24H, CLAUDE_LIMIT_24H, -0.8, -1.5, "Claude", "24 小时")
        if p_24h: penalties.append(p_24h)

        if penalties:
            penalties.sort(key=lambda x: x[0])
            return penalties[0]
        return 0.0, None

    elif eng == "grok":
        penalties = []
        p_7d = _calc_continuous_penalty(c_7d, GROK_WARN_7D, GROK_LIMIT_7D, -1.2, -2.5, "Grok", "7 天")
        if p_7d: penalties.append(p_7d)

        p_24h = _calc_continuous_penalty(c_24h, GROK_WARN_24H, GROK_LIMIT_24H, -1.0, -2.0, "Grok", "24 小时")
        if p_24h: penalties.append(p_24h)

        if penalties:
            penalties.sort(key=lambda x: x[0])
            return penalties[0]
        return 0.0, None

    elif eng == "muse":
        penalties = []
        p_7d = _calc_continuous_penalty(c_7d, MUSE_WARN_7D, MUSE_LIMIT_7D, -1.2, -2.5, "Muse", "7 天")
        if p_7d: penalties.append(p_7d)

        p_24h = _calc_continuous_penalty(c_24h, MUSE_WARN_24H, MUSE_LIMIT_24H, -0.8, -1.5, "Muse", "24 小时")
        if p_24h: penalties.append(p_24h)

        if penalties:
            penalties.sort(key=lambda x: x[0])
            return penalties[0]
        return 0.0, None

    return 0.0, None
