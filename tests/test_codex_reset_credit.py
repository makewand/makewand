"""Automatic Codex reset credit: only a live 100% weekly reading may spend one."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import stat
import sys
import tempfile
import time
import unittest
from datetime import datetime, timedelta
from pathlib import Path
from unittest.mock import patch

from makewand import codex_reset_credit as credit
from makewand import model_quota
from makewand.account_identity import account_ref

IDENTITY = {"identity_known": True, "account_ref": account_ref("codex", "acct"), "pool_ref": None,
            "credential_root": "/x"}


def _reply(used=100, minutes=10080, count=1, account="acct", status="available", resets=4102444800, expires=None):
    return {"accountId": account,
            "rateLimits": {"limitId": "codex", "primary": {"usedPercent": used, "windowDurationMins": minutes,
                                                           "resetsAt": resets}},
            "rateLimitResetCredits": {"availableCount": count, "credits": [
                {"id": "c", "resetType": "codexRateLimits", "status": status, "expiresAt": expires}] if count else []}}


class _Server:
    outcome = "reset"
    reply = None
    calls = []

    def request(self, method, params=None):
        type(self).calls.append((method, params))
        if method == "account/rateLimits/read":
            return type(self).reply
        return {"outcome": type(self).outcome}

    def close(self):
        pass


class RestoreQuotaTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        for patcher in (patch.object(credit, "CONFIG_DIR", Path(self.dir.name)),
                        patch.object(credit, "ensure_config_dir"),
                        patch.object(credit, "_AppServer", _Server),
                        patch.dict(os.environ, {credit.DISABLE_ENV: ""}),
                        patch.object(model_quota, "selected_scope", return_value=IDENTITY)):
            patcher.start()
            self.addCleanup(patcher.stop)
        _Server.calls = []
        _Server.outcome = "reset"
        _Server.reply = _reply()

    def _consumes(self):
        return [params for method, params in _Server.calls if method == "account/rateLimitResetCredit/consume"]

    def test_a_used_up_week_spends_one_credit_and_records_it(self):
        self.assertTrue(credit.restore_quota("before_launch"))
        self.assertEqual(len(self._consumes()), 1)
        self.assertGreater(credit.redeemed_at(IDENTITY["account_ref"]), 0)
        audit = (Path(self.dir.name) / credit.AUDIT_FILE).read_text()
        self.assertIn('"outcome": "reset"', audit)
        self.assertNotIn('"c"', audit)

    def test_the_key_changes_with_the_window(self):
        credit.restore_quota("a")
        _Server.reply = _reply(resets=4102444801)
        credit.restore_quota("b")
        keys = [params["idempotencyKey"] for params in self._consumes()]
        self.assertNotEqual(keys[0], keys[1])

    def test_nothing_is_spent_without_a_live_full_week_and_a_usable_credit(self):
        now = time.time()
        for reply in (_reply(used=99.9), _reply(minutes=300), _reply(count=0), _reply(status="used"),
                      _reply(account="other"), _reply(expires=now - 1), {"accountId": "acct"}):
            _Server.reply = reply
            self.assertFalse(credit.restore_quota("before_launch"), reply)
        self.assertEqual(self._consumes(), [])

    def test_an_expiring_credit_is_still_usable(self):
        _Server.reply = _reply(expires=time.time() + 60)
        self.assertTrue(credit.restore_quota("before_launch"))

    def test_switches_and_an_unknown_account_never_talk_to_codex(self):
        unknown = dict(IDENTITY, identity_known=False)
        with patch.dict(os.environ, {credit.DISABLE_ENV: "off"}):
            self.assertFalse(credit.restore_quota("x"))
        with patch.dict(os.environ, {"MAKEWAND_PROVIDER_WORKER": "1"}):
            self.assertFalse(credit.restore_quota("x"))
        with patch.object(model_quota, "selected_scope", return_value=unknown):
            self.assertFalse(credit.restore_quota("x"))
        self.assertEqual(_Server.calls, [])

    def test_outcomes(self):
        for outcome, restored in (("reset", True), ("alreadyRedeemed", False), ("noCredit", False),
                                  ("nothingToReset", False), ("surprise", False)):
            (Path(self.dir.name) / credit.STATE_FILE).unlink(missing_ok=True)
            _Server.outcome = outcome
            self.assertEqual(credit.restore_quota("x"), restored, outcome)
            self.assertEqual(credit.redeemed_at(IDENTITY["account_ref"]) > 0, restored, outcome)

    def test_a_failing_session_keeps_the_refusal(self):
        with patch.object(credit, "_AppServer", side_effect=OSError("no codex")):
            self.assertFalse(credit.restore_quota("x"))

    def test_after_a_reset_a_new_refusal_is_not_excused(self):
        credit.restore_quota("x")
        _Server.reply = _reply(used=20)
        for trigger in ("provider_refusal", "before_launch", "cached_limit"):
            self.assertFalse(credit.restore_quota(trigger))
        self.assertEqual(len(self._consumes()), 1)

    def test_a_full_short_window_keeps_the_credit(self):
        _Server.reply = _reply()
        _Server.reply["rateLimits"]["secondary"] = {"usedPercent": 100, "windowDurationMins": 300}
        self.assertFalse(credit.restore_quota("x"))
        _Server.reply["rateLimits"]["secondary"]["usedPercent"] = 40
        self.assertTrue(credit.restore_quota("x"))
        self.assertEqual(len(self._consumes()), 1)

    def test_already_redeemed_without_a_lost_reply_is_not_a_new_reset(self):
        _Server.outcome = "alreadyRedeemed"
        self.assertFalse(credit.restore_quota("x"))
        self.assertEqual(credit.redeemed_at(IDENTITY["account_ref"]), 0)

    def test_a_lost_reply_dates_the_reset_when_it_was_asked(self):
        class Lost(_Server):
            def request(self, method, params=None):
                if method.endswith("consume"):
                    raise OSError("reply lost")
                return super().request(method, params)
        asked = time.time()
        with patch.object(credit, "_AppServer", Lost):
            credit.restore_quota("x")
        _Server.outcome = "alreadyRedeemed"
        with patch.object(credit.time, "time", return_value=asked + 3600):
            self.assertTrue(credit.restore_quota("y"))
        self.assertLess(credit.redeemed_at(IDENTITY["account_ref"]), asked + 60)

    def test_another_account_keeps_a_pending_key_intact(self):
        class Lost(_Server):
            def request(self, method, params=None):
                if method.endswith("consume"):
                    _Server.calls.append((method, params))
                    raise OSError("reply lost")
                return super().request(method, params)
        other = dict(IDENTITY, account_ref=account_ref("codex", "other"))
        with patch.object(credit, "_AppServer", Lost):
            credit.restore_quota("a")
        with patch.object(model_quota, "selected_scope", return_value=other):
            _Server.reply = _reply(account="other")
            self.assertTrue(credit.restore_quota("b"))
        _Server.reply = _reply(resets=4102444801)
        _Server.outcome = "alreadyRedeemed"
        self.assertTrue(credit.restore_quota("c"))
        keys = [params["idempotencyKey"] for params in self._consumes()]
        self.assertEqual(keys[0], keys[2])
        self.assertNotEqual(keys[0], keys[1])

    def test_a_window_that_already_ended_spends_nothing(self):
        for resets in (int(time.time()) - 1, None, "soon"):
            _Server.reply = _reply(resets=resets)
            self.assertFalse(credit.restore_quota("x"), resets)
        self.assertEqual(self._consumes(), [])

    def test_a_lost_reply_is_retried_with_the_same_key_in_a_new_window(self):
        class Lost(_Server):
            def request(self, method, params=None):
                if method.endswith("consume"):
                    _Server.calls.append((method, params))
                    raise OSError("reply lost")
                return super().request(method, params)
        with patch.object(credit, "_AppServer", Lost):
            self.assertFalse(credit.restore_quota("x"))
        _Server.reply = _reply(resets=4102444801)
        _Server.outcome = "alreadyRedeemed"
        self.assertTrue(credit.restore_quota("y"))
        keys = [params["idempotencyKey"] for params in self._consumes()]
        self.assertEqual(keys[0], keys[1])
        # Settled: the next used-up window gets its own key.
        _Server.reply = _reply(resets=4102444802)
        _Server.outcome = "reset"
        credit.restore_quota("z")
        self.assertNotEqual(self._consumes()[-1]["idempotencyKey"], keys[0])

    def test_an_unwritable_state_file_spends_nothing(self):
        with patch.object(credit, "_save_state", side_effect=OSError):
            self.assertFalse(credit.restore_quota("x"))
        self.assertEqual(self._consumes(), [])

    def test_a_lock_held_elsewhere_gives_up_without_spending(self):
        with patch.object(credit, "LOCK_SECONDS", 0), patch.object(credit.filelock, "flock", side_effect=OSError):
            self.assertFalse(credit.restore_quota("x"))
        self.assertEqual(_Server.calls, [])

    def _quota(self, used, at):
        return {"account_ref": IDENTITY["account_ref"], "pool_ref": None,
                "windows": [{"pool_ref": None, "used_percent": used, "window_minutes": 10080,
                             "observed_at": datetime.fromtimestamp(at).astimezone().isoformat()}]}

    def test_the_gate_skips_only_evidence_from_before_the_reset(self):
        now = time.time()
        policy = type("P", (), {"codex_max_weekly": None})()
        limited = {"status": "limited", "reason": "r",
                   "updated_at": (datetime.now() - timedelta(minutes=5)).isoformat()}
        fresh = dict(limited, updated_at=(datetime.now() + timedelta(minutes=5)).isoformat())
        args = dict(info={}, policy=policy)
        self.assertTrue(model_quota.quota_reason("codex", quota=self._quota(100, now - 60), **args))
        self.assertTrue(model_quota.quota_reason("codex", info=limited, quota={}, policy=policy))
        credit.restore_quota("x")
        self.assertIsNone(model_quota.quota_reason("codex", quota=self._quota(100, now - 60), **args))
        self.assertIsNone(model_quota.quota_reason("codex", info=limited, quota={}, policy=policy))
        self.assertTrue(model_quota.quota_reason("codex", info=fresh, quota={}, policy=policy))
        self.assertTrue(model_quota.quota_reason("codex", quota=self._quota(100, time.time() + 30), **args))
        self.assertIn("已用 94", model_quota.quota_reason("codex", quota=self._quota(94, time.time() + 30), **args))

    def test_a_reset_leaves_other_windows_blocking(self):
        policy = type("P", (), {"codex_max_weekly": None})()
        short = self._quota(100, time.time() - 60)
        short["windows"][0]["window_minutes"] = 300
        credit.restore_quota("x")
        self.assertIn("300", model_quota.quota_reason("codex", quota=short, info={}, policy=policy))

    def test_a_launch_is_checked_again_after_a_restore(self):
        reasons = ["weekly", "short window"]
        with patch.object(model_quota, "quota_reason", side_effect=reasons), \
                patch("makewand.codex_reset_credit.restore_quota", return_value=True):
            self.assertEqual(model_quota.before_cli_launch("codex", "m")[2], "short window")

    def test_launch_proceeds_only_after_a_restore(self):
        with patch.object(model_quota, "quota_reason", return_value="limit"):
            with patch("makewand.codex_reset_credit.restore_quota", return_value=True) as restore, \
                    patch.object(model_quota, "quota_reason", side_effect=["limit", None]):
                self.assertIsNone(model_quota.before_cli_launch("codex", "m"))
            restore.assert_called_once_with("before_launch")
            with patch("makewand.codex_reset_credit.restore_quota", return_value=False):
                self.assertIsNotNone(model_quota.before_cli_launch("codex", "m"))
            with patch("makewand.codex_reset_credit.restore_quota") as restore:
                self.assertIsNotNone(model_quota.before_cli_launch("grok", "m"))
            restore.assert_not_called()


class CodexRefusalTests(unittest.TestCase):
    def _run(self, restored):
        try:
            from test_multi_model_optimization import _FakeProviderHome
        except ImportError:
            from tests.test_multi_model_optimization import _FakeProviderHome
        from makewand.providers.codex import execute_codex_task
        refusal = (1, "", "ERROR: You've hit your usage limit. Try again in 3 days.\n", None)
        with _FakeProviderHome() as home, patch("makewand.sandbox.is_bwrap_available", return_value=True), \
                patch("makewand.sandbox._provider_socket_preflight"), \
                patch("makewand.providers.codex.run_subprocess", return_value=refusal), \
                patch("makewand.providers.codex.parse_codex_quota", return_value=(True, "limit", None)), \
                patch("makewand.codex_reset_credit.restore_quota", return_value=restored) as restore, \
                patch("makewand.health.record_engine_limit") as record:
            result = execute_codex_task("review", cwd=str(home.workspace), readonly=True)
        restore.assert_called_once_with("provider_refusal")
        return result, record

    def test_a_restored_quota_does_not_record_a_limit_and_the_run_still_fails(self):
        result, record = self._run(True)
        self.assertFalse(result[0])
        self.assertIn("赠送重置", result[2])
        record.assert_not_called()

    def test_without_a_restore_the_limit_is_recorded(self):
        _result, record = self._run(False)
        record.assert_called_once()


@unittest.skipUnless(os.name == "posix", "POSIX fake executable")
class AppServerProtocolTests(unittest.TestCase):
    def test_the_stdio_session_reads_and_consumes(self):
        with tempfile.TemporaryDirectory() as home:
            log = Path(home) / "requests.jsonl"
            exe = Path(home) / "codex"
            exe.write_text(f"""#!{sys.executable}
