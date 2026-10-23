"""Provider evidence retention, the read index and test isolation.

The Go mirror is execution/evidence_store_test.go; both runtimes compact the
store described by tests/testdata/evidence_retention_cases.json identically.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import call_budget, filelock
from makewand import execution_evidence as evidence
from makewand.accounting_files import AccountingFiles, pin
from makewand.execution_contract import ExecutionRequest
from makewand.execution_runtime import current_context, execute, mark_provider_invocation
from makewand.ledger_compaction import RETENTION_SECONDS

TESTS = Path(__file__).resolve().parent
VECTORS = TESTS / "testdata" / "evidence_retention_cases.json"
ACCOUNT_A = "codex:account:" + "a" * 32
ACCOUNT_B = "codex:account:" + "b" * 32


def retention_store():
    """Expand the shared vectors exactly as the Go test does."""
    vectors = json.loads(VECTORS.read_text(encoding="utf-8"))

    def record(name, *layers):
        value = {"attempt_id": name[:-len(".json")]}
        for layer in (vectors["base_record"], *layers):
            value.update(layer)
        return json.dumps(value).encode()

    files = []
    for item in vectors["files"]:
        content = item["raw"].encode() if "raw" in item else record(item["name"], item["record"])
        files.append(dict(name=item["name"], age=item["age_minutes"] * 60, lock=item.get("lock", False),
                          kept=item["kept"], indexed=item.get("indexed", False), content=content))
    cap = vectors["cap"]
    for i in range(cap["settled"]):
        name = "cap-settled-%03d.json" % i
        kept = name not in cap["pruned"]
        files.append(dict(name=name, age=(cap["newest_age_minutes"] + cap["settled"] - 1 - i) * 60, lock=True,
                          kept=kept, indexed=kept, content=record(name, cap["record"], {
                              "settled": True, "result_status": "PASSED", "outcome_known": True})))
    for i in range(cap["open"]):
        name = "cap-open-%d.json" % i
        files.append(dict(name=name, age=(cap["newest_age_minutes"] + cap["settled"] + i) * 60, lock=False,
                          kept=True, indexed=True, content=record(name, cap["record"], {"settled": False})))
    return vectors, files


def saved_record(attempt, account, **fields):
    record = dict(schema=1, task_id="retention", attempt_id=attempt, engine="codex", account_ref=account,
                  identity_known=True, pool_ref=account + ":subscription", source="cli_json", observed_at_ms=1,
                  windows=[], result_status="UNKNOWN", outcome_known=False, tokens=None, process_exit_code=None,
                  settled=False)
    record.update(fields)
    return record


class EvidenceStoreTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.store = self.root / "evidence"
        self.store.mkdir()
        environment = patch.dict(os.environ, {"MAKEWAND_EXECUTION_EVIDENCE_DIR": str(self.store)})
        environment.start()
        self.addCleanup(environment.stop)
        for key in ("MAKEWAND_CALL_BUDGET_FILE", "MAKEWAND_MAX_MODEL_CALLS"):
            os.environ.pop(key, None)

    def write_aged(self, name, content, at_ns):
        path = self.store / name
        path.write_bytes(content)
        os.utime(path, ns=(at_ns, at_ns))

    def index(self):
        lines = (self.store / evidence.INDEX).read_bytes().splitlines()
        return json.loads(lines[0]), [json.loads(line) for line in lines[1:]]

    def test_shared_vectors_prune_like_go(self):
        vectors, files = retention_store()
        self.assertEqual(vectors["retention_minutes"] * 60, RETENTION_SECONDS)
        self.assertEqual(vectors["keep_per_account"], evidence.KEEP_PER_ACCOUNT)
        now = time.time()
        now_ns = int(now * 1e9)
        for item in files:
            at = now_ns - item["age"] * 10**9
            self.write_aged(item["name"], item["content"], at)
            if item["lock"]:
                self.write_aged(item["name"] + ".lock", b"", at)
        evidence.compact(force=True, now=now)
        for item in files:
            self.assertEqual((self.store / item["name"]).exists(), item["kept"], item["name"])
            if item["lock"]:
                self.assertEqual((self.store / (item["name"] + ".lock")).exists(), item["kept"], item["name"] + ".lock")
        header, entries = self.index()
        self.assertEqual(header, {"schema": 1, "compacted_at_ms": int(now * 1000)})
        indexed = sorted((item for item in files if item["kept"] and item["indexed"]),
                         key=lambda item: (-item["age"], item["name"]))
        self.assertEqual([entry["attempt_id"] for entry in entries], [item["name"][:-len(".json")] for item in indexed])
        other = next(entry for entry in entries if entry["attempt_id"] == "other-engine-fresh")
        self.assertEqual((other["engine"], other["account_ref"], other["pool_ref"]), ("agy", "", ""))

    def test_reads_follow_the_index_and_select_by_account(self):
        legacy = saved_record("legacy", ACCOUNT_A, result_status="PASSED", outcome_known=True)
        del legacy["settled"]
        self.write_aged("legacy.json", json.dumps(legacy).encode(), time.time_ns() - 3600 * 10**9)
        # An older runner left no index: the directory is scanned as before.
        self.assertEqual([r["attempt_id"] for r in evidence.read_recent(engine="codex", account_ref=ACCOUNT_A)], ["legacy"])
        saved = []
        for i in range(5):
            account = ACCOUNT_B if i % 2 else ACCOUNT_A
            evidence.save(saved_record("attempt%d" % i, account))
            if account == ACCOUNT_A:
                saved.append("attempt%d" % i)
            at = time.time_ns() + (i - 10) * 10**9  # coarse filesystem clocks must not reorder saves
            os.utime(self.store / ("attempt%d.json" % i), ns=(at, at))
        newest = [r["attempt_id"] for r in evidence.read_recent(
            engine="codex", account_ref=ACCOUNT_A, pool_ref=ACCOUNT_A + ":subscription", limit=2)]
        self.assertEqual(newest, [saved[2], saved[1]])
        # With an index the reader never lists the directory, so the record
        # written without one stays unread until compaction indexes it.
        self.assertNotIn("legacy", [r["attempt_id"] for r in evidence.read_recent(account_ref=ACCOUNT_A)])
        evidence.compact(force=True)
        self.assertEqual([r["attempt_id"] for r in evidence.read_recent(account_ref=ACCOUNT_A)], [*reversed(saved), "legacy"])
        self.assertEqual([r["attempt_id"] for r in evidence.read_recent()][:1], ["attempt4"])

    def test_reader_window_skips_torn_and_foreign_lines(self):
        line = lambda attempt: json.dumps({"attempt_id": attempt, "engine": "codex", "account_ref": "", "pool_ref": ""}) + "\n"
        image = line("oldest")
        while len(image) <= evidence.INDEX_WINDOW:
            image += line("filler")
        image += "{torn\n" + json.dumps({"attempt_id": "../escape", "engine": "codex"}) + "\n" + line("newest")
        (self.store / evidence.INDEX).write_text(image)
        with pin(self.store / evidence.INDEX) as files:
            entries = evidence._index_entries(files)
        self.assertEqual([entry["attempt_id"] for entry in entries], ["newest", "filler"])

    def test_index_image_keeps_lines_appended_during_compaction(self):
        now = time.time()
        records = [dict(name="a.json", group=("codex", "x", "x:subscription"), indexed=True),
                   dict(name="deleted.json", group=("codex", "x", "x:subscription"), indexed=True),
                   dict(name="odd name.json", group=("", "", ""), indexed=False),
                   dict(name="b.json", group=("agy", "", ""), indexed=True)]
        appended = (b'{"attempt_id":"a","engine":"codex","account_ref":"y","pool_ref":"y:subscription"}\n'
                    b'{"attempt_id":"new","engine":"claude","account_ref":"","pool_ref":""}\n{torn')
        image = evidence._index_image(records, {"deleted.json"}, appended, now)
        self.assertEqual(image, b"".join(line + b"\n" for line in (
            b'{"schema":1,"compacted_at_ms":%d}' % int(now * 1000),
            b'{"attempt_id":"b","engine":"agy","account_ref":"","pool_ref":""}',
            b'{"attempt_id":"a","engine":"codex","account_ref":"y","pool_ref":"y:subscription"}',
            b'{"attempt_id":"new","engine":"claude","account_ref":"","pool_ref":""}')))

    def test_compaction_is_hourly_and_skips_held_records(self):
        now = time.time()
        old = saved_record("old", ACCOUNT_A, settled=True, result_status="PASSED", outcome_known=True)
        self.write_aged("old.json", json.dumps(old).encode(), int(now * 1e9) - 49 * 3600 * 10**9)
        (self.store / evidence.INDEX).write_text(json.dumps({"schema": 1, "compacted_at_ms": int((now - 600) * 1000)}) + "\n")
        evidence.compact(now=now)
        self.assertTrue((self.store / "old.json").exists(), "compaction ran before it was due")
        with pin(self.store / "old.json") as files:
            held = files.open(files.lock_name, create=True, writable=True)
            try:
                filelock.flock(held, filelock.LOCK_EX | filelock.LOCK_NB)
                evidence.compact(now=now + 3600)
                self.assertTrue((self.store / "old.json").exists(), "a held record was deleted")
                filelock.flock(held, filelock.LOCK_UN)
            finally:
                os.close(held)
        self.assertEqual(self.index()[0]["compacted_at_ms"], int((now + 3600) * 1000))
        evidence.compact(now=now + 7200)
        self.assertFalse((self.store / "old.json").exists())
        self.assertFalse((self.store / "old.json.lock").exists())


class SettlementTests(unittest.TestCase):
    """Retention may delete a record only once nothing can still reference it."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.store = self.root / "evidence"
        environment = patch.dict(os.environ, {"MAKEWAND_EXECUTION_EVIDENCE_DIR": str(self.store)})
        environment.start()
        self.addCleanup(environment.stop)
        for key in ("MAKEWAND_CALL_BUDGET_FILE", "MAKEWAND_MAX_MODEL_CALLS"):
            os.environ.pop(key, None)

    def request(self):
        return ExecutionRequest(task_id="settlement", stage="review", engine="offline-fixture", prompt="fixture", readonly=True)

    def record(self, result):
        return json.loads((self.store / (result.attempt_id + ".json")).read_text())

    def run_attempt(self):
        during = []

        def callback(timeout):
            attempt = current_context()["_evidence"]["attempt_id"]
            during.append(json.loads((self.store / (attempt + ".json")).read_text()))
            return True, "ok", None
        result = execute(self.request(), callback)
        self.assertEqual(result.status, "PASSED")
        self.assertIs(during[0]["settled"], False, "a running attempt's record claimed settlement")
        return result

    def test_without_ledger_the_final_record_is_settled(self):
        self.assertTrue(self.record(self.run_attempt())["settled"])

    def test_ledger_settles_before_the_record(self):
        with patch.dict(os.environ, {"MAKEWAND_CALL_BUDGET_FILE": str(self.root / "budget.json"), "MAKEWAND_MAX_MODEL_CALLS": "5"}):
            self.assertTrue(self.record(self.run_attempt())["settled"])

    def test_failed_settled_record_is_unknown_and_keeps_the_original_charge(self):
        ledger = self.root / "budget.json"
        save = evidence.save

        def fail_settled_success(record):
            if record.get("settled") and record.get("result_status") == "PASSED":
                raise OSError("final settlement record unavailable")
            save(record)

        def callback(timeout):
            mark_provider_invocation()
            return True, "ok", None

        with patch.dict(os.environ, {"MAKEWAND_CALL_BUDGET_FILE": str(ledger), "MAKEWAND_MAX_MODEL_CALLS": "5"}), \
                patch.object(evidence, "save", side_effect=fail_settled_success):
            result = execute(self.request(), callback)
        self.assertEqual(result.status, "UNKNOWN")
        self.assertFalse(result.outcome_known)
        rows = json.loads(ledger.read_text())["attempts"]
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["status"], "completed")
        self.assertEqual(rows[0]["result_status"], "UNKNOWN")
        self.assertFalse(rows[0]["outcome_known"])
        self.assertEqual(self.record(result)["result_status"], "UNKNOWN")

    def assert_pending_retained(self, result):
        path = self.store / (result.attempt_id + ".json")
        self.assertTrue(path.with_suffix(".json.pending").exists())
        self.assertEqual(list(evidence.read_recent()), [])
        evidence.compact(force=True, now=time.time() + 49 * 3600)
        self.assertTrue(path.exists(), "retention deleted a record with uncertain publication")

    def test_first_record_rename_then_sync_failure_never_launches_or_degrades(self):
        replace = AccountingFiles.replace

        def fail_after_replace(files, name, fd):
            replace(files, name, fd)
            if files.path.parent == self.store:
                raise OSError("directory sync failed after record rename")

        called = []
        with patch.object(AccountingFiles, "replace", fail_after_replace):
            result = execute(self.request(), lambda timeout: called.append(timeout))
        self.assertEqual(called, [], "provider launched despite an uncertain first record")
        self.assertEqual(result.status, "UNKNOWN")
        self.assertFalse(result.outcome_known)
        self.assert_pending_retained(result)
        record = self.record(result)
        record.update(result_status="UNKNOWN", outcome_known=False, settled=True)
        evidence.save(record)
        self.assertFalse((self.store / (result.attempt_id + ".json.pending")).exists())
        self.assertEqual(list(evidence.read_recent())[0]["result_status"], "UNKNOWN")

    def test_published_passed_record_is_ignored_if_sync_and_correction_fail(self):
        ledger = self.root / "budget.json"
        replace = AccountingFiles.replace
        failed = False

        def fail_final_and_correction(files, name, fd):
            nonlocal failed
            if files.path.parent == self.store and failed:
                raise OSError("corrective record cannot be published")
            replace(files, name, fd)
            if files.path.parent == self.store:
                record = json.loads(files.path.read_text())
                if record.get("settled") and record.get("result_status") == "PASSED":
                    failed = True
                    raise OSError("settled record directory sync failed")

        def callback(timeout):
            mark_provider_invocation()
            return True, "ok", None

        with patch.dict(os.environ, {"MAKEWAND_CALL_BUDGET_FILE": str(ledger), "MAKEWAND_MAX_MODEL_CALLS": "5"}), \
                patch.object(AccountingFiles, "replace", fail_final_and_correction):
            result = execute(self.request(), callback)
        self.assertTrue(failed, "test never reached the post-rename failure")
        self.assertEqual(result.status, "UNKNOWN")
        self.assertFalse(result.outcome_known)
        rows = json.loads(ledger.read_text())["attempts"]
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["status"], "completed")
        self.assertEqual(rows[0]["result_status"], "UNKNOWN")
        self.assertFalse(rows[0]["outcome_known"])
        self.assertEqual(self.record(result)["result_status"], "PASSED", "fault must leave the stale published version")
        self.assert_pending_retained(result)

    def test_held_lease_or_failed_settlement_keeps_the_record_open(self):
        with patch.dict(os.environ, {"MAKEWAND_CALL_BUDGET_FILE": str(self.root / "budget.json"), "MAKEWAND_MAX_MODEL_CALLS": "5"}):
            with patch.object(call_budget, "complete", return_value=False):
                self.assertFalse(self.record(self.run_attempt())["settled"])
            with patch.object(call_budget, "complete", side_effect=call_budget.BudgetError("ledger unavailable")):
                result = execute(self.request(), lambda timeout: (True, "ok", None))
            self.assertEqual(result.status, "UNKNOWN")
            self.assertFalse(self.record(result)["settled"])


