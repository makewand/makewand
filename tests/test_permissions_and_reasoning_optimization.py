"""Tests for permissions & sandbox hardening, probe timeout dynamic adaptation,
and non-agentic execution boundaries.
"""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch, MagicMock

import makewand.config as config
import makewand.health as health
from makewand.health import _resolve_probe_timeout, probe_model
import makewand.orchestrator as orch
from makewand.discovery import resolve_model_and_effort


class TestProbeTimeoutDynamicAdaptation(unittest.TestCase):
    """Verifies that probe timeouts adapt dynamically based on reasoning tier and effort."""

    def test_muse_probe_timeout_scales_with_deep_reasoning(self):
        # Default base for Muse should be at least 45s (previously 25s)
        t_base = _resolve_probe_timeout("muse", 25, tier="standard", effort="medium")
        self.assertGreaterEqual(t_base, 45)

        # Deep reasoning effort: max / xhigh or deep tier must scale to at least 90s
        t_max = _resolve_probe_timeout("muse", 25, tier="standard", effort="max")
        self.assertGreaterEqual(t_max, 90)

        t_xhigh = _resolve_probe_timeout("muse", 25, tier="standard", effort="xhigh")
        self.assertGreaterEqual(t_xhigh, 90)

        t_deep = _resolve_probe_timeout("muse", 25, tier="deep", effort=None)
        self.assertGreaterEqual(t_deep, 90)

        t_high = _resolve_probe_timeout("muse", 25, tier="standard", effort="high")
        self.assertGreaterEqual(t_high, 60)

    def test_codex_probe_timeout_scales_with_deep_tier(self):
        # Base timeout for Codex
        t_base = _resolve_probe_timeout("codex", 35, tier="standard", effort="low")
        self.assertGreaterEqual(t_base, 45)

        # Deep tier / max effort scales to at least 90s
        t_deep = _resolve_probe_timeout("codex", 35, tier="deep", effort=None)
        self.assertGreaterEqual(t_deep, 90)

    def test_env_overrides_take_highest_precedence(self):
        with patch.dict(os.environ, {"MAKEWAND_PROBE_TIMEOUT_MUSE": "120"}):
            t = _resolve_probe_timeout("muse", 25, tier="fast", effort="low")
            self.assertEqual(t, 120)

        with patch.dict(os.environ, {"MAKEWAND_PROBE_TIMEOUT": "75"}, clear=False):
            # Without specific muse override
            t = _resolve_probe_timeout("grok", 20, tier="standard", effort="low")
            self.assertEqual(t, 75)

    def test_probe_model_passes_dynamic_timeout_and_avoids_false_needs_auth(self):
        with tempfile.TemporaryDirectory() as td:
            # Mock _run_model_probe returning timeout error
            with patch("makewand.health._run_model_probe") as mock_probe, \
                 patch("makewand.config.has_subscription_configured", return_value=True), \
                 patch("makewand.config.has_api_configured", return_value=False), \
                 patch("makewand.config.is_provider_enabled", return_value=True):

                # 1. Calculation timeout without login error must report warning, not needs_auth
                mock_probe.return_value = (124, "", "execution deadline expired", "Command timed out after 90 seconds")
                res = probe_model("muse", tier="deep", cwd=td)
                self.assertEqual(res["status"], "warning")
                self.assertIn("深度推理", res["reason"])

                # 2. If timeout combined with OAuth / login hint, report needs_auth
                mock_probe.return_value = (124, "Please visit https://auth.meta.com/login", "", "Command timed out")
                res_auth = probe_model("muse", tier="deep", cwd=td)
                self.assertEqual(res_auth["status"], "needs_auth")


class TestDiscoveryEffortFromConfig(unittest.TestCase):
    """Verifies that discovery reads reasoning_effort from user config."""

    def test_muse_settings_effort_detection(self):
        with tempfile.TemporaryDirectory() as td:
            muse_dir = Path(td) / ".config" / "muse"
            muse_dir.mkdir(parents=True)
            settings_file = muse_dir / "settings.json"
            settings_file.write_text(json.dumps({
                "model": "muse-spark-1.3-contributor",
                "reasoning_effort": "max"
            }))
            with patch.object(Path, "home", return_value=Path(td)):
                res = resolve_model_and_effort("muse", tier="standard")
                self.assertEqual(res["effort"], "max")

    def test_codex_config_effort_detection(self):
        with tempfile.TemporaryDirectory() as td:
            codex_dir = Path(td) / ".codex"
            codex_dir.mkdir(parents=True)
            cfg_file = codex_dir / "config.toml"
            cfg_file.write_text('model = "gpt-6.1-sol"\nmodel_reasoning_effort = "high"\n')
            with patch.object(Path, "home", return_value=Path(td)):
                res = resolve_model_and_effort("codex", tier="standard")
                self.assertEqual(res["effort"], "high")


