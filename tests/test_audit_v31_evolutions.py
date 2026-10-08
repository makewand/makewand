"""
Tests for v3.1 multi-model audit evolutions & fixes:
1. Untrusted repo write interception in api_client.py and local.py
2. Ensemble model divergence & 'all' providers in ensemble.py
3. Global --effort forwarding in cli.py, orchestrator.py, and task_dag.py
4. Pricing table protection in discovery.py
5. cmd_clean --all flag logic
"""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch, MagicMock

from makewand.providers.api_client import apply_agentic_code_output, call_api_chat
from makewand.providers.local import execute_local_task
from makewand.ensemble import resolve_ensemble_providers, run_ensemble, FRONTIER_ENSEMBLE_PROVIDERS, ALL_ENSEMBLE_PROVIDERS
from makewand.discovery import export_routing_overrides
from makewand.cli import cmd_clean
from makewand.orchestrator import run_pipeline, run_race, run_review, _run_pipeline_impl
from makewand.task_dag import TaskDAG, TaskNode, execute_task_dag


class TestUntrustedRepoInterception(unittest.TestCase):
    """Test untrusted repo write interception in api_client and local providers."""

    def test_apply_agentic_code_output_untrusted_rejects_writes(self):
        with tempfile.TemporaryDirectory() as td:
            sample_code = "```filepath: hello.txt\nhello world\n```"
            # In trusted repo, writes the file
            res_trusted = apply_agentic_code_output(sample_code, td, repo_trust="trusted")
            self.assertEqual(res_trusted, ["hello.txt"])
            self.assertTrue(os.path.exists(os.path.join(td, "hello.txt")))
            os.remove(os.path.join(td, "hello.txt"))

            # In untrusted repo, intercept and reject write
            res_untrusted = apply_agentic_code_output(sample_code, td, repo_trust="untrusted")
            self.assertEqual(res_untrusted, [])
            self.assertFalse(os.path.exists(os.path.join(td, "hello.txt")))

    def test_call_api_chat_untrusted_rejects_coder_role(self):
        with tempfile.TemporaryDirectory() as td:
            res = call_api_chat(
                provider="deepseek",
                prompt="write code",
                cwd=td,
                role="coder",
                repo_trust="untrusted",
                readonly=False,
            )
            # Must fail and indicate read-only constraint
            self.assertFalse(res[0])
            self.assertIn("只读", str(res[2]))

    def test_execute_local_task_untrusted_rejects_writable(self):
        with tempfile.TemporaryDirectory() as td:
            ok, out, err = execute_local_task(
                "write some code",
                cwd=td,
                readonly=False,
                repo_trust="untrusted",
            )
            self.assertFalse(ok)
            self.assertIn("只读", err)

    def test_call_api_chat_untrusted_rejects_non_readonly_any_role(self):
        with tempfile.TemporaryDirectory() as td:
            for r in ("coder", "architect", "engineer", "unknown"):
                res = call_api_chat(
                    provider="deepseek",
                    prompt="write code",
                    cwd=td,
                    role=r,
                    repo_trust="untrusted",
                    readonly=False,
                )
                self.assertFalse(res[0], f"Role {r} should be rejected in untrusted non-readonly repo")
                self.assertIn("只读", str(res[2]))

    @patch("makewand.config.has_subscription_configured", return_value=False)
    @patch("makewand.config.has_api_configured", return_value=True)
    def test_claude_api_fallback_untrusted_propagates_and_rejects(self, mock_api, mock_sub):
        from makewand.providers.claude import execute_claude_task
        with tempfile.TemporaryDirectory() as td:
            ok, out, err = execute_claude_task(
                "write code",
                cwd=td,
                readonly=False,
                repo_trust="untrusted",
            )
            self.assertFalse(ok)
            self.assertIn("只读", str(err))

    @patch("makewand.config.is_provider_enabled", return_value=True)
    @patch("makewand.providers.local.is_local_model_available", return_value=(True, "qwen2.5-coder", ["qwen2.5-coder"]))
    @patch("makewand.providers.api_client.call_api_chat", return_value=(True, "analysis result", None))
    def test_execute_local_task_untrusted_allows_readonly(self, mock_call, mock_avail, mock_en):
        with tempfile.TemporaryDirectory() as td:
            ok, out, err = execute_local_task(
                "analyze architecture",
                cwd=td,
                readonly=True,
                repo_trust="untrusted",
            )
            self.assertTrue(ok)
            self.assertEqual(out, "analysis result")


