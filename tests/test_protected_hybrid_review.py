"""Hybrid review must validate task constraints before spending a review call."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import unittest
from pathlib import Path
from unittest.mock import patch

import test_protected_candidate as fixtures
from makewand import candidate, orchestrator
from makewand.candidate import CandidateManager, CandidateMessage, build_manifest
from makewand.execution_contract import EXIT_PASSED, EXIT_TIMEOUT, EXIT_UNVERIFIED


@unittest.skipUnless(os.name == "posix" and hasattr(os, "O_NOFOLLOW"), "POSIX protection contract")
class ProtectedHybridReviewTests(unittest.TestCase):
    def setUp(self):
        fixtures.ProtectedCandidateTests.setUp(self)
        fixtures.ProtectedCandidateTests.save(self)
        ok, merged, message = fixtures.ProtectedCandidateTests.synthesize(self)
        self.assertTrue(ok, message)
        self.merged = merged
        self.merged_path = Path(merged["path"])

    def replace_meta(self, race):
        fixtures.ProtectedCandidateTests.replace_meta(self, race)

    def review(self):
        captured = io.StringIO()
        with contextlib.redirect_stdout(captured):
            code = orchestrator._review_saved_hybrid_impl("protected", output_json=True)
        return code, json.loads(captured.getvalue())

    def assert_preflight_rejected(self):
        before = build_manifest(self.base)
        with patch.object(orchestrator, "run_review") as dispatch, \
             patch.object(CandidateManager, "approve_hybrid_candidate") as approve:
            code, report = self.review()
        self.assertEqual(code, EXIT_UNVERIFIED, report)
        self.assertFalse(report["pass"])
        self.assertTrue(report.get("error"))
        dispatch.assert_not_called()
        approve.assert_not_called()
        self.assertEqual(build_manifest(self.base), before)
        self.assertIsNone(CandidateManager.get_race("protected")["candidates"]["M"]["review_passed"])

    def reseal_ordinary_candidate_evidence(self):
        race = CandidateManager.get_race("protected")
        merged = race["candidates"]["M"]
        merged["manifest"] = build_manifest(self.merged_path)
        merged["input_manifest"] = candidate._input_manifest(self.merged_path)
        merged["changes"] = candidate.get_candidate_files_changed(self.merged_path, merged["baseline_commit"])
        self.replace_meta(race)

    def test_invalid_saved_protection_metadata_rejects_before_review_dispatch(self):
        race = CandidateManager.get_race("protected")
        for metadata in (None, {}, {"schema": 1, "files": {"locked.txt": {"sha256": "bad", "mode": 0o640}}}):
            with self.subTest(metadata=metadata):
                race["protected_files"] = metadata
                self.replace_meta(race)
                self.assert_preflight_rejected()

    def test_resealed_candidate_protected_bytes_reject_before_review_dispatch(self):
        (self.merged_path / "locked.txt").write_text("candidate changed protected task input\n")
        self.reseal_ordinary_candidate_evidence()
        self.assert_preflight_rejected()

    def test_resealed_candidate_protected_mode_rejects_before_review_dispatch(self):
        (self.merged_path / "locked.txt").chmod(0o600)
        self.reseal_ordinary_candidate_evidence()
        self.assert_preflight_rejected()

    def test_original_destination_protected_bytes_reject_before_review_dispatch(self):
        (self.base / "locked.txt").write_text("external destination changed after merge\n")
        self.assert_preflight_rejected()

    def test_original_destination_protected_mode_rejects_before_review_dispatch(self):
        (self.base / "locked.txt").chmod(0o600)
        self.assert_preflight_rejected()

    def test_approval_protection_timeout_retains_typed_exit_status(self):
        def successful_review(**kwargs):
            print(json.dumps({"pass": True, "engine": "codex", "exit_code": EXIT_PASSED,
                              "raw_summary": 'MAKEWAND_VERDICT: {"pass": true, "defects": []}'}))
            return EXIT_PASSED
        before = build_manifest(self.base)
        with patch.object(orchestrator, "run_review", side_effect=successful_review) as dispatch, \
             patch.object(CandidateManager, "approve_hybrid_candidate", return_value=(
                 False, CandidateMessage("protected file hashing exceeded deadline", "TIMEOUT"))) as approve:
            code, report = self.review()
        dispatch.assert_called_once()
        approve.assert_called_once()
        self.assertEqual(code, EXIT_TIMEOUT, report)
        self.assertEqual(report["exit_code"], EXIT_TIMEOUT)
        self.assertFalse(report["pass"])
        self.assertEqual(build_manifest(self.base), before)
        self.assertIsNone(CandidateManager.get_race("protected")["candidates"]["M"]["review_passed"])


if __name__ == "__main__":
    unittest.main()
