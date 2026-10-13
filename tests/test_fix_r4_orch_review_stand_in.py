"""Review stand-ins never include placeholder engines and keep going after a failed stand-in
(standin-placeholders, round 4).

Round 3 replaced a reviewer that cannot receive the whole review prompt (agy over
its 31 KiB limit) by the next usable engine. Its pool came from the active
providers filtered only by health, so cursor and copilot, which have no executor
and which routing never dispatches, were chosen: dispatch_task answered with a
notice, the log claimed a completed red-team review and the run ended UNVERIFIED
for a missing verdict line, while a usable local model behind them was never
tried. A stand-in that failed also got no successor. The other active-provider
reviewer pools (cross review without routed reviewers, the re-review fallback)
had the same placeholder gap. Providers, health, the sandbox probe and the test
runner are fakes; every run uses a temporary repository.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import json
import re
import tempfile
import unittest
from unittest.mock import patch

from makewand import orchestrator as orch
from makewand.execution_contract import EXIT_PASSED, EXIT_UNVERIFIED
from makewand.git_helper import run_git_cmd

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")
EXECUTORS = ("claude", "codex", "agy", "grok", "muse", "local")


def module_source(lines):
    """About 33 bytes a line: 700 lines fit agy's whole-prompt limit, 1200 do not."""
    return "".join(f"ENTRY_{index:05d} = ({index}, 'value-{index}')\n" for index in range(lines))


class StandInTestCase(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-stand-in-r4-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.calls = []

    def make_repo(self, name="repo"):
        repo = self.root / name
        repo.mkdir()
        (repo / "app.py").write_text("VALUE = 1\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(repo))
            self.assertEqual(code, 0, error)
        return repo

    def run_task(self, repo, *, active, coders=None, reviewers=None, health=None, coder, failing=(),
                 verdicts=None, fix_fails=(), real_router=False, **options):
        """verdicts(engine, review_number) -> (pass, defects); fix_fails: coders whose fix fails."""
        status = {name: {"status": (health or {}).get(name, "healthy")} for name in active}
        reviews = []

        def provider(engine):
            def execute(prompt, **kwargs):
                readonly = bool(kwargs.get("readonly"))
                self.calls.append((engine, readonly))
                if engine in failing:
                    return False, None, f"{engine} simulated failure"
                if readonly:
                    reviews.append(engine)
                    passed, defects = verdicts(engine, len(reviews)) if verdicts else (True, [])
                    verdict = json.dumps({"pass": passed, "defects": defects}, ensure_ascii=False)
                    return True, f"Reviewed.\n{VERDICT_TAG.search(prompt).group(0)}: {verdict}", None
                if reviews and engine in fix_fails:
                    return False, None, f"{engine} fix failed (simulated)"
                coder(Path(kwargs["cwd"]), bool(reviews))
                return True, "implemented", None
            return execute

        out = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=(True, None)))
            stack.enter_context(patch("makewand.sandbox.verify_writable_sandbox_or_authorized", return_value=(True, None, None)))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value=status))
            if not real_router:
                stack.enter_context(patch.object(orch, "select_optimal_engine_pair", return_value=(
                    list(coders), list(reviewers), dict(primary_coder=coders[0], primary_reviewer=reviewers[0], reasons=[]))))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=lambda name: name in active))
            stack.enter_context(patch("makewand.config.get_active_providers", return_value=list(active)))
            stack.enter_context(patch("makewand.usage.record_engine_usage"))
            stack.enter_context(patch("makewand.memory.format_memory_hints_for_prompt", return_value=""))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, "1 passed in 0.01s")))
            for name in EXECUTORS:
                stack.enter_context(patch.object(orch, f"execute_{name}_task", side_effect=provider(name)))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(out))
            result = orch.run_workflow("Add lookup tables module", cwd=str(repo), force_code=True,
                                       total_timeout=120, **options)
        return result, out.getvalue()

    def reviews(self):
        return [engine for engine, readonly in self.calls if readonly]

    @staticmethod
    def large_module(workspace, fixing):
        (workspace / "tables.py").write_text(module_source(1200))

    @staticmethod
    def growing_module(workspace, fixing):
        """A change agy can review whole, then a fix whose re-review prompt it cannot."""
        (workspace / "tables.py").write_text(module_source(1000 if fixing else 700))

    def assert_no_placeholder_review(self, log):
        self.assertNotIn("Placeholder Provider", log)
        self.assertNotIn("CURSOR 独立红队审查完成", log)
        self.assertNotIn("改由 CURSOR", log)
        self.assertNotIn("改由 COPILOT", log)


