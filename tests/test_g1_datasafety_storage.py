"""G1 data-safety regressions for private artifacts, locks, git timeouts and race candidates.

Covers py-orchestrator#5/#6/#7, replay-0926-memory#20, runtime-state#8 and
py-security#3: nothing is written to fixed shared /tmp paths, artifacts are
0700/0600 with unpredictable names, candidates never copy .gitignore'd secrets
and candidate eviction is never silent.
"""

import contextlib
import io
import json
import os
import stat
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.git_helper as git_helper
import makewand.orchestrator as orch
from makewand import candidate
from makewand.git_helper import run_git_cmd


def git(path, *args):
    code, out, err = run_git_cmd(["git", *args], cwd=str(path))
    if code:
        raise AssertionError((args, out, err))
    return out.strip()


def mode_of(path) -> int:
    return stat.S_IMODE(os.lstat(path).st_mode)


class StorageHarness(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-g1-storage-")
        self.addCleanup(self._tmp.cleanup)
        self.base = Path(self._tmp.name)
        self.state = self.base / "state"
        self.config_dir = self.base / "config"
        for obj, name, value in ((config, "ARTIFACTS_DIR", self.state / "artifacts"),
                                 (config, "SHADOW_WORKTREES_DIR", self.state / "shadow"),
                                 (config, "CONFIG_DIR", self.config_dir),
                                 (config, "CANDIDATES_DIR", self.config_dir / "candidates"),
                                 (config, "BACKUPS_DIR", self.config_dir / "backups"),
                                 (orch, "CANDIDATES_DIR", self.config_dir / "candidates")):
            patcher = patch.object(obj, name, value, create=True)
            patcher.start()
            self.addCleanup(patcher.stop)

    def make_repo(self, name="repo"):
        repo = self.base / name
        repo.mkdir()
        git(repo, "init", "-q")
        git(repo, "config", "user.name", "G1")
        git(repo, "config", "user.email", "g1@example.invalid")
        (repo / ".gitignore").write_text(".env\ndata/\n")
        (repo / ".env").write_text("API_TOKEN=do-not-copy\n")
        (repo / "data").mkdir()
        (repo / "data" / "customers.csv").write_text("alice,secret\n")
        (repo / "app.py").write_text("BASE = 1\n")
        git(repo, "add", "-A")
        git(repo, "commit", "-q", "-m", "baseline")
        return repo


class ContractTests(StorageHarness):
    def test_default_paths_follow_xdg_state_home_and_env_overrides(self):
        import importlib
        import importlib.util
        import makewand.config as cfg
        with patch.dict(os.environ, {"XDG_STATE_HOME": str(self.base / "xdg")}, clear=False):
            os.environ.pop("MAKEWAND_ARTIFACTS_DIR", None)
            os.environ.pop("MAKEWAND_SHADOW_DIR", None)
            fresh = importlib.util.module_from_spec(importlib.util.find_spec("makewand.config"))
            fresh.__spec__.loader.exec_module(fresh)
            self.assertEqual(fresh.ARTIFACTS_DIR, self.base / "xdg" / "makewand" / "artifacts")
            self.assertEqual(fresh.SHADOW_WORKTREES_DIR, self.base / "xdg" / "makewand" / "shadow-worktrees")
        with patch.dict(os.environ, {"MAKEWAND_ARTIFACTS_DIR": str(self.base / "a"), "MAKEWAND_SHADOW_DIR": str(self.base / "s")}):
            fresh = importlib.util.module_from_spec(importlib.util.find_spec("makewand.config"))
            fresh.__spec__.loader.exec_module(fresh)
            self.assertEqual(fresh.ARTIFACTS_DIR, self.base / "a")
            self.assertEqual(fresh.SHADOW_WORKTREES_DIR, self.base / "s")
        self.assertIs(cfg, config)

    def test_ensure_private_dir_tightens_mode_and_refuses_symlinks_and_foreign_owner(self):
        target = self.base / "shared"
        target.mkdir(mode=0o777)
        os.chmod(target, 0o777)
        self.assertEqual(config.ensure_private_dir(target), target)
        self.assertEqual(mode_of(target), 0o700)

        real = self.base / "real"
        real.mkdir()
        link = self.base / "planted"
        link.symlink_to(real)
        with self.assertRaises(PermissionError):
            config.ensure_private_dir(link)

        if hasattr(os, "getuid"):
            with patch("os.getuid", return_value=os.getuid() + 4242):
                with self.assertRaises(PermissionError):
                    config.ensure_private_dir(real)


class ArtifactTests(StorageHarness):
    def test_artifact_dirs_are_private_unpredictable_and_retained(self):
        first = git_helper.create_private_artifact_dir("delivery")
        second = git_helper.create_private_artifact_dir("delivery")
        self.assertNotEqual(first, second)
        self.assertTrue(str(first).startswith(str(self.state / "artifacts")))
        self.assertFalse(str(first).startswith("/tmp/makewand-artifacts"))
        self.assertEqual(mode_of(first), 0o700)
        self.assertEqual(mode_of(self.state / "artifacts"), 0o700)
        written = git_helper.write_private_file(first / "x.patch", b"diff")
        self.assertEqual(mode_of(written), 0o600)
        script = git_helper.write_private_file(first / "apply.sh", "#!/bin/sh\n", mode=0o700)
        self.assertEqual(mode_of(script), 0o700)
        with self.assertRaises(FileExistsError):
            git_helper.write_private_file(first / "x.patch", b"again")
        os.symlink(self.base / "victim", first / "link.patch")
        with self.assertRaises(OSError):
            git_helper.write_private_file(first / "link.patch", b"redirect")
        self.assertFalse((self.base / "victim").exists())

        with patch.dict(os.environ, {"MAKEWAND_ARTIFACTS_KEEP": "5"}):
            for _ in range(8):
                git_helper.create_private_artifact_dir("rejected")
        kept = [p for p in (self.state / "artifacts").iterdir() if p.name.startswith(("delivery_", "rejected_"))]
        self.assertLessEqual(len(kept), 6)

    def test_shadow_delivery_artifacts_are_private(self):
        """py-security#3: delivery patch/manifest 0600, apply script 0700, directory 0700."""
        repo = self.make_repo("host")
        shadow = self.base / "shadow_copy"
        import shutil
        shutil.copytree(repo, shadow)
        baseline = git(repo, "rev-parse", "HEAD")
        result = git_helper.ShadowWorktreeResult(str(shadow), "makewand/g1-private", lambda: None,
                                                 baseline_commit=baseline, repo_head=baseline,
                                                 repo_root=str(repo), worktree_root=str(shadow))

        def dispatch(engine, prompt, cwd=None, readonly=False, **kwargs):
            if readonly:
                return True, 'MAKEWAND_VERDICT: {"pass": true, "defects": []}', None
            (Path(cwd) / "app.py").write_text("BASE = 2\n")
            return True, "done", None

        with contextlib.ExitStack() as stack:
            for name, value in (("check_load_backpressure", True),
                                ("check_working_tree_isolation", (False, "fixture")),
                                ("create_ephemeral_shadow_worktree", result),
                                ("get_or_update_status", {}),
                                ("select_optimal_engine_pair", (["codex"], ["claude"], {
                                    "primary_coder": "codex", "primary_reviewer": "claude", "reasons": []})),
                                ("run_local_tests", (True, None))):
                stack.enter_context(patch.object(orch, name, return_value=value))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
            stack.enter_context(patch("makewand.memory.format_memory_hints_for_prompt", return_value=""))
            out = io.StringIO()
            stack.enter_context(contextlib.redirect_stdout(out))
            self.assertTrue(orch.run_pipeline("Implement update", cwd=str(repo), force_code=True, auto_fix=False))
        deliveries = list((self.state / "artifacts").glob("delivery_*"))
        self.assertEqual(len(deliveries), 1)
        delivery = deliveries[0]
        self.assertEqual(mode_of(delivery), 0o700)
        self.assertEqual(mode_of(delivery / "makewand_delivery.patch"), 0o600)
        self.assertEqual(mode_of(delivery / "delivery_manifest.json"), 0o600)
        self.assertEqual(mode_of(delivery / "apply_delivery.sh"), 0o700)
        self.assertIn(str(delivery / "apply_delivery.sh"), out.getvalue())
        self.assertNotIn("/tmp/makewand-artifacts", out.getvalue())


class GitCommandTests(StorageHarness):
    def test_git_timeout_is_configurable_and_generous_by_default(self):
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("MAKEWAND_GIT_TIMEOUT", None)
            self.assertGreaterEqual(git_helper.get_git_timeout(), 300)
        captured = {}
        real_run = git_helper.subprocess.run

        def fake_run(*args, **kwargs):
            captured.update(kwargs)
            return real_run(*args, **kwargs)

        with patch.dict(os.environ, {"MAKEWAND_GIT_TIMEOUT": "7"}), \
                patch.object(git_helper.subprocess, "run", side_effect=fake_run):
            git_helper.run_git_cmd(["git", "--version"])
        self.assertEqual(captured.get("timeout"), 7.0)
        with patch.dict(os.environ, {"MAKEWAND_GIT_TIMEOUT": "not-a-number"}):
            self.assertEqual(git_helper.get_git_timeout(), git_helper.DEFAULT_GIT_TIMEOUT)

    def test_isolation_check_treats_unreadable_git_status_as_unsafe(self):
        repo = self.make_repo()
        real = git_helper.run_git_cmd

        def failing_status(cmd, *args, **kwargs):
            if isinstance(cmd, list) and "status" in cmd:
                return -1, "", "timed out"
            return real(cmd, *args, **kwargs)

        with patch.object(git_helper, "get_active_interactive_working_trees", return_value={}), \
                patch.object(git_helper, "run_git_cmd", side_effect=failing_status):
            safe, message = git_helper.check_working_tree_isolation(str(repo))
        self.assertFalse(safe)
        self.assertIn("git 状态", message)

    def test_ensure_git_worktree_removes_partial_git_on_add_failure(self):
        if hasattr(os, "geteuid") and os.geteuid() == 0:
            self.skipTest("root can read chmod 000 files")
        project = self.base / "plain"
        project.mkdir()
        (project / "app.py").write_text("x = 1\n")
        locked = project / "locked.bin"
        locked.write_bytes(b"x")
        locked.chmod(0)
        try:
            with contextlib.redirect_stderr(io.StringIO()):
                created, error = git_helper.init_git_baseline(str(project))
        finally:
            locked.chmod(0o600)
        self.assertFalse(created)
        self.assertIn("git add", error)
        self.assertFalse((project / ".git").exists())
        self.assertEqual((project / "app.py").read_text(), "x = 1\n")

    def test_stray_empty_git_directory_is_not_a_repository(self):
        parent = self.base / "stray"
        (parent / ".git").mkdir(parents=True)
        child = parent / "project"
        child.mkdir()
        self.assertIsNone(git_helper.find_git_root(child))

    def test_shadow_worktree_is_private_and_clone_failure_leaves_nothing(self):
        repo = self.make_repo()
        with contextlib.redirect_stderr(io.StringIO()):
            result = git_helper.create_ephemeral_shadow_worktree(str(repo), prefix="g1")
        self.assertIsNotNone(result[0])
        worktree = Path(result.worktree_root)
        self.assertTrue(str(worktree).startswith(str(self.state / "shadow")))
        self.assertEqual(mode_of(worktree), 0o700)
        self.assertFalse((worktree / ".env").exists(), "shadow clones never receive ignored secrets")
        result[2]()

        real = git_helper.run_git_cmd

        def failing_clone(cmd, *args, **kwargs):
            if isinstance(cmd, list) and "clone" in cmd:
                return -1, "", "timed out"
            return real(cmd, *args, **kwargs)

        with patch.object(git_helper, "run_git_cmd", side_effect=failing_clone), \
                contextlib.redirect_stderr(io.StringIO()):
            failed = git_helper.create_ephemeral_shadow_worktree(str(repo), prefix="g1")
        self.assertIsNone(failed[0])
        self.assertEqual(list((self.state / "shadow").iterdir()), [], "a partial clone is never reused")


class CandidateTests(StorageHarness):
    def test_isolated_copy_skips_ignored_files_in_git_and_plain_directories(self):
        repo = self.make_repo()
        target = self.base / "copy_git"
        git_helper.clone_isolated_worktree(str(repo), target)
        self.assertTrue((target / "app.py").exists())
        self.assertFalse((target / ".env").exists())
        self.assertFalse((target / "data").exists())

        plain = self.base / "plain_dir"
        plain.mkdir()
        (plain / ".gitignore").write_text("*.key\n")
        (plain / "main.py").write_text("print(1)\n")
        (plain / "server.key").write_text("PRIVATE\n")
        target_plain = self.base / "copy_plain"
        git_helper.clone_isolated_worktree(str(plain), target_plain)
        self.assertTrue((target_plain / "main.py").exists())
        self.assertFalse((target_plain / "server.key").exists())
        self.assertFalse((plain / ".git").exists())

    def test_race_candidates_are_private_without_secrets_and_host_is_not_git_initialized(self):
        """py-orchestrator#7/#8: candidates 0700, meta 0600, no .env copy, no host git init."""
        host = self.base / "race_host"
        host.mkdir()
        (host / ".gitignore").write_text(".env\n")
        (host / ".env").write_text("TOKEN=secret\n")
        (host / "app.py").write_text("BASE = 1\n")

        def dispatch(engine, prompt, cwd=None, **kwargs):
            (Path(cwd) / "app.py").write_text("CANDIDATE = 1\n")
            return True, "implemented", None

        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={}))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, None)))
            stack.enter_context(patch.object(orch, "execute_agy_task", return_value=(
                True, 'MAKEWAND_RACE_VERDICT: {"pass": true, "winner": "A", "defects": []}', None)))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = orch.run_race("Implement update", cwd=str(host), engine_a="codex", engine_b="claude")
        self.assertEqual(code, orch.EXIT_PASSED)
        self.assertFalse((host / ".git").exists(), "race must not git-init the user's directory")
        candidates = self.config_dir / "candidates"
        self.assertEqual(mode_of(candidates), 0o700)
        races = list(candidates.iterdir())
        self.assertEqual(len(races), 1)
        self.assertEqual(mode_of(races[0]), 0o700)
        self.assertEqual(mode_of(races[0] / "meta.json"), 0o600)
        self.assertFalse(list(races[0].rglob(".env")), "ignored secrets are never copied into candidates")

        holder = git_helper.WorkspaceLock(str(host)).acquire()
        try:
            ok, _, message = candidate.CandidateManager.apply_candidate(races[0].name, "A")
        finally:
            holder.release()
        self.assertFalse(ok)
        self.assertIn("另一个 makewand 任务正在此目录运行", message)
        self.assertEqual((host / "app.py").read_text(), "BASE = 1\n")

        ok, applied, message = candidate.CandidateManager.apply_candidate(races[0].name, "A")
        self.assertTrue(ok, message)
        self.assertEqual((host / "app.py").read_text(), "CANDIDATE = 1\n")
        self.assertEqual((host / ".env").read_text(), "TOKEN=secret\n")
        meta = json.loads((races[0] / "meta.json").read_text())
        self.assertEqual(meta.get("applied_candidate"), "A")
        self.assertEqual(mode_of(self.config_dir / "backups"), 0o700)

    def test_race_where_both_racers_fail_without_changes_keeps_no_candidates(self):
        """arch-product#7: a race that produced nothing leaves no candidate directories behind."""
        host = self.base / "race_empty"
        host.mkdir()
        (host / "app.py").write_text("BASE = 1\n")
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={}))
            stack.enter_context(patch.object(orch, "dispatch_task", return_value=(False, "", "quota")))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, None)))
            stack.enter_context(patch.object(orch, "execute_agy_task", return_value=(False, "", "unavailable")))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = orch.run_race("Implement update", cwd=str(host), engine_a="codex", engine_b="claude")
        self.assertEqual(code, orch.EXIT_FAILED)
        candidates = self.config_dir / "candidates"
        self.assertEqual(list(candidates.iterdir()) if candidates.exists() else [], [])

    def test_eviction_prefers_applied_candidates_and_reports_unapplied(self):
        candidates = self.config_dir / "candidates"
        candidates.mkdir(parents=True)
        for index in range(6):
            race = candidates / f"rc_{index}"
            race.mkdir()
            meta = {"created_at": f"2026-09-27T0{index}:00:00"}
            if index in (4, 5):
                meta["applied_at"] = "2026-09-28T00:00:00"
            (race / "meta.json").write_text(json.dumps(meta))
        err = io.StringIO()
        with contextlib.redirect_stderr(err):
            evicted = candidate.CandidateManager.prune_old_candidates(max_candidates=3)
        self.assertEqual(evicted, 3)
        remaining = sorted(p.name for p in candidates.iterdir())
        # Both applied ones go first even though they are the newest, then the oldest unapplied.
        self.assertEqual(remaining, ["rc_1", "rc_2", "rc_3"])
        self.assertIn("rc_0", err.getvalue())
        self.assertIn("从未应用", err.getvalue())
        self.assertNotIn("rc_4", err.getvalue())


if __name__ == "__main__":
    unittest.main()
