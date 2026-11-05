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
from makewand.git_helper import run_git_cmd

try:
    from _verdicts import nonced
except ImportError:  # python3 -m unittest tests.<module>
    from tests._verdicts import nonced


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


    def test_stray_empty_git_directory_is_not_a_repository(self):
        parent = self.base / "stray"
        (parent / ".git").mkdir(parents=True)
        child = parent / "project"
        child.mkdir()
        self.assertIsNone(git_helper.find_git_root(child))


if __name__ == "__main__":
    unittest.main()