class TestEnsembleEnhancements(unittest.TestCase):
    """Test ensemble 'all' provider expansion and model divergence fix."""

    @patch("makewand.providers.local.is_local_model_available", return_value=(True, "qwen2.5-coder", ["qwen2.5-coder"]))
    @patch("makewand.ensemble.is_provider_enabled", return_value=True)
    @patch("makewand.ensemble.has_subscription_configured", return_value=True)
    def test_resolve_ensemble_providers_all_includes_local(self, mock_sub, mock_en, mock_avail):
        # 'all' must include 'local'
        providers = resolve_ensemble_providers("all")
        self.assertIn("local", providers)
        self.assertIn("claude", providers)
        self.assertIn("codex", providers)

    @patch("makewand.providers.local.is_local_model_available", return_value=(False, "", []))
    @patch("makewand.ensemble.is_provider_enabled", return_value=True)
    @patch("makewand.ensemble.has_subscription_configured", return_value=True)
    def test_resolve_ensemble_providers_all_skips_unhealthy_local(self, mock_sub, mock_en, mock_avail):
        # If local is not available, do not include it
        providers = resolve_ensemble_providers("all")
        self.assertNotIn("local", providers)

    @patch("makewand.orchestrator.dispatch_task")
    def test_ensemble_single_expert_passes_resolved_model(self, mock_dispatch):
        mock_dispatch.return_value = (True, "LGTM", None)
        with patch("makewand.ensemble.resolve_ensemble_providers", return_value=["claude"]):
            run_ensemble("review code", providers="claude", tier="standard", effort="high")
            self.assertTrue(mock_dispatch.called)
            kwargs = mock_dispatch.call_args[1]
            self.assertIn("model", kwargs)
            self.assertIsNotNone(kwargs["model"])
            self.assertEqual(kwargs.get("effort"), "high")


class TestEffortForwarding(unittest.TestCase):
    """Verify reasoning effort is cleanly propagated to all pipeline and race stages."""

    @patch("makewand.orchestrator.dispatch_task")
    @patch("makewand.orchestrator.get_git_diff", return_value="diff --git a/test.py b/test.py\n+x = 1")
    @patch("makewand.orchestrator.run_local_tests", return_value=(True, "OK"))
    def test_run_pipeline_forwards_effort(self, mock_tests, mock_diff, mock_dispatch):
        mock_dispatch.return_value = (True, "```python\nx = 1\n```", None)
        with tempfile.TemporaryDirectory() as td:
            # Init empty git repo
            import subprocess
            subprocess.run(["git", "init"], cwd=td, capture_output=True)
            subprocess.run(["git", "config", "user.email", "test@test.com"], cwd=td, capture_output=True)
            subprocess.run(["git", "config", "user.name", "test"], cwd=td, capture_output=True)
            with open(os.path.join(td, "init.txt"), "w") as f:
                f.write("base")
            subprocess.run(["git", "add", "."], cwd=td, capture_output=True)
            subprocess.run(["git", "commit", "-m", "initial"], cwd=td, capture_output=True)

            with patch("makewand.orchestrator.resolve_review_verdict", return_value=("LGTM", {"pass": True, "execution_status": "PASSED"})):
                with patch("makewand.orchestrator.evaluate_review_verdict", return_value={"status": "PASSED"}):
                    run_pipeline(
                        "write code",
                        cwd=td,
                        effort="max",
                        auto_fix=False,
                        force_code=True,
                        forced_engine="claude",
                    )
            # Check implementation dispatch received effort="max"
            impl_call = [call for call in mock_dispatch.call_args_list if call[1].get("effort") == "max"]
            self.assertTrue(len(impl_call) > 0, "effort was not forwarded to dispatch_task")

    @patch("makewand.orchestrator.dispatch_task")
    @patch("makewand.orchestrator.get_git_diff", return_value="diff --git a/test.py b/test.py\n+x = 1")
    @patch("makewand.orchestrator.get_or_update_status", return_value={"codex": {"status": "healthy"}})
    def test_run_review_forwards_effort(self, mock_status, mock_diff, mock_dispatch):
        mock_dispatch.return_value = (True, "LGTM\nMAKEWAND_VERDICT: {\"pass\": true, \"defects\": []}", None)
        with tempfile.TemporaryDirectory() as td:
            run_review(cwd=td, effort="high", output_json=True)
            self.assertTrue(mock_dispatch.called)
            self.assertEqual(mock_dispatch.call_args_list[0][1].get("effort"), "high")

    @patch("makewand.orchestrator.run_pipeline")
    def test_execute_task_dag_forwards_effort(self, mock_pipeline):
        mock_pipeline.return_value = True
        dag = TaskDAG("build goal", [TaskNode("task-1", "title", "desc")])
        ok, msg, stages = execute_task_dag(dag, cwd="/tmp", effort="high")
        self.assertTrue(ok)
        self.assertTrue(mock_pipeline.called)
        self.assertEqual(mock_pipeline.call_args[1].get("effort"), "high")


