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
from makewand.linter import auto_format_files, fast_syntax_check

class TestLinter(unittest.TestCase):

    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.cwd = self.temp_dir.name

    def tearDown(self):
        self.temp_dir.cleanup()

    def test_fast_syntax_check_valid_files(self):
        py_file = Path(self.cwd) / "valid.py"
        py_file.write_text("def hello():\n    return 'world'\n", encoding="utf-8")

        json_file = Path(self.cwd) / "valid.json"
        json_file.write_text('{"status": "ok", "count": 10}', encoding="utf-8")

        ok, errors = fast_syntax_check(self.cwd, ["valid.py", "valid.json"])
        self.assertTrue(ok)
        self.assertEqual(errors, [])

    def test_fast_syntax_check_detects_python_syntax_error(self):
        py_file = Path(self.cwd) / "bad_syntax.py"
        py_file.write_text("def hello(\n    return 'unclosed paren'\n", encoding="utf-8")

        ok, errors = fast_syntax_check(self.cwd, ["bad_syntax.py"])
        self.assertFalse(ok)
        self.assertEqual(len(errors), 1)
        self.assertIn("bad_syntax.py", errors[0])

    def test_fast_syntax_check_detects_json_syntax_error(self):
        json_file = Path(self.cwd) / "bad.json"
        json_file.write_text('{"status": "unclosed', encoding="utf-8")

        ok, errors = fast_syntax_check(self.cwd, ["bad.json"])
        self.assertFalse(ok)
        self.assertEqual(len(errors), 1)
        self.assertIn("JSON 格式错误", errors[0])

    def test_auto_format_files_skips_nonexistent(self):
        res = auto_format_files(self.cwd, ["nonexistent.py"])
        self.assertEqual(res, {})

    def test_auto_format_files_safe_paths(self):
        py_file = Path(self.cwd) / "code.py"
        py_file.write_text("x = 1 + 2\n", encoding="utf-8")
        res = auto_format_files(self.cwd, ["code.py"])
        self.assertIn("code.py", res)

    def test_fast_syntax_check_detects_js_syntax_error(self):
        js_file = Path(self.cwd) / "bad.js"
        js_file.write_text("function bad( { return 1; }", encoding="utf-8")
        ok, errors = fast_syntax_check(self.cwd, ["bad.js"])
        self.assertFalse(ok)
        self.assertIn("bad.js", errors[0])

    def test_fast_syntax_check_detects_rust_syntax_error(self):
        rs_file = Path(self.cwd) / "bad.rs"
        rs_file.write_text("fn main( { println!(\"error\"); }", encoding="utf-8")
        ok, errors = fast_syntax_check(self.cwd, ["bad.rs"])
        self.assertFalse(ok)
        self.assertIn("bad.rs", errors[0])

    def test_fast_syntax_check_py_compile_shadowing_defense(self):
        """
        Ensures a malicious py_compile.py placed in the workspace cannot hijack execution,
        and syntax checking succeeds in-process safely.
        """
        canary = Path(self.cwd) / "pwned_canary.txt"
        malicious_py_compile = Path(self.cwd) / "py_compile.py"
        malicious_py_compile.write_text(f"from pathlib import Path\nPath({repr(str(canary))}).write_text('pwned')\n", encoding="utf-8")

        valid_py = Path(self.cwd) / "service.py"
        valid_py.write_text("def run():\n    return 42\n", encoding="utf-8")

        ok, errors = fast_syntax_check(self.cwd, ["service.py"])
        self.assertTrue(ok)
        self.assertEqual(errors, [])
        # The canary must NEVER have been created
        self.assertFalse(canary.exists(), "Security failure: workspace py_compile.py was executed!")

    def test_run_local_tests_fast_syntax_gate_integration(self):
        """
        Verifies that run_local_tests detects dirty files and fails fast on syntax errors
        even in a project without existing test suites.
        """
        from makewand.orchestrator import run_local_tests
        bad_py = Path(self.cwd) / "invalid.py"
        bad_py.write_text("def invalid(\n", encoding="utf-8")

        ok, out = run_local_tests(self.cwd)
        self.assertFalse(ok)
        self.assertIn("Fast Syntax Gate", out)
