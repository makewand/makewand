"""Native pipeline delivery through the same sealed candidate transaction.

The private baseline and delivery repository are prepared before generation.
Only the reviewed filesystem payload is copied into that trusted repository;
candidate-controlled Git administration is never copied into the delivery.
"""
import shutil
import uuid
from pathlib import Path

from makewand import config
from makewand.artifact import workspace_snapshot
from makewand.candidate import CandidateManager, build_manifest, _atomic_copy, _atomic_remove
from makewand.git_helper import (ShadowWorktreeResult, clone_isolated_worktree,
    create_private_shadow_dir, find_git_root, run_git_cmd, get_git_diff)
from makewand.protected_files import ProtectedFiles


def create_native_shadow_worktree(base_dir, prefix="shadow"):
    base = Path(base_dir).resolve()
    repo = Path(find_git_root(base) or base).resolve()
    shadow = create_private_shadow_dir(repo.name)
    try:
        clone_isolated_worktree(str(repo), shadow)
        code, baseline, error = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(shadow))
        if code:
            raise OSError(error)
        head_code, head, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(repo))
        effective = shadow / base.relative_to(repo)
        return ShadowWorktreeResult(str(effective), None, lambda: shutil.rmtree(shadow, ignore_errors=True),
            baseline_commit=baseline.strip(), repo_head=head.strip() if head_code == 0 else None,
            repo_root=str(repo), worktree_root=str(shadow))
    except BaseException:
        shutil.rmtree(shadow, ignore_errors=True)
        raise


def prepare_native_delivery(original_cwd, shadow, protected):
    source = Path(shadow.worktree_root)
    host = Path(shadow.repo_root)
    relative = Path(original_cwd).resolve().relative_to(host.resolve())
    protection = protected.to_dict()
    prefix = "" if relative == Path(".") else relative.as_posix() + "/"
    rebased = ProtectedFiles.from_dict({"schema": 1,
        "files": {prefix + name: value for name, value in protection["files"].items()}})
    race_id = "pl_" + uuid.uuid4().hex[:12]
    folder = config.ensure_private_dir(config.CANDIDATES_DIR / race_id)
    try:
        baseline, candidate = folder / "baseline", folder / "agent_a"
        inputs = workspace_snapshot(source)
        host_manifest = build_manifest(host)
        clone_isolated_worktree(str(source), baseline)
        clone_isolated_worktree(str(baseline), candidate)
        rebased.prepare_workspace(baseline)
        rebased.prepare_workspace(candidate)
        if workspace_snapshot(baseline) != inputs or workspace_snapshot(candidate) != inputs:
            raise OSError("Native delivery baseline cannot reproduce every pipeline input")
        rebased.verify(host)
        code, revision, error = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(candidate))
        if code:
            raise OSError(error)
        return {"race_id": race_id, "folder": folder, "baseline": baseline,
            "candidate": candidate, "source": source, "host": host,
            "baseline_commit": revision.strip(), "baseline_inputs": inputs,
            "host_manifest": host_manifest, "protection": rebased}
    except BaseException:
        shutil.rmtree(folder, ignore_errors=True)
        raise


def seal_native_delivery(capsule, prompt, reviewed_inputs, review_report, test_details):
    source, candidate = capsule["source"], capsule["candidate"]
    if workspace_snapshot(source) != reviewed_inputs:
        raise OSError("Pipeline changed after independent review")
    baseline = capsule["baseline_inputs"]
    for relative in sorted(baseline.keys() | reviewed_inputs.keys()):
        record = reviewed_inputs.get(relative)
        if record == baseline.get(relative):
            continue
        if record is None:
            _atomic_remove(str(candidate), relative)
        elif record[0] == "file":
            _atomic_copy(str(candidate), relative, source / relative,
                {"sha256": record[1], "mode": record[2]})
        else:
            raise OSError("Native delivery requires regular reviewed files")
    if workspace_snapshot(source) != reviewed_inputs or workspace_snapshot(candidate) != reviewed_inputs:
        raise OSError("Native delivery does not match the tested and reviewed inputs")
    capsule["protection"].verify(candidate)
    diff = get_git_diff(str(candidate), base_rev=capsule["baseline_commit"])
    CandidateManager.save_race(capsule["race_id"], prompt, str(capsule["host"]),
        capsule["baseline_commit"], {"path": str(candidate), "success": True,
            "test_passed": True, "test_details": test_details, "review_passed": True,
            "baseline_commit": capsule["baseline_commit"], "diff": diff,
            "manifest": build_manifest(candidate)}, {}, judge_report=review_report, winner="A",
        baseline_dir=capsule["baseline"], baseline_manifest=capsule["host_manifest"],
        frozen_baseline_manifest=build_manifest(capsule["baseline"]),
        protected_files=capsule["protection"].to_dict())
    return capsule["race_id"]
