"""A reviewer that cannot receive the whole prompt is replaced, not just skipped (n3-reviewer-capacity).

agy reads a review prompt over its WHOLE_PROMPT_LIMITS entry (31 KiB) through its
own paging file tools, so its verdict never counts. The pipeline then hands the
review to the next usable reviewer under the same provider rules (cross review:
never the coder, for a re-review never the fixer either; a single-tool run keeps
its one tool) and ends UNVERIFIED only when no allowed reviewer can take the whole
prompt. Providers, routing, health, the sandbox probe and the test runner are
fakes; the pipeline runs on temporary repositories.
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

from makewand import config, orchestrator as orch
from makewand.execution_contract import EXIT_PASSED, EXIT_UNVERIFIED
from makewand.git_helper import run_git_cmd

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")


def module_source(lines):
    """About 33 bytes a line: 700 lines fit agy's whole-prompt limit, 1200 do not."""
    return "".join(f"ENTRY_{index:05d} = ({index}, 'value-{index}')\n" for index in range(lines))


class ReviewStandInTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-review-stand-in-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.calls = []
        self.repos = 0

    def make_repo(self):
        self.repos += 1
        repo = self.root / f"repo{self.repos}"
        repo.mkdir()
        (repo / "app.py").write_text("VALUE = 1\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(repo))
            self.assertEqual(code, 0, error)
        return repo

    def run_task(self, repo, *, engines, coders, reviewers, coder, verdicts=None, **options):
        """verdicts(engine) -> (pass, defects); every review passes by default."""
        def provider(engine):
            def execute(prompt, **kwargs):
                self.calls.append((engine, bool(kwargs.get("readonly"))))
                if kwargs.get("readonly"):
                    passed, defects = verdicts(engine) if verdicts else (True, [])
                    verdict = json.dumps({"pass": passed, "defects": defects}, ensure_ascii=False)
                    return True, f"Reviewed.\n{VERDICT_TAG.search(prompt).group(0)}: {verdict}", None
                coder(Path(kwargs["cwd"]))
                return True, "implemented", None
            return execute

        out = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=(True, None)))
            stack.enter_context(patch("makewand.sandbox.verify_writable_sandbox_or_authorized", return_value=(True, None, None)))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={name: {"status": "healthy"} for name in engines}))
            stack.enter_context(patch.object(orch, "select_optimal_engine_pair", return_value=(
                list(coders), list(reviewers), dict(primary_coder=coders[0], primary_reviewer=reviewers[0], reasons=[]))))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=lambda name: name in engines))
            stack.enter_context(patch("makewand.config.get_active_providers", return_value=list(engines)))
            stack.enter_context(patch("makewand.usage.record_engine_usage"))
            stack.enter_context(patch("makewand.memory.format_memory_hints_for_prompt", return_value=""))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, "1 passed in 0.01s")))
            for name in engines:
                stack.enter_context(patch.object(orch, f"execute_{name}_task", side_effect=provider(name)))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(out))
            result = orch.run_workflow("Add lookup tables module", cwd=str(repo), force_code=True,
                                       total_timeout=120, **options)
        return result, out.getvalue()

    def reviews(self):
        return [engine for engine, readonly in self.calls if readonly]

    @staticmethod
    def write_large_module(workspace):
        (workspace / "tables.py").write_text(module_source(1200))

    def growing_coder(self):
        """A first change agy can review whole, then a fix whose re-review prompt it cannot."""
        rounds = []

        def coder(workspace):
            rounds.append(workspace)
            (workspace / "tables.py").write_text(module_source(700 if len(rounds) == 1 else 1200))
        return coder

    def test_only_routed_reviewer_over_its_limit_is_replaced_by_another_provider(self):
        repo = self.make_repo()
        result, log = self.run_task(repo, engines=("claude", "codex", "agy"), coders=("claude", "codex"),
                                    reviewers=("agy",), coder=self.write_large_module, auto_fix=False)
        self.assertEqual(result.status, "PASSED", log)
        self.assertEqual(result.exit_code, EXIT_PASSED)
        self.assertEqual(self.reviews(), ["codex"])  # never agy, never the coder claude
        self.assertIn(str(orch.reviewer_prompt_limit("agy")), log)
        self.assertIn("改由 CODEX 接替审查", log)
        self.assertEqual((repo / "tables.py").read_text(), module_source(1200))

    def test_unverified_only_when_no_allowed_reviewer_can_take_the_whole_prompt(self):
        repo = self.make_repo()
        artifacts = Path(config.ARTIFACTS_DIR)
        before = set(artifacts.rglob("unverified.patch")) if artifacts.exists() else set()
        result, log = self.run_task(repo, engines=("claude", "agy"), coders=("claude",), reviewers=("agy",),
                                    coder=self.write_large_module, auto_fix=False)
        self.assertEqual(result.status, "UNVERIFIED", log)
        self.assertEqual(result.exit_code, EXIT_UNVERIFIED)
        self.assertEqual(self.reviews(), [])  # cross review: the coder never reviews itself
        self.assertIn("AGY", result.error)
        self.assertNotIn("接替", log)
        self.assertEqual(len(set(artifacts.rglob("unverified.patch")) - before), 1)
        self.assertFalse((repo / "tables.py").exists())

    def test_re_review_stand_in_when_the_fix_outgrows_the_reviewer(self):
        def verdicts(engine):
            return (False, ["[P1] the lookup table stops at ENTRY_00699"]) if len(self.reviews()) == 1 else (True, [])

        repo = self.make_repo()
        result, log = self.run_task(repo, engines=("claude", "codex", "agy"), coders=("claude", "codex"),
                                    reviewers=("agy",), coder=self.growing_coder(), verdicts=verdicts, auto_fix=True)
        self.assertEqual(result.status, "PASSED", log)
        self.assertEqual(self.reviews(), ["agy", "codex"])
        self.assertIn("改由 CODEX 接替复审", log)
        self.assertEqual((repo / "tables.py").read_text(), module_source(1200))

    def test_re_review_never_falls_back_to_the_fixer(self):
        """The fixer (claude) never re-reviews its own fix: with only agy left the result is UNVERIFIED."""
        repo = self.make_repo()
        result, log = self.run_task(repo, engines=("claude", "agy"), coders=("claude",), reviewers=("agy",),
                                    coder=self.growing_coder(), auto_fix=True,
                                    verdicts=lambda engine: (False, ["[P1] the lookup table stops at ENTRY_00699"]))
        self.assertEqual(result.status, "UNVERIFIED", log)
        self.assertEqual(self.reviews(), ["agy"])
        self.assertNotIn("接替", log)
        self.assertFalse((repo / "tables.py").exists())

    def test_single_tool_workflow_keeps_its_one_tool(self):
        """A low-risk single workflow reviews with its own tool only: agy over its limit is UNVERIFIED."""
        repo = self.make_repo()
        result, log = self.run_task(repo, engines=("agy", "codex"), coders=("agy",), reviewers=("codex",),
                                    coder=self.write_large_module, auto_fix=False, workflow="single", risk="low")
        self.assertEqual(result.status, "UNVERIFIED", log)
        self.assertEqual(self.reviews(), [])
        self.assertNotIn(("codex", False), self.calls)
        self.assertNotIn("接替", log)

    def test_stand_ins_follow_the_provider_rules(self):
        cache = {name: {"status": "healthy"} for name in ("claude", "codex", "agy")}
        cache["grok"] = {"status": "limited"}
        with patch("makewand.config.is_provider_enabled", side_effect=lambda name: name in cache), \
                patch("makewand.config.get_active_providers", return_value=["claude", "grok", "codex", "agy"]):
            # Planned reviewers first, then active providers; never an excluded or unusable engine.
            self.assertEqual(orch._review_stand_ins(["agy"], {"claude"}, cache), ["agy", "codex"])
            self.assertEqual(orch._review_stand_ins(["agy"], {"claude", "codex"}, cache), ["agy"])
            # The coder only last, and only when the caller allows self-review.
            self.assertEqual(orch._review_stand_ins(["agy"], {"claude", "codex"}, cache, self_reviewer="claude"),
                             ["agy", "claude"])


if __name__ == "__main__":
    unittest.main()
