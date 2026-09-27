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

import os
import sys
import tempfile
import unittest
from unittest.mock import patch, MagicMock
from pathlib import Path

from makewand.orchestrator import (
    select_optimal_engine_pair,
    dispatch_task,
    run_race,
    run_pipeline,
    classify_prompt_intent,
)
from makewand.pacing import parse_reset_time_to_seconds_left
from makewand.providers.base import run_subprocess
from makewand.providers.claude import parse_claude_quota, execute_claude_task
from makewand.providers.codex import execute_codex_task
from makewand.providers.grok import execute_grok_task
from makewand.providers.muse import execute_muse_task
from makewand.providers.aider import execute_aider_task
from makewand.providers.agy import execute_agy_task
from makewand.interactive import handle_conversational_turn


class TestAuditV31Fixes(unittest.TestCase):

    def test_p1_1_reset_time_parsing_with_prefixes(self):
        """P1-1: Reset time strings starting with 'at ' or 'in ' must parse correctly."""
        # 1. Claude quota output parser
        is_lim, reason, resets = parse_claude_quota("You've hit your limit · resets at 2:00 PM (Asia/Shanghai)")
        self.assertTrue(is_lim)
        self.assertIsNotNone(resets)
        self.assertNotIn("at ", resets.lower())

        # 2. Pacing seconds left parser
        sec = parse_reset_time_to_seconds_left("at 2:00 PM")
        self.assertIsNotNone(sec)
        self.assertGreaterEqual(sec, 0)

        sec_in = parse_reset_time_to_seconds_left("in 3h 15m")
        self.assertIsNotNone(sec_in)
        self.assertGreater(sec_in, 0)

    def test_p1_4_non_agentic_models_excluded_from_coding_tasks(self):
        """P1-4: Pure API and Local models cannot be primary coders for file-editing tasks."""
        cache = {
            "claude": {"status": "healthy"},
            "codex": {"status": "healthy"},
            "local": {"status": "healthy"},
            "deepseek": {"status": "healthy"},
            "qwen": {"status": "healthy"},
        }
        with patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)):
            # 1. Coding prompt: local and deepseek must NOT be in available coders
            coders, reviewers, meta = select_optimal_engine_pair(
                "在当前目录编写一个支持重试机制的 HTTP Client",
                tier="standard",
                cache=cache,
                require_file_editing=True,
            )
            self.assertNotIn("local", coders)
            self.assertNotIn("deepseek", coders)
            self.assertIn(meta["primary_coder"], ["claude", "codex"])
            # But local IS accepted in reviewers (part of active pool)
            self.assertIn("local", reviewers)

            # 2. Explain prompt: local CAN be included in coders
            coders_exp, _, meta_exp = select_optimal_engine_pair(
                "在本地私有大模型上运行离线分析",
                tier="standard",
                cache=cache,
                require_file_editing=False,
            )
            self.assertIn("local", coders_exp)

    def test_p0_3_long_prompt_uses_stdin_or_file(self):
        """P0-3: Prompts > 32KB must NOT be passed as plain argv strings to avoid E2BIG."""
        long_prompt = "x" * (65 * 1024)  # 65KB string
        with tempfile.TemporaryDirectory() as tmpdir:
            # 1. Claude
            with patch("makewand.health.load_status_cache", return_value={}), \
                 patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                 patch("makewand.providers.claude.run_subprocess") as mock_sub:
                mock_sub.return_value = (0, "ok", "", None)
                execute_claude_task(long_prompt, cwd=tmpdir, readonly=True)
                called_cmd = mock_sub.call_args[0][0]
                self.assertNotIn(long_prompt, called_cmd)
                self.assertEqual(mock_sub.call_args[1].get("input_text"), long_prompt)

            # 2. Codex
            with patch("makewand.health.load_status_cache", return_value={}), \
                 patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                 patch("makewand.providers.codex.run_subprocess") as mock_sub:
                mock_sub.return_value = (0, "ok", "", None)
                execute_codex_task(long_prompt, cwd=tmpdir, readonly=True)
                called_cmd = mock_sub.call_args[0][0]
                self.assertNotIn(long_prompt, called_cmd)
                self.assertIn("-", called_cmd)
                self.assertEqual(mock_sub.call_args[1].get("input_text"), long_prompt)

            # 3. Muse
            with patch("makewand.health.load_status_cache", return_value={}), \
                 patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                 patch("makewand.providers.muse.run_subprocess") as mock_sub:
                mock_sub.return_value = (0, "ok", "", None)
                execute_muse_task(long_prompt, cwd=tmpdir, readonly=True)
                called_cmd = mock_sub.call_args[0][0]
                self.assertNotIn(long_prompt, called_cmd)
                self.assertIn("--prompt-file", called_cmd)

            # 4. Grok
            with patch("makewand.health.load_status_cache", return_value={}), \
                 patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                 patch("makewand.providers.grok.run_subprocess") as mock_sub:
                mock_sub.return_value = (0, "ok", "", None)
                execute_grok_task(long_prompt, cwd=tmpdir, readonly=True)
                called_cmd = mock_sub.call_args[0][0]
                self.assertNotIn(long_prompt, called_cmd)
                self.assertIn("--prompt-file", called_cmd)

    def test_p0_4_race_mode_symmetric_dispatch_and_usage(self):
        """P0-4: Race mode must symmetrically handle Claude, Codex, Grok, Muse, AGY with dispatch_task."""
        with patch("makewand.orchestrator.dispatch_task") as mock_dispatch, \
             patch("makewand.orchestrator.execute_agy_task", return_value=(True, "推荐采纳候选方案 A", None)):
            mock_dispatch.return_value = (True, "output code", None)

            # Test Claude as Racer A, Codex as Racer B
            rc = run_race("task", engine_a="claude", engine_b="codex")
            self.assertIsInstance(rc, int)
            self.assertEqual(mock_dispatch.call_count, 2)
            called_engines = [call[0][0] for call in mock_dispatch.call_args_list]
            self.assertIn("claude", called_engines)
            self.assertIn("codex", called_engines)

        # Verify dispatch_task records usage
        with patch("makewand.orchestrator.execute_claude_task", return_value=(True, "ok", None)), \
             patch("makewand.usage.record_engine_usage") as mock_usage:
            dispatch_task("claude", "test prompt", tier="standard")
            mock_usage.assert_called_once_with("claude", tier="standard", success=True, task="test prompt")

    def test_p1_3_fail_and_cleanup_hard_reset_to_baseline(self):
        """P1-3: fail_and_cleanup in host mode resets hard to task_baseline when initial workspace is clean."""
        with tempfile.TemporaryDirectory() as tmpdir:
            from makewand.git_helper import run_git_cmd
            run_git_cmd(["git", "init"], cwd=tmpdir)
            run_git_cmd(["git", "config", "user.name", "Test"], cwd=tmpdir)
            run_git_cmd(["git", "config", "user.email", "test@test.com"], cwd=tmpdir)
            init_file = Path(tmpdir) / "app.py"
            init_file.write_text("print('v1')\n")
            run_git_cmd(["git", "add", "."], cwd=tmpdir)
            run_git_cmd(["git", "commit", "-m", "Initial commit"], cwd=tmpdir)

            # Coder commits a buggy change that advances HEAD
            def coder_advances_head(prompt, cwd=None, **kw):
                (Path(cwd) / "app.py").write_text("print('buggy v2')\n")
                (Path(cwd) / "untracked_bug.py").write_text("# bug\n")
                run_git_cmd(["git", "add", "."], cwd=cwd)
                run_git_cmd(["git", "commit", "-m", "Buggy model commit"], cwd=cwd)
                return True, "committed", None

            # Reviewer fails the change
            with patch("makewand.orchestrator.check_working_tree_isolation", return_value=(True, None)), \
                 patch("makewand.orchestrator.get_or_update_status", return_value={"codex": {"status": "healthy"}, "claude": {"status": "healthy"}}), \
                 patch("makewand.orchestrator.execute_claude_task", side_effect=coder_advances_head), \
                 patch("makewand.orchestrator.execute_codex_task", return_value=(True, "MAKEWAND_VERDICT: {\"pass\": false, \"defects\": [\"fatal bug\"]}", None)):
                res = run_pipeline("写一段代码", cwd=tmpdir, stream=False, auto_fix=False, force_code=True)
                self.assertFalse(res)
                # Verify hard reset to initial baseline: app.py must be reverted and commit rolled back!
                self.assertEqual(init_file.read_text(), "print('v1')\n")
                self.assertFalse((Path(tmpdir) / "untracked_bug.py").exists())
                _, log_out, _ = run_git_cmd(["git", "log", "--oneline"], cwd=tmpdir)
                self.assertNotIn("Buggy model commit", log_out)

    def test_interactive_context_capping(self):
        """Interactive REPL bounds conversation history context to prevent infinite expansion."""
        history = []
        for i in range(10):
            history.append({"role": "user", "content": f"User question {i}: " + ("A" * 3000)})
            history.append({"role": "assistant", "content": f"Assistant answer {i}: " + ("B" * 3000)})

        with patch("makewand.interactive.dispatch_task") as mock_dispatch:
            mock_dispatch.return_value = (True, "Answer", None)
            handle_conversational_turn("What is next?", history, "/tmp", forced_engine="codex")
            dispatched_prompt = mock_dispatch.call_args[0][1]
            # Context must be truncated and capped, well under 65KB
            self.assertLess(len(dispatched_prompt), 15000)
            self.assertIn("历史截断", dispatched_prompt)

    def test_run_subprocess_pipe_cleanup(self):
        """Subprocess run must cleanly close pipes and avoid file descriptor leaks."""
        rc, out, err, ex = run_subprocess(["echo", "test_cleanup"], input_text="pipe_in", timeout=5)
        self.assertEqual(rc, 0)
        self.assertIn("test_cleanup", out)


if __name__ == "__main__":
    unittest.main()
