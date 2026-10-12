"""A provider CLI launched by makewand must not be able to dispatch makewand again."""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import subprocess
import sys
import unittest
from pathlib import Path

import makewand.cli as cli

LAUNCHER = str(Path(__file__).resolve().parent.parent / "bin" / "makewand")


def _run(args, worker):
    env = dict(os.environ)
    env.pop(cli.PROVIDER_WORKER_ENV, None)
    if worker:
        env[cli.PROVIDER_WORKER_ENV] = "1"
    return subprocess.run([sys.executable, "-I", LAUNCHER, *args], env=env,
                          capture_output=True, text=True, timeout=60, stdin=subprocess.DEVNULL)


class TestProviderWorkerGuard(unittest.TestCase):
    def test_dispatching_commands_refused_inside_worker(self):
        for args in (["run", "x"], ["review"], ["race", "x"], ["ensemble", "x"], ["agy", "x"], ["claude", "x"]):
            with self.subTest(args=args):
                r = _run(args, worker=True)
                self.assertEqual(r.returncode, 2, r.stderr)
                self.assertIn("递归", r.stderr)

    def test_safe_commands_allowed_inside_worker(self):
        self.assertTrue({"status", "quota", "search"} <= cli.WORKER_SAFE_SUBCOMMANDS)
        self.assertFalse({"run", "review", "race", "ensemble", "probe", "models"} & cli.WORKER_SAFE_SUBCOMMANDS)

    def test_guard_inactive_without_marker(self):
        r = _run(["run", "--help"], worker=False)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertNotIn("递归", r.stderr)


if __name__ == "__main__":
    unittest.main()
