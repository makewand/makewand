"""
G3 reliability regressions: routing penalty math and engine selection.

Covers runtime-state#2, py-reliability#5 (burn-rate hard clamp flattened the main
engines to 0.2 and put a failing agy first), py-reliability#4 (never-probed
"unknown" status became a -999 exclusion and hash-order routing),
runtime-state#3 / py-reliability#3 (agy reliability-based down-weighting) and
arch-product#6 (detect-only tools must not be dispatched).
"""

import json
import math
import os
import subprocess
import sys
import tempfile
import unittest
from datetime import datetime, timedelta
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
import makewand.usage as usage
import makewand.orchestrator as orchestrator
from makewand.orchestrator import select_optimal_engine_pair
from makewand.pacing import calculate_dynamic_pacing

# New penalty-math helpers are looked up lazily so that, on the pre-fix code,
# the behavioural tests below still run and fail on their assertions.
def burn_rate_factor(pen):
    return orchestrator.burn_rate_factor(pen)


def apply_burn_rate_penalty(score, pen):
    return orchestrator.apply_burn_rate_penalty(score, pen)


def reliability_factor(rate):
    return orchestrator.reliability_factor(rate)

REPO_ROOT = Path(__file__).resolve().parent.parent
CORE = ["claude", "codex", "grok", "agy", "muse"]


class _IsolatedState(unittest.TestCase):
    """Private status cache + usage ledger per test; never touches a real HOME."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-g3-routing-")
        root = Path(self._tmp.name)
        self.ledger = root / "usage_window.json"
        patches = [
            patch.object(config, "CONFIG_DIR", root / "cfg"),
            patch.object(config, "CANDIDATES_DIR", root / "cfg" / "candidates"),
            patch.object(config, "BACKUPS_DIR", root / "cfg" / "backups"),
            patch.object(config, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json"),
            patch.object(health, "STATUS_CACHE_FILE", root / "cfg" / "status.json"),
            patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json"),
            patch.object(usage, "USAGE_WINDOW_FILE", self.ledger),
            patch("makewand.config.get_active_providers", return_value=list(CORE)),
            patch("makewand.config.has_api_configured", return_value=False),
            patch("makewand.config.is_provider_enabled", return_value=True),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)

    def write_ledger(self, records):
        self.ledger.write_text(json.dumps(records), encoding="utf-8")

    @staticmethod
    def healthy_cache(engines=CORE):
        return {e: {"status": "healthy", "reason": "", "resets_at": None,
                    "updated_at": datetime.now().isoformat()} for e in engines}


class TestPenaltyMathBounds(unittest.TestCase):
    """py-reliability#5: bounded, continuous, monotone penalty math."""

    PENALTIES = [0.0, -0.2, -0.8, -1.49, -1.5, -1.51, -2.0, -3.0, -3.5, -4.0, -6.0, -1e9, float("-inf")]

    def test_burn_rate_factor_upper_and_lower_bounds(self):
        floor = orchestrator.BURN_PENALTY_MIN_FACTOR
        self.assertGreater(floor, 0.0)
        for pen in self.PENALTIES:
            f = burn_rate_factor(pen)
            self.assertLessEqual(f, 1.0, pen)
            self.assertGreaterEqual(f, floor, pen)
        self.assertEqual(burn_rate_factor(0.0), 1.0)
        self.assertEqual(burn_rate_factor(0.7), 1.0, "positive input is not a penalty")
        self.assertEqual(burn_rate_factor(float("nan")), 1.0)
        self.assertAlmostEqual(burn_rate_factor(-4.0), floor)

    def test_penalty_is_monotone_and_has_no_cliff_at_minus_1_5(self):
        score = 4.3  # codex base 1.8 + algorithm affinity 2.5
        values = [apply_burn_rate_penalty(score, p) for p in self.PENALTIES]
        for earlier, later in zip(values, values[1:]):
            self.assertLessEqual(later, earlier + 1e-12)
        # Old code: -1.49 -> 2.81, -1.51 -> max(0.2, 1.5-1.51) = 0.2 (a 2.6 point cliff).
        self.assertLess(abs(apply_burn_rate_penalty(score, -1.49) - apply_burn_rate_penalty(score, -1.51)), 0.05)

    def test_positive_scores_stay_eligible_and_non_positive_untouched(self):
        for score in (0.05, 0.8, 2.0, 5.3):
            for pen in self.PENALTIES:
                out = apply_burn_rate_penalty(score, pen)
                self.assertGreater(out, 0.0)
                self.assertGreaterEqual(out, score * orchestrator.BURN_PENALTY_MIN_FACTOR - 1e-12)
                self.assertLessEqual(out, score)
        self.assertEqual(apply_burn_rate_penalty(-999.0, -4.0), -999.0)
        self.assertEqual(apply_burn_rate_penalty(0.0, -4.0), 0.0)

    def test_reliability_factor_bounds(self):
        self.assertEqual(reliability_factor(None), 1.0)
        self.assertEqual(reliability_factor(float("nan")), 1.0)
        previous = None
        floor = orchestrator.RELIABILITY_MIN_FACTOR
        for rate in [x / 20 for x in range(0, 21)]:
            f = reliability_factor(rate)
            self.assertGreaterEqual(f, floor)
            self.assertLessEqual(f, 1.0)
            if previous is not None:
                self.assertGreaterEqual(f, previous - 1e-12)
            previous = f
        self.assertEqual(reliability_factor(1.0), 1.0)
        self.assertEqual(reliability_factor(0.0), floor)


