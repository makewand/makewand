"""
G3 reliability regressions: quota pacing.

Covers runtime-state#11 (time-of-day reset ignored updated_at's date) and
py-reliability#9 (bang-bang pacing without dead band, double counting of the
local burn-rate estimate, and `--tier auto` claiming dynamic pacing it never did).
"""

import tempfile
import unittest
from datetime import datetime, timedelta
from pathlib import Path
from unittest.mock import patch

import makewand.pacing as pacing
import makewand.usage as usage
from makewand.pacing import (
    CYCLE_7_DAYS,
    calculate_dynamic_pacing,
    parse_reset_time_to_seconds_left,
    resolve_dynamic_tier_and_effort,
)


def describe_auto_tier_signal(cache=None):
    return pacing.describe_auto_tier_signal(cache)

FIXED_NOW = datetime(2026, 9, 27, 21, 45, 0)


class _FixedDatetime(datetime):
    @classmethod
    def now(cls, tz=None):
        return FIXED_NOW if tz is None else FIXED_NOW.replace(tzinfo=tz)


class TestTimeOfDayResetAnchor(unittest.TestCase):
    """runtime-state#11."""

    def parse(self, text, updated_at):
        with patch.object(pacing, "datetime", _FixedDatetime):
            return parse_reset_time_to_seconds_left(text, updated_at)

    def test_passed_reset_is_zero_not_tomorrow(self):
        # Observed 10:03 today, "resets 8pm": it is 21:45 now, so the reset already happened.
        self.assertEqual(self.parse("8pm (Asia/Shanghai)", "2026-09-27T10:03:15.886096"), 0.0)

    def test_reset_later_same_day_is_counted_from_now(self):
        with patch.object(pacing, "datetime", _FixedDatetime):
            secs = parse_reset_time_to_seconds_left("11pm", "2026-09-27T20:00:00")
        self.assertAlmostEqual(secs, 75 * 60, delta=1)

    def test_clock_time_before_observation_rolls_to_next_day(self):
        # Observed yesterday 21:00 with "8pm": next 8pm after the observation is today 20:00 -> passed.
        self.assertEqual(self.parse("8pm", "2026-09-26T21:00:00"), 0.0)
        # Observed today 21:00 with "8pm": next is tomorrow 20:00.
        secs = self.parse("8pm", "2026-09-27T21:00:00")
        self.assertAlmostEqual(secs, 22.25 * 3600, delta=1)

    def test_without_updated_at_keeps_now_anchor(self):
        secs = self.parse("10pm", None)
        self.assertAlmostEqual(secs, 15 * 60, delta=1)


class _NoLedger(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-g3-pacing-")
        p = patch.object(usage, "USAGE_WINDOW_FILE", Path(self._tmp.name) / "usage.json")
        p.start()
        self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)


class TestContinuousPacing(_NoLedger):
    """py-reliability#9 (a): no step jumps at the thresholds."""

    @staticmethod
    def official(pct, seconds_left):
        reset = (datetime.now() + timedelta(seconds=seconds_left)).strftime("%Y-%m-%d %H:%M:%S")
        return {"status": "healthy", "reason": f"{pct}% remaining", "resets_at": reset,
                "updated_at": datetime.now().isoformat()}

    def test_delta_sweep_has_no_jumps_and_a_dead_band(self):
        seconds_left = CYCLE_7_DAYS * 0.5  # elapsed ratio 0.5
        boosts = []
        for tenth_pct in range(250, 751, 5):  # remaining 25.0% .. 75.0% in 0.5% steps
            pct = tenth_pct / 10.0
            info = self.official(int(round(pct)), seconds_left)
            boosts.append((pct, calculate_dynamic_pacing("claude", info)["routing_boost"]))
        jumps = [abs(b2 - b1) for (_, b1), (_, b2) in zip(boosts, boosts[1:])]
        self.assertLess(max(jumps), 0.5, f"old controller jumped 2.2 / 2.0 points at +-0.15: {max(jumps)}")
        for pct, boost in boosts:
            if abs((1 - pct / 100.0) - 0.5) <= 0.04:
                self.assertEqual(boost, 0.0, f"inside the dead band at {pct}%")

    def test_harvest_ramps_in(self):
        seconds_left = 2 * 3600
        boosts = [calculate_dynamic_pacing("codex", self.official(p, seconds_left))["routing_boost"] for p in range(8, 23)]
        jumps = [abs(b - a) for a, b in zip(boosts, boosts[1:])]
        self.assertLess(max(jumps), 0.5, f"old harvest switched 0 -> +3.0 between 14% and 15%: {boosts}")
        self.assertLess(boosts[0], 0.5)
        self.assertAlmostEqual(boosts[-1], 3.0)

    def test_saturated_states_keep_documented_extremes(self):
        under = calculate_dynamic_pacing("claude", self.official(95, 5 * 86400))
        over = calculate_dynamic_pacing("claude", self.official(30, 5 * 86400))
        self.assertAlmostEqual(under["routing_boost"], 2.2)
        self.assertEqual(under["recommended_tier"], "deep")
        self.assertAlmostEqual(over["routing_boost"], -2.0)
        self.assertEqual(over["recommended_tier"], "fast")


class TestHonestAutoTier(_NoLedger):
    """py-reliability#9 (b)(c): no official signal -> no pacing claims, no double counting."""

    def test_healthy_without_signals_is_labelled_neutral(self):
        info = {"status": "healthy", "reason": "Codex 订阅运行正常", "resets_at": None,
                "updated_at": datetime.now().isoformat()}
        p = calculate_dynamic_pacing("codex", info)
        self.assertEqual(p["routing_boost"], 0.0)
        self.assertIn("未进行动态调步", p["reason"])
        self.assertEqual(p.get("signal"), "none")
        self.assertEqual(p["recommended_tier"], "standard")
        self.assertIn("未做动态调步", describe_auto_tier_signal({"codex": info, "claude": info, "grok": info, "muse": info, "agy": {"status": "unknown"}}))

    def test_local_estimate_only_lowers_auto_tier(self):
        info = {"status": "healthy", "reason": "ok", "resets_at": None, "updated_at": datetime.now().isoformat()}
        with patch("makewand.usage.get_burn_rate_penalty", return_value=(-3.0, "heavy")):
            p = calculate_dynamic_pacing("codex", info)
            tier, _, _ = resolve_dynamic_tier_and_effort("codex", "auto", cache={"codex": info})
        self.assertEqual(p["routing_boost"], 0.0, "burn-rate is already applied once by the router")
        self.assertEqual(tier, "fast")
        self.assertIn("估算", p["reason"])
        self.assertEqual(p.get("signal"), "local_estimate")

    def test_estimated_percentage_is_not_fed_back_into_pacing(self):
        # A reset anchor plus a percentage *derived from the burn-rate penalty*:
        # the old code turned pen -4.0 into "5% left" and added over_burned -2.0 on top.
        reset = (datetime.now() + timedelta(days=3)).strftime("%Y-%m-%d %H:%M")
        info = {"status": "healthy", "reason": "运行正常", "resets_at": reset, "updated_at": datetime.now().isoformat()}
        with patch("makewand.usage.get_burn_rate_penalty", return_value=(-4.0, "heavy")):
            p = calculate_dynamic_pacing("claude", info)
        self.assertEqual(p["routing_boost"], 0.0)
        self.assertNotEqual(p["pacing_state"], "over_burned")


if __name__ == "__main__":
    unittest.main()
