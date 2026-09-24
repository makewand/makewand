"""
Codex CLI provider adapter (OpenAI subscription / gpt-6-astra).
"""

import os
import re
from datetime import datetime, timedelta
from typing import Tuple, Optional
from makewand.config import c, COLOR_CYAN
from makewand.providers.base import run_subprocess

def parse_codex_quota(output: str) -> Tuple[bool, str, Optional[str]]:
    lower = output.lower()
    if "hit your usage limit" in lower or "rate_limit_exceeded" in lower:
        reset_match = re.search(r"try again at\s+([^.\n]+)", output, re.IGNORECASE)
        reset_time = reset_match.group(1).strip() if reset_match else (datetime.now() + timedelta(hours=3)).isoformat()
        return True, f"使用额度耗尽 (重置时间: {reset_time})", reset_time
    if re.search(r"\b(?:rate\s*limit|usage\s*limit|quota\s*exceeded|too\s*many\s*requests|429\s+too\s*many|http\s+429|status(?:\s*code)?\s*[:=]?\s*429)\b", lower):
        iso_reset = (datetime.now() + timedelta(hours=3)).isoformat()
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
    if not has_subscription_configured("codex"):
        if has_api_configured("codex"):
            import sys
            print(c("[Makewand -> Codex] (纯 API 模式) 派发任务至 OpenAI API...", COLOR_CYAN), file=sys.stderr)
            ok, out, err = call_api_chat(provider="codex", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd)
            if ok:
                return True, out, None
            return False, out, err
        return False, None, "未找到 Codex CLI 订阅，且未配置 OPENAI_API_KEY"

    if cache.get("codex", {}).get("status") == "limited":
        if has_api_configured("codex"):
            import sys
            print(c("[Makewand -> Codex] 订阅配额已耗尽，无缝自动降级为 OpenAI API 模式接力执行...", COLOR_CYAN), file=sys.stderr)
            ok, out, err = call_api_chat(provider="codex", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd)
            if ok:
                return True, out, None
            return False, out, err
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
    if model:
        cmd.extend(["-c", f"model=\"{model}\""])
    else:
        from makewand.discovery import get_provider_model_tier
        resolved = get_provider_model_tier("codex", tier)
        cmd.extend(["-c", f"model=\"{resolved['model']}\""])
        cmd.extend(["-c", f"model_reasoning_effort=\"{resolved['effort']}\""])

    cmd.append(prompt)

    if is_bwrap_available():
        cmd = wrap_bwrap(cmd, workspace=cwd, allow_network=allow_network, readonly=readonly, repo_root=repo_root, is_provider=True, provider_name="codex")
    elif repo_trust == "untrusted":
        return False, None, "不可信仓库 (--repo-trust=untrusted) 强制要求 Bubblewrap 物理沙箱隔离，未检测到 bwrap，拒绝执行"

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
        if has_api_configured("codex"):
            import sys
            print(c(f"[Makewand -> Codex] 订阅触发限流 ({reason})，无缝切换为 OpenAI API Key 模式接力执行...", COLOR_CYAN), file=sys.stderr)
            ok, out_api, err_api = call_api_chat(provider="codex", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd)
            if ok:
                return True, out_api, None
            return False, out_api, err_api
        return False, None, f"Codex CLI 执行中触发额度限制: {reason} (可配置 OPENAI_API_KEY 实现自动接力)"

    return False, combined, ex or f"Codex returned exit code {code}"
