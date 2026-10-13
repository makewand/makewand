"""The test gate keeps the user's directories and names test output it cannot remove (empty-dirs).

run_local_tests removes the test output that a test run created (coverage/,
test-results/, junit-unit.xml, ...) with the directories left empty, but it
judged "created" from the pre-run file snapshot only: an empty directory the user
already had - test-results/, reports/junit/ and reports/ - counted as created and
was deleted after a successful run (e2e_t2.py t2_preexisting_empty_dir,
t2_preexisting_empty_nested). The directories that exist before the run are now
recorded and kept. Created test output that could run as code (an executable
bit, a .pth file, a symlink) is never removed; it failed the run only through the
generic note '测试开始至审查快照之间产物发生变化' that named no file. The gate now
names each such file and why, and so does the note. The test runner is real
(pytest in the sandbox); providers are fakes; every repository is temporary.
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
from makewand.artifact import workspace_snapshot
from makewand.git_helper import run_git_cmd

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")
PYTEST_INI = "[pytest]\naddopts = -p no:cacheprovider\n"


def passing_test_source(statements):
    """A passing test that runs statements with b = the directory holding it (the repository root)."""
    body = "\n".join("    " + line for line in statements)
    return ("import os, pathlib\nfrom app import VALUE\n\n\ndef test_value():\n"
            "    b = pathlib.Path(__file__).resolve().parent\n" + body + "\n    assert VALUE in (1, 2)\n")


class GateTestCase(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-final-dirs-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def make_repo(self, statements, name="repo", gitignore="__pycache__/\n"):
        repo = self.root / name
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


class PreExistingDirectoryTests(GateTestCase):
    def test_empty_directory_the_user_had_survives_test_output_written_into_it(self):
        """t2_preexisting_empty_dir: the user's empty test-results/ was gone after exit 0."""
        repo = self.make_repo(["p = b / 'test-results'", "p.mkdir(exist_ok=True)", "(p / 'out.json').write_text('{}')"])
        (repo / "test-results").mkdir()  # untracked and empty: git does not track directories
        ok, details, log = self.run_tests(repo)
        self.assertTrue(ok, details)
        self.assertIn("test-results/out.json", log)
        self.assertFalse((repo / "test-results/out.json").exists())
        self.assertTrue((repo / "test-results").is_dir())

    def test_empty_nested_directories_the_user_had_survive(self):
        """t2_preexisting_empty_nested: the user's empty reports/junit/ and reports/ were gone."""
        repo = self.make_repo(["p = b / 'reports' / 'junit'", "p.mkdir(parents=True, exist_ok=True)",
                               "(p / 'junit-unit.xml').write_text('<t/>')",
                               "q = b / 'coverage' / 'lcov-report'", "q.mkdir(parents=True, exist_ok=True)",
                               "(q / 'index.html').write_text('<html/>')"])
        (repo / "reports" / "junit").mkdir(parents=True)
        ok, details, _ = self.run_tests(repo)
        self.assertTrue(ok, details)
        self.assertFalse((repo / "reports/junit/junit-unit.xml").exists())
        self.assertTrue((repo / "reports/junit").is_dir())
        # Directories the run itself created still go with their files.
        self.assertFalse((repo / "coverage").exists())
        status = run_git_cmd(["git", "status", "--porcelain", "--untracked-files=all"], cwd=str(repo))[1]
        self.assertEqual(status.strip(), "")

    def test_without_a_directory_record_no_directory_is_removed(self):
        repo = self.make_repo([])
        before = workspace_snapshot(str(repo))
        (repo / "test-results" / "a").mkdir(parents=True)
        (repo / "test-results" / "a" / "out.json").write_text("{}")
        changed = ["test-results/a/out.json"]
        removed, rest = orch._discard_created_test_output(str(repo), before, changed)  # held_dirs unknown
        self.assertEqual((removed, rest), (changed, []))
        self.assertTrue((repo / "test-results" / "a").is_dir())
        (repo / "test-results" / "a" / "out.json").write_text("{}")
        removed, _ = orch._discard_created_test_output(str(repo), before, changed, held_dirs=set())
        self.assertEqual(removed, changed)
        self.assertFalse((repo / "test-results").exists())

    def test_directory_record_skips_git_metadata_links_and_ignored_trees(self):
        repo = self.make_repo([], gitignore="__pycache__/\nnode_modules/\n")
        for rel in ("node_modules/pkg/lib", "src/empty", "reports"):
            (repo / rel).mkdir(parents=True)
        (repo / "linked").symlink_to(repo / "src")
        found = orch._existing_directories(str(repo))
        self.assertTrue({"src", "src/empty", "reports"} <= found, found)
        self.assertFalse({"node_modules", "node_modules/pkg", "linked", ".git"} & found, found)
        self.assertFalse(any(rel.startswith(".git/") for rel in found))


