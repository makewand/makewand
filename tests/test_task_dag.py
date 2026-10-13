"""
Unit tests for Makewand Task DAG Engine & Plan Subcommand.
"""

try:  # 测试隔离必须先于 makewand 导入
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand.git_helper import run_git_cmd
from makewand.orchestrator import TaskNode, TaskDAG, decompose_task_to_dag, execute_task_dag


class TestTaskDAG(unittest.TestCase):
    def setUp(self):
        # A multi-task DAG stages its tasks in a private copy of the workspace, so
        # execution tests use a throwaway repository instead of the process cwd.
        temp = tempfile.TemporaryDirectory(prefix="makewand-dag-")
        self.addCleanup(temp.cleanup)
        self.repo = Path(temp.name) / "repo"
        self.repo.mkdir()
        (self.repo / "app.py").write_text("VALUE = 1\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            self.assertEqual(run_git_cmd(["git", *arguments], cwd=str(self.repo))[0], 0)
        sessions = patch("makewand.git_helper.get_active_interactive_working_trees", return_value={})
        sessions.start()
        self.addCleanup(sessions.stop)

    def test_topological_stages_sequential(self):
        t1 = TaskNode("t1", "Task 1", dependencies=[])
        t2 = TaskNode("t2", "Task 2", dependencies=["t1"])
        t3 = TaskNode("t3", "Task 3", dependencies=["t2"])

        dag = TaskDAG("Goal", [t1, t2, t3])
        stages = dag.topological_stages()

        self.assertEqual(len(stages), 3)
        self.assertEqual([t.task_id for t in stages[0]], ["t1"])
        self.assertEqual([t.task_id for t in stages[1]], ["t2"])
        self.assertEqual([t.task_id for t in stages[2]], ["t3"])

    def test_topological_stages_parallel_branches(self):
        # t1 -> (t2, t3) -> t4
        t1 = TaskNode("t1", "Init", dependencies=[])
        t2 = TaskNode("t2", "Frontend", dependencies=["t1"])
        t3 = TaskNode("t3", "Backend", dependencies=["t1"])
        t4 = TaskNode("t4", "E2E Tests", dependencies=["t2", "t3"])

        dag = TaskDAG("Feature", [t1, t2, t3, t4])
        stages = dag.topological_stages()

        self.assertEqual(len(stages), 3)
        self.assertEqual([t.task_id for t in stages[0]], ["t1"])
        stage2_ids = sorted([t.task_id for t in stages[1]])
        self.assertEqual(stage2_ids, ["t2", "t3"])
        self.assertEqual([t.task_id for t in stages[2]], ["t4"])

    def test_task_node_dependency_deduplication(self):
        t1 = TaskNode("t1", "Init", dependencies=[])
        t2 = TaskNode("t2", "Work", dependencies=["t1", "t1", "t1"])
        self.assertEqual(t2.dependencies, ["t1"])

        dag = TaskDAG("Feature", [t1, t2])
        stages = dag.topological_stages()
        self.assertEqual(len(stages), 2)
        self.assertEqual([t.task_id for t in stages[0]], ["t1"])
        self.assertEqual([t.task_id for t in stages[1]], ["t2"])

    def test_decompose_itemized_prompt(self):
        prompt = "1. 重构数据结构 `makewand/model.py` 2. 编写核心逻辑 3. 补充单元测试"
        dag = decompose_task_to_dag(prompt)

        self.assertEqual(len(dag.tasks), 3)
        stages = dag.topological_stages()
        self.assertEqual(len(stages), 3)
        self.assertIn("makewand/model.py", dag.tasks["task-1"].target_files)

    def test_decompose_generic_goal_fallback(self):
        prompt = "升级全部第三方依赖并保证测试通过"
        dag = decompose_task_to_dag(prompt)

        self.assertEqual(len(dag.tasks), 3)
        stages = dag.topological_stages()
        self.assertEqual(len(stages), 3)
        self.assertEqual(dag.tasks["task-1"].status, "pending")

    def test_execute_task_dag_success(self):
        t1 = TaskNode("t1", "Task 1")
        t2 = TaskNode("t2", "Task 2", dependencies=["t1"])
        dag = TaskDAG("Goal", [t1, t2])

        with patch("makewand.orchestrator.run_pipeline", return_value=True):
            ok, summary, results = execute_task_dag(dag, cwd=str(self.repo))
            self.assertTrue(ok)
            self.assertEqual(len(results), 2)
            self.assertEqual(dag.tasks["t1"].status, "passed")
            self.assertEqual(dag.tasks["t2"].status, "passed")

    def test_execute_task_dag_early_failure_stops(self):
        t1 = TaskNode("t1", "Task 1")
        t2 = TaskNode("t2", "Task 2", dependencies=["t1"])
        dag = TaskDAG("Goal", [t1, t2])

        with patch("makewand.orchestrator.run_pipeline", return_value=False):
            ok, summary, results = execute_task_dag(dag, cwd=str(self.repo))
            self.assertFalse(ok)
            self.assertEqual(dag.tasks["t1"].status, "failed")
            self.assertEqual(dag.tasks["t2"].status, "pending")

    def test_execute_task_dag_tiered_dispatch(self):
        t1 = TaskNode("t1", "Design Contracts")
        t2 = TaskNode("t2", "Implement Logic", dependencies=["t1"])
        dag = TaskDAG("Goal", [t1, t2])

        calls = []
        def mock_run_pipeline(*args, **kwargs):
            calls.append(kwargs)
            return True

        with patch("makewand.orchestrator.run_pipeline", side_effect=mock_run_pipeline):
            ok, summary, results = execute_task_dag(
                dag,
                cwd=str(self.repo),
                tiered=True,
                architect_engine="claude",
                worker_engine="local"
            )
            self.assertTrue(ok)
            self.assertEqual(len(calls), 2)
            # Stage 1: Architect (claude)
            self.assertEqual(calls[0].get("forced_engine"), "claude")
            self.assertEqual(calls[0].get("tier"), "power")
            # Stage 2: Worker (local)
            self.assertEqual(calls[1].get("forced_engine"), "local")
            self.assertEqual(calls[1].get("tier"), "fast")

    def test_execute_task_dag_tiered_dispatch_three_stages_with_audit(self):
        t1 = TaskNode("t1", "Design Contracts")
        t2 = TaskNode("t2", "Implement Logic", dependencies=["t1"])
        t3 = TaskNode("t3", "Final Audit and Quality Gate", dependencies=["t2"])
        dag = TaskDAG("Goal", [t1, t2, t3])

        calls = []
        def mock_run_pipeline(*args, **kwargs):
            calls.append(kwargs)
            return True

        with patch("makewand.orchestrator.run_pipeline", side_effect=mock_run_pipeline):
            ok, summary, results = execute_task_dag(
                dag,
                cwd=str(self.repo),
                tiered=True,
                architect_engine="claude",
                worker_engine="local"
            )
            self.assertTrue(ok)
            self.assertEqual(len(calls), 3)
            # Stage 1: Architect (claude, power tier)
            self.assertEqual(calls[0].get("forced_engine"), "claude")
            self.assertEqual(calls[0].get("tier"), "power")
            # Stage 2: Worker (local, fast tier)
            self.assertEqual(calls[1].get("forced_engine"), "local")
            self.assertEqual(calls[1].get("tier"), "fast")
            # Stage 3: Architect (claude, power tier for final audit)
            self.assertEqual(calls[2].get("forced_engine"), "claude")
            self.assertEqual(calls[2].get("tier"), "power")

    def test_execute_task_dag_propagates_local_only(self):
        t1 = TaskNode("t1", "Local Task 1")
        t2 = TaskNode("t2", "Local Task 2", dependencies=["t1"])
        dag = TaskDAG("Offline Goal", [t1, t2])

        calls = []
        def mock_run_pipeline(*args, **kwargs):
            calls.append(kwargs)
            return True

        with patch("makewand.orchestrator.run_pipeline", side_effect=mock_run_pipeline):
            ok, summary, results = execute_task_dag(
                dag,
                cwd=str(self.repo),
                local_only=True
            )
            self.assertTrue(ok)
            self.assertEqual(len(calls), 2)
            self.assertTrue(calls[0].get("local_only"))
            self.assertTrue(calls[1].get("local_only"))

    def test_task_dag_validate_detects_cycles_and_missing_deps(self):
        # 1. Missing dependency
        t1 = TaskNode("t1", "Task 1", dependencies=["ghost-task"])
        dag_missing = TaskDAG("Goal", [t1])
        valid, errs = dag_missing.validate()
        self.assertFalse(valid)
        self.assertTrue(any("ghost-task" in e for e in errs))

        # 2. Self cycle
        t2 = TaskNode("t2", "Task 2", dependencies=["t2"])
        dag_self = TaskDAG("Goal", [t2])
        valid, errs = dag_self.validate()
        self.assertFalse(valid)
        self.assertTrue(any("自循环" in e for e in errs))

        # 3. Circular dependency (tA -> tB -> tA)
        ta = TaskNode("ta", "A", dependencies=["tb"])
        tb = TaskNode("tb", "B", dependencies=["ta"])
        dag_cycle = TaskDAG("Goal", [ta, tb])
        valid, errs = dag_cycle.validate()
        self.assertFalse(valid)
        self.assertTrue(any("循环依赖环路" in e for e in errs))


if __name__ == "__main__":
    unittest.main()

