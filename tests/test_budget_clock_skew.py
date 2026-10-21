"""A call budget ledger written while the clock was ahead must not stop dispatch or lease cleanup.

Backward day (the daily caps count every bucket from today to the latest), forward
skew with a dead lease, capacity holds under a leading clock (dated from the
effective clock, re-dated on admission and by repair keeping their duration, moved
into today's buckets by repair, `race` included), the daily bucket's zone, and the
operator repair (``makewand budget clock --repair``). execution/budget_clock_test.go
checks the same rule in Go and that both languages pick the same bucket, count the
same buckets and re-date holds alike. Temporary ledgers only; no model is called.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

import test_workflow_scheduling as scheduling
from makewand import account_leases as leases
from makewand import call_budget as b
from makewand import config, layered_budget, orchestrator as orch
from makewand.execution_contract import EXIT_INTERNAL_ERROR, ExecutionRequest
from makewand.execution_runtime import execute, execution_context
from makewand.workflow import last_result

REPO = Path(__file__).resolve().parent.parent
ACCOUNT = "codex:account:" + "a" * 32
DAY = 86400
NOON = 1792065600  # 2026-10-15 12:00 UTC: half a day from either midnight


def dead_owner():
    """This process as seen from a previous boot: provably dead (Linux identity)."""
    ref = leases.process_ref(os.getpid())
    ref["boot"] = "previous-boot"
    return ref


class ClockSkewCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="mw-budget-clock-")
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "calls.json"
        env = patch.dict(os.environ, MAKEWAND_CALL_BUDGET_FILE=str(self.path), MAKEWAND_LAYERED_BUDGET="1",
                         MAKEWAND_MAX_MODEL_CALLS="100", MAKEWAND_PROJECT_DAILY_CALLS="100",
                         MAKEWAND_MACHINE_DAILY_CALLS="100", MAKEWAND_PROJECT_ROOT=self.tmp.name,
                         MAKEWAND_TASK_ID="skew-task", MAKEWAND_ACCOUNT_CONCURRENCY="1",
                         MAKEWAND_POOL_CONCURRENCY="1", MAKEWAND_LEASE_WAIT_SECONDS="0",
                         MAKEWAND_BUDGET_TIMEZONE="UTC")
        env.start()
        self.addCleanup(env.stop)
        warned = patch.object(layered_budget, "_warned", set())
        warned.start()
        self.addCleanup(warned.stop)
        self.stderr = io.StringIO()
        quiet = contextlib.redirect_stderr(self.stderr)
        quiet.__enter__()
        self.addCleanup(quiet.__exit__, None, None, None)

    def data(self):
        return json.loads(self.path.read_text())

    def edit(self, change):
        data = self.data()
        change(data)
        self.path.write_text(json.dumps(data))

    def spend(self, engine="fake", task_id="task", **kwargs):
        attempt = b.reserve(engine, "fast", task_id=task_id, **kwargs)
        b.complete(attempt, True, 0)
        return attempt

    def require_linux_identity(self):
        if not Path("/proc").is_dir() or sys.platform != "linux":
            self.skipTest("Linux process identity required")


class BackwardDayTests(ClockSkewCase):
    def test_backward_day_counts_into_the_latest_bucket_and_never_resets(self):
        os.environ["MAKEWAND_MACHINE_DAILY_CALLS"] = "4"
        today, tomorrow = layered_budget.day_bucket(NOON), layered_budget.day_bucket(NOON + DAY)
        with patch("time.time", return_value=NOON):
            self.spend(task_id="before")
            # The ledger was last written while the clock read tomorrow.
            self.edit(lambda data: data["scopes"].update(day=tomorrow))
            for task in ("after-1", "after-2"):
                self.spend(task_id=task)  # used to raise "budget clock moved back ..."
            scopes = self.data()["scopes"]
            self.assertEqual(scopes["day"], tomorrow)
            self.assertEqual(scopes["machine"][tomorrow]["used"], 2)
            self.assertEqual(scopes["machine"][today]["used"], 1)
            self.assertIn("ahead of today's bucket", self.stderr.getvalue())
            self.spend(task_id="after-3")
            with self.assertRaisesRegex(b.BudgetError, "budget exhausted"):
                b.reserve("fake", "fast", task_id="after-4")

    def test_daily_cap_counts_every_bucket_from_today_to_the_latest(self):
        # The finding's sequence: a cap of 3, one call today, the clock briefly reads
        # tomorrow (a capacity check writes the ledger then), and real time again.
        for scope, group in (("MAKEWAND_MACHINE_DAILY_CALLS", "machine"), ("MAKEWAND_PROJECT_DAILY_CALLS", "projects")):
            with self.subTest(scope=scope), patch.dict(os.environ, {scope: "3"}):
                self.path.unlink(missing_ok=True)
                with patch("time.time", return_value=NOON):
                    self.spend(task_id="before")
                with patch("time.time", return_value=NOON + DAY):
                    self.assertEqual(b.remaining_capacity(), 3)  # tomorrow's own bucket is empty
                with patch("time.time", return_value=NOON + 60):
                    for task in ("after-1", "after-2"):
                        self.spend(task_id=task)
                    # Calls charged into tomorrow's bucket were made today: 1 + 2 reach the cap.
                    with self.assertRaisesRegex(b.BudgetError, "budget exhausted"):
                        b.reserve("fake", "fast", task_id="after-3")  # was admitted: a fourth call today
                prefix = layered_budget.project_ref() + ":" if group == "projects" else ""
                today, tomorrow = (prefix + layered_budget.day_bucket(moment) for moment in (NOON, NOON + DAY))
                counters = self.data()["scopes"][group]
                self.assertEqual((counters[today]["used"], counters[tomorrow]["used"]), (1, 2))
                # On the real next day those two calls count again (conservative): one call is left.
                with patch("time.time", return_value=NOON + DAY + 60):
                    self.spend(task_id="next-day")
                    with self.assertRaisesRegex(b.BudgetError, "budget exhausted"):
                        b.reserve("fake", "fast", task_id="next-day-2")

    def test_daily_cap_counts_holds_on_every_bucket_from_today_to_the_latest(self):
        today, tomorrow = layered_budget.day_bucket(NOON), layered_budget.day_bucket(NOON + DAY)
        data = {"attempts": [], "scopes": {
            "tasks": {"t": {"maximum": 100, "used": 0}},
            "projects": {"p:" + today: {"maximum": 100, "used": 1}, "p:" + tomorrow: {"maximum": 100, "used": 0}},
            "machine": {today: {"maximum": 3, "used": 1}, tomorrow: {"maximum": 3, "used": 0}},
            "day": tomorrow, "clock": NOON + DAY}}
        keys = [["tasks", "t"], ["projects", "p:" + tomorrow], ["machine", tomorrow]]
        hold = {"remaining": 1, "expires_at": NOON + 600,
                "scopes": [["tasks", "t"], ["projects", "p:" + today], ["machine", today]]}
        # A hold made today, before the clock jumped, still occupies today's capacity.
        self.assertEqual(layered_budget.available(data, keys, {"h": hold}, now=NOON + 60), 1)
        self.assertEqual(layered_budget.available(data, keys, {"h": hold}, claim=hold, now=NOON + 60), 2)
        # Once the real day reaches the latest bucket, that bucket alone counts.
        self.assertEqual(layered_budget.available(data, keys, {"h": hold}, now=NOON + DAY), 3)


class ForwardSkewTests(ClockSkewCase):
    def test_lease_admitted_under_a_leading_clock_is_reclaimable_after_its_owner_dies(self):
        self.require_linux_identity()
        self.spend("codex", task_id="seed", account_ref=ACCOUNT)
        start = time.time()
        self.edit(lambda data: data["scopes"].update(clock=start + 2 * 3600))
        attempt = b.reserve("codex", "fast", task_id="worker", account_ref=ACCOUNT)
        lease = self.data()["leases"][attempt]
        self.assertLessEqual(lease["expires_at"], time.time() + layered_budget.CLOCK_SLACK + leases.TTL)
        self.assertIn("ahead of real time", self.stderr.getvalue())
        # The worker crashes without releasing its lease.
        self.edit(lambda data: data["leases"][attempt].update(owner=dead_owner()))
        with self.assertRaises(b.ConcurrencyBusyError):
            b.reserve("codex", "fast", task_id="next", account_ref=ACCOUNT)
        # Its expiry is at most the slack ahead of real time; once real time passes it,
        # the next worker reclaims the lease (it used to wait for the two-hour lead).
        with patch("time.time", return_value=lease["expires_at"] + 1):
            b.complete(b.reserve("codex", "fast", task_id="next", account_ref=ACCOUNT), True, 0)
        self.assertNotIn(attempt, self.data().get("leases", {}))

    def test_lease_written_while_the_clock_was_ahead_expires_without_repair(self):
        self.require_linux_identity()
        self.spend(task_id="seed")
        now = time.time()

        def skew(data):
            # A writer whose own clock read two hours ahead left a dead owner's lease.
            data["scopes"]["clock"] = now + 2 * 3600
            data["leases"] = {"dead": {"keys": leases.keys("codex", ACCOUNT), "owner": dead_owner(), "workers": [],
                                       "launch_pending": False, "heartbeat_at": now + 2 * 3600,
                                       "expires_at": now + 2 * 3600 + leases.TTL}}
        self.edit(skew)
        with self.assertRaises(b.ConcurrencyBusyError):
            b.reserve("codex", "fast", task_id="blocked", account_ref=ACCOUNT)
        # Admission dated it like a fresh heartbeat, at most the slack plus one TTL ahead.
        expires = self.data()["leases"]["dead"]["expires_at"]
        self.assertLessEqual(expires, time.time() + layered_budget.CLOCK_SLACK + leases.TTL)
        with patch("time.time", return_value=expires + 1):
            b.complete(b.reserve("codex", "fast", task_id="unblocked", account_ref=ACCOUNT), True, 0)
        self.assertNotIn("dead", self.data().get("leases", {}))

    def test_admission_persists_a_capped_clock(self):
        self.spend(task_id="seed")
        start = time.time()
        self.edit(lambda data: data["scopes"].update(clock=start + 2 * 3600))
        self.spend(task_id="heal")
        clock = self.data()["scopes"]["clock"]
        self.assertLessEqual(clock, time.time() + layered_budget.CLOCK_SLACK)
        self.assertGreaterEqual(clock, start)

    def test_capacity_hold_created_under_a_leading_clock_is_usable(self):
        with execution_context(task_id="skew-task"):
            self.spend(task_id="skew-task")
            self.edit(lambda data: data["scopes"].update(clock=time.time() + 2 * 3600))
            hold = b.reserve_capacity(1, "review")
            # The leading clock used to expire the new ten-minute hold at once.
            self.assertIn(hold, self.data().get("holds", {}))
            b.complete(b.reserve("fake", "fast", lease_id=hold, task_id="skew-task", stage="review"), True, 0)

    def test_effective_clock_caps_the_lead_and_warns_once(self):
        ahead = {"scopes": {"clock": 1000.0 + 7200}}
        self.assertEqual(layered_budget.effective_clock(ahead, now=1000.0), 1000.0 + layered_budget.CLOCK_SLACK)
        self.assertEqual(layered_budget.effective_clock(ahead, now=1000.0), 1000.0 + layered_budget.CLOCK_SLACK)
        # Within the slack the persisted clock still never runs backwards.
        self.assertEqual(layered_budget.effective_clock({"scopes": {"clock": 1100.0}}, now=1000.0), 1100.0)
        self.assertEqual(layered_budget.effective_clock({"scopes": {"clock": 900.0}}, now=1000.0), 1000.0)
        self.assertEqual(layered_budget.effective_clock({}, now=1000.0), 1000.0)
        self.assertEqual(self.stderr.getvalue().count("ahead of real time"), 1)


class HoldClockTests(ClockSkewCase):
    """Capacity holds follow the clock rule: dated from the effective clock, re-dated
    on admission and by --repair when created ahead of it, always keeping their duration."""

    def setUp(self):
        super().setUp()
        os.environ["MAKEWAND_MACHINE_DAILY_CALLS"] = "1"  # one held call blocks every other call

    def hold_written_ahead(self, lead, ttl=600):
        """A one-call review hold made by a process whose clock read `lead` seconds ahead of NOON."""
        with patch("time.time", return_value=NOON + lead), execution_context(task_id="skew-task"):
            return b.reserve_capacity(1, "review", ttl_seconds=ttl)

    def blocked(self, moment):
        """Whether the hold refuses another task's call at `moment`."""
        with patch("time.time", return_value=moment):
            try:
                self.spend(task_id=f"other-{moment}")
            except b.BudgetError:
                return True
        return False

    def test_admission_redates_a_hold_written_ahead_keeping_its_duration(self):
        hold = self.hold_written_ahead(2 * 3600)
        self.assertTrue(self.blocked(NOON))
        saved = self.data()["holds"][hold]
        # Re-dated like a lease: created at the effective clock, at most the slack ahead.
        self.assertEqual((saved["created_at"], saved["expires_at"]),
                         (NOON + layered_budget.CLOCK_SLACK, NOON + layered_budget.CLOCK_SLACK + 600))
        self.assertTrue(self.blocked(NOON + 600))
        # Its ten minutes and the slack have passed (it used to block until 2 h 10 min).
        self.assertFalse(self.blocked(NOON + layered_budget.CLOCK_SLACK + 601))

    def test_repair_moves_a_hold_written_ahead_back_by_its_lead(self):
        hold = self.hold_written_ahead(2 * 3600)
        with patch("time.time", return_value=NOON):
            report = b.repair_clock(apply=True)
        self.assertEqual((report["skewed_holds"], report["applied"]), (1, True))
        saved = self.data()["holds"][hold]
        self.assertEqual((saved["created_at"], saved["expires_at"]), (NOON, NOON + 600))
        self.assertTrue(self.blocked(NOON + 599))
        self.assertFalse(self.blocked(NOON + 601))  # was blocked: repair only capped holds 24 h ahead

    def test_repair_moves_a_future_day_hold_into_today(self):
        # The finding: a daily cap of 1 and a one-call hold written while the clock read tomorrow.
        hold = self.hold_written_ahead(DAY)
        today = layered_budget.day_bucket(NOON)
        with patch("time.time", return_value=NOON), execution_context(task_id="skew-task"):
            report = b.repair_clock(apply=True)
            self.assertEqual((report["moved_holds"], report["applied"]), (1, True))
            self.assertIn(["machine", today], self.data()["holds"][hold]["scopes"])
            # Repair used to leave one call available: another task could take the held call.
            self.assertEqual(b.remaining_capacity(), 0)
            with self.assertRaisesRegex(b.BudgetError, "budget exhausted"):
                b.reserve("fake", "fast", task_id="other")
            b.complete(b.reserve("fake", "fast", lease_id=hold, task_id="skew-task", stage="review"), True, 0)
        self.assertEqual(self.data()["scopes"]["machine"][today]["used"], 1)

    def test_new_hold_survives_its_own_save_under_the_permitted_lead(self):
        os.environ["MAKEWAND_MACHINE_DAILY_CALLS"] = "100"
        with patch("time.time", return_value=NOON):
            self.spend(task_id="seed")
            self.edit(lambda data: data["scopes"].update(clock=NOON + layered_budget.CLOCK_SLACK))
            with execution_context(task_id="skew-task"):
                hold = b.reserve_capacity(1, "review", ttl_seconds=60)
                # Dated from wall time, the compaction of its own save used to expire it.
                saved = self.data().get("holds", {})
                self.assertIn(hold, saved)
                self.assertEqual(saved[hold]["expires_at"] - saved[hold]["created_at"], 60)
                b.complete(b.reserve("fake", "fast", lease_id=hold, task_id="skew-task", stage="review"), True, 0)

    def test_undated_hold_counts_as_a_full_day_hold(self):
        # Holds written before they carried created_at: only an expiry more than a day
        # ahead of the effective clock proves a clock that was ahead.
        holds = {"near": {"remaining": 1, "expires_at": NOON + 2 * 3600},
                 "far": {"remaining": 1, "expires_at": NOON + 3 * DAY}}
        for hold in holds.values():
            layered_budget.redate_hold(hold, NOON)
        self.assertEqual(holds, {"near": {"remaining": 1, "expires_at": NOON + 2 * 3600},
                                 "far": {"remaining": 1, "expires_at": NOON + DAY}})
        dated = {"remaining": 1, "created_at": NOON - 60, "expires_at": NOON + 540}
        self.assertFalse(layered_budget.redate_hold(dated, NOON))
        self.assertEqual(dated["expires_at"], NOON + 540)

    def test_hold_creation_time_is_validated(self):
        # Absent in holds written before they were dated; after the expiry, merely expired.
        for fields, valid in (({}, True), ({"created_at": NOON}, True), ({"created_at": NOON + DAY}, True),
                              ({"created_at": -1}, False), ({"created_at": "x"}, False),
                              ({"created_at": None}, False), ({"created_at": True}, False)):
            data = {"schema": 1, "maximum": 3, "attempts": [],
                    "holds": {"x": {"remaining": 1, "expires_at": NOON + 600, **fields}}}
            with self.subTest(fields=fields):
                if valid:
                    b._validate(data)
                else:
                    with self.assertRaisesRegex(b.BudgetError, "creation time"):
                        b._validate(data)


