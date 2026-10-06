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

    def test_fast_syntax_check_routes_through_sandbox(self):
        """Regression test for Priority 1: fast_syntax_check must execute compilers inside sandbox."""
        from unittest.mock import patch
        js_file = Path(self.cwd) / "sample.js"
        js_file.write_text("console.log(1);\n", encoding="utf-8")

        with patch("makewand.linter.is_bwrap_available", return_value=True), \
             patch("makewand.linter.run_in_sandbox", return_value=(0, "", "", None)) as mock_sandbox:
            ok, errors = fast_syntax_check(self.cwd, ["sample.js"])
            self.assertTrue(ok)
            self.assertEqual(errors, [])
            self.assertTrue(mock_sandbox.called)
            cmd = mock_sandbox.call_args[0][0]
            self.assertEqual(cmd[0], "node")
            self.assertEqual(cmd[1], "-c")
            kwargs = mock_sandbox.call_args[1]
            self.assertTrue(kwargs.get("readonly"))
            self.assertFalse(kwargs.get("allow_network"))
            self.assertEqual(kwargs.get("audit_context"), "linter_syntax_check")

    def test_fast_syntax_check_non_bwrap_isolated_temp_env(self):
        """Regression test for Priority 1: when bwrap is absent, syntax checks must run in an isolated temp environment."""
        from unittest.mock import patch, MagicMock
        js_file = Path(self.cwd) / "script.js"
        js_file.write_text("const x = 1;\n", encoding="utf-8")

        with patch("makewand.linter.is_bwrap_available", return_value=False), \
             patch("subprocess.run") as mock_subproc:
            mock_subproc.return_value = MagicMock(returncode=0, stdout="", stderr="")
            ok, errors = fast_syntax_check(self.cwd, ["script.js"])
            self.assertTrue(ok)
            self.assertEqual(errors, [])
            self.assertTrue(mock_subproc.called)
            # The cwd must NOT be the host workspace
            run_cwd = mock_subproc.call_args[1].get("cwd")
            self.assertNotEqual(run_cwd, self.cwd)
            self.assertTrue(os.path.isdir(run_cwd) or "/tmp" in run_cwd)

