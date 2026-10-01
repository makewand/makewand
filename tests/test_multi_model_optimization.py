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


class _FakeProviderHome:
    """
    Temporary HOME with fake provider state directories, so sandbox assertions do
    not depend on the real ~/.claude / ~/.grok of the machine running the tests
    (eng-delivery#3) and never create placeholders in them.
    """

    def __enter__(self):
        self.home = Path(tempfile.mkdtemp(prefix="mm-fake-home-"))
        self.workspace = Path(tempfile.mkdtemp(prefix="mm-ws-"))
        for rel in (".claude/commands", ".codex", ".grok/bin", ".gemini"):
            (self.home / rel).mkdir(parents=True, exist_ok=True)
        (self.home / ".claude" / "CLAUDE.md").write_text("global\n")
        (self.home / ".claude" / "settings.json").write_text("{}\n")
        (self.home / ".grok" / "bin" / "grok").write_text("#!/bin/sh\n")
        (self.home / ".grok" / "config.toml").write_text("")
        self._env = patch.dict(os.environ, {"HOME": str(self.home)})
        self._env.start()
        return self

    def __exit__(self, *exc):
        self._env.stop()
        shutil.rmtree(self.home, ignore_errors=True)
        shutil.rmtree(self.workspace, ignore_errors=True)
        return False

    def path(self, rel):
        return str(self.home / rel)


def _pairs(cmd, flag):
    return [(cmd[i + 1], cmd[i + 2]) for i, x in enumerate(cmd[:-2]) if x == flag]


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
        with tempfile.TemporaryDirectory() as ws:
            bwrap_cmd = wrap_bwrap(cmd, workspace=ws, is_provider=False)
        self.assertIn("--unshare-pid", bwrap_cmd)

    @patch("makewand.sandbox.is_bwrap_available", return_value=True)
    def test_provider_name_strict_isolation_masks_other_credentials(self, mock_bwrap):
        with _FakeProviderHome() as fh:
            user_home = str(fh.home)
            # When provider_name="claude", claude dirs are mounted, but codex/gemini dirs must be masked with tmpfs
            cmd = ["claude", "-p", "review muse architecture"]
            bwrap_cmd = wrap_bwrap(cmd, workspace=str(fh.workspace), is_provider=True, provider_name="claude")

            # Claude state dir is mounted (writable state), its instruction/config paths read-only
            claude_config = fh.path(".claude")
            self.assertIn((claude_config, claude_config), _pairs(bwrap_cmd, "--bind"))
            ro = _pairs(bwrap_cmd, "--ro-bind")
            for rel in ("CLAUDE.md", "settings.json", "commands", "hooks", "skills", "plugins", "agents"):
                p = os.path.join(claude_config, rel)
                self.assertIn((p, p), ro, rel)

            # Other providers' auth dirs MUST NOT be mounted (isolated by HOME tmpfs)
            self.assertIn(("--tmpfs", user_home), list(zip(bwrap_cmd, bwrap_cmd[1:])))
            for other in (".codex", ".grok", ".gemini"):
                self.assertNotIn(fh.path(other), bwrap_cmd)


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

                # This non-Git fixture supplies the same plan at seal and apply.
                with patch("makewand.candidate.get_candidate_files_changed", return_value={"foo.txt": "M"}):
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

    @patch("shutil.which", return_value="/opt/grok/bin/grok")
    @patch("makewand.sandbox.is_bwrap_available", return_value=True)
    @patch("makewand.providers.grok.run_subprocess")
    def test_grok_cli_argument_construction(self, mock_run, mock_bwrap, mock_which):
        with _FakeProviderHome() as fh:
            self._grok_cli_argument_construction(mock_run, str(fh.workspace))

    def _grok_cli_argument_construction(self, mock_run, ws):
        mock_run.return_value = (0, "Grok response", "", None)

        # 1. Fast tier: grok-4.7-build-fast, effort low, readonly=True -> plan mode
        ok, out, err = execute_grok_task("analyze problem", cwd=ws, tier="fast", readonly=True)
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
        ok, out, err = execute_grok_task("write complex module", cwd=ws, tier="deep", readonly=False)
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
        with _FakeProviderHome() as fh:
            ws = str(fh.workspace)
            # Grok as active provider mounts ~/.grok, and does NOT mount ~/.claude or ~/.codex
            cmd = ["grok", "-p", "hello"]
            for readonly in (False, True):
                bwrap_cmd = wrap_bwrap(cmd, workspace=ws, is_provider=True, provider_name="grok", readonly=readonly)
                grok_dir = fh.path(".grok")
                binds = _pairs(bwrap_cmd, "--bind")
                ro = _pairs(bwrap_cmd, "--ro-bind")
                self.assertIn((grok_dir, grok_dir), binds)
                # bin/ and config.toml are re-mounted read-only AFTER the ~/.grok bind
                root_idx = bwrap_cmd.index(grok_dir)
                for rel in ("bin", "config.toml"):
                    p = os.path.join(grok_dir, rel)
                    self.assertIn((p, p), ro, rel)
                    self.assertGreater(bwrap_cmd.index(p), root_idx, rel)
                for other in (".claude", ".codex"):
                    self.assertNotIn(fh.path(other), bwrap_cmd)

            # Other provider (e.g. claude) mounts ~/.claude, and does NOT mount ~/.grok
            bwrap_cmd_claude = wrap_bwrap(["claude"], workspace=ws, is_provider=True, provider_name="claude")
            self.assertNotIn(fh.path(".grok"), bwrap_cmd_claude)
            self.assertNotIn(fh.path(".grok/bin"), bwrap_cmd_claude)

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
        with _FakeProviderHome() as fh:
            user_home = str(fh.home)

            # When provider_name="claude", HOME is tmpfs, only .claude is mounted, .codex is NEVER mounted
            cmd = wrap_bwrap(["claude", "-p", "test"], str(fh.workspace), is_provider=True, provider_name="claude")
            self.assertIn(("--tmpfs", user_home), list(zip(cmd, cmd[1:])))
            claude_dir = fh.path(".claude")
            self.assertIn((claude_dir, claude_dir), _pairs(cmd, "--bind"))
            self.assertNotIn(fh.path(".codex"), cmd)

            # read-only tasks mount the whole claude state root read-only
            cmd_ro = wrap_bwrap(["claude", "-p", "test"], str(fh.workspace), is_provider=True,
                                provider_name="claude", readonly=True)
            self.assertIn((claude_dir, claude_dir), _pairs(cmd_ro, "--ro-bind"))
            self.assertNotIn((claude_dir, claude_dir), _pairs(cmd_ro, "--bind"))

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
        with tempfile.TemporaryDirectory() as ws:
            cmd = wrap_bwrap(["echo", "hi"], ws)
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
            from makewand.git_helper import ensure_git_worktree
            self.assertTrue(ensure_git_worktree(str(cand_p)))
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


