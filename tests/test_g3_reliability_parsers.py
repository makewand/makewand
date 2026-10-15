"""
G3 reliability regressions: rate-limit detection must use context, not a bare "429".

Covers py-reliability#10 (agy substring "429") and replay-F01-F06-adaptive#12
(grok bare \\b429\\b), aligned with the codex/claude parsers.
"""

import unittest

import makewand.providers.agy as agy
from makewand.providers.agy import parse_agy_quota
from makewand.providers.claude import parse_claude_quota
from makewand.providers.codex import parse_codex_quota
from makewand.providers.grok import parse_grok_quota

NOT_RATE_LIMITS = [
    "Wrote 429 lines to src/app.py",
    "commit a429f1 created",
    "PID 14290 finished",
    "429 tests passed in 3.2s",
    "panic: runtime error: index out of range at main.go line 429",
    "compilation failed: 429 errors",
    "1429 warnings emitted",
    "sha256 9f429ab3c4d5 mismatch",
    "test_rate_limit_middleware passed",
]

# Each CLI's own refusals (strings of the installed binaries, see
# health.CLI_MESSAGE_CATALOG). A generic "Too many requests" or "status code: 429"
# line is no longer enough: it is just as likely tool output relayed into the
# transcript (tests/test_fix_health_quota_noise.py). A Python
# "google.api_core.exceptions.ResourceExhausted" traceback line is tool output too:
# agy and grok are not Python programs.
AGY_LIMITS = [
    "Error: RESOURCE_EXHAUSTED: Resource has been exhausted (e.g. check quota).",
    "Error: You have exhausted your quota on this model.",
    "Error: rpc error: code = ResourceExhausted desc = Resource has been exhausted (e.g. check quota).",
    "Error: googleapi: Error 429: Quota exceeded for quota metric 'Generate Content API requests per minute', "
    "rateLimitExceeded",
    "Quota exceeded for aiplatform.googleapis.com",
    "HTTP 429 Too Many Requests",
]
GROK_LIMITS = [
    "API error (status 429): You\u2019ve hit your team\u2019s API rate limit. Ask a team admin to purchase more credits "
    "for higher limits, or try again later.",
    "Error: API error (status 429): request rejected",
    "Error: You\u2019ve reached your free Grok Build usage limit for now. Get SuperGrok for much higher limits, or try "
    "again later: https://grok.com/supergrok?referrer=grok-build",
    "You hit your free usage limit.",
    "Error: failed after 3 retries: request error: Rate limited (429)",
    "HTTP 429 Too Many Requests",
]


class TestContextAnchored429(unittest.TestCase):
    def test_agy_ignores_incidental_429(self):
        for text in NOT_RATE_LIMITS:
            with self.subTest(text=text):
                self.assertFalse(parse_agy_quota(text)[0])

    def test_agy_detects_real_limits(self):
        for text in AGY_LIMITS:
            with self.subTest(text=text):
                self.assertTrue(parse_agy_quota(text)[0])

    def test_grok_ignores_incidental_429(self):
        for text in NOT_RATE_LIMITS:
            with self.subTest(text=text):
                self.assertFalse(parse_grok_quota(text)[0])

    def test_grok_detects_real_limits_and_auth(self):
        for text in GROK_LIMITS:
            with self.subTest(text=text):
                limited, reason, _ = parse_grok_quota(text)
                self.assertTrue(limited)
                self.assertNotIn("登录", reason)
        for text in ("Unauthorized (401)", "missing xAI credentials",
                     "Authentication failed. Run `grok login`, set XAI_API_KEY, or add api_key to ~/.grok/config.toml."):
            with self.subTest(text=text):
                self.assertEqual(parse_grok_quota(text)[2], "需登录授权")

    def test_grok_auth_needs_context(self):
        self.assertFalse(parse_grok_quota("return http.StatusUnauthorized // unauthorizedHandler")[0])

    def test_codex_and_claude_remain_aligned(self):
        for text in NOT_RATE_LIMITS:
            with self.subTest(text=text):
                self.assertFalse(parse_codex_quota(text)[0])
                self.assertFalse(parse_claude_quota(text)[0])


class TestAgyFailureClassification(unittest.TestCase):
    def test_region_and_auth(self):
        classify_agy_failure = agy.classify_agy_failure
        self.assertEqual(classify_agy_failure("Error: not currently available in your location")[0], "error")
        self.assertEqual(classify_agy_failure("User location is not supported for the API use.")[0], "error")
        self.assertEqual(classify_agy_failure("UNAUTHENTICATED: request had invalid authentication credentials")[0], "needs_auth")
        self.assertIsNone(classify_agy_failure("AssertionError: expected 3 got 4"))

    def test_agy_argument_rejection_is_recognized(self):
        msg = ('error: invalid model selection (--model "gemini-3.1-pro" --effort "max"): '
               'gemini-3.1-pro has no "max" effort (available: low, high)')
        self.assertTrue(agy._agy_invocation_rejection(agy._agy_user_facing(msg)))
        self.assertIsNone(agy._agy_invocation_rejection("AssertionError: expected 3 got 4"))

    def test_agy_startup_glog_is_not_an_auth_failure(self):
        # Every agy start logs this before silent keyring auth succeeds; a task
        # failure carrying it in stderr must not mark agy as needing re-login.
        stderr = (
            "E1012 08:11:57.633062     188 errorreport.go:224] error getting token source: You are not logged into Antigravity.\n"
            "I1012 08:11:57.843790       1 printmode.go:403] Print mode: not authenticated, trying silent auth\n"
            "I1012 08:11:57.965555       1 auth.go:157] ChainedAuth: authenticated via keyring (effective: keyring)\n"
            "W1012 08:12:30.100000     188 retry.go:88] got 429 RESOURCE_EXHAUSTED, retrying\n"
            "Task failed: timed out waiting for model\n"
        )
        self.assertIsNone(agy.classify_agy_failure(stderr))
        self.assertFalse(parse_agy_quota(stderr)[0])
        self.assertEqual(agy.classify_agy_failure("Error: authentication required. Run 'agy' to log in.")[0], "needs_auth")


if __name__ == "__main__":
    unittest.main()
