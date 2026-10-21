"""route --not / --self compare the candidate's model family, not only its provider.

agy carries other vendors' models: claude-opus-5-5-high on agy is the claude
family and gpt-oss-120b is the codex (gpt) family, as discovery.model_family and
the pipeline's reviewer-independence check name them. Built-in catalog, a
temporary outcome ledger and no quota command; no model is called.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import random
import tempfile
import unittest
from pathlib import Path

from makewand.discovery import model_family
from makewand.outcome_ledger import OutcomeLedger
from makewand.route_select import Policy, select

# The owner's review rule (route review hard --not claude --self claude) plus the
# tiers that list agy-proxied Claude rows.
CLAUDE_CASES = (
    ("review", "hard", "low"),
    ("review", "normal", "high"),
    ("review", "normal", "low"),
    ("implement", "normal", "low"),
)
# gpt-oss-120b-medium is a tier-2 row.
GPT_CASES = (("review", "normal", "low"), ("implement", "normal", "low"))


class RouteModelFamilyTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory(prefix="mw-route-family-")
        self.addCleanup(tmp.cleanup)
        self.ledger = OutcomeLedger(Path(tmp.name) / "routing")

    def _select(self, task_type, difficulty, risk, seed=20261021, **kwargs):
        kwargs.setdefault("discovered", {})  # empty discovery: the built-in catalog
        return select(task_type, difficulty, risk, policy=Policy(), quota_reader=lambda: {},
                      ledger=self.ledger, rng=random.Random(seed), now=1_800_000_000.0,
                      full=True, **kwargs)

    def _family(self, result, lineage):
        return [item.candidate_id for item in result.candidates if model_family(item.model) == lineage]

    def test_not_claude_leaves_no_anthropic_model_including_agy_proxied(self):
        for task_type, difficulty, risk in CLAUDE_CASES:
            with self.subTest(task=task_type, difficulty=difficulty, risk=risk):
                for seed in range(20):
                    result = self._select(task_type, difficulty, risk, seed=seed,
                                          not_tool="claude", self_tool="claude")
                    self.assertTrue(result.candidates)
                    self.assertEqual(self._family(result, "claude"), [])
                skipped = dict(result.skipped)
                self.assertEqual(skipped["claude"], "与 claude 同属 anthropic 家族")
                self.assertRegex(skipped["agy"], r"^claude-\S+ .*属于 claude 模型家族，与 claude 同家族$")
                # Only the proxied Claude rows go; agy's Gemini rows still compete.
                self.assertTrue(any(model_family(item.model) == "gemini" for item in result.candidates))
                if result.tier == 3:
                    self.assertIn("claude-opus-5-5-high", skipped["agy"])

    def test_skip_reason_lists_each_proxied_model_once_in_catalog_order(self):
        result = self._select("implement", "normal", "low", not_tool="claude")
        self.assertEqual(result.skipped, [
            ("claude", "与 claude 同属 anthropic 家族"),
            ("agy", "claude-sonnet-5-5-medium、claude-sonnet-5-5-high、claude-opus-5-5-medium"
                    " 属于 claude 模型家族，与 claude 同家族"),
        ])

    def test_not_codex_drops_gpt_oss_on_agy(self):
        for task_type, difficulty, risk in GPT_CASES:
            with self.subTest(task=task_type, difficulty=difficulty, risk=risk):
                result = self._select(task_type, difficulty, risk, not_tool="codex", self_tool="codex")
                self.assertEqual(self._family(result, "gpt"), [])
                self.assertEqual(dict(result.skipped), {
                    "codex": "与 codex 同属 openai 家族",
                    "agy": "gpt-oss-120b-medium 属于 gpt 模型家族，与 codex 同家族",
                })
                # The claude family is a different family here and stays.
                self.assertTrue(self._family(result, "claude"))

    def test_discovered_gpt_and_claude_ids_on_agy_are_matched_by_model(self):
        discovered = {"agy": {"gpt-oss-120b": ["medium", "high"], "claude-opus-5-5": ["high"],
                              "gemini-4-argon": ["high"]}}
        result = self._select("review", "hard", "low", discovered=discovered, not_tool="codex")
        self.assertEqual(self._family(result, "gpt"), [])
        self.assertIn("gpt-oss-120b-high", dict(result.skipped)["agy"])
        self.assertIn("agy/claude-opus-5-5-high/high", [item.candidate_id for item in result.candidates])

    def test_self_demotes_every_model_of_its_family(self):
        for tool, lineage, cases in (("claude", "claude", CLAUDE_CASES), ("codex", "gpt", GPT_CASES)):
            for task_type, difficulty, risk in cases:
                with self.subTest(tool=tool, task=task_type, difficulty=difficulty, risk=risk):
                    result = self._select(task_type, difficulty, risk, self_tool=tool)
                    self.assertEqual(result.skipped, [])
                    for item in result.candidates:
                        self.assertEqual(item.is_self, model_family(item.model) == lineage, item.candidate_id)
                    flags = [item.is_self for item in result.candidates]
                    # Every non-self candidate ranks before every self candidate.
                    self.assertEqual(flags, sorted(flags))
                    self.assertTrue(any(item.provider == "agy" and item.is_self for item in result.candidates))

    def test_other_families_and_the_slim_table_keep_provider_rules(self):
        grok = self._select("implement", "normal", "low", not_tool="grok", self_tool="muse")
        self.assertEqual([provider for provider, _reason in grok.skipped], ["grok"])
        self.assertTrue(all(item.is_self == (item.provider == "muse") for item in grok.candidates))
        # The slim table pins agy to Gemini: --not claude skips only the claude provider.
        slim = select("implement", "normal", "low", not_tool="claude", self_tool="agy", policy=Policy(),
                      quota_reader=lambda: {}, ledger=self.ledger, rng=random.Random(1),
                      now=1_800_000_000.0)
        self.assertEqual(slim.skipped, [("claude", "与 claude 同属 anthropic 家族")])
        self.assertEqual([item.provider for item in slim.candidates if item.is_self], ["agy"])


if __name__ == "__main__":
    unittest.main()
