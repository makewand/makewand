"""
Comprehensive unit tests for the multi-model joint orchestration optimizations:
1. Immunity to prompt keyword contamination in provider sandbox deduction
2. Strict fail-closed review quality gates (no falsified LGTM)
3. Multi-stack composite test runner (Python, Go, Node, Rust)
4. Candidate manager concurrency file lock & atomic file replacement
5. Untrusted repository policy enforcement across all providers and orchestrators
"""

import fcntl
import json
import os
import shutil
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch, MagicMock

# Ensure repo root is on sys.path
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from makewand.sandbox import wrap_bwrap, is_bwrap_available, run_in_sandbox
from makewand.orchestrator import run_local_tests, dispatch_task, run_pipeline, run_review, run_race
from makewand.candidate import CandidateManager
from makewand.providers.claude import execute_claude_task
from makewand.providers.codex import execute_codex_task
from makewand.providers.agy import execute_agy_task
from makewand.providers.muse import execute_muse_task
from makewand.providers.grok import execute_grok_task, parse_grok_quota


class TestProviderPromptContaminationImmunity(unittest.TestCase):
    """
    Validates that user prompts containing provider names (e.g. 'muse', 'claude', 'codex')
    do not contaminate sandbox provider deduction or namespace isolation.
    """

    @patch("makewand.sandbox.is_bwrap_available", return_value=True)
    def test_prompt_keywords_do_not_falsely_identify_muse(self, mock_bwrap):
        # A non-provider command whose arguments mention 'muse' should NOT be treated as Muse.
        # It must retain --unshare-pid.
        cmd = ["python3", "-c", "print('analyzing muse code and claude adapters')"]
        bwrap_cmd = wrap_bwrap(cmd, workspace="/tmp", is_provider=False)
        self.assertIn("--unshare-pid", bwrap_cmd)

    @patch("makewand.sandbox.is_bwrap_available", return_value=True)
    def test_provider_name_strict_isolation_masks_other_credentials(self, mock_bwrap):
        user_home = str(Path.home())
        # When provider_name="claude", claude dirs are mounted, but codex/gemini dirs must be masked with tmpfs
        cmd = ["claude", "-p", "review muse architecture"]
        bwrap_cmd = wrap_bwrap(cmd, workspace="/tmp", is_provider=True, provider_name="claude")

        # Claude auth dir should be ro-bound
        claude_config = os.path.join(user_home, ".claude")
        self.assertIn(claude_config, bwrap_cmd)

        # Other providers' auth dirs MUST NOT be mounted (isolated by HOME tmpfs)
        codex_config = os.path.join(user_home, ".codex")
        self.assertIn("--tmpfs", bwrap_cmd)
        self.assertIn(user_home, bwrap_cmd)
        self.assertNotIn(f"--bind {codex_config} {codex_config}", " ".join(bwrap_cmd))
        self.assertNotIn(f"--ro-bind {codex_config} {codex_config}", " ".join(bwrap_cmd))


class TestFailClosedReviewQualityGate(unittest.TestCase):
    """
    Validates that crashed or empty review responses never pass the quality gate
    and are never fabricated into LGTM.
    """

    @patch("makewand.orchestrator.check_working_tree_isolation", return_value=(True, None))
    @patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None))
    @patch("makewand.orchestrator.get_or_update_status", return_value={"codex": {"status": "healthy"}, "claude": {"status": "healthy"}, "agy": {"status": "healthy"}, "muse": {"status": "healthy"}, "grok": {"status": "healthy"}, "local": {"status": "healthy"}})
    @patch("makewand.orchestrator.execute_claude_task", return_value=(True, "code written", None))
    @patch("makewand.orchestrator.execute_codex_task", return_value=(False, "", "Model timeout"))
    @patch("makewand.orchestrator.execute_grok_task", return_value=(False, "", "Model timeout"))
    @patch("makewand.orchestrator.execute_agy_task", return_value=(False, "", "Model timeout"))
    @patch("makewand.orchestrator.execute_muse_task", return_value=(False, "", "Model timeout"))
    @patch("makewand.orchestrator.execute_local_task", return_value=(False, "", "Model timeout"))
    @patch("makewand.orchestrator.get_git_diff", return_value="diff --git a/a.py b/a.py\n+val = 1")
    @patch("makewand.orchestrator.run_local_tests", return_value=(True, "Tests passed"))
    def test_empty_review_fails_closed_without_fake_lgtm(self, mock_test, mock_diff, mock_local, mock_muse, mock_agy, mock_grok, mock_codex, mock_claude, mock_status, mock_burn, mock_iso):

        with tempfile.TemporaryDirectory() as tmp_dir:
            res = run_pipeline("实现测试任务", cwd=tmp_dir, stream=False, auto_fix=False)
            # Must FAIL closed, never deliver
            self.assertFalse(res)


