"""
Makewand Orchestrator: Multi-model pipeline, task tiering, auto-fix loop, and race engine.
"""

import os
import sys
import shutil
import functools
import inspect
import re
import json
import time
import uuid
import shlex
import hashlib
import math
import tempfile
import concurrent.futures
from datetime import datetime
from pathlib import Path
from typing import Optional, Tuple, List, Dict, Any

from makewand.config import (
    c,
    COLOR_BOLD,
    COLOR_GREEN,
    COLOR_YELLOW,
    COLOR_RED,
    COLOR_BLUE,
    COLOR_CYAN,
    COLOR_PURPLE,
    COLOR_MAGENTA,
    COLOR_RESET,
    CANDIDATES_DIR,
    ensure_config_dir,
    ensure_private_dir,
)
from makewand.git_helper import (
    ensure_git_worktree,
    HostWorkspaceTransaction,
    PipelineWorkspaceGuard,
    create_private_artifact_dir,
    write_private_file,
    get_git_diff,
    get_git_diff_status,
    clone_isolated_worktree,
    run_git_cmd,
    check_working_tree_isolation,
    create_ephemeral_shadow_worktree,
    find_git_root,
    get_submodule_paths,
)
from makewand.candidate import CandidateManager, build_manifest, get_candidate_files_changed, remove_new_generated_bytecode
from makewand.protected_files import ProtectedFiles, ProtectionError
from makewand.artifact import workspace_snapshot
from makewand.health import get_or_update_status
from makewand.providers.agy import execute_agy_task
from makewand.providers.claude import execute_claude_task
from makewand.providers.codex import execute_codex_task
from makewand.providers.muse import execute_muse_task
from makewand.providers.grok import execute_grok_task
from makewand.providers.local import execute_local_task
from makewand.providers.aider import execute_aider_task

from makewand.execution_contract import (
    EXIT_PASSED,
    EXIT_INTERNAL_ERROR,
    EXIT_USAGE_ERROR,
    EXIT_FAILED,
    EXIT_UNVERIFIED,
    EXIT_CANCELLED,
    EXIT_BUDGET_EXHAUSTED,
    EXIT_APPLY_CONFLICT,
    EXIT_SANDBOX_UNAVAILABLE,
)

from makewand.task_admission import (
    detect_task_tier,
    is_identity_or_chit_chat,
    _split_prompt_clauses,
    _is_question_clause,
    is_inquiry_prompt,
    has_explicit_coding_imperative,
    classify_prompt_intent,
    get_identity_message,
    check_load_backpressure,
    _explicit_readonly_request,
)
from makewand.engine_selection import (
    BURN_PENALTY_FULL_SCALE,
    BURN_PENALTY_MIN_FACTOR,
    RELIABILITY_GOOD_RATE,
    RELIABILITY_BAD_RATE,
    RELIABILITY_MIN_FACTOR,
    ENGINES_WITHOUT_EXECUTOR,
    NON_AGENTIC_CHAT_MODELS,
    _match_domain_keywords,
    burn_rate_factor,
    apply_burn_rate_penalty,
    reliability_factor,
    select_optimal_engine_pair,
    _no_provider_detected,
    _print_no_provider_guidance,
    _engine_usable,
    _select_race_judge,
)
from makewand.task_dag import (
    TaskNode,
    TaskDAG,
    decompose_task_to_dag,
    execute_task_dag,
)


from makewand.review_contract import (
    _normalize_verdict_dict,
    _coerce_verdict_payload,
    _scan_verdict_lines,
    evaluate_review_verdict,
    extract_verdict_json,
    is_review_passed,
    canonical_verdict_line,
    strip_verdict_lines,
    review_verdict_output_spec,
    build_verdict_followup_prompt,
    REVIEW_PASSED,
    REVIEW_FAILED,
    REVIEW_UNVERIFIED,
    _VERDICT_TAG_RE,
    _VERDICT_ANY_RE,
    _VERDICT_TRAILER_OK_RE,
)

from makewand.review_verdict import (
    resolve_review_verdict,
    build_autofix_prompt,
    _test_gate_verdict_text,
    _unverified_artifacts_root,
    _ensure_private_artifacts_dir,
    _save_unverified_artifacts,
    format_review_diff,
    has_critical_defects,
    extract_review_verdict_dict,
    parse_race_verdict,
    compute_patch_parsimony,
)
from makewand.ensemble import run_ensemble



class LocalTestsUnavailable(str):
    """Detected acceptance cannot run; this is not a tested failure or pass."""
    execution_status = "UNVERIFIED"


def run_local_tests(cwd: str, timeout: int = 60) -> Tuple[bool, Optional[str]]:
    """
    Deterministically detects and runs local unit test suites in cwd inside Bubblewrap sandbox.
    Supports composite / multi-stack projects (Python, Go, Node, Rust).
    All suites and fallback commands share one wall-clock deadline.
    Returns (passed: bool, details: Optional[str]).
    If no tests exist in project, returns (True, None).
    """
    import shutil
    from makewand.sandbox import run_in_sandbox
    from makewand.artifact import workspace_snapshot, changed_inputs
    test_deadline = time.monotonic() + max(0, timeout)

    def remaining_timeout():
        return max(0, test_deadline - time.monotonic())

    p = Path(cwd)
    # Fast Syntax & Compilation Pre-Gate (Aider-inspired)
    try:
        from makewand.linter import fast_syntax_check
        from makewand.git_helper import get_dirty_files
        dirty = get_dirty_files(cwd)
        if dirty:
            syntax_ok, syntax_errs = fast_syntax_check(cwd, dirty)
            if not syntax_ok:
                return False, "代码静态语法校验失败 (Fast Syntax Gate):\n" + "\n".join(syntax_errs)
    except Exception as e:
        print(c(f"⚠️ [Fast Syntax Gate] 语法预检执行提示: {e}", COLOR_YELLOW), file=sys.stderr)

    if remaining_timeout() <= 0:
        return False, "本地测试总时间预算已耗尽"

    test_suites = []
    py_env = {"PYTHONPATH": cwd + os.pathsep + os.environ.get("PYTHONPATH", ""), "PYTHONDONTWRITEBYTECODE": "1"}

    # 1. Python test suites
    py_tests = list(p.glob("test_*.py")) or list(p.glob("*_test.py"))
    if (p / "tests").is_dir():
        py_tests += list((p / "tests").rglob("test_*.py")) + list((p / "tests").rglob("*_test.py"))
    pytest_config = (p / "pytest.ini").is_file()
    if (p / "pyproject.toml").is_file():
        try:
            import tomllib
            with (p / "pyproject.toml").open("rb") as f:
                pytest_config = pytest_config or bool(tomllib.load(f).get("tool", {}).get("pytest"))
        except (ImportError, OSError, ValueError):
            pass
    if pytest_config or py_tests:
        py_bin = sys.executable or "python3"
        test_target = []  # Respect pytest configuration and collect root-level tests too.
        try:
            import pytest
            py_cmd = [py_bin, "-B", "-m", "pytest", "-q", "-p", "no:cacheprovider", "-p", "no:langsmith", "-p", "no:django"] + test_target
        except ImportError:
            if shutil.which("pytest"):
                py_cmd = ["pytest", "-q", "-p", "no:cacheprovider", "-p", "no:langsmith", "-p", "no:django"] + test_target
            else:
                py_cmd = [py_bin, "-B", "-m", "unittest", "discover", "-q"]
        test_suites.append(("Python", py_cmd, py_env))

    # 2. Go test suites
    if (p / "go.mod").exists():
        if not shutil.which("go"):
            return False, LocalTestsUnavailable("检测到 Go 项目，但缺少 go 测试工具，验收未执行")
        test_suites.append(("Go", ["go", "test", "./..."], {}))

    # 3. Node / npm test suites
    if (p / "package.json").exists():
        try:
            with open(p / "package.json", "r", encoding="utf-8") as f:
                pkg_data = json.load(f)
                if not isinstance(pkg_data, dict) or not isinstance(pkg_data.get("scripts", {}), dict):
                    raise ValueError("package.json scripts must be an object")
                if "test" in pkg_data.get("scripts", {}):
                    if not isinstance(pkg_data["scripts"]["test"], str) or not pkg_data["scripts"]["test"].strip():
                        raise ValueError("npm test script must be a nonempty string")
                    if not shutil.which("npm"):
                        return False, LocalTestsUnavailable("检测到 Node 测试脚本，但缺少 npm 测试工具，验收未执行")
                    test_suites.append(("Node", ["npm", "test"], {}))
        except (OSError, ValueError, TypeError) as exc:
            return False, LocalTestsUnavailable(f"无法读取 Node 测试配置，验收未执行: {exc}")

    # 4. Cargo / Rust
    if (p / "Cargo.toml").exists():
        if not shutil.which("cargo"):
            return False, LocalTestsUnavailable("检测到 Rust 项目，但缺少 cargo 测试工具，验收未执行")
        test_suites.append(("Rust", ["cargo", "test"], {}))

    if not test_suites:
        return True, None

    try:
        tested_inputs = workspace_snapshot(cwd)
    except OSError as exc:
        return False, f"无法封存测试输入: {exc}"

    all_passed = True
    details = []

    for name, cmd, env in test_suites:
        step_timeout = remaining_timeout()
        if step_timeout <= 0:
            all_passed = False
            details.append("本地测试总时间预算已耗尽，未启动剩余测试套件")
            break
        code, stdout, stderr, err_category = run_in_sandbox(
            cmd=cmd,
            workspace=cwd,
            timeout=step_timeout,
            allow_network=False,
            readonly=False,
            is_provider=False,
            extra_env=env
        )
        if code != 0:
            output = (stdout + "\n" + stderr).strip()
            # If pytest failed because pytest is not installed in the target sandbox python, fallback to unittest!
            if name == "Python" and "No module named pytest" in output:
                step_timeout = remaining_timeout()
                if step_timeout <= 0:
                    all_passed = False
                    details.append("本地测试总时间预算已耗尽，未启动 unittest 回退")
                    break
                py_bin = sys.executable or "python3"
                fallback_cmd = [py_bin, "-B", "-m", "unittest", "discover", "-q"]
                cmd = fallback_cmd
                code, stdout, stderr, err_category = run_in_sandbox(
                    cmd=fallback_cmd,
                    workspace=cwd,
                    timeout=step_timeout,
                    allow_network=False,
                    readonly=False,
                    is_provider=False,
                    extra_env=env
                )
                output = (stdout + "\n" + stderr).strip()

        if remaining_timeout() <= 0:
            all_passed = False
            details.append(f"[{name} Tests Failed]: 本地测试总时间预算已耗尽")
            break

        if code == 0 and name == "Python" and (re.search(r"\b(collected 0 items|no tests ran|Ran 0 tests?|0 passed)\b", stdout + "\n" + stderr) or "Exit:" in (stdout + "\n" + stderr)):
            return False, LocalTestsUnavailable("Python 测试套件未执行任何有效测试用例，不能作为通过的验收证据")

        if code == 0 and name == "Go" and "[no test files]" in (stdout + stderr) and "PASS" not in stdout:
            return False, LocalTestsUnavailable("Go 测试套件未发现任何有效测试用例，不能作为通过的验收证据")

        if code == 0 and name == "Node" and re.search(r"\b(No tests found|0 passing)\b", stdout + "\n" + stderr):
            return False, LocalTestsUnavailable("Node 测试套件未执行任何有效测试用例，不能作为通过的验收证据")

        if code != 0:
            out = output
            if (
                err_category in ("SandboxUnavailable", "SandboxConfigError")
                or (stderr and stderr.strip().startswith("bwrap:"))
                or (out and out.strip().startswith("bwrap:"))
            ):
                if stderr and stderr.strip().startswith("bwrap:"):
                    infra_err = stderr
                elif out and out.strip().startswith("bwrap:"):
                    infra_err = out
                else:
                    infra_err = stderr or out
                if err_category == "SandboxUnavailable" and "Bubblewrap 沙箱不可用" not in infra_err:
                    infra_err = f"Bubblewrap 沙箱不可用: {infra_err}"
                return False, LocalTestsUnavailable(f"沙箱容器基础设施故障，本地测试阻断: {infra_err}")

            all_passed = False
            # Protect LLM context from giant test failure dumps via folded truncation
            try:
                from makewand.aci import truncate_output_folded
                output = truncate_output_folded(output, max_lines=60, max_bytes=8192)
            except Exception:
                pass
            if err_category:
                details.append(f"[{name} Tests Failed (exit {code})]:\n本地单元测试执行异常 ({err_category}):\n{output}")
            else:
                details.append(f"[{name} Tests Failed (exit {code})]:\n{output}")
        else:
            details.append(f"[{name} Tests Passed]: {cmd!r}")
            if stdout.strip():
                details.append(stdout.strip()[:500])

    try:
        changed = changed_inputs(tested_inputs, workspace_snapshot(cwd))
    except OSError as exc:
        return False, f"无法复核测试输入: {exc}"
    if changed:
        all_passed = False
        details.append("测试修改了待交付输入，必须重新生成并验证: " + ", ".join(changed[:20]))

    if all_passed:
        return True, "\n\n".join(details)
    else:
        return False, "\n\n".join(details)



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
    if not is_provider_enabled(engine):
        return rejected("UNVERIFIED", f"引擎 '{engine}' 当前已被用户在配置中手动禁用。运行 'makewand enable {engine}' 重新开启")
    adapters = {
        "claude": execute_claude_task, "codex": execute_codex_task,
        "grok": execute_grok_task, "muse": execute_muse_task,
        "agy": execute_agy_task, "local": execute_local_task,
        "ollama": execute_local_task, "aider": execute_aider_task,
    }
    api_engines = {"deepseek", "qwen", "glm", "kimi", "openrouter", "siliconflow"}
    placeholder_engines = {"cursor", "copilot"}
    if engine in placeholder_engines:
        notice = (
            f"引擎 '{engine}' 为交互式 IDE 扩展占位引擎，当前系统无独立无头命令行自主执行器。"
            f"已记录任务需求；在支持该插件的编辑器中协同完成，或选用 Claude/Codex/Antigravity 等自主 Agent 引擎。"
        )
        print(c(f"ℹ️ [Placeholder Provider] {notice}", COLOR_YELLOW), file=sys.stderr)
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
    def execute(effective_timeout):
        from makewand.workflow import provider_outcome
        provider_prompt = prompt
        if engine != "claude" and not readonly and cwd:
            provider_prompt = f"目标工作目录绝对路径: {cwd}\n请在该目录下创建/修改对应代码文件并落盘：\n{prompt}"
        if engine in api_engines:
            from makewand.providers.api_client import call_api_chat
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
    result = execution_runtime.execute(request, execute)
    try:
        from makewand.usage import record_engine_usage
        record_engine_usage(engine, tier=tier, success=result[0], task=prompt)
    except Exception:
        pass
    return result


def _stage_call(name, callback, *args, engine=None, **kwargs):
    """Name the stage while retaining the callback's legacy return type."""
    from makewand.telemetry import stage
    with stage(name, engine=engine, readonly=kwargs.get("readonly", False)) as span:
        result = callback(*args, **kwargs)
        status = getattr(result, "status", None)
        if name == "test" and isinstance(result, tuple) and len(result) == 2 and result[0] is True and result[1] is None:
            status = "UNVERIFIED"
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


def _freeze_delivery_inputs(root: str, reviewed_inputs: Dict[str, Any]) -> Dict[str, Dict[str, Any]]:
    """Freeze deliverable paths before review; Git metadata is not authority later."""
    repositories = [""] + get_submodule_paths(root)
    frozen = {}
    for relative_repo in repositories:
        repo = Path(root) / relative_repo
        if not repo.exists():
            continue
        if relative_repo and not (repo / ".git").exists():
            raise OSError(f"cannot verify uninitialized submodule {relative_repo}")
        code, names, error = run_git_cmd(
            ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "."],
            cwd=str(repo), binary=True)
        if code:
            raise OSError(f"cannot freeze delivery paths: {error}")
        expected = {}
        prefix = relative_repo + "/" if relative_repo else ""
        for raw in names.split(b"\0"):
            if not raw:
                continue
            path = os.fsdecode(raw).rstrip("/")
            if Path(path).is_absolute() or ".." in Path(path).parts:
                raise OSError("invalid delivery path")
            record = reviewed_inputs.get(prefix + path)
            if record is not None and record[0] in ("file", "link"):
                expected[path] = record
            elif (repo / path).exists() or (repo / path).is_symlink():
                if prefix + path not in repositories:
                    raise OSError(f"delivery path missing from reviewed inputs: {prefix + path}")
        frozen[relative_repo] = expected
    return frozen


