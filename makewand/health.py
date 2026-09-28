"""
Health probing, quota monitoring, and status cache management.
"""

import os
import re
import json
from makewand import filelock as fcntl
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

# A cached "healthy"/"warning" verdict older than this is no longer evidence of
# anything: it is treated as neutral and the user is asked to re-probe.
STATUS_STALE_SECONDS = 6 * 3600
# Failure verdicts self-heal back to neutral ("unknown") after a TTL so that one
# probe timeout or one failed login check can never exclude a provider forever.
STATUS_TTL_SECONDS = {
    "error": 30 * 60,
    "needs_auth": 30 * 60,
}
# Geographic/account-level blocks rarely change within minutes; re-check later.
REGION_BLOCK_TTL_SECONDS = 6 * 3600

REAUTH_HINTS = {
    "claude": "运行 'claude' 并在会话内执行 /login",
    "codex": "运行 'codex login'",
    "muse": "运行 'muse login'",
    "grok": "运行 'grok' 按提示登录，或设置 XAI_API_KEY",
    "agy": "运行 'agy' 按提示登录 Google 账号",
}


def get_reauth_hint(provider: str) -> str:
    """Actionable re-login instruction for a provider in needs_auth state."""
    name = (provider or "").lower().strip()
    step = REAUTH_HINTS.get(name, f"重新登录 {name} CLI 或配置其 API Key")
    return f"{name} 需要重新登录：{step}，完成后运行 'makewand probe' 刷新状态"


def _parse_timestamp(value: Any) -> Optional[datetime]:
    if not value or not isinstance(value, str):
        return None
    try:
        clean = value.replace("Z", "+00:00") if value.endswith("Z") else value
        return datetime.fromisoformat(clean)
    except (TypeError, ValueError):
        return None


def status_age_seconds(info: Dict[str, Any]) -> Optional[float]:
    """Seconds since a cache entry was written, or None when unknown."""
    if not isinstance(info, dict):
        return None
    stamp = _parse_timestamp(info.get("updated_at"))
    if stamp is None:
        return None
    now = datetime.now(stamp.tzinfo) if stamp.tzinfo is not None else datetime.now()
    return (now - stamp).total_seconds()


def _entry_ttl_seconds(info: Dict[str, Any]) -> Optional[float]:
    explicit = info.get("ttl_seconds")
    if isinstance(explicit, (int, float)) and explicit > 0:
        return float(explicit)
    return STATUS_TTL_SECONDS.get(info.get("status"))

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
    rel_match = re.search(r"in\s+(\d+(?:\.\d+)?)\s*(hour|minute|min|hr|h|m)", resets_at, re.IGNORECASE)
    if rel_match and updated_at:
        try:
            val = float(rel_match.group(1))
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
    clean = re.sub(r"^(?:at|in)\s+", "", clean, flags=re.IGNORECASE).strip()
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
    now_dt = datetime.now()
    for model_name, info in cache.items():
        if isinstance(info, dict) and info.get("status") == "limited":
            resets_at = info.get("resets_at")
            updated_at = info.get("updated_at", "")
            # Normalize once to an absolute deadline. Rewriting the duration
            # without its reference time causes every read/save to subtract
            # the same elapsed interval again.
            if resets_at and updated_at and re.match(r"^in\s+", str(resets_at), re.IGNORECASE):
                relative = re.fullmatch(
                    r"in\s+((?:\d+(?:\.\d+)?\s*(?:hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)\s*)+)",
                    str(resets_at).strip(), re.IGNORECASE)
                if relative:
                    try:
                        parts = re.findall(r"(\d+(?:\.\d+)?)\s*([a-z]+)", relative.group(1), re.IGNORECASE)
                        seconds = sum(float(n) * (3600 if u.lower().startswith("h") else 60 if u.lower().startswith("m") else 1) for n, u in parts)
                        observed = datetime.fromisoformat(updated_at.replace("Z", "+00:00"))
                        resets_at = (observed + timedelta(seconds=seconds)).isoformat()
                        info["resets_at"] = resets_at
                    except (TypeError, ValueError, OverflowError):
                        pass
            if is_reset_time_passed(resets_at, updated_at):
                info["status"] = "healthy"
                info["reason"] = f"已过配额重置窗口 ({resets_at})，已自动恢复待命"
                info["resets_at"] = None
                info["updated_at"] = now_dt.isoformat()
        elif isinstance(info, dict) and info.get("status") in STATUS_TTL_SECONDS:
            ttl = _entry_ttl_seconds(info)
            age = status_age_seconds(info)
            if ttl is not None and (age is None or age >= ttl):
                previous = info.get("status")
                old_reason = str(info.get("reason") or "").strip()
                info["status"] = "unknown"
                info["expired_from"] = previous
                info["reason"] = (
                    f"上次状态 {previous}（{old_reason[:80] or '无详情'}）已超过 {int(ttl // 60)} 分钟有效期，"
                    "按中性处理；运行 'makewand probe' 重新探测"
                )
                info["resets_at"] = None
                info.pop("ttl_seconds", None)
                info["updated_at"] = now_dt.isoformat()
        if isinstance(info, dict):
            age = status_age_seconds(info)
            info["stale"] = bool(
                info.get("status") in ("healthy", "warning")
                and age is not None
                and age >= STATUS_STALE_SECONDS
            )
    return cache