class TestGenericModelSemVerDiscovery(unittest.TestCase):
    """
    Validates zero-code-change semantic version parsing and tier capability ranking:
    Future models (e.g. gpt-6.5, gpt-7, claude-6, grok-5) are automatically
    discovered, classified, and ranked without any hardcoded model strings.
    """

    def test_parse_semver_multi_format_resilience(self):
        from makewand.discovery import parse_semver
        self.assertEqual(parse_semver("gpt-6.1-sol"), (6, 1, 0))
        self.assertEqual(parse_semver("gpt-6-astra"), (6, 0, 0))
        self.assertEqual(parse_semver("gpt-6.5-sol"), (6, 5, 0))
        self.assertEqual(parse_semver("gpt-7-astra"), (7, 0, 0))
        self.assertEqual(parse_semver("claude-opus-5-5"), (5, 5, 0))
        self.assertEqual(parse_semver("claude-fable-5-1[1m]"), (5, 1, 0))
        self.assertEqual(parse_semver("claude-haiku-4-5-20251001"), (4, 5, 0))
        self.assertEqual(parse_semver("grok-4.7"), (4, 7, 0))
        self.assertEqual(parse_semver("grok-5.0-build-fast"), (5, 0, 0))
        self.assertEqual(parse_semver("gemini-3.8-pro"), (3, 8, 0))

    def test_rank_models_for_tier_current_and_future_simulation(self):
        from makewand.discovery import rank_models_for_tier

        # Simulated next-generation OpenAI models
        simulated_future_models = [
            ("gpt-6.5-sol", "Latest workhorse model for coding and everyday work."),
            ("gpt-7-astra", "Frontier intelligence for the most demanding work."),
            ("gpt-6.5-luna", "Fast and affordable model for easier tasks."),
            ("gpt-6.1-sol", "Previous generation workhorse model."),
            ("gpt-6-astra", "Previous generation frontier model."),
            ("gpt-6-luna", "Previous fast model.")
        ]

        # Fast tier picks latest fast model
        fast_ranked = rank_models_for_tier(simulated_future_models, "fast")
        self.assertEqual(fast_ranked[0][1], "gpt-6.5-luna")

        # Standard tier picks latest workhorse model
        std_ranked = rank_models_for_tier(simulated_future_models, "standard")
        self.assertEqual(std_ranked[0][1], "gpt-6.5-sol")

        # Deep tier picks latest frontier reasoning model
        deep_ranked = rank_models_for_tier(simulated_future_models, "deep")
        self.assertEqual(deep_ranked[0][1], "gpt-7-astra")

    def test_get_provider_model_tier_adapts_to_future_cache_without_code_changes(self):
        from makewand.discovery import get_provider_model_tier
        with tempfile.TemporaryDirectory() as tmp_codex:
            fake_base = Path(tmp_codex)
            future_cache = {
                "models": [
                    {"slug": "gpt-6.5-sol", "description": "Latest workhorse model for coding and everyday work."},
                    {"slug": "gpt-7-astra", "description": "Frontier intelligence for the most demanding work."},
                    {"slug": "gpt-6.5-luna", "description": "Fast and affordable model for easier tasks."}
                ]
            }
            (fake_base / "models_cache.json").write_text(json.dumps(future_cache), encoding="utf-8")

            with patch.dict(os.environ, {"CODEX_HOME": str(fake_base)}):
                # Ensure no other base dirs interfere
                with patch("pathlib.Path.home", return_value=fake_base):
                    fast_res = get_provider_model_tier("codex", "fast")
                    self.assertEqual(fast_res["model"], "gpt-6.5-luna")
                    self.assertEqual(fast_res["effort"], "low")

                    std_res = get_provider_model_tier("codex", "standard")
                    self.assertEqual(std_res["model"], "gpt-6.5-sol")

                    deep_res = get_provider_model_tier("codex", "deep")
                    self.assertEqual(deep_res["model"], "gpt-7-astra")
                    self.assertEqual(deep_res["effort"], "max")


