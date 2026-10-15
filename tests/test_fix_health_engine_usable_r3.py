"""
py-rel-19 (round 3): with no usable engine nothing is dispatched and the user
is told why for each engine, and every consumer reads failure verdicts alike.

Round 2 left hard-coded fallbacks reachable once selection stopped naming
unusable engines: a REPL turn dispatched agy, agy, claude, codex and local, and
an ensemble fell back to an unavailable local model. Failure verdicts without a
timestamp were stamped on every load and so never decayed until some save;
`makewand status` recommended "claude -> claude" with every engine in error;
the claude and codex adapters ran the logged-out CLI on needs_auth although
selection counted the state as relayed to a configured API key; and a failure
entry dated in the future survived `makewand probe` because save_status_cache
kept the entry whose updated_at string was larger.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import argparse
import contextlib
import os
import io
import json
import tempfile
import unittest
from datetime import datetime, timedelta
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
import makewand.interactive as interactive
from makewand import cli
from makewand.engine_selection import _engine_usable, _usable_or_api_relay, select_optimal_engine_pair
from makewand.ensemble import resolve_ensemble_providers, run_ensemble
from makewand.providers.claude import execute_claude_task
from makewand.providers.codex import execute_codex_task
from makewand.sandbox import SandboxUnavailableError

FAR_FUTURE = "9999-12-31T23:59:59"


def ago(**kwargs):
    return (datetime.now() - timedelta(**kwargs)).isoformat()


def ahead(**kwargs):
    return (datetime.now() + timedelta(**kwargs)).isoformat()


def live(status, **extra):
    return dict({"status": status, "reason": f"{status} reason", "updated_at": ago(minutes=1)}, **extra)


@contextlib.contextmanager
def routing(pool=("claude", "codex"), api=()):
    with patch("makewand.config.get_active_providers", return_value=list(pool)), \
         patch("makewand.config.is_provider_enabled", return_value=True), \
         patch("makewand.config.has_api_configured", side_effect=lambda e: e in api), \
         patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
         patch("makewand.usage.get_engine_usage_stats", return_value={}), \
         patch("makewand.usage.get_all_engine_reliability", return_value={}):
        yield


class _IsolatedStatusFile(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-fix-usable-r3-")
        root = Path(self._tmp.name)
        for p in (patch.object(config, "CONFIG_DIR", root / "cfg"),
                  patch.object(health, "STATUS_CACHE_FILE", root / "cfg" / "status.json"),
                  patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json"),
                  patch("makewand.config.is_provider_enabled", return_value=True)):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        (root / "cfg").mkdir(parents=True)

    def write(self, data):
        health.STATUS_CACHE_FILE.write_text(json.dumps(data), encoding="utf-8")

    def on_disk(self):
        return json.loads(health.STATUS_CACHE_FILE.read_text(encoding="utf-8"))


class TestNothingIsDispatchedWithoutAUsableEngine(unittest.TestCase):
    CACHE = {"claude": live("error", reason="broken CLI"), "codex": live("needs_auth", reason="login expired"),
             "agy": live("missing", reason="agy is not installed"), "grok": live("missing"),
             "muse": live("missing"), "local": live("missing")}

    def test_repl_turn_dispatches_nothing_and_names_each_engines_reason(self):
        calls, history, out = [], [], io.StringIO()

        def dispatch(engine, prompt, **kwargs):
            calls.append(engine)
            return False, None, f"fake failure {engine}"

        with routing(), patch.object(interactive, "get_or_update_status", return_value=self.CACHE), \
             patch.object(interactive, "dispatch_task", side_effect=dispatch), contextlib.redirect_stdout(out):
            interactive.handle_conversational_turn("解释一下这个函数", history, tempfile.gettempdir(), stream=False)
        self.assertEqual(calls, [], "no engine may be dispatched (the fallback used to be agy, agy, claude, codex, local)")
        self.assertEqual(history, [])
        text = out.getvalue()
        for expected in ("• claude:", "broken CLI", "• codex:", "login expired", "codex login", "makewand probe"):
            self.assertIn(expected, text)
        self.assertNotIn("• agy", text)

    def test_run_outcome_error_names_each_engines_reason(self):
        from makewand.engine_selection import _no_provider_error
        with routing():
            _, _, meta = select_optimal_engine_pair("解释一下这个函数", cache=self.CACHE)
        error = _no_provider_error(meta)
        self.assertTrue(error.startswith("No usable execution provider: "), error)
        for expected in ("claude", "broken CLI", "codex", "login expired"):
            self.assertIn(expected, error)
        with patch("makewand.config.get_active_providers", return_value=[]):
            self.assertEqual(_no_provider_error({}), "No enabled execution provider")

    def test_repl_turn_still_dispatches_the_usable_engines_only(self):
        calls = []
        cache = dict(self.CACHE, claude=live("healthy"))
        with routing(), patch.object(interactive, "get_or_update_status", return_value=cache), \
             patch.object(interactive, "dispatch_task", side_effect=lambda e, p, **k: calls.append(e) or (False, None, "x")), \
             contextlib.redirect_stdout(io.StringIO()):
            interactive.handle_conversational_turn("解释一下这个函数", [], tempfile.gettempdir(), stream=False)
        self.assertEqual(calls, ["claude"])

    @contextlib.contextmanager
    def ensemble_env(self, cache, local=(False, "Ollama 未响应", []), enabled=lambda name: True):
        with patch("makewand.health.load_status_cache", return_value=cache), \
             patch("makewand.ensemble.is_provider_enabled", side_effect=enabled), \
             patch("makewand.config.is_provider_enabled", side_effect=enabled), \
             patch("makewand.ensemble.has_subscription_configured", side_effect=lambda name: name != "local"), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.providers.local.is_local_model_available", return_value=local):
            yield

    def test_ensemble_without_a_usable_engine_is_empty_and_says_why(self):
        cache = dict(self.CACHE, agy=live("error", reason="region blocked", ttl_seconds=6 * 3600),
                     grok=live("limited", resets_at="in 3 hours"), muse=live("needs_auth"))
        unavailable = {}
        with self.ensemble_env(cache):
            self.assertEqual(resolve_ensemble_providers(None, unavailable=unavailable), [])
        self.assertEqual(sorted(unavailable), ["agy", "claude", "codex", "grok", "local", "muse"])
        self.assertIn("region blocked", unavailable["agy"])
        self.assertIn("Ollama 未响应", unavailable["local"])

        dispatched = []
        with self.ensemble_env(cache), \
             patch("makewand.orchestrator.dispatch_task", side_effect=lambda e, p, **k: dispatched.append(e) or (True, "ok", None)), \
             contextlib.redirect_stderr(io.StringIO()) as err:
            result = run_ensemble("审查这段代码", providers="claude,codex,agy")
        self.assertEqual(dispatched, [], "the local fallback used to be dispatched although it is not running")
        self.assertFalse(result["ok"])
        for engine in ("claude", "codex", "agy", "local"):
            self.assertIn(engine, result["error"])
            self.assertIn(engine, result["unavailable"])
            self.assertIn(f"• {engine}:", err.getvalue())

    def test_ensemble_falls_back_only_to_an_engine_that_can_run(self):
        cache = {"claude": live("error"), "agy": live("error", ttl_seconds=6 * 3600)}
        with self.ensemble_env(cache, local=(True, "", ["qwen2.5-coder"])):
            self.assertEqual(resolve_ensemble_providers(["claude"]), ["local"])
        with self.ensemble_env(cache, local=(True, "", ["qwen2.5-coder"]), enabled=lambda name: name != "local"):
            self.assertEqual(resolve_ensemble_providers(["claude"]), [])
        with self.ensemble_env({"claude": live("error")}):
            self.assertEqual(resolve_ensemble_providers(["claude"]), ["agy"])


class TestVerdictWithoutUsableTimestampDecays(_IsolatedStatusFile):
    def test_persisted_verdict_without_timestamp_decays_on_the_first_load(self):
        self.write({"agy": {"status": "error", "reason": "legacy entry"}, "codex": {"status": "needs_auth"}})
        for _ in range(2):  # every load, not only after some save wrote a timestamp
            cache = health.load_status_cache()
            self.assertEqual((cache["agy"]["status"], cache["codex"]["status"]), ("unknown", "unknown"))
            self.assertEqual(cache["agy"]["expired_from"], "error")
            self.assertIn("缺少有效的记录时间", cache["agy"]["reason"])
        self.assertTrue(_engine_usable("agy", {"agy": {"status": "error", "reason": "legacy entry"}})[0])
        self.assertTrue(_engine_usable("codex", {"codex": {"status": "needs_auth"}})[0])

    def test_selection_reads_a_raw_cache_as_load_status_cache_does(self):
        raw = {"claude": {"status": "error", "reason": "legacy entry"}, "codex": live("healthy"),
               "grok": live("needs_auth", updated_at=ago(minutes=31))}
        self.write(raw)
        loaded = health.load_status_cache()
        self.assertEqual((loaded["claude"]["status"], loaded["grok"]["status"]), ("unknown", "unknown"))
        with routing(pool=("claude", "codex", "grok")):
            from_raw = select_optimal_engine_pair("编写 react 组件", cache=json.loads(json.dumps(raw)))
            from_loaded = select_optimal_engine_pair("编写 react 组件", cache=loaded)
        self.assertEqual(from_raw[:2], from_loaded[:2])
        self.assertEqual(from_raw[0][0], "claude")
        self.assertEqual(raw["claude"]["status"], "error", "the caller's cache is not modified")


class TestFutureTimestampIsNoEvidence(_IsolatedStatusFile):
    def test_far_future_failure_is_expired_but_clock_skew_is_tolerated(self):
        for stamp in (FAR_FUTURE, ahead(hours=1), ahead(minutes=6)):
            for status in ("error", "needs_auth"):
                with self.subTest(stamp=stamp, status=status):
                    entry = {"status": status, "reason": "r", "updated_at": stamp, "source": "dispatch"}
                    self.assertTrue(health.failure_verdict_expired(entry))
                    self.assertTrue(_engine_usable("claude", {"claude": entry})[0])
        skewed = {"status": "error", "reason": "r", "updated_at": ahead(seconds=30)}
        self.assertFalse(health.failure_verdict_expired(skewed))
        self.assertFalse(_engine_usable("claude", {"claude": skewed})[0])

    def test_far_future_disk_entry_does_not_block_a_newer_verdict(self):
        self.write({"claude": {"status": "error", "reason": "e", "updated_at": FAR_FUTURE, "source": "dispatch"}})
        health.save_status_cache({"claude": {"status": "healthy", "reason": "ok", "resets_at": None,
                                             "updated_at": datetime.now().isoformat()}})
        self.assertEqual(self.on_disk()["claude"]["status"], "healthy")

    def test_newer_disk_verdict_still_wins_over_an_older_or_damaged_snapshot(self):
        self.write({"claude": {"status": "healthy", "reason": "ok", "updated_at": datetime.now().isoformat()}})
        for stamp in (ago(hours=1), "garbage", FAR_FUTURE):
            with self.subTest(stamp=stamp):
                health.save_status_cache({"claude": {"status": "limited", "reason": "old 429",
                                                     "resets_at": "in 1 hour", "updated_at": stamp}})
                self.assertEqual(self.on_disk()["claude"]["status"], "healthy")

    def test_probe_retires_a_failure_dated_in_the_future(self):
        for damaged in ({"status": "error", "reason": "e", "updated_at": FAR_FUTURE, "source": "dispatch"},
                        {"status": "limited", "reason": "q", "resets_at": "in 3 hours", "updated_at": FAR_FUTURE}):
            with self.subTest(status=damaged["status"]):
                self.write({"claude": damaged})
                probed = {"status": "healthy", "reason": "Claude Code 订阅运行正常", "resets_at": None,
                          "updated_at": datetime.now().isoformat(), "mode": "subscription"}
                with patch("makewand.config.get_all_supported_providers", return_value=["claude"]), \
                     patch.object(health, "probe_model", return_value=dict(probed)):
                    health.get_or_update_status(force_probe=True)
                self.assertEqual(self.on_disk()["claude"]["status"], "healthy")
                self.assertEqual(health.load_status_cache()["claude"]["status"], "healthy")

    def test_probe_waiter_does_not_reuse_an_entry_dated_in_the_future(self):
        started = datetime.now()
        self.assertFalse(health._probed_since({"status": "healthy", "updated_at": FAR_FUTURE}, started))
        self.assertTrue(health._probed_since({"status": "healthy", "updated_at": datetime.now().isoformat()}, started))

    def test_damaged_ttl_cannot_exclude_a_provider_forever(self):
        for ttl in (float("inf"), 1e300, 10 ** 400):
            with self.subTest(ttl=ttl):
                entry = {"status": "error", "reason": "r", "updated_at": ago(hours=25), "ttl_seconds": ttl}
                self.assertTrue(health.failure_verdict_expired(entry))
        # Python's json reads the "Infinity" that json.dump writes for float("inf").
        health.STATUS_CACHE_FILE.write_text(
            '{"agy": {"status": "error", "reason": "r", "updated_at": "%s", "ttl_seconds": Infinity}}' % ago(hours=25),
            encoding="utf-8")
        self.assertEqual(health.load_status_cache()["agy"]["status"], "unknown")
        self.assertFalse(health.failure_verdict_expired({"status": "error", "updated_at": ago(hours=2),
                                                         "ttl_seconds": 6 * 3600}))


class TestStatusRecommendsOnlyUsableEngines(unittest.TestCase):
    def topology(self, cache, pool):
        out = io.StringIO()
        args = argparse.Namespace(json=False, probe=False, repo_trust="trusted", cwd=None)
        with routing(pool=pool), patch("makewand.cli.get_or_update_status", return_value=cache), \
             contextlib.redirect_stdout(out):
            cli.cmd_status(args)
        text = out.getvalue()
        return text.split("Dynamic Topology")[1].split("AI 工具开关")[0]

    def test_nothing_is_recommended_when_every_engine_is_in_error(self):
        cache = {e: live("error", reason=f"{e} broken", source="dispatch") for e in ("claude", "codex", "agy")}
        section = self.topology(cache, ("claude", "codex", "agy"))
        self.assertNotIn("推荐通用分工", section, "it used to recommend claude -> claude")
        self.assertIn("都不可用", section)
        for engine in ("claude", "codex", "agy"):
            self.assertIn(f"{engine} broken", section)
        self.assertIn("makewand probe", section)

    def test_single_usable_engine_is_not_called_a_cross_model_pair(self):
        cache = {"claude": live("healthy"), "codex": live("error"), "agy": live("needs_auth")}
        section = self.topology(cache, ("claude", "codex", "agy"))
        self.assertNotIn("独立跨模型红队盲审", section)
        self.assertIn("当前唯一可用工具", section)
        self.assertIn("claude", section)


class TestAdaptersRelayWhatSelectionCountsAsRelayed(unittest.TestCase):
    ADAPTERS = {"claude": execute_claude_task, "codex": execute_codex_task}

    def dispatch(self, engine, status, key=True, policy="allow_paid"):
        calls = []

        def api(**kwargs):
            calls.append(kwargs.get("provider"))
            return True, "api answer", None

        with patch("makewand.health.load_status_cache", return_value={engine: live(status)}), \
             patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=key), \
             patch.dict(os.environ, {"MAKEWAND_API_POLICY": policy}), \
             patch("makewand.providers.api_client.call_api_chat", side_effect=api), \
             patch("makewand.sandbox.provider_sandbox_launch", side_effect=SandboxUnavailableError("cli path")), \
             contextlib.redirect_stderr(io.StringIO()):
            result = self.ADAPTERS[engine]("审查这段代码", readonly=True, cwd=tempfile.gettempdir())
        return calls, tuple(result)

    def test_needs_auth_with_an_api_key_is_relayed_instead_of_running_the_cli(self):
        for engine in self.ADAPTERS:
            with self.subTest(engine=engine):
                calls, result = self.dispatch(engine, "needs_auth")
                self.assertEqual(calls, [engine])
                self.assertEqual(result[:2], (True, "api answer"))

    def test_needs_auth_with_a_key_the_api_policy_forbids_still_runs_the_cli(self):
        for engine in self.ADAPTERS:
            with self.subTest(engine=engine):
                calls, result = self.dispatch(engine, "needs_auth", policy="subscription_only")
                self.assertEqual(calls, [])
                self.assertEqual(result[2], "cli path")

    def test_needs_auth_without_a_key_still_runs_the_cli(self):
        for engine in self.ADAPTERS:
            with self.subTest(engine=engine):
                calls, result = self.dispatch(engine, "needs_auth", key=False)
                self.assertEqual(calls, [])
                self.assertEqual(result[2], "cli path")

    def test_selection_and_adapters_agree_on_what_is_relayed(self):
        for engine in self.ADAPTERS:
            for status in ("limited", "needs_auth", "error"):
                for policy in ("allow_paid", "subscription_only"):
                    with self.subTest(engine=engine, status=status, policy=policy):
                        with routing(pool=(engine,), api=(engine,)), \
                             patch.dict(os.environ, {"MAKEWAND_API_POLICY": policy}):
                            relayable = _usable_or_api_relay(engine, {engine: live(status)})
                        calls, _ = self.dispatch(engine, status, policy=policy)
                        self.assertEqual(bool(calls), relayable)


if __name__ == "__main__":
    unittest.main()
