"""
Unit tests for Makewand Agent-Computer Interface (makewand/aci.py).
"""

try:  # 测试隔离必须先于 makewand 导入
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import tempfile
import unittest
from pathlib import Path
from makewand.aci import view_window, search_code, truncate_output_folded

class TestACI(unittest.TestCase):

    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.cwd = self.temp_dir.name

    def tearDown(self):
        self.temp_dir.cleanup()

    def test_truncate_output_folded_short_output(self):
        short = "line 1\nline 2\nline 3"
        res = truncate_output_folded(short, max_lines=10)
        self.assertEqual(res, short)

    def test_truncate_output_folded_long_output(self):
        long_output = "\n".join([f"log line {i}" for i in range(100)])
        res = truncate_output_folded(long_output, max_lines=20)
        self.assertIn("Makewand ACI: 已折叠", res)
        self.assertTrue(res.startswith("log line 0"))
        self.assertTrue(res.endswith("log line 99"))

    def test_view_window(self):
        test_file = Path(self.cwd) / "sample.py"
        test_file.write_text("\n".join([f"# Line {i}" for i in range(1, 51)]), encoding="utf-8")

        res = view_window("sample.py", line_number=25, window_size=5, cwd=self.cwd)
        self.assertIn("Total: 50 行", res)
        self.assertIn("窗口: L20-L30", res)
        self.assertIn("▶ L  25: # Line 25", res)
        self.assertIn("  L  20: # Line 20", res)
        self.assertNotIn("# Line 10", res)

    def test_view_window_nonexistent(self):
        res = view_window("no_such_file.py", cwd=self.cwd)
        self.assertIn("Error:", res)

    def test_search_code(self):
        f1 = Path(self.cwd) / "mod_a.py"
        f1.write_text("def find_my_secret():\n    return 42\n", encoding="utf-8")
        f2 = Path(self.cwd) / "mod_b.py"
        f2.write_text("class AnotherClass:\n    pass\n", encoding="utf-8")

        res = search_code("find_my_secret", cwd=self.cwd)
        self.assertIn("mod_a.py:L1:", res)
        self.assertIn("def find_my_secret():", res)
        self.assertNotIn("AnotherClass", res)
