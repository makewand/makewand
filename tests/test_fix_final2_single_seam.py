"""The test-harness guard has one source root seam: git_helper._makewand_source_root (single-seam).

The guard's rule (git_helper.harness_source_tree_conflict) had three copies of its
source root: git_helper's, candidate's and orchestrator's, the latter two passed as
source= (task_dag passed candidate's), and git_helper kept a dead alias of the guard.
A test replacing one copy saw some entry points judge its stand-in checkout and others
the real one. Now the rule takes no source argument and reads only git_helper's seam:
replacing it alone makes every writing entry point refuse the stand-in.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import inspect
import io
import os
import unittest
from unittest.mock import patch

from makewand import candidate as candidate_module, git_helper, orchestrator as orch, task_dag
from makewand.task_dag import TaskDAG, TaskNode
from makewand.workflow import last_result
from test_fix_orch_dag_r3 import DagFixture


class SingleSeamTests(DagFixture):
    def setUp(self):
        super().setUp()
        self.assertTrue(os.environ.get("MAKEWAND_TEST_ISOLATION_ROOT"))
        self.source = self.make_repo("checkout", files={"app.py": "A = 1\n", "pkg/mod.py": "M = 1\n"})
        patcher = patch.object(git_helper, "_makewand_source_root", return_value=self.source.resolve())
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_there_is_one_seam_and_no_source_argument(self):
        for module in (candidate_module, orch, task_dag):
            with self.subTest(module=module.__name__):
                self.assertFalse(hasattr(module, "_makewand_source_root"))
        self.assertNotIn("source", inspect.signature(git_helper.harness_source_tree_conflict).parameters)
        self.assertFalse(hasattr(git_helper, "_test_harness_source_conflict"))

    def test_replacing_the_one_seam_makes_every_entry_point_refuse_the_stand_in(self):
        target = str(self.source / "pkg")
        apply_refusal = candidate_module.harness_source_tree_conflict(target)
        self.assertIn("测试环境中拒绝把候选应用到工作区", apply_refusal or "")
        self.assertIn(str(self.source.resolve()), apply_refusal)
        self.assertIn(str(self.source.resolve()), orch._test_harness_source_tree_conflict(target) or "")
        txn = git_helper.HostWorkspaceTransaction(target)
        self.assertIn("测试环境中拒绝建立宿主模式事务", txn.capture_pre_snapshot() or "")
        single = TaskDAG("goal", [TaskNode("task-1", "only", "write a file")])
        with patch("makewand.orchestrator.run_pipeline") as pipeline, contextlib.redirect_stdout(io.StringIO()):
            ok, summary, _ = task_dag.execute_task_dag(single, cwd=target)
        self.assertFalse(ok)
        self.assertIn("测试环境中拒绝执行 DAG 子任务", summary)
        pipeline.assert_not_called()
        self.assertEqual(last_result().status, "UNVERIFIED")
        with patch.object(orch._task_dag, "execute_task_dag") as inner, contextlib.redirect_stdout(io.StringIO()):
            ok, summary, _ = orch.execute_task_dag(single, cwd=target)
        self.assertFalse(ok)
        inner.assert_not_called()
        self.assertEqual(git(self.source, "status", "--porcelain"), "")

    def test_the_real_checkout_is_not_the_stand_ins(self):
        real = Path(git_helper.__file__).resolve().parent.parent
        self.assertIsNone(candidate_module.harness_source_tree_conflict(str(real)))


def git(repo, *arguments):
    from makewand.git_helper import run_git_cmd
    code, out, error = run_git_cmd(["git", *arguments], cwd=str(repo))
    assert code == 0, error
    return out


if __name__ == "__main__":
    unittest.main()
