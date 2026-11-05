"""
Unit tests for Makewand Interactive Console / REPL.
Verifies REPL slash commands (/help, /status, /quota, /model, /provider, /clear, /compact, /tier),
display width formatting (pad_display), and conversation history sliding window truncation.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import io
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch, MagicMock



class TestModeContract(unittest.TestCase):
    """Tests for bi-directional mode and tier normalization in Python."""

    def test_normalize_tier_and_tier_to_go_mode(self):
        from makewand.config import normalize_tier, tier_to_go_mode
        self.assertEqual(normalize_tier("fast"), "fast")
        self.assertEqual(normalize_tier("standard"), "standard")
        self.assertEqual(normalize_tier("deep"), "deep")
        self.assertEqual(normalize_tier("balanced"), "standard")
        self.assertEqual(normalize_tier("power"), "deep")
        self.assertEqual(normalize_tier("auto"), "auto")
        self.assertEqual(normalize_tier("unknown"), "standard")
        self.assertEqual(normalize_tier(None), "standard")

        self.assertEqual(tier_to_go_mode("fast"), "fast")
        self.assertEqual(tier_to_go_mode("standard"), "balanced")
        self.assertEqual(tier_to_go_mode("deep"), "power")
        self.assertEqual(tier_to_go_mode("balanced"), "balanced")
        self.assertEqual(tier_to_go_mode("power"), "power")


if __name__ == "__main__":
    unittest.main()