class TestMultiFrameworkCompositeTestGate(unittest.TestCase):
    """
    Validates that run_local_tests detects multiple stacks (e.g. Python + Go/Node)
    and enforces that all detected suites must pass.
    """

    def test_composite_python_and_go_both_executed(self):
        with tempfile.TemporaryDirectory() as tmp_dir:
            p = Path(tmp_dir)
            # Create Python test marker and Go test marker
            (p / "test_main.py").write_text("import unittest\nclass T(unittest.TestCase):\n    def test_ok(self): pass\n")
            (p / "go.mod").write_text("module example.com/test\ngo 1.22\n")

            with patch("shutil.which", side_effect=lambda x: "/bin/" + x), \
                 patch("makewand.sandbox.run_in_sandbox") as mock_sandbox:
                # First suite (Python) succeeds, second suite (Go) fails
                mock_sandbox.side_effect = [
                    (0, "Python test pass", "", None),
                    (1, "", "Go compile error", None)
                ]
                passed, details = run_local_tests(tmp_dir)
                self.assertFalse(passed)
                self.assertIn("Go Tests Failed", details)
                self.assertEqual(mock_sandbox.call_count, 2)


class TestCandidateLockingAndAtomicReplace(unittest.TestCase):
    """
    Validates that apply_candidate uses concurrency file lock (flock)
    and atomic file replacement to prevent TOCTOU races.
    """

    def test_apply_candidate_atomic_file_write(self):
        import makewand.config as config
        orig_config_dir = config.CONFIG_DIR
        orig_cand_dir = config.CANDIDATES_DIR
        orig_backups_dir = config.BACKUPS_DIR

        with tempfile.TemporaryDirectory() as tmp_dir:
            try:
                config.CONFIG_DIR = Path(tmp_dir) / "config"
                config.CANDIDATES_DIR = config.CONFIG_DIR / "candidates"
                config.BACKUPS_DIR = config.CONFIG_DIR / "backups"
                config.ensure_config_dir()

                target_ws = Path(tmp_dir) / "workspace"
                target_ws.mkdir(parents=True)
                target_file = target_ws / "foo.txt"
                target_file.write_text("initial content")

                race_id = "rc_test"
                cand_dir = config.CANDIDATES_DIR / race_id / "agent_a"
                cand_dir.mkdir(parents=True)
                src_file = cand_dir / "foo.txt"
                src_file.write_text("candidate content")

                CandidateManager.save_race(
                    race_id=race_id,
                    prompt="test race",
                    base_cwd=str(target_ws),
                    baseline_commit="HEAD",
                    agent_a={"name": "Agent A", "path": str(cand_dir), "success": True, "duration": 1.0, "diff_size": 10},
                    agent_b={"name": "Agent B", "path": str(cand_dir), "success": True, "duration": 1.0, "diff_size": 10},
                    winner="A"
                )

                with patch("makewand.candidate.get_candidate_files_changed", return_value={"foo.txt": "M"}), \
                     patch("makewand.candidate.run_git_cmd", return_value=(0, "", "")):
                    ok, applied, msg = CandidateManager.apply_candidate(
                        race_id=race_id,
                        candidate_label="A",
                        force=True
                    )
                    self.assertTrue(ok, f"apply_candidate failed: {msg}")
                    self.assertEqual(target_file.read_text(), "candidate content")
                    # Verify lock file was created in config dir
                    self.assertTrue((config.CONFIG_DIR / "apply.lock").exists())
            finally:
                config.CONFIG_DIR = orig_config_dir
                config.CANDIDATES_DIR = orig_cand_dir
                config.BACKUPS_DIR = orig_backups_dir