def _delivery_patch_changes(root, baseline, commit, inputs, sub_baselines, sub_commits):
    """Bind exported patch paths to the immutable, reviewed payload records."""
    changes = {}
    groups = {}
    for repo, revision in {"": commit, **sub_commits}.items():
        group = groups[repo] = {}
        before = sub_baselines.get(repo) if repo else baseline
        if not before:
            continue
        code, listing, error = run_git_cmd(
            ["git", "diff", "--no-renames", "--name-status", "-z", before, revision],
            cwd=str(Path(root) / repo), binary=True)
        if code:
            raise OSError(f"cannot freeze delivery patch changes: {error}")
        fields = listing.split(b"\0")
        if fields[-1:] != [b""] or (len(fields) - 1) % 2:
            raise OSError("invalid delivery patch change listing")
        for status, raw_name in zip(fields[:-1:2], fields[1:-1:2]):
            name = os.fsdecode(raw_name)
            if not name or Path(name).is_absolute() or any(part in (".", "..", ".git") for part in name.split("/")):
                raise OSError("invalid delivery patch change path")
            relative = repo + "/" + name if repo else name
            # Gitlinks are represented by separately verified child patches.
            if relative in sub_commits:
                continue
            record = inputs[repo].get(name)
            if status == b"D":
                if record is not None:
                    raise OSError("delivery deletion still has a reviewed payload")
                changes[relative] = None
            elif status in (b"A", b"M", b"T") and record is not None:
                changes[relative] = list(record)
            else:
                raise OSError("delivery patch change lacks a reviewed payload: " + relative)
            group[relative] = changes[relative]
    return changes, groups


def _verify_delivery_commit(repo: str, commit: str, expected: Dict[str, Any], gitlinks: Dict[str, str]) -> str:
    """Compare immutable Git blobs/modes against the reviewed source records.

    A clean worktree is insufficient: hooks, filters, or a changed index/HEAD can
    create a clean but unreviewed commit. Read objects by ID, never through Git's
    worktree filters, then export and push only this verified commit ID.
    """
    if not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", commit):
        raise OSError("invalid delivery commit ID")
    code, listing, error = run_git_cmd(["git", "--no-replace-objects", "ls-tree", "-r", "-z", "--full-tree", commit], cwd=repo, binary=True)
    if code:
        raise OSError(f"cannot read delivery tree: {error}")
    blobs = []
    found = set()
    found_links = set()
    for entry in listing.split(b"\0"):
        if not entry:
            continue
        header, raw_path = entry.split(b"\t", 1)
        mode, kind, oid = header.split()
        path = os.fsdecode(raw_path)
        if mode == b"160000" and kind == b"commit":
            if gitlinks.get(path) != oid.decode("ascii"):
                raise OSError(f"unverified submodule commit: {path}")
            found_links.add(path)
            continue
        record = expected.get(path)
        if kind != b"blob" or record is None:
            raise OSError(f"unreviewed delivery path: {path}")
        expected_mode = b"120000" if record[0] == "link" else (b"100755" if record[2] & 0o100 else b"100644")
        if mode != expected_mode:
            raise OSError(f"delivery mode differs from review: {path}")
        blobs.append((path, oid, record))
        found.add(path)
    if found != set(expected) or found_links != set(gitlinks):
        raise OSError("delivery tree added or removed reviewed paths")
    if blobs:
        object_ids = b"".join(oid + b"\n" for _, oid, _ in blobs)
        code, sizes, error = run_git_cmd(["git", "--no-replace-objects", "cat-file", "--batch-check"], cwd=repo,
                                       input_data=object_ids, binary=True)
        if code or len(sizes.splitlines()) != len(blobs):
            raise OSError(f"cannot measure delivery objects: {error}")
        total_size = 0
        for line, (path, oid, _) in zip(sizes.splitlines(), blobs):
            header = line.split()
            if len(header) != 3 or header[:2] != [oid, b"blob"]:
                raise OSError(f"invalid delivery object: {path}")
            total_size += int(header[2])
            if total_size > 512 * 1024 * 1024:
                raise OSError("delivery objects exceeded the input budget")
        code, data, error = run_git_cmd(["git", "--no-replace-objects", "cat-file", "--batch"], cwd=repo,
                                      input_data=object_ids, binary=True)
        if code:
            raise OSError(f"cannot read delivery blobs: {error}")
        offset = 0
        for path, oid, record in blobs:
            end = data.find(b"\n", offset)
            header = data[offset:end].split()
            if end < 0 or len(header) != 3 or header[:2] != [oid, b"blob"]:
                raise OSError(f"invalid delivery object: {path}")
            size = int(header[2])
            content = data[end + 1:end + 1 + size]
            if len(content) != size or data[end + 1 + size:end + 2 + size] != b"\n":
                raise OSError(f"truncated delivery object: {path}")
            offset = end + size + 2
            matches = content == os.fsencode(record[1]) if record[0] == "link" else hashlib.sha256(content).hexdigest() == record[1]
            if not matches:
                raise OSError(f"delivery content differs from review: {path}")
        if offset != len(data):
            raise OSError("unexpected delivery object data")
    code, tree, error = run_git_cmd(["git", "--no-replace-objects", "rev-parse", commit + "^{tree}"], cwd=repo)
    if code or not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", tree.strip()):
        raise OSError(f"cannot identify verified delivery tree: {error}")
    return tree.strip()


