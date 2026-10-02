"""
Grok Build CLI provider adapter (xAI subscription / Grok Build interactive coding agent).
"""

import os
import re
from datetime import datetime, timedelta
from typing import Tuple, Optional
from makewand.config import c, COLOR_YELLOW
from makewand.providers.base import run_subprocess, model_process_failure, is_process_timeout
from makewand.workflow import provider_outcome

# Context-anchored patterns (aligned with codex/claude): a bare "429" token in
# arbitrary output ("line 429", "429 errors", PIDs, hashes) is never a limit.
_GROK_AUTH_RE = re.compile(
    r"missing\s+xai\s+credentials"
    r"|\b401\b[^\n]{0,40}\bunauthori[sz]ed\b"
    r"|\bunauthori[sz]ed\b[^\n]{0,40}\b401\b"
    r"|(?:^|[\s:\[(])unauthori[sz]ed(?:\s*[:.\])]|\s*$)"
    r"|\bauth(?:entication)?\s+required\b"
    r"|\blogin\s+required\b"
    r"|\bplease\s+sign\s+in\b"
    r"|\bsession\s+expired\b",
    re.MULTILINE,
)
_GROK_RATE_LIMIT_RE = re.compile(
    r"\b(?:"
    r"rate[\s_-]*limit(?:ed|\s+exceeded)?"
    r"|usage\s*limit"
    r"|too\s*many\s*requests"
    r"|insufficient\s+credits"
    r"|quota\s*exceeded"
    r"|resource[_\s]?exhausted"
    r"|429\s+too\s*many"
    r"|http(?:/[\d.]+)?\s+429"
    r"|status(?:\s*code)?\s*[:=]?\s*429"
    r"|code\s*[:=]\s*429"
    r")\b"
)


def parse_grok_quota(output: str) -> Tuple[bool, str, Optional[str]]:
    """
    Parses Grok CLI output for authentication errors and rate-limit / quota depletion markers.
    """
    lower = (output or "").lower()
    if _GROK_AUTH_RE.search(lower):
        return True, "未登录或需配置凭据 (请运行 'grok' 登录或在环境变量中配置 XAI_API_KEY)", "需登录授权"

    if _GROK_RATE_LIMIT_RE.search(lower):
        reset_match = re.search(r"resets?\s+(?:in|at)\s+([^.\n,]+)", output, re.IGNORECASE)
        if reset_match:
            reset_time = reset_match.group(1).strip()
            return True, f"xAI 订阅额度耗尽或频次受限 (429 / {reset_time})", reset_time
        iso_reset = (datetime.now() + timedelta(hours=2)).isoformat()
        return True, "xAI 订阅额度耗尽或频次受限 (429)", iso_reset

    return False, "", None

def execute_grok_task(
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
    Dispatches task to Grok Build CLI (xAI).
    If readonly=True, enforces --permission-mode plan.
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

    # If subscription is missing or limited, fallback to xAI API
    if not has_subscription_configured("grok"):
        if has_api_configured("grok"):
            import sys
            print(c("[Makewand -> Grok] (纯 API 模式) 派发任务至 xAI API...", COLOR_YELLOW), file=sys.stderr)
            return provider_outcome(call_api_chat(provider="grok", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd, role="reviewer" if readonly else "coder", repo_trust=repo_trust, readonly=readonly))
        return False, None, "未找到 Grok Build CLI 订阅，且未配置 XAI_API_KEY"

    if cache.get("grok", {}).get("status") in ["limited", "needs_auth"]:
        if has_api_configured("grok"):
            import sys
            print(c("[Makewand -> Grok] 订阅当前受限，无缝自动降级为 xAI API 模式接力执行...", COLOR_YELLOW), file=sys.stderr)
            return provider_outcome(call_api_chat(provider="grok", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd, role="reviewer" if readonly else "coder", repo_trust=repo_trust, readonly=readonly))
        return False, None, f"Grok Build 当前不可用: {cache['grok'].get('reason')} (可配置 XAI_API_KEY 作为备用 API 自动接力)"

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
            return False, None, "Grok 写入任务强制要求 Bubblewrap (bwrap) 沙箱隔离，系统未检测到 bwrap，拒绝执行"

    p_file = None
    p_dir = None
    try:
        if len(prompt.encode("utf-8")) > 32 * 1024:
            import tempfile
            p_dir = tempfile.mkdtemp(prefix="makewand-grok-")
            p_file = os.path.join(p_dir, "prompt.txt")
            with open(p_file, "w", encoding="utf-8") as pf:
                pf.write(prompt)
            cmd = ["grok", "--prompt-file", str(p_file), "--output-format", "plain"]
        else:
            cmd = ["grok", "-p", prompt, "--output-format", "plain"]

        if readonly:
            cmd.extend(["--permission-mode", "plan", "--max-turns", "25"])
        else:
            cmd.extend(["--always-approve", "--permission-mode", "bypassPermissions"])

        if cwd:
            cmd.extend(["--cwd", str(cwd)])

        from makewand.discovery import get_provider_model_tier
        resolved = get_provider_model_tier("grok", tier)
        target_model = model or resolved["model"]
        cmd.extend(["--model", target_model])
        target_effort = effort or resolved.get("effort")
        if target_effort and target_effort != "none":
            cmd.extend(["--reasoning-effort", target_effort])

        if is_bwrap_available():
            cmd = wrap_bwrap(cmd, workspace=cwd, allow_network=allow_network, readonly=readonly, repo_root=repo_root, is_provider=True, provider_name="grok", extra_ro_binds=[p_file] if p_file else None)
        elif repo_trust == "untrusted":
            return False, None, "不可信仓库 (--repo-trust=untrusted) 强制要求 Bubblewrap 物理沙箱隔离，未检测到 bwrap，拒绝执行"

        log_desc = "只读解析/审查任务 (Plan 模式)" if readonly else "代码任务 (自动审批执行)"
        import sys
        print(c(f"[Makewand -> Grok] 派发{log_desc} (Tier: {tier}, xAI Provider)...", COLOR_YELLOW), file=sys.stderr)
        from makewand.execution_runtime import mark_provider_invocation
        mark_provider_invocation()
        code, out, err, ex = run_subprocess(
            cmd,
            timeout=timeout,
            cwd=cwd,
            stream=stream,
            print_prefix=c("[Grok Live]", COLOR_YELLOW)
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
    combined = f"{out}\n{err}" if not stream else out

    if code == 0:
        return True, out, None

    if is_process_timeout(ex) or code < 0:
        return model_process_failure("grok", code, combined, err, ex, readonly)

    is_limited, reason, resets = parse_grok_quota(combined)
    if is_limited:
        record_engine_limit("grok", reason, resets)
        if has_api_configured("grok"):
            import sys
            print(c(f"[Makewand -> Grok] 订阅触发限制 ({reason})，无缝切换为 xAI API Key 模式接力执行...", COLOR_YELLOW), file=sys.stderr)
            return provider_outcome(call_api_chat(provider="grok", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd, role="reviewer" if readonly else "coder", repo_trust=repo_trust, readonly=readonly))
        return False, None, f"Grok Build 执行中检测到限制: {reason} (可配置 XAI_API_KEY 实现自动接力)"

    return model_process_failure("grok", code, combined, err, ex, readonly)