class TestUntrustedRepoSandboxEnforcement(unittest.TestCase):
    """
    Validates that untrusted repository trust level strictly enforces Bubblewrap sandbox,
    blocks network access, and prevents writable operations.
    """

    @patch("makewand.sandbox.is_bwrap_available", return_value=False)
    def test_untrusted_repo_fails_closed_in_providers(self, mock_bwrap):
        # Untrusted repo must refuse execution if bwrap is missing
        ok, out, err = execute_claude_task("test prompt", cwd="/tmp", repo_trust="untrusted")
        self.assertFalse(ok)
        self.assertIn("untrusted", err)

        ok, out, err = execute_codex_task("test prompt", cwd="/tmp", repo_trust="untrusted")
        self.assertFalse(ok)
        self.assertIn("untrusted", err)

        ok, out, err = execute_agy_task("test prompt", cwd="/tmp", repo_trust="untrusted")
        self.assertFalse(ok)
        self.assertIn("untrusted", err)

        ok, out, err = execute_muse_task("test prompt", cwd="/tmp", repo_trust="untrusted")
        self.assertFalse(ok)
        self.assertIn("untrusted", err)

        ok, out, err = execute_grok_task("test prompt", cwd="/tmp", repo_trust="untrusted")
        self.assertFalse(ok)
        self.assertIn("untrusted", err)

    @patch("makewand.sandbox.is_bwrap_available", return_value=True)
    def test_untrusted_repo_blocks_write_tasks(self, mock_bwrap):
        # Even with bwrap available, untrusted repo must reject writable operations
        ok, out, err = execute_claude_task("write code", cwd="/tmp", readonly=False, repo_trust="untrusted")
        self.assertFalse(ok)
        self.assertIn("只读", err)

        ok, out, err = execute_grok_task("write code", cwd="/tmp", readonly=False, repo_trust="untrusted")
        self.assertFalse(ok)
        self.assertIn("只读", err)

    @patch("makewand.sandbox.is_bwrap_available", return_value=False)
    def test_untrusted_repo_fails_closed_in_orchestrator(self, mock_bwrap):
        # run_pipeline in untrusted repo without bwrap must fail-closed immediately
        res = run_pipeline("test task", cwd="/tmp", repo_trust="untrusted")
        self.assertFalse(res)

        # run_review in untrusted repo without bwrap must return EXIT_UNVERIFIED (11)
        exit_code = run_review(cwd="/tmp", repo_trust="untrusted", output_json=True)
        self.assertEqual(exit_code, 11)


