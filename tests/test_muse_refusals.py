"""Meta's own refusals of a Muse Code call are known failures, not unknown outcomes.

Between 11-23 and 11-27 Muse Code ended eleven dispatches on this host with one of two
lines, each recorded as UNKNOWN (exit 17: "may have been spent, ask before retrying"):

    agent loop failed: model failed: API error 402 [request_id=...]: Billing verification failed. ...
    agent loop failed: model failed: model `muse-wonder-full-1.0` does not exist or you lack access [request_id=...]

Meta sends both before any model runs.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import tempfile
import unittest
from contextlib import contextmanager
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import makewand.health as health
from makewand.providers.muse import execute_muse_task, muse_model_refusal, parse_muse_quota

REQUEST = "[request_id=6ae80c60-0b5f-46dc-9a5d-4f27aa01bb02]"
BILLING = ("agent loop failed: model failed: API error 402 " + REQUEST + ": Billing verification failed. "
           "Billing Issue. To use this API key, your team needs to resolve a billing issue first. To resolve "
           "this issue, go here: https://dev.meta.ai/billing/?team_id=1 (billing_error)\n")
NO_ACCESS = "agent loop failed: model failed: model `muse-wonder-full-1.0` does not exist or you lack access " + REQUEST + "\n"
QUOTA = ("agent loop failed: model failed: API error 429 " + REQUEST
         + ": Subscription quota exhausted. Resets at 2026-11-16 08:00.\n")
TRANSPORT = ("agent loop failed: model failed after 2 attempts: transport error [body-decode]: response body "
             "could not be decoded (cause unclassified) (meta stream, 420 KiB received, 74s)\n")


class MuseRefusalLinesTests(unittest.TestCase):
    def test_a_billing_refusal_is_a_limit_of_the_whole_subscription(self):
        for output, stderr in (("", BILLING), (BILLING, None), ("", "retrying...\n" + BILLING)):
            limited, reason, reset = parse_muse_quota(output, stderr)
            self.assertTrue(limited)
            self.assertIn("402", reason)
            self.assertIn("账单", reason)
            self.assertNotIn("登录", reason)  # record_engine_limit files a reason with that word as a login failure
            self.assertEqual("待重置", reset)
            self.assertIn("quota", health.find_cli_messages("muse", ("quota",), output, stderr))
            self.assertIsNone(muse_model_refusal(output, stderr))

    def test_a_model_meta_does_not_offer_is_named(self):
        for output, stderr in (("", NO_ACCESS), (NO_ACCESS, None)):
            self.assertEqual("muse-wonder-full-1.0", muse_model_refusal(output, stderr))
            self.assertEqual((False, "", None), parse_muse_quota(output, stderr))
        retried = NO_ACCESS.replace("model failed:", "model failed after 3 attempts:")
        self.assertEqual("muse-wonder-full-1.0", muse_model_refusal("", retried))

    def test_other_lines_keep_their_meaning(self):
        limited, reason, reset = parse_muse_quota("", QUOTA)
        self.assertEqual((True, "2026-11-16 08:00"), (limited, reset))
        self.assertNotIn("402", reason)
        self.assertEqual((False, "", None), parse_muse_quota("", TRANSPORT))
        self.assertIsNone(muse_model_refusal("", TRANSPORT))

    def test_the_same_words_in_the_answer_are_task_content(self):
        answer = "The log I was asked about says:\n" + BILLING + NO_ACCESS + "Neither line is from this run.\n"
        self.assertEqual((False, "", None), parse_muse_quota(answer, ""))
        self.assertIsNone(muse_model_refusal(answer, ""))
        self.assertEqual((False, "", None), parse_muse_quota(answer, None))
        self.assertIsNone(muse_model_refusal(answer, None))
        said = "The model said: " + NO_ACCESS
        self.assertIsNone(muse_model_refusal(said, ""))


class MuseRefusalDispatchTests(unittest.TestCase):
    MODEL = "muse-wonder-full-1.0"

    def setUp(self):
        # Every test starts from its own empty status file (saving merges stored refusals).
        root = tempfile.TemporaryDirectory(prefix="mw-muse-status-")
        self.addCleanup(root.cleanup)
        cache = patch.object(health, "STATUS_CACHE_FILE", Path(root.name) / "status.json")
        cache.start()
        self.addCleanup(cache.stop)

    def dispatch(self, stderr):
        @contextmanager
        def prepare_sandbox(*args, **kwargs):
            yield SimpleNamespace(sandboxed=True, home=None, cmd=["muse"], pass_fds=(), env={})

        with tempfile.TemporaryDirectory(prefix="mw-muse-") as root, \
                patch("makewand.config.has_subscription_configured", return_value=True), \
                patch("makewand.providers.muse.detect_muse_guard", return_value=(False, None)), \
                patch("makewand.sandbox.is_bwrap_available", return_value=True), \
                patch("makewand.sandbox.provider_sandbox_launch", side_effect=prepare_sandbox), \
                patch("makewand.execution_runtime.mark_provider_invocation"), \
                patch("makewand.cli_failure_log.record") as unclassified, \
                patch("makewand.providers.muse.run_subprocess", return_value=(1, "", stderr, None)), \
                contextlib.redirect_stderr(io.StringIO()):
            result = execute_muse_task("fixture", cwd=root, model=self.MODEL, effort="high", readonly=True)
        return result, unclassified

    def stored(self):
        return health.load_status_cache().get("muse", {}).get("quota_limits") or []

    def test_a_billing_refusal_ends_as_a_known_failure_and_is_recorded(self):
        result, unclassified = self.dispatch(BILLING)
        self.assertIs(type(result), tuple)  # a plain failure: FAILED, outcome known; no UNKNOWN result object
        ok, _output, error = result
        self.assertFalse(ok)
        self.assertIn("402", error)
        self.assertIn("账单", error)
        unclassified.assert_not_called()
        rows = self.stored()
        self.assertEqual(1, len(rows))
        self.assertIsNone(rows[0]["quota_scope"]["model"])  # Meta refuses the account, not one model
        self.assertIn("402", rows[0]["reason"])
        self.assertEqual("limited", rows[0]["status"])

    def test_a_refused_model_ends_as_a_known_failure_recorded_for_that_model(self):
        result, unclassified = self.dispatch(NO_ACCESS)
        self.assertIs(type(result), tuple)
        ok, _output, error = result
        self.assertFalse(ok)
        self.assertIn(self.MODEL, error)
        self.assertIn("拒绝了这次调用", error)
        unclassified.assert_not_called()
        rows = self.stored()
        self.assertEqual([self.MODEL], [row["quota_scope"]["model"] for row in rows])

    def test_a_transport_error_is_still_unknown(self):
        result, unclassified = self.dispatch(TRANSPORT)
        self.assertEqual("UNKNOWN", result.status)
        self.assertFalse(result.outcome_known)
        unclassified.assert_called_once()
        self.assertEqual([], self.stored())


if __name__ == "__main__":
    unittest.main()
