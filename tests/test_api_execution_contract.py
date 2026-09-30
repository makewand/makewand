"""Offline transport boundaries: no replay, shared admissions and read-only API."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import io
import json
import os
import tempfile
import threading
import time
import unittest
import urllib.error
from pathlib import Path
from unittest.mock import MagicMock, patch

from makewand import call_budget
from makewand.execution_contract import ExecutionRequest
from makewand.execution_runtime import execute, execution_context, mark_provider_invocation
from makewand.providers.api_client import _make_http_request, call_api_chat


def response(content="ok", frames=None):
    value = MagicMock()
    value.status = 200
    value.__enter__.return_value = value
    value.read.return_value = json.dumps({"choices": [{"message": {"content": content}}]}).encode()
    if frames is not None:
        value.__iter__.return_value = iter(frames)
    return value


def refusal():
    return urllib.error.HTTPError("https://fixture.invalid", 503, "Unavailable", {}, io.BytesIO(b"refused"))


class APIExecutionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.ledger = Path(self.temp.name) / "budget.json"
        for fixture in (
            patch.dict(os.environ, {"MAKEWAND_CALL_BUDGET_FILE": str(self.ledger), "MAKEWAND_MAX_MODEL_CALLS": "3"}),
            patch("makewand.config.get_api_policy", return_value="allow_paid"),
            patch("makewand.providers.api_client.get_api_config", return_value={"api_key": "fixture", "base_url": "https://fixture.invalid", "model": "fixture"}),
        ):
            fixture.start()
            self.addCleanup(fixture.stop)

    def attempts(self):
        return json.loads(self.ledger.read_text())["attempts"]

    def request(self, **extra):
        return ExecutionRequest(task_id="api-fixture", stage="review", engine="codex", timeout_ms=10000,
            api_policy="allow_paid", **extra)

    @patch("urllib.request.urlopen")
    def test_public_api_and_enclosing_sdk_each_admit_once(self, transport):
        transport.return_value = response()
        direct = call_api_chat("codex", "test")
        self.assertEqual(direct.status, "PASSED")
        self.assertEqual(len(self.attempts()), 1)
        wrapped = execute(self.request(), lambda remaining: call_api_chat("codex", "test", timeout=remaining))
        self.assertEqual(wrapped.status, "PASSED")
        self.assertEqual(len(self.attempts()), 2)
        self.assertEqual(transport.call_count, 2)

    @patch("urllib.request.urlopen")
    def test_known_refusal_retry_consumes_new_admission(self, transport):
        transport.side_effect = [refusal(), response()]
        result = call_api_chat("codex", "test", backoff_factor=0)
        self.assertEqual(result.status, "PASSED")
        self.assertEqual(len(self.attempts()), 2)
        self.assertEqual(transport.call_count, 2)

    @patch("urllib.request.urlopen")
    def test_budget_exhaustion_stops_before_retry_post(self, transport):
        os.environ["MAKEWAND_MAX_MODEL_CALLS"] = "1"
        transport.side_effect = [refusal(), response()]
        result = call_api_chat("codex", "test", backoff_factor=0)
        self.assertEqual(result.status, "BUDGET_EXHAUSTED")
        self.assertEqual(len(self.attempts()), 1)
        self.assertEqual(transport.call_count, 1)

    @patch("urllib.request.urlopen")
    def test_connection_loss_never_uses_retry_success(self, transport):
        transport.side_effect = [urllib.error.URLError("connection closed"), response()]
        result = call_api_chat("codex", "test", backoff_factor=0)
        self.assertEqual(result.status, "UNKNOWN")
        self.assertFalse(result.outcome_known)
        self.assertEqual(len(self.attempts()), 1)
        self.assertEqual(transport.call_count, 1)

    @patch("urllib.request.urlopen")
    def test_cli_to_api_fallback_is_a_new_readonly_dispatch(self, transport):
        transport.return_value = response("```filepath: marker.txt\nforbidden\n```")
        def after_cli(remaining):
            mark_provider_invocation()  # A known refusal after an admitted CLI.
            return call_api_chat("codex", "review", cwd=self.temp.name, timeout=remaining)
        result = execute(self.request(readonly=True), after_cli)
        self.assertEqual(result.status, "PASSED")
        self.assertEqual(len(self.attempts()), 2)
        self.assertFalse((Path(self.temp.name) / "marker.txt").exists())
        self.assertTrue(all(item["readonly"] for item in self.attempts()))

    @patch("urllib.request.urlopen")
    def test_cli_fallback_cannot_bypass_exhausted_budget(self, transport):
        os.environ["MAKEWAND_MAX_MODEL_CALLS"] = "1"
        def after_cli(remaining):
            mark_provider_invocation()
            return call_api_chat("codex", "test", timeout=remaining)
        result = execute(self.request(), after_cli)
        self.assertEqual(result.status, "BUDGET_EXHAUSTED")
        self.assertEqual(len(self.attempts()), 1)
        transport.assert_not_called()

    @patch("urllib.request.urlopen")
    def test_parent_subscription_policy_cannot_be_overridden_by_global_config(self, transport):
        request = ExecutionRequest(task_id="api-fixture", stage="review", engine="codex", api_policy="subscription_only")
        result = execute(request, lambda remaining: call_api_chat("codex", "test"))
        self.assertEqual(result.status, "FAILED")
        self.assertEqual(len(self.attempts()), 1)
        transport.assert_not_called()

    @patch("urllib.request.urlopen")
    def test_invalid_api_timeout_refuses_before_any_admission(self, transport):
        for invalid in (0, -1, float("inf"), float("nan"), True):
            with self.subTest(invalid=invalid):
                self.assertEqual(call_api_chat("codex", "test", timeout=invalid).status, "INVALID_REQUEST")
        self.assertFalse(self.ledger.exists())
        transport.assert_not_called()

    def test_health_echo_retains_known_quota_refusal(self):
        from makewand import health
        raw = (1, "quota refused", "", None)
        with patch.object(health, "run_subprocess", return_value=raw) as process, patch.object(health, "parse_codex_quota", return_value=(True, "quota", None)):
            self.assertEqual(health._run_model_probe("codex", "fixture only", 1), raw)
        self.assertEqual(process.call_count, 1)
        self.assertEqual(self.attempts()[0]["result_status"], "FAILED")
        self.assertTrue(self.attempts()[0]["outcome_known"])

    def test_health_echo_connection_loss_is_an_unknown_attempt(self):
        from makewand import health
        with patch.object(health, "run_subprocess", return_value=(1, "", "connection closed", None)) as process:
            self.assertEqual(health._run_model_probe("codex", "fixture only", 1)[0], 127)
        self.assertEqual(process.call_count, 1)
        self.assertEqual(self.attempts()[0]["result_status"], "UNKNOWN")
        self.assertFalse(self.attempts()[0]["outcome_known"])

    @patch("urllib.request.urlopen")
    def test_extra_retry_releases_consumed_stage_hold(self, transport):
        coder = call_budget.reserve_capacity(1, "api-fixture", purpose="coder")
        judge = call_budget.reserve_capacity(1, "api-fixture", purpose="judge")
        transport.side_effect = [refusal(), response()]
        with execution_context(lease_id=coder):
            result = execute(self.request(), lambda remaining: call_api_chat("codex", "test", timeout=remaining, backoff_factor=0))
        self.assertEqual(result.status, "PASSED")
        ledger = json.loads(self.ledger.read_text())
        self.assertEqual(len(ledger["attempts"]), 2)
        self.assertEqual(ledger["holds"][judge]["remaining"], 1)

    @patch("urllib.request.urlopen")
    def test_truncated_stream_cannot_apply_partial_code(self, transport):
        transport.return_value = response(frames=[b'data: {"choices":[{"delta":{"content":"```filepath: marker.txt\\nforbidden\\n```"}}]}\n'])
        result = call_api_chat("codex", "test", cwd=self.temp.name, stream=True)
        self.assertEqual(result.status, "UNKNOWN")
        self.assertEqual(transport.call_count, 1)
        self.assertFalse((Path(self.temp.name) / "marker.txt").exists())

    @patch("urllib.request.urlopen")
    def test_anthropic_terminal_message_and_openai_finish_reason(self, transport):
        for terminal in (b'data: {"type":"message_stop"}\n', b'data: {"choices":[{"delta":{},"finish_reason":"stop"}]}\n'):
            with self.subTest(terminal=terminal):
                transport.return_value = response(frames=[b'data: {"delta":{"text":"complete"}}\n', terminal])
                code, text, error = _make_http_request("https://fixture.invalid", {}, {}, stream=True)
                self.assertEqual((code, text, error), (200, "complete", None))

    @patch("urllib.request.urlopen")
    def test_http_error_body_obeys_total_deadline(self, transport):
        entered, release, finished = threading.Event(), threading.Event(), threading.Event()
        class SlowBody(io.BytesIO):
            def read(self, *args):
                entered.set()
                try:
                    release.wait(2)
                    return super().read(*args)
                finally:
                    finished.set()
        transport.side_effect = urllib.error.HTTPError("https://fixture.invalid", 503, "Unavailable", {}, SlowBody(b"refused"))
        started = time.monotonic()
        try:
            code, _, error = _make_http_request("https://fixture.invalid", {}, {}, timeout=.1)
            elapsed = time.monotonic() - started
            self.assertTrue(entered.is_set())
            self.assertEqual(code, -1)
            self.assertEqual(error.execution_status, "TIMEOUT")
            self.assertLess(elapsed, .8)
            self.assertEqual(transport.call_count, 1)
        finally:
            release.set()
            self.assertTrue(finished.wait(2))
