"""
Claude Code provider adapter (Anthropic subscription).
"""

import re
from datetime import datetime
from typing import Tuple, Optional
from makewand.config import c, COLOR_BLUE
from makewand.providers.base import run_subprocess

def parse_claude_quota(output: str) -> Tuple[bool, str, Optional[str]]:
    lower = output.lower()
    if "hit your monthly spend limit" in lower or "hit your usage limit" in lower or "rate limit" in lower:
        reset_match = re.search(r"weekly limit resets\s+([^·\n]+)", output, re.IGNORECASE)
        reset_time = reset_match.group(1).strip() if reset_match else "待重置"
        return True, f"限流 / 额度耗尽 (重置时间: {reset_time})", reset_time
    if "rate_limit_error" in lower or "overloaded_error" in lower or "429" in lower:
        return True, "API 并发或频次超限 (429)", "短期恢复"
    return False, "", None

def execute_claude_task(
    prompt: str,
    cwd: Optional[str] = None,
    timeout: int = 300,
    tier: str = "standard",
    model: Optional[str] = None,
    stream: bool = False
) -> Tuple[bool, Optional[str], Optional[str]]:
    """
    Dispatches task to Claude Code with automatic headless permission bypass.
    """
    from makewand.health import load_status_cache, save_status_cache
    cache = load_status_cache()
    if cache.get("claude", {}).get("status") == "limited":
        return False, None, f"Claude Code 当前额度受限: {cache['claude'].get('reason')}"

    cmd = ["claude", "-p", prompt, "--dangerously-skip-permissions"]
    if model:
        cmd.extend(["--model", model])
    elif tier == "fast":
        cmd.extend(["--model", "haiku"])

    print(c(f"[Makewand -> Claude] 派发代码任务 (Tier: {tier}, 无头权限自动穿透)...", COLOR_BLUE))
    code, out, err, ex = run_subprocess(
        cmd,
        timeout=timeout,
        cwd=cwd,
        stream=stream,
        print_prefix=c("[Claude Live]", COLOR_BLUE)
    )
    combined = f"{out}\n{err}" if not stream else out

    if code == 0:
        return True, out, None

    is_limited, reason, resets = parse_claude_quota(combined)
    if is_limited:
        cache["claude"] = {
            "status": "limited",
            "reason": reason,
            "resets_at": resets,
            "updated_at": datetime.now().isoformat()
        }
        save_status_cache(cache)
        return False, None, f"Claude Code 执行中触发额度限制: {reason}"

    return False, combined, ex or f"Claude returned exit code {code}"
