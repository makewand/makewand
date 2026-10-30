"""
Antigravity CLI provider adapter (Google AI Pro / Gemini 3.8 Flash & Pro).
"""

import os
import re
from functools import lru_cache
from typing import Tuple, Optional
from makewand.config import c, COLOR_GREEN
from makewand.providers.base import ProviderAPIRelay, run_subprocess, model_process_failure, is_process_timeout, stderr_captured_apart

# Quota, region and login failures count only where agy prints them itself: one
# of agy's messages (health.CLI_MESSAGE_CATALOG["agy"]) on its own lines, which
# are stderr when the streams are captured apart (stdout carries the answer);
# in a merged stream only the error report agy ends with counts. The glog
# diagnostics agy writes to stderr ("I1012 08:11:57.843790  1 file.go:403]
# ...", entries may continue over lines) are never a verdict: every start logs
# "You are not logged into Antigravity" before silent keyring auth succeeds,
# and transient retries log 429s.
#
# agy's own status lines, notices and argument rejections are catalog data too
# (cli_message_catalog.json providers.agy, format strings of the agy binary):
# every read-only dispatch passes --mode plan with --disable-slash-commands, so
# agy may warn about either before it works.
_AGY_AUTH_OK_RE = re.compile(r"authenticated (?:successfully|via )", re.IGNORECASE)
# The pre-authentication notice every start prints; moot once silent auth succeeded.
_AGY_STARTUP_AUTH_NOTICE_RE = re.compile(
    r"^(?:you are not logged into antigravity\.?|print mode: not authenticated, trying silent auth)$", re.IGNORECASE)


@lru_cache(maxsize=None)
def _agy_catalog_regex(key: str):
    """One of catalog providers.agy's line lists as a single regex (None when empty)."""
    from makewand.health import cli_catalog_entry, cli_catalog_regex
    value = cli_catalog_entry("agy").get(key) or ()
    patterns = (value,) if isinstance(value, str) else tuple(value)
    return cli_catalog_regex("|".join("(?:%s)" % pattern for pattern in patterns)) if patterns else None


def _agy_catalog_match(key: str, line: str) -> bool:
    regex = _agy_catalog_regex(key)
    return bool(regex is not None and regex.match(line))


def _agy_own_line(raw: str, *, any_warning: bool = False) -> bool:
    """A status line or notice agy prints itself (neither answer nor error).

    With `any_warning` every "warning: ..." line counts too (agy has generic
    "warning: %s" formats); only for agy's own lines, never for the answer.
    """
    from makewand.health import normalize_cli_line
    line = normalize_cli_line(raw)
    return (_agy_catalog_match("status_lines", line) or _agy_catalog_match("notices", line)
            or (any_warning and _agy_catalog_match("generic_notice", line)))


def _agy_user_facing(output: str) -> str:
    """agy's output without its diagnostics: glog entries (with their continuation
    lines), status lines and notices (any "warning: ..." line), ANSI stripped."""
    from makewand.health import glog_entry_lines, _ANSI_ESCAPE_RE
    lines = (output or "").splitlines()
    skipped = glog_entry_lines(lines)
    kept = [_ANSI_ESCAPE_RE.sub("", raw).rstrip() for index, raw in enumerate(lines)
            if index not in skipped and not _agy_own_line(raw, any_warning=True)]
    return "\n".join(kept).strip("\n")


def _agy_invocation_rejection(own: str) -> Optional[str]:
    """agy's argument rejection, if it is agy's first own line (`own` from _agy_user_facing).

    agy rejects its arguments before it runs anything: error: invalid model
    selection (--model "gemini-3.1-pro" --effort "max"): gemini-3.1-pro has no
    "max" effort (available: low, high), or cobra's "Error: unknown flag:
    --effort". The same words in the answer or in a tool's output ("tar: unknown
    option", "Error: unknown flag: --json -- ...") are not an invocation error.
    """
    from makewand.health import normalize_cli_line
    first = normalize_cli_line(next((line for line in (own or "").splitlines() if line.strip()), ""))
    return first if _agy_catalog_match("invalid_invocation", first) else None


def _agy_stream_answer(output: str) -> str:
    """The answer in agy's merged stream-mode output (stderr merged into stdout).

    Only lines agy writes itself are removed, by their exact shape: glog
    headers with the JSON body or the one value line they log, status lines and
    agy's notices. Everything else stays as printed, including the model's code
    right after a log line and its own "Warning: ..." prose.
    """
    from makewand.health import glog_entry_lines
    lines = (output or "").splitlines()
    skipped = glog_entry_lines(lines, continuation="value")
    return "\n".join(raw for index, raw in enumerate(lines) if index not in skipped and not _agy_own_line(raw))