def is_status_stale(info: Optional[Dict[str, Any]]) -> bool:
    """True when a healthy/warning verdict is too old to be trusted."""
    if not isinstance(info, dict) or info.get("status") not in ("healthy", "warning"):
        return False
    age = status_age_seconds(info)
    return age is not None and age >= STATUS_STALE_SECONDS

def load_status_cache() -> Dict[str, Any]:
    ensure_config_dir()
    cache = {k: dict(v) for k, v in DEFAULT_CACHE.items()}
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
                disk_data = {k: dict(v) for k, v in DEFAULT_CACHE.items()}
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

def record_engine_failure(
    engine: str,
    status: str,
    reason: str,
    ttl_seconds: Optional[float] = None,
    source: str = "dispatch",
) -> None:
    """
    Writes a non-quota failure observed during a real dispatch (region block,
    authentication failure, broken CLI) into the shared status cache with a TTL.
    After the TTL the entry decays to neutral "unknown" (see _sanitize_cache).
    """
    if status not in STATUS_TTL_SECONDS:
        raise ValueError(f"unsupported failure status: {status}")
    model_name = engine.lower().strip()
    entry: Dict[str, Any] = {
        "status": status,
        "reason": reason,
        "resets_at": None,
        "updated_at": datetime.now().isoformat(),
        "source": source,
    }
    if ttl_seconds:
        entry["ttl_seconds"] = float(ttl_seconds)
    save_status_cache({model_name: entry})


def record_engine_limit(engine: str, reason: str, resets_at: Optional[str] = None) -> None:
    """
    Directly writes a live rate-limit/429 status event into the shared status cache.
    Allows immediate cross-session visibility without waiting for periodic polling probes.
    Login/credential failures reported through this legacy entry point are stored as
    needs_auth with a TTL instead of a fake quota limit.
    """
    model_name = engine.lower().strip()
    if resets_at == "需登录授权" or "登录" in str(reason or ""):
        record_engine_failure(model_name, "needs_auth", f"{reason} · {get_reauth_hint(model_name)}")
        return
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
        ok, reason, models = is_local_model_available()
        if ok:
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
            "reason": reason or "本地 Ollama / vLLM (http://localhost:11434) 未响应或未检测到可用模型",
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
        # `agy --version` only proves the binary is installed. Account, region and
        # quota availability are learned from real dispatches (providers/agy.py
        # writes region/auth failures back with a TTL) and the usage ledger.
        code, out, err, ex = run_subprocess("agy --version", timeout=5)
        if code == 0:
            version = out.strip() or "v1.x"
            sub_desc = (f"Antigravity 已安装 ({version})；仅版本检测，账号/地区可用性未经真实调用验证"
                        + (" (已配置 API 备用兜底)" if api_configured else ""))
            return {"status": "healthy", "reason": sub_desc, "resets_at": None, "updated_at": now,
                    "mode": mode, "verified": False}
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

PROBE_REUSE_SECONDS = 120


def _is_live_dispatch_failure(info: Any) -> bool:
    return (isinstance(info, dict) and info.get("source") == "dispatch"
            and info.get("status") in STATUS_TTL_SECONDS)


