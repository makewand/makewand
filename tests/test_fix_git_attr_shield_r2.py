"""Host-side git never runs a repository's filter: the cross-vendor review of PR #93 (round 2).

Every fixture plants a filter driver whose commands append to a marker file outside the
repositories (FilterFixture): a marker line means makewand ran the filter on the host.
Covered: nested repositories and submodules (no host git command starts a command inside
one, while a dirty registered submodule is still detected where it was before), the
delivery patch check and the generated apply script, SHA-256 repositories, global options
that would point git at another repository, and the inherited environment.
"""
try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation
    from _verdicts import nonced
    from test_fix_git_attr_shield import PATCH, FilterFixture
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation
    from tests._verdicts import nonced
    from tests.test_fix_git_attr_shield import PATCH, FilterFixture

import contextlib
import io
import json
import os
import shlex
import shutil
import subprocess
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.config as makewand_config
import makewand.orchestrator as orch
from makewand import candidate, delivery, git_helper
from makewand.git_helper import HostWorkspaceTransaction, ShadowWorktreeResult

SHA1_EMPTY_TREE = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
SHA256_EMPTY_TREE = "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321"
UTF8_BOM = b"\xef\xbb\xbf"
COMPLETED = subprocess.CompletedProcess([], 0, "", "")


class NestedFixture(FilterFixture):
    def arm(self, worktree, git_dir, name="x.txt", dirty=True):
        """Plant the filter in a nested repository: its config defines it, its info/attributes
        selects it for every path. dirty rewrites a tracked file at the same size with a newer
        mtime, so any git status there must hash it through the filter."""
        self.plant_filter(git_dir / "config")
        self.plant_attributes(git_dir)
        if dirty:
            tracked = worktree / name
            tracked.write_text(tracked.read_text().swapcase())
            future = os.stat(tracked).st_mtime + 3600
            os.utime(tracked, (future, future))

    def nested_repository(self, path, files=None):
        path.mkdir(parents=True, exist_ok=True)
        self.git(path, "init", "-q", "-b", "main")
        for name, text in (files or {"x.txt": "x\n"}).items():
            (path / name).write_text(text)
        self.git(path, "add", "-A")
        self.git(path, "commit", "-qm", "nested")
        return path

    def committed_submodule(self, name="top", register=True, ignore=None, dirty=True):
        """Layout A: a repository commits a gitlink to a nested repository with its own .git
        directory (registered in .gitmodules unless register is False) and the filter planted."""
        top = self.root / name
        top.mkdir()
        self.git(top, "init", "-q", "-b", "main")
        (top / "app.py").write_text("print(1)\n")
        sub = self.nested_repository(top / "sub")
        if register:
            (top / ".gitmodules").write_text('[submodule "sub"]\n\tpath = sub\n\turl = ./sub\n'
                                             + (f"\tignore = {ignore}\n" if ignore else ""))
        self.git(top, "add", "-A")
        self.git(top, "commit", "-qm", "init")
        self.arm(sub, sub / ".git", dirty=dirty)
        return top

    def global_config(self, **settings):
        """A user's global configuration ($HOME/.gitconfig) for the duration of the test."""
        home = self.root / "home"
        home.mkdir(exist_ok=True)
        for key, value in settings.items():
            self.git(self.root, "config", "-f", str(home / ".gitconfig"), key.replace("__", "."), value)
        patcher = patch.dict(os.environ, {"HOME": str(home), "XDG_CONFIG_HOME": str(home / "xdg")})
        patcher.start()
        self.addCleanup(patcher.stop)
        return home


