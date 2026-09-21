"""
Unit tests for git resilience and shadow tracking.
"""

import unittest
import tempfile
import shutil
from pathlib import Path
from makewand.git_helper import ensure_git_worktree, get_git_diff

class TestGitHelper(unittest.TestCase):
    def setUp(self):
        self.test_dir = Path(tempfile.mkdtemp(prefix="makewand_git_test_"))

    def tearDown(self):
        shutil.rmtree(self.test_dir, ignore_errors=True)

    def test_ensure_git_worktree_non_git(self):
        # Fresh non-git directory
        self.assertFalse((self.test_dir / ".git").exists())
        inited = ensure_git_worktree(str(self.test_dir))
        self.assertTrue(inited)
        self.assertTrue((self.test_dir / ".git").exists())

    def test_get_git_diff(self):
        ensure_git_worktree(str(self.test_dir))
        sample_file = self.test_dir / "sample.py"
        sample_file.write_text("print('hello world')\n", encoding="utf-8")
        diff = get_git_diff(str(self.test_dir))
        self.assertIn("sample.py", diff)
        self.assertIn("hello world", diff)

    def test_get_git_diff_deletion(self):
        sample_file = self.test_dir / "original.py"
        sample_file.write_text("print('initial')\n", encoding="utf-8")
        ensure_git_worktree(str(self.test_dir))
        sample_file.unlink()
        diff = get_git_diff(str(self.test_dir))
        self.assertIn("deleted file mode", diff)
        self.assertIn("original.py", diff)

    def test_root_dir_protection(self):
        # Root and /tmp should not be initialized
        self.assertFalse(ensure_git_worktree("/"))
        self.assertFalse(ensure_git_worktree("/tmp"))

    def test_check_working_tree_isolation(self):
        from unittest.mock import patch
        from makewand.git_helper import check_working_tree_isolation

        fake_active = {
            "/path/to/workspace/dev/sample_project_3": {
                "source": "external_terminal",
                "tty": "pts/36",
                "pid": 12345,
                "ai_type": "codex",
                "cwd": "/path/to/workspace/dev/sample_project_3"
            }
        }
        with patch("makewand.git_helper.get_active_interactive_working_trees", return_value=fake_active):
            # Target is conflicting with active session
            safe, msg = check_working_tree_isolation("/path/to/workspace/dev/sample_project_3")
            self.assertFalse(safe)
            self.assertIn("pts/36", msg)

            # Target is safe
            safe, msg = check_working_tree_isolation("/tmp/safe_isolated_dir")
            self.assertTrue(safe)
            self.assertIsNone(msg)

if __name__ == "__main__":
    unittest.main()
