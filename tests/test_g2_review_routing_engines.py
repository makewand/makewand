"""
G2 regressions for reviewer/contestant selection and race dead code.

- replay-0926-memory#18 / py-reliability#7: `makewand disable <engine>` was ignored by run_review (direct
  execute_*_task calls) and run_race fell back to agy unconditionally (contestants and judge).
- orch-extra#1: run_single_racer called an unimported record_engine_usage (swallowed NameError) and
  run_pipeline/run_race carried unused variables (ruff F821/F841).
"""

import contextlib
import io
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

import makewand.candidate as candidate
import makewand.config as config
import makewand.orchestrator as orch
from makewand.git_helper import run_git_cmd

PASS_LINE = 'MAKEWAND_VERDICT: {"pass": true, "defects": []}'
ROOT = Path(__file__).resolve().parent.parent


def enabled_except(*disabled):
    return lambda engine: str(engine).lower() not in disabled


class TestRunReviewHonoursDisableAndHealth(unittest.TestCase):
    def run_review(self, cache, disabled):
        mocks = {name: MagicMock(return_value=(True, PASS_LINE, None))
                 for name in ("execute_codex_task", "execute_grok_task", "execute_agy_task")}
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "get_git_diff", return_value="diff --git a/a b/a\n+x"))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value=cache))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=enabled_except(*disabled)))
            for name, mock in mocks.items():
                stack.enter_context(patch.object(orch, name, mock))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = orch.run_review(cwd="/tmp", output_json=True)
        return code, {name.split("_")[1]: mock.call_count for name, mock in mocks.items()}

    def test_all_reviewers_disabled_or_missing_is_unverified_without_any_call(self):
        cache = {"codex": {"status": "healthy"}, "grok": {"status": "missing"}, "agy": {"status": "healthy"}}
        code, calls = self.run_review(cache, disabled=("codex", "agy"))
        self.assertEqual(code, orch.EXIT_UNVERIFIED)
        self.assertEqual(calls, {"codex": 0, "grok": 0, "agy": 0})

    def test_disabled_codex_is_skipped_in_favour_of_enabled_grok(self):
        cache = {"codex": {"status": "healthy"}, "grok": {"status": "healthy"}, "agy": {"status": "healthy"}}
        code, calls = self.run_review(cache, disabled=("codex",))
        self.assertEqual(code, orch.EXIT_PASSED)
        self.assertEqual(calls, {"codex": 0, "grok": 1, "agy": 0})

    def test_probed_disabled_or_limited_status_is_skipped(self):
        cache = {"codex": {"status": "disabled"}, "grok": {"status": "limited"}, "agy": {"status": "healthy"}}
        code, calls = self.run_review(cache, disabled=())
        self.assertEqual(code, orch.EXIT_PASSED)
        self.assertEqual(calls, {"codex": 0, "grok": 0, "agy": 1})

    def test_local_only_review_routes_to_local_and_fails_closed_without_cloud_fallback(self):
        cloud_mocks = {name: MagicMock(return_value=(True, PASS_LINE, None))
                       for name in ("execute_codex_task", "execute_grok_task", "execute_agy_task")}
        local_mock = MagicMock(return_value=(True, PASS_LINE, None))
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