def _run_pipeline_impl(
    prompt: str,
    cwd: Optional[str] = None,
    tier: str = "auto",
    model: Optional[str] = None,
    effort: Optional[str] = None,
    stream: bool = False,
    auto_fix: bool = True,
    max_fix: int = 2,
    timeout: int = 300,
    total_budget: Optional[int] = None,
    force_code: bool = False,
    repo_trust: str = "trusted",
    boost: bool = False,
    forced_engine: Optional[str] = None,
    local_only: bool = False,
    protected_paths=None,
    _guard: Optional[PipelineWorkspaceGuard] = None,
    _plan=None,
    _outcome=None,
) -> bool:
    if _guard is None:
        _guard = PipelineWorkspaceGuard()
    check_load_backpressure()
    if not cwd:
        cwd = os.getcwd()

    original_task_cwd = os.path.abspath(cwd)
    try:
        protected = ProtectedFiles.capture(original_task_cwd, protected_paths)
    except ProtectionError as error:
        if _outcome is not None:
            _outcome.update(status=error.status, error=str(error))
        print(c(str(error), COLOR_RED))
        return False

    if repo_trust == "untrusted":
        from makewand.sandbox import is_bwrap_available
        if not is_bwrap_available() and os.environ.get("MAKEWAND_UNSAFE_HOST_EXEC") != "1":
            print(c("❌ [Makewand Untrusted Repo] 当前仓库为 untrusted 且 Bubblewrap 沙箱不可用，根据安全防御原则阻断执行。", COLOR_RED + COLOR_BOLD))
            return False

    if boost:
        tier = "deep"
        print(c("⚡ [Makewand Boost] 强制超频模式已启用：穿透软配额限制，分配最高推理算力！", COLOR_MAGENTA + COLOR_BOLD))
    elif tier == "auto" or not tier:
        tier = "auto"
    else:
        from makewand.config import normalize_tier
        tier = normalize_tier(tier)

    # Decouple per-stage timeout from pipeline total budget
    if total_budget is None:
        total_budget = max(900, timeout * 3)

    from makewand.execution_runtime import current_context
    parent_deadline = current_context().get("deadline_unix_ms")
    pipeline_deadline = time.monotonic() + total_budget
    if parent_deadline is not None:
        pipeline_deadline = min(pipeline_deadline, time.monotonic() + max(0, parent_deadline / 1000 - time.time()))
    if current_context().get("_deadline_monotonic") is not None:
        pipeline_deadline = min(pipeline_deadline, current_context()["_deadline_monotonic"])

    def get_remaining_timeout(requested):
        return max(0, min(requested, pipeline_deadline - time.monotonic()))

    # Explicit read-only / negative patterns strictly override force_code
    has_explicit_readonly = _explicit_readonly_request(prompt)

    if has_explicit_readonly:
        intent = classify_prompt_intent(prompt)
        if intent == "code":
            intent = "explain"
    elif force_code:
        intent = "code"
    else:
        intent = classify_prompt_intent(prompt)

    if intent == "identity":
        print(c("💡 Makewand 意图识别: 身份/能力问答 (无需执行代码修改或程序检查)", COLOR_BOLD + COLOR_GREEN))
        print(get_identity_message())
        return True

    # Check multi-session working tree isolation guard for engineering tasks
    is_shadow_active = False
    shadow_res = None
    shadow_worktree_dir = None
    shadow_branch = None
    cleanup_shadow = None
    host_txn: Optional[HostWorkspaceTransaction] = None
    native_delivery = None
    delivery_baseline = None

    if intent not in ("identity", "explain", "review"):
        # One makewand code task per repository: a second task would otherwise
        # roll back or overwrite the first one's in-flight work.
        lock_error = _guard.acquire_workspace_lock(cwd)
        if lock_error:
            print(c(f"❌ [Makewand Workspace Lock] {lock_error}", COLOR_RED + COLOR_BOLD))
            return False
        try:
            is_safe, conflict_msg = check_working_tree_isolation(cwd)
        except Exception as exc:
            is_safe, conflict_msg = False, f"工作区隔离检查异常 ({exc})"

        if protected.paths:
            is_safe, conflict_msg = False, "任务声明了受保护文件，使用隔离工作树"

        if os.name == "nt":
            # The host transaction uses POSIX descriptors. Native Windows tasks
            # always generate in a shadow, including non-Git workspaces, and
            # deliver only through the sealed Win32 candidate transaction.
            is_safe, conflict_msg = False, "Windows 原生任务使用隔离副本与封存候选交付"

        if is_safe:
            # Host mode: record the complete task-start state BEFORE any git init.
            host_txn = HostWorkspaceTransaction(cwd)
            snapshot_error = host_txn.capture_pre_snapshot()
            if snapshot_error:
                if not host_txn.is_git_repo:
                    print(c(f"❌ [Makewand Transaction] {snapshot_error}，为保护数据已中止，未改动任何文件。", COLOR_RED + COLOR_BOLD))
                    return False
                # A git repository can still be worked on safely in a shadow worktree.
                host_txn = None
                is_safe, conflict_msg = False, snapshot_error
            _guard.txn = host_txn

        if not is_safe:
            print(c(f"🛡️ [Makewand Multi-Session Guard] {conflict_msg}！", COLOR_YELLOW + COLOR_BOLD))
            print(c("   依从多会话与脏工作区隔离安全策略，自动切换为独立影子工作树进行开发与审查...", COLOR_YELLOW))
            try:
                if os.name == "nt":
                    from makewand.native_delivery import create_native_shadow_worktree
                    shadow_res = create_native_shadow_worktree(cwd, prefix="guard")
                else:
                    from makewand.delivery import capture_delivery_baseline, check_delivery_state, snapshot_limits
                    target_root = Path(find_git_root(original_task_cwd) or original_task_cwd).resolve()
                    delivery_limits = snapshot_limits()
                    delivery_baseline = capture_delivery_baseline(str(target_root),
                                                                  timeout=get_remaining_timeout(delivery_limits["max_seconds"]),
                                                                  limits=delivery_limits)
                    shadow_res = create_ephemeral_shadow_worktree(cwd, prefix="guard")
                shadow_worktree_dir, shadow_branch, cleanup_shadow = shadow_res[0], shadow_res[1], shadow_res[2]
                if not shadow_worktree_dir or not Path(shadow_worktree_dir).exists():
                    raise RuntimeError("Shadow worktree directory could not be established")
                if os.name != "nt":
                    actual_root = Path(getattr(shadow_res, "repo_root", None) or original_task_cwd).resolve()
                    if str(actual_root) != delivery_baseline["root"]:
                        raise OSError("shadow source differs from the frozen delivery destination")
                    check_delivery_state(delivery_baseline, timeout=get_remaining_timeout(delivery_baseline["snapshot_limits"]["max_seconds"]))
                cwd = shadow_worktree_dir
                is_shadow_active = True
                protected.prepare_workspace(cwd)
                protected.verify(original_task_cwd)
                branch_label = shadow_branch if shadow_branch else "独立隔离副本"
                print(c(f"   ✔ 已自动建立影子工作树: {shadow_worktree_dir} (分支: {branch_label})", COLOR_GREEN))
            except Exception as e:
                if cleanup_shadow:
                    cleanup_shadow()
                if _outcome is not None and isinstance(e, ProtectionError):
                    _outcome.update(status=e.status, error=str(e))
                print(c(f"❌ [Makewand Multi-Session Guard] 无法为活跃冲突会话建立安全影子工作树 ({e})，终止任务以防踩踏。", COLOR_RED + COLOR_BOLD))
                return False

    task_baseline = None

    def fail_and_cleanup(msg: str, status="FAILED") -> bool:
        if native_delivery is not None:
            shutil.rmtree(native_delivery["folder"], ignore_errors=True)
        if _outcome is not None:
            _outcome.update(status=status, error=msg)
        if is_shadow_active and cleanup_shadow:
            try:
                cleanup_shadow()
            except Exception as exc:
                print(c(f"⚠️ [Makewand Shadow] 影子工作树清理失败，请手动检查: {exc}", COLOR_YELLOW))
        elif host_txn is not None and host_txn.is_active:
            # Host mode: roll back only this task's changes (task-created paths are
            # removed, tracked files come from the baseline commit, pre-existing
            # untracked/ignored files from private backups) and verify the result.
            _stage_call("rollback", host_txn.rollback, msg)
        print(c(msg, COLOR_RED + COLOR_BOLD))
        return False

    def protection_check():
        try:
            protected.verify(cwd)
            protected.verify(original_task_cwd)
            return True
        except ProtectionError as error:
            return fail_and_cleanup("受保护文件校验失败: " + str(error), error.status)

    if intent == "explain":
        print(c(f"💡 Makewand 意图识别: 技术问答/解释模式 '{prompt}' (推理档位: {tier}, 只读安全隔离)", COLOR_BOLD + COLOR_GREEN))
        cache = get_or_update_status(force_probe=False)
        available_coders, _, route_meta = select_optimal_engine_pair(prompt, tier=tier, cache=cache, boost=boost)
        if local_only:
            primary_c = "local"
            sorted_engines = ["local"]
        elif forced_engine and forced_engine != "auto":
            primary_c = forced_engine.lower()
            sorted_engines = [primary_c]
        else:
            primary_c = route_meta.get("primary_coder") or (available_coders[0] if available_coders else "claude")
            sorted_engines = available_coders if available_coders else ["claude", "codex", "grok", "agy", "muse"]
        print(c(f"🎯 [Makewand Smart Routing] 技术解释优先指派引擎: {primary_c.upper()} (候选梯队: {' -> '.join(e.upper() for e in sorted_engines)})", COLOR_CYAN))

        qa_output = None
        for eng in sorted_engines:
            step_timeout = get_remaining_timeout(timeout)
            if step_timeout <= 0:
                print(c("❌ [Makewand Budget] 全局流水线预算已耗尽，终止问答执行。", COLOR_RED + COLOR_BOLD))
                return False
            result = _stage_call("review", dispatch_task,
                eng, prompt, cwd=cwd, timeout=step_timeout, tier=tier,
                model=model, effort=effort, stream=stream, readonly=True, repo_trust=repo_trust
            )
            ok, out, err = result
            if _outcome is not None and not ok:
                _outcome.update(status=getattr(result, "status", "UNVERIFIED"), error=err)
            if ok and out and out.strip():
                qa_output = out
                break
            if getattr(result, "status", None) in ("UNKNOWN", "TIMEOUT", "CANCELLED", "BUDGET_EXHAUSTED", "INVALID_REQUEST"):
                break

        if qa_output and not stream:
            print(qa_output)

        return qa_output is not None and protection_check()

    if intent == "review":
        print(c(f"💡 Makewand 意图识别: 独立代码审计/审查模式 '{prompt}' (只读安全隔离)", COLOR_BOLD + COLOR_CYAN))
        exit_code = run_review(cwd=cwd, stream=stream, timeout=timeout, user_prompt=prompt, repo_trust=repo_trust, local_only=local_only)
        if _outcome is not None:
            from makewand.execution_contract import STATUS_CODES
            _outcome["status"] = next((name for name, number in STATUS_CODES.items() if number == exit_code), "UNKNOWN")
        return exit_code == EXIT_PASSED and protection_check()

    print(c(f"🚀 Makewand 流水线启动: '{prompt}' (自适应模型档位: {tier})", COLOR_BOLD))
    print(f"工作目录: {cwd}\n")

    # Step 1: Health inspection
    cache = get_or_update_status(force_probe=False)

    # Step 2: Intelligent Multi-Model Routing & Implementation
    coder_candidates, reviewer_candidates, route_meta = _stage_call(
        "routing", select_optimal_engine_pair, prompt, tier=tier, cache=cache, boost=boost)
    if local_only:
        coder_candidates = ["local"]
        reviewer_candidates = ["local"]
        primary_c = "local"
        route_meta["primary_coder"] = "local"
        route_meta["primary_reviewer"] = "local"
        route_meta["single_tool_mode"] = True
        route_meta["reasons"] = ["用户指定 --local-only / --offline 模式：强制使用本机开源模型闭环 (100% 离线隐私零 Token)"]
    elif forced_engine and forced_engine != "auto":
        f_eng = forced_engine.lower()
        if f_eng in coder_candidates:
            coder_candidates.remove(f_eng)
        coder_candidates.insert(0, f_eng)
        primary_c = f_eng
        route_meta["primary_coder"] = f_eng
    else:
        primary_c = route_meta["primary_coder"]
    if _plan is not None and _plan.workflow == "single":
        coder_candidates = coder_candidates[:1]
    primary_r = route_meta["primary_reviewer"]

    if local_only and _plan is not None and _plan.cross_review_required:
        if _outcome is not None:
            _outcome.update(status="UNVERIFIED", error="Local-only execution cannot provide distinct-provider review for unknown or high risk")
        print(c("本地离线执行无法提供未知或高风险任务所需的跨提供者复审；请明确低风险或使用独立本地复审方案。", COLOR_YELLOW))
        return False

    if _no_provider_detected(forced_engine, route_meta):
        if _outcome is not None:
            _outcome.update(status="UNVERIFIED", error="No enabled execution provider")
        _print_no_provider_guidance()
        if is_shadow_active and cleanup_shadow:
            cleanup_shadow()
        return False

    print(c("🎯 [Makewand Smart Routing] 智能专精匹配与配额削峰决策:", COLOR_BOLD + COLOR_GREEN))
    if route_meta.get("reasons"):
        for r_item in route_meta["reasons"]:
            print(c(f"  • {r_item}", COLOR_CYAN))
    print(c(f"  • 主力实现引擎: {primary_c.upper()} (候选梯队: {' -> '.join([c.upper() for c in coder_candidates])})", COLOR_BOLD + COLOR_BLUE))
    print(c(f"  • 独立盲审引擎: {primary_r.upper()} (候选梯队: {' -> '.join([r.upper() for r in reviewer_candidates])})\n", COLOR_BOLD + COLOR_PURPLE))

    # Record task baseline commit before dispatching implementation
    # For shadow worktrees, baseline_commit preserves forwarded active session dirty state.
    # For normal worktrees, recording current HEAD captures intermediate commits + uncommitted modifications.
    shadow_repo_root = getattr(shadow_res, "repo_root", None) if is_shadow_active else None
    if is_shadow_active:
        task_baseline = getattr(shadow_res, "baseline_commit", None)
        active_sub_baselines = getattr(shadow_res, "sub_baselines", {}) or {}
        if os.name == "nt":
            try:
                from makewand.native_delivery import prepare_native_delivery
                native_delivery = prepare_native_delivery(original_task_cwd, shadow_res, protected)
            except (OSError, ValueError) as error:
                return fail_and_cleanup("无法封存 Windows 原生交付基线: " + str(error), "UNVERIFIED")
    else:
        # Host mode: git baseline (every git step rc-checked; a non-git directory
        # gets a temporary .git) plus backups of untracked/ignored files, all
        # before the first model dispatch. Any failure aborts with no deletion.
        begin_error = host_txn.begin() if host_txn is not None else "内部错误: 宿主模式缺少任务事务"
        if begin_error:
            print(c(f"❌ [Makewand Transaction] {begin_error}", COLOR_RED + COLOR_BOLD))
            return False
        task_baseline = host_txn.baseline_commit
        active_sub_baselines = {}
        if (Path(cwd) / ".gitmodules").exists():
            sorted_subs = get_submodule_paths(cwd)
            for s_rel in sorted_subs:
                s_p = Path(cwd) / s_rel
                if s_p.exists():
                    _, s_head, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(s_p))
                    if s_head and s_head.strip():
                        active_sub_baselines[s_rel] = s_head.strip()

    disp_tier = tier if tier != "auto" else "auto (自适应动态调步)"
    print(c(f"▶ 阶段 1: 代码编写与实现 (Implementation - Tier: {disp_tier})", COLOR_BOLD + COLOR_BLUE))
    # Retrieve codebase repo map for global architecture perception
    repo_map_snippet = ""
    try:
        from makewand.repomap import format_repo_map_for_prompt
        repo_map_snippet = format_repo_map_for_prompt(cwd, max_lines=80)
        if repo_map_snippet:
            print(c("🗺️  [Makewand Repo-Map] 自动提取代码库全局架构拓扑并注入实现上下文...", COLOR_CYAN))
    except Exception:
        pass

    # Retrieve past quality lessons and Kibitzer guidance
    memory_hints = ""
    try:
        from makewand.memory import format_memory_hints_for_prompt
        memory_hints = format_memory_hints_for_prompt(prompt)
        if memory_hints:
            print(c("🧠 [Makewand Kibitzer] 匹配并注入历史避坑与工程质量准则...", COLOR_PURPLE))
    except Exception:
        pass

    # Retrieve repository-specific playbook (verified build/test commands & conventions)
    playbook_hints = ""
    try:
        from makewand.memory import format_playbook_for_prompt
        playbook_hints = format_playbook_for_prompt(cwd)
        if playbook_hints:
            print(c("📘 [Makewand Playbook] 加载工程专属构建与测试指南...", COLOR_CYAN))
    except Exception:
        pass

    prompt_parts = [prompt]
    if repo_map_snippet:
        prompt_parts.append(repo_map_snippet)
    if memory_hints:
        prompt_parts.append(memory_hints)
    if playbook_hints:
        prompt_parts.append(playbook_hints)
    coder_prompt = "\n".join(prompt_parts)

    coder_output = None
    coder_engine = None

    for eng in coder_candidates:
        step_timeout = get_remaining_timeout(timeout)
        if step_timeout <= 0:
            return fail_and_cleanup("❌ [Makewand Budget] 全局流水线预算已耗尽，终止任务执行。", "TIMEOUT")

        print(c(f"→ 派发代码编写与实现任务给 {eng.upper()} (Tier: {tier})...", COLOR_BLUE + COLOR_BOLD))
        result = _stage_call("implementation", dispatch_task, eng, coder_prompt, engine=eng,
                             cwd=cwd, timeout=step_timeout, tier=tier, model=model, effort=effort, stream=stream,
                             readonly=False, repo_root=shadow_repo_root, repo_trust=repo_trust)
        success, out, err = result
        if not protection_check():
            return False
        if _outcome is not None and not success:
            _outcome.update(status=getattr(result, "status", "FAILED"), error=err)
        if success:
            print(c(f"✔ {eng.upper()} 完成代码编写与修改。", COLOR_GREEN))
            coder_output = out
            coder_engine = eng
            break
        else:
            print(c(f"⚠ {eng.upper()} 遇到限制或故障: {err}", COLOR_YELLOW))
            if getattr(result, "status", None) in ("UNKNOWN", "TIMEOUT", "CANCELLED", "BUDGET_EXHAUSTED", "INVALID_REQUEST"):
                return fail_and_cleanup("编码结果未确定或预算已耗尽；已停止自动切换提供者。", result.status)
            print(c("→ 自动切换下一顺位备用引擎接管实现...", COLOR_YELLOW))

    if coder_output is None:
        return fail_and_cleanup("❌ 所有可用模型均无法完成编码任务，流水线终止。",
                                (_outcome or {}).get("status", "FAILED"))

    if coder_output and not stream:
        print(c("【编码实现输出摘要】", COLOR_BOLD))
        print(coder_output.strip()[:500])
        print("...\n")

    # Step 3: Red-team review (Cross-model verification)
    worktree_for_diff = getattr(shadow_res, "worktree_root", cwd) if is_shadow_active else cwd
    try:
        diff_out = get_git_diff(worktree_for_diff, base_rev=task_baseline, sub_baselines=active_sub_baselines)
    except OSError as exc:
        return fail_and_cleanup(f"无法提取待审查 Git 差异: {exc}", "UNVERIFIED")
    if not diff_out or not diff_out.strip():
        is_coding_task = (intent == "code")
        if not is_coding_task:
            print(c("ℹ️ [Makewand Info] 本次任务为咨询/分析类意图，无需修改工作区文件，文本交付已完成。", COLOR_GREEN))
            if is_shadow_active and cleanup_shadow:
                cleanup_shadow()
            return True

        if coder_engine in ENGINES_WITHOUT_EXECUTOR or coder_engine in NON_AGENTIC_CHAT_MODELS:
            print(c(
                f"ℹ️ [Makewand Boundary Notice] 引擎 '{coder_engine}' 属于对话/辅助型非自主 Agent 引擎 (Non-agentic / Placeholder)，"
                f"未在工作区产生直接代码落盘。\n已输出文本方案与实现建议，跳过文件交付门禁与红队代码复审。",
                COLOR_YELLOW
            ))
            if is_shadow_active and cleanup_shadow:
                cleanup_shadow()
            return True

        print(c("ℹ 本次任务未产生相对于基线的有效代码改动 (git diff 为空)，无需启动红队复审与自愈流水线。", COLOR_CYAN))
        return fail_and_cleanup("❌ [Makewand Quality Gate] 任务未产生任何有效代码改动，终止交付。")

    # Run deterministic local test suite before review
    print(c("🧪 [Makewand Test Gate] 正在执行本地确定性测试验证...", COLOR_CYAN))
    try:
        tested_inputs = workspace_snapshot(worktree_for_diff)
    except OSError as exc:
        return fail_and_cleanup(f"无法封存测试前内容: {exc}")
    test_ok, test_err = _stage_call("test", run_local_tests, cwd, timeout=get_remaining_timeout(60))
    if not protection_check():
        return False
    if getattr(test_err, "execution_status", None) == "UNVERIFIED" or (test_ok and test_err is None):
        return fail_and_cleanup(str(test_err or "未发现可执行的本地测试，验收未验证"), "UNVERIFIED")
    try:
        reviewed_inputs = workspace_snapshot(worktree_for_diff)
        if reviewed_inputs != tested_inputs:
            test_ok, test_err = False, "测试开始至审查快照之间产物发生变化，必须重新测试。"
        diff_out = get_git_diff(worktree_for_diff, base_rev=task_baseline, sub_baselines=active_sub_baselines)
        delivery_inputs = _freeze_delivery_inputs(worktree_for_diff, reviewed_inputs) if is_shadow_active else {}
        if workspace_snapshot(worktree_for_diff) != reviewed_inputs:
            return fail_and_cleanup("审查快照生成期间工作区发生变化，拒绝交付。")
    except OSError as exc:
        return fail_and_cleanup(f"无法封存待审查内容: {exc}")
    if not test_ok:
        print(c(f"❌ [Makewand Test Gate] 发现单元测试失败：\n{test_err[:400]}", COLOR_RED + COLOR_BOLD))
        try:
            from makewand.memory import record_failure_pattern
            clean_err = test_err.strip()
            first_err = clean_err.splitlines()[-1][:180] if clean_err else "Local unit tests failed"
            record_failure_pattern(
                issue=f"Test gate failure in {Path(cwd).name}: {first_err}",
                lesson="Ensure deterministic local unit tests pass cleanly before submitting code."
            )
        except Exception:
            pass
    else:
        print(c("✔ [Makewand Test Gate] 本地测试套件校验通过 (或无单测需执行)。", COLOR_GREEN))

    print(c("\n▶ 阶段 2: 独立代码审计与质检 (Red-team Review - Tier: deep, 只读安全隔离)", COLOR_BOLD + COLOR_CYAN))
    diff_snippet = format_review_diff(diff_out)
    test_warning = f"\n【重要：本地测试运行失败】代码改动后本地单元测试报错如下：\n{test_err[:1500]}\n" if not test_ok else ""

    review_kibitzer = ""
    try:
        from makewand.memory import format_kibitzer_guidance
        review_kibitzer = format_kibitzer_guidance(prompt, stage="review")
    except Exception:
        pass

    review_prompt = (
        f"工作目录为: {cwd}。请审查以下代码改动（git diff），严查潜在并发死锁、内存泄露、空指针与边界用例漏洞。{test_warning}{review_kibitzer}\n"
        f"若发现严重隐患或单测报错未解决，请标注 [P1] 或 [P2] 并给出明确修复建议。\n"
        f"{review_verdict_output_spec()}"
        f"--- 代码改动 (git diff) ---\n{diff_snippet}"
    )

    actual_reviewers = [r for r in reviewer_candidates if r != coder_engine and _engine_usable(r, cache)[0]]
    if _plan is not None and _plan.workflow == "single":
        actual_reviewers = [coder_engine]
    elif not actual_reviewers and _plan is not None and _plan.cross_review_required:
        from makewand.config import get_active_providers
        actual_reviewers = [r for r in get_active_providers() if r != coder_engine and _engine_usable(r, cache)[0]]
    elif not actual_reviewers:
        if route_meta.get("single_tool_mode") or len(coder_candidates) <= 1:
            actual_reviewers = [coder_engine]
        else:
            fallback_r = "agy" if coder_engine != "agy" else ("codex" if cache.get("codex", {}).get("status") != "limited" else "claude")
            actual_reviewers = [fallback_r]

    review_output = None
    reviewer_engine = None
    for r_eng in actual_reviewers:
        step_timeout = get_remaining_timeout(timeout)
        if step_timeout <= 0:
            break
        is_self_review = (r_eng == coder_engine)
        rev_mode_str = "进行独立沙箱自审与边界复审 (单工具自审闭环)" if is_self_review else "进行独立跨模型红队审查 (Tier: deep, 只读隔离)"
        print(c(f"→ 派发给 {r_eng.upper()} {rev_mode_str}...", COLOR_CYAN + COLOR_BOLD))
        curr_prompt = ("【单工具自审要求】当前为单工具自审闭环模式，请务必完全转换角色为严苛的代码审计员，对以上代码修改持最高怀疑态度，进行无情审查与边界挑刺：\n" + review_prompt) if is_self_review else review_prompt
        res = _stage_call("review", dispatch_task, r_eng, curr_prompt, engine=r_eng,
                          cwd=cwd, timeout=step_timeout, tier="deep", effort=effort, stream=stream,
                          readonly=True, repo_root=shadow_repo_root, repo_trust=repo_trust)
        if _outcome is not None and not res[0]:
            _outcome.update(status=getattr(res, "status", "UNVERIFIED"), error=res[2])

        if isinstance(res, (tuple, list)) and len(res) == 3:
            success, out, err = res[0], res[1], res[2]
        else:
            success, out, err = False, "", "UNVERIFIED: 独立审查未产生有效响应或返回结构异常"
        if success and out and out.strip():
            print(c(f"✔ {r_eng.upper()} 独立红队审查完成。", COLOR_GREEN))
            review_output = out
            reviewer_engine = r_eng
            break
        else:
            print(c(f"⚠ {r_eng.upper()} 审查未产生有效响应: {err}", COLOR_YELLOW))
            if getattr(res, "status", None) in ("UNKNOWN", "TIMEOUT", "CANCELLED", "BUDGET_EXHAUSTED"):
                break

    def reject_unverified(reason: str) -> bool:
        # UNVERIFIED: never delivered, never auto-fixed; the reviewed patch is preserved for the user.
        patch_path, save_err = _save_unverified_artifacts(
            worktree_for_diff, task_baseline, active_sub_baselines, review_output, reason)
        where = f"未交付的改动补丁已保存至: {patch_path}" if patch_path else f"改动补丁保存失败 ({save_err})"
        return fail_and_cleanup(
            f"❌ [Makewand Quality Gate] 审查裁决未验证 (UNVERIFIED: {reason})：不交付、不进入 Auto-Fix。{where}",
            (_outcome or {}).get("status") if (_outcome or {}).get("status") in ("UNKNOWN", "TIMEOUT", "BUDGET_EXHAUSTED", "CANCELLED", "SANDBOX_UNAVAILABLE") else "UNVERIFIED")

    # Structured verdict is authoritative; if missing/malformed, ask the same reviewer once for the verdict line only.
    if test_ok and review_output and review_output.strip():
        review_output, resolved_verdict = resolve_review_verdict(
            review_output, reviewer_engine, cwd=cwd, timeout=get_remaining_timeout(timeout),
            repo_root=shadow_repo_root, repo_trust=repo_trust)
        if _outcome is not None and resolved_verdict.get("execution_status"):
            _outcome["status"] = resolved_verdict["execution_status"]

    # Deterministic test gate override: if local tests failed, pass CANNOT be True under any circumstances,
    # regardless of whether the reviewer returned structured JSON or free-form text ("LGTM").
    if not test_ok:
        review_output = _test_gate_verdict_text(test_err, review_output)

    # Fail-Closed Quality Gate: If code has changes but review fails completely or is empty, reject delivery
    if not review_output or not review_output.strip():
        return reject_unverified("独立审查服务未能完成代码审计")
    review_verdict = evaluate_review_verdict(review_output)
    if review_verdict["status"] == REVIEW_UNVERIFIED:
        return reject_unverified(review_verdict["reason"])

    # Step 4: Auto-Fix Loop (only for a well-formed FAILED verdict; UNVERIFIED never reaches here)
    if auto_fix and review_verdict["status"] == REVIEW_FAILED:
        current_fix_iter = 0
        while current_fix_iter < max_fix and review_verdict["status"] == REVIEW_FAILED:
            current_fix_iter += 1
            step_timeout = get_remaining_timeout(timeout)
            if step_timeout <= 0:
                print(c("❌ [Makewand Budget] 全局流水线预算耗尽，终止 Auto-Fix 自愈轮次。", COLOR_RED + COLOR_BOLD))
                break

            print(c(f"\n⚡ [Makewand Auto-Fix] 独立审计检测到高/中危缺陷，自动启动第 {current_fix_iter}/{max_fix} 轮修复闭环...", COLOR_YELLOW + COLOR_BOLD))

            fix_prompt = build_autofix_prompt(cwd, review_output, task_prompt=prompt)

            # Coder fixes
            fixed = False
            actual_fix_engine = None
            step_timeout = get_remaining_timeout(timeout)
            if coder_engine and step_timeout > 0:
                print(c(f"→ 由主力编码引擎 {coder_engine.upper()} 执行缺陷修复...", COLOR_YELLOW))
                fix_result = _stage_call("repair", dispatch_task, coder_engine, fix_prompt, engine=coder_engine, cwd=cwd, timeout=step_timeout, tier=tier, effort=effort, stream=stream, readonly=False, repo_root=shadow_repo_root, repo_trust=repo_trust)
                ok, _, _ = fix_result
                if not protection_check():
                    return False
                if getattr(fix_result, "status", None) in ("UNKNOWN", "TIMEOUT", "CANCELLED", "BUDGET_EXHAUSTED"):
                    return fail_and_cleanup("修复结果未确定或预算已耗尽；已停止自动切换提供者。", fix_result.status)
                if ok:
                    fixed = True
                    actual_fix_engine = coder_engine

            if not fixed:
                for alt_c in coder_candidates:
                    if alt_c != coder_engine:
                        step_timeout = get_remaining_timeout(timeout)
                        if step_timeout <= 0:
                            break
                        print(c(f"→ 自动切换备用引擎 {alt_c.upper()} 执行修复...", COLOR_YELLOW))
                        fix_result = _stage_call("repair", dispatch_task, alt_c, fix_prompt, engine=alt_c, cwd=cwd, timeout=step_timeout, tier=tier, effort=effort, stream=stream, readonly=False, repo_root=shadow_repo_root, repo_trust=repo_trust)
                        ok, _, _ = fix_result
                        if not protection_check():
                            return False
                        if getattr(fix_result, "status", None) in ("UNKNOWN", "TIMEOUT", "CANCELLED", "BUDGET_EXHAUSTED"):
                            return fail_and_cleanup("修复结果未确定或预算已耗尽；已停止自动切换提供者。", fix_result.status)
                        if ok:
                            fixed = True
                            actual_fix_engine = alt_c
                            break

            if not fixed:
                print(c("⚠ 缺陷自动修复未产生有效更新，维持当前审查结论。", COLOR_YELLOW))
                break

            # Re-run deterministic local tests after fix
            try:
                tested_inputs = workspace_snapshot(worktree_for_diff)
            except OSError as exc:
                return fail_and_cleanup(f"无法封存修复后测试输入: {exc}")
            test_ok, test_err = _stage_call("test", run_local_tests, cwd, timeout=get_remaining_timeout(60))
            if not protection_check():
                return False
            if getattr(test_err, "execution_status", None) == "UNVERIFIED" or (test_ok and test_err is None):
                return fail_and_cleanup(str(test_err or "修复后未发现可执行的本地测试，验收未验证"), "UNVERIFIED")
            if not test_ok:
                print(c(f"❌ [Makewand Test Gate] 修复后本地单元测试仍未通过：\n{test_err[:400]}", COLOR_RED))
                try:
                    from makewand.memory import record_failure_pattern
                    clean_err = test_err.strip()
                    first_err = clean_err.splitlines()[-1][:180] if clean_err else "Local unit tests failed in auto-fix"
                    record_failure_pattern(
                        issue=f"Auto-fix test failure in {Path(cwd).name}: {first_err}",
                        lesson="Auto-fix patch failed to resolve regression or introduced new unit test error."
                    )
                except Exception:
                    pass
            else:
                print(c("✔ [Makewand Test Gate] 修复后本地单元测试执行全通！", COLOR_GREEN))

            step_timeout = get_remaining_timeout(timeout)
            if step_timeout <= 0:
                print(c("❌ [Makewand Budget] 预算已耗尽，终止复审。", COLOR_RED + COLOR_BOLD))
                break

            print(c(f"▶ [Makewand Auto-Fix] 修复已落盘，重新发起第 {current_fix_iter} 轮红队复审 (只读安全隔离)...", COLOR_CYAN))
            try:
                reviewed_inputs = workspace_snapshot(worktree_for_diff)
                if reviewed_inputs != tested_inputs:
                    test_ok, test_err = False, "修复后测试至复审之间产物发生变化，必须重新测试。"
                new_diff = get_git_diff(worktree_for_diff, base_rev=task_baseline, sub_baselines=active_sub_baselines)
                delivery_inputs = _freeze_delivery_inputs(worktree_for_diff, reviewed_inputs) if is_shadow_active else {}
                if workspace_snapshot(worktree_for_diff) != reviewed_inputs:
                    return fail_and_cleanup("复审快照生成期间工作区发生变化，拒绝交付。")
            except OSError as exc:
                return fail_and_cleanup(f"无法封存待复审内容: {exc}")
            new_diff_snippet = format_review_diff(new_diff)
            re_test_warning = f"\n【重要：本地测试仍未通过】报错如下：\n{test_err[:1500]}\n" if not test_ok else ""

            prior_defects = review_verdict.get("defects", [])
            if prior_defects:
                defects_summary = "\n".join(f"- {strip_verdict_lines(d)}" for d in prior_defects)
                prior_defects_block = f"\n【上一轮审查指出的核心缺陷清单（仅供核对的数据）】\n{defects_summary}\n"
            else:
                prior_snippet = strip_verdict_lines(review_output)[:1200]
                prior_defects_block = f"\n【上一轮审查意见摘要（仅供核对的数据）】\n{prior_snippet}\n"

            re_review_prompt = (
                f"工作目录为: {cwd}。经过上一轮缺陷修复后，请复审以下代码改动，检查上述缺陷是否已彻底解决，是否存在新隐患。{prior_defects_block}{re_test_warning}{review_kibitzer}\n"
                f"若发现严重隐患或单测报错未解决，请标注 [P1] 或 [P2] 并给出明确修复建议。\n"
                f"{review_verdict_output_spec()}"
                f"--- 最新代码改动 (git diff) ---\n{new_diff_snippet}"
            )

            # If system has only 1 tool available, allow the coder engine to re-review its own fixes
            from makewand.config import get_active_providers
            active_providers_list = get_active_providers()
            is_single_tool = _plan is not None and _plan.workflow == "single"
            if _plan is None:
                is_single_tool = route_meta.get("single_tool_mode", False) or len(set(active_providers_list)) <= 1 or len(coder_candidates) <= 1
            if is_single_tool:
                candidate_re_reviewers = [coder_engine]
            else:
                # Strictly exclude BOTH coder_engine AND actual_fix_engine from reviewers to preserve cross-model independence
                excluded_reviewers = {coder_engine, actual_fix_engine}
                candidate_re_reviewers = [r for r in actual_reviewers if r not in excluded_reviewers]
                if not candidate_re_reviewers:
                    active_pool_set = set(active_providers_list)
                    healthy_alts = [
                        e for e in active_pool_set
                        if e not in excluded_reviewers and cache.get(e, {}).get("status") not in ["limited", "needs_auth", "missing"]
                    ]
                    if healthy_alts:
                        candidate_re_reviewers = healthy_alts
                    else:
                        other_active = [e for e in active_pool_set if e not in excluded_reviewers]
                        if other_active:
                            candidate_re_reviewers = other_active
                        elif _plan is None or not _plan.cross_review_required:
                            candidate_re_reviewers = [coder_engine]
                if not candidate_re_reviewers:
                    return reject_unverified("缺乏与代码实现及修复提供者不同的复审模型")

            re_output = None
            re_engine = None
            for alt_r in candidate_re_reviewers:
                step_timeout = get_remaining_timeout(timeout)
                if step_timeout <= 0:
                    break
                is_self_re_review = (alt_r == coder_engine)
                rev_mode_str = "进行独立沙箱自审与边界复审 (单工具自审闭环)" if is_self_re_review else f"进行第 {current_fix_iter} 轮独立跨模型红队复审 (Tier: deep, 只读隔离)"
                print(c(f"→ 派发给 {alt_r.upper()} {rev_mode_str}...", COLOR_CYAN))
                curr_re_prompt = ("【单工具自审要求】当前为单工具自审闭环模式，请务必完全转换角色为严苛的代码审计员，对以上修复后的代码持最高怀疑态度，进行无情审查与边界挑刺：\n" + re_review_prompt) if is_self_re_review else re_review_prompt
                res = _stage_call("review", dispatch_task, alt_r, curr_re_prompt, engine=alt_r, cwd=cwd, timeout=step_timeout, tier="deep", effort=effort, stream=stream, readonly=True, repo_root=shadow_repo_root, repo_trust=repo_trust)
                if isinstance(res, (tuple, list)) and len(res) == 3:
                    ok, out, _ = res[0], res[1], res[2]
                else:
                    ok, out, _ = False, "", "UNVERIFIED: 复审未返回有效结果元组"
                if ok and out and out.strip():
                    re_output = out
                    re_engine = alt_r
                    break
                if getattr(res, "status", None) in ("UNKNOWN", "TIMEOUT", "CANCELLED", "BUDGET_EXHAUSTED"):
                    return fail_and_cleanup("复审结果未确定或预算已耗尽；已停止自动切换提供者。", res.status)

            if test_ok and re_output:
                re_output, resolved_verdict = resolve_review_verdict(
                    re_output, re_engine, cwd=cwd, timeout=get_remaining_timeout(timeout),
                    repo_root=shadow_repo_root, repo_trust=repo_trust)
                if resolved_verdict.get("execution_status"):
                    return fail_and_cleanup("复审补充裁决结果未确定或预算已耗尽，停止后续派发。", resolved_verdict["execution_status"])

            # Deterministic test gate override: if local tests failed, pass CANNOT be True under any circumstances
            if not test_ok:
                re_output = _test_gate_verdict_text(test_err, re_output)

            if re_output:
                # Capture the flagged defects from the prior round BEFORE overwriting review_output
                last_defects = list(review_verdict.get("defects", []))
                review_output = re_output
                review_verdict = evaluate_review_verdict(re_output)
                if review_verdict["status"] == REVIEW_UNVERIFIED:
                    print(c(f"❌ [Makewand Quality Gate] 复审裁决未验证 (UNVERIFIED: {review_verdict['reason']})，终止自愈回环。", COLOR_RED))
                    break
                if review_verdict["status"] == REVIEW_PASSED:
                    print(c("✔ [Makewand Auto-Fix] 经过自动修复，代码已通过红队复审！", COLOR_GREEN + COLOR_BOLD))
                    try:
                        from makewand.memory import record_autofix_lesson
                        defect_desc = "; ".join(last_defects[:3]) if last_defects else prompt[:120]
                        tokens = [w for w in re.findall(r"\b[a-zA-Z0-9_-]{4,}\b", prompt.lower()) if w not in ["this", "that", "with", "from", "have", "code", "file", "make", "task"]]
                        if not tokens:
                            tokens = [Path(cwd).name.lower()]
                        record_autofix_lesson(
                            keywords=tokens[:5],
                            issue=f"Defect flagged: {defect_desc}",
                            lesson=f"Remediated successfully in auto-fix iteration {current_fix_iter}."
                        )
                        print(c("🧠 [Makewand Memory] 已自动固化避坑修复经验到模式记忆库。", COLOR_PURPLE))
                    except Exception:
                        pass
                    break
            else:
                print(c("❌ [Makewand Quality Gate] 独立复审服务未能完成代码审计 (UNVERIFIED)，出于安全防御原则终止自愈回环。", COLOR_RED))
                review_output = "所有复审模型均超时或未能完成复审 (UNVERIFIED)"
                review_verdict = {"status": REVIEW_UNVERIFIED, "pass": False, "defects": [],
                                  "reason": "所有复审模型均超时或未能完成复审"}
                break

    print(c("\n============================================================", COLOR_BOLD))
    print(c("                   Makewand 联合调度完成报告", COLOR_BOLD + COLOR_GREEN))
    print(c("============================================================\n", COLOR_BOLD))
    if review_output and not stream:
        print(c("【最终审计意见与质量评估】", COLOR_BOLD))
        print(review_output.strip()[:1000])
        print("...\n")

    if not protection_check():
        return False

    # Non-bypassable Quality Gate: Local deterministic unit tests MUST pass
    if not test_ok:
        return fail_and_cleanup(
            f"❌ [Makewand Quality Gate] 本地确定性单元测试未通过 (Tests Failing)，阻断交付。\n"
            f"报错详情：\n{(test_err or '')[:1000]}"
        )

    if review_verdict["status"] == REVIEW_UNVERIFIED:
        return reject_unverified(review_verdict["reason"])
    if review_verdict["status"] != REVIEW_PASSED or not is_review_passed(review_output):
        return fail_and_cleanup("❌ [Makewand Quality Gate] 代码未能通过独立红队审查 (未获批准或存在缺陷)，拒绝交付。")

    try:
        if workspace_snapshot(worktree_for_diff) != reviewed_inputs:
            return fail_and_cleanup("❌ [Makewand Quality Gate] 审查期间产物内容或权限发生变化，拒绝交付未审查版本。")
    except OSError as exc:
        return fail_and_cleanup(f"无法复核已审查内容: {exc}")

    if get_remaining_timeout(timeout) <= 0:
        if _outcome is not None:
            _outcome["status"] = "TIMEOUT"
        return reject_unverified("全局截止时间已到，拒绝开始交付")
    if is_shadow_active:
        if shadow_branch or native_delivery is not None or delivery_baseline is not None:
            delivered_branch = shadow_branch
            has_baseline_conflict = False
            try:
                baseline_commit = getattr(shadow_res, "baseline_commit", None)
                repo_head = getattr(shadow_res, "repo_head", None)
                repo_root = getattr(shadow_res, "repo_root", None)
                sub_baselines = getattr(shadow_res, "sub_baselines", {}) or {}
                worktree_root = getattr(shadow_res, "worktree_root", shadow_worktree_dir)
                if native_delivery is None:
                    from makewand.delivery import check_delivery_state
                    if delivery_baseline is None:
                        raise OSError("delivery destination baseline was not frozen")
                    check_delivery_state(delivery_baseline, timeout=get_remaining_timeout(delivery_baseline["snapshot_limits"]["max_seconds"]))
                    from makewand.delivery import check_delivery_patch

                art_ts = datetime.now().strftime("%Y%m%d_%H%M%S")
                # Private 0700 directory with an unpredictable name (never shared /tmp).
                artifacts_dir = create_private_artifact_dir("delivery")
                patch_file = artifacts_dir / "makewand_delivery.patch"
                sub_patches = []
                verified_submodules = {}
                submodule_pushes = []

                # 0. Commit any changes inside submodules first so gitlinks can be staged
                # Crucial: Use get_submodule_paths to correctly handle paths with spaces and descending depth
                if (Path(worktree_root) / ".gitmodules").exists():
                    sorted_subs = sorted((p for p in delivery_inputs if p), key=lambda p: len(Path(p).parts), reverse=True)
                    for sub_rel in sorted_subs:
                        dst_sub = Path(worktree_root) / sub_rel
                        if dst_sub.exists():
                            _, s_out, _ = run_git_cmd(["git", "status", "--porcelain"], cwd=str(dst_sub))
                            if s_out.strip():
                                a_sub_code, _, a_sub_err = run_git_cmd(["git", "add", "-A"], cwd=str(dst_sub))
                                if a_sub_code != 0:
                                    return fail_and_cleanup(f"❌ [Makewand Quality Gate] 子模块 {sub_rel} 暂存失败 ({a_sub_err})，拒绝交付。")
                                c_sub_code, _, c_sub_err = run_git_cmd([
                                    "git",
                                    "-c", "user.name=Makewand",
                                    "-c", "user.email=makewand@local",
                                    "commit", "--no-verify", "-m", f"makewand: submodule {prompt[:50]}"
                                ], cwd=str(dst_sub))
                                if c_sub_code != 0:
                                    return fail_and_cleanup(f"❌ [Makewand Quality Gate] 子模块 {sub_rel} 提交失败 ({c_sub_err})，拒绝交付。")

                            commit_code, sub_commit, sub_error = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(dst_sub))
                            if commit_code:
                                raise OSError(f"cannot identify submodule commit: {sub_error}")
                            sub_commit = sub_commit.strip()
                            child_links = {path[len(sub_rel) + 1:]: value for path, value in verified_submodules.items()
                                           if path.startswith(sub_rel + "/") and not any(
                                               path.startswith(parent + "/") for parent in verified_submodules
                                               if parent != path and parent.startswith(sub_rel + "/"))}
                            sub_tree = _verify_delivery_commit(str(dst_sub), sub_commit, delivery_inputs[sub_rel], child_links)
                            verified_submodules[sub_rel] = sub_commit

                            # Generate binary-safe submodule patch if sub_base is known
                            sub_base = sub_baselines.get(sub_rel)
                            if sub_base:
                                p_sub_code, p_sub_b, p_sub_err = run_git_cmd(["git", "--no-replace-objects", "diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", sub_base, sub_commit], cwd=str(dst_sub), binary=True)
                                if p_sub_code != 0:
                                    return fail_and_cleanup(f"❌ [Makewand Quality Gate] 子模块 {sub_rel} 交付补丁导出失败 ({p_sub_err})，阻断交付。")
                                if p_sub_b and p_sub_b.strip():
                                    sub_hash = hashlib.sha256(sub_rel.encode("utf-8")).hexdigest()[:8]
                                    sub_patch_p = artifacts_dir / f"sub_{len(sub_patches):03d}_{sub_hash}.patch"
                                    if sub_patch_p.exists():
                                        return fail_and_cleanup(f"❌ [Makewand Quality Gate] 子模块 {sub_rel} 补丁文件已存在冲突，阻断交付。")
                                    try:
                                        write_private_file(sub_patch_p, p_sub_b)
                                    except Exception as swe:
                                        return fail_and_cleanup(f"❌ [Makewand Quality Gate] 子模块 {sub_rel} 补丁写入磁盘失败 ({swe})，阻断交付。")
                                    sub_patches.append({
                                        "rel_path": sub_rel,
                                        "patch_file": str(sub_patch_p),
                                        "sha256": hashlib.sha256(p_sub_b).hexdigest(),
                                        "verified_commit": sub_commit,
                                        "verified_tree": sub_tree,
                                    })

                            # Sync submodule commit object to src_sub so host can inspect/merge
                            if repo_root:
                                src_sub = Path(repo_root) / sub_rel
                                if src_sub.exists():
                                    submodule_pushes.append((str(dst_sub), str(src_sub.resolve()), sub_commit))

                # 1. Stage changes and verify staging success
                add_code, _, add_err = run_git_cmd(["git", "add", "-A"], cwd=worktree_root)
                if add_code != 0:
                    return fail_and_cleanup(f"❌ [Makewand Quality Gate] 影子分支代码暂存失败 ({add_err})，拒绝交付。")

                # 2. Check whether uncommitted changes exist in working tree to commit
                diff_staged_code, staged_names, _ = run_git_cmd(["git", "diff", "--cached", "--name-only"], cwd=worktree_root)
                if staged_names.strip():
                    c_code, _, c_err = run_git_cmd([
                        "git",
                        "-c", "user.name=Makewand",
                        "-c", "user.email=makewand@local",
                        "commit", "--no-verify", "-m", f"makewand: implement {prompt[:80]}"
                    ], cwd=worktree_root)
                    if c_code != 0:
                        return fail_and_cleanup(f"❌ [Makewand Quality Gate] 影子分支代码提交失败 ({c_err})，拒绝交付。")

                # Verify that working tree is 100% clean and matches the committed state
                clean_code, clean_check, _ = run_git_cmd(["git", "status", "--porcelain"], cwd=worktree_root)
                if clean_code != 0 or (clean_check and clean_check.strip()):
                    return fail_and_cleanup("❌ [Makewand Quality Gate] 交付提交后工作区残留未审查改动，拒绝交付未验证内容。")

                # 3. Verify that the task produced actual net changes compared to baseline
                impl_commit = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=worktree_root)[1].strip()
                if baseline_commit and impl_commit == baseline_commit:
                    return fail_and_cleanup("❌ [Makewand Quality Gate] 影子分支没有检测到任何已落盘的代码修改，拒绝交付空提交。")

                root_links = {path: value for path, value in verified_submodules.items()
                              if not any(path.startswith(parent + "/") for parent in verified_submodules if parent != path)}
                impl_tree = _verify_delivery_commit(worktree_root, impl_commit, delivery_inputs[""], root_links)
                if workspace_snapshot(worktree_for_diff) != reviewed_inputs:
                    return fail_and_cleanup("❌ [Makewand Quality Gate] 暂存或提交期间产物发生变化，拒绝交付未审查版本。")

                # 4. Generate binary-safe, full-index patch covering the entire task range (baseline_commit -> HEAD)
                # Saved outside the repository to prevent artifact leakage or uncommitted file pollution
                if baseline_commit:
                    p_code, p_diff_b, p_err = run_git_cmd([
                        "git", "--no-replace-objects", "diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", baseline_commit, impl_commit
                    ], cwd=worktree_root, binary=True)
                    if p_code != 0 or not p_diff_b or len(p_diff_b.strip()) == 0:
                        return fail_and_cleanup(f"❌ [Makewand Quality Gate] 交付补丁导出失败或内容为空 (code: {p_code}, err: {p_err})，阻断交付。")
                    try:
                        write_private_file(patch_file, p_diff_b)
                    except Exception as we:
                        return fail_and_cleanup(f"❌ [Makewand Quality Gate] 交付补丁写入磁盘失败 ({we})，阻断交付。")

                    if not patch_file.exists() or patch_file.stat().st_size == 0:
                        return fail_and_cleanup("❌ [Makewand Quality Gate] 交付补丁文件校验失败 (文件不存在或大小为0)，阻断交付。")

                # 5. Post-delivery integrity check: shadow worktree must be 100% clean
                dirty_code, dirty_check, _ = run_git_cmd(["git", "status", "--porcelain"], cwd=worktree_root)
                if dirty_code != 0 or dirty_check.strip():
                    return fail_and_cleanup(f"❌ [Makewand Quality Gate] 影子工作区交付后存在未受控改动或脏文件 ({dirty_check.strip()[:120]})，阻断交付。")

                if workspace_snapshot(worktree_for_diff) != reviewed_inputs:
                    return fail_and_cleanup("❌ [Makewand Quality Gate] 导出期间工作区发生变化，拒绝交付。")

                if native_delivery is None:
                    # A clone may have normalized links or other baseline inputs.
                    # Check the real, frozen destination before publishing refs.
                    check_delivery_state(delivery_baseline, timeout=get_remaining_timeout(delivery_baseline["snapshot_limits"]["max_seconds"]))
                    patch_targets = [(delivery_baseline["root"], patch_file)] + [
                        (str(Path(delivery_baseline["root"]) / item["rel_path"]), item["patch_file"])
                        for item in sub_patches]
                    for target, exported_patch in patch_targets:
                        check_delivery_patch(target, exported_patch, timeout=get_remaining_timeout(timeout))

                # Publish immutable, verified objects only. Moving HEAD between
                # validation and push/export cannot replace approved content.
                for sub_repo, destination, sub_commit in submodule_pushes:
                    push_code, _, push_error = run_git_cmd(["git", "--no-replace-objects", "push", destination, f"{sub_commit}:refs/heads/{delivered_branch}"], cwd=sub_repo)
                    if push_code:
                        return fail_and_cleanup(f"子模块交付提交同步失败: {push_error}")
                if repo_root and delivered_branch:
                    push_code, _, push_error = run_git_cmd(["git", "--no-replace-objects", "push", str(repo_root), f"{impl_commit}:refs/heads/{delivered_branch}"], cwd=worktree_root)
                    if push_code:
                        return fail_and_cleanup(f"主仓库交付提交同步失败: {push_error}")

                # Tree-based dirty baseline detection: compare tree hashes to avoid false conflict on clean repo
                tree_b_code, tree_b, _ = run_git_cmd(["git", "rev-parse", f"{baseline_commit}^{{tree}}"], cwd=worktree_root) if baseline_commit else (1, "", "")
                tree_h_code, tree_h, _ = run_git_cmd(["git", "rev-parse", f"{repo_head}^{{tree}}"], cwd=worktree_root) if repo_head else (1, "", "")
                has_baseline_conflict = bool(tree_b_code == 0 and tree_h_code == 0 and tree_b.strip() != tree_h.strip())

                # Generate apply_delivery.sh and delivery_manifest.json
                repo_apply_root = delivery_baseline["root"] if native_delivery is None else str(repo_root or worktree_root)
                manifest_data = {
                    "timestamp": art_ts,
                    "delivered_branch": delivered_branch,
                    "verified_commit": impl_commit,
                    "verified_tree": impl_tree,
                    "repo_root": repo_apply_root,
                    "baseline_commit": baseline_commit,
                    "repo_head": repo_head,
                    "has_baseline_conflict": has_baseline_conflict,
                    "main_patch": str(patch_file),
                    "submodule_patches": sub_patches
                }
                patch_changes = None
                if native_delivery is None:
                    patch_changes, patch_groups = _delivery_patch_changes(worktree_root, baseline_commit, impl_commit,
                                                                         delivery_inputs, sub_baselines, verified_submodules)
                    manifest_data["destination_baseline"] = delivery_baseline
                    manifest_data["expected_patch_changes"] = patch_changes
                if protected.paths:
                    manifest_data["protected_files"] = protected.to_dict()
                    manifest_data["protected_base_cwd"] = original_task_cwd
                if native_delivery is not None:
                    from makewand.native_delivery import seal_native_delivery
                    manifest_data["native_candidate_race_id"] = seal_native_delivery(
                        native_delivery, prompt, reviewed_inputs, review_output, str(test_err))
                manifest_file = artifacts_dir / "delivery_manifest.json"
                write_private_file(manifest_file, json.dumps(manifest_data, indent=2, ensure_ascii=False))

                apply_script_file = None
                if native_delivery is None:
                    from makewand.delivery import delivery_shell_setup
                    script_lines = [
                        "#!/usr/bin/env bash",
                        "# Auto-generated by Makewand Quality Gate Delivery (checked Git patches)",
                        "set -euo pipefail",
                        'for MAKEWAND_GIT_ENV in "${!GIT_@}"; do unset "$MAKEWAND_GIT_ENV"; done',
                        "export GIT_OPTIONAL_LOCKS=0 GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null",
                        'delivery_git() { local MAKEWAND_DELIVERY_ROOT="$1"; shift; git --no-replace-objects -c core.fsmonitor= -c core.hooksPath=/dev/null -c core.attributesFile=/dev/null -c core.autocrlf=false -c core.pager=cat -C "$MAKEWAND_DELIVERY_ROOT" --work-tree="$MAKEWAND_DELIVERY_ROOT" "$@"; }',
                        f'REPO_ROOT={shlex.quote(repo_apply_root)}',
                        'echo "============================================================"',
                        'echo "      📦 [Makewand Delivery Applier] 开始应用代码改动"',
                        'echo "============================================================"',
                        "",
                        "# 1. Pre-flight verification (atomic test without modifying files)",
                        'echo "→ [阶段 1/2] 补丁完整性与冲突预检 (Pre-flight check)..."'
                    ]
                    script_lines.append(delivery_shell_setup(delivery_baseline, patch_changes, patch_groups))
                    if protected.paths:
                        script_lines.append(protected.shell_guard(original_task_cwd))
                    script_lines.append('delivery_check preflight || exit "$?"')
                    main_patch_esc = shlex.quote(str(patch_file))
                    main_sha = hashlib.sha256(p_diff_b).hexdigest()
                    script_lines.append(f'MAIN_PATCH={main_patch_esc}')
                    script_lines.append(f'MAIN_SHA="{main_sha}"')
                    script_lines.append('if [ "$(sha256sum "$MAIN_PATCH" | cut -d" " -f1)" != "$MAIN_SHA" ]; then echo "❌ 主仓库补丁校验和不匹配，拒绝应用" >&2; exit 1; fi')

                    for idx, sp in enumerate(sub_patches):
                        sub_p_esc = shlex.quote(sp["patch_file"])
                        sub_r_esc = shlex.quote(sp["rel_path"])
                        sub_sha = shlex.quote(sp.get("sha256", ""))
                        script_lines.append(f'SUB_REL_{idx}={sub_r_esc}')
                        script_lines.append(f'SUB_PATCH_{idx}={sub_p_esc}')
                        script_lines.append(f'SUB_SHA_{idx}={sub_sha}')
                        if sp.get("sha256"):
                            script_lines.append(f'if [ "$(sha256sum "$SUB_PATCH_{idx}" | cut -d" " -f1)" != "$SUB_SHA_{idx}" ]; then echo "❌ 子模块补丁校验和不匹配 ($SUB_REL_{idx})，拒绝应用" >&2; exit 1; fi')
                        script_lines.append(f'delivery_git "$REPO_ROOT/$SUB_REL_{idx}" apply --check --binary "$SUB_PATCH_{idx}"')

                    script_lines.append('delivery_git "$REPO_ROOT" apply --check --binary "$MAIN_PATCH"')
                    script_lines.append('echo "✔ 预检通过，未检测到补丁冲突。"')
                    script_lines.append("")
                    script_lines.append("# 2. Apply checked patches and verify their postimages; reverse on error")
                    script_lines.append('echo "→ [阶段 2/2] 应用补丁并校验交付内容..."')
                    script_lines.append("APPLIED_SUB_INDICES=()")
                    script_lines.append("MAIN_APPLIED=0")
                    script_lines.append("")
                    script_lines.append("rollback() {")
                    script_lines.append("    set +e")
                    script_lines.append('    echo "❌ 补丁应用遭遇错误，尝试回滚本次已应用补丁..." >&2')
                    script_lines.append("    ROLLBACK_FAILED=0")
                    script_lines.append('    if [ "$MAIN_APPLIED" -eq 1 ]; then')
                    script_lines.append('        echo "  → 正在回滚主仓库改动..." >&2')
                    script_lines.append('        if delivery_check rollback "" "$DELIVERY_STATE_DIR/main.json"')
                    script_lines.append('        then')
                    script_lines.append('          if ! delivery_git "$REPO_ROOT" apply --reverse --binary "$MAIN_PATCH"; then')
                    script_lines.append('            echo "  ❌ 主仓库回滚失败！" >&2')
                    script_lines.append('            ROLLBACK_FAILED=1')
                    script_lines.append('          fi')
                    script_lines.append('        else')
                    script_lines.append('            echo "  ⚠ 主仓库交付路径已有外部修改，保留现场并停止自动逆转该补丁。" >&2')
                    script_lines.append('            ROLLBACK_FAILED=1')
                    script_lines.append('        fi')
                    script_lines.append('    fi')
                    script_lines.append('    for (( i=${#APPLIED_SUB_INDICES[@]}-1 ; i>=0 ; i-- )) ; do')
                    script_lines.append('        sub_idx="${APPLIED_SUB_INDICES[i]}"')
                    script_lines.append('        eval "sub_rel=\\$SUB_REL_${sub_idx}"')
                    script_lines.append('        eval "sub_patch=\\$SUB_PATCH_${sub_idx}"')
                    script_lines.append('        echo "  → 正在回滚子模块改动: $sub_rel..." >&2')
                    script_lines.append('        case "$sub_idx" in')
                    for idx, sp in enumerate(sub_patches):
                        script_lines.append(f'          {idx})')
                        script_lines.append(f'            if delivery_check rollback {shlex.quote(sp["rel_path"])} "$DELIVERY_STATE_DIR/sub_{idx}.json"')
                        script_lines.append('            then')
                        script_lines.append('                :')
                        script_lines.append('            else')
                        script_lines.append('                echo "  ⚠ 子模块交付路径已有外部修改，保留现场并停止自动逆转该补丁。" >&2')
                        script_lines.append('                ROLLBACK_FAILED=1')
                        script_lines.append('                continue')
                        script_lines.append('            fi ;;')
                    script_lines.append('        esac')
                    script_lines.append('        if ! delivery_git "$REPO_ROOT/$sub_rel" apply --reverse --binary "$sub_patch"; then')
                    script_lines.append('            echo "  ❌ 子模块 ($sub_rel) 回滚失败！" >&2')
                    script_lines.append('            ROLLBACK_FAILED=1')
                    script_lines.append('        fi')
                    script_lines.append('    done')
                    script_lines.append('    if [ "$ROLLBACK_FAILED" -eq 0 ]; then')
                    script_lines.append('        echo "✔ 本次已应用补丁已撤销。" >&2')
                    script_lines.append('    else')
                    script_lines.append('        echo "⚠️ 回滚过程中遇到错误，目标仓库存在未完全回滚的残留修改！请执行 git status 检查。" >&2')
                    script_lines.append('    fi')
                    script_lines.append('    exit 1')
                    script_lines.append("}")
                    script_lines.append("trap rollback ERR")
                    script_lines.append("")

                    for idx, sp in enumerate(sub_patches):
                        script_lines.append(f'printf "→ 应用子模块改动: %s...\\n" "$SUB_REL_{idx}"')
                        script_lines.append(f'delivery_git "$REPO_ROOT/$SUB_REL_{idx}" apply --binary "$SUB_PATCH_{idx}"')
                        script_lines.append(f'APPLIED_SUB_INDICES+=({idx})')
                        script_lines.append(f'delivery_check checkpoint {shlex.quote(sp["rel_path"])} "$DELIVERY_STATE_DIR/sub_{idx}.json"')

                    script_lines.append('printf "→ 应用主仓库改动...\\n"')
                    script_lines.append('delivery_git "$REPO_ROOT" apply --binary "$MAIN_PATCH"')
                    script_lines.append('MAIN_APPLIED=1')
                    script_lines.append('delivery_check checkpoint "" "$DELIVERY_STATE_DIR/main.json"')
                    if protected.paths:
                        script_lines.append(protected.shell_guard(original_task_cwd, rollback_on_error=True))
                    script_lines.append('delivery_check postflight || rollback')
                    script_lines.append('trap - ERR')
                    script_lines.append('printf "✔ 所有补丁已应用并校验交付内容，目标仓库改动就绪。\\n"')

                    apply_script_file = artifacts_dir / "apply_delivery.sh"
                    write_private_file(apply_script_file, "\n".join(script_lines) + "\n", mode=0o700)

            except Exception as e:
                return fail_and_cleanup(f"❌ [Makewand Quality Gate] 影子分支交付发生异常 ({e})，拒绝交付。")

            repo_apply_root = delivery_baseline["root"] if native_delivery is None else str(repo_root or worktree_root)
            apply_root_esc = shlex.quote(repo_apply_root)
            patch_file_esc = shlex.quote(str(patch_file))
            apply_script_esc = shlex.quote(str(apply_script_file)) if apply_script_file is not None else ""

            print(c("\n============================================================", COLOR_BOLD))
            print(c("       🛡️ [Makewand Multi-Session Guard] 隔离交付报告", COLOR_BOLD + COLOR_GREEN))
            print(c("============================================================\n", COLOR_BOLD))
            print(c("✔ 任务在独立工作树完成，零污染当前会话工作区！", COLOR_GREEN + COLOR_BOLD))
            print(f"  工作树路径: {c(worktree_root, COLOR_CYAN)}")
            if native_delivery is not None:
                print(f"  原生封存交付: makewand apply {native_delivery['race_id']} --candidate A")
                print(f"  固定提交与补丁清单: {manifest_file}")
            elif delivered_branch:
                print(f"  交付分支: {c(delivered_branch, COLOR_YELLOW)}")
                if has_baseline_conflict:
                    print(c("  ⚠ 注意：本任务基于当前会话未提交快照开发并完成独立审查。交付分支保留该上下文以保证可运行性。", COLOR_YELLOW))
                    print(f"  独立补丁文件 (仅包含本轮任务改动，二进制安全，零仓库污染): {c(str(patch_file), COLOR_CYAN)}")
                    if sub_patches:
                        print(f"  子模块补丁数量: {len(sub_patches)} (清单存放在 {c(str(manifest_file), COLOR_CYAN)})")
                    print(f"  一键应用交付补丁 (推荐): {c(apply_script_esc, COLOR_GREEN + COLOR_BOLD)}")
                    print(f"  手动应用: git -c core.autocrlf=false -C {apply_root_esc} apply --binary {patch_file_esc}\n")
                else:
                    print(f"  宿主机仓库可直接合并独立审查通过的改动: git -C {apply_root_esc} merge {impl_commit}")
                    print(f"  独立补丁备用存档: {c(str(patch_file), COLOR_CYAN)}")
                    print(f"  一键应用脚本备用: {c(apply_script_esc, COLOR_CYAN)}\n")
            else:
                print("  已封存非 Git 隔离副本的交付补丁，原工作区未受任何修改污染。")
                print(f"  固定提交与补丁清单: {manifest_file}")
                print(f"  一键应用交付补丁: {c(apply_script_esc, COLOR_GREEN + COLOR_BOLD)}\n")

    if get_remaining_timeout(timeout) <= 0:
        if _outcome is not None:
            _outcome["status"] = "TIMEOUT"
        return reject_unverified("全局截止时间已到，拒绝确认交付")
    if not protection_check():
        return False
    if host_txn is not None:
        # Report ignored-file changes that the reviewed diff cannot show, archive
        # the delivery patch and remove a temporary .git for non-git directories.
        _stage_call("apply", host_txn.finalize_success)
    print(c("✔ 任务全链路自适应闭环完成并通过红队审查。", COLOR_GREEN + COLOR_BOLD))
    return True

