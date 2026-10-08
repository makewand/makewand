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

    def test_fast_syntax_check_non_bwrap_no_unsandboxed_host_exec(self):
        """Regression test for P1 commits-review-6 / P0 py-orch-1: when sandbox is unavailable, compilers must not run unsandboxed on host."""
        from unittest.mock import patch
        js_file = Path(self.cwd) / "script.js"
        js_file.write_text("const x = 1;\n", encoding="utf-8")

        with patch("makewand.linter.run_in_sandbox", return_value=(-1, "", "Bubblewrap (bwrap) sandbox is not available", "SandboxUnavailable")), \
             patch("subprocess.run") as mock_subproc:
            ok, errors = fast_syntax_check(self.cwd, ["script.js"])
            self.assertTrue(ok)
            self.assertEqual(errors, [])
            mock_subproc.assert_not_called()

    def test_fast_syntax_check_rust_standalone_sandboxed(self):
        """Regression test for P1 commits-review-6: standalone Rust files (without Cargo.toml) validate using --crate-type lib inside sandbox."""
        import shutil
        if not shutil.which("rustc"):
            self.skipTest("rustc not installed on host")

        rs_valid = Path(self.cwd) / "valid.rs"
        rs_valid.write_text("fn helper() {}\n", encoding="utf-8")
        rs_invalid = Path(self.cwd) / "invalid.rs"
        rs_invalid.write_text("fn helper() { let x = ; }\n", encoding="utf-8")

        # Valid standalone Rust without main() must succeed due to --crate-type lib
        ok, errors = fast_syntax_check(self.cwd, ["valid.rs"])
        self.assertTrue(ok, f"Expected valid.rs to succeed in sandbox, got: {errors}")
        self.assertEqual(errors, [])

        # Invalid Rust must fail with compilation error
        ok, errors = fast_syntax_check(self.cwd, ["invalid.rs"])
        self.assertFalse(ok)
        self.assertEqual(len(errors), 1)
        self.assertIn("invalid.rs", errors[0])

    def test_fast_syntax_check_go_package_and_fallback(self):
        """Regression test: Go syntax check must support multi-file packages and sandbox execution."""
        import shutil
        if not shutil.which("go") and not shutil.which("gofmt"):
            self.skipTest("Neither go nor gofmt installed on host")

        foo_go = Path(self.cwd) / "foo.go"
        foo_go.write_text("package main\nfunc main() { helper() }\n", encoding="utf-8")
        bar_go = Path(self.cwd) / "bar.go"
        bar_go.write_text("package main\nfunc helper() {}\n", encoding="utf-8")
        bad_go = Path(self.cwd) / "bad.go"
        bad_go.write_text("package main\nfunc main() { syntax error }\n", encoding="utf-8")

        # 1. Live sandbox mode
        ok, errors = fast_syntax_check(self.cwd, ["foo.go"])
        self.assertTrue(ok, f"Expected foo.go to pass in sandbox, got: {errors}")

        ok, errors = fast_syntax_check(self.cwd, ["bad.go"])
        self.assertFalse(ok)
        self.assertEqual(len(errors), 1)
        self.assertIn("bad.go", errors[0])

        # 2. When sandbox is unavailable, skip gracefully without false syntax errors
        from unittest.mock import patch
        with patch("makewand.linter.run_in_sandbox", return_value=(-1, "", "SandboxUnavailable", "SandboxUnavailable")):
            ok, errors = fast_syntax_check(self.cwd, ["foo.go", "bad.go"])
            self.assertTrue(ok)
            self.assertEqual(errors, [])

    def test_fast_syntax_check_go_gofmt_only(self):
        """Regression test: Go syntax check must succeed using gofmt when go binary is missing."""
        from unittest.mock import patch

        foo_go = Path(self.cwd) / "only_gofmt.go"
        foo_go.write_text("package main\nfunc main() {}\n", encoding="utf-8")

        with patch("shutil.which", side_effect=lambda x: "/usr/bin/gofmt" if x == "gofmt" else None), \
             patch("makewand.linter.run_in_sandbox", return_value=(0, "", "", None)) as mock_sandbox:
            ok, errors = fast_syntax_check(self.cwd, ["only_gofmt.go"])
            self.assertTrue(ok)
            self.assertEqual(errors, [])
            self.assertTrue(mock_sandbox.called)
            cmd = mock_sandbox.call_args[0][0]
            self.assertEqual(cmd[0], "/usr/bin/gofmt")

    def test_fast_syntax_check_bwrap_runtime_error_skipped_safely(self):
        """Regression test for P1 commits-review-6: when run_in_sandbox fails with bwrap runtime/namespace error, skip without failing or running unsandboxed."""
        from unittest.mock import patch
        js_file = Path(self.cwd) / "fallback.js"
        js_file.write_text("const a = 10;\n", encoding="utf-8")

        with patch("makewand.linter.run_in_sandbox", return_value=(1, "", "bwrap: Can't create user namespace: Operation not permitted\n", None)), \
             patch("subprocess.run") as mock_subproc:
            ok, errors = fast_syntax_check(self.cwd, ["fallback.js"])
            self.assertTrue(ok)
            self.assertEqual(errors, [])
            mock_subproc.assert_not_called()

    def test_fast_syntax_check_react_jsx_in_js_passes(self):
        """Regression test for P1 commits-review-6: React .js files containing JSX syntax must not fail syntax gate."""
        js_file = Path(self.cwd) / "App.js"
        js_file.write_text(
            "import React from 'react';\n"
            "export const App = () => {\n"
            "    return (\n"
            "        <div className=\"container\">\n"
            "            <h1>Hello World</h1>\n"
            "        </div>\n"
            "    );\n"
            "};\n"
            "export default App;\n",
            encoding="utf-8"
        )
        ok, errors = fast_syntax_check(self.cwd, ["App.js"])
        self.assertTrue(ok, f"Expected React JSX file App.js to pass syntax check, got: {errors}")
        self.assertEqual(errors, [])

    def test_fast_syntax_check_react_fragment_in_js_passes(self):
        """Regression test for P1 commits-review-6: React .js files with JSX fragments <>...</> must not fail syntax gate."""
        js_file = Path(self.cwd) / "FragmentComponent.js"
        js_file.write_text(
            "const FragmentComponent = () => (\n"
            "    <>\n"
            "        <span>First</span>\n"
            "        <span>Second</span>\n"
            "    </>\n"
            ");\n"
            "export default FragmentComponent;\n",
            encoding="utf-8"
        )
        ok, errors = fast_syntax_check(self.cwd, ["FragmentComponent.js"])
        self.assertTrue(ok, f"Expected fragment JSX to pass syntax check, got: {errors}")
        self.assertEqual(errors, [])

    def test_fast_syntax_check_react_component_tags_in_js_passes(self):
        """Regression test for P1 commits-review-6: React .js files with custom <Component /> tags must not fail."""
        js_file = Path(self.cwd) / "Dashboard.js"
        js_file.write_text(
            "const Dashboard = () => <UserProfile id={123} />;\n"
            "export default Dashboard;\n",
            encoding="utf-8"
        )
        ok, errors = fast_syntax_check(self.cwd, ["Dashboard.js"])
        self.assertTrue(ok, f"Expected custom JSX tag to pass syntax check, got: {errors}")
        self.assertEqual(errors, [])

    def test_fast_syntax_check_rust_cargo_crate_module_passes(self):
        """Regression test for P1 commits-review-6: Rust multi-file crate submodules referencing crate/super must not fail."""
        cargo_toml = Path(self.cwd) / "Cargo.toml"
        cargo_toml.write_text(
            "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
            encoding="utf-8"
        )
        src_dir = Path(self.cwd) / "src"
        src_dir.mkdir(parents=True, exist_ok=True)
        lib_rs = src_dir / "lib.rs"
        lib_rs.write_text("pub mod parser;\npub fn helper() {}\n", encoding="utf-8")
        parser_rs = src_dir / "parser.rs"
        parser_rs.write_text("use crate::helper;\npub fn parse() { helper(); }\n", encoding="utf-8")

        ok, errors = fast_syntax_check(self.cwd, ["src/parser.rs", "src/lib.rs"])
        self.assertTrue(ok, f"Expected Cargo crate modules to pass syntax gate without standalone rustc errors, got: {errors}")
        self.assertEqual(errors, [])

    def test_fast_syntax_check_rust_cargo_crate_nested_submodule_passes(self):
        """Regression test for P1 commits-review-6: Deeply nested modules in Cargo crates must be recognized."""
        cargo_toml = Path(self.cwd) / "Cargo.toml"
        cargo_toml.write_text("[package]\nname = \"nested\"\nversion = \"0.1.0\"\n", encoding="utf-8")
        nested_dir = Path(self.cwd) / "src" / "ast" / "tokens"
        nested_dir.mkdir(parents=True, exist_ok=True)
        token_rs = nested_dir / "token.rs"
        token_rs.write_text("use super::super::types;\npub struct Token;\n", encoding="utf-8")

        ok, errors = fast_syntax_check(self.cwd, ["src/ast/tokens/token.rs"])
        self.assertTrue(ok, f"Expected nested module to pass syntax check, got: {errors}")
        self.assertEqual(errors, [])




