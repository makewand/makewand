"""
Unit tests for Makewand Auto-Fix Pattern Memory.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import unittest
import tempfile
import shutil
from pathlib import Path
from unittest.mock import patch

from makewand.memory import (
    record_autofix_lesson,
    record_failure_pattern,
    get_relevant_hints,
    get_kibitzer_nudges,
    format_memory_hints_for_prompt,
    format_kibitzer_guidance,
    _load_patterns,
    _save_patterns,
)

class TestMemory(unittest.TestCase):
    def setUp(self):
        self.tmp_dir = Path(tempfile.mkdtemp(prefix="makewand_test_mem_"))
        self.patcher = patch("makewand.memory.PATTERNS_FILE", self.tmp_dir / "test_patterns.json")
        self.patcher_lock = patch("makewand.memory.PATTERNS_LOCK", self.tmp_dir / "test_patterns.lock")
        self.patcher.start()
        self.patcher_lock.start()

    def tearDown(self):
        self.patcher.stop()
        self.patcher_lock.stop()
        shutil.rmtree(self.tmp_dir, ignore_errors=True)

    def test_default_patterns_load(self):
        patterns = _load_patterns()
        self.assertTrue(len(patterns) >= 4)
        keywords = [k for p in patterns for k in p.get("keywords", [])]
        self.assertIn("mock", keywords)
        self.assertIn("worktree", keywords)

    def test_record_and_retrieve_lesson(self):
        record_autofix_lesson(
            keywords=["redis", "connection_pool"],
            issue="Connection leak when closing Redis client",
            lesson="Always call client.close() or use context manager with Redis connections"
        )

        hints = get_relevant_hints("Please refactor redis connection_pool handling")
        self.assertEqual(len(hints), 1)
        self.assertIn("Connection leak", hints[0]["issue"])
        self.assertIn("client.close()", hints[0]["lesson"])

    def test_format_memory_hints_for_prompt(self):
        prompt = "Write mock tests for tuple unpacking"
        formatted = format_memory_hints_for_prompt(prompt)
        self.assertIn("【历史避坑与质量规范提示 (Makewand Pattern Memory)】", formatted)
        self.assertIn("避坑点:", formatted)
        self.assertIn("防范准则:", formatted)

        # Prompt with no matching keywords returns empty string
        no_match = format_memory_hints_for_prompt("xyz non-matching query 12345")
        self.assertEqual(no_match, "")

    def test_record_lesson_when_save_fails_does_not_deadlock(self):
        # Simulate os.replace failure during initial write
        with patch("os.replace", side_effect=OSError("Disk full / replace failed")):
            success = record_autofix_lesson(
                keywords=["concurrency", "deadlock"],
                issue="Test issue",
                lesson="Test lesson"
            )
            self.assertFalse(success)

        # Ensure subsequent operations proceed normally without hanging on stale locks
        hints = get_relevant_hints("concurrency")
        self.assertIsInstance(hints, list)

    def test_kibitzer_nudges_and_formatting(self):
        # Implementation stage nudges
        nudges = get_kibitzer_nudges("Refactor goroutine and unbuffered channel handling in Go worker", stage="implementation")
        self.assertTrue(len(nudges) >= 1)
        self.assertTrue(any("Go goroutine leaks" in n["focus"] for n in nudges))

        guidance_impl = format_kibitzer_guidance("Refactor goroutine and unbuffered channel handling in Go worker", stage="implementation")
        self.assertIn("【Makewand Kibitzer 实时工程质量与避坑护航】", guidance_impl)
        self.assertIn("避坑要点:", guidance_impl)
        self.assertIn("质量准则:", guidance_impl)

        # Review stage nudges
        guidance_rev = format_kibitzer_guidance("Review rust borrow checker and unwrap usage", stage="review")
        self.assertIn("【Makewand Kibitzer 独立审计核查要点】", guidance_rev)
        self.assertIn("核验隐患:", guidance_rev)
        self.assertIn("验收准则:", guidance_rev)

        # Unmatched query
        self.assertEqual(format_kibitzer_guidance("completely unrelated 98765"), "")

    def test_record_failure_pattern(self):
        ok = record_failure_pattern(
            issue="Socket FD leak in HTTP client keep-alive",
            lesson="Always invoke resp.Body.Close() or configure Transport.ResponseHeaderTimeout",
            keywords=["socket", "http_client", "fd_leak"]
        )
        self.assertTrue(ok)
        nudges = get_kibitzer_nudges("Fix http_client socket connections", stage="implementation")
        self.assertTrue(len(nudges) >= 1)
        self.assertTrue(any("Socket FD leak" in n["focus"] for n in nudges))

if __name__ == "__main__":
    unittest.main()
