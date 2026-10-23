"""Ledger lock waits, mirrored from Go's withLockedFile.

A deadline bounds waiting for another holder, never the transaction's own
I/O, and admission without a deadline waits for the ledger like Go's Reserve.
A fake monotonic clock that only moves while the code under test sleeps or
opens files keeps these deterministic on slow hosts.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import tempfile
import threading
import time
import types
import unittest
from contextlib import contextmanager
from pathlib import Path
from unittest.mock import patch

from makewand import accounting_files, call_budget, filelock
from makewand.providers.base import contestant_scope


class FakeClock:
    def __init__(self):
        self.now = 1000.0
        self.guard = threading.Lock()
        self.passed = threading.Event()
        self.target = float("inf")

    def monotonic(self):
        with self.guard:
            return self.now

    def advance(self, seconds):
        with self.guard:
            self.now += seconds
            if self.now >= self.target:
                self.passed.set()

    def sleep(self, _seconds):
        self.advance(1)
        time.sleep(.001)

    def module(self):
        return types.SimpleNamespace(monotonic=self.monotonic, sleep=self.sleep, time=time.time)


class LedgerLockWaitTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.ledger = Path(temporary.name) / "calls.json"
        environment = patch.dict(os.environ)
        environment.start()
        self.addCleanup(environment.stop)
        for key in ("MAKEWAND_CALL_BUDGET_FILE", "MAKEWAND_MAX_MODEL_CALLS", "MAKEWAND_LAYERED_BUDGET",
                    "MAKEWAND_MACHINE_DAILY_CALLS", "MAKEWAND_PROJECT_DAILY_CALLS", "MAKEWAND_LEASE_WAIT_SECONDS"):
            os.environ.pop(key, None)

    def reserve(self, **kwargs):
        return call_budget.reserve("synthetic", "fast", budget_file=str(self.ledger), max_model_calls=4, **kwargs)

    @contextmanager
    def held_ledger_lock(self):
        with accounting_files.pin(self.ledger) as files:
            fd = files.open(files.lock_name, create=True, writable=True)
            try:
                filelock.flock(fd, filelock.LOCK_EX | filelock.LOCK_NB)
                try:
                    yield
                finally:
                    filelock.flock(fd, filelock.LOCK_UN)
            finally:
                os.close(fd)

    def test_admission_without_a_deadline_waits_for_the_ledger(self):
        # Native Windows flake (TestMixedGoPythonProcessesCompeteForLastAccountSlot):
        # eight Go and Python processes contended for the ledger and a Python
        # child gave up after a hidden five-second budget, reporting "execution
        # deadline expired" although no deadline was set. Go's Reserve waits.
        clock = FakeClock()
        clock.target = clock.now + 60
        outcome = {}

        def admit():
            try:
                outcome["attempt"] = self.reserve()
            except call_budget.BudgetError as error:
                outcome["error"] = error
            finally:
                clock.passed.set()

        with patch.object(call_budget, "time", clock.module()):
            with self.held_ledger_lock():
                worker = threading.Thread(target=admit)
                worker.start()
                self.assertTrue(clock.passed.wait(30))
            worker.join(30)
        self.assertFalse(worker.is_alive())
        self.assertNotIn("error", outcome)
        self.assertIsInstance(outcome.get("attempt"), str)

    def test_a_deadline_spent_opening_files_still_takes_a_free_lock(self):
        clock = FakeClock()
        real_pin = accounting_files.pin

        @contextmanager
        def slow_pin(value):
            with real_pin(value) as files:
                clock.advance(10)  # Windows ACL or scanner I/O used up the budget
                yield files

        with patch.object(call_budget, "time", clock.module()), patch.object(accounting_files, "pin", slow_pin):
            attempt = self.reserve(deadline_monotonic=clock.now + 5)
        self.assertIsInstance(attempt, str)

    def test_contended_lock_still_honors_the_deadline(self):
        clock = FakeClock()
        with patch.object(call_budget, "time", clock.module()), self.held_ledger_lock():
            with self.assertRaises(call_budget.BudgetDeadlineError):
                self.reserve(deadline_monotonic=clock.now + 5)

    def test_cancelled_admission_exits_while_another_process_holds_the_lock(self):
        ready, done = threading.Event(), threading.Event()
        outcome = {}

        def admit():
            with contestant_scope("cancel-lock-wait") as scope:
                outcome["scope"] = scope
                ready.set()
                try:
                    self.reserve()
                except call_budget.BudgetError as error:
                    outcome["error"] = error
                finally:
                    done.set()

        with self.held_ledger_lock():
            worker = threading.Thread(target=admit)
            worker.start()
            self.assertTrue(ready.wait(10))
            outcome["scope"].cancel_event.set()
            exited_while_locked = done.wait(5)
        worker.join(10)
        self.assertFalse(worker.is_alive())
        self.assertTrue(exited_while_locked, "cancellation waited for the ledger holder")
        self.assertIsInstance(outcome.get("error"), call_budget.BudgetCancelledError)

    def test_cancelled_contestant_can_still_settle_its_accounting(self):
        attempt = self.reserve()
        with contestant_scope("cancelled-settlement") as scope:
            scope.cancel_event.set()
            call_budget.complete(attempt, True, 0, budget_file=str(self.ledger), max_model_calls=4)

    def test_an_expired_deadline_starts_no_io(self):
        with self.assertRaises(call_budget.BudgetDeadlineError):
            self.reserve(deadline_monotonic=time.monotonic() - 1)
        self.assertFalse(Path(str(self.ledger) + ".lock").exists())


if __name__ == "__main__":
    unittest.main()
