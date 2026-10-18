"""A forced engine is decided first, the same way on every path (forced-engine).

`makewand run --provider X` (run_pipeline's forced_engine) was checked only by
_no_provider_detected, after routing: the pipeline refused it only when every
engine was unusable, and then even a usable forced engine was refused, while
the explain/Q&A path and a pipeline with some other usable engine dispatched
an unusable forced engine; the review path ignored it and the race refused it
with words of its own and no outcome error. Now the forced engine is decided
before anything else: an unusable one is refused with that engine's own reason
(UNVERIFIED, exit 11) identically on the explain/Q&A, pipeline, review and race
paths, and a usable one always runs. Routing, health and providers are fakes.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import os
import tempfile
import unittest
from datetime import datetime, timedelta
from unittest.mock import patch

from makewand import config, orchestrator as orch
from makewand.execution_contract import EXIT_UNVERIFIED, STATUS_CODES
from makewand.git_helper import run_git_cmd


def live(status, reason=""):
    return {"status": status, "reason": reason, "updated_at": (datetime.now() - timedelta(minutes=1)).isoformat()}


CACHE = {"claude": live("error", "broken CLI"), "codex": live("healthy", "ok"),
         "agy": live("missing", "not installed"), "grok": live("missing", "not installed"),
         "muse": live("missing", "not installed"), "local": live("missing", "not installed")}
REFUSAL = "指定的引擎 CLAUDE 当前不可用: 健康状态为 error：broken CLI"


class ForcedEngineTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-final2-forced-")
        self.addCleanup(temporary.cleanup)
        self.repo = Path(temporary.name) / "repo"
        self.repo.mkdir()
        (self.repo / "app.py").write_text("VALUE = 1\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(self.repo))
            self.assertEqual(code, 0, error)
        self.candidates = Path(temporary.name) / "candidates"
        self.calls = []

    def dispatch(self, engine, prompt, cwd=None, readonly=False, **kwargs):
        self.calls.append(engine)
        return False, None, f"fake failure {engine}"

    @contextlib.contextmanager
    def environment(self, cache, active=("claude", "codex"), intent=None, api=()):
        out = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value=cache))
            stack.enter_context(patch("makewand.health.load_status_cache", return_value=cache))
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=(True, None)))
            stack.enter_context(patch("makewand.sandbox.verify_writable_sandbox_or_authorized", return_value=(True, None, None)))
            stack.enter_context(patch("makewand.config.get_active_providers", return_value=list(active)))
            stack.enter_context(patch("makewand.config.has_api_configured", side_effect=lambda name: name in api))
            stack.enter_context(patch("makewand.usage.record_engine_usage"))
            stack.enter_context(patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)))
            stack.enter_context(patch("makewand.usage.get_engine_usage_stats", return_value={}))
            stack.enter_context(patch("makewand.usage.get_all_engine_reliability", return_value={}))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=self.dispatch))
            stack.enter_context(patch.object(orch, "get_git_diff", return_value="diff --git a/app.py b/app.py\n+VALUE = 2\n"))
            stack.enter_context(patch.object(config, "CANDIDATES_DIR", self.candidates))
            stack.enter_context(patch.object(orch, "CANDIDATES_DIR", self.candidates))
            if intent:
                stack.enter_context(patch.object(orch, "classify_prompt_intent", return_value=intent))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(out))
            yield out

    def run_task(self, cache, active=("claude", "codex"), intent=None, api=(), **options):
        with self.environment(cache, active, intent, api) as out:
            result = orch.run_workflow("Fix VALUE", cwd=str(self.repo), max_fix=1, total_timeout=60, **options)
        return result, out.getvalue()

    def assert_refused(self, result, log):
        self.assertEqual(self.calls, [], "an unusable forced engine is never dispatched")
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertEqual(STATUS_CODES[result.status], EXIT_UNVERIFIED)
        self.assertEqual(result.error, "Requested engine claude is not usable: 健康状态为 error：broken CLI"
                         "（有效期过后自动恢复，可运行 'makewand probe' 立即复查）")
        self.assertIn(REFUSAL, log)

    def test_unusable_forced_engine_is_refused_identically_on_every_path(self):
        cases = {"explain": dict(intent="explain"), "review": dict(intent="review"),
                 "pipeline": dict(force_code=True), "race": dict(force_code=True, workflow="race")}
        for path, options in cases.items():
            with self.subTest(path=path):
                self.calls.clear()
                result, log = self.run_task(CACHE, forced_engine="claude", **options)
                self.assert_refused(result, log)

    def test_usable_forced_engine_runs_when_routing_finds_no_usable_engine(self):
        # grok is not an active provider and its health is unknown: usable when forced. Every
        # active engine is unusable, which used to refuse even the forced engine.
        cache = dict(CACHE, codex=live("needs_auth", "login expired"), grok={"status": "unknown"})
        result, _ = self.run_task(cache, forced_engine="grok", force_code=True)
        self.assertEqual(self.calls, ["grok"])
        self.calls.clear()
        self.run_task(cache, forced_engine="grok", intent="explain")
        self.assertEqual(self.calls, ["grok"])

    def test_usable_forced_engine_reviews_alone_on_the_review_path(self):
        cache = dict(CACHE, claude=live("healthy", "ok"))
        result, log = self.run_task(cache, forced_engine="claude", intent="review")
        self.assertEqual(self.calls, ["claude"], "the review path used to ignore the forced engine")
        self.assertIn("指定的审查引擎 CLAUDE", log)

    def test_local_only_does_not_treat_the_ignored_provider_as_a_forced_engine(self):
        with patch.object(orch, '_no_provider_detected', wraps=orch._no_provider_detected) as guard:
            result, _ = self.run_task({}, active=(), local_only=True, forced_engine='grok', force_code=True, risk='low')
        self.assertEqual(self.calls, [])
        self.assertEqual(result.status, 'UNVERIFIED')
        self.assertIsNone(guard.call_args.args[0])

    def test_auto_is_no_forced_engine(self):
        self.run_task(CACHE, forced_engine="auto", intent="explain")
        self.assertEqual(self.calls, ["codex"])

    def test_forced_api_only_engine_needs_a_key_the_api_policy_allows(self):
        result, _ = self.run_task(CACHE, forced_engine="deepseek", intent="explain")
        self.assertEqual((self.calls, result.status), ([], "UNVERIFIED"))
        self.assertIn("未配置 DEEPSEEK API Key", result.error)
        result, _ = self.run_task(CACHE, forced_engine="deepseek", intent="explain", api=("deepseek",))
        self.assertEqual((self.calls, result.status), ([], "UNVERIFIED"))
        self.assertIn("subscription_only", result.error)  # a key the API policy does not let it use
        with patch.dict(os.environ, {"MAKEWAND_API_POLICY": "allow_paid"}):
            self.run_task(CACHE, forced_engine="deepseek", intent="explain", api=("deepseek",))
        self.assertEqual(self.calls, ["deepseek"])


if __name__ == "__main__":
    unittest.main()
