"""Actual provider adapters with offline process/HTTP fixtures, never model CLIs."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import importlib
import io
import json
import tempfile
import unittest
from unittest.mock import patch

from makewand import orchestrator as orch
from makewand.execution_contract import ExecutionResult, EXIT_UNKNOWN
from makewand.providers import base, local
from makewand.workflow import provider_outcome


ENGINES = ("claude", "codex", "grok", "muse", "agy", "aider")


class ProviderOutcomeTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.directory = directory.name

    @contextlib.contextmanager
    def adapter(self, engine, raw=(1, "partial response", "connection closed before final response", None)):
        module = importlib.import_module("makewand.providers." + engine)
        runner = base if engine == "aider" else module
        with contextlib.ExitStack() as stack:
            stack.enter_context(contextlib.redirect_stderr(io.StringIO()))
            stack.enter_context(patch("makewand.config.has_subscription_configured", return_value=True))
            stack.enter_context(patch("makewand.config.has_api_configured", return_value=False))
            stack.enter_context(patch("makewand.config.is_provider_enabled", return_value=True))
            stack.enter_context(patch("makewand.health.load_status_cache", return_value={}))
            stack.enter_context(patch("makewand.health.record_engine_limit"))
            stack.enter_context(patch("makewand.health.record_engine_failure"))
            stack.enter_context(patch("makewand.sandbox.is_bwrap_available", return_value=False))
            stack.enter_context(patch("makewand.git_helper.find_git_root", return_value=None))
            stack.enter_context(patch("makewand.discovery.get_provider_model_tier", return_value={"model": "fixture", "effort": "none"}))
            stack.enter_context(patch("makewand.providers.aider.is_aider_available", return_value=True))
            stack.enter_context(patch("makewand.providers.aider.is_provider_enabled", return_value=True))
            stack.enter_context(patch("makewand.providers.aider.get_api_config", return_value={}))
            process = stack.enter_context(patch.object(runner, "run_subprocess", return_value=raw))
            api = stack.enter_context(patch("makewand.providers.api_client.call_api_chat", return_value=(True, "unexpected fallback", None)))
            yield getattr(module, "execute_" + engine + "_task"), process, api

    def test_actual_cli_adapters_positive_disconnect_is_unknown(self):
        for engine in ENGINES:
            with self.subTest(engine=engine), self.adapter(engine) as (execute, process, api):
                result = execute("Review fixture", cwd=self.directory, readonly=True)
                self.assertIsInstance(result, tuple)
                self.assertEqual(len(result), 3)
                self.assertEqual(result.status, "UNKNOWN")
                self.assertFalse(result.outcome_known)
                self.assertIn("connection closed", result[2])
                self.assertTrue(result.readonly)
                process.assert_called_once()
                api.assert_not_called()

    def test_timeout_and_signal_precede_partial_quota_text(self):
        cases = ((-1, base.ProcessExecutionError("Command timed out after 1 seconds", "TIMEOUT"), "TIMEOUT"),
                 (-9, None, "UNKNOWN"))
        for engine in ENGINES:
            for code, error, expected in cases:
                with self.subTest(engine=engine, code=code), self.adapter(engine, (code, "partial", "429 Too Many Requests", error)) as (execute, process, api), \
                        patch("makewand.config.has_api_configured", return_value=True):
                    result = execute("Review fixture", cwd=self.directory, readonly=True)
                    self.assertEqual(result.status, expected)
                    self.assertFalse(result.outcome_known)
                    process.assert_called_once()
                    api.assert_not_called()

    def test_known_subscription_preflight_does_not_launch_process(self):
        with self.adapter("codex") as (execute, process, api), \
                patch("makewand.config.has_subscription_configured", return_value=False):
            result = execute("Review fixture", cwd=self.directory, readonly=True)
        self.assertFalse(result[0])
        self.assertEqual(getattr(provider_outcome(result), "status", "FAILED"), "FAILED")
        process.assert_not_called()
        api.assert_not_called()

    def test_explicit_quota_rejection_remains_known(self):
        for engine in ENGINES[:-1]:
            with self.subTest(engine=engine), self.adapter(engine, (1, "", "HTTP 429 Too Many Requests", None)) as (execute, process, api):
                result = execute("Review fixture", cwd=self.directory, readonly=True)
                self.assertFalse(result[0])
                self.assertEqual(getattr(provider_outcome(result), "status", "FAILED"), "FAILED")
                process.assert_called_once()
                api.assert_not_called()

    def test_api_modes_keep_typed_outcome_and_readonly_role(self):
        for engine in ENGINES[:-1]:
            for mode in ("missing", "cached", "quota"):
                raw = (1, "", "HTTP 429 Too Many Requests", None)
                typed = ExecutionResult(False, "partial", "transport disconnected", status="UNKNOWN", outcome_known=False)
                with self.subTest(engine=engine, mode=mode), self.adapter(engine, raw) as (execute, process, api), \
                        patch("makewand.config.has_subscription_configured", return_value=mode != "missing"), \
                        patch("makewand.config.has_api_configured", return_value=True), \
                        patch("makewand.health.load_status_cache", return_value={engine: {"status": "limited"}} if mode == "cached" else {}):
                    api.return_value = typed
                    result = execute("Review fixture", cwd=self.directory, readonly=True)
                    self.assertIs(result, typed)
                    self.assertEqual(api.call_args.kwargs["role"], "reviewer")
                    self.assertEqual(process.call_count, 1 if mode == "quota" else 0)

    def test_actual_codex_disconnect_stops_review_fallback(self):
        with self.adapter("codex") as (_, process, api), \
                patch.object(orch, "get_git_diff", return_value="diff fixture"), \
                patch.object(orch, "get_or_update_status", return_value={}), \
                patch.object(orch, "execute_grok_task", return_value=(True, 'MAKEWAND_VERDICT: {"pass": true, "defects": []}', None)) as fallback, \
                contextlib.redirect_stdout(io.StringIO()) as output:
            code = orch.run_review(cwd=self.directory, output_json=True)
        self.assertEqual(code, EXIT_UNKNOWN)
        self.assertFalse(json.loads(output.getvalue())["pass"])
        process.assert_called_once()
        fallback.assert_not_called()
        api.assert_not_called()

    def test_real_runner_positive_exit_carries_unknown_evidence(self):
        code, out, err, error = base.run_subprocess([sys.executable, "-c", "import sys; print('partial'); sys.stderr.write('disconnected'); sys.exit(1)"])
        self.assertEqual(code, 1)
        self.assertEqual(error.execution_status, "UNKNOWN")
        self.assertEqual(base.model_process_failure("fixture", code, out, err, error).status, "UNKNOWN")
        self.assertEqual(provider_outcome((False, out, error)).status, "UNKNOWN")

    def test_real_runner_spawn_failure_is_known(self):
        code, out, err, error = base.run_subprocess([str(Path(self.directory) / "absent-executable")])
        self.assertEqual(error.execution_status, "FAILED")
        result = base.model_process_failure("fixture", code, out, err, error)
        self.assertEqual(result.status, "FAILED")
        self.assertTrue(result.outcome_known)

    def test_legacy_transport_errors_are_typed_without_reading_model_text(self):
        for error, status in (("Network/URL Error: disconnected", "UNKNOWN"),
                              ("Execution Exception: disconnected", "UNKNOWN"),
                              ("JSON parse error: incomplete document", "UNKNOWN"),
                              ("Truncated stream: terminal event missing", "UNKNOWN"),
                              ("Total timeout exceeded: API call did not finish within 1s (monotonic deadline)", "TIMEOUT")):
            with self.subTest(error=error):
                self.assertEqual(provider_outcome((False, "partial", error)).status, status)
        model_text = (False, "Network/URL Error: a code example", "explicit refusal")
        self.assertIs(provider_outcome(model_text), model_text)

    def test_local_http_disconnect_timeout_and_truncated_json_are_typed(self):
        cases = (("Network/URL Error: disconnected", "UNKNOWN"),
                 ("JSON parse error: incomplete response", "UNKNOWN"),
                 ("Truncated stream: terminal event missing", "UNKNOWN"),
                 ("Total timeout exceeded: API call did not finish within 1s (monotonic deadline)", "TIMEOUT"))
        with patch("makewand.config.is_provider_enabled", return_value=True), \
                patch.object(local, "is_local_model_available", return_value=(True, "fixture", ["fixture"])), \
                patch.object(local, "has_active_gpu_training", return_value=(False, "")), \
                patch.object(local, "get_free_gpu_vram_mb", return_value=20000), \
                patch.object(local, "unload_local_model") as unload, \
                contextlib.redirect_stderr(io.StringIO()):
            for error, expected in cases:
                with self.subTest(error=error), patch("makewand.providers.api_client.call_api_chat", return_value=(False, "partial", error)) as api:
                    result = local.execute_local_task("Review fixture", cwd=self.directory, readonly=True)
                    self.assertEqual(result.status, expected)
                    self.assertFalse(result.outcome_known)
                    self.assertEqual(api.call_args.kwargs["role"], "reviewer")
            self.assertEqual(unload.call_count, len(cases))


if __name__ == "__main__":
    unittest.main()
