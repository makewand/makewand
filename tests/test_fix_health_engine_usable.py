"""
py-rel-19: an "error" verdict (e.g. agy region-blocked by a real dispatch) makes
the engine unusable for selection while it is within its TTL, with the same
expiry semantics as the other failure statuses. It used to count as usable, so a
region-blocked agy was still picked as race judge and kept in the review ladder.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import unittest
from datetime import datetime, timedelta
from unittest.mock import patch

from makewand.engine_selection import _engine_usable, _select_race_judge

REGION = "Antigravity 在当前地区/账号不可用 (真实派发返回地区限制)"


def ago(**kwargs):
    return (datetime.now() - timedelta(**kwargs)).isoformat()


class TestErrorVerdictExcludesEngine(unittest.TestCase):
    def setUp(self):
        p = patch("makewand.config.is_provider_enabled", return_value=True)
        p.start()
        self.addCleanup(p.stop)

    def test_live_error_is_unusable(self):
        for info in ({"status": "error", "reason": REGION, "updated_at": ago(minutes=5)},
                     {"status": "error", "reason": REGION, "updated_at": ago(hours=2), "ttl_seconds": 6 * 3600}):
            with self.subTest(info=info):
                usable, why = _engine_usable("agy", {"agy": info})
                self.assertFalse(usable)
                self.assertIn("error", why)
                self.assertIn("makewand probe", why)

    def test_expired_error_is_neutral_like_other_failure_statuses(self):
        for info in ({"status": "error", "reason": "probe timeout", "updated_at": ago(minutes=31)},
                     {"status": "error", "reason": REGION, "updated_at": ago(hours=7), "ttl_seconds": 6 * 3600},
                     {"status": "needs_auth", "reason": "login", "updated_at": ago(minutes=31)},
                     # A missing or unparseable timestamp is damaged evidence, as load_status_cache treats it.
                     {"status": "error", "reason": REGION},
                     {"status": "error", "reason": REGION, "updated_at": "garbage"}):
            with self.subTest(info=info):
                self.assertTrue(_engine_usable("agy", {"agy": info})[0])
        # Statuses without a TTL keep excluding the engine.
        self.assertFalse(_engine_usable("agy", {"agy": {"status": "limited", "updated_at": ago(days=3)}})[0])

    def test_region_blocked_agy_is_not_the_race_judge(self):
        cache = {"agy": {"status": "error", "reason": REGION, "updated_at": ago(minutes=1), "ttl_seconds": 6 * 3600},
                 "codex": {"status": "healthy"}, "claude": {"status": "healthy"}, "grok": {"status": "healthy"}}
        self.assertEqual(_select_race_judge(cache, ("claude", "grok")), "codex")

    def test_unprobed_agy_remains_the_preferred_judge(self):
        cache = {"agy": {"status": "unknown"}, "codex": {"status": "healthy"}}
        self.assertEqual(_select_race_judge(cache, ("claude", "grok")), "agy")


if __name__ == "__main__":
    unittest.main()
