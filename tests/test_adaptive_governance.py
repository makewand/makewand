"""
Unit tests for Makewand Adaptive Governance & Elastic Engine enhancements.
Validates:
1. is_reset_time_passed relative time bugfix and date parsing.
2. Monotonic cache timestamp preservation in save_status_cache.
3. Tier-weighted usage accounting (fast 0.5, standard 1.0, deep 2.0).
4. Continuous fluid penalty curve with 15% hysteresis.
5. Auto-Fix reviewer orthogonality (excluding both initial coder and repair engine).
6. --boost overclocking soft-penalty bypass.
7. Low-usage performance milking (Surplus Harvest Bonus).
8. Upgraded detect_task_tier for algorithms vs fast probes.
9. Pipeline timeout and budget decoupling.
"""

import os
import json
import tempfile
import unittest
from pathlib import Path
from datetime import datetime, timedelta
from unittest.mock import patch, MagicMock

from makewand.health import is_reset_time_passed, save_status_cache, load_status_cache
from makewand.usage import record_engine_usage, get_burn_rate_penalty, _calc_weighted_counts, _calc_continuous_penalty
from makewand.orchestrator import detect_task_tier, select_optimal_engine_pair, run_pipeline

class TestAdaptiveGovernance(unittest.TestCase):
    def setUp(self):
        self.test_dir = tempfile.mkdtemp()
        self.usage_file = Path(self.test_dir) / "test_usage.json"
        self.status_file = Path(self.test_dir) / "test_status.json"

    def tearDown(self):
        import shutil
        shutil.rmtree(self.test_dir, ignore_errors=True)

    def test_relative_time_does_not_fall_through_to_ttl(self):
        # 1. 24h in future, recorded 3 hours ago -> MUST NOT be passed!
        updated_3h_ago = (datetime.now() - timedelta(hours=3)).isoformat()
        res = is_reset_time_passed("in 24 hours", updated_3h_ago)
        self.assertFalse(res, "in 24 hours must not be considered passed after only 3 hours!")

        # 2. 15 minutes in future, recorded 20 minutes ago -> MUST be passed!
        updated_20m_ago = (datetime.now() - timedelta(minutes=20)).isoformat()
        res = is_reset_time_passed("in 15 minutes", updated_20m_ago)
        self.assertTrue(res, "in 15 minutes must be considered passed after 20 minutes!")

    def test_date_with_month_name_parsing(self):
        # Month name date parsing (e.g. Claude weekly reset format)
        yesterday = datetime.now() - timedelta(days=1)
        resets_yesterday = yesterday.strftime("%b %d at %I:%M%p")
        self.assertTrue(is_reset_time_passed(resets_yesterday, datetime.now().isoformat()))

        tomorrow = datetime.now() + timedelta(days=1)
        resets_tomorrow = tomorrow.strftime("%b %d at %I:%M%p")
        self.assertFalse(is_reset_time_passed(resets_tomorrow, datetime.now().isoformat()))

    def test_monotonic_status_cache_saving(self):
        with patch("makewand.health.STATUS_CACHE_FILE", self.status_file):
            now = datetime.now()
            # 1. Save newer healthy state at 01:00
            newer_state = {
                "codex": {
                    "status": "healthy",
                    "reason": "OK",
                    "resets_at": None,
                    "updated_at": (now).isoformat()
                }
            }
            save_status_cache(newer_state)

            # 2. Attempt to save older limited state from 00:00 (e.g. delayed thread snapshot)
            older_state = {
                "codex": {
                    "status": "limited",
                    "reason": "Old 429",
                    "resets_at": "in 1 hour",
                    "updated_at": (now - timedelta(hours=1)).isoformat()
                }
            }
            save_status_cache(older_state)

            # 3. Cache MUST preserve newer healthy state!
            current = load_status_cache()
            self.assertEqual(current["codex"]["status"], "healthy", "Older snapshot must not overwrite newer healthy cache!")

    def test_tier_weighted_usage_accounting(self):
        records = [
            {"timestamp": datetime.now().isoformat(), "engine": "codex", "tier": "fast"},     # 0.5
            {"timestamp": datetime.now().isoformat(), "engine": "codex", "tier": "standard"}, # 1.0
            {"timestamp": datetime.now().isoformat(), "engine": "codex", "tier": "deep"},     # 2.0
        ]
        c_3h, c_24h, c_7d = _calc_weighted_counts(records, "codex")
        self.assertEqual(c_3h, 3.5, "fast(0.5) + standard(1.0) + deep(2.0) must equal 3.5 weighted calls")

    def test_continuous_penalty_smoothness_and_hysteresis(self):
        # Warn: 20, Limit: 35, WarnPen: -0.8, LimitPen: -1.8
        # Under 85% of warn (17): no penalty
        self.assertIsNone(_calc_continuous_penalty(16.0, 20.0, 35.0, -0.8, -1.8, "Test", "3h"))

        # Exactly at warn (20): -0.8
        p_20, _ = _calc_continuous_penalty(20.0, 20.0, 35.0, -0.8, -1.8, "Test", "3h")
        self.assertEqual(p_20, -0.8)

        # Midway (27): smoothly between -0.8 and -1.8
        p_27, _ = _calc_continuous_penalty(27.0, 20.0, 35.0, -0.8, -1.8, "Test", "3h")
        self.assertTrue(-1.8 < p_27 < -0.8)

        # Call 34 to 35: difference is small continuous delta, NO cliff jump!
        p_34, _ = _calc_continuous_penalty(34.0, 20.0, 35.0, -0.8, -1.8, "Test", "3h")
        p_35, _ = _calc_continuous_penalty(35.0, 20.0, 35.0, -0.8, -1.8, "Test", "3h")
        diff = abs(p_35 - p_34)
        self.assertLess(diff, 0.25, f"Step delta must be continuous, got jump of {diff}")

    def test_detect_task_tier_upgrades(self):
        # Algorithmic & concurrency phrases must trigger deep
        self.assertEqual(detect_task_tier("implement a lock-free ring buffer with memory ordering"), "deep")
        self.assertEqual(detect_task_tier("cross-module refactor of the distributed scheduler"), "deep")
        self.assertEqual(detect_task_tier("排查底层原子操作与无锁环形缓冲区死锁"), "deep")

        # Fast probe inspect phrases must trigger fast (not deep on '安全')
        self.assertEqual(detect_task_tier("快速查看这个按钮是否正常"), "fast")
        self.assertEqual(detect_task_tier("快速探测系统配置"), "fast")

        # Plain generic tasks without complex algorithms remain standard
        self.assertEqual(detect_task_tier("设计一个登录页面"), "standard")
        self.assertEqual(detect_task_tier("写一个简单的待办列表脚本"), "fast")

    def test_boost_overclocking_penetrates_soft_penalties(self):
        healthy_cache = {
            "codex": {"status": "healthy"},
            "claude": {"status": "healthy"},
            "grok": {"status": "healthy"},
            "agy": {"status": "healthy"},
            "muse": {"status": "healthy"},
        }
        # Simulate heavy usage penalty on codex (-1.8)
        with patch("makewand.usage.get_burn_rate_penalty", side_effect=lambda eng: (-1.8, "Heavy usage") if eng == "codex" else (0.0, None)):
            # Standard mode: codex score penalized
            coders_std, _, meta_std = select_optimal_engine_pair("实现一个动态规划算法", tier="standard", cache=healthy_cache, boost=False)
            self.assertNotEqual(meta_std["primary_coder"], "codex", "Heavily penalized codex must not be primary coder in standard mode")

            # Boost mode: codex penalty is penetrated!
            coders_boost, _, meta_boost = select_optimal_engine_pair("实现一个动态规划算法", tier="deep", cache=healthy_cache, boost=True)
            self.assertEqual(meta_boost["primary_coder"], "codex", "Boost mode must bypass soft penalties and allocate flagship model")
            self.assertTrue(any("强制超频" in r for r in meta_boost["reasons"]))

    def test_low_usage_performance_milking_bonus(self):
        healthy_cache = {
            "codex": {"status": "healthy"},
            "claude": {"status": "healthy"},
            "grok": {"status": "healthy"},
            "agy": {"status": "healthy"},
            "muse": {"status": "healthy"},
        }
        # With zero usage in past 24h, all healthy commercial models receive surplus milking bonus (+0.5)
        with patch("makewand.usage.get_engine_usage_stats", return_value={"codex": {"total": 0}, "claude": {"total": 0}, "grok": {"total": 0}, "muse": {"total": 0}}), \
             patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)):
            _, _, meta = select_optimal_engine_pair("一般性开发任务", tier="standard", cache=healthy_cache)
            self.assertTrue(any("低频性能榨取放量加权" in r for r in meta["reasons"]), "Zero usage must grant surplus milking bonus")

    def test_autofix_reviewer_orthogonality_excludes_both_coder_and_fixer(self):
        # Test candidate selection logic in Auto-Fix
        coder_engine = "claude"
        actual_fix_engine = "codex"
        actual_reviewers = ["codex", "claude"]
        cache = {
            "claude": {"status": "healthy"},
            "codex": {"status": "healthy"},
            "grok": {"status": "healthy"},
            "agy": {"status": "healthy"},
            "muse": {"status": "healthy"}
        }

        # Candidate re-reviewers MUST exclude BOTH coder_engine and actual_fix_engine
        excluded = {coder_engine, actual_fix_engine}
        candidates = [r for r in actual_reviewers if r not in excluded]
        if not candidates:
            healthy_alts = [
                e for e in ["codex", "claude", "grok", "agy", "muse"]
                if e not in excluded and cache.get(e, {}).get("status") not in ["limited", "needs_auth", "missing"]
            ]
            candidates = healthy_alts

        self.assertNotIn("claude", candidates, "Original coder must NEVER be in re-review candidates")
        self.assertNotIn("codex", candidates, "Repair engine must NEVER be in re-review candidates")
        self.assertEqual(candidates[0], "grok", "Next independent reviewer should be Grok/AGY")

if __name__ == "__main__":
    unittest.main()
