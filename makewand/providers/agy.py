"""
Antigravity CLI provider adapter (Google AI Pro / Gemini 3.8 Flash & Pro).
"""

import re
from typing import Tuple, Optional
from makewand.config import c, COLOR_GREEN
from makewand.providers.base import run_subprocess

def parse_agy_quota(output: str) -> Tuple[bool, str, Optional[str]]:
    lower = output.lower()
    if "resourceexhausted" in lower or "quota exceeded" in lower or "429" in lower:
        return True, "Google AI 配额暂时耗尽", "待重置"
    return False, "", None

def execute_agy_task(
    prompt: str,
    cwd: Optional[str] = None,
    timeout: int = 300,
    tier: str = "standard",
    model: Optional[str] = None,
    stream: bool = False
) -> Tuple[bool, Optional[str], Optional[str]]:
    """
    Dispatches task to Antigravity CLI with automatic headless permission bypass.
    """
    cmd = ["agy", "-p", prompt, "--dangerously-skip-permissions", "--print-timeout", f"{timeout}s"]
    if model:
        cmd.extend(["--model", model])
    elif tier == "deep":
        cmd.extend(["--effort", "high"])
    elif tier == "fast":
        cmd.extend(["--effort", "low"])
    else:
        cmd.extend(["--effort", "medium"])

    print(c(f"[Makewand -> Antigravity] 派发架构/兜底任务 (Tier: {tier}, 权限自动穿透)...", COLOR_GREEN))
    code, out, err, ex = run_subprocess(
        cmd,
        timeout=timeout + 15,
        cwd=cwd,
        stream=stream,
        print_prefix=c("[AGY Live]", COLOR_GREEN)
    )
    combined = f"{out}\n{err}" if not stream else out

    is_limited, reason, _ = parse_agy_quota(combined)
    if is_limited:
        return False, None, f"Antigravity 配额受限: {reason}"
    if code == 0:
        return True, out, None
    return False, combined, ex or f"agy returned exit code {code}"
