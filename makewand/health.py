"""
Health probing, quota monitoring, and status cache management.
"""

import json
from datetime import datetime
from typing import Dict, Any
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

def load_status_cache() -> Dict[str, Any]:
    ensure_config_dir()
    cache = dict(DEFAULT_CACHE)
    if STATUS_CACHE_FILE.exists():
        try:
            with open(STATUS_CACHE_FILE, "r", encoding="utf-8") as f:
                data = json.load(f)
                cache.update(data)
                return cache
        except Exception:
            pass

    # Fallback to legacy trio status if available
    if LEGACY_TRIO_CACHE.exists():
        try:
            with open(LEGACY_TRIO_CACHE, "r", encoding="utf-8") as f:
                data = json.load(f)
                cache.update(data)
                return cache
        except Exception:
            pass

    return cache

def save_status_cache(cache: Dict[str, Any]):
    ensure_config_dir()
    try:
        with open(STATUS_CACHE_FILE, "w", encoding="utf-8") as f:
            json.dump(cache, f, indent=2, ensure_ascii=False)
        # Also update legacy trio cache file for backward compatibility
        if LEGACY_TRIO_CACHE.parent.exists():
            with open(LEGACY_TRIO_CACHE, "w", encoding="utf-8") as f:
                json.dump(cache, f, indent=2, ensure_ascii=False)
    except Exception:
        pass

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
