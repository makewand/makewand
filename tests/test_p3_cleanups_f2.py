"""P3 clean-ups (2026-10-21 evaluation, package F2), Python side.

- PYORCH-1021-4: ``--workflow single`` is documented as what it does: the coding
  engine reviews its own change in a separate read-only call.
- P3-4: NON_AGENTIC_CHAT_MODELS is derived from API_ONLY_ENGINES.
- P3-5: the verdict readers take no ``allow_legacy`` argument.
- A3 (follow-up to #92): discovery exports a tier of a provider the Go router
  prices only with a priced model.
"""

try:
    import _isolation  # noqa: F401
    import test_workflow_scheduling as scheduling
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401
    from tests import test_workflow_scheduling as scheduling

import ast
import contextlib
import inspect
import io
import json
import re
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import cli, config, discovery, engine_selection, orchestrator as orch, review_contract

REPO_ROOT = Path(__file__).resolve().parent.parent


class SingleWorkflowHelpTests(unittest.TestCase):
    def test_help_says_single_reviews_with_the_coding_engine(self):
        out = io.StringIO()
        with patch.object(sys, "argv", ["makewand", "run", "--help"]), contextlib.redirect_stdout(out), \
                self.assertRaises(SystemExit):
            cli.main()
        text = " ".join(out.getvalue().split())
        self.assertNotIn("independent read-only review", text)
        self.assertIn("single reviews with the coding engine itself in a separate read-only call, "
                      "not an independent review", text)

    def test_docs_say_single_reviews_with_the_coding_engine(self):
        readme = (REPO_ROOT / "README.md").read_text(encoding="utf-8")
        self.assertIn("由写代码的同一引擎在单独的只读调用中自审，没有独立引擎审查", readme)
        self.assertNotIn("仍需单独的只读审查和本地测试", readme)
        contract = " ".join((REPO_ROOT / "docs" / "EXECUTION_CONTRACT.md").read_text(encoding="utf-8").split())
        self.assertIn("uses one coder, which also reviews its change in a separate read-only call: "
                      "no independent engine reviews it", contract)


class SingleWorkflowReviewTests(unittest.TestCase):
    """The documented behaviour: another usable engine does not review a single run."""
    fixtures = scheduling.SchedulingTests.fixtures

    def setUp(self):
        scheduling.SchedulingTests.setUp(self)
        # The fake dispatches must not enter the shared usage history, which
        # later engine selection reads (test_reliability_fixes runs after this).
        patcher = patch("makewand.usage.record_engine_usage")
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_single_reviews_with_the_coder_even_when_another_engine_is_usable(self):
        with self.fixtures(("claude", "codex")):
            result = orch.run_workflow("Implement value", cwd=str(self.repo), force_code=True,
                                       workflow="single", risk="low", auto_fix=False, total_timeout=10)
        self.assertEqual(result.status, "PASSED")
        self.assertEqual([(name, readonly) for name, readonly, _, _ in self.calls],
                         [("claude", False), ("claude", True)])


class NonAgenticChatModelTests(unittest.TestCase):
    def test_derived_from_api_only_engines(self):
        self.assertEqual(engine_selection.NON_AGENTIC_CHAT_MODELS, frozenset({"local", "ollama"}) | config.API_ONLY_ENGINES)
        source = Path(engine_selection.__file__).read_text(encoding="utf-8")
        assignment = next(node for node in ast.walk(ast.parse(source)) if isinstance(node, ast.Assign)
                          and any(isinstance(t, ast.Name) and t.id == "NON_AGENTIC_CHAT_MODELS" for t in node.targets))
        names = {node.id for node in ast.walk(assignment.value) if isinstance(node, ast.Name)}
        literals = {node.value for node in ast.walk(assignment.value) if isinstance(node, ast.Constant)}
        self.assertIn("API_ONLY_ENGINES", names)
        self.assertFalse(literals & config.API_ONLY_ENGINES, "API-only engines are spelled out again")


class VerdictReaderSignatureTests(unittest.TestCase):
    def test_no_reader_takes_allow_legacy(self):
        for reader in (review_contract.evaluate_review_verdict, review_contract.extract_verdict_json,
                       review_contract.is_review_passed, review_contract._scan_verdict_lines):
            with self.subTest(reader=reader.__name__):
                self.assertNotIn("allow_legacy", inspect.signature(reader).parameters)
                with self.assertRaises(TypeError):
                    reader('MAKEWAND_VERDICT: {"pass": true, "defects": []}', nonce="0a1b2c3d", allow_legacy=True)

    def test_an_un_nonced_line_never_counts_when_a_nonce_is_expected(self):
        line = 'MAKEWAND_VERDICT: {"pass": true, "defects": []}'
        self.assertEqual(review_contract.evaluate_review_verdict(line, nonce="0a1b2c3d")["status"],
                         review_contract.REVIEW_UNVERIFIED)
        self.assertIsNone(review_contract.extract_verdict_json(line, nonce="0a1b2c3d"))
        self.assertFalse(review_contract.is_review_passed(line, nonce="0a1b2c3d"))
        self.assertTrue(review_contract.is_review_passed(line.replace("VERDICT:", "VERDICT_0a1b2c3d:"), nonce="0a1b2c3d"))


if __name__ == "__main__":
    unittest.main()
