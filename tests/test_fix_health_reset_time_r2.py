"""
py-rel-20 (round 2): reset-time handling never raises on out-of-range
timestamps, and a bare clock reset stored without an observation time heals.

Round 1 made load_status_cache, pacing and is_reset_time_passed raise ValueError
for a naive updated_at such as "0001-01-01T00:00:00" or "9999-12-31T23:59:59"
(the anchor was converted outside the parser's try block and
_observed_longer_than was unguarded); save_status_cache swallowed the same error,
so record_engine_limit silently stopped saving. A bare "8pm" with an empty
updated_at was anchored on "now" and never passed, while the old parser healed
it once that time of day had passed.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import json
import tempfile
import unittest
from datetime import datetime, timedelta
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
import makewand.pacing as pacing
import makewand.usage as usage
from makewand.health import is_reset_time_passed, parse_reset_datetime

OUT_OF_RANGE = ("0001-01-01T00:00:00", "9999-12-31T23:59:59")
RESETS = ("Oct 20, 8pm (Asia/Shanghai)", "8pm", "in 2 hours", "待重置", None)


def _local(*args):
    return datetime(*args).astimezone()


def _clock(moment):
    return moment.strftime("%H:%M")


def _future_month_day(clock):
    # A year-less "Oct 20, 8pm" is judged against the real clock: a fixed one
    # turns from future to past on that day and the test starts failing.
    moment = datetime.now() + timedelta(days=8)
    return f"{moment:%b} {moment.day}, {clock}"


class _IsolatedStatus(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-fix-reset-r2-")
        root = Path(self._tmp.name)
        for p in (patch.object(config, "CONFIG_DIR", root / "cfg"),
                  patch.object(health, "STATUS_CACHE_FILE", root / "cfg" / "status.json"),
                  patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json"),
                  patch.object(usage, "USAGE_WINDOW_FILE", root / "usage_window.json")):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        (root / "cfg").mkdir(parents=True)
        self.status_file = root / "cfg" / "status.json"

    def write_status(self, data):
        self.status_file.write_text(json.dumps(data), encoding="utf-8")


class TestOutOfRangeObservation(_IsolatedStatus):
    def test_load_status_cache_tolerates_out_of_range_naive_timestamps(self):
        for updated_at in OUT_OF_RANGE:
            for resets in RESETS:
                with self.subTest(updated_at=updated_at, resets=resets):
                    self.write_status({"claude": {"status": "limited", "reason": "x", "resets_at": resets,
                                                  "updated_at": updated_at}})
                    self.assertIn(health.load_status_cache()["claude"]["status"], ("limited", "unknown"))

    def test_observation_in_year_one_counts_as_long_ago(self):
        self.write_status({"claude": {"status": "limited", "reason": "x", "resets_at": None,
                                      "updated_at": "0001-01-01T00:00:00"}})
        self.assertEqual(health.load_status_cache()["claude"]["status"], "unknown")

    def test_parser_pacing_and_reset_check_tolerate_them(self):
        for updated_at in OUT_OF_RANGE:
            with self.subTest(updated_at=updated_at):
                self.assertIsInstance(parse_reset_datetime("8pm", observed_at=updated_at), datetime)
                self.assertGreaterEqual(pacing.parse_reset_time_to_seconds_left("8pm", updated_at=updated_at), 0.0)
                self.assertIsInstance(is_reset_time_passed(_future_month_day("8pm"), updated_at), bool)
                self.assertIsInstance(health._observed_longer_than(updated_at, 7200), bool)

    def test_record_engine_limit_still_saves_next_to_a_damaged_entry(self):
        self.write_status({"codex": {"status": "limited", "reason": "x", "resets_at": "8pm",
                                     "updated_at": "0001-01-01T00:00:00"}})
        health.record_engine_limit("claude", "limit", _future_month_day("8pm") + " (Asia/Shanghai)")
        disk = json.loads(self.status_file.read_text(encoding="utf-8"))
        self.assertEqual(disk["claude"]["status"], "limited")
        self.assertEqual(health.load_status_cache()["claude"]["status"], "limited")

    def test_dispatch_failure_with_out_of_range_timestamp_does_not_break_loading(self):
        usage.record_engine_usage("agy", tier="standard", success=True, task="run", route="cli")
        for updated_at in OUT_OF_RANGE:
            with self.subTest(updated_at=updated_at):
                self.write_status({"agy": {"status": "error", "reason": "region", "resets_at": None,
                                           "source": "dispatch", "ttl_seconds": 6 * 3600, "updated_at": updated_at}})
                self.assertIn(health.load_status_cache()["agy"]["status"], ("error", "unknown"))
                self.assertFalse(health._dispatch_failure_superseded("agy", {
                    "status": "error", "source": "dispatch", "updated_at": updated_at}))


class TestBareClockWithoutObservation(_IsolatedStatus):
    def test_is_todays_occurrence(self):
        for now in (datetime(2026, 10, 12, 19, 0), datetime(2026, 10, 12, 21, 0)):
            with self.subTest(now=now):
                self.assertEqual(parse_reset_datetime("8pm", now=now), _local(2026, 10, 12, 20, 0))
                self.assertEqual(parse_reset_datetime("8pm", observed_at="garbage", now=now), _local(2026, 10, 12, 20, 0))
        # With an observation it is still the first occurrence after it.
        self.assertEqual(parse_reset_datetime("8pm", observed_at=datetime(2026, 10, 12, 21, 0),
                                              now=datetime(2026, 10, 12, 21, 30)), _local(2026, 10, 13, 20, 0))
        # Durations still count from now when the observation is unknown.
        self.assertEqual(parse_reset_datetime("in 15 minutes", now=datetime(2026, 10, 12, 11, 0)),
                         _local(2026, 10, 12, 11, 15))

    def test_limit_without_timestamp_is_unknown_once_the_time_passed(self):
        now = datetime.now()
        if not 2 <= now.hour <= 21:
            self.skipTest("needs an hour of margin on both sides of the clock within today")
        passed, upcoming = _clock(now - timedelta(hours=1)), _clock(now + timedelta(hours=1))
        self.assertTrue(is_reset_time_passed(passed, ""))
        self.assertFalse(is_reset_time_passed(upcoming, ""))
        self.write_status({"claude": {"status": "limited", "reason": "x", "resets_at": passed, "updated_at": ""},
                           "codex": {"status": "limited", "reason": "x", "resets_at": upcoming, "updated_at": ""}})
        cache = health.load_status_cache()
        self.assertEqual(cache["claude"]["status"], "unknown")
        self.assertEqual(cache["codex"]["status"], "limited")


if __name__ == "__main__":
    unittest.main()
