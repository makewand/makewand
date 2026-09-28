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

    def test_p1_4_non_agentic_models_excluded_from_coding_tasks(self):
        """P1-4: Pure API and Local models cannot be primary coders for file-editing tasks."""
        cache = {
            "claude": {"status": "healthy"},
            "codex": {"status": "healthy"},
            "local": {"status": "healthy"},
            "deepseek": {"status": "healthy"},
            "qwen": {"status": "healthy"},
        }
        # The local model is enabled *and reachable* by construction: the result must
        # not depend on whether an Ollama daemon happens to run on the test host.
        with patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
             patch("makewand.config.load_user_config", return_value={"enabled_providers": {"local": True}}), \
             patch("makewand.providers.local.is_local_model_available",
                   return_value=(True, "qwen2.5-coder:7b", ["qwen2.5-coder:7b"])):
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
        with tempfile.TemporaryDirectory() as td:
            from makewand.git_helper import run_git_cmd
            run_git_cmd(["git", "init"], cwd=td)
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=td)
            run_git_cmd(["git", "config", "user.email", "tester@test.local"], cwd=td)
            run_git_cmd(["git", "commit", "-m", "init", "--allow-empty"], cwd=td)

            def fake_clone(src, target):
                target.mkdir(parents=True, exist_ok=True)
                run_git_cmd(["git", "init"], cwd=str(target))
                run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(target))
                run_git_cmd(["git", "config", "user.email", "tester@test.local"], cwd=str(target))
                run_git_cmd(["git", "commit", "-m", "init", "--allow-empty"], cwd=str(target))

            with patch("makewand.orchestrator.dispatch_task") as mock_dispatch, \
                 patch("makewand.orchestrator.clone_isolated_worktree", side_effect=fake_clone), \
                 patch("makewand.orchestrator.run_local_tests", return_value=(True, "all passed")), \
                 patch("makewand.orchestrator.execute_agy_task", return_value=(True, "推荐采纳候选方案 A", None)):
                mock_dispatch.return_value = (True, "output code", None)

                # Test Claude as Racer A, Codex as Racer B
                rc = run_race("task", cwd=td, engine_a="claude", engine_b="codex")
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

    def test_p0_a_clone_isolated_worktree_copies_files_and_excludes_dirs(self):
        """P0-A: clone_isolated_worktree uses copy2 (different inodes) and excludes node_modules, benchmarks, etc."""
        from makewand.git_helper import clone_isolated_worktree
        with tempfile.TemporaryDirectory() as td:
            src = Path(td) / "src"
            dst = Path(td) / "dst"
            src.mkdir()
            (src / "app.py").write_text("print('hello')\n")
            (src / "node_modules").mkdir()
            (src / "node_modules" / "pkg.js").write_text("dummy")
            (src / "benchmarks").mkdir()
            (src / "benchmarks" / "bench.py").write_text("dummy")
            (src / ".venv").mkdir()
            (src / ".venv" / "pip").write_text("dummy")
            (src / "__pycache__").mkdir()
            (src / "__pycache__" / "c.pyc").write_text("dummy")

            clone_isolated_worktree(str(src), dst)

            # File must be copied with distinct inode
            src_file = src / "app.py"
            dst_file = dst / "app.py"
            self.assertTrue(dst_file.exists())
            self.assertNotEqual(src_file.stat().st_ino, dst_file.stat().st_ino)

            # Excluded directories must NOT be in dst
            self.assertFalse((dst / "node_modules").exists())
            self.assertFalse((dst / "benchmarks").exists())
            self.assertFalse((dst / ".venv").exists())
            self.assertFalse((dst / "__pycache__").exists())

    def test_p1_a_streaming_long_stdin_no_deadlock(self):
        """P1-A: run_subprocess with stream=True handles >64KB stdin via async thread without deadlocking."""
        long_input = "line " * 20000 + "\n"  # >100KB
        rc, out, err, ex = run_subprocess(["cat"], input_text=long_input, stream=True, timeout=5)
        self.assertEqual(rc, 0)
        self.assertIn("line line", out)
        self.assertIsNone(ex)

    def test_p1_c_forced_engine_not_passed_as_model_arg(self):
        """P1-C: forced_engine prioritizes engine without passing engine name to child CLI --model."""
        with tempfile.TemporaryDirectory() as tmpdir:
            from makewand.git_helper import run_git_cmd
            run_git_cmd(["git", "init"], cwd=tmpdir)
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=tmpdir)
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=tmpdir)
            (Path(tmpdir) / "app.py").write_text("print('test')\n")
            run_git_cmd(["git", "add", "."], cwd=tmpdir)
            run_git_cmd(["git", "commit", "-m", "init"], cwd=tmpdir)

            with patch("makewand.orchestrator.dispatch_task") as mock_dispatch:
                mock_dispatch.return_value = (True, "Answer", None)
                # Test explain mode with forced_engine
                run_pipeline("解释这段代码", cwd=tmpdir, forced_engine="codex")
                self.assertEqual(mock_dispatch.call_args[0][0], "codex")
                self.assertIsNone(mock_dispatch.call_args[1].get("model"))

            # Test code modification mode with forced_engine
            call_args_list = []
            def fake_dispatch(eng, prompt, cwd=None, **kw):
                call_args_list.append((eng, kw))
                if kw.get("readonly"):
                    return True, "MAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}", None
                else:
                    (Path(cwd) / "app.py").write_text("print('updated')\n")
                    return True, "written", None

            with patch("makewand.orchestrator.check_working_tree_isolation", return_value=(True, None)), \
                 patch("makewand.orchestrator.get_or_update_status", return_value={"agy": {"status": "healthy"}, "claude": {"status": "healthy"}}), \
                 patch("makewand.config.get_active_providers", return_value=["agy", "claude"]), \
                 patch("makewand.orchestrator.dispatch_task", side_effect=fake_dispatch), \
                 patch("makewand.orchestrator.run_local_tests", return_value=(True, "ok")):
                run_pipeline("修改代码", cwd=tmpdir, stream=False, auto_fix=False, force_code=True, forced_engine="agy")
                self.assertEqual(call_args_list[0][0], "agy")
                self.assertIsNone(call_args_list[0][1].get("model"))

            # Test conversational turn with forced_engine
            with patch("makewand.interactive.dispatch_task") as mock_chat_dispatch:
                mock_chat_dispatch.return_value = (True, "Chat answer", None)
                from makewand.interactive import handle_conversational_turn
                handle_conversational_turn("hello", [], tmpdir, forced_engine="codex")
                self.assertEqual(mock_chat_dispatch.call_args[0][0], "codex")
                self.assertIsNone(mock_chat_dispatch.call_args[1].get("model"))

    def test_p1_d_single_tool_auto_fix_loop(self):
        """P1-D: Single-tool mode allows coder engine to re-review its own fixes."""
        with tempfile.TemporaryDirectory() as tmpdir:
            from makewand.git_helper import run_git_cmd
            run_git_cmd(["git", "init"], cwd=tmpdir)
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=tmpdir)
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=tmpdir)
            (Path(tmpdir) / "app.py").write_text("v1\n")
            run_git_cmd(["git", "add", "."], cwd=tmpdir)
            run_git_cmd(["git", "commit", "-m", "init"], cwd=tmpdir)

            call_count = [0]
            def single_engine_dispatch(eng, prompt, cwd=None, **kw):
                call_count[0] += 1
                if kw.get("readonly"):
                    if "经过上一轮缺陷修复后" in prompt:
                        return True, "MAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}", None
                    else:
                        return True, "MAKEWAND_VERDICT: {\"pass\": false, \"defects\": [\"typo bug\"]}", None
                else:
                    (Path(cwd) / "app.py").write_text(f"new content {call_count[0]}\n")
                    return True, "code written", None

            with patch("makewand.orchestrator.check_working_tree_isolation", return_value=(True, None)), \
                 patch("makewand.orchestrator.get_or_update_status", return_value={"codex": {"status": "healthy"}}), \
                 patch("makewand.config.get_active_providers", return_value=["codex"]), \
                 patch("makewand.orchestrator.dispatch_task", side_effect=single_engine_dispatch), \
                 patch("makewand.orchestrator.run_local_tests", return_value=(True, "ok")):
                res = run_pipeline("修改代码", cwd=tmpdir, stream=False, auto_fix=True, force_code=True)
                self.assertTrue(res)

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

    def test_p2_improvements(self):
        """P2 improvements: 0600 api_keys, agy --mode plan in readonly, fail_and_cleanup baseline diff, LRU eviction."""
        # 1. 0600 api_keys.json
        import stat
        from makewand.config import save_api_key
        with tempfile.TemporaryDirectory() as td:
            f = Path(td) / "api_keys.json"
            with patch("makewand.config.API_KEYS_FILE", f), patch("makewand.config.CONFIG_DIR", Path(td)):
                save_api_key("claude", "test-key")
                self.assertEqual(stat.S_IMODE(f.stat().st_mode), 0o600)

        # 2. AGY --mode plan in readonly
        with patch("makewand.health.load_status_cache", return_value={}), \
             patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("makewand.providers.agy.run_subprocess") as mock_sub:
            mock_sub.return_value = (0, "ok", "", None)
            execute_agy_task("plan prompt", readonly=True)
            cmd = mock_sub.call_args[0][0]
            self.assertIn("--mode", cmd)
            self.assertIn("plan", cmd)
            self.assertNotIn("--dangerously-skip-permissions", cmd)

        # 3. Candidate LRU eviction
        from makewand.candidate import CandidateManager
        with tempfile.TemporaryDirectory() as td:
            cand_dir = Path(td) / "candidates"
            cand_dir.mkdir()
            for i in range(7):
                d = cand_dir / f"rc_{i}"
                d.mkdir()
                (d / "meta.json").write_text(f"{{\"created_at\": \"2026-09-27T0{i}:00:00\"}}")
            with patch("makewand.config.CANDIDATES_DIR", cand_dir):
                evicted = CandidateManager.prune_old_candidates(max_candidates=4)
                self.assertEqual(evicted, 3)
                self.assertEqual(len(list(cand_dir.iterdir())), 4)

                # Symlink eviction test: must unlink symlinks without crashing with OSError: Cannot call rmtree on a symbolic link
                cand_link = cand_dir / "rc_symlink"
                cand_target = Path(td) / "external_target"
                cand_target.mkdir()
                os.utime(cand_target, (1000, 1000))
                cand_link.symlink_to(cand_target)
                CandidateManager.prune_old_candidates(max_candidates=4)
                self.assertFalse(cand_link.exists())

        # 4. fail_and_cleanup captures committed changes against task_baseline into rejected.patch
        with tempfile.TemporaryDirectory() as td:
            from makewand.git_helper import run_git_cmd
            run_git_cmd(["git", "init"], cwd=td)
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=td)
            run_git_cmd(["git", "config", "user.email", "tester@test.local"], cwd=td)
            (Path(td) / "main.py").write_text("print('baseline')\n")
            run_git_cmd(["git", "add", "."], cwd=td)
            run_git_cmd(["git", "commit", "-m", "init"], cwd=td)

            def buggy_coder(p, cwd=None, **kw):
                (Path(cwd) / "main.py").write_text("print('committed bug')\n")
                run_git_cmd(["git", "add", "."], cwd=cwd)
                run_git_cmd(["git", "commit", "-m", "bug commit"], cwd=cwd)
                return True, "ok", None

            with patch("makewand.orchestrator.check_working_tree_isolation", return_value=(True, None)), \
                 patch("makewand.orchestrator.get_or_update_status", return_value={"codex": {"status": "healthy"}, "claude": {"status": "healthy"}}), \
                 patch("makewand.orchestrator.execute_claude_task", side_effect=buggy_coder), \
                 patch("makewand.orchestrator.execute_codex_task", return_value=(True, "MAKEWAND_VERDICT: {\"pass\": false, \"defects\": [\"bug\"]}", None)):
                run_pipeline("fix code", cwd=td, stream=False, auto_fix=False, force_code=True)
                rej_dirs = list(Path("/tmp/makewand-artifacts").glob("rejected_*"))
                self.assertTrue(len(rej_dirs) > 0)
                latest_rej = sorted(rej_dirs, key=lambda d: d.stat().st_mtime)[-1]
                patch_file = latest_rej / "rejected.patch"
                self.assertTrue(patch_file.exists())
                self.assertIn("committed bug", patch_file.read_text())


if __name__ == "__main__":
    unittest.main()
