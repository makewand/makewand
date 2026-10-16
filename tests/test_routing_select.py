"""选择器：暂停、额度标记、周额度、同家族、Thompson 采样和策略文件。

全部使用临时目录和注入的 quota_reader。不读真实账本或配置，不执行外部命令。
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import random
import subprocess
import tempfile
import unittest
from datetime import datetime
from pathlib import Path
from unittest.mock import patch

from makewand.outcome_ledger import OutcomeLedger
from makewand.route_select import (
    COST_WEIGHT_SPREAD,
    QUOTA_TIMEOUT_SECONDS,
    Pause,
    Policy,
    RouteCandidate,
    RouteResult,
    default_policy_path,
    default_quota_reader,
    load_policy,
    parse_quota_text,
    quota_argv as _REAL_QUOTA_ARGV,
    select,
)
from makewand.routing_catalog import candidates, starting_tier

FAKE_QUOTA_ARGV = ("/opt/makewand/bin/makewand-server", "quota")
_BAR = "\u2500\u2500"
_QUOTA_SAMPLE = f"""{_BAR} agy
   subscription: signed in (no usage percentage available)
{_BAR} claude
   5h window   12% used
   weekly      84% used  (resets 10-20 15:04)
   top model   90% used (per-model weekly cap, informational)
   \u2192 healthy headroom
{_BAR} codex
   weekly      91% used  (resets 10-21 08:00)
{_BAR} grok
   no quota data (source unavailable)
