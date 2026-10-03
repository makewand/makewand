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

    from makewand.sandbox import is_bwrap_available, wrap_bwrap
    from makewand.git_helper import find_git_root
    from makewand.providers.base import run_subprocess, model_process_failure

    work_dir = os.path.abspath(cwd or os.getcwd())
    if not repo_root:
        repo_root = find_git_root(work_dir) or work_dir
    repo_root = os.path.abspath(repo_root)

    # Untrusted repo enforcement
    if repo_trust == "untrusted":
        allow_network = False
        if not is_bwrap_available():
            return False, None, "不可信仓库 (--repo-trust=untrusted) 强制要求 Bubblewrap 物理沙箱隔离，未检测到 bwrap，拒绝执行"
        if not readonly:
            return False, None, "不可信仓库 (--repo-trust=untrusted) 仅允许只读审计与分析，禁止执行写入或修改任务"

    # Fail-closed enforcement: if writable, sandbox is mandatory
    if not readonly:
        if not is_bwrap_available():
            return False, None, "Aider 写入任务强制要求 Bubblewrap (bwrap) 沙箱隔离，系统未检测到 bwrap，拒绝执行"

    p_file = None
    p_dir = None
    try:
        if len(prompt.encode("utf-8")) > 32 * 1024:
            import tempfile
            p_dir = tempfile.mkdtemp(prefix="makewand-aider-")
            p_file = os.path.join(p_dir, "prompt.txt")
            with open(p_file, "w", encoding="utf-8") as pf:
                pf.write(prompt)
            cmd = ["aider", "--message-file", str(p_file), "--yes-always", "--no-auto-commits", "--no-gitignore"]
        else:
            cmd = ["aider", "--message", prompt, "--yes-always", "--no-auto-commits", "--no-gitignore"]

        if readonly:
            cmd.append("--read-only")

        cfg = get_api_config("aider")
        active_model = model or cfg.get("model")
        if active_model:
            cmd += ["--model", active_model]

        if is_bwrap_available():
            cmd = wrap_bwrap(cmd, workspace=work_dir, allow_network=allow_network, readonly=readonly, repo_root=repo_root, is_provider=True, provider_name="aider", extra_ro_binds=[p_file] if p_file else None)

        log_desc = "只读解析任务" if readonly else "代码编写任务"
        print(c(f"[Makewand -> Aider] 派发{log_desc}至 Aider Pair Programmer (沙箱隔离)...", COLOR_GREEN), file=sys.stderr)

        from makewand.execution_runtime import mark_provider_invocation
        from makewand.sandbox import sandbox_lifecycle
        mark_provider_invocation()
        with sandbox_lifecycle(is_provider=True, provider_name="aider", cmd=cmd):
            code, out, err, ex = run_subprocess(
                cmd,
                timeout=timeout,
                cwd=work_dir,
                stream=stream,
                print_prefix=c("[Aider Live]", COLOR_GREEN)
            )
    finally:
        if p_file and os.path.exists(p_file):
            try:
                os.unlink(p_file)
            except Exception:
                pass
        if p_dir and os.path.exists(p_dir):
            try:
                import shutil
                shutil.rmtree(p_dir, ignore_errors=True)
            except Exception:
                pass
    if code == 0:
        return True, out or "Aider task completed successfully", None
    else:
        return model_process_failure("aider", code, out, err, ex, readonly)
