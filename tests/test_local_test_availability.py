"""Missing runners and malformed configuration cannot become passed checks."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import orchestrator as orch
from test_g1_datasafety_pipeline import PipelineHarness, make_project


class LocalTestAvailabilityTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def run_gate(self, tools=()):
        with patch("makewand.git_helper.get_dirty_files", return_value=[]), \
             patch("shutil.which", side_effect=lambda name: "/fixture/" + name if name in tools else None), \
             patch("makewand.sandbox.run_in_sandbox", return_value=(0, "passed", "", None)) as runner:
            result = orch.run_local_tests(str(self.root))
        return result, runner

    def assert_unavailable(self, tools=()):
        (passed, details), runner = self.run_gate(tools)
        self.assertFalse(passed)
        self.assertEqual(details.execution_status, "UNVERIFIED")
        runner.assert_not_called()

    def test_go_missing_runner_is_unverified(self):
        (self.root / "go.mod").write_text("module fixture.invalid/example\n")
        self.assert_unavailable()

    def test_rust_missing_runner_is_unverified(self):
        (self.root / "Cargo.toml").write_text('[package]\nname="fixture"\n')
        self.assert_unavailable()

    def test_node_missing_runner_is_unverified(self):
        (self.root / "package.json").write_text(json.dumps({"scripts": {"test": "node --test"}}))
        self.assert_unavailable()

    def test_malformed_node_config_is_unverified(self):
        for value in ('{broken', '[]', '{"scripts":[]}', '{"scripts":{"test":null}}', '{"scripts":{"test":""}}'):
            with self.subTest(value=value):
                (self.root / "package.json").write_text(value)
                self.assert_unavailable(("npm",))

    def test_composite_cannot_skip_a_missing_suite(self):
        (self.root / "test_app.py").write_text("def test_app(): pass\n")
        (self.root / "go.mod").write_text("module fixture.invalid/example\n")
        self.assert_unavailable()

    def test_node_without_test_script_is_not_a_missing_suite(self):
        (self.root / "package.json").write_text('{"scripts":{"start":"node app.js"}}')
        (passed, details), runner = self.run_gate()
        self.assertTrue(passed)
        self.assertIsNone(details)
        runner.assert_not_called()

    def test_unittest_zero_discovery_cannot_pass(self):
        (self.root / "test_app.py").write_text("def test_app(): pass\n")
        with patch("makewand.git_helper.get_dirty_files", return_value=[]), \
             patch.dict(sys.modules, {"pytest": None}), patch("shutil.which", return_value=None), \
             patch("makewand.sandbox.run_in_sandbox", return_value=(0, "", "Ran 0 tests in 0.000s\nOK", None)):
            passed, details = orch.run_local_tests(str(self.root))
        self.assertFalse(passed)
        self.assertEqual(details.execution_status, "UNVERIFIED")

    def test_sandbox_infrastructure_failures_fuse_as_unverified(self):
        (self.root / "test_app.py").write_text("def test_app(): pass\n")
        cases = [
            (-1, "", "bwrap not available", "SandboxUnavailable"),
            (-1, "", "config invalid", "SandboxConfigError"),
            (1, "", "bwrap: Can't bind mount: No such file", None),
            (1, "bwrap: initialization failed", "", None),
            (1, "bwrap: initialization failed", "warning: non-bwrap secondary stderr", None),
        ]
        for code, out, err, cat in cases:
            with self.subTest(code=code, cat=cat, err=err):
                with patch("makewand.git_helper.get_dirty_files", return_value=[]), \
                     patch.dict(sys.modules, {"pytest": None}), patch("shutil.which", return_value=None), \
                     patch("makewand.sandbox.run_in_sandbox", return_value=(code, out, err, cat)):
                    passed, details = orch.run_local_tests(str(self.root))
                self.assertFalse(passed)
                self.assertEqual(getattr(details, "execution_status", None), "UNVERIFIED")
                self.assertIn("沙箱容器基础设施故障，本地测试阻断", str(details))
                if out and out.strip().startswith("bwrap:"):
                    self.assertIn(out.strip(), str(details))
                elif err and err.strip().startswith("bwrap:"):
                    self.assertIn(err.strip(), str(details))


class PipelineMissingEvidenceTests(PipelineHarness):
    def test_missing_test_evidence_stops_before_review_and_restores_user_files(self):
        project = make_project(self.base / "no-tests")
        original = (project / "app.py").read_bytes()
        def coder(directory):
            (directory / "app.py").write_text("VALUE = 42\n")
            return True, "implemented", None
        self.assertFalse(self.run_pipeline(project, coder, review_pass=True, test_result=(True, None)))
        self.assertEqual(len(self.dispatch_calls), 1)
        self.assertEqual((project / "app.py").read_bytes(), original)
        self.assertIn("验收未验证", self.output)


if __name__ == "__main__":
    unittest.main()
