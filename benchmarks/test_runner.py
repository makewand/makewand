#!/usr/bin/env python3
"""Offline harness regressions. The fixture writers are stubs, never model evidence."""
import json
import hashlib
import importlib.util
import os
import subprocess
import sys
import tempfile
import shutil
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("benchmark_runner", ROOT / "runner.py")
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)
INTERVALS = '''def merge_intervals(intervals):
    result = []
    for start, end in sorted(intervals):
        if start > end:
            raise ValueError("reversed")
        if result and start <= result[-1][1]:
            result[-1] = (result[-1][0], max(end, result[-1][1]))
        else:
            result.append((start, end))
    return result
'''
SLUGS = '''import re
import unicodedata

def unique_slugs(titles):
    result, used = [], set()
    for title in titles:
        normalized = unicodedata.normalize("NFKD", title).encode("ascii", "ignore").decode().lower()
        base = re.sub("[^a-z0-9]+", "-", normalized).strip("-") or "item"
        slug, suffix = base, 2
        while slug in used:
            slug = f"{base}-{suffix}"
            suffix += 1
        result.append(slug)
        used.add(slug)
    return result
'''


class HarnessTests(unittest.TestCase):
    def test_early_exit_and_spoofed_success_never_pass(self):
        malicious = ["raise SystemExit(0)", "import os; os._exit(0)",
                     "print('independent acceptance passed'); raise SystemExit(0)",
                     "def merge_intervals(x): raise SystemExit(0)\ndef unique_slugs(x): raise SystemExit(0)",
                     "def merge_intervals(x): raise RuntimeError('broken')\ndef unique_slugs(x): raise RuntimeError('broken')"]
        for source in malicious:
            with self.subTest(source=source), tempfile.TemporaryDirectory() as directory:
                workspace = Path(directory)
                for case, module in (("intervals", "intervals.py"), ("slugs", "slugs.py")):
                    (workspace / module).write_text(source)
                    result = subprocess.run([sys.executable, "-I", str(ROOT / "fixtures" / case / "accept.py"), directory],
                                            capture_output=True, timeout=10)
                    self.assertNotEqual(result.returncode, 0)

    def test_independent_acceptance_rejects_successful_wrong_output(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            writer = root / "writer.py"
            writer.write_text(
                "from pathlib import Path\n"
                f"if Path('intervals.py').exists(): Path('intervals.py').write_text({INTERVALS!r})\n"
                f"if Path('slugs.py').exists(): Path('slugs.py').write_text({SLUGS!r})\n",
                encoding="utf-8",
            )
            arms = root / "arms.json"
            arms.write_text(json.dumps({"stub-good": [sys.executable, "-I", str(writer)],
                                       "stub-bad": [sys.executable, "-I", "-c", "print('all tests passed')"]}))
            output = root / "results"
            command = [sys.executable, "-I", str(ROOT / "runner.py"), "--arms", str(arms),
                       "--output", str(output), "--repeats", "1", "--timeout", "10",
                       "--evidence-kind", "offline", "--cases", "intervals", "slugs"]
            plan = subprocess.run(command, check=True, capture_output=True, text=True)
            self.assertEqual(len(json.loads(plan.stdout)["schedule"]), 4)
            self.assertFalse(output.exists(), "dry run launched or created output")
            subprocess.run([*command, "--execute"], check=True, capture_output=True, text=True)
            summary = json.loads((output / "summary.json").read_text())
            self.assertEqual(summary["stub-good"]["accepted"], 2)
            self.assertEqual(summary["stub-bad"]["accepted"], 0)
            self.assertEqual(summary["stub-good"]["evidence_kind"], "offline")
            plan = json.loads((output / "plan.json").read_text())
            self.assertEqual(plan["statistics"]["total_runs"], 4)
            self.assertEqual(plan["statistics"]["fixture_count"], 2)
            self.assertIn("revision", plan["repository"])
            self.assertEqual(len(plan["trusted_acceptance"]), 4)
            self.assertIn("not model evidence", plan["statistics"]["offline_results"])
            for result in output.glob("*/result.json"):
                self.assertIsNone(json.loads(result.read_text())["monetary_cost"])

    def test_model_arm_seed_owns_repository_inside_unrelated_checkout(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            checkout, home = root / "checkout", root / "home"
            checkout.mkdir()
            home.mkdir()
            subprocess.run(["git", "init", str(checkout)], check=True, capture_output=True)
            workspace = checkout / "nested" / "workspace"
            workspace.mkdir(parents=True)
            (workspace / "intervals.py").write_text("def merge_intervals(values): return []\n")
            prompt = root / "prompt.txt"
            prompt.write_text("Fix merge_intervals")
            # No model CLI is on PATH, HOME contains no account state, and the
            # admission ledger is temporary: only repository setup can execute.
            environment = dict(os.environ, HOME=str(home), PATH="/usr/bin:/bin",
                               MAKEWAND_CALL_BUDGET_FILE=str(root / "budget.json"), MAKEWAND_MAX_MODEL_CALLS="1")
            result = subprocess.run([sys.executable, "-I", str(ROOT / "model_arm.py"), "single",
                                     "--workspace", str(workspace), "--prompt-file", str(prompt)],
                                    env=environment, capture_output=True, timeout=15)
            self.assertNotEqual(result.returncode, 0)
            top = subprocess.run(["git", "-C", str(workspace), "rev-parse", "--show-toplevel"],
                                 check=True, capture_output=True, text=True)
            self.assertEqual(Path(top.stdout.strip()), workspace)
            self.assertTrue((workspace / ".git").is_dir())

    def test_capture_is_bounded_and_drains_both_streams(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            size = runner.OUTPUT_LIMIT + 12345
            result = runner.invoke([sys.executable, "-I", "-c",
                                    f"import os; os.write(1, b'a'*{size}); os.write(2, b'b'*{size})"],
                                   root, 10, root / "stdout", root / "stderr")
            self.assertEqual(result["status"], "completed")
            self.assertEqual(result["exit_code"], 0)
            for stream in ("stdout", "stderr"):
                self.assertEqual((root / stream).stat().st_size, runner.OUTPUT_LIMIT)
                self.assertEqual(result[stream + "_dropped_bytes"], 12345)

    @unittest.skipUnless(sys.platform == "linux", "Linux process marker cleanup")
    def test_timeout_and_orphaned_session_cannot_keep_capture_alive(self):
        for parent_exits in (False, True):
            with self.subTest(parent_exits=parent_exits), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                child_pid = root / "child.pid"
                child = "import os,time; time.sleep(30)"
                source = ("import subprocess,sys,time\n"
                          f"p=subprocess.Popen([sys.executable,'-I','-c',{child!r}], start_new_session=True)\n"
                          f"open({str(child_pid)!r},'w').write(str(p.pid))\n" +
                          ("" if parent_exits else "time.sleep(30)\n"))
                threads = set(threading.enumerate())
                result = runner.invoke([sys.executable, "-I", "-c", source], root, .5,
                                       root / "stdout", root / "stderr")
                self.assertEqual(result["status"], "incomplete_output" if parent_exits else "timeout")
                self.assertLess(result["seconds"], 3)
                pid = int(child_pid.read_text())
                proc = Path(f"/proc/{pid}/stat")
                if proc.exists():
                    self.assertIn(proc.read_text().split(")", 1)[1].split()[0], ("Z", "X"))
                self.assertEqual(set(threading.enumerate()), threads)

    def test_cli_versions_are_opt_in_and_dry_run_never_starts_them(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            executable = root / "claude"
            touched = root / "invoked"
            executable.write_text(f"#!{sys.executable}\nfrom pathlib import Path\n"
                                  f"Path({str(touched)!r}).write_text('invoked')\nprint('fake-cli 1.2.3')\n")
            executable.chmod(0o700)
            arms = root / "arms.json"
            arms.write_text(json.dumps({"stub": [sys.executable, "-c", "pass"]}))
            environment = dict(os.environ, PATH=str(root))
            command = [sys.executable, "-I", str(ROOT / "runner.py"), "--arms", str(arms),
                       "--record-cli-versions", "claude", "--output", str(root / "output")]
            result = subprocess.run(command, env=environment, capture_output=True, text=True, check=True)
            self.assertEqual(json.loads(result.stdout)["cli_versions"], {})
            self.assertFalse(touched.exists())
            self.assertFalse((root / "output").exists())
            with mock.patch.dict(os.environ, environment, clear=True):
                versions = runner.cli_versions(["claude"])
            self.assertEqual(versions["claude"]["version"], "fake-cli 1.2.3")
            self.assertEqual(versions["claude"]["exit_code"], 0)

    def test_adapter_substitution_and_budget_attempt_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            writer = root / "budget_stub.py"
            writer.write_text("import sys,time\nfrom pathlib import Path\n"
                              f"sys.path.insert(0,{str(ROOT.parent)!r})\n"
                              "from makewand.call_budget import reserve,complete\n"
                              "for success in (False, True):\n"
                              "    attempt=reserve('stub','standard',readonly=True)\n"
                              "    complete(attempt,success,0.01)\n"
                              f"assert Path(sys.argv[1]) == Path({str(ROOT / 'model_arm.py')!r})\n"
                              f"if Path('intervals.py').exists(): Path('intervals.py').write_text({INTERVALS!r})\n"
                              f"if Path('slugs.py').exists(): Path('slugs.py').write_text({SLUGS!r})\n")
            arms = root / "arms.json"
            arms.write_text(json.dumps({"stub": [sys.executable, "-I", str(writer), "{adapter}"]}))
            ledger = root / "ledger.json"
            ledger.write_text(json.dumps({"schema": 1, "maximum": 5,
                                          "attempts": [{"id": "unrelated", "benchmark_run": "unrelated", "status": "completed"}]}))
            output = root / "results"
            subprocess.run([sys.executable, "-I", str(ROOT / "runner.py"), "--arms", str(arms),
                            "--output", str(output), "--repeats", "1", "--execute",
                            "--budget-file", str(ledger), "--max-model-calls", "5",
                            "--evidence-kind", "offline", "--cases", "intervals", "slugs"], capture_output=True, check=True)
            summary = json.loads((output / "summary.json").read_text())["stub"]
            self.assertEqual(summary["provider_calls"], 4)
            self.assertEqual(summary["accepted"], 2)
            self.assertEqual(summary["unfinished_attempts"], 0)
            self.assertEqual(len(json.loads(ledger.read_text())["attempts"]), 5)
            for path in output.glob("*/result.json"):
                result = json.loads(path.read_text())
                self.assertEqual(result["provider_calls"], 2)
                self.assertEqual(result["budget"]["maximum"], 5)
                self.assertEqual([a["success"] for a in result["attempts"]], [False, True])
                self.assertIsNone(result["tokens"])
                self.assertIsNone(result["monetary_cost"])

    def test_missing_or_invalid_budget_is_unknown_not_zero(self):
        with tempfile.TemporaryDirectory() as directory:
            ledger = Path(directory) / "ledger.json"
            for value in (None, "[]", '{"schema":1,"attempts":[2],"maximum":1}',
                          '{"schema":true,"attempts":[],"maximum":1}',
                          '{"schema":1,"attempts":[],"maximum":true}',
                          '{"schema":1,"attempts":[{}],"maximum":1}',
                          '{"schema":1,"attempts":[{"id":"duplicate"},{"id":"duplicate"}],"maximum":3}',
                          '{"schema":1,"attempts":[],"maximum":1,"holds":[]}',
                          '{"schema":1,"attempts":[],"maximum":1,"holds":{"bad":{"remaining":true,"expires_at":2}}}',
                          '{"schema":1,"attempts":[],"maximum":1,"unknown_extension":NaN}',
                          json.dumps({"schema": 1, "attempts": [], "maximum": 1,
                                      "holds": {"bad": {"remaining": 1, "expires_at": 10 ** 400}}})):
                with self.subTest(value=value):
                    if value is not None:
                        ledger.write_text(value)
                    attempts, metadata, error = runner.budget_metadata(ledger, "run")
                    self.assertIsNone(attempts)
                    self.assertIsNone(metadata)
                    self.assertTrue(error)

    @unittest.skipUnless(hasattr(os, "mkfifo"), "FIFO protocol test requires POSIX")
    def test_candidate_fifo_measurements_are_rejected_without_blocking(self):
        with tempfile.TemporaryDirectory() as directory:
            fifo = Path(directory) / "measurement"
            os.mkfifo(fifo)
            program = ("import importlib.util,json,sys\nfrom pathlib import Path\n"
                       "spec=importlib.util.spec_from_file_location('runner',sys.argv[1])\n"
                       "module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)\n"
                       "path=Path(sys.argv[2])\n"
                       "attempts,metadata,error=module.budget_metadata(path,'trial')\n"
                       "assert attempts is None and metadata is None and error\n"
                       "events=module.execution_events(path,'trial','task')\n"
                       "assert events['status']=='invalid' and events['provider_dispatches'] is None\n")
            subprocess.run([sys.executable, "-I", "-c", program, str(ROOT / "runner.py"), str(fifo)],
                           capture_output=True, check=True, timeout=2)


class RegisteredFixtureTests(unittest.TestCase):
    def cases(self):
        return sorted(path for path in (ROOT / "fixtures").iterdir() if path.is_dir())

    def acceptance(self, case, mode):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory) / "workspace"
            shutil.copytree(case / "seed", workspace)
            subprocess.run([sys.executable, "-I", str(ROOT / "fixtures/offline_stub.py"),
                            "--case", case.name, "--workspace", str(workspace), "--mode", mode],
                           check=True, capture_output=True, timeout=10)
            return subprocess.run([sys.executable, "-I", str(case / "accept.py"), str(workspace)],
                                  capture_output=True, text=True, timeout=15)

    def test_twelve_tasks_cover_registered_risks_and_preserve_original_evidence(self):
        metadata = runner.fixture_metadata(self.cases())
        self.assertEqual(len(metadata), 12)
        self.assertEqual({risk: sum(item["risk"] == risk for item in metadata.values())
                          for risk in ("low", "medium", "high")}, {"low": 4, "medium": 4, "high": 4})
        self.assertEqual({item["category"] for item in metadata.values()},
                         {"function-fix", "multi-file-refactor", "input-boundary", "exception-recovery"})
        original = {
            "intervals/accept.py": "48ca629f5a7288d9fcf1bf4bc0ef8df4fce13b7dc3c2f65a65f7b2132bdce112",
            "intervals/prompt.txt": "0ab34f0ddc0765a7a94c34279475b67e7e0dcca191c6d7b1273d0f2b1074878a",
            "intervals/seed/intervals.py": "7793d1f970ede19b5cfc9b72d80c2603785ed53542ce8d1587dcd2aa96633a23",
            "slugs/accept.py": "6d973ee20d8e20599e21c5f7b2f433eac67e08fd866e5b2d08705e13837c5155",
            "slugs/prompt.txt": "d7520e50131a1adacb2b110c2e894bd7321eac8e6ff5a24f760b94e2d7866133",
            "slugs/seed/slugs.py": "03bcb85d4df9127d06a94a63665811d79da219c7c1e75d9abdf2b46cfb8ab223",
        }
        for relative, expected in original.items():
            self.assertEqual(hashlib.sha256((ROOT / "fixtures" / relative).read_bytes()).hexdigest(), expected)

    def test_correct_stub_is_independently_accepted_on_every_fixed_task(self):
        for case in self.cases():
            with self.subTest(case=case.name):
                result = self.acceptance(case, "correct")
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_successful_wrong_and_early_exit_stubs_are_rejected_on_every_fixed_task(self):
        for case in self.cases():
            for mode in ("wrong", "early-exit"):
                with self.subTest(case=case.name, mode=mode):
                    self.assertNotEqual(self.acceptance(case, mode).returncode, 0)

    def test_input_mutation_and_timeout_are_rejected(self):
        case = ROOT / "fixtures/intervals"
        mutation = self.acceptance(case, "mutating")
        self.assertNotEqual(mutation.returncode, 0)
        self.assertIn("mutated input", mutation.stderr)
        timed = self.acceptance(case, "timeout")
        self.assertNotEqual(timed.returncode, 0)
        self.assertIn("timed out", timed.stderr)

    def test_multi_file_refactor_rejects_shallow_shared_results(self):
        case = ROOT / "fixtures/deep-config"
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            shutil.copytree(case / "seed", workspace, dirs_exist_ok=True)
            (workspace / "config_merge.py").write_text(
                "def merge_values(payload):\n"
                "    def merge(base, override):\n"
                "        result = dict(base)\n"
                "        for key, value in override.items():\n"
                "            result[key] = merge(base[key], value) if key in base and isinstance(base[key],dict) and isinstance(value,dict) else value\n"
                "        return result\n"
                "    return merge(payload['base'],payload['override'])\n")
            result = subprocess.run([sys.executable, "-I", str(case / "accept.py"), str(workspace)],
                                    capture_output=True, text=True, timeout=15)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("mutated input", result.stderr)

    def test_risk_selection_is_predeclared_and_keeps_equal_budgets(self):
        result = subprocess.run([sys.executable, "-I", str(ROOT / "runner.py"), "--risk", "high",
                                 "--repeats", "2", "--timeout", "33", "--acceptance-timeout", "7"],
                                check=True, capture_output=True, text=True)
        plan = json.loads(result.stdout)
        self.assertEqual(len(plan["registered_tasks"]), 4)
        self.assertTrue(all(task["risk"] == "high" for task in plan["registered_tasks"].values()))
        self.assertEqual(plan["time_budget_seconds"], 33)
        self.assertEqual(plan["acceptance_timeout_seconds"], 7)


class ExecutionEventTests(unittest.TestCase):
    def event(self, identity="span", stage="provider", kind="start", **overrides):
        event = {"schema": 1, "event_id": identity, "task_id": "task", "benchmark_run": "trial",
                 "stage": stage, "event": kind, "engine": "stub", "attempt_id": identity if stage == "provider" else None,
                 "readonly": False, "status": None if kind == "start" else "PASSED", "start_unix_ms": 1,
                 "duration_ms": None if kind == "start" else 10, "artifact_digest": None,
                 "error_kind": None, "tokens": None, "monetary_cost": None, "peak_rss_bytes": None, "account_ref": None}
        event.update(overrides)
        return event

    def measure(self, events):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "events.jsonl"
            if events is not None:
                path.write_text("".join(json.dumps(event) + "\n" for event in events))
            return runner.execution_events(path, "trial", "task")

    def test_optional_missing_measurements_remain_unknown(self):
        for events in (None, [], [self.event(stage="workflow"), self.event(stage="workflow", kind="end")]):
            with self.subTest(events=events):
                result = self.measure(events)
                self.assertIsNone(result["provider_dispatches"])
                self.assertTrue(result["duration_ms_by_stage"] is None or result["duration_ms_by_stage"]["provider"] is None)

    def test_spans_count_failures_and_do_not_add_nested_stages_to_wall_time(self):
        events = [self.event("workflow", "workflow"), self.event("first"),
                  self.event("first", kind="end", status="FAILED", duration_ms=7), self.event("second"),
                  self.event("second", kind="end", duration_ms=11), self.event("workflow", "workflow", "end", duration_ms=20)]
        result = self.measure(events)
        self.assertEqual(result["status"], "measured")
        self.assertEqual(result["provider_dispatches"], 2)
        self.assertEqual(result["duration_ms_by_stage"]["provider"], 18)
        self.assertEqual(result["duration_ms_by_stage"]["workflow"], 20)
        self.assertEqual(result["end_status_count_by_stage"]["provider"], {"FAILED": 1, "PASSED": 1})
        self.assertEqual(result["unfinished_span_count_by_stage"]["provider"], 0)
        self.assertNotIn("total_seconds", result)

    def test_unfinished_span_has_no_invented_duration(self):
        result = self.measure([self.event()])
        self.assertEqual(result["unfinished_spans"], 1)
        self.assertEqual(result["provider_dispatches"], 1)
        self.assertIsNone(result["duration_ms_by_stage"]["provider"])
        self.assertIsNone(result["end_status_count_by_stage"]["provider"])
        self.assertEqual(result["unfinished_span_count_by_stage"]["provider"], 1)

    def test_shared_contract_fixture_and_actual_metadata_only_emitter(self):
        contract = json.loads((ROOT.parent / "makewand/execution_contract.json").read_text())
        event = dict(contract["fixtures"]["event"], benchmark_run="trial", task_id="task")
        start = dict(event, event="start", status=None, duration_ms=None)
        measured = self.measure([start, event])
        self.assertEqual(measured["status"], "measured")
        self.assertEqual(measured["duration_ms_by_stage"]["review"], event["duration_ms"])
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "actual-events.jsonl"
            from makewand.telemetry import stage
            from makewand.execution_runtime import execution_context
            with mock.patch.dict(os.environ, MAKEWAND_EXECUTION_EVENTS_FILE=str(path),
                                 MAKEWAND_TASK_ID="task", MAKEWAND_BENCHMARK_RUN_ID="trial"), \
                 execution_context(task_id="task"):
                with stage("provider", engine="offline-stub", attempt_id="offline-attempt") as span:
                    span.finish("PASSED")
            measured = runner.execution_events(path, "trial", "task")
            self.assertEqual(measured["status"], "measured")
            self.assertEqual(measured["provider_dispatches"], 1)

    def test_actual_sdk_provider_success_or_unknown_does_not_replace_workflow_outcome(self):
        from makewand.execution_contract import ExecutionRequest
        from makewand.execution_runtime import execute, execution_context
        from makewand.telemetry import stage
        for raises, expected in ((False, "PASSED"), (True, "UNKNOWN")):
            with self.subTest(raises=raises), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / "events.jsonl"
                budget = Path(directory) / "budget.json"
                def callback(timeout):
                    if raises:
                        raise RuntimeError("private prompt credential token")
                    return True, "local stub completed", None
                with mock.patch.dict(os.environ, {"MAKEWAND_EXECUTION_EVENTS_FILE": str(path),
                                     "MAKEWAND_TASK_ID": "task", "MAKEWAND_BENCHMARK_RUN_ID": "trial"}, clear=True), \
                     execution_context(task_id="task"):
                    with stage("workflow") as workflow:
                        result = execute(ExecutionRequest(task_id="task", stage="generation", engine="offline-stub",
                                         readonly=True, timeout_ms=1000, budget_file=str(budget), max_model_calls=2), callback)
                        self.assertEqual(result.status, expected)
                        workflow.finish("UNVERIFIED")
                measured = runner.execution_events(path, "trial", "task")
                self.assertEqual(measured["status"], "measured")
                self.assertEqual(measured["provider_dispatches"], 1)
                self.assertEqual(measured["end_status_count_by_stage"]["provider"], {expected: 1})
                self.assertEqual(measured["end_status_count_by_stage"]["workflow"], {"UNVERIFIED": 1})
                self.assertNotIn("private prompt credential token", path.read_text())
                self.assertEqual(len(runner.budget_metadata(budget, "trial")[0]), 1)

    def test_nullable_provider_attempt_preserves_unknown_dispatch_measurement(self):
        measured = self.measure([self.event(attempt_id=None), self.event(kind="end", attempt_id=None)])
        self.assertEqual(measured["status"], "measured")
        self.assertEqual(measured["duration_ms_by_stage"]["provider"], 10)
        self.assertIsNone(measured["provider_dispatches"])

    def test_actual_sdk_without_ledger_has_one_bound_dispatch_and_unknown_cost(self):
        from makewand.execution_contract import ExecutionRequest
        from makewand.execution_runtime import execute, execution_context
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "events.jsonl"
            callbacks = []
            with mock.patch.dict(os.environ, {"MAKEWAND_EXECUTION_EVENTS_FILE": str(path),
                                 "MAKEWAND_TASK_ID": "task", "MAKEWAND_BENCHMARK_RUN_ID": "trial"}, clear=True), \
                 execution_context(task_id="task"):
                result = execute(ExecutionRequest(task_id="task", stage="generation", engine="offline-stub",
                                 readonly=True, timeout_ms=1000),
                                 lambda timeout: (callbacks.append(timeout) is None, "offline stub", None))
            self.assertEqual(result.status, "PASSED")
            self.assertEqual(len(callbacks), 1)
            self.assertTrue(result.attempt_id)
            events = [json.loads(line) for line in path.read_text().splitlines()]
            self.assertEqual(len(events), 2)
            self.assertTrue(all(event["attempt_id"] == result.attempt_id for event in events))
            self.assertTrue(all(event["tokens"] is None and event["monetary_cost"] is None for event in events))
            measured = runner.execution_events(path, "trial", "task")
            self.assertEqual(measured["status"], "measured")
            self.assertEqual(measured["provider_dispatches"], 1)
            self.assertIsNone(result.tokens)
            self.assertIsNone(result.monetary_cost)

    def test_invalid_foreign_duplicate_and_negative_events_have_no_metrics(self):
        invalid = [[self.event(schema=True)], [self.event(benchmark_run="foreign")], [self.event(task_id="foreign")],
                   [self.event(), self.event()], [self.event(kind="end")],
                   [self.event(), self.event(kind="end", duration_ms=-1)],
                   [self.event(), self.event("other", attempt_id="span")],
                   [self.event(prompt="private text")], [self.event(tokens=0.5)],
                   [self.event(), self.event(kind="end", status="bogus")]]
        for events in invalid:
            with self.subTest(events=events):
                result = self.measure(events)
                self.assertEqual(result["status"], "invalid")
                self.assertIsNone(result["provider_dispatches"])
                self.assertIsNone(result["duration_ms_by_stage"])
                self.assertTrue(result["measurement_error"])

    def test_runner_isolates_events_and_includes_failed_trial_cost_per_delivery(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            writer = root / "event_stub.py"
            writer.write_text(
                "import json,os,subprocess,sys\nfrom pathlib import Path\n"
                "case, scenario = sys.argv[1:]\n"
                "mode = 'wrong' if scenario == 'wrong' or scenario == 'mixed' and case == 'slugs' else 'correct'\n"
                f"subprocess.run([sys.executable,'-I',{str(ROOT / 'fixtures/offline_stub.py')!r},'--case',case,'--mode',mode],check=True)\n"
                "path = os.environ.get('MAKEWAND_EXECUTION_EVENTS_FILE')\n"
                "if path and scenario != 'missing':\n"
                "    base = dict(schema=1,task_id=os.environ['MAKEWAND_TASK_ID'],benchmark_run=os.environ['MAKEWAND_BENCHMARK_RUN_ID'],engine='stub',readonly=False,start_unix_ms=1,artifact_digest=None,error_kind=None,tokens=None,monetary_cost=None,peak_rss_bytes=None,account_ref=None)\n"
                "    if scenario == 'invalid': base['benchmark_run'] = 'other-trial'\n"
                "    with open(path,'w') as stream:\n"
                "        for identity,stage in [('first','provider'),('second','provider'),('flow','workflow')]:\n"
                "            for kind in ('start','end'):\n"
                "                event=dict(base,event_id=identity,stage=stage,event=kind,attempt_id=identity if stage=='provider' else None,status=None if kind=='start' else 'FAILED' if identity=='first' else 'PASSED',duration_ms=None if kind=='start' else 3)\n"
                "                stream.write(json.dumps(event)+'\\n')\n")
            inherited = root / "unexpected-parent-events.jsonl"
            environment = dict(os.environ, MAKEWAND_EXECUTION_EVENTS_FILE=str(inherited))
            for scenario in ("mixed", "wrong", "missing", "invalid", "disabled"):
                with self.subTest(scenario=scenario):
                    arms = root / (scenario + ".json")
                    arms.write_text(json.dumps({"offline-stub": [sys.executable, "-I", str(writer), "{case}", scenario]}))
                    output = root / scenario
                    command = [sys.executable, "-I", str(ROOT / "runner.py"), "--arms", str(arms), "--output", str(output),
                               "--cases", "intervals", "slugs", "--repeats", "1", "--execute", "--evidence-kind", "offline"]
                    if scenario != "disabled":
                        command.append("--execution-events")
                    subprocess.run(command, env=environment, check=True, capture_output=True, timeout=30)
                    summary = json.loads((output / "summary.json").read_text())["offline-stub"]
                    rows = [json.loads(path.read_text()) for path in output.glob("*/result.json")]
                    self.assertEqual(len({row["task_id"] for row in rows}), 2)
                    self.assertTrue(all(row["total_seconds"] >= row["generation"]["seconds"] for row in rows))
                    self.assertTrue(all(row["tokens"] is None and row["monetary_cost"] is None for row in rows))
                    if scenario in ("mixed", "wrong"):
                        self.assertEqual(summary["provider_calls"], 4)
                        self.assertEqual(summary["stage_duration_ms"]["provider"], 12)
                        self.assertEqual(summary["stage_duration_ms"]["workflow"], 6)
                        self.assertEqual(summary["stage_end_status_counts"]["provider"], {"FAILED": 2, "PASSED": 2})
                        self.assertEqual(summary["unfinished_spans_by_stage"]["provider"], 0)
                        if scenario == "mixed":
                            self.assertEqual(summary["accepted"], 1)
                            self.assertEqual(summary["dispatches_per_accepted_delivery"], 4)
                            self.assertGreater(summary["seconds_per_accepted_delivery"], summary["accepted_mean_total_seconds"])
                        else:
                            self.assertEqual(summary["accepted"], 0)
                            for field in ("dispatches_per_accepted_delivery", "seconds_per_accepted_delivery", "accepted_mean_total_seconds"):
                                self.assertIsNone(summary[field])
                    else:
                        self.assertEqual(summary["accepted"], 2)
                        self.assertIsNone(summary["provider_calls"])
                        self.assertIsNone(summary["dispatches_per_accepted_delivery"])
                        self.assertTrue(all(value is None for value in summary["stage_duration_ms"].values()))
                        self.assertEqual(summary["event_measurement_errors"], 2 if scenario == "invalid" else 0)
                    if scenario == "disabled":
                        self.assertFalse(list(output.glob("*/events.jsonl")))
            self.assertFalse(inherited.exists(), "a disabled trial inherited a parent's event file")

    def test_trusted_acceptance_input_drift_prevents_false_success(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            copied = root / "benchmarks"
            shutil.copytree(ROOT, copied, ignore=shutil.ignore_patterns("results", "__pycache__"))
            (root / "makewand").symlink_to(ROOT.parent / "makewand", target_is_directory=True)
            writer = root / "tampering_stub.py"
            writer.write_text("from pathlib import Path\n"
                              f"Path({str(copied / 'fixtures/intervals/accept.py')!r}).write_text(\"print('forged acceptance')\\n\")\n")
            arms = root / "arms.json"
            arms.write_text(json.dumps({"tampering-stub": [sys.executable, "-I", str(writer)]}))
            output = root / "output"
            subprocess.run([sys.executable, "-I", str(copied / "runner.py"), "--arms", str(arms), "--output", str(output),
                            "--cases", "intervals", "--repeats", "1", "--execute", "--evidence-kind", "offline"],
                           check=True, capture_output=True, timeout=30)
            row = json.loads(next(output.glob("*/result.json")).read_text())
            self.assertEqual(row["generation"]["exit_code"], 0)
            self.assertEqual(row["acceptance"]["exit_code"], 0)
            self.assertFalse(row["trusted_inputs_unchanged"])
            self.assertFalse(row["passed"])


class EvaluationProtocolTests(unittest.TestCase):
    def test_visible_tests_are_preregistered_and_cannot_be_removed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            overlay = root / "visible" / "intervals"
            overlay.mkdir(parents=True)
            visible = overlay / "test_smoke.py"
            visible.write_text("# common visible test fixture\n")
            suffix = root / "protocol.txt"
            suffix.write_text("Keep the common visible test fixture unchanged.")
            stub = ROOT / "fixtures/offline_stub.py"
            bad = root / "bad.py"
            bad.write_text("import subprocess,sys,json,os\nfrom pathlib import Path\n"
                           "assert json.loads(os.environ['MAKEWAND_TASK_PROTECTED_PATHS']) == ['test_smoke.py']\n"
                           f"subprocess.run([sys.executable,'-I',{str(stub)!r},'--case','intervals','--mode','correct'],check=True)\n"
                           "Path('test_smoke.py').unlink()\n")
            arms = root / "arms.json"
            arms.write_text(json.dumps({"correct": [sys.executable, "-I", str(stub), "--case", "intervals", "--mode", "correct"],
                                       "removed-test": [sys.executable, "-I", str(bad)]}))
            output = root / "output"
            command = [sys.executable, "-I", str(ROOT / "runner.py"), "--arms", str(arms), "--output", str(output),
                       "--cases", "intervals", "--repeats", "1", "--seed-overlays", str(overlay.parent),
                       "--prompt-suffix-file", str(suffix), "--evidence-kind", "offline"]
            plan = json.loads(subprocess.run(command, check=True, capture_output=True, text=True).stdout)
            self.assertFalse(output.exists())
            self.assertEqual(plan["evaluation_protocol"]["seed_overlays"]["intervals"]["test_smoke.py"]["sha256"],
                             hashlib.sha256(visible.read_bytes()).hexdigest())
            subprocess.run([*command, "--execute"], check=True, capture_output=True, timeout=30)
            rows = {json.loads(p.read_text())["arm"]: json.loads(p.read_text()) for p in output.glob("*/result.json")}
            self.assertTrue(rows["correct"]["passed"])
            self.assertEqual(rows["removed-test"]["generation"]["exit_code"], 0)
            self.assertEqual(plan["evaluation_protocol"]["schema"], 2)
            self.assertEqual(plan["evaluation_protocol"]["protected_file_guard"], "task-file-v1")
            self.assertEqual(rows["removed-test"]["acceptance"]["exit_code"], 0)
            self.assertFalse(rows["removed-test"]["protocol_files_unchanged"])
            self.assertFalse(rows["removed-test"]["passed"])
            for path in output.glob("*/prompt.txt"):
                self.assertTrue(path.read_text().endswith(suffix.read_text()))

    def test_overlay_cannot_replace_original_fixture_source(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            overlay = root / "intervals"
            overlay.mkdir()
            (overlay / "intervals.py").write_text("replacement source")
            with self.assertRaisesRegex(ValueError, "cannot replace original source"):
                runner.evaluation_protocol([ROOT / "fixtures/intervals"], root)


if __name__ == "__main__":
    unittest.main(verbosity=2)
