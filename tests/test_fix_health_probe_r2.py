"""
py-rel-21 (round 2): failure verdicts recorded by the real agy probe stay
protected, later successes retire them, and an inconclusive probe says what it saw.

Round 1 added one real `agy -p` call to `makewand probe` while a dispatch failure
was live, but the verdict that call wrote (source "probe") was protected by
nothing: the next probe flipped a still region-blocked or still logged-out agy to
healthy with zero real calls. A later successful dispatch did not clear a
probe-recorded failure, an inconclusive real probe was reported as a version
check, and a success relayed to the API key was indistinguishable from a CLI run.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import contextlib
import inspect
import io
import json
import os
import shutil
import stat
import tempfile
import unittest
from datetime import datetime, timedelta
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
import makewand.usage as usage
from makewand.engine_selection import _engine_usable

REGION = "Error: Gemini Code Assist is not currently available in your location."
AUTH = 'Error: authentication failed: oauth2: "invalid_grant" "Token has been expired or revoked."'
QUOTA = "Error: You have exhausted your quota on this model."
GLOG_NOTICE = "I1012 08:11:57.843790       1 auth.go:120] You are not logged into Antigravity.\n"
KEYRING_OK = "I1012 08:11:57.965555       1 auth.go:157] ChainedAuth: authenticated via keyring (effective: keyring)\n"
REGION_REASON = "Antigravity 在当前地区/账号不可用 (真实派发返回地区限制)"
AUTH_REASON = f"Antigravity 认证失败 (真实派发) · {health.get_reauth_hint('agy')}"


class _Isolated(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-fix-probe-r2-")
        root = Path(self._tmp.name)
        self.root = root
        for p in (patch.object(config, "CONFIG_DIR", root / "cfg"),
                  patch.object(health, "STATUS_CACHE_FILE", root / "cfg" / "status.json"),
                  patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json"),
                  patch.object(usage, "USAGE_WINDOW_FILE", root / "usage_window.json")):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        (root / "cfg").mkdir(parents=True)

    def probe(self, real_result):
        """One `makewand probe` of agy; real_result answers the real `agy -p` call."""
        calls = []

        def fake_run(cmd, *args, **kwargs):
            calls.append(list(cmd))
            if list(cmd)[:2] == ["agy", "--version"]:
                return 0, "agy 9.9.9", "", None
            return real_result

        with patch("makewand.config.get_all_supported_providers", return_value=["agy"]), \
             patch("makewand.config.is_provider_enabled", return_value=True), \
             patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.health.run_subprocess", side_effect=fake_run):
            cache = health.get_or_update_status(force_probe=True)
        return cache["agy"], [c for c in calls if c[:2] == ["agy", "-p"]]

    @staticmethod
    def usable(cache_entry):
        with patch("makewand.config.is_provider_enabled", return_value=True):
            return _engine_usable("agy", {"agy": cache_entry})[0]


class TestProbeRecordedFailuresStayProtected(_Isolated):
    """Sub-item 1: a live failure, whoever recorded it, needs a real call to retire it."""

    def check_survives(self, status, ttl, stderr, reason):
        health.record_engine_failure("agy", status, reason, ttl_seconds=ttl)
        first, calls = self.probe((1, "", stderr, None))
        self.assertEqual((first["status"], first.get("source"), len(calls)), (status, "probe", 1))
        for attempt in (2, 3):  # still blocked / still logged out
            entry, calls = self.probe((1, "", stderr, None))
            self.assertEqual(len(calls), 1, f"probe #{attempt} must make the real call again")
            self.assertEqual(entry["status"], status, f"probe #{attempt} erased a live verdict")
            self.assertEqual(health.load_status_cache()["agy"]["status"], status)
            self.assertFalse(self.usable(health.load_status_cache()["agy"]))
        return entry

    def test_region_block_survives_repeated_probes(self):
        entry = self.check_survives("error", health.REGION_BLOCK_TTL_SECONDS, REGION, REGION_REASON)
        self.assertEqual(entry.get("ttl_seconds"), health.REGION_BLOCK_TTL_SECONDS)

    def test_login_failure_survives_repeated_probes(self):
        self.check_survives("needs_auth", None, AUTH, AUTH_REASON)

    def test_real_success_after_probe_recorded_failure_clears_it(self):
        health.record_engine_failure("agy", "error", REGION_REASON, ttl_seconds=health.REGION_BLOCK_TTL_SECONDS)
        self.probe((1, "", REGION, None))
        entry, calls = self.probe((0, "ok\n", KEYRING_OK, None))
        self.assertEqual(len(calls), 1)
        self.assertEqual(entry["status"], "healthy")
        self.assertIs(entry["verified"], True)
        self.assertTrue(self.usable(health.load_status_cache()["agy"]))

    def test_probe_recorded_failure_alone_triggers_the_real_call(self):
        health.record_engine_failure("agy", "needs_auth", "Antigravity 认证失败 (真实探活)", source="probe")
        entry, calls = self.probe((1, "", GLOG_NOTICE + AUTH, None))
        self.assertEqual(len(calls), 1)
        self.assertEqual(entry["status"], "needs_auth")

    def test_quota_limit_found_by_the_real_probe_is_protected_too(self):
        health.record_engine_failure("agy", "error", REGION_REASON, ttl_seconds=health.REGION_BLOCK_TTL_SECONDS)
        entry, _ = self.probe((1, "", QUOTA, None))
        self.assertEqual(entry["status"], "limited")
        entry, calls = self.probe((1, "", QUOTA, None))
        self.assertEqual((entry["status"], len(calls)), ("limited", 1))
        entry, calls = self.probe((0, "ok\n", "", None))
        self.assertEqual((entry["status"], entry.get("verified"), len(calls)), ("healthy", True, 1))

    def test_dispatch_recorded_quota_limit_gets_a_real_call(self):
        health.record_engine_limit("agy", "Google AI 配额暂时耗尽", "待重置")
        entry, calls = self.probe((1, "", QUOTA, None))
        self.assertEqual((entry["status"], len(calls)), ("limited", 1))
        entry, calls = self.probe((0, "ok\n", "", None))
        self.assertEqual((entry["status"], entry.get("verified")), ("healthy", True))

    def test_no_real_call_without_a_live_failure(self):
        health.record_engine_failure("agy", "error", REGION_REASON)
        stale = json.loads(health.STATUS_CACHE_FILE.read_text(encoding="utf-8"))
        stale["agy"]["updated_at"] = (datetime.now() - timedelta(hours=1)).isoformat()  # 30 min TTL passed
        health.STATUS_CACHE_FILE.write_text(json.dumps(stale), encoding="utf-8")
        entry, calls = self.probe((0, "ok\n", "", None))
        self.assertEqual(calls, [])
        self.assertIs(entry["verified"], False)


class TestInconclusiveProbeShowsWhatItSaw(_Isolated):
    """Sub-items 3 and 4: the kept failure names the real probe's own outcome."""

    def test_runtime_deadline_timeout_is_reported_as_a_timeout(self):
        health.record_engine_failure("agy", "needs_auth", AUTH_REASON)

        def fake_run(cmd, *args, **kwargs):
            return 0, "agy 9.9.9", "", None

        with patch("makewand.config.get_all_supported_providers", return_value=["agy"]), \
             patch("makewand.config.is_provider_enabled", return_value=True), \
             patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.health.run_subprocess", side_effect=fake_run), \
             patch("makewand.health._run_model_probe", return_value=(124, "", "", "execution deadline expired")) as real:
            entry = health.get_or_update_status(force_probe=True)["agy"]
        real.assert_called_once()
        self.assertEqual(entry["status"], "needs_auth")
        self.assertIn("真实探活超时", entry["reason"])
        self.assertNotIn("版本", entry["reason"])
        self.assertIn("保留真实派发失败记录", entry["reason"])

    def test_runner_timeout_is_reported_as_a_timeout(self):
        health.record_engine_failure("agy", "error", REGION_REASON, ttl_seconds=health.REGION_BLOCK_TTL_SECONDS)
        entry, _ = self.probe((-1, "", "", "Command timed out after 60 seconds"))
        self.assertEqual(entry["status"], "error")
        self.assertIn("真实探活超时", entry["reason"])

    def test_unrecognised_failure_shows_its_own_message(self):
        health.record_engine_failure("agy", "needs_auth", AUTH_REASON)
        entry, _ = self.probe((2, "", "panic: runtime error: index out of range [3] with length 3", None))
        self.assertEqual(entry["status"], "needs_auth")
        self.assertIn("panic: runtime error", entry["reason"])
        self.assertNotIn("版本", entry["reason"])

    def test_glog_only_exit_zero_real_probe_does_not_clear(self):
        health.record_engine_failure("agy", "error", REGION_REASON, ttl_seconds=health.REGION_BLOCK_TTL_SECONDS)
        entry, calls = self.probe((0, "", GLOG_NOTICE + "I1012 08:11:58.000001       1 print.go:88] "
                                      "Print mode: not authenticated, trying silent auth\n", None))
        self.assertEqual(len(calls), 1)
        self.assertEqual(entry["status"], "error")
        self.assertNotEqual(entry.get("verified"), True)
        self.assertIn("未返回有效输出", entry["reason"])

    def test_repeated_inconclusive_probes_do_not_stack_notes(self):
        health.record_engine_failure("agy", "needs_auth", AUTH_REASON)
        for _ in range(3):
            entry, _ = self.probe((-1, "", "", "Command timed out after 60 seconds"))
        self.assertEqual(entry["reason"].count(health._PROBE_NOTE_MARK), 1)
        self.assertTrue(entry["reason"].startswith(AUTH_REASON))

    def test_version_only_verdict_is_named_as_such(self):
        # A version-only verdict merged on its own keeps the failure and says what it was.
        previous = {"status": "error", "reason": REGION_REASON, "source": "dispatch",
                    "updated_at": datetime.now().isoformat(), "ttl_seconds": health.REGION_BLOCK_TTL_SECONDS}
        version_only = {"status": "healthy", "verified": False, "reason": "Antigravity 已安装 (9.9.9)"}
        kept = health._merge_probe_result("agy", previous, version_only)
        self.assertEqual(kept["status"], "error")
        self.assertIn("版本检测", kept["reason"])


