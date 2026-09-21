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

if __name__ == "__main__":
    unittest.main()
