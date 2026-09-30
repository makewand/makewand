"""Offline regression gate for the shared execution wire and runtime."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import concurrent.futures
import json
import os
import subprocess
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import call_budget, telemetry
from makewand.execution_contract import ExecutionRequest, ExecutionResult, STATUS_CODES
from makewand.execution_runtime import account_reference, current_context, execute, execution_context, task_id

ROOT = Path(__file__).resolve().parent.parent


class ExecutionContractTests(unittest.TestCase):
    def test_go_python_golden_wire_contract(self):
        contract = json.loads((ROOT / "makewand/execution_contract.json").read_text())
        self.assertEqual(STATUS_CODES, contract["status_codes"])
        self.assertEqual(ExecutionRequest.from_dict(contract["fixtures"]["request"]).to_dict(), contract["fixtures"]["request"])
        self.assertEqual(ExecutionResult.from_dict(contract["fixtures"]["result"]).to_dict(), contract["fixtures"]["result"])
        self.assertEqual(telemetry.validate_event(contract["fixtures"]["event"]), contract["fixtures"]["event"])

    def test_result_preserves_tuple_callers_without_losing_outcome(self):
        result = ExecutionResult(False, None, "deadline", status="TIMEOUT", outcome_known=False)
        self.assertIsInstance(result, tuple)
        ok, output, error = result
        self.assertEqual((ok, output, error), (False, None, "deadline"))
        self.assertEqual(result.exit_code, 16)
        self.assertFalse(result.outcome_known)
        self.assertEqual(ExecutionResult.from_dict(result.to_dict()), result)
        unmeasured = ExecutionResult(status="UNVERIFIED", duration_ms=None)
        self.assertIsNone(ExecutionResult.from_dict(unmeasured.to_dict()).duration_ms)

    def test_invalid_wire_cannot_claim_pass_or_known_cost(self):
        for fields in ({"success": True, "status": "UNVERIFIED"}, {"status": "made-up"},
                       {"status": "PASSED", "exit_code": 10}, {"success": 1},
                       {"status": "PASSED", "tokens": True}, {"status": "PASSED", "monetary_cost": float("nan")},
                       {"status": "PASSED", "monetary_cost": 10**400},
                       {"status": "PASSED", "readonly": "false"}, {"status": "PASSED", "duration_ms": -1},
                       {"status": "UNKNOWN", "outcome_known": True},
                       {"status": "PASSED", "artifact_digest": "unsealed"}):
            with self.subTest(fields=fields), self.assertRaises(ValueError):
                ExecutionResult(**fields)
        for fields in ({"timeout_ms": True}, {"timeout_ms": 0}, {"task_id": "bad\nidentity"},
                       {"readonly": 1}, {"max_model_calls": 2**100}, {"api_policy": "invented"}):
            args = dict(task_id="task", stage="review", engine="codex")
            args.update(fields)
            with self.subTest(fields=fields), self.assertRaises(ValueError):
                ExecutionRequest(**args)
        for decoder, value in ((ExecutionRequest.from_dict, dict(task_id="task", stage="review", engine="codex")),
                               (ExecutionResult.from_dict, dict(status="PASSED"))):
            with self.assertRaises(ValueError):
                decoder(value)

    def test_events_reject_prompt_and_fabricated_completions(self):
        fixture = json.loads((ROOT / "makewand/execution_contract.json").read_text())["fixtures"]["event"]
        for event in (dict(fixture, prompt="secret"), dict(fixture, status="pass"),
                      dict(fixture, duration_ms=True), dict(fixture, event="start")):
            with self.assertRaises(ValueError):
                telemetry.validate_event(event)


class ExecutionRuntimeTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name)
        self.budget = self.root / "budget.json"
        self.events = self.root / "events.jsonl"
        self.env = patch.dict(os.environ, dict(MAKEWAND_CALL_BUDGET_FILE=str(self.budget),
            MAKEWAND_MAX_MODEL_CALLS="3", MAKEWAND_EXECUTION_EVENTS_FILE=str(self.events),
            MAKEWAND_BENCHMARK_RUN_ID="offline-runtime", MAKEWAND_TASK_ID="offline-task"))
        self.env.start()

    def tearDown(self):
        self.env.stop()
        self.directory.cleanup()

    def request(self, **fields):
        values = dict(task_id="offline-task", stage="generation", engine="codex", timeout_ms=30000)
        values.update(fields)
        return ExecutionRequest(**values)

    def ledger(self):
        return json.loads(self.budget.read_text())

    def test_admission_counts_failures_and_disallows_fourth_callback(self):
        invoked = []
        def provider(timeout):
            invoked.append(timeout)
            return False, None, "explicit refusal"
        for _ in range(3):
            self.assertEqual(execute(self.request(), provider).status, "FAILED")
        self.assertEqual(execute(self.request(), provider).status, "BUDGET_EXHAUSTED")
        self.assertEqual(len(invoked), 3)
        self.assertEqual(len(self.ledger()["attempts"]), 3)
        self.assertTrue(all(entry["result_status"] == "FAILED" for entry in self.ledger()["attempts"]))

    def test_unknown_exception_records_attempt_without_replay_or_secrets(self):
        called = []
        def provider(timeout):
            called.append(timeout)
            raise OSError("secret-user credential-token password")
        result = execute(self.request(prompt="private prompt", cwd="/private/source"), provider)
        self.assertEqual(result.status, "UNKNOWN")
        self.assertFalse(result.outcome_known)
        self.assertEqual(len(called), 1)
        self.assertEqual(self.ledger()["attempts"][0]["result_status"], "UNKNOWN")
        events = self.events.read_text()
        for secret in ("secret-user", "credential-token", "password", "private prompt", "/private/source"):
            self.assertNotIn(secret, events)

    def test_expired_parent_prevents_admission_and_cannot_be_extended(self):
        called = []
        with execution_context(deadline_unix_ms=int(time.time() * 1000) - 1):
            with execution_context(deadline_unix_ms=int(time.time() * 1000) + 300000):
                result = execute(self.request(), lambda timeout: called.append(timeout))
        self.assertEqual(result.status, "TIMEOUT")
        self.assertTrue(result.outcome_known)
        self.assertEqual(called, [])
        self.assertFalse(self.budget.exists())

    def test_provider_receives_remaining_total_deadline(self):
        timeouts = []
        with execution_context(deadline_unix_ms=int(time.time() * 1000) + 400):
            result = execute(self.request(timeout_ms=60000), lambda timeout: (timeouts.append(timeout) is None, "ok", None))
        self.assertEqual(result.status, "PASSED")
        self.assertGreater(timeouts[0], 0)
        self.assertLessEqual(timeouts[0], .4)

    def test_nested_sdk_inherits_the_request_deadline_without_a_workflow_scope(self):
        timeouts = []
        def child(timeout):
            timeouts.append(timeout)
            return True, "nested", None
        def parent(timeout):
            self.assertIsNotNone(current_context().get("_deadline_monotonic"))
            return execute(self.request(timeout_ms=60000), child)
        result = execute(self.request(timeout_ms=1000), parent)
        self.assertEqual(result.status, "PASSED")
        self.assertGreater(timeouts[0], 0)
        self.assertLessEqual(timeouts[0], 1)
        self.assertEqual(len(self.ledger()["attempts"]), 2)

    def test_nested_sdk_cannot_drop_the_parents_explicit_budget(self):
        custom = self.root / "parent-budget.json"
        called = []
        def parent(timeout):
            return execute(self.request(), lambda child_timeout: (called.append(child_timeout) is None, "nested", None))
        result = execute(self.request(budget_file=str(custom), max_model_calls=1), parent)
        self.assertEqual(result.status, "BUDGET_EXHAUSTED")
        self.assertEqual(called, [])
        self.assertEqual(len(json.loads(custom.read_text())["attempts"]), 1)
        self.assertFalse(self.budget.exists())

    def test_nested_sdk_cannot_switch_the_parents_ledger(self):
        custom = self.root / "parent-budget.json"
        other = self.root / "other-budget.json"
        called = []
        def parent(timeout):
            return execute(self.request(budget_file=str(other), max_model_calls=100),
                lambda child_timeout: (called.append(child_timeout) is None, "nested", None))
        result = execute(self.request(budget_file=str(custom), max_model_calls=1), parent)
        self.assertEqual(result.status, "INVALID_REQUEST")
        self.assertEqual(called, [])
        self.assertEqual(len(json.loads(custom.read_text())["attempts"]), 1)
        self.assertFalse(other.exists())

    def test_unbounded_dispatch_still_has_an_observed_attempt_identity(self):
        os.environ.pop("MAKEWAND_CALL_BUDGET_FILE")
        os.environ.pop("MAKEWAND_MAX_MODEL_CALLS")
        result = execute(self.request(), lambda timeout: (True, "observed", None))
        self.assertEqual(result.status, "PASSED")
        self.assertEqual(len(result.attempt_id), 32)
        events = [json.loads(line) for line in self.events.read_text().splitlines()]
        self.assertEqual([event["attempt_id"] for event in events], [result.attempt_id] * 2)
        self.assertFalse(self.budget.exists())

    def test_contended_ledger_honors_deadline_without_invoking_provider(self):
        from makewand import filelock
        descriptor = os.open(str(self.budget) + ".lock", os.O_CREAT | os.O_RDWR, 0o600)
        invoked = []
        try:
            filelock.flock(descriptor, filelock.LOCK_EX)
            started = time.monotonic()
            result = execute(self.request(timeout_ms=30), lambda timeout: invoked.append(timeout))
            self.assertLess(time.monotonic() - started, .2)
        finally:
            filelock.flock(descriptor, filelock.LOCK_UN)
            os.close(descriptor)
        self.assertEqual(result.status, "TIMEOUT")
        self.assertTrue(result.outcome_known)
        self.assertEqual(invoked, [])
        self.assertFalse(self.budget.exists())

    def test_late_success_and_timeout_have_unknown_outcome(self):
        monotonic = [100.0]
        def late(timeout):
            monotonic[0] += .15
            return True, "late", None
        with patch("makewand.execution_runtime.time.monotonic", side_effect=lambda: monotonic[0]):
            result = execute(self.request(timeout_ms=100), late)
        self.assertEqual(result.status, "TIMEOUT")
        self.assertFalse(result.outcome_known)

    def test_accounting_cleanup_preserves_a_known_timely_provider_result(self):
        monotonic = [100.0]
        original = call_budget.complete
        def delayed_accounting(*args, **kwargs):
            monotonic[0] += .2
            return original(*args, **kwargs)
        with patch("makewand.execution_runtime.time.monotonic", side_effect=lambda: monotonic[0]), patch("makewand.call_budget.complete", side_effect=delayed_accounting):
            result = execute(self.request(timeout_ms=100), lambda timeout: (True, "timely provider result", None))
        self.assertEqual(result.status, "PASSED")
        self.assertTrue(result.outcome_known)
        self.assertEqual(result.duration_ms, 0)
        self.assertEqual(self.ledger()["attempts"][0]["result_status"], "PASSED")
        def timed_out(timeout):
            raise subprocess.TimeoutExpired("hidden command", timeout)
        result = execute(self.request(), timed_out)
        self.assertEqual(result.status, "TIMEOUT")
        self.assertFalse(result.outcome_known)

    def test_invalid_adapter_and_interruption_still_complete_accounting(self):
        result = execute(self.request(), lambda timeout: ("truthy", "bad", None))
        self.assertEqual(result.status, "UNKNOWN")
        def interrupted(timeout):
            raise KeyboardInterrupt()
        with self.assertRaises(KeyboardInterrupt):
            execute(self.request(), interrupted)
        self.assertEqual([a["result_status"] for a in self.ledger()["attempts"]], ["UNKNOWN", "CANCELLED"])

    def test_review_hold_cannot_be_eaten_and_claim_is_not_a_second_call(self):
        lease = call_budget.reserve_capacity(1, "workflow-task")
        self.assertEqual(call_budget.remaining_capacity(), 2)
        execute(self.request(), lambda timeout: (True, "A", None))
        execute(self.request(), lambda timeout: (True, "B", None))
        self.assertEqual(execute(self.request(), lambda timeout: (True, "steal", None)).status, "BUDGET_EXHAUSTED")
        with execution_context(lease_id=lease):
            result = execute(self.request(stage="review", readonly=True), lambda timeout: (True, "judge", None))
        self.assertEqual(result.status, "PASSED")
        ledger = self.ledger()
        self.assertEqual(len(ledger["attempts"]), 3)
        self.assertEqual(ledger["holds"][lease]["remaining"], 0)
        call_budget.release_capacity(lease)
        self.assertNotIn(lease, self.ledger()["holds"])

    def test_expired_hold_releases_only_unused_capacity(self):
        lease = call_budget.reserve_capacity(2, "workflow-task")
        with execution_context(lease_id=lease):
            execute(self.request(), lambda timeout: (False, None, "failed"))
        data = self.ledger()
        data["holds"][lease]["expires_at"] = time.time() - 1
        self.budget.write_text(json.dumps(data))
        self.assertEqual(call_budget.remaining_capacity(), 2)
        with execution_context(lease_id=lease):
            self.assertEqual(execute(self.request(), lambda timeout: (True, "bad", None)).status, "BUDGET_EXHAUSTED")
        self.assertEqual(len(self.ledger()["attempts"]), 1)

    def test_failed_multi_stage_capacity_plan_starts_no_provider(self):
        os.environ["MAKEWAND_MAX_MODEL_CALLS"] = "2"
        with self.assertRaises(call_budget.BudgetError):
            call_budget.reserve_capacity(3, "race-task")
        self.assertFalse(self.budget.exists())

    def test_explicit_ledger_does_not_mutate_process_environment(self):
        custom = self.root / "custom.json"
        execute(self.request(budget_file=str(custom), max_model_calls=1), lambda timeout: (True, "ok", None))
        self.assertTrue(custom.exists())
        self.assertFalse(self.budget.exists())
        self.assertEqual(os.environ["MAKEWAND_CALL_BUDGET_FILE"], str(self.budget))

    def test_account_reference_binds_selected_root_without_reading_credentials(self):
        first = account_reference("codex")
        with patch.dict(os.environ, CODEX_HOME=str(self.root / "selected-account")):
            second = account_reference("codex")
            result = execute(self.request(), lambda timeout: (True, "ok", None))
        self.assertNotEqual(first, second)
        self.assertEqual(result.account_ref, second)
        self.assertNotIn("selected-account", self.events.read_text())
        self.assertIsNone(account_reference("unknown"))

    def test_invalid_selected_credential_root_is_a_typed_refusal(self):
        with patch.dict(os.environ, CODEX_HOME="~makewand-nonexistent-audit-user/credentials"):
            result = execute(self.request(), lambda timeout: (True, "bad", None))
        self.assertEqual(result.status, "INVALID_REQUEST")
        self.assertFalse(self.budget.exists())

    def test_contexts_and_stage_events_are_private_and_thread_isolated(self):
        def run(index):
            with execution_context(task_id=f"thread-{index}"):
                with telemetry.stage("copy"):
                    self.assertEqual(current_context()["stage"], "copy")
                    return task_id()
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            self.assertEqual(list(pool.map(run, range(20))), [f"thread-{i}" for i in range(20)])
        events = [telemetry.validate_event(json.loads(line)) for line in self.events.read_text().splitlines()]
        self.assertEqual(len(events), 40)
        for i in range(20):
            pair = [event for event in events if event["task_id"] == f"thread-{i}"]
            self.assertEqual([event["event"] for event in pair], ["start", "end"])
            self.assertEqual(pair[0]["event_id"], pair[1]["event_id"])
            self.assertIsNone(pair[1]["tokens"])
            self.assertIsNone(pair[1]["monetary_cost"])
            self.assertIsNone(pair[1]["peak_rss_bytes"])
        self.assertEqual(self.events.stat().st_mode & 0o777, 0o600)

    def test_unavailable_telemetry_does_not_repeat_or_change_model_success(self):
        os.environ["MAKEWAND_EXECUTION_EVENTS_FILE"] = str(self.root)
        result = execute(self.request(), lambda timeout: (True, "accepted stage", None))
        self.assertEqual(result.status, "PASSED")
        self.assertEqual(len(self.ledger()["attempts"]), 1)

    def test_invalid_home_alias_in_telemetry_preserves_outcome(self):
        os.environ["MAKEWAND_EXECUTION_EVENTS_FILE"] = "~makewand-nonexistent-audit-user/events"
        result = execute(self.request(), lambda timeout: (True, "accepted", None))
        self.assertEqual(result.status, "PASSED")
        self.assertEqual(self.ledger()["attempts"][0]["result_status"], "PASSED")

    def test_failed_warning_stream_cannot_block_model_execution(self):
        import io
        sink = io.StringIO()
        os.environ["MAKEWAND_EXECUTION_EVENTS_FILE"] = str(self.root)
        with patch("makewand.telemetry._warned", False), patch("makewand.telemetry.sys.stderr", sink), patch.object(sink, "write", side_effect=OSError("closed warning stream")):
            result = execute(self.request(), lambda timeout: (True, "accepted", None))
        self.assertEqual(result.status, "PASSED")
        self.assertEqual(self.ledger()["attempts"][0]["result_status"], "PASSED")

    def test_invalid_budget_home_alias_refuses_before_dispatch(self):
        os.environ["MAKEWAND_CALL_BUDGET_FILE"] = "~makewand-nonexistent-audit-user/ledger"
        invoked = []
        result = execute(self.request(), lambda timeout: invoked.append(timeout))
        self.assertEqual(result.status, "BUDGET_EXHAUSTED")
        self.assertEqual(invoked, [])

    @unittest.skipUnless(hasattr(os, "mkfifo"), "requires POSIX FIFOs")
    def test_budget_fifo_is_rejected_without_waiting_for_a_writer(self):
        os.mkfifo(self.budget)
        started = time.monotonic()
        invoked = []
        result = execute(self.request(timeout_ms=50), lambda timeout: invoked.append(timeout))
        self.assertLess(time.monotonic() - started, .2)
        self.assertEqual(result.status, "BUDGET_EXHAUSTED")
        self.assertEqual(invoked, [])

    def test_malformed_holds_and_large_ledger_fail_closed(self):
        execute(self.request(), lambda timeout: (True, "ok", None))
        ledger = self.ledger()
        ledger["holds"] = {"invalid-expiry": dict(remaining=1, expires_at=10**400)}
        self.budget.write_text(json.dumps(ledger))
        self.assertEqual(execute(self.request(), lambda timeout: (True, "bad", None)).status, "BUDGET_EXHAUSTED")
        self.budget.write_text('{"schema":1,"maximum":3,"attempts":[],"unknown_extension":NaN}')
        self.assertEqual(execute(self.request(), lambda timeout: (True, "bad", None)).status, "BUDGET_EXHAUSTED")
        with self.budget.open("wb") as stream:
            stream.truncate(call_budget.MAX_LEDGER_BYTES + 1)
        self.assertEqual(execute(self.request(), lambda timeout: (True, "bad", None)).status, "BUDGET_EXHAUSTED")

    def test_contended_telemetry_is_bounded_and_does_not_replay(self):
        from makewand import filelock
        descriptor = os.open(str(self.events) + ".lock", os.O_CREAT | os.O_RDWR, 0o600)
        try:
            filelock.flock(descriptor, filelock.LOCK_EX)
            started = time.monotonic()
            result = execute(self.request(), lambda timeout: (True, "accepted", None))
            self.assertLess(time.monotonic() - started, .5)
        finally:
            filelock.flock(descriptor, filelock.LOCK_UN)
            os.close(descriptor)
        self.assertEqual(result.status, "PASSED")
        self.assertEqual(len(self.ledger()["attempts"]), 1)

    def test_maximum_without_a_shared_file_fails_closed(self):
        os.environ.pop("MAKEWAND_CALL_BUDGET_FILE")
        with self.assertRaises(call_budget.BudgetError):
            call_budget.reserve("codex", "standard")

    def test_lower_limit_persists_even_when_admission_is_refused(self):
        for _ in range(2):
            execute(self.request(), lambda timeout: (True, "ok", None))
        os.environ["MAKEWAND_MAX_MODEL_CALLS"] = "1"
        self.assertEqual(execute(self.request(), lambda timeout: (True, "bad", None)).status, "BUDGET_EXHAUSTED")
        os.environ["MAKEWAND_MAX_MODEL_CALLS"] = "3"
        self.assertEqual(self.ledger()["maximum"], 1)
        self.assertEqual(execute(self.request(), lambda timeout: (True, "bad", None)).status, "BUDGET_EXHAUSTED")


if __name__ == "__main__":
    unittest.main()
