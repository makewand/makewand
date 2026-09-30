"""Offline scheduling integration: no model CLIs or probes are invoked."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import tempfile
import time
import unittest
from unittest.mock import patch

from makewand import orchestrator as orch, cli, call_budget
from makewand.execution_contract import ExecutionResult, EXIT_BUDGET_EXHAUSTED, EXIT_UNVERIFIED, EXIT_PASSED, EXIT_TIMEOUT
from makewand.execution_runtime import current_context, execution_context
from makewand.workflow import choose_workflow, judge_reserve, last_result
from makewand.git_helper import run_git_cmd


class PolicyTests(unittest.TestCase):
    def test_unknown_and_prompt_words_do_not_establish_low_risk(self):
        self.assertEqual(choose_workflow().workflow, "pipeline")
        self.assertEqual(choose_workflow(evidence={"prompt": "typo format only"}).workflow, "pipeline")
        with self.assertRaises(ValueError):
            choose_workflow("single")

    def test_low_single_still_requires_a_separate_review(self):
        plan = choose_workflow("auto", "low")
        self.assertEqual(plan.workflow, "single")
        self.assertFalse(plan.cross_review_required)

    def test_explicit_workflow_wins_and_high_cannot_downgrade(self):
        self.assertEqual(choose_workflow("race", "low").workflow, "race")
        self.assertEqual(choose_workflow("pipeline", "low").workflow, "pipeline")
        with self.assertRaises(ValueError):
            choose_workflow("single", "high")

    def test_only_complete_structured_evidence_establishes_low_risk(self):
        evidence = dict(scope_complete=True, sensitive_interfaces=False,
                        external_side_effects=False, changed_files=2, changed_lines=100)
        self.assertEqual(choose_workflow(evidence=evidence).workflow, "single")
        for field, value in (("changed_files", True), ("changed_lines", 101), ("scope_complete", False)):
            self.assertEqual(choose_workflow(evidence=dict(evidence, **{field: value})).workflow, "pipeline")
        self.assertEqual(choose_workflow(evidence=dict(evidence, sensitive_interfaces=True)).risk, "high")

    def test_judge_reservation_rejects_nonfinite_or_total_budget(self):
        self.assertEqual(judge_reserve(100), 25)
        for value in (-1, float("nan"), float("inf"), 100, True):
            with self.assertRaises(ValueError):
                judge_reserve(100, value)


class SchedulingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="makewand-workflow-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        (self.repo / "app.py").write_text("VALUE = 1\n")
        for arguments in (["init"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-m", "baseline"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(self.repo))
            self.assertEqual(code, 0, error)
        self.calls = []

    @contextlib.contextmanager
    def fixtures(self, engines=("claude", "codex"), callback=None, tests=(True, "fixture verification passed")):
        def provider(engine):
            def execute(prompt, **kwargs):
                self.calls.append((engine, kwargs.get("readonly", False), dict(current_context()), kwargs["timeout"]))
                if callback is not None:
                    value = callback(engine, kwargs)
                    if value is not None:
                        return value
                if kwargs.get("readonly"):
                    verdict = ('MAKEWAND_RACE_VERDICT: {"pass": true, "winner": "A", "defects": []}'
                               if current_context().get("workflow") == "race" else
                               'MAKEWAND_VERDICT: {"pass": true, "defects": []}')
                    return True, verdict, None
                (Path(kwargs["cwd"]) / "app.py").write_text("VALUE = 2\n")
                return True, "implemented", None
            return execute
        with contextlib.ExitStack() as stack:
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch.os, "getloadavg", return_value=(0, 0, 0)))
            stack.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=(True, None)))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={name: {"status": "healthy"} for name in engines}))
            stack.enter_context(patch.object(orch, "select_optimal_engine_pair", return_value=(list(engines), list(reversed(engines)),
                dict(primary_coder=engines[0], primary_reviewer=engines[-1], reasons=[]))))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=lambda name: name in engines))
            stack.enter_context(patch("makewand.config.get_active_providers", return_value=list(engines)))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=tests))
            for name in ("claude", "codex"):
                stack.enter_context(patch.object(orch, "execute_" + name + "_task", side_effect=provider(name)))
            yield

    def test_low_single_makes_two_independent_calls_and_seals_tests(self):
        events = self.root / "events.jsonl"
        with self.fixtures(("claude",)), patch.dict(os.environ, {"MAKEWAND_EXECUTION_EVENTS_FILE": str(events)}):
            result = orch.run_workflow("Implement value", cwd=str(self.repo), force_code=True,
                                       workflow="single", risk="low", auto_fix=False, total_timeout=10)
        self.assertIsInstance(result, tuple)
        self.assertEqual(result.status, "PASSED")
        self.assertEqual([(name, ro) for name, ro, _, _ in self.calls], [("claude", False), ("claude", True)])
        self.assertEqual(len({context["task_id"] for _, _, context, _ in self.calls}), 1)
        ends = [json.loads(line) for line in events.read_text().splitlines() if json.loads(line)["event"] == "end"]
        self.assertTrue({"routing", "generation", "verification", "review", "apply", "workflow"} <= {event["stage"] for event in ends})
        self.assertTrue(all("Implement value" not in json.dumps(event) for event in ends))

    def test_high_with_one_provider_preserves_patch_and_rolls_back(self):
        with self.fixtures(("claude",)):
            result = orch.run_workflow("Implement value", cwd=str(self.repo), force_code=True,
                                       risk="high", auto_fix=False, total_timeout=10)
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertEqual(len(self.calls), 1)
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 1\n")

    def test_local_only_unknown_and_high_do_not_fall_back_to_cloud(self):
        with self.fixtures():
            for risk in ("auto", "high"):
                result = orch.run_workflow("Implement value", cwd=str(self.repo), force_code=True,
                                           local_only=True, risk=risk, auto_fix=False)
                self.assertEqual(result.status, "UNVERIFIED")
        self.assertEqual(self.calls, [])
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 1\n")

    def test_explicit_race_never_ignores_local_only(self):
        with self.fixtures(), patch.object(orch, "run_race") as race:
            result = orch.run_workflow("Implement value", cwd=str(self.repo), workflow="race", local_only=True)
        self.assertEqual(result.status, "INVALID_REQUEST")
        race.assert_not_called()

    def test_explicit_race_never_overrides_negative_write_instructions(self):
        with self.fixtures(), patch.object(orch, "_run_race_impl") as execute:
            result = orch.run_workflow("不要修改，只读分析这个项目", cwd=str(self.repo), workflow="race")
        self.assertEqual(result.status, "INVALID_REQUEST")
        execute.assert_not_called()

    def test_unknown_coder_outcome_stops_fallback_and_rolls_back(self):
        def fail(engine, kwargs):
            (Path(kwargs["cwd"]) / "app.py").write_text("PARTIAL = True\n")
            raise OSError("provider connection interrupted")
        with self.fixtures(callback=fail):
            result = orch.run_workflow("Implement value", cwd=str(self.repo), force_code=True, total_timeout=10)
        self.assertEqual(result.status, "UNKNOWN")
        self.assertEqual(len(self.calls), 1)
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 1\n")

    def test_legacy_runner_timeout_becomes_typed_and_stops_fallback(self):
        with self.fixtures(callback=lambda *_: (False, "partial", "Command timed out after 1.0 seconds")):
            result = orch.run_workflow("Implement value", cwd=str(self.repo), force_code=True, total_timeout=10)
        self.assertEqual(result.status, "TIMEOUT")
        self.assertEqual(len(self.calls), 1)

    def test_direct_command_respects_parent_deadline_and_tuple_compatibility(self):
        timeout_values = []
        with execution_context(deadline_unix_ms=int(time.time() * 1000 + 200)):
            result = cli._execute_direct_task("claude", lambda prompt, **kw: (timeout_values.append(kw["timeout"]) or True, "ok", None), "task", timeout=300)
        self.assertIsInstance(result, tuple)
        self.assertEqual(result.status, "PASSED")
        self.assertGreater(timeout_values[0], 0)
        self.assertLess(timeout_values[0], .25)

    def test_library_budget_without_ledger_is_closed(self):
        with patch.dict(os.environ, {"MAKEWAND_MAX_MODEL_CALLS": "1"}), patch.object(orch, "execute_claude_task") as execute, patch("makewand.config.is_provider_enabled", return_value=True):
            os.environ.pop("MAKEWAND_CALL_BUDGET_FILE", None)
            result = orch.dispatch_task("claude", "task")
        self.assertEqual(result.status, "BUDGET_EXHAUSTED")
        execute.assert_not_called()

    def test_readonly_review_unknown_does_not_try_another_provider(self):
        unknown = ExecutionResult(False, "partial", "outcome unavailable", status="UNKNOWN", outcome_known=False)
        with patch.object(orch, "get_git_diff", return_value="changed"), patch.object(orch, "get_or_update_status", return_value={}), patch.object(orch, "_engine_usable", return_value=(True, "")), patch.object(orch, "dispatch_task", side_effect=[unknown, (True, 'MAKEWAND_VERDICT: {"pass": true, "defects": []}', None)]) as dispatch, contextlib.redirect_stdout(io.StringIO()) as output:
            code = orch.run_review(cwd=str(self.repo), output_json=True)
        self.assertEqual(code, 17)
        self.assertEqual(dispatch.call_count, 1)
        self.assertIs(json.loads(output.getvalue())["pass"], False)

    def test_unknown_verdict_followup_stays_unknown_and_stops(self):
        unknown = ExecutionResult(False, None, "outcome unavailable", status="UNKNOWN", outcome_known=False)
        with patch.object(orch, "get_git_diff", return_value="changed"), patch.object(orch, "get_or_update_status", return_value={}), patch.object(orch, "_engine_usable", return_value=(True, "")), patch.object(orch, "dispatch_task", side_effect=[(True, "Looks good", None), unknown]) as dispatch, contextlib.redirect_stdout(io.StringIO()) as output:
            code = orch.run_review(cwd=str(self.repo), output_json=True)
        self.assertEqual(code, 17)
        self.assertEqual(dispatch.call_count, 2)
        self.assertIs(json.loads(output.getvalue())["pass"], False)

    def test_race_reserves_judge_before_any_generation_and_propagates_thread_context(self):
        ledger = self.root / "ledger.json"
        def callback(engine, kwargs):
            if not kwargs.get("readonly"):
                self.assertEqual(call_budget.remaining_capacity(), 0)
                self.assertLessEqual(kwargs["timeout"], 3)
            return None
        with self.fixtures(callback=callback), patch.dict(os.environ, {
            "MAKEWAND_MAX_MODEL_CALLS": "3", "MAKEWAND_CALL_BUDGET_FILE": str(ledger)}):
            result = orch.run_race("Implement value", cwd=str(self.repo), timeout=4,
                                   judge_reserve_seconds=1, engine_a="claude", engine_b="codex")
        self.assertEqual(result, EXIT_PASSED)
        data = json.loads(ledger.read_text())
        self.assertEqual(len(data["attempts"]), 3)
        self.assertEqual(data.get("holds"), {})
        self.assertEqual(len({context["task_id"] for _, _, context, _ in self.calls}), 1)
        self.assertTrue(all(context.get("deadline_unix_ms") for _, _, context, _ in self.calls))
        self.assertEqual([entry["stage"] for entry in data["attempts"]].count("review"), 1)

    def test_race_insufficient_budget_dispatches_nothing_and_releases_partial_holds(self):
        ledger = self.root / "ledger.json"
        with self.fixtures(), patch.dict(os.environ, {
            "MAKEWAND_MAX_MODEL_CALLS": "2", "MAKEWAND_CALL_BUDGET_FILE": str(ledger)}):
            code = orch.run_race("Implement value", cwd=str(self.repo), engine_a="claude", engine_b="codex")
        self.assertEqual(code, EXIT_BUDGET_EXHAUSTED)
        self.assertEqual(self.calls, [])
        data = json.loads(ledger.read_text())
        self.assertEqual(data.get("holds"), {})
        self.assertEqual(data["attempts"], [])

    def test_late_review_pass_is_rolled_back_before_delivery(self):
        def slow_review(engine, kwargs):
            if kwargs.get("readonly"):
                time.sleep(.15)
            return None
        with self.fixtures(("claude",), callback=slow_review):
            result = orch.run_workflow("Implement value", cwd=str(self.repo), force_code=True,
                                       workflow="single", risk="low", auto_fix=False, total_timeout=.15)
        self.assertEqual(result.status, "TIMEOUT")
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 1\n")

    def test_slow_accounting_after_timely_review_does_not_confirm_delivery(self):
        actual_complete = call_budget.complete
        review_seen = []
        elapsed = [0.0]
        def complete(*args, **kwargs):
            if current_context().get("readonly"):
                review_seen.append(True)
                # The provider has returned on time; only accounting advances
                # both clocks past the total task deadline.
                elapsed[0] = 11.0
            return actual_complete(*args, **kwargs)
        with self.fixtures(("claude",)), patch.object(call_budget, "complete", side_effect=complete), \
                patch.object(time, "time", side_effect=lambda: 1700000000.0 + elapsed[0]), \
                patch.object(time, "monotonic", side_effect=lambda: 1000.0 + elapsed[0]):
            result = orch.run_workflow("Implement value", cwd=str(self.repo), force_code=True,
                                       workflow="single", risk="low", auto_fix=False, total_timeout=10)
        self.assertEqual(review_seen, [True])
        self.assertEqual(len(self.calls), 2)
        self.assertEqual(result.status, "TIMEOUT")
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 1\n")

    def test_slow_judge_accounting_does_not_seal_a_winner(self):
        from makewand.candidate import CandidateManager
        actual_complete = call_budget.complete
        judge_seen = []
        elapsed = [0.0]
        def complete(*args, **kwargs):
            if current_context().get("readonly"):
                judge_seen.append(True)
                elapsed[0] = 11.0
            return actual_complete(*args, **kwargs)
        with self.fixtures(), patch.object(call_budget, "complete", side_effect=complete), \
                patch.object(time, "time", side_effect=lambda: 1700000000.0 + elapsed[0]), \
                patch.object(time, "monotonic", side_effect=lambda: 1000.0 + elapsed[0]):
            code = orch.run_race("Implement value", cwd=str(self.repo), timeout=10,
                judge_reserve_seconds=2, engine_a="claude", engine_b="codex")
        self.assertEqual(judge_seen, [True])
        self.assertEqual(len(self.calls), 3)
        self.assertEqual(code, EXIT_TIMEOUT)
        race = CandidateManager.get_race()
        self.assertIsNone(race["winner"])
        self.assertFalse(race["candidates"]["A"]["review_passed"])
        self.assertFalse(race["candidates"]["B"]["review_passed"])


class CLIWorkflowTests(unittest.TestCase):
    def run_cli(self, arguments):
        with patch.object(sys, "argv", ["makewand", *arguments]), patch.object(sys, "stdin", io.StringIO()), contextlib.redirect_stderr(io.StringIO()), contextlib.redirect_stdout(io.StringIO()):
            try:
                cli.main()
                return 0
            except SystemExit as error:
                return error.code

    def test_flags_on_both_sides_of_command_are_preserved(self):
        with patch.object(cli, "run_pipeline", return_value=True) as execute:
            code = self.run_cli(["--risk", "low", "run", "task", "--workflow", "single", "--total-timeout", ".5"])
        self.assertEqual(code, 0)
        self.assertEqual(execute.call_args.kwargs["workflow"], "single")
        self.assertEqual(execute.call_args.kwargs["risk"], "low")
        self.assertEqual(execute.call_args.kwargs["total_timeout"], .5)

    def test_cli_cannot_replace_inherited_ledger_but_accepts_path_aliases(self):
        with tempfile.TemporaryDirectory() as directory:
            parent = str(Path(directory) / "ledger.json")
            alias = str(Path(directory) / "nested" / ".." / "ledger.json")
            with patch.dict(os.environ, {"MAKEWAND_CALL_BUDGET_FILE": parent, "MAKEWAND_MAX_MODEL_CALLS": "5"}), patch.object(cli, "run_pipeline", return_value=True) as execute:
                self.assertEqual(self.run_cli(["run", "task", "--call-budget-file", str(Path(directory) / "other.json")]), 2)
                execute.assert_not_called()
                self.assertEqual(os.environ["MAKEWAND_CALL_BUDGET_FILE"], parent)
                self.assertEqual(self.run_cli(["run", "task", "--call-budget-file", alias]), 0)
                execute.assert_called_once()
                self.assertEqual(os.environ["MAKEWAND_CALL_BUDGET_FILE"], parent)
                self.assertFalse(Path(parent).exists())

    def test_cli_maximum_only_tightens_inherited_limit_and_rejects_invalid_parent(self):
        with tempfile.TemporaryDirectory() as directory:
            parent = str(Path(directory) / "ledger.json")
            for original, requested, expected in (("8", 12, 8), ("8", 3, 3), ("invalid", 3, None), ("0", 3, None), ("-1", 3, None)):
                with self.subTest(original=original, requested=requested), patch.dict(os.environ, {"MAKEWAND_CALL_BUDGET_FILE": parent, "MAKEWAND_MAX_MODEL_CALLS": original}), patch.object(cli, "run_pipeline", return_value=True) as execute:
                    code = self.run_cli(["run", "task", "--max-model-calls", str(requested)])
                    self.assertEqual(code, 0 if expected is not None else 2)
                    if expected is None:
                        execute.assert_not_called()
                    else:
                        execute.assert_called_once()
                        self.assertEqual(int(os.environ["MAKEWAND_MAX_MODEL_CALLS"]), expected)
            paths = []
            for _ in range(2):
                with patch.dict(os.environ), patch.object(cli, "run_pipeline", return_value=True):
                    os.environ.pop("MAKEWAND_CALL_BUDGET_FILE", None)
                    os.environ.pop("MAKEWAND_MAX_MODEL_CALLS", None)
                    self.assertEqual(self.run_cli(["run", "task", "--max-model-calls", "3"]), 0)
                    paths.append(os.environ["MAKEWAND_CALL_BUDGET_FILE"])
                    self.assertEqual(os.environ["MAKEWAND_MAX_MODEL_CALLS"], "3")
            self.assertNotEqual(*paths)

    def test_incompatible_choices_and_nonfinite_deadlines_are_usage_errors(self):
        for arguments in (["run", "task", "--workflow", "single"],
                          ["race", "task", "--workflow", "pipeline"],
                          ["run", "task", "--total-timeout", "nan"],
                          ["run", "task", "--judge-reserve-seconds", "1"]):
            with self.subTest(arguments=arguments), patch.object(cli, "run_pipeline") as pipeline, patch.object(cli, "run_race") as race:
                self.assertEqual(self.run_cli(arguments), 2)
                pipeline.assert_not_called()
                race.assert_not_called()


if __name__ == "__main__":
    unittest.main()
