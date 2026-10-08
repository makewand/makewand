"""
Codex CLI provider adapter (OpenAI subscription / gpt-6-astra).
"""

import os
import re
from datetime import datetime, timedelta
from typing import Tuple, Optional
from makewand.config import c, COLOR_CYAN
from makewand.providers.base import run_subprocess, model_process_failure, is_process_timeout
from makewand.workflow import provider_outcome

def parse_codex_quota(output: str) -> Tuple[bool, str, Optional[str]]:
    lower = output.lower()

    # 2. Hard quota / usage limit reached
    if any(k in lower for k in [
        "hit your usage limit",
        "hit your limit",
        "reached your usage limit",
        "reached your limit",
        "usage limit reached",
        "weekly limit reached",
        "rate_limit_exceeded",
        "exceeded your current quota",
        "exceeded your limit",
    ]):
        reset_match = re.search(r"(?:try\s+again\s+(?:at|in)|resets\s+(?:at|in)?)\s+([^·.\n]+)", output, re.IGNORECASE)
        reset_time = reset_match.group(1).strip().rstrip(". ") if reset_match else (datetime.now() + timedelta(hours=3)).isoformat()
        return True, f"使用额度耗尽 (重置时间: {reset_time})", reset_time

    # 3. 429 and rate limit patterns with context anchoring
    if re.search(r"\b(?:rate\s*limit(?:ed)?|usage\s*limit|quota\s*exceeded|too\s*many\s*requests|429\s+too\s*many|http\s+429|status(?:\s*code)?\s*[:=]?\s*429)\b", lower):
        if "middleware" not in lower and "test_" not in lower:
            reset_match = re.search(r"(?:try\s+again\s+(?:at|in)|resets\s+(?:at|in)?)\s+([^·.\n]+)", output, re.IGNORECASE)
            reset_time = reset_match.group(1).strip().rstrip(". ") if reset_match else (datetime.now() + timedelta(hours=3)).isoformat()
            return True, f"请求频率受限 (429 · 重置时间: {reset_time})", reset_time

    # Warnings are not hard 429/exhaustion evidence. Preserve the legacy
    # <=10% policy unless a valid reserve was explicitly selected.
    exact = re.search(r"weekly\s*limit:\s*(\d+(?:\.\d+)?)%\s*left", lower)
    upper = re.search(r"less\s*than\s*(\d+)%\s*of\s*(?:your\s+)?weekly\s*limit\s*left", lower)
    if exact or upper:
        from makewand.health import QUOTA_RESERVE_ENV, _quota_reserve_setting, _get_official_subscription_quota
        reserve, error = _quota_reserve_setting()
        if QUOTA_RESERVE_ENV not in os.environ or error:
            pct_val = float((exact or upper).group(1))
            if pct_val <= 10:
                return True, f"每周额度濒临耗尽 (剩余 {pct_val:g}%)", (datetime.now() + timedelta(hours=3)).isoformat()
        else:
            if exact:
                remaining = float(exact.group(1))
                known = 0 <= remaining <= 100
            else:
                # "less than 10%" is only an upper bound. Do not turn it into
                # assumed capacity; consult the unchanged account/TTL reader.
                quota = _get_official_subscription_quota("codex")
                remaining = quota.get("percentage") if quota and quota.get("source") == "official" else None
                known = isinstance(remaining, (int, float)) and not isinstance(remaining, bool)
            if not known or remaining <= 0 or remaining < reserve:
                detail = f"剩余 {remaining:g}% < {reserve:g}%" if known and remaining > 0 else "剩余额度耗尽或无法确认"
                return True, f"配额保护阈值拦截 ({detail})", (datetime.now() + timedelta(hours=3)).isoformat()

    return False, "", None