class NestedRepositoryTests(NestedFixture):
    """No host git command starts a command inside a nested repository (layouts A, B, C)."""

    def test_review_diff_never_enters_a_committed_submodule(self):
        # Layout A: git add -A refreshing the private index ran `git status` in sub.
        top = self.committed_submodule()
        (top / "app.py").write_text("print(2)\n")
        diff, error = git_helper.get_git_diff_status(str(top))
        self.assertIsNone(error)
        self.assertIn("print(2)", diff)
        self.assertIn("[Submodule: sub]", diff)  # its own change is still reviewed, shielded
        self.assertIn("+X", diff)
        self.assertEqual(self.marker_lines(), [])

    def test_review_diff_never_enters_a_gitlink_of_a_planted_submodule(self):
        # Layout B: the sandbox cannot write top/.git, but plants .gitmodules, a repository
        # sub whose HEAD commits a gitlink, and the nested repository it points to.
        top = self.repository(self.root / "top", {"app.py": "print(1)\n"})
        sub = top / "sub"
        sub.mkdir()
        self.git(sub, "init", "-q", "-b", "main")
        nested = self.nested_repository(sub / "nested")
        (sub / "s.txt").write_text("s\n")
        self.git(sub, "add", "-A")
        self.git(sub, "commit", "-qm", "s")
        (top / ".gitmodules").write_text('[submodule "sub"]\n\tpath = sub\n\turl = ./sub\n')
        self.arm(nested, nested / ".git")
        diff, error = git_helper.get_git_diff_status(str(top))
        self.assertIsNone(error)
        self.assertEqual(self.marker_lines(), [])

    def test_status_never_enters_a_gitlink(self):
        # A .gitmodules "ignore = none" overrides diff.ignoreSubmodules, not --ignore-submodules.
        for index, (register, ignore) in enumerate(((False, None), (True, None), (True, "none"))):
            with self.subTest(register=register, ignore=ignore):
                self.marker.unlink(missing_ok=True)
                top = self.committed_submodule(f"top{index}", register=register, ignore=ignore)
                code, _, error = git_helper.run_git_cmd(["git", "status", "--porcelain"], cwd=str(top))
                self.assertEqual(code, 0, error)
                self.assertEqual(self.marker_lines(), [])

    def test_add_all_stages_what_git_add_all_stages_without_entering_a_gitlink(self):
        top = self.committed_submodule()
        sub = top / "sub"
        gone = self.nested_repository(top / "gone")
        self.git(top, "add", "-A")
        self.git(top, "commit", "-qm", "gone")
        # The submodule moves on (committed without running the planted driver) and stays dirty.
        self.git(sub, "-c", "filter.evil.clean=cat", "-c", "filter.evil.smudge=cat", "commit", "-qam", "moved")
        self.arm(sub, sub / ".git")
        shutil.rmtree(gone)
        (top / "app.py").write_text("print(2)\n")
        (top / "new.txt").write_text("new\n")
        self.marker.unlink(missing_ok=True)
        code, _, error = git_helper.add_all(str(top))
        self.assertEqual(code, 0, error)
        self.assertEqual(self.marker_lines(), [])
        staged = self.git(top, "diff", "--cached", "--name-status", "--no-renames").split("\n")
        self.assertEqual(sorted(line for line in staged if line), ["A\tnew.txt", "D\tgone", "M\tapp.py", "M\tsub"])
        self.assertEqual(self.git(top, "ls-files", "-s", "sub").split()[1], self.git(sub, "rev-parse", "HEAD").strip())

    def test_add_all_intent_to_add_leaves_gitlinks_to_the_diff(self):
        top = self.committed_submodule()
        (top / "new.txt").write_text("new\n")
        code, _, error = git_helper.add_all(str(top), intent_to_add=True)
        self.assertEqual(code, 0, error)
        self.assertEqual(self.marker_lines(), [])
        self.assertIn("new.txt", self.git(top, "diff", "--name-only", "--ignore-submodules=dirty"))

    def test_every_hardened_command_carries_the_submodule_barrier(self):
        repo = self.repository(self.root / "repo", {"f.txt": "one\n"})
        with patch.object(git_helper.subprocess, "run", return_value=COMPLETED) as run:
            git_helper.run_git_cmd(["git", "status", "--porcelain"], cwd=str(repo))
        executed, env = run.call_args.args[0], run.call_args.kwargs["env"]
        settings = {env[f"GIT_CONFIG_KEY_{i}"]: env[f"GIT_CONFIG_VALUE_{i}"] for i in range(int(env["GIT_CONFIG_COUNT"]))}
        self.assertEqual(settings, {"diff.ignoreSubmodules": "dirty", "submodule.recurse": "false",
                                    "fetch.recurseSubmodules": "false", "push.recurseSubmodules": "no"})
        self.assertIn("--ignore-submodules=dirty", executed[executed.index("status"):])

    def test_a_users_submodule_recurse_never_enters_a_submodule(self):
        # submodule.recurse=true in a user's global configuration makes reset --hard and
        # checkout run read-tree inside each submodule, where its own smudge filter runs.
        origin = self.repository(self.root / "origin", {"x.txt": "x\n"})
        top = self.repository(self.root / "top", {"app.py": "print(1)\n"})
        self.git(top, "-c", "protocol.file.allow=always", "submodule", "add", "-q", str(origin), "sub")
        self.git(top, "commit", "-qm", "sub")
        sub, gitdir = top / "sub", top / ".git" / "modules" / "sub"
        (sub / "x.txt").write_text("y\n")
        self.git(sub, "commit", "-qam", "moved")  # the superproject's reset moves it back
        self.arm(sub, gitdir, dirty=False)
        self.global_config(submodule__recurse="true")
        for command in (["git", "reset", "--hard", "-q", "HEAD"], ["git", "checkout", "-q", "--", "."]):
            with self.subTest(command=command):
                code, _, error = git_helper.run_git_cmd(command, cwd=str(top))
                self.assertEqual(code, 0, error)
                self.assertEqual(self.marker_lines(), [])


