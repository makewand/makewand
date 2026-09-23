"""
Codex CLI provider adapter (OpenAI subscription / gpt-6-astra).
"""

import re
from datetime import datetime, timedelta
from typing import Tuple, Optional
from makewand.config import c, COLOR_CYAN
from makewand.providers.base import run_subprocess

def parse_codex_quota(output: str) -> Tuple[bool, str, Optional[str]]:
    lower = output.lower()
    if "hit your usage limit" in lower or "rate_limit_exceeded" in lower:
        reset_match = re.search(r"try again at\s+([^.\n]+)", output, re.IGNORECASE)
        reset_time = reset_match.group(1).strip() if reset_match else "待重置"
        return True, f"使用额度耗尽 (重置时间: {reset_time})", reset_time
    if "429" in lower or "too many requests" in lower:
        iso_reset = (datetime.now() + timedelta(minutes=15)).isoformat()
        return True, "请求频率受限 (429)", iso_reset
    return False, "", None

def execute_codex_task(
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
    Dispatches task to Codex CLI.
    If readonly=True, enforces read-only sandbox mode, preventing any file modifications.
    If repo_root is provided, wraps execution in bubblewrap with transparent repo_root bind-mount.
    """
    from makewand.health import load_status_cache, save_status_cache, record_engine_limit
    from makewand.sandbox import is_bwrap_available, wrap_bwrap
    from makewand.git_helper import find_git_root
    cache = load_status_cache()
    if cache.get("codex", {}).get("status") == "limited":
        return False, None, f"Codex CLI 当前额度受限: {cache['codex'].get('reason')}"

    # Resolve repo_root if not provided but cwd is given
    if not repo_root and cwd:
        repo_root = find_git_root(cwd) or cwd

    # Fail-closed enforcement: if writable, sandbox is mandatory
    if not readonly:
        if not is_bwrap_available():
            return False, None, "Codex 写入任务强制要求 Bubblewrap (bwrap) 沙箱隔离，系统未检测到 bwrap，拒绝执行"
        if not (repo_root and cwd):
            return False, None, "Codex 写入任务缺少工作区目录或仓库根路径，无法建立沙箱隔离，拒绝执行"

    cmd = ["codex", "exec", "--skip-git-repo-check"]
    if readonly:
        cmd.extend(["--sandbox", "read-only", "--ephemeral"])
    else:
        cmd.append("--dangerously-bypass-approvals-and-sandbox")

    if cwd:
        cmd.extend(["-C", str(cwd)])
    if model:
        cmd.extend(["-c", f"model=\"{model}\""])
    elif tier == "deep":
        cmd.extend(["-c", "reasoning_effort=\"high\""])
    elif tier == "fast":
        cmd.extend(["-c", "reasoning_effort=\"low\""])

    cmd.append(prompt)

    if repo_root and cwd and is_bwrap_available():
        cmd = wrap_bwrap(cmd, workspace=cwd, allow_network=True, readonly=readonly, repo_root=repo_root, is_provider=True)

    import sys
    print(c(f"[Makewand -> Codex] 派发任务 (Tier: {tier}, gpt-6-astra)...", COLOR_CYAN), file=sys.stderr)
    code, out, err, ex = run_subprocess(
        cmd,
        timeout=timeout,
        cwd=cwd,
        stream=stream,
        print_prefix=c("[Codex Live]", COLOR_CYAN)
    )
    combined = f"{out}\n{err}" if not stream else out

    if code == 0:
        return True, out, None

    is_limited, reason, resets = parse_codex_quota(combined)
    if is_limited:
        record_engine_limit("codex", reason, resets)
        return False, None, f"Codex CLI 执行中触发额度限制: {reason}"

    return False, combined, ex or f"Codex returned exit code {code}"
