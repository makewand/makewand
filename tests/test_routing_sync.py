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
    """Test suite for Go/Python model synchronization via routing.json."""

    def test_export_routing_overrides_creates_valid_schema(self):
        """Verify export_routing_overrides generates valid JSON with models and costs."""
        with tempfile.TemporaryDirectory() as td:
            config_dir = Path(td)
            exported = export_routing_overrides(config_dir)
            self.assertIsNotNone(exported)
            self.assertTrue(exported.exists())
            self.assertEqual(exported.name, "routing.json")

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

            # Crucial Go router invariant: every model ID in models must have a costs entry
            for prov, tiers in models.items():
                for tier, model_id in tiers.items():
                    if model_id:
                        self.assertIn(model_id, costs, f"Model {model_id} for {prov}/{tier} missing from costs")
                        cost = costs[model_id]
                        self.assertIn("input", cost)
                        self.assertIn("output", cost)
                        self.assertIsInstance(cost["input"], (int, float))
                        self.assertIsInstance(cost["output"], (int, float))

    def test_export_routing_overrides_preserves_custom_tables(self):
        """Verify existing user-configured strategies and costs are preserved."""
        with tempfile.TemporaryDirectory() as td:
            config_dir = Path(td)
            target = config_dir / "routing.json"
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


if __name__ == "__main__":
    unittest.main()