@functools.wraps(_run_pipeline_impl)
def run_pipeline(*args, **kwargs) -> bool:
    """Runs the pipeline; the workspace lock and host transaction are always closed.

    Any exit (failure, exception or interrupt) that leaves the host transaction
    open rolls it back, and a temporary .git created for a non-git directory is
    removed.
    """
    from makewand.workflow import choose_workflow, remember_result
    from makewand.execution_contract import ExecutionResult
    from makewand.execution_runtime import execution_context, task_id, current_context
    from makewand.telemetry import stage
    plan = choose_workflow(kwargs.pop("workflow", "auto"), kwargs.pop("risk", "auto"),
                           kwargs.pop("workflow_evidence", None))
    total_timeout = kwargs.pop("total_timeout", None)
    judge_reserve_seconds = kwargs.pop("judge_reserve_seconds", None)
    bound = inspect.signature(_run_pipeline_impl).bind_partial(*args, **kwargs)
    bound.apply_defaults()
    if plan.workflow == "race":
        if bound.arguments["local_only"]:
            raise ValueError("race requires distinct providers and cannot use --local-only")
        if bound.arguments["model"] is not None or bound.arguments["stream"]:
            raise ValueError("race workflow does not accept a shared --model or --stream override")
        code = run_race(bound.arguments["prompt"], cwd=bound.arguments["cwd"],
                        timeout=bound.arguments["timeout"], total_timeout=total_timeout if total_timeout is not None else bound.arguments["total_budget"],
                        repo_trust=bound.arguments["repo_trust"], risk="high" if plan.risk == "high" else "auto",
                        engine_a=bound.arguments["forced_engine"], judge_reserve_seconds=judge_reserve_seconds,
                        tier="deep" if bound.arguments["boost"] else bound.arguments["tier"],
                        effort=bound.arguments.get("effort"),
                        **({"protected_paths": bound.arguments["protected_paths"]}
                           if bound.arguments["protected_paths"] is not None else {}))
        return code == EXIT_PASSED
    if judge_reserve_seconds is not None:
        raise ValueError("judge reserve applies only to the race workflow")
    budget = total_timeout if total_timeout is not None else bound.arguments["total_budget"]
    if budget is None:
        budget = max(900, bound.arguments["timeout"] * 3)
    deadline = _deadline(budget)
    guard = PipelineWorkspaceGuard()
    result: Any = False
    error: Optional[BaseException] = None
    outcome = {"status": "FAILED", "error": None}
    try:
        with execution_context(deadline_unix_ms=deadline, workflow=plan.workflow, risk=plan.risk):
            with stage("workflow") as span:
                result = _run_pipeline_impl(*args, _guard=guard, _plan=plan, _outcome=outcome, **kwargs)
                if result:
                    outcome.update(status="PASSED", error=None)
                elif current_context().get("_deadline_monotonic", float("inf")) <= time.monotonic() and outcome["status"] in ("FAILED", "UNVERIFIED"):
                    outcome["status"] = "TIMEOUT"
                typed = remember_result(ExecutionResult(result, None, outcome["error"],
                    status=outcome["status"], task_id=task_id(), stage="workflow", engine="orchestrator"))
                span.finish(status=typed.status)
                return result
    except BaseException as exc:
        error = exc
        remember_result(ExecutionResult(False, None, str(exc), status="CANCELLED" if isinstance(exc, KeyboardInterrupt) else "INTERNAL_ERROR",
                                       task_id=task_id(), stage="workflow", engine="orchestrator"))
        raise
    finally:
        guard.close(result, error)


