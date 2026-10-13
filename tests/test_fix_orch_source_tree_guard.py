"""Tests never run a writing task against the makewand checkout itself (test-hazard).

A host-mode task snapshots the whole repository of its cwd and rolls it back on
failure. A test that calls run_pipeline without a temporary cwd therefore used to
snapshot the checkout under test and later revert edits and delete files created
meanwhile in it. Under the test harness the pipeline now refuses such a target
before any lock, transaction, shadow worktree or model call, and every pipeline
call in tests/ passes an explicit cwd.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import ast
import contextlib
import io
import os
import tempfile
import unittest
from unittest.mock import patch

import makewand
from makewand import orchestrator as orch
from makewand.execution_contract import EXIT_UNVERIFIED
from makewand.git_helper import run_git_cmd

TESTS_DIR = Path(__file__).resolve().parent
# Entry points that may write to (or snapshot) their cwd, and the positional
# index of cwd when it is not passed by keyword (None: keyword only).
PIPELINE_ENTRY_POINTS = {"run_pipeline": 1, "_run_pipeline_impl": 1, "run_workflow": None,
                         "run_race": 1, "_run_race_impl": 1}


class SourceTreeGuardTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-source-guard-")
        self.addCleanup(temporary.cleanup)
        self.parent = Path(temporary.name)
        # A stand-in for the makewand checkout: the real one is never a target here.
        self.checkout = self.parent / "checkout"
        (self.checkout / "pkg").mkdir(parents=True)
        (self.checkout / "app.py").write_text("VALUE = 1\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(self.checkout))
            self.assertEqual(code, 0, error)
        # Work in progress of another session in the checkout.
        (self.checkout / "app.py").write_text("VALUE = 2  # unsaved work\n")
        (self.checkout / "pkg" / "new_module.py").write_text("NEW = True\n")

    def run_task(self, target, isolation):
        out = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "_makewand_source_root", create=True,
                                             return_value=self.checkout.resolve()))
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=isolation))
            stack.enter_context(patch.object(orch, "HostWorkspaceTransaction",
                                             side_effect=AssertionError("host transaction on the source tree")))
            stack.enter_context(patch.object(orch, "create_ephemeral_shadow_worktree",
                                             side_effect=AssertionError("shadow worktree of the source tree")))
            dispatch = stack.enter_context(patch.object(orch, "dispatch_task",
                                                        side_effect=AssertionError("model call for the source tree")))
            stack.enter_context(contextlib.redirect_stdout(out))
            result = orch.run_workflow("Implement the feature", cwd=str(target), force_code=True,
                                       auto_fix=False, total_timeout=30)
        return result, dispatch, out.getvalue()

    def test_writing_task_on_the_source_tree_is_refused_before_any_side_effect(self):
        targets = {"checkout": self.checkout, "subdirectory": self.checkout / "pkg", "parent": self.parent}
        for isolation in ((True, None), (False, "dirty working tree")):
            for name, target in targets.items():
                with self.subTest(target=name, host_mode=isolation[0]):
                    result, dispatch, log = self.run_task(target, isolation)
                    self.assertEqual(result.status, "UNVERIFIED")
                    self.assertEqual(result.exit_code, EXIT_UNVERIFIED)
                    dispatch.assert_not_called()
                    self.assertIn("[Makewand Test Harness]", log)
                    self.assertIn(str(self.checkout.resolve()), log)
                    self.assertEqual((self.checkout / "app.py").read_text(), "VALUE = 2  # unsaved work\n")
                    self.assertEqual((self.checkout / "pkg" / "new_module.py").read_text(), "NEW = True\n")

    def test_guard_scope(self):
        real = Path(makewand.__file__).resolve().parent.parent
        self.assertEqual(orch._makewand_source_root(), real)
        # The real checkout and anything inside or above it is protected under the harness.
        for target in (real, real / "tests", real.parent):
            self.assertIsNotNone(orch._test_harness_source_tree_conflict(str(target)), target)
        unrelated = self.parent / "unrelated"
        unrelated.mkdir()
        self.assertIsNone(orch._test_harness_source_tree_conflict(str(unrelated)))
        self.assertIsNone(orch._test_harness_source_tree_conflict(str(self.checkout)))
        # Outside the harness makewand may work on its own repository.
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("MAKEWAND_TEST_ISOLATION_ROOT", None)
            self.assertIsNone(orch._test_harness_source_tree_conflict(str(real)))


class PipelineCallAuditTests(unittest.TestCase):
    def test_every_pipeline_call_in_tests_passes_an_explicit_cwd(self):
        """Without cwd a pipeline would work on the process cwd: the checkout under test."""
        missing = []
        for path in sorted(TESTS_DIR.glob("test_*.py")):
            tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
            for node in ast.walk(tree):
                if not isinstance(node, ast.Call):
                    continue
                func = node.func
                if isinstance(func, ast.Name):
                    name = func.id
                elif isinstance(func, ast.Attribute):
                    base = func.value
                    while isinstance(base, ast.Attribute):
                        base = base.value
                    if isinstance(base, ast.Name) and base.id == "self":
                        continue  # fixture helpers, which pass their own temporary cwd
                    name = func.attr
                else:
                    continue
                if name not in PIPELINE_ENTRY_POINTS:
                    continue
                keywords = {keyword.arg for keyword in node.keywords}
                if "cwd" in keywords or None in keywords or any(isinstance(arg, ast.Starred) for arg in node.args):
                    continue
                position = PIPELINE_ENTRY_POINTS[name]
                if position is not None and len(node.args) > position:
                    continue
                missing.append(f"{path.name}:{node.lineno}: {ast.get_source_segment(path.read_text(encoding='utf-8'), node)}")
        self.assertEqual(missing, [], "pipeline calls without an explicit (temporary) cwd")


if __name__ == "__main__":
    unittest.main()