class PlaceholderStandInTests(StandInTestCase):
    def test_usable_engine_behind_a_placeholder_takes_the_review(self):
        """standin_probe S8: active providers claude, agy, cursor, local."""
        repo = self.make_repo()
        result, log = self.run_task(repo, active=("claude", "agy", "cursor", "local"), coders=("claude",),
                                    reviewers=("agy",), health={"cursor": "unknown"}, coder=self.large_module,
                                    auto_fix=False)
        self.assertEqual(result.status, "PASSED", log)
        self.assertEqual(result.exit_code, EXIT_PASSED)
        self.assertEqual(self.reviews(), ["local"])
        self.assertIn("改由 LOCAL 接替审查", log)
        self.assert_no_placeholder_review(log)
        self.assertEqual((repo / "tables.py").read_text(), module_source(1200))

    def test_only_placeholders_left_is_unverified_for_capacity(self):
        """standin_probe S8b: the capacity reason, not a missing verdict line from cursor."""
        repo = self.make_repo()
        result, log = self.run_task(repo, active=("claude", "agy", "cursor"), coders=("claude",), reviewers=("agy",),
                                    health={"cursor": "unknown"}, coder=self.large_module, auto_fix=False)
        self.assertEqual(result.status, "UNVERIFIED", log)
        self.assertEqual(result.exit_code, EXIT_UNVERIFIED)
        self.assertEqual(self.reviews(), [])
        self.assertIn("AGY 无法完整接收审查提示", result.error)
        self.assertNotIn("结构化裁决行", result.error)
        self.assert_no_placeholder_review(log)
        self.assertFalse((repo / "tables.py").exists())

    def test_real_router_never_hands_the_review_to_a_placeholder(self):
        """standin_probe S11/S11b: cursor and copilot installed, codex limited."""
        for active, health in ((("claude", "agy", "cursor"), {"cursor": "unknown"}),
                               (("claude", "codex", "agy", "cursor", "copilot"),
                                {"cursor": "unknown", "copilot": "unknown", "codex": "limited"})):
            with self.subTest(active=active):
                self.calls = []
                repo = self.make_repo(f"repo-{len(active)}")
                result, log = self.run_task(repo, active=active, health=health, coder=self.large_module,
                                            auto_fix=False, real_router=True)
                self.assertEqual(result.status, "UNVERIFIED", log)
                self.assertEqual(self.reviews(), [])
                self.assertIn("AGY 无法完整接收审查提示", result.error)
                self.assert_no_placeholder_review(log)

    def test_cross_review_pool_from_active_providers_skips_placeholders(self):
        """No routed reviewer besides the coder: the active providers serve, never cursor."""
        repo = self.make_repo()
        result, log = self.run_task(repo, active=("claude", "cursor", "codex"), coders=("claude",),
                                    reviewers=("claude",), coder=self.large_module, auto_fix=False)
        self.assertEqual(result.status, "PASSED", log)
        self.assertEqual(self.reviews(), ["codex"])
        self.assert_no_placeholder_review(log)

    def test_re_review_pool_from_active_providers_skips_placeholders(self):
        """The fixer is the only routed reviewer: the re-review comes from the active providers."""
        repo = self.make_repo()
        result, log = self.run_task(
            repo, active=("claude", "codex", "cursor", "grok"), coders=("claude", "codex"), reviewers=("codex",),
            coder=self.growing_module, fix_fails=("claude",), auto_fix=True,
            verdicts=lambda engine, number: (False, ["[P1] the table stops at ENTRY_00699"]) if number == 1 else (True, []))
        self.assertEqual(result.status, "PASSED", log)
        self.assertEqual(self.reviews(), ["codex", "grok"])
        self.assert_no_placeholder_review(log)

    def test_stand_in_pool_excludes_engines_without_executor(self):
        cache = {name: {"status": "unknown" if name in ("cursor", "copilot") else "healthy"}
                 for name in ("claude", "codex", "agy", "cursor", "copilot", "local")}
        with patch("makewand.config.is_provider_enabled", return_value=True), \
                patch("makewand.config.get_active_providers",
                      return_value=["claude", "cursor", "copilot", "agy", "local", "codex"]):
            self.assertEqual(orch._review_stand_ins(["agy", "cursor"], {"claude"}, cache), ["agy", "local", "codex"])
        for engine in orch.ENGINES_WITHOUT_EXECUTOR:
            self.assertFalse(orch._dispatchable_reviewer(engine, {engine: {"status": "healthy"}}))


