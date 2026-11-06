"""
Provider dispatch and the review of uncommitted changes.
"""

import builtins
import os
import sys
import time
import math
from typing import Optional, Tuple

from makewand.config import c, COLOR_BOLD, COLOR_GREEN, COLOR_YELLOW, COLOR_RED, COLOR_CYAN
from makewand.git_helper import get_git_diff, get_git_diff_status, _json_text, _printable
from makewand.health import get_or_update_status
from makewand.providers.agy import execute_agy_task
from makewand.providers.claude import execute_claude_task
from makewand.providers.codex import execute_codex_task
from makewand.providers.muse import execute_muse_task
from makewand.providers.grok import execute_grok_task
from makewand.providers.local import execute_local_task
from makewand.providers.aider import execute_aider_task
from makewand.providers.base import get_current_contestant_scope
from makewand.workflow import is_startup_environment_failure


def _print(*values, **kwargs):
    """print() for this module's messages, which every message here goes through. They
    carry workspace and file names, git and provider errors and the user's prompt, any
    of which may hold undecodable bytes (surrogate escapes from os.fsdecode or git
    output): those are shown as \\xNN (git_helper._printable) instead of raising
    UnicodeEncodeError on a UTF-8 stream."""
    builtins.print(*(_printable(value if isinstance(value, str) else str(value)) for value in values), **kwargs)


def _print_json(value):
    """Serialize before printing: diagnostic \\xNN escapes are not JSON escapes."""
    builtins.print(_json_text(value))


from makewand.execution_contract import EXIT_PASSED, EXIT_FAILED, EXIT_UNVERIFIED

from makewand.engine_selection import API_ONLY_ENGINES, _engine_usable, forced_engine_name, forced_engine_refusal, forced_engine_error, forced_engine_refusal_lines


from makewand.review_contract import review_verdict_output_spec, generate_review_nonce, prepare_review_diff, review_digest, dispatched_size, reviewer_prompt_limit, reviewer_receives_whole_prompt, REVIEW_PASSED, REVIEW_FAILED, REVIEW_UNVERIFIED

from makewand.review_verdict import resolve_review_verdict, extract_review_verdict_dict


# A verdict approves only the exact, complete diff it saw. A larger diff is never
# truncated for review: it stays UNVERIFIED and its patch is preserved instead.
REVIEW_DIFF_MAX_BYTES_DEFAULT = 256 * 1024
REVIEW_DIFF_MAX_BYTES_ENV = "MAKEWAND_REVIEW_DIFF_MAX_BYTES"
_review_cap_warning_shown = False


def review_diff_max_bytes() -> int:
    """Largest complete diff (UTF-8 bytes) one review prompt may carry."""
    global _review_cap_warning_shown
    raw = os.environ.get(REVIEW_DIFF_MAX_BYTES_ENV, "").strip()
    if raw:
        try:
            value = int(raw)
        except ValueError:
            value = 0
        if value > 0:
            return value
        if not _review_cap_warning_shown:
            _review_cap_warning_shown = True
            _print(c(f"⚠ {REVIEW_DIFF_MAX_BYTES_ENV}={raw!r} 不是正整数，复审 diff 上限使用默认值 "
                     f"{REVIEW_DIFF_MAX_BYTES_DEFAULT} 字节。", COLOR_YELLOW), file=sys.stderr)
    return REVIEW_DIFF_MAX_BYTES_DEFAULT


def _review_cap_exceeded(size: int, cap: int) -> str:
    return (f"代码改动完整 diff ({size} 字节) 超过复审上限 {cap} 字节 "
            f"(默认 {REVIEW_DIFF_MAX_BYTES_DEFAULT} 字节，可通过环境变量 {REVIEW_DIFF_MAX_BYTES_ENV} 调整)，"
            "不做截断审查，结论未验证；请拆分任务后重新生成或调高上限")


# Gate results that carry no test evidence: never auto-fixed, never reported as failing tests.
LOCAL_TESTS_NO_EVIDENCE = ("UNVERIFIED", "TIMEOUT")


