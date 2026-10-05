#!/usr/bin/env python3
"""Commit-bound native Windows gate. Raw bytes and skips remain reviewable."""
import argparse
import hashlib
import json
import locale
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))
from windows_test_report import ResultError, python_inventory, summarize_go, summarize_python
from windows_verification_env import clean_checkout, isolated_env
MODULES = ["test_native_windows", "test_native_windows_pipeline", "test_native_delivery",
           "test_candidate_recovery", "test_windows_runtime_regression"]
GROUPS = [
    ("python_native", [sys.executable, "-I", "scripts/test_python.py", *MODULES], 2700),
    ("engine", ["go", "test", "-json", "-count=1", "./internal/engine", "-run", "^TestApply|^TestCheckpointIdentity|^TestWindowsApply|^TestEngineProcess|^TestOutputCapture|^TestPreviewProcess|^TestPendingApproval"], 900),
    ("tui", ["go", "test", "-json", "-count=1", "./internal/tui", "-run", "^TestPendingApproval"], 900),
    ("processjob", ["go", "test", "-json", "-count=1", "./internal/processjob"], 900),
    ("remotesession", ["go", "test", "-json", "-count=1", "./internal/remotesession"], 900),
    ("router_raw", ["go", "test", "-json", "-count=1", "./router", "./cmd/versus", "-run", "^TestCLIProcess|^TestRawCLI"], 900),
    ("execution", ["go", "test", "-json", "-count=1", "./execution", "-run", "^Test(LedgerConcurrent|LedgerMaximum|LedgerMalformed|LedgerRejects|ContextCannot|NestedContext|CanonicalExecution|MalformedWire|ContractNullable)"], 900),
    # ./... is essential: no hand-picked package list or test selector here.
    ("race_all", ["go", "test", "-race", "-json", "-count=1", "-timeout=10m", "./..."], 2100),
]


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write_json(path, value):
    temp = path.with_suffix(path.suffix + ".tmp")
    temp.write_text(json.dumps(value, ensure_ascii=True, indent=2) + "\n", encoding="utf-8")
    os.replace(temp, path)


def json_stream(text):
    decoder, values, offset = json.JSONDecoder(), [], 0
    while offset < len(text):
        if text[offset].isspace():
            offset += 1
            continue
        value, offset = decoder.raw_decode(text, offset)
        values.append(value)
    return values


