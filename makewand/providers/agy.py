"""
Antigravity CLI provider adapter (Google AI Pro / Gemini 3.8 Flash & Pro).
"""

import os
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
    effort: Optional[str] = None,
    stream: bool = False,
    readonly: bool = False,
    repo_root: Optional[str] = None,
    repo_trust: str = "trusted",
    allow_network: bool = True
) -> Tuple[bool, Optional[str], Optional[str]]:
    """
    Dispatches task to Antigravity CLI.
    If readonly=True, enforces read-only instructions and constraints.
    If repo_root is provided, wraps execution in bubblewrap with transparent repo_root bind-mount.
    """
    from makewand.sandbox import is_bwrap_available, wrap_bwrap
    from makewand.git_helper import find_git_root

    # Normalize cwd and repo_root to ensure sandbox is never bypassed
    if not cwd:
        cwd = os.getcwd()
    cwd = os.path.abspath(cwd)

    if not repo_root:
        repo_root = find_git_root(cwd) or cwd
    repo_root = os.path.abspath(repo_root)

    # Untrusted repo enforcement
    if repo_trust == "untrusted":
        allow_network = False
        if not is_bwrap_available():
            return False, None, "不可信仓库 (--repo-trust=untrusted) 强制要求 Bubblewrap 物理沙箱隔离，未检测到 bwrap，拒绝执行"
        if not readonly:
            return False, None, "不可信仓库 (--repo-trust=untrusted) 仅允许只读审计与分析，禁止执行写入或修改任务"

    from makewand.health import load_status_cache, record_engine_limit
    from makewand.config import has_api_configured, has_subscription_configured
    from makewand.providers.api_client import call_api_chat

    # If subscription is missing or limited, fallback to Gemini API
    if not has_subscription_configured("agy"):
        if has_api_configured("agy"):
            import sys
            print(c("[Makewand -> Antigravity] (纯 API 模式) 派发任务至 Google Gemini API...", COLOR_GREEN), file=sys.stderr)
            ok, out_api, err_api = call_api_chat(provider="agy", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd)
            if ok:
                return True, out_api, None
            return False, out_api, err_api
        return False, None, "未找到 agy CLI 订阅，且未配置 GEMINI_API_KEY"

    cache = load_status_cache()
    if cache.get("agy", {}).get("status") in ["limited", "needs_auth"]:
        if has_api_configured("agy"):
            import sys
            print(c("[Makewand -> Antigravity] 订阅当前受限，无缝自动降级为 Gemini API 模式接力执行...", COLOR_GREEN), file=sys.stderr)
            ok, out_api, err_api = call_api_chat(provider="agy", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd)
            if ok:
                return True, out_api, None
            return False, out_api, err_api
        return False, None, f"Antigravity 当前不可用: {cache['agy'].get('reason')} (可配置 GEMINI_API_KEY 作为备用 API 自动接力)"

    # Fail-closed enforcement: if writable, sandbox is mandatory
    if not readonly:
        if not is_bwrap_available():
            return False, None, "Antigravity 写入任务强制要求 Bubblewrap (bwrap) 沙箱隔离，系统未检测到 bwrap，拒绝执行"

    p_file = None
    try:
        final_prompt = f"【只读分析任务，严禁任何代码文件修改或写操作】\n{prompt}" if readonly else prompt
        base_flags = [
            "--print-timeout", f"{timeout}s",
            "--disable-slash-commands"
        ]
        if readonly:
            base_flags.extend(["--mode", "plan"])
        else:
            base_flags.append("--dangerously-skip-permissions")

        if len(final_prompt.encode("utf-8")) > 32 * 1024:
            import time
            p_file = os.path.join(cwd, f".makewand_agy_p_{os.getpid()}_{time.time_ns()}.txt")
            with open(p_file, "w", encoding="utf-8") as pf:
                pf.write(final_prompt)
            prompt_instruction = f"请读取并完整执行当前目录任务文件 {os.path.basename(p_file)} 中所指定的任务要求与代码规范："
            cmd = ["agy", "-p", prompt_instruction] + base_flags
        else:
            cmd = ["agy", "-p", final_prompt] + base_flags

        from makewand.discovery import get_provider_model_tier
        resolved = get_provider_model_tier("agy", tier)
        target_model = model or resolved.get("model")
        if target_model and target_model != "default":
            cmd.extend(["--model", target_model])
        target_effort = effort or resolved.get("effort")
        if target_effort and target_effort != "none":
            cmd.extend(["--effort", target_effort])

        if is_bwrap_available():
            cmd = wrap_bwrap(cmd, workspace=cwd, allow_network=allow_network, readonly=readonly, repo_root=repo_root, is_provider=True, provider_name="agy")
        elif repo_trust == "untrusted":
            return False, None, "不可信仓库 (--repo-trust=untrusted) 强制要求 Bubblewrap 物理沙箱隔离，未检测到 bwrap，拒绝执行"

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
    finally:
        if p_file and os.path.exists(p_file):
            try:
                os.unlink(p_file)
            except Exception:
                pass
    combined = f"{out}\n{err}" if not stream else out

    if code == 0:
        cleaned_out = out.strip() if out else ""
        if cleaned_out:
            return True, cleaned_out, None
        cleaned_err = err.strip() if err else ""
        if cleaned_err and not any(k in cleaned_err.lower() for k in ["error", "fatal", "timed out"]):
            return True, cleaned_err, None
        return False, None, "Antigravity 执行完成但未能产生有效输出内容"

    is_limited, reason, resets_at = parse_agy_quota(combined)
    if is_limited:
        record_engine_limit("agy", reason, resets_at)
        if has_api_configured("agy"):
            import sys
            print(c(f"[Makewand -> Antigravity] 订阅触发配额限制 ({reason})，无缝切换为 Gemini API Key 模式接力执行...", COLOR_GREEN), file=sys.stderr)
            ok, out_api, err_api = call_api_chat(provider="agy", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd)
            if ok:
                return True, out_api, None
            return False, out_api, err_api
        return False, None, f"Antigravity 配额受限: {reason} (可配置 GEMINI_API_KEY 实现自动接力)"
    return False, combined, ex or f"agy returned exit code {code}"
