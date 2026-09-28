"""G1 data-safety regressions for run_pipeline host mode.

Each test reproduces a data-loss path from the 2026-09 evaluation
(py-orchestrator#1/#2/#5/#8/#9/#10, replay-0926-memory#1/#13, arch-product#7):
failing tasks must only undo their own changes, never delete files that existed
before the task, and must not claim a restored baseline they did not verify.
"""

import contextlib
import io
import os
import stat
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.git_helper as git_helper
import makewand.orchestrator as orch
from makewand.git_helper import run_git_cmd

REPO_ROOT = Path(__file__).resolve().parent.parent
QUESTION = "这个项目支持 Windows 吗？"


def git(path, *args):
    code, out, err = run_git_cmd(["git", *args], cwd=str(path))
    if code:
        raise AssertionError((args, out, err))
    return out.strip()


def tree_state(root: Path):
    """Content-level view of a directory (excluding .git)."""
    state = {}
    for directory, dirs, files in os.walk(root):
        dirs[:] = [d for d in dirs if d != ".git"]
        base = Path(directory)
        for name in dirs + files:
            path = base / name
            rel = path.relative_to(root).as_posix()
            info = os.lstat(path)
            if stat.S_ISLNK(info.st_mode):
                state[rel] = ("link", os.readlink(path))
                if name in dirs:
                    dirs.remove(name)
            elif stat.S_ISDIR(info.st_mode):
                state[rel] = ("dir",)
            else:
                try:
                    content = path.read_bytes()
                except PermissionError:
                    content = None
                state[rel] = ("file", content, stat.S_IMODE(info.st_mode))
    return state


def make_project(root: Path, extra_ignore=""):
    root.mkdir(parents=True, exist_ok=True)
    (root / ".gitignore").write_text(".env\nvenv/\n*.log\ndata/\nlogs/\n" + extra_ignore)
    (root / ".env").write_text("SECRET=original\n")
    (root / "venv" / "lib").mkdir(parents=True)
    (root / "venv" / "lib" / "site.py").write_text("site = 1\n")
    (root / "notes.log").write_text("log line\n")
    (root / "data").mkdir()
    (root / "data" / "db.sqlite").write_bytes(b"SQLite format 3\x00" + b"\x01" * 64)
    (root / "logs").mkdir()
    (root / "logs" / "a.log").write_text("old log\n")
    (root / "app.py").write_text("print('hi')\n")
    return root


def make_git_project(root: Path):
    make_project(root)
    git(root, "init", "-q")
    git(root, "config", "user.name", "G1 Test")
    git(root, "config", "user.email", "g1@example.invalid")
    git(root, "add", "-A")
    git(root, "commit", "-q", "-m", "baseline")
    return root


