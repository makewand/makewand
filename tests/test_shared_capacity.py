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

    def test_ready_model_waiter_cannot_block_independent_tool_pool(self):
        current = capacity.policy()
        a = self.reserve("a", "codex")
        with self.assertRaises(budget.ConcurrencyBusyError):
            with capacity.admission(current, "codex", None, str(self.root / "queued.json"), "queued", "model-waiter"):
                self.fail("busy model admitted")
        self.finish("a", a)
        try:
            with patch.object(capacity, "tool_memory_available", return_value=True):
                with capacity.tool_scope(current, time.monotonic() + .6):
                    pass
        finally:
            capacity.cancel("model-waiter", current)

    def test_ready_tool_waiter_cannot_block_independent_model_pool(self):
        current = capacity.policy()
        with patch.object(capacity, "tool_memory_available", return_value=True):
            with capacity.tool_scope(current, time.monotonic() + 10):
                with capacity.locked() as (path, state):
                    ticket = json.loads(json.dumps(next(row for row in state["active"].values() if row["kind"] == "tool")))
                    ticket["id"] = "tool-waiter"
                    state["queue"].append(ticket)
                    capacity.save(path, state)
                try:
                    a = self.reserve("a", "codex")
                    self.finish("a", a)
                finally:
                    capacity.cancel("tool-waiter", current)

    def test_tilde_budget_path_preserves_slot_during_api_relocation(self):
        with patch.dict(os.environ, HOME=str(self.root), USERPROFILE=str(self.root)):
            a = budget.reserve("codex", "fast", task_id="tilde", budget_file="~/tilde.json")
            budget.api_lease(a, "codex", budget_file="~/tilde.json")
            budget.complete(a, True, 0, budget_file="~/tilde.json")
        self.assertEqual(len(json.loads((self.root / "tilde.json").read_text())["attempts"]), 1)

    def test_reaped_legacy_limit_does_not_poison_agy_two_slot_policy(self):
        from makewand import account_leases
        owner = subprocess.Popen([sys.executable, "-I", "-c", "import time; time.sleep(20)"])
        try:
            dead_owner = account_leases.process_ref(owner.pid)
        finally:
            owner.kill()
            owner.wait(timeout=5)
        os.environ["MAKEWAND_ACCOUNT_CONCURRENCY"] = "1"
        os.environ["MAKEWAND_POOL_CONCURRENCY"] = "1"
        old = self.reserve("ledger", "agy")
        path = self.root / "ledger.json"
        data = json.loads(path.read_text())
        data["leases"][old].update(owner=dead_owner, expires_at=0)
        path.write_text(json.dumps(data))
        os.environ.pop("MAKEWAND_ACCOUNT_CONCURRENCY")
        os.environ.pop("MAKEWAND_POOL_CONCURRENCY")
        first = self.reserve("ledger", "agy", task_id="new-first", project="first")
        try:
            second = self.reserve("ledger", "agy", task_id="new-second", project="second")
            self.finish("ledger", second)
        finally:
            self.finish("ledger", first)

    def dead_owner(self):
        owner = subprocess.Popen([sys.executable, "-I", "-c", "import time; time.sleep(20)"])
        try:
            return capacity.account_leases.process_ref(owner.pid)
        finally:
            owner.kill()
            owner.wait(timeout=5)

    def test_legacy_reaper_missing_lease_recovers_only_dead_cached_owner(self):
        a = self.reserve("a", "codex")
        source = self.root / "a.json"
        data = json.loads(source.read_text())
        data["leases"].pop(a)
        source.write_text(json.dumps(data))  # legacy reaper leaves attempt charged
        with capacity.locked() as (path, state):
            row = next(row for row in state["active"].values() if row["attempt"] == a)
            row["lease"]["expires_at"] = 0
            capacity.refresh(state, str(source))
            self.assertIn(row["id"], state["active"], "live owner was released")
            row["lease"]["owner"] = self.dead_owner()
            row["lease"]["workers"] = [capacity.account_leases.process_ref(os.getpid())]
            capacity.refresh(state, str(source))
            self.assertIn(row["id"], state["active"], "live worker was released")
            row["lease"]["workers"] = []
            entries = data["attempts"]
            data["attempts"] = []
            source.write_text(json.dumps(data))
            capacity.refresh(state, str(source))
            self.assertIn(row["id"], state["active"], "reset source freed an unproven slot")
            data["attempts"] = entries
            source.write_text(json.dumps(data))
            capacity.refresh(state, str(source))
            self.assertNotIn(row["id"], state["active"], "dead legacy slot never recovered")
            capacity.save(path, state)
        self.assertEqual(data["attempts"][0]["status"], "started")
        self.assertEqual(data["scopes"]["tasks"]["a"]["used"], 1)
        b = self.reserve("b", "codex")
        self.finish("b", b)

    def test_dead_source_lease_uses_original_ledger_clock(self):
        a = self.reserve("a", "codex")
        source = self.root / "a.json"
        data = json.loads(source.read_text())
        now = time.time()
        data["scopes"]["clock"] = now + 10
        data["leases"][a].update(owner=self.dead_owner(), expires_at=now + 5)
        source.write_text(json.dumps(data))
        with capacity.locked() as (path, state):
            capacity.refresh(state, str(source))
            self.assertFalse(any(row["attempt"] == a for row in state["active"].values()))

    def test_reaper_records_death_without_refunding_call(self):
        a = self.reserve("a", "codex")
        data = json.loads((self.root / "a.json").read_text())
        data["leases"][a].update(owner=self.dead_owner(), expires_at=0)
        capacity.account_leases.admission(data, "agy", None, capacity.policy())
        self.assertNotIn(a, data["leases"])
        self.assertIs(data["attempts"][0].get("lease_reaped"), True)
        self.assertEqual(data["attempts"][0]["status"], "started")
        self.assertEqual(data["scopes"]["tasks"]["a"]["used"], 1)
        (self.root / "a.json").write_text(json.dumps(data))
        with capacity.locked() as (path, state):
            capacity.refresh(state, str(self.root / "a.json"))
            self.assertFalse(any(row["attempt"] == a for row in state["active"].values()))
            capacity.save(path, state)

    def test_tool_state_loss_reports_unavailable_instead_of_raising(self):
        from makewand import orchestrator
        state_path = self.root / capacity.STATE_NAME
        for damage in ("delete", "reset", "corrupt"):
            def damage_state(*args, **kwargs):
                if damage == "delete":
                    state_path.unlink()
                elif damage == "reset":
                    state_path.write_text('{"schema":1,"active":{},"queue":[]}')
                else:
                    state_path.write_text('{broken')
                return True, None

            for entrypoint in ("tests", "sandbox"):
                with self.subTest(damage=damage, entrypoint=entrypoint):
                    state_path.unlink(missing_ok=True)
                    with patch.object(capacity, "tool_memory_available", return_value=True):
                        if entrypoint == "tests":
                            with patch.object(orchestrator, "_run_local_tests", side_effect=damage_state):
                                passed, detail = orchestrator.run_local_tests(str(self.root), timeout=2)
                            self.assertFalse(passed)
                            self.assertIsInstance(detail, orchestrator.LocalTestsUnavailable)
                        else:
                            @capacity.controlled_tool
                            def command(timeout=2):
                                damage_state()
                                return 0, "ok", "", None
                            result = command()
                            self.assertEqual(result[0], -1)
                            self.assertEqual(result[3], "LocalCapacityUnavailable")
                    self.assertIsNone(capacity._tool.get())

    def test_idle_completion_survives_compaction_until_cache_refresh(self):
        a = self.reserve("idle", "codex")
        self.finish("idle", a)
        source = self.root / "idle.json"
        data = json.loads(source.read_text())
        data["attempts"][0]["finished_at"] = time.time() - 49 * 3600
        source.write_text(json.dumps(data))
        budget.request_budget("idle", maximum=3, budget_file=str(source))
        self.assertIn(a, {row["id"] for row in json.loads(source.read_text())["attempts"]})
        b = self.reserve("next", "codex")
        self.finish("next", b)
        budget.request_budget("idle", maximum=3, budget_file=str(source))
        data = json.loads(source.read_text())
        self.assertNotIn(a, {row["id"] for row in data["attempts"]})
        self.assertEqual(data["scopes"]["tasks"]["idle"]["used"], 1)

    def test_compaction_retains_receipt_with_disabled_policy_or_corrupt_cache(self):
        for condition in ("disabled-policy", "corrupt-cache"):
            with self.subTest(condition=condition):
                a = self.reserve(condition, "codex")
                self.finish(condition, a)
                source = self.root / (condition + ".json")
                data = json.loads(source.read_text())
                data["attempts"][0]["finished_at"] = time.time() - 49 * 3600
                source.write_text(json.dumps(data))
                policy_path, state_path = self.root / capacity.POLICY_NAME, self.root / capacity.STATE_NAME
                policy_text, state_text = policy_path.read_text(), state_path.read_text()
                try:
                    if condition == "disabled-policy":
                        policy_path.unlink()
                    else:
                        state_path.write_text('{broken')
                    budget.request_budget(condition, maximum=3, budget_file=str(source))
                    self.assertIn(a, {row["id"] for row in json.loads(source.read_text())["attempts"]})
                finally:
                    policy_path.write_text(policy_text)
                    state_path.write_text(state_text)
                b = self.reserve(condition + "-next", "codex")
                self.finish(condition + "-next", b)
                budget.request_budget(condition, maximum=3, budget_file=str(source))
                self.assertNotIn(a, {row["id"] for row in json.loads(source.read_text())["attempts"]})

    def test_malformed_tool_state_reports_unavailable_before_running(self):
        from makewand import orchestrator
        row = {"id": "x", "project": "p", "label": "l", "kind": "tool", "ledger": "",
               "attempt": "", "started_at": 0, "limits": [1, 1, 1]}
        for place in ("active", "queue"):
            for entrypoint in ("tests", "sandbox"):
                with self.subTest(place=place, entrypoint=entrypoint):
                    state = {"schema": 1, "active": {}, "queue": []}
                    state[place] = {"x": row} if place == "active" else [row]
                    (self.root / capacity.STATE_NAME).write_text(json.dumps(state))
                    ran = []
                    with patch.object(capacity, "tool_memory_available", return_value=True):
                        if entrypoint == "tests":
                            with patch.object(orchestrator, "_run_local_tests", return_value=(True, None)) as command:
                                passed, detail = orchestrator.run_local_tests(str(self.root), timeout=2)
                            self.assertFalse(passed)
                            self.assertIsInstance(detail, orchestrator.LocalTestsUnavailable)
                            command.assert_not_called()
                        else:
                            @capacity.controlled_tool
                            def command(timeout=2):
                                ran.append(True)
                                return 0, "ok", "", None
                            result = command()
                            self.assertEqual(result[3], "LocalCapacityUnavailable")
                            self.assertFalse(ran)

    def test_invalid_tool_policy_reports_unavailable_before_running(self):
        from makewand import orchestrator
        for text in ('{broken', '{"schema":2}', '{"schema":1,"providers":[]}'):
            with self.subTest(text=text):
                (self.root / capacity.POLICY_NAME).write_text(text)
                with patch.object(orchestrator, "_run_local_tests", return_value=(True, None)) as command:
                    passed, detail = orchestrator.run_local_tests(str(self.root), timeout=2)
                self.assertFalse(passed)
                self.assertIsInstance(detail, orchestrator.LocalTestsUnavailable)
                command.assert_not_called()

                @capacity.controlled_tool
                def sandbox(timeout=2):
                    self.fail("invalid policy ran a local command")
                self.assertEqual(sandbox()[3], "LocalCapacityUnavailable")


if __name__ == "__main__":
    unittest.main()
