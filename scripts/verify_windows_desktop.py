#!/usr/bin/env python3
"""Ordinary-user desktop acceptance; prepare never asserts manual success."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))
from windows_verification_env import clean_checkout, isolated_env
from windows_test_gate import write_json

CASES = ("launch_and_help", "navigation_and_resize", "unicode_edit_without_submission", "clean_exit_and_terminal_restore")


def file_hash(path):
    if path.is_symlink() or not path.is_file():
        raise ValueError("a regular, non-symlink input is required")
    return hashlib.sha256(path.read_bytes()).hexdigest()


def bundle_inputs(binary):
    values = {"makewand.exe": file_hash(binary)}
    library = binary.parent / "lib"
    if library.is_symlink() or not library.is_dir():
        raise ValueError("use an extracted release bundle with its Python lib directory")
    total = 0
    for path in sorted(library.rglob("*")):
        if "__pycache__" in path.parts:
            continue
        if path.is_symlink():
            raise ValueError("bundle library symlinks are not accepted")
        if path.is_file():
            total += path.stat().st_size
            if len(values) > 10000 or total > 64 * 1024 * 1024:
                raise ValueError("bundle library exceeds verification limits")
            values[path.relative_to(binary.parent).as_posix()] = file_hash(path)
    if len(values) == 1:
        raise ValueError("empty bundle library")
    return values


def source_inputs(source, commit, env):
    clean_checkout(source, commit, env)
    paths = subprocess.check_output(["git", "ls-files", "-z"], cwd=source, env=env).decode("utf-8").split("\0")
    values = {}
    for name in filter(None, paths):
        rel = Path(name)
        if rel.is_absolute() or ".." in rel.parts:
            raise ValueError("unsafe tracked source path")
        if set(rel.parts) & {".git", ".aws", ".codex", ".agents"}:
            continue
        if rel.suffix in (".go", ".py", ".json", ".yml", ".yaml", ".mod", ".sum"):
            if (source / rel).resolve() != source / rel or any((source / Path(*rel.parts[:i])).is_symlink() for i in range(1, len(rel.parts))):
                raise ValueError("source symlink traversal is not accepted")
            values[name] = file_hash(source / rel)
    return values


def desktop_capabilities(proof, output, env):
    if os.name != "nt":
        raise ValueError("actual native Windows desktop is required")
    if not sys.stdin.isatty() or not sys.stdout.isatty():
        raise ValueError("use an interactive desktop terminal; redirected stdio is not acceptance")
    command = [str(proof), "--desktop", "--storage", str(output)]
    result = subprocess.run(command, env=env, capture_output=True, timeout=30, check=True)
    native = json.loads(result.stdout)
    required = ("native_windows", "non_system", "non_elevated", "non_winpe_workstation")
    if not all(native.get(key) is True for key in required) or native.get("actual_go_temp_filesystem") != "NTFS" or native.get("storage_filesystem") != "NTFS":
        raise ValueError("ordinary desktop/actual Go temp NTFS proof failed")
    with tempfile.TemporaryDirectory(dir=env["TEMP"]) as temp:
        child = subprocess.run([str(proof), "--desktop", "--storage", temp], env=env, capture_output=True, timeout=30, check=True)
        if json.loads(child.stdout).get("storage_filesystem") != "NTFS":
            raise ValueError("actual Python-created temp is not NTFS")
    return native


def check_binding(report, output):
    plan = json.loads((output / "local-launch-plan.json").read_text(encoding="utf-8"))
    if file_hash(output / "local-launch-plan.json") != report["local_launch_plan_sha256"]:
        raise ValueError("local launch plan changed")
    binary, source, proof, stub = (Path(plan[key]) for key in ("binary", "source", "proof", "stub"))
    env = isolated_env(output / "state", stub_codex=True)
    env["PATH"] = str(output / "bin") + os.pathsep + env.get("PATH", "")
    env["MAKEWAND_DESKTOP_STUB_LOG"] = str(output / "stub-events.jsonl")
    env["MAKEWAND_HOME"] = str(binary.parent)
    if bundle_inputs(binary) != report["bundle_inputs"] or source_inputs(source, report["commit"], env) != report["source_inputs"] or file_hash(proof) != report["proof_sha256"] or file_hash(stub) != report["stub_sha256"]:
        raise ValueError("verification inputs changed; prepare a new result")
    capabilities = desktop_capabilities(proof, output, env)
    if capabilities != report["capabilities"]:
        raise ValueError("desktop user/runtime capabilities changed")
    return binary, env


def final_state(report, categories):
    cases = report.get("manual_cases")
    runs = report.get("ui_runs")
    if not isinstance(cases, dict) or set(cases) != set(CASES) or not isinstance(runs, list):
        return "failed"
    if not runs or any(not isinstance(run, dict) or type(run.get("exit")) is not int or run["exit"] != 0 for run in runs) or "blocked_invocation" in categories:
        return "failed"
    for result in cases.values():
        if result is None:
            continue
        if not isinstance(result, dict) or set(result) != {"result", "observation"} or result["result"] not in ("pass", "fail") or not isinstance(result["observation"], str) or not result["observation"].strip() or len(result["observation"]) > 2000:
            return "failed"
        if result["result"] == "fail":
            return "failed"
    if any(result is None for result in cases.values()):
        return "pending_manual"
    return "manual_desktop_passed"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("operation", choices=("prepare", "launch", "record", "finalize"))
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--source", type=Path, default=ROOT)
    parser.add_argument("--commit")
    parser.add_argument("--expected-binary-sha256")
    parser.add_argument("--proof", type=Path)
    parser.add_argument("--stub", type=Path)
    parser.add_argument("--case", choices=CASES)
    parser.add_argument("--result", choices=("pass", "fail"))
    parser.add_argument("--observation")
    args = parser.parse_args()
    if os.name != "nt":
        raise ValueError("actual Windows desktop is required; this platform cannot pass")
    output = args.output.resolve()
    path = output / "desktop-result.json"
    if args.operation == "prepare":
        if not all((args.binary, args.commit, args.expected_binary_sha256, args.proof, args.stub)):
            raise ValueError("prepare requires exact commit, binary SHA, native proof and compiled stub")
        output.mkdir(parents=True, exist_ok=False)
        source, binary, proof = args.source.resolve(), args.binary.resolve(), args.proof.resolve()
        env = isolated_env(output / "state", stub_codex=True)
        sources = source_inputs(source, args.commit, env)
        sys.path.insert(0, str(source))
        from makewand.native_windows import ensure_private_directory
        ensure_private_directory(output)
        for directory in (output / "state", output / "state/home", output / "state/config", output / "state/codex", output / "state/tmp", output / "bin", output / "work"):
            directory.mkdir(exist_ok=True)
            ensure_private_directory(directory)
        stub = output / "bin/codex.exe"
        stub.write_bytes(args.stub.read_bytes())
        if file_hash(binary) != args.expected_binary_sha256:
            raise ValueError("release binary differs from the provided expected SHA256")
        capabilities = desktop_capabilities(proof, output, env)
        report = {"schema": 1, "state": "pending_manual", "scope": "stub_frontend_only", "live_provider_verified": False,
                  "commit": args.commit, "bundle_inputs": bundle_inputs(binary), "source_inputs": sources,
                  "proof_sha256": file_hash(proof), "stub_sha256": file_hash(stub), "capabilities": capabilities,
                  "manual_cases": dict.fromkeys(CASES), "ui_runs": []}
        # This private launch plan may contain local home paths; share the result
        # JSON and hashes rather than uploading the entire output directory.
        write_json(output / "local-launch-plan.json", {"binary": str(binary), "source": str(source), "proof": str(proof), "stub": str(stub)})
        report["local_launch_plan_sha256"] = file_hash(output / "local-launch-plan.json")
        write_json(path, report)
        print("Prepared pending_manual. No desktop interaction has passed yet.")
        return 0
    report = json.loads(path.read_text(encoding="utf-8"))
    binary, env = check_binding(report, output)
    if args.operation == "launch":
        print("Offline UI fixture: only /help, navigation, resizing, unsent editing and Ctrl+C. Do not submit natural language.")
        started = time.monotonic()
        status = subprocess.run([str(binary), "--mode", "fast"], cwd=output / "work", env=env, check=False).returncode
        check_binding(report, output)  # bind the actual completed UI run too
        report["ui_runs"].append({"exit": status, "elapsed_seconds": round(time.monotonic() - started, 3)})
        report["state"] = "pending_manual" if status == 0 else "failed"
    elif args.operation == "record":
        if not report["ui_runs"] or not args.case or not args.result or not args.observation or len(args.observation) > 2000:
            raise ValueError("record requires a real launch plus case/result/bounded observation")
        report["manual_cases"][args.case] = {"result": args.result, "observation": args.observation}
        report["state"] = "failed" if args.result == "fail" else "pending_manual"
    else:
        events_path = output / "stub-events.jsonl"
        if not events_path.is_file():
            raise ValueError("missing real fixture invocation evidence")
        events = [json.loads(line) for line in events_path.read_text(encoding="utf-8").splitlines()]
        if any(set(row) != {"category", "exit"} or row["category"] not in ("version", "synthetic_auth_status", "blocked_invocation") or type(row["exit"]) is not int or row["exit"] != (97 if row["category"] == "blocked_invocation" else 0) for row in events):
            raise ValueError("invalid fixture invocation evidence")
        categories = [row["category"] for row in events]
        if "version" not in categories:
            raise ValueError("no real frontend availability check reached the isolated stub")
        report["stub_event_counts"] = {key: categories.count(key) for key in set(categories)}
        report["stub_events_sha256"] = file_hash(events_path)
        report["state"] = final_state(report, categories)
    write_json(path, report)
    print(report["state"])
    if report["state"] == "failed":
        return 1
    return 2 if args.operation == "finalize" and report["state"] == "pending_manual" else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print("Desktop verification refused: " + str(error), file=sys.stderr)
        sys.exit(1)
