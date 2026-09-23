"""
Claude Code provider adapter (Anthropic subscription).
"""

import re
from datetime import datetime, timedelta
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
        iso_reset = (datetime.now() + timedelta(minutes=15)).isoformat()
        return True, "API 并发或频次超限 (429)", iso_reset
    return False, "", None

def execute_claude_task(
    prompt: str,
    cwd: Optional[str] = None,
    timeout: int = 300,
    tier: str = "standard",
    model: Optional[str] = None,
    stream: bool = False,
    readonly: bool = False,
    repo_root: Optional[str] = None
) -> Tuple[bool, Optional[str], Optional[str]]:
    """
    Dispatches task to Claude Code.
    If readonly=True, restricts tools to read-only inspection (Read, Grep, Glob) preventing writes.
    If repo_root is provided, wraps execution in bubblewrap with transparent repo_root bind-mount.
    """
    from makewand.health import load_status_cache, save_status_cache
    cache = load_status_cache()
    if cache.get("claude", {}).get("status") == "limited":
        return False, None, f"Claude Code 当前额度受限: {cache['claude'].get('reason')}"

    cmd = ["claude", "-p", prompt]
    if readonly:
        cmd.extend(["--tools", "Read,Grep,Glob", "--permission-mode", "plan"])
    else:
        cmd.append("--dangerously-skip-permissions")

    if model:
        cmd.extend(["--model", model])
    elif tier == "fast":
        cmd.extend(["--model", "haiku"])

    if repo_root and cwd:
        from makewand.sandbox import is_bwrap_available, wrap_bwrap
        if is_bwrap_available():
            cmd = wrap_bwrap(cmd, workspace=cwd, allow_network=True, readonly=readonly, repo_root=repo_root, is_provider=True)

    log_desc = "只读解析任务 (工具只读约束)" if readonly else "代码任务 (无头权限自动穿透)"
    import sys
    print(c(f"[Makewand -> Claude] 派发{log_desc} (Tier: {tier})...", COLOR_BLUE), file=sys.stderr)
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
        from makewand.health import record_engine_limit
        record_engine_limit("claude", reason, resets)
        return False, None, f"Claude Code 执行中触发额度限制: {reason}"

    return False, combined, ex or f"Claude returned exit code {code}"
