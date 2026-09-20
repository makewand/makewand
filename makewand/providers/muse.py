"""
Muse Code provider adapter (Meta subscription / Muse interactive coding agent).
"""

import re
from datetime import datetime
from typing import Tuple, Optional
from makewand.config import c, COLOR_PURPLE
from makewand.providers.base import run_subprocess

def parse_muse_quota(output: str) -> Tuple[bool, str, Optional[str]]:
    lower = output.lower()
    if any(k in lower for k in ["missing meta credentials", "open this page to sign in", "oauth/device", "auth required", "press enter to open"]):
        return True, "未登录或需配置凭据 (运行 'muse login' 或在 ~/.config/muse/env 中配置 META_API_KEY)", "需登录授权"
    if "rate limit" in lower or "usage limit" in lower or "429" in lower:
        return True, "Meta 订阅额度耗尽或频次受限", "待重置"
    return False, "", None

def execute_muse_task(
    prompt: str,
    cwd: Optional[str] = None,
    timeout: int = 300,
    tier: str = "standard",
    model: Optional[str] = None,
    stream: bool = False
) -> Tuple[bool, Optional[str], Optional[str]]:
    """
    Dispatches task to Muse Code with --yolo for headless execution.
    """
    from makewand.health import load_status_cache, save_status_cache
    cache = load_status_cache()
    if cache.get("muse", {}).get("status") in ["limited", "needs_auth"]:
        return False, None, f"Muse Code 当前不可用: {cache['muse'].get('reason')}"

    cmd = ["muse", "exec", "--yolo"]
    if cwd:
        cmd.extend(["--workspace", str(cwd)])
    if model:
        cmd.extend(["--model", model])
    elif tier == "deep":
        cmd.extend(["--reasoning-effort", "ultra"])
    elif tier == "fast":
        cmd.extend(["--reasoning-effort", "low"])
    else:
        cmd.extend(["--reasoning-effort", "high"])

    cmd.append(prompt)

    print(c(f"[Makewand -> Muse] 派发任务 (Tier: {tier}, Meta Provider)...", COLOR_PURPLE))
    code, out, err, ex = run_subprocess(
        cmd,
        timeout=timeout,
        cwd=cwd,
        stream=stream,
        print_prefix=c("[Muse Live]", COLOR_PURPLE)
    )
    combined = f"{out}\n{err}" if not stream else out

    if code == 0:
        return True, out, None

    is_limited, reason, resets = parse_muse_quota(combined)
    if is_limited:
        cache["muse"] = {
            "status": "needs_auth" if "登录" in reason else "limited",
            "reason": reason,
            "resets_at": resets,
            "updated_at": datetime.now().isoformat()
        }
        save_status_cache(cache)
        return False, None, f"Muse Code 执行中检测到限制: {reason}"

    return False, combined, ex or f"Muse returned exit code {code}"