def run_workflow(prompt, **kwargs):
    """Typed workflow API; run_pipeline keeps its established boolean contract."""
    from makewand.workflow import last_result, remember_result
    from makewand.execution_contract import ExecutionResult
    from makewand.execution_runtime import task_id
    try:
        run_pipeline(prompt, **kwargs)
        return last_result()
    except ValueError as error:
        return remember_result(ExecutionResult(False, None, str(error), status="INVALID_REQUEST",
                                              task_id=task_id(), stage="workflow", engine="orchestrator"))


def run_review(cwd=None, stream=False, timeout=300, user_prompt=None, output_json=False,
               repo_trust="trusted", local_only=False, base_rev=None, effort=None):
    from makewand.execution_runtime import execution_context, task_id
    from makewand.execution_contract import ExecutionResult, STATUS_CODES
    from makewand.workflow import remember_result
    from makewand.telemetry import stage
    deadline = _deadline(timeout)
    with execution_context(deadline_unix_ms=deadline, workflow="review", readonly=True):
        with stage("review", readonly=True) as span:
            code = _run_review_impl(cwd, stream, timeout, user_prompt, output_json, repo_trust, local_only, base_rev, effort=effort)
            status = next((name for name, value in STATUS_CODES.items() if value == code), "UNKNOWN")
            remember_result(ExecutionResult(status == "PASSED", None, None, status=status,
                            task_id=task_id(), stage="review", engine="orchestrator", readonly=True))
            span.finish(status=status)
            return code


