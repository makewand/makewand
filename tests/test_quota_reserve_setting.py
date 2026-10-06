"""Model-free reserve policy, cache and quota presentation regressions."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import copy
import json
import os
import tempfile
import time
import unittest
import warnings
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest.mock import patch

from makewand import health


class QuotaReserveSettingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="makewand-quota-reserve-test-")
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.codex = self.home / "selected-codex"
        self.snapshot_path = self.home / ".cache/makewand/quota-snapshot.json"
        self.now = datetime.now(timezone.utc)
        env = patch.dict(os.environ, {"HOME": str(self.home), "CODEX_HOME": str(self.codex)})
        env.start()
        self.addCleanup(env.stop)
        os.environ.pop(health.QUOTA_RESERVE_ENV, None)
        expand = patch("makewand.health.os.path.expanduser", side_effect=self.expanduser)
        expand.start()
        self.addCleanup(expand.stop)
        usage = patch("makewand.usage.get_predictive_pacing_status", return_value={})
        usage.start()
        self.addCleanup(usage.stop)

    def expanduser(self, value):
        return str(self.home / value[2:]) if value.startswith("~/") else value

    def snapshot(self, remaining, *, age=0, bound_home=None):
        self.snapshot_path.parent.mkdir(parents=True, exist_ok=True)
        data = {"taken_at": (self.now - timedelta(seconds=age)).isoformat(), "providers": [{
            "Provider": "codex", "HasData": True, "WeeklyPct": 100 - remaining,
            "CodexHome": str(bound_home or self.codex),
            "ResetAt": (self.now + timedelta(hours=1)).isoformat(),
        }]}
        self.snapshot_path.write_text(json.dumps(data), encoding="utf-8")
        modified = time.time() - age
        os.utime(self.snapshot_path, (modified, modified))

    def cached_limit(self, *, reserve_gate=False, legacy=False):
        info = {"status": "limited", "reason": "实际供应商 429 限流",
                "resets_at": (self.now + timedelta(hours=1)).isoformat(),
                "updated_at": self.now.isoformat()}
        if reserve_gate:
            info["quota_reserve_gate"] = True
        if legacy:
            info["reason"] = "保护阈值拦截 (剩余 6.0% < 8%): 官方报告每周窗口剩余额度: 6%"
        return {"codex": info}

    def test_unset_default_keeps_original_boundaries_and_description(self):
        for remaining, status in ((0, "limited"), (7.99, "limited"),
                                  (8, "warning"), (24.99, "warning"), (25, "healthy")):
            with self.subTest(remaining=remaining):
                self.snapshot(remaining)
                quota = health._get_official_subscription_quota("codex")
                self.assertEqual(quota["status"], status)
                self.assertEqual(quota["reserve_percent"], 8)
                self.assertIsNone(quota["reserve_error"])
                self.assertNotIn("配额保留阈值", quota["desc"])

    def test_explicit_four_percent_drives_official_text_and_boundary(self):
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "4"}):
            for remaining, status in ((6, "warning"), (4, "warning"), (3.99, "limited")):
                with self.subTest(remaining=remaining):
                    self.snapshot(remaining)
                    quota = health._get_official_subscription_quota("codex")
                    self.assertEqual(quota["status"], status)
                    self.assertEqual(quota["reserve_percent"], 4)
                    self.assertIn("配额保留阈值: 4%", quota["desc"])
                    parsed = health.calculate_provider_quota("codex", {
                        "status": "healthy", "reason": f"{int(remaining)}% remaining"})
                    self.assertEqual(parsed["status"], "warning" if remaining >= 4 else "limited")
                    self.assertEqual(parsed["reserve_percent"], 4)

    def test_zero_reserve_never_allows_exhausted_quota(self):
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "0"}):
            self.snapshot(0)
            self.assertEqual(health._get_official_subscription_quota("codex")["status"], "limited")
            cache = health._sanitize_cache({"codex": {"status": "unknown"}})
            self.assertEqual(cache["codex"]["status"], "limited")
            self.assertIn("官方额度已耗尽", cache["codex"]["reason"])
            self.snapshot(0.01)
            self.assertEqual(health._get_official_subscription_quota("codex")["status"], "warning")

    def test_raised_reserve_precedes_healthy_cutoff(self):
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "30"}):
            self.snapshot(26)
            self.assertEqual(health._get_official_subscription_quota("codex")["status"], "limited")
            parsed = health.calculate_provider_quota("codex", {"status": "healthy", "reason": "26% remaining"})
            self.assertEqual(parsed["status"], "limited")
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "100"}):
            self.snapshot(99)
            self.assertEqual(health._get_official_subscription_quota("codex")["status"], "limited")
            self.snapshot(100)
            self.assertEqual(health._get_official_subscription_quota("codex")["status"], "healthy")

    def test_valid_fractional_finite_settings(self):
        for value, expected in ((" 4.5 ", 4.5), ("0", 0), ("1e2", 100)):
            with self.subTest(value=value), patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: value}):
                with warnings.catch_warnings(record=True) as observed:
                    warnings.simplefilter("always")
                    self.assertEqual(health._quota_reserve_setting(), (expected, None))
                self.assertEqual(observed, [])

    def test_invalid_setting_warns_without_echo_and_uses_eight(self):
        self.snapshot(6)
        for value in ("", "NaN", "inf", "-1", "100.1", "secret-not-a-number"):
            with self.subTest(value=value), patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: value}):
                with warnings.catch_warnings(record=True) as observed:
                    warnings.simplefilter("always")
                    quota = health._get_official_subscription_quota("codex")
                self.assertEqual(quota["status"], "limited")
                self.assertEqual(quota["reserve_percent"], 8)
                self.assertEqual(quota["reserve_error"], health.QUOTA_RESERVE_ERROR)
                self.assertEqual(len(observed), 1)
                self.assertEqual(str(observed[0].message), health.QUOTA_RESERVE_ERROR)
                self.assertNotIn("secret-not-a-number", quota["desc"])
                self.assertIn("using the default 8% reserve", quota["desc"])

    def test_invalid_setting_is_visible_without_official_evidence(self):
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "NaN"}):
            with warnings.catch_warnings(record=True) as observed:
                warnings.simplefilter("always")
                quota = health.calculate_provider_quota("codex", {"status": "unknown"})
            self.assertEqual(quota["reserve_percent"], 8)
            self.assertEqual(quota["reserve_error"], health.QUOTA_RESERVE_ERROR)
            self.assertIsNone(quota["percentage"])
            self.assertIn(health.QUOTA_RESERVE_ERROR, quota["desc"])
            self.assertTrue(observed)

    def test_fresh_official_evidence_rechecks_only_our_buffer_limit(self):
        from makewand.engine_selection import _engine_usable
        self.snapshot(6)
        old = health._sanitize_cache({"codex": {"status": "unknown"}})
        self.assertEqual(old["codex"]["status"], "limited")
        self.assertIn("< 8%", old["codex"]["reason"])
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "4"}):
            cache = health._sanitize_cache(copy.deepcopy(old))
            self.assertEqual(cache["codex"]["status"], "unknown")
            self.assertIn("配额保留阈值: 4%", cache["codex"]["reason"])
            with patch("makewand.config.is_provider_enabled", return_value=True):
                self.assertTrue(_engine_usable("codex", cache)[0])
                self.assertFalse(_engine_usable("codex", cache, require_healthy=True)[0])
            displayed = health.calculate_provider_quota("codex", cache["codex"])
            self.assertEqual((displayed["percentage"], displayed["status"]), (6, "warning"))
            from makewand.pacing import calculate_dynamic_pacing, PACING_LIMITED
            self.assertNotEqual(calculate_dynamic_pacing("codex", cache["codex"])["pacing_state"], PACING_LIMITED)
            real_limit = self.cached_limit()
            original = copy.deepcopy(real_limit)
            self.assertEqual(health._sanitize_cache(real_limit)["codex"]["status"], "limited")
            self.assertEqual(real_limit["codex"]["reason"], original["codex"]["reason"])
            self.assertNotIn("quota_reserve_gate", real_limit["codex"])
        self.assertEqual(health._sanitize_cache(cache)["codex"]["status"], "limited")

    def test_exact_legacy_buffer_verdict_can_be_rechecked(self):
        self.snapshot(6)
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "4"}):
            cache = health._sanitize_cache(self.cached_limit(legacy=True))
            self.assertEqual(cache["codex"]["status"], "unknown")
            self.assertTrue(cache["codex"]["quota_reserve_gate"])

    def test_stale_or_wrong_account_cannot_release_cached_buffer_limit(self):
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "4"}):
            for fields in ({"age": 901}, {"bound_home": self.home / "other-account"}):
                with self.subTest(fields=fields):
                    self.snapshot(6, **fields)
                    cache = health._sanitize_cache(self.cached_limit(reserve_gate=True))
                    self.assertEqual(cache["codex"]["status"], "limited")
                    quota = health.calculate_provider_quota("codex", cache["codex"])
                    self.assertEqual(quota["status"], "limited")
                    self.assertEqual(quota["source"], "status")

    def test_auth_and_real_limits_are_not_cleared_by_positive_quota(self):
        self.snapshot(90)
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "4"}):
            for status in ("limited", "needs_auth", "error", "disabled", "missing"):
                with self.subTest(status=status):
                    info = {"status": status, "reason": "真实派发失败",
                            "resets_at": (self.now + timedelta(hours=1)).isoformat(),
                            "updated_at": self.now.isoformat()}
                    cache = health._sanitize_cache({"codex": info})
                    self.assertEqual(cache["codex"]["status"], status)
                    self.assertEqual(cache["codex"]["reason"], "真实派发失败")
                    self.assertEqual(health.calculate_provider_quota("codex", info)["status"], status)

    def test_codex_exact_warning_respects_explicit_reserve_only(self):
        from makewand.providers.codex import parse_codex_quota
        self.assertTrue(parse_codex_quota("weekly limit: 6% left")[0])
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "4"}):
            self.assertFalse(parse_codex_quota("weekly limit: 6% left")[0])
            self.assertFalse(parse_codex_quota("weekly limit: 4% left")[0])
            for value in ("3", "0"):
                self.assertTrue(parse_codex_quota(f"weekly limit: {value}% left")[0])
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "NaN"}):
            with warnings.catch_warnings(record=True) as observed:
                warnings.simplefilter("always")
                self.assertTrue(parse_codex_quota("weekly limit: 6% left")[0])
            self.assertTrue(observed)

    def test_explicit_codex_buffer_warning_can_recover_only_with_fresh_evidence(self):
        from makewand.providers.codex import parse_codex_quota
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "4"}):
            limited, reason, reset = parse_codex_quota("weekly limit: 3% left")
            self.assertTrue(limited)
            cache = {"codex": {"status": "limited", "reason": reason,
                               "resets_at": reset, "updated_at": self.now.isoformat()}}
            self.assertEqual(health._sanitize_cache(copy.deepcopy(cache))["codex"]["status"], "limited")
            self.snapshot(6)
            restored = health._sanitize_cache(cache)
            self.assertEqual(restored["codex"]["status"], "unknown")
            self.assertEqual(health.calculate_provider_quota("codex", restored["codex"])["percentage"], 6)

    def test_codex_ambiguous_warning_needs_fresh_selected_account_evidence(self):
        from makewand.providers.codex import parse_codex_quota
        text = "less than 10% of your weekly limit left"
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "4"}):
            self.assertTrue(parse_codex_quota(text)[0])
            self.snapshot(6)
            self.assertFalse(parse_codex_quota(text)[0])
            self.snapshot(3)
            self.assertTrue(parse_codex_quota(text)[0])
            self.snapshot(6, age=901)
            self.assertTrue(parse_codex_quota(text)[0])
            self.snapshot(6, bound_home=self.home / "other-account")
            self.assertTrue(parse_codex_quota(text)[0])

    def test_codex_hard_refusals_dominate_proactive_warnings(self):
        from makewand.providers.codex import parse_codex_quota
        self.snapshot(90)
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "4"}):
            for refusal in ("You have hit your usage limit.", "HTTP 429 too many requests"):
                for warning in ("weekly limit: 6% left", "less than 10% of your weekly limit left"):
                    with self.subTest(refusal=refusal, warning=warning):
                        limited, reason, _ = parse_codex_quota(warning + "\n" + refusal)
                        self.assertTrue(limited)
                        self.assertNotIn("保护阈值", reason)
            # An unrelated successful task without quota text cannot create a
            # cached limit just because a low reserve setting is present.
            self.assertEqual(parse_codex_quota("task completed"), (False, "", None))

    def test_quota_policy_read_preserves_snapshot_bytes_and_timestamps(self):
        self.snapshot(6)
        before = (self.snapshot_path.read_bytes(), self.snapshot_path.stat().st_mtime_ns)
        with patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "4"}):
            health._get_official_subscription_quota("codex")
            health._sanitize_cache(self.cached_limit(reserve_gate=True))
        self.assertEqual((self.snapshot_path.read_bytes(), self.snapshot_path.stat().st_mtime_ns), before)


if __name__ == "__main__":
    unittest.main()