def _merge_probe_result(model: str, previous: Any, probed: Dict[str, Any]) -> Dict[str, Any]:
    """A version-only probe must not erase a still-valid failure seen by a real dispatch."""
    if probed.get("verified") is False and probed.get("status") == "healthy" and _is_live_dispatch_failure(previous):
        kept = dict(previous)
        note = " (版本探测仅证明已安装，保留真实派发失败记录直到过期)"
        reason = str(previous.get("reason", ""))
        kept["reason"] = reason if note in reason else f"{reason}{note}"
        return kept
    return probed


def _probed_since(info: Any, started: datetime) -> bool:
    if not isinstance(info, dict):
        return False
    stamp = _parse_timestamp(info.get("updated_at"))
    if stamp is None:
        return False
    if stamp.tzinfo is not None:
        stamp = stamp.astimezone().replace(tzinfo=None)
    return stamp >= started - timedelta(seconds=PROBE_REUSE_SECONDS)


def get_or_update_status(force_probe: bool = False) -> Dict[str, Any]:
    from makewand.config import get_all_supported_providers
    cache = load_status_cache()
    if not force_probe:
        return cache
    # Probes of claude/codex/muse/grok are real model calls. Serialize them across
    # processes so two concurrent `makewand probe` runs do not double the spend: a
    # waiter reuses the fresh result of the probe that held the lock.
    ensure_config_dir()
    probe_lock = STATUS_CACHE_FILE.parent / ".probe.lock"
    started = datetime.now()
    try:
        lock_f = open(probe_lock, "a+")
    except OSError:
        lock_f = None
    try:
        if lock_f is not None:
            try:
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            except OSError:
                print("⏳ 另一个 makewand 探测正在进行，等待其结果以避免重复消耗额度...", flush=True)
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_EX)
                cache = load_status_cache()
                if all(_probed_since(cache.get(m), started) for m in get_all_supported_providers()):
                    return cache
        cache = load_status_cache()
        for model in get_all_supported_providers():
            cache[model] = _merge_probe_result(model, cache.get(model), probe_model(model))
        save_status_cache(cache)
        return cache
    finally:
        if lock_f is not None:
            try:
                fcntl.flock(lock_f.fileno(), fcntl.LOCK_UN)
            except OSError:
                pass
            lock_f.close()

def format_quota_bar(percentage: int, width: int = 20, colorize: bool = True) -> str:
    """
    Renders a colored progress bar matching modern CLI tools:
    [████████████████████] 100% (Green)
    [████████████░░░░░░░░]  60% (Yellow/Cyan)
    [████░░░░░░░░░░░░░░░░]  20% (Yellow/Red)
    [░░░░░░░░░░░░░░░░░░░░]   0% (Red)
    """
    pct = max(0, min(100, int(percentage)))
    filled = int(round(width * pct / 100.0))
    empty = width - filled

    if not colorize:
        bar = "█" * filled + "░" * empty
        return f"[{bar}] {pct:>3}%"

    from makewand.config import COLOR_GREEN, COLOR_YELLOW, COLOR_RED, COLOR_RESET, COLOR_BOLD
    if pct >= 50:
        bar_color = COLOR_GREEN
    elif pct >= 20:
        bar_color = COLOR_YELLOW
    else:
        bar_color = COLOR_RED

    bar = f"{bar_color}{'█' * filled}{COLOR_RESET}{'░' * empty}"
    pct_str = f"{bar_color}{COLOR_BOLD}{pct:>3}%{COLOR_RESET}"
    return f"[{bar}] {pct_str}"

QUOTA_SOURCE_LABELS = {
    "official": "官方报告",
    "local_estimate": "本地调用计数估算，非真实配额",
    "assumed": "无额度信号，未做估算",
    "status": "由健康状态推定",
    "unknown": "未探测",
}


