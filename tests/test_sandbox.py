"""
Unit tests for bubblewrap sandbox bridge.
"""

import os
import unittest
import tempfile
from pathlib import Path
from makewand.sandbox import is_bwrap_available, wrap_bwrap, run_in_sandbox

class TestSandbox(unittest.TestCase):
    def test_bwrap_availability(self):
        self.assertTrue(is_bwrap_available())

    def test_wrap_bwrap_command_structure(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            wrapped = wrap_bwrap(["echo", "hello"], workspace=tmpdir, allow_network=False)
            self.assertIn("--ro-bind", wrapped)
            self.assertIn("--bind", wrapped)
            self.assertIn("--unshare-net", wrapped)
            self.assertIn("echo", wrapped)

    def test_run_in_sandbox_execution(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            ret, out, err, ex = run_in_sandbox(["echo", "sandbox_active"], workspace=tmpdir)
            self.assertEqual(ret, 0)
            self.assertIn("sandbox_active", out)

            # Test write confinement: writing inside workspace succeeds
            test_file = os.path.join(tmpdir, "test.txt")
            ret, _, _, _ = run_in_sandbox(["bash", "-c", f"echo secret > {test_file}"], workspace=tmpdir)
            self.assertEqual(ret, 0)
            self.assertTrue(os.path.exists(test_file))

if __name__ == "__main__":
    unittest.main()