class SubmoduleDirtinessTests(NestedFixture):
    """A dirty registered submodule is still detected wherever git status used to report it."""

    def test_isolation_check_still_sees_a_dirty_submodule(self):
        top = self.committed_submodule()
        with patch.object(git_helper, "get_active_interactive_working_trees", return_value={}):
            safe, message = git_helper.check_working_tree_isolation(str(top))
        self.assertFalse(safe)
        self.assertIn("Dirty Working Tree", message)
        self.assertEqual(self.marker_lines(), [])

    def test_dirty_paths_still_list_a_dirty_submodule(self):
        top = self.committed_submodule()
        head = self.git(top, "rev-parse", "HEAD").strip()
        self.assertEqual(git_helper.dirty_submodule_paths(str(top)), ["sub"])
        self.assertIn("sub", git_helper.git_status_paths(str(top)))
        self.assertIn("sub", git_helper.get_dirty_files(str(top)))
        self.assertIn("sub", candidate.workspace_dirty_files(str(top)))
        self.assertEqual(candidate.get_candidate_files_changed(top, head).get("sub"), "M")
        self.assertEqual(self.marker_lines(), [])

    def test_an_untracked_file_makes_a_registered_submodule_dirty(self):
        top = self.committed_submodule(dirty=False)
        self.assertEqual(git_helper.dirty_submodule_paths(str(top)), [])
        (top / "sub" / "untracked.txt").write_text("u\n")
        self.assertEqual(git_helper.dirty_submodule_paths(str(top)), ["sub"])

    def test_a_dirty_nested_submodule_marks_its_outermost_gitlink(self):
        nested = self.repository(self.root / "nested-origin", {"n.txt": "n\n"})
        origin = self.repository(self.root / "origin", {"x.txt": "x\n"})
        self.git(origin, "-c", "protocol.file.allow=always", "submodule", "add", "-q", str(nested), "nested")
        self.git(origin, "commit", "-qm", "nested")
        top = self.repository(self.root / "top", {"app.py": "print(1)\n"})
        self.git(top, "-c", "protocol.file.allow=always", "submodule", "add", "-q", str(origin), "sub")
        self.git(top, "commit", "-qm", "sub")
        self.git(top, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init", "--recursive")
        self.assertEqual(git_helper.dirty_submodule_paths(str(top)), [])
        self.arm(top / "sub" / "nested", top / ".git" / "modules" / "sub" / "modules" / "nested", name="n.txt")
        self.assertEqual(git_helper.dirty_submodule_paths(str(top)), ["sub"])
        self.assertIn("sub", git_helper.git_status_paths(str(top)))
        self.assertEqual(self.marker_lines(), [])

    def test_an_unregistered_gitlink_is_neither_entered_nor_reported(self):
        top = self.committed_submodule(register=False)
        self.assertNotIn("sub", git_helper.git_status_paths(str(top)))
        self.assertEqual(git_helper.dirty_submodule_paths(str(top)), [])
        self.assertEqual(self.marker_lines(), [])

    def test_host_transaction_baseline_sees_a_dirty_submodule(self):
        top = self.committed_submodule()
        txn = HostWorkspaceTransaction(str(top))
        self.assertIsNone(txn.capture_pre_snapshot())
        self.assertIsNone(txn.begin())
        self.addCleanup(txn._abort_begin)
        self.assertTrue(txn.initial_dirty)
        self.assertIn("sub", txn.dirty_tracked)
        self.assertEqual(self.marker_lines(), [])

    def test_host_rollback_verification_sees_a_submodule_left_dirty(self):
        top = self.committed_submodule(dirty=False)
        txn = HostWorkspaceTransaction(str(top))
        self.assertIsNone(txn.capture_pre_snapshot())
        self.assertIsNone(txn.begin())
        self.addCleanup(txn._abort_begin)
        self.assertFalse(txn.initial_dirty)
        (top / "sub" / "x.txt").write_text("X\n")
        self.marker.unlink(missing_ok=True)
        problems = []
        txn._verify(problems, [])
        self.assertTrue(any("被跟踪文件未能恢复到基线" in problem and "sub" in problem for problem in problems), problems)
        self.assertEqual(self.marker_lines(), [])

    def test_dag_checkpoint_refuses_a_dirty_registered_submodule(self):
        from makewand.task_dag import DagStaging
        repo = self.repository(self.root / "repo", {"app.py": "print(1)\n"})
        with patch.object(makewand_config, "CANDIDATES_DIR", self.root / "candidates"):
            staging = DagStaging(str(repo))
            self.addCleanup(staging.discard)
            # A stage registers and creates a repository of its own and leaves it dirty.
            nested = self.nested_repository(staging.workspace / "sub")
            (staging.workspace / ".gitmodules").write_text('[submodule "sub"]\n\tpath = sub\n\turl = ./sub\n')
            self.arm(nested, nested / ".git")
            with self.assertRaisesRegex(OSError, "未受控改动"):
                staging.checkpoint("task-1")
        self.assertEqual(self.marker_lines(), [])


class ShadowDeliveryFixture(NestedFixture):
    """run_pipeline delivering a shadow copy (the fixture of test_delivery_binding, trimmed)."""

    def setUp(self):
        super().setUp()
        self.base = self.repository(self.root / "base", {"app.txt": "BASE\n"})
        self.shadow = self.root / "shadow"
        self.sub_baselines = {}
        self.reset_shadow()
        self.artifacts_root = _isolation.artifacts_root()
        self.artifacts_before = set(self.artifacts_root.glob("delivery_*"))
        self.addCleanup(self.clean_artifacts)
        self.output = ""

    def reset_shadow(self):
        shutil.rmtree(self.shadow, ignore_errors=True)
        shutil.copytree(self.base, self.shadow, symlinks=True)
        self.baseline = self.git(self.shadow, "rev-parse", "HEAD").strip()

    def delivery_artifacts(self):
        result = set()
        for path in set(self.artifacts_root.glob("delivery_*")) - self.artifacts_before:
            manifest = path / "delivery_manifest.json"
            if manifest.exists() and json.loads(manifest.read_text()).get("repo_root") == str(self.base):
                result.add(path)
        return result

    def clean_artifacts(self):
        for path in self.delivery_artifacts():
            shutil.rmtree(path, ignore_errors=True)

    def apply_delivery(self):
        outputs = self.delivery_artifacts()
        self.assertEqual(len(outputs), 1)
        script = next(iter(outputs)) / "apply_delivery.sh"
        return subprocess.run(["bash", str(script)], capture_output=True, text=True, timeout=60, cwd="/")

    def run_pipeline(self, coder, intercept=None):
        shadow = ShadowWorktreeResult(
            str(self.shadow), "makewand/attr-shield-r2", lambda: None, baseline_commit=self.baseline,
            repo_head=self.git(self.base, "rev-parse", "HEAD").strip(), repo_root=str(self.base),
            worktree_root=str(self.shadow), sub_baselines=self.sub_baselines)

        def dispatch(engine, prompt, cwd, readonly=False, **kwargs):
            if readonly:
                return True, nonced(prompt, 'MAKEWAND_VERDICT: {"pass": true, "defects": []}'), None
            coder(Path(cwd))
            return True, "implemented", None

        def git_intercept(command, **kwargs):
            if intercept:
                intercept(command)
            return git_helper.run_git_cmd(command, **kwargs)

        output = io.StringIO()
        with contextlib.ExitStack() as stack:
            for name, value in (("check_load_backpressure", True),
                                ("check_working_tree_isolation", (False, "controlled shadow fixture")),
                                ("create_ephemeral_shadow_worktree", shadow),
                                ("get_or_update_status", {}),
                                ("select_optimal_engine_pair", (["codex"], ["claude"], {
                                    "primary_coder": "codex", "primary_reviewer": "claude", "reasons": []})),
                                ("run_local_tests", (True, "fixture tests passed"))):
                stack.enter_context(patch.object(orch, name, return_value=value))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
            stack.enter_context(patch.object(orch, "run_git_cmd", side_effect=git_intercept))
            stack.enter_context(patch("makewand.memory.format_memory_hints_for_prompt", return_value=""))
            stack.enter_context(contextlib.redirect_stdout(output))
            result = orch.run_pipeline("Implement update", cwd=str(self.base), force_code=True, auto_fix=False)
        self.output = output.getvalue()
        return result


class ShadowDeliveryTests(ShadowDeliveryFixture):
    def test_shadow_delivery_never_enters_a_planted_repository(self):
        # Layout C: git add -A stages the planted repository as a gitlink and the
        # following git status entered it, re-hashing a file it keeps racy.
        def coder(shadow):
            (shadow / "app.txt").write_text("APPROVED\n")
            nested = self.nested_repository(shadow / "sub")
            (shadow / ".gitmodules").write_text('[submodule "sub"]\n\tpath = sub\n\turl = ./sub\n')
            self.arm(nested, nested / ".git")

        self.run_pipeline(coder)
        self.assertEqual(self.marker_lines(), [])

    def test_shadow_delivery_still_refuses_a_registered_submodule_left_dirty(self):
        # A registered submodule changed after the per-submodule commits: the clean
        # check after the delivery commit (git status, which no longer enters it) and
        # the submodule's own shielded status still refuse the delivery.
        nested = self.nested_repository(self.base / "sub")
        (self.base / ".gitmodules").write_text('[submodule "sub"]\n\tpath = sub\n\turl = ./sub\n')
        self.git(self.base, "add", "-A")
        self.git(self.base, "commit", "-qm", "submodule")
        self.reset_shadow()
        self.sub_baselines = {"sub": self.git(nested, "rev-parse", "HEAD").strip()}
        armed = []

        def intercept(command):
            if command[1:4] == ["diff", "--cached", "--name-only"] and not armed:
                armed.append(command)
                self.arm(self.shadow / "sub", self.shadow / "sub" / ".git")

        self.assertFalse(self.run_pipeline(lambda shadow: (shadow / "app.txt").write_text("APPROVED\n"), intercept))
        self.assertTrue(armed)
        self.assertIn("交付提交后工作区残留未审查改动", self.output)
        self.assertEqual(self.marker_lines(), [])


class DeliveryApplyTests(ShadowDeliveryFixture):
    """check_delivery_patch and the generated apply_delivery.sh run no filter driver."""

    def armed_destination(self):
        (self.base / ".gitattributes").write_text("*.txt filter=evil\n")
        self.git(self.base, "add", "-A")
        self.git(self.base, "commit", "-qm", "attributes")
        self.reset_shadow()
        self.plant_filter(self.base / ".git" / "config", required=True)
        self.plant_attributes(self.base / ".git")

    def test_delivery_patch_check_runs_no_filter(self):
        self.armed_destination()
        good, bad = self.root / "good.patch", self.root / "bad.patch"
        good.write_text("--- a/app.txt\n+++ b/app.txt\n@@ -1 +1 @@\n-BASE\n+APPROVED\n")
        bad.write_text("--- a/app.txt\n+++ b/app.txt\n@@ -1 +1 @@\n-OTHER\n+APPROVED\n")
        delivery.check_delivery_patch(str(self.base), good)
        with self.assertRaisesRegex(OSError, "does not match the frozen destination baseline"):
            delivery.check_delivery_patch(str(self.base), bad)
        self.assertEqual((self.base / "app.txt").read_text(), "BASE\n")
        self.assertEqual(self.marker_lines(), [])

    def test_generated_script_runs_no_filter(self):
        self.armed_destination()
        self.assertTrue(self.run_pipeline(lambda shadow: (shadow / "app.txt").write_text("APPROVED\n")), self.output)
        self.assertEqual(self.marker_lines(), [])  # makewand's own patch check
        result = self.apply_delivery()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.base / "app.txt").read_text(), "APPROVED\n")
        self.assertEqual(self.marker_lines(), [])  # the script's checks and applies

    def test_delivery_patch_check_refuses_a_configuration_git_cannot_list(self):
        good = self.root / "good.patch"
        good.write_text("--- a/app.txt\n+++ b/app.txt\n@@ -1 +1 @@\n-BASE\n+APPROVED\n")
        with (self.base / ".git" / "config").open("a") as config:
            config.write("[broken\n")
        with self.assertRaisesRegex(OSError, "Cannot list Git filter drivers"):
            delivery.check_delivery_patch(str(self.base), good)


