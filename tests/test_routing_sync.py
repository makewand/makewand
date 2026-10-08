"""Unit tests for makewand/discovery.py export_routing_overrides.

Verifies single-source-of-truth synchronization between Python model discovery
and Go router's <config_dir>/routing.json.
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
from unittest.mock import patch

from makewand.discovery import export_routing_overrides, get_provider_model_tier


class TestRoutingSync(unittest.TestCase):
    """Test suite for Python model discovery synchronization via discovered.json."""

    def test_export_routing_overrides_creates_valid_schema(self):
        """Verify export_routing_overrides generates valid JSON with models and costs."""
        with tempfile.TemporaryDirectory() as td:
            config_dir = Path(td)
            exported = export_routing_overrides(config_dir)
            self.assertIsNotNone(exported)
            self.assertTrue(exported.exists())
            self.assertEqual(exported.name, "discovered.json")

            data = json.loads(exported.read_text(encoding="utf-8"))
            self.assertIn("models", data)
            self.assertIn("costs", data)

            models = data["models"]
            costs = data["costs"]

            # Key providers must be present
            for prov in ("claude", "codex", "gemini", "agy", "muse", "grok", "local"):
                self.assertIn(prov, models)
                tiers = models[prov]
                self.assertIn("cheap", tiers)
                self.assertIn("mid", tiers)
                self.assertIn("premium", tiers)

            # Pricing table protection: unconfigured models must NOT be populated with 0.0
            # so the Go router's built-in benchmark price table is preserved.
            for prov, tiers in models.items():
                for tier, model_id in tiers.items():
                    if model_id and model_id in costs:
                        cost = costs[model_id]
                        self.assertIn("input", cost)
                        self.assertIn("output", cost)
                        self.assertIsInstance(cost["input"], (int, float))
                        self.assertIsInstance(cost["output"], (int, float))
            # In an unconfigured environment, unconfigured model IDs should not be forced to 0.0
            self.assertEqual(len(costs), 0)

    def test_export_routing_overrides_preserves_custom_discovered_tables(self):
        """Verify existing custom strategies and costs in discovered.json are preserved."""
        with tempfile.TemporaryDirectory() as td:
            config_dir = Path(td)
            target = config_dir / "discovered.json"
            initial_payload = {
                "costs": {
                    "custom-model": {"input": 12.5, "output": 25.0}
                },
                "strategies": {
                    "power": {
                        "code": {"tier": "premium", "providers": ["claude", "codex"]}
                    }
                },
                "context_budgets": {
                    "claude": {"mid": 99999}
                }
            }
            target.write_text(json.dumps(initial_payload), encoding="utf-8")

            exported = export_routing_overrides(config_dir)
            self.assertIsNotNone(exported)

            data = json.loads(exported.read_text(encoding="utf-8"))
            # Custom cost preserved
            self.assertIn("custom-model", data["costs"])
            self.assertEqual(data["costs"]["custom-model"]["input"], 12.5)

            # Custom strategy preserved
            self.assertIn("strategies", data)
            self.assertIn("power", data["strategies"])

            # Custom context budget preserved
            self.assertIn("context_budgets", data)
            self.assertEqual(data["context_budgets"]["claude"]["mid"], 99999)

            # Models are also populated
            self.assertIn("models", data)
            self.assertIn("claude", data["models"])

    def test_export_routing_overrides_never_modifies_user_routing_json(self):
        """Verify user's manually managed routing.json is completely untouched."""
        with tempfile.TemporaryDirectory() as td:
            config_dir = Path(td)
            user_routing = config_dir / "routing.json"
            initial_content = '{\n  "models": {"claude": {"mid": "claude-pinned"}}\n}'
            user_routing.write_text(initial_content, encoding="utf-8")

            exported = export_routing_overrides(config_dir)
            self.assertIsNotNone(exported)
            self.assertEqual(exported.name, "discovered.json")

            # routing.json must remain byte-identical
            self.assertEqual(user_routing.read_text(encoding="utf-8"), initial_content)

    def test_export_routing_overrides_aborts_on_corrupt_existing_file(self):
        """Verify export aborts without overwriting if discovered.json contains syntax error."""
        with tempfile.TemporaryDirectory() as td:
            config_dir = Path(td)
            target = config_dir / "discovered.json"
            corrupt_content = '{"unclosed json: ...'
            target.write_text(corrupt_content, encoding="utf-8")

            exported = export_routing_overrides(config_dir)
            # Must return None to indicate failure and refuse to overwrite
            self.assertIsNone(exported)
            # Corrupt file must remain intact
            self.assertEqual(target.read_text(encoding="utf-8"), corrupt_content)

    def test_explicit_zero_costs_preserved(self):
        """Verify explicit zero pricing is not discarded."""
        with tempfile.TemporaryDirectory() as td:
            config_dir = Path(td)
            target = config_dir / "discovered.json"
            initial = {
                "costs": {
                    "free-local-model": {"input": 0.0, "output": 0.0}
                }
            }
            target.write_text(json.dumps(initial), encoding="utf-8")

            exported = export_routing_overrides(config_dir)
            self.assertIsNotNone(exported)
            data = json.loads(exported.read_text(encoding="utf-8"))
            self.assertIn("free-local-model", data["costs"])
            self.assertEqual(data["costs"]["free-local-model"]["input"], 0.0)
            self.assertEqual(data["costs"]["free-local-model"]["output"], 0.0)


if __name__ == "__main__":
    unittest.main()
