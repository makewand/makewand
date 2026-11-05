"""
Grok Build CLI provider adapter (xAI subscription / Grok Build interactive coding agent).
"""

import json
import os
import re
from typing import Tuple, Optional
from makewand.config import c, COLOR_YELLOW
from makewand.execution_contract import ExecutionResult
from makewand.providers.base import ProviderAPIRelay, run_subprocess, model_process_failure, is_process_timeout, stderr_captured_apart


def parse_grok_quota(output: str, stderr: Optional[str] = None) -> Tuple[bool, str, Optional[str]]:
    """
    Login and quota / rate-limit refusals Grok Build prints itself
    (health.CLI_MESSAGE_CATALOG["grok"]). `stderr` is passed when the streams
    were captured apart: grok -p then writes the answer to stdout and its own
    errors to stderr. Otherwise `output` is the merged text.
    """
    from makewand.health import find_cli_messages, extract_reset_phrase
    found = find_cli_messages("grok", ("auth", "quota", "rate"), output, stderr)
    if "auth" in found:
        return True, "未登录或需配置凭据 (请运行 'grok' 登录或在环境变量中配置 XAI_API_KEY)", "需登录授权"

    line = next((line for kind, line in found.items() if kind != "auth"), None)
    if line is not None:
        reset_time = extract_reset_phrase(line)
        if reset_time:
            return True, f"xAI 订阅额度耗尽或频次受限 (429 / {reset_time})", reset_time
        return True, "xAI 订阅额度耗尽或频次受限 (429)", "待重置"

    return False, "", None

# Grok Build asks before any shell command outside its built-in read-only list.
# Nobody can answer a headless run: the turn ends with stopReason "cancelled" and
# exit code 0. An allow rule only counts when every segment of a pipeline matches
# one, so the built-in commands need rules too, and those rules skip the built-in
# list's own exclusions: the deny rules restore them. Anything that can run other
# programs or write (sed, find, awk, python, xargs, tee) stays out. The sandbox
# remains the read-only boundary. A rule without a trailing space is a bare prefix,
# so only names that no other program starts with get the argument-free form.
# A bare rule's deny keeps it from approving a longer name (git logattack runs git-logattack).
# GNU and Git long options may be abbreviated, so a deny rule names a short unique form.
# A textconv or external diff driver set in Git config has no flag to match: Grok
# documents that it prompts despite these rules, which cancels the turn as above.
_READONLY_BARE_COMMANDS = (
    "pwd", "sort", "uniq", "sha256sum", "sha512sum", "sha1sum", "md5sum",
    "git status", "git branch", "git log", "git diff", "git ls-files", "git show", "git rev-parse",
    "git blame", "git describe", "git merge-base", "git shortlog", "git check-ignore", "git check-attr",
    "git cat-file", "git ls-tree", "git for-each-ref", "git rev-list", "git name-rev", "git count-objects",
)
_READONLY_ALLOW_RULES = tuple(f"Bash({name} *)" for name in (
    "ls", "cat", "date", "ps", "head", "tail", "wc", "sort", "uniq", "tr", "cut", "grep", "rg",
    "sha256sum", "sha512sum", "sha1sum", "md5sum", "stat", "nl", "diff", "cmp", "comm",
    "basename", "dirname", "realpath", "readlink", "echo", "printf", "test", "seq", "du", "df", "which",
)) + tuple(f"Bash({name})" for name in _READONLY_BARE_COMMANDS)
_READONLY_DENY_RULES = tuple(f"Bash({name}[a-zA-Z0-9_.-]*)" for name in _READONLY_BARE_COMMANDS) + (
    "Bash(git difftool*)", "Bash(rg *--pre*)", "Bash(rg *--hostname-bin*)", "Bash(sort *--co*)",
    "Bash(sort *-o*)", "Bash(sort *-[a-zA-Z]o*)", "Bash(sort *-[a-zA-Z][a-zA-Z]o*)", "Bash(sort *-[a-zA-Z][a-zA-Z][a-zA-Z]o*)",
    "Bash(uniq * [a-zA-Z0-9_./~]*)",
    "Bash(git * --ext*)", "Bash(git * --ou*)", "Bash(git * --te*)", "Bash(git * --fil*)",
    "Bash(git branch [a-zA-Z0-9_]*)", "Bash(git branch * [a-zA-Z0-9_]*)", "Bash(git branch -[dDmMcCfut]*)", "Bash(git branch * -[dDmMcCfut]*)",
    "Bash(git branch -[a-zA-Z][dDmMcCfut]*)", "Bash(git branch * -[a-zA-Z][dDmMcCfut]*)", "Bash(git branch *--d*)", "Bash(git branch *--mo*)",
    "Bash(git branch *--cop*)", "Bash(git branch *--forc*)", "Bash(git branch *--t*)", "Bash(git branch *--no-t*)",
    "Bash(git branch *--se*)", "Bash(git branch *--u*)", "Bash(git branch *--e*)", "Bash(git branch *--cr*)",
)
_READONLY_RULES = (
    "This run is unattended. A shell command that needs approval cancels the whole run and loses your "
    "answer. Use read_file, grep and list_dir to read files. In the shell use only git read commands, "
    "ls, cat, head, tail, wc, sort, uniq, tr, cut, grep, rg, the sha*sum and md5sum commands, stat, "
    "nl, diff, cmp, comm, basename, dirname, realpath, readlink, echo, printf, test, seq, du, df and "
    "which, joined only by pipes, && or ;. Do not use sed, find, awk, python, xargs or tee, or subshells, "
    "command substitution, redirection or background jobs."
)
# Reviews of a large repository commonly take 40 to 60 turns; the run's timeout still bounds it.
_READONLY_MAX_TURNS = 80
# Grok's own line when the limit stops a run with a nonzero exit.
_MAX_TURNS_ERROR = re.compile(r"^error: max turns reached\s*$", re.IGNORECASE | re.MULTILINE)
# Exit code 0 with one of these is not a finished answer.
_INCOMPLETE_STOP_REASONS = frozenset(("cancelled", "refusal", "max_tokens", "max_turn_requests"))


