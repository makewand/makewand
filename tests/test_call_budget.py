"""Admission budget regressions, with real model CLIs blocked by isolation."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import concurrent.futures
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from makewand.call_budget import BudgetError, complete, reserve


class CallBudgetTests(unittest.TestCase):
    def test_concurrent_dispatches_never_exceed_budget(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
            "MAKEWAND_CALL_BUDGET_FILE": str(Path(directory) / "budget.json"),
            "MAKEWAND_MAX_MODEL_CALLS": "3",
        }):
            def attempt(_):
                try:
                    identifier = reserve("codex", "fast")
                    complete(identifier, False, .1)
                    return True
                except BudgetError:
                    return False
            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
                outcomes = list(pool.map(attempt, range(20)))
            self.assertEqual(sum(outcomes), 3)
            ledger = json.loads(Path(os.environ["MAKEWAND_CALL_BUDGET_FILE"]).read_text())
            self.assertEqual(len(ledger["attempts"]), 3)
            self.assertTrue(all(entry["status"] == "completed" for entry in ledger["attempts"]))

    def test_independent_processes_share_one_hard_limit(self):
        import subprocess
        import sys
        code = """import os, sys
sys.path.insert(0, sys.argv[1])
from makewand.call_budget import BudgetError, reserve, complete
try:
    attempt = reserve('codex', 'fast')
    complete(attempt, False, .01)
except BudgetError:
    raise SystemExit(2)
"""
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "budget.json"
            environment = dict(os.environ, MAKEWAND_CALL_BUDGET_FILE=str(path), MAKEWAND_MAX_MODEL_CALLS="3")
            processes = [subprocess.Popen([sys.executable, "-I", "-c", code, str(Path(__file__).resolve().parent.parent)],
                                          env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE) for _ in range(8)]
            statuses = []
            for process in processes:
                output, errors = process.communicate(timeout=15)
                self.assertIn(process.returncode, (0, 2), errors.decode())
                statuses.append(process.returncode)
            self.assertEqual(statuses.count(0), 3)
            self.assertEqual(len(json.loads(path.read_text())["attempts"]), 3)

    def test_failed_reservation_stops_provider_before_invocation(self):
        from makewand.orchestrator import dispatch_task
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
            "MAKEWAND_CALL_BUDGET_FILE": str(Path(directory) / "budget.json"),
            "MAKEWAND_MAX_MODEL_CALLS": "1",
        }), patch("makewand.config.is_provider_enabled", return_value=True), patch("makewand.orchestrator.execute_codex_task", return_value=(False, "", "unavailable")) as execute:
            self.assertFalse(dispatch_task("codex", "first")[0])
            self.assertFalse(dispatch_task("codex", "second")[0])
            self.assertEqual(execute.call_count, 1)

    def test_budget_cannot_be_increased_by_a_later_client(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
            "MAKEWAND_CALL_BUDGET_FILE": str(Path(directory) / "budget.json"),
            "MAKEWAND_MAX_MODEL_CALLS": "1",
        }):
            reserve("claude", "fast")
            os.environ["MAKEWAND_MAX_MODEL_CALLS"] = "12"
            with self.assertRaises(BudgetError):
                reserve("claude", "fast")

    def test_direct_cli_and_live_health_probe_share_admission(self):
        from makewand.cli import _execute_direct_task
        from makewand.health import _run_model_probe
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
            "MAKEWAND_CALL_BUDGET_FILE": str(Path(directory) / "budget.json"),
            "MAKEWAND_MAX_MODEL_CALLS": "1",
        }), patch("makewand.health.run_subprocess") as probe:
            execute = lambda prompt, **kwargs: (False, "", "failed")
            self.assertFalse(_execute_direct_task("claude", execute, "first")[0])
            self.assertNotEqual(_run_model_probe("codex", "unused", 1)[0], 0)
            probe.assert_not_called()
