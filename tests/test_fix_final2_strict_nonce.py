"""No environment lets an un-nonced verdict count, the test harness included (strict-nonce).

review_contract.legacy_verdict_allowed let a verdict line without the per-task
nonce count whenever MAKEWAND_TEST_ISOLATION_ROOT was set (or with
MAKEWAND_ALLOW_LEGACY_VERDICT=1, which no user documentation offers), and
parse_nonced_race_verdict fell back to it: production behaviour depended on a
test variable, and race tests passed only because their fake judges answered
with a bare 'MAKEWAND_RACE_VERDICT:' line. The exemption is gone; fake reviewers
answer with the nonce their prompt asks for (tests/_verdicts.py), and the legacy
format is read only without a nonce.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401
from _verdicts import nonced, prompt_nonce

import contextlib
import inspect
import io
import os
import unittest
from unittest.mock import patch

import test_workflow_scheduling as scheduling
from makewand import config, orchestrator as orch, review_contract
from makewand.candidate import CandidateManager
from makewand.execution_contract import EXIT_PASSED, EXIT_UNVERIFIED

RACE_LINE = 'MAKEWAND_RACE_VERDICT: {"pass": true, "winner": "A", "defects": []}'
REVIEW_LINE = 'MAKEWAND_VERDICT: {"pass": true, "defects": []}'
LEGACY_ENV = {"MAKEWAND_ALLOW_LEGACY_VERDICT": "1"}


class PolicyTests(unittest.TestCase):
    def test_the_test_harness_root_is_set_and_changes_nothing(self):
        self.assertTrue(os.environ.get("MAKEWAND_TEST_ISOLATION_ROOT"))
        for env in ({}, LEGACY_ENV):
            with self.subTest(env=env), patch.dict(os.environ, env):
                os.environ.pop("MAKEWAND_STRICT_NONCE", None)
                self.assertIsNone(orch.parse_nonced_race_verdict(RACE_LINE, "0a1b2c3d"))
                verdict = review_contract.evaluate_review_verdict(REVIEW_LINE, nonce="0a1b2c3d")
                self.assertEqual(verdict["status"], review_contract.REVIEW_UNVERIFIED)

    def test_verdict_code_reads_no_environment_switch(self):
        for module in (review_contract, orch):
            source = inspect.getsource(module)
            self.assertNotIn("MAKEWAND_ALLOW_LEGACY_VERDICT", source)
            self.assertNotIn("MAKEWAND_STRICT_NONCE", source)
        self.assertNotIn("MAKEWAND_TEST_ISOLATION_ROOT", inspect.getsource(review_contract))

    def test_fake_answers_take_the_nonce_their_prompt_asks_for(self):
        review_prompt = review_contract.review_verdict_output_spec(nonce="0a1b2c3d")
        self.assertEqual(prompt_nonce(review_prompt), "0a1b2c3d")
        self.assertEqual(nonced(review_prompt, REVIEW_LINE), REVIEW_LINE.replace("VERDICT:", "VERDICT_0a1b2c3d:"))
        self.assertEqual(nonced("implement the feature", REVIEW_LINE), REVIEW_LINE)


class RaceUnderTheHarnessTests(unittest.TestCase):
    fixtures = scheduling.SchedulingTests.fixtures

    def setUp(self):
        scheduling.SchedulingTests.setUp(self)
        self.candidates = self.root / "candidates"
        for patcher in (patch("makewand.usage.record_engine_usage"),
                        patch.object(config, "CANDIDATES_DIR", self.candidates),
                        patch.object(orch, "CANDIDATES_DIR", self.candidates),
                        patch.dict(os.environ, LEGACY_ENV)):
            patcher.start()
            self.addCleanup(patcher.stop)
        os.environ.pop("MAKEWAND_STRICT_NONCE", None)

    def judged_race(self, answer):
        """Both contestants finish (serial); agy, independent of both, judges with answer(prompt)."""
        engines = ("claude", "codex", "agy")

        def provider(engine):
            def execute(prompt, **options):
                if options.get("readonly"):
                    return True, answer(prompt), None
                (Path(options["cwd"]) / "app.py").write_text(f"VALUE = 2  # {engine}\n")
                return True, "implemented", None
            return execute

        with self.fixtures(engines=engines), contextlib.ExitStack() as stack:
            for name in engines:
                stack.enter_context(patch.object(orch, f"execute_{name}_task", side_effect=provider(name)))
            stack.enter_context(patch.object(orch.os, "getloadavg", return_value=(30, 30, 30)))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = orch.run_race("Implement value 2", cwd=str(self.repo), engine_a="claude", engine_b="codex",
                                 timeout=60)
        return code, CandidateManager.get_race()

    def test_an_unnonced_judge_verdict_selects_no_winner(self):
        code, race = self.judged_race(lambda prompt: RACE_LINE)
        self.assertEqual(code, EXIT_UNVERIFIED, "the test harness root used to let the bare line pick candidate A")
        self.assertIsNone(race["winner"])

    def test_the_nonced_judge_verdict_selects_the_winner(self):
        code, race = self.judged_race(lambda prompt: nonced(prompt, RACE_LINE))
        self.assertEqual(code, EXIT_PASSED)
        self.assertEqual(race["winner"], "A")


if __name__ == "__main__":
    unittest.main()
