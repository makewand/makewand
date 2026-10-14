"""
py-rel-19 (round 2): selection never names an engine that is not installed or
usable, and every consumer reads failure verdicts the same way.

With every active provider in a live failure state, select_optimal_engine_pair
fell back to a hard-coded ["agy"] in single-tool mode even when agy was not
installed; an error verdict with an API key slipped back in through the
fallbacks although the adapters only relay limited / needs_auth / missing work
to the API; a failure entry with a missing or unparseable timestamp stayed
excluded in _engine_usable while load_status_cache decayed it; and the ensemble
let error and needs_auth engines through.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import tempfile
import unittest
from datetime import datetime, timedelta
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
from makewand.engine_selection import (
    _engine_usable,
    _no_provider_detected,
    _print_no_provider_guidance,
    forced_engine_refusal,
    select_optimal_engine_pair,
)
from makewand.ensemble import resolve_ensemble_providers

REGION = "Antigravity 在当前地区/账号不可用 (真实派发返回地区限制)"


def ago(**kwargs):
    return (datetime.now() - timedelta(**kwargs)).isoformat()


def live(status, **extra):
    return dict({"status": status, "reason": f"{status} reason", "updated_at": ago(minutes=1)}, **extra)


class TestNoUsableEngine(unittest.TestCase):
    def route(self, cache, pool=("claude", "codex"), api=(), policy="subscription_only"):
        self.cache = cache
        with patch("makewand.config.get_active_providers", return_value=list(pool)), \
             patch("makewand.config.is_provider_enabled", return_value=True), \
             patch("makewand.config.has_api_configured", side_effect=lambda e: e in api), \
             patch.dict(os.environ, {"MAKEWAND_API_POLICY": policy}), \
             patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
             patch("makewand.usage.get_engine_usage_stats", return_value={}), \
             patch("makewand.usage.get_all_engine_reliability", return_value={}):
            return select_optimal_engine_pair("编写Python脚本", tier="standard", cache=cache)

    def assert_no_engine(self, result, *engines):
        coders, reviewers, meta = result
        self.assertEqual((coders, reviewers), ([], []))
        self.assertTrue(meta["no_usable_engines"])
        self.assertIsNone(meta["primary_coder"])
        self.assertFalse(meta["single_tool_mode"])
        self.assertNotIn("agy", json.dumps(meta["unusable_engines"]))
        self.assertEqual(sorted(meta["unusable_engines"]), sorted(engines))
        text = " ".join(meta["reasons"])
        for engine in engines:
            self.assertIn(engine, text)
        self.assertIn("makewand probe", text)
        self.assertTrue(_no_provider_detected(None, meta))
        # A forced engine is decided before routing (forced-engine): an unusable one is
        # refused with its own reason there; one that got this far runs.
        self.assertFalse(_no_provider_detected("claude", meta))
        self.assertIn(self.cache["claude"]["status"], forced_engine_refusal("claude", self.cache))

    def test_all_active_providers_in_live_error_never_fall_back_to_uninstalled_agy(self):
        self.assert_no_engine(self.route({"claude": live("error"), "codex": live("error"),
                                          "agy": {"status": "unknown"}}), "claude", "codex")

    def test_all_logged_out(self):
        self.assert_no_engine(self.route({"claude": live("needs_auth"), "codex": live("needs_auth")}),
                              "claude", "codex")

    def test_error_with_api_key_is_not_relayed(self):
        # The adapters only relay limited / needs_auth / missing work to the API key.
        self.assert_no_engine(self.route({"claude": live("error"), "codex": live("error")}, api=("claude",)),
                              "claude", "codex")

    def test_relayed_states_with_api_key_stay_selectable(self):
        # The adapters relay only with a key AND an API policy that allows paid calls.
        for status in ("needs_auth", "limited"):
            with self.subTest(status=status):
                coders, reviewers, meta = self.route({"claude": live(status), "codex": live("error")},
                                                     api=("claude",), policy="allow_paid")
                self.assertEqual(coders, ["claude"])
                self.assertNotIn("codex", reviewers)
                self.assertFalse(meta.get("no_usable_engines", False))

    def test_relayed_states_with_a_key_the_policy_forbids_are_not_usable(self):
        for status in ("needs_auth", "limited"):
            with self.subTest(status=status):
                self.assert_no_engine(self.route({"claude": live(status), "codex": live("error")}, api=("claude",)),
                                      "claude", "codex")

    def test_single_usable_engine_reviews_itself(self):
        coders, reviewers, meta = self.route({"claude": live("healthy"), "codex": live("error")})
        self.assertEqual((coders, reviewers, meta["single_tool_mode"]), (["claude"], ["claude"], True))
        self.assertFalse(_no_provider_detected(None, meta))

    def test_caller_override_still_wins(self):
        # local_only replaces the pair after routing; the pipeline must not stop then.
        _, _, meta = self.route({"claude": live("error"), "codex": live("error")})
        meta.update(primary_coder="local", primary_reviewer="local", single_tool_mode=True)
        with patch("makewand.config.get_active_providers", return_value=["local"]):
            self.assertFalse(_no_provider_detected(None, meta))

    def test_guidance_names_the_unusable_engines(self):
        _, _, meta = self.route({"claude": live("error"), "codex": live("needs_auth")})
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            _print_no_provider_guidance(meta)
        self.assertIn("claude", out.getvalue())
        self.assertIn("codex", out.getvalue())
        self.assertNotIn("(0 个)", out.getvalue())
        out = io.StringIO()
        with patch("makewand.config.get_active_providers", return_value=[]), contextlib.redirect_stdout(out):
            _print_no_provider_guidance()
        self.assertIn("(0 个)", out.getvalue())


class TestTimestampRuleIsShared(unittest.TestCase):
    """load_status_cache and selection read a failure verdict's age the same way."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-fix-usable-r2-")
        root = Path(self._tmp.name)
        for p in (patch.object(config, "CONFIG_DIR", root / "cfg"),
                  patch.object(health, "STATUS_CACHE_FILE", root / "cfg" / "status.json"),
                  patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json"),
                  patch("makewand.config.is_provider_enabled", return_value=True)):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        (root / "cfg").mkdir(parents=True)

    def entry(self, status, stamp):
        entry = {"status": status, "reason": REGION, "source": "dispatch", "ttl_seconds": 6 * 3600}
        if stamp is not None:
            entry["updated_at"] = stamp
        return entry

    def both(self, entry):
        health.STATUS_CACHE_FILE.write_text(json.dumps({"agy": entry}), encoding="utf-8")
        return health.load_status_cache()["agy"]["status"], _engine_usable("agy", {"agy": dict(entry)})[0]

    def test_unparseable_timestamp_is_neutral_in_both(self):
        for status in ("error", "needs_auth"):
            with self.subTest(status=status):
                self.assertEqual(self.both(self.entry(status, "garbage")), ("unknown", True))

    def test_verdict_without_timestamp_is_neutral_in_both(self):
        # Round 3: like master, a verdict without a timestamp decays at once (it
        # used to be stamped on every load and so stayed excluded until a save).
        for stamp in (None, ""):
            for status in ("error", "needs_auth"):
                with self.subTest(stamp=stamp, status=status):
                    self.assertEqual(self.both(self.entry(status, stamp)), ("unknown", True))

    def test_verdict_without_timestamp_is_saved_back_as_neutral(self):
        health.STATUS_CACHE_FILE.write_text(json.dumps({"agy": self.entry("error", None)}), encoding="utf-8")
        health.record_engine_limit("codex", "limit", "in 3 hours")  # any save
        saved = json.loads(health.STATUS_CACHE_FILE.read_text(encoding="utf-8"))["agy"]
        self.assertEqual((saved["status"], saved["expired_from"]), ("unknown", "error"))
        self.assertTrue(saved.get("updated_at"))
        self.assertEqual(self.both(saved), ("unknown", True))

    def test_fresh_timestamp_excludes_in_both(self):
        self.assertEqual(self.both(live("error", ttl_seconds=6 * 3600, source="dispatch")), ("error", False))


