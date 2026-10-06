#!/usr/bin/env python3
"""Offline measurement of the real race preparation path; no provider calls.

Each trial has a fresh Python worker and a private HOME. ``first`` means no
explicit priming, not an empty OS cache. ``repeated`` primes the same source
with one complete untimed three-copy preparation before the measured pass.
"""
import argparse
import hashlib
import itertools
import json
import math
import os
from pathlib import Path
import platform
import shutil
import signal
import subprocess
import sys
import time
import traceback

ROOT = Path(__file__).resolve().parent.parent
CONDITIONS = ("first", "repeated")
SOURCE_KINDS = ("plain", "git")


def write_json(path, value):
    with path.open("x", encoding="utf-8") as stream:
        if os.name != "nt":
            os.fchmod(stream.fileno(), 0o600)
        json.dump(value, stream, ensure_ascii=True, allow_nan=False, indent=2)
        stream.write("\n")


def write_capture(trial, name, value):
    """Private raw diagnostics for this synthetic fixture; event schema is unchanged."""
    with (trial / name).open("xb") as stream:
        if os.name != "nt":
            os.fchmod(stream.fileno(), 0o600)
        stream.write(value)
    return dict(file=name, bytes=len(value), sha256=hashlib.sha256(value).hexdigest())


def percentile(values, quantile):
    """Linear interpolation between ordered observations (also for small n)."""
    if not values:
        return None
    ordered = sorted(values)
    index = (len(ordered) - 1) * quantile
    lo, hi = math.floor(index), math.ceil(index)
    return ordered[lo] + (ordered[hi] - ordered[lo]) * (index - lo)


def private_environment(trial):
    # Do not inherit credential-bearing environment or Makewand policy/state.
    environment = {key: os.environ[key] for key in (
        "PATH", "SYSTEMROOT", "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT") if key in os.environ}
    home, temporary = trial / "home", trial / "tmp"
    home.mkdir(mode=0o700)
    temporary.mkdir(mode=0o700)
    environment.update(HOME=str(home), USERPROFILE=str(home),
        XDG_CONFIG_HOME=str(home / "config"), XDG_DATA_HOME=str(home / "data"),
        XDG_CACHE_HOME=str(home / "cache"), TMPDIR=str(temporary), TMP=str(temporary),
        TEMP=str(temporary), SystemTemp=str(temporary), PYTHONUTF8="1",
        MAKEWAND_API_POLICY="subscription_only", MAKEWAND_ENABLE_PROVIDERS="none",
        MAKEWAND_NO_DAEMON="1", MAKEWAND_TASK_ID="workspace-profile",
        MAKEWAND_BENCHMARK_RUN_ID=trial.name,
        MAKEWAND_EXECUTION_EVENTS_FILE=str(trial / "events.jsonl"))
    return environment