def parse_agy_quota(output: str, stderr: Optional[str] = None) -> Tuple[bool, str, Optional[str]]:
    """Quota refusals agy prints itself; `stderr` is passed when the streams were captured apart."""
    from makewand.health import find_cli_message
    if find_cli_message("agy", ("quota",), output, stderr) is not None:
        return True, "Google AI 配额暂时耗尽", "待重置"
    return False, "", None


def _without_startup_notice(text: Optional[str]) -> Optional[str]:
    from makewand.health import normalize_cli_line
    if text is None:
        return None
    return "\n".join(line for line in text.splitlines()
                     if not _AGY_STARTUP_AUTH_NOTICE_RE.match(normalize_cli_line(line)))


def classify_agy_failure(output: str, stderr: Optional[str] = None) -> Optional[Tuple[str, str, Optional[float]]]:
    """
    Classifies a non-quota agy failure that says something about the provider
    itself (not about the task). Returns (status, reason, ttl_seconds) or None.
    `stderr` is passed when the streams were captured apart.
    """
    from makewand.health import find_cli_messages, cli_own_output
    if _AGY_AUTH_OK_RE.search(cli_own_output("agy", output, stderr)):
        # Only the start-up notice is moot once silent authentication succeeded;
        # a backend rejection after the keyring load still is a login failure.
        output, stderr = _without_startup_notice(output), _without_startup_notice(stderr)
    found = find_cli_messages("agy", ("region", "auth"), output, stderr)
    if "region" in found:
        from makewand.health import REGION_BLOCK_TTL_SECONDS
        return ("error",
                "Antigravity 在当前地区/账号不可用 (真实派发返回地区限制)，暂停派发并在有效期后自动复查",
                REGION_BLOCK_TTL_SECONDS)
    if "auth" in found:
        from makewand.health import get_reauth_hint
        return "needs_auth", f"Antigravity 认证失败 (真实派发) · {get_reauth_hint('agy')}", None
    return None

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
    from makewand.sandbox import (
        is_bwrap_available,
        verify_writable_sandbox_or_authorized,
    )
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
    from makewand.config import has_subscription_configured
    from makewand.usage import note_dispatch_route

    relay = ProviderAPIRelay(
        "agy", color=COLOR_GREEN, prompt=prompt, model=model, tier=tier, stream=stream,
        timeout=timeout, role="reviewer" if readonly else "coder",
        repo_trust=repo_trust, readonly=readonly,
    )

    # If subscription is missing or limited, fallback to Gemini API
    if not has_subscription_configured("agy"):
        if relay.available:
            return relay("[Makewand -> Antigravity] (纯 API 模式) 派发任务至 Google Gemini API...", cwd=cwd)
        return False, None, relay.unavailable("未找到 agy CLI 订阅", "未配置 GEMINI_API_KEY")

    cache = load_status_cache()
    if cache.get("agy", {}).get("status") == "needs_auth" and relay.available:
        return relay("[Makewand -> Antigravity] 订阅当前受限，无缝自动降级为 Gemini API 模式接力执行...", cwd=cwd)

    if cache.get("agy", {}).get("status") == "limited":
        if relay.available:
            return relay("[Makewand -> Antigravity] 订阅当前受限，无缝自动降级为 Gemini API 模式接力执行...", cwd=cwd)
        from makewand.model_quota import refuse_before_launch
        return refuse_before_launch(relay.unavailable(f"Antigravity 当前不可用: {cache['agy'].get('reason')}", "可配置 GEMINI_API_KEY 作为备用 API 自动接力"))

    # Fail-closed enforcement: if writable, sandbox is mandatory unless explicitly authorized
    host_auth_source = None
    if not readonly:
        ok, host_auth_source, err = verify_writable_sandbox_or_authorized("agy", repo_trust=repo_trust)
        if not ok:
            return False, None, err

    p_file = None
    p_dir = None
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
            import tempfile
            p_dir = tempfile.mkdtemp(prefix="makewand-agy-")
            p_file = os.path.join(p_dir, "prompt.txt")
            with open(p_file, "w", encoding="utf-8") as pf:
                pf.write(final_prompt)
            prompt_instruction = f"请读取并完整执行任务文件 {p_file} 中所指定的任务要求与代码规范："
            cmd = ["agy", "-p", prompt_instruction] + base_flags
        else:
            cmd = ["agy", "-p", final_prompt] + base_flags

        from makewand.discovery import get_provider_model_tier, resolve_agy_model, _discover_local_agy_models
        resolved = get_provider_model_tier("agy", tier)
        target_model = model or resolved.get("model")
        requested_effort = effort or resolved.get("effort")
        if target_model and target_model != "default":
            try:
                _avail = _discover_local_agy_models().models
            except Exception:
                _avail = None
            final_model, final_effort = resolve_agy_model(target_model, requested_effort, _avail)
            cmd.extend(["--model", final_model])
            target_model = final_model
            if final_effort and final_effort != "none":
                cmd.extend(["--effort", final_effort])
        else:
            from makewand.discovery import normalize_provider_effort
            target_effort = normalize_provider_effort("agy", requested_effort)
            if target_effort and target_effort != "none":
                cmd.extend(["--effort", target_effort])

        from makewand.model_quota import before_cli_launch
        quota_refusal = before_cli_launch('agy', target_model, relay=relay, cwd=cwd)
        if quota_refusal is not None:
            return quota_refusal

        from makewand.sandbox import provider_sandbox_launch, SandboxUnavailableError, SandboxConfigError, ProviderStartupError
        from makewand.execution_runtime import mark_provider_invocation
        try:
            with provider_sandbox_launch(
                "agy", cmd, cwd,
                readonly=readonly,
                repo_root=repo_root,
                repo_trust=repo_trust,
                allow_network=allow_network,
                extra_ro_binds=[p_file] if p_file else None,
                host_auth_source=host_auth_source,
            ) as launch:
                log_desc = "只读解析任务 (禁止写操作)" if readonly else "架构/兜底任务 (权限自动穿透)"
                import sys
                print(c(f"[Makewand -> Antigravity] 派发{log_desc} (Tier: {tier})...", COLOR_GREEN), file=sys.stderr)
                note_dispatch_route("cli")
                mark_provider_invocation()
                code, out, err, ex = run_subprocess(
                    launch.cmd,
                    timeout=timeout,
                    cwd=cwd,
                    stream=stream,
                    print_prefix=c("[AGY Live]", COLOR_GREEN),
                    pass_fds=launch.pass_fds,
                    extra_env=launch.env,
                    separate_stderr=True,
                )
        except SandboxUnavailableError as exc:
            return False, None, str(exc)
        except SandboxConfigError as exc:
            if isinstance(exc, ProviderStartupError):
                return exc.as_result("agy", readonly)
            return False, None, f"Antigravity 沙箱构建失败，拒绝执行 (fail closed): {exc}"
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
    # agy's own lines: stderr, or the merged stream when the runner merged stderr into stdout.
    apart = stderr_captured_apart(err, stream)
    combined = f"{out}\n{err}" if apart else out
    own_err = err if apart else None

    if code == 0:
        # The answer is stdout as printed (a merged stream without the lines agy
        # writes itself); stderr never is. A run without an answer failed, and
        # so did a merged stream left with nothing but agy's own notices: its
        # generic "Warning: ..." lines cannot be told from the answer's there,
        # so they stay in an answer but never are one.
        answer = ((out if apart else _agy_stream_answer(out)) or "").strip()
        if answer and (apart or _agy_user_facing(answer).strip()):
            return True, answer, None
        return False, None, "Antigravity 执行完成但未能产生有效输出内容"

    if is_process_timeout(ex) or code < 0:
        return model_process_failure("agy", code, combined, err, ex, readonly)

    rejection = _agy_invocation_rejection(_agy_user_facing(err if apart else out))
    if rejection:
        # The CLI rejected its arguments before running anything: a known,
        # side-effect-free failure, so the caller may try the next provider.
        from makewand.execution_contract import ExecutionResult
        return ExecutionResult(False, combined, f"Antigravity 拒绝调用参数: {rejection}", status="FAILED", engine="agy",
                               readonly=readonly, outcome_known=True, error_kind="provider_configuration")

    is_limited, reason, resets_at = parse_agy_quota(out, own_err)
    if not is_limited:
        failure = classify_agy_failure(out, own_err)
        if failure:
            status, fail_reason, ttl = failure
            from makewand.health import record_engine_failure
            try:
                record_engine_failure("agy", status, fail_reason, ttl_seconds=ttl)
            except Exception:
                pass
            return False, combined, fail_reason
    if is_limited:
        record_engine_limit("agy", reason, resets_at, model=target_model)
        if relay.available:
            return relay(f"[Makewand -> Antigravity] 订阅触发配额限制 ({reason})，无缝切换为 Gemini API Key 模式接力执行...", cwd=cwd)
        return False, None, relay.unavailable(f"Antigravity 配额受限: {reason}", "可配置 GEMINI_API_KEY 实现自动接力")
    return model_process_failure("agy", code, combined, err, ex, readonly)