class TestProbeTimeoutRecognition(_Isolated):
    """The shared runtime reports a probe that outlived its deadline as 124 / "execution deadline expired"."""

    def test_runtime_deadline_is_a_timeout_for_every_real_probe(self):
        for engine in ("claude", "codex", "grok", "muse"):
            with self.subTest(engine=engine), \
                 patch("makewand.config.is_provider_enabled", return_value=True), \
                 patch("makewand.config.has_subscription_configured", return_value=True), \
                 patch("makewand.config.has_api_configured", return_value=False), \
                 patch("makewand.health._run_model_probe", return_value=(124, "", "", "execution deadline expired")):
                verdict = health.probe_model(engine)
                self.assertEqual(verdict["status"], "warning", "a slow probe is inconclusive, not an error")
                self.assertIn("超时", verdict["reason"])


class TestDispatchRetiresProbeRecordedFailures(_Isolated):
    """Sub-item 2: a later real CLI success clears a probe-recorded failure."""

    def test_cli_success_clears_a_probe_recorded_region_block(self):
        health.record_engine_failure("agy", "error", REGION_REASON, ttl_seconds=health.REGION_BLOCK_TTL_SECONDS)
        self.probe((1, "", REGION, None))
        self.assertEqual(health.load_status_cache()["agy"]["source"], "probe")
        usage.record_engine_usage("agy", tier="standard", success=True, task="forced agy run", route="cli")
        entry = health.load_status_cache()["agy"]
        self.assertEqual((entry["status"], entry["superseded"]), ("healthy", "error"))

    def test_cli_success_clears_a_probe_recorded_login_failure(self):
        health.record_engine_failure("agy", "needs_auth", "Antigravity 认证失败 (真实探活)", source="probe")
        usage.record_engine_usage("agy", tier="standard", success=True, task="agy run", route="cli")
        self.assertEqual(health.load_status_cache()["agy"]["status"], "healthy")


