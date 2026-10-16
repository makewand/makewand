"""完整路由目录：全部模型 × 推理深度、legacy、候选级账本、可复现选择。

不调用真实 AI CLI。discovery 的读取用临时 HOME 里的夹具。
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import random
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand.outcome_ledger import OutcomeLedger
from makewand.route_select import Policy, RouteCandidate, select
from makewand.routing_catalog import (
    TierCandidate,
    build_full_catalog,
    candidates,
)

_SEED = 20261016


def _index(rows):
    return {item.candidate_id: item for item in rows}


def _all_rows(catalog):
    found = {}
    for tier in (1, 2, 3):
        for item in catalog[tier]:
            found[item.candidate_id] = item
    return found


class _ScriptedRng:
    def __init__(self):
        self.calls = []

    def betavariate(self, alpha, beta):
        self.calls.append((alpha, beta))
        # 分数只由调用顺序决定，和 alpha/beta 无关，方便单独断言采样参数。
        return 1.0 / (1 + len(self.calls))


class FullCatalogTests(unittest.TestCase):
    def test_builtin_catalog_classifies_the_named_combinations(self):
        catalog = build_full_catalog(None)
        found = _all_rows(catalog)
        expect = {
            "codex/gpt-6-luna/low": (1, False),
            "codex/gpt-6-luna/high": (1, False),
            "codex/gpt-6.1-sol/low": (1, False),
            "codex/gpt-6.1-sol/medium": (2, False),
            "codex/gpt-6.1-sol/high": (3, False),
            "codex/gpt-6.1-sol/xhigh": (3, False),
            "codex/gpt-6-astra/low": (1, False),
            "codex/gpt-6-astra/medium": (3, False),
            "codex/gpt-6-astra/high": (3, False),
            "codex/gpt-6-astra/xhigh": (3, False),
            "codex/gpt-6-astra/ultra": (3, False),
            "codex/gpt-5.6/low": (1, True),
            "codex/gpt-5.6/medium": (2, True),
            "claude/claude-haiku-5-5/medium": (1, False),
            "claude/claude-haiku-4-5-20251001/low": (1, True),
            "claude/claude-sonnet-5/medium": (2, False),
            "claude/claude-sonnet-5/high": (2, False),
            "claude/claude-opus-5-5/high": (3, False),
            "claude/claude-opus-5-5/max": (3, False),
            "claude/claude-fable-5-1/max": (3, False),
            "claude/claude-opus-4-6/high": (3, True),
            "agy/gemini-3.8-flash-low/low": (1, True),
            "agy/gemini-3.8-flash-medium/medium": (1, True),
            "agy/gemini-3.8-flash-high/high": (2, True),
            "agy/gemini-4-argon-low/low": (1, False),
            "agy/gemini-4-argon-medium/medium": (2, False),
            "agy/gemini-4-argon-high/high": (3, False),
            "agy/claude-opus-5-5-high/high": (3, False),
            "grok/grok-4.7/low": (1, False),
            "grok/grok-4.7/medium": (2, False),
            "grok/grok-4.7/high": (3, False),
            "grok/grok-4.7/xhigh": (3, False),
            "grok/grok-4.7-build-fast/low": (1, False),
            "grok/grok-4.7-build-fast/high": (1, False),
            "grok/grok-4.5/low": (1, True),
            "muse/muse-spark-1.3/low": (1, False),
            "muse/muse-spark-1.3/medium": (2, False),
            "muse/muse-spark-1.3/high": (2, False),
            "muse/muse-wonder/medium": (2, False),
            "muse/muse-wonder/high": (3, False),
            "muse/muse-spark-1.2/low": (1, True),
        }
        for candidate_id, (tier, legacy) in expect.items():
            item = found[candidate_id]
            self.assertEqual(item.tier_class, tier, candidate_id)
            self.assertEqual(item.legacy, legacy, candidate_id)
            self.assertEqual(item.candidate_id, f"{item.provider}/{item.model}/{item.effort}")
            self.assertIn(item, catalog[tier])
            self.assertTrue(any("\u4e00" <= char <= "\u9fff" for char in item.label), item.label)

    def test_agy_uses_suffixed_id_and_other_providers_pass_effort(self):
        found = _all_rows(build_full_catalog(None))
        agy = found["agy/claude-opus-5-5-high/high"]
        self.assertEqual(agy.args, ("--model", "claude-opus-5-5-high"))
        self.assertNotIn("--effort", agy.args)
        self.assertEqual(agy.model, "claude-opus-5-5-high")
        flash = found["agy/gemini-3.8-flash-medium/medium"]
        self.assertEqual(flash.args, ("--model", "gemini-3.8-flash-medium"))
        self.assertNotIn("--effort", flash.args)
        sol = found["codex/gpt-6.1-sol/medium"]
        self.assertEqual(sol.args, ("--model", "gpt-6.1-sol", "--effort", "medium"))
        opus = found["claude/claude-opus-5-5/max"]
        self.assertEqual(opus.args, ("--model", "claude-opus-5-5", "--effort", "max"))
        for item in found.values():
            if item.provider == "agy":
                self.assertEqual(item.args, ("--model", item.model))
                self.assertTrue(item.model.endswith("-" + item.effort), item.candidate_id)
            else:
                self.assertEqual(item.args, ("--model", item.model, "--effort", item.effort))

    def test_legacy_is_in_the_catalog_but_out_of_the_default_tier_list(self):
        catalog = build_full_catalog(None)
        self.assertTrue(any(item.legacy for rows in catalog.values() for item in rows))
        for tier in (1, 2, 3):
            ranked = candidates(tier, full=True)
            self.assertTrue(ranked)
            self.assertTrue(all(not item.legacy for item in ranked))
            self.assertGreater(len(ranked), len(candidates(tier)))
            included = candidates(tier, full=True, include_legacy=True)
            self.assertEqual([item for item in included if not item.legacy], list(ranked))
            seen_legacy = False
            for item in included:
                if item.legacy:
                    seen_legacy = True
                elif seen_legacy:
                    self.fail(f"tier {tier}: current model {item.candidate_id} sorted after a legacy row")
            self.assertTrue(seen_legacy)

        ranked_ids = {item.candidate_id for tier in (1, 2, 3) for item in candidates(tier, full=True)}
        for legacy_id in (
            "codex/gpt-5.6/low",
            "claude/claude-haiku-4-5-20251001/low",
            "claude/claude-opus-4-6/high",
            "agy/gemini-3.8-flash-medium/medium",
            "agy/gemini-3.1-pro-high/high",
            "grok/grok-4.6/low",
            "grok/grok-4.5/low",
            "muse/muse-spark-1.2/low",
        ):
            self.assertNotIn(legacy_id, ranked_ids)
        included_ids = {
            item.candidate_id
            for tier in (1, 2, 3)
            for item in candidates(tier, full=True, include_legacy=True)
        }
        for legacy_id in (
            "codex/gpt-5.6/low",
            "agy/gemini-3.8-flash-low/low",
            "claude/claude-sonnet-4-6/medium",
        ):
            self.assertIn(legacy_id, included_ids)

    def test_cheaper_subscription_use_sorts_first_inside_a_tier(self):
        tier1 = candidates(1, full=True)
        ids = [item.candidate_id for item in tier1]
        self.assertEqual(ids[:6], [
            "agy/gemini-4-argon-low/low",
            "grok/grok-4.7/low",
            "grok/grok-4.7-build-fast/low",
            "grok/grok-4.7-build-fast/medium",
            "grok/grok-4.7-build-fast/high",
            "grok/grok-4.7-build-fast/xhigh",
        ])
        self.assertLess(ids.index("grok/grok-4.7/low"), ids.index("grok/grok-4.7-build-fast/low"))
        native = [item for item in tier1 if item.provider != "agy" or item.model.startswith("gemini")]
        order = []
        for item in native:
            if not order or order[-1] != item.provider:
                order.append(item.provider)
        self.assertEqual(order, ["agy", "grok", "muse", "codex", "claude"])

        def family_rank(model):
            if "luna" in model or "haiku" in model:
                return 0
            if "sol" in model or "sonnet" in model:
                return 1
            if "astra" in model or "opus" in model:
                return 2
            if "fable" in model:
                return 3
            return 1

        for provider in ("codex", "claude"):
            ranks = [family_rank(item.model) for item in tier1 if item.provider == provider]
            self.assertEqual(ranks, sorted(ranks), provider)
        # 同一模型里，浅推理在前。Claude 周额度阈值更紧，整段排在 codex 之后。
        luna = [item.effort for item in tier1 if item.model == "gpt-6-luna"]
        self.assertEqual(luna, ["low", "medium", "high"])
        codex_at = ids.index("codex/gpt-6-luna/low")
        claude_at = ids.index("claude/claude-haiku-5-5/low")
        self.assertLess(codex_at, claude_at)
        # 代理的 opus 比原生 Gemini 贵，排在原生订阅之后。
        self.assertLess(
            ids.index("claude/claude-haiku-5-5/low"),
            ids.index("agy/claude-opus-5-5-low/low"),
        )

    def test_slim_candidates_stay_the_short_table(self):
        slim = candidates(1)
        self.assertEqual([item.provider for item in slim], ["agy", "codex", "grok", "claude"])
        self.assertEqual(slim[0].args, ("--model", "gemini-3.8-flash-medium"))
        self.assertIsNone(slim[0].model)
        self.assertIsNone(slim[0].candidate_id)
        self.assertFalse(slim[0].legacy)
        self.assertNotEqual(
            [item.args for item in slim],
            [item.args for item in candidates(1, full=True)],
        )

    def test_discovered_provider_replaces_only_that_provider(self):
        discovered = {
            "codex": {"gpt-6-luna": ["high", "low"]},
            "agy": {"gemini-4-flash": ["medium", "low", "high"], "gemini-3.8-flash-low": []},
            "grok": {"available": ["grok-4.7 (Grok 4.7)"], "source": "detected"},
        }
        catalog = build_full_catalog(discovered)
        codex_ids = [item.candidate_id for rows in catalog.values() for item in rows if item.provider == "codex"]
        self.assertEqual(codex_ids, ["codex/gpt-6-luna/low", "codex/gpt-6-luna/high"])
        agy = _index(item for rows in catalog.values() for item in rows if item.provider == "agy")
        self.assertEqual(agy["agy/gemini-4-flash-low/low"].tier_class, 1)
        self.assertFalse(agy["agy/gemini-4-flash-low/low"].legacy)
        self.assertEqual(agy["agy/gemini-4-flash-medium/medium"].tier_class, 1)
        self.assertEqual(agy["agy/gemini-4-flash-high/high"].tier_class, 2)
        self.assertEqual(agy["agy/gemini-4-flash-high/high"].args, ("--model", "gemini-4-flash-high"))
        self.assertTrue(agy["agy/gemini-3.8-flash-low/low"].legacy)
        self.assertNotIn("agy/gemini-4-argon-high/high", agy)
        grok_ids = [item.candidate_id for rows in catalog.values() for item in rows if item.provider == "grok"]
        self.assertIn("grok/grok-4.7/low", grok_ids)
        self.assertNotIn("grok/grok-4.5/low", grok_ids)
        # muse 没有出现在 discovered 里，继续用内置默认。
        muse_ids = [item.candidate_id for rows in catalog.values() for item in rows if item.provider == "muse"]
        self.assertIn("muse/muse-wonder/high", muse_ids)
        with self.assertRaises(ValueError):
            build_full_catalog(["not-a-mapping"])

    def test_discover_model_efforts_reads_fixtures_and_feeds_the_catalog(self):
        home = Path(tempfile.mkdtemp(prefix="mw-e2-discover-"))
        self.addCleanup(shutil.rmtree, home, True)
        codex = home / ".codex" / "models_cache.json"
        codex.parent.mkdir(parents=True)
        codex.write_text(json.dumps({"models": [
            {"slug": "gpt-6-luna", "visibility": "list",
             "supported_reasoning_levels": [{"effort": "low"}, {"effort": "high"}, {"effort": "nope"}]},
            {"slug": "gpt-reserve", "visibility": "hide",
             "supported_reasoning_levels": [{"effort": "low"}]},
        ]}), encoding="utf-8")
        claude = home / ".claude" / "cache" / "model-catalog" / "catalog.json"
        claude.parent.mkdir(parents=True)
        claude.write_text(json.dumps({"catalog": {"config": {"models": [
            {"id": "claude-haiku-5-5", "thinking": {"effort_options": [{"id": "low"}, {"id": "medium"}]}},
        ]}}}), encoding="utf-8")
        grok = home / ".grok" / "models_cache.json"
        grok.parent.mkdir(parents=True)
        grok.write_text(json.dumps({"models": {"grok-4.7": {"info": {"reasoning_efforts": [
            {"id": "low"}, {"id": "xhigh"},
        ]}}}}), encoding="utf-8")
        agy = home / ".cache" / "makewand" / "agy_models_cache.json"
        agy.parent.mkdir(parents=True)
        agy.write_text(json.dumps({"updated_at": 0, "models": [
            {"id": "gemini-4-argon-high", "description": "Argon"},
        ]}), encoding="utf-8")
        env = {key: value for key, value in os.environ.items() if key not in ("CODEX_HOME", "CLAUDE_CONFIG_DIR")}
        env["HOME"] = str(home)
        env["CLAUDE_CONFIG_DIR"] = str(home / ".claude")
        with patch.dict(os.environ, env, clear=True), \
                patch("pathlib.Path.home", return_value=home), \
                patch("subprocess.run") as run:
            from makewand.discovery import discover_model_efforts
            found = discover_model_efforts()
        run.assert_not_called()
        self.assertEqual(found["codex"], {"gpt-6-luna": ["low", "high"]})
        self.assertNotIn("gpt-reserve", found["codex"])
        self.assertEqual(found["claude"]["claude-haiku-5-5"], ["low", "medium"])
        self.assertEqual(found["grok"]["grok-4.7"], ["low", "xhigh"])
        self.assertEqual(found["agy"]["gemini-4-argon-high"], ["high"])
        ranked = candidates(3, full=True, discovered=found)
        ids = [item.candidate_id for item in ranked]
        self.assertIn("agy/gemini-4-argon-high/high", ids)
        self.assertNotIn("codex/gpt-6-astra/high", ids)
        self.assertTrue(all(item.provider != "codex" or item.model == "gpt-6-luna" for item in candidates(1, full=True, discovered=found)))


class LedgerCandidateTests(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-e2-ledger-")
        self.addCleanup(self._tmp.cleanup)
        self.ledger = OutcomeLedger(Path(self._tmp.name) / "routing")
        self.now = 1_800_000_000.0

    def test_candidate_stats_do_not_mix_with_legacy_rows(self):
        # 旧记录：没有 model / effort，字段顺序保持原样。
        self.ledger.record("codex", 1, "implement", True, seconds=4, calls=2, ts=self.now)
        old = json.loads(self.ledger.outcomes_path.read_text(encoding="utf-8").splitlines()[0])
        self.assertEqual(list(old), ["ts", "provider", "tier", "task_type", "passed", "seconds", "calls"])

        self.ledger.record(
            "codex", 1, "implement", True, seconds=10, calls=1, ts=self.now,
            model="gpt-6-luna", effort="High",
        )
        # 另一档的同一候选也要算进候选级统计，不能被 tier 拆开。
        self.ledger.record(
            "codex", 9, "implement", False, seconds=30, calls=3, ts=self.now,
            model="gpt-6-luna", effort="high",
        )
        self.ledger.record(
            "codex", 1, "implement", True, seconds=100, calls=9, ts=self.now,
            model="gpt-6.1-sol", effort="medium",
        )
        self.ledger.record(
            "codex", 1, "review", False, seconds=1, calls=1, ts=self.now,
            model="gpt-6-luna", effort="high",
        )

        lines = self.ledger.outcomes_path.read_text(encoding="utf-8").splitlines()
        new = json.loads(lines[1])
        self.assertEqual(
            list(new)[:7],
            ["ts", "provider", "tier", "task_type", "passed", "seconds", "calls"],
        )
        self.assertEqual(new["model"], "gpt-6-luna")
        self.assertEqual(new["effort"], "high")

        tier = self.ledger.stats("codex", 1, "implement", self.now)
        # 旧记录 + luna low（记在第 1 档）+ sol。第 9 档的 luna 不进旧统计。
        self.assertEqual(tier.count, 3)
        self.assertEqual(tier.alpha, 1 + 3)
        self.assertEqual(tier.beta, 1)

        luna = self.ledger.stats("codex", 1, "implement", self.now, model="gpt-6-luna", effort="HIGH")
        self.assertEqual(luna.count, 2)
        self.assertEqual((luna.alpha, luna.beta), (2, 2))
        self.assertEqual(luna.avg_seconds, 20)
        self.assertEqual(luna.avg_calls, 2)
        # 候选级查询不吃没有 model 的旧记录，也不吃别的模型。
        sol = self.ledger.stats("codex", 3, "implement", self.now, model="gpt-6.1-sol", effort="medium")
        self.assertEqual(sol.count, 1)
        self.assertEqual(sol.alpha, 2)
        other = self.ledger.stats("codex", 1, "implement", self.now, model="gpt-6-astra", effort="high")
        self.assertEqual(other.count, 0)
        self.assertEqual((other.alpha, other.beta), (1, 1))

        with self.assertRaises(ValueError):
            self.ledger.record("codex", 1, "implement", True, model="  ")
        with self.assertRaises(TypeError):
            self.ledger.stats("codex", 1, "implement", self.now, now_ts=0)


class FullSelectTests(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-e2-select-")
        self.addCleanup(self._tmp.cleanup)
        self.ledger = OutcomeLedger(Path(self._tmp.name) / "routing")
        self.now = 1_800_000_000.0
        self.discovered = {
            "codex": {"gpt-6-luna": ["low", "high"], "gpt-6.1-sol": ["medium"], "gpt-5.6": ["low"]},
            "claude": {"claude-haiku-5-5": ["low"], "claude-opus-5-5": ["high"]},
            "agy": {"gemini-4-argon": ["low"], "claude-opus-5-5": ["high"], "gemini-3.8-flash": ["low"]},
            "grok": {"grok-4.7": ["low"]},
            "muse": {"muse-spark-1.3": ["low"], "muse-wonder": ["high"]},
        }

    def _select(self, **kwargs):
        kwargs.setdefault("task_type", "implement")
        kwargs.setdefault("difficulty", "mechanical")
        kwargs.setdefault("risk", "low")
        kwargs.setdefault("ledger", self.ledger)
        kwargs.setdefault("now", self.now)
        kwargs.setdefault("quota_reader", lambda: {})
        kwargs.setdefault("policy", Policy())
        kwargs.setdefault("discovered", self.discovered)
        kwargs.setdefault("full", True)
        return select(**kwargs)

    def test_full_select_order_is_reproducible_and_uses_candidate_stats(self):
        with patch("makewand.discovery.discover_model_efforts", side_effect=AssertionError("should be injected")):
            tied = self._select(rng=random.Random(0))
        # 注入了 discovered，不应再读 discovery。Constant 分数下保持目录顺序。
        from makewand.route_select import RouteCandidate as Candidate

        class Constant:
            def betavariate(self, alpha, beta):
                return 0.5

        catalog = candidates(1, full=True, discovered=self.discovered)
        tied = self._select(rng=Constant())
        self.assertEqual([item.candidate_id for item in tied.candidates], [item.candidate_id for item in catalog])
        for got, item in zip(tied.candidates, catalog):
            self.assertIsInstance(got, Candidate)
            self.assertEqual(got.provider, item.provider)
            self.assertEqual(got.args, item.args)
            self.assertEqual(got.label, item.label)
            self.assertEqual(got.model, item.model)
            self.assertEqual(got.effort, item.effort)
            self.assertEqual(got.candidate_id, item.candidate_id)
            self.assertFalse(got.is_self)
            self.assertEqual(got.sampled_score, 0.5)
        self.assertNotIn("--effort", tied.candidates[0].args)
        self.assertTrue(tied.candidates[0].model.endswith("-low"))

        # legacy 默认不进排序。显式要求才出现，并且排在当前模型之后。
        ids = [item.candidate_id for item in tied.candidates]
        self.assertNotIn("codex/gpt-5.6/low", ids)
        self.assertNotIn("agy/gemini-3.8-flash-low/low", ids)
        with_legacy = self._select(rng=Constant(), include_legacy=True)
        legacy_ids = [item.candidate_id for item in with_legacy.candidates]
        self.assertIn("codex/gpt-5.6/low", legacy_ids)
        self.assertIn("agy/gemini-3.8-flash-low/low", legacy_ids)
        self.assertLess(legacy_ids.index("agy/gemini-4-argon-low/low"), legacy_ids.index("codex/gpt-5.6/low"))

        for _ in range(30):
            self.ledger.record(
                "codex", 1, "implement", False, seconds=1, calls=1, ts=self.now,
                model="gpt-6-luna", effort="low",
            )
            self.ledger.record(
                "codex", 1, "implement", True, seconds=1, calls=1, ts=self.now,
                model="gpt-6-luna", effort="high",
            )
        # 旧式统计会把这两条合成一家 codex；候选级必须分开采样。
        merged = self.ledger.stats("codex", 1, "implement", self.now)
        self.assertEqual((merged.alpha, merged.beta), (31, 31))
        loser = self.ledger.stats("codex", 1, "implement", self.now, model="gpt-6-luna", effort="low")
        winner = self.ledger.stats("codex", 1, "implement", self.now, model="gpt-6-luna", effort="high")
        self.assertEqual((loser.alpha, loser.beta), (1, 31))
        self.assertEqual((winner.alpha, winner.beta), (31, 1))

        script = _ScriptedRng()
        self._select(rng=script)
        by_id = {item.candidate_id: item for item in catalog}
        self.assertEqual(len(script.calls), len(catalog))
        for item, (alpha, beta) in zip(catalog, script.calls):
            stats = self.ledger.stats(
                item.provider, 1, "implement", self.now, model=item.model, effort=item.effort,
            )
            self.assertEqual((alpha, beta), (stats.alpha, stats.beta), item.candidate_id)
            self.assertIn(item.candidate_id, by_id)

        first = self._select(rng=random.Random(_SEED))
        second = self._select(rng=random.Random(_SEED))
        self.assertEqual(first, second)
        replay = random.Random(_SEED)
        expected_score = {}
        for item in catalog:
            stats = self.ledger.stats(
                item.provider, 1, "implement", self.now, model=item.model, effort=item.effort,
            )
            expected_score[item.candidate_id] = replay.betavariate(float(stats.alpha), float(stats.beta))
        order = [item.candidate_id for item in catalog]
        expected_order = sorted(order, key=lambda cid: (-expected_score[cid], order.index(cid)))
        self.assertEqual([item.candidate_id for item in first.candidates], expected_order)
        self.assertLess(
            [item.candidate_id for item in first.candidates].index("codex/gpt-6-luna/high"),
            [item.candidate_id for item in first.candidates].index("codex/gpt-6-luna/low"),
        )
        self.assertIsInstance(first.candidates[0], RouteCandidate)

    def test_slim_select_leaves_the_new_fields_empty_and_skips_discovery(self):
        class Constant:
            def betavariate(self, alpha, beta):
                return 0.5

        with patch("makewand.discovery.discover_model_efforts", side_effect=AssertionError("slim path")):
            result = select(
                "implement", "mechanical",
                ledger=self.ledger, now=self.now, quota_reader=lambda: {}, policy=Policy(),
                rng=Constant(),
            )
        # 采样值相同就保持精简表的顺序；新字段保持空。
        self.assertEqual([item.provider for item in result.candidates], [item.provider for item in candidates(1)])
        self.assertEqual([item.args for item in result.candidates], [item.args for item in candidates(1)])
        for item in result.candidates:
            self.assertIsNone(item.model)
            self.assertIsNone(item.effort)
            self.assertIsNone(item.candidate_id)
            self.assertIsInstance(item, RouteCandidate)

    def test_full_select_reads_discovery_when_no_fixture_is_injected(self):
        with patch("makewand.discovery.discover_model_efforts", return_value={}) as probed:
            result = select(
                "implement", "mechanical", full=True,
                ledger=self.ledger, now=self.now, quota_reader=lambda: {}, policy=Policy(),
                rng=_ScriptedRng(),
            )
        probed.assert_called_once()
        self.assertEqual(
            [item.candidate_id for item in result.candidates],
            [item.candidate_id for item in candidates(1, full=True)],
        )

    def test_pause_skips_every_combo_of_that_provider_without_sampling_it(self):
        from makewand.route_select import Pause
        from datetime import datetime
        text = "2026-10-20T12:00"
        until = datetime.strptime(text, "%Y-%m-%dT%H:%M").timestamp()
        policy = Policy(paused=(Pause("codex", until, text),))
        script = _ScriptedRng()
        result = self._select(policy=policy, now=until - 1, rng=script)
        self.assertTrue(all(item.provider != "codex" for item in result.candidates))
        self.assertIn("暂停到", dict(result.skipped)["codex"])
        catalog = [item for item in candidates(1, full=True, discovered=self.discovered) if item.provider != "codex"]
        self.assertEqual(len(script.calls), len(catalog))
        self.assertEqual([item.candidate_id for item in result.candidates], [item.candidate_id for item in catalog])


if __name__ == "__main__":
    unittest.main()
