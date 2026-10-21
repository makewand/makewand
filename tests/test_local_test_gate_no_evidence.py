"""Tests that could not run are UNVERIFIED, an exhausted gate budget is TIMEOUT.

Neither is a failing test: no auto-fix round, no EXIT_FAILED, in `run` and in
`race`. Covers pytest hidden in the sandbox (the host interpreter can still import
it), nothing collected (exit 5) with a pytest configuration, pytest's import error
with and without quotes, and the gate budget; a real failing test stays FAILED, also
when it prints that import error, and such a run's test output is still removed. A
host pytest script is probed by importing pytest with the interpreter its first line
names, never by running it. The sandbox is replaced by host processes that run only
the fixtures written here.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import importlib.util
import io
import os
import re
import subprocess
import sys
import tempfile
import time
import types
import unittest
from pathlib import Path
from unittest.mock import patch

import test_workflow_scheduling as scheduling
from makewand import config, orchestrator as orch
from makewand.candidate import CandidateManager
from makewand.execution_contract import EXIT_FAILED, EXIT_TIMEOUT, EXIT_UNVERIFIED
from makewand.execution_runtime import current_context
from makewand.providers.base import ProcessExecutionError
from makewand.workflow import last_result
from test_g1_datasafety_pipeline import PipelineHarness, make_project

QUOTED = ("Traceback (most recent call last):\n"
          '  File "/home/alice/.local/bin/pytest", line 5, in <module>\n'
          "    from pytest import console_main\n"
          "ModuleNotFoundError: No module named 'pytest'\n")
NO_TESTS_RAN = ("\n----------------------------------------------------------------------\n"
                "Ran 0 tests in 0.000s\n\nNO TESTS RAN\n")
# pytest -q output of a failing test that prints pytest's import error itself.
FAILED_PRINTING_IMPORT_ERROR = (
    "F                                                                        [100%]\n"
    "=================================== FAILURES ===================================\n"
    "___________________________________ test_app ___________________________________\n\n"
    "    def test_app():\n        print(\"No module named 'pytest'\")\n>       assert 1 == 2\n"
    "E       assert 1 == 2\n\ntest_app.py:3: AssertionError\n"
    "----------------------------- Captured stdout call -----------------------------\n"
    "No module named 'pytest'\n"
    "=========================== short test summary info ============================\n"
    "FAILED test_app.py::test_app - assert 1 == 2\n1 failed in 0.01s\n")
# The interpreter the first line of the host pytest script in these tests names.
SCRIPT_PYTHON = "/home/alice/venv/bin/python3"


def exited(code):
    return ProcessExecutionError(f"Command returned exit code {code}", "UNKNOWN") if code else None


def is_probe(cmd):
    return cmd[-2:] == ["-c", "import pytest"]


def is_script_probe(cmd):
    return is_probe(cmd) and cmd[0] == SCRIPT_PYTHON


def run_gate(root, respond, commands=None, which=None, host_pytest=True, timeout=60, clock=None):
    """Run the gate on `root`; ``respond(cmd)`` plays the sandboxed process."""
    def execute(**kwargs):
        if commands is not None:
            commands.append(list(kwargs["cmd"]))
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
        return orch.run_local_tests(str(root), timeout=timeout)


class GateCase(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory(prefix="mw-gate-no-evidence-")
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        (self.root / "test_app.py").write_text("def test_app():\n    assert True\n")
        self.commands = []

    def pytest_script(self, first_line=f"#!{SCRIPT_PYTHON}"):
        """A host `pytest` console script, outside the tested project, whose first line is `first_line`."""
        tmp = tempfile.TemporaryDirectory(prefix="mw-gate-pytest-script-")
        self.addCleanup(tmp.cleanup)
        script = Path(tmp.name) / "pytest"
        script.write_text(f"{first_line}\nimport sys\nfrom pytest import console_main\nsys.exit(console_main())\n")
        return str(script)

    def gate(self, respond, which=None, host_pytest=True, timeout=60, clock=None):
        """Run the gate on the fixture project, recording each sandboxed command."""
        return run_gate(self.root, respond, self.commands, which=which, host_pytest=host_pytest,
                        timeout=timeout, clock=clock)

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
            if is_probe(cmd):
                return 1, "", QUOTED, exited(1)
            return 5, "", NO_TESTS_RAN, exited(5)
        script = self.pytest_script()
        self.assert_unverified(self.gate(respond, which={"pytest": script}, host_pytest=False))
        self.assertTrue(is_probe(self.commands[0]) and not is_script_probe(self.commands[0]))
        self.assertTrue(is_script_probe(self.commands[1]))
        self.assertIn("unittest", self.commands[2])
        self.assertFalse(any(cmd[0] in ("pytest", script) for cmd in self.commands))

    def test_host_pytest_script_the_sandbox_cannot_start_is_ruled_out(self):
        # A pytest script in a host directory the sandbox does not bind: bwrap cannot
        # exec its interpreter. The old gate called that a sandbox failure and never tried unittest.
        host_script = {"pytest": self.pytest_script()}

        def respond(cmd):
            if is_script_probe(cmd):
                return 1, "", f"bwrap: execvp {SCRIPT_PYTHON}: No such file or directory\n", exited(1)
            if is_probe(cmd):
                return 1, "", QUOTED, exited(1)
            return 0, "", "Ran 1 test in 0.001s\n\nOK\n", None
        ok, details = self.gate(respond, which=host_script, host_pytest=False)
        self.assertTrue(ok, details)
        self.assertIn("unittest", self.commands[-1])
        (self.root / "pytest.ini").write_text("[pytest]\n")
        self.commands.clear()
        details = self.assert_unverified(self.gate(respond, which=host_script, host_pytest=False))
        self.assertIn("项目配置了 pytest", details)
        self.assertEqual(len(self.commands), 2)

    def test_sandbox_that_cannot_start_the_interpreter_is_still_an_infrastructure_failure(self):
        def respond(cmd):
            return 1, "", f"bwrap: execvp {cmd[0]}: No such file or directory\n", exited(1)
        details = self.assert_unverified(self.gate(respond, which={"pytest": self.pytest_script()}))
        self.assertIn("沙箱容器基础设施故障", details)
        self.assertEqual(len(self.commands), 1)

    def test_pytest_script_is_probed_by_importing_pytest_with_its_interpreter(self):
        # `pytest --version` loads plugins and conftest.py, on the host under the unsafe
        # opt-in without bubblewrap. The probe only imports pytest, with the interpreter
        # the script's first line names, and the script itself then runs the tests.
        cases = {f"#!{SCRIPT_PYTHON}": [SCRIPT_PYTHON], f"#! {SCRIPT_PYTHON} -s": [SCRIPT_PYTHON, "-s"],
                 "#!/usr/bin/env python3": ["/usr/bin/env", "python3"], "#!/opt/pypy/bin/pypy3.10": ["/opt/pypy/bin/pypy3.10"]}
        for first_line, interpreter in cases.items():
            with self.subTest(first_line=first_line):
                self.commands.clear()

                def respond(cmd, interpreter=interpreter):
                    if cmd == [*interpreter, "-B", "-c", "import pytest"]:
                        return 0, "", "", None
                    if is_probe(cmd):
                        return 1, "", QUOTED, exited(1)
                    return 0, "1 passed in 0.01s\n", "", None
                ok, details = self.gate(respond, which={"pytest": self.pytest_script(first_line)}, host_pytest=False)
                self.assertTrue(ok, details)
                self.assertEqual(self.commands[1], [*interpreter, "-B", "-c", "import pytest"])
                self.assertEqual(self.commands[2][0], "pytest")
                self.assertEqual(len(self.commands), 3)
                self.assertFalse(any("--version" in cmd for cmd in self.commands))

    def test_pytest_script_without_a_python_first_line_is_not_run(self):
        # The probe runs what the first line names, so only a Python interpreter qualifies:
        # bash would run `-B -c "import pytest"` as ImageMagick's `import` command.
        for first_line in ("#!/bin/sh", "#!/bin/bash", "#!python3", "#!/usr/bin/env -S python3 -X utf8",
                           f"#!{SCRIPT_PYTHON} other.py", f"#!{SCRIPT_PYTHON}\r", "#!/usr\0/bin/python3",
                           "import sys", "\x7fELF"):
            with self.subTest(first_line=first_line):
                self.commands.clear()
                ok, details = self.gate(lambda cmd: (1, "", QUOTED, exited(1)) if is_probe(cmd) else
                                        (0, "", "Ran 1 test in 0.001s\n\nOK\n", None),
                                        which={"pytest": self.pytest_script(first_line)}, host_pytest=False)
                self.assertTrue(ok, details)
                self.assertEqual(len(self.commands), 2)
                self.assertIn("unittest", self.commands[1])
        self.assertIsNone(orch._script_python(str(self.root / "missing-pytest")))

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


class NoEvidenceAfterRunTests(GateCase):
    """A run that produced no evidence still goes through the checks after the suites."""

    def nothing_collected(self, side_effect):
        def respond(cmd):
            if is_probe(cmd):
                return 0, "", "", None
            side_effect()
            return 5, "\nno tests ran in 0.01s\n", "", exited(5)
        return respond

    def test_test_output_of_a_run_that_collected_nothing_is_removed(self):
        # pytest writes the --junitxml report even when it collects nothing.
        (self.root / "test_app.py").unlink()
        (self.root / "pytest.ini").write_text("[pytest]\naddopts = --junitxml=report.xml\n")
        report = self.root / "report.xml"
        details = self.assert_unverified(self.gate(self.nothing_collected(lambda: report.write_text("<testsuites/>"))))
        self.assertIn("exit 5", details)
        self.assertFalse(report.exists())

    def test_run_without_tests_that_changed_an_input_or_left_runnable_output_fails(self):
        (self.root / "app.py").write_text("VALUE = 1\n")

        def runnable_report():
            (self.root / "junit.xml").write_text("<testsuites/>")
            (self.root / "junit.xml").chmod(0o755)
        cases = {"changed input": (lambda: (self.root / "app.py").write_text("VALUE = 2\n"), "测试修改了待交付输入"),
                 "runnable output": (runnable_report, "可能作为代码执行")}
        for label, (side_effect, reason) in cases.items():
            with self.subTest(label):
                ok, details = self.gate(self.nothing_collected(side_effect))
                self.assertFalse(ok)
                self.assertNotIsInstance(details, orch.LocalTestsUnavailable)
                self.assertIn(reason, details)
                self.assertIn("exit 5", details)
                (self.root / "app.py").write_text("VALUE = 1\n")
                (self.root / "junit.xml").unlink(missing_ok=True)

    def test_failing_tests_stand_when_a_later_suite_ran_no_tests(self):
        # The old gate returned the Go suite's "no tests" as UNVERIFIED and dropped the
        # Python suite's failing test.
        (self.root / "go.mod").write_text("module example.invalid/fixture\n")

        def respond(cmd):
            if is_probe(cmd):
                return 0, "", "", None
            if cmd[0] == "go":
                return 0, "ok  \texample.invalid/fixture\t0.01s [no tests to run]\n", "", None
            return 1, "F\nFAILED test_app.py::test_app - assert 1 == 2\n1 failed in 0.01s\n", "", exited(1)
        ok, details = self.gate(respond, which={"go": "/usr/bin/go"})
        self.assertFalse(ok)
        self.assertNotIsInstance(details, orch.LocalTestsUnavailable)
        self.assertIn("[Python Tests Failed (exit 1)]", details)
        self.assertIn("Go 测试套件未发现任何有效测试用例", details)


class PytestImportErrorTests(GateCase):
    PYTEST = [sys.executable, "-B", "-m", "pytest", *orch._PYTEST_ARGS]
    UNITTEST = [sys.executable, "-B", "-m", "unittest", "discover", "-q"]
    LOADER_FAILURE = ("=" * 70 + "\nERROR: test_style (unittest.loader._FailedTest.test_style)\n" + "-" * 70 + "\n"
                      "ImportError: Failed to import test module: test_style\n" + QUOTED + "\n\n")
    ASSERTION_FAILURE = ("=" * 70 + "\nFAIL: test_app (test_app.T.test_app)\n" + "-" * 70 + "\n"
                         "Traceback (most recent call last):\n  File \"test_app.py\", line 5, in test_app\n"
                         "AssertionError: 1 != 2\n\n")
    SUMMARY = "-" * 70 + "\nRan 2 tests in 0.001s\n\nFAILED (failures=1, errors=1)\n"

    def test_pytest_import_error_is_what_a_runner_that_never_started_prints(self):
        # python -m pytest (runpy), a pytest script's traceback, an old unquoted ImportError.
        for stderr in ("/usr/bin/python3: No module named pytest\n", QUOTED,
                       "Traceback (most recent call last):\n  File \"x\", line 1\nImportError: No module named pytest\n"):
            self.assertIsNotNone(orch._pytest_import_error(self.PYTEST, "", stderr), stderr)
        for stdout, stderr in (("", QUOTED.replace("'pytest'", "'pytest_cov'")),
                               ("", "/usr/bin/python3: No module named pytest_asyncio\n"),
                               ("", QUOTED.replace("'pytest'", "'pytest.plugins'")),
                               ("", QUOTED.replace("'pytest'", "'requests'")),
                               ("", "No module named pytest\nanything printed after it\n"),
                               # pytest reports its session on stdout: it started, whatever stderr ends with.
                               (FAILED_PRINTING_IMPORT_ERROR, QUOTED),
                               ("1 failed in 0.01s\n", "/usr/bin/python3: No module named pytest\n")):
            self.assertIsNone(orch._pytest_import_error(self.PYTEST, stdout, stderr), (stdout, stderr))

    def test_unittest_reports_pytest_missing_only_when_nothing_else_failed(self):
        missing = orch._pytest_import_error(self.UNITTEST, "", self.LOADER_FAILURE + self.SUMMARY)
        self.assertEqual(missing, "ModuleNotFoundError: No module named 'pytest'")
        for stdout, stderr in (("No module named 'pytest'\n", self.ASSERTION_FAILURE + self.SUMMARY),
                               ("", self.LOADER_FAILURE + self.ASSERTION_FAILURE + self.SUMMARY),
                               ("", self.LOADER_FAILURE.replace("_FailedTest.test_style", "T.test_style") + self.SUMMARY),
                               ("", self.ASSERTION_FAILURE.replace("AssertionError: 1 != 2", QUOTED) + self.SUMMARY)):
            self.assertIsNone(orch._pytest_import_error(self.UNITTEST, stdout, stderr), stderr)

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

    def test_failing_test_that_prints_pytest_import_error_is_still_failed(self):
        # The finding: the gate searched all output for the import error, so this failing
        # test ended UNVERIFIED (exit 11) with no fix round. pytest ran it: it failed.
        outputs = {"captured": (FAILED_PRINTING_IMPORT_ERROR, ""),
                   "uncaptured, on stderr": (FAILED_PRINTING_IMPORT_ERROR, "No module named 'pytest'\n"),
                   "unquoted": (FAILED_PRINTING_IMPORT_ERROR.replace("'pytest'", "pytest"), "")}
        for label, (stdout, stderr) in outputs.items():
            with self.subTest(label):
                ok, details = self.gate(lambda cmd, out=stdout, err=stderr: (0, "", "", None) if is_probe(cmd)
                                        else (1, out, err, exited(1)))
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

    @unittest.skipUnless(importlib.util.find_spec("pytest"), "pytest required")
    def test_pytest_failure_that_prints_the_import_error_stays_failed(self):
        (self.root / "test_app.py").write_text(
            "import sys\n\ndef test_app():\n    print(\"No module named 'pytest'\")\n"
            "    print('No module named pytest', file=sys.stderr)\n    assert 1 == 2\n")
        ok, details = self.run_actual(hide_pytest=False)
        self.assertFalse(ok)
        self.assertNotIsInstance(details, orch.LocalTestsUnavailable)
        self.assertIn("1 failed", details)

    def test_unittest_failure_that_prints_the_import_error_stays_failed(self):
        failing = ("import sys\nimport unittest\n\nclass T(unittest.TestCase):\n    def test_app(self):\n"
                   "        print(\"ModuleNotFoundError: No module named 'pytest'\", file=sys.stderr)\n"
                   "        self.assertEqual(1, 2)\n")
        for beside in (None, "import pytest\n\ndef test_style():\n    assert True\n"):
            with self.subTest(beside_a_module_importing_pytest=bool(beside)):
                (self.root / "test_app.py").write_text(failing)
                if beside:
                    (self.root / "test_style.py").write_text(beside)
                ok, details = self.run_actual(hide_pytest=True)
                self.assertFalse(ok)
                self.assertNotIsInstance(details, orch.LocalTestsUnavailable)
                self.assertIn("AssertionError: 1 != 2", details)


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

    def test_failing_test_that_prints_pytest_import_error_ends_failed(self):
        # The finding's run: the gate's verdict on it, then what `run` makes of that verdict.
        tested = self.base / "tested"
        tested.mkdir()
        (tested / "test_app.py").write_text("def test_app():\n    assert 1 == 2\n")
        verdict = run_gate(tested, lambda cmd: (0, "", "", None) if is_probe(cmd)
                           else (1, FAILED_PRINTING_IMPORT_ERROR, "", exited(1)))
        project = make_project(self.base / "printed")
        self.assertFalse(self.run_pipeline(project, self.coder, review_pass=True, test_result=verdict))
        self.assertEqual((last_result().status, last_result().exit_code), ("FAILED", EXIT_FAILED))  # was 11
        self.assertIn("Tests Failing", self.output)


class RaceNoEvidenceTests(unittest.TestCase):
    """`makewand race` reads its three gates as run_pipeline does: a candidate whose tests
    could not run or ran out of time has no evidence, and is no failing candidate."""
    fixtures = scheduling.SchedulingTests.fixtures
    FAILING = (False, "[Python Tests Failed (exit 1)]:\n1 failed")

    def setUp(self):
        scheduling.SchedulingTests.setUp(self)
        for patcher in (patch("makewand.usage.record_engine_usage"),
                        patch.object(config, "CANDIDATES_DIR", self.root / "candidates"),
                        patch.object(orch, "CANDIDATES_DIR", self.root / "candidates")):
            patcher.start()
            self.addCleanup(patcher.stop)

    def race(self, gate, serial, **kwargs):
        """Race two candidates; ``gate`` plays run_local_tests. Above a load of 24 the
        contestants run one after the other and both are tested afterwards; otherwise
        each is tested as soon as it finishes (the Winner-Take-All gate)."""
        output = io.StringIO()
        load = 30 if serial else 0
        with self.fixtures(), contextlib.redirect_stdout(output), \
                patch.object(orch, "run_local_tests", side_effect=gate), \
                patch.object(orch.os, "getloadavg", return_value=(load, load, load)):
            code = orch.run_race("Implement value 2", cwd=str(self.repo), engine_a="claude",
                                 engine_b="codex", **{"timeout": 60, **kwargs})
        return code, output.getvalue()

    def assert_no_evidence(self, result, code, label):
        for serial in (True, False):
            with self.subTest(serial=serial):
                exit_code, output = self.race(lambda *_, **__: result, serial)
                self.assertEqual(exit_code, code, output)
                self.assertEqual(output.count(f"单测={label}"), 2, output)
                self.assertNotIn("两套候选方案均未通过本地单元测试", output)
                race = CandidateManager.get_race(re.search(r"Race ID: (rc_[0-9a-f]+)", output).group(1))
                self.assertEqual([race["candidates"][side]["test_passed"] for side in "AB"], [None, None])

    def test_tests_that_could_not_run_end_unverified(self):
        self.assert_no_evidence((False, orch.LocalTestsUnavailable("pytest 未收集到任何测试用例 (exit 5)")),
                                EXIT_UNVERIFIED, "未验证 (测试无法运行)")
        self.assertEqual(last_result().status, "UNVERIFIED")

    def test_gate_that_ran_out_of_time_ends_timeout(self):
        self.assert_no_evidence((False, orch.LocalTestsTimedOut("本地测试总时间预算已耗尽")),
                                EXIT_TIMEOUT, "未验证 (测试超时)")
        self.assertEqual(last_result().status, "TIMEOUT")

    def test_generation_budget_spent_before_a_gate_starts_is_timeout(self):
        # Serial: both candidates are tested after both finished. A's gate fails and runs
        # past the generation budget (the total minus the judge's reserve), so B's gate
        # cannot start; the judge's reserve is left. The old race ended FAILED.
        reserve, gates = 5, []

        def first_gate_spends_the_generation_budget(cwd, **kwargs):
            gates.append(cwd)
            generation_end = current_context()["deadline_unix_ms"] / 1000 - reserve
            time.sleep(max(0, generation_end - time.time()) + 0.3)
            return self.FAILING
        code, output = self.race(first_gate_spends_the_generation_budget, True, timeout=10,
                                 judge_reserve_seconds=reserve)
        self.assertEqual(len(gates), 1, output)  # B's gate could not start
        self.assertEqual(code, EXIT_TIMEOUT, output)
        self.assertIn("单测=失败", output)
        self.assertIn("单测=未验证 (测试超时)", output)

    def test_failing_tests_still_end_failed(self):
        for serial in (True, False):
            with self.subTest(serial=serial):
                code, output = self.race(lambda *_, **__: self.FAILING, serial)
                self.assertEqual(code, EXIT_FAILED, output)
                self.assertIn("两套候选方案均未通过本地单元测试", output)


if __name__ == "__main__":
    unittest.main()