class BudgetZoneTests(ClockSkewCase):
    def test_configured_zone_picks_the_bucket(self):
        expected = {"Asia/Shanghai": ("2026-10-15", "2026-10-16"), "UTC": ("2026-10-15", "2026-10-15"),
                    "America/New_York": ("2026-10-15", "2026-10-15")}
        for zone, days in expected.items():
            with self.subTest(zone=zone), patch.dict(os.environ, MAKEWAND_BUDGET_TIMEZONE=f" {zone} "):
                self.assertEqual((layered_budget.day_bucket(1792079999), layered_budget.day_bucket(1792080000)), days)
        for invalid in ("Asia/Nowhere", "Local", "../etc/passwd", "Asia"):
            with self.subTest(invalid=invalid), patch.dict(os.environ, MAKEWAND_BUDGET_TIMEZONE=invalid):
                with self.assertRaisesRegex(b.BudgetError, "MAKEWAND_BUDGET_TIMEZONE"):
                    layered_budget.day_bucket(0)
                with self.assertRaisesRegex(b.BudgetError, "MAKEWAND_BUDGET_TIMEZONE"):
                    b.reserve("fake", "fast", task_id="bad-zone")
                # A configuration error, as in Go: not an exhausted budget, and nothing runs.
                called = []
                result = execute(ExecutionRequest(engine="fake", task_id="bad-zone", stage="provider"),
                                 lambda timeout: called.append(timeout))
                self.assertEqual((result.status, result.error_kind, called),
                                 ("INTERNAL_ERROR", "budget_configuration", []))

    @unittest.skipUnless(hasattr(time, "tzset"), "time.tzset required")
    def test_unset_zone_follows_the_local_zone(self):
        try:
            for zone, day in (("Asia/Shanghai", "2026-10-16"), ("America/New_York", "2026-10-15")):
                with self.subTest(zone=zone), patch.dict(os.environ, MAKEWAND_BUDGET_TIMEZONE="", TZ=zone):
                    time.tzset()
                    self.assertEqual(layered_budget.day_bucket(1792080000), day)
        finally:
            time.tzset()

    def test_utc_needs_no_time_zone_database(self):
        # Go's LoadLocation resolves UTC without one; Windows without tzdata has none.
        from zoneinfo import ZoneInfoNotFoundError
        with patch("zoneinfo.ZoneInfo", side_effect=ZoneInfoNotFoundError("no time zone database")):
            with patch.dict(os.environ, MAKEWAND_BUDGET_TIMEZONE="UTC"):
                self.assertEqual(layered_budget.day_bucket(1792080000), "2026-10-15")
            with patch.dict(os.environ, MAKEWAND_BUDGET_TIMEZONE="Asia/Shanghai"):
                with self.assertRaises(b.BudgetConfigError):
                    layered_budget.day_bucket(1792080000)


