"""
G3 reliability regressions: status-cache semantics.

Covers py-reliability#6 (error / needs_auth never self-healed), runtime-state#7
(stale cache trusted forever), runtime-state#6 (needs_auth without an actionable
hint), runtime-state#3 / replay-0926-memory#17 / py-reliability#3 (agy real
dispatch failures never written back; version probe claimed "ready"), and the
probe mutex from py-reliability#3.
"""

import json
import tempfile
import threading
import time
import unittest
from datetime import datetime, timedelta
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
import makewand.usage as usage
from makewand import filelock
from makewand.health import (
    calculate_provider_quota,
    get_or_update_status,
    load_status_cache,
    probe_model,
    record_engine_limit,
    save_status_cache,
)


def is_status_stale(info):
    return health.is_status_stale(info)
from makewand.pacing import calculate_dynamic_pacing
from makewand.providers.agy import execute_agy_task


class _IsolatedStatus(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-g3-status-")
        root = Path(self._tmp.name)
        self.status_file = root / "cfg" / "status.json"
        patches = [
            patch.object(config, "CONFIG_DIR", root / "cfg"),
            patch.object(config, "CANDIDATES_DIR", root / "cfg" / "candidates"),
            patch.object(config, "BACKUPS_DIR", root / "cfg" / "backups"),
            patch.object(config, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json"),
            patch.object(health, "STATUS_CACHE_FILE", self.status_file),
            patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json"),
            patch.object(usage, "USAGE_WINDOW_FILE", root / "usage_window.json"),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        config.ensure_config_dir()

    def write_status(self, data):
        self.status_file.parent.mkdir(parents=True, exist_ok=True)
        self.status_file.write_text(json.dumps(data), encoding="utf-8")

    @staticmethod
    def ago(**kw):
        return (datetime.now() - timedelta(**kw)).isoformat()


class TestFailureStatusTTL(_IsolatedStatus):
    """py-reliability#6: one probe timeout must not exclude a provider forever."""

    def test_expired_error_and_needs_auth_become_neutral(self):
        self.write_status({
            "claude": {"status": "error", "reason": "Command timed out after 15 seconds", "resets_at": None, "updated_at": self.ago(minutes=31)},
            "muse": {"status": "needs_auth", "reason": "等待 OAuth", "resets_at": None, "updated_at": self.ago(days=3)},
            "grok": {"status": "error", "reason": "fresh failure", "resets_at": None, "updated_at": self.ago(minutes=5)},
        })
        cache = load_status_cache()
        self.assertEqual(cache["claude"]["status"], "unknown")
        self.assertEqual(cache["claude"]["expired_from"], "error")
        self.assertIn("makewand probe", cache["claude"]["reason"])
        self.assertEqual(cache["muse"]["status"], "unknown")
        self.assertEqual(cache["grok"]["status"], "error", "a fresh failure stays within its TTL")

        self.assertEqual(calculate_dynamic_pacing("claude", cache["claude"])["routing_boost"], 0.0)
        self.assertEqual(calculate_dynamic_pacing("muse", cache["muse"])["routing_boost"], 0.0)
        self.assertEqual(calculate_dynamic_pacing("grok", cache["grok"])["routing_boost"], -999.0)

    def test_explicit_ttl_is_honoured(self):
        self.write_status({"agy": {"status": "error", "reason": "region", "resets_at": None,
                                   "updated_at": self.ago(hours=2), "ttl_seconds": 6 * 3600}})
        self.assertEqual(load_status_cache()["agy"]["status"], "error")
        self.write_status({"agy": {"status": "error", "reason": "region", "resets_at": None,
                                   "updated_at": self.ago(hours=7), "ttl_seconds": 6 * 3600}})
        self.assertEqual(load_status_cache()["agy"]["status"], "unknown")


class TestStaleCache(_IsolatedStatus):
    """runtime-state#7: an old healthy verdict is neutral and asks for a re-probe."""

    def test_stale_healthy_is_flagged_and_neutral(self):
        self.write_status({
            "agy": {"status": "healthy", "reason": "installed", "resets_at": None, "updated_at": self.ago(hours=12)},
            "codex": {"status": "healthy", "reason": "ok", "resets_at": None, "updated_at": self.ago(minutes=10)},
        })
        cache = load_status_cache()
        self.assertTrue(is_status_stale(cache["agy"]))
        self.assertTrue(cache["agy"]["stale"])
        self.assertFalse(is_status_stale(cache["codex"]))
        quota = calculate_provider_quota("agy", cache["agy"])
        self.assertTrue(quota.get("stale"))
        self.assertIn("makewand probe", quota["desc"])
        pacing = calculate_dynamic_pacing("agy", cache["agy"])
        self.assertEqual(pacing["routing_boost"], 0.0, "stale agy must not keep its +0.5 preference")
        self.assertIn("makewand probe", pacing["reason"])

    def test_selector_reports_stale_engines(self):
        from makewand.orchestrator import select_optimal_engine_pair
        cache = {e: {"status": "healthy", "updated_at": self.ago(hours=8)} for e in ("claude", "codex", "grok")}
        with patch("makewand.config.get_active_providers", return_value=["claude", "codex", "grok"]), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.config.is_provider_enabled", return_value=True), \
             patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)):
            _, _, meta = select_optimal_engine_pair("实现导出功能", tier="standard", cache=cache)
        self.assertTrue(any("makewand probe" in r and "6 小时" in r for r in meta["reasons"]), meta["reasons"])


class TestNeedsAuthHint(_IsolatedStatus):
    """runtime-state#6: needs_auth must tell the user exactly what to do."""

    def test_quota_and_pacing_carry_reauth_hint(self):
        info = {"status": "needs_auth", "reason": "等待 OAuth 浏览器授权登录", "resets_at": None,
                "updated_at": datetime.now().isoformat()}
        quota = calculate_provider_quota("muse", info)
        self.assertIn("muse 需要重新登录", quota["desc"])
        self.assertIn("muse login", quota["desc"])
        pacing = calculate_dynamic_pacing("muse", info)
        self.assertEqual(pacing["routing_boost"], -999.0)
        self.assertIn("muse login", pacing["reason"])

    def test_login_failure_recorded_as_needs_auth_not_limited(self):
        record_engine_limit("muse", "未登录或需配置凭据 (运行 'muse login')", "需登录授权")
        entry = load_status_cache()["muse"]
        self.assertEqual(entry["status"], "needs_auth")
        self.assertIn("muse login", entry["reason"])


class TestAgyDispatchWriteBack(_IsolatedStatus):
    """runtime-state#3 / replay-0926-memory#17 / py-reliability#3."""

    def _dispatch(self, stderr_text):
        with patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("makewand.providers.agy.run_subprocess", return_value=(1, "", stderr_text, None)):
            return execute_agy_task("审查这段代码", cwd=self._tmp.name, readonly=True)

    def test_region_block_is_written_back_with_ttl(self):
        ok, _, err = self._dispatch("Error: Gemini Code Assist is not currently available in your location.")
        self.assertFalse(ok)
        entry = load_status_cache()["agy"]
        self.assertEqual(entry["status"], "error")
        self.assertEqual(entry["source"], "dispatch")
        self.assertEqual(entry.get("ttl_seconds"), 6 * 3600)
        self.assertIn("地区", entry["reason"])
        self.assertIn("地区", err)
        self.assertEqual(calculate_dynamic_pacing("agy", entry)["routing_boost"], -999.0)

    def test_auth_failure_is_written_back_as_needs_auth(self):
        ok, _, _ = self._dispatch("Error: authentication required. Please sign in.")
        self.assertFalse(ok)
        entry = load_status_cache()["agy"]
        self.assertEqual(entry["status"], "needs_auth")
        self.assertIn("重新登录", entry["reason"])

    def test_task_level_failure_does_not_mark_provider(self):
        ok, _, _ = self._dispatch("Traceback: SyntaxError in generated file")
        self.assertFalse(ok)
        self.assertEqual(load_status_cache()["agy"]["status"], "unknown")

    def test_version_probe_is_unverified_and_keeps_dispatch_failure(self):
        self._dispatch("Error: this service is not available in your country")
        with patch("makewand.config.get_all_supported_providers", return_value=["agy"]), \
             patch("makewand.config.is_provider_enabled", return_value=True), \
             patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.health.run_subprocess", return_value=(0, "agy 9.9.9", "", None)):
            fresh = probe_model("agy")
            cache = get_or_update_status(force_probe=True)
        self.assertEqual(fresh["status"], "healthy")
        self.assertIs(fresh["verified"], False)
        self.assertIn("未经真实调用验证", fresh["reason"])
        self.assertNotIn("运行就绪", fresh["reason"])
        self.assertEqual(cache["agy"]["status"], "error", "version probe must not erase a live dispatch failure")


class TestProbeMutex(_IsolatedStatus):
    """py-reliability#3: concurrent probes must not double the real model calls."""

    def test_waiter_reuses_fresh_results(self):
        providers = ["claude", "codex"]
        calls = []

        def fake_probe(name):
            calls.append(name)
            return {"status": "healthy", "reason": "ok", "resets_at": None, "updated_at": datetime.now().isoformat()}

        lock_path = self.status_file.parent / ".probe.lock"
        holder = open(lock_path, "a+")
        filelock.flock(holder.fileno(), filelock.LOCK_EX)
        result = {}
        with patch("makewand.config.get_all_supported_providers", return_value=providers), \
             patch("makewand.health.probe_model", side_effect=fake_probe):
            worker = threading.Thread(target=lambda: result.setdefault("cache", get_or_update_status(force_probe=True)))
            worker.start()
            time.sleep(0.3)
            self.assertTrue(worker.is_alive(), "second probe must wait for the running one")
            # The first prober finishes and publishes fresh results.
            save_status_cache({p: fake_probe(p) for p in providers})
            calls.clear()
            filelock.flock(holder.fileno(), filelock.LOCK_UN)
            holder.close()
            worker.join(10)
        self.assertFalse(worker.is_alive())
        self.assertEqual(calls, [], "waiter must reuse the fresh probe instead of calling the CLIs again")
        self.assertEqual(result["cache"]["claude"]["status"], "healthy")


if __name__ == "__main__":
    unittest.main()
