"""
Muse Code provider adapter (Meta subscription / Muse interactive coding agent).
"""

import os
import re
from datetime import datetime, timedelta
from typing import Tuple, Optional, List
from makewand.config import c, COLOR_PURPLE
from makewand.providers.base import run_subprocess, model_process_failure, is_process_timeout
from makewand.workflow import provider_outcome

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

def _is_dbus_systemd_available() -> bool:
    """Checks whether user-level systemd / D-Bus session bus is functional."""
    import shutil
    if not shutil.which("systemd-run"):
        return False

    addr = os.environ.get("DBUS_SESSION_BUS_ADDRESS", "")
    if addr:
        if addr.startswith("unix:path="):
            sock_path = addr.split("unix:path=", 1)[1].split(",", 1)[0]
            if not os.path.exists(sock_path):
                return False
    else:
        bus_path = f"/run/user/{os.getuid()}/bus"
        if not os.path.exists(bus_path):
            return False
    return True

def get_muse_executable(sandboxed: bool = False) -> Tuple[str, List[str]]:
    """
    Resolves the muse binary path and extra flags.
    In sandboxed environments (e.g. Bubblewrap, containers) or where D-Bus/systemd
    is unavailable, detects if 'muse' is a wrapper and directly invokes the real
    underlying binary (~/.local/libexec/muse-bin/muse) with --disable-sandbox to
    eliminate D-Bus Exit 17 crashes.
    """
    import shutil

    candidates = [
        os.environ.get("MUSE_REAL_BIN"),
        os.path.expanduser("~/.local/libexec/muse-bin/muse"),
    ]
    try:
        import pwd
        real_user_home = pwd.getpwuid(os.getuid()).pw_dir
        candidates.append(os.path.join(real_user_home, ".local/libexec/muse-bin/muse"))
    except Exception:
        pass

    real_bin = None
    for cand in candidates:
        if cand and os.path.isfile(cand) and os.access(cand, os.X_OK):
            real_bin = cand
            break

    muse_bin = shutil.which("muse") or "muse"

    needs_direct_passthrough = False
    if sandboxed or not _is_dbus_systemd_available():
        if real_bin:
            if muse_bin != real_bin or sandboxed:
                needs_direct_passthrough = True

    if needs_direct_passthrough and real_bin:
        return real_bin, ["--disable-sandbox"]
    return "muse", []

def execute_muse_task(
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
    Dispatches task to Muse Code.
    If readonly=True, omits --yolo bypass flag.
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
    if not has_subscription_configured("muse"):
        if has_api_configured("muse"):
            import sys
            print(c("[Makewand -> Muse] (纯 API 模式) 派发任务至 Meta API...", COLOR_PURPLE), file=sys.stderr)
            return provider_outcome(call_api_chat(provider="muse", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd, role="reviewer" if readonly else "coder"))
        return False, None, "未找到 Muse CLI 订阅，且未配置 META_API_KEY"

    if cache.get("muse", {}).get("status") in ["limited", "needs_auth"]:
        if has_api_configured("muse"):
            import sys
            print(c("[Makewand -> Muse] 订阅不可用或受限，无缝自动降级为 Meta API 模式接力执行...", COLOR_PURPLE), file=sys.stderr)
            return provider_outcome(call_api_chat(provider="muse", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd, role="reviewer" if readonly else "coder"))
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

    sandboxed = is_bwrap_available() or not _is_dbus_systemd_available()
    muse_bin, extra_flags = get_muse_executable(sandboxed=sandboxed)

    cmd = [muse_bin, "exec", "--no-session-log"]
    for flag in extra_flags:
        if flag not in cmd:
            cmd.append(flag)
    if not readonly:
        cmd.append("--yolo")
    else:
        cmd.extend(["--trust-workspace", "--disable-approval", "--disable-write"])
    if cwd:
        cmd.extend(["--workspace", str(cwd)])
    from makewand.discovery import get_provider_model_tier
    resolved = get_provider_model_tier("muse", tier)
    target_model = model or resolved.get("model")
    if target_model and target_model != "default":
        cmd.extend(["--model", target_model])
    target_effort = effort or resolved.get("effort")
    if target_effort and target_effort != "none":
        cmd.extend(["--reasoning-effort", target_effort])

    p_file = None
    p_dir = None
    try:
        if len(prompt.encode("utf-8")) > 32 * 1024:
            import tempfile
            p_dir = tempfile.mkdtemp(prefix="makewand-muse-")
            p_file = os.path.join(p_dir, "prompt.txt")
            with open(p_file, "w", encoding="utf-8") as pf:
                pf.write(prompt)
            cmd.extend(["--prompt-file", str(p_file)])
        else:
            cmd.append(prompt)

        if is_bwrap_available():
            try:
                cmd = wrap_bwrap(cmd, workspace=cwd, allow_network=allow_network, readonly=readonly, repo_root=repo_root, is_provider=True, provider_name="muse", extra_ro_binds=[p_file] if p_file else None)
            except SandboxConfigError as exc:
                return False, None, f"Muse 沙箱构建失败，拒绝执行 (fail closed): {exc}"
        elif repo_trust == "untrusted":
            return False, None, "不可信仓库 (--repo-trust=untrusted) 强制要求 Bubblewrap 物理沙箱隔离，未检测到 bwrap，拒绝执行"

        import sys
        print(c(f"[Makewand -> Muse] 派发任务 (Tier: {tier}, Meta Provider)...", COLOR_PURPLE), file=sys.stderr)
        from makewand.execution_runtime import mark_provider_invocation
        mark_provider_invocation()
        code, out, err, ex = run_subprocess(
            cmd,
            timeout=timeout,
            cwd=cwd,
            stream=stream,
            print_prefix=c("[Muse Live]", COLOR_PURPLE)
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
        return model_process_failure("muse", code, combined, err, ex, readonly)

    is_limited, reason, resets = parse_muse_quota(combined)
    if is_limited:
        record_engine_limit("muse", reason, resets)
        if has_api_configured("muse"):
            import sys
            print(c(f"[Makewand -> Muse] 订阅触发限流 ({reason})，无缝切换为 Meta API Key 模式接力执行...", COLOR_PURPLE), file=sys.stderr)
            return provider_outcome(call_api_chat(provider="muse", prompt=prompt, model=model, tier=tier, stream=stream, timeout=timeout, cwd=cwd, role="reviewer" if readonly else "coder"))
        return False, None, f"Muse Code 执行中检测到限制: {reason} (可配置 META_API_KEY 实现自动接力)"

    return model_process_failure("muse", code, combined, err, ex, readonly)
