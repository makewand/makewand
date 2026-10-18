"""Multi-stage DAG tasks build on earlier stages and report delivery honestly (py-orch-4).

Every stage runs through the real pipeline (mocked provider CLIs, real isolation
check, host transaction, review gate and candidate application). Later stages must
see earlier stages' results, and the final report/exit status must say what was
applied to the workspace and what is still pending.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import argparse
import contextlib
import io
import os
import re
import tempfile
import unittest
from unittest.mock import patch

from makewand import cli, config, orchestrator as orch
from makewand.candidate import CandidateManager
from makewand.execution_contract import EXIT_PASSED
from makewand.git_helper import run_git_cmd
from makewand.task_dag import TaskDAG, TaskNode, execute_task_dag
from makewand.workflow import last_result

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")
TASK_ID = re.compile(r"DAG 拓扑子任务 (task-\d+)")


class DagChainingTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="makewand-dag-chain-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.candidates = self.root / "candidates"
        self.seen = {}
        self.reject = set()
        for patcher in (patch.object(config, "CANDIDATES_DIR", self.candidates),
                        patch.object(orch, "CANDIDATES_DIR", self.candidates)):
            patcher.start()
            self.addCleanup(patcher.stop)

    def make_repo(self, git=True, dirty=False):
        repo = self.root / "repo"
        repo.mkdir()
        (repo / "app.py").write_text("VALUE = 1\n")
        if git:
            for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                              ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
                self.assertEqual(run_git_cmd(["git", *arguments], cwd=str(repo))[0], 0)
        if dirty:
            (repo / "app.py").write_text("VALUE = 1  # user work in progress\n")
        return repo

    @staticmethod
    def dag():
        return TaskDAG("build feature", [
            TaskNode("task-1", "contracts", "write step1"),
            TaskNode("task-2", "implement", "write step2 building on step1", dependencies=["task-1"]),
            TaskNode("task-3", "tests", "write step3 building on step2", dependencies=["task-2"]),
        ])

    @contextlib.contextmanager
    def fixtures(self):
        engines = ("claude", "codex")

        def provider(engine):
            def execute(prompt, **kwargs):
                if kwargs.get("readonly"):
                    # A stage's review diff names the step file that stage wrote.
                    rejected = any(f"step{task[-1]}.txt" in prompt for task in self.reject)
                    verdict = "false" if rejected else "true"
                    defects = '["[P1] rejected by fixture"]' if rejected else "[]"
                    return True, f'{VERDICT_TAG.search(prompt).group(0)}: {{"pass": {verdict}, "defects": {defects}}}', None
                task = TASK_ID.search(prompt).group(1)
                workspace = Path(kwargs["cwd"])
                self.seen[task] = sorted(path.name for path in workspace.glob("step*.txt"))
                (workspace / f"step{task[-1]}.txt").write_text(f"output of {task}\n")
                return True, "implemented", None
            return execute

        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch.os, "getloadavg", return_value=(0, 0, 0)))
            stack.enter_context(patch("makewand.git_helper.get_active_interactive_working_trees", return_value={}))
            stack.enter_context(patch("makewand.sandbox.verify_writable_sandbox_or_authorized", return_value=(True, None, None)))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={name: {"status": "healthy"} for name in engines}))
            stack.enter_context(patch.object(orch, "select_optimal_engine_pair", return_value=(
                list(engines), list(reversed(engines)), dict(primary_coder="claude", primary_reviewer="codex", reasons=[]))))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=lambda name: name in engines))
            stack.enter_context(patch("makewand.config.get_active_providers", return_value=list(engines)))
            stack.enter_context(patch("makewand.usage.record_engine_usage"))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, "fixture tests passed")))
            for name in engines:
                stack.enter_context(patch.object(orch, f"execute_{name}_task", side_effect=provider(name)))
            yield

    def execute(self, repo):
        with self.fixtures(), contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()):
            ok, summary, results = execute_task_dag(self.dag(), cwd=str(repo), tier="deep", auto_fix=False)
        return ok, summary, results, out.getvalue()

    def steps(self, repo):
        return sorted(path.name for path in repo.glob("step*.txt"))

    def test_later_stages_build_on_earlier_results_and_the_result_is_applied_once(self):
        for git in (True, False):
            with self.subTest(git=git):
                self.seen.clear()
                if (self.root / "repo").exists():
                    import shutil
                    shutil.rmtree(self.root / "repo")
                repo = self.make_repo(git=git)
                ok, summary, results, _ = self.execute(repo)
                self.assertTrue(ok, summary)
                self.assertEqual(self.seen, {"task-1": [], "task-2": ["step1.txt"],
                                             "task-3": ["step1.txt", "step2.txt"]})
                self.assertEqual(self.steps(repo), ["step1.txt", "step2.txt", "step3.txt"])
                self.assertEqual([item["status"] for item in results], ["passed"] * 3)
                self.assertIn("已一次性应用到工作区", summary)
                self.assertEqual(last_result().status, "PASSED")

    def test_unsafe_workspace_gets_one_pending_cumulative_candidate(self):
        repo = self.make_repo(dirty=True)
        ok, summary, _, _ = self.execute(repo)
        self.assertTrue(ok, summary)
        self.assertEqual(self.seen["task-3"], ["step1.txt", "step2.txt"])
        # Nothing was written into the dirty workspace; the report says it is pending.
        self.assertEqual(self.steps(repo), [])
        self.assertEqual((repo / "app.py").read_text(), "VALUE = 1  # user work in progress\n")
        race_id = re.search(r"makewand apply (dg_[0-9a-f]+) --candidate A", summary).group(1)
        self.assertIn("尚未交付到工作区", summary)
        applied, files, message = CandidateManager.apply_candidate(race_id, "A")
        self.assertTrue(applied, message)
        self.assertEqual(self.steps(repo), ["step1.txt", "step2.txt", "step3.txt"])
        self.assertEqual((repo / "app.py").read_text(), "VALUE = 1  # user work in progress\n")

    def test_failed_stage_reports_undelivered_progress_with_failing_exit_status(self):
        repo = self.make_repo()
        self.reject = {"task-2"}
        ok, summary, results, _ = self.execute(repo)
        self.assertFalse(ok)
        self.assertNotEqual(last_result().exit_code, EXIT_PASSED)
        self.assertEqual([(item["task"], item["status"]) for item in results],
                         [("task-1", "passed"), ("task-2", "failed")])
        self.assertNotIn("task-3", self.seen)
        # The workspace is untouched; the verified part is sealed and only applied on request.
        self.assertEqual(self.steps(repo), [])
        self.assertIn("工作区未被修改", summary)
        race_id = re.search(r"makewand apply (dg_[0-9a-f]+) --candidate A", summary).group(1)
        applied, _, message = CandidateManager.apply_candidate(race_id, "A")
        self.assertTrue(applied, message)
        self.assertEqual(self.steps(repo), ["step1.txt"])

    def test_plan_command_prints_the_delivery_and_matching_exit_code(self):
        repo = self.make_repo(dirty=True)
        args = argparse.Namespace(cwd=str(repo), prompt="1. write step1 2. write step2 3. write step3", tier="deep",
                                  repo_trust="trusted", json=False, execute=True, effort=None, tiered=False,
                                  architect=None, worker=None, local_only=False)
        with self.fixtures(), contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()), \
                self.assertRaises(SystemExit) as raised:
            cli.cmd_plan(args)
        log = out.getvalue()
        self.assertEqual(raised.exception.code, EXIT_PASSED)
        self.assertNotIn("全部 DAG 拓扑阶段均已高质量交付验收", log)
        self.assertRegex(log, r"尚未交付到工作区.*makewand apply dg_[0-9a-f]+ --candidate A")
        self.assertEqual(self.steps(repo), [])

        self.reject = {"task-1"}
        with self.fixtures(), contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()), \
                self.assertRaises(SystemExit) as raised:
            cli.cmd_plan(args)
        self.assertNotEqual(raised.exception.code, EXIT_PASSED)
        self.assertIn("没有任何子任务通过，工作区未被修改", out.getvalue())

    def test_private_stage_never_falls_back_to_a_shadow_worktree(self):
        repo = self.make_repo()
        with self.fixtures(), contextlib.redirect_stdout(io.StringIO()), \
                patch.object(orch.HostWorkspaceTransaction, "capture_pre_snapshot", return_value="snapshot refused"), \
                patch.object(orch, "create_ephemeral_shadow_worktree") as shadow:
            ok = orch.run_pipeline("【DAG 拓扑子任务 task-1: contracts】\n", cwd=str(repo), force_code=True,
                                   auto_fix=False, _private_workspace=True)
        self.assertFalse(ok)
        shadow.assert_not_called()
        self.assertEqual(self.seen, {})

    def test_single_task_dag_still_runs_directly_in_the_workspace(self):
        repo = self.make_repo()
        calls = []
        with patch("makewand.orchestrator.run_pipeline", side_effect=lambda *a, **k: calls.append(k) or True), \
                contextlib.redirect_stdout(io.StringIO()):
            ok, summary, _ = execute_task_dag(TaskDAG("goal", [TaskNode("task-1", "only")]), cwd=str(repo))
        self.assertTrue(ok)
        self.assertEqual(calls[0]["cwd"], str(repo))
        self.assertNotIn("_private_workspace", calls[0])
        self.assertFalse(self.candidates.exists() and any(self.candidates.iterdir()))


if __name__ == "__main__":
    unittest.main()
