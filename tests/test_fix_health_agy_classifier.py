"""
py-rel-22: agy output classification is anchored to agy's own output.

- multi-line glog bodies (a JSON response logged by an "I1012 ...] ..." line, or
  an error logged with %v that starts on the next line) are diagnostics, never
  a RESOURCE_EXHAUSTED / UNAUTHENTICATED verdict;
- a successful keyring load ("authenticated via keyring") does not suppress a
  backend rejection that follows it;
- region blocks arrive wrapped by grpc-go ("rpc error: code = FailedPrecondition
  desc = User location is not supported ..."), PermissionDenied or with a long
  subject and must keep pausing agy with the 6 h TTL;
- an argument rejection is agy's first own line even when agy's start-up
  notices ("warning: --mode plan is not supported in print mode ...", the
  GOOGLE_API_KEY notice) or ANSI colour come first; model or tool text ("tar:
  unknown option", an answer opening with "Error: unknown flag: --json -- ...")
  is not an invocation error;
- on exit 0 the answer is stdout (non-stream, untouched) or the merged stream
  without agy's own glog/status/notice lines; stderr is never an answer and a
  run without an answer is a failure;
- on a non-zero exit only agy's own lines (stderr) are classified.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
from makewand.providers import agy
from makewand.providers.agy import (classify_agy_failure, execute_agy_task, parse_agy_quota,
                                    _agy_invocation_rejection, _agy_user_facing)

KEYRING_OK = "I1012 08:11:57.965555       1 auth.go:157] ChainedAuth: authenticated via keyring (effective: keyring)\n"
STARTUP = ("E1012 08:11:57.633062     188 errorreport.go:224] error getting token source: You are not logged into "
           "Antigravity.\n"
           "I1012 08:11:57.843790       1 printmode.go:403] Print mode: not authenticated, trying silent auth\n")
GLOG_429_BODY = (
    "I1012 09:00:00.000000  151 http_helpers.go:418] Failed to make code assist backend request (x): {\n"
    "  \"error\": {\n"
    "    \"code\": 429,\n"
    "    \"message\": \"Resource has been exhausted\",\n"
    "    \"status\": \"RESOURCE_EXHAUSTED\"\n"
    "  }\n"
    "}\n"
)
GLOG_401_BODY = (
    "I1012 09:00:00.000000  151 http_helpers.go:418] Failed to make code assist backend request (x): {\n"
    "  \"error\": {\n"
    "    \"code\": 401,\n"
    "    \"status\": \"UNAUTHENTICATED\"\n"
    "  }\n"
    "}\n"
)
# Go's multi-line %v: the logged error starts on the next, unindented line.
GLOG_CONTINUED = ("W1012 08:12:30.100000     188 retry.go:88] attempt 1 failed, retrying:\n"
                  "RESOURCE_EXHAUSTED: Resource has been exhausted (e.g. check quota).\n")
RETRIED_OK = "Error: UNAVAILABLE (code 503)\n"
STATUS_LINE = "root agent idle; waiting up to 29m10s for 1 background task(s)\n"
# Notices agy prints itself (format strings of the agy binary).
MODE_WARNING = "warning: --mode plan is not supported in print mode; continuing in the default mode.\n"
ENV_WARNING = "Warning: Both GOOGLE_API_KEY and GEMINI_API_KEY are set. Using GOOGLE_API_KEY.\n"
REJECTION = ('error: invalid model selection (--model "gemini-3.1-pro" --effort "max"): '
             'gemini-3.1-pro has no "max" effort (available: low, high)\n')

REGION_BLOCKS = [
    "Error: failed to send message: rpc error: code = FailedPrecondition desc = User location is not supported for the "
    "API use.",
    "Error: rpc error: code = PermissionDenied desc = Gemini Code Assist is not currently available in your location.",
    "Error: Antigravity for Google AI Pro subscribers is not currently available in your location.",
    "Error: Gemini 3.1 Pro is not available in your country.",
    "Error: googleapi: Error 400: User location is not supported for the API use., failedPrecondition",
    "Error: Error 400, Message: User location is not supported for the API use., Status: FAILED_PRECONDITION",
    "Gemini Code Assist is not currently available in your location.",
    "User location is not supported for the API use.",
]


def invocation_rejected(text):
    return _agy_invocation_rejection(_agy_user_facing(text)) is not None


class TestGlogBodiesAreDiagnostics(unittest.TestCase):
    def test_multi_line_glog_body_is_not_a_quota_or_auth_verdict(self):
        self.assertFalse(parse_agy_quota(GLOG_429_BODY + RETRIED_OK)[0])
        self.assertIsNone(classify_agy_failure(GLOG_429_BODY + RETRIED_OK))
        self.assertIsNone(classify_agy_failure(GLOG_401_BODY + RETRIED_OK))

    def test_unindented_glog_continuation_is_not_classified(self):
        self.assertFalse(parse_agy_quota(GLOG_CONTINUED + RETRIED_OK)[0])
        self.assertIsNone(classify_agy_failure(GLOG_CONTINUED + RETRIED_OK))
        # agy's own error after the logged retry still counts.
        self.assertTrue(parse_agy_quota(GLOG_CONTINUED + "Error: You have exhausted your quota on this model.\n")[0])

    def test_truncated_glog_body_does_not_swallow_agys_error(self):
        truncated = "I1012 09:00:00.000000  151 http_helpers.go:418] body: {\n  \"error\": {\n    \"code\": 401,\n\n"
        self.assertEqual(classify_agy_failure(truncated + "Error: authentication required. Run 'agy' to log in, "
                                                          "then retry.\n")[0], "needs_auth")

    def test_user_facing_output_never_contains_glog_lines_or_bodies(self):
        text = _agy_user_facing(KEYRING_OK + GLOG_429_BODY + RETRIED_OK + STATUS_LINE + MODE_WARNING)
        self.assertEqual(text, "Error: UNAVAILABLE (code 503)")

    def test_answer_after_glog_body_is_kept(self):
        self.assertEqual(_agy_user_facing(GLOG_429_BODY + "The fix is in bank.py.\n"), "The fix is in bank.py.")
        self.assertEqual(_agy_user_facing(KEYRING_OK + "    def foo():\n        return 1\nDone.\n"),
                         "    def foo():\n        return 1\nDone.")


class TestBackendRejectionsAfterKeyringLoad(unittest.TestCase):
    def test_backend_auth_rejection_counts_although_keyring_loaded(self):
        for stderr in ("Error: UNAUTHENTICATED: Request had invalid authentication credentials.\n",
                       "Error: oauth2: \"invalid_grant\" \"Token has been expired or revoked.\"\n",
                       "Error: failed to send message: rpc error: code = Unauthenticated desc = Request had invalid "
                       "authentication credentials.\n",
                       "Error: authentication failed: oauth2: cannot fetch token: 400 Bad Request\n"):
            with self.subTest(stderr=stderr):
                self.assertEqual(classify_agy_failure(KEYRING_OK + stderr)[0], "needs_auth")

    def test_startup_notice_is_moot_once_silent_auth_succeeded(self):
        startup = "You are not logged into Antigravity.\n"
        self.assertIsNone(classify_agy_failure(startup + KEYRING_OK + "Task failed: timed out waiting for model\n"))
        self.assertEqual(classify_agy_failure("Error: You are not logged into Antigravity. Run `agy` to sign in.\n")[0],
                         "needs_auth")

    def test_answer_lines_are_not_a_login_failure(self):
        for text in ("UNAUTHENTICATED: returned by the new interceptor when the bearer token is missing.\n",
                     "oauth2: token refresh now retries when the access token expired.\n",
                     KEYRING_OK + "Docs: users see 'You are not logged into Antigravity' until they sign in.\n"):
            with self.subTest(text=text):
                self.assertIsNone(classify_agy_failure(text))


class TestRegionBlocks(unittest.TestCase):
    def test_region_blocks_in_agys_own_wrappers(self):
        for line in REGION_BLOCKS:
            with self.subTest(line=line):
                status, _, ttl = classify_agy_failure(KEYRING_OK + line + "\n")
                self.assertEqual(status, "error")
                self.assertEqual(ttl, health.REGION_BLOCK_TTL_SECONDS)

    def test_region_words_in_the_answer_are_not_a_block(self):
        for text in ("The API returns 'User location is not supported' for some users, so I added a fallback.\n",
                     "Note: the feature is not available in your region yet, the docs say.\n"):
            with self.subTest(text=text):
                self.assertIsNone(classify_agy_failure(text))


class TestInvalidInvocationIsAnchored(unittest.TestCase):
    def test_cli_rejection_is_recognised(self):
        for text in ("\n" + KEYRING_OK + REJECTION,
                     STARTUP + KEYRING_OK + REJECTION,
                     KEYRING_OK + MODE_WARNING + REJECTION,
                     ENV_WARNING + KEYRING_OK + REJECTION,
                     "\nError: unknown flag: --effort\nUsage:\n  agy [flags]\n",
                     "Error: unknown flag: --effort\r\nUsage:\r\n  agy [flags]\r\n",
                     "\x1b[31mError:\x1b[0m unknown flag: --effort\n"):
            with self.subTest(text=text):
                self.assertTrue(invocation_rejected(text))

    def test_model_or_tool_text_is_not_an_invocation_error(self):
        for text in ("Ran `tar -xzf a.tgz --strip`: tar: unknown option -- strip\nPartial changes written.\n",
                     "I edited cli.py.\nerror: unknown flag: --json (from the gh call)\n",
                     "Note: the API reports invalid model selection for old ids.\n",
                     "Error: unknown flag: --json -- your gh is too old, so I rewrote scripts/ci.sh without it.\n"
                     "Partial changes written.\n"):
            with self.subTest(text=text):
                self.assertFalse(invocation_rejected(text))


class AgyDispatchTestCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-fix-agy-")
        root = Path(self._tmp.name)
        self.status_file = root / "cfg" / "status.json"
        for p in (patch.object(config, "CONFIG_DIR", root / "cfg"),
                  patch.object(health, "STATUS_CACHE_FILE", self.status_file),
                  patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json")):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        (root / "cfg").mkdir(parents=True)

    def dispatch(self, out, err, code=1, stream=False):
        with patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(True, "config-ack")), \
             patch("makewand.sandbox.audit_unsafe_host_exec"), \
             patch.object(agy, "run_subprocess", return_value=(code, out, err, None)):
            return execute_agy_task("审查这段代码", cwd=self._tmp.name, readonly=True, stream=stream)

    def status(self):
        return health.load_status_cache()["agy"]["status"]


class TestDispatchWriteBack(AgyDispatchTestCase):
    """The classifiers drive execute_agy_task's status write-back and result kind."""

    def test_glog_body_does_not_mark_agy_limited(self):
        self.dispatch("", KEYRING_OK + GLOG_429_BODY + RETRIED_OK)
        self.assertEqual(self.status(), "unknown")

    def test_backend_rejection_after_keyring_marks_needs_auth(self):
        self.dispatch("", KEYRING_OK + "Error: UNAUTHENTICATED: Request had invalid authentication credentials.\n")
        self.assertEqual(self.status(), "needs_auth")

    def test_wrapped_region_block_pauses_agy_with_the_region_ttl(self):
        result = self.dispatch("", KEYRING_OK + REGION_BLOCKS[0] + "\n")
        self.assertFalse(result[0])
        entry = health.load_status_cache()["agy"]
        self.assertEqual(entry["status"], "error")
        self.assertEqual(entry.get("ttl_seconds"), health.REGION_BLOCK_TTL_SECONDS)

    def test_answer_lines_on_stdout_are_not_classified(self):
        for out in ("Rate limit exceeded on the GitHub API, so I stopped after two files.\n",
                    "Too many requests to the registry; I pinned versions instead.\n",
                    "UNAUTHENTICATED: returned by the new interceptor when the bearer token is missing.\n",
                    "Error: You have exhausted your quota on this model. (quoting the API docs)\n"):
            with self.subTest(out=out):
                result = self.dispatch(out, KEYRING_OK + "Error: exit status 1\n")
                self.assertFalse(result[0])
                self.assertEqual(self.status(), "unknown")

    def test_tool_text_is_not_a_known_invocation_failure(self):
        for out in ("Ran `tar -xzf a.tgz --strip`: tar: unknown option -- strip\nPartial changes written.\n",
                    "Error: unknown flag: --json -- your gh is too old, so I rewrote scripts/ci.sh without it.\n"):
            with self.subTest(out=out):
                result = self.dispatch(out, "")
                self.assertFalse(result[0])
                self.assertNotEqual(getattr(result, "error_kind", None), "provider_configuration")
                self.assertFalse(getattr(result, "outcome_known", False))

    def test_cli_argument_rejection_is_a_known_failure(self):
        for err in (KEYRING_OK + REJECTION, KEYRING_OK + MODE_WARNING + REJECTION, ENV_WARNING + KEYRING_OK + REJECTION,
                    "\x1b[31mError:\x1b[0m unknown flag: --effort\nUsage:\n  agy [flags]\n"):
            with self.subTest(err=err):
                result = self.dispatch("", err)
                self.assertEqual(getattr(result, "status", None), "FAILED")
                self.assertEqual(getattr(result, "error_kind", None), "provider_configuration")
                self.assertTrue(getattr(result, "outcome_known", False))
                self.assertNotIn("I1012", result[2])
                self.assertNotIn("warning", result[2].lower())


