"""
Unit tests for Makewand Auto-Fix Pattern Memory.
"""

import unittest
import tempfile
import shutil
from pathlib import Path
from unittest.mock import patch

from makewand.memory import (
    record_autofix_lesson,
    get_relevant_hints,
    format_memory_hints_for_prompt,
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

if __name__ == "__main__":
    unittest.main()
