"""Offline end-to-end file protection gates; providers and probes are stubs."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import subprocess
import shutil
import unittest
from unittest.mock import patch

import test_workflow_scheduling as scheduling
from makewand import orchestrator as orch, cli, config
from makewand.candidate import CandidateManager
from makewand.git_helper import run_git_cmd
from makewand.workflow import last_result


class ProtectedWorkflowTests(unittest.TestCase):
    fixtures = scheduling.SchedulingTests.fixtures

    def setUp(self):
        scheduling.SchedulingTests.setUp(self)
        self.protected = self.repo / "test_contract.py"
        self.original = b"# fixed user acceptance contract\n"
        self.protected.write_bytes(self.original)
        self.protected.chmod(0o664)
        for args in (["add", "."], ["commit", "-m", "protected contract"]):
            code, _, err = run_git_cmd(["git", *args], cwd=str(self.repo))
            self.assertEqual(code, 0, err)

    def run_protected(self, **options):
        return orch.run_workflow("Implement value", cwd=str(self.repo), force_code=True,
                                 protected_paths=["test_contract.py"], total_timeout=30, **options)

    def assert_host_unchanged(self):
        self.assertEqual(self.protected.read_bytes(), self.original)
        self.assertEqual(self.protected.stat().st_mode & 0o7777, 0o664)
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 1\n")

    def test_generation_violation_stops_before_tests_review_and_fallback(self):
        def change(_, kwargs):
            (Path(kwargs["cwd"]) / "test_contract.py").write_text("# relaxed contract\n")
        with self.fixtures(callback=change), patch.object(orch, "run_local_tests") as tests:
            result = self.run_protected(auto_fix=False)
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertEqual(len(self.calls), 1)
        tests.assert_not_called()
        self.assert_host_unchanged()

    def test_failed_provider_cannot_hide_violation_behind_fallback(self):
        def change(_, kwargs):
            (Path(kwargs["cwd"]) / "test_contract.py").unlink()
            return False, None, "ordinary provider failure"
        with self.fixtures(callback=change):
            result = self.run_protected(auto_fix=False)
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertEqual(len(self.calls), 1)
        self.assert_host_unchanged()

    def test_mode_violation_is_rejected(self):
        def change(_, kwargs):
            (Path(kwargs["cwd"]) / "test_contract.py").chmod(0o644)
        with self.fixtures(callback=change):
            result = self.run_protected(auto_fix=False, workflow="single", risk="low")
        self.assertEqual(result.status, "UNVERIFIED")
        self.assert_host_unchanged()

    def test_tests_cannot_modify_protected_file_before_review(self):
        def tests(cwd, **_):
            (Path(cwd) / "test_contract.py").write_text("# tests poisoned\n")
            return True, "passed"
        with self.fixtures(), patch.object(orch, "run_local_tests", side_effect=tests):
            result = self.run_protected(auto_fix=False)
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertEqual(len(self.calls), 1)
        self.assert_host_unchanged()

    def test_repair_violation_stops_before_retest_or_second_review(self):
        def provider(_, kwargs):
            if kwargs.get("readonly"):
                return True, 'MAKEWAND_VERDICT: {"pass": false, "defects": ["Fix value"]}', None
            if len(self.calls) >= 3:
                (Path(kwargs["cwd"]) / "test_contract.py").write_text("# repair changed tests\n")
        with self.fixtures(callback=provider), patch.object(orch, "run_local_tests", return_value=(True, "passed")) as tests:
            result = self.run_protected(auto_fix=True)
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertEqual(len(self.calls), 3)
        self.assertEqual(tests.call_count, 1)
        self.assert_host_unchanged()

    def test_valid_delivery_script_checks_host_drift_and_keeps_other_files_atomic(self):
        artifact_root = _isolation.artifacts_root()
        before = set(artifact_root.glob("delivery_*"))
        with self.fixtures():
            result = self.run_protected(auto_fix=False)
        self.assertEqual(result.status, "PASSED", result.error)
        self.assert_host_unchanged()
        outputs = [p for p in set(artifact_root.glob("delivery_*")) - before
                   if (p / "delivery_manifest.json").exists()]
        self.assertEqual(len(outputs), 1)
        delivery = outputs[0]
        manifest = json.loads((delivery / "delivery_manifest.json").read_text())
        self.assertIn("test_contract.py", manifest["protected_files"]["files"])
        self.protected.write_text("# user updated contract after generation\n")
        self.protected.chmod(0o664)
        failed = subprocess.run([str(delivery / "apply_delivery.sh")], capture_output=True, text=True)
        self.assertNotEqual(failed.returncode, 0)
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 1\n")
        self.assertIn("user updated", self.protected.read_text())
        self.protected.write_bytes(self.original)
        passed = subprocess.run([str(delivery / "apply_delivery.sh")], capture_output=True, text=True)
        self.assertEqual(passed.returncode, 0, passed.stderr)
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 2\n")
        self.assertEqual(self.protected.read_bytes(), self.original)
        self.assertEqual(self.protected.stat().st_mode & 0o7777, 0o664)

    def test_postapply_protection_failure_rolls_back_applied_patch(self):
        artifact_root = _isolation.artifacts_root()
        before = set(artifact_root.glob("delivery_*"))
        with self.fixtures():
            result = self.run_protected(auto_fix=False)
        self.assertEqual(result.status, "PASSED", result.error)
        outputs = [p for p in set(artifact_root.glob("delivery_*")) - before
                   if (p / "delivery_manifest.json").exists()]
        self.assertEqual(len(outputs), 1)
        actual_git = shutil.which("git")
        wrapper_dir = self.root / "wrapper"
        wrapper_dir.mkdir()
        wrapper = wrapper_dir / "git"
        wrapper.write_text("#!" + sys.executable + "\nimport subprocess,sys\nfrom pathlib import Path\n"
                           + "code = subprocess.run([" + repr(actual_git) + "] + sys.argv[1:]).returncode\n"
                           + "if code == 0 and 'apply' in sys.argv and '--check' not in sys.argv and '--reverse' not in sys.argv:\n"
                           + "    Path(" + repr(str(self.protected)) + ").write_text('# concurrent destination change\\n')\n"
                           + "sys.exit(code)\n")
        wrapper.chmod(0o755)
        env = dict(os.environ, PATH=str(wrapper_dir) + os.pathsep + os.environ["PATH"])
        failed = subprocess.run([str(outputs[0] / "apply_delivery.sh")], env=env, capture_output=True, text=True)
        self.assertNotEqual(failed.returncode, 0)
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 1\n")
        self.assertIn("concurrent destination change", self.protected.read_text())
        self.assertIn("回滚", failed.stderr)

    def test_invalid_declaration_does_not_dispatch(self):
        with self.fixtures():
            result = orch.run_workflow("Implement value", cwd=str(self.repo), force_code=True,
                                       protected_paths=["../outside"])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertEqual(self.calls, [])
        self.assert_host_unchanged()

    def test_race_violation_does_not_test_judge_archive_or_apply(self):
        def change(_, kwargs):
            (Path(kwargs["cwd"]) / "test_contract.py").write_text("# modified\n")
        candidates = self.root / "candidates"
        with self.fixtures(callback=change), patch.object(orch, "CANDIDATES_DIR", candidates), \
                patch.object(config, "CANDIDATES_DIR", candidates), \
                patch.object(orch, "run_local_tests") as tests, patch.object(CandidateManager, "save_race") as save:
            code = orch.run_race("Implement value", cwd=str(self.repo), engine_a="claude", engine_b="codex",
                                 timeout=30, protected_paths=["test_contract.py"])
        self.assertEqual(code, orch.EXIT_UNVERIFIED)
        self.assertEqual(last_result().status, "UNVERIFIED")
        tests.assert_not_called()
        save.assert_not_called()
        self.assert_host_unchanged()

    def test_race_persists_frozen_protection_and_valid_candidate_applies(self):
        candidates = self.root / "candidates"
        with self.fixtures(), patch.object(orch, "CANDIDATES_DIR", candidates), \
                patch.object(config, "CANDIDATES_DIR", candidates):
            code = orch.run_race("Implement value", cwd=str(self.repo), engine_a="claude", engine_b="codex",
                                 timeout=30, protected_paths=["test_contract.py"])
            self.assertEqual(code, 0)
            race = CandidateManager.get_race()
            self.assertIn("test_contract.py", race["protected_files"]["files"])
            ok, _, message = CandidateManager.apply_candidate(race["race_id"], race["winner"])
            self.assertTrue(ok, message)
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 2\n")
        self.assertEqual(self.protected.read_bytes(), self.original)
        self.assertEqual(self.protected.stat().st_mode & 0o7777, 0o664)


class ProtectedCliTests(unittest.TestCase):
    def invoke(self, args):
        with patch.object(sys, "argv", ["makewand", *args]), patch.object(sys.stdin, "isatty", return_value=True), \
                patch("makewand.collision.detect_cross_session_collisions", return_value={}), \
                contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as signal:
                cli.main()
            return signal.exception.code

    def test_protect_before_and_after_run_are_combined(self):
        with patch.object(cli, "run_pipeline", return_value=True) as run:
            self.assertEqual(self.invoke(["--protect", "test_one.py", "run", "task", "--protect", "test_two.py"]), 0)
        self.assertEqual(run.call_args.kwargs["protected_paths"], ["test_one.py", "test_two.py"])

    def test_race_forwards_protection(self):
        with patch.object(cli, "run_race", return_value=0) as race:
            self.assertEqual(self.invoke(["race", "task", "--protect", "test_one.py"]), 0)
        self.assertEqual(race.call_args.kwargs["protected_paths"], ["test_one.py"])

    def test_force_apply_keeps_protection(self):
        with patch.object(CandidateManager, "apply_candidate", return_value=(True, [], "ok")) as apply:
            self.assertEqual(self.invoke(["apply", "saved", "--force", "--protect", "test_one.py"]), 0)
        self.assertTrue(apply.call_args.kwargs["force"])
        self.assertEqual(apply.call_args.kwargs["protected_paths"], ["test_one.py"])

    def test_unsupported_direct_command_cannot_silently_ignore_flag(self):
        with patch.object(cli, "_execute_direct_task") as direct:
            self.assertEqual(self.invoke(["claude", "task", "--protect", "test_one.py"]), 2)
        direct.assert_not_called()


if __name__ == "__main__":
    unittest.main()
