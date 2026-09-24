"""
Unit tests for Makewand Hybrid Routing (Subscription + API Key Fallback) and Local Model Provider.
"""

import os
import sys
import unittest
from unittest.mock import patch, MagicMock

from makewand.config import (
    get_provider_execution_mode,
    has_api_configured,
    has_subscription_configured
)
from makewand.providers.local import (
    is_local_model_available,
    get_default_local_model,
    list_local_models,
    parse_local_quota,
    get_free_gpu_vram_mb,
    execute_local_task
)
from makewand.health import probe_model
from makewand.orchestrator import select_optimal_engine_pair

class TestHybridRouting(unittest.TestCase):

    def setUp(self):
        self.patch_cfg = patch("makewand.config.load_user_config", return_value={"enabled_providers": {"local": True}})
        self.mock_cfg = self.patch_cfg.start()
        self.patch_local = patch("makewand.providers.local.is_local_model_available", return_value=(True, "gemma4:31b", None))
        self.mock_local = self.patch_local.start()

    def tearDown(self):
        self.patch_cfg.stop()
        self.patch_local.stop()

    def test_execution_mode_detection(self):
        # 1. Local
        self.assertEqual(get_provider_execution_mode("local"), "local")
        self.assertEqual(get_provider_execution_mode("ollama"), "local")

        # 2. Subscription vs API vs Hybrid
        with patch("makewand.config.has_subscription_configured") as mock_sub, \
             patch("makewand.config.has_api_configured") as mock_api:

            # Both configured -> hybrid
            mock_sub.return_value = True
            mock_api.return_value = True
            self.assertEqual(get_provider_execution_mode("codex"), "hybrid")

            # Only subscription -> subscription
            mock_sub.return_value = True
            mock_api.return_value = False
            self.assertEqual(get_provider_execution_mode("codex"), "subscription")

            # Only API -> api
            mock_sub.return_value = False
            mock_api.return_value = True
            self.assertEqual(get_provider_execution_mode("codex"), "api")

            # Neither -> none
            mock_sub.return_value = False
            mock_api.return_value = False
            self.assertEqual(get_provider_execution_mode("codex"), "none")

    @patch("makewand.health.load_status_cache")
    @patch("makewand.config.has_subscription_configured")
    @patch("makewand.config.has_api_configured")
    @patch("makewand.providers.api_client.call_api_chat")
    def test_codex_limited_triggers_api_fallback(self, mock_call_api, mock_has_api, mock_has_sub, mock_cache):
        from makewand.providers.codex import execute_codex_task

        mock_cache.return_value = {"codex": {"status": "limited", "reason": "Monthly quota hit"}}
        mock_has_sub.return_value = True
        mock_has_api.return_value = True
        mock_call_api.return_value = (True, "Generated code via OpenAI API fallback", None)

        ok, out, err = execute_codex_task("Fix bug in auth.py")
        self.assertTrue(ok)
        self.assertEqual(out, "Generated code via OpenAI API fallback")
        self.assertIsNone(err)
        mock_call_api.assert_called_once()

    @patch("makewand.health.load_status_cache")
    @patch("makewand.config.has_subscription_configured")
    @patch("makewand.config.has_api_configured")
    @patch("makewand.providers.api_client.call_api_chat")
    def test_claude_missing_cli_uses_api_directly(self, mock_call_api, mock_has_api, mock_has_sub, mock_cache):
        from makewand.providers.claude import execute_claude_task

        mock_cache.return_value = {}
        mock_has_sub.return_value = False
        mock_has_api.return_value = True
        mock_call_api.return_value = (True, "Claude API direct response", None)

        ok, out, err = execute_claude_task("Write react component")
        self.assertTrue(ok)
        self.assertEqual(out, "Claude API direct response")
        mock_call_api.assert_called_once()

    def test_local_model_functions(self):
        avail, active, all_m = is_local_model_available()
        # In this host, Ollama is actively running on localhost:11434 with models
        if avail:
            self.assertTrue(len(all_m) > 0)
            self.assertIn(active, all_m)
            self.assertEqual(list_local_models(), all_m)
        is_lim, _, _ = parse_local_quota("any output")
        self.assertFalse(is_lim)

    @patch("makewand.providers.local.get_free_gpu_vram_mb")
    @patch("makewand.providers.local.is_local_model_available")
    @patch("makewand.providers.api_client.call_api_chat")
    def test_local_vram_safety_guard_offload(self, mock_api, mock_avail, mock_vram):
        mock_avail.return_value = (True, "gemma4:31b", ["gemma4:31b"])
        # Mock free VRAM is 6GB (< 8GB threshold)
        mock_vram.return_value = 6144
        mock_api.return_value = (True, "CPU execution result", None)

        ok, out, err = execute_local_task("Write script", model="gemma4:31b")
        self.assertTrue(ok)
        self.assertEqual(out, "CPU execution result")

        # Verify extra_params passed num_gpu=0 to protect GPU training tasks
        kwargs = mock_api.call_args[1]
        extra = kwargs.get("extra_params", {})
        self.assertEqual(extra.get("keep_alive"), "0")
        self.assertEqual(extra.get("options", {}).get("num_gpu"), 0)

    def test_orchestrator_hybrid_and_local_selection(self):
        # 1. Test local preference
        cache = {
            "agy": {"status": "healthy"},
            "claude": {"status": "healthy"},
            "codex": {"status": "healthy"},
            "local": {"status": "healthy"}
        }
        coders, reviewers, meta = select_optimal_engine_pair("在本地私有大模型上运行离线分析", cache=cache)
        # Should heavily boost local model
        self.assertIn("local", coders[:2])

        # 2. Test fallback when subscription is limited but API is available
        with patch("makewand.config.has_api_configured") as mock_api:
            mock_api.side_effect = lambda m: m == "codex"
            cache_limited = {
                "agy": {"status": "healthy"},
                "claude": {"status": "healthy"},
                "codex": {"status": "limited"},
                "local": {"status": "healthy"}
            }
            coders, reviewers, meta = select_optimal_engine_pair("算法优化与底层并发", cache=cache_limited, boost=True)
            # Codex should NOT be disqualified (-999) because it has API fallback!
            self.assertGreater(meta["scores"]["codex"], 0)

    def test_health_probe_modes(self):
        # Test probe for local model
        res = probe_model("local")
        self.assertIn("mode", res)
        self.assertEqual(res["mode"], "local")

    def test_provider_enable_disable_switch(self):
        from makewand.config import set_provider_enabled, is_provider_enabled, get_provider_execution_mode
        from makewand.orchestrator import dispatch_task

        # Disable local
        set_provider_enabled("local", False)
        self.assertFalse(is_provider_enabled("local"))
        self.assertEqual(get_provider_execution_mode("local"), "disabled")

        # Probe when disabled returns disabled status
        probe_res = probe_model("local")
        self.assertEqual(probe_res["status"], "disabled")

        # Dispatch when disabled returns error
        ok, out, err = dispatch_task("local", "echo test")
        self.assertFalse(ok)
        self.assertIn("禁用", err)

        # Re-enable local
        set_provider_enabled("local", True)
        self.assertTrue(is_provider_enabled("local"))

if __name__ == "__main__":
    unittest.main()
