"""One rule, one source root: the DAG's early harness refusal and apply agree (guard-delegation).

Under the test harness makewand never writes to its own source tree.
candidate.harness_source_tree_conflict delegates to the shared rule
(git_helper.harness_source_tree_conflict) with candidate._makewand_source_root,
the patch point tests use to stand a fixture checkout in for the source tree. But
task_dag.execute_task_dag's early refusal asked the shared rule without source=,
so it judged git_helper's own source root: with only candidate's root replaced, a
multi-task DAG aimed at the stand-in ran its stages and was refused only at apply
(SourceTreeGuardTests had to patch both modules). The early check now passes
candidate's source root, so both checks judge the same tree.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import os
import unittest
from unittest.mock import patch

from makewand import candidate as candidate_module, git_helper, task_dag
from makewand.workflow import last_result
from test_fix_orch_dag_r3 import DagFixture


class OneSourceRootTests(DagFixture):
    def setUp(self):
        super().setUp()
        self.assertTrue(os.environ.get("MAKEWAND_TEST_ISOLATION_ROOT"))
        self.outer = self.make_repo("outer", files={"README": "outer\n", "makewand-src/app.py": "A = 1\n"})
        self.source = self.outer / "makewand-src"
        # Only candidate's patch point stands in for the checkout under test.
        patcher = patch.object(candidate_module, "_makewand_source_root", return_value=self.source.resolve())
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_dag_refuses_the_stand_in_source_tree_before_any_stage(self):
        with patch("makewand.orchestrator.run_pipeline") as pipeline, contextlib.redirect_stdout(io.StringIO()), \
                contextlib.redirect_stderr(io.StringIO()):
            ok, summary, results = task_dag.execute_task_dag(self.code_chain(), cwd=str(self.source))
        self.assertFalse(ok)
        self.assertIn("测试环境中拒绝执行 DAG 子任务", summary)
        self.assertIn("未执行任何子任务", summary)
        pipeline.assert_not_called()
        self.assertEqual((results, self.store()), ([], []))
        self.assertEqual(last_result().status, "UNVERIFIED")
        self.assertEqual(sorted(path.name for path in self.source.iterdir()), ["app.py"])

    def test_dag_early_check_and_apply_ask_the_shared_rule_with_candidates_root(self):
        shared = git_helper.harness_source_tree_conflict
        with patch.object(git_helper, "harness_source_tree_conflict", side_effect=shared) as rule, \
                patch("makewand.orchestrator.run_pipeline"), contextlib.redirect_stdout(io.StringIO()):
            task_dag.execute_task_dag(self.code_chain(), cwd=str(self.source / "sub-dir-that-is-inside"))
            message = candidate_module.harness_source_tree_conflict(str(self.source))
        self.assertIn("测试环境中拒绝把候选应用到工作区", message)
        self.assertEqual([call.kwargs.get("source") for call in rule.call_args_list], [self.source.resolve()] * 2)

    def test_other_workspaces_still_pass_the_early_check(self):
        sibling = self.make_repo("sibling")
        ok, summary, _, _ = self.run_dag(self.code_chain(), sibling)
        self.assertTrue(ok, summary)
        self.assertNotIn("测试环境中拒绝", summary)


if __name__ == "__main__":
    unittest.main()