"""
_SEED = 20261016


def _epoch(text: str) -> float:
    return datetime.strptime(text, "%Y-%m-%dT%H:%M").timestamp()


def _chinese(text: str) -> bool:
    return any("\u4e00" <= char <= "\u9fff" for char in text)


class ConstantRng:
    def __init__(self, score: float):
        self.score = score
        self.calls = 0

    def betavariate(self, alpha, beta):
        self.calls += 1
        return self.score


class ScriptedRng:
    def __init__(self, scores):
        self._scores = list(scores)
        self.calls = []

    def betavariate(self, alpha, beta):
        self.calls.append((alpha, beta))
        if not self._scores:
            raise AssertionError("betavariate called more times than scripted")
        return self._scores.pop(0)


class PolicyTests(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-routing-policy-")
        self.addCleanup(self._tmp.cleanup)
        self.base = Path(self._tmp.name)

    def test_quotes_comments_blanks_and_bad_pause_entries(self):
        path = self.base / "policy"
        path.write_text(
            "\n".join([
                "# 站主策略",
                "",
                'CODEX_MAX_WEEKLY="91" # 引号外面的注释',
                "# CODEX_MAX_WEEKLY=10",
                "CLAUDE_MAX_WEEKLY=70 # 无引号注释",
                "export PAUSED=\"codex:2026-01-01T00:00 Codex:2026-10-20T12:00 "
                "坏条目 grok:2026-13-01T00:00 muse:2026-11-01T00:00 "
                "grok:2026-10-20T12:00:30\"",
                "",
            ]),
            encoding="utf-8",
        )
        policy = load_policy(path)
        self.assertEqual(policy.codex_max_weekly, 91)
        self.assertEqual(policy.claude_max_weekly, 70)
        self.assertEqual(
            policy.paused,
            (
                Pause("codex", _epoch("2026-10-20T12:00"), "2026-10-20T12:00"),
                Pause("muse", _epoch("2026-11-01T00:00"), "2026-11-01T00:00"),
            ),
        )

        single = self.base / "single"
        single.write_text("CLAUDE_MAX_WEEKLY='66'\nCODEX_MAX_WEEKLY=\"abc\"\n", encoding="utf-8")
        quoted = load_policy(single)
        self.assertEqual(quoted.claude_max_weekly, 66)
        # 数字解析失败时保留默认阈值，不让整份策略失效。
        self.assertEqual(quoted.codex_max_weekly, 90)

    def test_missing_file_uses_defaults(self):
        missing = self.base / "no-such-policy"
        policy = load_policy(missing)
        self.assertEqual(policy.codex_max_weekly, 90)
        self.assertEqual(policy.claude_max_weekly, 85)
        self.assertEqual(policy.paused, ())

        home = Path.home()
        root = Path(_isolation.state().root)
        self.assertTrue(home.resolve().is_relative_to(root.resolve()))
        default = home / ".config" / "mw-hub" / "policy"
        backup = default.read_bytes() if default.exists() else None
        if default.exists():
            default.unlink()

        def restore():
            if backup is None:
                if default.exists():
                    default.unlink()
                return
            default.parent.mkdir(parents=True, exist_ok=True)
            default.write_bytes(backup)

        self.addCleanup(restore)
        with patch.dict(os.environ):
            os.environ.pop("MW_HUB_POLICY", None)
            self.assertEqual(default_policy_path(), default)
            loaded = load_policy()
        self.assertEqual(loaded.codex_max_weekly, 90)
        self.assertEqual(loaded.claude_max_weekly, 85)
        self.assertEqual(loaded.paused, ())

    def test_mw_hub_policy_overrides_the_default_path(self):
        home = Path.home()
        root = Path(_isolation.state().root)
        self.assertTrue(home.resolve().is_relative_to(root.resolve()))
        default = home / ".config" / "mw-hub" / "policy"
        default.parent.mkdir(parents=True, exist_ok=True)
        backup = default.read_bytes() if default.exists() else None
        default.write_text("CODEX_MAX_WEEKLY=11\nCLAUDE_MAX_WEEKLY=12\n", encoding="utf-8")

        def restore():
            if backup is None:
                default.unlink()
                return
            default.write_bytes(backup)

        self.addCleanup(restore)
        custom = self.base / "from-env"
        custom.write_text('CODEX_MAX_WEEKLY="22"\nCLAUDE_MAX_WEEKLY="23"\n', encoding="utf-8")
        missing = self.base / "missing-env-policy"
        with patch.dict(os.environ, {"MW_HUB_POLICY": str(custom)}):
            self.assertEqual(default_policy_path(), custom)
            chosen = load_policy()
            # 显式路径不被环境变量盖掉。
            explicit = load_policy(default)
        self.assertEqual(chosen.codex_max_weekly, 22)
        self.assertEqual(chosen.claude_max_weekly, 23)
        self.assertEqual(explicit.codex_max_weekly, 11)
        self.assertEqual(explicit.claude_max_weekly, 12)
        with patch.dict(os.environ, {"MW_HUB_POLICY": str(missing)}):
            self.assertEqual(load_policy().codex_max_weekly, 90)
            self.assertEqual(load_policy().claude_max_weekly, 85)


class QuotaTextTests(unittest.TestCase):
    def test_parse_official_layout(self):
        parsed = parse_quota_text(_QUOTA_SAMPLE)
        self.assertEqual(parsed["claude"], (84.0, "resets 10-20 15:04"))
        self.assertEqual(parsed["codex"], (91.0, "resets 10-21 08:00"))
        self.assertNotIn("agy", parsed)
        self.assertNotIn("grok", parsed)
        # 第一条 weekly 用量为准，后面的 “per-model weekly” 不能改写。
        self.assertEqual(parsed["claude"][0], 84.0)

    def test_unusable_text_is_empty(self):
        self.assertEqual(parse_quota_text(""), {})
        self.assertEqual(parse_quota_text(None), {})
        self.assertEqual(parse_quota_text("No providers detected. Install claude / codex / agy and sign in."), {})
        self.assertEqual(parse_quota_text("weekly 99% used\n"), {})
        first_only = f"{_BAR} codex\n   weekly      10% used\n   weekly      99% used  (resets 01-01 00:00)\n"
        self.assertEqual(parse_quota_text(first_only), {"codex": (10.0, "")})

    def setUp(self):
        argv = patch("makewand.route_select.quota_argv", return_value=FAKE_QUOTA_ARGV)
        argv.start()
        self.addCleanup(argv.stop)

    def test_quota_argv_uses_the_installed_go_component(self):
        # setUp replaces route_select.quota_argv; _REAL_QUOTA_ARGV is the real one.
        with patch("makewand.cli._find_go_binary", return_value="/opt/mw/bin/makewand-server"):
            self.assertEqual(_REAL_QUOTA_ARGV(), ("/opt/mw/bin/makewand-server", "quota"))
        with patch("makewand.cli._find_go_binary", return_value=None):
            self.assertIsNone(_REAL_QUOTA_ARGV())

    def test_default_reader_runs_nothing_without_the_go_component(self):
        with patch("makewand.route_select.quota_argv", return_value=None), \
                patch("makewand.route_select.subprocess.run") as run:
            self.assertEqual(default_quota_reader(), {})
        run.assert_not_called()

    def test_default_reader_returns_empty_when_the_command_fails(self):
        with patch("makewand.route_select.subprocess.run", side_effect=FileNotFoundError("missing")):
            self.assertEqual(default_quota_reader(), {})
        timeout = subprocess.TimeoutExpired(FAKE_QUOTA_ARGV, QUOTA_TIMEOUT_SECONDS)
        with patch("makewand.route_select.subprocess.run", side_effect=timeout):
            self.assertEqual(default_quota_reader(), {})
        with patch("makewand.route_select.subprocess.run", side_effect=OSError("boom")):
            self.assertEqual(default_quota_reader(), {})
        garbage = subprocess.CompletedProcess(FAKE_QUOTA_ARGV, 1, stdout="not quota output", stderr="err")
        with patch("makewand.route_select.subprocess.run", return_value=garbage) as run:
            self.assertEqual(default_quota_reader(), {})
        argv, kwargs = run.call_args
        self.assertEqual(tuple(argv[0]), FAKE_QUOTA_ARGV)
        self.assertEqual(kwargs["timeout"], QUOTA_TIMEOUT_SECONDS)
        self.assertFalse(kwargs["check"])

    def test_default_reader_parses_stdout(self):
        completed = subprocess.CompletedProcess(FAKE_QUOTA_ARGV, 0, stdout=_QUOTA_SAMPLE, stderr="")
        with patch("makewand.route_select.subprocess.run", return_value=completed):
            parsed = default_quota_reader()
        self.assertEqual(parsed["codex"], (91.0, "resets 10-21 08:00"))
        self.assertEqual(parsed["claude"][0], 84.0)


class SelectTests(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-routing-select-")
        self.addCleanup(self._tmp.cleanup)
        self.base = Path(self._tmp.name)
        self.ledger = OutcomeLedger(self.base / "routing")
        self.now = 1_800_000_000.0

    def _select(self, task_type="implement", difficulty="mechanical", **kwargs):
        kwargs.setdefault("risk", "low")
        kwargs.setdefault("ledger", self.ledger)
        kwargs.setdefault("now", self.now)
        kwargs.setdefault("rng", ConstantRng(0.5))
        kwargs.setdefault("quota_reader", lambda: {})
        kwargs.setdefault("policy", Policy())
        return select(task_type, difficulty, **kwargs)

    def test_tied_scores_keep_catalog_order(self):
        with patch("makewand.route_select.time.time", side_effect=AssertionError("wall clock")):
            result = self._select()
        self.assertIsInstance(result, RouteResult)
        self.assertEqual(result.tier, 1)
        self.assertEqual(result.escalate_tier, 2)
        self.assertEqual(result.skipped, [])
        self.assertFalse(self.ledger.outcomes_path.exists())
        self.assertFalse(self.ledger.marks_path.exists())
        for got, item in zip(result.candidates, candidates(1)):
            self.assertIsInstance(got, RouteCandidate)
            self.assertEqual(got.provider, item.provider)
            self.assertEqual(got.args, item.args)
            self.assertIsInstance(got.args, tuple)
            self.assertEqual(got.label, item.label)
            self.assertFalse(got.is_self)
            self.assertEqual(got.sampled_score, 0.5)
        self.assertEqual([item.provider for item in result.candidates], [item.provider for item in candidates(1)])

    def test_pause_skips_until_the_local_timestamp(self):
        text = "2026-10-20T12:00"
        until = _epoch(text)
        path = self.base / "policy"
        path.write_text(f'PAUSED="codex:{text} bogus"\n', encoding="utf-8")
        policy = load_policy(path)
        self.assertEqual(policy.paused, (Pause("codex", until, text),))

        before = self._select(policy=policy, now=until - 1)
        self.assertEqual(dict(before.skipped)["codex"], f"暂停到 {text}")
        self.assertTrue(_chinese(dict(before.skipped)["codex"]))
        self.assertNotIn("codex", [item.provider for item in before.candidates])

        at_expiry = self._select(policy=policy, now=until)
        self.assertNotIn("codex", dict(at_expiry.skipped))
        self.assertIn("codex", [item.provider for item in at_expiry.candidates])

        after = self._select(policy=policy, now=until + 60)
        self.assertNotIn("codex", dict(after.skipped))
        self.assertIn("codex", [item.provider for item in after.candidates])

    def test_active_mark_skips_and_expired_mark_does_not(self):
        self.ledger.mark("claude", self.now + 3600, "官方周额度用完")
        self.ledger.mark("codex", self.now + 1, "")
        self.ledger.mark("grok", self.now, "到点")
        self.ledger.mark("agy", self.now - 10, "已过期")
        result = self._select()
        skipped = dict(result.skipped)
        self.assertEqual(skipped["claude"], "额度标记未过期：官方周额度用完")
        self.assertEqual(skipped["codex"], "额度标记未过期")
        self.assertTrue(_chinese(skipped["claude"]))
        self.assertTrue(_chinese(skipped["codex"]))
        self.assertNotIn("grok", skipped)
        self.assertNotIn("agy", skipped)
        names = [item.provider for item in result.candidates]
        self.assertEqual(names, ["agy", "grok"])
        self.assertFalse(self.ledger.outcomes_path.exists())

    def test_weekly_threshold_skips_codex_and_claude_only(self):
        def reader():
            return {
                "codex": (90, "resets 10-20 15:04"),
                "claude": (85, "resets 10-21 08:00"),
                "agy": (100, "resets soon"),
                "grok": (100, ""),
            }

        result = self._select(quota_reader=reader)
        skipped = dict(result.skipped)
        self.assertEqual(skipped["codex"], "周额度已用 90%，达到阈值 90%（resets 10-20 15:04）")
        self.assertEqual(skipped["claude"], "周额度已用 85%，达到阈值 85%（resets 10-21 08:00）")
        self.assertTrue(_chinese(skipped["codex"]))
        self.assertNotIn("agy", skipped)
        self.assertNotIn("grok", skipped)
        self.assertEqual([item.provider for item in result.candidates], ["agy", "grok"])

        under = self._select(quota_reader=lambda: {"codex": (89.9, ""), "claude": (84, "resets 10-21 08:00")})
        self.assertEqual(under.skipped, [])
        self.assertEqual([item.provider for item in under.candidates], ["agy", "codex", "grok", "claude"])

        custom = Policy(codex_max_weekly=50, claude_max_weekly=40)
        mixed = self._select(
            policy=custom,
            quota_reader=lambda: {"Codex": (50, ""), "claude": (39, "")},
        )
        self.assertEqual(dict(mixed.skipped), {"codex": "周额度已用 50%，达到阈值 50%"})
        self.assertIn("claude", [item.provider for item in mixed.candidates])

    def test_unknown_quota_does_not_exclude(self):
        partial = self._select(quota_reader=lambda: {"codex": (99, "resets tomorrow")})
        self.assertIn("codex", dict(partial.skipped))
        self.assertIn("claude", [item.provider for item in partial.candidates])

        self.assertEqual(self._select(quota_reader=lambda: {}).skipped, [])
        self.assertEqual(self._select(quota_reader=lambda: None).skipped, [])
        garbage = self._select(quota_reader=lambda: {"codex": "unknown", "claude": (None, "")})
        self.assertEqual(garbage.skipped, [])

        stdout = f"{_BAR} codex\n   weekly      95% used  (resets 10-20 15:04)\n{_BAR} claude\n   weekly      10% used\n"
        completed = subprocess.CompletedProcess(FAKE_QUOTA_ARGV, 0, stdout=stdout, stderr="")
        with patch("makewand.route_select.quota_argv", return_value=FAKE_QUOTA_ARGV), \
                patch("makewand.route_select.subprocess.run", return_value=completed) as run:
            wired = self._select(quota_reader=None)
        self.assertTrue(run.called)
        self.assertEqual(
            dict(wired.skipped)["codex"],
            "周额度已用 95%，达到阈值 90%（resets 10-20 15:04）",
        )
        self.assertNotIn("claude", dict(wired.skipped))

        with patch("makewand.route_select.subprocess.run", side_effect=AssertionError("command")) as run:
            quiet = self._select(quota_reader=lambda: {})
        self.assertFalse(run.called)
        self.assertEqual(quiet.skipped, [])

    def test_not_tool_skips_the_same_family(self):
        result = self._select(not_tool="codex")
        self.assertEqual(dict(result.skipped), {"codex": "与 codex 同属 openai 家族"})
        self.assertTrue(_chinese(result.skipped[0][1]))
        self.assertEqual([item.provider for item in result.candidates], ["agy", "grok", "claude"])

        claude = self._select(not_tool=" Claude ")
        self.assertEqual(dict(claude.skipped), {"claude": "与 claude 同属 anthropic 家族"})
        self.assertIn("codex", [item.provider for item in claude.candidates])

        blank = self._select(not_tool="  ", self_tool="")
        self.assertEqual(blank.skipped, [])
        self.assertTrue(all(not item.is_self for item in blank.candidates))

        with self.assertRaises(ValueError) as ctx:
            self._select(not_tool="openai")
        self.assertIn("codex", str(ctx.exception))

    def test_self_sorts_after_every_other_candidate(self):
        rng = ScriptedRng([0.99, 0.1, 0.4, 0.3])
        result = self._select(self_tool="agy", rng=rng)
        self.assertEqual([item.provider for item in result.candidates], ["grok", "claude", "codex", "agy"])
        self.assertEqual([item.sampled_score for item in result.candidates], [0.4, 0.3, 0.1, 0.99])
        self.assertTrue(result.candidates[-1].is_self)
        self.assertTrue(all(item.is_self is False for item in result.candidates[:-1]))
        self.assertEqual(result.candidates[0].args, ("--tier", "fast", "--effort", "low"))

        tied = self._select(self_tool="codex", rng=ConstantRng(0.5))
        self.assertEqual([item.provider for item in tied.candidates], ["agy", "grok", "claude", "codex"])
        self.assertTrue(all(item.sampled_score == 0.5 for item in tied.candidates))
        self.assertTrue(tied.candidates[-1].is_self)
        self.assertTrue(all(item.is_self is False for item in tied.candidates[:-1]))

    def test_skipped_candidates_are_not_sampled(self):
        text = "2026-10-20T12:00"
        policy = Policy(paused=(Pause("agy", _epoch(text), text),))
        rng = ScriptedRng([0.2, 0.9, 0.3, 7])
        result = self._select(policy=policy, now=_epoch(text) - 1, rng=rng)
        self.assertEqual([item.provider for item in result.candidates], ["grok", "claude", "codex"])
        self.assertEqual([item.sampled_score for item in result.candidates], [0.9, 0.3, 0.2])
        self.assertEqual(len(rng.calls), 3)
        self.assertEqual(rng._scores, [7])

    def test_beta_sampling_is_reproducible_and_ranks_the_winner_first(self):
        for _ in range(50):
            self.ledger.record("codex", 1, "implement", True, seconds=1, calls=1, ts=self.now)
            self.ledger.record("agy", 1, "implement", False, seconds=1, calls=1, ts=self.now)
        winner = self.ledger.stats("codex", 1, "implement", now=self.now)
        loser = self.ledger.stats("agy", 1, "implement", now=self.now)
        self.assertEqual((winner.alpha, winner.beta), (51, 1))
        self.assertEqual((loser.alpha, loser.beta), (1, 51))

        first = self._select(rng=random.Random(_SEED))
        second = self._select(rng=random.Random(_SEED))
        self.assertEqual(first, second)

        replay = random.Random(_SEED)
        expected = {}
        for item in candidates(1):
            stats = self.ledger.stats(item.provider, 1, "implement", now=self.now)
            expected[item.provider] = replay.betavariate(float(stats.alpha), float(stats.beta))
        names = [item.provider for item in first.candidates]
        catalog = [item.provider for item in candidates(1)]
        last = len(catalog) - 1

        def weighted(name):  # cheapest-first weighting by catalog position
            return expected[name] * (1.0 - COST_WEIGHT_SPREAD * catalog.index(name) / last)

        expected_order = sorted(catalog, key=lambda name: (-weighted(name), catalog.index(name)))
        self.assertEqual(names, expected_order)
        for item in first.candidates:
            self.assertEqual(item.sampled_score, expected[item.provider])
            self.assertFalse(item.is_self)
        self.assertLess(names.index("codex"), names.index("agy"))

    def test_without_outcomes_cheaper_candidates_rank_first_more_often(self):
        catalog = [item.provider for item in candidates(1)]
        first_counts = {name: 0 for name in catalog}
        for seed in range(2000):
            result = self._select(rng=random.Random(seed))
            first_counts[result.candidates[0].provider] += 1
        # Equal priors: the cheapest catalog position wins clearly more often than the costliest.
        self.assertGreater(first_counts[catalog[0]], first_counts[catalog[-1]] * 1.3, first_counts)

    def test_strong_outcomes_still_overtake_the_cost_weight(self):
        catalog = [item.provider for item in candidates(1)]
        costly, cheap = catalog[-1], catalog[0]
        for _ in range(50):
            self.ledger.record(costly, 1, "implement", True, ts=self.now)
            self.ledger.record(cheap, 1, "implement", False, ts=self.now)
        for seed in range(50):
            names = [item.provider for item in self._select(rng=random.Random(seed)).candidates]
            self.assertLess(names.index(costly), names.index(cheap))

    def test_tier_override_and_escalate_tier(self):
        pressed = self._select("test", "normal")
        self.assertEqual(starting_tier("test", "normal"), 1)
        self.assertEqual(pressed.tier, 1)
        self.assertEqual(pressed.escalate_tier, 2)

        middle = self._select("implement", "normal")
        self.assertEqual(middle.tier, 2)
        self.assertEqual(middle.escalate_tier, 3)
        self.assertEqual(
            [item.provider for item in middle.candidates],
            [item.provider for item in candidates(2)],
        )

        strong = self._select("review", "hard")
        self.assertEqual(strong.tier, 3)
        self.assertIsNone(strong.escalate_tier)

        high = self._select("doc", "mechanical", risk="high")
        self.assertEqual(high.tier, 3)
        self.assertIsNone(high.escalate_tier)

        overridden = self._select("implement", "hard", tier_override=1)
        self.assertEqual(starting_tier("implement", "hard"), 3)
        self.assertEqual(overridden.tier, 1)
        self.assertEqual(overridden.escalate_tier, 2)
        self.assertEqual(
            [item.provider for item in overridden.candidates],
            [item.provider for item in candidates(1)],
        )

        as_text = self._select("implement", "mechanical", tier_override="2")
        self.assertEqual(as_text.tier, 2)
        self.assertEqual(as_text.escalate_tier, 3)

        with self.assertRaises(ValueError) as ctx:
            self._select(tier_override=4)
        self.assertIn("legal values", str(ctx.exception))
        self.assertIn("3", str(ctx.exception))

        with self.assertRaises(ValueError) as ctx:
            self._select("nope", "normal", tier_override=1)
        self.assertIn("review", str(ctx.exception))

        with self.assertRaises(ValueError):
            self._select(risk="medium")

    def test_skip_reasons_follow_catalog_order(self):
        text = "2099-01-01T00:00"
        until = _epoch(text)
        policy = Policy(paused=(Pause("codex", until, text),))
        self.ledger.mark("claude", until, "熔断")
        calls = []

        def reader():
            calls.append("read")
            return {"agy": (100, "")}

        result = self._select(policy=policy, now=self.now, not_tool="grok", quota_reader=reader)
        # codex 已暂停、claude 已有标记，没有还要看周额度的候选。
        self.assertEqual(calls, [])
        self.assertEqual(
            result.skipped,
            [
                ("codex", f"暂停到 {text}"),
                ("grok", "与 grok 同属 xai 家族"),
                ("claude", "额度标记未过期：熔断"),
            ],
        )
        self.assertTrue(all(_chinese(reason) for _provider, reason in result.skipped))
        self.assertEqual([item.provider for item in result.candidates], ["agy"])

        both = Policy(paused=(Pause("codex", until, text), Pause("claude", until, text)))

        def boom():
            raise AssertionError("quota reader should not be called")

        quiet = self._select(policy=both, now=self.now, quota_reader=boom)
        self.assertEqual(
            [provider for provider, _reason in quiet.skipped],
            ["codex", "claude"],
        )


if __name__ == "__main__":
    unittest.main()
