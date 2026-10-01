"""Offline native Windows runtime: generate, test, seal, review and deliver.

Only model responses are fixtures; filesystem, Git, process Jobs, test execution,
review binding and candidate apply/rollback use the real implementations.
"""
try:
    import _isolation
except ImportError:
    from tests import _isolation

import contextlib
import io
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from makewand import config, orchestrator, sandbox
from makewand.candidate import CandidateManager, build_manifest
from makewand.providers.base import run_subprocess


@unittest.skipUnless(os.name == "nt", "requires native Windows runtime")
class NativeWindowsPipelineTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="makewand-windows-flow-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.workspace = self.root / "workspace"
        self.workspace.mkdir()
        config.ensure_config_dir()
        self.config_file = Path(config.CONFIG_DIR) / "config.json"
        previous = self.config_file.read_bytes() if self.config_file.exists() else None
        def restore():
            if previous is None:
                self.config_file.unlink(missing_ok=True)
            else:
                self.config_file.write_bytes(previous)
        self.addCleanup(restore)
        self.config_file.write_text(json.dumps({
            sandbox.ACK_VERSION_KEY: sandbox.UNSAFE_HOST_EXEC_ACK_CURRENT_VERSION,
            sandbox.ACK_HOST_KEY: socket.gethostname(), sandbox.ACK_AT_KEY: "offline-fixture",
        }), encoding="utf-8")
        self.env = mock.patch.dict(os.environ, {"MAKEWAND_UNSAFE_HOST_EXEC": "1"})
        self.env.start()
        self.addCleanup(self.env.stop)
        (self.workspace / "app.py").write_bytes(b"def answer():\n    return 0\n")
        (self.workspace / "test_app.py").write_bytes(
            b"import unittest\nfrom app import answer\nclass TestAnswer(unittest.TestCase):\n"
            b"    def test_answer(self):\n        self.assertEqual(answer(), 42)\n")
        (self.workspace / "protected.txt").write_bytes(b"keep exactly\r\n")

    def git(self, *arguments):
        result = subprocess.run(["git", *arguments], cwd=self.workspace, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def dispatch_fixture(self, *args, cwd=None, readonly=False, **kwargs):
        if readonly:
            verdict = 'MAKEWAND_RACE_VERDICT: {"pass": true, "winner": "A", "defects": []}'
            code = "print(" + repr(verdict) + ")"
        else:
            code = "from pathlib import Path; Path('app.py').write_bytes(b'def answer():\\n    return 42\\n'); print('fixture generation completed')"
        code, output, stderr, error = run_subprocess([sys.executable, "-B", "-c", code], cwd=cwd, timeout=20)
        return code == 0 and error is None, output, error or stderr or None

    def run_fixture_race(self):
        healthy = {name: {"status": "healthy"} for name in ("claude", "codex", "agy")}
        output = io.StringIO()
        with mock.patch("makewand.orchestrator.get_or_update_status", return_value=healthy), \
             mock.patch("makewand.config.is_provider_enabled", return_value=True), \
             mock.patch("makewand.orchestrator.dispatch_task", side_effect=self.dispatch_fixture), \
             mock.patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
             contextlib.redirect_stdout(output):
            result = orchestrator.run_race("修复 answer 并保证测试通过", cwd=str(self.workspace),
                engine_a="claude", engine_b="codex", timeout=90, judge_reserve_seconds=10,
                protected_paths=["protected.txt"])
        self.race_output = output.getvalue()
        return result

    def test_full_generation_test_seal_review_apply_git_and_non_git(self):
        for git_workspace in (False, True):
            with self.subTest(git=git_workspace):
                # Each scenario starts from the same clean preimage. Committing
                # the previous scenario's delivered 42 and resetting afterward
                # would accidentally test a dirty conflicting workspace.
                (self.workspace / "app.py").write_bytes(b"def answer():\n    return 0\n")
                if git_workspace:
                    self.git("init", "-q")
                    self.git("config", "user.name", "Offline Windows fixture")
                    self.git("config", "user.email", "fixture@example.invalid")
                    self.git("config", "core.autocrlf", "false")
                    self.git("add", "-A")
                    self.git("commit", "-qm", "baseline")
                existing = {item["race_id"] for item in CandidateManager.list_races()}
                self.assertEqual(self.run_fixture_race(), orchestrator.EXIT_PASSED, self.race_output)
                races = [item for item in CandidateManager.list_races() if item["race_id"] not in existing]
                self.assertEqual(len(races), 1)
                race_id = races[0]["race_id"]
                self.addCleanup(CandidateManager.discard_race, race_id)
                race = CandidateManager.get_race(race_id)
                candidate = race["candidates"]["A"]
                self.assertIs(candidate["test_passed"], True)
                self.assertIs(candidate["review_passed"], True)
                self.assertTrue(candidate["test_details"])
                self.assertEqual((self.workspace / "app.py").read_bytes(), b"def answer():\n    return 0\n")
                if not git_workspace:
                    self.assertFalse((self.workspace / ".git").exists())
                ok, paths, message = CandidateManager.apply_candidate(race_id, "A", dry_run=True)
                self.assertTrue(ok, message)
                self.assertTrue(paths)
                ok, paths, message = CandidateManager.apply_candidate(race_id, "A")
                self.assertTrue(ok, message)
                self.assertEqual((self.workspace / "app.py").read_bytes(), b"def answer():\n    return 42\n")
                self.assertEqual((self.workspace / "protected.txt").read_bytes(), b"keep exactly\r\n")

    def test_unacknowledged_windows_execution_stays_blocked(self):
        self.config_file.write_text("{}", encoding="utf-8")
        with mock.patch("sys.stdin.isatty", return_value=False):
            code, _, _, _ = sandbox.run_in_sandbox([sys.executable, "-c", "raise SystemExit(0)"],
                workspace=str(self.workspace), timeout=10, readonly=False)
        self.assertNotEqual(code, 0)

    def test_normal_pipeline_exports_verified_native_delivery_for_git_and_non_git(self):
        healthy = {name: {"status": "healthy"} for name in ("claude", "codex", "agy")}
        for git_workspace in (False, True):
            with self.subTest(git=git_workspace):
                (self.workspace / "app.py").write_bytes(b"def answer():\n    return 0\n")
                if git_workspace:
                    self.git("init", "-q")
                    self.git("config", "user.name", "Offline Windows pipeline fixture")
                    self.git("config", "user.email", "fixture@example.invalid")
                    self.git("config", "core.autocrlf", "false")
                    self.git("add", "-A")
                    self.git("commit", "-qm", "baseline")
                observed = []

                def dispatch(*args, cwd=None, readonly=False, **kwargs):
                    observed.append((Path(cwd), readonly))
                    if not readonly:
                        return self.dispatch_fixture(*args, cwd=cwd, readonly=False, **kwargs)
                    verdict = 'MAKEWAND_VERDICT: {"pass": true, "defects": []}'
                    code, out, stderr, error = run_subprocess([sys.executable, "-B", "-c", "print(" + repr(verdict) + ")"],
                                                             cwd=cwd, timeout=20)
                    return code == 0 and error is None, out, error or stderr or None

                artifacts = set(Path(config.ARTIFACTS_DIR).glob("delivery_*/delivery_manifest.json"))
                output = io.StringIO()
                with mock.patch("makewand.orchestrator.get_or_update_status", return_value=healthy), \
                     mock.patch("makewand.config.is_provider_enabled", return_value=True), \
                     mock.patch("makewand.orchestrator.dispatch_task", side_effect=dispatch), \
                     mock.patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
                     contextlib.redirect_stdout(output):
                    ok = orchestrator.run_pipeline("修复 answer 功能并通过现有测试", cwd=str(self.workspace),
                        force_code=True, forced_engine="claude", stream=False, timeout=90,
                        protected_paths=["protected.txt"])
                self.assertTrue(ok, output.getvalue())
                self.assertTrue(observed)
                self.assertTrue(all(path != self.workspace for path, _ in observed))
                self.assertTrue(any(readonly for _, readonly in observed))
                self.assertEqual((self.workspace / "app.py").read_bytes(), b"def answer():\n    return 0\n")
                if not git_workspace:
                    self.assertFalse((self.workspace / ".git").exists())
                manifests = set(Path(config.ARTIFACTS_DIR).glob("delivery_*/delivery_manifest.json")) - artifacts
                self.assertEqual(len(manifests), 1, output.getvalue())
                manifest_path = next(iter(manifests))
                delivery = json.loads(manifest_path.read_text(encoding="utf-8"))
                self.assertRegex(delivery["verified_commit"], r"^[0-9a-f]{40,64}$")
                self.assertRegex(delivery["verified_tree"], r"^[0-9a-f]{40,64}$")
                patch = Path(delivery["main_patch"])
                self.assertTrue(patch.read_bytes())
                race_id = delivery["native_candidate_race_id"]
                self.addCleanup(CandidateManager.discard_race, race_id)
                candidate = CandidateManager.get_race(race_id)["candidates"]["A"]
                self.assertIs(candidate["test_passed"], True)
                self.assertIs(candidate["review_passed"], True)
                self.assertTrue(candidate["test_details"])
                # The archived patch must independently reproduce the sealed
                # payload, including for hosts without a Git repository.
                exported = self.root / ("patch-export-git" if git_workspace else "patch-export-plain")
                shutil.copytree(self.workspace, exported, ignore=shutil.ignore_patterns(".git"))
                for arguments in (("--check", "--binary"), ("--binary",)):
                    # Reviewed Git blobs preserve literal LF/CRLF bytes; Git's
                    # global Windows checkout preference must not alter them.
                    applied = subprocess.run(["git", "-c", "core.autocrlf=false", "apply", *arguments, str(patch)],
                                             cwd=exported, capture_output=True, encoding="utf-8", errors="surrogateescape")
                    self.assertEqual(applied.returncode, 0, applied.stderr)
                self.assertEqual(build_manifest(exported), candidate["manifest"])
                ok, paths, message = CandidateManager.apply_candidate(race_id, "A")
                self.assertTrue(ok, message)
                self.assertTrue(paths)
                self.assertEqual((self.workspace / "app.py").read_bytes(), b"def answer():\n    return 42\n")
                self.assertEqual((self.workspace / "protected.txt").read_bytes(), b"keep exactly\r\n")

    def test_windows_cli_stubs_override_installed_agents(self):
        isolated = _isolation.activate()
        for name in _isolation.STUBBED_CLIS:
            resolved = shutil.which(name)
            self.assertIsNotNone(resolved)
            self.assertEqual(Path(resolved).parent, isolated.bin_dir)
            self.assertEqual(Path(resolved).suffix.lower(), ".cmd")

    def test_pipeline_non_git_always_generates_in_shadow(self):
        healthy = {name: {"status": "healthy"} for name in ("claude", "codex")}
        observed = []
        def fail_generation(*args, cwd=None, **kwargs):
            observed.append(Path(cwd))
            return False, None, "deliberate fixture failure"
        with mock.patch("makewand.orchestrator.check_working_tree_isolation", return_value=(True, "")), \
             mock.patch("makewand.orchestrator.HostWorkspaceTransaction", side_effect=AssertionError("Windows used POSIX host transaction")), \
             mock.patch("makewand.orchestrator.get_or_update_status", return_value=healthy), \
             mock.patch("makewand.config.is_provider_enabled", return_value=True), \
             mock.patch("makewand.orchestrator.dispatch_task", side_effect=fail_generation), \
             mock.patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
             contextlib.redirect_stdout(io.StringIO()):
            self.assertFalse(orchestrator.run_pipeline("修复 answer 功能", cwd=str(self.workspace),
                force_code=True, forced_engine="claude", stream=False, timeout=30))
        self.assertTrue(observed)
        self.assertTrue(all(path != self.workspace for path in observed))
        self.assertFalse((self.workspace / ".git").exists())
        self.assertEqual((self.workspace / "app.py").read_bytes(), b"def answer():\n    return 0\n")


if __name__ == "__main__":
    unittest.main()
