"""In host mode the review diff takes each submodule against its task-start commit,
also from a subdirectory (sub-baselines).

The pipeline built the submodule baselines from cwd/.gitmodules, so a task started
in a subdirectory had none, and the review diff fell back to the commit the
superproject records for the submodule: commits the user had made in the submodule
before the task were shown, and reviewed, as the task's own changes. It now uses
the host transaction's baselines, keyed by root-relative paths like
get_git_diff_status() looks them up. The UNVERIFIED patch export finds submodules
the same way. Providers and routing are fakes; repositories are temporary.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import re
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from makewand import orchestrator as orch
from makewand.execution_contract import EXIT_PASSED
from makewand.git_helper import run_git_cmd
from makewand.review_verdict import _unverified_patch_repos

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")


class SubmoduleBaselineTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-sub-baselines-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        for rel, content in {"pkg/mod.py": "M = 1\n", "app.py": "VALUE = 1\n"}.items():
            (self.repo / rel).parent.mkdir(parents=True, exist_ok=True)
            (self.repo / rel).write_text(content)
        self.git(self.repo, "init", "-q")
        self.identity(self.repo)
        self.git(self.repo, "add", "-A")
        self.git(self.repo, "commit", "-qm", "base")
        source = self.root / "sub-source"
        source.mkdir()
        (source / "lib.py").write_text("L = 1\n")
        self.git(source, "init", "-q")
        self.identity(source)
        self.git(source, "add", "-A")
        self.git(source, "commit", "-qm", "sub base")
        subprocess.run([shutil.which("git"), "-c", "protocol.file.allow=always", "submodule", "-q", "add",
                        str(source), "sub"], cwd=str(self.repo), check=True, capture_output=True)
        self.git(self.repo, "commit", "-qm", "add sub")
        sub = self.repo / "sub"
        self.identity(sub)
        # The user's own work in the submodule before the task: a commit the superproject does not record.
        (sub / "user_work.py").write_text("USER = 1\n")
        self.git(sub, "add", "-A")
        self.git(sub, "commit", "-qm", "user's local commit")
        self.task_start_commit = self.git(sub, "rev-parse", "HEAD").strip()

    def git(self, cwd, *args):
        code, out, error = run_git_cmd(["git", *args], cwd=str(cwd))
        self.assertEqual(code, 0, (args, error))
        return out

    def identity(self, repo):
        self.git(repo, "config", "user.name", "Fixture")
        self.git(repo, "config", "user.email", "fixture@example.invalid")

    def run_task(self, cwd):
        engines = ("claude", "codex")
        prompts = []

        def provider(engine):
            def execute(task, **kwargs):
                prompts.append((engine, bool(kwargs.get("readonly")), task))
                if kwargs.get("readonly"):
                    return True, f"Reviewed.\n{VERDICT_TAG.search(task).group(0)}: " + '{"pass": true, "defects": []}', None
                (self.repo / "pkg/mod.py").write_text("M = 2\n")
                (self.repo / "sub/lib.py").write_text("L = 2\n")
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
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, "1 passed in 0.01s")))
            stack.enter_context(patch.object(orch, "execute_claude_task", side_effect=provider("claude")))
            stack.enter_context(patch.object(orch, "execute_codex_task", side_effect=provider("codex")))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(out))
            result = orch.run_workflow("Bump M and L", cwd=str(cwd), force_code=True, auto_fix=False, total_timeout=60)
        return result, out.getvalue(), [task for _, readonly, task in prompts if readonly]

    def test_review_from_a_subdirectory_shows_only_the_tasks_submodule_change(self):
        for cwd in (self.repo / "pkg", self.repo):
            with self.subTest(cwd=cwd.name):
                result, log, reviews = self.run_task(cwd)
                self.assertEqual(result.status, "PASSED", log)
                self.assertEqual(result.exit_code, EXIT_PASSED)
                self.assertEqual(len(reviews), 1)
                self.assertIn("[Submodule: sub]", reviews[0])
                self.assertIn(f"diff against {self.task_start_commit}", reviews[0])
                self.assertIn("+L = 2", reviews[0])
                self.assertNotIn("user_work.py", reviews[0])  # the user's commit, not the task's
                self.assertEqual((self.repo / "sub/lib.py").read_text(), "L = 2\n")
                self.assertEqual(self.git(self.repo / "sub", "rev-parse", "HEAD").strip(), self.task_start_commit)
                # Ready for the next subtest: the delivered change is kept as the new state.
                (self.repo / "sub/lib.py").write_text("L = 1\n")
                (self.repo / "pkg/mod.py").write_text("M = 1\n")

    def test_unverified_patch_finds_submodules_from_a_subdirectory(self):
        (self.repo / "sub/lib.py").write_text("L = 2\n")
        base = self.git(self.repo, "rev-parse", "HEAD").strip()
        repos = _unverified_patch_repos(str(self.repo / "pkg"), base, {"sub": self.task_start_commit})
        self.assertEqual([(rel, base_rev) for rel, _, base_rev in repos],
                         [("", base), ("sub", self.task_start_commit)])
        self.assertEqual(Path(repos[1][1]).resolve(), (self.repo / "sub").resolve())


if __name__ == "__main__":
    unittest.main()
