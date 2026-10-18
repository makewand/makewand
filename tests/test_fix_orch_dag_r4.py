"""Multi-stage DAG delivery: the cases round 3 still missed (py-orch-dag r4).

Every stage runs through the real pipeline (mocked provider CLIs, real host
transaction, review gate and candidate seal/apply). makewand apply writes regular
files only and never through a symlink, so a workspace entry it cannot replace is
never answered with an apply (or a --force) that cannot succeed: the result is
kept for manual recovery and the reason is named. The private staging copy
ignores what the user's repository ignores (.git/info/exclude, core.excludesFile).
Starting a DAG never hashes an untracked dependency tree no copy receives. Under
the test harness, git_helper's guard refuses makewand's own source tree before any
staging copy or task.
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
import shutil
import tempfile
import unittest
from unittest.mock import patch

from makewand import candidate as candidate_module, cli, config, git_helper, orchestrator as orch, task_dag
from makewand.candidate import CandidateManager
from makewand.git_helper import clone_isolated_worktree, run_git_cmd
from makewand.task_dag import TaskDAG, TaskNode, execute_task_dag
from makewand.workflow import last_result

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")
TASK_ID = re.compile(r"DAG 拓扑子任务 (task-\d+)")
RACE_ID = re.compile(r"\b(dg_[0-9a-f]{12})\b")
APPLY = re.compile(r"makewand apply dg_")


def git(repo, *arguments):
    code, out, error = run_git_cmd(["git", *arguments], cwd=str(repo))
    assert code == 0, error
    return out


class DagFixture(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="makewand-dag-r4-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.candidates = self.root / "candidates"
        self.coded = []
        for patcher in (patch.object(config, "CANDIDATES_DIR", self.candidates),
                        patch.object(orch, "CANDIDATES_DIR", self.candidates)):
            patcher.start()
            self.addCleanup(patcher.stop)

    def make_repo(self, name="repo", files=None):
        repo = self.root / name
        repo.mkdir(parents=True)
        for relative, text in (files or {"app.py": "VALUE = 1\n"}).items():
            (repo / relative).parent.mkdir(parents=True, exist_ok=True)
            (repo / relative).write_text(text)
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            git(repo, *arguments)
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
    def fixtures(self, coder=None, reviewer=None):
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
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, "fixture tests passed")))
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

    @staticmethod
    def steps(directory):
        return sorted(path.name for path in directory.glob("step*.txt"))

    def assert_kept_for_manual_recovery(self, summary, race):
        workspace = Path(race["candidates"]["A"]["path"])
        self.assertTrue(workspace.is_dir())
        self.assertIn(f"git -C {workspace} diff", summary)
        self.assertRegex(summary, r"makewand inspect dg_[0-9a-f]{12} --candidate A")
        self.assertNotRegex(summary, APPLY)


class ApplyObstacleTests(DagFixture):
    """Item force-directory: no apply (or --force) is offered that cannot succeed."""

    def test_directory_where_the_dag_adds_a_file_is_never_offered_an_apply(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", ".gitignore": "logs/\n"})
        (repo / "logs").mkdir()
        (repo / "logs" / "a.log").write_text("the user's log\n")
        # A file the copy lacks too, which alone would get the --force that works.
        (repo / "dist").mkdir()
        (repo / "dist" / "bundle.js").write_text("the user's build\n")

        def coder(task, workspace):
            if task == "task-1":
                (workspace / "logs").write_text("a file named logs\n")
                (workspace / "dist").mkdir(exist_ok=True)
                (workspace / "dist" / "bundle.js").write_text("built inside the DAG\n")
            self.write_step(task, workspace)

        ok, summary, _, _ = self.run_dag(self.code_chain(), repo, coder=coder)
        self.assertFalse(ok, summary)
        self.assertEqual(last_result().status, "APPLY_CONFLICT")
        self.assertIn("logs (目录)", summary)
        self.assertIn("--force 也不例外", summary)
        race = self.race_of(summary)
        self.assert_kept_for_manual_recovery(summary, race)
        self.assertEqual((Path(race["candidates"]["A"]["path"]) / "logs").read_text(), "a file named logs\n")
        self.assertEqual((repo / "logs" / "a.log").read_text(), "the user's log\n")
        self.assertEqual((repo / "dist" / "bundle.js").read_text(), "the user's build\n")
        self.assertEqual(self.steps(repo), [])
        for force in (False, True):
            applied, conflicts, message = CandidateManager.apply_candidate(race["race_id"], "A", force=force)
            self.assertFalse(applied, message)
            self.assertEqual(conflicts, ["logs"])
            self.assertIn("logs (目录)", message)
            self.assertIn("--force 也不例外", message)
            self.assertNotIn("使用 --force", message)
        self.assertTrue((repo / "logs").is_dir())

    def test_symlink_that_replaced_a_target_during_the_dag_is_never_offered_force(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", "other.py": "O = 1\n"})

        def coder(task, workspace):
            if task == "task-1":
                (workspace / "app.py").write_text("A = 2\n")
            if task == "task-2":
                (repo / "app.py").unlink()
                os.symlink("other.py", repo / "app.py")
            self.write_step(task, workspace)

        ok, summary, _, _ = self.run_dag(self.code_chain(), repo, coder=coder)
        self.assertFalse(ok, summary)
        self.assertEqual(last_result().status, "APPLY_CONFLICT")
        self.assertIn("app.py (符号链接)", summary)
        self.assert_kept_for_manual_recovery(summary, self.race_of(summary))
        self.assertTrue((repo / "app.py").is_symlink())
        self.assertEqual((repo / "other.py").read_text(), "O = 1\n")

    def test_file_where_a_written_file_needs_a_directory_is_never_offered_an_apply(self):
        repo = self.make_repo()
        # An untracked script named build: isolated copies never receive it.
        (repo / "build").write_text("#!/bin/sh\nmake\n")

        def coder(task, workspace):
            if task == "task-1":
                (workspace / "build").mkdir()
                (workspace / "build" / "out.py").write_text("OUT = 1\n")
            self.write_step(task, workspace)

        ok, summary, _, _ = self.run_dag(self.code_chain(), repo, coder=coder)
        self.assertFalse(ok, summary)
        self.assertEqual(last_result().status, "APPLY_CONFLICT")
        self.assertIn("build/out.py (上级路径 build 不是目录)", summary)
        self.assert_kept_for_manual_recovery(summary, self.race_of(summary))
        self.assertEqual((repo / "build").read_text(), "#!/bin/sh\nmake\n")

    def test_rejected_result_with_an_obstacle_is_offered_no_force(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", ".gitignore": "logs/\n"})
        (repo / "logs").mkdir()

        def coder(task, workspace):
            if task == "task-1":
                (workspace / "logs").write_text("a file named logs\n")
            self.write_step(task, workspace)

        def reviewer(task, tag, cwd):
            verdict = "false" if task == "task-2" else "true"
            defects = '["[P1] step1 is wrong"]' if verdict == "false" else "[]"
            return True, f'{tag}: {{"pass": {verdict}, "defects": {defects}}}', None

        dag = self.chain(("step 1", "write step1"), ("审查", "审查 task-1 的改动，只分析不要修改代码"))
        ok, summary, _, _ = self.run_dag(dag, repo, coder=coder, reviewer=reviewer)
        self.assertFalse(ok)
        self.assertIn("未通过子任务 task-2 的独立审查", summary)
        self.assertIn("logs (目录)", summary)
        self.assert_kept_for_manual_recovery(summary, self.race_of(summary))

    def test_file_replaced_by_a_directory_across_stages_stops_as_unwritable(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", "gen": "a generated file\n"})

        def coder(task, workspace):
            if task == "task-1":
                (workspace / "gen").unlink()
            if task == "task-2":
                (workspace / "gen").mkdir()
                (workspace / "gen" / "out.py").write_text("OUT = 1\n")
            self.write_step(task, workspace)

        ok, summary, results, _ = self.run_dag(self.code_chain(3), repo, coder=coder)
        self.assertFalse(ok)
        self.assertEqual(last_result().status, "UNVERIFIED")
        self.assertEqual(self.coded, ["task-1", "task-2"])
        race = self.race_of(summary)
        self.assertEqual(race["dag"]["blocked"], {"task": "task-2", "paths": ["gen"], "reason": "unwritable"})
        self.assert_kept_for_manual_recovery(summary, race)
        applied, _, message = CandidateManager.apply_candidate(race["race_id"], "A", force=True)
        self.assertFalse(applied, message)
        self.assertEqual((repo / "gen").read_text(), "a generated file\n")

    def test_dag_listing_and_inspect_offer_no_apply_while_an_obstacle_remains(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", ".gitignore": "logs/\n"})
        (repo / "logs").mkdir()

        def coder(task, workspace):
            if task == "task-1":
                (workspace / "logs").write_text("a file named logs\n")
            self.write_step(task, workspace)

        ok, summary, _, _ = self.run_dag(self.code_chain(), repo, coder=coder)
        race_id = RACE_ID.search(summary).group(1)

        def run_cli(function, **arguments):
            with contextlib.redirect_stdout(io.StringIO()) as out:
                function(argparse.Namespace(**arguments))
            return re.sub("\x1b\\[[0-9;]*m", "", out.getvalue())

        listing = run_cli(cli.cmd_candidates)
        self.assertNotIn(f"makewand apply {race_id}", listing)
        self.assertIn("logs (目录) 使 apply 无法交付", listing)
        self.assertIn("当前无法 apply", run_cli(cli.cmd_inspect, race_id=race_id, candidate=None))
        shutil.rmtree(repo / "logs")  # the user moved the directory away
        self.assertIn(f"makewand apply {race_id} --candidate A", run_cli(cli.cmd_candidates))
        applied, _, message = CandidateManager.apply_candidate(race_id, "A")
        self.assertTrue(applied, message)
        self.assertEqual((repo / "logs").read_text(), "a file named logs\n")


class ApplicationObstacleTests(DagFixture):
    """candidate.application_obstacles names exactly what makes apply --force fail."""

    def seal(self, race_id, repo, mutate):
        workspace = self.candidates / race_id / "agent_a"
        clone_isolated_worktree(str(repo), workspace)
        base = git(workspace, "rev-parse", "HEAD").strip()
        mutate(workspace)
        CandidateManager.save_race(race_id=race_id, prompt="obstacle", base_cwd=str(repo),
                                   baseline_commit=git(repo, "rev-parse", "HEAD").strip(),
                                   agent_a={"model": "M", "path": str(workspace), "success": True,
                                            "test_passed": True, "review_passed": True,
                                            "baseline_commit": base, "diff": "+change"},
                                   agent_b={}, winner="A")
        return CandidateManager.get_race(race_id)["candidates"]["A"]["changes"]

    def test_obstacles_are_what_apply_refuses_also_with_force(self):
        def write(relative, text="candidate\n"):
            def mutate(workspace):
                (workspace / relative).parent.mkdir(parents=True, exist_ok=True)
                (workspace / relative).write_text(text)
            return mutate

        def delete(relative):
            return lambda workspace: (workspace / relative).unlink()

        def to_directory(repo):
            (repo / "logs").mkdir()
            (repo / "logs" / "a.log").write_text("user log\n")

        def to_symlink(repo):
            (repo / "app.py").unlink()
            os.symlink("other.py", repo / "app.py")

        def to_linked_directory(repo):
            (repo / "real").rename(repo / "moved")
            os.symlink("moved", repo / "real")

        def to_file(relative):
            def workspace_change(repo):
                if (repo / relative).is_dir():
                    shutil.rmtree(repo / relative)
                (repo / relative).write_text("a file\n")
            return workspace_change

        cases = [
            ("rc_obsdir001", "directory", write("logs"), to_directory, {"logs": "目录"}),
            ("rc_obslnk001", "symlink target", write("app.py", "A = 2\n"), to_symlink, {"app.py": "符号链接"}),
            ("rc_obsfif001", "fifo", write("pipe"), lambda repo: os.mkfifo(repo / "pipe"), {"pipe": "特殊文件"}),
            ("rc_obsanc001", "symlinked directory", write("real/x.py", "X = 2\n"), to_linked_directory,
             {"real/x.py": "上级路径 real 是符号链接"}),
            ("rc_obsfil001", "file above a write", write("build/out.py"), to_file("build"),
             {"build/out.py": "上级路径 build 不是目录"}),
            ("rc_obsdel001", "file above a deletion", delete("gone/x.py"), to_file("gone"), {}),
            ("rc_obsreg001", "regular file", write("app.py", "A = 2\n"), lambda repo: None, {}),
            ("rc_obsnew001", "new file", write("new/deep/file.py"), lambda repo: None, {}),
        ]
        if hasattr(os, "geteuid") and os.geteuid() != 0:  # root ignores permission bits
            cases += [
                ("rc_obsro0001", "read-only directory", write("ro/new.py"), lambda repo: os.chmod(repo / "ro", 0o555),
                 {"ro/new.py": "所在目录 ro 不可写"}),
                ("rc_obsunr001", "unreadable file", write("secret.txt", "s2\n"),
                 lambda repo: os.chmod(repo / "secret.txt", 0o000), {"secret.txt": "不可读的文件"}),
            ]
        for race_id, name, candidate_change, workspace_change, expected in cases:
            with self.subTest(name):
                repo = self.make_repo(race_id, files={"app.py": "A = 1\n", "other.py": "O = 1\n", "real/x.py": "X = 1\n",
                                                      "gone/x.py": "G = 1\n", "ro/x.py": "R = 1\n",
                                                      "secret.txt": "s1\n"})
                changes = self.seal(race_id, repo, candidate_change)
                workspace_change(repo)
                try:
                    self.assertEqual(candidate_module.application_obstacles(repo, changes), expected)
                    applied, _, message = CandidateManager.apply_candidate(race_id, "A", force=True)
                    self.assertEqual(applied, not expected, message)
                finally:
                    for path in (repo / "ro", repo / "secret.txt"):
                        if path.exists() and not path.is_symlink():
                            os.chmod(path, 0o755 if path.is_dir() else 0o644)

    def test_race_listing_offers_no_apply_for_a_candidate_with_an_obstacle(self):
        repo = self.make_repo("listing")
        self.seal("rc_listing01", repo, lambda workspace: (workspace / "logs").write_text("candidate\n"))
        race = CandidateManager.get_race("rc_listing01")
        kind, _ = candidate_module.describe_race_outcome(race)
        self.assertIn("makewand apply rc_listing01", cli._outcome_operations(race, kind))
        (repo / "logs").mkdir()  # the user created a directory where the candidate adds a file
        operations = cli._outcome_operations(race, kind)
        self.assertNotIn("makewand apply rc_listing01", operations)
        self.assertIn("logs (目录) 使 apply 选手 A 无法交付", operations)

    def test_existing_targets_of_additions_leaves_out_entries_apply_cannot_replace(self):
        repo = self.make_repo()
        (repo / "logs").mkdir()
        (repo / "dist").mkdir()
        (repo / "dist" / "bundle.js").write_text("the user's build\n")
        os.symlink("app.py", repo / "cfg")
        changes = {"logs": "A", "dist/bundle.js": "A", "cfg": "A", "new.py": "A", "app.py": "M"}
        self.assertEqual(candidate_module.existing_targets_of_additions(repo, list(changes), changes),
                         ["dist/bundle.js"])


class IgnoreRuleTests(DagFixture):
    """Item info-exclude: the staging copy ignores what the user's repository ignores."""

    def ignored(self, repo, paths):
        result = {}
        for path in paths:
            code, _, _ = run_git_cmd(["git", "check-ignore", "-q", "--no-index", "--", path], cwd=str(repo))
            result[path] = code == 0
        return result

    def test_staging_copy_ignores_what_the_repository_ignores(self):
        repo = self.make_repo(files={"app.py": "A = 1\n", ".gitignore": "*.tmp\n"})
        (repo / ".git" / "info").mkdir(exist_ok=True)
        (repo / ".git" / "info" / "exclude").write_text(".venv/\n*.log\n!keep.log\n")
        (self.root / "repo-excludes").write_text("local.cfg\n")
        git(repo, "config", "core.excludesFile", str(self.root / "repo-excludes"))
        staging = task_dag.DagStaging(str(repo))
        self.addCleanup(staging.discard)
        paths = [".venv/bin/python", "debug.log", "keep.log", "local.cfg", "x.tmp", "app.py", "src/new.py"]
        expected = self.ignored(repo, paths)
        self.assertEqual(expected, {".venv/bin/python": True, "debug.log": True, "keep.log": False,
                                    "local.cfg": True, "x.tmp": True, "app.py": False, "src/new.py": False})
        self.assertEqual(self.ignored(staging.workspace, paths), expected)

    def test_default_and_disabled_excludes_file_decide_alike_in_the_copy(self):
        xdg = self.root / "xdg"
        (xdg / "git").mkdir(parents=True)
        (xdg / "git" / "ignore").write_text("from-default.txt\n")
        with patch.dict(os.environ, {"XDG_CONFIG_HOME": str(xdg)}):
            for disabled in (False, True):
                with self.subTest(disabled=disabled):
                    repo = self.make_repo(f"default-{disabled}")
                    if disabled:  # an empty core.excludesFile turns the default one off
                        git(repo, "config", "core.excludesFile", "")
                    staging = task_dag.DagStaging(str(repo))
                    self.addCleanup(staging.discard)
                    expected = {"from-default.txt": not disabled}
                    self.assertEqual(self.ignored(repo, ["from-default.txt"]), expected)
                    self.assertEqual(self.ignored(staging.workspace, ["from-default.txt"]), expected)

    def test_paths_ignored_only_by_info_exclude_or_the_repository_excludes_file_are_never_delivered(self):
        def venv(task, workspace):
            if task == "task-1":
                (workspace / ".venv" / "bin").mkdir(parents=True)
                (workspace / ".venv" / "bin" / "python").write_text("#!/bin/sh\n")
                (workspace / ".venv" / "pyvenv.cfg").write_text("home = /usr\n")
            self.write_step(task, workspace)

        for variant in ("info/exclude", "core.excludesFile"):
            with self.subTest(variant):
                repo = self.make_repo(variant.replace("/", "-").replace(".", "-"))
                if variant == "info/exclude":
                    (repo / ".git" / "info").mkdir(exist_ok=True)
                    (repo / ".git" / "info" / "exclude").write_text(".venv/\n")
                else:
                    (self.root / "excludes").write_text(".venv/\n")
                    git(repo, "config", "core.excludesFile", str(self.root / "excludes"))
                ok, summary, _, _ = self.run_dag(self.code_chain(), repo, coder=venv)
                self.assertFalse((repo / ".venv").exists(), summary)
                match = RACE_ID.search(summary)
                if match:
                    changes = CandidateManager.get_race(match.group(1))["candidates"]["A"].get("changes") or {}
                    self.assertFalse([path for path in changes if path.startswith(".venv/")], changes)
                self.assertEqual((repo / "app.py").read_text(), "VALUE = 1\n")


