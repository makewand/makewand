"""With no usable engine an explain prompt dispatches nothing, and every stop names each engine's reason (py-rel-19).

Round 3 stopped REPL turns and ensembles from dispatching when every active engine
is disabled, limited, logged out, failing or missing, but the orchestrator's
explain path still fell back to a hard-coded ladder: `makewand "解释…"` dispatched
claude, codex, grok, agy and muse, launching logged-out or uninstalled CLIs
(rel19_callers.py, e2e_nousable.py). The pipeline's stop said only "No enabled
execution provider" although engines were enabled, and run_race's "not enough
engines" message named no engine. Now the explain path dispatches nothing and
says why for each engine, the run outcome carries each engine's reason, and the
race lists each engine's state. Routing and providers are fakes.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import tempfile
import unittest
from datetime import datetime, timedelta
from unittest.mock import patch

from makewand import orchestrator as orch
from makewand.execution_contract import EXIT_UNVERIFIED
from makewand.git_helper import run_git_cmd


def live(status, reason):
    return {"status": status, "reason": reason, "updated_at": (datetime.now() - timedelta(minutes=1)).isoformat()}


UNUSABLE = {"claude": live("error", "broken CLI"), "codex": live("needs_auth", "login expired"),
            "agy": live("missing", "agy is not installed"), "grok": live("missing", "not installed"),
            "muse": live("missing", "not installed"), "local": live("missing", "not installed")}


class NoUsableEngineTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-final-explain-")
        self.addCleanup(temporary.cleanup)
        self.repo = Path(temporary.name) / "repo"
        self.repo.mkdir()
        (self.repo / "app.py").write_text("VALUE = 1\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(self.repo))
            self.assertEqual(code, 0, error)
        self.calls = []

    def dispatch(self, engine, prompt, **kwargs):
        self.calls.append(engine)
        return False, None, f"fake failure {engine}"

    @contextlib.contextmanager
    def routing(self, cache, active=("claude", "codex")):
        out = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value=cache))
            stack.enter_context(patch("makewand.health.get_or_update_status", return_value=cache))
            stack.enter_context(patch("makewand.health.load_status_cache", return_value=cache))
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=(True, None)))
            stack.enter_context(patch("makewand.sandbox.verify_writable_sandbox_or_authorized", return_value=(True, None, None)))
            stack.enter_context(patch("makewand.config.is_provider_enabled", return_value=True))
            stack.enter_context(patch("makewand.config.get_active_providers", return_value=list(active)))
            stack.enter_context(patch("makewand.config.has_api_configured", return_value=False))
            stack.enter_context(patch("makewand.usage.record_engine_usage"))
            stack.enter_context(patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)))
            stack.enter_context(patch("makewand.usage.get_engine_usage_stats", return_value={}))
            stack.enter_context(patch("makewand.usage.get_all_engine_reliability", return_value={}))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=self.dispatch))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(out))
            yield out

    def explain(self, cache, **options):
        with self.routing(cache) as out, patch.object(orch, "classify_prompt_intent", return_value="explain"):
            result = orch.run_workflow("解释 VALUE 的含义", cwd=str(self.repo), max_fix=1, total_timeout=60, **options)
        return result, out.getvalue()

    def assert_names_each_engine(self, text):
        for expected in ("claude", "broken CLI", "codex", "login expired", "codex login"):
            self.assertIn(expected, text)

    def test_explain_dispatches_nothing_and_names_each_engines_reason(self):
        result, log = self.explain(UNUSABLE)
        self.assertEqual(self.calls, [], "the explain path used to fall back to claude, codex, grok, agy and muse")
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertTrue(result.error.startswith("No usable execution provider: "), result.error)
        self.assert_names_each_engine(result.error)
        self.assertIn("• claude:", log)
        self.assertIn("• codex:", log)
        self.assertNotIn("候选梯队", log)

    def test_explain_still_asks_the_usable_engine_or_the_one_the_user_forced(self):
        self.explain(dict(UNUSABLE, codex=live("healthy", "ok")))
        self.assertEqual(self.calls, ["codex"])
        self.calls.clear()
        # A usable forced engine runs even when nothing else is usable ...
        self.explain(dict(UNUSABLE, claude=live("healthy", "ok")), forced_engine="claude")
        self.assertEqual(self.calls, ["claude"])
        self.calls.clear()
        # ... an unusable one is refused with its own reason (forced-engine).
        result, log = self.explain(UNUSABLE, forced_engine="claude")
        self.assertEqual(self.calls, [])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("broken CLI", result.error)

    def test_explain_with_no_active_provider_points_to_setup(self):
        with self.routing({}, active=()) as out, patch.object(orch, "classify_prompt_intent", return_value="explain"):
            result = orch.run_workflow("解释 VALUE 的含义", cwd=str(self.repo), max_fix=1, total_timeout=60)
        self.assertEqual(self.calls, [])
        self.assertEqual((result.status, result.error), ("UNVERIFIED", "No enabled execution provider"))
        self.assertIn("接入引导", out.getvalue())

    def test_pipeline_outcome_names_each_engines_reason(self):
        with self.routing(UNUSABLE):
            result = orch.run_workflow("Fix VALUE", cwd=str(self.repo), force_code=True, max_fix=1, total_timeout=60)
        self.assertEqual(self.calls, [])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertNotEqual(result.error, "No enabled execution provider")
        self.assert_names_each_engine(result.error)

    def test_race_without_two_healthy_engines_lists_each_engines_state(self):
        cache = dict(UNUSABLE, claude=live("healthy", "ok"), codex=live("unknown", ""),
                     agy={"status": "error", "reason": "region blocked", "source": "dispatch", "ttl_seconds": 21600,
                          "updated_at": (datetime.now() - timedelta(minutes=1)).isoformat()})
        with self.routing(cache, active=("claude", "codex", "agy")) as out:
            code = orch.run_race("Fix VALUE", cwd=str(self.repo), timeout=60)
        self.assertEqual(code, EXIT_UNVERIFIED)
        self.assertEqual(self.calls, [])
        text = out.getvalue()
        self.assertIn("未派发任何任务", text)
        self.assertIn("(可用: CLAUDE)", text)
        self.assertIn("• codex: 健康状态为 unknown", text)
        self.assertIn("• agy: 健康状态为 error：region blocked", text)
        self.assertNotIn("• claude:", text)

    def test_race_message_names_logged_out_and_failing_engines(self):
        with self.routing(UNUSABLE) as out:
            code = orch.run_race("Fix VALUE", cwd=str(self.repo), timeout=60)
        self.assertEqual(code, EXIT_UNVERIFIED)
        self.assertEqual(self.calls, [])
        text = out.getvalue()
        self.assertIn("• claude: 健康状态为 error：broken CLI", text)
        self.assertIn("• codex: 健康状态为 needs_auth：login expired", text)


if __name__ == "__main__":
    unittest.main()