class TestPricingTableProtection(unittest.TestCase):
    """Verify that export_routing_overrides outputs discovered.json, preserves routing.json and explicit costs."""

    def test_unconfigured_model_costs_not_zeroed(self):
        with tempfile.TemporaryDirectory() as td:
            config_dir = Path(td)
            exported = export_routing_overrides(config_dir)
            self.assertIsNotNone(exported)
            self.assertEqual(exported.name, "discovered.json")
            data = json.loads(exported.read_text(encoding="utf-8"))
            costs = data.get("costs", {})
            # Crucial invariant: unconfigured models must NOT be forcefully populated with 0.0
            self.assertEqual(len(costs), 0, "Unconfigured models were populated into costs map")

    def test_user_routing_json_never_mutated(self):
        with tempfile.TemporaryDirectory() as td:
            config_dir = Path(td)
            user_routing = config_dir / "routing.json"
            initial_content = '{\n  "costs": {"custom": {"input": 4.5, "output": 18.0}}\n}'
            user_routing.write_text(initial_content, encoding="utf-8")
            exported = export_routing_overrides(config_dir)
            self.assertIsNotNone(exported)
            self.assertEqual(exported.name, "discovered.json")
            # routing.json must not be touched or modified
            self.assertEqual(user_routing.read_text(encoding="utf-8"), initial_content)

    def test_explicit_zero_and_positive_costs_preserved(self):
        with tempfile.TemporaryDirectory() as td:
            config_dir = Path(td)
            target = config_dir / "discovered.json"
            initial = {
                "costs": {
                    "explicit-zero-model": {"input": 0.0, "output": 0.0},
                    "positive-model": {"input": 3.0, "output": 12.0}
                }
            }
            target.write_text(json.dumps(initial), encoding="utf-8")
            exported = export_routing_overrides(config_dir)
            data = json.loads(exported.read_text(encoding="utf-8"))
            # Both explicit zero and positive costs must be preserved
            self.assertIn("explicit-zero-model", data["costs"])
            self.assertEqual(data["costs"]["explicit-zero-model"]["input"], 0.0)
            self.assertIn("positive-model", data["costs"])
            self.assertEqual(data["costs"]["positive-model"]["input"], 3.0)


