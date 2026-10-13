"""
py-rel-20 / py-rel-25: one reset-time parser for the status cache and pacing.

"Oct 20, 8pm (Asia/Shanghai)" used to be read by health.is_reset_time_passed as
Oct 2 00:00 (a limited provider flipped back to healthy on the next load), the
"Weekly limit reached · resets ..." wording was not captured (3 h fallback), and
pacing read a reset that passed an hour ago as 11 months away.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import json
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
import makewand.pacing as pacing
from makewand.health import extract_reset_phrase, is_reset_time_passed, parse_reset_datetime
from makewand.providers.claude import parse_claude_quota
from makewand.providers.codex import parse_codex_quota
from makewand.providers.grok import parse_grok_quota


def _local(*args):
    return datetime(*args).astimezone()


def _month_day(moment, clock):
    return f"{moment:%b} {moment.day}, {clock}"


def _shanghai():
    try:
        from zoneinfo import ZoneInfo
        return ZoneInfo("Asia/Shanghai")
    except Exception:  # no tz database (Windows without tzdata)
        return None


class TestSharedResetParser(unittest.TestCase):
    NOW = datetime(2026, 10, 12, 11, 0)

    def parse(self, text, observed=None, now=None):
        return parse_reset_datetime(text, observed_at=observed, now=now or self.NOW)

    def test_month_day_with_hour_or_minutes_or_24h_clock(self):
        self.assertEqual(self.parse("Oct 20, 8pm"), _local(2026, 10, 20, 20, 0))
        self.assertEqual(self.parse("Oct 20, 8:30pm"), _local(2026, 10, 20, 20, 30))
        self.assertEqual(self.parse("Oct 20, 20:15"), _local(2026, 10, 20, 20, 15))
        self.assertEqual(self.parse("Oct 3 at 2pm"), _local(2026, 10, 3, 14, 0))
        self.assertEqual(self.parse("October 3, 2026, 2:00 PM"), _local(2026, 10, 3, 14, 0))
        self.assertEqual(self.parse("Oct 20th, 2026 8:00 PM"), _local(2026, 10, 20, 20, 0))
        self.assertEqual(self.parse("10:40 on 4 Nov"), _local(2026, 11, 4, 10, 40))
        self.assertEqual(self.parse("10-14 10:40"), _local(2026, 10, 14, 10, 40))

    def test_area_city_zone_is_honoured(self):
        shanghai = _shanghai()
        if shanghai is None:
            self.skipTest("zoneinfo database unavailable")
        self.assertEqual(self.parse("Oct 20, 8pm (Asia/Shanghai)"), datetime(2026, 10, 20, 20, 0, tzinfo=shanghai))
        self.assertEqual(self.parse("Oct 20, 8pm (America/New_York)"),
                         datetime(2026, 10, 21, 0, 0, tzinfo=timezone.utc))
        observed = datetime(2026, 10, 12, 2, 0, tzinfo=timezone.utc)  # 10:00 in Shanghai
        self.assertEqual(self.parse("8pm (Asia/Shanghai)", observed=observed),
                         datetime(2026, 10, 12, 20, 0, tzinfo=shanghai))
        self.assertEqual(self.parse("Oct 20, 8pm UTC+8"), datetime(2026, 10, 20, 12, 0, tzinfo=timezone.utc))

    def test_relative_durations_count_from_the_observation(self):
        observed = datetime(2026, 10, 12, 9, 0)
        self.assertEqual(self.parse("in 3 hours", observed=observed), _local(2026, 10, 12, 12, 0))
        self.assertEqual(self.parse("in 3h 20m", observed=observed), _local(2026, 10, 12, 12, 20))
        self.assertEqual(self.parse("2 hours", observed=observed), _local(2026, 10, 12, 11, 0))
        self.assertEqual(self.parse("in 4 days 2 hours 31 minutes", observed=observed),
                         _local(2026, 10, 16, 11, 31))
        self.assertEqual(self.parse("in 15 minutes"), _local(2026, 10, 12, 11, 15))

    def test_year_rollover_is_anchored_on_the_observation(self):
        # py-rel-25: a reset that passed an hour ago is in the past, not next year.
        passed = self.parse("Oct 12, 10am", observed=datetime(2026, 10, 12, 8, 0))
        self.assertEqual(passed, _local(2026, 10, 12, 10, 0))
        self.assertEqual(self.parse("Oct 12, 10am"), _local(2026, 10, 12, 10, 0))
        # Observed on New Year's Eve, "Jan 1" is next year; observed on Jan 1, "Dec 31" was yesterday.
        self.assertEqual(self.parse("Jan 1, 5am", observed=datetime(2026, 12, 31, 23, 0)), _local(2027, 1, 1, 5, 0))
        self.assertEqual(self.parse("Dec 31, 11pm", observed=datetime(2027, 1, 1, 1, 0)), _local(2026, 12, 31, 23, 0))

    def test_clock_time_and_iso_and_epoch(self):
        self.assertEqual(self.parse("8pm", observed=datetime(2026, 10, 12, 21, 0)), _local(2026, 10, 13, 20, 0))
        self.assertEqual(self.parse("at 2:00 PM.", observed=datetime(2026, 10, 12, 9, 0)), _local(2026, 10, 12, 14, 0))
        self.assertEqual(self.parse("10:58 (2026-09-27)"), _local(2026, 9, 27, 10, 58))
        self.assertEqual(self.parse("2026-09-28 07:53"), _local(2026, 9, 28, 7, 53))
        self.assertEqual(self.parse("2026-10-20T12:00:00Z"), datetime(2026, 10, 20, 12, 0, tzinfo=timezone.utc))
        self.assertEqual(self.parse("1760000000"), datetime.fromtimestamp(1760000000, timezone.utc))
        self.assertIsNone(self.parse("待重置"))
        self.assertIsNone(self.parse("later"))


class TestResetPhraseCapture(unittest.TestCase):
    def test_session_weekly_monthly_wording(self):
        for message, expected in (
            ("Weekly limit reached · resets Oct 20, 8pm (Asia/Shanghai)", "Oct 20, 8pm (Asia/Shanghai)"),
            ("Weekly limit reached ∙ resets Oct 20, 8pm (Asia/Shanghai)", "Oct 20, 8pm (Asia/Shanghai)"),
            ("5-hour limit reached ∙ resets 8pm", "8pm"),
            ("Session limit reached · resets 3:45am", "3:45am"),
            ("Monthly limit reached · resets Nov 1", "Nov 1"),
            ("You've hit your limit · resets at 2:00 PM (Asia/Shanghai)", "2:00 PM (Asia/Shanghai)"),
            ("You've reached your usage limit. Try again in 2 hours.", "in 2 hours"),
        ):
            with self.subTest(message=message):
                self.assertEqual(extract_reset_phrase(message), expected)

    def test_claude_weekly_limit_keeps_its_reset_instead_of_three_hours(self):
        limited, reason, resets = parse_claude_quota("Weekly limit reached · resets Oct 20, 8pm (Asia/Shanghai)")
        self.assertTrue(limited)
        self.assertEqual(resets, "Oct 20, 8pm (Asia/Shanghai)")
        self.assertIn("Oct 20, 8pm", reason)
        limited, _, resets = parse_claude_quota("You've hit your weekly limit · resets Oct 20, 8pm (Asia/Shanghai)")
        self.assertTrue(limited)
        self.assertEqual(resets, "Oct 20, 8pm (Asia/Shanghai)")

    def test_claude_epoch_suffix_is_the_reset(self):
        limited, _, resets = parse_claude_quota("Claude AI usage limit reached|1760000000")
        self.assertTrue(limited)
        self.assertEqual(parse_reset_datetime(resets), datetime.fromtimestamp(1760000000, timezone.utc))

    def test_codex_and_grok_capture_relative_and_dated_resets(self):
        self.assertEqual(parse_codex_quota("You've hit your usage limit. Try again in 3 days 2 hours.")[2],
                         "in 3 days 2 hours")
        self.assertEqual(parse_grok_quota("Rate limit reached: resets in 45m")[2], "in 45m")


class TestHealthAndPacingAgree(unittest.TestCase):
    def test_future_month_day_reset_is_not_passed(self):
        now = datetime.now()
        future = now + timedelta(days=8)
        text = _month_day(future, "8pm")
        self.assertFalse(is_reset_time_passed(text, now.isoformat()))
        secs = pacing.parse_reset_time_to_seconds_left(text, updated_at=now.isoformat())
        expected = (future.replace(hour=20, minute=0, second=0, microsecond=0).astimezone()
                    - datetime.now().astimezone()).total_seconds()
        self.assertAlmostEqual(secs, expected, delta=120)

    def test_reset_that_passed_an_hour_ago_is_passed_everywhere(self):
        now = datetime.now()
        past = now - timedelta(hours=1)
        text = _month_day(past, past.strftime("%I:%M%p").lstrip("0").lower())
        observed = (now - timedelta(hours=3)).isoformat()
        self.assertTrue(is_reset_time_passed(text, observed))
        self.assertEqual(pacing.parse_reset_time_to_seconds_left(text, updated_at=observed), 0.0)

    def test_pacing_uses_the_shared_parser_for_zones(self):
        if _shanghai() is None:
            self.skipTest("zoneinfo database unavailable")
        future = datetime.now(_shanghai()) + timedelta(days=3)
        text = _month_day(future, "8pm") + " (Asia/Shanghai)"
        expected = future.replace(hour=20, minute=0, second=0, microsecond=0)
        secs = pacing.parse_reset_time_to_seconds_left(text, updated_at=datetime.now().isoformat())
        self.assertAlmostEqual(secs, (expected - datetime.now(timezone.utc)).total_seconds(), delta=120)


class TestPacingUsesTheOfficialWindow(unittest.TestCase):
    """A 5-hour window nearly used up must not read as a week of unused quota."""

    def pace(self, percentage, reset_in, window_minutes):
        now = datetime.now()
        quota = {"percentage": percentage, "status": "warning" if percentage < 25 else "healthy",
                 "source": "official", "resets_at": (now + timedelta(seconds=reset_in)).isoformat(),
                 "is_unlimited": False, "window_minutes": window_minutes, "updated_at": now.isoformat()}
        with patch.object(pacing, "calculate_provider_quota", lambda provider, info: dict(quota)):
            return pacing.calculate_dynamic_pacing("claude", info={"status": "healthy", "updated_at": now.isoformat()})

    def test_five_hour_window_is_paced_against_five_hours(self):
        verdict = self.pace(15, 3600, 300)
        self.assertNotEqual(verdict["pacing_state"], pacing.PACING_UNDER_BURNED)
        self.assertLessEqual(verdict["routing_boost"], 0.0)
        self.assertNotEqual(verdict["recommended_tier"], "deep")

    def test_weekly_window_still_detects_under_burn(self):
        verdict = self.pace(60, 3 * 86400, 10080)
        self.assertEqual(verdict["pacing_state"], pacing.PACING_UNDER_BURNED)


class TestLimitedProviderStaysLimited(unittest.TestCase):
    """End to end: provider message -> record_engine_limit -> next load_status_cache."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-fix-reset-")
        root = Path(self._tmp.name)
        for p in (patch.object(config, "CONFIG_DIR", root / "cfg"),
                  patch.object(health, "STATUS_CACHE_FILE", root / "cfg" / "status.json"),
                  patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json")):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        (root / "cfg").mkdir(parents=True)
        self.status_file = root / "cfg" / "status.json"

    def test_weekly_reset_days_ahead_survives_reload(self):
        future = datetime.now() + timedelta(days=8)
        for message in (f"Weekly limit reached · resets {_month_day(future, '8pm')}",
                        f"You've hit your limit · resets {_month_day(future, '8:30pm')}"):
            with self.subTest(message=message):
                limited, reason, resets = parse_claude_quota(message)
                self.assertTrue(limited)
                health.record_engine_limit("claude", reason, resets)
                entry = health.load_status_cache()["claude"]
                self.assertEqual(entry["status"], "limited")
                self.assertEqual(entry["resets_at"], resets)

    def test_passed_reset_heals_on_reload(self):
        observed = datetime.now() - timedelta(hours=3)
        past = datetime.now() - timedelta(hours=1)
        self.status_file.write_text(json.dumps({"claude": {
            "status": "limited", "reason": "limit", "updated_at": observed.isoformat(),
            "resets_at": _month_day(past, past.strftime("%H:%M"))}}), encoding="utf-8")
        self.assertEqual(health.load_status_cache()["claude"]["status"], "healthy")


if __name__ == "__main__":
    unittest.main()
