"""
Unit tests for Makewand Universal Tool Adaptation & Dynamic Active Tool Pool.
Validates dynamic topology adaptation:
- N == 0 (no tools)
- N == 1 (single tool resilient self-critique mode)
- N >= 2 (cross-model joint orchestration)
- Mainstream ecosystem tools (Aider CLI, DeepSeek API, Aliyun Qwen API, etc.)
"""

import os
import sys
from pathlib import Path

# Ensure repo root is on sys.path
REPO_ROOT = Path(__file__).resolve().parent.parent
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

import unittest
from unittest.mock import patch, MagicMock

from makewand.config import (
    ALL_SUPPORTED_PROVIDERS,
    get_all_supported_providers,
    normalize_provider_name,
    get_api_config,
    has_api_configured,
    has_subscription_configured,
    get_provider_execution_mode,
    get_active_providers,
    is_provider_enabled,
    set_provider_enabled
)
from makewand.orchestrator import (
    select_optimal_engine_pair,
    dispatch_task
)

class TestUniversalToolAdaptation(unittest.TestCase):

    def setUp(self):
        # Clean environment overrides before each test
        self.orig_env = os.environ.copy()

    def tearDown(self):
        os.environ.clear()
        os.environ.update(self.orig_env)

    def test_all_supported_providers_inventory(self):
        """Ensures all mainstream AI tools and ecosystems are registered."""
        all_tools = get_all_supported_providers()
        expected = [
            "claude", "codex", "agy", "grok", "muse", "aider", "cursor", "copilot",
            "deepseek", "qwen", "glm", "kimi", "openrouter", "siliconflow", "local"
        ]
        for tool in expected:
            self.assertIn(tool, all_tools, f"Missing tool {tool} in supported inventory")

    def test_provider_alias_normalization(self):
        """Verifies aliases map to standard canonical names."""
        self.assertEqual(normalize_provider_name("dashscope"), "qwen")
        self.assertEqual(normalize_provider_name("aliyun"), "qwen")
        self.assertEqual(normalize_provider_name("gemini"), "agy")
        self.assertEqual(normalize_provider_name("google"), "agy")
        self.assertEqual(normalize_provider_name("anthropic"), "claude")
        self.assertEqual(normalize_provider_name("openai"), "codex")
        self.assertEqual(normalize_provider_name("xai"), "grok")
        self.assertEqual(normalize_provider_name("meta"), "muse")
        self.assertEqual(normalize_provider_name("ollama"), "local")
        self.assertEqual(normalize_provider_name("zhipu"), "glm")
        self.assertEqual(normalize_provider_name("moonshot"), "kimi")
        self.assertEqual(normalize_provider_name("silicon"), "siliconflow")

    def test_dynamic_active_providers_detection(self):
        """Tests dynamic active tool pool detection based on real-time environment."""
        with patch("makewand.config.has_subscription_configured") as mock_sub, \
             patch("makewand.config.has_api_configured") as mock_api, \
             patch("makewand.config.is_provider_enabled") as mock_en:

            mock_en.return_value = True

            # Case A: User only logged into Claude
            mock_sub.side_effect = lambda p: p == "claude"
            mock_api.side_effect = lambda p: False
            active = get_active_providers()
            self.assertEqual(active, ["claude"])

            # Case B: User logged into Claude and Codex, and provided DEEPSEEK_API_KEY
            mock_sub.side_effect = lambda p: p in ("claude", "codex")
            mock_api.side_effect = lambda p: p == "deepseek"
            active = get_active_providers()
            self.assertIn("claude", active)
            self.assertIn("codex", active)
            self.assertIn("deepseek", active)
            self.assertNotIn("agy", active)

            # Case C: A tool is disabled by user -> excluded even if subscription present
            mock_en.side_effect = lambda p: p != "claude"
            active = get_active_providers()
            self.assertNotIn("claude", active)

    def test_single_tool_mode_resilience(self):
        """Tests that N=1 triggers resilient single-tool mode (self-critique) without failing."""
        with patch("makewand.config.get_active_providers") as mock_active, \
             patch("makewand.orchestrator.get_or_update_status") as mock_status:

            mock_active.return_value = ["claude"]
            mock_status.return_value = {"claude": {"status": "healthy"}}

            coders, reviewers, meta = select_optimal_engine_pair("Write a quicksort function")
            self.assertEqual(coders[0], "claude")
            self.assertEqual(reviewers[0], "claude")
            self.assertTrue(meta["single_tool_mode"])
            self.assertIn("单工具实现 + 独立沙箱自审闭条模式" if "单工具实现 + 独立沙箱自审闭条模式" in " ".join(meta["reasons"]) else "单工具", " ".join(meta["reasons"]))

    def test_multi_tool_cross_model_orchestration(self):
        """Tests that N>=2 pairs different models for cross-model red-team review."""
        with patch("makewand.config.get_active_providers") as mock_active, \
             patch("makewand.orchestrator.get_or_update_status") as mock_status:

            mock_active.return_value = ["claude", "codex", "agy"]
            mock_status.return_value = {
                "claude": {"status": "healthy"},
                "codex": {"status": "healthy"},
                "agy": {"status": "healthy"}
            }

            coders, reviewers, meta = select_optimal_engine_pair("Refactor user model and write tests")
            self.assertFalse(meta["single_tool_mode"])
            self.assertNotEqual(coders[0], reviewers[0], "Coder and Reviewer must be different in multi-tool mode")
            self.assertIn(coders[0], ["claude", "codex", "agy"])
            self.assertIn(reviewers[0], ["claude", "codex", "agy"])

    def test_dispatch_task_aider(self):
        """Tests dispatching tasks to Aider CLI."""
        with patch("makewand.orchestrator.execute_aider_task") as mock_aider:
            mock_aider.return_value = (True, "Aider completed edits", None)
            ok, out, err = dispatch_task("aider", "Fix bug in app.py", cwd="/tmp")
            self.assertTrue(ok)
            self.assertEqual(out, "Aider completed edits")
            mock_aider.assert_called_once()

    def test_dispatch_task_deepseek_api(self):
        """Tests dispatching tasks to DeepSeek cloud API."""
        with patch("makewand.providers.api_client.call_api_chat") as mock_api:
            mock_api.return_value = (True, "DeepSeek analysis passed", None)
            ok, out, err = dispatch_task("deepseek", "Analyze complexity", cwd="/tmp")
            self.assertTrue(ok)
            self.assertEqual(out, "DeepSeek analysis passed")
            mock_api.assert_called_once()
            args, kwargs = mock_api.call_args
            provider = kwargs.get("provider") or (args[0] if args else None)
            self.assertEqual(provider, "deepseek")

    def test_dispatch_task_qwen_api(self):
        """Tests dispatching tasks to Aliyun Qwen cloud API."""
        with patch("makewand.providers.api_client.call_api_chat") as mock_api:
            mock_api.return_value = (True, "Qwen response generated", None)
            ok, out, err = dispatch_task("qwen", "Generate SQL query", cwd="/tmp")
            self.assertTrue(ok)
            self.assertEqual(out, "Qwen response generated")
            mock_api.assert_called_once()
            args, kwargs = mock_api.call_args
            provider = kwargs.get("provider") or (args[0] if args else None)
            self.assertEqual(provider, "qwen")

    def test_cli_enable_disable_universal(self):
        """Tests enabling and disabling any of the supported providers."""
        with patch("makewand.config.save_user_config") as mock_save, \
             patch("makewand.config.load_user_config") as mock_load:
            mock_load.return_value = {"enabled_providers": {}}
            mock_save.return_value = True

            # Test enable deepseek
            ok = set_provider_enabled("deepseek", True)
            self.assertTrue(ok)
            mock_save.assert_called()

            # Test disable qwen
            ok = set_provider_enabled("qwen", False)
            self.assertTrue(ok)

            # Test invalid tool name returns False
            ok = set_provider_enabled("invalid_tool_xyz", True)
            self.assertFalse(ok)

if __name__ == "__main__":
    unittest.main()
