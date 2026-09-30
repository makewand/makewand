"""Candidate writes obey the parent deadline, including lock and rollback."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import time
import unittest
from unittest.mock import patch

import test_hybrid_integrity as fixtures
from makewand import candidate, config, filelock
from makewand.candidate import CandidateManager, build_manifest
from makewand.execution_runtime import execution_context


class CandidateDeadlineTests(unittest.TestCase):
    def setUp(self):
        fixtures.HybridIntegrityTests.setUp(self)
        self.base, _, _, _ = fixtures.HybridIntegrityTests.race(self)
        self.baseline = build_manifest(self.base)

    def test_expired_apply_cannot_write_even_with_force(self):
        with execution_context(deadline_unix_ms=int(time.time() * 1000) - 1):
            success, changed, message = CandidateManager.apply_candidate("hybrid", "A", force=True)
        self.assertFalse(success)
        self.assertEqual(changed, [])
        self.assertEqual(message.status, "TIMEOUT")
        self.assertEqual(build_manifest(self.base), self.baseline)

    def test_slow_copy_rolls_back_without_marking_candidate_applied(self):
        monotonic = [100.0]
        original = candidate._atomic_copy
        copies = []
        def copy(*args, **kwargs):
            value = original(*args, **kwargs)
            copies.append(args[1])
            if len(copies) == 1:
                monotonic[0] += 11
            return value
        with patch("makewand.candidate.time.monotonic", side_effect=lambda: monotonic[0]):
            with execution_context(deadline_unix_ms=int(time.time() * 1000) + 10000):
                with patch.object(candidate, "_atomic_copy", side_effect=copy), patch.object(CandidateManager, "_mark_applied") as mark:
                    success, changed, message = CandidateManager.apply_candidate("hybrid", "A")
        self.assertFalse(success)
        self.assertEqual(changed, [])
        self.assertEqual(message.status, "TIMEOUT")
        self.assertGreaterEqual(len(copies), 2)
        mark.assert_not_called()
        self.assertEqual(build_manifest(self.base), self.baseline)

    def test_contended_apply_lock_honors_total_deadline(self):
        descriptor = os.open(config.CONFIG_DIR / "apply.lock", os.O_CREAT | os.O_RDWR, 0o600)
        try:
            filelock.flock(descriptor, filelock.LOCK_EX)
            started = time.monotonic()
            with execution_context(deadline_unix_ms=int(time.time() * 1000) + 30):
                success, changed, message = CandidateManager.apply_candidate("hybrid", "A")
            self.assertLess(time.monotonic() - started, .2)
        finally:
            filelock.flock(descriptor, filelock.LOCK_UN)
            os.close(descriptor)
        self.assertFalse(success)
        self.assertEqual(changed, [])
        self.assertEqual(message.status, "TIMEOUT")
        self.assertEqual(build_manifest(self.base), self.baseline)


if __name__ == "__main__":
    unittest.main()
