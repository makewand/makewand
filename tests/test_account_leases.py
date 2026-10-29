"""No vendor calls: account/pool leases under process races and crashes."""
try:
    import _isolation
except ImportError:
    from tests import _isolation
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from makewand import call_budget as b, account_leases as leases
from makewand.execution_contract import ExecutionRequest
from makewand.execution_runtime import execute

REPO = str(Path(__file__).resolve().parent.parent)
ACCOUNT = "codex:account:" + "a" * 32


class AccountLeaseTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "calls.json"
        env = patch.dict(os.environ, MAKEWAND_CALL_BUDGET_FILE=str(self.path),
                         MAKEWAND_LAYERED_BUDGET="1", MAKEWAND_MAX_MODEL_CALLS="100",
                         MAKEWAND_PROJECT_DAILY_CALLS="100", MAKEWAND_MACHINE_DAILY_CALLS="100",
                         MAKEWAND_ACCOUNT_CONCURRENCY="1", MAKEWAND_POOL_CONCURRENCY="1",
                         MAKEWAND_PROJECT_ROOT=self.tmp.name, MAKEWAND_TASK_ID="lease-task",
                         MAKEWAND_LEASE_WAIT_SECONDS="0")
        env.start()
        self.addCleanup(env.stop)

    def data(self):
        return json.loads(self.path.read_text())

    def save(self, data):
        self.path.write_text(json.dumps(data))

    def test_expiry_and_heartbeat_loss_do_not_release_live_owner(self):
        a = b.reserve("codex", "fast", account_ref=ACCOUNT)
        data = self.data()
        data["leases"][a]["expires_at"] = 0
        self.save(data)
        with self.assertRaisesRegex(b.BudgetError, "concurrency"):
            b.reserve("codex", "fast", task_id="other", account_ref=ACCOUNT)
        b.lease_update(a)
        self.assertGreater(self.data()["leases"][a]["expires_at"], 0)
        self.assertEqual(len(self.data()["attempts"]), 1)

    def test_live_worker_outlives_dead_owner_and_expired_lease(self):
        if not Path("/proc").is_dir():
            self.skipTest("Linux process identity required")
        worker = subprocess.Popen([sys.executable, "-I", "-c", "import time; time.sleep(20)"], start_new_session=True)
        self.addCleanup(lambda: worker.poll() is None and worker.kill())
        owner = subprocess.Popen([sys.executable, "-I", "-c", "import time; time.sleep(20)"], start_new_session=True)
        owner_ref = leases.process_ref(owner.pid)
        owner.kill()
        owner.wait(timeout=5)
        a = b.reserve("codex", "fast", account_ref=ACCOUNT)
        b.lease_update(a, "launch")
        b.lease_update(a, "worker", worker.pid)
        data = self.data()
        data["leases"][a].update(owner=owner_ref, expires_at=0)
        self.save(data)
        with self.assertRaisesRegex(b.BudgetError, "concurrency"):
            b.reserve("codex", "fast", account_ref=ACCOUNT)
        worker.kill()
        worker.wait(timeout=5)
        second = b.reserve("codex", "fast", account_ref=ACCOUNT)
        b.complete(second, False, 0, result_status="UNKNOWN", outcome_known=False)
        self.assertEqual(len(self.data()["attempts"]), 2)
        self.assertEqual(self.data()["scopes"]["tasks"]["lease-task"]["used"], 2)

    def test_unknown_launch_gap_reclaimed_after_proven_owner_death(self):
        a = b.reserve("codex", "fast", account_ref=ACCOUNT)
        b.lease_update(a, "launch")
        data = self.data()
        data["leases"][a]["owner"]["boot"] = "previous-boot"
        data["leases"][a]["expires_at"] = 0
        self.save(data)
        second = b.reserve("codex", "fast", account_ref=ACCOUNT)
        b.complete(second, False, 0)
        b.lease_update(a, "launch_failed")
        b.complete(a, False, 0)
        self.assertEqual(self.data()["leases"], {})

    def test_agy_aliases_share_unknown_capacity(self):
        a = b.reserve("agy", "fast")
        for name in ("gemini", "antigravity", "agy"):
            with self.subTest(name=name), self.assertRaisesRegex(b.BudgetError, "concurrency"):
                b.reserve(name, "fast")
        b.complete(a, False, 0)

    def test_unknown_directories_share_provider_pool(self):
        a = b.reserve("codex", "fast", account_ref="codex:unknown:directory-a")
        with self.assertRaisesRegex(b.BudgetError, "concurrency"):
            b.reserve("codex", "fast", account_ref="codex:unknown:directory-b")
        b.complete(a, False, 0)
        b.reserve("codex", "fast", account_ref=ACCOUNT)

    def test_login_switch_after_admission_cannot_launch_another_account(self):
        from makewand.sandbox import provider_sandbox_launch
        from makewand.providers.base import run_subprocess
        from makewand.account_identity import account_ref
        root = Path(self.tmp.name) / "login"
        root.mkdir()
        auth = root / "auth.json"
        auth.write_text('{"tokens":{"account_id":"A"}}')
        full = b.reserve("codex", "fast", account_ref=account_ref("codex", "B"))
        entered = []
        def callback(remaining):
            auth.write_text('{"tokens":{"account_id":"B"}}')
            with provider_sandbox_launch("codex", [sys.executable, "-I", "-c", "print('forbidden')"],
                                         str(Path(self.tmp.name) / "work"), readonly=True) as launch:
                entered.append(True)
                code, out, err, error = run_subprocess(launch.cmd, extra_env=launch.env)
                return code == 0, out, err
        with patch.dict(os.environ, CODEX_HOME=str(root)), \
                patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                patch("makewand.sandbox.authorize_unsandboxed_launch", return_value="fixture"):
            result = execute(ExecutionRequest(task_id="switch", stage="provider", engine="codex"), callback)
        self.assertFalse(result.success)
        self.assertEqual(entered, [])
        b.complete(full, False, 0)

    def test_copy_time_login_change_is_rejected(self):
        from makewand.sandbox import ProviderHome
        root = Path(self.tmp.name) / "login"
        root.mkdir()
        auth = root / "auth.json"
        auth.write_text('{"tokens":{"account_id":"A"}}')
        original = ProviderHome._copy_in
        def swapped(home, src, *args):
            auth.write_text('{"tokens":{"account_id":"B"}}')
            return original(home, src, *args)
        def callback(remaining):
            home = ProviderHome("codex")
            try:
                with patch.object(ProviderHome, "_copy_in", swapped):
                    home.materialize()
                return True, "forbidden", None
            finally:
                home.finalize(write_back=False)
        with patch.dict(os.environ, CODEX_HOME=str(root)):
            result = execute(ExecutionRequest(task_id="copy-switch", stage="provider", engine="codex"), callback)
        self.assertFalse(result.success)

    def test_unsandboxed_login_switch_invalidates_attribution(self):
        from makewand.sandbox import provider_sandbox_launch
        from makewand.providers.base import run_subprocess
        root = Path(self.tmp.name) / "login"
        root.mkdir()
        auth = root / "auth.json"
        auth.write_text('{"tokens":{"account_id":"A"}}')
        work = Path(self.tmp.name) / "work"
        work.mkdir()
        observed = []
        def callback(remaining):
            command = [sys.executable, "-I", "-c", "import os,json; print(json.load(open(os.path.join(os.environ['CODEX_HOME'],'auth.json')))['tokens']['account_id'])"]
            with provider_sandbox_launch("codex", command, str(work), readonly=True) as launch:
                auth.write_text('{"tokens":{"account_id":"B"}}')
                code, out, err, error = run_subprocess(launch.cmd, extra_env=launch.env)
                observed.append(out.strip())
                return code == 0, out, err
        with patch.dict(os.environ, CODEX_HOME=str(root)), \
                patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                patch("makewand.sandbox.authorize_unsandboxed_launch", return_value="fixture"):
            result = execute(ExecutionRequest(task_id="frozen", stage="provider", engine="codex"), callback)
        self.assertEqual(observed, ["B"])
        self.assertEqual(result.status, "UNKNOWN")
        self.assertFalse(result.outcome_known)

    def test_same_account_in_two_roots_and_account_switch(self):
        from makewand.account_identity import account_identity
        roots = [Path(self.tmp.name) / name for name in ("a", "b")]
        for root in roots:
            root.mkdir()
            (root / "auth.json").write_text('{"tokens":{"account_id":"fixture-account"}}')
        one, two = [account_identity("codex", root)["account_ref"] for root in roots]
        self.assertEqual(one, two)
        a = b.reserve("codex", "fast", account_ref=one)
        with self.assertRaises(b.BudgetError):
            b.reserve("codex", "fast", account_ref=two)
        (roots[1] / "auth.json").write_text('{"tokens":{"account_id":"other-fixture"}}')
        other = account_identity("codex", roots[1])["account_ref"]
        second = b.reserve("codex", "fast", account_ref=other)
        b.complete(second, True, 0)
        b.complete(a, True, 0)

    def test_crashed_reservation_keeps_count_and_reclaims_only_dead_lease(self):
        code = "import sys,os;sys.path.insert(0,sys.argv[1]);from makewand import call_budget as b;b.reserve('codex','fast',account_ref=sys.argv[2]);os._exit(0)"
        subprocess.run([sys.executable, "-I", "-c", code, REPO, ACCOUNT], check=True, timeout=5)
        data = self.data()
        for lease in data["leases"].values():
            lease["expires_at"] = 0
        self.save(data)
        a = b.reserve("codex", "fast", account_ref=ACCOUNT)
        b.complete(a, False, 0, result_status="CANCELLED", outcome_known=False)
        b.complete(a, True, 0)
        self.assertEqual(len(self.data()["attempts"]), 2)
        self.assertEqual(self.data()["scopes"]["tasks"]["lease-task"]["used"], 2)

    def test_provider_does_not_start_on_account_refusal(self):
        b.reserve("codex", "fast", account_ref=ACCOUNT)
        called = []
        request = ExecutionRequest(engine="codex", task_id="new", stage="review", readonly=True, account_ref=ACCOUNT)
        result = execute(request, lambda timeout: called.append(1))
        self.assertEqual(result.status, "FAILED")
        self.assertEqual(result.error_kind, "concurrency_busy")
        self.assertEqual(called, [])

    def test_fake_cli_normal_cleanup_releases_lease(self):
        from makewand.providers.base import run_subprocess
        request = ExecutionRequest(engine="codex", task_id="cleanup", stage="provider", readonly=True, account_ref=ACCOUNT)
        def callback(timeout):
            code, out, err, error = run_subprocess([sys.executable, "-I", "-c", "print('fake worker')"], timeout=5)
            return code == 0, out, str(error) if error else None
        for _ in range(2):
            self.assertEqual(execute(request, callback).status, "PASSED")
        self.assertEqual(self.data()["leases"], {})

    def test_cancelled_completion_cannot_release_live_worker(self):
        if not Path("/proc").is_dir():
            self.skipTest("Linux process identity required")
        worker = subprocess.Popen([sys.executable, "-I", "-c", "import time;time.sleep(20)"], start_new_session=True)
        try:
            a = b.reserve("codex", "fast", account_ref=ACCOUNT)
            b.lease_update(a, "worker", worker.pid)
            b.complete(a, False, 0, result_status="CANCELLED", outcome_known=False)
            with self.assertRaisesRegex(b.BudgetError, "concurrency"):
                b.reserve("codex", "fast", account_ref=ACCOUNT)
            worker.kill()
            worker.wait(timeout=5)
            b.lease_update(a, "stopped")
            self.assertEqual(self.data()["leases"], {})
            self.assertEqual(self.data()["scopes"]["tasks"]["lease-task"]["used"], 1)
        finally:
            if worker.poll() is None:
                worker.kill()
            worker.wait(timeout=5)

    def test_malformed_scope_and_lease_metadata_fail_closed(self):
        a = b.reserve("codex", "fast", account_ref=ACCOUNT)
        original = self.data()
        for edit in (lambda d: d["scopes"].update(clock=True),
                     lambda d: d["attempts"][0].update(scopes=[["wrong", "key"]]),
                     lambda d: d["leases"][a].update(expires_at=True),
                     lambda d: d["lease_limits"].update({ACCOUNT: True})):
            data = json.loads(json.dumps(original))
            edit(data)
            self.save(data)
            with self.assertRaises(b.BudgetError):
                b.reserve("codex", "fast", account_ref=ACCOUNT)

    def test_unknown_and_known_login_cannot_assert_independent_pools(self):
        known = b.reserve("codex", "fast", account_ref=ACCOUNT)
        with self.assertRaisesRegex(b.BudgetError, "concurrency"):
            b.reserve("codex", "fast", account_ref="codex:unknown:unbound")
        other = b.reserve("codex", "fast", account_ref="codex:account:" + "b" * 32)
        b.complete(other, True, 0)
        b.complete(known, True, 0)
        unknown = b.reserve("codex", "fast", account_ref="codex:unknown:unbound")
        with self.assertRaisesRegex(b.BudgetError, "concurrency"):
            b.reserve("codex", "fast", account_ref=ACCOUNT)
        b.complete(unknown, True, 0)
