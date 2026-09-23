"""
Health probing, quota monitoring, and status cache management.
"""

import os
import re
import json
import fcntl
from datetime import datetime, timedelta
from typing import Dict, Any, Optional
from pathlib import Path
from makewand.config import (
    STATUS_CACHE_FILE,
    LEGACY_TRIO_CACHE,
    ensure_config_dir
)
from makewand.providers.base import check_cli_installed, run_subprocess
from makewand.providers.agy import parse_agy_quota
from makewand.providers.claude import parse_claude_quota
from makewand.providers.codex import parse_codex_quota
from makewand.providers.muse import parse_muse_quota

DEFAULT_CACHE = {
    "agy": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""},
    "claude": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""},
    "codex": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""},
    "muse": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""}
}

def is_reset_time_passed(resets_at: Optional[str], updated_at: str = "") -> bool:
    if not resets_at or resets_at == "待重置":
        if updated_at:
            try:
                clean_up = updated_at.replace("Z", "+00:00") if updated_at.endswith("Z") else updated_at
                up_dt = datetime.fromisoformat(clean_up)
                now = datetime.now(up_dt.tzinfo) if up_dt.tzinfo is not None else datetime.now()
                if up_dt.tzinfo is not None and getattr(now, "tzinfo", None) is None:
                    try:
                        now = now.astimezone()
                    except Exception:
                        now = now.replace(tzinfo=up_dt.tzinfo)
                if (now - up_dt).total_seconds() > 4 * 3600:
                    return True
            except Exception:
                pass
        return False

    clean_resets = resets_at.replace("Z", "+00:00") if resets_at.endswith("Z") else resets_at
    try:
        dt = datetime.fromisoformat(clean_resets)
        now = datetime.now(dt.tzinfo) if dt.tzinfo is not None else datetime.now()
        if dt.tzinfo is not None and getattr(now, "tzinfo", None) is None:
            try:
                now = now.astimezone()
            except Exception:
                now = now.replace(tzinfo=dt.tzinfo)
        return now >= dt
    except Exception:
        pass

    clean = re.sub(r"\(.*?\)", "", resets_at).strip()
    for fmt in ("%I:%M %p", "%I %p", "%H:%M", "%I:%M%p", "%I%p"):
        try:
            t = datetime.strptime(clean, fmt).time()
            now = datetime.now()
            if updated_at:
                try:
                    clean_up = updated_at.replace("Z", "+00:00") if updated_at.endswith("Z") else updated_at
                    up_dt = datetime.fromisoformat(clean_up)
                    reset_dt = datetime.combine(up_dt.date(), t)
                    if up_dt.tzinfo is not None:
                        now = datetime.now(up_dt.tzinfo)
                        if getattr(now, "tzinfo", None) is None:
                            try:
                                now = now.astimezone()
                            except Exception:
                                now = now.replace(tzinfo=up_dt.tzinfo)
                        reset_dt = reset_dt.replace(tzinfo=up_dt.tzinfo)
                    if reset_dt < up_dt:
                        reset_dt += timedelta(days=1)
                    return now >= reset_dt
                except Exception:
                    pass
            reset_dt = datetime.combine(now.date(), t)
            return now >= reset_dt
        except Exception:
            continue
    return False

def _sanitize_cache(cache: Dict[str, Any]) -> Dict[str, Any]:
    for model_name, info in cache.items():
        if isinstance(info, dict) and info.get("status") == "limited":
            if is_reset_time_passed(info.get("resets_at"), info.get("updated_at", "")):
                info["status"] = "healthy"
                info["reason"] = f"已过配额重置窗口 ({info.get('resets_at')})，已自动恢复待命"
                info["resets_at"] = None
                info["updated_at"] = datetime.now().isoformat()
    return cache

def load_status_cache() -> Dict[str, Any]:
    ensure_config_dir()
    cache = dict(DEFAULT_CACHE)
    loaded = False
    if STATUS_CACHE_FILE.exists():
        try:
            with open(STATUS_CACHE_FILE, "r", encoding="utf-8") as f:
                try:
                    fcntl.flock(f.fileno(), fcntl.LOCK_SH)
                    data = json.load(f)
                    cache.update(data)
                    loaded = True
                finally:
                    fcntl.flock(f.fileno(), fcntl.LOCK_UN)
        except Exception:
            pass

    # Fallback to legacy trio status if available
    if not loaded and LEGACY_TRIO_CACHE.exists():
        try:
            with open(LEGACY_TRIO_CACHE, "r", encoding="utf-8") as f:
                try:
                    fcntl.flock(f.fileno(), fcntl.LOCK_SH)
                    data = json.load(f)
                    cache.update(data)
                finally:
                    fcntl.flock(f.fileno(), fcntl.LOCK_UN)
        except Exception:
            pass

    return _sanitize_cache(cache)