def calculate_provider_quota(provider: str, info: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
    """
    Returns a quota *indicator* (0-100) for display and pacing.

    Only percentages parsed from provider output ("NN% left") are official
    ("source": "official"). Everything else is either inferred from the health
    status or estimated from makewand's own local call counts
    ("source": "local_estimate"); callers must not present those as real quota.
    "unknown" (never probed) is neutral: it is not an exhausted quota.
    """
    if info is None:
        info = load_status_cache().get(provider, {})

    status = info.get("status", "unknown")
    reason = info.get("reason", "")
    resets_at = info.get("resets_at")
    updated_at = info.get("updated_at", "")

    # 1. Limited / Exhausted (0%)
    if status == "limited":
        reset_desc = f" (预计解封: {resets_at})" if resets_at else " (已达当前限额)"
        res = {
            "percentage": 0,
            "status": "limited",
            "desc": f"额度已耗尽{reset_desc}",
            "resets_at": resets_at,
            "is_unlimited": False,
            "source": "status",
        }
    # 2. Never probed / expired verdict: neutral, not "0% left"
    elif status == "unknown":
        res = {
            "percentage": None,
            "status": "unknown",
            "desc": reason or "未探测 (中性处理；运行 'makewand probe' 获取实时状态)",
            "resets_at": None,
            "is_unlimited": False,
            "source": "unknown",
        }
    # 3. Disabled / Missing / Needs Auth / Error
    elif status in ("disabled", "needs_auth", "error", "missing"):
        desc = reason or "未就绪或未授权"
        if status == "needs_auth" and "重新登录" not in desc:
            desc = f"{desc} · {get_reauth_hint(provider)}"
        res = {
            "percentage": 0,
            "status": status,
            "desc": desc,
            "resets_at": None,
            "is_unlimited": False,
            "source": "status",
        }
    # 4. Unlimited local models or enterprise tiers
    elif provider == "local":
        res = {
            "percentage": 100,
            "status": "healthy",
            "desc": "本地私有模型 · 无云端额度 · 0 Token 成本",
            "resets_at": None,
            "is_unlimited": True,
            "source": "assumed",
        }
    elif provider == "agy":
        res = {
            "percentage": 100,
            "status": "healthy",
            "desc": "无额度查询接口，未做估算 (可用性以真实派发结果为准)",
            "resets_at": None,
            "is_unlimited": True,
            "source": "assumed",
        }
    elif reason and re.search(r"(\d+)\s*%\s*(?:left|remaining|剩余)", reason, re.IGNORECASE):
        pct_match = re.search(r"(\d+)\s*%\s*(?:left|remaining|剩余)", reason, re.IGNORECASE)
        pct = int(pct_match.group(1))
        res = {
            "percentage": max(0, min(100, pct)),
            "status": "healthy" if pct > 20 else ("warning" if pct > 0 else "limited"),
            "desc": f"官方报告剩余额度: {pct}%",
            "resets_at": resets_at,
            "is_unlimited": False,
            "source": "official",
        }
    else:
        # 5. Estimate from rolling usage and burn rate penalty
        try:
            from makewand.usage import get_burn_rate_penalty, get_engine_usage_stats
            penalty, pen_reason = get_burn_rate_penalty(provider)
            u24 = get_engine_usage_stats(window_hours=24.0).get(provider, {}).get("total", 0)

            if penalty <= -4.0:
                pct = 5
                desc = f"高频调用削峰保护中 (24h 调用: {u24}次)"
            elif penalty <= -3.0:
                pct = 20
                desc = f"额度消耗较快 (24h 调用: {u24}次)"
            elif penalty <= -2.0:
                pct = 40
                desc = f"滑动窗口用量活跃 (24h 调用: {u24}次)"
            elif penalty <= -1.0:
                pct = 65
                desc = f"滑动窗口运行平稳 (24h 调用: {u24}次)"
            else:
                if u24 == 0:
                    pct = 100
                    desc = "额度充沛 · 滑动窗口无压力"
                else:
                    pct = max(75, 100 - min(25, u24 * 2))
                    desc = f"额度充沛 · 运行健康 (24h 调用: {u24}次)"

            res = {
                "percentage": pct,
                "status": "healthy" if pct >= 25 else "warning",
                "desc": f"{desc} [本地调用计数估算，非真实配额]",
                "resets_at": resets_at,
                "is_unlimited": False,
                "source": "local_estimate",
            }
        except Exception:
            res = {
                "percentage": 85,
                "status": "healthy",
                "desc": "运行健康 [本地调用计数估算，非真实配额]",
                "resets_at": resets_at,
                "is_unlimited": False,
                "source": "local_estimate",
            }

    res["updated_at"] = updated_at
    res["source_label"] = QUOTA_SOURCE_LABELS.get(res.get("source"), "")
    if is_status_stale(info):
        res["stale"] = True
        res["desc"] = f"{res['desc']} · 状态缓存已超过 {STATUS_STALE_SECONDS // 3600} 小时未刷新，按中性处理 (运行 'makewand probe')"
    return res

