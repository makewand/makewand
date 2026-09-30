#!/usr/bin/env python3
"""Subscription comparison adapter; all model tasks use one admission ledger."""
import argparse
import json
import os
import math
import subprocess
import sys
import tempfile
import time
from pathlib import Path


def _seed_repository(workspace, remaining):
    """Create a fresh seed without touching an ancestor checkout's Git state."""
    from makewand.git_helper import SAFE_GIT_SECURITY_FLAGS
    environment = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
    environment.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull,
                       GIT_CONFIG_SYSTEM=os.devnull)
    commands = (["init", "--template="], ["config", "user.name", "Makewand Benchmark"],
                ["config", "user.email", "benchmark@example.invalid"],
                ["add", "-A"], ["commit", "--allow-empty", "-m", "benchmark seed"])
    for command in commands:
        seconds = remaining()
        if seconds <= 0:
            raise subprocess.TimeoutExpired("benchmark seed setup", seconds)
        completed = subprocess.run(["git", "--no-pager", *SAFE_GIT_SECURITY_FLAGS, *command],
            cwd=str(workspace), env=environment, capture_output=True, text=True, timeout=seconds)
        if completed.returncode:
            raise RuntimeError("Benchmark repository setup failed: " + (completed.stderr or "Git exited without diagnostics"))



def _protected_single(dispatch, engine, prompt, workspace, state, protected, remaining):
    """Generate once in a copy; use the sealed atomic applier for delivery.

    These comparison arms intentionally have no local-test/review gate. Their
    candidate metadata records that lack of evidence, and force skips those
    gates only; the frozen file constraints and atomic apply still hold.
    """
    from makewand.candidate import CandidateManager, build_manifest
    from makewand.git_helper import clone_isolated_worktree, run_git_cmd
    from makewand.workflow import provider_outcome
    from makewand.execution_contract import ExecutionResult
    from makewand.telemetry import stage
    import uuid
    baseline, candidate = state / "single-baseline", state / "single-candidate"
    host_manifest = build_manifest(workspace)
    clone_isolated_worktree(str(workspace), baseline)
    protected.prepare_workspace(baseline)
    clone_isolated_worktree(str(baseline), candidate)
    protected.prepare_workspace(candidate)
    protected.verify(workspace)
    code, revision, error = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(candidate))
    if code:
        raise RuntimeError("Cannot bind single baseline: " + str(error))
    with stage("generation", engine=engine) as generation:
        result = provider_outcome(dispatch(engine, prompt, cwd=str(candidate), timeout=remaining(), tier="standard"))
        protected.verify(candidate)
        protected.verify(workspace)
        generation.finish(status=result.status, artifact_digest=result.artifact_digest, error_kind=result.error_kind)
    if not result.success:
        return result
    race_id = "single_" + uuid.uuid4().hex[:12]
    CandidateManager.save_race(race_id, prompt, str(workspace), revision.strip(),
        agent_a=dict(path=str(candidate), baseline_commit=revision.strip(), model=engine,
                     success=True, test_passed=None, review_passed=False),
        agent_b=dict(success=False, test_passed=None, review_passed=False),
        baseline_dir=baseline, baseline_manifest=host_manifest,
        protected_files=protected.to_dict())
    with stage("apply") as apply:
        ok, _, message = CandidateManager.apply_candidate(race_id, "A", force=True)
        applied = ExecutionResult(ok, result.output, None if ok else str(message),
                                  status="PASSED" if ok else getattr(message, "status", "APPLY_CONFLICT"))
        apply.finish(status=applied.status)
    return applied


