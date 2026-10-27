"""Admission notes ledgers other than the default one for `makewand observe`."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import call_budget, ledger_registry


class LedgerRegistryTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.config = Path(self.tmp.name) / "config"
        self.config.mkdir()
        env = patch.dict(os.environ, {"MAKEWAND_CONFIG_DIR": str(self.config)})
        env.start()
        self.addCleanup(env.stop)
        for name in ("_noted", "_announced"):
            noted = patch.object(ledger_registry, name, set())
            noted.start()
            self.addCleanup(noted.stop)

    def reserve(self, ledger):
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            attempt = call_budget.reserve("synthetic", "fast", budget_file=str(ledger), max_model_calls=3)
        call_budget.complete(attempt, True, 0.1, budget_file=str(ledger), max_model_calls=3)
        return stderr.getvalue()

    def rows(self):
        path = self.config / ledger_registry.REGISTRY_NAME
        if not path.exists():
            return []
        return [json.loads(line) for line in path.read_text().splitlines()]

    def test_non_default_ledger_is_registered_once_with_one_notice(self):
        own = Path(self.tmp.name) / "project" / "call-budget.json"
        own.parent.mkdir()
        first = self.reserve(own)
        second = self.reserve(own)
        self.assertIn("not the default", first)
        self.assertEqual(second, "")
        rows = self.rows()
        self.assertEqual([row["path"] for row in rows], [str(own.resolve())])
        self.assertEqual(ledger_registry.known_ledgers(), [str(own.resolve())])
        self.assertEqual(os.stat(self.config / ledger_registry.REGISTRY_NAME).st_mode & 0o777, 0o600)

    def test_default_ledger_is_not_registered(self):
        self.assertEqual(self.reserve(self.config / "call-budget.json"), "")
        self.assertEqual(self.rows(), [])

    def test_registry_failure_never_blocks_admission(self):
        own = Path(self.tmp.name) / "call-budget.json"
        with patch.object(ledger_registry, "registry_path", return_value=self.config):  # a directory: open fails
            self.reserve(own)
        self.assertEqual(self.rows(), [])

    def test_known_ledgers_skips_bad_lines_and_keeps_newest(self):
        alive = Path(self.tmp.name) / "alive.json"
        gone = Path(self.tmp.name) / "gone.json"
        (self.config / ledger_registry.REGISTRY_NAME).write_text(
            "not json\n"
            + json.dumps({"path": str(gone), "ts": 2}) + "\n"
            + json.dumps({"path": str(alive), "ts": 1}) + "\n"
            + json.dumps({"path": str(alive), "ts": 3}) + "\n")
        self.assertEqual(ledger_registry.known_ledgers(), [str(alive), str(gone)])

    def test_failed_registration_is_retried_and_creates_the_config_directory(self):
        own = Path(self.tmp.name) / "call-budget.json"
        with patch.object(ledger_registry, "registry_path", return_value=self.config):  # a directory
            self.reserve(own)
        self.assertEqual(self.rows(), [])
        missing = self.config / "nested"
        with patch.dict(os.environ, {"MAKEWAND_CONFIG_DIR": str(missing)}):
            self.assertEqual(self.reserve(own), "")  # the notice was already given
            self.assertTrue((missing / ledger_registry.REGISTRY_NAME).is_file())

    @unittest.skipUnless(hasattr(os, "mkfifo"), "needs FIFOs")
    def test_fifo_registry_and_ledger_never_block(self):
        os.mkfifo(self.config / ledger_registry.REGISTRY_NAME)
        own = Path(self.tmp.name) / "call-budget.json"
        self.reserve(own)  # returns: the FIFO is opened non-blocking and refused
        self.assertEqual(ledger_registry.known_ledgers(), [])
        os.unlink(self.config / ledger_registry.REGISTRY_NAME)
        fifo = Path(self.tmp.name) / "fifo.json"
        os.mkfifo(fifo)
        (self.config / ledger_registry.REGISTRY_NAME).write_text(json.dumps({"path": str(fifo), "ts": 1}) + "\n")
        self.assertEqual(ledger_registry.recent_activity(), [])

    def test_oversized_ledger_is_skipped(self):
        big = Path(self.tmp.name) / "big.json"
        with open(big, "wb") as handle:
            handle.truncate(ledger_registry._MAX_READ + 1)
        (self.config / ledger_registry.REGISTRY_NAME).write_text(json.dumps({"path": str(big), "ts": 1}) + "\n")
        self.assertEqual(ledger_registry.recent_activity(), [])

    def test_recent_activity_summarizes_registered_ledgers(self):
        own = Path(self.tmp.name) / "call-budget.json"
        own.write_text(json.dumps({"attempts": [
            {"engine": "muse", "task_id": "t1", "started_at": 1000.0, "result_status": "PASSED"},
            {"engine": "agy", "task_id": "t1", "started_at": 1500.0, "result_status": "UNKNOWN"},
            {"engine": "codex", "task_id": "old", "started_at": 10.0},
            {"engine": "claude", "started_at": None},
        ]}))
        (self.config / ledger_registry.REGISTRY_NAME).write_text(json.dumps({"path": str(own), "ts": 1}) + "\n")
        self.assertEqual(ledger_registry.recent_activity(window_seconds=1000, now=1900.0), [{
            "path": str(own), "calls": 2, "tasks": ["t1"], "engines": ["agy", "muse"], "abnormal": 1}])


if __name__ == "__main__":
    unittest.main()