class TestGrokProviderIntegration(unittest.TestCase):
    """
    Validates complete Grok Build CLI (xAI) integration:
    1. CLI argument construction (tiers, reasoning-effort, permission-mode, headless format)
    2. Sandbox isolation (mounting ~/.grok, masking other providers)
    3. Status probe and quota parsing
    4. Model discovery and dynamic configuration
    5. Orchestrator affinity scoring
    """

    @patch("shutil.which", return_value="/home/user/.local/bin/grok")
    @patch("makewand.sandbox.is_bwrap_available", return_value=True)
    @patch("makewand.providers.grok.run_subprocess")
    def test_grok_cli_argument_construction(self, mock_run, mock_bwrap, mock_which):
        mock_run.return_value = (0, "Grok response", "", None)

        # 1. Fast tier: grok-4.7-build-fast, effort low, readonly=True -> plan mode
        ok, out, err = execute_grok_task("analyze problem", cwd="/tmp", tier="fast", readonly=True)
        self.assertTrue(ok)
        cmd_args = mock_run.call_args[0][0]
        self.assertIn("--model", cmd_args)
        self.assertEqual(cmd_args[cmd_args.index("--model") + 1], "grok-4.7-build-fast")
        self.assertIn("--reasoning-effort", cmd_args)
        self.assertEqual(cmd_args[cmd_args.index("--reasoning-effort") + 1], "low")
        self.assertIn("--permission-mode", cmd_args)
        self.assertEqual(cmd_args[cmd_args.index("--permission-mode") + 1], "plan")
        self.assertIn("--output-format", cmd_args)
        self.assertEqual(cmd_args[cmd_args.index("--output-format") + 1], "plain")

        # 2. Deep tier: grok-4.7, effort high, writable -> always-approve and bypassPermissions
        ok, out, err = execute_grok_task("write complex module", cwd="/tmp", tier="deep", readonly=False)
        self.assertTrue(ok)
        cmd_args = mock_run.call_args[0][0]
        self.assertIn("--model", cmd_args)
        self.assertEqual(cmd_args[cmd_args.index("--model") + 1], "grok-4.7")
        self.assertIn("--reasoning-effort", cmd_args)
        self.assertEqual(cmd_args[cmd_args.index("--reasoning-effort") + 1], "high")
        self.assertIn("--always-approve", cmd_args)
        self.assertIn("--permission-mode", cmd_args)
        self.assertEqual(cmd_args[cmd_args.index("--permission-mode") + 1], "bypassPermissions")

    @patch("makewand.sandbox.is_bwrap_available", return_value=True)
    def test_grok_sandbox_isolation_credentials(self, mock_bwrap):
        user_home = str(Path.home())
        # Grok as active provider mounts ~/.grok, and does NOT mount ~/.claude or ~/.codex
        cmd = ["grok", "-p", "hello"]
        bwrap_cmd = wrap_bwrap(cmd, workspace="/tmp", is_provider=True, provider_name="grok")
        grok_dir = os.path.join(user_home, ".grok")
        claude_dir = os.path.join(user_home, ".claude")
        codex_dir = os.path.join(user_home, ".codex")

        self.assertIn(grok_dir, bwrap_cmd)
        self.assertNotIn(f"--bind {claude_dir} {claude_dir}", " ".join(bwrap_cmd))
        self.assertNotIn(f"--bind {codex_dir} {codex_dir}", " ".join(bwrap_cmd))

        # Other provider (e.g. claude) mounts ~/.claude, and does NOT mount ~/.grok
        bwrap_cmd_claude = wrap_bwrap(["claude"], workspace="/tmp", is_provider=True, provider_name="claude")
        self.assertNotIn(f"--bind {grok_dir} {grok_dir}", " ".join(bwrap_cmd_claude))
        self.assertNotIn(f"--ro-bind {grok_dir} {grok_dir}", " ".join(bwrap_cmd_claude))

    def test_grok_quota_parsing(self):
        # Healthy output
        is_lim, reason, resets = parse_grok_quota("All models ready. Quota healthy.")
        self.assertFalse(is_lim)

        # Rate limited
        is_lim, reason, resets = parse_grok_quota("Rate limit reached: resets in 45m")
        self.assertTrue(is_lim)
        self.assertIn("429", reason)

        # Missing auth
        is_lim, reason, resets = parse_grok_quota("Please sign in with grok auth")
        self.assertTrue(is_lim)
        self.assertIn("登录", reason)

    def test_grok_discovery_matrix(self):
        from makewand.discovery import discover_available_models
        models = discover_available_models()
        self.assertIn("grok", models)
        self.assertEqual(models["grok"]["current_default"], "grok-4.7")
        self.assertIn("grok-4.7", models["grok"]["available"])

    def test_grok_orchestrator_affinity(self):
        from makewand.orchestrator import select_optimal_engine_pair
        cache = {
            "claude": {"status": "healthy"},
            "codex": {"status": "healthy"},
            "grok": {"status": "healthy"},
            "agy": {"status": "healthy"},
            "muse": {"status": "healthy"}
        }
        with patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)):
            coders, reviewers, meta = select_optimal_engine_pair("使用 grok 进行逻辑推演和快速原型设计", cache=cache)
            # Grok keywords should boost grok score significantly
            self.assertGreater(meta["scores"]["grok"], 3.0)
            self.assertIn("Grok", " ".join(meta["reasons"]))