class TestEnsembleUsesEngineUsability(unittest.TestCase):
    def resolve(self, cache, api=(), requested=("claude", "codex", "grok", "muse"), policy="subscription_only"):
        with patch("makewand.ensemble.is_provider_enabled", return_value=True), \
             patch("makewand.config.is_provider_enabled", return_value=True), \
             patch("makewand.ensemble.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", side_effect=lambda e: e in api), \
             patch.dict(os.environ, {"MAKEWAND_API_POLICY": policy}), \
             patch("makewand.health.load_status_cache", return_value=cache):
            return resolve_ensemble_providers(list(requested))

    def test_error_and_needs_auth_are_excluded(self):
        cache = {"claude": live("error"), "codex": live("needs_auth"), "grok": live("limited"),
                 "muse": live("healthy")}
        self.assertEqual(self.resolve(cache), ["muse"])

    def test_api_key_relays_needs_auth_and_limited_but_not_error(self):
        cache = {"claude": live("error"), "codex": live("needs_auth"), "grok": live("limited"),
                 "muse": live("healthy")}
        self.assertEqual(self.resolve(cache, api=("claude", "codex", "grok"), policy="allow_paid"),
                         ["codex", "grok", "muse"])
        # Under the default subscription_only policy a key is no relay (api-policy-relay).
        self.assertEqual(self.resolve(cache, api=("claude", "codex", "grok")), ["muse"])

    def test_expired_failure_is_neutral(self):
        cache = {"claude": {"status": "error", "reason": "probe timeout", "updated_at": ago(minutes=31)}}
        self.assertEqual(self.resolve(cache, requested=("claude",)), ["claude"])

    def test_fallback_skips_a_region_blocked_agy(self):
        cache = {"claude": live("error"), "agy": live("error", ttl_seconds=6 * 3600)}
        # Round 3: the local fallback is taken only when a local model answers (the
        # isolated test endpoint does not), so nothing is left to dispatch.
        self.assertEqual(self.resolve(cache, requested=("claude",)), [])
        with patch("makewand.providers.local.is_local_model_available", return_value=(True, "", ["qwen2.5-coder"])):
            self.assertEqual(self.resolve(cache, requested=("claude",)), ["local"])
        self.assertEqual(self.resolve({"claude": live("error")}, requested=("claude",)), ["agy"])


if __name__ == "__main__":
    unittest.main()