class TestCmdCleanEnhancement(unittest.TestCase):
    """Verify cmd_clean cleans isolation and cache directories when --all is set."""

    def test_cmd_clean_with_all_flag(self):
        with tempfile.TemporaryDirectory() as td:
            iso_dir = os.path.join(td, "makewand-test-isolation-12345")
            os.makedirs(iso_dir, exist_ok=True)
            with open(os.path.join(iso_dir, "temp.txt"), "w") as f:
                f.write("junk")

            mock_args = MagicMock()
            mock_args.all = True

            with patch("glob.glob", side_effect=lambda pat: [iso_dir] if "test-isolation" in pat else []):
                with patch("sys.exit") as mock_exit:
                    cmd_clean(mock_args)
                    mock_exit.assert_called_with(0)
                    self.assertFalse(os.path.exists(iso_dir))

    def test_cmd_clean_with_broken_symlink(self):
        with tempfile.TemporaryDirectory() as td:
            broken_link = os.path.join(td, "makewand-test-isolation-broken")
            # Point to nonexistent target
            os.symlink(os.path.join(td, "nonexistent"), broken_link)
            self.assertTrue(os.path.islink(broken_link))

            mock_args = MagicMock()
            mock_args.all = True

            with patch("glob.glob", side_effect=lambda pat: [broken_link] if "test-isolation" in pat else []):
                with patch("sys.exit") as mock_exit:
                    cmd_clean(mock_args)
                    mock_exit.assert_called_with(0)
                    self.assertFalse(os.path.lexists(broken_link))

    def test_cmd_clean_cache_roots_symlink_does_not_delete_target(self):
        """Regression test for P1-S1: cache_roots symlink to external dir must be unlinked without deleting target."""
        with tempfile.TemporaryDirectory() as td:
            external_dir = os.path.join(td, "external_user_files")
            os.makedirs(external_dir, exist_ok=True)
            sentinel_file = os.path.join(external_dir, "important_data.txt")
            with open(sentinel_file, "w") as f:
                f.write("critical host data")

            cache_base = os.path.join(td, "cache")
            os.makedirs(cache_base, exist_ok=True)
            symlink_cache = os.path.join(cache_base, "makewand")
            os.symlink(external_dir, symlink_cache)
            self.assertTrue(os.path.islink(symlink_cache))

            mock_args = MagicMock()
            mock_args.all = True

            with patch.dict(os.environ, {"XDG_CACHE_HOME": cache_base, "MAKEWAND_TEST_ISOLATION_ROOT": ""}):
                with patch("glob.glob", return_value=[]):
                    with patch("sys.exit") as mock_exit:
                        cmd_clean(mock_args)
                        mock_exit.assert_called_with(0)

            # The symlink itself must have been unlinked
            self.assertFalse(os.path.lexists(symlink_cache))
            # The target directory and its contents must remain completely intact!
            self.assertTrue(os.path.exists(sentinel_file))
            with open(sentinel_file, "r") as f:
                self.assertEqual(f.read(), "critical host data")

    def test_cmd_clean_cache_roots_broken_symlink_is_safely_unlinked(self):
        """Regression test for P1-S1: broken symlinks in cache_roots must be unlinked safely."""
        with tempfile.TemporaryDirectory() as td:
            cache_base = os.path.join(td, "cache")
            os.makedirs(cache_base, exist_ok=True)
            broken_symlink = os.path.join(cache_base, "makewand")
            os.symlink(os.path.join(td, "nonexistent_target"), broken_symlink)
            self.assertTrue(os.path.islink(broken_symlink))
            self.assertFalse(os.path.exists(broken_symlink))

            mock_args = MagicMock()
            mock_args.all = True

            with patch.dict(os.environ, {"XDG_CACHE_HOME": cache_base, "MAKEWAND_TEST_ISOLATION_ROOT": ""}):
                with patch("glob.glob", return_value=[]):
                    with patch("sys.exit") as mock_exit:
                        cmd_clean(mock_args)
                        mock_exit.assert_called_with(0)

            self.assertFalse(os.path.lexists(broken_symlink))


if __name__ == "__main__":
    unittest.main()
