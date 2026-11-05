"""
Unit tests for git resilience and shadow tracking.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import os
import uuid
import unittest
import tempfile
import shutil
from pathlib import Path
from makewand.git_helper import get_git_diff

class TestGitHelper(unittest.TestCase):
    def setUp(self):
        self.test_dir = Path(tempfile.mkdtemp(prefix="makewand_git_test_"))

    def tearDown(self):
        shutil.rmtree(self.test_dir, ignore_errors=True)


    def test_submodule_diff_without_explicit_sub_baselines(self):
        from makewand.git_helper import get_git_diff, run_git_cmd
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            main_repo = root / "main"
            child_repo = root / "child"
            main_repo.mkdir()
            child_repo.mkdir()

            run_git_cmd(["git", "init"], cwd=str(main_repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(main_repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(main_repo))
            (main_repo / "main.txt").write_text("main")
            run_git_cmd(["git", "add", "-A"], cwd=str(main_repo))
            run_git_cmd(["git", "commit", "-m", "init main"], cwd=str(main_repo))

            run_git_cmd(["git", "init"], cwd=str(child_repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(child_repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(child_repo))
            (child_repo / "sub.txt").write_text("sub v1")
            run_git_cmd(["git", "add", "-A"], cwd=str(child_repo))
            run_git_cmd(["git", "commit", "-m", "init child"], cwd=str(child_repo))

            run_git_cmd(["git", "-c", "protocol.file.allow=always", "submodule", "add", str(child_repo.resolve()), "child_sub"], cwd=str(main_repo))
            run_git_cmd(["git", "commit", "-m", "add submodule"], cwd=str(main_repo))

            # Commit a change inside child_sub
            sub_in_main = main_repo / "child_sub"
            (sub_in_main / "sub.txt").write_text("sub v2 committed in child")
            run_git_cmd(["git", "add", "-A"], cwd=str(sub_in_main))
            run_git_cmd(["git", "commit", "-m", "commit inside child"], cwd=str(sub_in_main))

            # get_git_diff with sub_baselines=None (simulating independent review)
            diff_out = get_git_diff(str(main_repo), sub_baselines=None)
            self.assertIn("sub v2 committed in child", diff_out)

    def test_git_info_attributes_shielded_in_linked_worktree(self):
        from makewand.git_helper import run_git_cmd
        with tempfile.TemporaryDirectory(prefix="makewand_git_attr_") as td:
            root = Path(td)
            repo = root / "repo"
            linked = root / "linked"
            marker = root / "clean_marker"
            repo.mkdir()

            run_git_cmd(["git", "init"], cwd=str(repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(repo))
            (repo / "f.txt").write_text("base")
            run_git_cmd(["git", "add", "f.txt"], cwd=str(repo))
            run_git_cmd(["git", "commit", "-m", "init"], cwd=str(repo))

            run_git_cmd(["git", "worktree", "add", "-b", "linked-br", str(linked)], cwd=str(repo))

            # Configure harmful clean filter in main repo
            run_git_cmd(["git", "config", "filter.harmful.clean", f"cat; printf leaked > {marker}"], cwd=str(repo))
            (repo / ".git" / "info" / "attributes").write_text("*.txt filter=harmful\n")

            (linked / "f.txt").write_text("modified")
            rc, out, err = run_git_cmd(["git", "add", "-A"], cwd=str(linked))
            self.assertFalse(marker.exists(), "host clean filter must not execute in linked worktree!")


    def test_git_info_attributes_shielded_concurrently(self):
        import concurrent.futures
        from makewand.git_helper import run_git_cmd
        with tempfile.TemporaryDirectory() as td:
            repo = Path(td) / "repo"
            marker = Path(td) / "clean_marker"
            repo.mkdir()

            run_git_cmd(["git", "init"], cwd=str(repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(repo))
            (repo / "f.txt").write_text("base")
            run_git_cmd(["git", "add", "f.txt"], cwd=str(repo))
            run_git_cmd(["git", "commit", "-m", "init"], cwd=str(repo))

            run_git_cmd(["git", "config", "filter.harmful.clean", f"cat; printf leaked > {marker}"], cwd=str(repo))
            attr_file = repo / ".git" / "info" / "attributes"
            attr_content = "*.txt filter=harmful\n"
            attr_file.write_text(attr_content)

            def worker(idx):
                # Run git status, diff, or rev-parse
                cmd = ["git", "status"] if idx % 2 == 0 else ["git", "diff"]
                code, out, err = run_git_cmd(cmd, cwd=str(repo))
                return code, err

            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as executor:
                futures = [executor.submit(worker, i) for i in range(16)]
                for f in concurrent.futures.as_completed(futures):
                    code, err = f.result()
                    self.assertEqual(code, 0, f"Git command failed: {err}")

            self.assertFalse(marker.exists(), "host clean filter executed during concurrent operations!")
            self.assertTrue(attr_file.exists(), "attributes file was deleted or not restored!")
            self.assertEqual(attr_file.read_text(), attr_content)

    def test_git_info_attributes_orphan_self_healing(self):
        from makewand.git_helper import run_git_cmd
        with tempfile.TemporaryDirectory() as td:
            repo = Path(td) / "repo"
            repo.mkdir()
            run_git_cmd(["git", "init"], cwd=str(repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(repo))

            info_dir = repo / ".git" / "info"
            info_dir.mkdir(parents=True, exist_ok=True)
            orphan = info_dir / "attributes.makewand_shield_99999_0"
            content = "*.dat filter=special\n"
            orphan.write_text(content)

            code, out, err = run_git_cmd(["git", "status"], cwd=str(repo))
            self.assertEqual(code, 0, f"Git status failed: {err}")

            attr_file = info_dir / "attributes"
            self.assertTrue(attr_file.exists(), "attributes file was not self-healed from orphan!")
            self.assertEqual(attr_file.read_text(), content)
            self.assertFalse(orphan.exists(), "orphan shield file was not cleaned up!")

    def test_git_info_attributes_worktree_deduplication(self):
        from makewand.git_helper import run_git_cmd
        with tempfile.TemporaryDirectory() as td:
            main_repo = Path(td) / "main"
            wt_repo = Path(td) / "worktree"
            main_repo.mkdir()

            run_git_cmd(["git", "init"], cwd=str(main_repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(main_repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(main_repo))
            (main_repo / "f.txt").write_text("base")
            run_git_cmd(["git", "add", "f.txt"], cwd=str(main_repo))
            run_git_cmd(["git", "commit", "-m", "init"], cwd=str(main_repo))

            run_git_cmd(["git", "worktree", "add", str(wt_repo), "-b", "wt-branch"], cwd=str(main_repo))

            main_info = main_repo / ".git" / "info"
            main_info.mkdir(parents=True, exist_ok=True)
            main_attr = main_info / "attributes"
            content = "*.txt -text\n"
            main_attr.write_text(content)

            # In linked worktree, run_git_cmd must succeed without deadlocks or errors
            code, out, err = run_git_cmd(["git", "status"], cwd=str(wt_repo))
            self.assertEqual(code, 0, f"worktree git status failed: {err}")
            self.assertTrue(main_attr.exists())
            self.assertEqual(main_attr.read_text(), content)


    def test_sanitize_git_env(self):
        from makewand.git_helper import _sanitize_git_env
        input_env = {
            "PATH": "/usr/bin:/bin",
            "USER": "tester",
            "GIT_DIR": "/evil/dir",
            "GIT_WORK_TREE": "/evil/wt",
            "GIT_INDEX_FILE": "/evil/index",
            "GIT_COMMON_DIR": "/evil/common",
            "GIT_HOOKS_PATH": "/evil/hooks",
            "GIT_EXEC_PATH": "/evil/libexec",
            "GIT_EXTERNAL_DIFF": "/evil/diff",
            "GIT_DIFF_OPTS": "-u",
            "GIT_PAGER": "cat",
            "GIT_EDITOR": "/evil/editor",
            "GIT_SEQUENCE_EDITOR": "/evil/seq_editor",
            "GIT_SSH": "evil_ssh",
            "GIT_SSH_COMMAND": "evil_ssh_cmd",
            "GIT_ASKPASS": "evil_askpass",
            "GIT_CONFIG_COUNT": "1",
            "GIT_CONFIG_KEY_0": "core.fsmonitor",
            "GIT_CONFIG_VALUE_0": "evil.sh",
            "GIT_OPTIONAL_LOCKS": "1",
        }
        sanitized = _sanitize_git_env(input_env)
        self.assertEqual(sanitized["PATH"], "/usr/bin:/bin")
        self.assertEqual(sanitized["USER"], "tester")
        self.assertEqual(sanitized["GIT_OPTIONAL_LOCKS"], "0")
        for k in input_env:
            if k not in ("PATH", "USER", "GIT_OPTIONAL_LOCKS"):
                self.assertNotIn(k, sanitized, f"Dangerous var {k} should have been filtered")


    def test_find_git_root_refuses_home_and_root(self):
        """Regression test for [P1] codex3-multihome-and-home-escape: find_git_root stops at HOME and root."""
        from makewand.git_helper import find_git_root
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as td:
            fake_home = Path(td) / "home"
            fake_home.mkdir()
            (fake_home / ".git").mkdir()
            project_dir = fake_home / "Downloads" / "unpacked_project"
            project_dir.mkdir(parents=True)
            (project_dir / "file.txt").write_text("code")

            with patch("pathlib.Path.home", return_value=fake_home):
                # find_git_root from project_dir must NOT return fake_home
                root = find_git_root(project_dir)
                self.assertIsNone(root)
                # find_git_root for fake_home itself must be refused
                self.assertIsNone(find_git_root(fake_home))


if __name__ == "__main__":
    unittest.main()
