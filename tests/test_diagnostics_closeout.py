"""Failure classification and approval records, with offline native fixtures."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import builtins
import io
import json
import os
import tempfile
import unittest
from datetime import datetime, timezone
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

from makewand import accounting_files, health, route_cli
from makewand.outcome_ledger import OutcomeLedger
from makewand.providers.muse import parse_muse_quota


# The archived 2026-10-28 CLI failure; synthetic request ID, no live probe.
MUSE_QUOTA = (
    "agent loop failed: model failed: API error 429 "
    "[request_id=00000000-0000-4000-8000-000000000001]: Subscription quota exhausted. "
    "Your usage window resets at 2026-10-28T10:47:45Z. (rate_limit_error)"
)


class MuseQuotaCloseoutTests(unittest.TestCase):
    def test_observed_cli_quota_is_classified_on_both_streams(self):
        for output, stderr in (("", MUSE_QUOTA), (MUSE_QUOTA, ""), (MUSE_QUOTA, None)):
            with self.subTest(stderr=stderr):
                limited, _, resets = parse_muse_quota(output, stderr)
                self.assertTrue(limited)
                self.assertEqual(health.parse_reset_datetime(resets),
                                 datetime(2026, 10, 28, 10, 47, 45, tzinfo=timezone.utc))

    def test_quota_quoted_in_an_answer_does_not_override_generic_failure(self):
        output = "I investigated this diagnostic:\n" + MUSE_QUOTA + "\nThe fix is complete."
        self.assertFalse(parse_muse_quota(output, "error: turn failed")[0])
        self.assertFalse(parse_muse_quota(output + "\nerror: turn failed")[0])

    def test_other_http_errors_do_not_match_the_observed_quota_format(self):
        for line in (MUSE_QUOTA.replace("429", "503"),
                     MUSE_QUOTA.replace("Subscription quota exhausted.", "Unrecognized model."),
                     "API error 429: request rejected"):
            with self.subTest(line=line):
                self.assertFalse(parse_muse_quota("", line)[0])

    def test_absolute_reset_preserves_zone_before_sentence_punctuation(self):
        for text, expected in (
            ("2026-10-28T10:47:45Z.", datetime(2026, 10, 28, 10, 47, 45, tzinfo=timezone.utc)),
            ("2026-10-28T10:47:45+08:00. (rate_limit_error)",
             datetime(2026, 10, 28, 2, 47, 45, tzinfo=timezone.utc)),
            ("2026-10-28T10:47:45.123456Z.",
             datetime(2026, 10, 28, 10, 47, 45, 123456, tzinfo=timezone.utc)),
        ):
            with self.subTest(text=text):
                self.assertEqual(health.parse_reset_datetime(text), expected)
        self.assertIsNone(health.parse_reset_datetime("2026-10-28.9"))


class ApprovalRecordCloseoutTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.ledger = OutcomeLedger(Path(directory.name) / "routing")

    def approve(self, **changes):
        details = dict(seconds=4, calls=1, review_validated=True, candidate_approved=True,
                       candidate_hash="a" * 64, request_id="review-closeout")
        details.update(changes)
        return self.ledger.record("codex", 3, "review", True, **details)

    def test_new_approval_requires_candidate_hash_without_writing_a_row(self):
        with self.assertRaisesRegex(ValueError, "candidate_hash"):
            self.ledger.record("codex", 3, "review", True, seconds=4, calls=1,
                               review_validated=True, candidate_approved=True,
                               request_id="review-closeout")
        self.assertEqual(list(self.ledger.records()), [])

    def test_new_approval_requires_a_linked_request(self):
        with self.assertRaisesRegex(ValueError, "request_id"):
            self.ledger.record("codex", 3, "review", True, seconds=4, calls=1,
                               review_validated=True, candidate_approved=True,
                               candidate_hash="a" * 64, infer_request_id=lambda rows: None)
        self.assertEqual(list(self.ledger.records()), [])

    def test_unique_request_inference_is_applied_before_approval_validation(self):
        row = self.ledger.record("codex", 3, "review", True, seconds=4, calls=1,
                                 review_validated=True, candidate_approved=True,
                                 candidate_hash="a" * 64, infer_request_id=lambda rows: "unique-request")
        self.assertEqual(row["request_id"], "unique-request")
        self.assertTrue(row["candidate_approved"])

    def test_explicit_approval_requires_valid_review_work_and_measured_usage(self):
        for changes in ({"review_validated": None}, {"seconds": None}, {"calls": None}):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                self.approve(**changes)
        with self.assertRaises(ValueError):
            self.ledger.record("codex", 3, "review", False, review_validated=False,
                               candidate_approved=True, candidate_hash="a" * 64,
                               request_id="failed-review", seconds=4, calls=1)
        self.assertEqual(list(self.ledger.records()), [])

    def test_complete_approval_is_recorded_once(self):
        row = self.approve()
        self.assertEqual(list(self.ledger.records()), [row])

    def test_request_inference_reads_through_the_already_locked_handle(self):
        previous = self.ledger.record("codex", 3, "test", True, request_id="previous")
        original_open = builtins.open

        def other_handle(file, *args, **kwargs):
            if isinstance(file, (str, os.PathLike)) and Path(file) == self.ledger.outcomes_path:
                raise PermissionError("Windows byte lock is held through another handle")
            return original_open(file, *args, **kwargs)

        def infer(rows):
            self.assertEqual(rows, [previous])
            return "current"

        with patch("builtins.open", side_effect=other_handle):
            row = self.ledger.record("codex", 3, "review", True, seconds=4, calls=1,
                                     review_validated=True, candidate_approved=True,
                                     candidate_hash="a" * 64, infer_request_id=infer)
        self.assertEqual(row["request_id"], "current")
        self.assertEqual(list(self.ledger.records()), [previous, row])

    def test_unreadable_locked_history_cannot_be_treated_as_empty(self):
        with patch("makewand.outcome_ledger.os.fdopen", side_effect=OSError("read unavailable")):
            with self.assertRaises(OSError):
                self.ledger.record("codex", 3, "review", True, seconds=4, calls=1,
                                   review_validated=True, candidate_approved=True,
                                   candidate_hash="a" * 64, infer_request_id=lambda rows: "current")
        self.assertEqual(list(self.ledger.records()), [])

    def test_legacy_unbound_approval_remains_readable_and_is_not_rewritten(self):
        self.ledger.directory.mkdir(parents=True, exist_ok=True)
        old = {"ts": 1, "provider": "codex", "tier": 3, "task_type": "review",
               "passed": True, "seconds": None, "calls": None, "candidate_approved": True}
        raw = (json.dumps(old) + "\n").encode()
        self.ledger.outcomes_path.write_bytes(raw)
        self.assertEqual(list(self.ledger.records()), [old])
        self.approve()
        self.assertTrue(self.ledger.outcomes_path.read_bytes().startswith(raw))
        self.assertEqual(len(list(self.ledger.records())), 2)

    def test_unlinked_work_quality_can_still_be_recorded_without_code_approval(self):
        row = self.ledger.record("codex", 3, "review", True)
        self.assertTrue(row["passed"])
        self.assertNotIn("candidate_approved", row)

    def test_cli_rejects_unbound_approval_with_actionable_message(self):
        args = SimpleNamespace(provider="codex", outcome_tier=3, task_type="review", result="pass",
                               seconds=4, calls=1, candidate_verdict="pass", candidate_hash=None,
                               request_id="review-closeout", caller="codex")
        error = io.StringIO()
        with patch.object(route_cli, "OutcomeLedger", return_value=self.ledger), \
                contextlib.redirect_stderr(error), contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(SystemExit) as raised:
                route_cli.cmd_outcome(args)
        self.assertEqual(raised.exception.code, 2)
        self.assertIn("candidate_hash", error.getvalue())
        self.assertEqual(list(self.ledger.records()), [])


class WindowsAccountingAliasCloseoutTests(unittest.TestCase):
    def test_ordinary_drive_and_unc_aliases_have_one_canonical_name(self):
        for value, expected in (
            (r"\\?\C:\state\calls.json", r"C:\state\calls.json"),
            (r"\\?\UNC\server\share\state\calls.json", r"\\server\share\state\calls.json"),
            ("//?/UNC/server/share/state/calls.json", r"\\server\share\state\calls.json"),
        ):
            with self.subTest(value=value):
                self.assertEqual(accounting_files._windows_ordinary_path(value), expected)

    def test_other_device_namespaces_and_ambiguous_names_stay_rejected(self):
        for value in (
            r"\\.\pipe\makewand", r"\\?\GLOBALROOT\Device\HarddiskVolume1\calls.json",
            r"\\?\Volume{00000000-0000-0000-0000-000000000001}\calls.json",
            r"\\?\C:relative\calls.json", r"\\?\UNC\server",
            r"\\?\C:\state\NUL", r"\\?\C:\state\calls.json:stream",
            r"\\?\C:\state\..\calls.json", r"\\?\C:\state\calls.json.",
        ):
            with self.subTest(value=value), self.assertRaises(ValueError):
                accounting_files._windows_ordinary_path(value)

    @unittest.skipUnless(os.name == "nt", "native Windows path and private-handle fixture")
    def test_native_pin_normalizes_namespace_retained_by_windows_resolve(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "calls.json"
            original_resolve = Path.resolve

            def retained_namespace(path, *args, **kwargs):
                resolved = str(original_resolve(path, *args, **kwargs))
                if not resolved.startswith("\\\\?\\"):
                    resolved = "\\\\?\\UNC\\" + resolved[2:] if resolved.startswith("\\\\") else "\\\\?\\" + resolved
                return Path(resolved)

            with patch.object(Path, "resolve", retained_namespace):
                with accounting_files.pin(target) as files:
                    self.assertFalse(str(files.path).startswith("\\\\?\\"))
                    descriptor = files.open(files.name, create=True, writable=True)
                    try:
                        os.write(descriptor, b"private fixture\n")
                    finally:
                        os.close(descriptor)
                    self.assertTrue(os.path.samefile(files.path, target))
            self.assertEqual(target.read_bytes(), b"private fixture\n")


if __name__ == "__main__":
    unittest.main()