class TestCliOptimizationsAndCircuitBreaker(unittest.TestCase):
    """
    Validates CLI ergonomics (-C cwd redirection, -f/--prompt-file, non-TTY stdin piping)
    and the low-quota circuit breaker (<= 8% quota protection for external subscriptions).
    """

    def test_resolve_cli_prompt_from_positional_string(self):
        from makewand.cli import _resolve_cli_prompt
        import argparse
        args = argparse.Namespace(prompt="hello world", prompt_file=None)
        res = _resolve_cli_prompt(args)
        self.assertEqual(res, "hello world")

    def test_resolve_cli_prompt_from_file_flag(self):
        from makewand.cli import _resolve_cli_prompt
        import argparse
        with tempfile.NamedTemporaryFile("w+", delete=False) as tf:
            tf.write("prompt from task file\n")
            tf_name = tf.name
        try:
            args = argparse.Namespace(prompt=None, prompt_file=tf_name)
            res = _resolve_cli_prompt(args)
            self.assertEqual(res, "prompt from task file")
        finally:
            os.remove(tf_name)

    def test_resolve_cli_prompt_from_piped_stdin(self):
        from makewand.cli import _resolve_cli_prompt
        import argparse
        import io
        args = argparse.Namespace(prompt=None, prompt_file=None)
        with patch("sys.stdin", io.StringIO("prompt from piped stdin\n")):
            with patch("sys.stdin.isatty", return_value=False):
                res = _resolve_cli_prompt(args)
                self.assertEqual(res, "prompt from piped stdin")

    def test_resolve_cli_prompt_missing_raises_exit(self):
        from makewand.cli import _resolve_cli_prompt
        import argparse
        args = argparse.Namespace(prompt=None, prompt_file=None)
        with patch("sys.stdin.isatty", return_value=True):
            with self.assertRaises(SystemExit):
                _resolve_cli_prompt(args, required=True)

    def test_low_quota_circuit_breaker_threshold(self):
        from makewand.health import _get_official_subscription_quota
        # Simulate snapshot cache with 6% quota remaining (94% used)
        snapshot_data = {
            "providers": [
                {
                    "Provider": "codex",
                    "HasData": True,
                    "WeeklyPct": 94,
                    "ResetAt": "2026-10-04T10:40:00"
                }
            ]
        }
        with tempfile.NamedTemporaryFile("w+", delete=False) as tf:
            json.dump(snapshot_data, tf)
            tf_path = tf.name

        try:
            with patch("os.path.expanduser", return_value=tf_path):
                q = _get_official_subscription_quota("codex")
                self.assertIsNotNone(q)
                self.assertEqual(q["percentage"], 6)
                self.assertEqual(q["status"], "limited")  # <= 8% is limited
        finally:
            os.remove(tf_path)

    def test_safe_quota_above_circuit_breaker(self):
        from makewand.health import _get_official_subscription_quota
        snapshot_data = {
            "providers": [
                {
                    "Provider": "claude",
                    "HasData": True,
                    "WeeklyPct": 85,
                    "ResetAt": "2026-10-04T11:59:00"
                }
            ]
        }
        with tempfile.NamedTemporaryFile("w+", delete=False) as tf:
            json.dump(snapshot_data, tf)
            tf_path = tf.name

        try:
            with patch("os.path.expanduser", return_value=tf_path):
                q = _get_official_subscription_quota("claude")
                self.assertIsNotNone(q)
                self.assertEqual(q["percentage"], 15)
                self.assertEqual(q["status"], "warning")  # > 8% but < 25% is warning
        finally:
            os.remove(tf_path)

    def test_status_json_contains_privacy_attestation(self):
        from makewand.cli import build_status_json
        cache = {
            "codex": {"status": "limited", "updated_at": "2026-09-30T09:00:00"},
            "local": {"status": "healthy", "updated_at": "2026-09-30T09:00:00"}
        }
        st = build_status_json(cache)
        self.assertIn("privacy_attestation", st)
        pa = st["privacy_attestation"]
        self.assertTrue(pa["local_only_supported"])
        self.assertIn("--local-only", pa["offline_flags"])
        self.assertIn("trusted", pa["repo_trust_levels"])

    def test_cli_parser_routing_with_cwd_and_file(self):
        import subprocess
        # Test CLI auto-routing to run with -C and -f
        with tempfile.NamedTemporaryFile("w+", delete=False) as tf:
            tf.write("echo test task\n")
            tf_path = tf.name

        try:
            cmd = [
                sys.executable, "-c",
                f"import sys, os; from makewand.cli import main; sys.argv = ['makewand', '-C', '/tmp', '-f', '{tf_path}', '--help']; main()"
            ]
            res = subprocess.run(cmd, capture_output=True, text=True)
            self.assertEqual(res.returncode, 0)
            self.assertIn("usage: makewand", res.stdout)
        finally:
            os.remove(tf_path)

    def test_cli_subcommand_inherits_cwd_and_file(self):
        import subprocess
        with tempfile.NamedTemporaryFile("w+", delete=False) as tf:
            tf.write("echo task for codex\n")
            tf_path = tf.name

        try:
            cmd = [
                sys.executable, "-c",
                f"import sys, os; from makewand.cli import main; sys.argv = ['makewand', 'codex', '-C', '/tmp', '-f', '{tf_path}', '--help']; main()"
            ]
            res = subprocess.run(cmd, capture_output=True, text=True)
            self.assertEqual(res.returncode, 0)
            self.assertIn("usage: makewand codex", res.stdout)
        finally:
            os.remove(tf_path)


