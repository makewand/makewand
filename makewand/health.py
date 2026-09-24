"""
Health probing, quota monitoring, and status cache management.
"""

import os
import re
import json
import fcntl
import uuid
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
from makewand.providers.grok import parse_grok_quota

DEFAULT_CACHE = {
    "agy": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""},
    "claude": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""},
    "codex": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""},
    "muse": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""},
    "grok": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""},
    "local": {"status": "unknown", "reason": "", "resets_at": None, "updated_at": ""}
}

STATUS_LOCK_FILE = STATUS_CACHE_FILE.parent / ".status.lock"

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
                if (now - up_dt).total_seconds() > 2 * 3600:
                    return True
            except Exception:
                pass
        return False

    # Relative time support (e.g. "in 3 hours", "in 15 minutes", "in 24 hours")
    rel_match = re.search(r"in\s+(\d+)\s*(hour|minute|min|hr|h|m)", resets_at, re.IGNORECASE)
    if rel_match and updated_at:
        try:
            val = int(rel_match.group(1))
            unit = rel_match.group(2).lower()
            delta = timedelta(hours=val) if unit.startswith("h") else timedelta(minutes=val)
            clean_up = updated_at.replace("Z", "+00:00") if updated_at.endswith("Z") else updated_at
            up_dt = datetime.fromisoformat(clean_up)
            now = datetime.now(up_dt.tzinfo) if up_dt.tzinfo is not None else datetime.now()
            return (now - up_dt) >= delta
        except Exception:
            pass

    # Month-day date support (e.g. "Oct 3 at 2pm", "October 3, 2026, 2:00 PM")
    date_match = re.search(r"([A-Za-z]{3,9})\s+(\d{1,2})(?:st|nd|rd|th)?(?:\s*,\s*(\d{4}))?(?:\s+at\s+|\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?", resets_at, re.IGNORECASE)
    if date_match:
        try:
            month_str = date_match.group(1).capitalize()[:3]
            day_val = int(date_match.group(2))
            year_val = int(date_match.group(3)) if date_match.group(3) else datetime.now().year
            hour_val = int(date_match.group(4))
            min_val = int(date_match.group(5)) if date_match.group(5) else 0
            ampm = date_match.group(6).lower() if date_match.group(6) else ""
            if ampm == "pm" and hour_val < 12:
                hour_val += 12
            elif ampm == "am" and hour_val == 12:
                hour_val = 0
            months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"]
            if month_str in months:
                month_val = months.index(month_str) + 1
                reset_dt = datetime(year_val, month_val, day_val, hour_val, min_val)
                now = datetime.now()
                return now >= reset_dt
        except Exception:
            pass

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

    # Fallback TTL for any unparseable non-empty resets_at string (prevent sticky freeze)
    # Strictly do NOT trigger for relative times that haven't arrived yet
    if updated_at and not rel_match and not date_match:
        try:
            clean_up = updated_at.replace("Z", "+00:00") if updated_at.endswith("Z") else updated_at
            up_dt = datetime.fromisoformat(clean_up)
            now = datetime.now(up_dt.tzinfo) if up_dt.tzinfo is not None else datetime.now()
            if (now - up_dt).total_seconds() > 2 * 3600:
                return True
        except Exception:
            pass

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
    lock_file = STATUS_CACHE_FILE.parent / ".status.lock"
    try:
        with open(lock_file, "a+") as lock_f:
            try:
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_SH)
                if STATUS_CACHE_FILE.exists():
                    with open(STATUS_CACHE_FILE, "r", encoding="utf-8") as f:
                        data = json.load(f)
                        cache.update(data)
                        loaded = True
            finally:
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_UN)
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
                            if inc_time and disk_time and disk_time > inc_time:
                                continue
                        disk_data[k] = v
                    else:
                        disk_data[k] = v

                merged = _sanitize_cache(disk_data)

                tmp_file = STATUS_CACHE_FILE.parent / f".status_{os.getpid()}_{datetime.now().timestamp()}_{uuid.uuid4().hex[:8]}.tmp"
                with open(tmp_file, "w", encoding="utf-8") as f:
                    json.dump(merged, f, indent=2, ensure_ascii=False)
                    f.flush()
                    os.fsync(f.fileno())
                os.replace(tmp_file, STATUS_CACHE_FILE)

                # Also update legacy trio cache file for backward compatibility
                if LEGACY_TRIO_CACHE.parent.exists():
                    tmp_legacy = LEGACY_TRIO_CACHE.parent / f".trio_{os.getpid()}_{datetime.now().timestamp()}_{uuid.uuid4().hex[:8]}.tmp"
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
    from makewand.config import has_api_configured, has_subscription_configured, is_provider_enabled

    if not is_provider_enabled(model_name):
        return {
            "status": "disabled",
            "reason": f"用户已在配置中手动禁用此引擎 (运行 'makewand enable {model_name}' 重新开启)",
            "resets_at": None,
            "updated_at": now,
            "mode": "disabled"
        }

    # 1. Local self-hosted models (Ollama, vLLM, LocalAI)
    if model_name in ("local", "ollama"):
        from makewand.providers.local import is_local_model_available, get_default_local_model, list_local_models
        if is_local_model_available():
            active_m = get_default_local_model()
            all_m = list_local_models() or []
            m_desc = f"{active_m}" + (f" (共 {len(all_m)} 个本地模型)" if len(all_m) > 1 else "")
            return {
                "status": "healthy",
                "reason": f"本地私有模型就绪 (当前: {m_desc}, 0 Token 成本)",
                "resets_at": None,
                "updated_at": now,
                "mode": "local"
            }
        return {
            "status": "missing",
            "reason": "本地 Ollama / vLLM (http://localhost:11434) 未响应或未检测到可用模型",
            "resets_at": None,
            "updated_at": now,
            "mode": "local"
        }

    api_configured = has_api_configured(model_name)
    sub_configured = has_subscription_configured(model_name)

    # 2. Subscription CLI not installed
    if not sub_configured:
        if api_configured:
            return {
                "status": "healthy",
                "reason": f"未安装订阅 CLI，已启用纯 API Key 模式",
                "resets_at": None,
                "updated_at": now,
                "mode": "api"
            }
        return {
            "status": "missing",
            "reason": f"CLI 命令 '{model_name}' 未在系统 PATH 中找到，且未配置备用 API Key",
            "resets_at": None,
            "updated_at": now,
            "mode": "none"
        }

    # 3. Subscription Probes
    mode = "hybrid" if api_configured else "subscription"

    if model_name == "claude":
        cmd = 'claude -p "echo ok"'
        code, out, err, ex = run_subprocess(cmd, timeout=15)
        combined = f"{out}\n{err}"
        is_limited, reason, resets = parse_claude_quota(combined)
        if is_limited:
            reason_str = f"{reason} (已就绪备用 API 兜底接力)" if api_configured else reason
            return {"status": "limited", "reason": reason_str, "resets_at": resets, "updated_at": now, "mode": mode, "api_fallback": api_configured}
        if code == 0:
            sub_desc = "Claude Code 订阅运行正常" + (" (已配置 API 备用兜底)" if api_configured else "")
            return {"status": "healthy", "reason": sub_desc, "resets_at": None, "updated_at": now, "mode": mode}
        if ex:
            return {"status": "error", "reason": ex, "resets_at": None, "updated_at": now, "mode": mode}
        return {"status": "healthy" if "ok" in out.lower() else "warning", "reason": combined.strip()[:120], "resets_at": None, "updated_at": now, "mode": mode}

    elif model_name == "codex":
        cmd = 'codex exec --sandbox read-only --skip-git-repo-check "echo ok"'
        code, out, err, ex = run_subprocess(cmd, timeout=35)
        combined = f"{out}\n{err}"
        is_limited, reason, resets = parse_codex_quota(combined)
        if is_limited:
            reason_str = f"{reason} (已就绪备用 API 兜底接力)" if api_configured else reason
            return {"status": "limited", "reason": reason_str, "resets_at": resets, "updated_at": now, "mode": mode, "api_fallback": api_configured}
        if code == 0:
            sub_desc = "Codex 订阅运行正常" + (" (已配置 API 备用兜底)" if api_configured else "")
            return {"status": "healthy", "reason": sub_desc, "resets_at": None, "updated_at": now, "mode": mode}
        if ex:
            return {"status": "error", "reason": ex, "resets_at": None, "updated_at": now, "mode": mode}
        return {"status": "warning", "reason": combined.strip()[:120], "resets_at": None, "updated_at": now, "mode": mode}

    elif model_name == "agy":
        code, out, err, ex = run_subprocess("agy --version", timeout=5)
        if code == 0:
            version = out.strip() or "v1.x"
            sub_desc = f"Antigravity (Google AI Pro, {version}) 运行就绪" + (" (已配置 API 备用兜底)" if api_configured else "")
            return {"status": "healthy", "reason": sub_desc, "resets_at": None, "updated_at": now, "mode": mode}
        return {"status": "warning", "reason": err.strip()[:100] or "agy 版本检测异常", "resets_at": None, "updated_at": now, "mode": mode}

    elif model_name == "muse":
        cmd = 'muse exec --disable-write --trust-workspace "echo ok"'
        code, out, err, ex = run_subprocess(cmd, timeout=25)
        combined = f"{out}\n{err}"
        is_limited, reason, resets = parse_muse_quota(combined)
        if is_limited:
            reason_str = f"{reason} (已就绪备用 API 兜底接力)" if api_configured else reason
            return {"status": "needs_auth" if "登录" in reason else "limited", "reason": reason_str, "resets_at": resets, "updated_at": now, "mode": mode, "api_fallback": api_configured}
        if code == 0:
            sub_desc = "Muse Code (Meta 订阅) 运行正常" + (" (已配置 API 备用兜底)" if api_configured else "")
            return {"status": "healthy", "reason": sub_desc, "resets_at": None, "updated_at": now, "mode": mode}
        if ex and "timed out" in ex.lower():
            return {"status": "needs_auth", "reason": "等待 OAuth 浏览器授权登录 (请运行 'muse login')", "resets_at": None, "updated_at": now, "mode": mode}
        return {"status": "warning", "reason": combined.strip()[:120] or ex or "Muse 状态未知", "resets_at": None, "updated_at": now, "mode": mode}

    elif model_name == "grok":
        cmd = 'grok -p "echo ok" --output-format plain'
        code, out, err, ex = run_subprocess(cmd, timeout=20)
        combined = f"{out}\n{err}"
        is_limited, reason, resets = parse_grok_quota(combined)
        if is_limited:
            reason_str = f"{reason} (已就绪备用 API 兜底接力)" if api_configured else reason
            return {"status": "needs_auth" if "登录" in reason else "limited", "reason": reason_str, "resets_at": resets, "updated_at": now, "mode": mode, "api_fallback": api_configured}
        if code == 0:
            sub_desc = "Grok Build (xAI 订阅) 运行正常" + (" (已配置 API 备用兜底)" if api_configured else "")
            return {"status": "healthy", "reason": sub_desc, "resets_at": None, "updated_at": now, "mode": mode}
        if ex:
            return {"status": "error", "reason": ex, "resets_at": None, "updated_at": now, "mode": mode}
    elif model_name == "aider":
        code, out, err, ex = run_subprocess("aider --version", timeout=5)
        if code == 0:
            version = out.strip().replace("aider ", "v")
            sub_desc = f"Aider AI Pair Programmer ({version}) 运行就绪" + (" (已配置 API 备用兜底)" if api_configured else "")
            return {"status": "healthy", "reason": sub_desc, "resets_at": None, "updated_at": now, "mode": mode}
        return {"status": "warning", "reason": err.strip()[:100] or "aider 版本检测异常", "resets_at": None, "updated_at": now, "mode": mode}

    elif model_name in ("deepseek", "qwen", "glm", "kimi", "openrouter", "siliconflow"):
        if api_configured:
            return {
                "status": "healthy",
                "reason": f"{model_name.upper()} 云端 API 就绪",
                "resets_at": None,
                "updated_at": now,
                "mode": "api"
            }
        return {
            "status": "missing",
            "reason": f"未配置 {model_name.upper()} API Key (设置环境变量即可激活)",
            "resets_at": None,
            "updated_at": now,
            "mode": "none"
        }

    return {"status": "unknown", "reason": "Unknown model", "resets_at": None, "updated_at": now, "mode": "none"}

def get_or_update_status(force_probe: bool = False) -> Dict[str, Any]:
    from makewand.config import get_all_supported_providers
    cache = load_status_cache()
    if force_probe:
        for model in get_all_supported_providers():
            cache[model] = probe_model(model)
        save_status_cache(cache)
    return cache