class FailedStandInTests(StandInTestCase):
    def test_failed_stand_in_is_replaced_by_the_next_one(self):
        """standin_probe S9: codex stands in for agy and fails; grok, usable, takes over."""
        repo = self.make_repo()
        result, log = self.run_task(repo, active=("claude", "codex", "agy", "grok"), coders=("claude",),
                                    reviewers=("agy",), coder=self.large_module, failing=("codex",), auto_fix=False)
        self.assertEqual(result.status, "PASSED", log)
        self.assertEqual([engine for engine, readonly in self.calls if readonly], ["codex", "grok"])
        self.assertIn("改由 CODEX 接替审查", log)
        self.assertIn("改由 GROK 接替审查", log)
        self.assertEqual((repo / "tables.py").read_text(), module_source(1200))

    def test_unverified_when_every_stand_in_fails(self):
        repo = self.make_repo()
        result, log = self.run_task(repo, active=("claude", "codex", "agy", "grok"), coders=("claude",),
                                    reviewers=("agy",), coder=self.large_module, failing=("codex", "grok"),
                                    auto_fix=False)
        self.assertEqual(result.status, "UNVERIFIED", log)
        self.assertEqual([engine for engine, readonly in self.calls if readonly], ["codex", "grok"])
        self.assertIn("AGY 无法完整接收审查提示", result.error)
        self.assertFalse((repo / "tables.py").exists())

    def test_failed_routed_reviewer_keeps_the_routed_ladder(self):
        """Only a stand-in is replaced: a routed reviewer that fails falls through to the next routed one."""
        repo = self.make_repo()
        result, log = self.run_task(repo, active=("claude", "codex", "agy", "grok"), coders=("claude",),
                                    reviewers=("codex",), coder=self.large_module, failing=("codex",), auto_fix=False)
        self.assertEqual(result.status, "UNVERIFIED", log)
        self.assertEqual([engine for engine, readonly in self.calls if readonly], ["codex"])
        self.assertNotIn("接替", log)

    def test_failed_re_review_stand_in_is_replaced(self):
        """The re-review outgrows agy; codex stands in and fails; grok re-reviews."""
        repo = self.make_repo()
        result, log = self.run_task(
            repo, active=("claude", "codex", "agy", "grok"), coders=("claude",), reviewers=("agy",),
            coder=self.growing_module, failing=("codex",), auto_fix=True,
            verdicts=lambda engine, number: (False, ["[P1] the table stops at ENTRY_00699"]) if number == 1 else (True, []))
        self.assertEqual(result.status, "PASSED", log)
        self.assertEqual([engine for engine, readonly in self.calls if readonly], ["agy", "codex", "grok"])
        self.assertIn("改由 CODEX 接替复审", log)
        self.assertIn("改由 GROK 接替复审", log)
        self.assertEqual((repo / "tables.py").read_text(), module_source(1000))


if __name__ == "__main__":
    unittest.main()
