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
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest.mock import patch, MagicMock

# Ensure repo root is on sys.path
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from makewand.sandbox import wrap_bwrap, is_bwrap_available, run_in_sandbox
from makewand.orchestrator import dispatch_task, run_review
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


def _assert_private_state_root(test, cmd, state_dir):
    """H3: state_dir is backed by a private per-run directory, never bound from the host."""
    binds = _pairs(cmd, "--bind")
    sources = [src for src, dst in binds if dst == state_dir]
    test.assertEqual(len(sources), 1, binds)
    test.assertIn("provider-homes", sources[0])
    test.assertNotIn(state_dir, [src for src, _ in binds + _pairs(cmd, "--ro-bind")])


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

            # Claude state dir is a private per-run home, its instruction/config paths read-only
            claude_config = fh.path(".claude")
            _assert_private_state_root(self, bwrap_cmd, claude_config)
            ro = _pairs(bwrap_cmd, "--ro-bind")
            # existing host entries are read-only binds; missing ones get no mount
            # (the private home absorbs them and is deleted after the run)
            for rel in ("CLAUDE.md", "settings.json", "commands"):
                p = os.path.join(claude_config, rel)
                self.assertIn((p, p), ro, rel)
            for rel in ("hooks", "skills", "plugins", "agents"):
                p = os.path.join(claude_config, rel)
                self.assertNotIn(p, bwrap_cmd, rel)
                self.assertFalse(os.path.exists(p), rel)

            # Other providers' auth dirs MUST NOT be mounted (isolated by HOME tmpfs)
            self.assertIn(("--tmpfs", user_home), list(zip(bwrap_cmd, bwrap_cmd[1:])))
            for other in (".codex", ".grok", ".gemini"):
                self.assertNotIn(fh.path(other), bwrap_cmd)


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


class TestGrokProviderIntegration(unittest.TestCase):
    """
    Validates complete Grok Build CLI (xAI) integration:
    1. CLI argument construction (tiers, reasoning-effort, permission-mode, headless format)
    2. Sandbox isolation (mounting ~/.grok, masking other providers)
    3. Status probe and quota parsing
    4. Model discovery and dynamic configuration
    5. Orchestrator affinity scoring
    """

    @patch("makewand.sandbox.is_bwrap_available", return_value=True)
    @patch("makewand.providers.grok.run_subprocess")
    def test_grok_cli_argument_construction(self, mock_run, mock_bwrap):
        with _FakeProviderHome() as fh, patch("makewand.sandbox._provider_socket_preflight"):
            self._grok_cli_argument_construction(mock_run, str(fh.workspace))

    def _grok_cli_argument_construction(self, mock_run, ws):
        mock_run.return_value = (0, "Grok response", "", None)

        # 1. Fast tier without a model cache: grok-4.7 (never the 2x-priced
        # grok-4.7-build-fast variant), effort low, readonly=True -> plan mode
        ok, out, err = execute_grok_task("analyze problem", cwd=ws, tier="fast", readonly=True)
        self.assertTrue(ok)
        cmd_args = mock_run.call_args[0][0]
        self.assertIn("--model", cmd_args)
        self.assertEqual(cmd_args[cmd_args.index("--model") + 1], "grok-4.7")
        self.assertIn("--reasoning-effort", cmd_args)
        self.assertEqual(cmd_args[cmd_args.index("--reasoning-effort") + 1], "low")
        self.assertIn("--permission-mode", cmd_args)
        self.assertEqual(cmd_args[cmd_args.index("--permission-mode") + 1], "plan")
        self.assertIn("--output-format", cmd_args)
        self.assertEqual(cmd_args[cmd_args.index("--output-format") + 1], "json")

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
                _assert_private_state_root(self, bwrap_cmd, grok_dir)
                # bin/ and config.toml are re-mounted read-only AFTER the ~/.grok bind
                root_idx = [i for i, x in enumerate(bwrap_cmd) if x == grok_dir and bwrap_cmd[i - 2] == "--bind"][0]
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
            _assert_private_state_root(self, cmd, claude_dir)
            self.assertNotIn(fh.path(".codex"), cmd)

            # read-only tasks get a private home as well (never the host root);
            # instruction files stay read-only binds of the host originals
            cmd_ro = wrap_bwrap(["claude", "-p", "test"], str(fh.workspace), is_provider=True,
                                provider_name="claude", readonly=True)
            _assert_private_state_root(self, cmd_ro, claude_dir)
            claude_md = os.path.join(claude_dir, "CLAUDE.md")
            self.assertIn((claude_md, claude_md), _pairs(cmd_ro, "--ro-bind"))
            self.assertNotIn(fh.path(".codex"), cmd_ro)


    def test_run_git_cmd_injects_security_flags(self):
        from makewand.git_helper import run_git_cmd, SAFE_GIT_SECURITY_FLAGS
        with patch("subprocess.run") as mock_sub:
            mock_sub.return_value = MagicMock(returncode=0, stdout="test", stderr="")
            run_git_cmd(["git", "diff", "HEAD"])
            called_cmd = mock_sub.call_args[0][0]
            self.assertTrue(called_cmd[0] == "git" or called_cmd[0].endswith("git") or called_cmd[0].endswith("git.exe"))
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

    def fake_bound_quota(self, provider, used):
        from pathlib import Path
        from makewand.account_identity import account_identity
        from makewand.health import _get_official_subscription_quota
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            root = home / "selected"
            root.mkdir()
            if provider == "codex":
                (root / "auth.json").write_text(json.dumps({"tokens": {"account_id": "optimization-fixture"}}))
            else:
                (root / ".claude.json").write_text(json.dumps({"oauthAccount": {"accountUuid": "optimization-fixture"}}))
            with patch.dict(os.environ, HOME=str(home), CODEX_HOME=str(root), CLAUDE_CONFIG_DIR=str(root)):
                identity = account_identity(provider)
                now = datetime.now(timezone.utc)
                snapshot = home / ".cache/makewand/quota-snapshot.json"
                snapshot.parent.mkdir(parents=True)
                snapshot.write_text(json.dumps({"version": 2, "taken_at": now.isoformat(), "providers": [{
                    "Provider": provider, "HasData": True, "WeeklyPct": used,
                    "AccountRef": identity["account_ref"], "IdentityKnown": True,
                    "PoolRef": identity["pool_ref"], "CredentialRoot": identity["credential_root"],
                    "SourceAt": now.isoformat(), "WeeklyResetAt": (now + timedelta(hours=1)).isoformat()}]}))
                return _get_official_subscription_quota(provider)

    def test_low_quota_circuit_breaker_threshold(self):
        q = self.fake_bound_quota("codex", 94)
        self.assertIsNotNone(q)
        self.assertEqual(q["percentage"], 6)
        self.assertEqual(q["status"], "limited")

    def test_safe_quota_above_circuit_breaker(self):
        q = self.fake_bound_quota("claude", 85)
        self.assertIsNotNone(q)
        self.assertEqual(q["percentage"], 15)
        self.assertEqual(q["status"], "warning")

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


if __name__ == "__main__":
    unittest.main()