@unittest.skipIf(os.name == "nt", "uses a POSIX shell script as the fake agy")
class FakeAgyDispatch(_Isolated):
    """Sub-item 4: real dispatches through orchestrator.dispatch_task with a fake agy on PATH."""

    SCRIPTS = {
        "region": f"echo '{GLOG_NOTICE.strip()}' >&2\necho '{REGION}' >&2\nexit 1\n",
        "glog_exit0": (f"echo '{GLOG_NOTICE.strip()}' >&2\n"
                       "echo 'I1012 08:11:58.000001       1 print.go:88] Print mode: not authenticated, "
                       "trying silent auth' >&2\nexit 0\n"),
        "ok": f"echo '{KEYRING_OK.strip()}' >&2\necho ok\nexit 0\n",
    }

    def setUp(self):
        super().setUp()
        # Outside the task directory: binaries under cwd are never resolved (safe_which).
        self.bin = Path(tempfile.mkdtemp(prefix="mw-fake-agy-"))
        self.addCleanup(shutil.rmtree, self.bin, True)
        self.work = self.root / "work"
        self.work.mkdir()
        self.agy = self.bin / "agy"
        path = patch.dict(os.environ, {"PATH": f"{self.bin}{os.pathsep}{os.environ['PATH']}"})
        path.start()
        self.addCleanup(path.stop)

    def dispatch(self, behaviour):
        self.agy.write_text("#!/bin/sh\n" + self.SCRIPTS[behaviour], encoding="utf-8")
        self.agy.chmod(self.agy.stat().st_mode | stat.S_IXUSR)
        from makewand import orchestrator
        with patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(True, "config-ack")), \
             patch("makewand.sandbox.audit_unsafe_host_exec"), \
             contextlib.redirect_stderr(io.StringIO()):
            return orchestrator.dispatch_task("agy", "审查这段代码", cwd=str(self.work), readonly=True, timeout=30)

    def test_glog_only_exit_zero_does_not_clear_a_region_block(self):
        self.assertFalse(self.dispatch("region")[0])
        self.assertEqual(health.load_status_cache()["agy"]["status"], "error")
        result = self.dispatch("glog_exit0")
        self.assertFalse(result[0], "an exit-0 run without an answer is a failure")
        entry = health.load_status_cache()["agy"]
        self.assertEqual(entry["status"], "error")
        self.assertNotEqual(entry.get("verified"), True)
        self.assertFalse(usage.get_dispatch_records("agy")[-1]["success"])

    def test_real_answer_clears_the_region_block(self):
        self.dispatch("region")
        self.assertTrue(self.dispatch("ok")[0])
        record = usage.get_dispatch_records("agy")[-1]
        self.assertEqual((record["success"], record.get("route")), (True, "cli"))
        entry = health.load_status_cache()["agy"]
        self.assertEqual((entry["status"], entry.get("verified")), ("healthy", True))


