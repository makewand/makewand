"""
Unit tests for bubblewrap sandbox bridge.
"""

import os
import unittest
import tempfile
from pathlib import Path
from makewand.sandbox import is_bwrap_available, wrap_bwrap, run_in_sandbox

class TestSandbox(unittest.TestCase):
    def test_bwrap_availability(self):
        self.assertTrue(is_bwrap_available())

    def test_wrap_bwrap_command_structure(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            wrapped = wrap_bwrap(["echo", "hello"], workspace=tmpdir, allow_network=False)
            self.assertIn("--ro-bind", wrapped)
            self.assertIn("--bind", wrapped)
            self.assertIn("--unshare-net", wrapped)
            self.assertIn("echo", wrapped)

    def test_run_in_sandbox_execution(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            ret, out, err, ex = run_in_sandbox(["echo", "sandbox_active"], workspace=tmpdir)
            self.assertEqual(ret, 0)
            self.assertIn("sandbox_active", out)

            # Test write confinement: writing inside workspace succeeds
            test_file = os.path.join(tmpdir, "test.txt")
            ret, _, _, _ = run_in_sandbox(["bash", "-c", f"echo secret > {test_file}"], workspace=tmpdir)
            self.assertEqual(ret, 0)
            self.assertTrue(os.path.exists(test_file))

    def test_general_sandbox_isolates_home_credentials(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            # 1. Verify wrap_bwrap for general code (is_provider=False) uses tmpfs for user_home
            user_home = str(Path.home())
            wrapped_general = wrap_bwrap(["echo", "hi"], workspace=tmpdir, is_provider=False)
            self.assertIn("--tmpfs", wrapped_general)
            # Find where --tmpfs user_home is specified
            tmpfs_indices = [i for i, x in enumerate(wrapped_general) if x == "--tmpfs"]
            tmpfs_targets = [wrapped_general[i + 1] for i in tmpfs_indices if i + 1 < len(wrapped_general)]
            self.assertIn(user_home, tmpfs_targets)

            # 2. Verify wrap_bwrap for provider (is_provider=True) also strictly isolates user_home with tmpfs
            wrapped_provider = wrap_bwrap(["echo", "hi"], workspace=tmpdir, is_provider=True)
            p_tmpfs_indices = [i for i, x in enumerate(wrapped_provider) if x == "--tmpfs"]
            p_tmpfs_targets = [wrapped_provider[i + 1] for i in p_tmpfs_indices if i + 1 < len(wrapped_provider)]
            self.assertIn(user_home, p_tmpfs_targets)

    def test_subdirectory_mounts_full_worktree(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            # Initialize a git repo with a root file and a subpackage
            from makewand.git_helper import run_git_cmd
            run_git_cmd(["git", "init"], cwd=tmpdir)
            run_git_cmd(["git", "config", "user.name", "test"], cwd=tmpdir)
            run_git_cmd(["git", "config", "user.email", "test@test"], cwd=tmpdir)
            root_file = Path(tmpdir) / "root.txt"
            root_file.write_text("root content\n")
            sub_dir = Path(tmpdir) / "pkg"
            sub_dir.mkdir()
            sub_file = sub_dir / "sub.txt"
            sub_file.write_text("sub content\n")
            run_git_cmd(["git", "add", "-A"], cwd=tmpdir)
            run_git_cmd(["git", "commit", "-m", "init"], cwd=tmpdir)

            # Run inside subdirectory
            wrapped = wrap_bwrap(["cat", "../root.txt"], workspace=str(sub_dir))
            # mount_root should be tmpdir
            bind_indices = [i for i, x in enumerate(wrapped) if x in ("--bind", "--ro-bind")]
            mounted_dirs = [wrapped[i + 1] for i in bind_indices if i + 1 < len(wrapped)]
            self.assertIn(os.path.abspath(tmpdir), mounted_dirs)

    def test_sandbox_cargo_credentials_blocked(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            # Test that in general execution, reading ~/.cargo/credentials.toml fails / is blocked
            ret, out, err, ex = run_in_sandbox(["cat", os.path.expanduser("~/.cargo/credentials.toml")], workspace=tmpdir)
            self.assertNotEqual(ret, 0)

    def test_workspace_cannot_expand_sandbox_via_gitdir(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            # Create an external directory with a sentinel file
            victim_dir = Path(tmpdir) / "victim"
            victim_dir.mkdir()
            sentinel = victim_dir / "secret.txt"
            sentinel.write_text("original content\n")

            # Workspace attempts to point .git to victim_dir
            ws = Path(tmpdir) / "ws"
            ws.mkdir()
            (ws / ".git").write_text(f"gitdir: {victim_dir}\n")

            # Run in sandbox attempting to modify the sentinel file
            ret, out, err, _ = run_in_sandbox(["bash", "-c", f"echo hacked > {sentinel}"], workspace=str(ws), repo_root=str(victim_dir))
            # Must fail, and sentinel content must be intact!
            self.assertNotEqual(ret, 0)
            self.assertEqual(sentinel.read_text(), "original content\n")

    def test_core_worktree_cannot_expand_sandbox_writable_mount(self):
        import shutil
        test_base = Path.cwd() / ".test_worktree_defense"
        test_base.mkdir(parents=True, exist_ok=True)
        try:
            from makewand.git_helper import run_git_cmd
            # Create a victim parent dir with a secret file
            parent_dir = test_base / "parent"
            parent_dir.mkdir(parents=True, exist_ok=True)
            secret = parent_dir / "parent_secret.txt"
            secret.write_text("protected parent data\n")

            # Create an untrusted repo in a child dir
            ws = parent_dir / "child_repo"
            ws.mkdir(parents=True, exist_ok=True)
            run_git_cmd(["git", "init"], cwd=str(ws))
            run_git_cmd(["git", "config", "user.name", "test"], cwd=str(ws))
            run_git_cmd(["git", "config", "user.email", "test@test"], cwd=str(ws))
            (ws / "child.txt").write_text("child data\n")
            run_git_cmd(["git", "add", "-A"], cwd=str(ws))
            run_git_cmd(["git", "commit", "-m", "init"], cwd=str(ws))

            # Set core.worktree to parent directory
            run_git_cmd(["git", "config", "core.worktree", str(parent_dir)], cwd=str(ws))

            # Attempt to write to parent_secret.txt inside sandbox
            ret, out, err, _ = run_in_sandbox(["bash", "-c", f"echo overwritten > {secret}"], workspace=str(ws))
            # Must fail with read-only filesystem error, and protected parent data must remain intact!
            self.assertNotEqual(ret, 0)
            self.assertEqual(secret.read_text(), "protected parent data\n")
        finally:
            shutil.rmtree(test_base, ignore_errors=True)

    def test_git_directory_rename_and_tamper_blocked_in_sandbox(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            from makewand.git_helper import run_git_cmd
            ws = Path(tmpdir) / "repo"
            ws.mkdir()
            run_git_cmd(["git", "init"], cwd=str(ws))
            run_git_cmd(["git", "config", "user.name", "test"], cwd=str(ws))
            run_git_cmd(["git", "config", "user.email", "test@test"], cwd=str(ws))
            (ws / "file.txt").write_text("hello\n")
            run_git_cmd(["git", "add", "."], cwd=str(ws))
            run_git_cmd(["git", "commit", "-m", "init"], cwd=str(ws))

            # Attempt to rename .git inside sandbox (must fail due to mount point protection)
            ret_rename, _, _, _ = run_in_sandbox(["python3", "-c", "import os; os.rename('.git', '.git.bak')"], workspace=str(ws), readonly=False)
            self.assertNotEqual(ret_rename, 0)
            self.assertTrue((ws / ".git").is_dir())
            self.assertFalse((ws / ".git.bak").exists())

            # Attempt to modify .git/config inside sandbox (must fail with read-only fs error)
            ret_write, _, _, _ = run_in_sandbox(["bash", "-c", "echo malicious >> .git/config"], workspace=str(ws), readonly=False)
            self.assertNotEqual(ret_write, 0)

if __name__ == "__main__":
    unittest.main()

