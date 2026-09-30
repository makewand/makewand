#!/usr/bin/env python3
"""Reproducible local benchmark harness. No provider is invoked without --execute."""
import argparse
import hashlib
import json
import math
import os
import random
import shutil
import queue
import selectors
import stat
import subprocess
import sys
import time
import threading
import tempfile
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parent
# Load only repository-owned helpers even when launched with python -I.
sys.path.insert(0, str(ROOT.parent))


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


OUTPUT_LIMIT = 16 * 1024 * 1024
CLI_VERSION_COMMANDS = {name: [name, "--version"] for name in
                        ("claude", "codex", "agy", "muse", "grok", "aider", "makewand")}
EXECUTION_STAGES = ("prepare", "copy", "generation", "verification", "review", "merge", "apply", "workflow", "provider")
EVENT_FIELDS = {"schema", "event_id", "task_id", "benchmark_run", "stage", "event", "engine",
                "attempt_id", "readonly", "status", "start_unix_ms", "duration_ms", "artifact_digest",
                "error_kind", "tokens", "monetary_cost", "peak_rss_bytes", "account_ref"}


def _read_protocol_text(path, limit):
    """Read a bounded regular file without opening a candidate-supplied FIFO."""
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NONBLOCK", 0) | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(descriptor, "rb") as source:
        info = os.fstat(source.fileno())
        if path.is_symlink() or not stat.S_ISREG(info.st_mode) or info.st_size > limit:
            raise ValueError("measurement input must be a bounded regular file")
        data = source.read(limit + 1)
        if len(data) > limit:
            raise ValueError("measurement input exceeds file protocol bounds")
    return data.decode("utf-8")


def fixture_metadata(cases):
    metadata = {}
    for case in cases:
        item = json.loads((case / "metadata.json").read_text(encoding="utf-8"))
        if (not isinstance(item, dict) or item.get("schema") != 1 or item.get("id") != case.name
                or item.get("risk") not in ("low", "medium", "high")
                or item.get("category") not in ("function-fix", "multi-file-refactor", "input-boundary", "exception-recovery")
                or item.get("preregistered") is not True or not item.get("public_apis")):
            raise ValueError("invalid preregistered fixture metadata: " + case.name)
        metadata[case.name] = item
    return metadata


def evaluation_protocol(cases, overlays=None, suffix=None):
    """Preregister optional visible tests without changing original fixtures."""
    files = {}
    if overlays is not None:
        overlays = overlays.resolve(strict=True)
        if not overlays.is_dir():
            raise ValueError("seed overlays must be a directory")
        for case in cases:
            directory = overlays / case.name
            if not directory.is_dir() or directory.is_symlink():
                raise ValueError("missing regular seed overlay: " + case.name)
            for path in directory.rglob("*"):
                relative = path.relative_to(directory)
                if path.is_symlink() or any(part in (".git", "__pycache__") for part in relative.parts):
                    raise ValueError("seed overlays cannot contain links or repository state")
                if path.is_file():
                    _read_protocol_text(path, 1024 * 1024)
                    if (case / "seed" / relative).exists():
                        raise ValueError("seed overlays cannot replace original source: " + str(relative))
            files[case.name] = snapshot(directory)
            if not files[case.name]:
                raise ValueError("empty seed overlay: " + case.name)
    text = _read_protocol_text(suffix, 65536) if suffix is not None else None
    return {"schema": 2, "protected_file_guard": "task-file-v1", "seed_overlays_path": str(overlays) if overlays is not None else None,
            "seed_overlays": files, "prompt_suffix_path": str(suffix.resolve()) if suffix is not None else None,
            "prompt_suffix_sha256": hashlib.sha256(text.encode()).hexdigest() if text is not None else None}


def protocol_files_unchanged(workspace, expected):
    for name, info in expected.items():
        path = workspace / name
        try:
            contents = _read_protocol_text(path, 1024 * 1024)
            if hashlib.sha256(contents.encode()).hexdigest() != info["sha256"] or stat.S_IMODE(path.stat().st_mode) != info["mode"]:
                return False
        except (OSError, ValueError, UnicodeError):
            return False
    return True


