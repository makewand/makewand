"""Real Go test output distinguishes executed cases from empty/skipped suites."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import orchestrator as orch


@unittest.skipUnless(shutil.which("go"), "Go toolchain required for actual test-output regression")
class GoTestVerificationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.cache = tempfile.TemporaryDirectory(prefix="makewand-go-gate-cache-")
        cls.addClassCleanup(cls.cache.cleanup)
        cls.go = shutil.which("go")

    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="makewand-go-test-gate-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        (self.root / "go.mod").write_text("module example.test/gate\ngo 1.23\n")
        (self.root / "app.go").write_text("package gate\nfunc Value() int { return 7 }\n")
        self.results = []

    def write_case(self, body):
        (self.root / "app_test.go").write_text('package gate\nimport "testing"\n' + body)

    def run_gate(self):
        def actual_go_process(cmd, workspace, timeout, **kwargs):
            # The boundary under test is Go execution/discovery, not the existing
            # sandbox implementation. Execute the real tool offline with owned
            # caches; no model, network dependency or user Go configuration.
            self.assertEqual(cmd[0:2], ["go", "test"])
            env = dict(os.environ, GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off", GOENV="off",
                       GOFLAGS="-mod=readonly", GOCACHE=self.cache.name, GOPATH=self.cache.name,
                       GOMODCACHE=str(Path(self.cache.name) / "modules"))
            result = subprocess.run([self.go, *cmd[1:]], cwd=workspace, env=env,
                                    capture_output=True, text=True, timeout=timeout)
            self.results.append(result)
            return result.returncode, result.stdout, result.stderr, None
        with patch("makewand.git_helper.get_dirty_files", return_value=[]), \
             patch("makewand.sandbox.run_in_sandbox", side_effect=actual_go_process):
            return orch.run_local_tests(str(self.root), timeout=60)

    def test_mixed_no_test_dependencies_and_executed_case_pass(self):
        dependency = self.root / "dep"
        dependency.mkdir()
        (dependency / "dep.go").write_text("package dep\nfunc Value() int { return 7 }\n")
        self.write_case('func TestValue(t *testing.T) { if Value() != 7 { t.Fatal("value") } }\n')
        passed, details = self.run_gate()
        self.assertEqual(self.results[0].returncode, 0)
        self.assertIn("[no test files]", self.results[0].stdout)
        self.assertTrue(passed, details)
        self.assertIn("Go Tests Passed", details)

    def test_no_go_cases_is_unverified(self):
        passed, details = self.run_gate()
        self.assertEqual(self.results[0].returncode, 0)
        self.assertIn("[no test files]", self.results[0].stdout)
        self.assertFalse(passed)
        self.assertEqual(details.execution_status, "UNVERIFIED")

    def test_all_skipped_go_cases_is_unverified(self):
        self.write_case('func TestSkipped(t *testing.T) { t.Skip("controlled skip") }\n')
        passed, details = self.run_gate()
        self.assertEqual(self.results[0].returncode, 0)
        self.assertFalse(passed)
        self.assertEqual(details.execution_status, "UNVERIFIED")

    def test_testmain_early_exit_is_unverified(self):
        (self.root / "app_test.go").write_text('package gate\nimport ("os"; "testing")\n'
            'func TestMain(m *testing.M) { os.Exit(0) }\n'
            'func TestNeverRun(t *testing.T) { t.Fatal("must execute") }\n')
        passed, details = self.run_gate()
        self.assertEqual(self.results[0].returncode, 0)
        self.assertFalse(passed)
        self.assertEqual(details.execution_status, "UNVERIFIED")

    def test_real_failure_cannot_be_hidden_by_another_passing_case(self):
        self.write_case('func TestPass(t *testing.T) {}\n'
                         'func TestFail(t *testing.T) { t.Fatal("actual deliberate failure") }\n')
        passed, details = self.run_gate()
        self.assertNotEqual(self.results[0].returncode, 0)
        self.assertFalse(passed)
        self.assertNotIsInstance(details, orch.LocalTestsUnavailable)
        self.assertIn("actual deliberate failure", details)
        self.assertIn("Go Tests Failed", details)


if __name__ == "__main__":
    unittest.main(verbosity=2)
