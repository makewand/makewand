"""
Aider Multi-Model AI Pair Programmer Provider.
Integrates local aider CLI into Makewand orchestration pipeline.
"""
import os
import sys
import shutil
import subprocess
from typing import Tuple, Optional
from makewand.config import c, COLOR_GREEN, COLOR_RED, COLOR_YELLOW, is_provider_enabled, get_api_config

def is_aider_available() -> bool:
    """Returns True if aider executable is found in system PATH."""
    return shutil.which("aider") is not None

def execute_aider_task(
    prompt: str,
    cwd: Optional[str] = None,
    timeout: int = 300,
    tier: str = "standard",
    model: Optional[str] = None,
    stream: bool = False,
    readonly: bool = False,
    repo_root: Optional[str] = None,
    repo_trust: str = "trusted",
    allow_network: bool = True,
    role: str = "coder",
    **kwargs
) -> Tuple[bool, Optional[str], Optional[str]]:
    """
    Executes a task using local Aider pair programming CLI.
    Runs non-interactively with --message and --yes-always.
    """
    if not is_provider_enabled("aider"):
        return False, None, "Aider 当前已被用户在配置中手动禁用。运行 'makewand enable aider' 重新开启"

    if not is_aider_available():
        return False, None, "未在 PATH 中找到 'aider' 命令。请先运行 'pip install aider-chat' 或使用其他活跃模型"

    work_dir = cwd or os.getcwd()
    cmd = ["aider", "--message", prompt, "--yes-always", "--no-auto-commits", "--no-gitignore"]

    if readonly:
        cmd.append("--read-only")

    cfg = get_api_config("aider")
    active_model = model or cfg.get("model")
    if active_model:
        cmd += ["--model", active_model]

    print(c(f"[Makewand -> Aider] 派发任务至 Aider Pair Programmer...", COLOR_GREEN), file=sys.stderr)

    try:
        proc = subprocess.run(
            cmd,
            cwd=work_dir,
            timeout=timeout,
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True
        )
        out = proc.stdout
        err = proc.stderr
        if proc.returncode == 0:
            return True, out or "Aider task completed successfully", None
        else:
            return False, out, err or f"Aider exited with code {proc.returncode}"
    except subprocess.TimeoutExpired:
        return False, None, f"Aider task timed out after {timeout}s"
    except Exception as e:
        return False, None, f"Aider execution error: {e}"