class PipelineHarness(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-g1-pipeline-")
        self.addCleanup(self._restore_permissions_then_cleanup)
        self.base = Path(self._tmp.name)
        self.state = self.base / "state"
        for name, value in (("ARTIFACTS_DIR", self.state / "artifacts"),
                            ("SHADOW_WORKTREES_DIR", self.state / "shadow")):
            patcher = patch.object(config, name, value, create=True)
            patcher.start()
            self.addCleanup(patcher.stop)

    def _restore_permissions_then_cleanup(self):
        for directory, dirs, files in os.walk(self.base):
            for name in dirs + files:
                path = os.path.join(directory, name)
                if not os.path.islink(path):
                    try:
                        os.chmod(path, 0o700)
                    except OSError:
                        pass
        self._tmp.cleanup()

    def run_pipeline(self, cwd, coder, review_pass=False, intent="code", isolation=(True, None),
                     route=None, git_intercept=None, prompt=QUESTION):
        self.dispatch_calls = []

        def dispatch(engine, prompt_text, cwd=None, readonly=False, **kwargs):
            self.dispatch_calls.append((engine, readonly))
            if readonly:
                verdict = "true" if review_pass else "false"
                defects = "[]" if review_pass else '["rejected"]'
                return True, f'MAKEWAND_VERDICT: {{"pass": {verdict}, "defects": {defects}}}', None
            return coder(Path(cwd))

        route = route or (["codex"], ["claude"], {"primary_coder": "codex", "primary_reviewer": "claude", "reasons": []})
        out, err = io.StringIO(), io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=isolation))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={}))
            stack.enter_context(patch.object(orch, "classify_prompt_intent", return_value=intent))
            if route != "real":
                stack.enter_context(patch.object(orch, "select_optimal_engine_pair", return_value=route))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, None)))
            stack.enter_context(patch("makewand.memory.format_memory_hints_for_prompt", return_value=""))
            if git_intercept is not None:
                real = git_helper.run_git_cmd

                def intercepted(cmd, *args, **kwargs):
                    result = git_intercept(cmd)
                    return result if result is not None else real(cmd, *args, **kwargs)

                stack.enter_context(patch.object(git_helper, "run_git_cmd", side_effect=intercepted))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(err))
            result = orch.run_pipeline(prompt, cwd=str(cwd), auto_fix=False)
        self.output = out.getvalue() + err.getvalue()
        return result

    def coder_calls(self):
        return [call for call in self.dispatch_calls if not call[1]]


