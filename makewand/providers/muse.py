"""
Muse Code provider adapter (Meta subscription / Muse interactive coding agent).
"""

import os
import re
from datetime import datetime, timedelta
from typing import Tuple, Optional
from makewand.config import c, COLOR_PURPLE
from makewand.providers.base import run_subprocess

def parse_muse_quota(output: str) -> Tuple[bool, str, Optional[str]]:
    lower = output.lower()
    if any(k in lower for k in ["missing meta credentials", "open this page to sign in", "oauth/device", "auth required", "press enter to open"]):
        return True, "未登录或需配置凭据 (运行 'muse login' 或在 ~/.config/muse/env 中配置 META_API_KEY)", "需登录授权"
    if "rate limit" in lower or "usage limit" in lower or re.search(r"\b(?:429|http\s+429|too\s*many\s*requests)\b", lower):
        reset_match = re.search(r"resets?\s+(?:in|at)\s+([^.\n,]+)", output, re.IGNORECASE)
        if reset_match:
            reset_time = reset_match.group(1).strip()
            return True, f"Meta 订阅额度耗尽或频次受限 ({reset_time})", reset_time
        iso_reset = (datetime.now() + timedelta(hours=2)).isoformat()
        return True, "Meta 订阅额度耗尽或频次受限", iso_reset
    return False, "", None

def execute_muse_task(
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
    Dispatches task to Muse Code.
    If readonly=True, omits --yolo bypass flag.
    If repo_root is provided, wraps execution in bubblewrap with transparent repo_root bind-mount.
    """
    from makewand.health import load_status_cache, save_status_cache, record_engine_limit
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

    # If subscription is limited or missing, fallback to API
    if not has_subscription_configured("muse"):
        if has_api_configured("muse"):
            import sys
            print(c("[Makewand -> Muse] (纯 API 模式) 派发任务至 Meta API...", COLOR_PURPLE), file=sys.stderr)
            ok, out, err = call_api_chat(provider="muse", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd)
            if ok:
                return True, out, None
            return False, out, err
        return False, None, "未找到 Muse CLI 订阅，且未配置 META_API_KEY"

    if cache.get("muse", {}).get("status") in ["limited", "needs_auth"]:
        if has_api_configured("muse"):
            import sys
            print(c("[Makewand -> Muse] 订阅不可用或受限，无缝自动降级为 Meta API 模式接力执行...", COLOR_PURPLE), file=sys.stderr)
            ok, out, err = call_api_chat(provider="muse", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd)
            if ok:
                return True, out, None
            return False, out, err
        return False, None, f"Muse Code 当前不可用: {cache['muse'].get('reason')} (可配置 META_API_KEY 作为备用 API 自动接力)"

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
            return False, None, "Muse 写入任务强制要求 Bubblewrap (bwrap) 沙箱隔离，系统未检测到 bwrap，拒绝执行"

    cmd = ["muse", "exec", "--no-session-log"]
    if not readonly:
        cmd.append("--yolo")
    else:
        cmd.extend(["--trust-workspace", "--disable-approval", "--disable-write"])
    if cwd:
        cmd.extend(["--workspace", str(cwd)])
    if model:
        cmd.extend(["--model", model])
    else:
        from makewand.discovery import get_provider_model_tier
        resolved = get_provider_model_tier("muse", tier)
        if resolved.get("model") and resolved["model"] != "default":
            cmd.extend(["--model", resolved["model"]])
        if resolved.get("effort"):
            cmd.extend(["--reasoning-effort", resolved["effort"]])

    cmd.append(prompt)

    if is_bwrap_available():
        cmd = wrap_bwrap(cmd, workspace=cwd, allow_network=allow_network, readonly=readonly, repo_root=repo_root, is_provider=True, provider_name="muse")
    elif repo_trust == "untrusted":
        return False, None, "不可信仓库 (--repo-trust=untrusted) 强制要求 Bubblewrap 物理沙箱隔离，未检测到 bwrap，拒绝执行"

    import sys
    print(c(f"[Makewand -> Muse] 派发任务 (Tier: {tier}, Meta Provider)...", COLOR_PURPLE), file=sys.stderr)
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
        record_engine_limit("muse", reason, resets)
        if has_api_configured("muse"):
            import sys
            print(c(f"[Makewand -> Muse] 订阅触发限流 ({reason})，无缝切换为 Meta API Key 模式接力执行...", COLOR_PURPLE), file=sys.stderr)
            ok, out_api, err_api = call_api_chat(provider="muse", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd)
            if ok:
                return True, out_api, None
            return False, out_api, err_api
        return False, None, f"Muse Code 执行中检测到限制: {reason} (可配置 META_API_KEY 实现自动接力)"

    return False, combined, ex or f"Muse returned exit code {code}"