class TestRoutingInversionRegression(_IsolatedState):
    """runtime-state#2: real-state dry run put agy (93% failing) first and clamped claude/codex/grok to 0.2."""

    PENALTIES = {"claude": -3.5, "codex": -4.0, "grok": -1.72, "muse": -2.56}

    def _agy_failure_ledger(self):
        now = datetime.now()
        records = []
        for i in range(42):
            records.append({
                "timestamp": (now - timedelta(hours=1 + i)).isoformat(),
                "engine": "agy", "tier": "standard", "success": i < 3, "task": "real task",
            })
        return records

    def test_failing_agy_is_not_primary_and_main_engines_are_not_flattened(self):
        self.write_ledger(self._agy_failure_ledger())
        with patch("makewand.usage.get_burn_rate_penalty",
                   side_effect=lambda e: (self.PENALTIES.get(e, 0.0), "heavy" if e in self.PENALTIES else None)):
            coders, reviewers, meta = select_optimal_engine_pair(
                "实现用户数据导出功能并落盘", tier="standard", cache=self.healthy_cache())
        scores = meta["scores"]
        self.assertNotEqual(coders[0], "agy", f"agy with 3/42 real successes must not lead: {scores}")
        for engine in ("claude", "codex", "grok"):
            self.assertIn(engine, coders, "bounded penalty must keep main engines eligible")
            self.assertGreater(scores[engine], 0.2 + 1e-6, f"{engine} must not be clamped to 0.2: {scores}")
        self.assertGreater(len({round(scores[e], 6) for e in ("claude", "codex", "grok")}), 1,
                           "main engines must not collapse to one identical floor score")
        self.assertLess(scores["agy"], 1.4, "low real success rate must down-weight agy and drop its +0.5")
        self.assertTrue(any("真实派发成功率" in r for r in meta["reasons"]))

    def test_task_affinity_survives_moderate_burn_penalty(self):
        # Old clamp: codex (1.8 + 2.5 affinity) with pen -1.8 -> max(0.2, 1.5 - 1.8) = 0.2.
        with patch("makewand.usage.get_burn_rate_penalty",
                   side_effect=lambda e: (-1.8, "heavy") if e == "codex" else (0.0, None)):
            coders, _, meta = select_optimal_engine_pair(
                "修复 goroutine 并发死锁", tier="standard", cache=self.healthy_cache())
        self.assertEqual(coders[0], "codex", meta["scores"])
        self.assertGreater(meta["scores"]["codex"], 2.0)

    def test_reviewer_scores_use_bounded_penalty(self):
        with patch("makewand.usage.get_burn_rate_penalty",
                   side_effect=lambda e: (-4.0, "heavy") if e == "codex" else (0.0, None)):
            _, reviewers, _ = select_optimal_engine_pair(
                "实现用户数据导出功能并落盘", tier="standard", cache=self.healthy_cache())
        # Old code: 2.2 + (-4.0) = -1.8 -> codex silently dropped from the reviewer pool.
        self.assertIn("codex", reviewers)