import json, sys
reply = {json.dumps(_reply())!r}
for line in sys.stdin:
    message = json.loads(line)
    open({str(log)!r}, "a").write(line)
    if "id" not in message:
        continue
    print(json.dumps({{"method": "noise"}}), flush=True)
    method = message["method"]
    result = json.loads(reply) if method == "account/rateLimits/read" else {{"outcome": "reset"}}
    print(json.dumps({{"id": message["id"], "result": result}}), flush=True)
""")
            exe.chmod(exe.stat().st_mode | stat.S_IEXEC)
            with patch.object(credit, "CONFIG_DIR", Path(home)), patch.object(credit, "ensure_config_dir"), \
                    patch("makewand.providers.base.safe_which", return_value=str(exe)), \
                    patch.dict(os.environ, {credit.DISABLE_ENV: ""}), \
                    patch.object(model_quota, "selected_scope", return_value=IDENTITY):
                self.assertTrue(credit.restore_quota("x"))
            methods = [json.loads(line)["method"] for line in log.read_text().splitlines()]
        self.assertEqual(methods, ["initialize", "initialized", "account/rateLimits/read",
                                   "account/rateLimitResetCredit/consume"])

    def test_a_silent_server_ends_at_the_session_limit(self):
        with tempfile.TemporaryDirectory() as home:
            exe = Path(home) / "codex"
            exe.write_text(f"#!{sys.executable}\nimport time\ntime.sleep(60)\n")
            exe.chmod(exe.stat().st_mode | stat.S_IEXEC)
            with patch.object(credit, "CONFIG_DIR", Path(home)), patch.object(credit, "ensure_config_dir"), \
                    patch.object(credit, "SESSION_SECONDS", 0.5), \
                    patch("makewand.providers.base.safe_which", return_value=str(exe)), \
                    patch.dict(os.environ, {credit.DISABLE_ENV: ""}), \
                    patch.object(model_quota, "selected_scope", return_value=IDENTITY):
                started = time.monotonic()
                self.assertFalse(credit.restore_quota("x"))
            self.assertLess(time.monotonic() - started, 20)

    def test_a_wrapper_child_holding_stdout_is_killed_with_the_session(self):
        with tempfile.TemporaryDirectory() as home:
            pid_file = Path(home) / "child.pid"
            exe = Path(home) / "codex"
            exe.write_text(f"""#!{sys.executable}
import subprocess, sys, time
child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(120)"])
open({str(pid_file)!r}, "w").write(str(child.pid))
time.sleep(120)
""")
            exe.chmod(exe.stat().st_mode | stat.S_IEXEC)
            with patch.object(credit, "CONFIG_DIR", Path(home)), patch.object(credit, "ensure_config_dir"), \
                    patch.object(credit, "SESSION_SECONDS", 1), \
                    patch("makewand.providers.base.safe_which", return_value=str(exe)), \
                    patch.dict(os.environ, {credit.DISABLE_ENV: ""}), \
                    patch.object(model_quota, "selected_scope", return_value=IDENTITY):
                started = time.monotonic()
                self.assertFalse(credit.restore_quota("x"))
            self.assertLess(time.monotonic() - started, 20)
            child = int(pid_file.read_text())
            for _ in range(50):
                try:
                    with open(f"/proc/{child}/stat") as handle:
                        if handle.read().rsplit(")", 1)[1].split()[0] == "Z":
                            break
                except OSError:
                    break
                time.sleep(0.1)
            else:
                self.fail("the wrapper's child survived the session")


if __name__ == "__main__":
    unittest.main()
