"""Behavioral Go/Python provider controls and API configuration contract."""
try:
    import _isolation
except ImportError:
    from tests import _isolation

import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import config, discovery


class SharedConfigContract(unittest.TestCase):
    def test_provider_controls_match_shared_fixture(self):
        cases = json.loads((Path(__file__).parent / "fixtures/shared_config_contract.json").read_text())
        for case in cases:
            with self.subTest(case=case["name"]), tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, case["env"], clear=True), patch.object(config, "CONFIG_FILE", Path(directory) / "config.json"):
                config.CONFIG_FILE.write_text(json.dumps(case["config"]))
                if case.get("expect_error"):
                    with self.assertRaises(config.ConfigError):
                        config.is_provider_enabled("claude")
                    continue
                for provider, expected in case["expected"].items():
                    self.assertEqual(config.is_provider_enabled(provider), expected, provider)

    def test_flat_nested_file_alias_and_environment_api_fields(self):
        settings = {"claude_api_key": "flat-key", "claude_model": "flat-model", "claude_base_url": "https://flat.invalid",
                    "api": {"claude": {"api_key": "nested-key", "model": "nested-model"}, "gemini": {"api_key": "nested-google", "model": "nested-google-model"}}}
        keys = {"anthropic": {"api_key": "alias-key", "model": "alias-model"}, "claude": {"api_key": "file-key", "base_url": "https://file.invalid"},
                "openai": {"api_key": "openai-file-key", "model": "openai-file-model"}}
        with patch.dict(os.environ, {}, clear=True), patch.object(config, "load_user_config", return_value=settings), patch.object(config, "load_api_keys", return_value={}):
            self.assertEqual(config.get_api_config("anthropic"), {"api_key": "flat-key", "model": "flat-model", "base_url": "https://flat.invalid"})
            self.assertEqual(config.get_api_config("agy")["api_key"], "nested-google")
        env = {"ANTHROPIC_MODEL": "env-model", "ANTHROPIC_BASE_URL": "https://env.invalid", "GOOGLE_API_KEY": "env-google", "OPENAI_API_KEY": ""}
        with patch.dict(os.environ, env, clear=True), patch.object(config, "load_user_config", return_value=settings), patch.object(config, "load_api_keys", return_value=keys):
            self.assertEqual(config.get_api_config("claude"), {"api_key": "file-key", "model": "env-model", "base_url": "https://env.invalid"})
            self.assertEqual(config.get_api_config("gemini")["api_key"], "env-google")
            self.assertEqual(config.get_api_config("codex")["api_key"], "openai-file-key")
            self.assertEqual(config.get_api_config("openai")["model"], "openai-file-model")
            self.assertFalse(config.is_api_allowed("claude"))
            self.assertFalse(config.is_api_allowed("codex"))

    def test_existing_provider_environment_aliases_remain_supported(self):
        cases = [
            ("grok", "api_key", ["XAI_API_KEY", "GROK_API_KEY"]),
            ("muse", "api_key", ["META_API_KEY", "MUSE_API_KEY"]),
            ("qwen", "api_key", ["DASHSCOPE_API_KEY", "QWEN_API_KEY"]),
            ("kimi", "api_key", ["MOONSHOT_API_KEY", "KIMI_API_KEY"]),
            ("glm", "api_key", ["ZHIPU_API_KEY", "GLM_API_KEY", "ZHIPUAI_API_KEY"]),
            ("glm", "base_url", ["ZHIPU_BASE_URL", "GLM_BASE_URL"]),
            ("glm", "model", ["GLM_MODEL", "ZHIPU_MODEL"]),
            ("local", "base_url", ["LOCAL_MODEL_ENDPOINT", "OLLAMA_ENDPOINT", "OLLAMA_HOST"]),
            ("local", "model", ["LOCAL_MODEL_NAME", "OLLAMA_MODEL"]),
            ("aider", "api_key", ["AIDER_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "DEEPSEEK_API_KEY"]),
        ]
        for provider, field, names in cases:
            for name in names:
                value = "https://fixture.invalid/v1" if field == "base_url" else "fixture-value"
                with self.subTest(provider=provider, env=name), patch.dict(os.environ, {name: value}, clear=True), patch.object(config, "load_user_config", return_value={}), patch.object(config, "load_api_keys", return_value={}):
                    self.assertEqual(config.get_api_config(provider)[field], value)
            env = {name: str(index) for index, name in enumerate(names)}
            if field == "base_url":
                env = {name: f"https://fixture-{index}.invalid/v1" for index, name in enumerate(names)}
            with patch.dict(os.environ, env, clear=True), patch.object(config, "load_user_config", return_value={}), patch.object(config, "load_api_keys", return_value={}):
                self.assertEqual(config.get_api_config(provider)[field], env[names[0]])

    def test_disabled_discovery_does_not_launch_cli_or_local_probe(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"HOME": directory, "MAKEWAND_DISABLE_AGY": "1", "MAKEWAND_DISABLE_LOCAL": "1"}, clear=True), patch.object(config, "load_user_config", return_value={}), patch("subprocess.run") as run, patch("makewand.providers.local.is_local_model_available") as local_probe:
            self.assertIsNone(discovery._sync_agy_models_cache(Path(directory) / "models.json"))
            discovery.discover_available_models()
            discovery.get_provider_model_tier("local")
            run.assert_not_called()
            local_probe.assert_not_called()


if __name__ == "__main__":
    unittest.main()