def _apply_protected_pipeline_delivery(state, workspace, protected, remaining):
    """Apply this trial's sole SDK-sealed shadow delivery, with its guard."""
    files = list((state / "artifacts").glob("delivery_*/delivery_manifest.json"))
    if len(files) != 1:
        raise RuntimeError("Protected pipeline must produce exactly one sealed delivery")
    manifest = json.loads(files[0].read_text(encoding="utf-8"))
    if manifest.get("protected_base_cwd") != str(workspace) or manifest.get("protected_files") != protected.to_dict():
        raise RuntimeError("Protected pipeline delivery is not bound to this trial")
    from makewand.protected_files import ProtectedFiles
    ProtectedFiles.from_dict(manifest["protected_files"]).verify(workspace)
    completed = subprocess.run([str(files[0].parent / "apply_delivery.sh")], cwd=str(workspace),
                               capture_output=True, text=True, timeout=remaining())
    if completed.returncode:
        raise RuntimeError("Protected pipeline delivery failed: " + completed.stderr[:1000])


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("arm", choices=("single", "single-claude", "single-codex", "pipeline", "pipeline-single", "race"))
    parser.add_argument("--workspace", type=Path, required=True)
    parser.add_argument("--prompt-file", type=Path, required=True)
    parser.add_argument("--protect", action="append", default=None, metavar="PATH", help="Preserve a relative seed file; repeat as needed")
    parser.add_argument("--timeout", type=float, default=600)
    parser.add_argument("--risk", choices=("auto", "low", "medium", "high"), default="auto")
    parser.add_argument("--auto-fix", action="store_true",
                        help="enable bounded repair for pipeline arms after a verified failed review")
    parser.add_argument("--max-fix", type=int,
                        help="maximum repair rounds with --auto-fix (1 or 2; default: 2)")
    args = parser.parse_args(argv)
    if not math.isfinite(args.timeout) or args.timeout <= 0:
        parser.error("--timeout must be finite and positive")
    if (args.auto_fix or args.max_fix is not None) and args.arm not in ("pipeline", "pipeline-single"):
        parser.error("--auto-fix and --max-fix require a pipeline arm")
    if args.max_fix is not None and not args.auto_fix:
        parser.error("--max-fix requires --auto-fix")
    if args.max_fix is not None and not 1 <= args.max_fix <= 2:
        parser.error("--max-fix must be 1 or 2")
    max_fix = (args.max_fix if args.max_fix is not None else 2) if args.auto_fix else 0
    if not os.environ.get("MAKEWAND_CALL_BUDGET_FILE"):
        parser.error("a shared MAKEWAND_CALL_BUDGET_FILE is required")
    deadline_ms = int((time.time() + args.timeout) * 1000)
    workspace = args.workspace.resolve()
    if not workspace.is_dir() or workspace in (Path(workspace.anchor), Path.home().resolve(), Path(tempfile.gettempdir()).resolve()):
        parser.error("--workspace must be a dedicated existing seed directory")
    if os.path.lexists(workspace / ".git"):
        parser.error("the seed must be fresh and have no existing .git entry")
    arm = "single-claude" if args.arm == "single" else args.arm
    if arm == "pipeline-single" and args.risk != "low":
        parser.error("pipeline-single requires --risk low; other risks require independent cross review")
    # Remove API billing credentials only from this process. Subscription login
    # files remain readable to the official CLI sandbox; never alter user state.
    for key in list(os.environ):
        if key.endswith(("_API_KEY", "_AUTH_TOKEN")) or key in (
                "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"):
            os.environ.pop(key, None)
    # Multiple workspaces under one trial parent must never share candidates,
    # status, configuration, or usage. Keep each private state for inspection.
    state = Path(tempfile.mkdtemp(prefix="makewand-state-", dir=str(workspace.parent)))
    os.chmod(state, 0o700)
    enabled = "codex" if arm == "single-codex" else "claude" if arm in ("single-claude", "pipeline-single") else "claude,codex"
    os.environ.update(MAKEWAND_CONFIG_DIR=str(state / "config"),
                      MAKEWAND_ARTIFACTS_DIR=str(state / "artifacts"),
                      MAKEWAND_SHADOW_DIR=str(state / "shadows"),
                      MAKEWAND_USAGE_FILE=str(state / "usage.json"),
                      MAKEWAND_API_POLICY="subscription_only",
                      MAKEWAND_ENABLE_PROVIDERS=enabled,
                      MAKEWAND_NO_DAEMON="1")
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
    from makewand import config
    config.ensure_config_dir()
    config.CONFIG_FILE.write_text(json.dumps({"api_policy": "subscription_only"}), encoding="utf-8")
    os.chmod(config.CONFIG_FILE, 0o600)
    from makewand.execution_contract import ExecutionResult, STATUS_CODES
    from makewand.execution_runtime import execution_context, current_context, task_id
    from makewand.telemetry import stage
    from makewand.orchestrator import dispatch_task, run_workflow, run_race
    from makewand.workflow import provider_outcome
    from makewand.protected_files import ProtectedFiles, ProtectionError

    policy_risk = "auto" if args.risk == "medium" else args.risk
    metadata = dict(schema=1, arm=arm, risk=args.risk, policy_risk=policy_risk,
                    auto_fix=args.auto_fix, max_fix=max_fix,
                    timeout_seconds=args.timeout, independent_review=arm in ("pipeline", "pipeline-single", "race"),
                    distinct_review_provider_required=arm == "pipeline", distinct_contestants=arm == "race", hybrid=False)
    metadata_path = state / "adapter.json"
    metadata_path.write_text(json.dumps(metadata), encoding="utf-8")
    os.chmod(metadata_path, 0o600)

    def outcome(status, error=None):
        return ExecutionResult(status == "PASSED", None, error, status=status,
                               task_id=task_id(), stage="workflow", engine="benchmark")

    def remaining():
        return max(0.0, current_context()["_deadline_monotonic"] - time.monotonic())

    workflow = "direct" if arm.startswith("single-") else "single" if arm == "pipeline-single" else arm
    with execution_context(deadline_unix_ms=deadline_ms, workflow=workflow,
                           risk="medium" if args.risk == "auto" else args.risk):
        with stage("workflow") as span:
            try:
                with stage("prepare") as prepare:
                    prompt = args.prompt_file.read_text(encoding="utf-8")
                    inherited_protection = ProtectedFiles.capture(workspace)
                    protected = (ProtectedFiles.capture(workspace, [*inherited_protection.paths, *args.protect])
                                 if args.protect is not None else inherited_protection)
                    inherited_protection.verify(workspace)
                    metadata["protected_paths"] = list(protected.paths)
                    _seed_repository(workspace, remaining)
                    prepare.finish(status="PASSED")
                if remaining() <= 0:
                    result = outcome("TIMEOUT", "benchmark preparation exhausted the total deadline")
                elif arm.startswith("single-") and protected.paths:
                    result = _protected_single(dispatch_task, arm.removeprefix("single-"), prompt,
                                               workspace, state, protected, remaining)
                    print(result.output or result.error or "No provider output")
                elif arm.startswith("single-"):
                    engine = arm.removeprefix("single-")
                    with stage("generation", engine=engine) as generation:
                        result = provider_outcome(dispatch_task(engine, prompt,
                            cwd=str(workspace), timeout=remaining(), tier="standard"))
                        if not isinstance(result, ExecutionResult):
                            result = ExecutionResult(*result)
                        generation.finish(status=result.status, artifact_digest=result.artifact_digest,
                                          error_kind=result.error_kind)
                    print(result.output or result.error or "No provider output")
                elif arm in ("pipeline", "pipeline-single"):
                    result = run_workflow(prompt, cwd=str(workspace), timeout=remaining(),
                        total_timeout=remaining(), tier="standard", force_code=True, forced_engine="claude",
                        auto_fix=args.auto_fix, max_fix=max_fix, workflow=workflow, risk=policy_risk,
                        **({"protected_paths": list(protected.paths)} if protected.paths else {}))
                    if result.success and protected.paths:
                        with stage("apply") as apply:
                            _apply_protected_pipeline_delivery(state, workspace, protected, remaining)
                            protected.verify(workspace)
                            apply.finish(status="PASSED")
                else:
                    code = run_race(prompt, cwd=str(workspace), timeout=remaining(), total_timeout=remaining(),
                        engine_a="codex", engine_b="claude", synthesize_hybrid=False, risk=policy_risk,
                        tier="standard", **({"protected_paths": list(protected.paths)} if protected.paths else {}))
                    status = next((name for name, value in STATUS_CODES.items() if value == code), "UNKNOWN")
                    result = outcome(status)
                    if result.success:
                        from makewand.candidate import CandidateManager
                        race = CandidateManager.get_race()
                        if remaining() <= 0:
                            result = outcome("TIMEOUT", "benchmark deadline expired before candidate application")
                        elif not race or race.get("winner") not in ("A", "B"):
                            result = outcome("UNVERIFIED", "race has no independently approved A/B winner")
                        else:
                            with stage("apply") as apply:
                                ok, _, message = CandidateManager.apply_candidate(race["race_id"], race["winner"])
                                result = outcome("PASSED" if ok else getattr(message, "status", "APPLY_CONFLICT"), str(message))
                                apply.finish(status=result.status)
                            print(message)
                if result.success and remaining() <= 0:
                    result = outcome("TIMEOUT", "benchmark total deadline expired")
            except ProtectionError as error:
                result = outcome(error.status, str(error))
            except subprocess.TimeoutExpired:
                result = outcome("TIMEOUT", "benchmark preparation exceeded the total deadline")
            except KeyboardInterrupt:
                result = outcome("CANCELLED", "benchmark interrupted")
            except (OSError, RuntimeError, ValueError) as error:
                result = outcome("INTERNAL_ERROR", str(error))
            span.finish(status=result.status)
            metadata.update(status=result.status, exit_code=result.exit_code, task_id=task_id())
            metadata_path.write_text(json.dumps(metadata), encoding="utf-8")
            print("MAKEWAND_BENCHMARK_ADAPTER: " + json.dumps(metadata), file=sys.stderr)
            if result.error:
                print(result.error, file=sys.stderr)
            return result.exit_code


if __name__ == "__main__":
    raise SystemExit(main())
