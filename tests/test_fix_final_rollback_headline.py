"""The rollback headline tells the truth, and sampled fingerprints are shared fairly across sizes (rollback-headline).

* When a re-hash is refused during a plain rollback (the fingerprint budget is
  used up), the file is listed as unverified, but the headline still said
  '已回滚本次任务的全部改动并逐项核验，工作区已恢复基线' (r4_bn_rollback.py). It
  now says that the workspace cannot be confirmed to be back at its baseline.
* Round 4 took the sampled fingerprints of undigested files from one pool,
  smallest file first, so a large file that round 3 always sampled got none once
  smaller files filled the pool, and a mere hard link added elsewhere then
  blocked the delivery (r4_c1big.py). Samples now take turns across size classes
  within the same budget.

Every repository is temporary; no provider runs.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import os
import unittest
from unittest.mock import patch

from makewand import git_helper
from test_fix_delivery_host_block import HostDeliveryCase

FULL_RESTORE = "已回滚本次任务的全部改动并逐项核验，工作区已恢复基线"


class RollbackHeadlineTests(HostDeliveryCase):
    def test_headline_does_not_claim_a_verified_baseline_when_a_file_could_not_be_rechecked(self):
        """r4_bn_rollback.py: a digested, unbacked file patched behind a hard link; the re-hash is refused."""
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="node_modules/\n")
        self.write({"node_modules/lib/index.js": "module.exports = 'good';\n"})
        txn = self.start()
        rel = "node_modules/lib/index.js"
        self.assertIn(rel, txn.fingerprints)
        (self.root / "app.py").write_text("VALUE = 2\n")
        target = self.root / rel
        info = os.stat(target)
        target.write_text("module.exports = 'EVIL';\n")
        os.utime(target, ns=(info.st_atime_ns, info.st_mtime_ns))
        os.link(target, self.base / "elsewhere.js")
        with patch.object(git_helper._DigestBudget, "expired", lambda self: "budget used up (forced)"):
            ok = self.rollback(txn)
        self.assertTrue(ok)  # nothing the rollback is responsible for failed
        self.assertTrue(txn.unverified)
        self.assertNotIn(FULL_RESTORE, self.output)
        self.assertIn("不能确认工作区已完全恢复基线", self.output)
        self.assertIn(rel, self.output)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    def test_verified_rollback_keeps_its_headline(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="node_modules/\n")
        self.write({"node_modules/lib/index.js": "module.exports = 'good';\n"})
        txn = self.start()
        (self.root / "app.py").write_text("VALUE = 2\n")
        self.assertTrue(self.rollback(txn))
        self.assertEqual(txn.unverified, [])
        self.assertIn(FULL_RESTORE, self.output)


class FairSampleTests(HostDeliveryCase):
    def test_order_takes_turns_across_size_classes(self):
        sizes = {"s1": 100, "s2": 101, "s3": 102, "m1": 5000, "m2": 6000, "big": 80 * 1024 * 1024}
        self.assertEqual(git_helper._fair_sample_order(sizes), ["s1", "m1", "big", "s2", "m2", "s3"])
        self.assertEqual(git_helper._fair_sample_order({}), [])

    def test_large_file_keeps_a_sample_and_a_hard_link_added_elsewhere_still_delivers(self):
        """r4_c1big.py: the large file got no sample once smaller files filled the pool; the hard link then blocked."""
        limits = {"MAKEWAND_FINGERPRINT_FILE_LIMIT": str(2 * 1024 * 1024),
                  "MAKEWAND_FINGERPRINT_TOTAL_LIMIT": str(2 * 1024 * 1024)}
        with patch.dict(os.environ, limits):
            self.commit({"app.py": "VALUE = 1\n"}, gitignore="node_modules/\n")
            blob = os.urandom(110 * 1024)
            self.write({f"node_modules/p{index % 8}/f{index}.bin": blob[:100 * 1024 + index] for index in range(40)})
            big = "node_modules/big/addon.node"
            self.write({big: os.urandom(3 * 1024 * 1024)})
            txn = self.start()
            self.assertNotIn(big, txn.fingerprints)  # over the per-file digest limit
            self.assertIn(big, txn.samples)
            small_sampled = [rel for rel in txn.samples if rel != big]
            self.assertTrue(small_sampled)  # the small files still get their share
            (self.root / "app.py").write_text("VALUE = 2\n")
            os.link(self.root / big, self.base / "addon-elsewhere.node")
            self.finalize(txn)
        self.assertDelivered(txn)


if __name__ == "__main__":
    unittest.main()
