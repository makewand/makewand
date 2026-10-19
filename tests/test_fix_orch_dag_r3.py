"""Multi-stage DAG delivery: the cases round 2 still missed (py-orch-dag r3).

Every stage runs through the real pipeline (mocked provider CLIs, real host
transaction, review gate and candidate seal/apply). Files Git tracks or would add
below dependency/build directory names (dist/, build/, vendor/, .vscode/, ...) are
sealed and delivered like any other file; a workspace file the private copy never
had is not overwritten unchecked; a read-only node leaves nothing behind, also in
ignored paths and nested repositories; a second Ctrl-C while sealing still reports
what is kept; a deleted-then-recreated file gets the guidance that works; and the
test harness never applies a candidate to makewand's own source tree.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import re
import shutil
import stat
import tempfile
import unittest
from unittest.mock import patch

from makewand import candidate as candidate_module, config, git_helper, orchestrator as orch, task_dag
from makewand.candidate import CandidateManager, build_manifest
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


class DagFixture(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="makewand-dag-r3-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.candidates = self.root / "candidates"
        self.coded = []
        for patcher in (patch.object(config, "CANDIDATES_DIR", self.candidates),
                        patch.object(orch, "CANDIDATES_DIR", self.candidates)):
            patcher.start()
            self.addCleanup(patcher.stop)

    def make_repo(self, name="repo", files=None, dirty=None, git_repo=True):
        repo = self.root / name
        repo.mkdir(parents=True)
        for relative, text in (files or {"app.py": "VALUE = 1\n"}).items():
            (repo / relative).parent.mkdir(parents=True, exist_ok=True)
            (repo / relative).write_text(text)
        if git_repo:
            for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                              ["config", "user.email", "fixture@example.invalid"], ["add", "."],
                              ["commit", "-qm", "base"]):
                git(repo, *arguments)
        for relative, text in (dirty or {}).items():
            (repo / relative).parent.mkdir(parents=True, exist_ok=True)
            (repo / relative).write_text(text)
        return repo

    @staticmethod
    def write_step(task, workspace):
        (workspace / f"step{task[-1]}.txt").write_text(f"output of {task}\n")

    @staticmethod
    def chain(*titles):
        tasks, previous = [], None
        for index, (title, description) in enumerate(titles, start=1):
            task_id = f"task-{index}"
            tasks.append(TaskNode(task_id, title, description, dependencies=[previous] if previous else []))
            previous = task_id
        return TaskDAG("build feature", tasks)

    def code_chain(self, count=2):
        return self.chain(*[(f"step {index}", f"write step{index}") for index in range(1, count + 1)])

    @contextlib.contextmanager
    def fixtures(self, coder=None, reviewer=None, tests=(True, "fixture tests passed")):
        engines = ("claude", "codex")
        coder = coder or self.write_step

        def provider(engine):
            def execute(prompt, **kwargs):
                match = TASK_ID.search(prompt)
                task = match.group(1) if match else None
                if kwargs.get("readonly"):
                    tag = VERDICT_TAG.search(prompt)
                    if reviewer is not None:
                        return reviewer(task, tag.group(0) if tag else None, Path(kwargs["cwd"]))
                    if tag is None:
                        return True, "explanation of the design", None
                    return True, f'{tag.group(0)}: {{"pass": true, "defects": []}}', None
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

    def race_of(self, summary):
        return CandidateManager.get_race(RACE_ID.search(summary).group(1))

    def store(self):
        if not self.candidates.exists():
            return []
        return sorted((entry.name, (entry / "meta.json").exists()) for entry in self.candidates.iterdir())


class DependencyDirectoryNameTests(DagFixture):
    """Item ignore-dirs: project files below PROJECT_IGNORE_DIRS names are part of the result."""

    def test_manifest_binds_files_git_tracks_or_would_add_below_dependency_dir_names(self):
        repo = self.make_repo(files={
            "app.py": "A = 1\n", ".gitignore": "node_modules/\n*.map\n", "vendor/lib.py": "V = 1\n",
            ".vscode/settings.json": "{}\n", "target/t.txt": "tracked, later behind a symlink\n"})
        (repo / "dist").mkdir()
        (repo / "dist" / "bundle.js").write_text("untracked, not ignored\n")
        (repo / "dist" / "bundle.js.map").write_text("ignored\n")
        (repo / "node_modules" / "dep").mkdir(parents=True)
        (repo / "node_modules" / "dep" / "index.js").write_text("ignored dependency\n")
        (repo / ".makewand_sandbox_home").mkdir()
        (repo / ".makewand_sandbox_home" / "state").write_text("sandbox scratch\n")
        # Ignored only by .git/info/exclude, or by an excludes file like a user's global one.
        (repo / ".git" / "info").mkdir(exist_ok=True)
        (repo / ".git" / "info" / "exclude").write_text(".venv/\n")
        (repo / ".venv" / "lib").mkdir(parents=True)
        (repo / ".venv" / "lib" / "site.py").write_text("ignored virtualenv\n")
        (self.root / "global-excludes").write_text("env/\n")
        git(repo, "config", "core.excludesFile", str(self.root / "global-excludes"))
        (repo / "env").mkdir()
        (repo / "env" / "activate").write_text("ignored virtualenv\n")
        # A tracked path whose parent became a symlink: Git still lists it from the index.
        elsewhere = self.root / "elsewhere"
        elsewhere.mkdir()
        (elsewhere / "t.txt").write_text("outside the workspace\n")
        shutil.rmtree(repo / "target")
        os.symlink(elsewhere, repo / "target")

        manifest = build_manifest(repo)
        for relative in ("app.py", "vendor/lib.py", ".vscode/settings.json", "dist/bundle.js"):
            self.assertIn(relative, manifest)
        for relative in ("dist/bundle.js.map", "node_modules/dep/index.js", ".makewand_sandbox_home/state",
                         "target/t.txt", "target", ".venv/lib/site.py", "env/activate"):
            self.assertNotIn(relative, manifest)
        # Records sealed before project files were bound are verified the way they were sealed.
        legacy = build_manifest(repo, project_files=False)
        self.assertNotIn("vendor/lib.py", legacy)
        self.assertNotIn("dist/bundle.js", legacy)
        self.assertEqual(legacy, {path: record for path, record in manifest.items()
                                  if path.split("/")[0] not in ("vendor", ".vscode", "dist")})

    def test_stage_changes_below_dependency_dir_names_are_applied_in_place(self):
        repo = self.make_repo(files={
            "app.py": "A = 1\n", ".vscode/settings.json": "{}\n", "vendor/lib.py": "V = 1\n",
            "build/gen.py": "G = 1\n", "dist/index.js": "bundle v1\n", "dist/old.js": "old\n"})

        def coder(task, workspace):
            if task == "task-1":
                (workspace / ".vscode" / "settings.json").write_text('{"x": 1}\n')
                (workspace / "vendor" / "lib.py").write_text("V = 2\n")
                (workspace / "build" / "gen.py").write_text("G = 2\n")
                (workspace / "dist" / "index.js").write_text("bundle v2\n")
                (workspace / "dist" / "old.js").unlink()
                (workspace / "dist" / "new.js").write_text("new chunk\n")
            self.write_step(task, workspace)

        ok, summary, _, _ = self.run_dag(self.code_chain(), repo, coder=coder)
        self.assertTrue(ok, summary)
        self.assertEqual(last_result().status, "PASSED")
        self.assertIn("已一次性应用到工作区", summary)
        self.assertNotIn("makewand apply", summary)
        self.assertEqual((repo / ".vscode" / "settings.json").read_text(), '{"x": 1}\n')
        self.assertEqual((repo / "vendor" / "lib.py").read_text(), "V = 2\n")
        self.assertEqual((repo / "build" / "gen.py").read_text(), "G = 2\n")
        self.assertEqual((repo / "dist" / "index.js").read_text(), "bundle v2\n")
        self.assertEqual((repo / "dist" / "new.js").read_text(), "new chunk\n")
        self.assertFalse((repo / "dist" / "old.js").exists())
        changes = self.race_of(summary)["candidates"]["A"]["changes"]
        self.assertEqual(changes["dist/old.js"], "D")
        self.assertEqual(changes["dist/new.js"], "A")

    def test_pending_result_on_a_dirty_workspace_offers_the_apply_that_works(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", ".vscode/settings.json": "{}\n"},
                              dirty={"app.py": "A = 1  # user WIP\n"})

        def coder(task, workspace):
            if task == "task-1":
                (workspace / ".vscode" / "settings.json").write_text('{"x": 1}\n')
            self.write_step(task, workspace)

        ok, summary, _, _ = self.run_dag(self.code_chain(), repo, coder=coder)
        self.assertTrue(ok, summary)
        race_id = RACE_ID.search(summary).group(1)
        self.assertIn(f"应用: makewand apply {race_id} --candidate A", summary)
        self.assertNotIn("--force", summary)
        self.assertNotIn("DAG 运行期间", summary)
        applied, _, message = CandidateManager.apply_candidate(race_id, "A")
        self.assertTrue(applied, message)
        self.assertEqual((repo / ".vscode" / "settings.json").read_text(), '{"x": 1}\n')
        self.assertEqual((repo / "app.py").read_text(), "A = 1  # user WIP\n")

    def test_workspace_file_the_staging_copy_lacks_is_never_overwritten_silently(self):
        repo = self.make_repo()
        # Untracked, not ignored build output: the private copy never receives it.
        (repo / "dist").mkdir()
        (repo / "dist" / "bundle.js").write_text("the user's own build\n")

        def coder(task, workspace):
            if task == "task-1":
                (workspace / "dist").mkdir(exist_ok=True)
                (workspace / "dist" / "bundle.js").write_text("built inside the DAG\n")
            self.write_step(task, workspace)

        ok, summary, _, _ = self.run_dag(self.code_chain(), repo, coder=coder)
        self.assertTrue(ok, summary)
        race_id = RACE_ID.search(summary).group(1)
        self.assertIn("dist/bundle.js", summary)
        self.assertIn("未复制进私有暂存区", summary)
        self.assertIn(f"makewand apply {race_id} --candidate A --force", summary)
        self.assertNotIn(f"应用: makewand apply {race_id} --candidate A", summary)
        applied, conflicts, message = CandidateManager.apply_candidate(race_id, "A")
        self.assertFalse(applied, message)
        self.assertIn("dist/bundle.js", conflicts)
        self.assertIn("由候选新建", message)
        self.assertEqual((repo / "dist" / "bundle.js").read_text(), "the user's own build\n")
        applied, _, message = CandidateManager.apply_candidate(race_id, "A", force=True)
        self.assertTrue(applied, message)
        self.assertEqual((repo / "dist" / "bundle.js").read_text(), "built inside the DAG\n")

    def test_candidate_adding_a_file_the_workspace_already_has_conflicts(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", ".gitignore": "*.log\n"})
        (repo / "run.log").write_text("the user's ignored notes\n")
        candidate = self.root / "candidate"
        clone_isolated_worktree(str(repo), candidate)
        base = git(candidate, "rev-parse", "HEAD").strip()
        (candidate / ".gitignore").write_text("")
        (candidate / "run.log").write_text("generated by the candidate\n")
        preimage = build_manifest(repo)  # records the user's ignored run.log
        self.assertIn("run.log", preimage)
        conflicts = CandidateManager.detect_conflicts(str(repo), candidate, baseline_manifest=preimage,
                                                      candidate_baseline_commit=base)
        self.assertEqual(conflicts, ["run.log"])

    def test_record_sealed_before_project_files_were_bound_still_applies(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", ".vscode/settings.json": "{}\n"})
        workspace = self.candidates / "rc_legacy0001" / "agent_a"
        clone_isolated_worktree(str(repo), workspace)
        base = git(workspace, "rev-parse", "HEAD").strip()
        (workspace / "app.py").write_text("A = 2\n")
        CandidateManager.save_race(race_id="rc_legacy0001", prompt="legacy", base_cwd=str(repo),
                                   baseline_commit=git(repo, "rev-parse", "HEAD").strip(),
                                   agent_a={"model": "M", "path": str(workspace), "success": True,
                                            "test_passed": True, "review_passed": True,
                                            "baseline_commit": base, "diff": "+A = 2"},
                                   agent_b={}, winner="A")
        meta_file = self.candidates / "rc_legacy0001" / "meta.json"
        meta = json.loads(meta_file.read_text())
        meta.pop("manifest_scope")
        meta["candidates"]["A"]["manifest"] = build_manifest(workspace, project_files=False)
        meta_file.write_text(json.dumps(meta))
        applied, _, message = CandidateManager.apply_candidate("rc_legacy0001", "A")
        self.assertTrue(applied, message)
        self.assertEqual((repo / "app.py").read_text(), "A = 2\n")

    def test_non_git_workspace_change_below_a_dependency_dir_name_is_applied(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", "vendor/lib.php": "<?php // v1\n"}, git_repo=False)

        def coder(task, workspace):
            if task == "task-1":
                (workspace / "vendor" / "lib.php").write_text("<?php // v2\n")
            self.write_step(task, workspace)

        ok, summary, _, _ = self.run_dag(self.code_chain(), repo, coder=coder)
        self.assertTrue(ok, summary)
        self.assertIn("已一次性应用到工作区", summary)
        self.assertEqual((repo / "vendor" / "lib.php").read_text(), "<?php // v2\n")

    def test_result_apply_can_never_write_stops_without_offering_apply(self):
        repo = self.make_repo()

        def coder(task, workspace):
            if task == "task-1":
                (workspace / "tool.sh").write_text("#!/bin/sh\n")
                os.chmod(workspace / "tool.sh", 0o4755)
            self.write_step(task, workspace)

        ok, summary, results, _ = self.run_dag(self.code_chain(3), repo, coder=coder)
        self.assertFalse(ok)
        self.assertNotRegex(summary, r"makewand apply dg_")
        self.assertIn("无法写入", summary)
        self.assertIn("tool.sh", summary)
        self.assertEqual(self.coded, ["task-1"])
        self.assertEqual([(item["task"], item["status"]) for item in results], [("task-1", "passed")])
        race = self.race_of(summary)
        self.assertEqual(race["dag"]["blocked"], {"task": "task-1", "paths": ["tool.sh"], "reason": "unwritable"})
        applied, _, message = CandidateManager.apply_candidate(race["race_id"], "A", force=True)
        self.assertFalse(applied, message)
        self.assertFalse((repo / "tool.sh").exists())


class ReadOnlyNodeTests(DagFixture):
    """Items readonly-ignored and nested-repo-restore: nothing a read-only node writes survives it."""

    def explain_dag(self):
        return self.chain(("impl", "write step1"), ("解释", "只解释 step1 的设计思路"), ("impl", "write step3"))

    def test_writes_to_ignored_paths_are_found_discarded_and_reported(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", ".gitignore": "*.log\nbuild/\n"})
        seen = {}

        def coder(task, workspace):
            if task == "task-3":
                seen["evil"] = (workspace / "build" / "evil.py").exists()
                seen["build"] = (workspace / "build").exists()
                seen["log"] = (workspace / "debug.log").exists()
            self.write_step(task, workspace)

        def reviewer(task, tag, cwd):
            if tag is None and task == "task-2":
                (cwd / "debug.log").write_text("log of a read-only node\n")
                (cwd / "build").mkdir()
                (cwd / "build" / "evil.py").write_text("EVIL = 1\n")
                return True, "explanation", None
            return True, f'{tag}: {{"pass": true, "defects": []}}', None

        ok, summary, _, _ = self.run_dag(self.explain_dag(), repo, coder=coder, reviewer=reviewer)
        self.assertTrue(ok, summary)
        self.assertEqual(seen, {"evil": False, "build": False, "log": False})
        race = self.race_of(summary)
        self.assertEqual(race["dag"]["discarded"], {"task-2": ["build/evil.py", "debug.log"]})
        self.assertIn("build/evil.py", summary)
        self.assertEqual(race["candidates"]["A"]["changes"], {"step1.txt": "A", "step3.txt": "A"})
        self.assertFalse((Path(race["candidates"]["A"]["path"]) / "build").exists())
        self.assertFalse((repo / "build").exists())

    def test_restore_discards_ignored_files_the_stage_wrote_changed_or_deleted(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", ".gitignore": "*.log\nbuild/\nout/\n"})
        staging = task_dag.DagStaging(str(repo))
        self.addCleanup(staging.discard)
        workspace = staging.workspace
        (workspace / "step1.txt").write_text("output of task-1\n")
        (workspace / "out").mkdir()  # ignored tool output kept by an earlier stage
        (workspace / "out" / "cache.txt").write_text("cache v1\n")
        (workspace / "out" / "keep.txt").write_text("keep\n")
        (workspace / "build").mkdir()
        (workspace / "build" / "report.txt").write_text("report\n")
        (workspace / "pkg").mkdir()  # an empty directory, e.g. a new subdirectory the DAG runs in
        staging.checkpoint("task-1")
        head = staging.show_cumulative_changes()
        (workspace / "debug.log").write_text("written by a read-only node\n")
        (workspace / "build" / "evil.py").write_text("EVIL = 1\n")
        (workspace / "out" / "cache.txt").write_text("tampered\n")
        (workspace / "out" / "keep.txt").unlink()
        (workspace / "pkg").rmdir()
        self.assertEqual(staging.restore_after_readonly(head),
                         ["build/evil.py", "debug.log", "out/cache.txt", "out/keep.txt", "pkg/"])
        # Untouched ignored files stay, a removed directory comes back; changed ignored
        # files cannot be restored and are removed.
        self.assertEqual(sorted(path.relative_to(workspace).as_posix() for path in workspace.rglob("*")
                                if ".git" not in path.parts),
                         [".gitignore", "app.py", "build", "build/report.txt", "out", "pkg", "step1.txt"])
        self.assertTrue(staging.unchanged_since_last_checkpoint())
        # Unchanged ignored files are left alone: a second clean read-only pass reports nothing.
        (workspace / "out" / "cache.txt").write_text("cache v2\n")
        staging.checkpoint("task-3")
        head = staging.show_cumulative_changes()
        self.assertEqual(staging.restore_after_readonly(head), [])
        self.assertEqual((workspace / "out" / "cache.txt").read_text(), "cache v2\n")

    def test_empty_directory_the_dag_runs_in_survives_a_readonly_node_that_wrote_something(self):
        repo = self.make_repo()
        (repo / "newpkg").mkdir()  # untracked and empty: the staging copy recreates it

        def reviewer(task, tag, cwd):
            if tag is None and task == "task-1":
                (cwd / "stray.txt").write_text("written by a read-only node\n")
                return True, "explanation", None
            return True, f'{tag}: {{"pass": true, "defects": []}}', None

        dag = self.chain(("解释", "只解释 app.py 的设计思路"), ("impl", "write step2"))
        ok, summary, results, _ = self.run_dag(dag, repo / "newpkg", reviewer=reviewer)
        self.assertTrue(ok, summary)
        self.assertEqual([item["status"] for item in results], ["passed", "passed"])
        self.assertEqual(self.race_of(summary)["dag"]["discarded"], {"task-1": ["newpkg/stray.txt"]})
        self.assertEqual(sorted(path.name for path in (repo / "newpkg").iterdir()), ["step2.txt"])

    def test_readonly_node_that_writes_nothing_reports_nothing(self):
        repo = self.make_repo()
        ok, summary, _, _ = self.run_dag(self.explain_dag(), repo)
        self.assertTrue(ok, summary)
        self.assertNotIn("丢弃", summary)
        self.assertEqual(self.race_of(summary)["dag"]["discarded"], {})

    def test_nested_repository_and_read_only_directory_are_discarded(self):
        repo = self.make_repo()
        seen = {}

        def coder(task, workspace):
            if task == "task-3":
                seen["nested"] = (workspace / "nested").exists()
                seen["locked"] = (workspace / "locked").exists()
            self.write_step(task, workspace)

        def reviewer(task, tag, cwd):
            if tag is None and task == "task-2":
                (cwd / "nested").mkdir()
                (cwd / "nested" / "x.py").write_text("X = 1\n")
                git(cwd / "nested", "init", "-q")
                (cwd / "locked").mkdir()
                (cwd / "locked" / "f.txt").write_text("f\n")
                os.chmod(cwd / "locked", stat.S_IRUSR | stat.S_IXUSR)
                return True, "explanation", None
            return True, f'{tag}: {{"pass": true, "defects": []}}', None

        ok, summary, results, _ = self.run_dag(self.explain_dag(), repo, coder=coder, reviewer=reviewer)
        self.assertTrue(ok, summary)
        self.assertEqual([item["status"] for item in results], ["passed", "passed", "passed"])
        self.assertEqual(seen, {"nested": False, "locked": False})
        race = self.race_of(summary)
        self.assertEqual(race["dag"]["discarded"], {"task-2": ["locked/f.txt", "nested/"]})
        self.assertEqual(sorted(path.name for path in repo.iterdir() if path.name != ".git"),
                         ["app.py", "step1.txt", "step3.txt"])


class InterruptTests(DagFixture):
    """Item double-interrupt: a second Ctrl-C while the interrupt handler seals."""

    def interrupted_coder(self, task, workspace):
        if task == "task-2":
            raise KeyboardInterrupt("first Ctrl-C")
        self.write_step(task, workspace)

    def run_interrupted(self, repo, **patches):
        out = io.StringIO()
        with self.fixtures(coder=self.interrupted_coder), contextlib.ExitStack() as stack, \
                contextlib.redirect_stdout(out), contextlib.redirect_stderr(io.StringIO()):
            for target, value in patches.items():
                stack.enter_context(patch.object(CandidateManager, target, side_effect=value))
            with self.assertRaises(KeyboardInterrupt) as raised:
                execute_task_dag(self.code_chain(), cwd=str(repo), tier="deep", auto_fix=False)
        return str(raised.exception), re.sub("\x1b\\[[0-9;]*m", "", out.getvalue())

    def test_second_interrupt_during_the_seal_still_reports_the_kept_copy(self):
        def second(*arguments, **options):
            raise KeyboardInterrupt("second Ctrl-C during seal")

        error, log = self.run_interrupted(self.make_repo(), save_race=second)
        self.assertEqual(error, "first Ctrl-C")
        self.assertIn("封存过程中再次被中断", log)
        [(name, has_meta)] = self.store()
        self.assertFalse(has_meta)
        self.assertIn(str(self.candidates / name / "agent_a"), log)
        self.assertEqual(last_result().status, "CANCELLED")
        self.assertIn(str(self.candidates / name / "agent_a"), last_result()[2])

    def test_second_interrupt_after_the_record_was_written_reports_the_sealed_candidate(self):
        def second(*arguments, **options):
            raise KeyboardInterrupt("second Ctrl-C while pruning")

        error, log = self.run_interrupted(self.make_repo(), _prune_old_candidates_locked=second)
        self.assertEqual(error, "first Ctrl-C")
        [(name, has_meta)] = self.store()
        self.assertTrue(has_meta)
        self.assertIn("封存过程中再次被中断", log)
        self.assertIn(f"makewand inspect {name}", log)
        self.assertEqual(CandidateManager.get_race(name)["candidates"]["A"]["changes"], {"step1.txt": "A"})


class WorkInProgressDeletionTests(DagFixture):
    """Item wip-deleted-message: a file the user deleted (uncommitted) and a stage re-created."""

    def test_pending_summary_names_the_uncommitted_deletion_and_the_apply_that_works(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", "gone.py": "G = 1\n"})
        (repo / "gone.py").unlink()

        def coder(task, workspace):
            if task == "task-1":
                (workspace / "gone.py").write_text("G = 2  # recreated by the DAG\n")
            self.write_step(task, workspace)

        ok, summary, _, _ = self.run_dag(self.code_chain(), repo, coder=coder)
        self.assertTrue(ok, summary)
        race_id = RACE_ID.search(summary).group(1)
        self.assertIn("gone.py", summary)
        self.assertIn("已被删除", summary)
        self.assertIn("git rm", summary)
        self.assertIn(f"再执行: makewand apply {race_id} --candidate A", summary)
        self.assertNotIn("DAG 运行期间", summary)
        self.assertNotIn("--force", summary)
        applied, _, message = CandidateManager.apply_candidate(race_id, "A")
        self.assertFalse(applied)
        self.assertIn("提交这次删除", message)
        self.assertFalse((repo / "gone.py").exists())
        git(repo, "rm", "-q", "gone.py")
        git(repo, "commit", "-qm", "the user commits the deletion")
        applied, _, message = CandidateManager.apply_candidate(race_id, "A")
        self.assertTrue(applied, message)
        self.assertEqual((repo / "gone.py").read_text(), "G = 2  # recreated by the DAG\n")


class SourceTreeGuardTests(DagFixture):
    """Item apply-guard: under the test harness nothing is applied to makewand's own source tree."""

    def setUp(self):
        super().setUp()
        self.assertTrue(os.environ.get("MAKEWAND_TEST_ISOLATION_ROOT"))
        self.outer = self.make_repo("outer", files={"README": "outer\n", "makewand-src/app.py": "A = 1\n"})
        self.source = self.outer / "makewand-src"
        # Stand in for the checkout under test; never the real one. apply, the DAG's early
        # refusal and a host transaction all judge git_helper's source root, the one seam.
        patcher = patch.object(git_helper, "_makewand_source_root", return_value=self.source.resolve())
        patcher.start()
        self.addCleanup(patcher.stop)

    def seal(self, race_id, base_cwd):
        workspace = self.candidates / race_id / "agent_a"
        clone_isolated_worktree(str(base_cwd), workspace)
        base = git(workspace, "rev-parse", "HEAD").strip()
        (workspace / "applied.txt").write_text("candidate file\n")
        CandidateManager.save_race(race_id=race_id, prompt="guard", base_cwd=str(base_cwd),
                                   baseline_commit=git(base_cwd, "rev-parse", "HEAD").strip(),
                                   agent_a={"model": "M", "path": str(workspace), "success": True,
                                            "test_passed": True, "review_passed": True,
                                            "baseline_commit": base, "diff": "+candidate file"},
                                   agent_b={}, winner="A")

    def test_apply_refuses_the_source_tree_a_directory_inside_it_and_its_parents(self):
        (self.source / "sub").mkdir()
        (self.source / "sub" / "mod.py").write_text("M = 1\n")
        git(self.outer, "add", ".")
        git(self.outer, "commit", "-qm", "sub")
        for race_id, target in (("rc_source0001", self.source), ("rc_inside0001", self.source / "sub"),
                                ("rc_parent0001", self.outer)):
            with self.subTest(target=target.name):
                self.seal(race_id, target)
                applied, _, message = CandidateManager.apply_candidate(race_id, "A")
                self.assertFalse(applied)
                self.assertIn("测试环境中拒绝", message)
                self.assertFalse((target / "applied.txt").exists())
                applied, preview, message = CandidateManager.apply_candidate(race_id, "A", dry_run=True)
                self.assertTrue(applied, message)
        ok, _, message = CandidateManager.recover_interrupted_applications(str(self.source))
        self.assertFalse(ok)
        self.assertIn("测试环境中拒绝", message)

    def test_apply_to_any_other_workspace_still_works(self):
        sibling = self.make_repo("sibling")
        self.seal("rc_sibling001", sibling)
        applied, _, message = CandidateManager.apply_candidate("rc_sibling001", "A")
        self.assertTrue(applied, message)
        self.assertTrue((sibling / "applied.txt").exists())
        with patch.dict(os.environ):
            os.environ.pop("MAKEWAND_TEST_ISOLATION_ROOT")
            self.assertIsNone(candidate_module.harness_source_tree_conflict(str(self.source)))

    def test_dag_result_the_harness_refuses_to_apply_is_offered_for_inspection_only(self):
        repo = self.make_repo("elsewhere")
        refusal = "测试环境中拒绝把候选应用到 makewand 源码目录 (fixture)"

        # The DAG's early check (git_helper's guard) passes for this repository; apply is refused.
        with patch.object(candidate_module, "harness_source_tree_conflict", return_value=refusal, create=True):
            ok, summary, _, _ = self.run_dag(self.code_chain(), repo)
        self.assertFalse(ok)
        self.assertIn(refusal, summary)
        self.assertNotRegex(summary, r"makewand apply dg_")
        self.assertRegex(summary, r"makewand inspect dg_[0-9a-f]{12} --candidate A")
        self.assertEqual(last_result().status, "UNVERIFIED")
        self.assertEqual(sorted(path.name for path in repo.iterdir() if path.name != ".git"), ["app.py"])

    def test_multi_task_dag_run_directly_refuses_the_source_tree_before_any_stage(self):
        with patch("makewand.orchestrator.run_pipeline") as pipeline, contextlib.redirect_stdout(io.StringIO()):
            ok, summary, results = task_dag.execute_task_dag(self.code_chain(), cwd=str(self.source))
        self.assertFalse(ok)
        self.assertIn("测试环境中拒绝", summary)
        self.assertIn("未执行任何子任务", summary)
        pipeline.assert_not_called()
        self.assertEqual((results, self.store()), ([], []))
        self.assertEqual(last_result().status, "UNVERIFIED")
        self.assertEqual(sorted(path.name for path in self.source.iterdir()), ["app.py"])


if __name__ == "__main__":
    unittest.main()
