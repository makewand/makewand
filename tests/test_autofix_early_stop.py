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
        """Test capture_dirty_files and restore_dirty_files clean rollback with spaces, unicode, and directory trees."""
        repo_dir = self.repo
        app_py = repo_dir / "app.py"
        app_py.write_text("VALUE = 100\n")

        # Untracked files with spaces and unicode existing before repair
        pre_untracked = repo_dir / "pre space.txt"
        pre_untracked.write_text("pre-existing untracked\n")
        pre_unicode = repo_dir / "测试_模块.py"
        pre_unicode.write_text("# 初始中文代码\n")

        # Capture snapshot
        saved = capture_dirty_files(repo_dir)
        self.assertIn("app.py", saved)
        self.assertIn("pre space.txt", saved)
        self.assertIn("测试_模块.py", saved)
        self.assertEqual(saved["app.py"], b"VALUE = 100\n")
        self.assertEqual(saved["pre space.txt"], b"pre-existing untracked\n")
        self.assertEqual(saved["测试_模块.py"], "# 初始中文代码\n".encode("utf-8"))

        # Simulate broken repair that mutates files and creates new rogue files
        app_py.write_text("VALUE = BROKEN_DIVERGED\n")
        pre_untracked.write_text("mutated pre\n")
        pre_unicode.write_text("# 损坏的中文代码\n")
        rogue_1 = repo_dir / "rogue space 1.py"
        rogue_1.write_text("rogue 1\n")
        rogue_2 = repo_dir / "sub" / "nested" / "rogue2.py"
        rogue_2.parent.mkdir(parents=True, exist_ok=True)
        rogue_2.write_text("rogue 2\n")

        # Perform rollback
        restore_dirty_files(repo_dir, saved)

        # Verify exact restoration
        self.assertEqual(app_py.read_text(), "VALUE = 100\n")
        self.assertEqual(pre_untracked.read_text(), "pre-existing untracked\n")
        self.assertEqual(pre_unicode.read_text(), "# 初始中文代码\n")
        self.assertFalse(rogue_1.exists(), "Rogue file with spaces created during repair must be deleted")
        self.assertFalse(rogue_2.exists(), "Rogue nested file must be deleted")
        self.assertFalse((repo_dir / "sub").exists(), "Empty rogue directory tree must be pruned on rollback")

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

    def test_anti_timeout_before_review_rolls_back(self):
        """
        When remaining time before re-review is insufficient (< AUTO_FIX_MIN_REVIEW_SECONDS),
        auto-fix must abort and roll back unreviewed repair changes.
        """
        def mock_tests(cwd, **_):
            return True, "1 passed"

        def mock_provider(engine, kwargs):
            if kwargs.get("readonly"):
                return True, 'MAKEWAND_VERDICT: {"pass": false, "defects": ["P1: flaw"]}', None
            if len(self.calls) == 1:
                (Path(kwargs["cwd"]) / "app.py").write_text("VALUE = INITIAL\n")
                return True, "initial", None
            if len(self.calls) >= 3:
                (Path(kwargs["cwd"]) / "app.py").write_text("VALUE = UNREVIEWED_MUTATION\n")
                (Path(kwargs["cwd"]) / "rogue_timeout.py").write_text("rogue\n")
                return True, "repaired", None
            return True, "ok", None

        with self.fixtures(callback=mock_provider), \
             patch.object(orch, "run_local_tests", side_effect=mock_tests), \
             patch.object(orch, "AUTO_FIX_MIN_REVIEW_SECONDS", 9999.0):
            res = orch.run_workflow(
                "Short timeout task",
                cwd=str(self.repo),
                auto_fix=True,
                max_fix=3,
                total_timeout=60,
                force_code=True,
            )

        # Call 1: coder, Call 2: review, Call 3: repair. NO Call 4 (re-review skipped due to anti-timeout)!
        self.assertEqual(len(self.calls), 3)
        self.assertFalse(res.success)
        # Rogue file created during unreviewed repair must be rolled back
        self.assertFalse((self.repo / "rogue_timeout.py").exists())


if __name__ == "__main__":
    unittest.main()
