"""Local gate deadline and clean-input regressions; no model tools execute."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import json
import os
import subprocess
import sys
import tempfile
import time
import types
import unittest
from pathlib import Path
from unittest.mock import patch
import makewand.orchestrator as orch
from makewand.artifact import workspace_snapshot

REAL_MONOTONIC = time.monotonic


class LocalTestDeadlineTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="makewand-test-deadline-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        (self.root / "test_sample.py").write_text("def test_sample(): pass\n")
        self.clock = 100.0
        stack = contextlib.ExitStack()
        self.addCleanup(stack.close)
        self.stack = stack
        stack.enter_context(patch.object(orch.time, "monotonic", side_effect=lambda: self.clock))
        stack.enter_context(patch("makewand.git_helper.get_dirty_files", return_value=[]))
        stack.enter_context(patch.object(orch.shutil, "which", side_effect=lambda name: "/bin/true" if name == "go" else None))
        stack.enter_context(patch.dict(sys.modules, {"pytest": types.SimpleNamespace()}))

    def invoke(self, execute, timeout=1, composite=False):
        if composite:
            (self.root / "go.mod").write_text("module example.invalid/fixture\n")
        with patch("makewand.sandbox.run_in_sandbox", side_effect=execute) as run:
            ok, details = orch.run_local_tests(str(self.root), timeout=timeout)
        return ok, details, run

    def test_composite_suites_receive_decreasing_remaining_time(self):
        timeouts = []
        def execute(**kwargs):
            timeouts.append(kwargs["timeout"])
            self.clock += .3
            return 0, "passed", "", None
        ok, details, run = self.invoke(execute, composite=True)
        self.assertTrue(ok)
        self.assertEqual(run.call_count, 2)
        self.assertAlmostEqual(timeouts[0], 1)
        self.assertAlmostEqual(timeouts[1], .7)
        self.assertIn("Python Tests Passed", details)
        self.assertIn("Go Tests Passed", details)

    def test_composite_suite_cannot_get_a_new_complete_timeout(self):
        timeouts = []
        def execute(**kwargs):
            timeouts.append(kwargs["timeout"])
            self.clock += min(.7, kwargs["timeout"])
            if len(timeouts) == 2:
                return -1, "", "timeout", "Timeout"
            return 0, "passed", "", None
        ok, details, run = self.invoke(execute, composite=True)
        self.assertFalse(ok)
        self.assertEqual(run.call_count, 2)
        self.assertAlmostEqual(timeouts[1], .3)
        self.assertIn("总时间预算已耗尽", details)

    def test_late_success_cannot_pass_or_start_the_next_suite(self):
        def execute(**kwargs):
            self.clock += 1.1
            return 0, "late pass", "", None
        ok, details, run = self.invoke(execute, composite=True)
        self.assertFalse(ok)
        self.assertEqual(run.call_count, 1)
        self.assertIn("总时间预算已耗尽", details)

    def test_pytest_fallback_shares_budget_and_disables_bytecode(self):
        commands, timeouts = [], []
        def execute(**kwargs):
            commands.append(kwargs["cmd"])
            timeouts.append(kwargs["timeout"])
            self.clock += .6 if len(commands) == 1 else .1
            return (1, "", "No module named pytest", None) if len(commands) == 1 else (0, "ok", "", None)
        ok, details, run = self.invoke(execute)
        self.assertTrue(ok)
        self.assertEqual(run.call_count, 2)
        self.assertAlmostEqual(timeouts[1], .4)
        self.assertIn("unittest", commands[1])
        self.assertTrue(all("-B" in command for command in commands))
        self.assertIn("unittest", details)

    def test_expired_pytest_never_starts_fallback(self):
        def execute(**kwargs):
            self.clock += 1.1
            return 1, "", "No module named pytest", None
        ok, details, run = self.invoke(execute)
        self.assertFalse(ok)
        self.assertEqual(run.call_count, 1)
        self.assertIn("未启动 unittest 回退", details)

    def test_static_precheck_consumes_the_same_budget(self):
        def precheck(*args):
            self.clock += 1.1
            return True, []
        with patch("makewand.git_helper.get_dirty_files", return_value=["test_sample.py"]), patch("makewand.linter.fast_syntax_check", side_effect=precheck):
            ok, details, run = self.invoke(lambda **kwargs: (0, "", "", None))
        self.assertFalse(ok)
        run.assert_not_called()
        self.assertIn("总时间预算已耗尽", details)

    def test_short_positive_budget_is_preserved(self):
        ok, _, run = self.invoke(lambda **kwargs: (0, "", "", None), timeout=.25)
        self.assertTrue(ok)
        self.assertAlmostEqual(run.call_args.kwargs["timeout"], .25)

    def test_success_does_not_create_or_modify_workspace_playbook(self):
        for existing in (False, True):
            with self.subTest(existing=existing):
                playbook = self.root / ".makewand/playbook.json"
                if existing:
                    playbook.parent.mkdir(exist_ok=True)
                    playbook.write_text(json.dumps({"test": ["existing command"]}))
                before = workspace_snapshot(self.root)
                with patch("makewand.memory.record_verified_command") as record:
                    ok, details, _ = self.invoke(lambda **kwargs: (0, "", "", None))
                self.assertTrue(ok)
                record.assert_not_called()
                self.assertEqual(workspace_snapshot(self.root), before)
                self.assertIn("Python Tests Passed", details)

    def test_real_python_gate_preserves_imported_source_without_pyc(self):
        (self.root / "module.py").write_text("VALUE = 1\n")
        (self.root / "test_sample.py").write_text("import unittest\nfrom module import VALUE\nclass T(unittest.TestCase):\n    def test_value(self): self.assertEqual(VALUE, 1)\n")
        before = workspace_snapshot(self.root)
        def execute(**kwargs):
            command = kwargs["cmd"]
            # Force the deterministic unittest fallback; execute only this
            # authored fixture with the actual interpreter and a cleared env.
            if "pytest" in command:
                return 1, "", "No module named pytest", None
            process = subprocess.run(command, cwd=kwargs["workspace"], env={"PATH": os.environ["PATH"]},
                                     capture_output=True, text=True, timeout=kwargs["timeout"])
            return process.returncode, process.stdout, process.stderr, None
        ok, _, run = self.invoke(execute, timeout=5)
        self.assertTrue(ok)
        self.assertEqual(run.call_count, 2)
        self.assertEqual(workspace_snapshot(self.root), before)
        self.assertEqual(list(self.root.rglob("*.pyc")), [])

    def test_real_processes_stop_at_shared_deadline(self):
        timeouts = []
        def execute(**kwargs):
            timeouts.append(kwargs["timeout"])
            try:
                process = subprocess.run([sys.executable, "-I", "-c", "import time; time.sleep(.65)"],
                                         cwd=kwargs["workspace"], capture_output=True, text=True,
                                         timeout=kwargs["timeout"])
                return process.returncode, process.stdout, process.stderr, None
            except subprocess.TimeoutExpired:
                return -1, "", "fixture timeout", "Timeout"
        with patch.object(orch.time, "monotonic", side_effect=REAL_MONOTONIC):
            started = REAL_MONOTONIC()
            ok, details, run = self.invoke(execute, composite=True)
            elapsed = REAL_MONOTONIC() - started
        self.assertFalse(ok)
        self.assertEqual(run.call_count, 2)
        self.assertLess(timeouts[1], .5)
        self.assertLess(elapsed, 1.5)
        self.assertIn("总时间预算已耗尽", details)


if __name__ == "__main__":
    unittest.main(verbosity=2)