def execute_codex_task(
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
    Dispatches task to Codex CLI.
    If readonly=True, enforces read-only sandbox mode, preventing any file modifications.
    If repo_root is provided, wraps execution in bubblewrap with transparent repo_root bind-mount.
    """
    from makewand.health import load_status_cache, save_status_cache, record_engine_limit
    from makewand.sandbox import is_bwrap_available, wrap_bwrap, SandboxConfigError
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
    if not has_subscription_configured("codex"):
        if has_api_configured("codex"):
            import sys
            print(c("[Makewand -> Codex] (纯 API 模式) 派发任务至 OpenAI API...", COLOR_CYAN), file=sys.stderr)
            return provider_outcome(call_api_chat(provider="codex", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd, role="reviewer" if readonly else "coder", repo_trust=repo_trust, readonly=readonly))
        return False, None, "未找到 Codex CLI 订阅，且未配置 OPENAI_API_KEY"

    if cache.get("codex", {}).get("status") == "limited":
        if has_api_configured("codex"):
            import sys
            print(c("[Makewand -> Codex] 订阅配额已耗尽，无缝自动降级为 OpenAI API 模式接力执行...", COLOR_CYAN), file=sys.stderr)
            return provider_outcome(call_api_chat(provider="codex", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd, role="reviewer" if readonly else "coder", repo_trust=repo_trust, readonly=readonly))
        return False, None, f"Codex CLI 当前额度受限: {cache['codex'].get('reason')} (可配置 OPENAI_API_KEY 作为备用 API 自动接力)"

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
            return False, None, "Codex 写入任务强制要求 Bubblewrap (bwrap) 沙箱隔离，系统未检测到 bwrap，拒绝执行"

    cmd = ["codex", "exec", "--skip-git-repo-check"]
    if readonly:
        cmd.extend(["--sandbox", "read-only", "--ephemeral"])
    else:
        cmd.append("--dangerously-bypass-approvals-and-sandbox")

    if cwd:
        cmd.extend(["-C", str(cwd)])
    from makewand.discovery import get_provider_model_tier
    resolved = get_provider_model_tier("codex", tier)
    target_model = model or resolved["model"]
    cmd.extend(["-c", f"model=\"{target_model}\""])
    target_effort = effort or resolved.get("effort")
    if target_effort and target_effort != "none":
        cmd.extend(["-c", f"model_reasoning_effort=\"{target_effort}\""])

    input_text = None
    if len(prompt.encode("utf-8")) > 32 * 1024:
        cmd.append("-")
        input_text = prompt
    else:
        cmd.append(prompt)

    if is_bwrap_available():
        try:
            cmd = wrap_bwrap(cmd, workspace=cwd, allow_network=allow_network, readonly=readonly, repo_root=repo_root, is_provider=True, provider_name="codex")
        except SandboxConfigError as exc:
            return False, None, f"Codex 沙箱构建失败，拒绝执行 (fail closed): {exc}"
    elif repo_trust == "untrusted":
        return False, None, "不可信仓库 (--repo-trust=untrusted) 强制要求 Bubblewrap 物理沙箱隔离，未检测到 bwrap，拒绝执行"

    import sys
    print(c(f"[Makewand -> Codex] 派发任务 (Tier: {tier}, {target_model})...", COLOR_CYAN), file=sys.stderr)
    from makewand.execution_runtime import mark_provider_invocation
    from makewand.sandbox import sandbox_lifecycle
    mark_provider_invocation()
    with sandbox_lifecycle(is_provider=True, provider_name="codex", cmd=cmd, readonly=readonly):
        code, out, err, ex = run_subprocess(
            cmd,
            timeout=timeout,
            cwd=cwd,
            input_text=input_text,
            stream=stream,
            print_prefix=c("[Codex Live]", COLOR_CYAN)
        )
    combined = f"{out}\n{err}" if not stream else out

    if code == 0:
        return True, out, None

    if is_process_timeout(ex) or code < 0:
        return model_process_failure("codex", code, combined, err, ex, readonly)

    is_limited, reason, resets = parse_codex_quota(combined)
    if is_limited:
        record_engine_limit("codex", reason, resets)
        if has_api_configured("codex"):
            import sys
            print(c(f"[Makewand -> Codex] 订阅触发限流 ({reason})，无缝切换为 OpenAI API Key 模式接力执行...", COLOR_CYAN), file=sys.stderr)
            return provider_outcome(call_api_chat(provider="codex", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd, role="reviewer" if readonly else "coder", repo_trust=repo_trust, readonly=readonly))
        return False, None, f"Codex CLI 执行中触发额度限制: {reason} (可配置 OPENAI_API_KEY 实现自动接力)"

    return model_process_failure("codex", code, combined, err, ex, readonly)
