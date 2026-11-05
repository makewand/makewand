"""status reads the quota snapshot `makewand-server quota` writes; it refreshes one that is old."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import argparse
import os
import subprocess
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import cli

ARGV = ("/fake/makewand-server", "quota", "--json")


class StatusQuotaRefreshTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.home = Path(temp.name)
        self.snapshot = self.home / ".cache" / "makewand" / "quota-snapshot.json"
        self.snapshot.parent.mkdir(parents=True)
        for guard in (patch("pathlib.Path.home", return_value=self.home),
                      patch("makewand.route_select.quota_argv", return_value=ARGV)):
            guard.start()
            self.addCleanup(guard.stop)

    def _refresh(self, trust="trusted", **run):
        with patch("subprocess.run", **run) as process:
            cli._refresh_official_quota(argparse.Namespace(repo_trust=trust))
        return process

    def _age(self, seconds):
        self.snapshot.write_text("{}", encoding="utf-8")
        then = time.time() - seconds
        os.utime(self.snapshot, (then, then))

    def test_an_old_snapshot_is_refreshed_with_a_short_limit(self):
        self._age(3600)
        process = self._refresh()
        process.assert_called_once()
        self.assertEqual(process.call_args.args[0], [*ARGV, "--timeout", "10s"])
        self.assertEqual(process.call_args.kwargs["timeout"], 15)

    def test_a_missing_snapshot_is_refreshed(self):
        self._refresh().assert_called_once()

    def test_a_recent_snapshot_is_left_alone(self):
        self._age(60)
        self._refresh().assert_not_called()

    def test_an_untrusted_repository_never_reaches_the_reader(self):
        self._age(3600)
        self._refresh(trust="untrusted").assert_not_called()

    def test_without_the_reader_nothing_runs(self):
        with patch("makewand.route_select.quota_argv", return_value=None):
            self._refresh().assert_not_called()

    def test_a_failed_refresh_stays_quiet(self):
        for error in (OSError("gone"), subprocess.TimeoutExpired("quota", 15)):
            self._refresh(side_effect=error)


if __name__ == "__main__":
    unittest.main()
