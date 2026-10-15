"""Reviewer fallbacks pick only usable engines, in a fixed order (rereview-usable).

When the implementing and the fixing provider are both excluded from the
re-review, the pipeline used to fall back to any active provider whose cached
status was not limited/needs_auth/missing, iterating a set: an engine in a live
"error" state (e.g. agy) was picked depending on the hash seed. Re-reviewers and
the initial fallback reviewer now come from engine_selection._engine_usable in
active-provider order; without one, cross review fails closed as UNVERIFIED.
Only provider adapters, routing, the sandbox probe and the test runner are
mocked; the pipeline runs on a temporary repository.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import re
import tempfile
import unittest
from datetime import datetime
from unittest.mock import patch

from makewand import orchestrator as orch
from makewand.execution_contract import EXIT_UNVERIFIED
from makewand.git_helper import PipelineWorkspaceGuard, run_git_cmd
from makewand.health import failure_verdict_expired

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")


class ReviewerFallbackTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-rereview-")
        self.addCleanup(temporary.cleanup)
        self.repo = Path(temporary.name) / "repo"
        self.repo.mkdir()
        (self.repo / "app.py").write_text("VALUE = 1\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(self.repo))
            self.assertEqual(code, 0, error)
        self.calls = []

    @contextlib.contextmanager
    def fixtures(self, engines, reviewers, first_review_fails=True):
        # agy is in a live error state (written just now, so its TTL has not run
        # out: an error verdict without updated_at counts as expired); the others
        # are healthy.
        cache = {name: {"status": "healthy"} for name in engines}
        cache["agy"] = {"status": "error", "reason": "Antigravity backend unavailable",
                        "updated_at": datetime.now().isoformat()}
        self.assertFalse(failure_verdict_expired(cache["agy"]), "the agy fixture must be a live error verdict")
        writes = {"claude": 0}

        def provider(engine):
            def execute(prompt, **kwargs):
                readonly = bool(kwargs.get("readonly"))
                self.calls.append((engine, readonly))
                if readonly:
                    tag = VERDICT_TAG.search(prompt).group(0)
                    if first_review_fails and not any(ro for _, ro in self.calls[:-1]):
                        return True, f'{tag}: {{"pass": false, "defects": ["[P1] VALUE is off by one"]}}', None
                    return True, f'{tag}: {{"pass": true, "defects": []}}', None
                if engine == "claude":
                    writes["claude"] += 1
                    if writes["claude"] > 1:  # the implementer cannot run the repair round
                        return False, None, "quota exceeded"
                    (Path(kwargs["cwd"]) / "app.py").write_text("VALUE = 3\n")
                else:  # the alternate coder repairs
                    (Path(kwargs["cwd"]) / "app.py").write_text("VALUE = 2\n")
                return True, "implemented", None
            return execute

        out = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=(True, None)))
            stack.enter_context(patch("makewand.sandbox.verify_writable_sandbox_or_authorized", return_value=(True, None, None)))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value=cache))
            stack.enter_context(patch.object(orch, "select_optimal_engine_pair", return_value=(
                ["claude", "codex"], list(reviewers), dict(primary_coder="claude", primary_reviewer=reviewers[0], reasons=[]))))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=lambda name: name in engines))
            stack.enter_context(patch("makewand.config.get_active_providers", return_value=list(engines)))
            stack.enter_context(patch("makewand.usage.record_engine_usage"))
            stack.enter_context(patch("makewand.memory.format_memory_hints_for_prompt", return_value=""))
            stack.enter_context(patch("makewand.memory.record_autofix_lesson", return_value=None))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, "1 passed in 0.01s")))
            for name in engines:
                stack.enter_context(patch.object(orch, f"execute_{name}_task", side_effect=provider(name)))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(out))
            yield out

    def reviewers_called(self):
        return [engine for engine, readonly in self.calls if readonly]

    def test_re_review_never_falls_back_to_an_engine_in_an_error_state(self):
        # The implementer (claude) and the repairer (codex) are both excluded: no usable re-reviewer.
        with self.fixtures(("claude", "codex", "agy"), ["codex"]) as out:
            result = orch.run_workflow("Fix VALUE", cwd=str(self.repo), force_code=True, max_fix=1, total_timeout=60)
        self.assertEqual(self.reviewers_called(), ["codex"], out.getvalue())
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertEqual(result.exit_code, EXIT_UNVERIFIED)
        self.assertIn("缺乏与代码实现及修复提供者不同的复审模型", result.error)
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 1\n")

    def test_re_review_uses_the_usable_engine_in_active_provider_order(self):
        with self.fixtures(("claude", "codex", "agy", "grok", "muse"), ["codex"]) as out:
            result = orch.run_workflow("Fix VALUE", cwd=str(self.repo), force_code=True, max_fix=1, total_timeout=60)
        self.assertEqual(self.reviewers_called(), ["codex", "grok"], out.getvalue())
        self.assertEqual(result.status, "PASSED", out.getvalue())
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 2\n")

    def test_initial_fallback_reviewer_is_a_usable_engine(self):
        # Without a workflow plan, no usable routed reviewer: the fallback used to be agy unconditionally.
        guard = PipelineWorkspaceGuard()
        result = False
        with self.fixtures(("claude", "codex", "agy"), ["agy"], first_review_fails=False) as out:
            try:
                result = orch._run_pipeline_impl("Fix VALUE", cwd=str(self.repo), force_code=True, auto_fix=False,
                                                 _guard=guard, _outcome={"status": "FAILED", "error": None})
            finally:
                guard.close(result, None)
        self.assertEqual(self.reviewers_called(), ["codex"], out.getvalue())
        self.assertTrue(result, out.getvalue())


if __name__ == "__main__":
    unittest.main()