class RaceBudgetZoneTests(unittest.TestCase):
    """`makewand race` refuses an invalid zone as a configuration error, as `run` and Go do."""
    fixtures = scheduling.SchedulingTests.fixtures

    def setUp(self):
        scheduling.SchedulingTests.setUp(self)
        for patcher in (patch("makewand.usage.record_engine_usage"),
                        patch.object(config, "CANDIDATES_DIR", self.root / "candidates"),
                        patch.object(orch, "CANDIDATES_DIR", self.root / "candidates"),
                        patch.dict(os.environ, MAKEWAND_LAYERED_BUDGET="1", MAKEWAND_BUDGET_TIMEZONE="Asia/Nowhere",
                                   MAKEWAND_CALL_BUDGET_FILE=str(self.root / "calls.json"),
                                   MAKEWAND_PROJECT_ROOT=str(self.repo))):
            patcher.start()
            self.addCleanup(patcher.stop)

    def test_invalid_zone_is_an_internal_error_and_dispatches_nothing(self):
        with self.fixtures(), contextlib.redirect_stderr(io.StringIO()):
            code = orch.run_race("Implement value 2", cwd=str(self.repo), engine_a="claude",
                                 engine_b="codex", timeout=60)
        self.assertEqual(code, EXIT_INTERNAL_ERROR)  # was 13, BUDGET_EXHAUSTED
        self.assertEqual(self.calls, [])
        self.assertEqual(last_result().status, "INTERNAL_ERROR")


