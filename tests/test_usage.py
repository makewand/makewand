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

from makewand.usage import record_engine_usage, get_engine_usage_stats, get_burn_rate_penalty, _load_raw_usage_records

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


if __name__ == "__main__":
    unittest.main()

