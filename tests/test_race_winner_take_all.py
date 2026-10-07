"""
Unit tests for Winner-Take-All Fast Cancellation during concurrent race (Fair12 optimization).
Verifies:
1. Low-level contestant_scope and cancel_contestant fast SIGTERM process cancellation.
2. makewand race Winner-Take-All: contestant A completes and passes verification gates,
   immediately sending SIGTERM cancellation to competing contestant B to save weekly quota.
3. Verification gate filtering: a contestant that completes first but fails the test gate
   does NOT cancel the competitor.
"""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import sys
import time
import subprocess
import threading
import unittest
from pathlib import Path
from unittest.mock import patch, MagicMock

import test_workflow_scheduling as scheduling
from makewand import orchestrator as orch, config
from makewand.candidate import CandidateManager
from makewand.providers.base import (
    contestant_scope,
    cancel_contestant,
    run_subprocess,
    get_current_contestant_scope,
)


class TestRaceWinnerTakeAll(unittest.TestCase):
    fixtures = scheduling.SchedulingTests.fixtures

    def setUp(self):
        scheduling.SchedulingTests.setUp(self)
        self._usage_patch = patch("makewand.usage.record_engine_usage")
        self._usage_patch.start()

    def tearDown(self):
        self._usage_patch.stop()
        scheduling.SchedulingTests.tearDown(self)

    def test_contestant_scope_and_fast_process_termination(self):
        """
        Verify that cancel_contestant immediately sends SIGTERM to the running process
        registered in contestant_scope, terminating a 10s process within < 0.2s.
        """
        scope_id = "test_racer_scope_1"
        started_event = threading.Event()
        result_holder = {}

        def run_racer():
            with contestant_scope(scope_id):
                started_event.set()
                # Run a sleep process that would normally take 10 seconds
                t0 = time.monotonic()
                ret, out, err, exc = run_subprocess(["sleep", "10"], timeout=15)
                elapsed = time.monotonic() - t0
                result_holder["elapsed"] = elapsed
                result_holder["ret"] = ret
                result_holder["exc"] = exc

        th = threading.Thread(target=run_racer)
        th.start()
        started_event.wait(timeout=2.0)
        time.sleep(0.05)  # allow process to start

        # Send fast cancellation
        t_cancel = time.monotonic()
        cancelled = cancel_contestant(scope_id)
        self.assertTrue(cancelled)

        th.join(timeout=2.0)
        self.assertFalse(th.is_alive(), "Contestant thread must terminate quickly after cancel")
        self.assertLess(result_holder.get("elapsed", 99), 1.0, "Process must terminate in under 1s, not 10s")
        self.assertEqual(getattr(result_holder.get("exc"), "execution_status", None), "CANCELLED")

    def test_race_winner_take_all_cancels_running_competitor(self):
        """
        In concurrent race, contestant A finishes first and passes verification gate.
        Contestant B (slow) should be cancelled via SIGTERM and A declared winner directly.
        """
        candidates = self.root / "candidates"
        b_was_cancelled = threading.Event()

        def mock_provider(engine):
            def execute(prompt, **kwargs):
                self.calls.append(engine)
                ws = Path(kwargs["cwd"])
                if engine == "claude":
                    # Fast candidate A: finishes quickly with valid code
                    (ws / "app.py").write_text("VALUE = 42\n")
                    return True, "implemented A", None
                else:
                    # Slow candidate B: simulate long work by checking scope cancellation
                    scope = get_current_contestant_scope()
                    # Wait up to 5 seconds, or until cancelled
                    for _ in range(50):
                        if scope and scope.cancel_event.is_set():
                            b_was_cancelled.set()
                            return False, None, "cancelled by competitor win"
                        time.sleep(0.05)
                    (ws / "app.py").write_text("VALUE = 99\n")
                    return True, "implemented B", None
            return execute

        with self.fixtures(), \
             patch.object(orch, "CANDIDATES_DIR", candidates), \
             patch.object(config, "CANDIDATES_DIR", candidates), \
             patch.object(orch, "execute_claude_task", side_effect=mock_provider("claude")), \
             patch.object(orch, "execute_codex_task", side_effect=mock_provider("codex")), \
             patch.object(orch, "run_local_tests", return_value=(True, "1 passed")):

            t0 = time.monotonic()
            code = orch.run_race(
                "Implement value 42",
                cwd=str(self.repo),
                engine_a="claude",
                engine_b="codex",
                timeout=30,
            )
            elapsed = time.monotonic() - t0

            self.assertEqual(code, 0)
            self.assertTrue(b_was_cancelled.is_set(), "Contestant B should have received cancellation signal")
            self.assertLess(elapsed, 2.5, "Winner-Take-All race must finish fast without waiting for slow B")

            race = CandidateManager.get_race()
            self.assertIsNotNone(race)
            self.assertEqual(race["winner"], "A")
            self.assertTrue(race["candidates"]["A"]["success"])

    def test_failed_gate_does_not_cancel_competitor(self):
        """
        If contestant A finishes first but FAILS verification gate (tests fail),
        fast cancellation must NOT be sent to B. Contestant B should complete and win.
        """
        candidates = self.root / "candidates"

        def mock_tests(cwd, **_):
            # If checking candidate A (wt_a): fail!
            if "agent_a" in str(cwd):
                return False, "Agent A test failure"
            # If checking candidate B (wt_b): pass!
            return True, "Agent B test pass"

        def mock_provider(engine):
            def execute(prompt, **kwargs):
                ws = Path(kwargs["cwd"])
                if engine == "claude":
                    # A finishes fast but writes buggy code
                    (ws / "app.py").write_text("VALUE = BUGGY\n")
                    return True, "implemented buggy A", None
                else:
                    # B finishes slightly later with good code
                    time.sleep(0.2)
                    (ws / "app.py").write_text("VALUE = GOOD_B\n")
                    return True, "implemented good B", None
            return execute

        with self.fixtures(), \
             patch.object(orch, "CANDIDATES_DIR", candidates), \
             patch.object(config, "CANDIDATES_DIR", candidates), \
             patch.object(orch, "execute_claude_task", side_effect=mock_provider("claude")), \
             patch.object(orch, "execute_codex_task", side_effect=mock_provider("codex")), \
             patch.object(orch, "run_local_tests", side_effect=mock_tests):

            code = orch.run_race(
                "Implement good value",
                cwd=str(self.repo),
                engine_a="claude",
                engine_b="codex",
                timeout=30,
            )

            self.assertEqual(code, 0)
            race = CandidateManager.get_race()
            self.assertIsNotNone(race)
            # B should have won because A failed tests
            self.assertEqual(race["winner"], "B")
            self.assertFalse(race["candidates"]["A"]["test_passed"])
            self.assertTrue(race["candidates"]["B"]["test_passed"])

    def test_cancelled_competitor_skips_wasteful_tests(self):
        """
        When contestant A wins early, contestant B is cancelled.
        The system must NOT execute run_local_tests on the cancelled contestant's worktree.
        """
        candidates = self.root / "candidates"
        tested_cwds = []

        def mock_tests(cwd, **_):
            tested_cwds.append(str(cwd))
            return True, "1 passed"

        def mock_provider(engine):
            def execute(prompt, **kwargs):
                ws = Path(kwargs["cwd"])
                if engine == "claude":
                    # Fast candidate A
                    (ws / "app.py").write_text("VALUE = 42\n")
                    return True, "implemented A", None
                else:
                    # Slow candidate B: simulate work
                    scope = get_current_contestant_scope()
                    for _ in range(50):
                        if scope and scope.cancel_event.is_set():
                            return False, None, "cancelled"
                        time.sleep(0.05)
                    (ws / "app.py").write_text("VALUE = 99\n")
                    return True, "implemented B", None
            return execute

        with self.fixtures(), \
             patch.object(orch, "CANDIDATES_DIR", candidates), \
             patch.object(config, "CANDIDATES_DIR", candidates), \
             patch.object(orch, "execute_claude_task", side_effect=mock_provider("claude")), \
             patch.object(orch, "execute_codex_task", side_effect=mock_provider("codex")), \
             patch.object(orch, "run_local_tests", side_effect=mock_tests):

            code = orch.run_race(
                "Implement value 42",
                cwd=str(self.repo),
                engine_a="claude",
                engine_b="codex",
                timeout=30,
            )

            self.assertEqual(code, 0)
            # Candidate A was tested
            self.assertTrue(any("agent_a" in p for p in tested_cwds), "Contestant A should be tested")
            # Candidate B was cancelled and MUST NOT be tested
            self.assertFalse(any("agent_b" in p for p in tested_cwds), "Cancelled contestant B must NOT be tested")


if __name__ == "__main__":
    unittest.main()
