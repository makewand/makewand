"""Multi-stage DAG delivery stays truthful in the cases round 1 missed (py-orch-4 r2).

Every stage runs through the real pipeline (mocked provider CLIs, real host
transaction, review gate and candidate seal/apply). Read-only nodes review the
cumulative result and never contribute changes; a DAG started in a subdirectory
sees the whole repository; results apply cannot deliver are never offered to it;
pending candidates built on the user's work in progress apply once it is
committed; a running DAG's staging copy survives concurrent pruning; an
interrupted DAG reports what it kept. Listings label records by what happened.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import argparse
import contextlib
import io
import json
import os
import re
import shutil
import tempfile
import unittest
from datetime import datetime, timedelta
from unittest.mock import patch

from makewand import cli, config, orchestrator as orch, task_dag
from makewand.candidate import CandidateManager, build_manifest
from makewand.execution_contract import EXIT_PASSED
from makewand.git_helper import clone_isolated_worktree, run_git_cmd
from makewand.task_dag import TaskDAG, TaskNode, execute_task_dag
from makewand.workflow import last_result

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")
TASK_ID = re.compile(r"DAG 拓扑子任务 (task-\d+)")
RACE_ID = re.compile(r"\b(dg_[0-9a-f]{12})\b")


def git(repo, *arguments):
    code, out, error = run_git_cmd(["git", *arguments], cwd=str(repo))
    assert code == 0, error
    return out


class DagRoundTwoTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="makewand-dag-r2-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.candidates = self.root / "candidates"
        self.reviews = []
        self.coded = []
        for patcher in (patch.dict(os.environ, {"MAKEWAND_STRICT_NONCE": "1"}),
                        patch.object(config, "CANDIDATES_DIR", self.candidates),
                        patch.object(orch, "CANDIDATES_DIR", self.candidates)):
            patcher.start()
            self.addCleanup(patcher.stop)

    def make_repo(self, name="repo", files=None, dirty=None):
        repo = self.root / name
        repo.mkdir()
        for relative, text in (files or {"app.py": "VALUE = 1\n"}).items():
            (repo / relative).parent.mkdir(parents=True, exist_ok=True)
            (repo / relative).write_text(text)
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            git(repo, *arguments)
        for relative, text in (dirty or {}).items():
            (repo / relative).write_text(text)
        return repo

    @staticmethod
    def write_step(task, workspace):
        (workspace / f"step{task[-1]}.txt").write_text(f"output of {task}\n")

    @staticmethod
    def chain(third=("tests", "write step3 building on step2")):
        return TaskDAG("build feature", [
            TaskNode("task-1", "contracts", "write step1"),
            TaskNode("task-2", "implement", "write step2 building on step1", dependencies=["task-1"]),
            TaskNode("task-3", third[0], third[1], dependencies=["task-2"]),
        ])

    @contextlib.contextmanager
    def fixtures(self, coder=None, reviewer=None, tests=(True, "fixture tests passed")):
        engines = ("claude", "codex")
        coder = coder or self.write_step

        def provider(engine):
            def execute(prompt, **kwargs):
                if kwargs.get("readonly"):
                    self.reviews.append(prompt)
                    tag = VERDICT_TAG.search(prompt)
                    if reviewer is not None:
                        return reviewer(prompt, tag.group(0) if tag else None, Path(kwargs["cwd"]))
                    if tag is None:
                        return True, "explanation of the design", None
                    return True, f'{tag.group(0)}: {{"pass": true, "defects": []}}', None
                task = TASK_ID.search(prompt).group(1)
                self.coded.append(task)
                coder(task, Path(kwargs["cwd"]))
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
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=tests))
            for name in engines:
                stack.enter_context(patch.object(orch, f"execute_{name}_task", side_effect=provider(name)))
            yield

    def run_dag(self, dag, cwd, **fixtures):
        with self.fixtures(**fixtures), contextlib.redirect_stdout(io.StringIO()) as out, \
                contextlib.redirect_stderr(io.StringIO()):
            ok, summary, results = execute_task_dag(dag, cwd=str(cwd), tier="deep", auto_fix=False)
        return ok, summary, results, out.getvalue()

    @staticmethod
    def steps(directory):
        return sorted(path.name for path in directory.glob("step*.txt"))

    def store(self):
        """(name, has meta.json) of every entry in the candidate store."""
        if not self.candidates.exists():
            return []
        return sorted((entry.name, (entry / "meta.json").exists()) for entry in self.candidates.iterdir())

    # -- (a) read-only nodes --------------------------------------------------
    def test_review_node_reviews_the_cumulative_changes_and_is_not_reported_as_tested(self):
        repo = self.make_repo()
        dag = self.chain(("审查实现", "审查前序子任务的改动，只分析不要修改代码"))
        ok, summary, results, _ = self.run_dag(dag, repo)
        self.assertTrue(ok, summary)
        node_reviews = [prompt for prompt in self.reviews if "DAG 拓扑子任务 task-3" in prompt]
        self.assertEqual(len(node_reviews), 1, summary)
        self.assertIn("step1.txt", node_reviews[0])
        self.assertIn("step2.txt", node_reviews[0])
        self.assertEqual(self.coded, ["task-1", "task-2"])
        self.assertEqual([item["kind"] for item in results], ["code", "code", "review"])
        self.assertNotIn("全部 3 个子任务均通过本地测试", summary)
        self.assertIn("代码子任务 task-1, task-2 均通过本地测试与独立审查", summary)
        self.assertIn("只读子任务 task-3 (审查: 已独立审查此前的累积改动)", summary)
        self.assertEqual(self.steps(repo), ["step1.txt", "step2.txt"])

    def test_files_written_during_a_readonly_node_are_discarded_and_reported(self):
        def reviewer(prompt, tag, cwd):
            if tag is None:  # the explain dispatch of task-2; its read-only mode is not enforced
                (cwd / "unreviewed.py").write_text("import os  # written during a read-only dispatch\n")
                return True, "explanation of step1", None
            return True, f'{tag}: {{"pass": true, "defects": []}}', None

        dag = TaskDAG("goal", [
            TaskNode("task-1", "contracts", "write step1"),
            TaskNode("task-2", "解释设计", "只解释 step1 的设计思路", dependencies=["task-1"]),
            TaskNode("task-3", "implement", "write step3", dependencies=["task-2"]),
        ])
        for dirty in (False, True):
            with self.subTest(dirty=dirty):
                repo = self.make_repo(f"repo-{dirty}", dirty={"app.py": "VALUE = 1  # WIP\n"} if dirty else None)
                ok, summary, results, _ = self.run_dag(dag, repo, reviewer=reviewer)
                self.assertTrue(ok, summary)
                self.assertEqual([item["kind"] for item in results], ["code", "explain", "code"])
                self.assertIn("已丢弃", summary)
                self.assertIn("unreviewed.py", summary)
                race = CandidateManager.get_race(RACE_ID.search(summary).group(1))
                self.assertEqual(race["candidates"]["A"]["changes"], {"step1.txt": "A", "step3.txt": "A"})
                self.assertEqual(race["dag"]["discarded"], {"task-2": ["unreviewed.py"]})
                if dirty:
                    applied, files, message = CandidateManager.apply_candidate(race["race_id"], "A")
                    self.assertTrue(applied, message)
                self.assertFalse((repo / "unreviewed.py").exists())
                self.assertEqual(self.steps(repo), ["step1.txt", "step3.txt"])

    def test_readonly_node_edits_and_deletions_are_reverted_even_when_timestamps_are_preserved(self):
        repo = self.make_repo(files={"app.py": "VALUE = 1\n", "notes.md": "notes\n"})

        def reviewer(prompt, tag, cwd):
            if tag is None:
                # Same size, timestamps restored, and Git told to ignore ctime: a
                # stat-based check (or `git reset --hard`) would miss this edit.
                git(cwd, "config", "core.trustctime", "false")
                step1 = cwd / "step1.txt"
                before = step1.stat()
                with open(step1, "r+") as handle:
                    handle.write("OUTPUT")
                os.utime(step1, ns=(before.st_atime_ns, before.st_mtime_ns))
                (cwd / "notes.md").unlink()
                (cwd / "app.py").chmod(0o600)
                return True, "explanation", None
            return True, f'{tag}: {{"pass": true, "defects": []}}', None

        dag = TaskDAG("goal", [TaskNode("task-1", "contracts", "write step1"),
                               TaskNode("task-2", "解释", "只解释 step1 的设计思路", dependencies=["task-1"])])
        ok, summary, _, _ = self.run_dag(dag, repo, reviewer=reviewer)
        self.assertTrue(ok, summary)
        race = CandidateManager.get_race(RACE_ID.search(summary).group(1))
        self.assertEqual(race["dag"]["discarded"], {"task-2": ["app.py", "notes.md", "step1.txt"]})
        self.assertEqual(race["candidates"]["A"]["changes"], {"step1.txt": "A"})
        self.assertEqual((repo / "step1.txt").read_text(), "output of task-1\n")
        staged = Path(race["candidates"]["A"]["path"])
        self.assertEqual((staged / "notes.md").read_text(), "notes\n")
        self.assertEqual((staged / "app.py").stat().st_mode & 0o777, (repo / "app.py").stat().st_mode & 0o777)

    def test_restore_rewrites_a_file_git_considered_unchanged(self):
        staging = task_dag.DagStaging(str(self.make_repo()))
        self.addCleanup(staging.discard)
        (staging.workspace / "step1.txt").write_text("output of task-1\n")
        staging.checkpoint("task-1")
        head = staging.show_cumulative_changes()
        (staging.workspace / "step1.txt").write_text("CHANGED BY A READ-ONLY NODE\n")
        real = run_git_cmd

        def stat_trusting_git(command, *arguments, **options):
            if "--hard" in command:  # as when size, timestamps and inode still match
                return 0, "", ""
            return real(command, *arguments, **options)

        with patch("makewand.git_helper.run_git_cmd", side_effect=stat_trusting_git):
            self.assertEqual(staging.restore_after_readonly(head), ["step1.txt"])
        self.assertEqual((staging.workspace / "step1.txt").read_text(), "output of task-1\n")
        self.assertTrue(staging.unchanged_since_last_checkpoint())

    def test_review_node_rejecting_the_cumulative_result_leaves_only_an_unapproved_candidate(self):
        repo = self.make_repo()

        def reviewer(prompt, tag, cwd):
            verdict = "false" if "DAG 拓扑子任务 task-2" in prompt else "true"
            defects = '["[P1] step1 is wrong"]' if verdict == "false" else "[]"
            return True, f'{tag}: {{"pass": {verdict}, "defects": {defects}}}', None

        dag = TaskDAG("goal", [TaskNode("task-1", "contracts", "write step1"),
                               TaskNode("task-2", "审查", "审查 task-1 的改动，只分析不要修改代码", dependencies=["task-1"])])
        ok, summary, results, _ = self.run_dag(dag, repo, reviewer=reviewer)
        self.assertFalse(ok)
        self.assertNotEqual(last_result().exit_code, EXIT_PASSED)
        self.assertEqual([(item["task"], item["status"]) for item in results], [("task-1", "passed"), ("task-2", "failed")])
        self.assertIn("未通过子任务 task-2 的独立审查", summary)
        race = CandidateManager.get_race(RACE_ID.search(summary).group(1))
        self.assertIs(race["candidates"]["A"]["review_passed"], False)
        applied, _, message = CandidateManager.apply_candidate(race["race_id"], "A")
        self.assertFalse(applied, message)
        self.assertEqual(self.steps(repo), [])
        self.assertIn(f"makewand apply {race['race_id']} --candidate A --force", summary)
        applied, _, message = CandidateManager.apply_candidate(race["race_id"], "A", force=True)
        self.assertTrue(applied, message)
        self.assertEqual(self.steps(repo), ["step1.txt"])

    def test_dag_of_readonly_nodes_only_delivers_nothing(self):
        repo = self.make_repo()
        dag = TaskDAG("goal", [TaskNode("task-1", "解释", "只解释 app.py"),
                               TaskNode("task-2", "解释", "只解释 VALUE 的用途", dependencies=["task-1"])])
        ok, summary, _, _ = self.run_dag(dag, repo)
        self.assertTrue(ok, summary)
        self.assertEqual(self.coded, [])
        self.assertIn("没有产生需要交付的代码改动，工作区未被修改", summary)
        self.assertNotIn("dg_", summary)
        self.assertEqual(self.store(), [])

    def test_dag_records_are_listed_and_inspected_as_dag_results(self):
        repo = self.make_repo(dirty={"app.py": "VALUE = 1  # WIP\n"})
        dag = TaskDAG("goal", [TaskNode("task-1", "contracts", "write step1"),
                               TaskNode("task-2", "审查", "审查 task-1 的改动，只分析不要修改代码", dependencies=["task-1"])])
        ok, summary, _, _ = self.run_dag(dag, repo)
        self.assertTrue(ok, summary)
        race_id = RACE_ID.search(summary).group(1)

        def run_cli(function, **arguments):
            with contextlib.redirect_stdout(io.StringIO()) as out:
                function(argparse.Namespace(**arguments))
            return re.sub("\x1b\\[[0-9;]*m", "", out.getvalue())

        listing = run_cli(cli.cmd_candidates)
        self.assertIn("多阶段 DAG 完整累积结果 (2/2 个子任务)", listing)
        self.assertNotIn("推荐胜出", listing)
        self.assertNotIn("选手 B", listing)
        self.assertIn(f"makewand apply {race_id} --candidate A", listing)
        details = run_cli(cli.cmd_inspect, race_id=race_id, candidate=None)
        self.assertIn("结果类型: 多阶段 DAG", details)
        self.assertIn("task-1: 代码子任务", details)
        self.assertIn("task-2: 只读审查", details)
        self.assertNotIn("参赛方案表现对比", details)
        self.assertNotIn("主裁推荐", details)
        self.assertIn("+output of task-1", run_cli(cli.cmd_inspect, race_id=race_id, candidate="A"))

    # -- (b) subdirectories ---------------------------------------------------
    def test_dag_started_in_a_subdirectory_stages_the_whole_repository(self):
        repo = self.make_repo(files={"shared.py": "SHARED = 1\n", "conftest.py": "# root conftest\n", "sub/mod.py": "M = 1\n"})
        seen = {}

        def coder(task, workspace):
            seen[task] = (workspace.name, (workspace.parent / "shared.py").exists(), (workspace.parent / "conftest.py").exists())
            self.write_step(task, workspace)

        ok, summary, _, _ = self.run_dag(self.chain(), repo / "sub", coder=coder)
        self.assertTrue(ok, summary)
        self.assertEqual(set(seen.values()), {("sub", True, True)})
        self.assertEqual(self.steps(repo / "sub"), ["step1.txt", "step2.txt", "step3.txt"])
        self.assertEqual(self.steps(repo), [])
        self.assertIn(f"已一次性应用到工作区 {repo.resolve()} ", summary)

    def test_empty_untracked_subdirectory_is_recreated_in_the_staging_copy(self):
        repo = self.make_repo()
        (repo / "newpkg").mkdir()
        ok, summary, _, _ = self.run_dag(self.chain(), repo / "newpkg")
        self.assertTrue(ok, summary)
        self.assertEqual(self.steps(repo / "newpkg"), ["step1.txt", "step2.txt", "step3.txt"])

    def test_ignored_subdirectory_is_refused_before_any_stage(self):
        repo = self.make_repo(files={"app.py": "VALUE = 1\n", ".gitignore": "build/\n"})
        (repo / "build").mkdir()
        (repo / "build" / "out.txt").write_text("generated\n")
        ok, summary, results, _ = self.run_dag(self.chain(), repo / "build")
        self.assertFalse(ok)
        self.assertIn("被 .gitignore 忽略", summary)
        self.assertEqual((self.coded, results), ([], []))
        self.assertEqual(self.store(), [])

    # -- (c) results apply cannot deliver --------------------------------------
    def test_symlink_from_a_stage_stops_the_dag_without_offering_an_impossible_apply(self):
        repo = self.make_repo()

        def coder(task, workspace):
            if task == "task-1":
                os.symlink("app.py", workspace / "app_link.py")
            self.write_step(task, workspace)

        ok, summary, results, _ = self.run_dag(self.chain(), repo, coder=coder)
        self.assertFalse(ok)
        self.assertNotRegex(summary, r"makewand apply dg_")
        self.assertIn("符号链接", summary)
        self.assertEqual(self.coded, ["task-1"])
        self.assertEqual([(item["task"], item["status"]) for item in results], [("task-1", "passed")])
        race = CandidateManager.get_race(RACE_ID.search(summary).group(1))
        self.assertEqual(race["dag"]["blocked"], {"task": "task-1", "paths": ["app_link.py"]})
        staging = Path(race["candidates"]["A"]["path"])
        self.assertIn(str(staging), summary)
        self.assertTrue((staging / "app_link.py").is_symlink())
        self.assertEqual(sorted(path.name for path in repo.iterdir() if path.name != ".git"), ["app.py"])
        applied, _, message = CandidateManager.apply_candidate(race["race_id"], "A", force=True)
        self.assertFalse(applied, message)

    # -- (d) work in progress -------------------------------------------------
    def wip_dag(self):
        repo = self.make_repo(dirty={"app.py": "VALUE = 1  # user WIP\n"})

        def coder(task, workspace):
            if task == "task-1":
                (workspace / "app.py").write_text((workspace / "app.py").read_text() + "EXTRA = 1\n")
            else:
                self.write_step(task, workspace)

        ok, summary, _, _ = self.run_dag(self.chain(), repo, coder=coder)
        self.assertTrue(ok, summary)
        return repo, summary, RACE_ID.search(summary).group(1)

    def test_pending_result_built_on_work_in_progress_applies_once_it_is_committed(self):
        repo, summary, race_id = self.wip_dag()
        self.assertIn("app.py", summary)
        self.assertIn("请先提交这些修改", summary)
        self.assertIn(f"makewand apply {race_id} --candidate A", summary)
        applied, _, message = CandidateManager.apply_candidate(race_id, "A")
        self.assertFalse(applied)
        self.assertIn("提交 (git commit) 这些修改后即可应用", message)
        git(repo, "commit", "-qam", "user commits the work in progress")
        applied, _, message = CandidateManager.apply_candidate(race_id, "A")
        self.assertTrue(applied, message)
        self.assertEqual((repo / "app.py").read_text(), "VALUE = 1  # user WIP\nEXTRA = 1\n")
        self.assertEqual(self.steps(repo), ["step2.txt", "step3.txt"])

    def test_apply_after_stashing_the_work_in_progress_says_how_to_recover(self):
        repo, _, race_id = self.wip_dag()
        git(repo, "stash", "-q")
        applied, _, message = CandidateManager.apply_candidate(race_id, "A")
        self.assertFalse(applied)
        self.assertIn("git stash pop", message)
        self.assertEqual((repo / "app.py").read_text(), "VALUE = 1\n")
        git(repo, "stash", "pop", "-q")
        git(repo, "commit", "-qam", "wip")
        applied, _, message = CandidateManager.apply_candidate(race_id, "A")
        self.assertTrue(applied, message)

    def test_committed_preimage_is_no_conflict_but_a_different_commit_is(self):
        repo = self.make_repo(dirty={"app.py": "VALUE = 1  # WIP\n"})
        candidate = self.root / "candidate"
        clone_isolated_worktree(str(repo), candidate)
        base = git(candidate, "rev-parse", "HEAD").strip()
        (candidate / "app.py").write_text("VALUE = 1  # WIP\nEXTRA = 1\n")
        preimage, head = build_manifest(repo), git(repo, "rev-parse", "HEAD").strip()
        git(repo, "commit", "-qam", "commit the work in progress")
        detect = lambda: CandidateManager.detect_conflicts(str(repo), candidate, baseline_manifest=preimage,  # noqa: E731
                                                           baseline_commit=head, candidate_baseline_commit=base)
        self.assertEqual(detect(), [])
        (repo / "app.py").write_text("VALUE = 2\n")
        git(repo, "commit", "-qam", "a different change")
        self.assertEqual(detect(), ["app.py"])

    # -- (e) concurrent pruning -------------------------------------------------
    def test_concurrent_pruning_keeps_the_running_staging_copy(self):
        repo = self.make_repo()
        state = {}

        def coder(task, workspace):
            if task == "task-2":
                for index in range(CandidateManager.DEFAULT_MAX_CANDIDATES):
                    other = self.candidates / f"rc_other{index:02d}"
                    other.mkdir()
                    (other / "meta.json").write_text(json.dumps(
                        {"race_id": other.name, "created_at": (datetime.now() + timedelta(minutes=index + 1)).isoformat()}))
                CandidateManager.prune_old_candidates()
                state["kept"] = workspace.exists()
            self.write_step(task, workspace)

        ok, summary, _, _ = self.run_dag(self.chain(), repo, coder=coder)
        self.assertTrue(ok, summary)
        self.assertTrue(state["kept"])
        self.assertEqual(self.steps(repo), ["step1.txt", "step2.txt", "step3.txt"])
        # Sealed: an ordinary candidate again, without the in-progress marker.
        race_id = RACE_ID.search(summary).group(1)
        self.assertFalse((self.candidates / race_id / ".in-progress.lock").exists())

    def test_in_progress_directories_survive_prune_and_discard_all_until_released(self):
        from makewand.candidate import CandidateClaim
        running = self.candidates / "dg_running00000"
        running.mkdir(parents=True)
        claim = CandidateClaim(running)
        try:
            for index in range(CandidateManager.DEFAULT_MAX_CANDIDATES):
                other = self.candidates / f"rc_newer{index:02d}"
                other.mkdir()
                (other / "meta.json").write_text(json.dumps({"race_id": other.name, "created_at": datetime.now().isoformat()}))
            self.assertEqual(CandidateManager.prune_old_candidates(), 0)
            ok, message = CandidateManager.discard_race(all_races=True)
            self.assertTrue(ok)
            self.assertIn("dg_running00000", message)
            self.assertEqual(self.store(), [("dg_running00000", False)])
        finally:
            claim.release()
        self.assertEqual(list(running.iterdir()), [])
        CandidateManager.discard_race(all_races=True)
        self.assertEqual(self.store(), [])

    def test_failure_report_never_claims_a_vanished_staging_copy_is_kept(self):
        repo = self.make_repo()

        def coder(task, workspace):
            if task == "task-2":
                shutil.rmtree(Path(str(workspace)).parent)
                return
            self.write_step(task, workspace)

        ok, summary, _, _ = self.run_dag(self.chain(), repo, coder=coder)
        self.assertFalse(ok)
        self.assertIn("已不存在", summary)
        self.assertNotIn("保留", summary)

    # -- (f) interrupts ---------------------------------------------------------
    def test_interrupt_after_a_passed_stage_seals_and_reports_the_verified_part(self):
        repo = self.make_repo()

        def coder(task, workspace):
            if task == "task-2":
                raise KeyboardInterrupt("Ctrl-C during task-2")
            self.write_step(task, workspace)

        out = io.StringIO()
        with self.fixtures(coder=coder), contextlib.redirect_stdout(out), contextlib.redirect_stderr(io.StringIO()), \
                self.assertRaises(KeyboardInterrupt):
            execute_task_dag(self.chain(), cwd=str(repo), tier="deep", auto_fix=False)
        self.assertEqual(last_result().status, "CANCELLED")
        self.assertIn("DAG 被中断", out.getvalue())
        race_id = re.search(r"makewand apply (dg_[0-9a-f]+) --candidate A", out.getvalue()).group(1)
        self.assertEqual(self.store(), [(race_id, True)])
        applied, _, message = CandidateManager.apply_candidate(race_id, "A")
        self.assertTrue(applied, message)
        self.assertEqual(self.steps(repo), ["step1.txt"])

    def test_interrupt_during_a_readonly_node_restores_and_keeps_its_writes_out(self):
        repo = self.make_repo()

        def reviewer(prompt, tag, cwd):
            if tag is None:
                (cwd / "stray.py").write_text("STRAY = 1\n")
                raise KeyboardInterrupt("Ctrl-C during the explanation")
            return True, f'{tag}: {{"pass": true, "defects": []}}', None

        dag = self.chain(("解释", "只解释 step2 的设计思路"))
        with self.fixtures(reviewer=reviewer), contextlib.redirect_stdout(io.StringIO()) as out, \
                contextlib.redirect_stderr(io.StringIO()), self.assertRaises(KeyboardInterrupt):
            execute_task_dag(dag, cwd=str(repo), tier="deep", auto_fix=False)
        race = CandidateManager.get_race(RACE_ID.search(out.getvalue()).group(1))
        self.assertEqual(race["candidates"]["A"]["changes"], {"step1.txt": "A", "step2.txt": "A"})
        self.assertFalse((Path(race["candidates"]["A"]["path"]) / "stray.py").exists())

    def test_interrupt_before_any_stage_passes_leaves_nothing_behind(self):
        repo = self.make_repo()

        def coder(task, workspace):
            raise KeyboardInterrupt("Ctrl-C during task-1")

        with self.fixtures(coder=coder), contextlib.redirect_stdout(io.StringIO()), \
                contextlib.redirect_stderr(io.StringIO()), self.assertRaises(KeyboardInterrupt):
            execute_task_dag(self.chain(), cwd=str(repo), tier="deep", auto_fix=False)
        self.assertEqual(self.store(), [])

    # -- environment the private copy lacks -------------------------------------
    def test_stage_failure_names_ignored_dependencies_missing_from_the_staging_copy(self):
        repo = self.make_repo(files={"app.py": "VALUE = 1\n", ".gitignore": "node_modules/\n.env\n"})
        (repo / "node_modules" / "left-pad").mkdir(parents=True)
        (repo / "node_modules" / "left-pad" / "index.js").write_text("module.exports = 1\n")
        (repo / ".env").write_text("TOKEN=fixture\n")
        ok, summary, _, log = self.run_dag(self.chain(), repo, tests=(False, "Error: Cannot find module 'left-pad'"))
        self.assertFalse(ok)
        self.assertIn("私有暂存区不包含", summary)
        self.assertIn("node_modules/", summary)
        self.assertIn(".env", summary)
        self.assertIn("私有暂存区不包含", log.split("拓扑阶段 1/")[0])

        def reject(prompt, tag, cwd):  # a review rejection has nothing to do with dependencies
            return True, f'{tag}: {{"pass": false, "defects": ["[P1] wrong"]}}', None

        ok, summary, _, _ = self.run_dag(self.chain(), repo, reviewer=reject)
        self.assertFalse(ok)
        self.assertNotIn("私有暂存区不包含", summary)

    def test_multi_stage_dag_refuses_native_windows_with_a_clear_message(self):
        repo = self.make_repo()
        with patch.object(task_dag, "_native_windows", return_value=True), \
                patch("makewand.orchestrator.run_pipeline") as pipeline, contextlib.redirect_stdout(io.StringIO()):
            ok, summary, results = execute_task_dag(self.chain(), cwd=str(repo))
        self.assertFalse(ok)
        self.assertIn("WSL2", summary)
        self.assertIn("未执行任何子任务", summary)
        pipeline.assert_not_called()
        self.assertEqual((results, self.store()), ([], []))


LEGACY_WTA_REPORT = ('MAKEWAND_RACE_VERDICT: {"pass": true, "winner": "A", "defects": []}\n'
                     "[Winner-Take-All] 选手 A (CLAUDE) 率先完成并通过所有质量验证门禁，选手 B 已被快速熔断取消。直接裁定选手 A 胜出。")
REVIEWED_WTA_REPORT = ("[Winner-Take-All] 选手 A (CLAUDE) 率先通过本地测试门禁，竞争选手已被取消。独立只读复审 (CODEX) 结论: passed\n"
                       'MAKEWAND_VERDICT_0a1b2c3d: {"pass": true, "defects": []}')
JUDGE_REPORT = 'B is cleaner.\nMAKEWAND_RACE_VERDICT_0a1b2c3d: {"pass": true, "winner": "B", "defects": []}'


class CandidateLabelTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="makewand-labels-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        patcher = patch.object(config, "CANDIDATES_DIR", self.root / "candidates")
        patcher.start()
        self.addCleanup(patcher.stop)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        (self.repo / "app.py").write_text("VALUE = 1\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            git(self.repo, *arguments)

    def record(self, race_id, judge_report, winner):
        agents = {}
        for label in ("A", "B"):
            workspace = config.CANDIDATES_DIR / race_id / f"agent_{label.lower()}"
            clone_isolated_worktree(str(self.repo), workspace)
            base = git(workspace, "rev-parse", "HEAD").strip()
            (workspace / "app.py").write_text(f"VALUE = 2  # {label}\n")
            agents[label] = {"model": f"MODEL-{label}", "path": str(workspace), "success": True,
                             "test_passed": True, "review_passed": winner == label, "baseline_commit": base,
                             "diff": f"+VALUE = 2  # {label}"}
        CandidateManager.save_race(race_id=race_id, prompt=f"task of {race_id}", base_cwd=str(self.repo),
                                   baseline_commit=git(self.repo, "rev-parse", "HEAD").strip(),
                                   agent_a=agents["A"], agent_b=agents["B"], judge_report=judge_report, winner=winner)

    def run_cli(self, function, **arguments):
        with contextlib.redirect_stdout(io.StringIO()) as out:
            function(argparse.Namespace(**arguments))
        return re.sub("\x1b\\[[0-9;]*m", "", out.getvalue())

    def test_race_listings_label_records_by_what_actually_happened(self):
        self.record("rc_judge0000", JUDGE_REPORT, "B")
        self.record("rc_reviewed00", REVIEWED_WTA_REPORT, "A")
        self.record("rc_legacy000", LEGACY_WTA_REPORT, "A")
        listing = self.run_cli(cli.cmd_candidates)
        blocks = {block.split()[0]: block for block in listing.split("• ID: ")[1:]}
        self.assertNotIn("推荐胜出", listing)
        self.assertIn("主裁推荐: 选手 B", blocks["rc_judge0000"])
        self.assertIn("Winner-Take-All 胜出: 选手 A", blocks["rc_reviewed00"])
        self.assertNotIn("主裁推荐", blocks["rc_reviewed00"])
        self.assertIn("旧版 Winner-Take-All", blocks["rc_legacy000"])
        self.assertNotIn("主裁推荐", blocks["rc_legacy000"])
        self.assertIn("--force", blocks["rc_legacy000"])

        legacy = self.run_cli(cli.cmd_inspect, race_id="rc_legacy000", candidate=None)
        self.assertNotIn("主裁推荐", legacy)
        self.assertIn("旧版 Winner-Take-All 记录: 选手 A 未经任何独立复审", legacy)
        self.assertIn("主裁推荐: 选手 B", self.run_cli(cli.cmd_inspect, race_id="rc_judge0000", candidate=None))
        self.assertIn("Winner-Take-All 胜出: 选手 A",
                      self.run_cli(cli.cmd_inspect, race_id="rc_reviewed00", candidate=None))


class ProbeHelpTests(unittest.TestCase):
    def test_status_probe_help_names_the_real_agy_call(self):
        out = io.StringIO()
        with patch.object(sys, "argv", ["makewand", "status", "--help"]), contextlib.redirect_stdout(out), \
                self.assertRaises(SystemExit) as raised:
            cli.main()
        self.assertEqual(raised.exception.code, 0)
        text = " ".join(out.getvalue().split())
        self.assertIn("real model calls for claude/codex/muse/grok", text)
        self.assertIn("agy makes one real call", text)


if __name__ == "__main__":
    unittest.main()
