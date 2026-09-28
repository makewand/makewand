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

RATE_LIMITS = [
    "Error: 429 Too Many Requests",
    "HTTP 429",
    "status code: 429",
    "google.api_core.exceptions.ResourceExhausted: RESOURCE_EXHAUSTED",
    "rate limit exceeded, retry later",
    "Too many requests",
]


class TestContextAnchored429(unittest.TestCase):
    def test_agy_ignores_incidental_429(self):
        for text in NOT_RATE_LIMITS:
            with self.subTest(text=text):
                self.assertFalse(parse_agy_quota(text)[0])

    def test_agy_detects_real_limits(self):
        for text in RATE_LIMITS + ["Quota exceeded for aiplatform.googleapis.com"]:
            with self.subTest(text=text):
                self.assertTrue(parse_agy_quota(text)[0])

    def test_grok_ignores_incidental_429(self):
        for text in NOT_RATE_LIMITS:
            with self.subTest(text=text):
                self.assertFalse(parse_grok_quota(text)[0])

    def test_grok_detects_real_limits_and_auth(self):
        for text in RATE_LIMITS + ["usage limit reached"]:
            with self.subTest(text=text):
                limited, reason, _ = parse_grok_quota(text)
                self.assertTrue(limited)
                self.assertNotIn("登录", reason)
        for text in ("Error: 401 Unauthorized", "missing xAI credentials", "login required"):
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


if __name__ == "__main__":
    unittest.main()