class TestUnknownStatusIsNeutral(_IsolatedState):
    """py-reliability#4: never-probed providers must not be treated as 0% quota (-999)."""

    def test_unknown_pacing_is_neutral(self):
        for engine in CORE:
            p = calculate_dynamic_pacing(engine, dict(health.DEFAULT_CACHE[engine]))
            self.assertEqual(p["routing_boost"], 0.0, engine)
            self.assertNotEqual(p["pacing_state"], "limited", engine)
            self.assertIn("makewand probe", p["reason"])

    def test_fresh_install_routes_by_base_scores(self):
        cache = {k: dict(v) for k, v in health.DEFAULT_CACHE.items()}
        with patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)):
            coders, reviewers, meta = select_optimal_engine_pair("实现用户数据导出功能并落盘", tier="standard", cache=cache)
        self.assertEqual(coders[0], "claude", meta["scores"])
        self.assertTrue(all(meta["scores"][e] > 0 for e in CORE), meta["scores"])
        self.assertFalse(any("无可用自主工具" in r for r in meta["reasons"]))

    def test_fresh_install_primary_is_independent_of_hash_seed(self):
        script = (
            "import sys, tempfile, pathlib; sys.path.insert(0, sys.argv[1])\n"
            "from unittest.mock import patch\n"
            "import makewand.config as config, makewand.health as health, makewand.usage as usage\n"
            "tmp = pathlib.Path(tempfile.mkdtemp())\n"
            "config.CONFIG_DIR = tmp; config.CANDIDATES_DIR = tmp / 'c'; config.BACKUPS_DIR = tmp / 'b'\n"
            "config.LEGACY_TRIO_CACHE = tmp / 'none' / 't.json'; health.LEGACY_TRIO_CACHE = config.LEGACY_TRIO_CACHE\n"
            "health.STATUS_CACHE_FILE = tmp / 'status.json'; usage.USAGE_WINDOW_FILE = tmp / 'usage.json'\n"
            "from makewand.orchestrator import select_optimal_engine_pair\n"
            "pool = ['claude', 'codex', 'grok', 'agy', 'muse', 'copilot', 'cursor']\n"
            "with patch('makewand.config.get_active_providers', return_value=pool), \\\n"
            "     patch('makewand.config.has_api_configured', return_value=False), \\\n"
            "     patch('makewand.config.is_provider_enabled', return_value=True):\n"
            "    c, r, m = select_optimal_engine_pair('实现用户数据导出功能并落盘', tier='standard', cache={k: dict(v) for k, v in health.DEFAULT_CACHE.items()})\n"
            "print(c[0], r[0])\n"
        )
        outputs = set()
        for seed in ("1", "2", "3"):
            env = {k: v for k, v in os.environ.items() if k != "PYTHONHASHSEED"}
            env["PYTHONHASHSEED"] = seed
            res = subprocess.run([sys.executable, "-I", "-c", script, str(REPO_ROOT)],
                                 capture_output=True, text=True, env=env, timeout=120)
            self.assertEqual(res.returncode, 0, res.stderr)
            outputs.add(res.stdout.strip().splitlines()[-1])
        self.assertEqual(len(outputs), 1, f"routing must not depend on PYTHONHASHSEED: {outputs}")
        self.assertTrue(next(iter(outputs)).startswith("claude "), outputs)


class TestDetectOnlyToolsAreNotDispatched(_IsolatedState):
    """arch-product#6: cursor/copilot have no execution adapter."""

    def test_cursor_and_copilot_never_selected(self):
        pool = list(CORE) + ["cursor", "copilot"]
        cache = self.healthy_cache(pool)
        with patch("makewand.config.get_active_providers", return_value=pool), \
             patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)):
            coders, reviewers, meta = select_optimal_engine_pair("实现用户数据导出功能并落盘", tier="standard", cache=cache)
        for tool in ("cursor", "copilot"):
            self.assertNotIn(tool, coders)
            self.assertNotIn(tool, reviewers)


class TestAgyReliabilityBonus(_IsolatedState):
    """runtime-state#3: agy loses its +0.5 when real dispatches mostly fail, keeps it otherwise."""

    def _ledger(self, successes, failures):
        now = datetime.now()
        rows = []
        for i in range(successes + failures):
            rows.append({"timestamp": (now - timedelta(minutes=10 * (i + 1))).isoformat(),
                         "engine": "agy", "tier": "standard", "success": i < successes, "task": "t"})
        return rows

    def test_low_success_rate_cancels_bonus(self):
        self.write_ledger(self._ledger(1, 9))
        p = calculate_dynamic_pacing("agy", {"status": "healthy", "updated_at": datetime.now().isoformat()})
        self.assertEqual(p["routing_boost"], 0.0)
        self.assertEqual(p["signal"], "reliability")

    def test_healthy_success_rate_keeps_bonus(self):
        self.write_ledger(self._ledger(9, 1))
        p = calculate_dynamic_pacing("agy", {"status": "healthy", "updated_at": datetime.now().isoformat()})
        self.assertEqual(p["routing_boost"], 0.5)

    def test_insufficient_evidence_is_neutral(self):
        self.write_ledger(self._ledger(0, 2))
        rate, weight, raw = usage.get_engine_reliability("agy")
        self.assertIsNone(rate)
        self.assertEqual(raw, 2)

    def test_old_failures_decay(self):
        now = datetime.now()
        rows = [{"timestamp": (now - timedelta(days=6)).isoformat(), "engine": "codex", "tier": "standard",
                 "success": False, "task": "t"} for _ in range(30)]
        rows += [{"timestamp": (now - timedelta(minutes=5 * (i + 1))).isoformat(), "engine": "codex",
                  "tier": "standard", "success": True, "task": "t"} for i in range(6)]
        self.write_ledger(rows)
        rate, _, raw = usage.get_engine_reliability("codex")
        self.assertIsNotNone(rate)
        self.assertEqual(raw, 36)
        undecayed = 6 / 36
        self.assertGreater(rate, undecayed + 0.2, "week-old failures must weigh less than recent successes")


if __name__ == "__main__":
    unittest.main()