class RefusedTestOutputTests(GateTestCase):
    def test_created_output_that_could_run_as_code_is_named_with_its_reason(self):
        repo = self.make_repo(["c = b / 'coverage'", "c.mkdir(exist_ok=True)",
                               "(c / 'run.sh').write_text('#!/bin/sh\\necho hi\\n')", "os.chmod(c / 'run.sh', 0o755)",
                               "h = b / 'htmlcov'", "h.mkdir(exist_ok=True)", "(h / 'evil.pth').write_text('import os')",
                               "t = b / 'test-results'", "t.mkdir(exist_ok=True)", "(t / 'index.html').symlink_to('../app.py')",
                               "(c / 'index.html').write_text('<html/>')"])
        ok, details, log = self.run_tests(repo)
        self.assertFalse(ok)
        for expected in ("coverage/run.sh（带可执行位）", "htmlcov/evil.pth（安全敏感文件名）",
                         "test-results/index.html（符号链接）"):
            self.assertIn(expected, details)
        self.assertIn("不会被自动移除", details)
        # They stay for the review to see; inert output beside them is removed as before.
        self.assertTrue((repo / "coverage/run.sh").exists())
        self.assertTrue(os.path.islink(repo / "test-results/index.html"))
        self.assertFalse((repo / "coverage/index.html").exists())
        self.assertIn("coverage/index.html", log)

    def test_snapshot_note_names_what_changed(self):
        before = {"a.txt": ("file", "1", 0o644), "b.txt": ("file", "2", 0o644)}
        after = {"a.txt": ("file", "1", 0o644), "c/run.sh": ("file", "3", 0o755)}
        self.assertEqual(orch._snapshot_change_note("X", before, after), "X: b.txt, c/run.sh")
        self.assertEqual(orch._snapshot_change_note("X", before, dict(before)), "X。")


class PipelineNamesTheFileTests(GateTestCase):
    def test_run_failed_by_executable_test_output_names_the_file(self):
        """e2e_t2.py t2_exec_report: the run failed with '测试开始至审查快照之间产物发生变化' only."""
        repo = self.make_repo(["c = b / 'coverage'", "c.mkdir(exist_ok=True)",
                               "(c / 'run.sh').write_text('#!/bin/sh\\necho pwn\\n')", "os.chmod(c / 'run.sh', 0o755)"])
        engines = ("claude", "codex")

        def provider(engine):
            def execute(task, **kwargs):
                if kwargs.get("readonly"):
                    return True, f"Reviewed.\n{VERDICT_TAG.search(task).group(0)}: " + '{"pass": true, "defects": []}', None
                (Path(kwargs["cwd"]) / "app.py").write_text("VALUE = 2\n")
                return True, "implemented", None
            return execute

        out = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=(True, None)))
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
        log = out.getvalue()
        self.assertEqual(result.status, "FAILED", log)
        self.assertIn("测试开始至审查快照之间产物发生变化，必须重新测试: coverage/run.sh", log)
        self.assertIn("coverage/run.sh（带可执行位）", log)
        self.assertEqual((repo / "app.py").read_text(), "VALUE = 1\n")  # rolled back
        self.assertFalse((repo / "coverage").exists())


if __name__ == "__main__":
    unittest.main()
