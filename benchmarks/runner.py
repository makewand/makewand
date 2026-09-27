#!/usr/bin/env python3
"""Reproducible local benchmark harness. No provider is invoked without --execute."""
import argparse
import hashlib
import json
import math
import os
import random
import shutil
import signal
import stat
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent


def snapshot(directory):
    result = {}
    for path in sorted(directory.rglob("*")):
        if ".git" in path.parts or "__pycache__" in path.parts:
            continue
        relative = path.relative_to(directory).as_posix()
        if path.is_symlink():
            result[relative] = {"symlink": os.readlink(path)}
        elif path.is_file():
            result[relative] = {"sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
                                "mode": stat.S_IMODE(path.stat().st_mode)}
    return result


def invoke(argv, workspace, timeout, stdout, stderr):
    started = time.monotonic()
    try:
        with stdout.open("wb") as out, stderr.open("wb") as err:
            process = subprocess.Popen(argv, cwd=workspace, stdin=subprocess.DEVNULL,
                                       stdout=out, stderr=err, start_new_session=os.name != "nt")
            try:
                code = process.wait(timeout=timeout)
                status = "completed"
            except subprocess.TimeoutExpired:
                if os.name == "nt":
                    subprocess.run(["taskkill", "/F", "/T", "/PID", str(process.pid)], capture_output=True)
                else:
                    os.killpg(process.pid, signal.SIGKILL)
                process.wait()
                code, status = process.returncode, "timeout"
    except OSError as error:
        stderr.write_text(str(error), encoding="utf-8")
        code, status = None, "unavailable"
    return {"status": status, "exit_code": code, "seconds": time.monotonic() - started}


def percentile(values, quantile):
    ordered = sorted(values)
    return ordered[max(0, math.ceil(quantile * len(ordered)) - 1)] if ordered else None


def run(args):
    arms = json.loads(args.arms.read_text(encoding="utf-8"))
    if not arms or any(not isinstance(name, str) or not name.replace("-", "").replace("_", "").isalnum()
                       or not isinstance(command, list) or not command or not all(isinstance(arg, str) for arg in command)
                       for name, command in arms.items()):
        raise ValueError("arms must map simple names to nonempty argv lists")
    cases = sorted(path for path in (ROOT / "fixtures").iterdir() if path.is_dir())
    schedule = [(case, arm, repeat) for repeat in range(args.repeats) for case in cases for arm in arms]
    random.Random(args.seed).shuffle(schedule)
    plan = {"schema": 1, "seed": args.seed, "repeats": args.repeats,
            "time_budget_seconds": args.timeout, "arms": arms,
            "fixtures": snapshot(ROOT / "fixtures"),
            "schedule": [[case.name, arm, repeat] for case, arm, repeat in schedule]}
    if not args.execute:
        print(json.dumps(plan, ensure_ascii=False, indent=2))
        return 0
    args.output.mkdir(parents=True, exist_ok=False)
    (args.output / "plan.json").write_text(json.dumps(plan, ensure_ascii=False, indent=2), encoding="utf-8")
    rows = []
    for index, (case, arm, repeat) in enumerate(schedule):
        directory = args.output / f"{index:04d}-{case.name}-{arm}-{repeat}"
        workspace = directory / "workspace"
        shutil.copytree(case / "seed", workspace)
        prompt = (case / "prompt.txt").read_text(encoding="utf-8")
        (directory / "prompt.txt").write_text(prompt, encoding="utf-8")
        substitutions = {"prompt": prompt, "prompt_file": str((directory / "prompt.txt").resolve()),
                         "workspace": str(workspace.resolve()), "timeout": str(args.timeout)}
        command = [arg.format_map(substitutions) for arg in arms[arm]]
        generation = invoke(command, workspace, args.timeout, directory / "stdout.txt", directory / "stderr.txt")
        before = snapshot(workspace)
        acceptance = invoke([sys.executable, "-I", str(case / "accept.py"), str(workspace.resolve())],
                            directory, args.acceptance_timeout, directory / "acceptance.stdout", directory / "acceptance.stderr")
        unchanged = before == snapshot(workspace)
        passed = (generation["status"] == "completed" and generation["exit_code"] == 0
                  and acceptance["status"] == "completed" and acceptance["exit_code"] == 0 and unchanged)
        row = {"case": case.name, "arm": arm, "repeat": repeat, "generation": generation,
               "acceptance": acceptance, "artifact_unchanged": unchanged, "passed": passed,
               "provider_calls": None, "tokens": None, "monetary_cost": None, "artifact": before}
        rows.append(row)
        (directory / "result.json").write_text(json.dumps(row, indent=2), encoding="utf-8")
    summary = {}
    for arm in arms:
        arm_rows = [row for row in rows if row["arm"] == arm]
        durations = [row["generation"]["seconds"] for row in arm_rows]
        summary[arm] = {"runs": len(arm_rows), "accepted": sum(row["passed"] for row in arm_rows),
                        "infrastructure_errors": sum(row["generation"]["status"] != "completed" for row in arm_rows),
                        "p50_seconds": percentile(durations, .5), "p95_seconds": percentile(durations, .95)}
    (args.output / "summary.json").write_text(json.dumps(summary, indent=2), encoding="utf-8")
    print(json.dumps(summary, indent=2))
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--arms", type=Path, default=ROOT / "arms.example.json")
    parser.add_argument("--output", type=Path, default=ROOT / "results" / time.strftime("%Y%m%d-%H%M%S"))
    parser.add_argument("--repeats", type=int, default=3)
    parser.add_argument("--seed", type=int, default=20260927)
    parser.add_argument("--timeout", type=int, default=600)
    parser.add_argument("--acceptance-timeout", type=int, default=30)
    parser.add_argument("--execute", action="store_true")
    args = parser.parse_args()
    if min(args.repeats, args.timeout, args.acceptance_timeout) <= 0:
        parser.error("repeats and timeouts must be positive")
    args.output = args.output.resolve()
    return run(args)


if __name__ == "__main__":
    raise SystemExit(main())