class TestCatalogDrivenTierResolutionAndSandboxWhitelist(unittest.TestCase):
    """
    Tests for the newly refactored dynamic catalog-driven model resolution,
    strict tmpfs whitelist sandbox isolation, and transactional rollback.
    """

    def test_dynamic_claude_catalog_tier_resolution(self):
        from makewand.discovery import get_provider_model_tier
        
        deep_res = get_provider_model_tier("claude", "deep")
        self.assertEqual(deep_res["model"], "fable")
        self.assertEqual(deep_res["effort"], "max")
        self.assertEqual(deep_res["full_id"], "claude-fable-5-1")

        std_res = get_provider_model_tier("claude", "standard")
        self.assertEqual(std_res["model"], "sonnet")
        self.assertIn(std_res["effort"], ["medium", "high"])

        fast_res = get_provider_model_tier("claude", "fast")
        self.assertEqual(fast_res["model"], "haiku")

    def test_dynamic_codex_tier_resolution(self):
        from makewand.discovery import get_provider_model_tier
        deep_res = get_provider_model_tier("codex", "deep")
        self.assertEqual(deep_res["model"], "gpt-6-astra")
        self.assertEqual(deep_res["effort"], "max")

    def test_dynamic_grok_tier_resolution(self):
        from makewand.discovery import get_provider_model_tier
        deep_res = get_provider_model_tier("grok", "deep")
        self.assertEqual(deep_res["model"], "grok-4.7")
        self.assertEqual(deep_res["effort"], "high")

    @patch("makewand.sandbox.is_bwrap_available", return_value=True)
    def test_sandbox_strict_tmpfs_whitelist_isolation(self, mock_bwrap):
        from makewand.sandbox import wrap_bwrap
        user_home = str(Path.home())
        
        # When provider_name="claude", HOME is tmpfs, only .claude is mounted, .codex is NEVER mounted
        cmd = wrap_bwrap(["claude", "-p", "test"], "/tmp", is_provider=True, provider_name="claude")
        self.assertIn("--tmpfs", cmd)
        self.assertIn(user_home, cmd)
        claude_dir = os.path.join(user_home, ".claude")
        codex_dir = os.path.join(user_home, ".codex")
        self.assertIn(claude_dir, cmd)
        self.assertNotIn(f"--bind {codex_dir} {codex_dir}", " ".join(cmd))
        self.assertNotIn(f"--ro-bind {codex_dir} {codex_dir}", " ".join(cmd))

    def test_pipeline_transactional_rollback_on_test_failure(self):
        from makewand.git_helper import run_git_cmd
        from makewand.orchestrator import run_pipeline

        with tempfile.TemporaryDirectory() as base_tmp:
            run_git_cmd(["git", "init"], cwd=base_tmp)
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=base_tmp)
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=base_tmp)
            init_file = Path(base_tmp) / "main.py"
            init_file.write_text("print('baseline')\n")
            run_git_cmd(["git", "add", "-A"], cwd=base_tmp)
            run_git_cmd(["git", "commit", "-m", "init"], cwd=base_tmp)

            # Mock coder that modifies main.py, but mock test runner to FAIL
            def fake_coder(*args, **kwargs):
                (Path(base_tmp) / "main.py").write_text("print('broken code')\n")
                return True, "Code written", None

            def fake_test_runner(cwd, *args, **kwargs):
                return False, "Simulated unit test failure"

            with patch("makewand.orchestrator.dispatch_task", side_effect=fake_coder), \
                 patch("makewand.orchestrator.run_local_tests", side_effect=fake_test_runner), \
                 patch("makewand.orchestrator.check_working_tree_isolation", return_value=(True, None)):
                res = run_pipeline("修改代码", cwd=base_tmp, auto_fix=False)
                self.assertFalse(res)
                # Verify transactional rollback: main.py must be reverted to baseline!
                self.assertEqual(init_file.read_text(), "print('baseline')\n")

    def test_run_git_cmd_injects_security_flags(self):
        from makewand.git_helper import run_git_cmd, SAFE_GIT_SECURITY_FLAGS
        with patch("subprocess.run") as mock_sub:
            mock_sub.return_value = MagicMock(returncode=0, stdout="test", stderr="")
            run_git_cmd(["git", "diff", "HEAD"])
            called_cmd = mock_sub.call_args[0][0]
            self.assertEqual(called_cmd[0], "git")
            for flag in SAFE_GIT_SECURITY_FLAGS:
                self.assertIn(flag, called_cmd)

    def test_get_git_diff_status_fails_closed_on_error(self):
        from makewand.git_helper import get_git_diff_status
        with tempfile.TemporaryDirectory() as tmpdir:
            diff_text, err = get_git_diff_status(tmpdir)
            self.assertEqual(diff_text, "")
            self.assertIsNotNone(err)
            self.assertIn("Not inside a valid git working tree", err)

    def test_untrusted_readonly_provider_enforces_bwrap_even_without_cwd(self):
        from makewand.providers.claude import execute_claude_task
        with patch("makewand.health.load_status_cache", return_value={}), \
             patch("makewand.sandbox.is_bwrap_available", return_value=True), \
             patch("makewand.sandbox.wrap_bwrap") as mock_wrap, \
             patch("makewand.providers.claude.run_subprocess", return_value=(0, "ok", "", None)):
            mock_wrap.side_effect = lambda cmd, **kw: cmd
            ok, _, _ = execute_claude_task("test prompt", cwd=None, readonly=True, repo_trust="untrusted")
            self.assertTrue(ok)
            self.assertTrue(mock_wrap.called)

    def test_sandbox_masks_var_tmp_and_run(self):
        from makewand.sandbox import wrap_bwrap
        cmd = wrap_bwrap(["echo", "hi"], "/tmp")
        self.assertIn("/var/tmp", cmd)
        self.assertIn("/run", cmd)

    def test_run_git_cmd_injects_no_textconv_for_diff(self):
        from makewand.git_helper import run_git_cmd
        with patch("subprocess.run") as mock_sub:
            mock_sub.return_value = MagicMock(returncode=0, stdout="test", stderr="")
            run_git_cmd(["git", "diff", "HEAD"])
            called_cmd = mock_sub.call_args[0][0]
            self.assertIn("--no-ext-diff", called_cmd)
            self.assertIn("--no-textconv", called_cmd)
            self.assertIn("--attr-source=4b825dc642cb6eb9a060e54bf8d69288fbee4904", called_cmd)

    def test_cli_preserves_repo_trust_flag_at_root_and_subcommand(self):
        from makewand.cli import main
        with patch("sys.argv", ["makewand", "--repo-trust", "untrusted", "review", "--json"]):
            with patch("makewand.cli.run_review") as mock_rev:
                mock_rev.return_value = 0
                try:
                    main()
                except SystemExit:
                    pass
                self.assertEqual(mock_rev.call_args[1].get("repo_trust"), "untrusted")

        with patch("sys.argv", ["makewand", "review", "--repo-trust", "untrusted", "--json"]):
            with patch("makewand.cli.run_review") as mock_rev:
                mock_rev.return_value = 0
                try:
                    main()
                except SystemExit:
                    pass
                self.assertEqual(mock_rev.call_args[1].get("repo_trust"), "untrusted")

    def test_apply_candidate_blocks_failed_tests(self):
        from makewand.candidate import CandidateManager
        race_id = "test_race_block_fail"
        with tempfile.TemporaryDirectory() as tmp_dir:
            cand_p = Path(tmp_dir) / "wt_a"
            cand_p.mkdir(parents=True, exist_ok=True)
            (cand_p / "foo.py").write_text("x = 1\n")
            CandidateManager.save_race(
                race_id=race_id,
                prompt="test block",
                base_cwd=tmp_dir,
                baseline_commit="",
                agent_a={"path": str(cand_p), "success": True, "test_passed": False},
                agent_b={},
                winner="A"
            )
            ok, _, msg = CandidateManager.apply_candidate(race_id=race_id, candidate_label="A")
            self.assertFalse(ok)
            self.assertIn("单元测试未通过", msg)


if __name__ == "__main__":
    unittest.main()


