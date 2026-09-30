"""
Unit tests for health monitoring and quota parsers.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

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

        # Proactive weekly warning (e.g. from session zainaqi)
        output_warn = "⚠ Heads up, you have less than 10% of your weekly limit left. Run /status for a breakdown. ⚠ weekly limit: 7% left · /status"
        limited_warn, reason_warn, resets_warn = parse_codex_quota(output_warn)
        self.assertTrue(limited_warn)
        self.assertIn("7%", reason_warn)

        # Reached usage limit with relative time
        output_reached = "You've reached your usage limit. Try again in 2 hours."
        limited_r, reason_r, resets_r = parse_codex_quota(output_reached)
        self.assertTrue(limited_r)
        self.assertIn("2 hours", resets_r)

        # Weekly limit reached
        output_weekly = "weekly limit reached · resets at 10-04 10:40"
        limited_w, reason_w, resets_w = parse_codex_quota(output_weekly)
        self.assertTrue(limited_w)
        self.assertIn("10-04 10:40", resets_w)

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

    def test_official_quota_reader(self):
        import os
        import json
        from datetime import datetime, timedelta, timezone
        from unittest.mock import patch
        from makewand.health import calculate_provider_quota
        cache_dir = os.path.expanduser("~/.cache/makewand")
        os.makedirs(cache_dir, exist_ok=True)
        cache_file = os.path.join(cache_dir, "quota-snapshot.json")
        codex_home = os.path.join(cache_dir, "official-quota-test-codex")
        now = datetime.now(timezone.utc)
        claude_reset = (now + timedelta(days=4)).isoformat().replace("+00:00", "Z")
        codex_reset = (now + timedelta(days=5)).isoformat().replace("+00:00", "Z")
        sample_snapshot = {
            "version": 1,
            "taken_at": now.isoformat().replace("+00:00", "Z"),
            "providers": [
                {
                    "Provider": "claude",
                    "FiveHourPct": 10,
                    "WeeklyPct": 75,
                    "ScopedPct": 100,
                    "Authed": True,
                    "HasData": True,
                    "WeeklyResetAt": claude_reset,
                    "ResetAt": claude_reset,
                },
                {
                    "Provider": "codex",
                    "FiveHourPct": None,
                    "WeeklyPct": 92,
                    "ScopedPct": None,
                    "Authed": True,
                    "HasData": True,
                    "CodexHome": codex_home,
                    "WeeklyResetAt": codex_reset,
                    "ResetAt": codex_reset,
                },
            ],
        }
        try:
            with open(cache_file, "w", encoding="utf-8") as f:
                json.dump(sample_snapshot, f)

            q_claude = calculate_provider_quota("claude", {"status": "healthy"})
            self.assertEqual(q_claude["source"], "official")
            self.assertEqual(q_claude["percentage"], 25)
            self.assertIn("官方报告", q_claude["desc"])
            self.assertIn("100%", q_claude["desc"])

            with patch.dict(os.environ, {"CODEX_HOME": codex_home}):
                q_codex = calculate_provider_quota("codex", {"status": "healthy"})
                self.assertEqual(q_codex["source"], "official")
                self.assertEqual(q_codex["percentage"], 8)
                self.assertIn("官方报告", q_codex["desc"])
                self.assertEqual(q_codex["status"], "warning")
        finally:
            if os.path.exists(cache_file):
                os.remove(cache_file)

if __name__ == "__main__":
    unittest.main()