class TestPlaceholderAndNonAgenticHandlers(unittest.TestCase):
    """Verifies graceful execution of placeholder providers and non-agentic diff handling."""

    def test_cursor_and_copilot_placeholder_dispatch(self):
        for engine in ("cursor", "copilot"):
            with contextlib.redirect_stderr(io.StringIO()):
                res = orch.dispatch_task(engine, "Implement feature")
            self.assertTrue(res.success)
            self.assertEqual(res.status, "PASSED")
            self.assertIn("占位引擎", res.output)

    def test_inquiry_intent_empty_diff_succeeds(self):
        # Inquiry prompt without file diff should complete successfully
        with tempfile.TemporaryDirectory() as td:
            prompt = "请解释一下什么是 MVCC 机制？"
            with patch.object(orch, "check_load_backpressure", return_value=True), \
                 patch.object(orch, "get_or_update_status", return_value={}), \
                 patch.object(orch, "select_optimal_engine_pair", return_value=(["claude"], ["codex"], {"primary_coder": "claude", "primary_reviewer": "codex"})), \
                 patch.object(orch, "dispatch_task", return_value=(True, "MVCC 是多版本并发控制机制...", None)), \
                 patch.object(orch, "get_git_diff", return_value=""), \
                 contextlib.redirect_stdout(io.StringIO()), \
                 contextlib.redirect_stderr(io.StringIO()):
                ok = orch.run_pipeline(prompt, cwd=td)
                self.assertTrue(ok)

    def test_non_agentic_coder_empty_diff_handled_gracefully(self):
        # When coder is non-agentic (deepseek/local/cursor), empty diff should not crash pipeline
        with tempfile.TemporaryDirectory() as td:
            prompt = "在当前目录编写一个算法"
            with patch.object(orch, "check_load_backpressure", return_value=True), \
                 patch.object(orch, "get_or_update_status", return_value={}), \
                 patch.object(orch, "select_optimal_engine_pair", return_value=(["deepseek"], ["codex"], {"primary_coder": "deepseek", "primary_reviewer": "codex"})), \
                 patch.object(orch, "dispatch_task", return_value=(True, "实现代码如下...", None)), \
                 patch.object(orch, "get_git_diff", return_value=""), \
                 contextlib.redirect_stdout(io.StringIO()), \
                 contextlib.redirect_stderr(io.StringIO()):
                ok = orch.run_pipeline(prompt, cwd=td, forced_engine="deepseek")
                self.assertTrue(ok)

    def test_agentic_coder_empty_diff_fails_quality_gate(self):
        # When an agentic coder (codex/claude/agy) produces no diff for a coding prompt, quality gate fails
        with tempfile.TemporaryDirectory() as td:
            prompt = "编写一个算法并在当前目录落盘"
            with patch.object(orch, "check_load_backpressure", return_value=True), \
                 patch.object(orch, "get_or_update_status", return_value={}), \
                 patch.object(orch, "select_optimal_engine_pair", return_value=(["codex"], ["claude"], {"primary_coder": "codex", "primary_reviewer": "claude"})), \
                 patch.object(orch, "dispatch_task", return_value=(True, "done", None)), \
                 patch.object(orch, "get_git_diff", return_value=""), \
                 contextlib.redirect_stdout(io.StringIO()), \
                 contextlib.redirect_stderr(io.StringIO()):
                ok = orch.run_pipeline(prompt, cwd=td, forced_engine="codex")
                self.assertFalse(ok)


