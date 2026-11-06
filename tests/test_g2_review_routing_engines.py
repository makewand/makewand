"""
G2 regressions for reviewer/contestant selection and race dead code.

- replay-0926-memory#18 / py-reliability#7: `makewand disable <engine>` was ignored by run_review (direct
  execute_*_task calls) and run_race fell back to agy unconditionally (contestants and judge).
- orch-extra#1: run_single_racer called an unimported record_engine_usage (swallowed NameError) and
  run_pipeline/run_race carried unused variables (ruff F821/F841).
"""

import contextlib
import io
import json
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

import makewand.config as config
import makewand.orchestrator as orch
from makewand.git_helper import run_git_cmd

try:
    from _verdicts import answer_with_nonce, nonced
except ImportError:  # python3 -m unittest tests.<module>
    from tests._verdicts import answer_with_nonce, nonced

PASS_LINE = 'MAKEWAND_VERDICT: {"pass": true, "defects": []}'
ROOT = Path(__file__).resolve().parent.parent


def enabled_except(*disabled):
    return lambda engine: str(engine).lower() not in disabled


class TestRunReviewHonoursDisableAndHealth(unittest.TestCase):
    def run_review(self, cache, disabled, engine=None):
        mocks = {name: MagicMock(side_effect=answer_with_nonce((True, PASS_LINE, None)))
                 for name in ("execute_codex_task", "execute_grok_task", "execute_agy_task")}
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "get_git_diff", return_value="diff --git a/a b/a\n+x"))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value=cache))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=enabled_except(*disabled)))
            for name, mock in mocks.items():
                stack.enter_context(patch.object(orch, name, mock))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = orch.run_review(cwd="/tmp", output_json=True, engine=engine)
        return code, {name.split("_")[1]: mock.call_count for name, mock in mocks.items()}

    def test_all_reviewers_disabled_or_missing_is_unverified_without_any_call(self):
        cache = {"codex": {"status": "healthy"}, "grok": {"status": "missing"}, "agy": {"status": "healthy"}}
        code, calls = self.run_review(cache, disabled=("codex", "agy"))
        self.assertEqual(code, orch.EXIT_UNVERIFIED)
        self.assertEqual(calls, {"codex": 0, "grok": 0, "agy": 0})

    def test_disabled_codex_falls_to_agy_and_never_to_grok(self):
        cache = {"codex": {"status": "healthy"}, "grok": {"status": "healthy"}, "agy": {"status": "healthy"}}
        code, calls = self.run_review(cache, disabled=("codex",))
        self.assertEqual(code, orch.EXIT_PASSED)
        self.assertEqual(calls, {"codex": 0, "grok": 0, "agy": 1})

    def test_a_healthy_grok_is_not_a_default_reviewer(self):
        cache = {"codex": {"status": "healthy"}, "grok": {"status": "healthy"}, "agy": {"status": "healthy"}}
        code, calls = self.run_review(cache, disabled=("codex", "agy"))
        self.assertEqual(code, orch.EXIT_UNVERIFIED)
        self.assertEqual(calls, {"codex": 0, "grok": 0, "agy": 0})

    def test_no_reviewer_message_names_only_the_default_ladder(self):
        cache = {"codex": {"status": "healthy"}, "grok": {"status": "healthy"}, "agy": {"status": "healthy"}}
        output = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "get_git_diff", return_value="diff --git a/a b/a\n+x"))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value=cache))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=enabled_except("codex", "agy")))
            for name in ("execute_codex_task", "execute_grok_task", "execute_agy_task"):
                stack.enter_context(patch.object(orch, name, side_effect=AssertionError(name)))
            stack.enter_context(contextlib.redirect_stdout(output))
            code = orch.run_review(cwd="/tmp", output_json=True)
        self.assertEqual(code, orch.EXIT_UNVERIFIED)
        message = " ".join(json.loads(output.getvalue())["defects"])
        self.assertIn("(codex/agy 均被禁用或不可用)", message)
        self.assertNotIn("grok", message)

    def test_grok_still_reviews_when_forced(self):
        cache = {"codex": {"status": "healthy"}, "grok": {"status": "healthy"}, "agy": {"status": "healthy"}}
        with patch("makewand.config.has_subscription_configured", return_value=True):
            code, calls = self.run_review(cache, disabled=(), engine="grok")
        self.assertEqual(code, orch.EXIT_PASSED)
        self.assertEqual(calls, {"codex": 0, "grok": 1, "agy": 0})

    def test_probed_disabled_or_limited_status_is_skipped(self):
        cache = {"codex": {"status": "disabled"}, "grok": {"status": "limited"}, "agy": {"status": "healthy"}}
        code, calls = self.run_review(cache, disabled=())
        self.assertEqual(code, orch.EXIT_PASSED)
        self.assertEqual(calls, {"codex": 0, "grok": 0, "agy": 1})

    def test_local_only_review_routes_to_local_and_fails_closed_without_cloud_fallback(self):
        cloud_mocks = {name: MagicMock(side_effect=answer_with_nonce((True, PASS_LINE, None)))
                       for name in ("execute_codex_task", "execute_grok_task", "execute_agy_task")}
        local_mock = MagicMock(side_effect=answer_with_nonce((True, PASS_LINE, None)))
        cache = {"codex": {"status": "healthy"}, "grok": {"status": "healthy"}, "agy": {"status": "healthy"}, "local": {"status": "healthy"}}
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "get_git_diff", return_value="diff --git a/a b/a\n+x"))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value=cache))
            stack.enter_context(patch("makewand.config.is_provider_enabled", return_value=True))
            for name, mock in cloud_mocks.items():
                stack.enter_context(patch.object(orch, name, mock))
            stack.enter_context(patch.object(orch, "execute_local_task", local_mock))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = orch.run_review(cwd="/tmp", output_json=True, local_only=True)

        self.assertEqual(code, orch.EXIT_PASSED)
        self.assertEqual(local_mock.call_count, 1)
        for name, mock in cloud_mocks.items():
            self.assertEqual(mock.call_count, 0, f"Cloud engine {name} must NOT be called in local_only mode")


class TestOrchestratorStaticHygiene(unittest.TestCase):
    @unittest.skipUnless(shutil.which("ruff"), "ruff not installed")
    def test_no_undefined_names_or_unused_locals(self):
        proc = subprocess.run(
            ["ruff", "check", "--no-cache", "--select", "F821,F841", str(ROOT / "makewand" / "orchestrator.py")],
            capture_output=True, text=True, timeout=60)
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)


if __name__ == "__main__":
    unittest.main()
