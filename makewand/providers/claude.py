"""
Claude Code provider adapter (Anthropic subscription).
"""

import os
import re
from datetime import datetime, timedelta
from typing import Tuple, Optional
from makewand.config import c, COLOR_BLUE
from makewand.providers.base import ProviderAPIRelay, run_subprocess, model_process_failure, is_process_timeout, stderr_captured_apart

# Built-in tools available to read-only Claude tasks (review / analysis).
CLAUDE_READONLY_TOOLS = "Read,Grep,Glob"

# "Claude AI usage limit reached|<unix seconds>": the reset is the epoch suffix.
_CLAUDE_EPOCH_RESET_RE = re.compile(r"usage\s+limit\s+reached\s*\|\s*(\d{10})\b", re.IGNORECASE)


def parse_claude_quota(output: str, stderr: Optional[str] = None) -> Tuple[bool, str, Optional[str]]:
    """Limit refusals Claude Code prints itself (health.CLI_MESSAGE_CATALOG["claude"]).

    `stderr` is passed when the streams were captured apart; otherwise
    `output` is the merged text.
    """
    from makewand.health import find_cli_messages, extract_reset_phrase
    found = find_cli_messages("claude", ("quota", "overload", "rate"), output, stderr)
    line = found.get("quota")
    if line is not None:
        epoch = _CLAUDE_EPOCH_RESET_RE.search(line)
        reset_time = (datetime.fromtimestamp(int(epoch.group(1))).isoformat() if epoch
                      else extract_reset_phrase(line) or (datetime.now() + timedelta(hours=3)).isoformat())
        return True, f"限流 / 额度耗尽 (重置时间: {reset_time})", reset_time
    if "overload" in found:
        iso_reset = (datetime.now() + timedelta(minutes=3)).isoformat()
        return True, "Anthropic 服务端负载过高 (Overloaded)", iso_reset
    line = found.get("rate")
    if line is not None:
        reset_time = extract_reset_phrase(line)
        if reset_time:
            return True, f"限流 / 额度耗尽 (重置时间: {reset_time})", reset_time
        iso_reset = (datetime.now() + timedelta(hours=3)).isoformat()
        return True, "API 并发或频次超限 (429)", iso_reset
    return False, "", None

