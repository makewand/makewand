"""Offline scheduling integration: no model CLIs or probes are invoked."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401
from _verdicts import answer_with_nonce, nonced, nonced_result

import contextlib
import io
import json
import os
import tempfile
import time
import unittest
from unittest.mock import patch

from makewand import orchestrator as orch, cli, call_budget
from makewand.execution_contract import ExecutionResult, EXIT_BUDGET_EXHAUSTED, EXIT_UNVERIFIED, EXIT_PASSED, EXIT_TIMEOUT
from makewand.execution_runtime import current_context, execution_context
from makewand.workflow import choose_workflow, last_result
from makewand.git_helper import run_git_cmd


class PolicyTests(unittest.TestCase):
    def test_unknown_and_prompt_words_do_not_establish_low_risk(self):
        self.assertEqual(choose_workflow().workflow, "pipeline")
        self.assertEqual(choose_workflow(evidence={"prompt": "typo format only"}).workflow, "pipeline")
        with self.assertRaises(ValueError):
            choose_workflow("single")

    def test_low_single_still_requires_a_separate_review(self):
        plan = choose_workflow("auto", "low")
        self.assertEqual(plan.workflow, "single")
        self.assertFalse(plan.cross_review_required)

    def test_explicit_workflow_wins_and_high_cannot_downgrade(self):
        self.assertEqual(choose_workflow("race", "low").workflow, "race")
        self.assertEqual(choose_workflow("pipeline", "low").workflow, "pipeline")
        with self.assertRaises(ValueError):
            choose_workflow("single", "high")

    def test_only_complete_structured_evidence_establishes_low_risk(self):
        evidence = dict(scope_complete=True, sensitive_interfaces=False,
                        external_side_effects=False, changed_files=2, changed_lines=100)
        self.assertEqual(choose_workflow(evidence=evidence).workflow, "single")
        for field, value in (("changed_files", True), ("changed_lines", 101), ("scope_complete", False)):
            self.assertEqual(choose_workflow(evidence=dict(evidence, **{field: value})).workflow, "pipeline")
        self.assertEqual(choose_workflow(evidence=dict(evidence, sensitive_interfaces=True)).risk, "high")


if __name__ == "__main__":
    unittest.main()
