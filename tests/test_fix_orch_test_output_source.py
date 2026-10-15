"""The test gate takes test output from git_helper.is_test_output_path (test-output-source).

orchestrator._filter_non_deliverable_test_changes kept its own copy of the test
output lists, which matched the host-mode delivery check's definition only while a
test pinned both. It now calls the shared definition: test tool configuration
(.coveragerc, pytest.ini, pytest.toml) a test run rewrites is a tested input, and
whatever the shared definition calls test output passes the gate, unless git
tracks it (git_helper.untracked_test_output): a tracked file is a tested input.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import tempfile
import unittest
from unittest.mock import patch

from makewand import git_helper, orchestrator as orch
from makewand.git_helper import run_git_cmd


class TestOutputSourceTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-test-output-source-")
        self.addCleanup(temporary.cleanup)
        self.plain = temporary.name  # neither a repository nor a .gitignore: only test output is dropped

    def test_gate_asks_the_shared_definition(self):
        asked = []

        def shared(rel, root=None):
            asked.append(rel)
            return rel == "results/pytest-junit.xml"

        with patch.object(git_helper, "is_test_output_path", side_effect=shared):
            kept = orch._filter_non_deliverable_test_changes(["results/pytest-junit.xml", "src/app.py"], self.plain)
        self.assertEqual(kept, ["src/app.py"])
        self.assertEqual(sorted(asked), ["results/pytest-junit.xml", "src/app.py"])

    def test_test_tool_configuration_is_a_tested_input(self):
        changed = [".coveragerc", "pytest.ini", "pytest.toml", "src/app.py"]
        self.assertEqual(orch._filter_non_deliverable_test_changes(changed, self.plain), changed)

    def test_gate_and_delivery_agree_on_what_is_test_output(self):
        for rel in (".coveragerc", "pytest.ini", "junit-unit.xml", "reports/junit_results.xml", "HTMLCOV/index.html",
                    "build\\test.tmp", ".hypothesis/examples/1a2b", "coverage.xml", "src/app.py", "README.md"):
            with self.subTest(rel=rel):
                dropped = orch._filter_non_deliverable_test_changes([rel], self.plain) == []
                self.assertEqual(dropped, git_helper.is_test_output_path(rel))

    def test_tracked_report_is_a_tested_input(self):
        repo = Path(self.plain) / "repo"
        (repo / "coverage").mkdir(parents=True)
        (repo / "coverage" / "index.html").write_text("<html/>\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "-A"], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(repo))
            self.assertEqual(code, 0, error)
        changed = ["coverage/index.html", "coverage/app_py.html", "junit-unit.xml"]
        self.assertEqual(orch._filter_non_deliverable_test_changes(changed, str(repo)), ["coverage/index.html"])


if __name__ == "__main__":
    unittest.main()
