"""Offline evidence lifecycle tests: only owned homes and local fake children."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import execution_evidence as evidence
from makewand.account_identity import account_ref
from makewand.execution_contract import ExecutionRequest
from makewand.execution_runtime import current_context, execute
from makewand.providers.base import run_subprocess, model_process_failure
from makewand.sandbox import ProviderHome
from makewand.health import _get_official_subscription_quota


def events(account="account-a", used=42, stamp=None):
    return "\n".join(json.dumps(value) for value in (
        {"type": "session_meta", "payload": {"account_id": account, "model": "actual-model"}},
        {"type": "event_msg", "timestamp": stamp or time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
         "payload": {"type": "token_count", "rate_limits": {"primary": {
             "used_percent": used, "window_minutes": 300, "resets_at": int(time.time()) + 3600}}}},
        {"type": "item.completed", "item": {"type": "agent_message", "text": "PRIVATE PROMPT access_token SECRET"}},
    ))


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.home = self.root / "home"
        self.selected = self.home / ".codex"
        self.selected.mkdir(parents=True)
        (self.selected / "auth.json").write_text(json.dumps({"tokens": {"account_id": "account-a", "access_token": "SECRET"}}))
        self.store = self.root / "evidence"
        self.env = patch.dict(os.environ, {"CODEX_HOME": str(self.selected),
            "MAKEWAND_EXECUTION_EVIDENCE_DIR": str(self.store),
            "MAKEWAND_CALL_BUDGET_FILE": str(self.root / "budget.json"), "MAKEWAND_MAX_MODEL_CALLS": "5"})
        self.env.start()

    def tearDown(self):
        self.env.stop()
        self.temp.cleanup()

    def request(self):
        return ExecutionRequest(task_id="offline-evidence", stage="review", engine="codex", model="requested-model", prompt="PRIVATE PROMPT", readonly=True)

    def record(self, result):
        return json.loads((self.store / (result.attempt_id + ".json")).read_text())

    def test_private_home_is_saved_before_removal_and_quota_survives(self):
        private = []
        def callback(timeout):
            home = ProviderHome("codex", str(self.home))
            home.materialize()
            root = Path(home.credentials[0].private_root)
            private.append(root)
            sessions = root / "sessions"
            sessions.mkdir()
            # The fake child only writes a parent-supplied static event sample.
            code, out, err, ex = run_subprocess([sys.executable, "-I", "-c",
                "import pathlib,sys; pathlib.Path(sys.argv[1]).write_text(sys.argv[2])",
                str(sessions / "sample.jsonl"), events()], timeout=5)
            self.assertEqual(code, 0, (err, ex))
            home.finalize(write_back=False)
            stored = list(evidence.read_recent())
            self.assertEqual(stored[0]["result_status"], "UNKNOWN")
            self.assertFalse(root.exists())
            return True, "answer", None
        result = execute(self.request(), callback)
        self.assertEqual(result.status, "PASSED")
        stored = self.record(result)
        self.assertEqual(stored["actual_model"], "actual-model")
        self.assertEqual(stored["account_ref"], account_ref("codex", "account-a"))
        self.assertEqual(stored["windows"][0]["used_percent"], 42)
        self.assertTrue(stored["outcome_known"])
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["percentage"], 58)
        self.assertEqual(quota["selected_source"], "execution_evidence")
        text = json.dumps(stored)
        for secret in ("SECRET", "access_token", "PRIVATE PROMPT", "requested-model", str(private[0])):
            self.assertNotIn(secret, text)
        self.assertEqual((self.store / (result.attempt_id + ".json")).stat().st_mode & 0o777, 0o600)

    def test_ephemeral_stdout_and_unknown_exit_are_retained_without_retry(self):
        called = []
        def callback(timeout):
            called.append(1)
            code, out, err, ex = run_subprocess([sys.executable, "-I", "-c",
                "import sys; print(sys.argv[1]); sys.exit(7)", events()], timeout=5, evidence_protocol=True)
            return model_process_failure("codex", code, out, err, ex)
        result = execute(self.request(), callback)
        self.assertEqual(len(called), 1)
        self.assertEqual(result.status, "UNKNOWN")
        self.assertFalse(self.record(result)["outcome_known"])
        self.assertEqual(self.record(result)["process_exit_code"], 7)
        self.assertEqual(self.record(result)["actual_model"], "actual-model")
        self.assertEqual(self.record(result)["windows"][0]["used_percent"], 42)
        self.assertEqual(json.loads((self.root / "budget.json").read_text())["attempts"][0]["outcome_known"], False)

    def test_timeout_without_metadata_stays_unknown(self):
        def callback(timeout):
            code, out, err, ex = run_subprocess([sys.executable, "-I", "-c", "import time; time.sleep(5)"], timeout=.1)
            return model_process_failure("codex", code, out, err, ex)
        result = execute(self.request(), callback)
        self.assertEqual(result.status, "TIMEOUT")
        stored = self.record(result)
        self.assertFalse(stored["outcome_known"])
        self.assertNotIn("actual_model", stored)
        self.assertEqual(stored["windows"], [])

    def test_wrong_account_stale_and_message_bodies_do_not_authorize_quota(self):
        def callback(timeout):
            record = current_context()["_evidence"]
            evidence.protocol(record, events(account="account-b", used=99))
            evidence.protocol(record, json.dumps({"type": "item.completed", "item": {
                "type": "agent_message", "text": events(), "rate_limits": {"primary": {"used_percent": 1}}}}))
            self.assertEqual(record["windows"], [])
            evidence.protocol(record, events(stamp="2020-01-01T00:00:00Z"))
            return True, "ok", None
        result = execute(self.request(), callback)
        self.assertEqual(result.status, "PASSED")
        quota = _get_official_subscription_quota("codex")
        self.assertTrue(quota is None or quota["percentage"] is None)
        (self.selected / "auth.json").write_text('{"tokens":{"account_id":"account-b"}}')
        quota = _get_official_subscription_quota("codex")
        self.assertTrue(quota is None or quota["percentage"] is None)

    def test_initial_write_failure_degrades_telemetry_and_after_launch_is_unknown(self):
        # go-server-1021-6 parity: nothing has started when the first record
        # fails (an unwritable directory, a full disk), so the call runs and
        # keeps its real outcome; only its telemetry is missing.
        called = []
        with patch.object(evidence, "save", side_effect=OSError("SECRET")) as save:
            result = execute(self.request(), lambda timeout: called.append(1) or (True, "answer", None))
        self.assertEqual(called, [1])
        self.assertEqual((result.status, result.outcome_known), ("PASSED", True))
        self.assertEqual(save.call_count, 1)
        self.assertEqual(list(self.store.glob("*.json")), [])
        attempt = json.loads((self.root / "budget.json").read_text())["attempts"][0]
        self.assertEqual((attempt["status"], attempt["result_status"]), ("completed", "PASSED"))
        def callback(timeout):
            current_context()["_evidence"]["persistence_failed"] = True
            return True, "answer", None
        result = execute(self.request(), callback)
        self.assertEqual(result.status, "UNKNOWN")
        self.assertFalse(self.record(result)["outcome_known"])

    def test_unbounded_execution_still_records_and_parallel_attempts_do_not_clobber(self):
        from concurrent.futures import ThreadPoolExecutor
        os.environ.pop("MAKEWAND_CALL_BUDGET_FILE")
        os.environ.pop("MAKEWAND_MAX_MODEL_CALLS")
        with ThreadPoolExecutor(max_workers=4) as pool:
            results = list(pool.map(lambda _: execute(self.request(), lambda timeout: (True, "ok", None)), range(8)))
        self.assertEqual(len(list(self.store.glob("*.json"))), 8)
        self.assertTrue(all(self.record(result)["outcome_known"] for result in results))

    def test_plain_stdout_is_not_protocol_evidence(self):
        def callback(timeout):
            run_subprocess([sys.executable, "-I", "-c", "import sys; print(sys.argv[1])", events()], timeout=5)
            return True, "ok", None
        stored = self.record(execute(self.request(), callback))
        self.assertNotIn("actual_model", stored)
        self.assertEqual(stored["windows"], [])

    def test_private_account_switch_binds_observed_login_and_rejects_symlinks(self):
        def callback(timeout):
            home = ProviderHome("codex", str(self.home))
            home.materialize()
            root = Path(home.credentials[0].private_root)
            (root / "auth.json").write_text('{"tokens":{"account_id":"account-b","access_token":"SECRET"}}')
            (root / "sessions").mkdir()
            (root / "sessions" / "b.jsonl").write_text(events(account="account-b"))
            home.finalize(write_back=False)
            return True, "ok", None
        stored = self.record(execute(self.request(), callback))
        self.assertEqual(stored["account_ref"], account_ref("codex", "account-b"))
        self.assertEqual(stored["pool_ref"], stored["account_ref"] + ":subscription")
        self.assertTrue(stored["windows"])
        self.assertIsNone(_get_official_subscription_quota("codex"))
        def linked(timeout):
            home = ProviderHome("codex", str(self.home))
            home.materialize()
            root = Path(home.credentials[0].private_root)
            (root / "auth.json").unlink()
            (root / "auth.json").symlink_to(self.selected / "auth.json")
            home.finalize(write_back=False)
            return True, "ok", None
        stored = self.record(execute(self.request(), linked))
        self.assertFalse(stored["identity_known"])
        self.assertEqual(stored["windows"], [])

    def test_claude_private_session_projects_model_is_projected(self):
        selected = self.home / ".claude"
        selected.mkdir()
        (self.home / ".claude.json").write_text('{"oauthAccount":{"accountUuid":"claude-account"}}')
        (selected / ".credentials.json").write_text('{"access_token":"SECRET"}')
        request = ExecutionRequest(task_id="claude-evidence", stage="review", engine="claude")
        def callback(timeout):
            home = ProviderHome("claude", str(self.home))
            home.materialize()
            root = Path(home.credentials[0].private_root)
            (root / "projects").mkdir()
            (root / "projects" / "session.jsonl").write_text(json.dumps({"type":"assistant", "message": {
                "model":"claude-observed", "content":[{"text":"PRIVATE PROMPT SECRET"}]}}))
            home.finalize(write_back=False)
            return True, "ok", None
        with patch.dict(os.environ, {"HOME":str(self.home), "CLAUDE_CONFIG_DIR":str(selected)}):
            stored = self.record(execute(request, callback))
        self.assertEqual(stored["actual_model"], "claude-observed")
        self.assertEqual(stored["account_ref"], account_ref("claude", "claude-account"))
        self.assertNotIn("SECRET", json.dumps(stored))

    def test_interruption_persists_cancelled_unknown_attempt(self):
        def callback(timeout):
            raise KeyboardInterrupt()
        with self.assertRaises(KeyboardInterrupt):
            execute(self.request(), callback)
        records = list(evidence.read_recent())
        self.assertEqual(len(records), 1)
        self.assertEqual(records[0]["result_status"], "CANCELLED")
        self.assertFalse(records[0]["outcome_known"])

    def test_protocol_token_measurement_is_settled_without_retaining_messages(self):
        def callback(timeout):
            evidence.protocol(current_context()["_evidence"], json.dumps({"type":"turn.completed", "usage": {
                "input_tokens": 21, "output_tokens": 13}, "text":"PRIVATE PROMPT SECRET"}))
            return True, "ok", None
        result = execute(self.request(), callback)
        self.assertEqual(result.tokens, 34)
        self.assertEqual(self.record(result)["tokens"], 34)
        ledger = json.loads((self.root / "budget.json").read_text())
        self.assertEqual(ledger["attempts"][0]["tokens"], 34)

    def test_codex_json_adapter_returns_only_agent_message(self):
        import contextlib
        from types import SimpleNamespace
        from makewand.providers.codex import execute_codex_task
        raw = events() + "\n" + json.dumps({"type":"item.completed", "item": {"type":"agent_message", "text":"final answer"}})
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch("makewand.config.is_provider_enabled", return_value=True))
            stack.enter_context(patch("makewand.config.has_subscription_configured", return_value=True))
            stack.enter_context(patch("makewand.health.load_status_cache", return_value={}))
            stack.enter_context(patch("makewand.discovery.get_provider_model_tier", return_value={"model":"requested-model", "effort":"none"}))
            launch = contextlib.nullcontext(SimpleNamespace(cmd=["fake-codex"], pass_fds=(), env={}))
            sandbox = stack.enter_context(patch("makewand.sandbox.provider_sandbox_launch", return_value=launch))
            process = stack.enter_context(patch("makewand.providers.codex.run_subprocess", return_value=(0, raw, "", None)))
            result = execute_codex_task("PRIVATE PROMPT", cwd=str(self.root), readonly=True)
        self.assertTrue(result[0])
        self.assertEqual(result[1], "PRIVATE PROMPT access_token SECRET\nfinal answer")
        self.assertIn("--json", sandbox.call_args.args[1])
        self.assertIn("--ephemeral", sandbox.call_args.args[1])
        self.assertTrue(process.call_args.kwargs["evidence_protocol"])

    def test_api_transport_does_not_claim_subscription_identity(self):
        def callback(timeout):
            evidence.api_transport()
            return True, "api answer", None
        stored = self.record(execute(self.request(), callback))
        self.assertFalse(stored["identity_known"])
        self.assertEqual(stored["account_ref"], "")
        self.assertEqual(stored["pool_ref"], "")
        self.assertEqual(stored["source"], "api")


if __name__ == "__main__":
    unittest.main()