class ObjectFormatTests(NestedFixture):
    """The empty-tree attribute source is the repository's own object id, never a ref name."""

    def sha256_repository(self, name="s256"):
        repo = self.root / name
        repo.mkdir()
        self.git(repo, "init", "-q", "--object-format=sha256", "-b", "main")
        return repo

    def test_sha256_repository_works(self):
        repo = self.sha256_repository()
        (repo / "f.txt").write_text("one\n")
        diff, error = git_helper.get_git_diff_status(str(repo))  # unborn: diffs against its own empty tree
        self.assertIsNone(error)
        self.assertIn("+one", diff)
        self.assertEqual(git_helper.empty_tree_oid(str(repo)), SHA256_EMPTY_TREE)
        self.git(repo, "add", "-A")
        self.git(repo, "commit", "-qm", "c")
        (repo / "f.txt").write_text("one\nchanged\n")
        diff, error = git_helper.get_git_diff_status(str(repo))
        self.assertIsNone(error)
        self.assertIn("+changed", diff)
        code, out, error = git_helper.run_git_cmd(["git", "status", "--porcelain"], cwd=str(repo))
        self.assertEqual((code, out.strip()), (0, "M f.txt"), error)
        self.git(repo, "checkout", "--", "f.txt")
        for options in (("--index",), ()):
            code, _, error = git_helper.run_git_cmd(["git", "apply", *options, "-"], cwd=str(repo), input_data=PATCH.encode())
            self.assertEqual(code, 0, error)
            self.git(repo, "reset", "-q", "--hard")

    def test_a_ref_named_like_the_sha1_empty_tree_selects_no_filter(self):
        # In a SHA-256 repository the 40-hex SHA-1 id is no object id: git resolved it as
        # this branch, whose tree selects the filter for every path.
        repo = self.sha256_repository()
        (repo / "f.txt").write_text("one\n")
        (repo / ".gitattributes").write_text("* filter=evil\n")
        self.git(repo, "add", "-A")
        evil_tree = self.git(repo, "write-tree").strip()
        self.git(repo, "rm", "-q", "--cached", ".gitattributes")
        (repo / ".gitattributes").unlink()
        self.git(repo, "commit", "-qm", "c")
        evil = self.git(repo, "commit-tree", evil_tree, "-m", "evil").strip()
        self.git(repo, "update-ref", f"refs/heads/{SHA1_EMPTY_TREE}", evil)
        self.arm(repo, repo / ".git", name="f.txt", dirty=True)
        (repo / ".git" / "info" / "attributes").unlink()  # only the ref selects it
        diff, error = git_helper.get_git_diff_status(str(repo))
        self.assertIsNone(error)
        self.assertEqual(self.marker_lines(), [])

    def test_sha1_repository_is_unchanged(self):
        repo = self.repository(self.root / "s1", {"f.txt": "one\n"})
        self.assertEqual(git_helper.empty_tree_oid(str(repo)), SHA1_EMPTY_TREE)
        with patch.object(git_helper.subprocess, "run", return_value=COMPLETED) as run:
            git_helper.run_git_cmd(["git", "diff", "HEAD"], cwd=str(repo))
        self.assertIn(f"--attr-source={SHA1_EMPTY_TREE}", run.call_args.args[0])
        # A ref of that name loses to the object id (git warns that it is ambiguous).
        (repo / ".gitattributes").write_text("* filter=evil\n")
        self.git(repo, "add", ".gitattributes")
        evil = self.git(repo, "commit-tree", self.git(repo, "write-tree").strip(), "-m", "evil").strip()
        self.git(repo, "rm", "-q", "--cached", ".gitattributes")
        (repo / ".gitattributes").unlink()
        self.git(repo, "update-ref", f"refs/heads/{SHA1_EMPTY_TREE}", evil)
        self.plant_filter(repo / ".git" / "config")
        (repo / "f.txt").write_text("ONE\n")
        diff, error = git_helper.get_git_diff_status(str(repo))
        self.assertIsNone(error)
        self.assertIn("+ONE", diff)
        self.assertEqual(self.marker_lines(), [])

    def test_object_format_is_read_as_git_reads_it(self):
        # Each config, prefixed by repositoryformatversion 1, against git's own reading of it.
        cases = [
            b"[extensions]\n\tobjectFormat = sha256\n",
            b"[Extensions]\nOBJECTFORMAT=sha256\n",
            b'[extensions]\n\tobjectformat = "sha256"\n',
            b"[extensions]\n\tobjectformat = sha256 ; a comment\n",
            b"[extensions]\n\tobjectformat = sha256# a comment\n",
            b"[extensions]\n\tobjectformat = sha2\\\n56\n",
            b"[extensions]\r\n\tobjectformat = sha256\r\n",
            UTF8_BOM + b"[extensions]\n\tobjectformat = sha256\n",
            b"[extensions]\n" + UTF8_BOM + b"objectformat = sha256\n",
            b"[extensions]\n\tobjectformat = sha256\n\tobjectformat = sha1\n",
            b"[extensions]\n\tobjectformat = sha1\n[extensions]\n\tobjectformat = sha256\n",
            b'[extensions "x"]\n\tobjectformat = sha256\n',
            b"[extensions.x]\n\tobjectformat = sha256\n",
            b"[include]\n\tpath = other\n",
            b"# [extensions]\n; objectformat = sha256\n",
            b"[extensions]objectformat = sha256\n",
            b"[extensions] objectformat\t=\tsha256\t \n",
            b"[extensions]\n\tobjectformat = sha256\x00junk\n",
            b'[extensions]\n\tobjectformat = sha""256\n',
            b"[extensions]\n\tobjectformat = sha256\r\r\n",
            b"[extensions]\n\tobjectformat = sha256\\",
            b"[extensions]\n\tobjectformat = sha256",
            b"[extensions]\n\tobjectformat\n",
            b"[extensions]\n\tobjectformat = sha512\n",
            b"[extensions]\n\tobjectformat = SHA256\n",
            b'[extensions]\n\tobjectformat = "sha256\n',
            b'[extensions]\n\tobjectformat = "sha" "256"\n',
            b"[extensions]\n\tobjectformat = s\\ha256\n",
            b"[extensions\n\tobjectformat = sha256\n",
            b"[extensions]\n\t-objectformat = sha256\n",
            b"[extensions]\n\tobject_format = sha256\n",
            b"\xef\xbb[extensions]\n\tobjectformat = sha256\n",
            b"objectformat = sha256\n",
            b'[ "x"]\n\tobjectformat = sha256\n',
            b"[]\n",
        ]
        env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
        accepted = 0
        for index, case in enumerate(cases):
            with self.subTest(config=case):
                git_dir = self.root / f"fmt{index}" / ".git"
                for directory in ("objects", "refs"):
                    (git_dir / directory).mkdir(parents=True)
                (git_dir / "HEAD").write_text("ref: refs/heads/main\n")
                config = git_dir / "config"
                version = b"[core]\n\trepositoryformatversion = 1\n"
                bom = len(UTF8_BOM) if case.startswith(UTF8_BOM) else 0
                config.write_bytes(case[:bom] + version + case[bom:])
                # The parser reads every value git's config reader reads, and fails where it fails.
                listing = subprocess.run(["git", "config", "--file", str(config), "--null", "--get-all",
                                          "extensions.objectformat"], env=env, capture_output=True)
                try:
                    values = [(value or b"").split(b"\0", 1)[0] for name, value
                              in git_helper._git_config_entries(config.read_bytes()) if name == b"extensions.objectformat"]
                except ValueError:
                    values = None
                if listing.returncode in (0, 1):
                    self.assertEqual(values, listing.stdout.split(b"\0")[:-1], listing.stderr)
                else:
                    self.assertIsNone(values, listing.stderr)
                # And the object format is git's wherever git accepts the repository; where git
                # refuses it, every git command fails whatever attribute source makewand names.
                oracle = subprocess.run(["git", "rev-parse", "--show-object-format"], cwd=str(git_dir.parent),
                                        env=env, capture_output=True, text=True)
                if oracle.returncode == 0:
                    accepted += 1
                    self.assertEqual(git_helper._object_format(git_dir), oracle.stdout.strip())
        self.assertGreater(accepted, 15)

    def test_an_object_format_git_cannot_read_refuses_to_start_git(self):
        repo = self.repository(self.root / "repo", {"f.txt": "one\n"})
        self.git(repo, "config", "core.repositoryformatversion", "1")
        self.git(repo, "config", "extensions.objectFormat", "sha512")
        with patch.object(git_helper.subprocess, "run") as run:
            code, _, error = git_helper.run_git_cmd(["git", "status"], cwd=str(repo))
        self.assertNotEqual(code, 0)
        self.assertIn("Cannot shield Git attributes", error)
        run.assert_not_called()


