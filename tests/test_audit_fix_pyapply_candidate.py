"""[H7-py]/[H8] Candidate publication is the concurrency check, and backups are durable."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import candidate, config
from makewand.candidate import CandidateManager, build_manifest, file_record

POSIX = os.name == "posix"


@unittest.skipUnless(POSIX, "POSIX publication path")
class AtomicCopyConcurrencyTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="makewand-pyapply-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.workspace = self.root / "workspace"
        self.workspace.mkdir()
        self.source = self.root / "candidate.txt"
        self.source.write_text("candidate\n")

    def leftovers(self):
        return sorted(p.name for p in self.workspace.iterdir() if p.name.startswith(".makewand-"))

    def modes(self):
        """Run every scenario with renameat2 and with the portable fallback."""
        yield "renameat2" if candidate._renameat2_function() is not None else "unavailable"
        yield "fallback"

    def run_mode(self, mode):
        if mode == "fallback":
            return patch.object(candidate, "_renameat2", return_value=False)
        return patch.object(candidate, "_RENAMEAT2", candidate._RENAMEAT2)

    def test_new_file_does_not_clobber_concurrently_created_target(self):
        for mode in self.modes():
            with self.subTest(mode=mode):
                target = self.workspace / ("new-" + mode + ".txt")

                def create_concurrently():
                    target.write_text("user wrote this first\n")

                with self.run_mode(mode), self.assertRaisesRegex(ValueError, "changed concurrently"):
                    candidate._atomic_copy(str(self.workspace), target.name, self.source,
                                           before_replace=create_concurrently, expected_preimage=None)
                self.assertEqual(target.read_text(), "user wrote this first\n")
                self.assertEqual(self.leftovers(), [])

    def test_new_file_is_published_when_absent(self):
        for mode in self.modes():
            with self.subTest(mode=mode):
                name = "fresh-" + mode + ".txt"
                with self.run_mode(mode):
                    candidate._atomic_copy(str(self.workspace), name, self.source, expected_preimage=None)
                self.assertEqual((self.workspace / name).read_text(), "candidate\n")
                self.assertEqual(self.leftovers(), [])

    def test_existing_target_replaced_only_when_displaced_file_matches_preimage(self):
        for mode in self.modes():
            with self.subTest(mode=mode):
                target = self.workspace / ("same-" + mode + ".txt")
                target.write_text("original\n")
                preimage = file_record(target)
                with self.run_mode(mode):
                    candidate._atomic_copy(str(self.workspace), target.name, self.source,
                                           expected_preimage=preimage)
                self.assertEqual(target.read_text(), "candidate\n")
                self.assertEqual(self.leftovers(), [])

    def test_concurrent_edit_keeps_user_content_and_reports_candidate_conflict_path(self):
        if candidate._renameat2_function() is None:
            self.skipTest("renameat2 unavailable")
        target = self.workspace / "edited.txt"
        target.write_text("original\n")
        preimage = file_record(target)

        def edit_concurrently():
            target.write_text("newer user edit\n")

        with self.assertRaisesRegex(ValueError, r"changed concurrently.*preserved at (\.makewand-conflict-[0-9a-f]{32})") as raised:
            candidate._atomic_copy(str(self.workspace), target.name, self.source,
                                   before_replace=edit_concurrently, expected_preimage=preimage)
        conflict = str(raised.exception).rsplit("preserved at ", 1)[1]
        self.assertEqual(target.read_text(), "newer user edit\n")
        self.assertEqual((self.workspace / conflict).read_text(), "candidate\n")
        self.assertEqual(self.leftovers(), [conflict])

    def test_exchange_detects_edit_landing_after_the_last_userspace_check(self):
        """Edits racing the old verify-then-replace window are caught after the swap."""
        if candidate._renameat2_function() is None:
            self.skipTest("renameat2 unavailable")
        target = self.workspace / "late.txt"
        target.write_text("original\n")
        preimage = file_record(target)
        real = candidate._renameat2
        calls = []

        def edit_then_exchange(directory, source, destination, flags):
            if not calls:
                target.write_text("late user edit\n")
            calls.append(flags)
            return real(directory, source, destination, flags)

        with patch.object(candidate, "_renameat2", side_effect=edit_then_exchange), \
                self.assertRaisesRegex(ValueError, "changed concurrently"):
            candidate._atomic_copy(str(self.workspace), target.name, self.source, expected_preimage=preimage)
        self.assertEqual(calls, [candidate._RENAME_EXCHANGE, candidate._RENAME_EXCHANGE])
        self.assertEqual(target.read_text(), "late user edit\n")

    def test_vanished_target_is_a_conflict_not_a_creation(self):
        for mode in self.modes():
            with self.subTest(mode=mode):
                target = self.workspace / ("gone-" + mode + ".txt")
                target.write_text("original\n")
                preimage = file_record(target)
                with self.run_mode(mode), self.assertRaisesRegex(ValueError, "changed concurrently"):
                    candidate._atomic_copy(str(self.workspace), target.name, self.source,
                                           before_replace=target.unlink, expected_preimage=preimage)
                self.assertFalse(target.exists())
                self.assertEqual(self.leftovers(), [])

    def test_before_replace_errors_are_not_swallowed(self):
        def broken_hook():
            raise TypeError("hook bug")

        with self.assertRaisesRegex(TypeError, "hook bug"):
            candidate._atomic_copy(str(self.workspace), "hook.txt", self.source, before_replace=broken_hook)
        self.assertFalse((self.workspace / "hook.txt").exists())
        self.assertEqual(self.leftovers(), [])

    def test_unsupported_renameat2_errnos_fall_back(self):
        import errno as errno_module

        class Failing:
            def __call__(self, *args):
                return -1

        for code in (errno_module.ENOSYS, errno_module.EINVAL):
            with self.subTest(errno=code), patch.object(candidate, "_RENAMEAT2", [Failing()]), \
                    patch("ctypes.get_errno", return_value=code):
                self.assertFalse(candidate._renameat2(-100, "a", "b", candidate._RENAME_EXCHANGE))


@unittest.skipUnless(POSIX, "POSIX backup durability path")
class BackupDurabilityTests(unittest.TestCase):
    def setUp(self):
        import test_hybrid_integrity as fixtures
        fixtures.HybridIntegrityTests.setUp(self)
        self.base, _, _, _ = fixtures.HybridIntegrityTests.race(self)
        self.baseline = build_manifest(self.base)

    def test_backup_ancestor_directories_are_fsynced(self):
        synced = []
        real = candidate._fsync_directory

        def record(path):
            synced.append(Path(path))
            return real(path)

        with patch.object(candidate, "_fsync_directory", side_effect=record):
            success, _, message = CandidateManager.apply_candidate("hybrid", "A")
        self.assertTrue(success, message)
        self.assertIn(Path(config.BACKUPS_DIR), synced)
        backup_dirs = [p for p in Path(config.BACKUPS_DIR).iterdir() if p.is_dir()]
        self.assertEqual(len(backup_dirs), 1)
        # preimages/ was created for the backup: its entry lives in the backup dir.
        self.assertIn(backup_dirs[0], synced)
        self.assertIn(backup_dirs[0] / "preimages", synced)

    def test_backup_directory_fsync_failure_aborts_and_rolls_back(self):
        real = candidate._fsync_directory

        def fail_inside_backup(path):
            if Path(path).name == "preimages":
                raise OSError(5, "Input/output error")
            return real(path)

        with patch.object(candidate, "_fsync_directory", side_effect=fail_inside_backup):
            success, changed, message = CandidateManager.apply_candidate("hybrid", "A")
        self.assertFalse(success)
        self.assertEqual(changed, [])
        self.assertIn("Input/output error", str(message))
        self.assertEqual(build_manifest(self.base), self.baseline)


if __name__ == "__main__":
    unittest.main()
