"""
Unit tests for Makewand Auto-Linter & Fast Syntax Gate (makewand/linter.py).
"""

try:  # 测试隔离必须先于 makewand 导入
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import tempfile
import unittest
from pathlib import Path
from makewand.linter import auto_format_files

class TestLinter(unittest.TestCase):

    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.cwd = self.temp_dir.name

    def tearDown(self):
        self.temp_dir.cleanup()


    def test_auto_format_files_skips_nonexistent(self):
        res = auto_format_files(self.cwd, ["nonexistent.py"])
        self.assertEqual(res, {})

    def test_auto_format_files_safe_paths(self):
        py_file = Path(self.cwd) / "code.py"
        py_file.write_text("x = 1 + 2\n", encoding="utf-8")
        res = auto_format_files(self.cwd, ["code.py"])
        self.assertIn("code.py", res)


    def test_prettier_passes_no_config(self):
        """Regression test for P1-S3: prettier must receive --no-config to avoid code execution."""
        from unittest.mock import patch, MagicMock
        js_file = Path(self.cwd) / "index.js"
        js_file.write_text("const a = 1;\n", encoding="utf-8")

        def mock_which(cmd):
            if cmd == "prettier":
                return "/usr/bin/prettier"
            return None

        with patch("shutil.which", side_effect=mock_which), \
             patch("subprocess.run") as mock_run:
            mock_run.return_value = MagicMock(returncode=0)
            res = auto_format_files(self.cwd, ["index.js"])
            self.assertTrue(res.get("index.js"))
            self.assertTrue(mock_run.called)
            called_cmd = mock_run.call_args[0][0]
            self.assertEqual(called_cmd[0], "prettier")
            self.assertIn("--no-config", called_cmd)
            self.assertIn("--write", called_cmd)


