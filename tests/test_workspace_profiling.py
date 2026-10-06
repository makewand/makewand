"""Real offline copy/Git measurements preserve the workflow's business result."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

import test_workflow_scheduling as scheduling
from makewand import git_helper, orchestrator as orch, telemetry
from makewand.candidate import build_manifest
from makewand.execution_contract import EXIT_FAILED, EXIT_PASSED
from makewand.execution_runtime import current_context, execution_context

ROOT = Path(__file__).resolve().parent.parent


def read_events(path):
    return [telemetry.validate_event(json.loads(line)) for line in path.read_text().splitlines()]


class WorkspaceTelemetryTests(unittest.TestCase):
    setUp = scheduling.SchedulingTests.setUp
    fixtures = scheduling.SchedulingTests.fixtures

    def assert_pairs(self, events):
        starts = {event["event_id"]: event for event in events if event["event"] == "start"}
        ends = {event["event_id"]: event for event in events if event["event"] == "end"}
        self.assertEqual(len(events), 2 * len(starts))
        self.assertEqual(set(starts), set(ends))
        for event_id, start in starts.items():
            end = ends[event_id]
            for field in ("task_id", "stage", "engine", "start_unix_ms", "readonly"):
                self.assertEqual(start[field], end[field])
            self.assertGreaterEqual(end["duration_ms"], 0)
        return list(ends.values())

    def assert_metadata_only(self, events):
        serialized = json.dumps(events)
        for value in (str(self.repo), str(self.root), "VALUE =", "Secret prompt", "secret-git-error"):
            self.assertNotIn(value, serialized)
        self.assertTrue(all(event["tokens"] is None and event["monetary_cost"] is None
                            and event["peak_rss_bytes"] is None for event in events))

    def test_race_measures_real_three_copies_without_consuming_extra_budget(self):
        events, ledger = self.root / "events.jsonl", self.root / "ledger.json"
        deadline = int(time.time() * 1000) + 30000
        manifest = build_manifest(self.repo)
        with self.fixtures(), patch.dict(os.environ, {"MAKEWAND_EXECUTION_EVENTS_FILE": str(events),
                "MAKEWAND_MAX_MODEL_CALLS": "3", "MAKEWAND_CALL_BUDGET_FILE": str(ledger)}), \
                execution_context(deadline_unix_ms=deadline):
            result = orch.run_race("Secret prompt", cwd=str(self.repo), timeout=30,
                                   engine_a="claude", engine_b="codex")
        self.assertEqual(result, EXIT_PASSED)
        self.assertEqual(build_manifest(self.repo), manifest)
        self.assertEqual(len(self.calls), 3)
        self.assertEqual(len(json.loads(ledger.read_text())["attempts"]), 3)
        self.assertTrue(all(context["deadline_unix_ms"] <= deadline for _, _, context, _ in self.calls))
        recorded = read_events(events)
        ends = self.assert_pairs(recorded)
        copies = [event for event in ends if event["stage"] == "copy"]
        self.assertEqual({event["engine"] for event in copies},
                         {"race-baseline", "race-candidate-a", "race-candidate-b"})
        self.assertEqual(len(copies), 3)
        git = [event for event in ends if event["engine"] == "git-baseline"]
        self.assertEqual(len(git), 3)
        self.assertTrue(all(event["status"] == "PASSED" for event in copies + git))
        self.assertTrue({"race-host-manifest", "race-baseline-manifest", "race-host-check"}
                        <= {event["engine"] for event in ends})
        self.assert_metadata_only(recorded)

    def test_copy_failure_still_stops_before_generation_and_records_failure(self):
        events, ledger = self.root / "events.jsonl", self.root / "ledger.json"
        error = OSError("copy interrupted")
        with self.fixtures(), patch.dict(os.environ, {"MAKEWAND_EXECUTION_EVENTS_FILE": str(events),
                "MAKEWAND_MAX_MODEL_CALLS": "3", "MAKEWAND_CALL_BUDGET_FILE": str(ledger)}), \
                patch.object(git_helper, "_copy_file_with_reflink", side_effect=error):
            result = orch.run_race("Secret prompt", cwd=str(self.repo), timeout=30,
                                   engine_a="claude", engine_b="codex")
        self.assertEqual(result, EXIT_FAILED)
        self.assertEqual(self.calls, [])
        self.assertEqual(json.loads(ledger.read_text())["attempts"], [])
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 1\n")
        recorded = read_events(events)
        ends = self.assert_pairs(recorded)
        self.assertEqual([event["status"] for event in ends if event["stage"] == "copy"], ["FAILED"])
        self.assert_metadata_only(recorded)

    def test_git_failure_is_propagated_and_no_error_text_enters_spans(self):
        events = self.root / "events.jsonl"
        original = git_helper.run_git_cmd
        def fail_commit(arguments, **kwargs):
            if isinstance(arguments, list) and arguments[1] == "commit":
                return 1, "", "secret-git-error"
            return original(arguments, **kwargs)
        with patch.dict(os.environ, {"MAKEWAND_EXECUTION_EVENTS_FILE": str(events)}), \
                patch.object(git_helper, "run_git_cmd", side_effect=fail_commit):
            with self.assertRaisesRegex(OSError, "secret-git-error"):
                orch._stage_call("copy", git_helper.clone_isolated_worktree,
                                 str(self.repo), self.root / "clone", engine="race-baseline")
        recorded = read_events(events)
        ends = self.assert_pairs(recorded)
        self.assertEqual({(event["stage"], event["status"]) for event in ends},
                         {("copy", "FAILED"), ("prepare", "FAILED")})
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 1\n")
        self.assert_metadata_only(recorded)

    def test_unwritable_event_sink_does_not_change_actual_copy_or_replay(self):
        sink = self.root / "event-directory"
        sink.mkdir()
        original = git_helper.run_git_cmd
        target = self.root / "clone"
        with patch.dict(os.environ, {"MAKEWAND_EXECUTION_EVENTS_FILE": str(sink)}), \
                patch.object(telemetry, "_warned", False), contextlib.redirect_stderr(io.StringIO()) as stderr, \
                patch.object(git_helper, "run_git_cmd", wraps=original) as git:
            result = orch._stage_call("copy", git_helper.clone_isolated_worktree,
                                      str(self.repo), target, engine="race-baseline")
        self.assertIsNone(result)
        self.assertEqual(build_manifest(target), build_manifest(self.repo))
        self.assertEqual(sum(call.args[0][1] == "commit" for call in git.call_args_list), 1)
        self.assertEqual(stderr.getvalue().count("execution telemetry unavailable"), 1)
        self.assertNotIn(str(sink), stderr.getvalue())

    def test_typed_and_malformed_exception_metadata_preserve_the_original_error(self):
        class TypedError(Exception):
            status = "TIMEOUT"
        class InvalidError(Exception):
            status = ["TIMEOUT"]
        class RaisingError(Exception):
            @property
            def execution_status(self):
                raise RuntimeError("metadata property failed")
        events = self.root / "events.jsonl"
        deadline = int(time.time() * 1000) + 30000
        with patch.dict(os.environ, {"MAKEWAND_EXECUTION_EVENTS_FILE": str(events)}), \
                execution_context(stage="workflow", deadline_unix_ms=deadline, lease_id="fixture-lease"):
            before = dict(current_context())
            for exception in (TypedError("raw secret"), InvalidError("raw secret"), RaisingError("raw secret")):
                try:
                    with telemetry.stage("prepare", engine="race-host-manifest", readonly=True):
                        self.assertEqual(current_context()["deadline_unix_ms"], deadline)
                        self.assertEqual(current_context()["lease_id"], "fixture-lease")
                        raise exception
                except Exception as caught:
                    self.assertIs(caught, exception)
                else:
                    self.fail("original exception was swallowed")
                self.assertEqual(current_context(), before)
        ends = self.assert_pairs(read_events(events))
        self.assertEqual([event["status"] for event in ends], ["TIMEOUT", "FAILED", "FAILED"])
        self.assertNotIn("raw secret", events.read_text())


class ProfileCLITests(unittest.TestCase):
    def test_real_offline_profile_has_samples_spans_and_preserves_prior_output(self):
        with tempfile.TemporaryDirectory(prefix="makewand-profile-test-") as directory:
            output = Path(directory) / "measurement"
            command = [sys.executable, str(ROOT / "benchmarks/profile_workspace.py"), "--output", str(output),
                       "--file-counts", "2", "--file-bytes", "64", "--source-kinds", "plain",
                       "--repeats", "2"]
            process = subprocess.run(command, capture_output=True, timeout=60)
            self.assertEqual(process.returncode, 0, process.stderr.decode())
            report_bytes = (output / "profile.json").read_bytes()
            report = json.loads(report_bytes)
            self.assertEqual(report["provider_dispatches"], 0)
            self.assertIsNone(report["tokens"])
            self.assertIsNone(report["monetary_cost"])
            self.assertEqual(len(report["summary"]), 2)
            for group in report["summary"]:
                self.assertEqual((group["samples"], group["passed"]), (2, 2))
                self.assertGreaterEqual(group["p95_seconds"], group["p50_seconds"])
            for trial in report["trials"]:
                self.assertEqual(trial["result"]["copies_verified"], 3)
                self.assertEqual(trial["result"]["span_count"], 9)
                events = read_events(output / trial["trial"] / "events.jsonl")
                self.assertTrue(all(event["status"] == "PASSED" for event in events if event["event"] == "end"))
                self.assertNotIn(str(output), json.dumps(events))
                for stream in ("stdout", "stderr"):
                    capture = trial["process"][stream]
                    log = output / trial["trial"] / capture["file"]
                    self.assertEqual(hashlib.sha256(log.read_bytes()).hexdigest(), capture["sha256"])
                    self.assertEqual(log.stat().st_size, capture["bytes"])
                    if os.name != "nt":
                        self.assertEqual(log.stat().st_mode & 0o777, 0o600)
            again = subprocess.run(command, capture_output=True, timeout=5)
            self.assertEqual(again.returncode, 2)
            self.assertEqual((output / "profile.json").read_bytes(), report_bytes)

    def test_missing_trial_measurement_stays_failed_with_unknown_metrics(self):
        spec = importlib.util.spec_from_file_location("workspace_profile", ROOT / "benchmarks/profile_workspace.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        groups = module.summarize([dict(plan=dict(file_count=2, file_bytes=64, source_kind="plain", condition="first"),
                                         process=dict(exit_code=-9, timed_out=True), result=None)])
        self.assertEqual(groups[0]["passed"], 0)
        self.assertIsNone(groups[0]["p50_seconds"])
        self.assertIsNone(groups[0]["maximum_worker_peak_rss_bytes"])


if __name__ == "__main__":
    unittest.main()