class TestEmptyTestSuiteGuards(unittest.TestCase):
    """Verifies that empty test runs in Python, Go, and Node cannot pass as verified evidence."""

    def test_python_pytest_collected_zero_items_rejected(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            (root / "test_empty.py").write_text("def helper(): pass\n")
            with patch("makewand.git_helper.get_dirty_files", return_value=[]), \
                 patch("makewand.sandbox.run_in_sandbox", return_value=(0, "collected 0 items\n", "", None)):
                passed, details = orch.run_local_tests(str(root))
                self.assertFalse(passed)
                self.assertEqual(getattr(details, "execution_status", None), "UNVERIFIED")

    def test_go_no_test_files_rejected(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            (root / "go.mod").write_text("module example.com/test\n")
            with patch("makewand.git_helper.get_dirty_files", return_value=[]), \
                 patch("shutil.which", return_value="/usr/bin/go"), \
                 patch("makewand.sandbox.run_in_sandbox", return_value=(0, "? \texample.com/test\t[no test files]\n", "", None)):
                passed, details = orch.run_local_tests(str(root))
                self.assertFalse(passed)
                self.assertEqual(getattr(details, "execution_status", None), "UNVERIFIED")

    def test_python_pytest_zero_passed_or_exit_rejected(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            (root / "test_exit.py").write_text("def test_one(): pass\n")
            with patch("makewand.git_helper.get_dirty_files", return_value=[]), \
                 patch("makewand.sandbox.run_in_sandbox", return_value=(0, "=== 0 passed in 0.01s ===\n", "", None)):
                passed, details = orch.run_local_tests(str(root))
                self.assertFalse(passed)
                self.assertEqual(getattr(details, "execution_status", None), "UNVERIFIED")

            with patch("makewand.git_helper.get_dirty_files", return_value=[]), \
                 patch("makewand.sandbox.run_in_sandbox", return_value=(0, "Exit: early stop triggered\n", "", None)):
                passed, details = orch.run_local_tests(str(root))
                self.assertFalse(passed)
                self.assertEqual(getattr(details, "execution_status", None), "UNVERIFIED")


class TestSocketOverlayOrdering(unittest.TestCase):
    """Verifies that pre-existing pathname AF_UNIX sockets are properly masked and ordered after .git overlays."""

    def test_socket_in_workspace_is_masked_after_git_overlay(self):
        import socket
        from makewand.sandbox import wrap_bwrap
        with tempfile.TemporaryDirectory() as td:
            ws = Path(td) / "repo"
            git_dir = ws / ".git"
            git_dir.mkdir(parents=True)
            sock_path = ws / "daemon.sock"
            s = socket.socket(socket.AF_UNIX)
            s.bind(str(sock_path))
            s.listen(1)
            try:
                cmd = wrap_bwrap(["echo", "hi"], workspace=str(ws))
                # sock_path must be masked with --ro-bind /dev/null
                self.assertIn(str(sock_path), cmd)
                idx = cmd.index(str(sock_path))
                self.assertEqual(cmd[idx - 2], "--ro-bind")
                self.assertEqual(cmd[idx - 1], "/dev/null")
                # .git overlay must appear before the socket mask
                git_idx = cmd.index(str(git_dir))
                self.assertLess(git_idx, idx)
            finally:
                s.close()


class TestCodexAndClaudeProbeTimeoutRelaxation(unittest.TestCase):
    """Verifies that Codex and Claude probe timeouts due to deep reasoning report warning rather than error."""

    def test_codex_probe_timeout_reports_warning(self):
        with tempfile.TemporaryDirectory() as td:
            with patch("makewand.health._run_model_probe") as mock_probe, \
                 patch("makewand.config.has_subscription_configured", return_value=True), \
                 patch("makewand.config.has_api_configured", return_value=False), \
                 patch("makewand.config.is_provider_enabled", return_value=True):
                mock_probe.return_value = (124, "", "execution deadline expired", "Command timed out after 90 seconds")
                res = probe_model("codex", tier="deep", cwd=td)
                self.assertEqual(res["status"], "warning")
                self.assertIn("深度推理", res["reason"])

    def test_claude_probe_timeout_reports_warning(self):
        with tempfile.TemporaryDirectory() as td:
            with patch("makewand.health._run_model_probe") as mock_probe, \
                 patch("makewand.config.has_subscription_configured", return_value=True), \
                 patch("makewand.config.has_api_configured", return_value=False), \
                 patch("makewand.config.is_provider_enabled", return_value=True):
                mock_probe.return_value = (124, "", "execution deadline expired", "Command timed out after 60 seconds")
                res = probe_model("claude", tier="deep", cwd=td)
                self.assertEqual(res["status"], "warning")
                self.assertIn("深度思考", res["reason"])


if __name__ == "__main__":
    unittest.main()