class GlobalOptionTests(NestedFixture):
    """run_git_cmd shields the repository its cwd names; a caller cannot point git elsewhere."""

    def test_options_that_point_git_elsewhere_are_refused(self):
        repo = self.repository(self.root / "repo", {"f.txt": "one\n"})
        other = str(self.root / "other")
        refused = [
            ["git", "-C", other, "status"], ["git", "--git-dir", other, "status"],
            ["git", f"--git-dir={other}", "status"], ["git", "--work-tree", other, "status"],
            ["git", f"--work-tree={other}", "status"], ["git", "--namespace", "n", "status"],
            ["git", "--namespace=n", "status"], ["git", "--bare", "status"],
            ["git", "-c", "include.path=/x", "status"], ["git", "-c", "Include.Path=/x", "status"],
            ["git", "-c", " include.path=/x", "status"],
            ["git", "-c", "includeIf.gitdir:/x/.path=/y", "status"], ["git", "-c", "INCLUDEIF.onbranch:m.path=/y", "status"],
            ["git", "--config-env", "core.x=HOME", "status"], ["git", "--config-env=core.x=HOME", "status"],
            ["git", "--attr-source", "HEAD", "status"], ["git", "--attr-source=HEAD", "status"],
            ["git", "--exec-path=/x", "status"], f"git -C {shlex.quote(other)} status",
        ]
        for command in refused:
            with self.subTest(command=command), patch.object(git_helper.subprocess, "run") as run:
                code, _, error = git_helper.run_git_cmd(command, cwd=str(repo))
                self.assertEqual(code, -1)
                self.assertIn("cwd", error)
                run.assert_not_called()
        allowed = [["git", "-c", "user.name=x", "status"], ["git", "--no-replace-objects", "--literal-pathspecs", "status"],
                   ["git", "commit", "-C", "HEAD", "--dry-run"], ["git", "-c", "core.includeIf=x", "status"]]
        for command in allowed:
            with self.subTest(command=command), patch.object(git_helper.subprocess, "run", return_value=COMPLETED) as run:
                git_helper.run_git_cmd(command, cwd=str(repo))
                run.assert_called()

    def test_the_subcommand_is_found_behind_options_with_values(self):
        for command, index in ((["git", "-c", "a=b", "status"], 3), (["git", "--shallow-file", "x", "log"], 3),
                               (["git", "--no-pager", "-c", "a=b", "--literal-pathspecs", "diff"], 5),
                               (["git", "--attr-source", "x", "diff"], 3), (["git", "--config-env", "a=B", "add"], 3)):
            with self.subTest(command=command):
                self.assertEqual(git_helper._git_subcommand_index(command), index)

    def test_apply_cannot_be_pointed_at_another_repository(self):
        target = self.repository(self.root / "target", {"f.txt": "one\n", ".gitattributes": "*.txt filter=evil\n"})
        self.plant_filter(target / ".git" / "config")
        other = self.root / "other"
        other.mkdir()
        code, _, error = git_helper.run_git_cmd(["git", "-C", str(target), "apply", "-"], cwd=str(other),
                                                input_data=PATCH.encode())
        self.assertNotEqual(code, 0)
        self.assertEqual((target / "f.txt").read_text(), "one\n")
        self.assertEqual(self.marker_lines(), [])

    def test_a_clone_never_checks_out_and_takes_no_attribute_source(self):
        for command in (["git", "clone", "--shared", "a", "b"], ["git", "-c", "protocol.file.allow=always", "clone", "a", "b"]):
            with self.subTest(command=command), patch.object(git_helper.subprocess, "run", return_value=COMPLETED) as run:
                git_helper.run_git_cmd(command, cwd=str(self.root))
                executed = run.call_args.args[0]
                self.assertEqual(executed[executed.index("clone") + 1], "--no-checkout")
                self.assertFalse([argument for argument in executed if argument.startswith("--attr-source")])

    def test_a_shadow_clone_runs_no_global_filter(self):
        # The untrusted repository's .gitattributes selects a filter the user defines globally.
        repo = self.repository(self.root / "repo", {"f.txt": "one\n", ".gitattributes": "* filter=evil\n"})
        home = self.global_config()
        self.plant_filter(home / ".gitconfig")
        for trust in ("untrusted", "trusted"):
            with self.subTest(trust=trust):
                self.marker.unlink(missing_ok=True)
                result = git_helper.create_ephemeral_shadow_worktree(str(repo), repo_trust=trust)
                self.assertIsNotNone(result[0])
                self.addCleanup(result[2])
                self.assertEqual(Path(result[0], "f.txt").read_text(), "one\n")
                self.assertEqual(self.marker_lines(), [])

    def test_ignore_listings_of_a_plain_directory_name_their_repository_by_cwd(self):
        work = self.root / "work"
        work.mkdir()
        (work / ".gitignore").write_text("*.log\n")
        (work / "a.py").write_text("a\n")
        (work / "b.log").write_text("b\n")
        with patch.object(git_helper, "run_git_cmd", wraps=git_helper.run_git_cmd) as run:
            self.assertEqual(git_helper.list_workspace_copy_paths(work), [".gitignore", "a.py"])
            self.assertEqual(git_helper.list_workspace_ignored_paths(work), {"b.log"})
        for call in run.call_args_list:
            self.assertFalse([argument for argument in call.args[0]
                              if str(argument).startswith(("--git-dir", "--work-tree", "-C"))], call)


