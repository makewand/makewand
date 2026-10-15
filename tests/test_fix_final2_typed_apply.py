"""apply_candidate types its outcome; the CLI and the DAG map the type, never message words (typed-apply).

cli.cmd_apply and task_dag._deliver_dag derived APPLY_CONFLICT from the substring
"冲突" in apply_candidate's message: a failed hybrid synthesis ("语义合并冲突") exited
14 as a workspace conflict, while a target that no longer matched its frozen preimage
("Application target changed since its baseline") exited 10. CandidateManager.apply_candidate
now returns an ApplyResult - still the (ok, files, message) tuple - whose status
(PASSED, APPLY_CONFLICT, TIMEOUT, UNVERIFIED, UNKNOWN, FAILED) both map to their exit
status and recorded outcome. task_dag._dag_result swallowed a ValueError from the
execution contract, so a DAG outcome could go unrecorded: it now records or raises.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import argparse
import contextlib
import copy
import io
import pickle
import unittest
from unittest.mock import patch

from makewand import candidate as candidate_module, cli, git_helper, task_dag
from makewand.candidate import CandidateManager, CandidateMessage
from makewand.execution_contract import EXIT_APPLY_CONFLICT, EXIT_FAILED, EXIT_PASSED, EXIT_UNVERIFIED
from makewand.git_helper import clone_isolated_worktree
from makewand.workflow import last_result, remember_result
from test_fix_orch_dag_r3 import DagFixture, git


class ApplyResultTests(unittest.TestCase):
    def test_it_is_the_legacy_tuple(self):
        result = candidate_module.ApplyResult(False, ["a.py"], "message")
        ok, files, message = result
        self.assertEqual((ok, files, message), (False, ["a.py"], "message"))
        self.assertEqual(result, (False, ["a.py"], "message"))
        self.assertEqual(len(result), 3)
        self.assertEqual((result.ok, result.files, result.message), (False, ["a.py"], "message"))
        typed = candidate_module.ApplyResult(False, ["a.py"], CandidateMessage("conflict", "APPLY_CONFLICT"))
        for copied in (copy.copy(typed), copy.deepcopy(typed), pickle.loads(pickle.dumps(typed))):
            self.assertEqual(copied, (False, ["a.py"], "conflict"))
            self.assertEqual(copied.status, "APPLY_CONFLICT")

    def test_status(self):
        self.assertEqual(candidate_module.ApplyResult(True, [], "applied").status, "PASSED")
        self.assertEqual(candidate_module.ApplyResult(True, [], CandidateMessage("x", "FAILED")).status, "PASSED")
        self.assertEqual(candidate_module.ApplyResult(False, [], "plain failure").status, "FAILED")
        self.assertEqual(candidate_module.ApplyResult(False, [], "检测到工作区冲突 (words only)").status, "FAILED")
        for status in ("APPLY_CONFLICT", "TIMEOUT", "UNVERIFIED", "UNKNOWN"):
            self.assertEqual(candidate_module.ApplyResult(False, [], CandidateMessage("x", status)).status, status)
        self.assertEqual(candidate_module.ApplyResult(False, [], CandidateMessage("x", "PASSED")).status, "FAILED")
        self.assertEqual(candidate_module.ApplyResult(False, [], CandidateMessage("x", "NOT_A_STATUS")).status, "FAILED")
        self.assertEqual(candidate_module.ApplyResult(False, [], "x", status="TIMEOUT").status, "TIMEOUT")


class TypedApplyTests(DagFixture):
    def setUp(self):
        super().setUp()
        self.repo = self.make_repo(files={"app.py": "A = 1\n", "lib.py": "L = 1\n"})

    def seal(self, race_id, edits):
        workspace = self.candidates / race_id / "agent_a"
        clone_isolated_worktree(str(self.repo), workspace)
        base = git(workspace, "rev-parse", "HEAD").strip()
        for name, text in edits.items():
            (workspace / name).parent.mkdir(parents=True, exist_ok=True)
            (workspace / name).write_text(text)
        CandidateManager.save_race(race_id=race_id, prompt="typed", base_cwd=str(self.repo),
                                   baseline_commit=git(self.repo, "rev-parse", "HEAD").strip(),
                                   agent_a={"model": "M", "path": str(workspace), "success": True, "test_passed": True,
                                            "review_passed": True, "baseline_commit": base, "diff": "+typed"},
                                   agent_b={}, winner="A")
        return race_id

    def cli_apply(self, race_id, **overrides):
        namespace = argparse.Namespace(race_id=race_id, candidate="A", dry_run=False, force=False, merge=False,
                                       protected_paths=None)
        vars(namespace).update(overrides)
        remember_result(None)
        with contextlib.redirect_stdout(io.StringIO()), self.assertRaises(SystemExit) as raised:
            cli.cmd_apply(namespace)
        return raised.exception.code

    def test_success_and_dry_run_are_passed(self):
        race_id = self.seal("rc_typedok001", {"app.py": "A = 2\n"})
        result = CandidateManager.apply_candidate(race_id, "A", dry_run=True)
        self.assertEqual((result.ok, result.status), (True, "PASSED"))
        self.assertEqual(self.cli_apply(race_id), EXIT_PASSED)
        self.assertEqual((self.repo / "app.py").read_text(), "A = 2\n")

    def test_a_workspace_conflict_is_apply_conflict(self):
        race_id = self.seal("rc_typedcf001", {"app.py": "A = 2\n"})
        (self.repo / "app.py").write_text("A = 3  # the user's later edit\n")
        result = CandidateManager.apply_candidate(race_id, "A")
        self.assertEqual((result.ok, result.files, result.status), (False, ["app.py"], "APPLY_CONFLICT"))
        self.assertEqual(self.cli_apply(race_id), EXIT_APPLY_CONFLICT)
        self.assertEqual(last_result().status, "APPLY_CONFLICT")

    def test_an_entry_apply_never_writes_is_apply_conflict_also_with_force(self):
        race_id = self.seal("rc_typedob001", {"new/out.py": "OUT = 1\n"})
        (self.repo / "new").write_text("a file where the candidate needs a directory\n")
        for force in (False, True):
            result = CandidateManager.apply_candidate(race_id, "A", force=force)
            self.assertEqual((result.ok, result.status), (False, "APPLY_CONFLICT"), result.message)
            self.assertEqual(self.cli_apply(race_id, force=force), EXIT_APPLY_CONFLICT)

    def test_a_target_changed_since_its_frozen_preimage_is_apply_conflict(self):
        """Words never decided: the preimage check's message is English, without 冲突."""
        race_id = self.seal("rc_typedfz001", {"app.py": "A = 2\n"})
        (self.repo / "app.py").write_text("A = 3  # changed after the conflict check\n")
        with patch.object(CandidateManager, "detect_conflicts", return_value=[]):
            result = CandidateManager.apply_candidate(race_id, "A")
            self.assertEqual((result.ok, result.files, result.status), (False, ["app.py"], "APPLY_CONFLICT"))
            self.assertIn("Application target changed since its baseline", result.message)
            self.assertEqual(self.cli_apply(race_id), EXIT_APPLY_CONFLICT)
        self.assertEqual((self.repo / "app.py").read_text(), "A = 3  # changed after the conflict check\n")

    def test_a_failed_hybrid_synthesis_is_no_workspace_conflict(self):
        race_id = self.seal("rc_typedhy001", {"app.py": "A = 2\n"})
        merge_failure = (False, None, "语义合并未通过: 语义合并冲突: app.py")
        with patch.object(CandidateManager, "_create_hybrid_candidate_locked", return_value=merge_failure):
            result = CandidateManager.apply_candidate(race_id, merge=True)
            self.assertIn("冲突", result.message)
            self.assertEqual(result.status, "FAILED")
            self.assertEqual(self.cli_apply(race_id, candidate=None, merge=True), EXIT_FAILED)
        self.assertEqual(last_result().status, "FAILED")

    def test_the_test_harness_refusal_is_unverified(self):
        race_id = self.seal("rc_typedhv001", {"app.py": "A = 2\n"})
        with patch.object(git_helper, "_makewand_source_root", return_value=self.repo.resolve()):
            result = CandidateManager.apply_candidate(race_id, "A")
            self.assertEqual(result.status, "UNVERIFIED")
            self.assertEqual(self.cli_apply(race_id), EXIT_UNVERIFIED)
        self.assertEqual(last_result().status, "UNVERIFIED")
        self.assertEqual((self.repo / "app.py").read_text(), "A = 1\n")