class IsolationGuardTests(unittest.TestCase):
    """GEC-1021-4: test runs never write evidence into the user's cache."""

    def test_every_platform_default_resolves_inside_the_isolation_root(self):
        root = Path(_isolation.state().root).resolve()
        with tempfile.TemporaryDirectory() as real:
            # LocalAppData, the Windows user cache, is not under the isolated HOME.
            for platform in ("win32", "darwin", "linux"):
                with patch.object(sys, "platform", platform), patch.dict(os.environ, {"LOCALAPPDATA": real, "LocalAppData": real}):
                    self.assertTrue(evidence.directory().resolve().is_relative_to(root), platform)

    def test_fresh_test_process_writes_into_its_own_root(self):
        with tempfile.TemporaryDirectory() as real:
            environment = {key: value for key, value in os.environ.items()
                           if key not in (_isolation.ROOT_ENV, "MAKEWAND_EXECUTION_EVIDENCE_DIR")}
            environment.update(HOME=real, XDG_CACHE_HOME=os.path.join(real, ".cache"), LOCALAPPDATA=real, LocalAppData=real)
            code = ("import sys; sys.path[:0] = [sys.argv[1], sys.argv[2]]; import _isolation; "
                    "sys.platform = 'win32'; from makewand import execution_evidence as e; "
                    "e.save(dict(schema=1, task_id='guard', attempt_id='guard', engine='isolation-guard', "
                    "account_ref='', identity_known=False, pool_ref='', source='unavailable', observed_at_ms=1, "
                    "windows=[], result_status='PASSED', outcome_known=True, tokens=None, process_exit_code=None, settled=True))")
            completed = subprocess.run([sys.executable, "-I", "-c", code, str(TESTS), str(TESTS.parent)],
                                       env=environment, capture_output=True, text=True, timeout=120)
            self.assertEqual(completed.returncode, 0, completed.stderr)
            leaked = [str(path) for path in Path(real).rglob("*")
                      if path.is_file() and "execution-evidence" in path.parts]
            self.assertEqual(leaked, [], "a test process wrote evidence into the user cache")


if __name__ == "__main__":
    unittest.main()
