"""Tests for py-orch-5: tests writing to ignored files or test artifacts pass the test gate."""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.orchestrator as orch
from makewand.git_helper import run_git_cmd


def git(path, *args):
    code, out, error = run_git_cmd(["git", *args], cwd=str(path))
    if code != 0:
        raise AssertionError(f"git command failed: {args}, stdout={out}, stderr={error}")
    return out.strip()


class TestPyOrch5IgnoredTestOutputs(unittest.TestCase):
    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory(prefix="makewand_py_orch_5_")
        self.addCleanup(self.temp_dir.cleanup)
        self.root = Path(self.temp_dir.name)

    def test_filter_non_deliverable_test_changes_ephemeral(self):
        paths = [
            ".coverage",
            "coverage.xml",
            "coverage.json",
            "pytest.log",
            "test.log",
            "test_results.xml",
            ".pytest_cache/v/cache/nodeids",
            ".hypothesis/examples/example.py",
            "htmlcov/index.html",
            "build/test.tmp",
            "app.py",
            "tests/test_main.py",
        ]
        filtered = orch._filter_non_deliverable_test_changes(paths, str(self.root))
        self.assertEqual(sorted(filtered), ["app.py", "tests/test_main.py"])

    def test_filter_non_deliverable_test_changes_git_ignored(self):
        git(self.root, "init")
        git(self.root, "config", "user.name", "Test")
        git(self.root, "config", "user.email", "test@example.invalid")
        (self.root / ".gitignore").write_text("data/\n*.db\ncustom_output.txt\n", encoding="utf-8")
        git(self.root, "add", ".gitignore")
        git(self.root, "commit", "-m", "init")

        paths = [
            "data/fixture.json",
            "test.db",
            "custom_output.txt",
            "src/logic.py",
        ]
        filtered = orch._filter_non_deliverable_test_changes(paths, str(self.root))
        self.assertEqual(filtered, ["src/logic.py"])

    def test_filter_non_deliverable_test_changes_nongit_ignored(self):
        (self.root / ".gitignore").write_text("reports/\n*.sqlite\nlocal_cache.dat\n", encoding="utf-8")
        paths = [
            "reports/summary.txt",
            "test.sqlite",
            "local_cache.dat",
            "server.py",
        ]
        filtered = orch._filter_non_deliverable_test_changes(paths, str(self.root))
        self.assertEqual(filtered, ["server.py"])

    def test_run_local_tests_allows_writing_to_ignored_test_artifacts(self):
        repo = self.root / "repo"
        repo.mkdir()
        git(repo, "init")
        git(repo, "config", "user.name", "Test")
        git(repo, "config", "user.email", "test@example.invalid")

        (repo / ".gitignore").write_text("*.log\ncoverage/\n.coverage\n", encoding="utf-8")
        (repo / "app.py").write_text("def add(a, b): return a + b\n", encoding="utf-8")
        (repo / "test_app.py").write_text(
            "import unittest\nfrom pathlib import Path\nfrom app import add\n"
            "class TestAdd(unittest.TestCase):\n"
            "    def test_add(self):\n"
            "        self.assertEqual(add(1, 2), 3)\n"
            "        Path('test.log').write_text('test execution completed\\n')\n"
            "        cov_dir = Path('coverage')\n"
            "        cov_dir.mkdir(exist_ok=True)\n"
            "        (cov_dir / 'coverage.json').write_text('{\"percent\": 100}\\n')\n"
            "        Path('.coverage').write_text('bytecode_data')\n",
            encoding="utf-8"
        )
        git(repo, "add", ".gitignore", "app.py", "test_app.py")
        git(repo, "commit", "-m", "init")

        ok, details = orch.run_local_tests(str(repo))
        self.assertTrue(ok, details or "")
        self.assertNotIn("测试修改了待交付输入", details or "")

    def test_run_local_tests_rejects_modifying_deliverable_source(self):
        repo = self.root / "repo_bad"
        repo.mkdir()
        git(repo, "init")
        git(repo, "config", "user.name", "Test")
        git(repo, "config", "user.email", "test@example.invalid")

        (repo / "app.py").write_text("def add(a, b): return a + b\n", encoding="utf-8")
        (repo / "test_app.py").write_text(
            "import unittest\nfrom pathlib import Path\n"
            "class TestTamper(unittest.TestCase):\n"
            "    def test_tamper(self):\n"
            "        Path('app.py').write_text('TAMPERED SOURCE')\n",
            encoding="utf-8"
        )
        git(repo, "add", "app.py", "test_app.py")
        git(repo, "commit", "-m", "init")

        ok, details = orch.run_local_tests(str(repo))
        self.assertFalse(ok)
        self.assertIn("测试修改了待交付输入", details or "")
        self.assertIn("app.py", details or "")
