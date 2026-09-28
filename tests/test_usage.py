"""
Unit tests for makewand.usage module.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import unittest
import tempfile
import json
import shutil
from pathlib import Path
from datetime import datetime, timedelta
from unittest.mock import patch

from makewand.usage import (
    record_engine_usage,
    get_engine_usage_stats,
    get_burn_rate_penalty,
    _load_raw_usage_records,
    _save_raw_usage_records
)

class TestUsage(unittest.TestCase):
    def setUp(self):
        self.test_dir = tempfile.mkdtemp()
        self.test_file = Path(self.test_dir) / "usage_window.json"

    def tearDown(self):
        shutil.rmtree(self.test_dir, ignore_errors=True)

    def test_record_and_stats(self):
        with patch("makewand.usage.USAGE_WINDOW_FILE", self.test_file):
            record_engine_usage("codex", tier="standard", success=True, task="test task")
            record_engine_usage("claude", tier="deep", success=True, task="refactor")
            record_engine_usage("claude", tier="fast", success=False, task="failed task")

            stats = get_engine_usage_stats(window_hours=1.0)
            self.assertEqual(stats["codex"]["total"], 1)
            self.assertEqual(stats["codex"]["success"], 1)
            self.assertEqual(stats["claude"]["total"], 2)
            self.assertEqual(stats["claude"]["success"], 1)
            self.assertEqual(stats["claude"]["failed"], 1)
            self.assertEqual(stats["agy"]["total"], 0)

    def test_burn_rate_penalty_codex(self):
        with patch("makewand.usage.USAGE_WINDOW_FILE", self.test_file):
            # Initially 0 penalty
            pen, reason = get_burn_rate_penalty("codex")
            self.assertEqual(pen, 0.0)

            # Insert 22 records in last 2 hours
            now = datetime.now()
            records = [
                {
                    "timestamp": (now - timedelta(minutes=i * 5)).isoformat(),
                    "engine": "codex",
                    "tier": "standard",
                    "success": True,
                    "task": "task"
                }
                for i in range(22)
            ]
            _save_raw_usage_records(records)

            pen, reason = get_burn_rate_penalty("codex")
            self.assertAlmostEqual(pen, -0.8, delta=0.2)
            self.assertIn("削峰保护", reason)

            # Insert more to exceed limit (> 35)
            records.extend([
                {
                    "timestamp": (now - timedelta(minutes=i * 2)).isoformat(),
                    "engine": "codex",
                    "tier": "standard",
                    "success": True,
                    "task": "task"
                }
                for i in range(15)
            ])
            _save_raw_usage_records(records)

            pen, reason = get_burn_rate_penalty("codex")
            self.assertLessEqual(pen, -1.8)
            self.assertIn("熔断保护", reason)

    def test_burn_rate_penalty_claude(self):
        with patch("makewand.usage.USAGE_WINDOW_FILE", self.test_file):
            now = datetime.now()
            # 40 records in last 12 hours
            records = [
                {
                    "timestamp": (now - timedelta(hours=i * 0.25)).isoformat(),
                    "engine": "claude",
                    "tier": "standard",
                    "success": True,
                    "task": "task"
                }
                for i in range(40)
            ]
            _save_raw_usage_records(records)

            pen, reason = get_burn_rate_penalty("claude")
            self.assertAlmostEqual(pen, -0.8, delta=0.2)
            self.assertIn("削峰保护", reason)

    def test_burn_rate_penalty_agy_unlimited(self):
        with patch("makewand.usage.USAGE_WINDOW_FILE", self.test_file):
            now = datetime.now()
            records = [
                {
                    "timestamp": (now - timedelta(minutes=i)).isoformat(),
                    "engine": "agy",
                    "tier": "deep",
                    "success": True,
                    "task": "arch"
                }
                for i in range(100)
            ]
            _save_raw_usage_records(records)

            pen, reason = get_burn_rate_penalty("agy")
            self.assertEqual(pen, 0.0)
            self.assertIsNone(reason)

    def test_concurrent_record_no_lost_updates(self):
        import concurrent.futures
        with patch("makewand.usage.USAGE_WINDOW_FILE", self.test_file):
            total_tasks = 40
            def record_task(idx):
                record_engine_usage(
                    engine="codex" if idx % 2 == 0 else "claude",
                    tier="standard",
                    success=True,
                    task=f"concurrent_task_{idx}"
                )

            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as executor:
                futures = [executor.submit(record_task, i) for i in range(total_tasks)]
                concurrent.futures.wait(futures)

            stats = get_engine_usage_stats(window_hours=1.0)
            self.assertEqual(stats["codex"]["total"], 20)
            self.assertEqual(stats["claude"]["total"], 20)
            records = _load_raw_usage_records(max_age_days=1.0)
            self.assertEqual(len(records), total_tasks)

    def test_predictive_pacing_status(self):
        from makewand.usage import get_predictive_pacing_status
        with patch("makewand.usage.USAGE_WINDOW_FILE", self.test_file):
            # 1. agy should always be nominal
            res_agy = get_predictive_pacing_status("agy")
            self.assertEqual(res_agy["status"], "nominal")
            self.assertEqual(res_agy["quota_risk"], "low")

            # 2. Initially empty usage -> nominal
            res_codex = get_predictive_pacing_status("codex")
            self.assertEqual(res_codex["status"], "nominal")
            self.assertEqual(res_codex["quota_risk"], "low")

            # 3. Inject heavy usage for codex -> critical
            now_iso = datetime.now().isoformat()
            records = [
                {"timestamp": now_iso, "engine": "codex", "tier": "standard", "success": True}
                for _ in range(35)
            ]
            _save_raw_usage_records(records)
            res_heavy = get_predictive_pacing_status("codex")
            self.assertEqual(res_heavy["status"], "critical")
            self.assertEqual(res_heavy["quota_risk"], "high")
            self.assertGreaterEqual(res_heavy["utilization_pct"], 90.0)


if __name__ == "__main__":
    unittest.main()

