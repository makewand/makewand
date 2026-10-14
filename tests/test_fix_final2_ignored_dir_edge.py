"""A directory that held only ignored files survives a test run that empties it (ignored-dir-edge).

run_local_tests records the directories that exist before the run
(_existing_directories) so that removing the test output the run created never
removes one of them. The record skipped the trees .gitignore covers, so a
pre-existing directory holding only ignored files - which git lists as one
ignored directory - was not recorded: when the tests deleted those files and
wrote test output next to them, removing the output removed the user's
directory too. Ignored directories are now recorded (their content is still not
walked), within the same snapshot time and entry limits. The test runner is
real (pytest in the sandbox); every repository is temporary.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import os
import tempfile
import unittest
from unittest.mock import patch

from makewand import orchestrator as orch
from makewand.git_helper import run_git_cmd

PYTEST_INI = "[pytest]\naddopts = -p no:cacheprovider\n"


def passing_test_source(statements):
    body = "\n".join("    " + line for line in statements)
    return ("import os, pathlib\nfrom app import VALUE\n\n\ndef test_value():\n"
            "    b = pathlib.Path(__file__).resolve().parent\n" + body + "\n    assert VALUE == 1\n")


class IgnoredDirectoryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-ignored-dir-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def make_repo(self, statements, gitignore):
        repo = self.root / "repo"
        repo.mkdir()
        for rel, content in {"app.py": "VALUE = 1\n", ".gitignore": gitignore, "pytest.ini": PYTEST_INI,
                             "test_app.py": passing_test_source(statements)}.items():
            (repo / rel).write_text(content)
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "-A"], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(repo))
            self.assertEqual(code, 0, error)
        return repo

    def run_tests(self, repo):
        with contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()):
            ok, details = orch.run_local_tests(str(repo), timeout=60)
        return ok, details or "", out.getvalue()

    def test_directory_of_ignored_files_the_tests_empty_survives(self):
        repo = self.make_repo(["p = b / 'test-results'", "(p / 'old.log').unlink()",
                               "(p / 'out.json').write_text('{}')"], gitignore="__pycache__/\n*.log\n")
        (repo / "test-results").mkdir()
        (repo / "test-results" / "old.log").write_text("the user's ignored log\n")
        ok, details, log = self.run_tests(repo)
        self.assertTrue(ok, details)
        self.assertIn("test-results/out.json", log)  # the created test output is removed ...
        self.assertFalse((repo / "test-results" / "out.json").exists())
        self.assertTrue((repo / "test-results").is_dir())  # ... but not the user's directory

    def test_ignored_tree_is_recorded_without_walking_it(self):
        repo = self.make_repo([], gitignore="__pycache__/\nvendor/\n")
        for index in range(30):
            (repo / "vendor" / f"pkg{index}" / "sub").mkdir(parents=True)
        found = orch._existing_directories(str(repo))
        self.assertIn("vendor", found)
        self.assertFalse(any(rel.startswith("vendor/") for rel in found), found)

    def test_the_entry_limit_still_applies(self):
        repo = self.make_repo([], gitignore="__pycache__/\n*.log\n")
        for index in range(12):
            (repo / f"logs{index}").mkdir()
            (repo / f"logs{index}" / "a.log").write_text("x\n")  # each one an ignored directory
        with patch.dict(os.environ, {"MAKEWAND_WORKSPACE_SNAPSHOT_MAX_FILES": "5"}):
            self.assertIsNone(orch._existing_directories(str(repo)))
        self.assertTrue({f"logs{index}" for index in range(12)} <= orch._existing_directories(str(repo)))


if __name__ == "__main__":
    unittest.main()