class TestResidentDaemon(unittest.TestCase):
    """Direction 1: Resident Daemon and IPC Socket Fast-Path unit tests."""

    def test_daemon_io_stream_duck_typing(self):
        import threading
        from makewand.daemon import DaemonIOStream

        class MockSocket:
            def __init__(self):
                self.sent = []
            def sendall(self, b):
                self.sent.append(b)

        sock = MockSocket()
        lock = threading.Lock()
        stream = DaemonIOStream(sock, "out", lock)

        self.assertTrue(stream.isatty())
        self.assertTrue(stream.writable())
        self.assertFalse(stream.readable())
        self.assertFalse(stream.seekable())
        self.assertEqual(stream.encoding, "utf-8")
        self.assertEqual(stream.errors, "replace")

        stream.write("hello world\n")
        self.assertEqual(len(sock.sent), 1)
        data = json.loads(sock.sent[0].decode())
        self.assertEqual(data["type"], "out")
        self.assertEqual(data["data"], "hello world\n")

        stream.writelines(["line1\n", "line2\n"])
        self.assertEqual(len(sock.sent), 3)

    def test_daemon_socket_and_pid_paths(self):
        from makewand.daemon import get_daemon_socket_path, get_daemon_pid_path, get_daemon_log_path
        sock_p = get_daemon_socket_path()
        pid_p = get_daemon_pid_path()
        log_p = get_daemon_log_path()
        self.assertTrue(str(sock_p).endswith("makewand.sock"))
        self.assertTrue(str(pid_p).endswith("makewand.pid"))
        self.assertTrue(str(log_p).endswith("daemon.log"))

    def test_daemon_status_when_not_running(self):
        from makewand.daemon import is_daemon_running, daemon_status_cmd
        with patch("makewand.daemon.get_daemon_pid_path") as mock_pid:
            mock_pid.return_value = Path("/tmp/nonexistent_makewand_pid_file.pid")
            running, pid = is_daemon_running()
            self.assertFalse(running)
            self.assertIsNone(pid)
            # daemon_status_cmd exits 0
            code = daemon_status_cmd()
            self.assertEqual(code, 0)


