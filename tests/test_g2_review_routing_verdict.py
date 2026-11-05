"""
G2 regressions for the review verdict contract.

- py-orchestrator#4 / replay-0926-memory#5: free-text LGTM without MAKEWAND_VERDICT used to pass.
- arch-product#1: prose keywords (deadlock/[P1]/race condition) used to veto an explicit pass=true verdict,
  triggering useless auto-fix rounds and a hard reset that destroyed approved output.
- py-security#5: review text was pasted verbatim into the writable coder's auto-fix prompt.
Contract: MAKEWAND_VERDICT decides; missing/malformed -> ask the reviewer once -> UNVERIFIED
(no delivery, no auto-fix, patch preserved; `makewand review` exits 11).
"""

import contextlib
import io
import json
import re
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.orchestrator as orch
from makewand.git_helper import run_git_cmd

try:
    from _verdicts import nonced
except ImportError:  # python3 -m unittest tests.<module>
    from tests._verdicts import nonced

PASS_LINE = 'MAKEWAND_VERDICT: {"pass": true, "defects": []}'
FAIL_LINE = 'MAKEWAND_VERDICT: {"pass": false, "defects": ["[P1] off-by-one in reverse()"]}'


def git(path, *args):
    code, out, err = run_git_cmd(["git", *args], cwd=str(path))
    if code:
        raise AssertionError((args, out, err))
    return out.strip()


class TestRunReviewVerdict(unittest.TestCase):
    DIFF = "diff --git a/app.py b/app.py\n+x = 1"

    def review(self, outputs, output_json=False):
        outputs = list(outputs)
        seen = []

        def codex(prompt, **kwargs):
            seen.append(prompt)
            return True, nonced(prompt, outputs.pop(0)) if outputs else "", None

        buf = io.StringIO()
        with patch.object(orch, "get_git_diff", return_value=self.DIFF), \
             patch.object(orch, "get_or_update_status", return_value={"codex": {"status": "healthy"}}), \
             patch("makewand.config.is_provider_enabled", return_value=True), \
             patch.object(orch, "execute_codex_task", side_effect=codex), \
             patch.object(orch, "execute_grok_task", return_value=(False, None, "unused")), \
             patch.object(orch, "execute_agy_task", return_value=(False, None, "unused")), \
             contextlib.redirect_stdout(buf):
            code = orch.run_review(cwd="/tmp", output_json=output_json)
        return code, seen, buf.getvalue()

    def test_bug_but_lgtm_is_unverified_exit_11(self):
        code, seen, _ = self.review(["The code has a bug in line 3 but overall LGTM", "still LGTM"])
        self.assertEqual(code, orch.EXIT_UNVERIFIED)
        self.assertEqual(len(seen), 2, "the reviewer is asked exactly once more")
        self.assertIn("MAKEWAND_VERDICT", seen[0])

    def test_followup_verdict_passes(self):
        code, seen, _ = self.review(["LGTM", PASS_LINE])
        self.assertEqual(code, orch.EXIT_PASSED)

    def test_explicit_failure_exit_10(self):
        code, seen, _ = self.review(["looks good to me\n" + FAIL_LINE])
        self.assertEqual(code, orch.EXIT_FAILED)
        self.assertEqual(len(seen), 1)

    def test_json_output_reports_unverified(self):
        code, _, out = self.review(["LGTM", "LGTM"], output_json=True)
        payload = json.loads(out.strip())
        self.assertEqual(code, orch.EXIT_UNVERIFIED)
        self.assertFalse(payload["pass"])
        self.assertEqual(payload["exit_code"], orch.EXIT_UNVERIFIED)
        self.assertEqual(payload["verdict_status"], orch.REVIEW_UNVERIFIED)


if __name__ == "__main__":
    unittest.main()
