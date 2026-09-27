"""Cross-process lock semantics used by candidate, quota and state writes."""
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from makewand import filelock


class FileLockTests(unittest.TestCase):
    def test_exclusive_lock_excludes_another_process_then_unlocks(self):
        root = Path(__file__).resolve().parent.parent
        with tempfile.TemporaryDirectory() as directory:
            lock = Path(directory) / "lock"
            child = (
                "import sys; sys.path.insert(0, sys.argv[1]); from makewand import filelock; "
                "f=open(sys.argv[2], 'a+'); filelock.flock(f, filelock.LOCK_EX | filelock.LOCK_NB)"
            )
            with lock.open("a+") as held:
                filelock.flock(held, filelock.LOCK_EX)
                result = subprocess.run([sys.executable, "-I", "-c", child, str(root), str(lock)], capture_output=True)
                self.assertNotEqual(result.returncode, 0)
                filelock.flock(held, filelock.LOCK_UN)
                result = subprocess.run([sys.executable, "-I", "-c", child, str(root), str(lock)], capture_output=True)
                self.assertEqual(result.returncode, 0, result.stderr)
