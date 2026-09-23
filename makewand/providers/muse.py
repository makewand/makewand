"""
Muse Code provider adapter (Meta subscription / Muse interactive coding agent).
"""

import re
from datetime import datetime, timedelta
from typing import Tuple, Optional
from makewand.config import c, COLOR_PURPLE
from makewand.providers.base import run_subprocess

def parse_muse_quota(output: str) -> Tuple[bool, str, Optional[str]]:
    lower = output.lower()
    if any(k in lower for k in ["missing meta credentials", "open this page to sign in", "oauth/device", "auth required", "press enter to open"]):
        return True, "未登录或需配置凭据 (运行 'muse login' 或在 ~/.config/muse/env 中配置 META_API_KEY)", "需登录授权"
    if "rate limit" in lower or "usage limit" in lower or "429" in lower:
        iso_reset = (datetime.now() + timedelta(minutes=15)).isoformat()
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
    repo_root: Optional[str] = None
) -> Tuple[bool, Optional[str], Optional[str]]:
    """
    Dispatches task to Muse Code.
    If readonly=True, omits --yolo bypass flag.
    If repo_root is provided, wraps execution in bubblewrap with transparent repo_root bind-mount.
    """
    from makewand.health import load_status_cache, save_status_cache, record_engine_limit
    from makewand.sandbox import is_bwrap_available, wrap_bwrap
    from makewand.git_helper import find_git_root
    cache = load_status_cache()
    if cache.get("muse", {}).get("status") in ["limited", "needs_auth"]:
        return False, None, f"Muse Code 当前不可用: {cache['muse'].get('reason')}"

    # Resolve repo_root if not provided but cwd is given
    if not repo_root and cwd:
        repo_root = find_git_root(cwd) or cwd

    # Fail-closed enforcement: if writable, sandbox is mandatory
    if not readonly:
        if not is_bwrap_available():
            return False, None, "Muse 写入任务强制要求 Bubblewrap (bwrap) 沙箱隔离，系统未检测到 bwrap，拒绝执行"
        if not (repo_root and cwd):
            return False, None, "Muse 写入任务缺少工作区目录或仓库根路径，无法建立沙箱隔离，拒绝执行"

    cmd = ["muse", "exec"]
    if not readonly:
        cmd.append("--yolo")
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

    if repo_root and cwd and is_bwrap_available():
        cmd = wrap_bwrap(cmd, workspace=cwd, allow_network=True, readonly=readonly, repo_root=repo_root, is_provider=True)

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
        return False, None, f"Muse Code 执行中检测到限制: {reason}"

    return False, combined, ex or f"Muse returned exit code {code}"
