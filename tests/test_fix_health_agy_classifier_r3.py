"""
py-rel-22, round 3: agy's own lines are told apart from the answer by agy's own
formats (cli_message_catalog.json providers.agy, format strings of the agy 1.3.2
binary), not by a two-entry allowlist.

1. An argument rejection ('error: invalid model selection (...)', 'Error: unknown
   flag: --effort') is agy's first own line behind any of its notices: the
   '--mode plan has no effect while slash command expansion is disabled' warning
   every read-only dispatch triggers, the account / slash-command warnings, the
   credential check warning and the environment configuration warning.
2. Stream mode with a merged stream, exit 0: an output of agy's notices and
   diagnostics only is a failure ('warning: run ended with no output ...', a
   glog header ending in ':' with the value it logs); agy's notices are not part
   of the answer; the model's code after a truncated logged JSON body stays.
3. Stream mode with a merged stream, non-zero exit: answer lines before agy's
   final error are not its verdict.
4. A plain line after a glog header without ':' is not agy's verdict either.
With the real runner, stream mode keeps agy's stderr apart from the answer.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import contextlib
import io
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
from makewand.providers import agy, base
from makewand.providers.agy import classify_agy_failure, execute_agy_task, parse_agy_quota

K = "I1012 08:11:57.965555       1 auth.go:157] ChainedAuth: authenticated via keyring (effective: keyring)\n"
STARTUP = ("E1012 08:11:57.633062     188 errorreport.go:224] error getting token source: You are not logged into "
           "Antigravity.\n"
           "I1012 08:11:57.843790       1 printmode.go:403] Print mode: not authenticated, trying silent auth\n")
REJ = ('error: invalid model selection (--model "gemini-3.1-pro" --effort "max"): '
       'gemini-3.1-pro has no "max" effort (available: low, high)\n')
UNK = "Error: unknown flag: --effort\nUsage:\n  agy [flags]\n"
# agy's notices (format strings of the agy binary, instantiated).
NOTICES = {
    "mode_noeffect": "warning: --mode plan has no effect while slash command expansion is disabled.\n",
    "mode_account": "warning: --mode plan is not available for this account; continuing in the default mode.\n",
    "mode_print": "warning: --mode plan is not supported in print mode; continuing in the default mode.\n",
    "slash": "warning: slash commands are unavailable for this run; the prompt was sent without expansion.\n",
    "credentials": "Warning: could not verify credentials (context deadline exceeded); continuing.\n",
    "environment": "Non-fatal environment configuration warning: GOOGLE_CLOUD_PROJECT is set but empty\n",
    "keys": "Warning: Both GOOGLE_API_KEY and GEMINI_API_KEY are set. Using GOOGLE_API_KEY.\n",
}
NO_OUTPUT = "warning: run ended with no output and no recorded error\n"
IDLE = "root agent idle; waiting up to 29m10s for 1 background task(s)\n"
UNAVAILABLE = "Error: UNAVAILABLE (code 503)\n"


class AgyDispatchCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-fix-agy3-")
        root = Path(self._tmp.name)
        for p in (patch.object(config, "CONFIG_DIR", root / "cfg"),
                  patch.object(health, "STATUS_CACHE_FILE", root / "cfg" / "status.json"),
                  patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json")):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        (root / "cfg").mkdir(parents=True)

    def dispatch(self, out, err, code=1, stream=False, runner=None):
        """execute_agy_task with run_subprocess replaced: by default a double that
        returns plain strings (stream mode: the historical merged contract)."""
        runner = runner or (lambda *args, **kwargs: (code, out, err, None))
        with patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("makewand.sandbox._agy_host_credits_disabled", return_value=True), \
             patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(True, "config-ack")), \
             patch("makewand.sandbox.audit_unsafe_host_exec"), \
             patch.object(agy, "run_subprocess", side_effect=runner), \
             contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            result = execute_agy_task("审查这段代码", cwd=self._tmp.name, readonly=True, stream=stream)
        status = health.load_status_cache()["agy"]["status"]
        if health.STATUS_CACHE_FILE.exists():
            health.STATUS_CACHE_FILE.unlink()
        return result, status


class TestInvocationRejectionBehindAnyNotice(AgyDispatchCase):
    def assert_known_rejection(self, result):
        self.assertEqual(getattr(result, "status", None), "FAILED")
        self.assertEqual(getattr(result, "error_kind", None), "provider_configuration")
        self.assertTrue(getattr(result, "outcome_known", False))
        self.assertNotIn("warning", result[2].lower())

    def test_rejection_after_each_notice(self):
        for name, notice in NOTICES.items():
            for rejection in (REJ, UNK):
                with self.subTest(notice=name, rejection=rejection[:30]):
                    result, _ = self.dispatch("", STARTUP + K + notice + rejection)
                    self.assert_known_rejection(result)
                    result, _ = self.dispatch(STARTUP + K + notice + rejection, "", stream=True)
                    self.assert_known_rejection(result)

    def test_rejection_after_several_notices(self):
        notices = NOTICES["mode_noeffect"] + NOTICES["credentials"] + NOTICES["environment"]
        result, _ = self.dispatch("", K + notices + REJ)
        self.assert_known_rejection(result)

    def test_tool_and_model_text_is_not_a_rejection(self):
        for out, err in (("Ran tar:\ntar: unknown option -- strip\nPartial changes written.\n", "Error: exit status 2\n"),
                         ("Error: unknown flag: --json -- your gh is too old, so I rewrote scripts/ci.sh.\n", ""),
                         ('error: invalid model selection (--model "a" --effort "b") is what the CLI prints\n',
                          UNAVAILABLE)):
            with self.subTest(out=out[:40]):
                result, _ = self.dispatch(out, err)
                self.assertNotEqual(getattr(result, "error_kind", None), "provider_configuration")
                self.assertFalse(getattr(result, "outcome_known", False))
        result, _ = self.dispatch(NOTICES["mode_noeffect"] + "Ran tar:\ntar: unknown option -- strip\n", "",
                                  stream=True)
        self.assertNotEqual(getattr(result, "error_kind", None), "provider_configuration")


class TestMergedStreamAnswer(AgyDispatchCase):
    def answer(self, merged):
        return self.dispatch(merged, "", code=0, stream=True)[0]

    def test_notices_and_diagnostics_alone_are_no_answer(self):
        for merged in [K + notice for notice in NOTICES.values()] + [
                K + NO_OUTPUT,
                K + NOTICES["mode_noeffect"] + NOTICES["credentials"] + IDLE,
                "W1012 08:12:30.100000     188 retry.go:88] attempt 1 failed, retrying:\n"
                "RESOURCE_EXHAUSTED: Resource has been exhausted (e.g. check quota).\n"]:
            with self.subTest(merged=merged[-70:]):
                ok, text, message = self.answer(merged)
                self.assertFalse(ok)
                self.assertIsNone(text)
                self.assertIn("未能产生有效输出", message)

    def test_notices_are_not_part_of_the_answer(self):
        for name, notice in NOTICES.items():
            with self.subTest(notice=name):
                self.assertEqual(self.answer(notice + K + "The fix is in bank.py.\n" + IDLE),
                                 (True, "The fix is in bank.py.", None))

    def test_the_value_a_glog_line_logs_is_not_the_answer(self):
        merged = ("W1012 08:12:30.100000     188 retry.go:88] attempt 1 failed, retrying:\n"
                  "RESOURCE_EXHAUSTED: Resource has been exhausted (e.g. check quota).\nThe fix is in bank.py.\n")
        self.assertEqual(self.answer(merged), (True, "The fix is in bank.py.", None))

    def test_code_after_a_truncated_logged_body_stays(self):
        merged = ("E1012 09:00:00.000000  151 http_helpers.go:418] body: {\n  \"error\": {\n"
                  "    def foo():\n        return 1\nDone.\n")
        ok, text, _ = self.answer(merged)
        self.assertTrue(ok)
        self.assertEqual(text, "def foo():\n        return 1\nDone.")

    def test_model_prose_starting_with_warning_stays(self):
        merged = K + "Warning: this changes the public API.\nThe fix is in bank.py.\n"
        self.assertEqual(self.answer(merged)[1], "Warning: this changes the public API.\nThe fix is in bank.py.")


class TestMergedStreamFailureVerdict(AgyDispatchCase):
    ANSWERS = [
        "Error: UNAUTHENTICATED: the interceptor now returns this for missing tokens.",
        "Resource has been exhausted (e.g. check quota).",
        "Request had invalid authentication credentials. The fixture now returns that.",
        "You have exhausted your quota on this model.",
        "UNAUTHENTICATED: returned by the new interceptor when the bearer token is missing.",
        "Error: Gemini Nano is not supported in your region, so I gated the feature.",
        "Google Pay is not available in your country, so checkout falls back to cards.",
    ]

    def test_answer_lines_before_agys_final_error_are_not_its_verdict(self):
        for line in self.ANSWERS:
            with self.subTest(line=line):
                result, status = self.dispatch(K + f"Partial answer.\n{line}\n" + UNAVAILABLE, "", stream=True)
                self.assertFalse(result[0])
                self.assertEqual(status, "unknown")

    def test_agys_own_final_error_still_counts(self):
        for err, expected in (("Error: RESOURCE_EXHAUSTED (code 429): You have exhausted your quota on this model.\n",
                               "limited"),
                              ("Error: UNAUTHENTICATED: Request had invalid authentication credentials.\n",
                               "needs_auth"),
                              ("Error: rpc error: code = FailedPrecondition desc = User location is not supported for "
                               "the API use.\n", "error")):
            with self.subTest(err=err[:40]):
                _, status = self.dispatch(K + "Partial answer.\n" + err, "", stream=True)
                self.assertEqual(status, expected)


class TestGlogContinuationWithoutColon(AgyDispatchCase):
    def test_plain_line_after_a_glog_header_is_not_agys_verdict(self):
        for header in ("W1012 08:12:30.100000     188 retry.go:88] attempt 1 failed, retrying in 2s\n",
                       "E1012 08:12:30.100000     188 client.go:120] request failed with status 429\n"):
            for body in ("RESOURCE_EXHAUSTED: Resource has been exhausted (e.g. check quota).\n",
                         "Resource has been exhausted (e.g. check quota).\n"):
                with self.subTest(header=header[-30:], body=body[:20]):
                    err = header + body + UNAVAILABLE
                    self.assertFalse(parse_agy_quota("", err)[0])
                    result, status = self.dispatch("", err)
                    self.assertFalse(result[0])
                    self.assertEqual(status, "unknown")

    def test_region_block_after_the_keyring_line_still_counts(self):
        status, _, ttl = classify_agy_failure("", K + "Gemini Code Assist is not currently available in your "
                                                      "location.\n")
        self.assertEqual((status, ttl), ("error", health.REGION_BLOCK_TTL_SECONDS))


class TestRealRunnerKeepsAgyStderrApart(AgyDispatchCase):
    def runner(self, code, out, err, calls):
        script = ("import sys\nsys.stdout.write(%r)\nsys.stdout.flush()\nsys.stderr.write(%r)\nsys.exit(%d)\n"
                  % (out, err, code))

        def run(cmd, **kwargs):
            calls.append(kwargs)
            return base.run_subprocess([sys.executable, "-I", "-c", script], **kwargs)
        return run

    def test_stream_answer_is_stdout_and_notices_stay_on_stderr(self):
        calls = []
        result, _ = self.dispatch(None, None, stream=True, runner=self.runner(
            0, "The fix is in bank.py.\n", K + NOTICES["mode_noeffect"] + IDLE, calls))
        self.assertTrue(calls[0]["separate_stderr"])
        self.assertEqual(result, (True, "The fix is in bank.py.", None))
        result, _ = self.dispatch(None, None, stream=True, runner=self.runner(0, "", K + NOTICES["mode_noeffect"], []))
        self.assertFalse(result[0])

    def test_stream_failure_reads_agys_stderr(self):
        answer = "Partial answer.\nYou have exhausted your quota on this model.\n"
        _, status = self.dispatch(None, None, stream=True, runner=self.runner(1, answer, K + UNAVAILABLE, []))
        self.assertEqual(status, "unknown")
        result, _ = self.dispatch(None, None, stream=True, runner=self.runner(
            1, "", K + NOTICES["mode_noeffect"] + REJ, []))
        self.assertEqual(getattr(result, "error_kind", None), "provider_configuration")


if __name__ == "__main__":
    unittest.main()
