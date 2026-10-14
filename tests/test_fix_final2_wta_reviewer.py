"""The Winner-Take-All reviewer is independent of both contestants when one can review (wta-reviewer).

_select_wta_reviewer took the configured race judge first - agy whenever agy is
usable - even when agy was the losing contestant, while the race judge picker
(_select_independent_race_judge) and the CHANGELOG prefer a provider independent
of both contestants. It now applies the judge picker's rule: the configured judge
first, then the judge order, an independent provider preferred; with none, the
judge picker's fallback (the other contestant); never the winner under cross
review, never a reviewer that cannot receive the whole prompt. Provider CLIs are
fakes; the real run_race code runs.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import os
import re
import time
import unittest
from unittest.mock import patch

import test_workflow_scheduling as scheduling
from makewand import config, orchestrator as orch
from makewand.candidate import CandidateManager
from makewand.execution_contract import EXIT_PASSED
from makewand.providers.base import get_current_contestant_scope

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")
HEALTHY = {"status": "healthy"}


def enabled(*names):
    return patch("makewand.config.is_provider_enabled", side_effect=lambda name: name in names)


class SelectionTests(unittest.TestCase):
    def test_losing_agy_contestant_is_not_the_reviewer_when_an_independent_one_is_usable(self):
        cache = {"agy": HEALTHY, "claude": HEALTHY, "codex": HEALTHY}
        with enabled("agy", "claude", "codex"):
            self.assertEqual(orch._select_wta_reviewer(cache, "claude", ("claude", "agy")), "codex")
            self.assertEqual(orch._select_wta_reviewer(cache, "agy", ("claude", "agy")), "codex")

    def test_the_judge_pickers_rule_applies(self):
        cache = {"agy": HEALTHY, "claude": HEALTHY, "codex": HEALTHY, "grok": HEALTHY}
        with enabled("agy", "claude", "codex", "grok"):
            # Not a contestant: the configured judge (agy) reviews, as it would judge.
            self.assertEqual(orch._select_wta_reviewer(cache, "claude", ("claude", "codex")), "agy")
            judge = orch._select_independent_race_judge(cache, ("claude", "codex"), "prompt")
            self.assertEqual(judge, "agy")
            # agy is a contestant: the next independent engine in the judge order.
            self.assertEqual(orch._select_wta_reviewer(cache, "codex", ("codex", "agy")), "claude")
            self.assertEqual(orch._select_independent_race_judge(cache, ("codex", "agy"), "prompt"), "claude")

    def test_without_an_independent_reviewer_the_other_contestant_reviews(self):
        cache = {"agy": HEALTHY, "claude": HEALTHY}
        with enabled("agy", "claude"):
            self.assertEqual(orch._select_wta_reviewer(cache, "claude", ("claude", "agy")), "agy")
            self.assertEqual(orch._select_wta_reviewer(cache, "agy", ("claude", "agy")), "claude")
        with enabled("claude"):  # the winner alone can review
            # The winner never reviews itself under cross review; without it, only as the last resort.
            self.assertIsNone(orch._select_wta_reviewer(cache, "claude", ("claude", "agy")))
            self.assertEqual(orch._select_wta_reviewer(cache, "claude", ("claude", "agy"),
                                                       cross_review_required=False), "claude")

    def test_an_independent_reviewer_that_cannot_receive_the_whole_prompt_is_skipped(self):
        cache = {"agy": HEALTHY, "claude": HEALTHY, "codex": HEALTHY, "grok": HEALTHY}
        large = "x" * (orch.reviewer_prompt_limit("agy") + 1)
        with enabled("agy", "claude", "codex", "grok"):
            self.assertEqual(orch._select_wta_reviewer(cache, "claude", ("claude", "codex"), review_prompt=large), "grok")


class WinnerTakeAllRaceTests(unittest.TestCase):
    fixtures = scheduling.SchedulingTests.fixtures

    def setUp(self):
        scheduling.SchedulingTests.setUp(self)
        self.candidates = self.root / "candidates"
        for patcher in (patch("makewand.usage.record_engine_usage"),
                        patch.object(config, "CANDIDATES_DIR", self.candidates),
                        patch.object(orch, "CANDIDATES_DIR", self.candidates)):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.reviews = []

    def test_wta_winner_is_reviewed_by_a_provider_independent_of_the_losing_agy(self):
        engines = ("claude", "agy", "codex")

        def provider(engine):
            def execute(prompt, **options):
                if options.get("readonly"):
                    self.reviews.append(engine)
                    return True, VERDICT_TAG.search(prompt).group(0) + ': {"pass": true, "defects": []}', None
                workspace = Path(options["cwd"])
                if engine == "claude":  # fast and passing: wins outright
                    (workspace / "app.py").write_text("VALUE = 2\n")
                    return True, "implemented", None
                scope = get_current_contestant_scope()
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline:
                    if scope is not None and scope.cancel_event.is_set():
                        return False, None, "cancelled by competitor win"
                    time.sleep(0.02)
                (workspace / "app.py").write_text("VALUE = 2  # slow\n")
                return True, "implemented", None
            return execute

        with self.fixtures(engines=engines), contextlib.ExitStack() as stack:
            for name in engines:
                stack.enter_context(patch.object(orch, f"execute_{name}_task", side_effect=provider(name)))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = orch.run_race("Implement value 2", cwd=str(self.repo), engine_a="claude", engine_b="agy", timeout=60)
        race = CandidateManager.get_race()
        self.assertEqual(code, EXIT_PASSED)
        self.assertEqual(race["winner"], "A")
        self.assertEqual(self.reviews, ["codex"], "agy, the losing contestant, used to review the winner")


if __name__ == "__main__":
    unittest.main()
