"""Offline cache/config provenance: defaults never masquerade as discovery."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import json
import os
import tempfile
import unittest
from unittest.mock import patch

from makewand.discovery import discover_available_models, get_provider_model_tier


class DiscoveryProvenanceTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.home = patch("pathlib.Path.home", return_value=self.root)
        self.home.start()
        self.addCleanup(self.home.stop)
        environment = patch.dict(os.environ, {"CODEX_HOME": "", "CLAUDE_CONFIG_DIR": ""})
        environment.start()
        self.addCleanup(environment.stop)

    def write(self, path, value):
        path = self.root / path
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value))
        return path

    def test_absent_caches_and_static_agy_presets_are_not_dynamic(self):
        for provider in ("claude", "codex", "grok", "muse", "agy"):
            for tier in ("fast", "standard", "deep"):
                with self.subTest(provider=provider, tier=tier):
                    result = get_provider_model_tier(provider, tier)
                    self.assertFalse(result["is_dynamic"])
                    self.assertEqual(result["source"], "builtin")
                    self.assertEqual(result["effort_source"], "builtin")

    def test_valid_muse_model_is_detected_but_effort_is_not_discovered(self):
        self.write(".config/muse/settings.json", {"model": "vendor/model-9"})
        result = get_provider_model_tier("muse", "deep")
        self.assertTrue(result["is_dynamic"])
        self.assertEqual(result["model"], "vendor/model-9")
        self.assertEqual(result["effort_source"], "builtin")
        self.assertEqual(discover_available_models()["muse"]["default_source"], "detected")

    def test_empty_invalid_or_malformed_muse_settings_do_not_mark_detection(self):
        path = self.write(".config/muse/settings.json", {})
        for value in (None, "", "   ", False, [], {}, "model\ninvalid", "--option"):
            with self.subTest(value=value):
                path.write_text(json.dumps({"model": value}))
                self.assertFalse(get_provider_model_tier("muse")["is_dynamic"])
                self.assertEqual(discover_available_models()["muse"]["default_source"], "builtin")
        path.write_text("{broken")
        self.assertFalse(get_provider_model_tier("muse")["is_dynamic"])

    def test_invalid_catalog_model_rows_do_not_create_dynamic_claims(self):
        self.write(".claude/cache/model-catalog/catalog.json", {"catalog": {"config": {"models": [{"id": None}, {"id": False}, {"id": ""}, None]}}})
        self.write(".codex/models_cache.json", {"models": [{"slug": False}, {"id": []}, {"slug": ""}, None]})
        self.write(".grok/models_cache.json", {"models": {"": {}, "--option": {}}})
        for provider in ("claude", "codex", "grok"):
            with self.subTest(provider=provider):
                self.assertFalse(get_provider_model_tier(provider)["is_dynamic"])
                self.assertEqual(discover_available_models()[provider]["source"], "builtin")

    def test_selected_codex_home_cannot_borrow_other_profile_catalog(self):
        self.write(".codex/models_cache.json", {"models": [{"slug": "gpt-other-profile"}]})
        selected = self.root / "selected"
        selected.mkdir()
        with patch.dict(os.environ, {"CODEX_HOME": str(selected)}):
            self.assertFalse(get_provider_model_tier("codex")["is_dynamic"])
            self.assertEqual(discover_available_models()["codex"]["source"], "builtin")
            (selected / "config.toml").write_text('model = "gpt-selected-profile"\n')
            result = get_provider_model_tier("codex")
            self.assertTrue(result["is_dynamic"])
            self.assertEqual(result["model"], "gpt-selected-profile")

    def test_claude_catalog_effort_comes_only_from_listed_options(self):
        self.write(".claude/cache/model-catalog/catalog.json", {"catalog": {"config": {"models": [
            {"id": "claude-fixture-9", "name": "Fixture", "thinking": {"effort_options": [{"id": "high"}]}}
        ]}}})
        for tier in ("fast", "standard", "deep"):
            with self.subTest(tier=tier):
                result = get_provider_model_tier("claude", tier)
                self.assertTrue(result["is_dynamic"])
                self.assertEqual(result["model"], "claude-fixture-9")
                self.assertEqual(result["effort"], "high")
                self.assertEqual(result["effort_source"], "detected")

    def test_model_only_catalog_does_not_claim_effort_capability(self):
        self.write(".codex/models_cache.json", {"models": [{"slug": "gpt-fixture-9", "description": "workhorse"}]})
        result = get_provider_model_tier("codex")
        self.assertTrue(result["is_dynamic"])
        self.assertEqual(result["effort_source"], "builtin")

    def test_builtin_or_detected_names_do_not_invent_comparative_superiority(self):
        before = discover_available_models()["claude"]["available"]
        self.assertNotIn("Mythos", " ".join(before))
        self.write(".claude/cache/model-catalog/catalog.json", {"catalog": {"config": {"models": [
            {"id": "claude-fixture-9", "name": "Fixture", "description": "most capable", "notice": {"text": "most capable"}}
        ]}}})
        self.assertEqual(discover_available_models()["claude"]["available"], ["claude-fixture-9 (Fixture)"])


if __name__ == "__main__":
    unittest.main()
