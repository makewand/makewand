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


    def test_effective_clock_caps_the_lead_and_warns_once(self):
        ahead = {"scopes": {"clock": 1000.0 + 7200}}
        self.assertEqual(layered_budget.effective_clock(ahead, now=1000.0), 1000.0 + layered_budget.CLOCK_SLACK)
        self.assertEqual(layered_budget.effective_clock(ahead, now=1000.0), 1000.0 + layered_budget.CLOCK_SLACK)
        # Within the slack the persisted clock still never runs backwards.
        self.assertEqual(layered_budget.effective_clock({"scopes": {"clock": 1100.0}}, now=1000.0), 1100.0)
        self.assertEqual(layered_budget.effective_clock({"scopes": {"clock": 900.0}}, now=1000.0), 1000.0)
        self.assertEqual(layered_budget.effective_clock({}, now=1000.0), 1000.0)
        self.assertEqual(self.stderr.getvalue().count("ahead of real time"), 1)


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
