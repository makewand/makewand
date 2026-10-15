"""Undecodable bytes in a workspace path or a new file name no longer break delivery, races or copy errors (nonutf8-rest).

A file name that is not valid UTF-8 reaches makewand as surrogate escapes
(os.fsdecode, git output read with surrogateescape). Three places still failed:

* A shadow-mode run in a workspace whose own path holds such a byte failed at
  delivery: apply_delivery.sh named REPO_ROOT with shlex.quote and was written as
  strict UTF-8 ('影子分支交付发生异常 … surrogates not allowed', e2e_nu.py
  nu_shadow_ws). Paths are now spelt as $'\\xNN' shell words and the script is
  written byte-exact; the script applies the change to that very directory.
* run_race with an undecodable new file ended UNVERIFIED '候选封存完整性检查失败 …
  surrogates not allowed' (race_nonutf8.py): candidate metadata was strict UTF-8
  JSON. It is now escaped JSON that loads back to the same names, and apply
  writes the original bytes.
* clone_isolated_worktree, its copy_inputs and list_workspace_copy_paths raised
  OSError messages embedding git's stderr with raw surrogates; they now show such
  bytes as \\xNN.

Providers, routing and the test runner are fakes; every repository is temporary.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import os
import re
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch

import makewand.config as config
from makewand import git_helper, orchestrator as orch
from makewand.candidate import CandidateManager
from makewand.git_helper import clone_isolated_worktree, list_workspace_copy_paths, run_git_cmd
from makewand.providers.base import get_current_contestant_scope

LATIN = b"lat\xe9n.txt"
VERDICT_TAG = re.compile(r"MAKEWAND_(?:RACE_)?VERDICT_[0-9a-f]+(?=:)")


def has_surrogate(text):
    return any(0xD800 <= ord(char) <= 0xDFFF for char in text)


def strict_stream():
    return io.TextIOWrapper(io.BytesIO(), encoding="utf-8", errors="strict", write_through=True)


def path_of(*parts):
    """A str path from byte components, as os.fsdecode gives it."""
    return os.fsdecode(os.path.join(*(os.fsencode(part) for part in parts)))


def git(root, *arguments):
    code, out, error = run_git_cmd(["git", *arguments], cwd=str(root))
    if code != 0:
        raise AssertionError(f"git {arguments}: {out} {error}")
    return out


class NonUtf8TestCase(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-final-nonutf8-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def make_repo(self, name=b"repo"):
        repo = Path(path_of(str(self.root), name))
        repo.mkdir()
        (repo / "app.py").write_text("VALUE = 1\n")
        (repo / "notes.md").write_text("notes\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"], ["config", "user.email", "fixture@example.invalid"],
                          ["add", "."], ["commit", "-qm", "base"]):
            git(repo, *arguments)
        return repo


class ShadowDeliveryTests(NonUtf8TestCase):
    def test_shadow_delivery_in_a_workspace_with_an_undecodable_path_applies_byte_exact(self):
        repo = self.make_repo(b"repo\xff")
        (repo / "notes.md").write_text("user wip\n")  # a dirty workspace: the task runs in a shadow worktree
        artifacts = Path(path_of(str(self.root), b"artifacts\xfe"))
        engines = ("claude", "codex")

        def provider(engine):
            def execute(task, **kwargs):
                if kwargs.get("readonly"):
                    return True, f"Reviewed.\n{VERDICT_TAG.search(task).group(0)}: " + '{"pass": true, "defects": []}', None
                workspace = Path(kwargs["cwd"])
                (workspace / "app.py").write_text("VALUE = 2\n")
                with open(os.path.join(os.fsencode(str(workspace)), LATIN), "wb") as handle:
                    handle.write(b"latin\n")
                return True, "implemented", None
            return execute

        out = strict_stream()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(config, "ARTIFACTS_DIR", artifacts))
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch("makewand.git_helper.get_active_interactive_working_trees", return_value={}))
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
            result = orch.run_workflow("Do the task", cwd=str(repo), force_code=True, auto_fix=False, total_timeout=120)
        log = out.buffer.getvalue().decode("utf-8")
        self.assertEqual(result.status, "PASSED", log)
        self.assertNotIn("影子分支交付发生异常", log)
        self.assertEqual((repo / "app.py").read_text(), "VALUE = 1\n")  # nothing applied yet: the user applies
        scripts = [os.path.join(current, b"apply_delivery.sh") for current, _, files in os.walk(os.fsencode(str(artifacts)))
                   if b"apply_delivery.sh" in files]
        self.assertEqual(len(scripts), 1)
        with open(scripts[0], "rb") as handle:
            script = handle.read()
        script.decode("utf-8")  # every path is spelt as an escape; the script itself is valid UTF-8
        self.assertIn(b"repo\\xff", script)
        self.assertIn(b"artifacts\\xfe", script)
        applied = subprocess.run([b"bash", scripts[0]], capture_output=True, env=dict(os.environ, PATH="/usr/bin:/bin"))
        self.assertEqual(applied.returncode, 0, applied.stderr.decode("utf-8", "backslashreplace"))
        self.assertEqual((repo / "app.py").read_text(), "VALUE = 2\n")
        with open(os.path.join(os.fsencode(str(repo)), LATIN), "rb") as handle:
            self.assertEqual(handle.read(), b"latin\n")
        self.assertEqual((repo / "notes.md").read_text(), "user wip\n")


class RaceMetadataTests(NonUtf8TestCase):
    def seal(self, base, race_id):
        workspace = config.CANDIDATES_DIR / race_id / "agent_a"
        clone_isolated_worktree(str(base), workspace)
        baseline = git(workspace, "rev-parse", "HEAD").strip()
        (workspace / "app.py").write_text("VALUE = 42\n")
        with open(os.path.join(os.fsencode(str(workspace)), LATIN), "wb") as handle:
            handle.write(b"latin\n")
        return CandidateManager.save_race(race_id=race_id, prompt="nonutf8", base_cwd=str(base),
                                          baseline_commit=git(base, "rev-parse", "HEAD").strip(),
                                          agent_a={"model": "M", "path": str(workspace), "success": True, "test_passed": True,
                                                   "review_passed": True, "baseline_commit": baseline, "diff": "+42"},
                                          agent_b={}, winner="A")

    def test_candidate_with_an_undecodable_file_name_is_sealed_and_applied_byte_exact(self):
        candidates = self.root / "candidates"
        base = self.make_repo()
        with patch.object(config, "CANDIDATES_DIR", candidates):
            race_dir = self.seal(base, "rc_nonutf8a1")
            meta = (race_dir / "meta.json").read_bytes()
            meta.decode("utf-8")
            self.assertIn(b"lat\\udce9n.txt", meta)
            self.assertIn(os.fsdecode(LATIN), CandidateManager.get_race("rc_nonutf8a1")["candidates"]["A"]["changes"])
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                applied, files, detail = CandidateManager.apply_candidate("rc_nonutf8a1", "A")
        self.assertTrue(applied, detail)
        self.assertEqual((base / "app.py").read_text(), "VALUE = 42\n")
        with open(os.path.join(os.fsencode(str(base)), LATIN), "rb") as handle:
            self.assertEqual(handle.read(), b"latin\n")

    def test_metadata_text_round_trips_undecodable_names(self):
        from makewand.candidate import _metadata_text
        import json
        data = {"path": os.fsdecode(b"a\xffb"), "note": "审查"}
        text = _metadata_text(data)
        text.encode("utf-8")
        self.assertEqual(json.loads(text), data)
        self.assertIn("审查", _metadata_text({"note": "审查"}))  # readable when nothing needs escaping

    def test_race_with_an_undecodable_new_file_is_sealed(self):
        """race_nonutf8.py: '候选封存完整性检查失败 … surrogates not allowed' (UNVERIFIED)."""
        import test_workflow_scheduling as scheduling

        class Fixture(scheduling.SchedulingTests):
            def runTest(self):
                pass

        fixture = Fixture()
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        candidates = fixture.root / "candidates"

        def callback(engine, kwargs):
            if kwargs.get("readonly"):
                return None
            workspace = Path(kwargs["cwd"])
            if engine == "claude":
                (workspace / "app.py").write_text("VALUE = 42\n")
                with open(os.path.join(os.fsencode(str(workspace)), LATIN), "wb") as handle:
                    handle.write(b"latin\n")
                return True, "implemented A", None
            scope = get_current_contestant_scope()
            for _ in range(100):  # B finishes only when the winner-take-all cancels it
                if scope and scope.cancel_event.is_set():
                    return False, None, "cancelled by competitor win"
                time.sleep(0.05)
            return False, None, "B did not finish"

        out = strict_stream()
        prompts = []

        def provider(engine):
            def execute(prompt, **kwargs):
                if kwargs.get("readonly"):
                    prompts.append(prompt)
                    tag = VERDICT_TAG.search(prompt).group(0)
                    body = '{"pass": true, "winner": "A", "defects": []}' if "RACE" in tag else '{"pass": true, "defects": []}'
                    return True, f"{tag}: {body}", None
                return callback(engine, kwargs)
            return execute

        with fixture.fixtures(), patch.object(orch, "CANDIDATES_DIR", candidates), \
                patch.object(config, "CANDIDATES_DIR", candidates), \
                patch.object(orch, "execute_claude_task", side_effect=provider("claude")), \
                patch.object(orch, "execute_codex_task", side_effect=provider("codex")), \
                contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
            code = orch.run_race("Implement value 42", cwd=str(fixture.repo), engine_a="claude", engine_b="codex", timeout=60)
        log = out.buffer.getvalue().decode("utf-8")
        self.assertEqual(code, 0, log)
        self.assertNotIn("候选封存完整性检查失败", log)
        self.assertFalse(any(has_surrogate(prompt) for prompt in prompts))
        races = sorted(candidates.glob("rc_*"))
        self.assertEqual(len(races), 1)
        with patch.object(config, "CANDIDATES_DIR", candidates), contextlib.redirect_stdout(io.StringIO()):
            applied, _, detail = CandidateManager.apply_candidate(races[0].name, "A")
        self.assertTrue(applied, detail)
        with open(os.path.join(os.fsencode(str(fixture.repo)), LATIN), "rb") as handle:
            self.assertEqual(handle.read(), b"latin\n")


class CopyErrorMessageTests(NonUtf8TestCase):
    @staticmethod
    def failing(subcommand, stderr=b"fatal: cannot read 'lat\xe9n.txt': \xff\n"):
        real = git_helper.run_git_cmd

        def fake(command, *args, **kwargs):
            if isinstance(command, list) and command[:2] == ["git", subcommand]:
                return 128, b"", stderr
            return real(command, *args, **kwargs)
        return patch.object(git_helper, "run_git_cmd", side_effect=fake)

    def assert_printable(self, error):
        message = str(error)
        self.assertFalse(has_surrogate(message), ascii(message))
        message.encode("utf-8")
        self.assertIn("\\xe9", message)

    def test_listing_error_shows_undecodable_bytes(self):
        repo = self.make_repo()
        with self.failing("ls-files"), self.assertRaises(OSError) as raised:
            list_workspace_copy_paths(str(repo))
        self.assert_printable(raised.exception)

    def test_isolated_copy_errors_show_undecodable_bytes(self):
        repo = self.make_repo()
        with self.failing("ls-files"), self.assertRaises(OSError) as raised:
            clone_isolated_worktree(str(repo), self.root / "copy-a")
        self.assert_printable(raised.exception)
        with self.failing("commit", stderr="fatal: bad \udce9 identity"), self.assertRaises(OSError) as raised:
            clone_isolated_worktree(str(repo), self.root / "copy-b")
        self.assert_printable(raised.exception)


if __name__ == "__main__":
    unittest.main()