def run_worker(trial, timeout):
    environment = private_environment(trial)
    command = [sys.executable, "-I", str(Path(__file__).resolve()), "--worker", str(trial)]
    process = job = None
    started = time.monotonic()
    timed_out = False
    try:
        if os.name == "nt":
            sys.path.insert(0, str(ROOT))
            from makewand.windows_process import WindowsJob
            job = WindowsJob()
            process = job.start(command, env=environment, stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        else:
            process = subprocess.Popen(command, env=environment, stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
        try:
            stdout, stderr = process.communicate(timeout=timeout)
        except subprocess.TimeoutExpired:
            timed_out = True
            if job is not None:
                job.terminate()
            else:
                os.killpg(process.pid, signal.SIGKILL)
            stdout, stderr = process.communicate(timeout=5)
        return dict(exit_code=process.returncode, timed_out=timed_out,
                    worker_wall_seconds=time.monotonic() - started,
                    stdout=write_capture(trial, "stdout.log", stdout),
                    stderr=write_capture(trial, "stderr.log", stderr))
    finally:
        # Also retire children after normal leader exit; no Git child may linger.
        if job is not None:
            job.close()
        elif process is not None:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        if process is not None and process.poll() is None:
            process.wait(timeout=5)


def peak_memory():
    try:
        import resource
        if sys.platform not in ("linux", "darwin"):
            return dict(worker_peak_rss_bytes=None, largest_terminated_child_peak_rss_bytes=None)
        scale = 1 if sys.platform == "darwin" else 1024
        return dict(worker_peak_rss_bytes=resource.getrusage(resource.RUSAGE_SELF).ru_maxrss * scale,
                    largest_terminated_child_peak_rss_bytes=resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss * scale)
    except (ImportError, OSError):
        return dict(worker_peak_rss_bytes=None, largest_terminated_child_peak_rss_bytes=None)


def prepare_three_copies(source, destination):
    from makewand.candidate import build_manifest
    from makewand.git_helper import clone_isolated_worktree
    from makewand.telemetry import stage
    with stage("prepare", engine="race-host-manifest", readonly=True):
        original = build_manifest(source)
    baseline = destination / "baseline"
    with stage("copy", engine="race-baseline"):
        clone_isolated_worktree(str(source), baseline)
    with stage("prepare", engine="race-baseline-manifest", readonly=True):
        frozen = build_manifest(baseline)
    for name, engine in (("agent_a", "race-candidate-a"), ("agent_b", "race-candidate-b")):
        with stage("copy", engine=engine):
            clone_isolated_worktree(str(baseline), destination / name)
    with stage("prepare", engine="race-host-check", readonly=True):
        if build_manifest(source) != original:
            raise OSError("profile source changed")
    # Verification is outside the preparation timer and spans, and checks the
    # actual bytes/modes. No constant or successful process exit stands in for it.
    return original, frozen


def worker(trial):
    sys.path.insert(0, str(ROOT))
    from makewand.candidate import build_manifest
    from makewand.git_helper import run_git_cmd
    from makewand.telemetry import validate_event
    plan = json.loads((trial / "plan.json").read_text(encoding="utf-8"))
    source = trial / "source"
    source.mkdir(mode=0o700)
    block = bytes(range(256))
    content = (block * math.ceil(plan["file_bytes"] / len(block)))[:plan["file_bytes"]]
    for index in range(plan["file_count"]):
        bucket = source / f"group_{index // 64:04d}"
        bucket.mkdir(mode=0o700, exist_ok=True)
        (bucket / f"file_{index:06d}.bin").write_bytes(content)
    if plan["source_kind"] == "git":
        for arguments in (["init", "-q"], ["config", "user.name", "Profile"],
                          ["config", "user.email", "profile@local"], ["add", "-A"],
                          ["commit", "-q", "--no-verify", "-m", "Profile baseline"]):
            code, _, _ = run_git_cmd(["git", *arguments], cwd=str(source))
            if code:
                raise OSError("profile source baseline failed")
    if plan["condition"] == "repeated":
        events = os.environ.pop("MAKEWAND_EXECUTION_EVENTS_FILE")
        try:
            prepare_three_copies(source, trial / "priming")
        finally:
            os.environ["MAKEWAND_EXECUTION_EVENTS_FILE"] = events
        shutil.rmtree(trial / "priming")
    start = time.monotonic()
    original, frozen = prepare_three_copies(source, trial / "measured")
    seconds = time.monotonic() - start
    if frozen != original or any(build_manifest(trial / "measured" / name) != original
        for name in ("agent_a", "agent_b")):
        raise OSError("profile copy verification failed")
    events = [validate_event(json.loads(line)) for line in (trial / "events.jsonl").read_text(encoding="utf-8").splitlines()]
    starts = {event["event_id"]: event for event in events if event["event"] == "start"}
    ends = {event["event_id"]: event for event in events if event["event"] == "end"}
    if (len(events) != 2 * len(starts) or set(starts) != set(ends)
            or any(event["status"] != "PASSED" for event in ends.values())):
        raise OSError("profile stage measurement incomplete")
    durations = {}
    for event in ends.values():
        key = event["stage"] + "/" + event["engine"]
        durations.setdefault(key, []).append(event["duration_ms"] / 1000)
    result = dict(status="PASSED", preparation_seconds=seconds, file_count=len(original),
                  total_source_bytes=plan["file_count"] * plan["file_bytes"],
                  copies_verified=3, span_count=len(ends), stage_seconds=durations,
                  process_tree_peak_rss_bytes=None,
                  source_manifest_sha256=hashlib.sha256(json.dumps(original, sort_keys=True,
                    separators=(",", ":")).encode()).hexdigest(), **peak_memory())
    # Preserve inputs, raw spans and hashes; large generated bytes are disposable.
    shutil.rmtree(source)
    shutil.rmtree(trial / "measured")
    write_json(trial / "result.json", result)
    return 0


def summarize(trials):
    groups = {}
    for trial in trials:
        plan = trial["plan"]
        key = (plan["file_count"], plan["file_bytes"], plan["source_kind"], plan["condition"])
        groups.setdefault(key, []).append(trial)
    result = []
    for key, entries in groups.items():
        passed = [entry["result"] for entry in entries if (entry.get("result") or {}).get("status") == "PASSED"
                  and entry["process"]["exit_code"] == 0 and not entry["process"]["timed_out"]]
        values = [entry["preparation_seconds"] for entry in passed]
        stages = sorted({name for entry in passed for name in entry["stage_seconds"]})
        stage_metrics = {}
        for name in stages:
            # Total within one stage/engine per trial, e.g. the three Git baselines.
            samples = [sum(entry["stage_seconds"].get(name, [])) for entry in passed]
            stage_metrics[name] = dict(p50_seconds=percentile(samples, .5), p95_seconds=percentile(samples, .95))
        rss = [entry["worker_peak_rss_bytes"] for entry in passed if entry["worker_peak_rss_bytes"] is not None]
        result.append(dict(file_count=key[0], file_bytes=key[1], source_kind=key[2], condition=key[3],
            samples=len(entries), passed=len(passed), p50_seconds=percentile(values, .5),
            p95_seconds=percentile(values, .95), maximum_worker_peak_rss_bytes=max(rss) if rss else None,
            stages=stage_metrics))
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, help="new private artifact directory; must not exist")
    parser.add_argument("--file-counts", nargs="+", type=int, default=[8, 128])
    parser.add_argument("--file-bytes", nargs="+", type=int, default=[1024, 65536])
    parser.add_argument("--source-kinds", nargs="+", choices=SOURCE_KINDS, default=list(SOURCE_KINDS))
    parser.add_argument("--conditions", nargs="+", choices=CONDITIONS, default=list(CONDITIONS))
    parser.add_argument("--repeats", type=int, default=3)
    parser.add_argument("--worker-timeout", type=float, default=180)
    parser.add_argument("--worker", type=Path, help=argparse.SUPPRESS)
    args = parser.parse_args(argv)
    if args.worker is not None:
        try:
            return worker(args.worker)
        except Exception as error:
            # Raw diagnostics stay in private worker stderr; structured metrics
            # and execution events never copy error text or workspace paths.
            traceback.print_exc()
            write_json(args.worker / "result.json", dict(status="FAILED", error_kind=type(error).__name__))
            return 1
    if args.output is None:
        parser.error("--output is required")
    if (not 1 <= args.repeats <= 100 or not math.isfinite(args.worker_timeout)
            or not 1 <= args.worker_timeout <= 3600 or any(not 1 <= count <= 100000 for count in args.file_counts)
            or any(not 1 <= size <= 512 * 1024 * 1024 for size in args.file_bytes)
            or any(count * size > 512 * 1024 * 1024 for count, size in itertools.product(args.file_counts, args.file_bytes))):
        parser.error("workload must be bounded: 1..100 repeats, <=100k files, <=512 MiB per source, 1..3600s worker")
    if len(set(args.file_counts)) != len(args.file_counts) or len(set(args.file_bytes)) != len(args.file_bytes):
        parser.error("workload dimensions must be unique")
    if not shutil.which("git"):
        parser.error("Git is required; no provider CLI is used")
    try:
        args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    except FileExistsError:
        parser.error("output already exists; previous evidence was preserved")
    trials = []
    for index, (count, size, source_kind, condition, repeat) in enumerate(itertools.product(
            args.file_counts, args.file_bytes, args.source_kinds, args.conditions, range(args.repeats))):
        trial = args.output / f"trial_{index:04d}"
        trial.mkdir(mode=0o700)
        plan = dict(file_count=count, file_bytes=size, source_kind=source_kind, condition=condition, repeat=repeat)
        write_json(trial / "plan.json", plan)
        process = run_worker(trial, args.worker_timeout)
        result_path = trial / "result.json"
        result = json.loads(result_path.read_text(encoding="utf-8")) if result_path.exists() else None
        trials.append(dict(trial=trial.name, plan=plan, process=process, result=result))
    report = dict(schema=1, evidence_kind="offline-workspace-preparation", platform=platform.system(),
        python_version=platform.python_version(), provider_dispatches=0, tokens=None, monetary_cost=None,
        conditions=dict(first="newly written source; no explicit priming; OS cache not cleared",
                        repeated="same source primed by one untimed full three-copy pass; OS cache not cleared"),
        timing_scope="source manifest, three safe isolated copies including Git baselines, baseline manifest, host manifest recheck; validation excluded",
        rss_scope="fresh worker lifetime including imports/setup/priming; separate largest terminated child (Git/bwrap), not combined process-tree peak",
        percentile_method="linear interpolation at (n-1)*quantile; passed samples only; small samples descriptive",
        nested_spans_are_additive=False, worker_timeout_seconds=args.worker_timeout,
        source_sha256={name: hashlib.sha256((ROOT / name).read_bytes()).hexdigest() for name in (
            "benchmarks/profile_workspace.py", "makewand/git_helper.py", "makewand/orchestrator.py", "makewand/telemetry.py", "makewand/candidate.py")},
        summary=summarize(trials), trials=trials)
    write_json(args.output / "profile.json", report)
    return 0 if all(trial["process"]["exit_code"] == 0 and (trial.get("result") or {}).get("status") == "PASSED" for trial in trials) else 1


if __name__ == "__main__":
    sys.exit(main())
