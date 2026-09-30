"""Model-free quota freshness and account-root regressions."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import tempfile
import time
import unittest
from datetime import datetime, timezone
from pathlib import Path
from unittest.mock import patch
from makewand.health import _get_official_subscription_quota, _sanitize_cache


def iso(seconds):
    return datetime.fromtimestamp(seconds, timezone.utc).isoformat()


class QuotaFreshnessTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.env = patch.dict(os.environ, {"HOME": str(self.home)}, clear=False)
        self.env.start()
        self.addCleanup(self.env.stop)
        self.codex = self.home / ".codex"
        self.now = time.time()

    def snapshot(self, provider="codex", age=0, **fields):
        path = self.home / ".cache/makewand/quota-snapshot.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        data = {"taken_at": iso(self.now - age), "providers": [{"Provider": provider, "HasData": True, **fields}]}
        path.write_text(json.dumps(data))
        os.utime(path, (self.now - age, self.now - age))
        return path

    def session(self, windows, age=0, root=None, name="events", events=None):
        path = (root or self.codex) / "sessions" / (name + ".jsonl")
        path.parent.mkdir(parents=True, exist_ok=True)
        if events is None:
            events = [{"timestamp": iso(self.now - age), "payload": {"rate_limits": windows}}]
        path.write_text("\n".join(json.dumps(event) for event in events) + "\n")
        os.utime(path, (self.now - age, self.now - age))
        return path

    def window(self, used, minutes=10080, reset=None):
        return {"used_percent": used, "window_minutes": minutes,
                "resets_at": reset if reset is not None else self.now + 3600}

    def test_fresh_low_quota_remains_limited(self):
        self.snapshot(WeeklyPct=96, WeeklyResetAt=iso(self.now + 3600))
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["percentage"], 4)
        self.assertEqual(quota["status"], "limited")
        self.assertEqual(quota["window_minutes"], 10080)
        with patch("makewand.health._get_official_subscription_quota", return_value=quota):
            self.assertEqual(_sanitize_cache({"codex": {"status": "unknown"}})["codex"]["status"], "limited")

    def test_stale_snapshot_does_not_override_fresh_selected_session(self):
        self.snapshot(age=3600, WeeklyPct=100, ResetAt=iso(self.now + 3600))
        self.session({"primary": self.window(23)})
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["percentage"], 77)
        self.assertEqual(quota["selected_source"], "codex_session")
        self.assertEqual(quota["status"], "healthy")

    def test_newer_valid_source_replaces_same_window(self):
        self.snapshot(age=60, WeeklyPct=99, ResetAt=iso(self.now + 3600))
        self.session({"primary": self.window(12)})
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["percentage"], 88)
        self.assertEqual(quota["selected_source"], "codex_session")

    def test_new_weekly_sample_does_not_erase_fresh_rolling_limit(self):
        self.snapshot(age=30, WeeklyPct=70, FiveHourPct=97,
                      WeeklyResetAt=iso(self.now + 86400), FiveHourResetAt=iso(self.now + 600))
        self.session({"primary": self.window(15)})
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["percentage"], 3)
        self.assertEqual(quota["status"], "limited")
        self.assertEqual(quota["window_minutes"], 300)
        self.assertEqual(len(quota["windows"]), 2)

    def test_window_roles_follow_duration_not_primary_secondary_position(self):
        self.session({"primary": self.window(10, 10080), "secondary": self.window(94, 300)})
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["percentage"], 6)
        self.assertEqual(quota["window_minutes"], 300)
        self.assertIn("5 小时", quota["window"])
        self.assertEqual({w["window_minutes"] for w in quota["windows"]}, {300, 10080})

    def test_latest_event_is_used_not_first_matching_event(self):
        events = [{"timestamp": iso(self.now - 100), "payload": {"rate_limits": {"primary": self.window(99)}}},
                  {"timestamp": iso(self.now), "payload": {"rate_limits": {"primary": self.window(19)}}}]
        self.session({}, events=events)
        self.assertEqual(_get_official_subscription_quota("codex")["percentage"], 81)

    def test_recent_file_append_cannot_renew_old_event(self):
        path = self.session({"primary": self.window(99)}, age=3600)
        with path.open("a") as stream:
            stream.write('{"payload":{"unrelated":true}}\n')
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["status"], "unknown")
        self.assertIsNone(quota["percentage"])
        self.assertIn("stale_observation", quota["unverified_reasons"])

    def test_reset_windows_are_unknown_without_inventing_capacity(self):
        self.snapshot(WeeklyPct=100, ResetAt=iso(self.now - 1))
        self.session({"primary": self.window(100, reset=self.now - 1)})
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["status"], "unknown")
        self.assertIsNone(quota["percentage"])
        self.assertIn("window_reset", quota["unverified_reasons"])

    def test_one_reset_window_does_not_erase_an_active_limited_window(self):
        self.snapshot(WeeklyPct=100, FiveHourPct=94,
                      WeeklyResetAt=iso(self.now - 1), FiveHourResetAt=iso(self.now + 100))
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["percentage"], 6)
        self.assertEqual(quota["window_minutes"], 300)
        self.assertEqual(quota["status"], "limited")

    def test_explicit_home_never_uses_other_account_or_unbound_snapshot(self):
        selected = self.home / "other-account"
        self.snapshot(WeeklyPct=99, ResetAt=iso(self.now + 3600))
        self.session({"primary": self.window(99)})
        self.session({"primary": self.window(20)}, root=selected)
        with patch.dict(os.environ, {"CODEX_HOME": str(selected)}):
            quota = _get_official_subscription_quota("codex")
            self.assertEqual(quota["percentage"], 80)
            self.assertEqual(quota["selected_source"], "codex_session")
        with patch.dict(os.environ, {"CODEX_HOME": str(self.home / "missing-account")}):
            quota = _get_official_subscription_quota("codex")
            self.assertEqual(quota["status"], "unknown")
            self.assertIsNone(quota["percentage"])

    def test_default_home_does_not_scan_alternate_accounts(self):
        self.session({"primary": self.window(2)}, root=self.home / ".codex-2")
        self.assertIsNone(_get_official_subscription_quota("codex"))

    def test_file_touch_does_not_renew_snapshot_source_timestamp(self):
        path = self.snapshot(age=3600, WeeklyPct=96, ResetAt=iso(self.now + 3600))
        os.utime(path, None)
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["status"], "unknown")
        self.assertIsNone(quota["percentage"])

    def test_missing_duration_is_explicitly_unknown_and_missing_reset_is_none(self):
        self.session({"primary": {"used_percent": 94}})
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["percentage"], 6)
        self.assertEqual(quota["status"], "limited")
        self.assertIsNone(quota["window_minutes"])
        self.assertIsNone(quota["resets_at"])

    def test_fractional_threshold_is_not_rounded_into_extra_capacity(self):
        self.session({"primary": self.window(92.4)})
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["percentage"], 7.6)
        self.assertEqual(quota["status"], "limited")

    def test_reader_preserves_input_files(self):
        snapshot = self.snapshot(WeeklyPct=50, ResetAt=iso(self.now + 3600))
        session = self.session({"primary": self.window(60)})
        originals = {path: (path.read_bytes(), path.stat().st_mtime_ns) for path in (snapshot, session)}
        _get_official_subscription_quota("codex")
        self.assertEqual({path: (path.read_bytes(), path.stat().st_mtime_ns) for path in originals}, originals)


if __name__ == "__main__":
    unittest.main(verbosity=2)
