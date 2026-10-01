"""Probe policy regressions using harmless local programs, never model CLIs."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import tempfile
import unittest
from unittest.mock import patch

from makewand import cli, health, execution_runtime


class ProbePolicyRegressions(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.repo = self.root / "untrusted-repo"
        self.repo.mkdir()
        self.ledger = self.root / "calls.json"

    def invoke(self, arguments):
        previous = os.getcwd()
        try:
            with patch.object(sys, "argv", ["makewand", *arguments]), \
                    patch.object(sys, "stdin", io.StringIO()), \
                    contextlib.redirect_stdout(io.StringIO()) as output, \
                    contextlib.redirect_stderr(io.StringIO()):
                try:
                    cli.main()
                    return 0, output.getvalue()
                except SystemExit as error:
                    return error.code, output.getvalue()
        finally:
            os.chdir(previous)

    def test_all_live_cli_aliases_reject_untrusted_before_any_process_or_probe(self):
        commands = (("probe",), ("status", "--probe", "--json"), ("quota", "--probe"))
        for arguments in commands:
            for before in (True, False):
                flags = ("--repo-trust", "untrusted", "-C", str(self.repo))
                argv = (*flags, *arguments) if before else (*arguments, *flags)
                with self.subTest(argv=argv), patch.dict(os.environ, {"MAKEWAND_CALL_BUDGET_FILE": str(self.ledger), "MAKEWAND_MAX_MODEL_CALLS": "1"}), \
                        patch.object(health, "run_subprocess") as process, patch.object(health, "probe_model") as probe:
                    code, output = self.invoke(argv)
                    self.assertEqual(code, 11)
                    if "--json" in arguments:
                        self.assertEqual(json.loads(output)["status"], "UNVERIFIED")
                    process.assert_not_called()
                    probe.assert_not_called()
        self.assertFalse(self.ledger.exists())

    def test_cached_untrusted_status_is_allowed_and_policy_is_preserved(self):
        with patch.object(cli, "get_or_update_status", return_value={}) as status:
            code, _ = self.invoke(["status", "--json", "--repo-trust", "untrusted", "-C", str(self.repo)])
        self.assertEqual(code, 0)
        self.assertEqual(status.call_args.kwargs, dict(force_probe=False, repo_trust="untrusted", cwd=str(self.repo)))

    def test_trusted_live_alias_preserves_explicit_policy_and_directory(self):
        with patch.object(cli, "get_or_update_status", return_value={}) as status:
            code, _ = self.invoke(["status", "--probe", "--json", "--repo-trust", "trusted", "-C", str(self.repo)])
        self.assertEqual(code, 0)
        self.assertEqual(status.call_args.kwargs, dict(force_probe=True, repo_trust="trusted", cwd=str(self.repo)))

    def test_internal_untrusted_echo_is_unverified_without_admission(self):
        with patch.dict(os.environ, {"MAKEWAND_CALL_BUDGET_FILE": str(self.ledger), "MAKEWAND_MAX_MODEL_CALLS": "1"}), \
                patch.object(health, "run_subprocess") as process:
            raw = health._run_model_probe("codex", [sys.executable, "-c", "print('not reached')"], 1,
                                          repo_trust="untrusted", cwd=str(self.repo))
        self.assertEqual(raw[3].execution_status, "UNVERIFIED")
        process.assert_not_called()
        self.assertFalse(self.ledger.exists())

    def test_actual_harmless_echo_uses_neutral_cwd_and_typed_trusted_request(self):
        script = self.root / "harmless_echo.py"
        script.write_text("import os, json; print(json.dumps({'cwd': os.getcwd()}))\n")
        with patch.dict(os.environ, {"MAKEWAND_CALL_BUDGET_FILE": str(self.ledger), "MAKEWAND_MAX_MODEL_CALLS": "1"}), \
                patch.object(execution_runtime, "execute", wraps=execution_runtime.execute) as execute:
            raw = health._run_model_probe("codex", [sys.executable, "-I", str(script)], 2,
                                          repo_trust="trusted", cwd=str(self.repo))
        self.assertEqual(raw[0], 0)
        self.assertNotEqual(json.loads(raw[1])["cwd"], str(self.repo))
        request = execute.call_args.args[0]
        self.assertEqual(request.repo_trust, "trusted")
        self.assertTrue(request.readonly)
        self.assertEqual(request.cwd, str(self.repo))
        self.assertEqual(len(json.loads(self.ledger.read_text())["attempts"]), 1)

    def test_version_only_cli_runs_outside_untrusted_repository(self):
        binaries = self.root / "bin"
        binaries.mkdir()
        for name in ("agy", "aider"):
            executable = binaries / name
            executable.write_text("#!" + sys.executable + "\nimport os\nprint(os.getcwd())\n")
            executable.chmod(0o700)
        with patch.dict(os.environ, {"PATH": str(binaries) + os.pathsep + os.environ["PATH"]}), \
                patch("makewand.config.is_provider_enabled", return_value=True), \
                patch("makewand.config.has_subscription_configured", return_value=True), \
                patch("makewand.config.has_api_configured", return_value=False), \
                patch.object(health, "_run_model_probe") as inference:
            for name in ("agy", "aider"):
                with self.subTest(name=name):
                    result = health.probe_model(name, repo_trust="untrusted", cwd=str(self.repo))
                    self.assertEqual(result["status"], "healthy")
                    self.assertNotIn(str(self.repo), result["reason"])
                    self.assertIn("makewand-version-", result["reason"])
        inference.assert_not_called()


if __name__ == "__main__":
    unittest.main()