def execution_events(path, run_id, task_id):
    """Validate complete spans before aggregating optional self-reported timings.

    Events never replace independent acceptance or wall-clock measurement.
    A missing end counts as unfinished; its duration remains unknown.
    """
    result = {"status": "missing", "duration_ms_by_stage": None, "span_count_by_stage": None,
              "end_status_count_by_stage": None, "unfinished_span_count_by_stage": None,
              "unfinished_spans": None, "provider_dispatches": None, "measurement_error": None}
    try:
        from makewand.telemetry import validate_event
        try:
            contents = _read_protocol_text(path, 4 * 1024 * 1024)
        except FileNotFoundError:
            return result
        spans, attempts, record_count, missing_attempt = {}, {}, 0, False
        def text_field(value, required=False):
            return value is None and not required or (isinstance(value, str) and 0 < len(value) <= 512 and not any(ord(c) < 32 for c in value))
        def number(value, integer=False):
            return type(value) in ((int,) if integer else (int, float)) and value >= 0 and math.isfinite(value)
        for line in contents.splitlines():
            record_count += 1
            if record_count > 10000 or len(line.encode("utf-8")) > 65536:
                raise ValueError("execution events exceed record protocol bounds")
            event = json.loads(line)
            validate_event(event)
            if not isinstance(event, dict) or set(event) != EVENT_FIELDS or type(event.get("schema")) is not int or event["schema"] != 1:
                raise ValueError("invalid execution event schema")
            if event["benchmark_run"] != run_id or event["task_id"] != task_id:
                raise ValueError("execution event belongs to another trial")
            if event["event"] not in ("start", "end"):
                raise ValueError("invalid execution event stage or kind")
            if not all(text_field(event[key], required=key in ("event_id", "task_id", "benchmark_run"))
                       for key in ("event_id", "task_id", "benchmark_run", "engine", "attempt_id", "error_kind", "account_ref")):
                raise ValueError("invalid execution event identity")
            if type(event["readonly"]) is not bool or not number(event["start_unix_ms"], integer=True):
                raise ValueError("invalid execution event types")
            if event["artifact_digest"] is not None and (not isinstance(event["artifact_digest"], str)
                    or len(event["artifact_digest"]) != 64 or any(c not in "0123456789abcdef" for c in event["artifact_digest"])):
                raise ValueError("invalid execution artifact digest")
            for key, integer in (("monetary_cost", False), ("peak_rss_bytes", True)):
                if event[key] is not None and not number(event[key], integer):
                    raise ValueError("invalid optional execution measurement")
            tokens = event["tokens"]
            if tokens is not None and not number(tokens, integer=True):
                raise ValueError("invalid execution token measurement")
            kind, identity = event["event"], event["event_id"]
            if kind == "start":
                if identity in spans or event["status"] is not None or event["duration_ms"] is not None:
                    raise ValueError("duplicate or malformed execution span start")
                if event["stage"] == "provider":
                    attempt = event["attempt_id"]
                    if attempt is None:
                        missing_attempt = True
                    else:
                        if attempt in attempts:
                            raise ValueError("duplicate provider attempt")
                        attempts[attempt] = identity
                spans[identity] = {"start": event, "end": None}
            else:
                span = spans.get(identity)
                if span is None or span["end"] is not None or not text_field(event["status"], required=True) or not number(event["duration_ms"], integer=True):
                    raise ValueError("unmatched or malformed execution span end")
                for key in ("stage", "engine", "attempt_id", "readonly", "start_unix_ms"):
                    if span["start"][key] != event[key]:
                        raise ValueError("execution span changed identity")
                span["end"] = event
        if not spans:
            return result
        durations, counts, statuses, unfinished = {}, {}, {}, {}
        stages = sorted(set(EXECUTION_STAGES) | {span["start"]["stage"] for span in spans.values()})
        for stage in stages:
            selected = [span for span in spans.values() if span["start"]["stage"] == stage]
            counts[stage] = len(selected) if selected else None
            ended = [span["end"] for span in selected if span["end"] is not None]
            statuses[stage] = ({status: sum(event["status"] == status for event in ended)
                                for status in sorted({event["status"] for event in ended})} if ended else None)
            unfinished[stage] = sum(span["end"] is None for span in selected) if selected else None
            durations[stage] = (sum(span["end"]["duration_ms"] for span in selected)
                                if selected and all(span["end"] is not None for span in selected) else None)
        result.update(status="measured", duration_ms_by_stage=durations, span_count_by_stage=counts,
                      end_status_count_by_stage=statuses, unfinished_span_count_by_stage=unfinished,
                      unfinished_spans=sum(span["end"] is None for span in spans.values()),
                      provider_dispatches=len(attempts) if attempts and not missing_attempt else None)
    except (OSError, ValueError, TypeError, UnicodeError, OverflowError) as error:
        result.update(status="invalid", measurement_error=str(error))
    return result


