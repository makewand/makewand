"""Test output a test run creates neither fails the pipeline nor gets delivered (test-output-pipeline).

run_local_tests already let test output (git_helper.is_test_output_path) pass its
own check, but the pipeline compares the workspace before and after the test run
and failed the test stage on any non-ignored file a test wrote ('测试开始至审查快照
之间产物发生变化'), so a coverage report or a JUnit file at its default location
ended every run with exit 10 (e2e t_cov_dir, t_junit_named), and had it passed, the
report would have been reviewed and delivered as part of the change. Test output
the run itself created is now removed again by run_local_tests, exactly what the
host-mode delivery check would drop unreviewed (_is_inert_test_artifact). The test
runner is real (pytest in the sandbox); providers and routing are fakes; every
repository is temporary.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import os
import re
import tempfile
import unittest
from unittest.mock import patch

from makewand import orchestrator as orch
from makewand.execution_contract import EXIT_PASSED
from makewand.git_helper import run_git_cmd

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")
PYTEST_INI = "[pytest]\naddopts = -p no:cacheprovider\n"


def writing_test_module(writes, executable=()):
    """A passing test that writes each (path, content) next to itself, chmod +x for executable."""
    lines = ["import os, pathlib", "from app import VALUE", "", "def test_value():",
             "    root = pathlib.Path(__file__).resolve().parent"]
    for rel, content in writes:
        lines += [f"    p = root / {rel!r}", "    p.parent.mkdir(parents=True, exist_ok=True)", f"    p.write_text({content!r})"]
        if rel in executable:
            lines.append("    os.chmod(p, 0o755)")
    lines.append("    assert VALUE in (1, 2)")
    return "\n".join(lines) + "\n"


class TestOutputTestCase(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-test-output-r4-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def make_repo(self, writes, executable=(), extra=None, name="repo"):
        repo = self.root / name
        repo.mkdir()
        files = {"app.py": "VALUE = 1\n", ".gitignore": "__pycache__/\n", "pytest.ini": PYTEST_INI,
                 "test_app.py": writing_test_module(writes, executable)}
        files.update(extra or {})
        for rel, content in files.items():
            (repo / rel).parent.mkdir(parents=True, exist_ok=True)
            (repo / rel).write_text(content)
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "-A"], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(repo))
            self.assertEqual(code, 0, error)
        return repo

    def run_tests(self, repo):
        with contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()):
            ok, details = orch.run_local_tests(str(repo), timeout=60)
        return ok, details, out.getvalue()


class RunLocalTestsTests(TestOutputTestCase):
    def test_test_output_the_run_created_is_removed(self):
        repo = self.make_repo([("coverage/index.html", "<html/>\n"), ("junit-unit.xml", "<testsuite/>\n"),
                               ("HTMLCOV/index.html", "<html/>\n"), ("reports/junit_results.xml", "<testsuite/>\n")])
        ok, details, log = self.run_tests(repo)
        self.assertTrue(ok, details)
        for rel in ("coverage/index.html", "junit-unit.xml", "HTMLCOV/index.html", "reports/junit_results.xml"):
            self.assertFalse((repo / rel).exists(), rel)
            self.assertIn(rel, log)
        # Report directories the run created are gone with their files.
        for directory in ("coverage", "HTMLCOV", "reports"):
            self.assertFalse((repo / directory).exists(), directory)
        status = run_git_cmd(["git", "status", "--porcelain", "--untracked-files=all"], cwd=str(repo))[1]
        self.assertEqual(status.strip(), "")

    def test_directory_that_held_inputs_stays(self):
        repo = self.make_repo([("reports/junit.xml", "<testsuite/>\n")], extra={"reports/README.md": "reports\n"})
        ok, details, _ = self.run_tests(repo)
        self.assertTrue(ok, details)
        self.assertFalse((repo / "reports/junit.xml").exists())
        self.assertEqual((repo / "reports/README.md").read_text(), "reports\n")

    def test_test_output_that_could_run_stays_and_tested_inputs_still_fail(self):
        repo = self.make_repo([("test-results/rerun.sh", "#!/bin/sh\n"), ("coverage/conftest.py", "import os\n")],
                              executable=("test-results/rerun.sh",))
        ok, details, _ = self.run_tests(repo)
        self.assertFalse(ok)
        self.assertIn("coverage/conftest.py", details)  # Python source is a tested input
        self.assertTrue((repo / "test-results/rerun.sh").exists())  # executable: never dropped unreviewed
        self.assertTrue((repo / "coverage/conftest.py").exists())

    def test_tracked_report_the_run_rewrites_stays_and_is_a_tested_input(self):
        """Only what the run created goes; a file git tracks is an input whatever its name says."""
        repo = self.make_repo([("coverage/index.html", "<html>new</html>\n")],
                              extra={"coverage/index.html": "<html>old</html>\n"})
        ok, details, _ = self.run_tests(repo)
        self.assertFalse(ok)
        self.assertIn("coverage/index.html", details)
        self.assertEqual((repo / "coverage/index.html").read_text(), "<html>new</html>\n")

    def test_pre_existing_test_output_the_run_rewrites_stays(self):
        """A report an earlier run left behind, which git does not track, is the user's file: the
        run may rewrite it (test output, not a tested input) and it is never removed."""
        for gitignore, rel in (("__pycache__/\ncoverage/\n", "coverage/index.html"),  # ignored, as reports usually are
                               ("__pycache__/\n", "junit-unit.xml")):                  # untracked and not ignored
            with self.subTest(rel=rel):
                repo = self.make_repo([(rel, "<report>new</report>\n")], extra={".gitignore": gitignore},
                                      name="repo-" + rel.split("/")[0])
                (repo / rel).parent.mkdir(parents=True, exist_ok=True)
                (repo / rel).write_text("<report>old</report>\n")  # after the commit: never tracked
                ok, details, log = self.run_tests(repo)
                self.assertTrue(ok, details)
                self.assertEqual((repo / rel).read_text(), "<report>new</report>\n")
                self.assertNotIn(rel, log)  # nothing removed


class PipelineTestOutputTests(TestOutputTestCase):
    def run_task(self, repo, isolation):
        engines = ("claude", "codex")
        prompts = []

        def provider(engine):
            def execute(task, **kwargs):
                prompts.append((engine, bool(kwargs.get("readonly")), task))
                if kwargs.get("readonly"):
                    return True, f"Reviewed.\n{VERDICT_TAG.search(task).group(0)}: " + '{"pass": true, "defects": []}', None
                (Path(kwargs["cwd"]) / "app.py").write_text("VALUE = 2\n")
                return True, "implemented", None
            return execute

        out = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=isolation))
            stack.enter_context(patch("makewand.sandbox.verify_writable_sandbox_or_authorized", return_value=(True, None, None)))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={e: {"status": "healthy"} for e in engines}))
            stack.enter_context(patch.object(orch, "select_optimal_engine_pair", return_value=(
                list(engines), ["codex", "claude"], dict(primary_coder="claude", primary_reviewer="codex", reasons=[]))))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=lambda name: name in engines))
            stack.enter_context(patch("makewand.config.get_active_providers", return_value=list(engines)))
            stack.enter_context(patch("makewand.usage.record_engine_usage"))
            stack.enter_context(patch.object(orch, "execute_claude_task", side_effect=provider("claude")))
            stack.enter_context(patch.object(orch, "execute_codex_task", side_effect=provider("codex")))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(out))
            result = orch.run_workflow("Set VALUE to 2", cwd=str(repo), force_code=True, auto_fix=False, total_timeout=120)
        return result, out.getvalue(), prompts

    def test_reports_written_by_the_tests_are_neither_a_failure_nor_delivered(self):
        """e2e t_cov_dir / t_junit_named: exit 10 before, the report never reaches the review or delivery."""
        writes = [("coverage/index.html", "<html>report</html>\n"), ("junit-unit.xml", "<testsuite/>\n")]
        for isolation in ((True, None), (False, "dirty working tree")):
            with self.subTest(host_mode=isolation[0]):
                repo = self.make_repo(writes, name="repo-host" if isolation[0] else "repo-shadow")
                result, log, prompts = self.run_task(repo, isolation)
                self.assertEqual(result.status, "PASSED", log)
                self.assertEqual(result.exit_code, EXIT_PASSED)
                review = [task for engine, readonly, task in prompts if readonly]
                self.assertEqual(len(review), 1)
                self.assertIn("app.py", review[0])
                self.assertNotIn("coverage/index.html", review[0])
                self.assertNotIn("junit-unit.xml", review[0])
                self.assertFalse((repo / "coverage").exists())
                self.assertFalse((repo / "junit-unit.xml").exists())
                if isolation[0]:
                    self.assertEqual((repo / "app.py").read_text(), "VALUE = 2\n")
                else:
                    branches = run_git_cmd(["git", "for-each-ref", "--format=%(refname)", "refs/heads/makewand/"],
                                           cwd=str(repo))[1].split()
                    self.assertEqual(len(branches), 1, log)
                    tree = run_git_cmd(["git", "ls-tree", "-r", "--name-only", branches[0]], cwd=str(repo))[1].split()
                    self.assertIn("app.py", tree)
                    self.assertNotIn("junit-unit.xml", tree)
                    self.assertFalse(any(name.startswith("coverage/") for name in tree), tree)

    def test_a_tested_input_the_tests_rewrite_still_fails_the_run(self):
        repo = self.make_repo([("pytest.ini", "[pytest]\n")])
        result, log, _ = self.run_task(repo, (True, None))
        self.assertEqual(result.status, "FAILED", log)
        self.assertIn("pytest.ini", log)
        self.assertEqual((repo / "app.py").read_text(), "VALUE = 1\n")


if __name__ == "__main__":
    unittest.main()
