"""Real filesystem attacks against one pinned accounting transaction."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import stat
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import accounting_files, call_budget, filelock


class AccountingPathSecurityTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.parent = self.root / "chosen"
        self.parent.mkdir()
        self.ledger = self.parent / "calls.json"

    def reserve(self, path=None):
        return call_budget.reserve("synthetic", "fast", budget_file=path or self.ledger,
                                   max_model_calls=4, deadline_monotonic=time.monotonic() + 2)

    def symlink(self, link, target, directory=False):
        try:
            link.symlink_to(target, target_is_directory=directory)
        except OSError as error:
            if os.name == "nt" and getattr(error, "winerror", None) == 1314:
                self.skipTest("Windows symbolic links require developer mode or SeCreateSymbolicLinkPrivilege")
            raise

    def test_parent_swap_cannot_redirect_transaction(self):
        outside = self.root / "outside"
        outside.mkdir()
        sentinel = outside / self.ledger.name
        sentinel.write_bytes(b"outside-sentinel\n")
        moved = self.root / "moved-original"
        with call_budget._ledger(self.ledger, 4) as (files, data):
            if os.name == "nt":
                # Every ancestor is genuinely held without DELETE sharing.
                with self.assertRaises(OSError):
                    self.parent.rename(moved)
            else:
                self.parent.rename(moved)
                self.symlink(self.parent, outside, directory=True)
            call_budget._save(files, data)
        self.assertEqual(sentinel.read_bytes(), b"outside-sentinel\n")
        committed = self.ledger if os.name == "nt" else moved / self.ledger.name
        self.assertEqual(json.loads(committed.read_bytes()), data)

    def test_upstream_swap_cannot_redirect_transaction(self):
        child = self.parent / "nested"
        child.mkdir()
        self.ledger = child / "calls.json"
        outside = self.root / "outside"
        (outside / "nested").mkdir(parents=True)
        sentinel = outside / "nested" / "calls.json"
        sentinel.write_bytes(b"upstream-sentinel\n")
        moved = self.root / "moved-original"
        with call_budget._ledger(self.ledger, 4) as (files, data):
            if os.name == "nt":
                with self.assertRaises(OSError):
                    self.parent.rename(moved)
            else:
                self.parent.rename(moved)
                self.symlink(self.parent, outside, directory=True)
            call_budget._save(files, data)
        self.assertEqual(sentinel.read_bytes(), b"upstream-sentinel\n")
        committed = self.ledger if os.name == "nt" else moved / "nested" / "calls.json"
        self.assertEqual(json.loads(committed.read_bytes()), data)

    def test_initial_parent_and_leaf_aliases_keep_canonical_budget_identity(self):
        first = self.reserve()
        alias_parent = self.root / "parent-alias"
        self.symlink(alias_parent, self.parent, directory=True)
        leaf_alias = self.root / "ledger-alias"
        self.symlink(leaf_alias, self.ledger)
        second = self.reserve(alias_parent / self.ledger.name)
        third = self.reserve(leaf_alias)
        call_budget.complete(first, False, 0, result_status="UNKNOWN", outcome_known=False,
                             budget_file=leaf_alias, max_model_calls=4)
        data = json.loads(self.ledger.read_bytes())
        self.assertEqual([entry["id"] for entry in data["attempts"]], [first, second, third])
        self.assertEqual(data["attempts"][0]["result_status"], "UNKNOWN")
        self.assertTrue(leaf_alias.is_symlink())
        self.assertFalse(Path(str(leaf_alias) + ".lock").exists())

    def test_operator_git_directory_and_missing_nested_parents_are_supported(self):
        path = self.parent / ".git" / "operator-accounting" / "calls.json"
        identifier = self.reserve(path)
        self.assertEqual(json.loads(path.read_bytes())["attempts"][0]["id"], identifier)

    def test_hardlinked_lock_and_ledger_are_refused_without_touching_target(self):
        for name in ("calls.json.lock", "calls.json"):
            with self.subTest(name=name):
                target = self.root / (name + ".outside")
                target.write_bytes(b"outside-sentinel\n")
                if os.name != "nt":
                    target.chmod(0o644)
                before = target.stat()
                entry = self.parent / name
                os.link(target, entry)
                try:
                    with self.assertRaises(call_budget.BudgetError):
                        self.reserve()
                    self.assertEqual(target.read_bytes(), b"outside-sentinel\n")
                    self.assertEqual(stat.S_IMODE(target.stat().st_mode), stat.S_IMODE(before.st_mode))
                finally:
                    entry.unlink()

    def test_inserted_symlink_is_refused_before_replacement(self):
        outside = self.root / "outside.json"
        outside.write_bytes(b"outside-sentinel\n")
        with call_budget._ledger(self.ledger, 4) as (files, data):
            self.symlink(self.ledger, outside)
            with self.assertRaises((OSError, ValueError)):
                call_budget._save(files, data)
        self.assertEqual(outside.read_bytes(), b"outside-sentinel\n")
        self.assertEqual(list(self.parent.glob(".call-budget-*")), [])

    def test_lock_entry_replacement_is_rejected_before_any_save(self):
        if os.name == "nt":
            with call_budget._ledger(self.ledger, 4):
                with self.assertRaises(OSError):
                    Path(str(self.ledger) + ".lock").unlink()
            return
        with call_budget._ledger(self.ledger, 4) as (files, data):
            lock = Path(str(self.ledger) + ".lock")
            lock.unlink()
            lock.write_bytes(b"replacement-lock")
            with self.assertRaises(ValueError):
                call_budget._save(files, data)
        self.assertFalse(self.ledger.exists())

    def test_failure_before_publish_preserves_previous_json_and_cleans_temporary(self):
        self.reserve()
        before = self.ledger.read_bytes()
        with patch.object(accounting_files.AccountingFiles, "replace", side_effect=OSError("synthetic before publish")):
            with self.assertRaises(call_budget.BudgetError):
                self.reserve()
        self.assertEqual(self.ledger.read_bytes(), before)
        self.assertEqual(list(self.parent.glob(".call-budget-*")), [])

    def test_replaced_temporary_is_not_deleted_by_failure_cleanup(self):
        original_replace = accounting_files.AccountingFiles.replace
        moved = self.root / "original-created-temporary"
        replacements = []

        def substitute(files, name, fd):
            source = files.path.parent / name
            source.rename(moved)
            source.write_bytes(b"replacement-sentinel\n")
            replacements.append(source)
            return original_replace(files, name, fd)

        with patch.object(accounting_files.AccountingFiles, "replace", substitute):
            with self.assertRaises(call_budget.BudgetError):
                self.reserve()
        self.assertEqual(len(replacements), 1)
        self.assertEqual(replacements[0].read_bytes(), b"replacement-sentinel\n")
        self.assertFalse(self.ledger.exists())
        if os.name == "nt":
            self.assertFalse(moved.exists())  # fixed-handle deletion of our object
        else:
            self.assertEqual(json.loads(moved.read_bytes())["maximum"], 4)

    @unittest.skipIf(os.name == "nt", "Backslash is a Windows separator, not a legal Windows leaf")
    def test_literal_backslash_in_posix_operator_filename_remains_supported(self):
        path = self.parent / "operator\\budget.json"
        identifier = self.reserve(path)
        self.assertEqual(json.loads(path.read_bytes())["attempts"][0]["id"], identifier)

    def test_contended_lock_keeps_actual_caller_deadline(self):
        with accounting_files.pin(self.ledger) as files:
            fd = files.open(files.lock_name, create=True, writable=True)
            try:
                filelock.flock(fd, filelock.LOCK_EX | filelock.LOCK_NB)
                began = time.monotonic()
                with self.assertRaises(call_budget.BudgetDeadlineError):
                    call_budget.reserve("synthetic", "fast", budget_file=self.ledger,
                                        max_model_calls=4, deadline_monotonic=began + .05)
                elapsed = time.monotonic() - began
                self.assertGreaterEqual(elapsed, .04)
                self.assertLess(elapsed, .5)
            finally:
                filelock.flock(fd, filelock.LOCK_UN)
                os.close(fd)
        self.assertFalse(self.ledger.exists())

    @unittest.skipIf(os.name == "nt", "POSIX owner identity uses geteuid; Windows uses real token/DACL checks")
    def test_foreign_owner_is_refused_before_chmod(self):
        self.ledger.write_text('{"schema":1,"maximum":4,"attempts":[]}')
        self.ledger.chmod(0o644)
        with patch("makewand.accounting_files.os.geteuid", return_value=os.geteuid() + 1):
            with self.assertRaises(call_budget.BudgetError):
                self.reserve()
        self.assertEqual(stat.S_IMODE(self.ledger.stat().st_mode), 0o644)


if __name__ == "__main__":
    unittest.main()
