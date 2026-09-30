"""Subscription arm contract tests: all model/workflow entry points are stubs."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import hashlib
import importlib.util
import io
import json
import os
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch

from makewand import config, orchestrator
from makewand.candidate import CandidateManager, CandidateMessage
from makewand.execution_contract import ExecutionResult, EXIT_UNVERIFIED
from makewand.execution_runtime import account_reference, current_context

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("benchmark_model_arm", ROOT / "benchmarks" / "model_arm.py")
model_arm = importlib.util.module_from_spec(spec)
spec.loader.exec_module(model_arm)


class BenchmarkModelArmTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.prompt = self.root / "prompt.txt"
        self.prompt.write_text("Implement the fixture")
        self.environment = {"MAKEWAND_CALL_BUDGET_FILE": str(self.root / "budget.json"),
                            "MAKEWAND_MAX_MODEL_CALLS": "20"}
        self.elapsed = 0.0

    def workspace(self, name="workspace"):
        workspace = self.root / name
        workspace.mkdir(parents=True)
        (workspace / "app.py").write_text("VALUE = 1\n")
        return workspace

    @contextlib.contextmanager
    def fixtures(self, prepare=None):
        # main normally runs in a new interpreter. Patch cached configuration
        # paths for these in-process tests; all writes stay in our temp root.
        private = self.root / "test-config"
        private.mkdir(exist_ok=True)
        with patch.dict(os.environ, self.environment), \
                patch.object(config, "CONFIG_FILE", private / "config.json"), \
                patch.object(config, "ensure_config_dir", return_value=None), \
                patch.object(model_arm, "_seed_repository", side_effect=prepare or (lambda *_: None)), \
                patch.object(time, "time", side_effect=lambda: 1700000000.0 + self.elapsed), \
                patch.object(time, "monotonic", side_effect=lambda: 1000.0 + self.elapsed), \
                contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            yield

    def invoke(self, arm, workspace=None, risk="medium", timeout=10, options=()):
        return model_arm.main([arm, "--workspace", str(workspace or self.workspace()),
                               "--prompt-file", str(self.prompt), "--risk", risk,
                               "--timeout", str(timeout), *options])

    def metadata(self):
        return [json.loads(path.read_text()) for path in self.root.glob("makewand-state-*/adapter.json")]

    def test_two_single_baselines_and_alias_preserve_canonical_outcome(self):
        events = self.root / "events.jsonl"
        self.environment["MAKEWAND_EXECUTION_EVENTS_FILE"] = str(events)
        for arm, engine in (("single", "claude"), ("single-claude", "claude"), ("single-codex", "codex")):
            with self.subTest(arm=arm), self.fixtures(), \
                    patch.object(orchestrator, "dispatch_task", return_value=ExecutionResult(False, "partial", "disconnected", status="UNKNOWN")) as dispatch:
                self.assertEqual(self.invoke(arm, self.workspace(arm)), 17)
                self.assertEqual(dispatch.call_args.args[0], engine)
        metadata = self.metadata()
        self.assertEqual(len(metadata), 3)
        self.assertTrue(all(row["risk"] == "medium" and row["policy_risk"] == "auto" for row in metadata))
        self.assertTrue(all(row["independent_review"] is False for row in metadata))
        self.assertTrue(all(row["status"] == "UNKNOWN" for row in metadata))
        generation = [json.loads(line) for line in events.read_text().splitlines()
                      if json.loads(line)["stage"] == "generation" and json.loads(line)["event"] == "end"]
        self.assertEqual([row["engine"] for row in generation], ["claude", "claude", "codex"])
        self.assertTrue(all(row["status"] == "UNKNOWN" for row in generation))

    def test_pipeline_medium_and_high_use_explicit_cross_review_without_auto_fix(self):
        for risk, expected in (("medium", "auto"), ("high", "high")):
            with self.subTest(risk=risk), self.fixtures(), \
                    patch.object(orchestrator, "run_workflow", return_value=ExecutionResult(False, None, "budget", status="BUDGET_EXHAUSTED")) as workflow:
                self.assertEqual(self.invoke("pipeline", self.workspace(risk), risk), 13)
                options = workflow.call_args.kwargs
                self.assertEqual(options["workflow"], "pipeline")
                self.assertEqual(options["risk"], expected)
                self.assertFalse(options["auto_fix"])
                self.assertEqual(options["max_fix"], 0)
        self.assertEqual({row["risk"] for row in self.metadata()}, {"medium", "high"})
        self.assertTrue(all(row["auto_fix"] is False and row["max_fix"] == 0 for row in self.metadata()))

    def test_pipeline_auto_fix_options_reach_workflow_and_metadata(self):
        cases = (("pipeline", "medium", ("--auto-fix",), 2),
                 ("pipeline", "high", ("--auto-fix", "--max-fix", "1"), 1),
                 ("pipeline-single", "low", ("--auto-fix", "--max-fix", "2"), 2))
        for index, (arm, risk, options, maximum) in enumerate(cases):
            with self.subTest(arm=arm, risk=risk, maximum=maximum), self.fixtures(), \
                    patch.object(orchestrator, "run_workflow", return_value=ExecutionResult(True, "done", None)) as workflow:
                self.assertEqual(self.invoke(arm, self.workspace(str(index)), risk, options=options), 0)
                arguments = workflow.call_args.kwargs
                self.assertTrue(arguments["auto_fix"])
                self.assertEqual(arguments["max_fix"], maximum)
                self.assertEqual(arguments["workflow"], "single" if arm == "pipeline-single" else "pipeline")
        metadata = self.metadata()
        self.assertEqual(len(metadata), 3)
        self.assertTrue(all(row["auto_fix"] is True for row in metadata))
        self.assertEqual(sorted(row["max_fix"] for row in metadata), [1, 2, 2])

    def test_repair_flags_are_rejected_before_nonpipeline_or_invalid_execution(self):
        cases = [(arm, options) for arm in ("single", "single-claude", "single-codex", "race")
                 for options in (("--auto-fix",), ("--max-fix", "2"))]
        cases.extend(("pipeline", options) for options in (
            ("--max-fix", "2"), ("--auto-fix", "--max-fix", "0"),
            ("--auto-fix", "--max-fix", "3"), ("--auto-fix", "--max-fix", "-1")))
        with self.fixtures(), patch.object(orchestrator, "dispatch_task") as dispatch, \
                patch.object(orchestrator, "run_workflow") as workflow, \
                patch.object(orchestrator, "run_race") as race:
            for index, (arm, options) in enumerate(cases):
                with self.subTest(arm=arm, options=options), self.assertRaises(SystemExit) as error:
                    self.invoke(arm, self.workspace(str(index)), options=options)
                self.assertEqual(error.exception.code, 2)
        dispatch.assert_not_called()
        workflow.assert_not_called()
        race.assert_not_called()
        self.assertEqual(self.metadata(), [])

    def test_auto_fix_pipeline_inherits_preparation_deadline_without_fresh_budget(self):
        def prepare(workspace, remaining):
            self.assertEqual(remaining(), 10)
            self.elapsed = 3
        def implementation(*args, **kwargs):
            self.assertTrue(kwargs["auto_fix"])
            self.assertEqual(kwargs["max_fix"], 2)
            self.assertEqual(kwargs["timeout"], 7)
            self.assertEqual(current_context()["_deadline_monotonic"], 1010)
            self.assertEqual(current_context()["deadline_unix_ms"], 1700000010000)
            self.elapsed = 9
            self.assertEqual(current_context()["_deadline_monotonic"] - time.monotonic(), 1)
            self.elapsed = 11
            kwargs["_outcome"].update(status="UNVERIFIED", error="repair exceeded deadline")
            return False
        with self.fixtures(prepare), \
                patch.object(orchestrator, "run_workflow", wraps=orchestrator.run_workflow) as workflow, \
                patch.object(orchestrator, "_run_pipeline_impl", side_effect=implementation):
            self.assertEqual(self.invoke("pipeline", options=("--auto-fix", "--max-fix", "2")), 16)
        self.assertEqual(workflow.call_args.kwargs["total_timeout"], 7)
        self.assertEqual(self.metadata()[0]["status"], "TIMEOUT")

    def test_prepare_consumes_deadline_before_generation(self):
        def prepare(workspace, remaining):
            self.assertEqual(remaining(), 10)
            self.elapsed = 7.0
        def dispatch(*args, **kwargs):
            self.assertEqual(kwargs["timeout"], 3)
            self.assertEqual(current_context()["_deadline_monotonic"], 1010)
            return ExecutionResult(True, "done", None)
        with self.fixtures(prepare), patch.object(orchestrator, "dispatch_task", side_effect=dispatch):
            self.assertEqual(self.invoke("single-codex"), 0)

    def test_expired_preparation_never_dispatches(self):
        def prepare(*_):
            self.elapsed = 11
        with self.fixtures(prepare), patch.object(orchestrator, "dispatch_task") as dispatch:
            self.assertEqual(self.invoke("single-claude"), 16)
        dispatch.assert_not_called()

    def test_race_apply_inherits_parent_deadline_and_has_no_hybrid(self):
        def race(*args, **kwargs):
            self.assertFalse(kwargs["synthesize_hybrid"])
            self.assertEqual(kwargs["risk"], "auto")
            self.assertEqual(kwargs["total_timeout"], 10)
            self.elapsed = 9
            return 0
        def apply(*args):
            self.assertEqual(current_context()["_deadline_monotonic"], 1010)
            self.assertEqual(current_context()["risk"], "medium")
            self.assertEqual(args, ("fixture-race", "B"))
            return False, [], CandidateMessage("deadline", "TIMEOUT")
        with self.fixtures(), patch.object(orchestrator, "run_race", side_effect=race), \
                patch.object(CandidateManager, "get_race", return_value={"race_id": "fixture-race", "winner": "B"}), \
                patch.object(CandidateManager, "apply_candidate", side_effect=apply):
            self.assertEqual(self.invoke("race"), 16)
        self.assertTrue(self.metadata()[0]["independent_review"])

    def test_late_race_success_cannot_start_apply(self):
        def race(*_args, **_kwargs):
            self.elapsed = 11
            return 0
        with self.fixtures(), patch.object(orchestrator, "run_race", side_effect=race), \
                patch.object(CandidateManager, "get_race", return_value={"race_id": "fixture-race", "winner": "A"}), \
                patch.object(CandidateManager, "apply_candidate") as apply:
            self.assertEqual(self.invoke("race"), 16)
        apply.assert_not_called()

    def test_race_cancel_and_apply_conflict_keep_canonical_codes(self):
        with self.fixtures(), patch.object(orchestrator, "run_race", return_value=12), \
                patch.object(CandidateManager, "apply_candidate") as apply:
            self.assertEqual(self.invoke("race", self.workspace("cancel")), 12)
        apply.assert_not_called()
        with self.fixtures(), patch.object(orchestrator, "run_race", return_value=0), \
                patch.object(CandidateManager, "get_race", return_value={"race_id": "fixture-race", "winner": "A"}), \
                patch.object(CandidateManager, "apply_candidate", return_value=(False, [], "baseline changed")):
            self.assertEqual(self.invoke("race", self.workspace("conflict")), 14)

    def test_billing_env_scrub_preserves_selected_codex_profile_and_shared_ledger(self):
        profile = self.root / "selected-profile"
        profile.mkdir()
        self.environment.update(CODEX_HOME=str(profile), OPENAI_API_KEY="fixture-secret",
            ANTHROPIC_AUTH_TOKEN="fixture-secret", CLAUDE_CODE_USE_BEDROCK="1")
        def dispatch(*args, **kwargs):
            self.assertNotIn("OPENAI_API_KEY", os.environ)
            self.assertNotIn("ANTHROPIC_AUTH_TOKEN", os.environ)
            self.assertNotIn("CLAUDE_CODE_USE_BEDROCK", os.environ)
            self.assertEqual(os.environ["MAKEWAND_CALL_BUDGET_FILE"], self.environment["MAKEWAND_CALL_BUDGET_FILE"])
            self.assertEqual(account_reference("codex"), "codex:" + hashlib.sha256(str(profile.resolve()).encode()).hexdigest()[:16])
            self.assertEqual(os.environ["MAKEWAND_ENABLE_PROVIDERS"], "codex")
            return ExecutionResult(True, "done", None)
        with self.fixtures(), patch.object(orchestrator, "dispatch_task", side_effect=dispatch):
            self.assertEqual(self.invoke("single-codex"), 0)
        self.assertFalse(any(profile.iterdir()))

    def test_existing_git_link_is_rejected_before_execution(self):
        workspace = self.workspace()
        (workspace / ".git").write_text("gitdir: /nonexistent/external\n")
        with self.fixtures(), patch.object(orchestrator, "dispatch_task") as dispatch:
            with self.assertRaises(SystemExit) as error:
                self.invoke("single-codex", workspace)
        self.assertEqual(error.exception.code, 2)
        dispatch.assert_not_called()
        self.assertEqual(list(self.root.glob("makewand-state-*")), [])

    def test_nested_seed_git_does_not_mutate_parent_attributes_or_index(self):
        checkout = self.root / "checkout"
        checkout.mkdir()
        subprocess.run(["git", "init", str(checkout)], check=True, capture_output=True)
        attributes = checkout / ".git" / "info" / "attributes"
        attributes.write_text("*.py -diff\n")
        original_inode = attributes.stat().st_ino
        workspace = checkout / "nested" / "workspace"
        workspace.mkdir(parents=True)
        (workspace / "app.py").write_text("VALUE = 1\n")
        with self.fixtures(), patch.object(model_arm, "_seed_repository", wraps=model_arm._seed_repository) as seed, \
                patch.object(orchestrator, "dispatch_task", return_value=ExecutionResult(False, None, "offline", status="UNVERIFIED")):
            # Replace the outer fixture's mock with the actual pure-Git helper.
            seed.side_effect = ORIGINAL_SEED
            self.assertEqual(self.invoke("single-codex", workspace), 11)
        self.assertTrue((workspace / ".git").is_dir())
        self.assertFalse((checkout / ".git" / "index").exists())
        self.assertEqual(attributes.read_text(), "*.py -diff\n")
        self.assertEqual(attributes.stat().st_ino, original_inode)

    def test_registered_arm_matrix_has_four_explicit_baselines_and_risk_labels(self):
        arms = json.loads((ROOT / "benchmarks" / "arms.subscription.json").read_text())
        self.assertEqual(set(arms), {"single-claude", "single-codex", "pipeline-cross-review", "race-and-apply"})
        for name in ("single-claude", "single-codex"):
            self.assertIn(name, arms[name])
        for argv in arms.values():
            self.assertEqual(argv[argv.index("--risk") + 1], "{risk}")
            self.assertEqual(argv[argv.index("--timeout") + 1], "{timeout}")
        pipeline = arms["pipeline-cross-review"]
        self.assertIn("--auto-fix", pipeline)
        self.assertEqual(pipeline[pipeline.index("--max-fix") + 1], "2")
        for arm in ("single-claude", "single-codex", "race-and-apply"):
            self.assertNotIn("--auto-fix", arms[arm])
            self.assertNotIn("--max-fix", arms[arm])


ORIGINAL_SEED = model_arm._seed_repository


class ProtectedBenchmarkArmTests(unittest.TestCase):
    setUp = BenchmarkModelArmTests.setUp
    workspace = BenchmarkModelArmTests.workspace
    @contextlib.contextmanager
    def fixtures(self, prepare=None):
        self.diagnostics = io.StringIO()
        with BenchmarkModelArmTests.fixtures(self, prepare=prepare), contextlib.redirect_stderr(self.diagnostics):
            yield

    invoke = BenchmarkModelArmTests.invoke
    metadata = BenchmarkModelArmTests.metadata

    @contextlib.contextmanager
    def candidate_state(self):
        with patch.object(config, "CONFIG_DIR", self.root / "test-config"), \
                patch.object(config, "CANDIDATES_DIR", self.root / "candidates"):
            yield

    def protected_workspace(self):
        workspace = self.workspace()
        (workspace / "test_contract.py").write_text("# keep contract\n")
        (workspace / "test_contract.py").chmod(0o664)
        return workspace

    def test_single_violation_is_rejected_before_any_host_apply(self):
        workspace = self.protected_workspace()
        def generate(engine, prompt, cwd, **_):
            self.assertNotEqual(Path(cwd), workspace)
            (Path(cwd) / "app.py").write_text("VALUE = 2\n")
            (Path(cwd) / "test_contract.py").write_text("# relaxed\n")
            return ExecutionResult(True, "generated", None)
        with self.fixtures(prepare=model_arm._seed_repository), self.candidate_state(), \
                patch.object(orchestrator, "dispatch_task", side_effect=generate) as provider:
            code = self.invoke("single-claude", workspace, options=["--protect", "test_contract.py"])
        self.assertEqual(code, EXIT_UNVERIFIED, self.diagnostics.getvalue())
        self.assertEqual(provider.call_count, 1)
        self.assertEqual((workspace / "app.py").read_text(), "VALUE = 1\n")
        self.assertEqual((workspace / "test_contract.py").read_text(), "# keep contract\n")
        self.assertEqual(self.metadata()[0]["status"], "UNVERIFIED")

    def test_valid_single_applies_atomically_without_fabricating_review_evidence(self):
        workspace = self.protected_workspace()
        def generate(engine, prompt, cwd, **_):
            (Path(cwd) / "app.py").write_text("VALUE = 2\n")
            self.assertEqual((Path(cwd) / "test_contract.py").stat().st_mode & 0o7777, 0o664)
            return ExecutionResult(True, "generated", None)
        with self.fixtures(prepare=model_arm._seed_repository), self.candidate_state(), \
                patch.object(orchestrator, "dispatch_task", side_effect=generate) as provider:
            code = self.invoke("single-codex", workspace, options=["--protect", "test_contract.py"])
            race = CandidateManager.get_race()
        self.assertEqual(code, 0, self.diagnostics.getvalue())
        self.assertEqual(provider.call_count, 1)
        self.assertEqual((workspace / "app.py").read_text(), "VALUE = 2\n")
        self.assertEqual((workspace / "test_contract.py").stat().st_mode & 0o7777, 0o664)
        self.assertIsNone(race["candidates"]["A"]["test_passed"])
        self.assertIs(race["candidates"]["A"]["review_passed"], False)
        self.assertFalse(self.metadata()[0]["independent_review"])

    def test_single_inherits_runner_file_policy(self):
        workspace = self.protected_workspace()
        self.environment["MAKEWAND_TASK_PROTECTED_PATHS"] = '["test_contract.py"]'
        def generate(engine, prompt, cwd, **_):
            (Path(cwd) / "test_contract.py").unlink()
            return ExecutionResult(False, None, "failure", status="FAILED")
        with self.fixtures(prepare=model_arm._seed_repository), self.candidate_state(), \
                patch.object(orchestrator, "dispatch_task", side_effect=generate):
            self.assertEqual(self.invoke("single-claude", workspace), EXIT_UNVERIFIED, self.diagnostics.getvalue())
        self.assertEqual((workspace / "test_contract.py").read_text(), "# keep contract\n")
        self.assertEqual(self.metadata()[0]["protected_paths"], ["test_contract.py"])

    def test_pipeline_and_race_forward_frozen_declarations(self):
        for arm, name in (("pipeline", "run_workflow"), ("race", "run_race")):
            workspace = self.workspace(arm)
            (workspace / "test_contract.py").write_text("# keep\n")
            return_value = ExecutionResult(False, None, "failed", status="UNVERIFIED") if arm == "pipeline" else EXIT_UNVERIFIED
            with self.fixtures(), patch.object(orchestrator, name, return_value=return_value) as run:
                self.assertEqual(self.invoke(arm, workspace, options=["--protect", "test_contract.py"]), EXIT_UNVERIFIED)
            self.assertEqual(run.call_args.kwargs["protected_paths"], ["test_contract.py"])

    def test_invalid_file_policy_never_seeds_or_dispatches(self):
        with self.fixtures() as _, patch.object(orchestrator, "dispatch_task") as dispatch:
            self.assertEqual(self.invoke("single-claude", options=["--protect", "../outside"]), EXIT_UNVERIFIED, self.diagnostics.getvalue())
        dispatch.assert_not_called()

    def test_explicit_adapter_paths_add_to_runner_policy(self):
        workspace = self.protected_workspace()
        (workspace / "LICENSE").write_text("user license\n")
        self.environment["MAKEWAND_TASK_PROTECTED_PATHS"] = '["test_contract.py"]'
        with self.fixtures(), patch.object(orchestrator, "run_workflow", return_value=ExecutionResult(False, None, None, status="UNVERIFIED")) as run:
            self.assertEqual(self.invoke("pipeline", workspace, options=["--protect", "LICENSE"]), EXIT_UNVERIFIED)
        self.assertEqual(set(run.call_args.kwargs["protected_paths"]), {"LICENSE", "test_contract.py"})

    def test_pipeline_manifest_cannot_weaken_authoritative_policy_before_writes(self):
        from makewand.protected_files import ProtectedFiles
        workspace = self.protected_workspace()
        protected = ProtectedFiles.capture(workspace, ["test_contract.py"])
        delivery = self.root / "state" / "artifacts" / "delivery_fixture"
        delivery.mkdir(parents=True)
        (delivery / "delivery_manifest.json").write_text(json.dumps({
            "protected_base_cwd": str(workspace), "protected_files": {"schema": 1, "files": {}}}))
        with patch.object(subprocess, "run") as apply:
            with self.assertRaises(RuntimeError):
                model_arm._apply_protected_pipeline_delivery(self.root / "state", workspace, protected, lambda: 30)
        apply.assert_not_called()


if __name__ == "__main__":
    unittest.main()