class TestThreeWaySemanticMerger(unittest.TestCase):
    """Direction 2: 3-Way AST and Patch Semantic Merger unit tests."""

    def test_ast_merge_disjoint_python_functions(self):
        from makewand.merger import ast_merge_python_file

        base_code = '''def calculate_total(prices):
    return sum(prices)

def format_currency(val):
    return f"${val}"
'''

        # Candidate A improves calculate_total
        code_a = '''def calculate_total(prices):
    # Support empty or None
    if not prices:
        return 0.0
    return float(sum(prices))

def format_currency(val):
    return f"${val}"
'''

        # Candidate B improves format_currency
        code_b = '''def calculate_total(prices):
    return sum(prices)

def format_currency(val):
    # Support negative numbers and decimals
    return f"${val:,.2f}"
'''

        ok, merged, strategy = ast_merge_python_file(base_code, code_a, code_b)
        self.assertTrue(ok)
        self.assertIn("float(sum(prices))", merged)
        self.assertIn("${val:,.2f}", merged)
        self.assertEqual(strategy, "ast_symbol_splice")

    def test_ast_merge_with_new_imports(self):
        from makewand.merger import ast_merge_python_file

        base_code = '''def process_item(item):
    return item
'''

        code_a = '''import math

def process_item(item):
    return math.sqrt(item)
'''

        code_b = '''import sys

def process_item(item):
    return item

def get_platform():
    return sys.platform
'''

        ok, merged, strategy = ast_merge_python_file(base_code, code_a, code_b)
        self.assertTrue(ok)
        self.assertIn("import math", merged)
        self.assertIn("import sys", merged)
        self.assertIn("math.sqrt", merged)
        self.assertIn("get_platform", merged)

    def test_semantic_merge_candidate_worktrees(self):
        from makewand.merger import semantic_merge_candidate_worktrees

        with tempfile.TemporaryDirectory() as td:
            base_dir = Path(td) / "base"
            cand_a = Path(td) / "cand_a"
            cand_b = Path(td) / "cand_b"
            out_dir = Path(td) / "merged"

            for d in (base_dir, cand_a, cand_b, out_dir):
                d.mkdir(parents=True, exist_ok=True)

            # Base files
            (base_dir / "common.py").write_text("def a(): return 1\ndef b(): return 2\n")
            (base_dir / "file_a_only.txt").write_text("initial a\n")
            (base_dir / "file_b_only.txt").write_text("initial b\n")

            # Clone to cand_a and cand_b
            shutil.copytree(base_dir, cand_a, dirs_exist_ok=True)
            shutil.copytree(base_dir, cand_b, dirs_exist_ok=True)
            shutil.copytree(base_dir, out_dir, dirs_exist_ok=True)

            # Candidate A touches common.py (func a) and file_a_only.txt
            (cand_a / "common.py").write_text("def a(): return 100\ndef b(): return 2\n")
            (cand_a / "file_a_only.txt").write_text("modified by a\n")

            # Candidate B touches common.py (func b) and file_b_only.txt
            (cand_b / "common.py").write_text("def a(): return 1\ndef b(): return 200\n")
            (cand_b / "file_b_only.txt").write_text("modified by b\n")

            ok, changes, conflicts, msg = semantic_merge_candidate_worktrees(
                base_cwd=str(base_dir),
                cand_a_dir=cand_a,
                cand_b_dir=cand_b,
                output_dir=out_dir
            )

            self.assertTrue(ok, msg)
            self.assertEqual(len(conflicts), 0)
            self.assertEqual((out_dir / "file_a_only.txt").read_text().strip(), "modified by a")
            self.assertEqual((out_dir / "file_b_only.txt").read_text().strip(), "modified by b")
            merged_py = (out_dir / "common.py").read_text()
            self.assertIn("return 100", merged_py)
            self.assertIn("return 200", merged_py)

    def test_candidate_manager_hybrid_candidate_lifecycle(self):
        from makewand.candidate import build_manifest
        from makewand.git_helper import clone_isolated_worktree, ensure_git_worktree, run_git_cmd
        with tempfile.TemporaryDirectory() as td:
            base_dir = Path(td) / "workspace"
            base_dir.mkdir()
            (base_dir / "module.py").write_text("def x(): return 1\ndef y(): return 2\n")
            (base_dir / "test_module.py").write_text("def test_module():\n    assert True\n")
            self.assertTrue(ensure_git_worktree(str(base_dir)))
            baseline = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(base_dir))[1].strip()
            race_id = "test_rc_hybrid_1"
            with patch("makewand.candidate.config.CANDIDATES_DIR", Path(td) / "candidates"), \
                 patch("makewand.orchestrator.run_local_tests", return_value=(True, "fixture tests passed")):
                race_dir = Path(td) / "candidates" / race_id
                cand_a_dir, cand_b_dir, frozen = [race_dir / name for name in ("A", "B", "baseline")]
                for destination in (cand_a_dir, cand_b_dir, frozen):
                    clone_isolated_worktree(str(base_dir), destination)
                (cand_a_dir / "module.py").write_text("def x(): return 10\ndef y(): return 2\n")
                (cand_b_dir / "module.py").write_text("def x(): return 1\ndef y(): return 20\n")
                agents = []
                for model, path in (("model-a", cand_a_dir), ("model-b", cand_b_dir)):
                    candidate_baseline = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(path))[1].strip()
                    agents.append({"model": model, "path": str(path), "baseline_commit": candidate_baseline,
                                   "success": True, "test_passed": True})
                CandidateManager.save_race(race_id, "merge complementary fixes", str(base_dir), baseline,
                                           *agents, baseline_dir=frozen,
                                           frozen_baseline_manifest=build_manifest(frozen))
                ok, cand_m, msg = CandidateManager.create_hybrid_candidate(race_id)
                self.assertTrue(ok, msg)
                self.assertIsNotNone(cand_m)
                self.assertEqual(cand_m["label"], "M")
                self.assertIs(cand_m["test_passed"], True)
                self.assertIsNone(cand_m["review_passed"])
                self.assertEqual(CandidateManager.get_race(race_id)["candidates"]["M"], cand_m)


