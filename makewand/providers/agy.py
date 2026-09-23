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
    stream: bool = False,
    readonly: bool = False,
    repo_root: Optional[str] = None
) -> Tuple[bool, Optional[str], Optional[str]]:
    """
    Dispatches task to Antigravity CLI.
    If readonly=True, enforces read-only instructions and constraints.
    If repo_root is provided, wraps execution in bubblewrap with transparent repo_root bind-mount.
    """
    from makewand.sandbox import is_bwrap_available, wrap_bwrap
    from makewand.git_helper import find_git_root

    # Resolve repo_root if not provided but cwd is given
    if not repo_root and cwd:
        repo_root = find_git_root(cwd) or cwd

    # Fail-closed enforcement: if writable, sandbox is mandatory
    if not readonly:
        if not is_bwrap_available():
            return False, None, "Antigravity 写入任务强制要求 Bubblewrap (bwrap) 沙箱隔离，系统未检测到 bwrap，拒绝执行"
        if not (repo_root and cwd):
            return False, None, "Antigravity 写入任务缺少工作区目录或仓库根路径，无法建立沙箱隔离，拒绝执行"

    final_prompt = f"【只读分析任务，严禁任何代码文件修改或写操作】\n{prompt}" if readonly else prompt
    cmd = [
        "agy", "-p", final_prompt,
        "--print-timeout", f"{timeout}s",
        "--dangerously-skip-permissions",
        "--disable-slash-commands"
    ]
    if readonly:
        cmd.extend(["--mode", "plan"])

    if model:
        cmd.extend(["--model", model])
    elif tier == "deep":
        cmd.extend(["--effort", "high"])
    elif tier == "fast":
        cmd.extend(["--effort", "low"])
    else:
        cmd.extend(["--effort", "medium"])

    if repo_root and cwd:
        if is_bwrap_available():
            cmd = wrap_bwrap(cmd, workspace=cwd, allow_network=True, readonly=readonly, repo_root=repo_root, is_provider=True)

    log_desc = "只读解析任务 (禁止写操作)" if readonly else "架构/兜底任务 (权限自动穿透)"
    import sys
    print(c(f"[Makewand -> Antigravity] 派发{log_desc} (Tier: {tier})...", COLOR_GREEN), file=sys.stderr)
    code, out, err, ex = run_subprocess(
        cmd,
        timeout=timeout + 15,
        cwd=cwd,
        stream=stream,
        print_prefix=c("[AGY Live]", COLOR_GREEN)
    )
    combined = f"{out}\n{err}" if not stream else out

    if code == 0:
        cleaned_out = out.strip() if out else ""
        if cleaned_out:
            return True, cleaned_out, None
        cleaned_err = err.strip() if err else ""
        if cleaned_err and not any(k in cleaned_err.lower() for k in ["error", "fatal", "timed out"]):
            return True, cleaned_err, None
        return False, None, "Antigravity 执行完成但未能产生有效输出内容"

    is_limited, reason, _ = parse_agy_quota(combined)
    if is_limited:
        return False, None, f"Antigravity 配额受限: {reason}"
    return False, combined, ex or f"agy returned exit code {code}"
