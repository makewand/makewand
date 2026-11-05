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
from makewand.execution_contract import EXIT_PASSED, EXIT_UNVERIFIED

RACE_LINE = 'MAKEWAND_RACE_VERDICT: {"pass": true, "winner": "A", "defects": []}'
REVIEW_LINE = 'MAKEWAND_VERDICT: {"pass": true, "defects": []}'
LEGACY_ENV = {"MAKEWAND_ALLOW_LEGACY_VERDICT": "1"}


class PolicyTests(unittest.TestCase):

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


if __name__ == "__main__":
    unittest.main()