def _terminate(process):
    if os.name == "nt":
        subprocess.run(["taskkill", "/F", "/T", "/PID", str(process.pid)],
                       capture_output=True, timeout=5)
        try:
            process.wait(timeout=2)
        except subprocess.TimeoutExpired:
            process.kill()
        return
    # This stdlib-only helper checks process identity before signalling and also
    # finds marked children that escaped their parent's session on Linux.
    from makewand.daemon_context import terminate_worker
    terminate_worker(process)


def invoke(argv, workspace, timeout, stdout, stderr, env=None):
    started = time.monotonic()
    deadline = started + timeout
    retained, dropped = [0, 0], [0, 0]
    process = None
    selector = None
    readers = []
    stop_capture = threading.Event()
    status, code = "unavailable", None
    capture_error = None
    try:
        with stdout.open("wb") as out, stderr.open("wb") as err:
            marker = uuid.uuid4().hex
            process_env = dict(os.environ if env is None else env,
                               MAKEWAND_DAEMON_REQUEST_ID=marker)
            process = subprocess.Popen(argv, cwd=workspace, stdin=subprocess.DEVNULL,
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                       env=process_env, start_new_session=os.name != "nt")
            process.request_id = marker
            streams = [process.stdout, process.stderr]
            destinations = [out, err]
            if os.name != "nt":
                selector = selectors.DefaultSelector()
                for index, stream in enumerate(streams):
                    os.set_blocking(stream.fileno(), False)
                    selector.register(stream, selectors.EVENT_READ, index)
                def receive(wait):
                    chunks = []
                    for key, _ in selector.select(wait):
                        chunk = os.read(key.fileobj.fileno(), 65536)
                        if chunk:
                            chunks.append((key.data, chunk))
                        else:
                            selector.unregister(key.fileobj)
                            key.fileobj.close()
                    return chunks
                def pending():
                    return bool(selector.get_map())
            else:
                # Windows pipes are not selectable. Reader threads never own or
                # write the result files; a bounded queue bounds memory as well.
                messages = queue.Queue(maxsize=32)
                closed = set()
                def capture(stream, index):
                    try:
                        while not stop_capture.is_set():
                            chunk = stream.read(65536)
                            while not stop_capture.is_set():
                                try:
                                    messages.put((index, chunk), timeout=.05)
                                    break
                                except queue.Full:
                                    pass
                            if not chunk:
                                break
                    finally:
                        stream.close()
                for index, stream in enumerate(streams):
                    reader = threading.Thread(target=capture, args=(stream, index), daemon=True)
                    reader.start()
                    readers.append(reader)
                def receive(wait):
                    try:
                        index, chunk = messages.get(timeout=wait)
                    except queue.Empty:
                        return []
                    if not chunk:
                        closed.add(index)
                        return []
                    return [(index, chunk)]
                def pending():
                    return len(closed) < 2
            exited_at = None
            cleanup_deadline = None
            while True:
                now = time.monotonic()
                code = process.poll()
                if code is not None and exited_at is None:
                    exited_at = now
                if code is not None and not pending():
                    if cleanup_deadline is None:
                        status = "completed"
                    break
                if cleanup_deadline is None:
                    if now >= deadline:
                        status = "timeout" if code is None else "incomplete_output"
                    elif exited_at is not None and now - exited_at >= .25:
                        status = "incomplete_output"
                    else:
                        status = None
                    if status is not None:
                        _terminate(process)
                        cleanup_deadline = time.monotonic() + 1
                elif now >= cleanup_deadline:
                    break
                for index, chunk in receive(.05):
                    keep = min(len(chunk), max(0, OUTPUT_LIMIT - retained[index]))
                    destinations[index].write(chunk[:keep])
                    retained[index] += keep
                    dropped[index] += len(chunk) - keep
    except OSError as error:
        capture_error = str(error)
        stderr.write_text(capture_error, encoding="utf-8")
        status = "unavailable"
    finally:
        stop_capture.set()
        if process is not None:
            _terminate(process)
            code = process.poll()
        if selector is not None:
            selector.close()
            for stream in (process.stdout, process.stderr):
                if not stream.closed:
                    stream.close()
        for reader in readers:
            reader.join(timeout=.2)
        # Reader threads on Windows close only their own pipes. They cannot race
        # with a closed destination file even if a descendant escaped taskkill.
    return {"status": status, "exit_code": code, "seconds": time.monotonic() - started,
            "stdout_dropped_bytes": dropped[0], "stderr_dropped_bytes": dropped[1],
            "capture_error": capture_error}