class HostManifestTests(DagFixture):
    """Item manifest-cost: an untracked dependency tree no copy receives is never hashed for a DAG."""

    def dependency_repo(self, count=30):
        repo = self.make_repo(files={"app.py": "A = 1\n", "vendor/lib.php": "V = 1\n"})
        (repo / "node_modules" / "kept").mkdir(parents=True)
        (repo / "node_modules" / "kept" / "index.js").write_text("vendored on purpose\n")
        git(repo, "add", "-f", "node_modules/kept/index.js")
        git(repo, "commit", "-qm", "vendored dependency")
        (repo / "node_modules" / "pkg").mkdir()
        for index in range(count):
            (repo / "node_modules" / "pkg" / f"f{index}.js").write_text("x" * 64)
        for relative in ("dist/bundle.js", "vendor/local.php", "vendor/pkg/node_modules/x.js", ".venv/pyvenv.cfg"):
            (repo / relative).parent.mkdir(parents=True, exist_ok=True)
            (repo / relative).write_text("untracked, not ignored\n")
        return repo

    def test_host_manifest_leaves_out_untracked_dependency_trees_but_binds_project_files(self):
        repo = self.dependency_repo()
        host = candidate_module.build_manifest(repo, host=True)
        full = candidate_module.build_manifest(repo)
        for relative in ("app.py", "vendor/lib.php", "node_modules/kept/index.js", "vendor/local.php"):
            self.assertIn(relative, host)
            self.assertIn(relative, full)
        for relative in ("node_modules/pkg/f0.js", "dist/bundle.js", "vendor/pkg/node_modules/x.js", ".venv/pyvenv.cfg"):
            self.assertNotIn(relative, host)
            self.assertIn(relative, full)  # a candidate copy's manifest still binds what a stage created there

    def test_host_manifest_leaves_out_only_what_isolated_copies_never_receive(self):
        repo = self.dependency_repo(count=3)
        copy = self.root / "copy"
        clone_isolated_worktree(str(repo), copy)
        received = set(candidate_module.build_manifest(copy))
        host = set(candidate_module.build_manifest(repo, host=True))
        left_out = set(candidate_module.build_manifest(repo)) - host
        self.assertTrue(left_out)
        self.assertEqual(left_out & received, set())
        self.assertLessEqual(received, host)

    def test_dag_start_hashes_no_untracked_dependency_tree(self):
        repo = self.dependency_repo(count=40).resolve()  # DagStaging hashes below the resolved root
        hashed = []
        real = candidate_module._manifest_digest  # what build_manifest reads every file with

        def spy(path, budget):
            hashed.append(str(path))
            return real(path, budget)

        with patch.object(candidate_module, "_manifest_digest", side_effect=spy):
            staging = task_dag.DagStaging(str(repo))
        staging.discard()
        self.assertIn(str(repo / "node_modules" / "kept" / "index.js"), hashed)
        self.assertEqual([path for path in hashed if path.startswith(str(repo / "node_modules" / "pkg"))], [])


