"""成绩账本与额度标记：统计、时间衰减、坏行、并发追加、原子标记。

全部使用临时目录。默认路径只在显式改写 XDG_STATE_HOME / HOME 后解析，
不碰真实状态目录。
"""

try:
    import _isolation  # noqa: F401  隔离必须先于 makewand
except ImportError:  # python3 -m unittest tests.test_routing_ledger
    from tests import _isolation  # noqa: F401

import json
import multiprocessing
import os
import stat
import tempfile
import threading
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import filelock
from makewand.outcome_ledger import OutcomeLedger

_DAY = 86400.0


def _append_outcomes_worker(directory: str, provider: str, count: int, ts0: float) -> None:
    """子进程入口：连续追加 count 条完整的成绩记录。"""
    ledger = OutcomeLedger(directory)
    for index in range(count):
        ledger.record(
            provider,
            1,
            "implement",
            index % 2 == 0,
            seconds=float(index + 1),
            calls=index,
            ts=ts0 + index,
        )


def _mode(path: Path) -> int:
    return stat.S_IMODE(path.stat().st_mode)


class OutcomeLedgerTests(unittest.TestCase):
    def test_valid_review_of_rejected_code_improves_reviewer_statistics(self):
        now = 1_700_000_000.0
        self.ledger.record("codex", 3, "review", True, seconds=4, calls=1, ts=now,
                           review_validated=True, candidate_approved=False,
                           request_id="user-42", caller="agy", candidate_hash="b" * 64)
        stats = self.ledger.stats("codex", 3, "review", now=now)
        self.assertEqual((stats.alpha, stats.beta, stats.avg_seconds, stats.avg_calls), (2, 1, 4, 1))
        self.assertFalse(next(self.ledger.records())["candidate_approved"])

    def test_cooldown_uses_failure_kind_not_candidate_verdict(self):
        now = 1_700_000_000.0
        self.ledger.record("codex", 3, "review", False, ts=now, failure_kind="startup", request_id="one")
        self.assertIn("codex", self.ledger.request_cooldowns("one", now=now + 299))
        self.assertNotIn("codex", self.ledger.request_cooldowns("one", now=now + 301))
        self.ledger.record("agy", 3, "review", False, ts=now, failure_kind="unknown", request_id="one")
        self.assertIn("agy", self.ledger.request_cooldowns("one", now=now + 3600))
        self.assertEqual(self.ledger.request_cooldowns("other", now=now + 1), {})
        self.ledger.record("grok", 3, "review", True, ts=now, review_validated=True,
                           candidate_approved=False, request_id="one")
        self.assertNotIn("grok", self.ledger.request_cooldowns("one", now=now + 1))

    def test_request_history_read_error_cannot_be_an_empty_history(self):
        self.ledger.record("codex", 3, "review", False, request_id="one", failure_kind="unknown")
        with patch("builtins.open", side_effect=PermissionError("unreadable")):
            with self.assertRaises(PermissionError):
                self.ledger.request_cooldowns("one")

    def test_request_history_corruption_does_not_erase_unknown(self):
        self.ledger.record("codex", 3, "review", False, request_id="one", failure_kind="unknown")
        with self.ledger.outcomes_path.open("a") as stream:
            stream.write('{"broken":\n')
        self.assertEqual(len(list(self.ledger.records())), 1)  # legacy statistics remain tolerant
        with self.assertRaisesRegex(ValueError, "corrupt"):
            self.ledger.request_cooldowns("one")

    def test_invalid_utf8_cannot_remove_request_retry_restrictions(self):
        for kind in ("unknown", "timeout"):
            with self.subTest(kind=kind):
                ledger = OutcomeLedger(self.base / kind)
                ledger.record("codex", 3, "review", False, request_id="one", failure_kind=kind)
                payload = ledger.outcomes_path.read_bytes()
                ledger.outcomes_path.write_bytes(payload.replace(b'"codex"', b'"cod\xffx"'))
                # Legacy quality statistics tolerate damaged text; request
                # restrictions must fail closed instead of renaming a provider.
                self.assertEqual(len(list(ledger.records())), 1)
                with self.assertRaises(UnicodeDecodeError):
                    ledger.request_cooldowns("one")

    def test_unknown_restriction_survives_all_later_failures(self):
        for kind in ("startup", "auth", "quota", "verification", "invalid_output"):
            with self.subTest(kind=kind):
                request = "request-" + kind
                self.ledger.record("codex", 3, "review", False, ts=100, request_id=request, failure_kind="unknown")
                self.ledger.record("codex", 3, "review", False, ts=101, request_id=request, failure_kind=kind)
                self.assertIn("retry-approved", self.ledger.request_cooldowns(request, now=10000).get("codex", ""))

    def test_clock_rollback_cannot_remove_retry_approval_boundary(self):
        for kind in ("unknown", "timeout"):
            with self.subTest(kind=kind):
                request = "clock-" + kind
                self.ledger.record("codex", 3, "review", False, ts=2000, request_id=request, failure_kind=kind)
                self.assertIn("retry-approved", self.ledger.request_cooldowns(request, now=1000).get("codex", ""))
                # Accepted work recorded after rollback must also be able to
                # supersede the restriction despite its lower wall timestamp.
                self.ledger.record("codex", 3, "review", True, ts=1001, request_id=request,
                                   review_validated=True, candidate_approved=False)
                self.assertNotIn("codex", self.ledger.request_cooldowns(request, now=1002))

    def test_invalid_details_cannot_poison_routing_history(self):
        for details in ({"candidate_hash": "not-a-hash"}, {"caller": "secret contents"},
                        {"request_id": "space is invalid"}, {"failure_kind": "unknown"},
                        {"review_validated": False}):
            with self.subTest(details=details), self.assertRaises(ValueError):
                self.ledger.record("codex", 3, "review", True, **details)

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-routing-ledger-")
        self.addCleanup(self._tmp.cleanup)
        self.base = Path(self._tmp.name)
        self.ledger = OutcomeLedger(self.base / "routing")

    def test_record_stats_match_beta_prior_and_means(self):
        now = 1_700_000_000.0
        empty = self.ledger.stats("codex", 1, "implement", now)
        self.assertEqual(empty.alpha, 1)
        self.assertEqual(empty.beta, 1)
        self.assertEqual(empty.n, 0)
        self.assertIsNone(empty.avg_seconds)
        self.assertIsNone(empty.avg_calls)

        locked = []
        real_flock = filelock.flock

        def spy_flock(fd, operation):
            locked.append(operation)
            return real_flock(fd, operation)

        with patch("makewand.outcome_ledger.time.time", side_effect=AssertionError("wall clock")), \
                patch("makewand.outcome_ledger.filelock.flock", spy_flock):
            self.ledger.record("codex", 1, "implement", True, seconds=10, calls=2, ts=now)
            self.ledger.record("codex", 1, "implement", False, seconds=30, calls=4, ts=now)
            self.ledger.record("codex", 1, "review", True, seconds=100, calls=9, ts=now)
            self.ledger.record("codex", 2, "implement", True, seconds=100, calls=9, ts=now)
            self.ledger.record("agy", 1, "implement", True, seconds=100, calls=9, ts=now)
            stats = self.ledger.stats("codex", 1, "implement", now)

        self.assertIn(filelock.LOCK_EX, locked)
        self.assertIn(filelock.LOCK_UN, locked)
        self.assertEqual(stats.alpha, 2)
        self.assertEqual(stats.beta, 2)
        self.assertEqual(stats.n, 2)
        self.assertEqual(stats.avg_seconds, 20)
        self.assertEqual(stats.avg_calls, 3)
        self.assertEqual(stats.count, 2)
        # 其它 provider / 档位 / 类型不混入，没有匹配行时仍是先验和 None。
        other = self.ledger.stats("claude", 1, "implement", now)
        self.assertEqual((other.alpha, other.beta, other.n), (1, 1, 0))
        self.assertEqual(other.count, 0)
        self.assertIsNone(other.avg_seconds)
        self.assertIsNone(other.avg_calls)

        lines = self.ledger.outcomes_path.read_text(encoding="utf-8").splitlines()
        self.assertEqual(len(lines), 5)
        for line in lines:
            row = json.loads(line)
            self.assertEqual(
                list(row),
                ["ts", "provider", "tier", "task_type", "passed", "seconds", "calls"],
            )

    def test_exponential_decay_around_half_life(self):
        half = 14 * _DAY
        # 注入 now，time.time 一旦被调用就失败。
        with patch("makewand.outcome_ledger.time.time", side_effect=AssertionError("wall clock")):
            self.ledger.record("codex", 2, "test", True, seconds=10, calls=2, ts=0)
            self.ledger.record("codex", 2, "test", False, seconds=40, calls=8, ts=half)
            untouched = self.ledger.stats("codex", 2, "test", now=-1)
            before = self.ledger.stats("codex", 2, "test", now=half / 2)
            at_half = self.ledger.stats("codex", 2, "test", now=half)
            after = self.ledger.stats("codex", 2, "test", now=half * 2)
            # 默认半衰期就是 14 天；改成 2 天时，年龄 2 天的权重应为 1/2。
            custom = self.ledger.stats("codex", 2, "test", now=2 * _DAY, half_life_days=2)

        self.assertEqual((untouched.alpha, untouched.beta, untouched.n), (1, 1, 0))
        self.assertIsNone(untouched.avg_seconds)
        self.assertIsNone(untouched.avg_calls)

        # 半衰期前：第一条年龄 0.5 个半衰期，权重 sqrt(1/2)；第二条还没发生。
        earlier_weight = 0.5 ** 0.5
        self.assertAlmostEqual(before.n, earlier_weight)
        self.assertAlmostEqual(before.alpha, 1 + earlier_weight)
        self.assertEqual(before.beta, 1)
        self.assertEqual(before.avg_seconds, 10)
        self.assertEqual(before.avg_calls, 2)
        self.assertGreater(before.n, 0.5)

        # 正好一个半衰期：旧样本权重 1/2，新样本权重 1。
        self.assertEqual(untouched.count, 0)
        self.assertEqual(before.count, 1)
        self.assertEqual(at_half.count, 2)
        self.assertEqual(at_half.n, 1.5)
        self.assertEqual(at_half.alpha, 1.5)
        self.assertEqual(at_half.beta, 2)
        self.assertEqual(at_half.avg_seconds, 30)
        self.assertEqual(at_half.avg_calls, 6)

        # 再过一个半衰期：两条权重同时减半，平均值不变，有效样本数减半。
        self.assertEqual(after.n, 0.75)
        self.assertEqual(after.alpha, 1.25)
        self.assertEqual(after.beta, 1.5)
        self.assertEqual(after.avg_seconds, 30)
        self.assertEqual(after.avg_calls, 6)
        self.assertLess(after.alpha - 1, at_half.alpha - 1)

        self.assertEqual(custom.n, 0.5)
        self.assertEqual(custom.alpha, 1.5)
        self.assertEqual(custom.beta, 1)
        self.assertEqual(custom.avg_seconds, 10)
        self.assertEqual(custom.avg_calls, 2)


    def test_stats_many_matches_individual_stats_and_reads_once(self):
        now = 1_700_000_000.0
        rows = [
            ("codex", 1, "implement", True, 10, 2, now, "gpt-6-luna", "low"),
            ("codex", 1, "implement", False, 30, 4, now - 14 * _DAY, "gpt-6-luna", "low"),
            ("codex", 2, "implement", True, 5, 1, now, "gpt-6-luna", "high"),
            ("codex", 1, "review", False, 7, 1, now, "gpt-6-luna", "low"),
            ("agy", 1, "implement", True, 3, 1, now, None, None),
            ("codex", 1, "implement", True, 99, 9, now + 1000, "gpt-6-luna", "low"),
        ]
        for provider, tier, task, passed, sec, calls, ts, model, effort in rows:
            kwargs = {}
            if model is not None:
                kwargs = {"model": model, "effort": effort}
            self.ledger.record(provider, tier, task, passed, seconds=sec, calls=calls, ts=ts, **kwargs)
        requests = [
            ("codex", 1, "implement"),
            ("codex", 1, "implement", "gpt-6-luna", "low"),
            ("codex", 1, "implement", "gpt-6-luna", "high"),
            ("codex", 9, "implement", "gpt-6-luna", "low"),
            ("agy", 1, "implement"),
            ("claude", 1, "implement"),
            {"provider": "codex", "tier": 1, "task_type": "implement", "model": "gpt-6-luna"},
        ]
        reads = []
        real_read = self.ledger._read_rows

        def counting():
            reads.append(1)
            return real_read()

        with patch.object(self.ledger, "_read_rows", counting):
            batch = self.ledger.stats_many(requests, now=now)
        self.assertEqual(len(reads), 1)
        self.assertEqual(len(batch), len(requests))
        for request, got in zip(requests, batch):
            if isinstance(request, dict):
                want = self.ledger.stats(
                    request["provider"], request["tier"], request["task_type"], now,
                    model=request.get("model"), effort=request.get("effort"))
            elif len(request) == 3:
                want = self.ledger.stats(request[0], request[1], request[2], now)
            else:
                want = self.ledger.stats(
                    request[0], request[1], request[2], now,
                    model=request[3], effort=request[4] if len(request) > 4 else None)
            self.assertEqual(
                (got.alpha, got.beta, got.n, got.count, got.avg_seconds, got.avg_calls),
                (want.alpha, want.beta, want.n, want.count, want.avg_seconds, want.avg_calls),
                request)
        # alias is the same implementation
        self.assertEqual(self.ledger.batch_stats(requests, now=now), batch)

    def test_bad_lines_are_skipped(self):
        good_pass = {
            "ts": 100,
            "provider": "codex",
            "tier": 1,
            "task_type": "implement",
            "passed": True,
            "seconds": 2,
            "calls": 1,
        }
        good_fail = {
            "ts": 100,
            "provider": "codex",
            "tier": 1,
            "task_type": "implement",
            "passed": False,
            "seconds": 4,
            "calls": 3,
        }
        other = dict(good_pass, provider="agy", seconds=100, calls=100)
        lines = [
            "not json\n",
            "\n",
            "   \n",
            "{}\n",
            json.dumps({"ts": 1}) + "\n",
            json.dumps(dict(good_pass, ts="yesterday")) + "\n",
            json.dumps(dict(good_pass, tier="1")) + "\n",
            json.dumps(dict(good_pass, passed="true")) + "\n",
            json.dumps(dict(good_pass, seconds="2")) + "\n",
            json.dumps(dict(good_pass, calls=1.5)) + "\n",
            json.dumps(dict(good_pass, provider="")) + "\n",
            json.dumps(dict(good_pass, tier=0)) + "\n",
            json.dumps(dict(good_pass, tier=-2)) + "\n",
            json.dumps(dict(good_pass, seconds=-1)) + "\n",
            json.dumps(dict(good_pass, calls=-2)) + "\n",
            "[]\n",
            json.dumps(good_pass, ensure_ascii=False) + "\n",
            "{truncated\n",
            json.dumps(other) + "\n",
            json.dumps(good_fail) + "\n",
        ]
        self.ledger.directory.mkdir(parents=True, exist_ok=True)
        self.ledger.outcomes_path.write_text("".join(lines), encoding="utf-8")
        stats = self.ledger.stats("codex", 1, "implement", now=100)
        self.assertEqual(stats.alpha, 2)
        self.assertEqual(stats.beta, 2)
        self.assertEqual(stats.n, 2)
        self.assertEqual(stats.avg_seconds, 3)
        self.assertEqual(stats.avg_calls, 2)
        self.assertEqual(stats.count, 2)
        self.assertEqual(len(list(self.ledger.records())), 3)

        # 坏行不挡住后续追加。
        self.ledger.record("codex", 1, "implement", True, seconds=6, calls=5, ts=100)
        stats = self.ledger.stats("codex", 1, "implement", now=100)
        self.assertEqual(stats.alpha, 3)
        self.assertEqual(stats.beta, 2)
        self.assertEqual(stats.n, 3)
        self.assertEqual(stats.avg_seconds, 4)
        self.assertEqual(stats.avg_calls, 3)


    def test_append_after_truncated_tail_line_keeps_new_record(self):
        self.ledger.record("codex", 1, "implement", True, seconds=1, calls=1, ts=100)
        # Simulate a crash mid-append: a truncated line with no newline.
        with open(self.ledger.outcomes_path, "ab") as stream:
            stream.write(b'{"ts": 100, "provider": "codex", "truncated')
        self.ledger.record("agy", 1, "implement", False, seconds=2, calls=1, ts=100)
        rows = list(self.ledger.records())
        self.assertEqual(
            [(row["provider"], row["passed"]) for row in rows],
            [("codex", True), ("agy", False)],
        )
        self.assertEqual(self.ledger.stats("agy", 1, "implement", now=100).count, 1)
        self.assertEqual(self.ledger.stats("codex", 1, "implement", now=100).count, 1)
        self.assertTrue(self.ledger.outcomes_path.read_bytes().endswith(b"\n"))

    @unittest.skipUnless("fork" in multiprocessing.get_all_start_methods(), "needs the fork start method")
    def test_two_processes_append_four_hundred_intact_lines(self):
        directory = self.ledger.directory
        ctx = multiprocessing.get_context("fork")
        procs = []
        try:
            for provider, ts0 in (("proc-a", 1_000_000.0), ("proc-b", 2_000_000.0)):
                proc = ctx.Process(
                    target=_append_outcomes_worker,
                    args=(str(directory), provider, 200, ts0),
                )
                proc.start()
                procs.append(proc)
            for proc in procs:
                proc.join(60)
                self.assertFalse(proc.is_alive(), "worker timed out")
                self.assertEqual(proc.exitcode, 0)
        finally:
            for proc in procs:
                if proc.is_alive():
                    proc.terminate()
                    proc.join(5)

        raw = self.ledger.outcomes_path.read_bytes()
        self.assertTrue(raw.endswith(b"\n"))
        lines = raw.splitlines()
        self.assertEqual(len(lines), 400)
        seen = {"proc-a": set(), "proc-b": set()}
        for line in lines:
            row = json.loads(line.decode("utf-8"))
            for key in ("ts", "provider", "tier", "task_type", "passed", "seconds", "calls"):
                self.assertIn(key, row)
            self.assertIn(row["provider"], seen)
            seen[row["provider"]].add(row["calls"])
            self.assertIs(type(row["passed"]), bool)
        self.assertEqual(seen["proc-a"], set(range(200)))
        self.assertEqual(seen["proc-b"], set(range(200)))

    def test_marks_round_trip_expiry_and_corrupt_file(self):
        with patch("makewand.outcome_ledger.time.time", side_effect=AssertionError("wall clock")):
            created = self.ledger.mark("codex", 1_000, "周额度")
            self.ledger.mark("agy", 2_000, "paused")
            active = self.ledger.active_marks(now=1_500)
            still = self.ledger.active_marks(now=999)
            expired = self.ledger.active_marks(now=1_000)
            both_expired = self.ledger.active_marks(now=2_000)

        self.assertEqual(created.until, 1_000)
        self.assertEqual(created.reason, "周额度")
        self.assertEqual(set(active), {"agy"})
        self.assertEqual(active["agy"].until, 2_000)
        self.assertEqual(active["agy"].reason, "paused")
        self.assertEqual(set(still), {"codex", "agy"})
        self.assertNotIn("codex", expired)
        self.assertIn("agy", expired)
        self.assertEqual(both_expired, {})

        self.ledger.mark("agy", 5_000, "again")
        reread = OutcomeLedger(self.ledger.directory)
        self.assertEqual(reread.active_marks(now=3_000)["agy"].reason, "again")
        self.assertEqual(reread.active_marks(now=3_000)["agy"].until, 5_000)
        # 过期条目仍留在文件里，只是 active_marks 不返回。
        stored = json.loads(self.ledger.marks_path.read_text(encoding="utf-8"))
        self.assertIn("codex", stored)

        self.ledger.marks_path.write_bytes(b"{not-json\xff")
        self.assertEqual(self.ledger.active_marks(now=0), {})
        self.ledger.mark("codex", 80, "recovered")
        stored = json.loads(self.ledger.marks_path.read_text(encoding="utf-8"))
        self.assertEqual(stored, {"codex": {"until": 80.0, "reason": "recovered"}})
        self.assertEqual(self.ledger.active_marks(now=0)["codex"].reason, "recovered")
        self.assertEqual(self.ledger.active_marks(now=80), {})
        self.assertEqual(_mode(self.ledger.marks_path), 0o600)

        self.ledger.marks_path.write_text("[1, 2, 3]", encoding="utf-8")
        self.assertEqual(self.ledger.active_marks(now=0), {})
        self.ledger.mark("muse", 10, "ok")
        stored = json.loads(self.ledger.marks_path.read_text(encoding="utf-8"))
        self.assertEqual(set(stored), {"muse"})
        self.assertEqual(stored["muse"]["reason"], "ok")

    def test_mark_write_is_atomic_replace(self):
        self.ledger.mark("codex", 100, "old")
        destination = self.ledger.marks_path
        inode_before = destination.stat().st_ino
        observed = {}
        real_replace = os.replace

        def spy(src, dst):
            observed["src"] = Path(src)
            observed["dst"] = Path(dst)
            observed["src_body"] = Path(src).read_text(encoding="utf-8")
            observed["dst_before"] = destination.read_text(encoding="utf-8")
            real_replace(src, dst)

        with patch("makewand.outcome_ledger.os.replace", spy):
            self.ledger.mark("agy", 300, "new")

        self.assertNotEqual(observed["src"], observed["dst"])
        self.assertEqual(observed["src"].parent, self.ledger.directory)
        self.assertEqual(observed["dst"], destination)
        self.assertEqual(set(json.loads(observed["dst_before"])), {"codex"})
        written = json.loads(observed["src_body"])
        self.assertEqual(set(written), {"codex", "agy"})
        self.assertEqual(json.loads(destination.read_text(encoding="utf-8")), written)
        self.assertNotEqual(destination.stat().st_ino, inode_before)
        self.assertEqual(list(self.ledger.directory.glob(".marks-*")), [])

        def boom(src, dst):
            raise OSError("replace failed")

        with patch("makewand.outcome_ledger.os.replace", boom):
            with self.assertRaises(OSError):
                self.ledger.mark("grok", 400, "nope")
        body = json.loads(destination.read_text(encoding="utf-8"))
        self.assertNotIn("grok", body)
        self.assertIn("codex", body)
        self.assertEqual(list(self.ledger.directory.glob(".marks-*")), [])

    def test_public_interface_has_no_legacy_aliases(self):
        import makewand.outcome_ledger as ledger_module

        stats = self.ledger.stats("codex", 1, "implement", now=0)
        self.assertEqual((stats.alpha, stats.beta, stats.n, stats.count), (1, 1, 0, 0))
        for name in ("record_outcome", "append", "get_stats", "base_dir", "dir"):
            self.assertFalse(hasattr(self.ledger, name), name)
        for name in ("record", "record_outcome", "stats", "get_stats", "mark", "active_marks"):
            self.assertFalse(hasattr(ledger_module, name), name)
        self.assertTrue(hasattr(ledger_module, "default_ledger"))
        self.assertTrue(hasattr(ledger_module, "routing_dir"))
        for name in (
            "mean_seconds",
            "average_seconds",
            "mean_calls",
            "average_calls",
            "effective_n",
            "to_dict",
            "keys",
            "items",
            "get",
        ):
            self.assertFalse(hasattr(stats, name), name)
        with self.assertRaises(TypeError):
            stats["alpha"]  # noqa: B018 - Stats 不再冒充 dict
        created = self.ledger.mark("codex", 100, "quota")
        self.assertEqual((created.until, created.reason), (100, "quota"))
        self.assertNotIsInstance(created, dict)
        self.assertFalse(hasattr(created, "until_ts"))
        with self.assertRaises(TypeError):
            created["until"]  # noqa: B018 - Mark 不再是 dict
        with self.assertRaises(TypeError):
            self.ledger.record("codex", 1, "implement", True, duration=1)
        with self.assertRaises(TypeError):
            self.ledger.record("codex", 1, "implement", True, duration_seconds=1)
        with self.assertRaises(TypeError):
            self.ledger.record("codex", 1, "implement", True, elapsed=1)
        with self.assertRaises(TypeError):
            self.ledger.record("codex", 1, "implement", True, call_count=1)
        with self.assertRaises(TypeError):
            self.ledger.record("codex", 1, "implement", True, timestamp=1)
        with self.assertRaises(TypeError):
            self.ledger.stats("codex", 1, "implement", now_ts=0)
        with self.assertRaises(TypeError):
            OutcomeLedger(self.base / "other", directory=self.base / "other")

    def test_records_snapshot_does_not_block_append(self):
        self.ledger.record("codex", 1, "implement", True, seconds=1, calls=1, ts=100)
        iterator = self.ledger.records()
        self.assertEqual(next(iterator)["provider"], "codex")
        done = []

        def append_one():
            self.ledger.record("codex", 1, "implement", False, seconds=2, calls=2, ts=101)
            done.append(True)

        worker = threading.Thread(target=append_one, daemon=True)
        worker.start()
        worker.join(10)
        self.assertFalse(worker.is_alive(), "records() 持锁堵住了并发追加")
        self.assertEqual(done, [True])
        self.assertEqual(self.ledger.stats("codex", 1, "implement", now=101).count, 2)

    @unittest.skipUnless(hasattr(os, "O_NOFOLLOW"), "needs O_NOFOLLOW")
    def test_record_refuses_symlinked_outcomes_file(self):
        victim = self.base / "victim.txt"
        self.ledger.directory.mkdir(parents=True, exist_ok=True)
        victim.write_text("keep\n", encoding="utf-8")
        os.chmod(victim, 0o644)
        os.symlink(victim, self.ledger.outcomes_path)
        with self.assertRaises(OSError):
            self.ledger.record("codex", 1, "implement", True, seconds=1, calls=1, ts=1)
        self.assertEqual(victim.read_text(encoding="utf-8"), "keep\n")
        self.assertEqual(_mode(victim), 0o644)

    def test_mark_tightens_preexisting_loose_lock_file(self):
        self.ledger.directory.mkdir(parents=True, exist_ok=True)
        lock_path = self.ledger.directory / "marks.json.lock"
        lock_path.write_text("", encoding="utf-8")
        os.chmod(lock_path, 0o644)
        self.ledger.mark("codex", 100, "quota")
        self.assertEqual(_mode(lock_path), 0o600)
        self.assertEqual(_mode(self.ledger.marks_path), 0o600)

    def test_record_fsyncs_directory_when_creating_outcomes_file(self):
        fresh = OutcomeLedger(self.base / "fresh-routing")
        self.assertFalse(fresh.outcomes_path.exists())
        calls = []
        real_fsync = os.fsync

        def spy(fd):
            calls.append(fd)
            return real_fsync(fd)

        with patch("makewand.outcome_ledger.os.fsync", spy):
            fresh.record("codex", 1, "implement", True, seconds=1, calls=1, ts=1)
        # 文件本身一次 fsync + 新建文件时目录一次 fsync。
        self.assertGreaterEqual(len(calls), 2)

    def test_record_raises_instead_of_spinning_on_zero_write(self):
        with patch("makewand.outcome_ledger.os.write", return_value=0):
            with self.assertRaises(OSError):
                self.ledger.record("codex", 1, "implement", True, seconds=1, calls=1, ts=1)

    def test_relative_base_dir_stays_anchored_after_chdir(self):
        work = self.base / "relwork"
        work.mkdir()
        original_cwd = os.getcwd()
        try:
            os.chdir(work)
            ledger = OutcomeLedger(Path("routing"))
            ledger.record("codex", 1, "implement", True, seconds=1, calls=1, ts=1)
            os.chdir(self.base)
            ledger.record("codex", 1, "implement", True, seconds=1, calls=1, ts=2)
        finally:
            os.chdir(original_cwd)
        lines = (work / "routing" / "outcomes.jsonl").read_text(encoding="utf-8").splitlines()
        self.assertEqual(len(lines), 2)
        self.assertFalse((self.base / "routing" / "outcomes.jsonl").exists())


    def test_read_only_ledger_needs_no_writable_directory(self):
        missing = OutcomeLedger(self.base / "missing" / "routing")
        self.assertFalse((self.base / "missing").exists())
        self.assertEqual(missing.stats("codex", 1, "implement", now=0).count, 0)
        self.assertEqual(list(missing.records()), [])
        self.assertEqual(missing.active_marks(now=0), {})
        self.assertFalse((self.base / "missing").exists())
        # Writing creates the directory (and only then).
        missing.record("codex", 1, "implement", True, seconds=1, calls=1, ts=1)
        self.assertTrue(missing.outcomes_path.is_file())
        self.assertEqual(missing.stats("codex", 1, "implement", now=1).count, 1)

    def test_state_dir_follows_xdg_and_is_private(self):
        loose = self.base / "loose"
        loose.mkdir()
        os.chmod(loose, 0o755)
        ledger = OutcomeLedger(loose)
        # Construction alone does not create/tighten: only writes do.
        self.assertEqual(_mode(loose), 0o755)
        ledger.record("codex", 1, "doc", False, seconds=1.5, calls=0, ts=1)
        self.assertEqual(_mode(loose), 0o700)
        self.assertEqual(_mode(ledger.outcomes_path), 0o600)

        xdg = self.base / "xdg"
        with patch.dict(os.environ, {"XDG_STATE_HOME": str(xdg)}, clear=False):
            os.environ.pop("MAKEWAND_ROUTING_DIR", None)
            default = OutcomeLedger()
            default.record("codex", 1, "implement", True, seconds=1, calls=1, ts=10)
            default.mark("codex", 20, "quota")
        routing = xdg / "makewand" / "routing"
        self.assertEqual(default.directory, routing)
        self.assertTrue((routing / "outcomes.jsonl").is_file())
        self.assertTrue((routing / "marks.json").is_file())
        self.assertEqual(_mode(routing), 0o700)
        self.assertEqual(_mode(routing.parent), 0o700)
        self.assertEqual(_mode(routing / "outcomes.jsonl"), 0o600)
        self.assertEqual(_mode(routing / "marks.json"), 0o600)
        # 相对的 XDG_STATE_HOME 无效，退回 HOME/.local/state。
        home = self.base / "home"
        with patch.dict(os.environ, {"HOME": str(home), "XDG_STATE_HOME": "relative"}, clear=False):
            os.environ.pop("MAKEWAND_ROUTING_DIR", None)
            fallback = OutcomeLedger()
            # Reading a missing ledger is an empty ledger and creates nothing.
            self.assertEqual(fallback.stats("codex", 1, "implement", now=0).count, 0)
            self.assertEqual(fallback.active_marks(now=0), {})
        expected = home / ".local" / "state" / "makewand" / "routing"
        self.assertEqual(fallback.directory, expected)
        self.assertFalse(expected.exists())


if __name__ == "__main__":
    unittest.main()