def repository_metadata():
    def git(*args):
        result = subprocess.run(["git", "-c", "core.fsmonitor=", "-C", str(ROOT.parent), *args],
                                capture_output=True, text=True, timeout=5)
        return result.stdout.strip() if result.returncode == 0 else None
    try:
        revision = git("rev-parse", "HEAD")
        changes = git("status", "--porcelain")
        return {"revision": revision, "dirty": bool(changes) if changes is not None else None}
    except (OSError, subprocess.TimeoutExpired):
        return {"revision": None, "dirty": None}


def acceptance_metadata(cases):
    paths = [ROOT / "acceptance.py", ROOT / "acceptance_worker.py"]
    paths.extend(case / "accept.py" for case in cases)
    return {str(path.relative_to(ROOT)): {"sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
                                          "mode": stat.S_IMODE(path.stat().st_mode)} for path in paths}


def cli_versions(names):
    versions = {}
    clean_env = {key: value for key, value in os.environ.items()
                 if not key.endswith(("_API_KEY", "_AUTH_TOKEN"))}
    for name in names:
        executable = shutil.which(name)
        if executable is None:
            versions[name] = {"path": None, "version": None, "status": "unavailable"}
            continue
        with tempfile.TemporaryDirectory(prefix="makewand-cli-version-") as temporary:
            directory = Path(temporary)
            result = invoke([executable, "--version"], directory, 5,
                            directory / "stdout", directory / "stderr", env=clean_env)
            # Keep the actual version line, bounded; no auth status or model
            # command is used for this opt-in inventory.
            lines = (directory / "stdout").read_text(errors="replace").splitlines()
            versions[name] = {"path": executable, "version": lines[0][:512] if lines else None,
                              "status": result["status"], "exit_code": result["exit_code"]}
    return versions


def budget_metadata(path, run_id):
    if path is None:
        return None, None, None
    try:
        from makewand.call_budget import BudgetError, _reject_nonfinite_json, _validate
        try:
            ledger = json.loads(_read_protocol_text(path, 16 * 1024 * 1024), parse_constant=_reject_nonfinite_json)
            _validate(ledger)
        except BudgetError as error:
            raise ValueError(str(error)) from error
        if ledger.get("schema") != 1 or not isinstance(ledger.get("attempts"), list):
            raise ValueError("invalid model call budget ledger")
        if not isinstance(ledger.get("maximum"), int) or ledger["maximum"] <= 0:
            raise ValueError("invalid model call budget maximum")
        if any(not isinstance(entry, dict) for entry in ledger["attempts"]):
            raise ValueError("invalid model call budget attempt")
        attempts = [entry for entry in ledger["attempts"] if entry.get("benchmark_run") == run_id]
        metadata = {"maximum": ledger["maximum"], "total_reserved": len(ledger["attempts"]),
                    "unfinished_attempts": sum(entry.get("status") != "completed" for entry in attempts),
                    "unit": "Makewand task dispatch attempts; vendor internal turns are unmeasured"}
        return attempts, metadata, None
    except (OSError, ValueError, AttributeError, TypeError, UnicodeError, OverflowError) as error:
        return None, None, str(error)