class TestCliRouteEvidence(_Isolated):
    """Sub-item 5: with an API key, only a success the ledger knows was a CLI run clears needs_auth."""

    def record(self, route=None, **policy):
        with patch("makewand.config.has_api_configured", return_value=policy.get("key", True)), \
             patch("makewand.config.is_api_allowed", return_value=policy.get("allowed", True)):
            usage.record_engine_usage("claude", tier="standard", success=True, task="run", route=route)

    def status_with_key(self, key=True):
        with patch("makewand.config.has_api_configured", return_value=key):
            return health.load_status_cache()["claude"]["status"]

    def test_cli_success_clears_needs_auth_although_an_api_key_is_set(self):
        health.record_engine_failure("claude", "needs_auth", "未登录")
        self.record(route="cli")
        self.assertEqual(self.status_with_key(), "healthy")

    def test_api_success_never_clears_a_cli_failure(self):
        health.record_engine_failure("claude", "needs_auth", "未登录")
        health.record_engine_failure("agy", "error", REGION_REASON, ttl_seconds=health.REGION_BLOCK_TTL_SECONDS)
        self.record(route="api")
        with patch("makewand.config.has_api_configured", return_value=True), \
             patch("makewand.config.is_api_allowed", return_value=True):
            usage.record_engine_usage("agy", tier="standard", success=True, task="relayed", route="api")
        self.assertEqual(self.status_with_key(), "needs_auth")
        self.assertEqual(self.status_with_key(key=False), "needs_auth")
        self.assertEqual(health.load_status_cache()["agy"]["status"], "error")

    def test_success_of_unknown_route_clears_needs_auth_only_without_an_api_key(self):
        health.record_engine_failure("claude", "needs_auth", "未登录")
        self.record()  # key configured and paid API allowed: either route was possible
        self.assertNotIn("route", usage.get_dispatch_records("claude")[-1])
        self.assertEqual(self.status_with_key(), "needs_auth")
        self.assertEqual(self.status_with_key(key=False), "healthy")

    def test_success_under_subscription_only_policy_counts_as_cli(self):
        health.record_engine_failure("claude", "needs_auth", "未登录")
        self.record(allowed=False)  # the API relay is refused, so the CLI served it
        self.assertEqual(usage.get_dispatch_records("claude")[-1]["route"], "cli")
        self.assertEqual(self.status_with_key(), "healthy")

    def test_route_inference(self):
        cases = ((dict(key=False, allowed=True), "cli"), (dict(key=True, allowed=False), "cli"),
                 (dict(key=True, allowed=True), None))
        for policy, expected in cases:
            with self.subTest(policy=policy), \
                 patch("makewand.config.has_api_configured", return_value=policy["key"]), \
                 patch("makewand.config.is_api_allowed", return_value=policy["allowed"]):
                self.assertEqual(usage.dispatch_route("claude"), expected)
                self.assertIsNone(usage.dispatch_route("deepseek"))
        self.record(route="api", key=False, allowed=False)
        self.assertEqual(usage.get_dispatch_records("claude")[-1]["route"], "api")