class HarnessGuardTests(DagFixture):
    """Item dag-harness-guard: git_helper's guard refuses the source tree before any staging copy or task."""

    def setUp(self):
        super().setUp()
        self.assertTrue(os.environ.get("MAKEWAND_TEST_ISOLATION_ROOT"))
        # Stand in for the checkout under test; never the real one. git_helper's source
        # root is the one seam: the DAG's early refusal, apply and a host transaction all
        # ask git_helper's rule, which reads only it (single-seam).
        self.source = self.make_repo("checkout", files={"app.py": "A = 1\n", "pkg/mod.py": "M = 1\n"})
        patcher = patch.object(git_helper, "_makewand_source_root", return_value=self.source.resolve())
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_single_task_dag_refuses_the_source_tree_before_running_it(self):
        single = TaskDAG("goal", [TaskNode("task-1", "only", "write a file")])
        for target in (self.source, self.source / "pkg"):
            with self.subTest(target=target.name):
                with patch("makewand.orchestrator.run_pipeline") as pipeline, contextlib.redirect_stdout(io.StringIO()):
                    ok, summary, results = task_dag.execute_task_dag(single, cwd=str(target))
                self.assertFalse(ok)
                self.assertIn("测试环境中拒绝", summary)
                self.assertIn("未执行任何子任务", summary)
                pipeline.assert_not_called()
                self.assertEqual(results, [])
                self.assertEqual(last_result().status, "UNVERIFIED")
        self.assertEqual(git(self.source, "status", "--porcelain"), "")

    def test_multi_task_dag_asks_the_shared_guard_before_any_staging_copy(self):
        repo = self.make_repo()
        asked = []

        def shared(path, action="执行会修改文件的任务"):
            asked.append(path)
            return "测试环境中拒绝 (fixture)"

        with patch.object(git_helper, "harness_source_tree_conflict", side_effect=shared), \
                patch.object(task_dag, "DagStaging", side_effect=AssertionError("staging copy of a refused target")), \
                patch("makewand.orchestrator.run_pipeline") as pipeline, contextlib.redirect_stdout(io.StringIO()):
            ok, summary, results = task_dag.execute_task_dag(self.code_chain(), cwd=str(repo))
        self.assertFalse(ok)
        self.assertEqual(asked, [str(repo)])
        self.assertIn("测试环境中拒绝 (fixture)", summary)
        pipeline.assert_not_called()
        self.assertEqual((results, self.store()), ([], []))
        self.assertEqual(last_result().status, "UNVERIFIED")

    def test_apply_guard_is_git_helpers_definition(self):
        asked = []

        def shared(path, action="执行会修改文件的任务"):
            asked.append((str(path), action))
            return "refused by the shared definition"

        with patch.object(git_helper, "harness_source_tree_conflict", side_effect=shared):
            self.assertEqual(candidate_module.harness_source_tree_conflict(str(self.source)),
                             "refused by the shared definition")
        self.assertEqual(asked, [(str(self.source), "把候选应用到工作区")])
        # The rule itself, with the one seam standing in: apply refuses the stand-in.
        self.assertIn(str(self.source.resolve()), candidate_module.harness_source_tree_conflict(str(self.source)))


if __name__ == "__main__":
    unittest.main()
