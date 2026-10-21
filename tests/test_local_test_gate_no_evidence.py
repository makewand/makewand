"""Tests that could not run are UNVERIFIED, an exhausted gate budget is TIMEOUT.

Neither is a failing test: no auto-fix round, no EXIT_FAILED. Covers pytest hidden
in the sandbox (the host interpreter can still import it), nothing collected
(exit 5) with a pytest configuration, pytest's import error with and without
quotes, and the gate budget; a real failing test stays FAILED. The sandbox is
replaced by host processes that run only the fixtures written here.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import importlib.util
import os
import subprocess
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import orchestrator as orch
from makewand.providers.base import ProcessExecutionError
from makewand.workflow import last_result
from test_g1_datasafety_pipeline import PipelineHarness, make_project

QUOTED = ("Traceback (most recent call last):\n"
          '  File "/home/user/.local/bin/pytest", line 5, in <module>\n'
          "    from pytest import console_main\n"
          "ModuleNotFoundError: No module named 'pytest'\n")
NO_TESTS_RAN = ("\n----------------------------------------------------------------------\n"
                "Ran 0 tests in 0.000s\n\nNO TESTS RAN\n")


def exited(code):
    return ProcessExecutionError(f"Command returned exit code {code}", "UNKNOWN") if code else None


def is_probe(cmd):
    return cmd[-2:] == ["-c", "import pytest"]


def is_script_probe(cmd):
    return cmd[0] == "pytest" and "--version" in cmd


class GateCase(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory(prefix="mw-gate-no-evidence-")
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        (self.root / "test_app.py").write_text("def test_app():\n    assert True\n")
        self.commands = []

    def gate(self, respond, which=None, host_pytest=True, timeout=60, clock=None):
        """Run the gate; ``respond(cmd)`` plays the sandboxed process."""
        def execute(**kwargs):
            self.commands.append(list(kwargs["cmd"]))
            return respond(kwargs["cmd"])
        tools = which or {}
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch("makewand.git_helper.get_dirty_files", return_value=[]))
            stack.enter_context(patch.object(orch.shutil, "which", side_effect=lambda name: tools.get(name)))
            # What the host interpreter can import must not decide the runner.
            stack.enter_context(patch.dict(sys.modules, {"pytest": types.SimpleNamespace() if host_pytest else None}))
            stack.enter_context(patch("makewand.sandbox.run_in_sandbox", side_effect=execute))
            if clock is not None:
                stack.enter_context(patch.object(orch.time, "monotonic", side_effect=lambda: clock[0]))
            return orch.run_local_tests(str(self.root), timeout=timeout)

    def assert_unverified(self, result):
        ok, details = result
        self.assertFalse(ok)
        self.assertIsInstance(details, orch.LocalTestsUnavailable)
        self.assertEqual(details.execution_status, "UNVERIFIED")
        return details

    def ran_pytest(self):
        return [cmd for cmd in self.commands if "-m" in cmd and "pytest" in cmd]


class PytestHiddenInSandboxTests(GateCase):
    def test_host_pytest_hidden_in_sandbox_never_runs_pytest_and_is_unverified(self):
        def respond(cmd):
            if is_probe(cmd):
                return 1, "", QUOTED, exited(1)
            return 5, "", NO_TESTS_RAN, exited(5)  # unittest finds no pytest-style test
        details = self.assert_unverified(self.gate(respond))
        self.assertEqual(self.ran_pytest(), [])
        self.assertIn("unittest", self.commands[-1])
        self.assertIn("pytest", details)

    def test_user_site_pytest_script_that_cannot_import_pytest_is_ruled_out(self):
        # /usr/bin/python3 -I with ~/.local/bin/pytest: the old gate ran the script and
        # reported its quoted ModuleNotFoundError as a failing test (exit 1).
        def respond(cmd):
            if is_probe(cmd) or is_script_probe(cmd):
                return 1, "", QUOTED, exited(1)
            return 5, "", NO_TESTS_RAN, exited(5)
        self.assert_unverified(self.gate(respond, which={"pytest": "/home/user/.local/bin/pytest"},
                                         host_pytest=False))
        self.assertTrue(is_probe(self.commands[0]))
        self.assertTrue(is_script_probe(self.commands[1]))
        self.assertIn("unittest", self.commands[2])
        self.assertFalse(any(cmd[0] == "pytest" and "--version" not in cmd for cmd in self.commands))

    def test_pytest_configuration_without_pytest_in_sandbox_runs_nothing(self):
        (self.root / "pytest.ini").write_text("[pytest]\n")
        details = self.assert_unverified(self.gate(lambda cmd: (1, "", QUOTED, exited(1))))
        self.assertEqual(len(self.commands), 1)
        self.assertTrue(is_probe(self.commands[0]))
        self.assertIn("项目配置了 pytest", details)

    def test_unittest_style_tests_still_run_without_pytest(self):
        def respond(cmd):
            if is_probe(cmd):
                return 1, "", QUOTED, exited(1)
            return 0, "", "Ran 1 test in 0.001s\n\nOK\n", None
        ok, details = self.gate(respond)
        self.assertTrue(ok, details)
        self.assertIn("unittest", self.commands[-1])

    def test_probe_runs_read_only_in_the_sandbox_and_picks_pytest(self):
        calls = []

        def execute(**kwargs):
            calls.append(kwargs)
            return 0, "1 passed in 0.01s\n", "", None
        with patch("makewand.git_helper.get_dirty_files", return_value=[]), \
                patch.object(orch.shutil, "which", return_value=None), \
                patch.dict(sys.modules, {"pytest": None}), \
                patch("makewand.sandbox.run_in_sandbox", side_effect=execute):
            ok, details = orch.run_local_tests(str(self.root))
        self.assertTrue(ok, details)
        self.assertEqual(calls[0]["cmd"][-2:], ["-c", "import pytest"])
        self.assertTrue(calls[0]["readonly"])
        self.assertFalse(calls[0]["allow_network"])
        self.assertEqual(calls[1]["cmd"][1:4], ["-B", "-m", "pytest"])
        self.assertFalse(calls[1]["readonly"])


class NothingCollectedTests(GateCase):
    def test_exit_5_with_pytest_config_and_no_tests_is_unverified(self):
        (self.root / "test_app.py").unlink()
        for name, text in (("pytest.ini", "[pytest]\n"), ("pyproject.toml", "[tool.pytest.ini_options]\n")):
            with self.subTest(config=name):
                for stale in ("pytest.ini", "pyproject.toml"):
                    (self.root / stale).unlink(missing_ok=True)
                (self.root / name).write_text(text)
                self.commands.clear()

                def respond(cmd):
                    if is_probe(cmd):
                        return 0, "", "", None
                    return 5, "\nno tests ran in 0.01s\n", "", exited(5)
                details = self.assert_unverified(self.gate(respond))
                self.assertIn("exit 5", details)
                self.assertEqual(len(self.ran_pytest()), 1)

    def test_unittest_exit_5_is_unverified(self):
        def respond(cmd):
            if is_probe(cmd):
                return 1, "", "/usr/bin/python3: No module named pytest\n", exited(1)
            return 5, "", NO_TESTS_RAN, exited(5)
        self.assert_unverified(self.gate(respond))


class PytestImportErrorTests(GateCase):
    def test_pytest_import_error_matches_with_and_without_quotes_only(self):
        for text in ("No module named pytest", "No module named 'pytest'",
                     "ModuleNotFoundError: No module named 'pytest'\n"):
            self.assertIsNotNone(orch._PYTEST_MISSING.search(text), text)
        for text in ("No module named 'pytest_cov'", "No module named pytest_asyncio",
                     "No module named 'pytest.plugins'", "No module named 'requests'"):
            self.assertIsNone(orch._PYTEST_MISSING.search(text), text)

    def test_quoted_error_from_the_chosen_runner_is_unverified(self):
        cases = {
            # The module probe passed, yet the run cannot import pytest.
            "pytest module": (lambda cmd: (0, "", "", None) if is_probe(cmd) else (1, "", QUOTED, exited(1)), {}),
            # A test module imports pytest; unittest is the runner.
            "unittest": (lambda cmd: (1, "", QUOTED, exited(1)) if is_probe(cmd) else
                         (1, "", "ERROR: test_app (unittest.loader._FailedTest.test_app)\n" + QUOTED
                          + "\nRan 1 test in 0.000s\n\nFAILED (errors=1)\n", exited(1)), {}),
        }
        for label, (respond, which) in cases.items():
            with self.subTest(runner=label):
                self.commands.clear()
                details = self.assert_unverified(self.gate(respond, which=which))
                self.assertIn("No module named 'pytest'", details)

    def test_unquoted_error_from_the_chosen_runner_is_unverified(self):
        def respond(cmd):
            if is_probe(cmd):
                return 0, "", "", None
            return 1, "", "/usr/bin/python3: No module named pytest\n", exited(1)
        self.assert_unverified(self.gate(respond))


class GateBudgetTests(GateCase):
    def test_budget_spent_by_a_suite_is_timeout_not_a_failure(self):
        clock = [100.0]

        def respond(cmd):
            if is_probe(cmd):
                clock[0] += 1
                return 0, "", "", None
            clock[0] += 59
            return -1, "", "", ProcessExecutionError("Command timed out after 59 seconds", "TIMEOUT")
        ok, details = self.gate(respond, clock=clock)
        self.assertFalse(ok)
        self.assertIsInstance(details, orch.LocalTestsTimedOut)
        self.assertIsInstance(details, orch.LocalTestsUnavailable)
        self.assertEqual(details.execution_status, "TIMEOUT")
        self.assertIn("总时间预算已耗尽", details)
        self.assertNotIn("Tests Failed", details)

    def test_budget_spent_before_the_gate_starts_is_timeout(self):
        clock = [100.0]

        def precheck(*args):
            clock[0] += 61
            return True, []
        with patch("makewand.linter.fast_syntax_check", side_effect=precheck), \
                patch("makewand.git_helper.get_dirty_files", return_value=["test_app.py"]), \
                patch.object(orch.time, "monotonic", side_effect=lambda: clock[0]), \
                patch("makewand.sandbox.run_in_sandbox") as run:
            ok, details = orch.run_local_tests(str(self.root), timeout=60)
        self.assertFalse(ok)
        run.assert_not_called()
        self.assertEqual(details.execution_status, "TIMEOUT")

    def test_real_failure_before_the_budget_ran_out_stays_failed(self):
        (self.root / "go.mod").write_text("module example.invalid/fixture\n")
        clock = [100.0]

        def respond(cmd):
            if is_probe(cmd):
                return 0, "", "", None
            if cmd[0] == "go":
                clock[0] += 60
                return -1, "", "", ProcessExecutionError("Command timed out after 60 seconds", "TIMEOUT")
            return 1, "F\nFAILED test_app.py::test_app - assert 1 == 2\n1 failed in 0.01s\n", "", exited(1)
        ok, details = self.gate(respond, which={"go": "/usr/bin/go"}, clock=clock)
        self.assertFalse(ok)
        self.assertNotIsInstance(details, orch.LocalTestsUnavailable)
        self.assertIn("[Python Tests Failed (exit 1)]", details)
        self.assertIn("总时间预算已耗尽", details)


class RealFailureTests(GateCase):
    def test_real_failing_test_is_still_failed(self):
        def respond(cmd):
            if is_probe(cmd):
                return 0, "", "", None
            return 1, "F\nFAILED test_app.py::test_app - assert 1 == 2\n1 failed in 0.01s\n", "", exited(1)
        ok, details = self.gate(respond)
        self.assertFalse(ok)
        self.assertNotIsInstance(details, orch.LocalTestsUnavailable)
        self.assertIn("[Python Tests Failed (exit 1)]", details)
        self.assertIn("1 failed", details)

    def test_stage_span_reports_unverified_and_timeout_not_failed(self):
        statuses = []

        class Span:
            def finish(self, status):
                statuses.append(status)

        @contextlib.contextmanager
        def fake_stage(name, **kwargs):
            yield Span()
        with patch("makewand.telemetry.stage", fake_stage):
            orch._stage_call("test", lambda: (False, orch.LocalTestsTimedOut("budget")))
            orch._stage_call("test", lambda: (False, orch.LocalTestsUnavailable("no runner")))
            orch._stage_call("test", lambda: (False, "[Python Tests Failed (exit 1)]"))
        self.assertEqual(statuses, ["TIMEOUT", "UNVERIFIED", "FAILED"])


class ActualInterpreterTests(GateCase):
    """Actual interpreter output. pytest is hidden by a module that raises the import
    error the sandbox produces when the user site holding pytest is masked."""

    def setUp(self):
        super().setUp()
        hide = tempfile.TemporaryDirectory(prefix="mw-gate-hide-pytest-")
        self.addCleanup(hide.cleanup)
        self.hide = Path(hide.name)
        (self.hide / "pytest.py").write_text(
            "raise ModuleNotFoundError(\"No module named 'pytest'\", name='pytest')\n")

    def run_actual(self, hide_pytest):
        def fixture_process(cmd, workspace, timeout, extra_env=None, **kwargs):
            self.commands.append(list(cmd))
            env = {"PATH": os.environ.get("PATH", os.defpath), "PYTEST_DISABLE_PLUGIN_AUTOLOAD": "1",
                   **(extra_env or {})}
            result = subprocess.run(cmd, cwd=workspace, env=env, capture_output=True, text=True,
                                    timeout=min(timeout, 60))
            return result.returncode, result.stdout, result.stderr, exited(result.returncode)
        python_path = os.pathsep.join([str(self.hide)] if hide_pytest else [])
        with patch("makewand.git_helper.get_dirty_files", return_value=[]), \
                patch.object(orch.shutil, "which", return_value=None), \
                patch.dict(os.environ, {"PYTHONPATH": python_path}), \
                patch("makewand.sandbox.run_in_sandbox", side_effect=fixture_process):
            return orch.run_local_tests(str(self.root), timeout=120)

    def test_hidden_pytest_with_pytest_style_tests_is_unverified(self):
        self.assert_unverified(self.run_actual(hide_pytest=True))
        self.assertIn("unittest", self.commands[-1])

    def test_hidden_pytest_with_a_test_module_importing_pytest_is_unverified(self):
        (self.root / "test_app.py").write_text(
            "import unittest\nimport pytest\n\nclass T(unittest.TestCase):\n    def test_app(self):\n        pass\n")
        details = self.assert_unverified(self.run_actual(hide_pytest=True))
        self.assertIn("No module named 'pytest'", details)

    def test_hidden_pytest_still_fails_a_real_unittest_failure(self):
        (self.root / "test_app.py").write_text(
            "import unittest\n\nclass T(unittest.TestCase):\n    def test_app(self):\n        self.assertEqual(1, 2)\n")
        ok, details = self.run_actual(hide_pytest=True)
        self.assertFalse(ok)
        self.assertNotIsInstance(details, orch.LocalTestsUnavailable)
        self.assertIn("Python Tests Failed", details)

    @unittest.skipUnless(importlib.util.find_spec("pytest"), "pytest required")
    def test_pytest_config_without_tests_is_unverified(self):
        (self.root / "test_app.py").unlink()
        (self.root / "pytest.ini").write_text("[pytest]\n")
        details = self.assert_unverified(self.run_actual(hide_pytest=False))
        self.assertIn("exit 5", details)

    @unittest.skipUnless(importlib.util.find_spec("pytest"), "pytest required")
    def test_pytest_still_fails_a_real_failure(self):
        (self.root / "test_app.py").write_text("def test_app():\n    assert 1 == 2\n")
        ok, details = self.run_actual(hide_pytest=False)
        self.assertFalse(ok)
        self.assertNotIsInstance(details, orch.LocalTestsUnavailable)
        self.assertIn("1 failed", details)


class PipelineNoEvidenceTests(PipelineHarness):
    def coder(self, directory):
        (directory / "app.py").write_text("VALUE = 42\n")
        return True, "implemented", None

    def test_gate_timeout_ends_as_timeout_without_review_or_fix(self):
        project = make_project(self.base / "timeout")
        original = (project / "app.py").read_bytes()
        result = self.run_pipeline(project, self.coder, review_pass=True,
                                   test_result=(False, orch.LocalTestsTimedOut("本地测试总时间预算已耗尽")))
        self.assertFalse(result)
        self.assertEqual(last_result().status, "TIMEOUT")
        self.assertEqual(len(self.dispatch_calls), 1)  # the coder only: no review, no fix round
        self.assertNotIn("Tests Failing", self.output)
        self.assertEqual((project / "app.py").read_bytes(), original)

    def test_tests_that_could_not_run_end_as_unverified(self):
        project = make_project(self.base / "unverified")
        result = self.run_pipeline(project, self.coder, review_pass=True, test_result=(
            False, orch.LocalTestsUnavailable("pytest 未收集到任何测试用例 (exit 5)")))
        self.assertFalse(result)
        self.assertEqual(last_result().status, "UNVERIFIED")
        self.assertEqual(len(self.dispatch_calls), 1)
        self.assertNotIn("Tests Failing", self.output)

    def test_failing_tests_still_end_as_failed(self):
        project = make_project(self.base / "failed")
        result = self.run_pipeline(project, self.coder, review_pass=True,
                                   test_result=(False, "[Python Tests Failed (exit 1)]:\n1 failed"))
        self.assertFalse(result)
        self.assertEqual(last_result().status, "FAILED")
        self.assertIn("Tests Failing", self.output)


if __name__ == "__main__":
    unittest.main()
