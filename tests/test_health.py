"""
Unit tests for health monitoring and quota parsers.
"""

import unittest
from makewand.providers.claude import parse_claude_quota
from makewand.providers.codex import parse_codex_quota
from makewand.providers.agy import parse_agy_quota
from makewand.providers.muse import parse_muse_quota
from makewand.health import load_status_cache, save_status_cache, is_reset_time_passed

class TestHealth(unittest.TestCase):
    def test_claude_quota_parser(self):
        output = "You've hit your monthly spend limit · raise it at claude.ai · your weekly limit resets 8pm (Asia/Shanghai)"
        limited, reason, resets = parse_claude_quota(output)
        self.assertTrue(limited)
        self.assertIn("8pm", resets)

        # 429 Rate limit should generate an ISO timestamp ~15 min in the future
        output_429 = "rate_limit_error: 429 Too Many Requests"
        limited_429, reason_429, resets_429 = parse_claude_quota(output_429)
        self.assertTrue(limited_429)
        self.assertIsNotNone(resets_429)
        self.assertFalse(is_reset_time_passed(resets_429))

        ok_out = "ok"
        limited, _, _ = parse_claude_quota(ok_out)
        self.assertFalse(limited)

    def test_codex_quota_parser(self):
        output = "You have hit your usage limit. Try again at 10:58 AM."
        limited, reason, resets = parse_codex_quota(output)
        self.assertTrue(limited)
        self.assertIn("10:58 AM", resets)

        # 429 Rate limit
        output_429 = "Error: 429 too many requests"
        limited_429, reason_429, resets_429 = parse_codex_quota(output_429)
        self.assertTrue(limited_429)
        self.assertIsNotNone(resets_429)
        self.assertFalse(is_reset_time_passed(resets_429))

        ok_out = "OpenAI Codex v0.155.1\nsucceeded"
        limited, _, _ = parse_codex_quota(ok_out)
        self.assertFalse(limited)

    def test_agy_quota_parser(self):
        output = "Error: ResourceExhausted: 429 Resource has been exhausted"
        limited, reason, _ = parse_agy_quota(output)
        self.assertTrue(limited)

        ok_out = "AGY_HEADLESS_OK"
        limited, _, _ = parse_agy_quota(ok_out)
        self.assertFalse(limited)

    def test_muse_quota_parser(self):
        output = "missing meta credentials: run `muse login` or set META_API_KEY"
        limited, reason, resets = parse_muse_quota(output)
        self.assertTrue(limited)
        self.assertEqual(resets, "需登录授权")

        # 429 Rate limit
        output_429 = "429 rate limit exceeded"
        limited_429, reason_429, resets_429 = parse_muse_quota(output_429)
        self.assertTrue(limited_429)
        self.assertIsNotNone(resets_429)
        self.assertFalse(is_reset_time_passed(resets_429))

    def test_status_cache_io(self):
        cache = load_status_cache()
        self.assertIn("agy", cache)
        self.assertIn("claude", cache)
        self.assertIn("codex", cache)
        self.assertIn("muse", cache)

    def test_reset_time_passed(self):
        from datetime import datetime, timedelta
        # 10:58 AM with morning updated_at is past in late afternoon
        self.assertTrue(is_reset_time_passed("10:58 AM", "2026-09-20T10:00:00"))
        # 4 hours expired fallback
        self.assertTrue(is_reset_time_passed(None, "2026-09-20T10:00:00"))
        # Past ISO timestamp
        self.assertTrue(is_reset_time_passed("2026-09-20T00:00:00"))
        # Future ISO timestamp
        self.assertFalse(is_reset_time_passed("2099-01-01T00:00:00"))

        # Future time today
        future_time = (datetime.now() + timedelta(hours=2)).strftime("%I:%M %p")
        recent_up = (datetime.now() - timedelta(minutes=10)).isoformat()
        self.assertFalse(is_reset_time_passed(future_time, recent_up))

        # Overnight rollover: quota logged 10 min ago, reset time of day is 30 min ago
        # which means next reset is tomorrow at that time. Should NOT be considered passed.
        overnight_time = (datetime.now() - timedelta(minutes=30)).strftime("%I:%M %p")
        self.assertFalse(is_reset_time_passed(overnight_time, recent_up))

if __name__ == "__main__":
    unittest.main()