def run_command(name, argv, output, env, timeout):
    started = time.monotonic()
    with (output / (name + ".stdout.log")).open("xb") as stdout, (output / (name + ".stderr.log")).open("xb") as stderr:
        proc = subprocess.Popen(argv, cwd=ROOT, env=env, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr)
        try:
            status = proc.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            # taskkill /T ends the whole timed-out test tree, not just its leader.
            subprocess.run(["taskkill", "/PID", str(proc.pid), "/T", "/F"], capture_output=True, timeout=30, check=False)
            proc.wait(timeout=30)
            status = -1
    safe_argv = ["python" if value == sys.executable else value.replace(str(ROOT), "<checkout>").replace(str(output), "<output>") for value in argv]
    return {"argv": safe_argv, "exit": status, "elapsed_seconds": round(time.monotonic() - started, 3),
            "stdout_sha256": digest(output / (name + ".stdout.log")), "stderr_sha256": digest(output / (name + ".stderr.log"))}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--expected-sha", required=True)
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    summary = {"state": "running", "expected_sha": args.expected_sha, "groups": [], "desktop_interaction": "not_tested"}
    write_json(output / "summary.json", summary)
    try:
        if os.name != "nt":
            raise ValueError("native Windows is required")
        if not re.fullmatch(r"[0-9a-fA-F]{40}", args.expected_sha):
            raise ValueError("expected SHA must be an exact Git commit")
        state = output / "isolated-state"
        env = isolated_env(state)
        env.update(CGO_ENABLED="1", CC="gcc")
        head = clean_checkout(ROOT, args.expected_sha, env)
        compiler = run_command("compiler", ["gcc", "-v"], output, env, 60)
        if compiler["exit"]:
            raise ValueError("native compiler proof failed")
        synchronization = subprocess.check_output(["gcc", "-print-file-name=libsynchronization.a"], env=env).decode().strip()
        if not Path(synchronization).is_absolute() or not Path(synchronization).is_file():
            raise ValueError("MinGW-w64 runtime 8+ synchronization library is missing")
        go_env = json.loads(subprocess.check_output(["go", "env", "-json", "GOOS", "GOARCH", "CGO_ENABLED", "CC", "GOVERSION"], cwd=ROOT, env=env))
        if go_env["GOOS"] != "windows" or go_env["GOARCH"] != "amd64" or go_env["CGO_ENABLED"] != "1":
            raise ValueError("race requires the native Windows/amd64 CGO toolchain")
        build = run_command("proof_build", ["go", "build", "-o", str(output / "runtime-proof.exe"), "scripts/windows_runtime_proof.go"], output, env, 300)
        if build["exit"]:
            raise ValueError("runtime proof build failed")
        proof = run_command("runtime", [str(output / "runtime-proof.exe"), "--storage", str(output)], output, env, 60)
        if proof["exit"]:
            raise ValueError("native NTFS/actual Go temp proof failed")
        summary.update(commit=head, go=go_env, compiler=compiler, native=json.loads((output / "runtime.stdout.log").read_bytes()), python={"version": sys.version, "stdout_encoding": sys.stdout.encoding, "preferred_encoding": locale.getpreferredencoding(False)})
        with tempfile.TemporaryDirectory(dir=state / "tmp") as temp:
            # The same volume proof applies to the actual Python-created temp.
            py_proof = run_command("python_temp", [str(output / "runtime-proof.exe"), "--storage", temp], output, env, 60)
            if py_proof["exit"]:
                raise ValueError("actual Python temp NTFS proof failed")
        listed = run_command("packages", ["go", "list", "-json", "./..."], output, env, 300)
        if listed["exit"]:
            raise ValueError("complete package discovery failed")
        package_info = json_stream((output / "packages.stdout.log").read_text(encoding="utf-8"))
        expected = {p["ImportPath"]: bool(p.get("TestGoFiles") or p.get("XTestGoFiles")) for p in package_info}
        if not expected:
            raise ValueError("no Go packages discovered")
        summary["expected_packages"] = expected
        inventory = python_inventory(ROOT)
        summary["expected_python_methods"] = sorted(inventory)
        for name, argv, timeout in GROUPS:
            print("BEGIN " + name, flush=True)
            row = {"name": name, "state": "running"}
            summary["groups"].append(row)
            write_json(output / "summary.json", summary)
            try:
                row.update(run_command(name, argv, output, env, timeout))
                if name == "python_native":
                    row.update(summarize_python((output / (name + ".stderr.log")).read_bytes(), inventory, locale.getpreferredencoding(False)))
                else:
                    selected = expected if name == "race_all" else {p: has for p, has in expected.items() if any(p.endswith(a[1:]) for a in argv if a.startswith("./"))}
                    row.update(summarize_go((output / (name + ".stdout.log")).read_bytes(), selected, full_race=name == "race_all"))
                if row["exit"]:
                    raise ValueError("test command failed")
                row["state"] = "passed"
            except Exception as error:
                if isinstance(error, ResultError):
                    row.update(error.report)
                row.update(state="failed", error=str(error))
            write_json(output / "summary.json", summary)
            print("END " + name + " " + row["state"], flush=True)
        summary["state"] = "passed" if all(g["state"] == "passed" for g in summary["groups"]) and len(summary["groups"]) == len(GROUPS) else "failed"
        clean_checkout(ROOT, head, env)
    except Exception as error:
        summary.update(state="failed", error=str(error))
    write_json(output / "summary.json", summary)
    step_summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if step_summary:
        with open(step_summary, "a", encoding="utf-8") as stream:
            stream.write("Native Windows gate: **" + summary["state"] + "**\n\n")
            for group in summary["groups"]:
                stream.write("- " + group["name"] + ": " + group["state"] + "; explicit skips: " + str(len(group.get("skips", []))) + "\n")
    return 0 if summary["state"] == "passed" else 1


if __name__ == "__main__":
    sys.exit(main())
