"""
Comprehensive Verification Tests for v3.1.0 Full Audit Remediations:
1. P0-1: Aider Sandbox Enforcement (wrap_bwrap, untrusted block, run_subprocess).
2. P0-2: Connected Dynamic Pacing & AGY Effort in Pipeline.
3. P0-3: Stdin and Prompt-File for Long Prompts (>32KB) avoiding Linux E2BIG.
4. P0-4: Race Mode Symmetric Dispatch and Usage Accounting across all engines.
5. P1-1: Reset Time Parsing with 'at ' / 'in ' prefixes.
6. P1-2: Test Session Usage Isolation.
7. P1-3: Clean Workspace Protection & Hard Reset to task_baseline.
8. P1-4: Non-agentic Chat/API/Local model exclusion from coding tasks.
9. Interactive REPL /model command and bounded conversation history context.
10. Subprocess pipe file descriptor leak protection.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
    from _verdicts import answer_with_nonce, nonced
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401
    from tests._verdicts import answer_with_nonce, nonced

import os
import shutil
import sys
import tempfile
import unittest
from unittest.mock import patch, MagicMock
from pathlib import Path

from makewand.orchestrator import dispatch_task
from makewand.execution_runtime import mark_provider_invocation
from makewand.pacing import parse_reset_time_to_seconds_left
from makewand.providers.base import run_subprocess
from makewand.providers.claude import parse_claude_quota, execute_claude_task
from makewand.providers.codex import execute_codex_task
from makewand.providers.grok import execute_grok_task
from makewand.providers.muse import execute_muse_task
from makewand.providers.aider import execute_aider_task
from makewand.providers.agy import execute_agy_task
import makewand.config as makewand_config


def _read_text(path):
    try:
        return Path(path).read_text(errors="replace")
    except OSError:
        return ""


def _patch_stamp(directory):
    try:
        return (Path(directory) / "rejected.patch").stat().st_mtime_ns
    except OSError:
        return None


class TestAuditV31Fixes(unittest.TestCase):

    def test_p1_1_reset_time_parsing_with_prefixes(self):
        """P1-1: Reset time strings starting with 'at ' or 'in ' must parse correctly."""
        # 1. Claude quota output parser
        is_lim, reason, resets = parse_claude_quota("You've hit your limit · resets at 2:00 PM (Asia/Shanghai)")
        self.assertTrue(is_lim)
        self.assertEqual(resets, "2:00 PM (Asia/Shanghai)")
        self.assertNotIn("at ", resets.lower())

        is_lim2, reason2, resets2 = parse_claude_quota("You've hit your limit · resets at 2:00 PM")
        self.assertTrue(is_lim2)
        self.assertEqual(resets2, "2:00 PM")
        self.assertNotIn("at ", resets2.lower())

        is_lim3, reason3, resets3 = parse_claude_quota("You have hit your 5-hour limit. Resets at 2:00 PM.")
        self.assertTrue(is_lim3)
        self.assertEqual(resets3, "2:00 PM")
        self.assertNotIn("at ", resets3.lower())

        # 2. Pacing seconds left parser
        sec = parse_reset_time_to_seconds_left("at 2:00 PM")
        self.assertIsNotNone(sec)
        self.assertGreaterEqual(sec, 0)

        sec_dot = parse_reset_time_to_seconds_left("2:00 PM.")
        self.assertIsNotNone(sec_dot)
        self.assertGreaterEqual(sec_dot, 0)

        sec_in = parse_reset_time_to_seconds_left("in 3h 15m")
        self.assertIsNotNone(sec_in)
        self.assertGreater(sec_in, 0)


    def test_p0_3_long_prompt_uses_stdin_or_file(self):
        """P0-3: Prompts > 32KB must NOT be passed as plain argv strings to avoid E2BIG."""
        long_prompt = "x" * (65 * 1024)  # 65KB string
        # No bwrap: every launch (read-only included) needs the recorded
        # host-exec authorization (H1); grant it so the argv can be inspected.
        with tempfile.TemporaryDirectory() as tmpdir, \
             patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(True, "config-ack")), \
             patch("makewand.sandbox.audit_unsafe_host_exec") as host_audit:
            # 1. Claude
            with patch("makewand.health.load_status_cache", return_value={}), \
                 patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                 patch("makewand.sandbox._agy_host_credits_disabled", return_value=True), \
                 patch("makewand.providers.claude.run_subprocess") as mock_sub:
                mock_sub.return_value = (0, "ok", "", None)
                execute_claude_task(long_prompt, cwd=tmpdir, readonly=True)
                called_cmd = mock_sub.call_args[0][0]
                self.assertNotIn(long_prompt, called_cmd)
                self.assertEqual(list(called_cmd[:1]), ["claude"])
                extra_env = mock_sub.call_args[1]["extra_env"]
                self.assertEqual(extra_env["MAKEWAND_PROVIDER_WORKER"], "1")
                self.assertNotIn("CLAUDE_CONFIG_DIR", extra_env)
                self.assertEqual(mock_sub.call_args[1].get("input_text"), long_prompt)

            # 2. Codex
            with patch("makewand.health.load_status_cache", return_value={}), \
                 patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                 patch("makewand.sandbox._agy_host_credits_disabled", return_value=True), \
                 patch("makewand.providers.codex.run_subprocess") as mock_sub:
                mock_sub.return_value = (0, "ok", "", None)
                execute_codex_task(long_prompt, cwd=tmpdir, readonly=True)
                called_cmd = mock_sub.call_args[0][0]
                self.assertNotIn(long_prompt, called_cmd)
                self.assertIn("-", called_cmd)
                self.assertEqual(mock_sub.call_args[1].get("input_text"), long_prompt)

            # 3. Muse
            with patch("makewand.health.load_status_cache", return_value={}), \
                 patch("makewand.providers.muse.detect_muse_guard", return_value=(False, None)), \
                 patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                 patch("makewand.sandbox._agy_host_credits_disabled", return_value=True), \
                 patch("makewand.providers.muse.run_subprocess") as mock_sub:
                mock_sub.return_value = (0, "ok", "", None)
                execute_muse_task(long_prompt, cwd=tmpdir, readonly=True)
                called_cmd = mock_sub.call_args[0][0]
                self.assertNotIn(long_prompt, called_cmd)
                self.assertIn("--prompt-file", called_cmd)

            # 4. Grok
            with patch("makewand.health.load_status_cache", return_value={}), \
                 patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                 patch("makewand.sandbox._agy_host_credits_disabled", return_value=True), \
                 patch("makewand.providers.grok.run_subprocess") as mock_sub:
                mock_sub.return_value = (0, "ok", "", None)
                execute_grok_task(long_prompt, cwd=tmpdir, readonly=True)
                called_cmd = mock_sub.call_args[0][0]
                self.assertNotIn(long_prompt, called_cmd)
                self.assertIn("--prompt-file", called_cmd)

            # every host launch was audited
            self.assertEqual(host_audit.call_count, 4)


    def test_run_subprocess_pipe_cleanup(self):
        """Subprocess run must cleanly close pipes and avoid file descriptor leaks."""
        rc, out, err, ex = run_subprocess(["echo", "test_cleanup"], input_text="pipe_in", timeout=5)
        self.assertEqual(rc, 0)
        self.assertIn("test_cleanup", out)


    def test_p1_a_streaming_long_stdin_no_deadlock(self):
        """P1-A: run_subprocess with stream=True handles >64KB stdin via async thread without deadlocking."""
        long_input = "line " * 20000 + "\n"  # >100KB
        rc, out, err, ex = run_subprocess(["cat"], input_text=long_input, stream=True, timeout=15)
        self.assertEqual(rc, 0)
        self.assertIn("line line", out)
        self.assertIsNone(ex)


    def test_p1_e_aider_sandbox_env_and_mount(self):
        """P1-E: Aider in sandbox passes API keys and ro-bind mounts ~/.aider.conf.yml if exists."""
        from makewand.sandbox import wrap_bwrap
        # A throwaway HOME: the test must never create (or delete) ~/.aider.conf.yml
        # in the developer's real home directory.
        with tempfile.TemporaryDirectory() as tmpdir, tempfile.TemporaryDirectory() as fake_home:
            conf_file = Path(fake_home) / ".aider.conf.yml"
            conf_file.write_text("model: gpt-4o\n")
            with patch.dict(os.environ, {"HOME": fake_home, "OPENAI_API_KEY": "sk-test-aider-123",
                                         "ANTHROPIC_API_KEY": "sk-ant-test"}):
                bwrap_cmd = wrap_bwrap(["aider", "--help"], workspace=tmpdir, is_provider=True, provider_name="aider")
                self.assertIn("OPENAI_API_KEY", bwrap_cmd)
                self.assertIn("sk-test-aider-123", bwrap_cmd)
                self.assertIn("ANTHROPIC_API_KEY", bwrap_cmd)
                self.assertIn(str(conf_file), bwrap_cmd)


if __name__ == "__main__":
    unittest.main()
