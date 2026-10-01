"""Real Git regressions for corrupt refs and non-mutating diff inspection."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import git_helper as git
from makewand.candidate import get_candidate_files_changed


class GitDiffFailClosedTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.command("init", "-q")
        self.command("config", "user.name", "Fixture")
        self.command("config", "user.email", "fixture@example.invalid")

    def command(self, *arguments):
        code, output, error = git.run_git_cmd(["git", *arguments], cwd=str(self.root))
        self.assertEqual(code, 0, error)
        return output.strip()

    def baseline(self):
        (self.root / "app.py").write_text("VALUE = 1\n")
        self.command("add", "-A")
        self.command("commit", "-qm", "baseline")

    def test_unborn_branch_diff_preserves_absent_real_index(self):
        (self.root / "new.py").write_text("VALUE = 1\n")
        diff, error = git.get_git_diff_status(str(self.root))
        self.assertIsNone(error)
        self.assertIn("new.py", diff)
        self.assertFalse((self.root / ".git/index").exists())

    def test_diff_includes_untracked_without_changing_index_or_lock(self):
        self.baseline()
        index = self.root / ".git/index"
        before = index.read_bytes()
        lock = self.root / ".git/index.lock"
        lock.write_text("owned by another writer")
        (self.root / "new.py").write_text("VALUE = 2\n")
        diff, error = git.get_git_diff_status(str(self.root))
        self.assertIsNone(error)
        self.assertIn("new.py", diff)
        self.assertEqual(index.read_bytes(), before)
        self.assertEqual(lock.read_text(), "owned by another writer")

    def test_missing_head_object_is_not_an_unborn_branch(self):
        self.baseline()
        branch = self.command("symbolic-ref", "HEAD")
        (self.root / ".git" / branch).write_text("1" * 40 + "\n")
        diff, error = git.get_git_diff_status(str(self.root))
        self.assertFalse(diff)
        self.assertIsNotNone(error)
        with self.assertRaises(OSError):
            git.get_git_diff(str(self.root))

    def test_candidate_plan_expands_untracked_directories_to_files(self):
        self.baseline()
        index = self.root / ".git/index"
        before = index.read_bytes()
        new_dir = self.root / "new package" / "nested"
        new_dir.mkdir(parents=True)
        (new_dir / "module.py").write_text("VALUE = 2\n")
        changes = get_candidate_files_changed(self.root)
        self.assertEqual(changes, {"new package/nested/module.py": "A"})
        self.assertEqual(index.read_bytes(), before)

    def test_failed_diff_never_falls_back_to_empty_success(self):
        self.baseline()
        original = git.run_git_cmd
        calls = []
        def intercepted(command, **kwargs):
            if "diff" in command:
                calls.append(command)
                return 128, "", "fixture diff failure"
            return original(command, **kwargs)
        with patch.object(git, "run_git_cmd", side_effect=intercepted):
            diff, error = git.get_git_diff_status(str(self.root))
        self.assertFalse(diff)
        self.assertIn("fixture diff failure", error)
        self.assertEqual(len(calls), 1)

    def test_candidate_plan_checks_failed_intent_to_add(self):
        self.baseline()
        original = git.run_git_cmd
        def intercepted(command, **kwargs):
            if "--intent-to-add" in command:
                return 128, "", "fixture add failure"
            return original(command, **kwargs)
        with patch.object(git, "run_git_cmd", side_effect=intercepted):
            with self.assertRaisesRegex(OSError, "fixture add failure"):
                get_candidate_files_changed(self.root)

    def test_failed_attribute_shield_cannot_start_host_git(self):
        attributes = self.root / ".git/info/attributes"
        attributes.write_text("*.py filter=unsafe\n")
        with patch.object(Path, "rename", side_effect=PermissionError("fixture denied")), \
             patch.object(git.subprocess, "run") as command:
            code, _, error = git.run_git_cmd(["git", "diff"], cwd=str(self.root))
        self.assertNotEqual(code, 0)
        self.assertIn("Cannot shield Git attributes", error)
        command.assert_not_called()


if __name__ == "__main__":
    unittest.main()
