"""Tier catalog: candidate order, starting tier, escalation, and JSON overrides."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import tempfile
import unittest
from pathlib import Path

from makewand.routing_catalog import DIFFICULTIES, PROVIDER_FAMILIES, RISKS, TASK_TYPES, TIERS, TierCandidate, candidates, escalate, family, starting_tier


# DESIGN table, including the Chinese label carried with each candidate.
EXPECTED = {
    1: (
        TierCandidate("agy", ("--model", "gemini-3.8-flash-medium"), "便宜快 · Gemini Flash"),
        TierCandidate("codex", ("--tier", "fast", "--effort", "low"), "便宜快 · 低推理"),
        TierCandidate("grok", ("--tier", "fast", "--effort", "low"), "便宜快 · 低推理"),
        TierCandidate("claude", ("--tier", "fast", "--effort", "low"), "便宜快 · 低推理"),
    ),
    2: (
        TierCandidate("codex", ("--tier", "standard", "--effort", "medium"), "中档 · 中等推理"),
        TierCandidate("agy", ("--model", "gemini-4-argon-medium"), "中档 · Gemini Argon"),
        TierCandidate("grok", ("--tier", "standard", "--effort", "medium"), "中档 · 中等推理"),
        TierCandidate("claude", ("--tier", "standard", "--effort", "medium"), "中档 · 中等推理"),
        TierCandidate("muse", ("--tier", "standard"), "中档"),
    ),
    3: (
        TierCandidate("codex", ("--tier", "deep", "--effort", "high"), "强档 · 深推理"),
        TierCandidate("agy", ("--model", "gemini-4-argon-high"), "强档 · Gemini Argon"),
        TierCandidate("claude", ("--tier", "deep", "--effort", "high"), "强档 · 深推理"),
        TierCandidate("grok", ("--tier", "deep", "--effort", "high"), "强档 · 深推理"),
    ),
}

# Difficulty map. test/doc presses non-hard work down to tier 1 and does not
# press hard. review+hard is 3.
# risk high is 3 for every type and difficulty.
_LOW_RISK_TIER = {"mechanical": 1, "normal": 2, "hard": 3}


def _assert_lists_legal(testcase, message, legal):
    for value in legal:
        testcase.assertIn(value, message)


class RoutingCatalogTests(unittest.TestCase):

    def test_agy_never_names_claude_or_opus(self):
        seen = []
        for tier in TIERS:
            for item in candidates(tier):
                if item.provider != "agy":
                    continue
                blob = " ".join(item.args).lower()
                self.assertNotIn("claude", blob)
                self.assertNotIn("opus", blob)
                self.assertIn("gemini", blob)
                self.assertEqual(item.args[0], "--model")
                seen.append(item.args[1])
        self.assertEqual(seen, [
            "gemini-3.8-flash-medium",
            "gemini-4-argon-medium",
            "gemini-4-argon-high",
        ])

    def test_labels_are_chinese(self):
        for tier in TIERS:
            for item in candidates(tier):
                self.assertTrue(any("\u4e00" <= char <= "\u9fff" for char in item.label), item.label)

    def test_starting_tier_all_combinations(self):
        for task_type in TASK_TYPES:
            for difficulty, tier in _LOW_RISK_TIER.items():
                with self.subTest(task_type=task_type, difficulty=difficulty, risk="low"):
                    pressed = task_type in ("test", "doc") and difficulty != "hard"
                    self.assertEqual(starting_tier(task_type, difficulty, "low"), 1 if pressed else tier)
                with self.subTest(task_type=task_type, difficulty=difficulty, risk="high"):
                    self.assertEqual(starting_tier(task_type, difficulty, "high"), 3)

        # Default risk is low. test/doc hard is not pressed. review+hard starts at 3.
        self.assertEqual(starting_tier("implement", "mechanical"), 1)
        self.assertEqual(starting_tier("implement", "normal"), 2)
        self.assertEqual(starting_tier("implement", "hard"), 3)
        self.assertEqual(starting_tier("research", "normal"), 2)
        for task_type in ("test", "doc"):
            self.assertEqual(starting_tier(task_type, "mechanical"), 1)
            self.assertEqual(starting_tier(task_type, "normal"), 1)
            self.assertEqual(starting_tier(task_type, "hard"), 3)
            self.assertEqual(starting_tier(task_type, "mechanical", "high"), 3)
            self.assertEqual(starting_tier(task_type, "normal", "high"), 3)
        self.assertEqual(starting_tier("review", "mechanical"), 1)
        self.assertEqual(starting_tier("review", "normal"), 2)
        self.assertEqual(starting_tier("review", "hard"), 3)
        self.assertEqual(starting_tier("review", "hard", "low"), 3)
        self.assertEqual(starting_tier("review", "hard", "high"), 3)

    def test_escalate_climbs_then_stops(self):
        self.assertEqual(escalate(1), 2)
        self.assertEqual(escalate(2), 3)
        self.assertIsNone(escalate(3))
        self.assertEqual(escalate(escalate(1)), 3)
        self.assertIsNone(escalate(escalate(escalate(1))))

    def test_escalate_rejects_unknown_tiers(self):
        for bad in (0, 4, -1, "1", "3", 1.0, True, False, None):
            with self.subTest(bad=bad):
                with self.assertRaises(ValueError) as caught:
                    escalate(bad)
                _assert_lists_legal(self, str(caught.exception), ("1", "2", "3"))

    def test_invalid_task_type_difficulty_and_risk_name_legal_values(self):
        cases = (
            ("nope", "normal", "low", TASK_TYPES),
            ("Review", "normal", "low", TASK_TYPES),
            ("", "normal", "low", TASK_TYPES),
            (None, "normal", "low", TASK_TYPES),
            (1, "normal", "low", TASK_TYPES),
            ("review", "easy", "low", DIFFICULTIES),
            ("review", "Hard", "low", DIFFICULTIES),
            ("review", "", "low", DIFFICULTIES),
            ("review", None, "high", DIFFICULTIES),
            ("test", "hard", "medium", RISKS),
            ("test", "hard", "HIGH", RISKS),
            ("test", "hard", "", RISKS),
            ("review", "hard", None, RISKS),
            ("nope", "hard", "high", TASK_TYPES),
            ("implement", "nope", "high", DIFFICULTIES),
        )
        for task_type, difficulty, risk, legal in cases:
            with self.subTest(task_type=task_type, difficulty=difficulty, risk=risk):
                with self.assertRaises(ValueError) as caught:
                    starting_tier(task_type, difficulty, risk)
                _assert_lists_legal(self, str(caught.exception), legal)

    def test_family_mapping(self):
        self.assertEqual(family("codex"), "openai")
        self.assertEqual(family("claude"), "anthropic")
        self.assertEqual(family("agy"), "google")
        self.assertEqual(family("grok"), "xai")
        self.assertEqual(family("muse"), "meta")
        self.assertEqual(family("Codex"), "openai")
        self.assertEqual(family(" AGY "), "google")
        self.assertEqual(
            {name: family(name) for name in PROVIDER_FAMILIES},
            dict(PROVIDER_FAMILIES),
        )
        with self.assertRaises(ValueError) as caught:
            family("opus")
        _assert_lists_legal(self, str(caught.exception), PROVIDER_FAMILIES)
        with self.assertRaises(ValueError) as caught:
            family(None)
        _assert_lists_legal(self, str(caught.exception), PROVIDER_FAMILIES)


if __name__ == "__main__":
    unittest.main()
