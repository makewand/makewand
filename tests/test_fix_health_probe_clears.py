"""
py-rel-21: `makewand probe` (as the re-login hint instructs) and a later
successful real dispatch clear stale dispatch-recorded needs_auth / error
records. agy's probe used to be `agy --version` only, which can never retire a
failure, so a re-logged-in agy stayed excluded until the TTL ran out.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import tempfile
import unittest
from datetime import datetime, timedelta
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
import makewand.usage as usage

KEYRING_OK = "I1012 08:11:57.965555       1 auth.go:157] ChainedAuth: authenticated via keyring (effective: keyring)\n"
REGION_REASON = "Antigravity 在当前地区/账号不可用 (真实派发返回地区限制)"


def _is_real_agy_call(cmd):
    seq = list(cmd)
    return any(seq[i] == "agy" and i + 1 < len(seq) and seq[i + 1] == "-p" for i in range(len(seq)))

class _Isolated(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-fix-probe-")
        root = Path(self._tmp.name)
        for p in (patch.object(config, "CONFIG_DIR", root / "cfg"),
                  patch.object(health, "STATUS_CACHE_FILE", root / "cfg" / "status.json"),
                  patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json"),
                  patch.object(usage, "USAGE_WINDOW_FILE", root / "usage_window.json")):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        (root / "cfg").mkdir(parents=True)


class TestProbeRetiresDispatchFailures(_Isolated):
    def probe(self, real_call_result):
        calls = []

        def fake_run(cmd, *args, **kwargs):
            calls.append(list(cmd))
            if list(cmd)[:2] == ["agy", "--version"]:
                return 0, "agy 9.9.9", "", None
            return real_call_result

        with patch("makewand.config.get_all_supported_providers", return_value=["agy"]), \
             patch("makewand.config.is_provider_enabled", return_value=True), \
             patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.health.run_subprocess", side_effect=fake_run):
            cache = health.get_or_update_status(force_probe=True)
        return cache["agy"], calls

    def test_successful_real_probe_clears_dispatch_needs_auth(self):
        health.record_engine_failure("agy", "needs_auth", f"Antigravity 认证失败 (真实派发) · {health.get_reauth_hint('agy')}")
        self.assertIn("makewand probe", health.get_reauth_hint("agy"))
        entry, calls = self.probe((0, "ok\n", KEYRING_OK, None))
        self.assertEqual(entry["status"], "healthy")
        self.assertIs(entry["verified"], True)
        real = [c for c in calls if _is_real_agy_call(c)]
        self.assertEqual(len(real), 1, calls)
        self.assertIn("plan", real[0])  # read-only probe
        self.assertEqual(health.load_status_cache()["agy"]["status"], "healthy")

    def test_successful_real_probe_clears_dispatch_region_block(self):
        health.record_engine_failure("agy", "error", REGION_REASON, ttl_seconds=health.REGION_BLOCK_TTL_SECONDS)
        entry, _ = self.probe((0, "ok\n", "", None))
        self.assertEqual(entry["status"], "healthy")

    def test_failed_real_probe_refreshes_the_failure(self):
        health.record_engine_failure("agy", "error", REGION_REASON, ttl_seconds=health.REGION_BLOCK_TTL_SECONDS)
        entry, _ = self.probe((1, "", "Error: Gemini Code Assist is not currently available in your location.", None))
        self.assertEqual(entry["status"], "error")
        self.assertEqual(entry.get("ttl_seconds"), health.REGION_BLOCK_TTL_SECONDS)
        self.assertEqual(entry.get("source"), "probe")

    def test_inconclusive_real_probe_keeps_the_dispatch_failure(self):
        health.record_engine_failure("agy", "needs_auth", "Antigravity 认证失败 (真实派发)")
        entry, _ = self.probe((-1, "", "", "Command timed out after 60 seconds"))
        self.assertEqual(entry["status"], "needs_auth")
        self.assertIn("保留真实派发失败记录", entry["reason"])

    def test_no_real_call_without_a_dispatch_failure(self):
        entry, calls = self.probe((0, "ok\n", "", None))
        self.assertIs(entry["verified"], False)
        self.assertEqual([c for c in calls if _is_real_agy_call(c)], [])


class TestSuccessfulDispatchRetiresFailures(_Isolated):
    def test_later_successful_dispatch_clears_dispatch_error(self):
        health.record_engine_failure("agy", "error", REGION_REASON, ttl_seconds=health.REGION_BLOCK_TTL_SECONDS)
        self.assertEqual(health.load_status_cache()["agy"]["status"], "error")
        usage.record_engine_usage("agy", tier="standard", success=False, task="still blocked")
        self.assertEqual(health.load_status_cache()["agy"]["status"], "error")
        usage.record_engine_usage("agy", tier="standard", success=True, task="forced agy run")
        entry = health.load_status_cache()["agy"]
        self.assertEqual(entry["status"], "healthy")
        self.assertEqual(entry["superseded"], "error")

    def test_success_before_the_failure_does_not_count(self):
        usage.record_engine_usage("agy", tier="standard", success=True, task="earlier run")
        health.save_status_cache({"agy": {"status": "error", "reason": REGION_REASON, "resets_at": None,
                                          "source": "dispatch", "ttl_seconds": health.REGION_BLOCK_TTL_SECONDS,
                                          "updated_at": (datetime.now() + timedelta(seconds=1)).isoformat()}})
        self.assertEqual(health.load_status_cache()["agy"]["status"], "error")

    def test_api_fallback_success_does_not_clear_cli_login_failure(self):
        health.record_engine_failure("grok", "needs_auth", "未登录")
        # Key configured and paid API allowed: the ledger cannot tell which route served it.
        with patch("makewand.config.has_api_configured", return_value=True), \
             patch("makewand.config.is_api_allowed", return_value=True):
            usage.record_engine_usage("grok", tier="standard", success=True, task="served by XAI_API_KEY")
        with patch("makewand.config.has_api_configured", return_value=True):
            self.assertEqual(health.load_status_cache()["grok"]["status"], "needs_auth")
        with patch("makewand.config.has_api_configured", return_value=False):
            self.assertEqual(health.load_status_cache()["grok"]["status"], "healthy")


if __name__ == "__main__":
    unittest.main()