class TestCrossSessionCollisionDetection(unittest.TestCase):
    """Direction 3: Cross-Session Collision Detection and Host-wide Awareness tests."""

    def test_detect_cross_session_collisions_clean(self):
        from makewand.collision import detect_cross_session_collisions
        with tempfile.TemporaryDirectory() as td:
            rep = detect_cross_session_collisions(td)
            self.assertIn("has_collision", rep)
            self.assertIn("collisions", rep)
            self.assertIn("suggested_worktree_cmd", rep)

    def test_detect_cross_session_collisions_with_simulated_peer(self):
        from makewand.collision import detect_cross_session_collisions, format_collision_warning
        with tempfile.TemporaryDirectory() as td:
            fake_proc = {
                "pid": 999999,
                "ppid": 1,
                "ai_type": "claude",
                "comm": "claude",
                "args": "claude -p edit",
                "tty": "pts/99",
                "etime": "01:00",
                "cwd": os.path.realpath(td)
            }
            with patch("makewand.collision.get_active_ai_processes", return_value=[fake_proc]):
                rep = detect_cross_session_collisions(td)
                self.assertTrue(rep["has_collision"])
                self.assertEqual(len(rep["same_worktree_sessions"]), 1)
                self.assertEqual(rep["same_worktree_sessions"][0]["ai_type"], "claude")

                warning_text = format_collision_warning(rep)
                self.assertIn("碰撞感知", warning_text)
                self.assertIn("PID", warning_text)
                self.assertIn("999999", warning_text)
                self.assertIn("git worktree add", warning_text)

    def test_get_all_active_sessions_report(self):
        from makewand.collision import get_all_active_sessions_report
        rep = get_all_active_sessions_report()
        self.assertIn("total_active_sessions", rep)
        self.assertIn("sessions_by_repo", rep)


if __name__ == "__main__":
    unittest.main()