def execute_claude_task(
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
    Dispatches task to Claude Code.
    If readonly=True, restricts the built-in tool set to read-only inspection (Read, Grep, Glob)
    under a non-bypass permission mode, so every other tool is denied without prompting.
    If repo_root is provided, wraps execution in bubblewrap with transparent repo_root bind-mount.
    """
    from makewand.health import load_status_cache, save_status_cache
    from makewand.sandbox import (
        is_bwrap_available,
        SandboxConfigError, ProviderStartupError,
        ProviderAccountChangedError,
        verify_writable_sandbox_or_authorized,
    )
    from makewand.git_helper import find_git_root
    # Untrusted repo enforcement
    if repo_trust == "untrusted":
        allow_network = False
        if not is_bwrap_available():
            return False, None, "不可信仓库 (--repo-trust=untrusted) 强制要求 Bubblewrap 物理沙箱隔离，未检测到 bwrap，拒绝执行"
        if not readonly:
            return False, None, "不可信仓库 (--repo-trust=untrusted) 仅允许只读审计与分析，禁止执行写入或修改任务"

    cache = load_status_cache()
    from makewand.config import has_subscription_configured
    from makewand.usage import note_dispatch_route

    relay = ProviderAPIRelay(
        "claude", color=COLOR_BLUE, prompt=prompt, model=model, tier=tier, stream=stream,
        timeout=timeout, role="reviewer" if readonly else "coder",
        repo_trust=repo_trust, readonly=readonly,
    )

    # If subscription is missing or limited, fallback to Anthropic API
    if not has_subscription_configured("claude"):
        if relay.available:
            return relay("[Makewand -> Claude] (纯 API 模式) 派发任务至 Anthropic Claude API...", cwd=cwd)
        return False, None, relay.unavailable("未找到 Claude CLI 订阅", "未配置 ANTHROPIC_API_KEY")

    # A live login failure is relayed like a quota limit (engine selection counts
    # needs_auth as usable only with an allowed API relay); otherwise the CLI runs.
    if cache.get("claude", {}).get("status") == "needs_auth" and relay.available:
        return relay("[Makewand -> Claude] 订阅登录失效，自动降级为 Anthropic API 模式接力执行...", cwd=cwd)

    if cache.get("claude", {}).get("status") == "limited":
        if relay.available:
            return relay("[Makewand -> Claude] 订阅额度受限，无缝自动降级为 Anthropic API 模式接力执行...", cwd=cwd)
        return False, None, relay.unavailable(f"Claude Code 当前额度受限: {cache['claude'].get('reason')}",
                                     "可配置 ANTHROPIC_API_KEY 作为备用 API 自动接力")

    # Normalize cwd and repo_root to ensure sandbox is never bypassed
    if not cwd:
        cwd = os.getcwd()
    cwd = os.path.abspath(cwd)

    if not repo_root:
        repo_root = find_git_root(cwd) or cwd
    repo_root = os.path.abspath(repo_root)

    # Fail-closed enforcement: if writable, sandbox is mandatory unless explicitly authorized
    host_auth_source = None
    if not readonly:
        ok, host_auth_source, err = verify_writable_sandbox_or_authorized("claude", repo_trust=repo_trust)
        if not ok:
            return False, None, err

    input_text = None
    if len(prompt.encode("utf-8")) > 32 * 1024:
        cmd = ["claude", "-p"]
        input_text = prompt
    else:
        cmd = ["claude", "-p", prompt]
    if readonly:
        # Read-only inspection must never run under a bypass mode: --allowedTools
        # only pre-approves tools and is meaningless with
        # --dangerously-skip-permissions. --tools limits the built-in tool set
        # itself, dontAsk denies anything not pre-approved (no prompt in -p mode),
        # project/local settings (repo-controlled hooks) and MCP servers are not
        # loaded, and no session transcript is persisted.
        cmd.extend([
            "--tools", CLAUDE_READONLY_TOOLS,
            "--allowedTools", CLAUDE_READONLY_TOOLS,
            "--permission-mode", "dontAsk",
            "--setting-sources", "user",
            "--strict-mcp-config",
            "--no-session-persistence",
        ])
    else:
        cmd.append("--dangerously-skip-permissions")

    from makewand.discovery import get_provider_model_tier, normalize_provider_effort
    resolved = get_provider_model_tier("claude", tier)
    target_model = model or resolved["model"]
    cmd.extend(["--model", target_model])
    target_effort = normalize_provider_effort("claude", effort or resolved.get("effort"))
    if target_effort and target_effort != "none":
        cmd.extend(["--effort", target_effort])

    from makewand.sandbox import provider_sandbox_launch, SandboxUnavailableError
    from makewand.execution_runtime import mark_provider_invocation
    try:
        with provider_sandbox_launch(
            "claude", cmd, cwd,
            readonly=readonly,
            repo_root=repo_root,
            repo_trust=repo_trust,
            allow_network=allow_network,
            host_auth_source=host_auth_source,
        ) as launch:
            log_desc = "只读解析任务 (工具只读约束)" if readonly else "代码任务 (无头权限自动穿透)"
            import sys
            print(c(f"[Makewand -> Claude] 派发{log_desc} (Tier: {tier})...", COLOR_BLUE), file=sys.stderr)
            note_dispatch_route("cli")
            mark_provider_invocation()
            code, out, err, ex = run_subprocess(
                launch.cmd,
                timeout=timeout,
                cwd=cwd,
                input_text=input_text,
                stream=stream,
                print_prefix=c("[Claude Live]", COLOR_BLUE),
                pass_fds=launch.pass_fds,
                extra_env=launch.env,
                separate_stderr=True,
            )
    except ProviderAccountChangedError as exc:
        from makewand.providers.base import ProcessExecutionError
        return False, None, ProcessExecutionError(str(exc), "UNKNOWN")
    except SandboxUnavailableError as exc:
        return False, None, str(exc)
    except SandboxConfigError as exc:
        if isinstance(exc, ProviderStartupError):
            return exc.as_result("claude", readonly)
        return False, None, f"Claude 沙箱构建失败，拒绝执行 (fail closed): {exc}"
    apart = stderr_captured_apart(err, stream)
    combined = f"{out}\n{err}" if apart else out

    if code == 0:
        return True, out, None

    if is_process_timeout(ex) or code < 0:
        return model_process_failure("claude", code, combined, err, ex, readonly)

    is_limited, reason, resets = parse_claude_quota(out, err if apart else None)
    if is_limited:
        from makewand.health import record_engine_limit
        record_engine_limit("claude", reason, resets)
        if relay.available:
            return relay(f"[Makewand -> Claude] 订阅触发限流 ({reason})，无缝切换为 Anthropic API Key 模式接力执行...", cwd=cwd)
        return False, None, relay.unavailable(f"Claude Code 执行中触发额度限制: {reason}", "可配置 ANTHROPIC_API_KEY 实现自动接力")

    return model_process_failure("claude", code, combined, err, ex, readonly)
