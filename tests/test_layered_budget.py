"""Offline three-scope admission, launch boundaries and clock regression tests."""
try:
    import _isolation
except ImportError:
    from tests import _isolation
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from makewand import call_budget as b
from makewand.execution_contract import ExecutionRequest
from makewand.execution_runtime import execute, execution_context


class LayeredBudgetTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "calls.json"
        env = patch.dict(os.environ, MAKEWAND_CALL_BUDGET_FILE=str(self.path),
                         MAKEWAND_LAYERED_BUDGET="1", MAKEWAND_MAX_MODEL_CALLS="5",
                         MAKEWAND_PROJECT_DAILY_CALLS="10", MAKEWAND_MACHINE_DAILY_CALLS="10",
                         MAKEWAND_PROJECT_ROOT=self.tmp.name, MAKEWAND_TASK_ID="task-a",
                         MAKEWAND_ACCOUNT_CONCURRENCY="100", MAKEWAND_POOL_CONCURRENCY="100")
        env.start()
        self.addCleanup(env.stop)

    def data(self):
        return json.loads(self.path.read_text())

    def test_task_limit_does_not_shrink_machine_or_other_tasks(self):
        b.reserve("fake", "fast", task_id="small", max_model_calls=1)
        with self.assertRaises(b.BudgetError):
            b.reserve("fake", "fast", task_id="small", max_model_calls=1)
        for _ in range(5):
            b.reserve("fake", "fast", task_id="large", max_model_calls=5)
        self.assertEqual(self.data()["maximum"], 10)
        self.assertEqual(self.data()["scopes"]["tasks"]["small"]["maximum"], 1)

    def test_machine_and_project_rejection_never_enter_callback(self):
        for scope in ("MAKEWAND_PROJECT_DAILY_CALLS", "MAKEWAND_MACHINE_DAILY_CALLS"):
            with self.subTest(scope=scope), patch.dict(os.environ, {scope: "1"}):
                self.path.unlink(missing_ok=True)
                b.reserve("fake", "fast", task_id="seed")
                request = ExecutionRequest(engine="fake", task_id="new", stage="repair", readonly=True)
                called = []
                result = execute(request, lambda timeout: called.append(1))
                self.assertEqual(result.status, "BUDGET_EXHAUSTED")
                self.assertEqual(called, [])

    def test_unstarted_release_and_repeated_settlement(self):
        a = b.reserve("fake", "fast", pending=True)
        b.release_unstarted(a)
        b.release_unstarted(a)
        with self.assertRaises(b.BudgetError):
            b.start(a)
        self.assertEqual(self.data()["scopes"]["tasks"]["task-a"]["used"], 0)
        a = b.reserve("fake", "fast", pending=True)
        b.start(a)
        b.release_unstarted(a)
        b.complete(a, False, 1, result_status="UNKNOWN", outcome_known=False)
        b.complete(a, True, 1, result_status="PASSED")
        self.assertEqual(self.data()["scopes"]["tasks"]["task-a"]["used"], 1)
        self.assertEqual(self.data()["attempts"][-1]["result_status"], "UNKNOWN")

    def test_midnight_shanghai_and_day_rollback(self):
        # 15:59:59 UTC, then 16:00 UTC: Shanghai midnight in the configured zone, regardless of TZ.
        with patch.dict(os.environ, MAKEWAND_MACHINE_DAILY_CALLS="2", MAKEWAND_BUDGET_TIMEZONE="Asia/Shanghai",
                        TZ="America/New_York"):
            with patch("time.time", return_value=1792079999):
                b.reserve("fake", "fast")
            with patch("time.time", return_value=1792080000):
                b.reserve("fake", "fast")
            # Back across that midnight: admitted, counting into the later day, never reset.
            with patch("time.time", return_value=1792079999):
                b.reserve("fake", "fast")
                with self.assertRaisesRegex(b.BudgetError, "budget exhausted"):
                    b.reserve("fake", "fast")
        scopes = self.data()["scopes"]
        self.assertEqual(scopes["day"], "2026-10-16")
        self.assertEqual({day: counter["used"] for day, counter in scopes["machine"].items()},
                         {"2026-10-15": 1, "2026-10-16": 2})

    def test_review_hold_cannot_be_stolen_and_midnight_rechecks_daily(self):
        with execution_context(task_id="task-a"), patch.dict(os.environ, MAKEWAND_MACHINE_DAILY_CALLS="2"):
            hold = b.reserve_capacity(1, "review")
            b.reserve("fake", "fast", task_id="other")
            with self.assertRaises(b.BudgetError):
                b.reserve("fake", "fast", task_id="other")
            b.reserve("fake", "fast", task_id="task-a", lease_id=hold, stage="review")
            b.release_capacity(hold)

    def test_exception_and_cancel_do_not_refund(self):
        for stage in ("provider", "repair", "review"):
            request = ExecutionRequest(engine="fake", task_id="task-a", stage=stage, readonly=True)
            result = execute(request, lambda timeout: (_ for _ in ()).throw(RuntimeError()))
            self.assertEqual(result.status, "UNKNOWN")
        request = ExecutionRequest(engine="fake", task_id="task-a", stage="provider", readonly=True)
        with self.assertRaises(KeyboardInterrupt):
            execute(request, lambda timeout: (_ for _ in ()).throw(KeyboardInterrupt()))
        self.assertEqual(self.data()["scopes"]["tasks"]["task-a"]["used"], 4)

    def test_hold_crossing_midnight_cannot_bypass_new_day_machine_cap(self):
        with execution_context(task_id="task-a"), patch.dict(os.environ, MAKEWAND_MACHINE_DAILY_CALLS="1"):
            with patch("time.time", return_value=1792079999):
                hold = b.reserve_capacity(1, "review", ttl_seconds=60)
            with patch("time.time", return_value=1792080000):
                a = b.reserve("fake", "fast", task_id="other")
                b.complete(a, True, 0)
                with self.assertRaises(b.BudgetError):
                    b.reserve("fake", "fast", lease_id=hold, task_id="task-a", stage="review")
        self.assertEqual(len(self.data()["attempts"]), 1)

    def test_daily_configuration_without_path_uses_shared_default(self):
        with patch.dict(os.environ, MAKEWAND_CALL_BUDGET_FILE="", MAKEWAND_CONFIG_DIR=self.tmp.name):
            a = b.reserve("fake", "fast", task_id="one")
            b.complete(a, True, 0)
            a = b.reserve("fake", "fast", task_id="two")
            b.complete(a, True, 0)
            self.assertEqual(len(json.loads((Path(self.tmp.name) / "call-budget.json").read_text())["attempts"]), 2)

    def test_day_clock_rollback_does_not_revive_expired_hold(self):
        with execution_context(task_id="task-a"):
            with patch("time.time", return_value=1792079900):
                hold = b.reserve_capacity(1, "review", ttl_seconds=1)
            with patch("time.time", return_value=1792079902):
                b.remaining_capacity()
            with patch("time.time", return_value=1792079900):
                with self.assertRaises(b.BudgetError):
                    b.reserve("fake", "fast", lease_id=hold, task_id="task-a")
