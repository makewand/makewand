"""Host capacity protocols; all providers are offline fixtures."""
try:
    import _isolation
except ImportError:
    from tests import _isolation
import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import call_budget as budget, shared_capacity as capacity

REPO = str(Path(__file__).resolve().parent.parent)
POLICY = {"schema": 1, "global_max": 3, "project_max": 1, "tool_max": 2,
          "wait_seconds": 300, "providers": {"codex": 1, "agy": 2, "claude": 1}}


class SharedCapacityTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.env = patch.dict(os.environ, MAKEWAND_CONFIG_DIR=str(self.root), MAKEWAND_LAYERED_BUDGET="1",
                              MAKEWAND_LEASE_WAIT_SECONDS="0", MAKEWAND_MAX_MODEL_CALLS="3")
        self.env.start()
        self.addCleanup(self.env.stop)
        for name in ("MAKEWAND_CALL_BUDGET_FILE", "MAKEWAND_ACCOUNT_CONCURRENCY", "MAKEWAND_POOL_CONCURRENCY", "MAKEWAND_PROVIDER_CONCURRENCY"):
            os.environ.pop(name, None)
        (self.root / capacity.POLICY_NAME).write_text(json.dumps(POLICY))

    def reserve(self, name, engine="agy", task_id=None, project=None):
        os.environ["MAKEWAND_PROJECT_ROOT"] = str(self.root / (project or name))
        Path(os.environ["MAKEWAND_PROJECT_ROOT"]).mkdir(exist_ok=True)
        return budget.reserve(engine, "fast", task_id=task_id or name, budget_file=str(self.root / (name + ".json")))

    def finish(self, name, attempt):
        budget.complete(attempt, True, 0, budget_file=str(self.root / (name + ".json")))

    def test_private_ledgers_share_global_and_provider_caps(self):
        a = self.reserve("a", "codex")
        b = self.reserve("b")
        c = self.reserve("c", "gemini")
        with self.assertRaises(budget.ConcurrencyBusyError):
            self.reserve("d", "claude")
        self.assertFalse((self.root / "d.json").exists())
        self.finish("a", a)
        with self.assertRaises(budget.ConcurrencyBusyError):
            self.reserve("d", "antigravity")
        self.finish("b", b)
        self.finish("c", c)
        d = self.reserve("d", "antigravity")
        self.finish("d", d)
        self.assertEqual(json.loads((self.root / "b.json").read_text())["scopes"]["tasks"]["b"]["used"], 1)

    def test_project_and_request_cannot_bypass_via_private_ledger(self):
        a = self.reserve("a", "codex", task_id="same-request", project="project")
        with self.assertRaises(budget.ConcurrencyBusyError):
            self.reserve("b", "agy", project="project")
        with self.assertRaises(budget.ConcurrencyBusyError):
            self.reserve("c", "agy", task_id="same-request")
        self.finish("a", a)
        b = self.reserve("b", "agy", project="project")
        self.finish("b", b)

    def test_expiry_alone_and_missing_active_source_do_not_release(self):
        a = self.reserve("a", "codex")
        path = self.root / "a.json"
        data = json.loads(path.read_text())
        data["leases"][a]["expires_at"] = 0
        path.write_text(json.dumps(data))
        with self.assertRaises(budget.ConcurrencyBusyError):
            self.reserve("b", "codex")
        path.unlink()
        with self.assertRaises(budget.ConcurrencyBusyError):
            self.reserve("b", "codex")
        path.write_text(json.dumps(data))
        self.finish("a", a)

    def test_dead_owner_does_not_release_live_worker(self):
        if not Path("/proc").is_dir():
            self.skipTest("Linux process identity required")
        worker = subprocess.Popen([sys.executable, "-I", "-c", "import time; time.sleep(20)"], start_new_session=True)
        try:
            a = self.reserve("a", "codex")
            budget.lease_update(a, "worker", worker.pid, budget_file=str(self.root / "a.json"))
            path = self.root / "a.json"
            data = json.loads(path.read_text())
            data["leases"][a]["owner"]["boot"] = "previous-boot"
            data["leases"][a]["expires_at"] = 0
            path.write_text(json.dumps(data))
            with self.assertRaises(budget.ConcurrencyBusyError):
                self.reserve("b", "codex")
            worker.kill()
            worker.wait(timeout=5)
            b = self.reserve("b", "codex")
            self.finish("b", b)
        finally:
            if worker.poll() is None:
                worker.kill()
                worker.wait(timeout=5)

    def test_queue_wait_never_spends_and_reserves_execution_time(self):
        a = self.reserve("a", "codex")
        os.environ["MAKEWAND_LEASE_WAIT_SECONDS"] = "300"
        start = time.monotonic()
        os.environ["MAKEWAND_PROJECT_ROOT"] = str(self.root / "b")
        with self.assertRaises(budget.ConcurrencyBusyError):
            budget.reserve("codex", "fast", task_id="b", budget_file=str(self.root / "b.json"), deadline_monotonic=start + 3)
        self.assertLess(time.monotonic() - start, 2.5)
        self.assertFalse((self.root / "b.json").exists())
        self.assertEqual(json.loads((self.root / capacity.STATE_NAME).read_text())["queue"], [])
        self.finish("a", a)

    def test_no_policy_preserves_legacy_limits(self):
        (self.root / capacity.POLICY_NAME).unlink()
        a = self.reserve("a")
        b = self.reserve("b")
        self.assertFalse((self.root / capacity.STATE_NAME).exists())
        self.finish("a", a)
        self.finish("b", b)

    def test_fifo_skips_blocked_provider_and_prefers_less_recent_project(self):
        current = capacity.policy()
        with capacity.admission(current, "codex", None, str(self.root / "a.json"), "a", "first") as gate:
            budget._reserve_once("codex", "fast", task_id="a", budget_file=str(self.root / "a.json"), capacity_gate=gate, capacity_policy=current)
        # Earlier Codex cannot run, but AGY must not be stuck behind it.
        with self.assertRaises(budget.ConcurrencyBusyError):
            with capacity.admission(current, "codex", None, str(self.root / "b.json"), "b", "blocked"):
                self.fail("Codex slot unexpectedly free")
        os.environ["MAKEWAND_PROJECT_ROOT"] = str(self.root / "other")
        with capacity.admission(current, "agy", None, str(self.root / "c.json"), "c", "eligible") as gate:
            c = budget._reserve_once("agy", "fast", task_id="c", budget_file=str(self.root / "c.json"), capacity_gate=gate, capacity_policy=current)
        self.finish("c", c)

    def test_invalid_policy_and_corrupt_state_fail_before_dispatch(self):
        (self.root / capacity.POLICY_NAME).write_text('{"schema":1}')
        with self.assertRaises(budget.BudgetConfigError):
            self.reserve("a")
        (self.root / capacity.POLICY_NAME).write_text(json.dumps(POLICY))
        (self.root / capacity.STATE_NAME).write_text('{broken')
        with self.assertRaises(budget.BudgetConfigError):
            self.reserve("a")
        self.assertFalse((self.root / "a.json").exists())

    def test_api_relocation_keeps_one_charged_slot(self):
        a = self.reserve("a", "codex")
        budget.api_lease(a, "codex", budget_file=str(self.root / "a.json"))
        state = json.loads((self.root / capacity.STATE_NAME).read_text())
        self.assertEqual(len(state["active"]), 1)
        self.assertEqual(next(iter(state["active"].values()))["lease"]["keys"][0], "codex:api:provider")
        with self.assertRaises(budget.ConcurrencyBusyError):
            self.reserve("b", "codex")
        self.finish("a", a)
        self.assertEqual(len(json.loads((self.root / "a.json").read_text())["attempts"]), 1)

    def test_tool_slots_are_separate_and_track_actual_subprocess(self):
        current = capacity.policy()
        with patch.object(capacity, "tool_memory_available", return_value=True):
            with capacity.tool_scope(current, time.monotonic() + 30):
                from makewand.providers.base import run_subprocess
                code, _, _, _ = run_subprocess([sys.executable, "-I", "-c", "print('fixture')"], timeout=10)
                self.assertEqual(code, 0)
                a = self.reserve("a", "codex")
                self.finish("a", a)
                with capacity.tool_scope(current, time.monotonic() + 10):
                    with self.assertRaises(budget.ConcurrencyBusyError):
                        with capacity.tool_scope(current, time.monotonic() + .15):
                            self.fail("third heavy tool started")
            state = json.loads((self.root / capacity.STATE_NAME).read_text())
            self.assertFalse(any(row["kind"] == "tool" for row in state["active"].values()))
            self.assertEqual(state["queue"], [])

    def test_heavy_tool_memory_headroom_refusal_spends_no_model_calls(self):
        with patch.object(capacity, "tool_memory_available", return_value=False):
            with self.assertRaises(budget.ConcurrencyBusyError):
                with capacity.tool_scope(capacity.policy(), time.monotonic() + .15):
                    self.fail("memory headroom gate bypassed")
        self.assertFalse((self.root / "call-budget.json").exists())

    def test_cancelled_waiter_removes_ticket_even_after_scope_cancellation(self):
        a = self.reserve("a", "codex")
        current = capacity.policy()
        with self.assertRaises(budget.ConcurrencyBusyError):
            with capacity.admission(current, "codex", None, str(self.root / "b.json"), "b", "cancelled"):
                self.fail("busy provider admitted")
        with patch.object(budget, "_check_admission_cancelled", side_effect=budget.BudgetCancelledError("cancelled")):
            capacity.cancel("cancelled", current)
        self.assertEqual(json.loads((self.root / capacity.STATE_NAME).read_text())["queue"], [])
        self.finish("a", a)

    def test_less_recent_project_precedes_same_project_backlog(self):
        a = self.reserve("a", "codex", project="project-a")
        current = capacity.policy()
        for project, ticket in (("project-a", "again"), ("project-b", "fresh")):
            os.environ["MAKEWAND_PROJECT_ROOT"] = str(self.root / project)
            with self.assertRaises(budget.ConcurrencyBusyError):
                with capacity.admission(current, "codex", None, str(self.root / (ticket + ".json")), ticket, ticket):
                    self.fail("busy provider admitted")
        self.finish("a", a)
        os.environ["MAKEWAND_PROJECT_ROOT"] = str(self.root / "project-a")
        with self.assertRaises(budget.ConcurrencyBusyError):
            with capacity.admission(current, "codex", None, str(self.root / "again.json"), "again", "again"):
                self.fail("same-project backlog jumped ahead")
        os.environ["MAKEWAND_PROJECT_ROOT"] = str(self.root / "project-b")
        with capacity.admission(current, "codex", None, str(self.root / "fresh.json"), "fresh", "fresh") as gate:
            fresh = budget._reserve_once("codex", "fast", task_id="fresh", budget_file=str(self.root / "fresh.json"), capacity_gate=gate, capacity_policy=current)
        self.finish("fresh", fresh)
        capacity.cancel("again", current)

    def test_linked_git_worktrees_share_project_without_running_git(self):
        repository = self.root / "main"
        worktree = self.root / "worktree"
        metadata = repository / ".git" / "worktrees" / "linked"
        metadata.mkdir(parents=True)
        worktree.mkdir()
        (metadata / "commondir").write_text("../..\n")
        (worktree / ".git").write_text(f"gitdir: {metadata}\n")
        os.environ.pop("MAKEWAND_PROJECT_ROOT", None)
        self.assertEqual(capacity.project(repository)[0], capacity.project(worktree)[0])

    def test_reset_active_source_does_not_free_cached_slot(self):
        a = self.reserve("a", "codex")
        path = self.root / "a.json"
        original = path.read_text()
        path.write_text('{"schema":1,"maximum":3,"attempts":[]}')
        with self.assertRaises(budget.ConcurrencyBusyError):
            self.reserve("b", "codex")
        path.write_text(original)
        self.finish("a", a)

    def test_policy_cannot_be_bypassed_by_disabling_call_accounting(self):
        for name in ("MAKEWAND_LAYERED_BUDGET", "MAKEWAND_MACHINE_DAILY_CALLS", "MAKEWAND_PROJECT_DAILY_CALLS"):
            os.environ.pop(name, None)
        os.environ.pop("MAKEWAND_MAX_MODEL_CALLS", None)
        with self.assertRaises(budget.BudgetConfigError):
            budget.reserve("agy", "fast", task_id="unaccounted")
        self.assertFalse((self.root / capacity.STATE_NAME).exists())

    def test_contended_host_lock_cannot_consume_execution_deadline(self):
        from makewand import filelock
        state = self.root / capacity.STATE_NAME
        with open(str(state) + ".lock", "wb") as holder:
            filelock.flock(holder.fileno(), filelock.LOCK_EX)
            os.environ["MAKEWAND_LEASE_WAIT_SECONDS"] = "300"
            start = time.monotonic()
            with self.assertRaises(budget.ConcurrencyBusyError):
                budget.reserve("agy", "fast", task_id="waiting", budget_file=str(self.root / "waiting.json"), deadline_monotonic=start + 3)
            # Ticket cleanup has a separate bounded one-second lock budget.
            self.assertLess(time.monotonic() - start, 3.25)
            filelock.flock(holder.fileno(), filelock.LOCK_UN)
        self.assertFalse((self.root / "waiting.json").exists())

    def test_native_runner_stopped_proof_clears_job_worker_fence(self):
        real_is_dir = Path.is_dir
        with patch.object(capacity, "tool_memory_available", return_value=True):
            with capacity.tool_scope(capacity.policy(), time.monotonic() + 2):
                capacity.tool_update("launch")
                capacity.tool_update("job_worker", os.getpid())
                with patch.object(Path, "is_dir", lambda path: False if str(path) == "/proc" else real_is_dir(path)):
                    capacity.tool_update("stopped")
                state = json.loads((self.root / capacity.STATE_NAME).read_text())
                tool = next(row for row in state["active"].values() if row["kind"] == "tool")
                self.assertEqual(tool["lease"]["workers"], [])
                self.assertFalse(tool["lease"]["launch_pending"])


if __name__ == "__main__":
    unittest.main()