class TestPublicLedgerAccessor(_Isolated):
    """Sub-item 6: health reads the ledger through usage.get_dispatch_records."""

    def test_health_no_longer_uses_the_private_reader(self):
        self.assertNotIn("_load_raw_usage_records", inspect.getsource(health))

    def test_get_dispatch_records_filters_by_engine_and_returns_copies(self):
        usage.record_engine_usage("agy", tier="fast", success=False, task="a")
        usage.record_engine_usage("codex", tier="standard", success=True, task="b")
        records = usage.get_dispatch_records("AGY ")
        self.assertEqual([(r["engine"], r["success"]) for r in records], [("agy", False)])
        records[0]["success"] = True
        self.assertFalse(usage.get_dispatch_records("agy")[0]["success"])
        self.assertEqual(len(usage.get_dispatch_records()), 2)

    def test_superseding_reads_the_public_accessor(self):
        health.record_engine_failure("agy", "error", REGION_REASON, ttl_seconds=health.REGION_BLOCK_TTL_SECONDS)
        later = (datetime.now() + timedelta(seconds=5)).isoformat()
        with patch("makewand.usage.get_dispatch_records",
                   return_value=[{"timestamp": later, "engine": "agy", "success": True, "route": "cli"}]) as reader:
            self.assertEqual(health.load_status_cache()["agy"]["status"], "healthy")
        reader.assert_called_with("agy", max_age_days=2.0)


if __name__ == "__main__":
    unittest.main()
