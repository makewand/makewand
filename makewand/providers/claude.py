"""
Claude Code provider adapter (Anthropic subscription).
"""

import os
import re
from datetime import datetime, timedelta
from typing import Tuple, Optional
from makewand.config import c, COLOR_BLUE
from makewand.providers.base import run_subprocess

def parse_claude_quota(output: str) -> Tuple[bool, str, Optional[str]]:
    lower = output.lower()
    if any(k in lower for k in ["hit your monthly spend limit", "hit your usage limit", "usage limit reached", "5-hour limit", "weekly limit reached", "exceeded your current quota"]):
        reset_match = re.search(r"(?:session|weekly|monthly|daily)?\s*limit resets\s+([^·\n]+)", output, re.IGNORECASE)
        reset_time = reset_match.group(1).strip() if reset_match else (datetime.now() + timedelta(hours=3)).isoformat()
        return True, f"限流 / 额度耗尽 (重置时间: {reset_time})", reset_time
    if "overloaded_error" in lower or "server overloaded" in lower:
        iso_reset = (datetime.now() + timedelta(minutes=3)).isoformat()
        return True, "Anthropic 服务端负载过高 (Overloaded)", iso_reset
    if "rate_limit_error" in lower or (re.search(r"\b(?:rate\s*limit(?:ed)?|too\s*many\s*requests|http\s+429|status(?:\s*code)?\s*[:=]?\s*429)\b", lower) and "middleware" not in lower and "test_" not in lower):
        reset_match = re.search(r"(?:session|weekly|monthly|daily)?\s*limit resets\s+([^·\n]+)", output, re.IGNORECASE)
        if reset_match:
            reset_time = reset_match.group(1).strip()
            return True, f"限流 / 额度耗尽 (重置时间: {reset_time})", reset_time
        iso_reset = (datetime.now() + timedelta(hours=3)).isoformat()
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
    repo_root: Optional[str] = None,
    repo_trust: str = "trusted",
    allow_network: bool = True
) -> Tuple[bool, Optional[str], Optional[str]]:
    """
    Dispatches task to Claude Code.
    If readonly=True, restricts tools to read-only inspection (Read, Grep, Glob) preventing writes.
    If repo_root is provided, wraps execution in bubblewrap with transparent repo_root bind-mount.
    """
    from makewand.health import load_status_cache, save_status_cache
    from makewand.sandbox import is_bwrap_available, wrap_bwrap
    from makewand.git_helper import find_git_root
    # Untrusted repo enforcement
    if repo_trust == "untrusted":
        allow_network = False
        if not is_bwrap_available():
            return False, None, "不可信仓库 (--repo-trust=untrusted) 强制要求 Bubblewrap 物理沙箱隔离，未检测到 bwrap，拒绝执行"
        if not readonly:
            return False, None, "不可信仓库 (--repo-trust=untrusted) 仅允许只读审计与分析，禁止执行写入或修改任务"

    cache = load_status_cache()
    from makewand.config import has_api_configured, has_subscription_configured
    from makewand.providers.api_client import call_api_chat

    # If subscription is missing or limited, fallback to Anthropic API
    if not has_subscription_configured("claude"):
        if has_api_configured("claude"):
            import sys
            print(c("[Makewand -> Claude] (纯 API 模式) 派发任务至 Anthropic Claude API...", COLOR_BLUE), file=sys.stderr)
            ok, out_api, err_api = call_api_chat(provider="claude", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd)
            if ok:
                return True, out_api, None
            return False, out_api, err_api
        return False, None, "未找到 Claude CLI 订阅，且未配置 ANTHROPIC_API_KEY"

    if cache.get("claude", {}).get("status") == "limited":
        if has_api_configured("claude"):
            import sys
            print(c("[Makewand -> Claude] 订阅额度受限，无缝自动降级为 Anthropic API 模式接力执行...", COLOR_BLUE), file=sys.stderr)
            ok, out_api, err_api = call_api_chat(provider="claude", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd)
            if ok:
                return True, out_api, None
            return False, out_api, err_api
        return False, None, f"Claude Code 当前额度受限: {cache['claude'].get('reason')} (可配置 ANTHROPIC_API_KEY 作为备用 API 自动接力)"

    # Normalize cwd and repo_root to ensure sandbox is never bypassed
    if not cwd:
        cwd = os.getcwd()
    cwd = os.path.abspath(cwd)

    if not repo_root:
        repo_root = find_git_root(cwd) or cwd
    repo_root = os.path.abspath(repo_root)

    # Fail-closed enforcement: if writable, sandbox is mandatory
    if not readonly:
        if not is_bwrap_available():
            return False, None, "Claude 写入任务强制要求 Bubblewrap (bwrap) 沙箱隔离，系统未检测到 bwrap，拒绝执行"

    cmd = ["claude", "-p", prompt]
    if readonly:
        cmd.extend(["--allowed-tools", "Read,Grep,Glob", "--permission-mode", "plan"])
    else:
        cmd.append("--dangerously-skip-permissions")

    if model:
        cmd.extend(["--model", model])
    else:
        from makewand.discovery import get_provider_model_tier
        resolved = get_provider_model_tier("claude", tier)
        cmd.extend(["--model", resolved["model"]])
        if resolved.get("effort") and resolved["effort"] != "none":
            cmd.extend(["--effort", resolved["effort"]])

    if is_bwrap_available():
        cmd = wrap_bwrap(cmd, workspace=cwd, allow_network=allow_network, readonly=readonly, repo_root=repo_root, is_provider=True, provider_name="claude")
    elif repo_trust == "untrusted":
        return False, None, "不可信仓库 (--repo-trust=untrusted) 强制要求 Bubblewrap 物理沙箱隔离，未检测到 bwrap，拒绝执行"

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
        if has_api_configured("claude"):
            import sys
            print(c(f"[Makewand -> Claude] 订阅触发限流 ({reason})，无缝切换为 Anthropic API Key 模式接力执行...", COLOR_BLUE), file=sys.stderr)
            ok, out_api, err_api = call_api_chat(provider="claude", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd)
            if ok:
                return True, out_api, None
            return False, out_api, err_api
        return False, None, f"Claude Code 执行中触发额度限制: {reason} (可配置 ANTHROPIC_API_KEY 实现自动接力)"

    return False, combined, ex or f"Claude returned exit code {code}"