class DagDeliveryTypedStatusTests(DagFixture):
    def deliver(self, apply_result):
        self.runs = getattr(self, "runs", 0) + 1
        repo = self.make_repo(f"repo{self.runs}")
        with patch.object(CandidateManager, "apply_candidate", return_value=apply_result) as apply:
            ok, summary, _, _ = self.run_dag(self.code_chain(), repo)
        apply.assert_called_once()
        return ok, summary

    def test_message_words_never_make_a_conflict(self):
        ok, summary = self.deliver(candidate_module.ApplyResult(False, [], "无法合成: 语义合并冲突 (no workspace conflict)"))
        self.assertFalse(ok)
        self.assertEqual(last_result().status, "FAILED")
        self.assertRegex(summary, r"处理后可执行: makewand apply dg_[0-9a-f]{12} --candidate A")

    def test_a_typed_conflict_gets_the_conflict_guidance_without_the_word(self):
        ok, summary = self.deliver(candidate_module.ApplyResult(False, ["app.py"], CandidateMessage(
            "Application target changed since its baseline: app.py", "APPLY_CONFLICT")))
        self.assertFalse(ok)
        self.assertEqual(last_result().status, "APPLY_CONFLICT")
        self.assertRegex(summary, r"审查: makewand inspect dg_[0-9a-f]{12} --candidate A")
        self.assertNotIn("处理后可执行", summary)

    def test_unknown_and_protected_file_outcomes_keep_their_status(self):
        for status in ("UNKNOWN", "UNVERIFIED", "TIMEOUT"):
            with self.subTest(status=status):
                ok, summary = self.deliver(candidate_module.ApplyResult(False, [], CandidateMessage(f"{status} outcome", status)))
                self.assertFalse(ok)
                self.assertEqual(last_result().status, status)
                # Not the test harness refusal: the candidate can still be applied later.
                self.assertRegex(summary, r"处理后可执行: makewand apply dg_[0-9a-f]{12} --candidate A")

    def test_a_plain_tuple_from_a_stand_in_apply_is_failed(self):
        ok, _ = self.deliver((False, [], "检测到工作区冲突 (a stand-in without a type)"))
        self.assertFalse(ok)
        self.assertEqual(last_result().status, "FAILED")


class DagResultTests(unittest.TestCase):
    def test_a_nul_in_the_summary_is_recorded_escaped(self):
        remember_result(None)
        task_dag._dag_result(False, "FAILED", "path a\0b failed")
        self.assertEqual(last_result().status, "FAILED")
        self.assertEqual(last_result().error, "path a\\0b failed")

    def test_an_outcome_the_contract_refuses_raises_instead_of_vanishing(self):
        for ok, status in ((False, "NOT_A_STATUS"), (False, "PASSED")):
            with self.subTest(status=status):
                remember_result(None)
                with self.assertRaises(ValueError):
                    task_dag._dag_result(ok, status, "summary")

    def test_every_outcome_is_recorded(self):
        for ok, status in ((True, "PASSED"), (False, "APPLY_CONFLICT"), (False, "UNVERIFIED"), (True, "FAILED")):
            with self.subTest(status=status):
                remember_result(None)
                task_dag._dag_result(ok, status, "summary")
                self.assertEqual(last_result().status, status)
                self.assertEqual(last_result().success, status == "PASSED")


if __name__ == "__main__":
    unittest.main()
