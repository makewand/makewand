"""The orchestrator never crashes on undecodable bytes in names, errors or prompts (nonutf8-orchestrator).

A file name that is not valid UTF-8 reaches the orchestrator as surrogate escapes
(os.fsdecode, git output read with surrogateescape): in the review diff when the
user sets core.quotePath=false, in git and OS error messages, in the workspace path
and in a prompt taken from argv. Printing such text to a UTF-8 stream, or sizing it
with str.encode("utf-8"), raised UnicodeEncodeError (e2e3.py nonutf8_reviewed_qp:
orchestrator.py:1652; e2e3tb.py nonutf8_unreadable: fail_and_cleanup). Every message
of the module now shows such bytes as \\xNN, sizes count the byte each escape stands
for, and providers receive the prompt as UTF-8 text. Providers, routing and the test
runner are fakes; stdout is a strict UTF-8 stream.
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
from makewand.execution_contract import EXIT_PASSED, EXIT_UNVERIFIED
from makewand.git_helper import run_git_cmd
from makewand.review_contract import reviewer_receives_whole_prompt

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")
LATIN_NAME = b"lat\xe9n.txt"


def strict_stream():
    return io.TextIOWrapper(io.BytesIO(), encoding="utf-8", errors="strict", write_through=True)


def has_surrogate(text):
    return any(0xD800 <= ord(char) <= 0xDFFF for char in text)


class NonUtf8PipelineTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-nonutf8-r4-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.prompts = []

    def make_repo(self, name=b"repo", quote_path=True):
        repo = Path(os.fsdecode(os.path.join(os.fsencode(str(self.root)), name)))
        repo.mkdir()
        (repo / "app.py").write_text("VALUE = 1\n")
        steps = [["init", "-q"], ["config", "user.name", "Fixture"], ["config", "user.email", "fixture@example.invalid"],
                 ["add", "."], ["commit", "-qm", "base"]]
        if not quote_path:
            steps.append(["config", "core.quotePath", "false"])
        for arguments in steps:
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(repo))
            self.assertEqual(code, 0, error)
        return repo

    @staticmethod
    def write_latin_file(workspace):
        (workspace / "app.py").write_text("VALUE = 2\n")
        with open(os.path.join(os.fsencode(str(workspace)), LATIN_NAME), "wb") as handle:
            handle.write(b"latin\n")

    def run_task(self, repo, prompt="Do the task", isolation=(True, None), coder=None):
        engines = ("claude", "codex")

        def provider(engine):
            def execute(task, **kwargs):
                self.prompts.append((engine, task))
                if kwargs.get("readonly"):
                    return True, f"Reviewed.\n{VERDICT_TAG.search(task).group(0)}: " + '{"pass": true, "defects": []}', None
                (coder or self.write_latin_file)(Path(kwargs["cwd"]))
                return True, "implemented", None
            return execute

        out = strict_stream()
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
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, "1 passed in 0.01s")))
            stack.enter_context(patch.object(orch, "execute_claude_task", side_effect=provider("claude")))
            stack.enter_context(patch.object(orch, "execute_codex_task", side_effect=provider("codex")))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(out))
            result = orch.run_workflow(prompt, cwd=str(repo), force_code=True, auto_fix=False, total_timeout=60)
        return result, out.buffer.getvalue().decode("utf-8")

    def test_reviewed_new_file_with_an_undecodable_name_and_quote_path_off(self):
        """e2e3.py nonutf8_reviewed_qp: the review diff holds the raw name; it used to crash sizing it."""
        for isolation in ((True, None), (False, "dirty working tree")):
            with self.subTest(host_mode=isolation[0]):
                self.prompts = []
                repo = self.make_repo(b"repo-host" if isolation[0] else b"repo-shadow", quote_path=False)
                result, log = self.run_task(repo, isolation=isolation)
                self.assertEqual(result.status, "PASSED", log)
                self.assertEqual(result.exit_code, EXIT_PASSED)
                review = [task for engine, task in self.prompts if engine == "codex"]
                self.assertEqual(len(review), 1)
                self.assertFalse(any(has_surrogate(task) for _, task in self.prompts))
                if isolation[0]:
                    # The user's core.quotePath=false keeps the raw name: shown to the reviewer as \xNN.
                    # (A shadow clone has git's default config, which quotes it.)
                    self.assertIn("lat\\xe9n.txt", review[0])
                    self.assertTrue(os.path.exists(os.path.join(os.fsencode(str(repo)), LATIN_NAME)))

    def test_git_error_naming_an_undecodable_file_is_reported(self):
        """e2e3tb.py nonutf8_unreadable: fail_and_cleanup printed the raw escape."""
        repo = self.make_repo()
        error = OSError("git diff failed with exit code 128: error: open(\"lat\udce9n.txt\"): Permission denied")
        with patch.object(orch, "get_git_diff", side_effect=error):
            result, log = self.run_task(repo)
        self.assertEqual(result.status, "UNVERIFIED", log)
        self.assertEqual(result.exit_code, EXIT_UNVERIFIED)
        self.assertIn('open("lat\\xe9n.txt")', log)
        self.assertIn("lat\\xe9n.txt", result.error)
        self.assertFalse(has_surrogate(result.error))
        self.assertEqual((repo / "app.py").read_text(), "VALUE = 1\n")  # rolled back

    def test_unreadable_file_with_an_undecodable_name_ends_unverified(self):
        """The real git error of e2e3tb.py nonutf8_unreadable (mode 000), end to end."""
        def coder(workspace):
            self.write_latin_file(workspace)
            os.chmod(os.path.join(os.fsencode(str(workspace)), LATIN_NAME), 0)

        repo = self.make_repo()
        try:
            result, log = self.run_task(repo, coder=coder)
        finally:
            target = os.path.join(os.fsencode(str(repo)), LATIN_NAME)
            if os.path.lexists(target):
                os.chmod(target, 0o600)
        if os.geteuid() == 0:
            self.skipTest("root reads mode 000 files")
        self.assertEqual(result.status, "UNVERIFIED", log)
        self.assertIn("lat\\xe9n.txt", log)

    def test_prompt_and_workspace_with_undecodable_bytes(self):
        """A prompt from argv and a workspace path, both with surrogate escapes."""
        repo = self.make_repo(b"repo-\xff")
        result, log = self.run_task(repo, prompt="Add the caf\udce9 module",
                                    coder=lambda workspace: (workspace / "app.py").write_text("VALUE = 2\n"))
        self.assertEqual(result.status, "PASSED", log)
        self.assertIn("caf\\xe9", log)
        self.assertIn("repo-\\xff", log)
        self.assertFalse(any(has_surrogate(task) for _, task in self.prompts))
        self.assertIn("caf\\xe9", self.prompts[0][1])
        self.assertEqual((repo / "app.py").read_text(), "VALUE = 2\n")


class NonUtf8HelperTests(unittest.TestCase):
    def test_module_messages_escape_undecodable_bytes(self):
        out = strict_stream()
        with contextlib.redirect_stdout(out):
            orch.print("name: lat\udce9n.txt", OSError("open(\"b\udcff\")"))
        self.assertEqual(out.buffer.getvalue().decode("utf-8"), 'name: lat\\xe9n.txt open("b\\xff")\n')

    def test_sizes_count_the_byte_each_escape_stands_for(self):
        self.assertEqual(orch._utf8_size("lat\udce9n.txt"), len(b"lat\xe9n.txt"))
        self.assertEqual(orch._utf8_size("数据"), 6)
        self.assertEqual(orch._utf8_size(None), 0)
        self.assertEqual(orch._utf8_size("\ud800"), 3)  # a lone surrogate no file name produced

    def test_whole_prompt_check_measures_the_dispatched_prompt(self):
        limit = orch.reviewer_prompt_limit("agy")
        self.assertTrue(reviewer_receives_whole_prompt("agy", "x" * (limit - 4) + "\udce9"))
        self.assertFalse(reviewer_receives_whole_prompt("agy", "x" * (limit - 3) + "\udce9"))

    def test_unverified_artifacts_keep_a_reason_with_undecodable_bytes(self):
        from makewand.review_verdict import _save_unverified_artifacts
        with tempfile.TemporaryDirectory(prefix="makewand-nonutf8-artifacts-") as temporary:
            repo = Path(temporary) / "repo"
            repo.mkdir()
            (repo / "app.py").write_text("VALUE = 1\n")
            for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                              ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
                self.assertEqual(run_git_cmd(["git", *arguments], cwd=str(repo))[0], 0)
            (repo / "app.py").write_text("VALUE = 2\n")
            base = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(repo))[1].strip()
            patch_path, error = _save_unverified_artifacts(str(repo), base, {}, "review of lat\udce9n.txt",
                                                           "git: cannot hash lat\udce9n.txt")
        self.assertIsNone(error)
        self.assertIn(b"+VALUE = 2", patch_path.read_bytes())
        review = (patch_path.parent / "review.txt").read_text(encoding="utf-8")
        self.assertIn("cannot hash lat\\xe9n.txt", review)
        self.assertIn("review of lat\\xe9n.txt", review)

    def test_providers_receive_utf8_text(self):
        received = []

        def execute(prompt, **kwargs):
            received.append(prompt)
            return True, "ok", None

        with patch.object(orch, "execute_codex_task", side_effect=execute), \
                patch("makewand.config.is_provider_enabled", return_value=True), \
                patch("makewand.usage.record_engine_usage"):
            result = orch.dispatch_task("codex", "review lat\udce9n.txt", cwd=None, timeout=30, readonly=True)
        self.assertTrue(result[0], result[2])
        self.assertEqual(received, ["review lat\\xe9n.txt"])


if __name__ == "__main__":
    unittest.main()