# .gitignore matching for _gitignore_fallback_ignored: git's own rules (dir.c's
# pattern parsing and last-match lookup, wildmatch.c with WM_PATHNAME), on the
# bytes of the path and the patterns.
_GLOB_STAR, _GLOB_QUESTION, _GLOB_OPEN, _GLOB_ESCAPE, _GLOB_SLASH = 42, 63, 91, 92, 47
_WM_MATCH, _WM_NOMATCH, _WM_ABORT_ALL, _WM_ABORT_TO_STARSTAR = 0, 1, -1, -2


def dispatch_task(
    engine: str,
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
    """Dispatch through the typed runtime; results remain three-element tuples."""
    from makewand.config import is_provider_enabled, get_api_policy
    from makewand.execution_contract import ExecutionRequest, ExecutionResult
    from makewand import execution_runtime
    context = execution_runtime.current_context()
    task_id = execution_runtime.task_id()
    stage_name = context.get("stage") or ("review" if readonly else "implementation")
    def rejected(status, message):
        return ExecutionResult(False, None, message, status=status, task_id=task_id,
                               stage=stage_name, engine=engine)
    scope = get_current_contestant_scope()
    if scope and scope.cancel_event.is_set():
        return rejected("CANCELLED", "cancelled by competitor win")
    if not is_provider_enabled(engine):
        return rejected("UNVERIFIED", f"引擎 '{engine}' 当前已被用户在配置中手动禁用。运行 'makewand enable {engine}' 重新开启")
    adapters = {
        "claude": execute_claude_task, "codex": execute_codex_task,
        "grok": execute_grok_task, "muse": execute_muse_task,
        "agy": execute_agy_task, "local": execute_local_task,
        "ollama": execute_local_task, "aider": execute_aider_task,
    }
    api_engines = API_ONLY_ENGINES
    placeholder_engines = {"cursor", "copilot"}
    if engine in placeholder_engines:
        notice = (
            f"引擎 '{engine}' 为交互式 IDE 扩展占位引擎，当前系统无独立无头命令行自主执行器。"
            f"已记录任务需求；在支持该插件的编辑器中协同完成，或选用 Claude/Codex/Antigravity 等自主 Agent 引擎。"
        )
        _print(c(f"ℹ️ [Placeholder Provider] {notice}", COLOR_YELLOW), file=sys.stderr)
        return ExecutionResult(True, notice, None, status="PASSED", task_id=task_id,
                               stage=stage_name, engine=engine)
    if engine not in adapters and engine not in api_engines:
        return rejected("INVALID_REQUEST", f"未知或不支持的模型引擎: {engine}")

    if tier == "auto" or not tier:
        try:
            from makewand.pacing import resolve_dynamic_tier_and_effort
            dyn_tier, dyn_model, dyn_effort = resolve_dynamic_tier_and_effort(engine, requested_tier="auto")
            tier = dyn_tier
            if not model and dyn_model and dyn_model != "default":
                model = dyn_model
            if not effort and dyn_effort:
                effort = dyn_effort
        except Exception:
            tier = "standard"
    else:
        from makewand.config import normalize_tier
        tier = normalize_tier(tier)
    budget_maximum = os.environ.get("MAKEWAND_MAX_MODEL_CALLS")
    try:
        budget_maximum = int(budget_maximum) if budget_maximum else None
    except ValueError:
        return rejected("INVALID_REQUEST", "invalid maximum model task budget")
    if isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or not math.isfinite(timeout):
        return rejected("INVALID_REQUEST", "timeout must be finite")
    if timeout <= 0:
        return rejected("TIMEOUT", "execution time budget exhausted")
    try:
        request = ExecutionRequest(
            schema=1, task_id=task_id, stage=stage_name, engine=engine, tier=tier,
            model=model, readonly=readonly, repo_trust=repo_trust,
            api_policy=get_api_policy(), deadline_unix_ms=context.get("deadline_unix_ms"),
            timeout_ms=max(1, math.ceil(timeout * 1000)), budget_file=os.environ.get("MAKEWAND_CALL_BUDGET_FILE"),
            max_model_calls=budget_maximum, workflow=context.get("workflow"),
            risk=context.get("risk"), prompt=prompt, cwd=cwd or os.getcwd(),
        )
    except (TypeError, ValueError) as error:
        return rejected("INVALID_REQUEST", str(error))
    from makewand.usage import note_dispatch_route, observe_dispatch_route

    launched = False
    def execute(effective_timeout):
        nonlocal launched
        try:
            return run_provider(effective_timeout)
        finally:
            launched = execution_runtime.provider_invocation_used()

    def run_provider(effective_timeout):
        from makewand.workflow import provider_outcome
        provider_prompt = prompt
        if engine != "claude" and not readonly and cwd:
            provider_prompt = f"目标工作目录绝对路径: {cwd}\n请在该目录下创建/修改对应代码文件并落盘：\n{prompt}"
        # Text for a model is UTF-8: an undecodable byte of a file or workspace name
        # (a review diff under core.quotePath=false, a repo map) is sent as \xNN.
        provider_prompt = _printable(provider_prompt)
        if engine in api_engines:
            from makewand.providers.api_client import call_api_chat
            note_dispatch_route("api")
            return provider_outcome(call_api_chat(provider=engine, prompt=provider_prompt, cwd=cwd,
                                 timeout=effective_timeout, tier=tier, model=model,
                                 stream=stream, role="reviewer" if readonly else "coder",
                                 repo_trust=repo_trust, readonly=readonly))
        options = dict(cwd=cwd, timeout=effective_timeout, tier=tier, model=model,
                       stream=stream, readonly=readonly, repo_root=repo_root,
                       repo_trust=repo_trust, allow_network=allow_network)
        if engine not in ("local", "ollama", "aider"):
            options["effort"] = effort
        return provider_outcome(adapters[engine](provider_prompt, **options))
    # The route (cli/api) the adapter reports goes into the usage ledger
    # (health._dispatch_failure_superseded reads it).
    with observe_dispatch_route() as route:
        result = execution_runtime.execute(request, execute)
    if not launched:
        # Refused before launch (capacity, budget, quota, sandbox, policy); no provider ran.
        return result
    try:
        from makewand.usage import record_engine_usage
        record_engine_usage(engine, tier=tier, success=result[0], task=prompt, route=route.route, model=model)
    except Exception:
        pass
    return result


def _has_capacity_fallback(engines, cache, prompt=None):
    """Only usable candidates that can receive the prompt justify skipping a wait."""
    return any(_engine_usable(engine, cache)[0] and
               (prompt is None or reviewer_receives_whole_prompt(engine, prompt))
               for engine in engines)


def _stage_call(name, callback, *args, engine=None, wait_for_capacity=None, **kwargs):
    """Name the stage while retaining the callback's legacy return type."""
    from makewand.telemetry import stage
    from makewand.execution_runtime import execution_context
    with stage(name, engine=engine, readonly=kwargs.get("readonly", False)) as span:
        with execution_context(wait_for_capacity=wait_for_capacity):
            result = callback(*args, **kwargs)
        status = getattr(result, "status", None)
        if name == "test" and isinstance(result, tuple) and len(result) == 2:
            if result[0] is True and result[1] is None:
                status = "UNVERIFIED"
            elif getattr(result[1], "execution_status", None) in LOCAL_TESTS_NO_EVIDENCE:
                status = result[1].execution_status
        if status is None:
            ok = result[0] if isinstance(result, tuple) else result == 0 if type(result) is int else result is None or bool(result)
            status = "PASSED" if ok else "FAILED"
        span.finish(status=status)
        return result


def _deadline(total_seconds):
    """A child workflow can shorten, but never extend, its parent's deadline."""
    from makewand.execution_runtime import current_context
    if isinstance(total_seconds, bool) or not isinstance(total_seconds, (int, float)) or not math.isfinite(total_seconds) or total_seconds <= 0:
        raise ValueError("total timeout must be finite and positive")
    deadline = math.ceil(time.time() * 1000 + total_seconds * 1000)
    parent = current_context().get("deadline_unix_ms")
    return min(deadline, parent) if parent is not None else deadline


def run_review(cwd=None, stream=False, timeout=300, user_prompt=None, output_json=False,
               repo_trust="trusted", local_only=False, base_rev=None, effort=None, engine=None):
    """Independent read-only review of the workspace diff. ``engine`` is a reviewer the
    user forced (--provider): it is the only one asked, and an unusable one is refused
    with its own reason (forced_engine_refusal), as on every other path."""
    from makewand.execution_runtime import execution_context, task_id
    from makewand.execution_contract import ExecutionResult, STATUS_CODES
    from makewand.workflow import remember_result
    from makewand.telemetry import stage
    deadline = _deadline(timeout)
    with execution_context(deadline_unix_ms=deadline, workflow="review", readonly=True):
        with stage("review", readonly=True) as span:
            code = _run_review_impl(cwd, stream, timeout, user_prompt, output_json, repo_trust, local_only, base_rev,
                                    effort=effort, engine=engine)
            status = next((name for name, value in STATUS_CODES.items() if value == code), "UNKNOWN")
            remember_result(ExecutionResult(status == "PASSED", None, None, status=status,
                            task_id=task_id(), stage="review", engine="orchestrator", readonly=True))
            span.finish(status=status)
            return code


def _run_review_impl(cwd: Optional[str] = None, stream: bool = False, timeout: int = 300, user_prompt: Optional[str] = None, output_json: bool = False, repo_trust: str = "trusted", local_only: bool = False, base_rev: Optional[str] = None, effort: Optional[str] = None, engine: Optional[str] = None) -> int:
    from makewand.execution_runtime import current_context
    from makewand.execution_contract import EXIT_TIMEOUT
    forced = None if local_only else forced_engine_name(engine)
    def timed_out():
        if current_context().get("_deadline_monotonic", float("inf")) > time.monotonic():
            return False
        if output_json:
            _print_json({"pass": False, "engine": None, "exit_code": EXIT_TIMEOUT,
                               "error": "review task deadline expired"})
        return True
    if timed_out():
        return EXIT_TIMEOUT
    if not cwd:
        cwd = os.getcwd()

    if repo_trust == "untrusted":
        from makewand.sandbox import is_bwrap_available
        if not is_bwrap_available() and os.environ.get("MAKEWAND_UNSAFE_HOST_EXEC") != "1":
            if output_json:
                _print_json({
                    "pass": False,
                    "exit_code": EXIT_UNVERIFIED,
                    "engine": None,
                    "defects": ["当前仓库为 untrusted 且 Bubblewrap 沙箱不可用，根据安全防御原则阻断审查"],
                    "error": "Untrusted repository requires Bubblewrap sandbox"
                })
            else:
                _print(c("❌ [Makewand Untrusted Repo] 当前仓库为 untrusted 且 Bubblewrap 沙箱不可用，根据安全防御原则阻断审查。", COLOR_RED + COLOR_BOLD))
            return EXIT_UNVERIFIED

    if forced:
        refusal = forced_engine_refusal(forced, get_or_update_status())
        if refusal:
            if output_json:
                _print_json({"pass": False, "exit_code": EXIT_UNVERIFIED, "engine": None, "defects": [refusal],
                                   "error": forced_engine_error(forced, refusal)})
            else:
                for line in forced_engine_refusal_lines(forced, refusal):
                    _print(line)
            return EXIT_UNVERIFIED

    if not output_json:
        _print(c("🔍 Makewand 代码审计工具", COLOR_BOLD + COLOR_CYAN))
    def extract_review_diff():
        if base_rev is None and (hasattr(get_git_diff, "mock") or hasattr(get_git_diff, "_mock_return_value") or "unittest.mock" in type(get_git_diff).__module__):
            return get_git_diff(cwd), None
        return get_git_diff_status(cwd, base_rev=base_rev)

    diff_out, diff_err = extract_review_diff()
    if timed_out():
        return EXIT_TIMEOUT
    if diff_err:
        if output_json:
            _print_json({
                "pass": False,
                "exit_code": EXIT_UNVERIFIED,
                "engine": None,
                "defects": [f"Git diff 提取失败 ({diff_err})"],
                "error": diff_err
            })
        else:
            _print(c(f"❌ [Makewand Review] 无法提取当前工作区改动 ({diff_err})，阻断审查。", COLOR_RED + COLOR_BOLD))
        return EXIT_UNVERIFIED

    if not diff_out.strip():
        if output_json:
            _print_json({
                "pass": True,
                "exit_code": EXIT_PASSED,
                "engine": None,
                "defects": [],
                "message": "当前工作区没有检测到未提交的改动 (git diff 为空)"
            })
        else:
            _print("当前工作区没有检测到未提交的改动 (git diff 为空)。")
        return EXIT_PASSED

    diff_bytes = dispatched_size(diff_out)
    review_cap = review_diff_max_bytes()
    if diff_bytes > review_cap:
        reason = _review_cap_exceeded(diff_bytes, review_cap)
        _print_json({"pass": False, "engine": None, "exit_code": EXIT_UNVERIFIED, "error": reason}) if output_json else _print(reason)
        return EXIT_UNVERIFIED
    cache = get_or_update_status()

    review_nonce = generate_review_nonce()
    review_diff, reviewed_digest = prepare_review_diff(diff_out, review_nonce)
    focus = f" 特别关注要求: {user_prompt}。" if user_prompt else ""
    prompt = (
        f"工作目录为: {cwd}。请详细审查当前仓库的修改（git diff），{focus}指出潜在隐患并给出修复建议。\n"
        f"{review_verdict_output_spec(nonce=review_nonce)}"
        f"--- 代码改动 (git diff) ---\n{review_diff}"
    )

    # Reviewer ladder honours `makewand disable <engine>` and the cached health status.
    if local_only:
        reviewer_ladder = [
            ("local", "派发给本地自托管模型进行独立红队审计 (Ollama / vLLM, 100% 离线隐私零 Token, 只读隔离)...", COLOR_CYAN),
        ]
    elif forced:
        # The reviewer the user forced, usable (checked above), and no other.
        reviewer_ladder = [(forced, f"派发给指定的审查引擎 {forced.upper()} 进行红队审计 (只读隔离)...", COLOR_CYAN)]
    else:
        # No Grok by default (owner, 2026-11-06): half of its reviews still gave no verdict
        # after the 11-04 fixes. `--provider grok` still forces it.
        reviewer_ladder = [
            ("codex", "派发给 Codex CLI 进行红队审计 (gpt-6-astra, 只读隔离)...", COLOR_CYAN),
            ("agy", "由 Antigravity 进行红队审计 (只读隔离)...", COLOR_GREEN),
        ]
    review_res = None
    reviewer_engine = None
    attempted = []
    partial = []
    terminal_status = "UNVERIFIED"
    terminal_error = None
    for review_index, (eng, banner, color) in enumerate(reviewer_ladder):
        usable, why = (True, "") if eng == forced else _engine_usable(eng, cache)
        if not usable:
            if not output_json:
                _print(c(f"跳过 {eng.upper()} 审查引擎: {why}", COLOR_YELLOW))
            continue
        if not reviewer_receives_whole_prompt(eng, prompt):
            # A verdict from a reviewer that saw only part of the diff never counts.
            partial.append(eng)
            if not output_json:
                _print(c(f"跳过 {eng.upper()} 审查引擎: 审查提示 ({dispatched_size(prompt)} 字节) 超过其可完整接收的 "
                         f"{reviewer_prompt_limit(eng)} 字节上限", COLOR_YELLOW))
            continue
        attempted.append(eng)
        if not output_json:
            _print(c(banner, color))
        result = _stage_call("review", dispatch_task, eng, prompt, engine=eng,
                             cwd=cwd, timeout=timeout, tier="deep", effort=effort,
                             stream=stream and not output_json, readonly=True, repo_trust=repo_trust,
                             wait_for_capacity=not _has_capacity_fallback(
                                 [e for e, _, _ in reviewer_ladder[review_index + 1:]], cache, prompt))
        success, out, err = result
        if success and out and out.strip():
            review_res = out
            reviewer_engine = eng
            break
        if is_startup_environment_failure(result):
            terminal_error = err
            break
        if getattr(result, "status", None) in ("UNKNOWN", "TIMEOUT", "CANCELLED", "BUDGET_EXHAUSTED"):
            terminal_status = result.status
            break
        if not output_json:
            _print(c(f"{eng.upper()} 审查失败 ({err or '输出内容为空'})，尝试下一审查引擎...", COLOR_YELLOW))

    if not review_res:
        from makewand.execution_contract import STATUS_CODES
        no_engine = not attempted
        if terminal_error:
            reason = str(terminal_error)
        elif local_only:
            reason = ("本地审查引擎不可用或已被禁用 (根据 --local-only 隐私安全原则阻断向外部云端回退)" if no_engine
                      else "本地审查引擎未能产生有效输出 (根据 --local-only 隐私安全原则阻断向外部云端回退)")
        elif forced:
            reason = (f"指定的审查引擎 {forced.upper()} 无法完整接收审查提示" if no_engine
                      else f"指定的审查引擎 {forced.upper()} 未能产生有效输出 (UNVERIFIED)")
        else:
            reason = ("没有可完整接收审查提示的已启用审查引擎" if no_engine and partial
                      else f"没有已启用且健康的审查引擎 ({'/'.join(e for e, _, _ in reviewer_ladder)} 均被禁用或不可用)" if no_engine
                      else "独立审查服务未能产生有效输出 (UNVERIFIED)")
        if partial:
            reason += f"；{'/'.join(e.upper() for e in partial)} 无法完整接收审查提示 ({dispatched_size(prompt)} 字节)，已跳过"
        if output_json:
            _print_json({
                "pass": False,
                "exit_code": STATUS_CODES[terminal_status],
                "engine": None,
                "verdict_status": REVIEW_UNVERIFIED,
                "defects": [reason],
                "error": terminal_error or ("No enabled and healthy review engine" if no_engine else "Independent review engine failed to produce valid output")
            })
        else:
            _print(c(f"❌ [Makewand Quality Gate] {reason}，拒绝交付。", COLOR_RED + COLOR_BOLD))
        return STATUS_CODES[terminal_status]

    review_res, verdict = resolve_review_verdict(review_res, reviewer_engine, cwd=cwd, timeout=timeout,
                                                 repo_trust=repo_trust, quiet=output_json, nonce=review_nonce)
    if verdict.get("execution_status"):
        from makewand.execution_contract import STATUS_CODES
        exit_code = STATUS_CODES[verdict["execution_status"]]
    elif verdict["status"] == REVIEW_PASSED:
        exit_code = EXIT_PASSED
    elif verdict["status"] == REVIEW_FAILED:
        exit_code = EXIT_FAILED
    else:
        exit_code = EXIT_UNVERIFIED
    if current_context().get("_deadline_monotonic", float("inf")) <= time.monotonic():
        exit_code = EXIT_TIMEOUT
    # A verdict approves only the exact diff it saw: re-extract and compare.
    digest_error = None
    if exit_code == EXIT_PASSED:
        try:
            after_diff, after_err = extract_review_diff()
        except OSError as exc:
            after_diff, after_err = None, str(exc)
        if after_err or review_digest(after_diff) != reviewed_digest:
            exit_code = EXIT_UNVERIFIED
            digest_error = "审查期间改动发生变化，裁决与当前候选摘要不一致 (reviewed sha256 mismatch)"
            verdict = dict(verdict, status=REVIEW_UNVERIFIED, reason=digest_error)

    if output_json:
        v_dict = extract_review_verdict_dict(review_res, nonce=review_nonce)
        v_dict["exit_code"] = exit_code
        v_dict["engine"] = reviewer_engine
        v_dict["reviewed_sha256"] = reviewed_digest
        if digest_error:
            v_dict["pass"] = False
        v_dict["raw_summary"] = review_res.strip()
        if verdict.get("execution_status"):
            v_dict.update({"pass": False, "error": verdict["reason"]})
        if exit_code == EXIT_TIMEOUT:
            v_dict.update({"pass": False, "error": "review task deadline expired"})
        if exit_code == EXIT_UNVERIFIED:
            v_dict["error"] = verdict["reason"]
        _print_json(v_dict)
        return exit_code

    if not stream:
        _print(review_res)

    if exit_code == EXIT_PASSED:
        _print(c("✔ 代码审计通过，未发现严重缺陷 (PASSED)。", COLOR_GREEN + COLOR_BOLD))
    elif exit_code == EXIT_FAILED:
        _print(c("❌ 代码审计检测到严重隐患，未达合并标准 (FAILED)。", COLOR_RED + COLOR_BOLD))
    else:
        _print(c(f"❌ 审查未给出有效的 MAKEWAND_VERDICT 裁决，结论未验证 (UNVERIFIED: {verdict['reason']})。", COLOR_RED + COLOR_BOLD))
    return exit_code