class RaceHoldClockTests(unittest.TestCase):
    """`makewand race` dates its coder and judge holds from the ledger's effective clock."""
    fixtures = scheduling.SchedulingTests.fixtures

    def setUp(self):
        scheduling.SchedulingTests.setUp(self)
        self.ledger = self.root / "calls.json"
        for patcher in (patch("makewand.usage.record_engine_usage"),
                        patch.object(config, "CANDIDATES_DIR", self.root / "candidates"),
                        patch.object(orch, "CANDIDATES_DIR", self.root / "candidates"),
                        patch.object(layered_budget, "_warned", set()),
                        patch.dict(os.environ, MAKEWAND_LAYERED_BUDGET="1", MAKEWAND_BUDGET_TIMEZONE="UTC",
                                   MAKEWAND_CALL_BUDGET_FILE=str(self.ledger), MAKEWAND_PROJECT_ROOT=str(self.repo))):
            patcher.start()
            self.addCleanup(patcher.stop)

    def test_race_holds_keep_their_duration_under_the_permitted_lead(self):
        # Dated from wall time under a ledger clock 300 s ahead, each hold lost 300 s.
        b.complete(b.reserve("fake", "fast", task_id="seed"), True, 0)
        data = json.loads(self.ledger.read_text())
        lead = data["scopes"]["clock"] = time.time() + layered_budget.CLOCK_SLACK
        self.ledger.write_text(json.dumps(data))
        made, reserve_capacity = [], b.reserve_capacity

        def recorded(count, owner, purpose="review", ttl_seconds=600):
            lease = reserve_capacity(count, owner, purpose=purpose, ttl_seconds=ttl_seconds)
            made.append((purpose, ttl_seconds, json.loads(self.ledger.read_text())["holds"][lease]))
            return lease
        with self.fixtures(), patch.object(b, "reserve_capacity", side_effect=recorded), \
                contextlib.redirect_stderr(io.StringIO()):
            orch.run_race("Implement value 2", cwd=str(self.repo), engine_a="claude", engine_b="codex", timeout=60)
        self.assertEqual([purpose for purpose, _, _ in made], ["coder_a", "coder_b", "judge"])
        for purpose, ttl, hold in made:
            self.assertGreaterEqual(hold["created_at"], lead, purpose)
            self.assertEqual(hold["expires_at"] - hold["created_at"], ttl, purpose)


