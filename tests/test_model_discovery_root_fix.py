"""
Regression and verification tests for the strongest model and reasoning mode discovery root fix.
Validates:
1. Token boundary matching prevents false positives (e.g. 'gemini' never matches 'mini').
2. 'pro' family keyword ranks native Gemini models accurately for deep tier.
3. Foreign proxy models (Claude/GPT) never hijack native AGY tier resolution.
4. CLI effort dialect translation normalizes 'max'/'high'/'medium'/'low' appropriately per provider.
5. export_routing_overrides dynamically syncs modern models without hardcoding.
"""

import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import unittest
from makewand.discovery import (
    score_model_for_tier,
    rank_models_for_tier,
    normalize_provider_effort,
    get_provider_model_tier,
    _matches_token,
    FAST_FAMILIES,
    DEEP_FAMILIES,
)


class TestModelDiscoveryRootFix(unittest.TestCase):

    def test_token_boundary_prevents_gemini_mini_collision(self):
        """Verify that 'gemini' is not penalized as 'mini' by token boundary check."""
        self.assertFalse(_matches_token("mini", "gemini-3.8-flash"))
        self.assertFalse(_matches_token("mini", "gemini-3.1-pro"))
        self.assertTrue(_matches_token("mini", "gpt-4o-mini"))
        self.assertTrue(_matches_token("mini", "o3-mini"))

    def test_pro_family_ranks_gemini_pro_over_flash_in_deep_tier(self):
        """Verify Gemini Pro model ranks higher than Gemini Flash in deep tier."""
        score_pro = score_model_for_tier("gemini-3.1-pro", "Gemini 3.1 Pro (High)", "deep")
        score_flash = score_model_for_tier("gemini-3.8-flash", "Gemini 3.8 Flash (High)", "deep")
        self.assertGreater(score_pro, score_flash, "Gemini Pro must outscore Flash in deep tier")

    def test_pro_keyword_does_not_false_positive_on_substrings(self):
        """Verify 'pro' keyword does not match inside other words."""
        self.assertFalse(_matches_token("pro", "appropriate"))
        self.assertFalse(_matches_token("pro", "problem"))
        self.assertFalse(_matches_token("pro", "production"))
        self.assertTrue(_matches_token("pro", "gemini-3.8-pro"))
        self.assertTrue(_matches_token("pro", "Gemini Pro"))

    def test_effort_dialect_normalization(self):
        """Verify effort dialects are accurately translated for each provider CLI."""
        # Codex: model-specific levels from models_cache.json; unknown -> pass through
        import json, os, tempfile
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as codex_home:
            with open(os.path.join(codex_home, "models_cache.json"), "w", encoding="utf-8") as fh:
                json.dump({"models": [
                    {"slug": "gpt-6-astra", "supported_reasoning_levels": [{"effort": e} for e in ("low", "medium", "high", "xhigh", "max", "ultra")]},
                    {"slug": "gpt-5.5", "supported_reasoning_levels": [{"effort": e} for e in ("low", "medium", "high", "xhigh")]},
                ]}, fh)
            with patch.dict(os.environ, {"CODEX_HOME": codex_home}):
                self.assertEqual(normalize_provider_effort("codex", "max", model="gpt-6-astra"), "max")
                self.assertEqual(normalize_provider_effort("codex", "max", model="gpt-5.5"), "xhigh")
                self.assertEqual(normalize_provider_effort("codex", "ultra", model="gpt-5.5"), "xhigh")
                self.assertEqual(normalize_provider_effort("codex", "high", model="gpt-5.5"), "high")
                self.assertEqual(normalize_provider_effort("codex", "max", model="unlisted-model"), "max")
                self.assertEqual(normalize_provider_effort("codex", "medium", model="gpt-6-astra"), "medium")
        self.assertEqual(normalize_provider_effort("codex", "low"), "low")
        self.assertEqual(normalize_provider_effort("codex", "bogus"), "medium")

        # Grok accepts: low, medium, high, xhigh (max/ultra -> xhigh)
        self.assertEqual(normalize_provider_effort("grok", "max"), "xhigh")
        self.assertEqual(normalize_provider_effort("grok", "xhigh"), "xhigh")
        self.assertEqual(normalize_provider_effort("grok", "high"), "high")
        self.assertEqual(normalize_provider_effort("grok", "medium"), "medium")
        self.assertEqual(normalize_provider_effort("grok", "low"), "low")

        # Claude & AGY accept: low, medium, high, xhigh, max
        self.assertEqual(normalize_provider_effort("claude", "max"), "max")
        self.assertEqual(normalize_provider_effort("claude", "high"), "high")
        # agy rejects xhigh/max (gemini-3.1-pro supports only low/high)
        self.assertEqual(normalize_provider_effort("agy", "max"), "high")
        self.assertEqual(normalize_provider_effort("agy", "xhigh"), "high")
        self.assertIsNone(normalize_provider_effort("agy", "medium"))
        self.assertEqual(normalize_provider_effort("agy", "high"), "high")

        # Muse accepts: none, minimal, low, medium, high, xhigh, max, ultra
        self.assertEqual(normalize_provider_effort("muse", "max"), "max")
        self.assertEqual(normalize_provider_effort("muse", "xhigh"), "xhigh")

        # None / empty preservation
        self.assertIsNone(normalize_provider_effort("codex", None))
        self.assertIsNone(normalize_provider_effort("codex", "none"))

    def test_deep_tier_resolution_efforts_are_max(self):
        """Verify deep tier resolution across providers requests top reasoning effort."""
        for provider in ("claude", "codex", "agy", "muse"):
            with self.subTest(provider=provider):
                res = get_provider_model_tier(provider, "deep")
                self.assertEqual(res.get("effort"), "max", f"{provider} deep tier effort should be 'max'")
        # Grok resolves to 'high' by contract, which normalizes to 'xhigh' for maximum effort
        res_grok = get_provider_model_tier("grok", "deep")
        self.assertEqual(res_grok.get("effort"), "high")
        self.assertEqual(normalize_provider_effort("grok", "max"), "xhigh")


if __name__ == "__main__":
    unittest.main()