def save_status_cache(cache: Dict[str, Any]):
    ensure_config_dir()
    lock_file = STATUS_CACHE_FILE.parent / ".status.lock"
    try:
        with open(lock_file, "w") as lock_f:
            fcntl.flock(lock_f.fileno(), fcntl.LOCK_EX)
            try:
                # Merge with current on-disk data so concurrent probes don't clobber each other
                disk_data = dict(DEFAULT_CACHE)
                if STATUS_CACHE_FILE.exists():
                    try:
                        with open(STATUS_CACHE_FILE, "r", encoding="utf-8") as f:
                            loaded = json.load(f)
                            if isinstance(loaded, dict):
                                disk_data.update(loaded)
                    except Exception:
                        pass

                for k, v in cache.items():
                    if k not in disk_data:
                        disk_data[k] = v
                    elif isinstance(v, dict):
                        disk_item = disk_data.get(k, {})
                        if isinstance(disk_item, dict):
                            # If incoming status is 'unknown' while disk has active status ('limited' or 'healthy'), preserve disk
                            if v.get("status") == "unknown" and disk_item.get("status") in ("limited", "healthy"):
                                continue
                            # If incoming has older timestamp, do not overwrite newer disk status
                            inc_time = v.get("updated_at", "")
                            disk_time = disk_item.get("updated_at", "")
                            if inc_time and disk_time and disk_time > inc_time and disk_item.get("status") == "limited":
                                continue
                        disk_data[k] = v
                    else:
                        disk_data[k] = v

                merged = _sanitize_cache(disk_data)

                tmp_file = STATUS_CACHE_FILE.parent / f".status_{os.getpid()}_{datetime.now().timestamp()}.tmp"
                with open(tmp_file, "w", encoding="utf-8") as f:
                    json.dump(merged, f, indent=2, ensure_ascii=False)
                    f.flush()
                    os.fsync(f.fileno())
                os.replace(tmp_file, STATUS_CACHE_FILE)

                # Also update legacy trio cache file for backward compatibility
                if LEGACY_TRIO_CACHE.parent.exists():
                    tmp_legacy = LEGACY_TRIO_CACHE.parent / f".trio_{os.getpid()}_{datetime.now().timestamp()}.tmp"
                    with open(tmp_legacy, "w", encoding="utf-8") as f:
                        json.dump(merged, f, indent=2, ensure_ascii=False)
                        f.flush()
                        os.fsync(f.fileno())
                    os.replace(tmp_legacy, LEGACY_TRIO_CACHE)
            finally:
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_UN)
    except Exception:
        pass

def record_engine_limit(engine: str, reason: str, resets_at: Optional[str] = None) -> None:
    """
    Directly writes a live rate-limit/429 status event into the shared status cache.
    Allows immediate cross-session visibility without waiting for periodic polling probes.
    """
    model_name = engine.lower().strip()
    now = datetime.now().isoformat()
    status_entry = {
        model_name: {
            "status": "limited",
            "reason": reason,
            "resets_at": resets_at,
            "updated_at": now
        }
    }
    save_status_cache(status_entry)

def probe_model(model_name: str) -> Dict[str, Any]:
    now = datetime.now().isoformat()
    if not check_cli_installed(model_name):
        return {
            "status": "missing",
            "reason": f"CLI 命令 '{model_name}' 未在系统 PATH 中找到",
            "resets_at": None,
            "updated_at": now
        }

    if model_name == "claude":
        cmd = 'claude -p "echo ok"'
        code, out, err, ex = run_subprocess(cmd, timeout=15)
        combined = f"{out}\n{err}"
        is_limited, reason, resets = parse_claude_quota(combined)
        if is_limited:
            return {"status": "limited", "reason": reason, "resets_at": resets, "updated_at": now}
        if code == 0:
            return {"status": "healthy", "reason": "Claude Code 订阅运行正常", "resets_at": None, "updated_at": now}
        if ex:
            return {"status": "error", "reason": ex, "resets_at": None, "updated_at": now}
        return {"status": "healthy" if "ok" in out.lower() else "warning", "reason": combined.strip()[:120], "resets_at": None, "updated_at": now}

    elif model_name == "codex":
        cmd = 'codex exec --skip-git-repo-check "echo ok"'
        code, out, err, ex = run_subprocess(cmd, timeout=35)
        combined = f"{out}\n{err}"
        is_limited, reason, resets = parse_codex_quota(combined)
        if is_limited:
            return {"status": "limited", "reason": reason, "resets_at": resets, "updated_at": now}
        if code == 0:
            return {"status": "healthy", "reason": "Codex 订阅运行正常", "resets_at": None, "updated_at": now}
        if ex:
            return {"status": "error", "reason": ex, "resets_at": None, "updated_at": now}
        return {"status": "warning", "reason": combined.strip()[:120], "resets_at": None, "updated_at": now}

    elif model_name == "agy":
        code, out, err, ex = run_subprocess("agy --version", timeout=5)
        if code == 0:
            version = out.strip() or "v1.x"
            return {"status": "healthy", "reason": f"Antigravity (Google AI Pro, {version}) 运行就绪", "resets_at": None, "updated_at": now}
        return {"status": "warning", "reason": err.strip()[:100] or "agy 版本检测异常", "resets_at": None, "updated_at": now}

    elif model_name == "muse":
        cmd = 'muse exec --yolo "echo ok"'
        code, out, err, ex = run_subprocess(cmd, timeout=8)
        combined = f"{out}\n{err}"
        is_limited, reason, resets = parse_muse_quota(combined)
        if is_limited:
            return {"status": "needs_auth" if "登录" in reason else "limited", "reason": reason, "resets_at": resets, "updated_at": now}
        if code == 0:
            return {"status": "healthy", "reason": "Muse Code (Meta 订阅) 运行正常", "resets_at": None, "updated_at": now}
        if ex and "timed out" in ex.lower():
            # Muse often times out when waiting for interactive browser enter
            return {"status": "needs_auth", "reason": "等待 OAuth 浏览器授权登录 (请运行 'muse login')", "resets_at": None, "updated_at": now}
        return {"status": "warning", "reason": combined.strip()[:120] or ex or "Muse 状态未知", "resets_at": None, "updated_at": now}

    return {"status": "unknown", "reason": "Unknown model", "resets_at": None, "updated_at": now}

def get_or_update_status(force_probe: bool = False) -> Dict[str, Any]:
    cache = load_status_cache()
    if force_probe:
        for model in ["agy", "claude", "codex", "muse"]:
            cache[model] = probe_model(model)
        save_status_cache(cache)
    return cache