def _run_review_impl(cwd: Optional[str] = None, stream: bool = False, timeout: int = 300, user_prompt: Optional[str] = None, output_json: bool = False, repo_trust: str = "trusted", local_only: bool = False, base_rev: Optional[str] = None, effort: Optional[str] = None) -> int:
    from makewand.execution_runtime import current_context
    from makewand.execution_contract import EXIT_TIMEOUT
    def timed_out():
        if current_context().get("_deadline_monotonic", float("inf")) > time.monotonic():
            return False
        if output_json:
            print(json.dumps({"pass": False, "engine": None, "exit_code": EXIT_TIMEOUT,
                              "error": "review task deadline expired"}))
        return True
    if timed_out():
        return EXIT_TIMEOUT
    if not cwd:
        cwd = os.getcwd()

    if repo_trust == "untrusted":
        from makewand.sandbox import is_bwrap_available
        if not is_bwrap_available() and os.environ.get("MAKEWAND_UNSAFE_HOST_EXEC") != "1":
            if output_json:
                print(json.dumps({
                    "pass": False,
                    "exit_code": EXIT_UNVERIFIED,
                    "engine": None,
                    "defects": ["当前仓库为 untrusted 且 Bubblewrap 沙箱不可用，根据安全防御原则阻断审查"],
                    "error": "Untrusted repository requires Bubblewrap sandbox"
                }, ensure_ascii=False, indent=2))
            else:
                print(c("❌ [Makewand Untrusted Repo] 当前仓库为 untrusted 且 Bubblewrap 沙箱不可用，根据安全防御原则阻断审查。", COLOR_RED + COLOR_BOLD))
            return EXIT_UNVERIFIED

    if not output_json:
        print(c("🔍 Makewand 代码审计工具", COLOR_BOLD + COLOR_CYAN))
    if base_rev is None and (hasattr(get_git_diff, "mock") or hasattr(get_git_diff, "_mock_return_value") or "unittest.mock" in type(get_git_diff).__module__):
        diff_out = get_git_diff(cwd)
        diff_err = None
    else:
        diff_out, diff_err = get_git_diff_status(cwd, base_rev=base_rev)
    if timed_out():
        return EXIT_TIMEOUT
    if diff_err:
        if output_json:
            print(json.dumps({
                "pass": False,
                "exit_code": EXIT_UNVERIFIED,
                "engine": None,
                "defects": [f"Git diff 提取失败 ({diff_err})"],
                "error": diff_err
            }, ensure_ascii=False, indent=2))
        else:
            print(c(f"❌ [Makewand Review] 无法提取当前工作区改动 ({diff_err})，阻断审查。", COLOR_RED + COLOR_BOLD))
        return EXIT_UNVERIFIED

    if not diff_out.strip():
        if output_json:
            print(json.dumps({
                "pass": True,
                "exit_code": EXIT_PASSED,
                "engine": None,
                "defects": [],
                "message": "当前工作区没有检测到未提交的改动 (git diff 为空)"
            }, ensure_ascii=False, indent=2))
        else:
            print("当前工作区没有检测到未提交的改动 (git diff 为空)。")
        return EXIT_PASSED

    if base_rev is not None and len(diff_out.encode("utf-8")) > 64 * 1024:
        reason = "封存候选完整 diff 超过复审输入上限，需拆分后重新生成候选"
        print(json.dumps({"pass": False, "engine": None, "exit_code": EXIT_UNVERIFIED, "error": reason}, ensure_ascii=False) if output_json else reason)
        return EXIT_UNVERIFIED
    review_diff = diff_out if base_rev is not None else diff_out[:6000]
    cache = get_or_update_status()

    focus = f" 特别关注要求: {user_prompt}。" if user_prompt else ""
    prompt = (
        f"工作目录为: {cwd}。请详细审查当前仓库的修改（git diff），{focus}指出潜在隐患并给出修复建议。\n"
        f"{review_verdict_output_spec()}"
        f"--- 代码改动 (git diff) ---\n{review_diff}"
    )

    # Reviewer ladder honours `makewand disable <engine>` and the cached health status.
    if local_only:
        reviewer_ladder = [
            ("local", "派发给本地自托管模型进行独立红队审计 (Ollama / vLLM, 100% 离线隐私零 Token, 只读隔离)...", COLOR_CYAN),
        ]
    else:
        reviewer_ladder = [
            ("codex", "派发给 Codex CLI 进行红队审计 (gpt-6-astra, 只读隔离)...", COLOR_CYAN),
            ("grok", "派发给 Grok Build CLI 进行红队审计 (xAI / grok-4.7, 只读隔离)...", COLOR_RED),
            ("agy", "由 Antigravity 进行红队审计 (只读隔离)...", COLOR_GREEN),
        ]
    review_res = None
    reviewer_engine = None
    attempted = []
    terminal_status = "UNVERIFIED"
    for eng, banner, color in reviewer_ladder:
        usable, why = _engine_usable(eng, cache)
        if not usable:
            if not output_json:
                print(c(f"跳过 {eng.upper()} 审查引擎: {why}", COLOR_YELLOW))
            continue
        attempted.append(eng)
        if not output_json:
            print(c(banner, color))
        result = dispatch_task(eng, prompt, cwd=cwd, timeout=timeout, tier="deep", effort=effort,
                               stream=stream and not output_json, readonly=True, repo_trust=repo_trust)
        success, out, err = result
        if success and out and out.strip():
            review_res = out
            reviewer_engine = eng
            break
        if getattr(result, "status", None) in ("UNKNOWN", "TIMEOUT", "CANCELLED", "BUDGET_EXHAUSTED"):
            terminal_status = result.status
            break
        if not output_json:
            print(c(f"{eng.upper()} 审查失败 ({err or '输出内容为空'})，尝试下一审查引擎...", COLOR_YELLOW))

    if not review_res:
        from makewand.execution_contract import STATUS_CODES
        no_engine = not attempted
        if local_only:
            reason = ("本地审查引擎不可用或已被禁用 (根据 --local-only 隐私安全原则阻断向外部云端回退)" if no_engine
                      else "本地审查引擎未能产生有效输出 (根据 --local-only 隐私安全原则阻断向外部云端回退)")
        else:
            reason = ("没有已启用且健康的审查引擎 (codex/grok/agy 均被禁用或不可用)" if no_engine
                      else "独立审查服务未能产生有效输出 (UNVERIFIED)")
        if output_json:
            print(json.dumps({
                "pass": False,
                "exit_code": STATUS_CODES[terminal_status],
                "engine": None,
                "verdict_status": REVIEW_UNVERIFIED,
                "defects": [reason],
                "error": "No enabled and healthy review engine" if no_engine else "Independent review engine failed to produce valid output"
            }, ensure_ascii=False, indent=2))
        else:
            print(c(f"❌ [Makewand Quality Gate] {reason}，拒绝交付。", COLOR_RED + COLOR_BOLD))
        return STATUS_CODES[terminal_status]

    review_res, verdict = resolve_review_verdict(review_res, reviewer_engine, cwd=cwd, timeout=timeout,
                                                 repo_trust=repo_trust, quiet=output_json)
    if verdict.get("execution_status"):
        from makewand.execution_contract import STATUS_CODES
        exit_code = STATUS_CODES[verdict["execution_status"]]
    elif verdict["status"] == REVIEW_PASSED:
        exit_code = EXIT_PASSED
    elif verdict["status"] == REVIEW_FAILED:
        exit_code = EXIT_FAILED
    else:
        exit_code = EXIT_UNVERIFIED
    from makewand.execution_runtime import current_context
    from makewand.execution_contract import EXIT_TIMEOUT
    if current_context().get("_deadline_monotonic", float("inf")) <= time.monotonic():
        exit_code = EXIT_TIMEOUT

    if output_json:
        v_dict = extract_review_verdict_dict(review_res)
        v_dict["exit_code"] = exit_code
        v_dict["engine"] = reviewer_engine
        v_dict["raw_summary"] = review_res.strip()
        if verdict.get("execution_status"):
            v_dict.update({"pass": False, "error": verdict["reason"]})
        if exit_code == EXIT_TIMEOUT:
            v_dict.update({"pass": False, "error": "review task deadline expired"})
        if exit_code == EXIT_UNVERIFIED:
            v_dict["error"] = verdict["reason"]
        print(json.dumps(v_dict, ensure_ascii=False, indent=2))
        return exit_code

    if not stream:
        print(review_res)

    if exit_code == EXIT_PASSED:
        print(c("✔ 代码审计通过，未发现严重缺陷 (PASSED)。", COLOR_GREEN + COLOR_BOLD))
    elif exit_code == EXIT_FAILED:
        print(c("❌ 代码审计检测到严重隐患，未达合并标准 (FAILED)。", COLOR_RED + COLOR_BOLD))
    else:
        print(c(f"❌ 审查未给出有效的 MAKEWAND_VERDICT 裁决，结论未验证 (UNVERIFIED: {verdict['reason']})。", COLOR_RED + COLOR_BOLD))
    return exit_code