class TestExitZeroAnswer(AgyDispatchTestCase):
    def test_non_stream_answer_is_stdout_untouched(self):
        answer = "The log shows:\n```\n" + KEYRING_OK + "```\nSo keyring auth worked."
        self.assertEqual(self.dispatch(answer + "\n", KEYRING_OK + MODE_WARNING, code=0), (True, answer, None))

    def test_stderr_is_never_the_answer(self):
        for err in (KEYRING_OK + "I1012 08:12:00.000000       1 printmode.go:500] Print mode: silent auth succeeded\n",
                    KEYRING_OK + MODE_WARNING, "Some plain text agy printed to stderr\n"):
            with self.subTest(err=err):
                ok, text, message = self.dispatch("", err, code=0)
                self.assertFalse(ok)
                self.assertIsNone(text)
                self.assertIn("未能产生有效输出", message)

    def test_stream_answer_drops_only_agys_own_lines(self):
        merged = KEYRING_OK + MODE_WARNING + "The fix is in bank.py.\n" + GLOG_429_BODY + STATUS_LINE
        self.assertEqual(self.dispatch(merged, "", code=0, stream=True), (True, "The fix is in bank.py.", None))
        # The model's indented code right after a log line stays.
        merged = "Patch:\n```python\ndef f():\n" + KEYRING_OK + "    return 1\n```\nDone.\n"
        self.assertEqual(self.dispatch(merged, "", code=0, stream=True)[1],
                         "Patch:\n```python\ndef f():\n    return 1\n```\nDone.")

    def test_stream_without_an_answer_is_a_failure(self):
        ok, text, _ = self.dispatch(KEYRING_OK + MODE_WARNING + STATUS_LINE, "", code=0, stream=True)
        self.assertFalse(ok)
        self.assertIsNone(text)


if __name__ == "__main__":
    unittest.main()