class RepairTests(ClockSkewCase):
    def skewed_ledger(self):
        """Two calls, a live lease and a capacity hold, written while the clock read tomorrow."""
        real = time.time()
        with patch("time.time", return_value=real + DAY), execution_context(task_id="skew-task"):
            self.spend(task_id="ahead-1")
            self.spend(task_id="ahead-2")
            self.lease = b.reserve("codex", "fast", task_id="ahead-3", account_ref=ACCOUNT)
            self.hold = b.reserve_capacity(1, "review", ttl_seconds=layered_budget.MAX_HOLD_SECONDS)
        return layered_budget.day_bucket(real), layered_budget.day_bucket(real + DAY)

    def test_report_names_the_skew_and_writes_nothing(self):
        today, tomorrow = self.skewed_ledger()
        before = self.path.read_bytes()
        report = b.repair_clock()
        self.assertEqual(self.path.read_bytes(), before)
        self.assertTrue(report["skewed"])
        self.assertFalse(report["applied"])
        self.assertGreater(report["clock_lead_seconds"], DAY - 120)
        self.assertEqual((report["day"], report["today"]), (tomorrow, today))
        self.assertEqual((report["skewed_leases"], report["skewed_holds"]), (1, 1))

    def test_repair_brings_clock_day_lease_and_hold_back_without_refunding(self):
        os.environ["MAKEWAND_MACHINE_DAILY_CALLS"] = "5"
        today, tomorrow = self.skewed_ledger()
        report = b.repair_clock(apply=True)
        now = time.time()
        self.assertTrue(report["applied"])
        data = self.data()
        scopes = data["scopes"]
        self.assertLessEqual(scopes["clock"], now)
        self.assertEqual(scopes["day"], today)
        # The calls stay counted in tomorrow's bucket and are charged to today's as well.
        self.assertEqual(scopes["machine"][tomorrow]["used"], 3)
        self.assertEqual(scopes["machine"][today]["used"], 3)
        project = layered_budget.project_ref()
        self.assertEqual(scopes["projects"][f"{project}:{today}"]["used"], 3)
        self.assertLessEqual(data["leases"][self.lease]["expires_at"], now + leases.TTL)
        self.assertLessEqual(data["leases"][self.lease]["heartbeat_at"], now)
        # The hold moved back by its lead, keeping its duration, and into today's buckets.
        hold = data["holds"][self.hold]
        self.assertEqual(hold["expires_at"] - hold["created_at"], layered_budget.MAX_HOLD_SECONDS)
        self.assertLessEqual(hold["expires_at"], now + layered_budget.MAX_HOLD_SECONDS)
        self.assertEqual(hold["scopes"][1:], [["projects", f"{project}:{today}"], ["machine", today]])
        self.assertEqual(report["moved_holds"], 1)
        self.assertFalse(b.repair_clock()["skewed"])
        # Today's bucket now holds the earlier calls and the hold too: one free call left of five.
        self.spend(task_id="after-repair")
        with self.assertRaisesRegex(b.BudgetError, "budget exhausted"):
            b.reserve("fake", "fast", task_id="over")
        b.complete(b.reserve("fake", "fast", lease_id=self.hold, task_id="skew-task", stage="review"), True, 0)

    def test_repair_redates_a_dead_lease_from_a_skewed_writer(self):
        self.require_linux_identity()
        self.spend(task_id="seed")
        now = time.time()

        def skew(data):
            # The finding's ledger: clock two hours ahead, a dead owner's lease at clock + TTL.
            data["scopes"]["clock"] = now + 2 * 3600
            data["leases"] = {"dead": {"keys": leases.keys("codex", ACCOUNT), "owner": dead_owner(), "workers": [],
                                       "launch_pending": False, "heartbeat_at": now + 2 * 3600,
                                       "expires_at": now + 2 * 3600 + leases.TTL}}
        self.edit(skew)
        report = b.repair_clock(apply=True)
        self.assertEqual((report["skewed_leases"], report["applied"]), (1, True))
        self.assertLessEqual(self.data()["leases"]["dead"]["expires_at"], time.time() + leases.TTL)
        with patch("time.time", return_value=time.time() + leases.TTL + 1):
            b.complete(b.reserve("codex", "fast", task_id="unblocked", account_ref=ACCOUNT), True, 0)
        self.assertNotIn("dead", self.data().get("leases", {}))

    def test_missing_and_lifetime_ledgers_need_no_repair(self):
        self.assertFalse(b.repair_clock(apply=True)["exists"])
        self.assertFalse(self.path.exists())
        self.path.write_text(json.dumps({"schema": 1, "maximum": 5, "attempts": []}))
        before = self.path.read_bytes()
        # Even with layered admission enabled (as --call-budget-file sets it), the
        # repair neither converts a lifetime ledger nor persists a tighter maximum.
        os.environ["MAKEWAND_MAX_MODEL_CALLS"] = "3"
        for apply in (False, True):
            report = b.repair_clock(apply=apply)
            self.assertEqual((report["exists"], report["layered"], report["applied"]), (True, False, False))
            self.assertEqual(self.path.read_bytes(), before)


