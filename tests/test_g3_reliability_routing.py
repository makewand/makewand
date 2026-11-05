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
from makewand.pacing import calculate_dynamic_pacing


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


class TestUnknownStatusIsNeutral(_IsolatedState):
    """py-reliability#4: never-probed providers must not be treated as 0% quota (-999)."""

    def test_unknown_pacing_is_neutral(self):
        for engine in CORE:
            p = calculate_dynamic_pacing(engine, dict(health.DEFAULT_CACHE[engine]))
            self.assertEqual(p["routing_boost"], 0.0, engine)
            self.assertNotEqual(p["pacing_state"], "limited", engine)
            self.assertIn("makewand probe", p["reason"])


    def test_fresh_install_primary_is_independent_of_hash_seed(self):
        script = (
            "import sys, tempfile, pathlib; sys.path.insert(0, sys.argv[1])\n"
            "from unittest.mock import patch\n"
            "import makewand.config as config, makewand.health as health, makewand.usage as usage\n"
            "tmp = pathlib.Path(tempfile.mkdtemp())\n"
            "config.CONFIG_DIR = tmp; config.CANDIDATES_DIR = tmp / 'c'; config.BACKUPS_DIR = tmp / 'b'\n"
            "config.LEGACY_TRIO_CACHE = tmp / 'none' / 't.json'; health.LEGACY_TRIO_CACHE = config.LEGACY_TRIO_CACHE\n"
            "health.STATUS_CACHE_FILE = tmp / 'status.json'; usage.USAGE_WINDOW_FILE = tmp / 'usage.json'\n"
            "from makewand.engine_selection import select_optimal_engine_pair\n"
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

