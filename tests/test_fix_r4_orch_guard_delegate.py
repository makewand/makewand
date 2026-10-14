"""The orchestrator's test-harness guard is git_helper's one definition, and the race's typed
result names the refusal (guard-delegate).

orchestrator._test_harness_source_tree_conflict kept its own copy of the rule that
git_helper (host transaction) and candidate (apply) also had; it now delegates to
git_helper.harness_source_tree_conflict, whose one source root
(git_helper._makewand_source_root) tests replace with a stand-in checkout. run_race
refused such a target with only a printed line: its typed result had status
UNVERIFIED and error None. A stand-in checkout plays the source tree; the real one is
never a target.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import tempfile
import unittest
from unittest.mock import patch

from makewand import git_helper, orchestrator as orch
from makewand.execution_contract import EXIT_UNVERIFIED
from makewand.git_helper import run_git_cmd
from makewand.workflow import last_result


class GuardDelegateTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-guard-delegate-")
        self.addCleanup(temporary.cleanup)
        self.parent = Path(temporary.name)
        self.checkout = self.parent / "checkout"
        (self.checkout / "pkg").mkdir(parents=True)
        (self.checkout / "app.py").write_text("VALUE = 1\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(self.checkout))
            self.assertEqual(code, 0, error)
        source = patch.object(git_helper, "_makewand_source_root", return_value=self.checkout.resolve())
        source.start()
        self.addCleanup(source.stop)

    def test_guard_is_git_helpers_definition(self):
        asked = []

        def shared(path, action="执行会修改文件的任务"):
            asked.append(path)
            return "refused by the shared definition"

        with patch.object(git_helper, "harness_source_tree_conflict", side_effect=shared):
            self.assertEqual(orch._test_harness_source_tree_conflict(str(self.parent / "elsewhere")),
                             "refused by the shared definition")
        self.assertEqual(asked, [str(self.parent / "elsewhere")])

    def test_guard_matches_git_helper_for_every_target(self):
        other = self.parent / "other"
        other.mkdir()
        for target in (self.checkout, self.checkout / "pkg", self.parent, other):
            with self.subTest(target=target.name):
                expected = git_helper.harness_source_tree_conflict(str(target))
                self.assertEqual(orch._test_harness_source_tree_conflict(str(target)), expected)
                self.assertEqual(expected is None, target == other)

    def test_race_refusal_carries_its_reason_in_the_typed_result(self):
        engines = ("claude", "codex")
        out = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={e: {"status": "healthy"} for e in engines}))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=lambda name: name in engines))
            stack.enter_context(patch("makewand.config.get_active_providers", return_value=list(engines)))
            stack.enter_context(patch.object(orch, "clone_isolated_worktree",
                                             side_effect=AssertionError("candidate copy of the source tree")))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=AssertionError("model call for the source tree")))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(out))
            code = orch.run_race("Implement the feature", cwd=str(self.checkout), engine_a="claude", engine_b="codex",
                                 timeout=30)
        self.assertEqual(code, EXIT_UNVERIFIED, out.getvalue())
        result = last_result()
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIsNotNone(result.error)
        self.assertIn(str(self.checkout.resolve()), result.error)
        self.assertIn(result.error, out.getvalue())
        self.assertEqual((self.checkout / "app.py").read_text(), "VALUE = 1\n")


if __name__ == "__main__":
    unittest.main()