def _read_json_reply(output):
    """(text, stopReason) of Grok's JSON object; other output is returned as it is."""
    try:
        reply = json.loads(output)
    except (TypeError, ValueError):
        return output, None
    if not isinstance(reply, dict):
        return output, None
    text, stop = reply.get("text"), reply.get("stopReason")
    return text if isinstance(text, str) else output, stop if isinstance(stop, str) else None


def _read_json_error(output):
    """The message of Grok's JSON error object, or None for any other output."""
    try:
        reply = json.loads(output)
    except (TypeError, ValueError):
        return None
    message = reply.get("message") if isinstance(reply, dict) else None
    return message if isinstance(message, str) else None


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
    If readonly=True, enforces --permission-mode plan and reports a turn that did not finish.
    If repo_root is provided, wraps execution in bubblewrap with transparent repo_root bind-mount.
    """
    from makewand.health import load_status_cache, record_engine_limit
    from makewand.sandbox import (
        is_bwrap_available,
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
        "grok", color=COLOR_YELLOW, prompt=prompt, model=model, tier=tier, stream=stream,
        timeout=timeout, role="reviewer" if readonly else "coder",
        repo_trust=repo_trust, readonly=readonly,
    )

    # If subscription is missing or limited, fallback to xAI API
    if not has_subscription_configured("grok"):
        if relay.available:
            return relay("[Makewand -> Grok] (纯 API 模式) 派发任务至 xAI API...", cwd=cwd)
        return False, None, relay.unavailable("未找到 Grok Build CLI 订阅", "未配置 XAI_API_KEY")

    if cache.get("grok", {}).get("status") == "needs_auth" and relay.available:
        return relay("[Makewand -> Grok] 订阅当前受限，无缝自动降级为 xAI API 模式接力执行...", cwd=cwd)

    if cache.get("grok", {}).get("status") == "limited":
        if relay.available:
            return relay("[Makewand -> Grok] 订阅当前受限，无缝自动降级为 xAI API 模式接力执行...", cwd=cwd)
        from makewand.model_quota import refuse_before_launch
        return refuse_before_launch(relay.unavailable(f"Grok Build 当前不可用: {cache['grok'].get('reason')}", "可配置 XAI_API_KEY 作为备用 API 自动接力"))

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
        ok, host_auth_source, err = verify_writable_sandbox_or_authorized("grok", repo_trust=repo_trust)
        if not ok:
            return False, None, err

    p_file = None
    p_dir = None
    try:
        # Streaming prints as it arrives, so only a captured reply can carry the stop reason.
        json_reply = readonly and not stream
        output_format = "json" if json_reply else "plain"
        if len(prompt.encode("utf-8")) > 32 * 1024:
            import tempfile
            p_dir = tempfile.mkdtemp(prefix="makewand-grok-")
            p_file = os.path.join(p_dir, "prompt.txt")
            with open(p_file, "w", encoding="utf-8") as pf:
                pf.write(prompt)
            cmd = ["grok", "--prompt-file", str(p_file), "--output-format", output_format]
        else:
            cmd = ["grok", "-p", prompt, "--output-format", output_format]

        if readonly:
            cmd.extend(["--permission-mode", "plan", "--max-turns", str(_READONLY_MAX_TURNS), "--rules", _READONLY_RULES])
            for rule in _READONLY_ALLOW_RULES:
                cmd.extend(["--allow", rule])
            for rule in _READONLY_DENY_RULES:
                cmd.extend(["--deny", rule])
        else:
            cmd.extend(["--always-approve", "--permission-mode", "bypassPermissions"])

        if cwd:
            cmd.extend(["--cwd", str(cwd)])

        from makewand.discovery import get_provider_model_tier, normalize_provider_effort
        resolved = get_provider_model_tier("grok", tier)
        target_model = model or resolved["model"]
        cmd.extend(["--model", target_model])
        target_effort = normalize_provider_effort("grok", effort or resolved.get("effort"))
        if target_effort and target_effort != "none":
            cmd.extend(["--reasoning-effort", target_effort])

        from makewand.model_quota import before_cli_launch
        quota_refusal = before_cli_launch('grok', target_model, relay=relay, cwd=cwd)
        if quota_refusal is not None:
            return quota_refusal

        from makewand.sandbox import provider_sandbox_launch, SandboxUnavailableError, SandboxConfigError, ProviderStartupError
        from makewand.execution_runtime import mark_provider_invocation
        try:
            with provider_sandbox_launch(
                "grok", cmd, cwd,
                readonly=readonly,
                repo_root=repo_root,
                repo_trust=repo_trust,
                allow_network=allow_network,
                extra_ro_binds=[p_file] if p_file else None,
                host_auth_source=host_auth_source,
            ) as launch:
                log_desc = "只读解析/审查任务 (Plan 模式)" if readonly else "代码任务 (自动审批执行)"
                import sys
                print(c(f"[Makewand -> Grok] 派发{log_desc} (Tier: {tier}, xAI Provider)...", COLOR_YELLOW), file=sys.stderr)
                note_dispatch_route("cli")
                mark_provider_invocation()
                code, out, err, ex = run_subprocess(
                    launch.cmd,
                    timeout=timeout,
                    cwd=cwd,
                    stream=stream,
                    print_prefix=c("[Grok Live]", COLOR_YELLOW),
                    pass_fds=launch.pass_fds,
                    extra_env=launch.env,
                    separate_stderr=True,
                )
        except SandboxUnavailableError as exc:
            return False, None, str(exc)
        except SandboxConfigError as exc:
            if isinstance(exc, ProviderStartupError):
                return exc.as_result("grok", readonly)
            return False, None, f"Grok 沙箱构建失败，拒绝执行 (fail closed): {exc}"
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
    # Grok Build's own lines are stderr, or the merged stream when the runner
    # merged stderr into stdout.
    apart = stderr_captured_apart(err, stream)
    own_lines = err
    # In JSON mode the error object on stdout is Grok's own line, not the model's answer.
    message = _read_json_error(out) if json_reply and code != 0 else None
    if message is not None:
        out, own_lines = message, f"{message}\n{err}"
    combined = f"{out}\n{err}" if apart else out

    if code == 0:
        if not json_reply:
            return True, out, None
        text, stop = _read_json_reply(out)
        if stop in _INCOMPLETE_STOP_REASONS:
            return ExecutionResult(
                False, text, f"Grok 未完成本轮（stopReason={stop}）；cancelled 通常是命令需要审批而无人应答",
                status="FAILED", engine="grok", readonly=readonly, outcome_known=True, error_kind="incomplete_turn")
        return True, text, None

    if is_process_timeout(ex) or code < 0:
        return model_process_failure("grok", code, combined, err, ex, readonly)

    if apart and _MAX_TURNS_ERROR.search(err or ""):
        # The model ran and stopped at a known limit, so the outcome is not in doubt.
        return ExecutionResult(
            False, out, f"Grok 未完成本轮（已到 --max-turns {_READONLY_MAX_TURNS} 上限）",
            status="FAILED", engine="grok", readonly=readonly, outcome_known=True, error_kind="incomplete_turn")

    is_limited, reason, resets = parse_grok_quota(out, own_lines if apart else None)
    if is_limited:
        record_engine_limit("grok", reason, resets, model=target_model)
        if relay.available:
            return relay(f"[Makewand -> Grok] 订阅触发限制 ({reason})，无缝切换为 xAI API Key 模式接力执行...", cwd=cwd)
        return False, None, relay.unavailable(f"Grok Build 执行中检测到限制: {reason}", "可配置 XAI_API_KEY 实现自动接力")

    return model_process_failure("grok", code, combined, err, ex, readonly)
