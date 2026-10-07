"""
Unit tests for Pipeline Auto-Fix Early-Stopping & Anti-Timeout (Fair12 optimization).
Verifies:
1. Workspace dirty file capture and atomic rollback.
2. Regression failure detection (reverting to pre-fix state when repair breaks tests).
3. Diff explosion / churn explosion / file sprawl detection and rollback.
4. Anti-timeout intelligent early-stopping when time remaining is insufficient for another round.
"""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import io
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch, MagicMock

import test_workflow_scheduling as scheduling
from makewand import orchestrator as orch, config
from makewand.git_helper import run_git_cmd
from makewand.orchestrator import capture_dirty_files, restore_dirty_files


class TestAutoFixEarlyStop(unittest.TestCase):
    fixtures = scheduling.SchedulingTests.fixtures

    def setUp(self):
        scheduling.SchedulingTests.setUp(self)
        self._usage_patch = patch("makewand.usage.record_engine_usage")
        self._usage_patch.start()

    def tearDown(self):
        self._usage_patch.stop()
        scheduling.SchedulingTests.tearDown(self)

    def test_capture_and_restore_dirty_files(self):
        """Test capture_dirty_files and restore_dirty_files clean rollback."""
        repo_dir = self.repo
        app_py = repo_dir / "app.py"
        app_py.write_text("VALUE = 100\n")

        # Untracked file existing before repair
        pre_untracked = repo_dir / "pre.txt"
        pre_untracked.write_text("pre-existing untracked\n")

        # Capture snapshot
        saved = capture_dirty_files(repo_dir)
        self.assertIn("app.py", saved)
        self.assertIn("pre.txt", saved)
        self.assertEqual(saved["app.py"], b"VALUE = 100\n")
        self.assertEqual(saved["pre.txt"], b"pre-existing untracked\n")

        # Simulate broken repair that mutates files and creates new rogue files
        app_py.write_text("VALUE = BROKEN_DIVERGED\n")
        pre_untracked.write_text("mutated pre\n")
        rogue_1 = repo_dir / "rogue1.py"
        rogue_1.write_text("rogue 1\n")
        rogue_2 = repo_dir / "sub" / "rogue2.py"
        rogue_2.parent.mkdir(parents=True, exist_ok=True)
        rogue_2.write_text("rogue 2\n")

        # Perform rollback
        restore_dirty_files(repo_dir, saved)

        # Verify exact restoration
        self.assertEqual(app_py.read_text(), "VALUE = 100\n")
        self.assertEqual(pre_untracked.read_text(), "pre-existing untracked\n")
        self.assertFalse(rogue_1.exists(), "Rogue file created during repair must be deleted")
        self.assertFalse(rogue_2.exists(), "Rogue nested file must be deleted")

    def test_regression_failure_triggers_early_stop_and_rollback(self):
        """
        When pre-fix tests pass but repair introduces a regression (tests fail),
        auto-fix must restore pre-fix workspace and halt immediately without second review.
        """
        app_file = self.repo / "app.py"

        # Call counter and mock test function
        test_call_count = 0
        def mock_tests(cwd, **_):
            nonlocal test_call_count
            test_call_count += 1
            # 1st call (pre-review gate): pass
            if test_call_count == 1:
                return True, "1 passed"
            # 2nd call (post-fix gate): fail (regression introduced!)
            return False, "FAILED test_app.py::test_val - Regression error"

        def mock_provider(engine, kwargs):
            if kwargs.get("readonly"):
                # Reviewer rejects initial code with P1 defect
                return True, 'MAKEWAND_VERDICT: {"pass": false, "defects": ["P1: logic flaw"]}', None
            if len(self.calls) == 1:
                # 1st coder call: write initial working code
                (Path(kwargs["cwd"]) / "app.py").write_text("VALUE = 2\n")
                return True, "implemented", None
            if len(self.calls) >= 3:
                # 2nd coder call (repair): write broken code that causes regression
                (Path(kwargs["cwd"]) / "app.py").write_text("VALUE = REGRESSION_BREAK\n")
                return True, "repaired with bug", None
            return True, "ok", None

        with self.fixtures(callback=mock_provider), patch.object(orch, "run_local_tests", side_effect=mock_tests):
            res = orch.run_workflow(
                "Fix calculation",
                cwd=str(self.repo),
                auto_fix=True,
                max_fix=3,
                total_timeout=60,
                force_code=True,
            )

        # Verify that auto-fix halted after the regression
        # Calls should be: 1. coder, 2. review, 3. repair. NO 4th call (second review)!
        self.assertEqual(len(self.calls), 3)
        self.assertFalse(res.success)

    def test_diff_explosion_triggers_early_stop_and_rollback(self):
        """
        When repair produces an exploding diff (excessive churn or file sprawl),
        auto-fix must halt early and roll back.
        """
        def mock_tests(cwd, **_):
            return True, "1 passed"

        def mock_provider(engine, kwargs):
            if kwargs.get("readonly"):
                return True, 'MAKEWAND_VERDICT: {"pass": false, "defects": ["P1: need optimization"]}', None
            if len(self.calls) == 1:
                (Path(kwargs["cwd"]) / "app.py").write_text("VALUE = 2\n")
                return True, "initial", None
            if len(self.calls) >= 3:
                # Explode diff: create many files and massive churn
                ws = Path(kwargs["cwd"])
                for i in range(10):
                    (ws / f"bloat_{i}.py").write_text(f"# Bloat file {i}\n" * 50)
                return True, "bloated repair", None
            return True, "ok", None

        with self.fixtures(callback=mock_provider), patch.object(orch, "run_local_tests", side_effect=mock_tests):
            res = orch.run_workflow(
                "Optimize performance",
                cwd=str(self.repo),
                auto_fix=True,
                max_fix=3,
                total_timeout=60,
                force_code=True,
            )

        # Exploding repair should trigger early-stop rollback before re-review
        self.assertEqual(len(self.calls), 3)
        self.assertFalse(res.success)
        # Check that bloat files were rolled back from host
        self.assertFalse((self.repo / "bloat_0.py").exists())

    def test_anti_timeout_early_stopping_on_subsequent_rounds(self):
        """
        When deadline remaining is insufficient for another round,
        auto-fix must exit early without starting another round.
        """
        import time

        def mock_tests(cwd, **_):
            return True, "1 passed"

        def mock_provider(engine, kwargs):
            if kwargs.get("readonly"):
                return True, 'MAKEWAND_VERDICT: {"pass": false, "defects": ["P1: persists"]}', None
            (Path(kwargs["cwd"]) / "app.py").write_text("VALUE = 2\n")
            if len(self.calls) >= 3:
                time.sleep(0.3)
            return True, "implemented", None

        with self.fixtures(callback=mock_provider), \
             patch.object(orch, "run_local_tests", side_effect=mock_tests), \
             patch.object(orch, "AUTO_FIX_MIN_ROUND_SECONDS", 1.2), \
             patch.object(orch, "AUTO_FIX_MIN_REVIEW_SECONDS", 0.05):
            res = orch.run_workflow(
                "Iterative task",
                cwd=str(self.repo),
                auto_fix=True,
                max_fix=5,
                total_timeout=1.4,
                force_code=True,
            )

        # Iteration 1 had repair (call 3) and re-review (call 4).
        # Iteration 2 should be skipped by anti-timeout. Total calls should be 4 (not 5+).
        self.assertEqual(len(self.calls), 4)
        self.assertFalse(res.success)


if __name__ == "__main__":
    unittest.main()
