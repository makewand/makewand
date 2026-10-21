"""scripts/test_python.py never passes while pytest-style tests go unrun (eng-1021-2).

unittest collects only TestCase methods, so a module of module-level pytest
functions used to contribute nothing and the gate still exited 0. The gate is
copied into a scratch tree with fixture modules (the real isolation is already
active and inherited through the environment) and run as a subprocess.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
GATE = ROOT / "scripts" / "test_python.py"
sys.path.insert(0, str(ROOT / "scripts"))
from windows_test_report import summarize_python  # noqa: E402

PLAIN = """
    import unittest

    class Plain(unittest.TestCase):
        def test_one(self):
            self.assertTrue(True)
"""
PYTEST_FUNCTIONS = """
    def helper():
        return 1

    def test_module_level():
        assert helper() == 1
"""
PYTEST_CLASS = """
    class TestThing:
        def test_method(self):
            assert True
"""
MIXED = """
    import unittest

    class Mixed(unittest.TestCase):
        def test_one(self):
            self.assertTrue(True)

    def test_module_level():
        assert True
"""


class UnittestGateGuardTests(unittest.TestCase):
    def tree(self, **modules):
        directory = Path(tempfile.mkdtemp(prefix="makewand-gate-guard-"))
        self.addCleanup(shutil.rmtree, directory, ignore_errors=True)
        (directory / "scripts").mkdir()
        (directory / "tests").mkdir()
        shutil.copy2(GATE, directory / "scripts" / "test_python.py")
        (directory / "tests" / "_isolation.py").write_text("def activate():\n    return None\n", encoding="utf-8")
        for name, body in modules.items():
            (directory / "tests" / f"{name}.py").write_text(textwrap.dedent(body), encoding="utf-8")
        return directory

    def run_gate(self, tree, *names):
        return subprocess.run([sys.executable, "-I", "-B", str(tree / "scripts" / "test_python.py"), *names],
                              capture_output=True, timeout=300)

    def assert_refused(self, result, *mentions):
        stderr = result.stderr.decode("utf-8", "replace")
        self.assertNotEqual(result.returncode, 0, stderr)
        self.assertIn("python3 -I -m pytest -q -p no:cacheprovider tests", stderr)
        for mention in mentions:
            self.assertIn(mention, stderr)

    def test_discovery_with_only_testcases_passes_and_ends_with_the_footer(self):
        result = self.run_gate(self.tree(test_plain=PLAIN))
        self.assertEqual(result.returncode, 0, result.stderr.decode("utf-8", "replace"))
        # The native Windows gate parses this output and requires the footer last.
        report = summarize_python(result.stderr, {"test_plain.Plain.test_one"}, "utf-8")
        self.assertEqual(report["pass"], 1)

    def test_discovery_fails_when_a_module_has_module_level_tests(self):
        result = self.run_gate(self.tree(test_plain=PLAIN, test_functions=PYTEST_FUNCTIONS))
        self.assert_refused(result, "test_functions: 1 pytest-style test(s)", "test_module_level")
        self.assertNotIn(b"helper", result.stderr.split(b"leaves tests unrun", 1)[1])
        self.assertIn(b"Ran 1 test", result.stderr, "the TestCase tests still run")

    def test_discovery_fails_for_a_pytest_style_class(self):
        result = self.run_gate(self.tree(test_plain=PLAIN, test_class=PYTEST_CLASS))
        self.assert_refused(result, "test_class: 1 pytest-style test(s)", "TestThing")

    def test_named_module_without_unittest_tests_fails(self):
        result = self.run_gate(self.tree(test_functions=PYTEST_FUNCTIONS), "test_functions")
        self.assert_refused(result, "test_functions: 1 pytest-style test(s)")

    def test_named_mixed_module_fails_but_a_named_testcase_passes(self):
        tree = self.tree(test_mixed=MIXED)
        self.assert_refused(self.run_gate(tree, "test_mixed"), "test_mixed: 1 pytest-style test(s)")
        selected = self.run_gate(tree, "test_mixed.Mixed")
        self.assertEqual(selected.returncode, 0, selected.stderr.decode("utf-8", "replace"))

    def test_named_module_or_tree_without_tests_fails(self):
        empty = "import unittest\n"
        self.assert_refused(self.run_gate(self.tree(test_empty=empty), "test_empty"), "test_empty: no unittest tests")
        self.assert_refused(self.run_gate(self.tree()), "no tests were collected")

    def test_real_gate_refuses_a_pytest_only_module(self):
        probe = subprocess.run([sys.executable, "-I", "-c", "import pytest"], capture_output=True, timeout=120)
        if probe.returncode:
            # The module imports pytest; without it the gate fails on that import instead.
            self.skipTest("pytest is not importable by python -I")
        result = subprocess.run([sys.executable, "-I", "-B", str(GATE), "test_verified_issues_r7"],
                                cwd=ROOT, capture_output=True, timeout=600)
        self.assert_refused(result, "test_verified_issues_r7: 6 pytest-style test(s)")


if __name__ == "__main__":
    unittest.main()