class TestRunRaceHonoursDisableAndHealth(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory(prefix="g2-race-")
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        for args in (["init"], ["config", "user.name", "G2"], ["config", "user.email", "g2@example.invalid"]):
            run_git_cmd(["git", *args], cwd=str(self.repo))
        (self.repo / "app.py").write_text("BASE = 1\n", encoding="utf-8")
        (self.repo / "test_app.py").write_text("def test_fixture():\n    assert True\n", encoding="utf-8")
        run_git_cmd(["git", "add", "-A"], cwd=str(self.repo))
        run_git_cmd(["git", "commit", "-m", "baseline"], cwd=str(self.repo))
        self.candidates = self.root / "config" / "candidates"
        self.dispatched = []
        stack = contextlib.ExitStack()
        self.addCleanup(stack.close)
        for obj, name, value in [(orch, "CANDIDATES_DIR", self.candidates),
                                 (config, "CANDIDATES_DIR", self.candidates),
                                 (config, "CONFIG_DIR", self.root / "config")]:
            stack.enter_context(patch.object(obj, name, value))

    def race(self, cache, disabled, engine_a=None, engine_b=None, judge_verdict=None):
        verdict = judge_verdict or 'MAKEWAND_RACE_VERDICT: {"pass": true, "winner": "A", "defects": []}'

        def dispatch(engine, prompt, cwd=None, readonly=False, **kwargs):
            self.dispatched.append((engine, readonly))
            if readonly:
                return True, verdict, None
            (Path(cwd) / "app.py").write_text(f"CANDIDATE = '{engine}'\n", encoding="utf-8")
            return True, "implemented", None

        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value=cache))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=enabled_except(*disabled)))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, "fixture tests passed")))
            stack.enter_context(patch.object(orch, "execute_agy_task", side_effect=AssertionError("race judging must use the unified dispatcher")))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = orch.run_race("Implement update", cwd=str(self.repo), engine_a=engine_a, engine_b=engine_b)
        return code, self.dispatched.count(("agy", True))

    def test_no_contestant_falls_back_to_disabled_agy(self):
        cache = {e: {"status": "missing"} for e in ("claude", "codex", "grok", "muse", "local")}
        cache["agy"] = {"status": "healthy"}
        code, judge_calls = self.race(cache, disabled=("agy",))
        self.assertEqual(code, orch.EXIT_UNVERIFIED)
        self.assertEqual(self.dispatched, [], "a disabled engine must never be dispatched")
        self.assertEqual(judge_calls, 0)
        self.assertFalse(self.candidates.exists() and any(self.candidates.iterdir()))

    def test_disabled_agy_judge_is_replaced_by_enabled_non_contestant(self):
        cache = {"codex": {"status": "healthy"}, "claude": {"status": "healthy"},
                 "grok": {"status": "healthy"}, "agy": {"status": "healthy"}}
        code, judge_calls = self.race(cache, disabled=("agy",), engine_a="codex", engine_b="claude")
        self.assertEqual(judge_calls, 0, "disabled agy must not judge")
        self.assertIn(("grok", True), self.dispatched)
        self.assertNotIn("agy", [engine for engine, _ in self.dispatched])
        self.assertEqual(code, orch.EXIT_PASSED)
        self.assertEqual(candidate.CandidateManager.get_race()["winner"], "A")

    def test_explicitly_requested_disabled_engine_is_rejected_up_front(self):
        cache = {"codex": {"status": "healthy"}, "claude": {"status": "healthy"}}
        code, _ = self.race(cache, disabled=("claude",), engine_a="codex", engine_b="claude")
        self.assertEqual(code, orch.EXIT_UNVERIFIED)
        self.assertEqual(self.dispatched, [])

    def test_enabled_agy_still_judges(self):
        cache = {"codex": {"status": "healthy"}, "claude": {"status": "healthy"}, "agy": {"status": "healthy"}}
        code, judge_calls = self.race(cache, disabled=(), engine_a="codex", engine_b="claude")
        self.assertEqual(judge_calls, 1)
        self.assertEqual(code, orch.EXIT_PASSED)


class TestOrchestratorStaticHygiene(unittest.TestCase):
    @unittest.skipUnless(shutil.which("ruff"), "ruff not installed")
    def test_no_undefined_names_or_unused_locals(self):
        proc = subprocess.run(
            ["ruff", "check", "--no-cache", "--select", "F821,F841", str(ROOT / "makewand" / "orchestrator.py")],
            capture_output=True, text=True, timeout=60)
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)

    def test_race_racer_does_not_reference_unimported_usage_recorder(self):
        import ast
        import inspect
        tree = ast.parse(inspect.getsource(orch.run_race).lstrip())
        names = {node.id for node in ast.walk(tree) if isinstance(node, ast.Name)}
        self.assertNotIn("record_engine_usage", names,
                         "dispatch_task already records usage; a bare call here is an undefined name")


if __name__ == "__main__":
    unittest.main()
