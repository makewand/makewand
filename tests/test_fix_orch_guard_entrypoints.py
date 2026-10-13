"""Every orchestrator entry point that writes to a workspace refuses makewand's own
source tree under the test harness, not only the pipeline (guard-entrypoints).

A race copies cwd into its candidates and an approved candidate is applied back
into it; a multi-task DAG runs its pipelines in a private staging copy (which the
pipeline's guard accepts) and then applies the cumulative result to cwd outside any
pipeline. Without cwd both work on the process cwd: the checkout under test. A
stand-in checkout plays the source tree here; the real one is never a target.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import os
import tempfile
import unittest
from unittest.mock import patch

from makewand import orchestrator as orch, task_dag
from makewand.execution_contract import EXIT_UNVERIFIED
from makewand.git_helper import run_git_cmd
from makewand.task_dag import TaskDAG, TaskNode
from makewand.workflow import last_result


class GuardEntryPointTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-guard-entry-")
        self.addCleanup(temporary.cleanup)
        self.parent = Path(temporary.name)
        self.checkout = self.parent / "checkout"
        (self.checkout / "pkg").mkdir(parents=True)
        (self.checkout / "app.py").write_text("VALUE = 1\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(self.checkout))
            self.assertEqual(code, 0, error)
        # Work in progress of another session in the checkout.
        (self.checkout / "app.py").write_text("VALUE = 2  # unsaved work\n")
        source = patch.object(orch, "_makewand_source_root", return_value=self.checkout.resolve())
        source.start()
        self.addCleanup(source.stop)

    def assert_checkout_untouched(self):
        self.assertEqual((self.checkout / "app.py").read_text(), "VALUE = 2  # unsaved work\n")
        self.assertEqual(sorted(path.name for path in self.checkout.iterdir()), [".git", "app.py", "pkg"])

    @contextlib.contextmanager
    def race_fixtures(self):
        out = io.StringIO()
        engines = ("claude", "codex")
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={name: {"status": "healthy"} for name in engines}))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=lambda name: name in engines))
            stack.enter_context(patch("makewand.config.get_active_providers", return_value=list(engines)))
            stack.enter_context(patch.object(orch, "clone_isolated_worktree",
                                             side_effect=AssertionError("candidate copy of the source tree")))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=AssertionError("model call for the source tree")))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(out))
            yield out

    @contextlib.contextmanager
    def process_cwd(self, path):
        previous = os.getcwd()
        os.chdir(path)
        try:
            yield
        finally:
            os.chdir(previous)

    def test_race_refuses_the_source_tree_before_any_side_effect(self):
        for name, target in (("checkout", self.checkout), ("subdirectory", self.checkout / "pkg"), ("parent", self.parent)):
            with self.subTest(target=name), self.race_fixtures() as out:
                code = orch.run_race("Implement the feature", cwd=str(target), engine_a="claude", engine_b="codex", timeout=30)
            self.assertEqual(code, EXIT_UNVERIFIED, out.getvalue())
            self.assertEqual(last_result().status, "UNVERIFIED")
            self.assertIn("[Makewand Test Harness]", out.getvalue())
            self.assertIn(str(self.checkout.resolve()), out.getvalue())
        self.assert_checkout_untouched()

    def test_race_without_cwd_refuses_the_process_cwd(self):
        with self.race_fixtures() as out, self.process_cwd(self.checkout):
            code = orch.run_race("Implement the feature", cwd=None, engine_a="claude", engine_b="codex", timeout=30)
        self.assertEqual(code, EXIT_UNVERIFIED, out.getvalue())
        self.assertIn("[Makewand Test Harness]", out.getvalue())
        self.assert_checkout_untouched()

    def test_direct_race_implementation_refuses_the_source_tree(self):
        with self.race_fixtures() as out:
            code = orch._run_race_impl("Implement the feature", cwd=str(self.checkout), engine_a="claude",
                                       engine_b="codex", timeout=30)
        self.assertEqual(code, EXIT_UNVERIFIED, out.getvalue())
        self.assert_checkout_untouched()

    def test_race_workflow_refuses_the_source_tree(self):
        with self.race_fixtures() as out:
            result = orch.run_workflow("Implement the feature", cwd=str(self.checkout), workflow="race",
                                       force_code=True, total_timeout=30)
        self.assertEqual(result.status, "UNVERIFIED", out.getvalue())
        self.assertEqual(result.exit_code, EXIT_UNVERIFIED)
        self.assertIn("[Makewand Test Harness]", out.getvalue())
        self.assert_checkout_untouched()

    def dag(self):
        return TaskDAG("goal", [TaskNode("t1", "first", "write a file"),
                                TaskNode("t2", "second", "write another file", dependencies=["t1"])])

    def test_dag_refuses_to_deliver_into_the_source_tree(self):
        """drivers/dag_probe.py: two tasks, no cwd, staged files applied into the source root."""
        stages = []

        def fake_pipeline(prompt, **kwargs):
            stages.append(kwargs["cwd"])
            index = len(list(Path(kwargs["cwd"]).glob("DAG_STAGE_*.txt")))
            Path(kwargs["cwd"], f"DAG_STAGE_{index}.txt").write_text("stage output\n")
            return True

        for name, cwd in (("explicit cwd", str(self.checkout)), ("no cwd", None)):
            with self.subTest(case=name), patch.object(orch, "run_pipeline", side_effect=fake_pipeline), \
                    patch.object(orch, "check_working_tree_isolation", return_value=(True, None)), \
                    self.process_cwd(self.checkout), contextlib.redirect_stdout(io.StringIO()) as out:
                ok, summary, results = orch.execute_task_dag(self.dag(), cwd=cwd)
            self.assertFalse(ok)
            self.assertEqual(results, [])
            self.assertIn(str(self.checkout.resolve()), summary)
            self.assertIn("[Makewand Test Harness]", out.getvalue())
            self.assertEqual(last_result().status, "UNVERIFIED")
        self.assertEqual(stages, [])
        self.assertEqual(list(self.checkout.glob("DAG_STAGE_*.txt")), [])
        self.assert_checkout_untouched()

    def test_dag_on_another_repository_is_delegated_unchanged(self):
        other = self.parent / "other"
        other.mkdir()
        with patch.object(task_dag, "execute_task_dag", return_value=(True, "done", [{"task": "t1"}])) as inner, \
                patch.object(orch, "run_pipeline", return_value=False), contextlib.redirect_stdout(io.StringIO()):
            outcome = orch.execute_task_dag(self.dag(), cwd=str(other), tier="deep", auto_fix=False)
        self.assertEqual(outcome, (True, "done", [{"task": "t1"}]))
        self.assertEqual(inner.call_args.args[1], str(other))
        self.assertEqual(inner.call_args.kwargs, {"tier": "deep", "auto_fix": False})


if __name__ == "__main__":
    unittest.main()