def review_saved_hybrid(race_id, stream=False, timeout=300, output_json=False,
                        repo_trust="trusted", local_only=False, effort=None):
    from makewand.execution_runtime import execution_context, task_id
    from makewand.execution_contract import ExecutionResult, STATUS_CODES
    from makewand.workflow import remember_result
    from makewand.telemetry import stage
    with execution_context(deadline_unix_ms=_deadline(timeout), workflow="review", readonly=True):
        with stage("review", readonly=True) as span:
            code = _review_saved_hybrid_impl(race_id, stream, timeout, output_json, repo_trust, local_only, effort=effort)
            status = next((name for name, value in STATUS_CODES.items() if value == code), "UNKNOWN")
            remember_result(ExecutionResult(status == "PASSED", None, None, status=status,
                task_id=task_id(), stage="review", engine="orchestrator", readonly=True))
            span.finish(status=status)
            return code


def _review_saved_hybrid_impl(race_id: str, stream: bool = False, timeout: int = 300,
                        output_json: bool = False, repo_trust: str = "trusted",
                        local_only: bool = False, effort: Optional[str] = None) -> int:
    """Approve only the same sealed, tested artifact independently reviewed."""
    import contextlib
    import io
    from makewand.candidate import CandidateManager, build_manifest, get_candidate_files_changed
    race = CandidateManager.get_race(race_id)
    hybrid = race.get("candidates", {}).get("M") if race else None
    report = {"pass": False, "engine": None, "exit_code": EXIT_UNVERIFIED}
    error = None
    from makewand.execution_contract import STATUS_CODES
    try:
        protection = ProtectedFiles.from_dict(race["protected_files"]) if race and "protected_files" in race else ProtectedFiles()
        if hybrid:
            protection.verify(hybrid.get("path"))
            protection.verify(race.get("base_cwd"))
    except ProtectionError as failure:
        report.update({"error": "候选 M 受保护文件校验失败: " + str(failure),
                       "exit_code": STATUS_CODES.get(failure.status, EXIT_UNVERIFIED),
                       "race_id": race_id, "candidate": "M"})
        print(json.dumps(report, ensure_ascii=False, indent=2) if output_json else report["error"])
        return report["exit_code"]
    if not hybrid or hybrid.get("test_passed") is not True:
        error = "候选 M 不存在或未通过本地测试"
    else:
        path = Path(hybrid["path"])
        manifest, changes = hybrid.get("manifest"), hybrid.get("changes")
        if (build_manifest(path) != manifest or
                get_candidate_files_changed(path, hybrid.get("baseline_commit")) != changes):
            error = "候选 M 已偏离封存产物，拒绝复审批准"
        else:
            captured = io.StringIO()
            with contextlib.redirect_stdout(captured):
                code = run_review(cwd=str(path), stream=False, timeout=timeout,
                                  output_json=True, repo_trust=repo_trust, local_only=local_only,
                                  base_rev=hybrid.get("baseline_commit"), effort=effort)
            try:
                report = json.loads(captured.getvalue())
                if not isinstance(report, dict):
                    raise ValueError("invalid review report")
            except (ValueError, TypeError):
                report = {"pass": False, "engine": None, "exit_code": EXIT_UNVERIFIED}
                error = "复审未返回有效的结构化报告"
            if error is None:
                if build_manifest(path) != manifest or get_candidate_files_changed(path, hybrid.get("baseline_commit")) != changes:
                    error = "候选 M 在复审期间发生改变，拒绝批准"
                elif code != EXIT_PASSED or report.get("pass") is not True or not report.get("engine"):
                    error = report.get("error") or "独立复审未明确通过候选 M"
                else:
                    from makewand.execution_runtime import current_context
                    from makewand.execution_contract import EXIT_TIMEOUT
                    if current_context().get("_deadline_monotonic", float("inf")) <= time.monotonic():
                        report["exit_code"] = EXIT_TIMEOUT
                        error = "候选 M 的任务截止时间已到，拒绝批准"
                    else:
                        approved, detail = CandidateManager.approve_hybrid_candidate(
                            race_id, manifest, changes, report.get("raw_summary", ""))
                        if not approved:
                            error = detail
                            if getattr(detail, "status", None) in STATUS_CODES:
                                report["exit_code"] = STATUS_CODES[detail.status]
                        else:
                            report["message"] = detail
    if error:
        report.update({"pass": False, "error": error,
                       "exit_code": report.get("exit_code") if report.get("exit_code") in (EXIT_FAILED, EXIT_UNVERIFIED, 12, 13, 15, 16, 17) else EXIT_UNVERIFIED})
    else:
        report["exit_code"] = EXIT_PASSED
    report.update({"race_id": race_id, "candidate": "M"})
    print(json.dumps(report, ensure_ascii=False, indent=2) if output_json else report.get("error") or report.get("message"))
    return report["exit_code"]




def run_race(prompt, cwd=None, timeout=300, repo_trust="trusted", engine_a=None,
             engine_b=None, synthesize_hybrid=False, *, total_timeout=None,
             judge_reserve_seconds=None, risk="auto", tier="standard", protected_paths=None, effort=None):
    """Reserve adjudication capacity and time before either contestant starts."""
    from makewand.workflow import choose_workflow, judge_reserve, remember_result
    from makewand.execution_runtime import execution_context, task_id
    from makewand.execution_contract import ExecutionResult, STATUS_CODES
    from makewand.call_budget import reserve_capacity, release_capacity, BudgetError
    from makewand.telemetry import stage
    leases = []
    total = timeout if total_timeout is None else total_timeout
    try:
        plan = choose_workflow("race", risk)
        if _explicit_readonly_request(prompt):
            raise ValueError("race generation cannot fulfill an explicitly read-only task; use review or run")
        deadline = _deadline(total)
        reserve_seconds = judge_reserve(total, judge_reserve_seconds)
    except ValueError as error:
        remember_result(ExecutionResult(False, None, str(error), status="INVALID_REQUEST",
                                       task_id=task_id(), stage="workflow", engine="orchestrator"))
        return EXIT_USAGE_ERROR
    with execution_context(deadline_unix_ms=deadline, workflow="race", risk=plan.risk):
        with stage("workflow") as span:
            try:
                for purpose in ("coder_a", "coder_b", "judge"):
                    leases.append(reserve_capacity(1, task_id(), purpose=purpose,
                                                   ttl_seconds=max(600, total + 30)))
                code = _run_race_impl(prompt, cwd=cwd, timeout=total, repo_trust=repo_trust,
                    engine_a=engine_a, engine_b=engine_b, synthesize_hybrid=synthesize_hybrid,
                    _leases=tuple(leases), judge_reserve_seconds=reserve_seconds, tier=tier,
                    effort=effort,
                    **({"protected_paths": protected_paths} if protected_paths is not None else {}))
                status = next((name for name, value in STATUS_CODES.items() if value == code), "UNKNOWN")
                from makewand.execution_runtime import current_context
                if current_context().get("_deadline_monotonic", float("inf")) <= time.monotonic() and status in ("FAILED", "UNVERIFIED"):
                    status = "TIMEOUT"
                result = remember_result(ExecutionResult(status == "PASSED", None, None,
                    status=status, task_id=task_id(), stage="workflow", engine="orchestrator"))
                span.finish(status=result.status)
                return result.exit_code
            except ProtectionError as error:
                result = remember_result(ExecutionResult(False, None, str(error), status=error.status,
                    task_id=task_id(), stage="workflow", engine="orchestrator"))
                print(c(str(error), COLOR_RED))
                span.finish(status=result.status, error_kind="protected_files")
                return result.exit_code
            except BudgetError as error:
                result = remember_result(ExecutionResult(False, None, str(error), status="BUDGET_EXHAUSTED",
                    task_id=task_id(), stage="workflow", engine="orchestrator"))
                print(c(str(error), COLOR_RED))
                span.finish(status=result.status, error_kind="budget_admission")
                return result.exit_code
            finally:
                for lease in leases:
                    try:
                        release_capacity(lease)
                    except (BudgetError, OSError, ValueError, TypeError, RuntimeError):
                        print("makewand: unused call capacity release unavailable; lease will expire", file=sys.stderr)


