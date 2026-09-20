"""
Unit tests for health monitoring and quota parsers.
"""

import unittest
from makewand.providers.claude import parse_claude_quota
from makewand.providers.codex import parse_codex_quota
from makewand.providers.agy import parse_agy_quota
from makewand.providers.muse import parse_muse_quota
from makewand.health import load_status_cache, save_status_cache

class TestHealth(unittest.TestCase):
    def test_claude_quota_parser(self):
        output = "You've hit your monthly spend limit · raise it at claude.ai · your weekly limit resets 8pm (Asia/Shanghai)"
        limited, reason, resets = parse_claude_quota(output)
        self.assertTrue(limited)
        self.assertIn("8pm", resets)

        ok_out = "ok"
        limited, _, _ = parse_claude_quota(ok_out)
        self.assertFalse(limited)

    def test_codex_quota_parser(self):
        output = "You have hit your usage limit. Try again at 10:58 AM."
        limited, reason, resets = parse_codex_quota(output)
        self.assertTrue(limited)
        self.assertIn("10:58 AM", resets)

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

    def test_status_cache_io(self):
        cache = load_status_cache()
        self.assertIn("agy", cache)
        self.assertIn("claude", cache)
        self.assertIn("codex", cache)
        self.assertIn("muse", cache)

if __name__ == "__main__":
    unittest.main()