class EnvironmentTests(NestedFixture):
    def test_attribute_source_and_discovery_variables_are_dropped(self):
        inherited = {"GIT_ATTR_SOURCE": "HEAD", "GIT_CEILING_DIRECTORIES": "/", "GIT_NAMESPACE": "n",
                     "GIT_DISCOVERY_ACROSS_FILESYSTEM": "1", "PATH": "/usr/bin"}
        clean = git_helper._sanitize_git_env(inherited)
        self.assertEqual({key for key in inherited if key in clean}, {"PATH"})

    def test_an_inherited_attribute_source_does_not_break_apply(self):
        # git 2.55 segfaults on work-tree apply under any attribute source.
        repo = self.repository(self.root / "repo", {"f.txt": "one\n"})
        with patch.dict(os.environ, {"GIT_ATTR_SOURCE": SHA1_EMPTY_TREE}):
            code, _, error = git_helper.run_git_cmd(["git", "apply", "-"], cwd=str(repo), input_data=PATCH.encode())
        self.assertEqual(code, 0, error)
        self.assertEqual((repo / "f.txt").read_text(), "one\ntwo\n")

    def test_filter_drivers_are_listed_inside_the_shield(self):
        # One shield from the driver listing until git apply exits: info/attributes never
        # comes back in between, as it did around a separate listing command.
        repo = self.repository(self.root / "repo", {"f.txt": "one\n", ".gitattributes": "*.txt filter=evil\n"})
        self.plant_filter(repo / ".git" / "config")
        self.plant_attributes(repo / ".git")
        moves = []
        rename = Path.rename

        def recording(source, target):
            moves.append((Path(source).name, Path(target).name))
            return rename(source, target)

        with patch.object(Path, "rename", recording):
            code, _, error = git_helper.run_git_cmd(["git", "apply", "-"], cwd=str(repo), input_data=PATCH.encode())
        self.assertEqual(code, 0, error)
        self.assertEqual(len([move for move in moves if "attributes" in move[0]]), 2, moves)
        self.assertEqual(self.marker_lines(), [])
        self.assertEqual((repo / ".git" / "info" / "attributes").read_text(), "* filter=evil\n")


if __name__ == "__main__":
    unittest.main()