class BudgetCommandTests(ClockSkewCase):
    def run_cli(self, *args, worker=False):
        env = dict(os.environ, MAKEWAND_NO_DAEMON="1")
        env.pop("MAKEWAND_PROVIDER_WORKER", None)
        if worker:
            env["MAKEWAND_PROVIDER_WORKER"] = "1"
        return subprocess.run([sys.executable, "-I", str(REPO / "bin" / "makewand"), *args], env=env,
                              cwd=self.tmp.name, capture_output=True, text=True, timeout=60,
                              stdin=subprocess.DEVNULL)

    def test_budget_clock_reports_and_repairs(self):
        self.spend(task_id="seed")
        self.edit(lambda data: data["scopes"].update(clock=time.time() + 2 * 3600,
                                                     day=layered_budget.day_bucket(time.time() + DAY)))
        before = self.path.read_bytes()
        shown = self.run_cli("budget", "clock")
        self.assertEqual(shown.returncode, 0, shown.stderr)
        self.assertIn("领先实际时间", shown.stdout)
        self.assertIn("makewand budget clock --repair", shown.stdout)
        self.assertEqual(self.path.read_bytes(), before)
        refused = self.run_cli("budget", "clock", "--repair", worker=True)
        self.assertEqual(refused.returncode, 2)
        self.assertEqual(self.path.read_bytes(), before)
        repaired = self.run_cli("budget", "clock", "--repair", "--json")
        self.assertEqual(repaired.returncode, 0, repaired.stderr)
        report = json.loads(repaired.stdout)
        self.assertTrue(report["applied"])
        self.assertEqual(report["path"], str(self.path))
        self.assertEqual(self.data()["scopes"]["day"], layered_budget.day_bucket(time.time()))
        again = self.run_cli("budget", "clock")
        self.assertIn("时钟正常", again.stdout)


if __name__ == "__main__":
    unittest.main()
