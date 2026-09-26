"""
Unit tests for Makewand Dynamic Quota Pacing Engine.
"""

import unittest
from datetime import datetime, timedelta
from unittest.mock import patch

from makewand.pacing import (
    parse_reset_time_to_seconds_left,
    calculate_dynamic_pacing,
    get_all_providers_pacing,
    resolve_dynamic_tier_and_effort,
    PACING_HARVEST,
    PACING_UNDER_BURNED,
    PACING_BALANCED,
    PACING_OVER_BURNED,
    PACING_LIMITED,
    CYCLE_7_DAYS,
)


class TestDynamicQuotaPacing(unittest.TestCase):
    def test_parse_reset_time_to_seconds_left(self):
        # 1. ISO format future
        future_dt = datetime.now() + timedelta(hours=5, minutes=30)
        future_str = future_dt.strftime("%Y-%m-%d %H:%M")
        secs = parse_reset_time_to_seconds_left(future_str)
        self.assertIsNotNone(secs)
        self.assertAlmostEqual(secs, 5.5 * 3600, delta=120)

        # 2. Relative string
        secs_rel = parse_reset_time_to_seconds_left("in 3 hours")
        self.assertIsNotNone(secs_rel)
        self.assertAlmostEqual(secs_rel, 3 * 3600, delta=10)

        # 3. None or empty
        self.assertIsNone(parse_reset_time_to_seconds_left(None))
        self.assertIsNone(parse_reset_time_to_seconds_left(""))

    def test_unlimited_provider_pacing(self):
        p_agy = calculate_dynamic_pacing("agy", {"status": "healthy"})
        self.assertEqual(p_agy["pacing_state"], PACING_BALANCED)
        self.assertEqual(p_agy["quota_percentage"], 100)
        self.assertEqual(p_agy["recommended_tier"], "deep")

        p_loc = calculate_dynamic_pacing("local", {"status": "healthy"})
        self.assertEqual(p_loc["pacing_state"], PACING_BALANCED)
        self.assertEqual(p_loc["quota_percentage"], 100)

    def test_limited_provider_pacing(self):
        p_ltd = calculate_dynamic_pacing("claude", {"status": "limited", "reason": "limit reached", "resets_at": "tomorrow"})
        self.assertEqual(p_ltd["pacing_state"], PACING_LIMITED)
        self.assertEqual(p_ltd["quota_percentage"], 0)
        self.assertEqual(p_ltd["routing_boost"], -999.0)

    def test_harvest_window_pacing(self):
        # Reset in 2 hours out of 7 days, but 40% quota still remaining!
        reset_time = (datetime.now() + timedelta(hours=2)).strftime("%Y-%m-%d %H:%M")
        info = {
            "status": "healthy",
            "reason": "40% remaining",
            "resets_at": reset_time
        }
        p = calculate_dynamic_pacing("codex", info)
        self.assertEqual(p["pacing_state"], PACING_HARVEST)
        self.assertEqual(p["recommended_tier"], "deep")
        self.assertEqual(p["recommended_effort"], "max")
        self.assertGreater(p["routing_boost"], 2.5)

    def test_under_burned_surplus_pacing(self):
        # 5 days left out of 7 days (elapsed = 2/7 = 28%), but 90% quota remains (consumed = 10%)
        # delta = 0.10 - 0.28 = -0.18 (< -0.15)
        reset_time = (datetime.now() + timedelta(days=5)).strftime("%Y-%m-%d %H:%M")
        info = {
            "status": "healthy",
            "reason": "90% remaining",
            "resets_at": reset_time
        }
        p = calculate_dynamic_pacing("claude", info)
        self.assertEqual(p["pacing_state"], PACING_UNDER_BURNED)
        self.assertEqual(p["recommended_tier"], "deep")
        self.assertGreater(p["routing_boost"], 1.5)

    def test_over_burned_throttle_pacing(self):
        # 5 days left out of 7 days (elapsed = 28%), but only 30% quota remains (consumed = 70%)
        # delta = 0.70 - 0.28 = +0.42 (> 0.15)
        reset_time = (datetime.now() + timedelta(days=5)).strftime("%Y-%m-%d %H:%M")
        info = {
            "status": "healthy",
            "reason": "30% remaining",
            "resets_at": reset_time
        }
        p = calculate_dynamic_pacing("claude", info)
        self.assertEqual(p["pacing_state"], PACING_OVER_BURNED)
        self.assertEqual(p["recommended_tier"], "fast")
        self.assertEqual(p["recommended_effort"], "low")
        self.assertLess(p["routing_boost"], 0.0)

    def test_resolve_dynamic_tier_and_effort(self):
        # Under-burned should resolve to deep tier
        reset_time = (datetime.now() + timedelta(days=5)).strftime("%Y-%m-%d %H:%M")
        cache = {
            "codex": {
                "status": "healthy",
                "reason": "95% remaining",
                "resets_at": reset_time
            }
        }
        tier, model, effort = resolve_dynamic_tier_and_effort("codex", requested_tier="auto", cache=cache)
        self.assertEqual(tier, "deep")
        self.assertIn("astra", model.lower())


if __name__ == "__main__":
    unittest.main()