class NonGitDirectoryTests(PipelineHarness):
    def test_yes_no_question_without_changes_keeps_every_preexisting_file(self):
        """py-orchestrator#1 / replay#1 variant A: .env, venv, logs, data survive; .git is removed."""
        project = make_project(self.base / "nongit")
        before = tree_state(project)

        result = self.run_pipeline(project, lambda cwd: (True, "是的，支持 Windows。", None))

        self.assertFalse(result)
        self.assertEqual(tree_state(project), before)
        self.assertFalse((project / ".git").exists(), "makewand-created .git must be removed")
        self.assertIn("逐项核验", self.output)

    def test_failed_git_add_aborts_before_dispatch_and_keeps_sources(self):
        """py-orchestrator#1 variant B / #5: git add failure aborts; nothing is deleted."""
        if hasattr(os, "geteuid") and os.geteuid() == 0:
            self.skipTest("root can read chmod 000 files")
        project = make_project(self.base / "unreadable")
        locked = project / "locked.bin"
        locked.write_bytes(b"secret")
        locked.chmod(0)
        before = tree_state(project)

        result = self.run_pipeline(project, lambda cwd: (True, "unused", None))

        self.assertFalse(result)
        self.assertEqual(self.coder_calls(), [], "no model may be dispatched without a complete baseline")
        self.assertEqual(tree_state(project), before)
        self.assertTrue(os.path.lexists(locked))
        self.assertFalse((project / ".git").exists())
        self.assertIn("git add", self.output)

    def test_rejected_change_is_rolled_back_and_git_removed(self):
        """py-orchestrator#8: rollback restores sources, removes new files and the temporary .git."""
        project = make_project(self.base / "reject")
        before = tree_state(project)

        def coder(cwd):
            (cwd / "app.py").write_text("print('changed')\n")
            (cwd / "new_module.py").write_text("x = 1\n")
            (cwd / "notes.log").write_text("overwritten\n")
            (cwd / ".env").unlink()
            return True, "done", None

        self.assertFalse(self.run_pipeline(project, coder, review_pass=False))
        self.assertEqual(tree_state(project), before)
        self.assertFalse((project / ".git").exists())
        rejected = list((self.state / "artifacts").glob("rejected_*/rejected.patch"))
        self.assertEqual(len(rejected), 1)
        self.assertIn("changed", rejected[0].read_text())

    def test_success_archives_patch_privately_and_removes_git(self):
        """arch-product#7 / decision (a): delivery patch saved (0600) before .git removal."""
        project = make_project(self.base / "success")

        def coder(cwd):
            (cwd / "app.py").write_text("print('improved')\n")
            (cwd / "helper.py").write_text("def helper():\n    return 1\n")
            return True, "done", None

        self.assertTrue(self.run_pipeline(project, coder, review_pass=True))
        self.assertFalse((project / ".git").exists())
        self.assertEqual((project / "app.py").read_text(), "print('improved')\n")
        self.assertEqual((project / ".env").read_text(), "SECRET=original\n")
        patches = list((self.state / "artifacts").glob("delivery_*/makewand_delivery.patch"))
        self.assertEqual(len(patches), 1)
        self.assertIn("helper.py", patches[0].read_text())
        self.assertEqual(stat.S_IMODE(patches[0].stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(patches[0].parent.stat().st_mode), 0o700)

    def test_exception_during_task_still_rolls_back(self):
        """py-orchestrator#10: an exception mid-task never leaves partial changes or .git behind."""
        project = make_project(self.base / "boom")
        before = tree_state(project)

        def coder(cwd):
            (cwd / "half_written.py").write_text("partial")
            (cwd / ".env").write_text("SECRET=pwned\n")
            raise RuntimeError("provider crashed")

        with self.assertRaises(RuntimeError):
            self.run_pipeline(project, coder)
        self.assertEqual(tree_state(project), before)
        self.assertFalse((project / ".git").exists())

    def test_relocated_preexisting_file_is_moved_back_not_deleted(self):
        """Moving a pre-existing ignored file must not turn it into a 'new' file that gets deleted."""
        project = make_project(self.base / "moved")
        big = project / "data" / "big.bin"
        big.write_bytes(os.urandom(2 * 1024 * 1024))  # above the 1 MB backup budget
        before = tree_state(project)

        def coder(cwd):
            (cwd / "data" / "big.bin").rename(cwd / "big_moved.bin")
            (cwd / "app.py").write_text("print('changed')\n")
            return True, "done", None

        self.assertFalse(self.run_pipeline(project, coder))
        self.assertEqual(tree_state(project), before)

    def test_symlinked_directory_swap_cannot_redirect_restore(self):
        """A task replacing an ignored directory with an outside symlink cannot make restore write outside."""
        project = make_project(self.base / "swap")
        outside = self.base / "outside"
        outside.mkdir()
        before = tree_state(project)

        def coder(cwd):
            import shutil
            shutil.rmtree(cwd / "logs")
            os.symlink(str(outside), cwd / "logs")
            return True, "done", None

        self.assertFalse(self.run_pipeline(project, coder))
        self.assertEqual(tree_state(project), before)
        self.assertEqual(list(outside.iterdir()), [], "restore must never write through the planted symlink")

    def test_stale_temporary_git_from_interrupted_run_is_removed(self):
        project = make_project(self.base / "stale")
        created, error = git_helper.init_git_baseline(str(project), ephemeral=True)
        self.assertTrue(created, error)
        before = tree_state(project)

        self.assertFalse(self.run_pipeline(project, lambda cwd: (True, "answer only", None)))
        self.assertFalse((project / ".git").exists())
        self.assertEqual(tree_state(project), before)


    def test_workspace_containing_makewand_state_or_home_is_refused(self):
        """Makewand's own backups/locks and the user's home are never swept into a rollback."""
        project = make_project(self.base / "contains_state")
        before = tree_state(project)
        with patch.object(config, "ARTIFACTS_DIR", project / "mw-state" / "artifacts"):
            self.assertFalse(self.run_pipeline(project, lambda cwd: (True, "unused", None)))
        self.assertIn("状态目录", self.output)
        self.assertEqual(self.coder_calls(), [])
        # Only the configured state directory itself (holding the lock) may appear.
        after = {k: v for k, v in tree_state(project).items() if not k.startswith("mw-state")}
        self.assertEqual(after, before)

        home_parent = make_project(self.base / "contains_home")
        (home_parent / "user_home").mkdir()
        before = tree_state(home_parent)
        with patch.dict(os.environ, {"HOME": str(home_parent / "user_home")}):
            self.assertFalse(self.run_pipeline(home_parent, lambda cwd: (True, "unused", None)))
        self.assertIn("用户主目录", self.output)
        self.assertEqual(self.coder_calls(), [])
        self.assertEqual(tree_state(home_parent), before)
        self.assertFalse((home_parent / ".git").exists())


class GitHostModeTests(PipelineHarness):
    def test_rejected_task_restores_modified_ignored_files(self):
        """py-orchestrator#2 F1: ignored files changed by the coder are restored or reported."""
        project = make_git_project(self.base / "repo")
        before = tree_state(project)

        def coder(cwd):
            (cwd / "app.py").write_text("print('bad')\n")
            (cwd / ".env").write_text("SECRET=pwned_by_agent\n")
            (cwd / "logs" / "a.log").unlink()
            (cwd / "venv" / "lib" / "site.py").unlink()
            (cwd / "venv" / "lib" / "evil.pth").write_text("import os\n")
            return True, "done", None

        self.assertFalse(self.run_pipeline(project, coder))
        after = tree_state(project)
        self.assertEqual((project / "app.py").read_text(), "print('hi')\n")
        self.assertEqual((project / ".env").read_text(), "SECRET=original\n")
        self.assertEqual((project / "logs" / "a.log").read_text(), "old log\n")
        self.assertFalse((project / "venv" / "lib" / "evil.pth").exists())
        # venv content is metadata-only: the loss must be reported, never hidden.
        self.assertNotIn("venv/lib/site.py", after)
        self.assertIn("venv/lib/site.py", self.output)
        self.assertNotIn("已恢复基线", self.output)
        self.assertIn("回滚未能完全恢复", self.output)
        before.pop("venv/lib/site.py")
        self.assertEqual(after, before)

    def test_successful_task_reports_ignored_file_changes(self):
        """py-orchestrator#2 F2: ignored changes are not in the reviewed diff, so delivery warns."""
        project = make_git_project(self.base / "repo_ok")

        def coder(cwd):
            (cwd / "app.py").write_text("print('good')\n")
            (cwd / ".env").write_text("SECRET=changed\n")
            (cwd / "venv" / "lib" / "evil.pth").write_text("import os\n")
            return True, "done", None

        self.assertTrue(self.run_pipeline(project, coder, review_pass=True))
        self.assertIn("不在审查 diff 中", self.output)
        self.assertIn(".env", self.output)
        self.assertIn("venv/lib/evil.pth", self.output)
        self.assertTrue((project / ".git").exists(), "a user's own repository is never removed")

    def test_git_status_failure_at_baseline_deletes_nothing(self):
        """py-orchestrator#5: a failed/timed-out git status aborts before dispatch with zero deletions."""
        project = make_git_project(self.base / "status_fail")
        (project / "untracked_note.txt").write_text("keep me\n")
        before = tree_state(project)

        def failing_status(cmd):
            if isinstance(cmd, list) and "status" in cmd:
                return -1, b"" if "-z" in cmd else "", "timed out"
            return None

        self.assertFalse(self.run_pipeline(project, lambda cwd: (True, "unused", None), git_intercept=failing_status))
        self.assertEqual(self.coder_calls(), [])
        self.assertEqual(tree_state(project), before)

    def test_git_status_failure_during_rollback_never_deletes_preexisting_files(self):
        project = make_git_project(self.base / "status_fail_late")
        before = tree_state(project)
        state = {"coded": False}

        def coder(cwd):
            state["coded"] = True
            (cwd / "app.py").write_text("print('bad')\n")
            (cwd / "scratch.tmp").write_text("new\n")
            return True, "done", None

        def failing_status_after_coding(cmd):
            if state["coded"] and isinstance(cmd, list) and "status" in cmd:
                return 128, b"" if "-z" in cmd else "", "fatal: index locked"
            return None

        self.assertFalse(self.run_pipeline(project, coder, git_intercept=failing_status_after_coding))
        self.assertEqual(tree_state(project), before)
        self.assertNotIn("已恢复基线", self.output)
        self.assertIn("无法核验 git 状态", self.output)

    def test_failed_reset_stops_before_any_deletion(self):
        """py-orchestrator#5/#10: a failed reset --hard is reported and nothing is deleted afterwards."""
        project = make_git_project(self.base / "reset_fail")

        def coder(cwd):
            (cwd / "app.py").write_text("print('bad')\n")
            (cwd / "created.py").write_text("x = 1\n")
            return True, "done", None

        def failing_reset(cmd):
            if isinstance(cmd, list) and "reset" in cmd:
                return 128, "", "fatal: Unable to create index.lock"
            return None

        self.assertFalse(self.run_pipeline(project, coder, git_intercept=failing_reset))
        self.assertTrue((project / "created.py").exists(), "no deletion after a failed reset")
        self.assertEqual((project / ".env").read_text(), "SECRET=original\n")
        self.assertIn("git reset --hard 失败", self.output)
        self.assertNotIn("已恢复基线", self.output)

    def test_second_process_is_refused_by_workspace_lock(self):
        """py-orchestrator#9 / replay#13: a concurrent makewand task on the same repository is refused."""
        project = make_git_project(self.base / "locked_repo")
        holder_code = textwrap.dedent(f"""
            import sys
            sys.path.insert(0, {str(REPO_ROOT)!r})
            import makewand.config as config
            from pathlib import Path
            config.ARTIFACTS_DIR = Path({str(self.state / 'artifacts')!r})
            from makewand.git_helper import WorkspaceLock
            lock = WorkspaceLock({str(project)!r}).acquire()
            print("locked", flush=True)
            sys.stdin.read()
            lock.release()
        """)
        holder = subprocess.Popen([sys.executable, "-I", "-c", holder_code], stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, text=True)
        try:
            self.assertEqual(holder.stdout.readline().strip(), "locked")
            before = tree_state(project)

            def coder(cwd):
                (cwd / "app.py").write_text("print('clobber')\n")
                return True, "done", None

            self.assertFalse(self.run_pipeline(project / "venv", coder))
            self.assertEqual(self.coder_calls(), [])
            self.assertIn("另一个 makewand 任务正在此目录运行", self.output)
            self.assertEqual(tree_state(project), before)
        finally:
            holder.stdin.close()
            holder.wait(timeout=30)
        # Once the other task finished, the repository is usable again.
        self.assertFalse(self.run_pipeline(project, lambda cwd: (True, "no change", None)))
        self.assertEqual(len(self.coder_calls()), 1)
        lock_files = list((self.state / "artifacts" / ".locks").glob("*.lock"))
        self.assertTrue(lock_files)
        self.assertFalse(any(p.name.endswith(".lock") for p in project.rglob("*.lock")),
                         "the lock file must not live inside the user repository")


class NoProviderTests(PipelineHarness):
    def test_zero_providers_report_no_false_detection_and_do_not_touch_workspace(self):
        """arch-product#7: N=0 must not claim a detected tool, init git or leave candidates."""
        project = make_project(self.base / "n0")
        before = tree_state(project)
        with patch("makewand.config.get_active_providers", return_value=[]):
            result = self.run_pipeline(project, lambda cwd: (True, "unused", None), route="real")
        self.assertFalse(result)
        self.assertNotIn("检测到 1 个", self.output)
        self.assertIn("未检测到任何可用的 AI 编码工具", self.output)
        self.assertEqual(self.coder_calls(), [])
        self.assertFalse((project / ".git").exists())
        self.assertEqual(tree_state(project), before)


if __name__ == "__main__":
    unittest.main()