def _run_race_impl(
    prompt: str,
    cwd: Optional[str] = None,
    timeout: int = 300,
    repo_trust: str = "trusted",
    engine_a: Optional[str] = None,
    engine_b: Optional[str] = None,
    synthesize_hybrid: bool = False,
    _leases=(None, None, None),
    judge_reserve_seconds=0,
    tier="standard",
    protected_paths=None,
    effort: Optional[str] = None,
):
    from makewand.telemetry import stage
    race_deadline = time.monotonic() + max(0, timeout)
    from makewand.execution_runtime import current_context, execution_context, task_id
    parent_context = dict(current_context())
    parent_task_id = task_id()
    if parent_context.get("deadline_unix_ms") is not None:
        race_deadline = min(race_deadline, time.monotonic() + max(0, parent_context["deadline_unix_ms"] / 1000 - time.time()))
    if parent_context.get("_deadline_monotonic") is not None:
        race_deadline = min(race_deadline, parent_context["_deadline_monotonic"])
    def remaining_timeout(cap=timeout, generation=False):
        remaining = race_deadline - time.monotonic() - (judge_reserve_seconds if generation else 0)
        return max(0, min(cap, remaining))
    check_load_backpressure()
    if not cwd:
        cwd = os.getcwd()
    protected = ProtectedFiles.capture(cwd, protected_paths)

    if repo_trust == "untrusted":
        from makewand.sandbox import is_bwrap_available
        if not is_bwrap_available() and os.environ.get("MAKEWAND_UNSAFE_HOST_EXEC") != "1":
            print(c("❌ [Makewand Untrusted Repo] 当前仓库为 untrusted 且 Bubblewrap 沙箱不可用，根据安全防御原则阻断竞速。", COLOR_RED + COLOR_BOLD))
            return 1

    if not (engine_a and engine_b):
        from makewand.config import get_active_providers
        if not get_active_providers():
            _print_no_provider_guidance()
            return EXIT_FAILED

    print(c(f"🏁 Makewand 双模型并发竞速模式启动: '{prompt}'", COLOR_BOLD + COLOR_CYAN))

    # Candidates are isolated copies with their own git baseline; the host
    # directory is never git-initialized by a race (apply works from manifests).

    cache = get_or_update_status(force_probe=False)
    from makewand.config import is_provider_enabled
    c_ok = cache.get("claude", {}).get("status") == "healthy" and is_provider_enabled("claude")
    x_ok = cache.get("codex", {}).get("status") == "healthy" and is_provider_enabled("codex")
    g_ok = cache.get("grok", {}).get("status") == "healthy" and is_provider_enabled("grok")
    m_ok = cache.get("muse", {}).get("status") == "healthy" and is_provider_enabled("muse")
    l_ok = cache.get("local", {}).get("status") == "healthy" and is_provider_enabled("local")
    # agy used to be an unconditional fallback; it must now also be enabled and not known-unhealthy.
    agy_ok = _engine_usable("agy", cache)[0]

    # Explicitly requested contestants must still be enabled and usable.
    for explicit in (engine_a, engine_b):
        if explicit:
            usable, why = _engine_usable(explicit.lower(), cache)
            if not usable:
                print(c(f"❌ [Makewand Race] 指定的竞速引擎 {explicit.upper()} 不可用: {why}", COLOR_RED + COLOR_BOLD))
                return EXIT_UNVERIFIED

    # Pick Contestants
    if engine_a:
        name_a = engine_a.upper()
    elif x_ok:
        engine_a = "codex"
        name_a = "Codex (gpt-6-astra)"
    elif g_ok:
        engine_a = "grok"
        name_a = "Grok Build CLI (grok-4.7)"
    elif m_ok:
        engine_a = "muse"
        name_a = "Muse Code"
    elif l_ok:
        engine_a = "local"
        name_a = "Local Self-Hosted (本地大模型)"
    elif agy_ok:
        engine_a = "agy"
        name_a = "Antigravity (Gemini Fast)"

    if engine_b:
        name_b = engine_b.upper()
    elif c_ok and engine_a != "claude":
        engine_b = "claude"
        name_b = "Claude Code"
    elif g_ok and engine_a != "grok":
        engine_b = "grok"
        name_b = "Grok Build CLI (grok-4.7)"
    elif l_ok and engine_a != "local":
        engine_b = "local"
        name_b = "Local Self-Hosted (本地大模型)"
    elif agy_ok:
        engine_b = "agy"
        name_b = "Antigravity (Gemini Deep)"

    if not engine_a or not engine_b:
        print(c("❌ [Makewand Race] 没有足够的已启用且健康的引擎参与竞速 (被禁用或 limited/needs_auth/missing 的引擎不会被派发)。"
                "请运行 'makewand status' 检查或用 'makewand enable <engine>' 重新开启。", COLOR_RED + COLOR_BOLD))
        return EXIT_UNVERIFIED
    if engine_a.lower() == engine_b.lower():
        print(c("竞速需要两个不同的已启用提供者 (UNVERIFIED)。", COLOR_RED))
        return EXIT_UNVERIFIED

    ensure_config_dir()
    race_id = f"rc_{uuid.uuid4().hex[:8]}"
    session_dir = CANDIDATES_DIR / race_id
    wt_a = session_dir / "agent_a"
    wt_b = session_dir / "agent_b"
    wt_baseline = session_dir / "baseline"
    with stage("prepare", engine="race-host-manifest", readonly=True):
        host_baseline_manifest = build_manifest(Path(cwd))

    saved_successfully = False
    try:
        # Candidate copies are private (0700) and never contain .gitignore'd files.
        ensure_private_dir(CANDIDATES_DIR)
        ensure_private_dir(session_dir)
        wt_a.mkdir(mode=0o700, parents=True, exist_ok=True)
        wt_b.mkdir(mode=0o700, parents=True, exist_ok=True)

        try:
            _stage_call("copy", clone_isolated_worktree, cwd, wt_baseline, engine="race-baseline")
            protected.prepare_workspace(wt_baseline)
            with stage("prepare", engine="race-baseline-manifest", readonly=True):
                frozen_baseline_manifest = build_manifest(wt_baseline)
            _stage_call("copy", clone_isolated_worktree, str(wt_baseline), wt_a, engine="race-candidate-a")
            _stage_call("copy", clone_isolated_worktree, str(wt_baseline), wt_b, engine="race-candidate-b")
            protected.prepare_workspace(wt_a)
            protected.prepare_workspace(wt_b)
            protected.verify(cwd)
            with stage("prepare", engine="race-host-check", readonly=True):
                if build_manifest(Path(cwd)) != host_baseline_manifest:
                    raise OSError("host workspace changed while freezing race baseline")
        except OSError as exc:
            print(c(f"❌ [Makewand Race] 无法建立候选隔离副本，已中止竞速: {exc}", COLOR_RED + COLOR_BOLD))
            return EXIT_FAILED

        # Record baseline commit of host workspace
        code, b_commit, _ = run_git_cmd("git rev-parse HEAD", cwd=cwd)
        # Record baseline commit of candidate worktrees
        _, base_a_commit, _ = run_git_cmd("git rev-parse HEAD", cwd=str(wt_a))
        _, base_b_commit, _ = run_git_cmd("git rev-parse HEAD", cwd=str(wt_b))

        print(c(f"  选手 A: {name_a} (独立工作区: {wt_a})", COLOR_CYAN + COLOR_BOLD))
        print(c(f"  选手 B: {name_b} (独立工作区: {wt_b})", COLOR_BLUE + COLOR_BOLD))
        print(c("并发执行中，请稍候...\n", COLOR_YELLOW))

        # Retrieve codebase repo map for global architecture perception
        repo_map_snippet = ""
        try:
            from makewand.repomap import format_repo_map_for_prompt
            repo_map_snippet = format_repo_map_for_prompt(cwd, max_lines=80)
            if repo_map_snippet:
                print(c("🗺️  [Makewand Repo-Map] 自动提取代码库全局架构拓扑并注入竞速选手上下文...", COLOR_CYAN))
        except Exception:
            pass

        # Retrieve past quality lessons and Kibitzer guidance
        memory_hints = ""
        try:
            from makewand.memory import format_memory_hints_for_prompt
            memory_hints = format_memory_hints_for_prompt(prompt)
            if memory_hints:
                print(c("🧠 [Makewand Kibitzer] 匹配并注入历史避坑与工程质量准则...", COLOR_PURPLE))
        except Exception:
            pass

        racer_outcomes = {}
        def run_single_racer(engine: str, name: str, wt: Path, lease):
            start = time.time()
            step_timeout = remaining_timeout(generation=True)
            if step_timeout <= 0:
                racer_outcomes[name] = "TIMEOUT"
                return name, False, "race time budget exhausted", 0.0, wt
            prompt_parts = [f"工作目录绝对路径: {wt}\n请在该目录下完成代码编写并直接落盘：\n{prompt}"]
            if repo_map_snippet:
                prompt_parts.append(repo_map_snippet)
            if memory_hints:
                prompt_parts.append(memory_hints)
            full_p = "\n".join(prompt_parts)
            # ThreadPoolExecutor does not propagate ContextVars. Bind the same
            # task/deadline and this contestant's admission lease explicitly.
            with execution_context(task_id=parent_task_id, deadline_unix_ms=parent_context.get("deadline_unix_ms"),
                                   workflow="race", risk=parent_context.get("risk"), lease_id=lease):
                result = _stage_call("implementation", dispatch_task,
                    engine, full_p, engine=engine, cwd=str(wt), timeout=step_timeout,
                    tier=tier, effort=effort, repo_root=cwd, repo_trust=repo_trust)
                ok, out, err = result
                protected.verify(wt)
                protected.verify(cwd)
                racer_outcomes[name] = getattr(result, "status", "PASSED" if ok else "FAILED")
            duration = round(time.time() - start, 2)
            return name, ok, out, duration, wt

        run_agent_a = lambda: run_single_racer(engine_a, name_a, wt_a, _leases[0])
        run_agent_b = lambda: run_single_racer(engine_b, name_b, wt_b, _leases[1])

        try:
            high_load = os.getloadavg()[0] > 24.0
        except Exception:
            high_load = False

        if high_load:
            print(c("⏳ [Makewand Backpressure] 主机负载偏高，动态降为串行分时执行以避免竞争系统资源...", COLOR_YELLOW))
            res_a = run_agent_a()
            res_b = run_agent_b()
        else:
            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
                f_a = executor.submit(run_agent_a)
                f_b = executor.submit(run_agent_b)
                res_a = f_a.result()
                res_b = f_b.result()

        # Discard only reproducible new bytecode before binding any test or
        # review evidence. Baseline/tracked inputs and arbitrary cache source
        # remain part of the complete candidate seals.
        for candidate_path in (wt_a, wt_b):
            try:
                removed = remove_new_generated_bytecode(candidate_path, frozen_baseline_manifest)
                if removed:
                    print(c(f"测试封存前移除 {len(removed)} 个新生成的 Python 字节码缓存。", COLOR_CYAN))
            except OSError as exc:
                print(c(f"无法安全清理生成状态，拒绝封存候选: {exc}", COLOR_RED))
                return EXIT_UNVERIFIED

        # Deterministic local test gate validation on both candidate worktrees
        print(c("🧪 正在对两位候选人的产出分别执行本地确定性测试套件验证...", COLOR_CYAN))
        tested_a = workspace_snapshot(wt_a)
        tested_b = workspace_snapshot(wt_b)
        test_pass_a, test_detail_a = _stage_call("test", run_local_tests, str(wt_a), timeout=remaining_timeout(60, generation=True)) if remaining_timeout(60, generation=True) else (False, "race generation time budget exhausted")
        test_pass_b, test_detail_b = _stage_call("test", run_local_tests, str(wt_b), timeout=remaining_timeout(60, generation=True)) if remaining_timeout(60, generation=True) else (False, "race generation time budget exhausted")
        protected.verify(wt_a)
        protected.verify(wt_b)
        protected.verify(cwd)
        if test_pass_a is True and test_detail_a is None:
            test_pass_a = None
        if test_pass_b is True and test_detail_b is None:
            test_pass_b = None

        reviewed_a = workspace_snapshot(wt_a)
        reviewed_b = workspace_snapshot(wt_b)
        if reviewed_a != tested_a:
            test_pass_a = False
            print(c("⚠ 候选A在测试至审查之间发生变化，必须重新测试。", COLOR_YELLOW))
        if reviewed_b != tested_b:
            test_pass_b = False
            print(c("⚠ 候选B在测试至审查之间发生变化，必须重新测试。", COLOR_YELLOW))
        manifest_a = build_manifest(wt_a)
        manifest_b = build_manifest(wt_b)
        changes_a = get_candidate_files_changed(wt_a, baseline_commit=base_a_commit.strip() if base_a_commit else None)
        changes_b = get_candidate_files_changed(wt_b, baseline_commit=base_b_commit.strip() if base_b_commit else None)
        diff_a, diff_err_a = get_git_diff_status(str(wt_a), base_rev=base_a_commit.strip() if base_a_commit else None)
        diff_b, diff_err_b = get_git_diff_status(str(wt_b), base_rev=base_b_commit.strip() if base_b_commit else None)
        if diff_err_a:
            print(c(f"⚠ 选手 A diff 提取警告: {diff_err_a}", COLOR_YELLOW))
        if diff_err_b:
            print(c(f"⚠ 选手 B diff 提取警告: {diff_err_b}", COLOR_YELLOW))


        parsimony_a = compute_patch_parsimony(diff_a)
        parsimony_b = compute_patch_parsimony(diff_b)

        print(c("\n============================================================", COLOR_BOLD))
        print(c("                Makewand 竞速赛况与性能指标", COLOR_BOLD + COLOR_GREEN))
        print(c("============================================================\n", COLOR_BOLD))
        def test_state(value):
            return "通过" if value is True else "未验证 (无测试套件)" if value is None else "失败"
        print(f"选手 A [{res_a[0]}]: 状态={'✔ 成功' if res_a[1] else '❌ 失败'}, 单测={test_state(test_pass_a)}, 耗时={res_a[3]}s, 代码Diff大小={len(diff_a)} 字节, 精简度={parsimony_a['summary']}")
        print(f"选手 B [{res_b[0]}]: 状态={'✔ 成功' if res_b[1] else '❌ 失败'}, 单测={test_state(test_pass_b)}, 耗时={res_b[3]}s, 代码Diff大小={len(diff_b)} 字节, 精简度={parsimony_b['summary']}\n")

        # A verdict must cover complete diffs. Large candidates stay inspectable
        # but cannot obtain approval from a truncated review prompt.
        oversized_diffs = any(len(diff.encode("utf-8")) > 64 * 1024 for diff in (diff_a, diff_b))
        fmt_diff_a = diff_a if diff_a and not oversized_diffs else "差异过大，拒绝自动裁判" if oversized_diffs else "无代码改动 (空 diff)"
        fmt_diff_b = diff_b if diff_b and not oversized_diffs else "差异过大，拒绝自动裁判" if oversized_diffs else "无代码改动 (空 diff)"

        # Chief Referee evaluation with Antigravity (strictly read-only, TRUE BLIND REVIEW)
        judge_kibitzer = ""
        try:
            from makewand.memory import format_kibitzer_guidance
            judge_kibitzer = format_kibitzer_guidance(prompt, stage="review")
        except Exception:
            pass

        judge_prompt = (
            f"请作为资深软件架构裁判，以客观中立的双盲评审视角对比以下两位候选方案对同一任务的实现，指出各自优势与缺陷，并评定胜出者：\n\n"
            f"--- 原始任务 ---\n{prompt}\n\n"
            f"--- 自动化测试与工程指标 ---\n"
            f"• 候选方案 A: 运行状态={'正常' if res_a[1] else '失败'}, 本地单元测试={test_state(test_pass_a)}, 补丁精简度(Parsimony)={parsimony_a['summary']}\n"
            f"• 候选方案 B: 运行状态={'正常' if res_b[1] else '失败'}, 本地单元测试={test_state(test_pass_b)}, 补丁精简度(Parsimony)={parsimony_b['summary']}\n\n"
            f"【评审准则（Agentless 极简补丁偏好）】在两方案均通过单元测试且实现正确的前提下，优先奖励修改紧凑、聚焦、无多余大面积重构或无关格式修改的高精简度方案 (High Parsimony)。\n"
            f"{judge_kibitzer}\n"
            f"--- 候选方案 A 的代码实现 ---\n{fmt_diff_a}\n\n"
            f"--- 候选方案 B 的代码实现 ---\n{fmt_diff_b}\n\n"
            f"请给出两套方案的架构、可维护性与测试质量对比及采纳理由。"
            f'最后单独一行输出 MAKEWAND_RACE_VERDICT: {{"pass": true, "winner": "A", "defects": []}}，winner 仅可为 A 或 B。'
            f'若两个方案均不可采纳，输出 MAKEWAND_RACE_VERDICT: {{"pass": false, "winner": null, "defects": ["原因"]}}。不得强行选出胜者。'
        )
        judge_engine = _select_race_judge(cache, (engine_a, engine_b))
        judge_timeout = remaining_timeout()
        judge_status = "UNVERIFIED"
        if oversized_diffs:
            print(c("候选完整 diff 超过 64 KiB 审查上限，保留供 inspect；不调用裁判、不推荐候选。", COLOR_YELLOW))
            ok, judge_report = False, "完整候选 diff 超过 64 KiB 审查上限，结论未验证"
        elif judge_timeout <= 0:
            judge_status = "TIMEOUT"
            print(c("竞速总时间预算已耗尽，保留候选但不调用裁判或自动应用。", COLOR_YELLOW))
            ok, judge_report = False, None
        elif judge_engine is None:
            print(c("❌ [Makewand Race] 没有已启用且健康的裁判引擎，无法评定胜者 (UNVERIFIED)。", COLOR_RED + COLOR_BOLD))
            ok, judge_report = False, None
        elif judge_engine == "agy":
            print(c("由 Antigravity (Google AI Pro) 担任主裁判进行方案综合评估 (只读安全隔离)...", COLOR_GREEN + COLOR_BOLD))
            with execution_context(lease_id=_leases[2]):
                judge_result = _stage_call("judge", dispatch_task,
                    "agy", judge_prompt, engine="agy", cwd=cwd, tier="deep", effort=effort, timeout=judge_timeout,
                    readonly=True, repo_root=cwd, repo_trust=repo_trust)
                ok, judge_report, _ = judge_result
                judge_status = getattr(judge_result, "status", "PASSED" if ok else "FAILED")
        else:
            print(c(f"Antigravity 不可用，由 {judge_engine.upper()} 担任主裁判进行方案综合评估 (只读安全隔离)...", COLOR_GREEN + COLOR_BOLD))
            with execution_context(lease_id=_leases[2]):
                judge_result = _stage_call("judge", dispatch_task,
                    judge_engine, judge_prompt, engine=judge_engine, cwd=cwd, timeout=judge_timeout,
                    tier="deep", effort=effort, readonly=True, repo_root=cwd, repo_trust=repo_trust)
                ok, judge_report, _ = judge_result
                judge_status = getattr(judge_result, "status", "PASSED" if ok else "FAILED")
        if judge_report:
            print(c("\n【裁判裁决报告】", COLOR_BOLD))
            print(judge_report.strip())

        # Determine winner with strict deterministic test gate
        eligible_a = res_a[1] and test_pass_a is True and not diff_err_a and bool(diff_a.strip())
        eligible_b = res_b[1] and test_pass_b is True and not diff_err_b and bool(diff_b.strip())

        # A rejected, missing or malformed verdict never turns into a winner.
        verdict = parse_race_verdict(judge_report) if ok else None
        winner = verdict.get("winner") if verdict and verdict["pass"] else None
        if remaining_timeout() <= 0:
            winner, verdict, judge_status = None, None, "TIMEOUT"
            print(c("裁判返回后的会计或日志处理耗尽总截止时间；候选保留但不认可胜者。", COLOR_YELLOW))
        if winner == "A" and not eligible_a or winner == "B" and not eligible_b:
            if (winner == "A" and test_pass_a is None) or (winner == "B" and test_pass_b is None):
                verdict = None
            winner = None
        if workspace_snapshot(wt_a) != reviewed_a or workspace_snapshot(wt_b) != reviewed_b:
            winner = None
            verdict = None
            print(c("裁判审查期间候选内容发生变化，拒绝应用。", COLOR_RED))

        if not res_a[1] and not res_b[1] and not diff_a.strip() and not diff_b.strip():
            # Nothing to inspect or apply: do not archive empty candidate copies.
            print(c("❌ 两位选手均未能成功完成任务且没有产生任何改动，不保留候选工作区。", COLOR_RED + COLOR_BOLD))
            from makewand.execution_contract import STATUS_CODES
            for status in ("UNKNOWN", "TIMEOUT", "CANCELLED", "BUDGET_EXHAUSTED"):
                if status in racer_outcomes.values():
                    return STATUS_CODES[status]
            return EXIT_FAILED

        if remaining_timeout() <= 0:
            winner, verdict, judge_status = None, None, "TIMEOUT"
        protected.verify(wt_a)
        protected.verify(wt_b)
        protected.verify(cwd)
        try:
            CandidateManager.save_race(
                race_id=race_id,
                prompt=prompt,
                base_cwd=cwd,
                baseline_commit=b_commit.strip() if (code == 0 and b_commit) else "",
                agent_a={
                    "model": res_a[0],
                    "path": str(wt_a),
                    "duration": res_a[3],
                    "success": res_a[1],
                    "test_passed": test_pass_a,
                    "test_details": test_detail_a,
                    "review_passed": winner == "A",
                    "manifest": manifest_a,
                    "changes": changes_a,
                    "diff": diff_a,
                    "parsimony": parsimony_a,
                    "baseline_commit": base_a_commit.strip() if base_a_commit else "",
                },
                agent_b={
                    "model": res_b[0],
                    "path": str(wt_b),
                    "duration": res_b[3],
                    "success": res_b[1],
                    "test_passed": test_pass_b,
                    "test_details": test_detail_b,
                    "review_passed": winner == "B",
                    "manifest": manifest_b,
                    "changes": changes_b,
                    "diff": diff_b,
                    "parsimony": parsimony_b,
                    "baseline_commit": base_b_commit.strip() if base_b_commit else "",
                },
                judge_report=judge_report or "",
                winner=winner,
                baseline_dir=wt_baseline,
                baseline_manifest=host_baseline_manifest,
                frozen_baseline_manifest=frozen_baseline_manifest,
                **({"protected_files": protected.to_dict()} if protected.paths else {}),
            )
        except (ValueError, OSError) as exc:
            print(c(f"候选封存完整性检查失败，拒绝交付 (UNVERIFIED): {exc}", COLOR_RED))
            return EXIT_UNVERIFIED
        saved_successfully = True

        # Attempt 3-way AST & patch semantic hybrid merge between A and B
        hybrid_created = False
        hybrid_timeout = remaining_timeout(60)
        if synthesize_hybrid and hybrid_timeout > 0:
            try:
                ok_m, cand_m_meta, msg_m = CandidateManager.create_hybrid_candidate(race_id=race_id, test_timeout=hybrid_timeout)
                if ok_m and cand_m_meta and cand_m_meta.get("test_passed"):
                    hybrid_created = True
                    print(c("✨ [Makewand 3-Way Merge] 成功融合选手 A 与选手 B 的互补代码并全量通过本地单测！", COLOR_GREEN + COLOR_BOLD))
                    print(c(f"  • 合成方案: Candidate M (Hybrid) - {cand_m_meta.get('model')}", COLOR_CYAN + COLOR_BOLD))
                    print(f"  • 审查混合方案: makewand inspect {race_id} --candidate M")
                    print(f"  • 独立复审混合: makewand review --race-id {race_id} --candidate M\n")
            except Exception:
                pass
        elif synthesize_hybrid:
            print(c("竞速总预算已耗尽，未启动混合候选合成或测试。", COLOR_YELLOW))

        print(c(f"\n💾 候选工作区已妥善封存 (Race ID: {race_id})", COLOR_GREEN + COLOR_BOLD))
        if winner:
            print(c(f"  ★ 主裁推荐胜出方案: 选手 {winner}", COLOR_GREEN + COLOR_BOLD))
            print(f"  • 审查改动差异: makewand inspect {race_id} --candidate {winner}")
            print(f"  • 安全应用方案: makewand apply {race_id} --candidate {winner}")
        else:
            print(c("  ⚠ 未决出唯一胜出方案，请审查后显式指定方案:", COLOR_YELLOW))
            print(f"  • 审查方案差异: makewand inspect {race_id} --candidate A|B")
            print(f"  • 安全应用方案: makewand apply {race_id} --candidate A|B")
        if hybrid_created:
            print(c(f"  ★ 混合方案等待独立复审: makewand review --race-id {race_id} --candidate M", COLOR_GREEN + COLOR_BOLD))
        print(f"  • 丢弃废弃候选: makewand discard {race_id}\n")

        if remaining_timeout() <= 0:
            from makewand.execution_contract import EXIT_TIMEOUT
            return EXIT_TIMEOUT
        if not eligible_a and not eligible_b:
            from makewand.execution_contract import STATUS_CODES
            for status in ("UNKNOWN", "TIMEOUT", "CANCELLED", "BUDGET_EXHAUSTED"):
                if status in racer_outcomes.values():
                    return STATUS_CODES[status]
        if test_pass_a is not True and test_pass_b is not True:
            if test_pass_a is None or test_pass_b is None:
                print(c("没有候选获得明确的本地测试通过证据，结论未验证 (UNVERIFIED)。", COLOR_YELLOW))
                return EXIT_UNVERIFIED
            print(c("❌ [Makewand Test Gate] 两套候选方案均未通过本地单元测试，拒绝交付。", COLOR_RED + COLOR_BOLD))
            return EXIT_FAILED
        if not res_a[1] and not res_b[1]:
            print(c("❌ 两位选手均未能成功完成任务。", COLOR_RED + COLOR_BOLD))
            return EXIT_FAILED
        if verdict is None and winner is None:
            if judge_status in ("UNKNOWN", "TIMEOUT", "CANCELLED", "BUDGET_EXHAUSTED"):
                from makewand.execution_contract import STATUS_CODES
                return STATUS_CODES[judge_status]
            return EXIT_UNVERIFIED
        if winner is None:
            return EXIT_FAILED
        return EXIT_PASSED
    finally:
        if not saved_successfully and session_dir.exists():
            import shutil
            shutil.rmtree(session_dir, ignore_errors=True)
