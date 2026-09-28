"""
G3 reliability regressions: bounded REPL history (runtime-state#1).

The history file lived at ~/.config/makewand/history regardless of
MAKEWAND_CONFIG_DIR, had no length limit, and was loaded/rewritten whole on
every start (a 909 MB file cost ~6 GB RSS and triggered an OOM kill).
"""

import os
import stat
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.interactive as interactive
from makewand.interactive import setup_readline

HISTORY_MAX_ENTRIES = 1000
HISTORY_MAX_BYTES = 5 * 1024 * 1024


def get_history_file():
    return interactive.get_history_file()


def compact_history_file(path):
    return interactive.compact_history_file(path)


def _read_tail_lines(path, max_lines, read_limit):
    return interactive._read_tail_lines(path, max_lines, read_limit)


class _FakeReadline:
    def __init__(self):
        self.length = None
        self.read_sizes = []
        self.written = []

    def set_history_length(self, n):
        self.length = n

    def read_history_file(self, path):
        self.read_sizes.append(os.path.getsize(path))

    def write_history_file(self, path):
        self.written.append(path)
        Path(path).write_text("\n".join(f"entry {i}" for i in range(self.length or 5)) + "\n")

    def __getattr__(self, name):  # completer plumbing
        return lambda *a, **k: None


class TestBoundedHistory(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-g3-history-")
        self.cfg = Path(self._tmp.name) / "cfg"
        p = patch.object(config, "CONFIG_DIR", self.cfg)
        p.start()
        self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        self.registered = []
        for target, value in (("_readline_initialized", False),):
            q = patch.object(interactive, target, value)
            q.start()
            self.addCleanup(q.stop)
        r = patch.object(interactive.atexit, "register", side_effect=self.registered.append)
        r.start()
        self.addCleanup(r.stop)

    def _write_big_history(self, lines):
        self.cfg.mkdir(parents=True, exist_ok=True)
        path = self.cfg / "history"
        block = "".join(f"prompt line {i:07d} ....................\n" for i in range(10000))
        with open(path, "w") as f:
            for _ in range(lines // 10000):
                f.write(block)
        return path

    def test_history_path_follows_config_dir(self):
        self.assertEqual(get_history_file(), self.cfg / "history")
        self.assertEqual(interactive.HISTORY_MAX_ENTRIES, HISTORY_MAX_ENTRIES)
        self.assertEqual(interactive.HISTORY_MAX_BYTES, HISTORY_MAX_BYTES)

    def test_oversized_history_is_compacted_before_loading(self):
        path = self._write_big_history(200000)  # ~8 MB
        self.assertGreater(path.stat().st_size, HISTORY_MAX_BYTES)
        fake = _FakeReadline()
        with patch.object(interactive, "readline", fake):
            setup_readline()
        self.assertEqual(fake.length, HISTORY_MAX_ENTRIES)
        self.assertTrue(fake.read_sizes, "history must still be loaded after compaction")
        self.assertLessEqual(max(fake.read_sizes), HISTORY_MAX_BYTES)
        lines = path.read_text().splitlines()
        self.assertEqual(len(lines), HISTORY_MAX_ENTRIES)
        self.assertEqual(lines[-1], "prompt line 0009999 ....................")
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)

        # Exit hook rewrites a bounded file with private permissions.
        self.assertEqual(len(self.registered), 1)
        self.registered[0]()
        self.assertEqual(fake.written, [str(path)])
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)

    def test_small_history_is_left_alone(self):
        self.cfg.mkdir(parents=True, exist_ok=True)
        path = self.cfg / "history"
        path.write_text("a\nb\n")
        self.assertFalse(compact_history_file(path))
        self.assertEqual(path.read_text(), "a\nb\n")

    def test_tail_reader_is_memory_bounded(self):
        path = self._write_big_history(100000)  # ~4 MB
        lines = _read_tail_lines(path, max_lines=10**9, read_limit=256 * 1024)
        self.assertLessEqual(sum(len(l) + 1 for l in lines), 256 * 1024)
        self.assertEqual(lines[-1], b"prompt line 0009999 ....................")

    def test_real_readline_respects_limit(self):
        try:
            import readline
        except ImportError:
            self.skipTest("readline unavailable")
        path = self._write_big_history(200000)
        with patch.object(interactive, "readline", readline):
            try:
                setup_readline()
                self.assertEqual(readline.get_history_length(), HISTORY_MAX_ENTRIES)
                self.assertLessEqual(readline.get_current_history_length(), HISTORY_MAX_ENTRIES)
            finally:
                readline.clear_history()
                readline.set_history_length(-1)
        self.assertLessEqual(path.stat().st_size, HISTORY_MAX_BYTES)


if __name__ == "__main__":
    unittest.main()
