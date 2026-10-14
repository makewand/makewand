"""The fingerprint time budget runs out in the order the size budget admitted files (hash-time-order).

HostWorkspaceTransaction._fingerprint_unbacked_files admits task-start digests smallest
first and sampled fingerprints in turns across size classes (_fair_sample_order), but
_hash_pre_files re-sorted the admitted files into path order before reading them, so
when MAKEWAND_FINGERPRINT_SECONDS ran out (a slow disk) the files that lost their
digest or sample were the ones late in path order: a large file sorting late still
missed the sample the fair order gave it. The files are now read in the admitted order.
A fake clock stands for the slow disk: each expiry check advances it by one second.

Every repository is temporary; no provider runs.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import os
import types
import unittest
from unittest.mock import patch

from makewand import git_helper
from test_fix_delivery_host_block import HostDeliveryCase


class FakeClock:
    """time for git_helper: monotonic() advances one second per call."""

    def __init__(self):
        self.now = 1000.0
        self.calls = 0

    def monotonic(self):
        self.calls += 1
        self.now += 1.0
        return self.now


@unittest.skipUnless(git_helper.HOST_TRANSACTION_SUPPORTED, "host transactions need POSIX directory handles")
class HashTimeOrderTests(HostDeliveryCase):
    def fingerprint_again(self, txn, seconds):
        """Re-run the task-start fingerprinting with a fake clock: `seconds` expiry checks fit per budget."""
        txn.fingerprints, txn.samples, txn.digest_gaps = {}, {}, {}
        clock = FakeClock()
        with patch.dict(os.environ, {"MAKEWAND_FINGERPRINT_SECONDS": str(seconds)}), \
                patch.object(git_helper, "time", types.SimpleNamespace(monotonic=clock.monotonic)):
            txn._fingerprint_unbacked_files()
        return clock

    def test_a_large_file_late_in_path_order_keeps_its_sample_when_time_runs_out(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="node_modules/\n")
        small = {f"node_modules/a{index:02d}/f.bin": os.urandom(1024) for index in range(40)}
        big = "node_modules/zz/big.bin"  # last in path order, second in the fair order
        self.write(dict(small, **{big: os.urandom(3 * 1024 * 1024)}))
        # No file fits the per-file digest limit: each needs a sample.
        with patch.dict(os.environ, {"MAKEWAND_FINGERPRINT_FILE_LIMIT": "512"}):
            txn = self.start()
            self.addCleanup(self.rollback, txn)
            sizes = {rel: txn.pre[rel].size for rel in txn.unbacked}
            self.assertEqual(git_helper._fair_sample_order(sizes)[1], big)
            self.fingerprint_again(txn, seconds=5)
        self.assertEqual(txn.fingerprints, {})
        self.assertEqual(len(txn.samples), 5)
        self.assertIn(big, txn.samples)
        self.assertEqual(set(txn.samples), set(git_helper._fair_sample_order(sizes)[:5]))
        self.assertEqual(txn.samples[big], git_helper._file_sha256_at(str(self.root), big, sampled=True))

    def test_digests_lost_to_the_time_budget_are_the_largest_not_the_last_in_path_order(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="node_modules/\n")
        self.write({"node_modules/a/large.bin": os.urandom(100 * 1024),  # first in path order
                    "node_modules/z/s1.bin": b"1" * 10, "node_modules/z/s2.bin": b"2" * 20,
                    "node_modules/z/s3.bin": b"3" * 30})
        txn = self.start()
        self.addCleanup(self.rollback, txn)
        self.fingerprint_again(txn, seconds=3)
        self.assertEqual(set(txn.fingerprints), {"node_modules/z/s1.bin", "node_modules/z/s2.bin", "node_modules/z/s3.bin"})
        self.assertIn("MAKEWAND_FINGERPRINT_SECONDS", txn.digest_gaps["node_modules/a/large.bin"])
        # The file that lost its digest is the next in line for a sample.
        self.assertIn("node_modules/a/large.bin", txn.samples)

    def test_files_in_many_directories_are_all_read_with_bounded_handles(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="node_modules/\n")
        count = git_helper._HASH_DIRECTORY_HANDLES * 2 + 5
        files = {f"node_modules/d{index:03d}/f.txt": f"file {index}\n" for index in range(count)}
        self.write(files)
        txn = self.start()
        self.addCleanup(self.rollback, txn)
        before = len(os.listdir("/proc/self/fd")) if os.path.isdir("/proc/self/fd") else None
        txn.fingerprints, txn.samples, txn.digest_gaps = {}, {}, {}
        txn._fingerprint_unbacked_files()
        self.assertEqual(set(txn.fingerprints), set(files))
        for rel, data in files.items():
            self.assertEqual(txn.fingerprints[rel], git_helper._file_sha256_at(str(self.root), rel))
        if before is not None:
            self.assertEqual(len(os.listdir("/proc/self/fd")), before)  # every directory handle closed


if __name__ == "__main__":
    unittest.main()