def percentile(values, quantile):
    ordered = sorted(values)
    return ordered[max(0, math.ceil(quantile * len(ordered)) - 1)] if ordered else None


def run(args):
    arms = json.loads(args.arms.read_text(encoding="utf-8"))
    if not isinstance(arms, dict) or not arms or any(not isinstance(name, str) or not name.replace("-", "").replace("_", "").isalnum()
                       or not isinstance(command, list) or not command or not all(isinstance(arg, str) for arg in command)
                       for name, command in arms.items()):
        raise ValueError("arms must map simple names to nonempty argv lists")
    cases = sorted(path for path in (ROOT / "fixtures").iterdir() if path.is_dir())
    registered_tasks = fixture_metadata(cases)
    if getattr(args, "cases", None):
        unknown = set(args.cases) - {case.name for case in cases}
        if unknown:
            raise ValueError("unknown benchmark cases: " + ", ".join(sorted(unknown)))
        cases = [case for case in cases if case.name in args.cases]
    if getattr(args, "risk", None):
        cases = [case for case in cases if registered_tasks[case.name]["risk"] in args.risk]
    if not cases:
        raise ValueError("no fixed fixtures match the selected cases and risks")
    registered_tasks = {case.name: registered_tasks[case.name] for case in cases}
    overlays = getattr(args, "seed_overlays", None)
    suffix = getattr(args, "prompt_suffix_file", None)
    protocol = evaluation_protocol(cases, overlays, suffix)
    schedule = [(case, arm, repeat) for repeat in range(args.repeats) for case in cases for arm in arms]
    random.Random(args.seed).shuffle(schedule)
    plan = {"schema": 1, "run_id": uuid.uuid4().hex, "seed": args.seed, "repeats": args.repeats,
            "time_budget_seconds": args.timeout, "acceptance_timeout_seconds": args.acceptance_timeout, "arms": arms,
            "evidence_kind": args.evidence_kind,
            "repository": repository_metadata(), "trusted_acceptance": acceptance_metadata(cases),
            "registered_tasks": registered_tasks, "risk_selection": getattr(args, "risk", None),
            "execution_events": bool(getattr(args, "execution_events", False)),
            "evaluation_protocol": protocol,
            "execution_contract_sha256": hashlib.sha256((ROOT.parent / "makewand/execution_contract.json").read_bytes()).hexdigest(),
            "cli_versions_requested": args.record_cli_versions, "cli_versions": {},
            "statistics": {"fixture_count": len(cases), "arm_count": len(arms),
                           "runs_per_arm": len(cases) * args.repeats, "total_runs": len(schedule),
                           "quantile_method": "nearest rank",
                           "interpretation": "Exploratory results on fixed deterministic Python tasks do not establish general model capability.",
                           "offline_results": "Stub results validate the harness only; they are not model evidence."},
            "budget": {"path": str(args.budget_file.resolve()) if args.budget_file else None,
                       "configured_maximum": args.max_model_calls, "unit": "logical dispatch attempts"},
            "implementation": snapshot(ROOT.parent / "makewand"),
            "adapter_sha256": hashlib.sha256((ROOT / "model_arm.py").read_bytes()).hexdigest(),
            "runner_sha256": hashlib.sha256((ROOT / "runner.py").read_bytes()).hexdigest(),
            "fixtures": snapshot(ROOT / "fixtures"),
            "schedule": [[case.name, arm, repeat] for case, arm, repeat in schedule]}
    if not args.execute:
        print(json.dumps(plan, ensure_ascii=False, indent=2))
        return 0
    plan["cli_versions"] = cli_versions(args.record_cli_versions)
    args.output.mkdir(parents=True, exist_ok=False)
    (args.output / "plan.json").write_text(json.dumps(plan, ensure_ascii=False, indent=2), encoding="utf-8")
    execution_env = dict(os.environ, MAKEWAND_API_POLICY="subscription_only")
    budget_path = getattr(args, "budget_file", None)
    if budget_path:
        execution_env["MAKEWAND_CALL_BUDGET_FILE"] = str(budget_path.resolve())
        if args.max_model_calls:
            execution_env["MAKEWAND_MAX_MODEL_CALLS"] = str(args.max_model_calls)
    acceptance_env = dict(execution_env)
    for field in ("MAKEWAND_EXECUTION_EVENTS_FILE", "MAKEWAND_TASK_ID", "MAKEWAND_BENCHMARK_RUN_ID",
                  "MAKEWAND_CALL_BUDGET_FILE", "MAKEWAND_MAX_MODEL_CALLS", "MAKEWAND_TASK_PROTECTED_PATHS"):
        acceptance_env.pop(field, None)
    rows = []
    for index, (case, arm, repeat) in enumerate(schedule):
        if snapshot(ROOT / "fixtures") != plan["fixtures"] or acceptance_metadata(cases) != plan["trusted_acceptance"]:
            raise ValueError("trusted benchmark inputs changed; stopped before the next trial")
        if (evaluation_protocol(cases, overlays, suffix) != protocol
                or snapshot(ROOT.parent / "makewand") != plan["implementation"]
                or hashlib.sha256((ROOT / "model_arm.py").read_bytes()).hexdigest() != plan["adapter_sha256"]
                or hashlib.sha256((ROOT / "runner.py").read_bytes()).hexdigest() != plan["runner_sha256"]):
            raise ValueError("evaluation protocol or implementation changed; stopped before the next trial")
        directory = args.output / f"{index:04d}-{case.name}-{arm}-{repeat}"
        workspace = directory / "workspace"
        shutil.copytree(case / "seed", workspace)
        if overlays is not None:
            shutil.copytree(overlays / case.name, workspace, dirs_exist_ok=True)
        prompt = (case / "prompt.txt").read_text(encoding="utf-8")
        if suffix is not None:
            prompt += "\n\n" + _read_protocol_text(suffix, 65536)
        (directory / "prompt.txt").write_text(prompt, encoding="utf-8")
        substitutions = {"prompt": prompt, "prompt_file": str((directory / "prompt.txt").resolve()),
                         "workspace": str(workspace.resolve()), "timeout": str(args.timeout),
                         "adapter": str(ROOT / "model_arm.py"), "case": case.name,
                         "risk": registered_tasks[case.name]["risk"]}
        command = [arg.format_map(substitutions) for arg in arms[arm]]
        run_id = plan["run_id"] + ":" + directory.name
        task_id = uuid.uuid4().hex
        run_env = dict(execution_env, MAKEWAND_BENCHMARK_RUN_ID=run_id, MAKEWAND_TASK_ID=task_id)
        declared_paths = sorted(protocol["seed_overlays"].get(case.name, {}))
        if declared_paths:
            run_env["MAKEWAND_TASK_PROTECTED_PATHS"] = json.dumps(declared_paths)
        else:
            run_env.pop("MAKEWAND_TASK_PROTECTED_PATHS", None)
        run_env.pop("MAKEWAND_EXECUTION_EVENTS_FILE", None)
        event_path = directory / "events.jsonl"
        if getattr(args, "execution_events", False):
            run_env["MAKEWAND_EXECUTION_EVENTS_FILE"] = str(event_path.resolve())
        trial_started = time.monotonic()
        generation = invoke(command, workspace, args.timeout, directory / "stdout.txt", directory / "stderr.txt", env=run_env)
        protected_unchanged = protocol_files_unchanged(workspace, protocol["seed_overlays"].get(case.name, {}))

        before = snapshot(workspace)
        acceptance = invoke([sys.executable, "-I", str(case / "accept.py"), str(workspace.resolve())],
                            directory, args.acceptance_timeout, directory / "acceptance.stdout", directory / "acceptance.stderr", env=acceptance_env)
        unchanged = before == snapshot(workspace)
        total_seconds = time.monotonic() - trial_started
        trusted_unchanged = (snapshot(ROOT / "fixtures") == plan["fixtures"]
                             and acceptance_metadata(cases) == plan["trusted_acceptance"]
                             and evaluation_protocol(cases, overlays, suffix) == protocol)
        protected_unchanged = protected_unchanged and protocol_files_unchanged(workspace, protocol["seed_overlays"].get(case.name, {}))
        passed = (generation["status"] == "completed" and generation["exit_code"] == 0
                  and acceptance["status"] == "completed" and acceptance["exit_code"] == 0 and unchanged and trusted_unchanged and protected_unchanged)
        row = {"case": case.name, "arm": arm, "repeat": repeat, "generation": generation,
               "acceptance": acceptance, "artifact_unchanged": unchanged, "passed": passed,
               "trusted_inputs_unchanged": trusted_unchanged, "total_seconds": total_seconds,
               "protocol_files_unchanged": protected_unchanged,
               "risk": registered_tasks[case.name]["risk"], "task_id": task_id, "benchmark_run": run_id,
               "provider_calls": None, "tokens": None, "monetary_cost": None, "artifact": before,
               "api_policy": "subscription_only", "attempts": None,
               "evidence_kind": args.evidence_kind, "budget": None, "measurement_error": None}
        if budget_path:
            attempts, metadata, error = budget_metadata(budget_path, run_id)
            row.update(attempts=attempts, budget=metadata, measurement_error=error)
            row["provider_calls"] = len(attempts) if attempts is not None else None
            if error:
                row["passed"] = False
        row["execution_events"] = execution_events(event_path, run_id, task_id) if getattr(args, "execution_events", False) else None
        row["dispatch_measurement_source"] = "budget-ledger" if row["provider_calls"] is not None else None
        if budget_path is None and row["execution_events"] and row["execution_events"]["provider_dispatches"] is not None:
            row["provider_calls"] = row["execution_events"]["provider_dispatches"]
            row["dispatch_measurement_source"] = "execution-events"
        rows.append(row)
        (directory / "result.json").write_text(json.dumps(row, indent=2), encoding="utf-8")
    summary = {}
    for arm in arms:
        arm_rows = [row for row in rows if row["arm"] == arm]
        durations = [row["generation"]["seconds"] for row in arm_rows]
        accepted = sum(row["passed"] for row in arm_rows)
        dispatches = sum(row["provider_calls"] for row in arm_rows) if all(row["provider_calls"] is not None for row in arm_rows) else None
        measured_events = [row["execution_events"] for row in arm_rows]
        stage_names = set(EXECUTION_STAGES)
        for event in measured_events:
            if event and event["duration_ms_by_stage"]:
                stage_names.update(event["duration_ms_by_stage"])
        stages = {stage: (sum(event["duration_ms_by_stage"][stage] for event in measured_events)
                           if measured_events and all(event and event["duration_ms_by_stage"] and event["duration_ms_by_stage"].get(stage) is not None for event in measured_events)
                           else None) for stage in sorted(stage_names)}
        end_statuses, unfinished_spans = {}, {}
        for stage in sorted(stage_names):
            selected = [event["end_status_count_by_stage"][stage] for event in measured_events
                        if event and event["end_status_count_by_stage"] and event["end_status_count_by_stage"].get(stage) is not None]
            end_statuses[stage] = ({status: sum(counts.get(status, 0) for counts in selected)
                                   for status in sorted({status for counts in selected for status in counts})}
                                  if selected and len(selected) == len(measured_events) else None)
            unfinished_spans[stage] = (sum(event["unfinished_span_count_by_stage"][stage] for event in measured_events)
                                       if measured_events and all(event and event["unfinished_span_count_by_stage"]
                                          and event["unfinished_span_count_by_stage"].get(stage) is not None for event in measured_events) else None)
        summary[arm] = {"runs": len(arm_rows), "accepted": sum(row["passed"] for row in arm_rows),
                        "protocol_integrity_errors": sum(not row["protocol_files_unchanged"] for row in arm_rows),
                        "infrastructure_errors": sum(row["generation"]["status"] != "completed"
                                                    or row["acceptance"]["status"] != "completed"
                                                    or row["measurement_error"] is not None or not row["trusted_inputs_unchanged"] for row in arm_rows),
                        "p50_seconds": percentile(durations, .5), "p95_seconds": percentile(durations, .95),
                        "provider_calls": dispatches,
                        "dispatches_per_accepted_delivery": dispatches / accepted if dispatches is not None and accepted else None,
                        "seconds_per_accepted_delivery": sum(row["total_seconds"] for row in arm_rows) / accepted if accepted else None,
                        "accepted_mean_total_seconds": sum(row["total_seconds"] for row in arm_rows if row["passed"]) / accepted if accepted else None,
                        "stage_duration_ms": stages,
                        "stage_end_status_counts": end_statuses,
                        "unfinished_spans_by_stage": unfinished_spans,
                        "event_measurement_errors": sum(bool(event and event["measurement_error"]) for event in measured_events),
                        "acceptance_rate": sum(row["passed"] for row in arm_rows) / len(arm_rows),
                        "human_revision_seconds": None, "evidence_kind": args.evidence_kind,
                        "distinct_fixtures": len(cases),
                        "interpretation": plan["statistics"]["interpretation"],
                        "unfinished_attempts": sum(row["budget"]["unfinished_attempts"] for row in arm_rows if row["budget"])}
    (args.output / "summary.json").write_text(json.dumps(summary, indent=2), encoding="utf-8")
    print(json.dumps(summary, indent=2))
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--arms", type=Path, default=ROOT / "arms.example.json")
    parser.add_argument("--output", type=Path, default=ROOT / "results" / time.strftime("%Y%m%d-%H%M%S"))
    parser.add_argument("--cases", nargs="+", help="Run only these fixed fixture names")
    parser.add_argument("--risk", nargs="+", choices=("low", "medium", "high"), help="Select preregistered risk groups before execution")
    parser.add_argument("--execution-events", action="store_true", help="Collect optional JSONL spans in a unique file for each trial")
    parser.add_argument("--seed-overlays", type=Path, help="Preregister visible test files by fixture; originals cannot be replaced")
    parser.add_argument("--prompt-suffix-file", type=Path, help="Append the same preregistered protocol instructions to every arm")
    parser.add_argument("--repeats", type=int, default=3)
    parser.add_argument("--seed", type=int, default=20260927)
    parser.add_argument("--timeout", type=int, default=600)
    parser.add_argument("--acceptance-timeout", type=int, default=30)
    parser.add_argument("--execute", action="store_true")
    parser.add_argument("--evidence-kind", choices=("live", "offline", "unclassified"), default="unclassified",
                        help="Provenance label; offline stubs are not model capability evidence")
    parser.add_argument("--record-cli-versions", nargs="+", choices=tuple(CLI_VERSION_COMMANDS), default=[],
                        help="On --execute only, record installed CLI --version output without model requests")
    parser.add_argument("--budget-file", type=Path, help="Shared ledger used by Makewand model task adapters")
    parser.add_argument("--max-model-calls", type=int, help="Hard upper bound for ledger-aware adapters; failures count")
    args = parser.parse_args()
    if min(args.repeats, args.timeout, args.acceptance_timeout) <= 0:
        parser.error("repeats and timeouts must be positive")
    if args.max_model_calls is not None and (args.max_model_calls <= 0 or args.budget_file is None):
        parser.error("--max-model-calls requires a positive value and --budget-file")
    args.output = args.output.resolve()
    return run(args)


if __name__ == "__main__":
    raise SystemExit(main())
