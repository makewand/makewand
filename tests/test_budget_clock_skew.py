"""A call budget ledger written while the clock was ahead must not stop dispatch or lease cleanup.

Backward day, forward skew with a dead lease, a capacity hold created under a
leading clock, the daily bucket's zone, and the operator repair
(``makewand budget clock --repair``). execution/budget_clock_test.go checks the
same rule in Go and that both languages pick the same bucket. Temporary ledgers
only; no model is called.
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

from makewand import account_leases as leases
from makewand import call_budget as b
from makewand import layered_budget
from makewand.execution_runtime import execution_context

REPO = Path(__file__).resolve().parent.parent
ACCOUNT = "codex:account:" + "a" * 32
DAY = 86400


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
        os.environ["MAKEWAND_MACHINE_DAILY_CALLS"] = "3"
        self.spend(task_id="before")
        today = layered_budget.day_bucket(time.time())
        tomorrow = layered_budget.day_bucket(time.time() + DAY)
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

    @unittest.skipUnless(hasattr(time, "tzset"), "time.tzset required")
    def test_unset_zone_follows_the_local_zone(self):
        try:
            for zone, day in (("Asia/Shanghai", "2026-10-16"), ("America/New_York", "2026-10-15")):
                with self.subTest(zone=zone), patch.dict(os.environ, MAKEWAND_BUDGET_TIMEZONE="", TZ=zone):
                    time.tzset()
                    self.assertEqual(layered_budget.day_bucket(1792080000), day)
        finally:
            time.tzset()


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
        os.environ["MAKEWAND_MACHINE_DAILY_CALLS"] = "4"
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
        self.assertLessEqual(data["holds"][self.hold]["expires_at"], now + layered_budget.MAX_HOLD_SECONDS)
        self.assertFalse(b.repair_clock()["skewed"])
        # Today's bucket now holds the earlier calls too: one call left of four.
        self.spend(task_id="after-repair")
        with self.assertRaisesRegex(b.BudgetError, "budget exhausted"):
            b.reserve("fake", "fast", task_id="over")

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
        for key in ("MAKEWAND_LAYERED_BUDGET", "MAKEWAND_MACHINE_DAILY_CALLS",
                    "MAKEWAND_PROJECT_DAILY_CALLS", "MAKEWAND_MAX_MODEL_CALLS"):
            os.environ.pop(key)  # restored by setUp's patch
        before = self.path.read_bytes()
        report = b.repair_clock(apply=True)
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
