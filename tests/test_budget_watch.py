"""observe lists raised task caps and redispatches after UNKNOWN/TIMEOUT."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import budget_watch, ledger_registry


def attempt(task, engine, started, seconds, status):
    return {"task_id": task, "engine": engine, "started_at": started, "seconds": seconds, "result_status": status}


class BudgetWatchTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.config = Path(self.tmp.name) / "config"
        self.config.mkdir()
        env = patch.dict(os.environ, {"MAKEWAND_CONFIG_DIR": str(self.config)})
        env.start()
        self.addCleanup(env.stop)

    def write(self, path, tasks, attempts):
        Path(path).write_text(json.dumps({"scopes": {"tasks": tasks}, "attempts": attempts}))

    def test_raised_caps_and_retries_after_failure_are_listed(self):
        now = 100000.0
        self.write(self.config / "call-budget.json",
                   {"raised": {"maximum": 7, "used": 7, "last_used": now - 10},
                    "default": {"maximum": 3, "used": 3, "last_used": now - 10},
                    "stale": {"maximum": 9, "used": 1, "last_used": now - 200000}},
                   [attempt("retried", "codex", now - 500, 30, "UNKNOWN"),
                    attempt("retried", "muse", now - 400, 60, "PASSED"),
                    attempt("parallel", "muse", now - 500, 300, "UNKNOWN"),
                    attempt("parallel", "codex", now - 499, 20, "PASSED"),
                    attempt("clean", "codex", now - 500, 30, "PASSED"),
                    attempt("clean", "codex", now - 400, 30, "PASSED")])
        found = {(f["kind"], f["task"]) for f in budget_watch.flags(now=now)}
        self.assertEqual(found, {("raised_cap", "raised"), ("retry_after_failure", "retried")})

    def test_parallel_peers_and_unknown_end_times_are_not_retries(self):
        now = 100000.0
        self.write(self.config / "call-budget.json", {},
                   [attempt("ensemble", "grok", now - 500, 0.8, "UNKNOWN"),
                    attempt("ensemble", "codex", now - 499.5, 30, "PASSED"),
                    {"task_id": "no-end", "engine": "muse", "started_at": now - 500, "result_status": "UNKNOWN"},
                    attempt("no-end", "codex", now - 100, 30, "PASSED"),
                    attempt("finished", "codex", now - 500, None, "TIMEOUT") | {"finished_at": now - 300},
                    attempt("finished", "codex", now - 200, 10, "PASSED")])
        self.assertEqual({(f["kind"], f["task"]) for f in budget_watch.flags(now=now)},
                         {("retry_after_failure", "finished")})

    def test_malformed_ledgers_do_not_crash(self):
        for data in ({"scopes": []}, {"scopes": 1}, {"scopes": {"tasks": {"t": {"maximum": float("inf"), "last_used": 1}}}},
                     {"attempts": [{"task_id": ["x"], "started_at": 1}, {"task_id": "t", "started_at": float("nan")}, 3]},
                     [], "text"):
            with self.subTest(data=data):
                (self.config / "call-budget.json").write_text(json.dumps(data))
                self.assertEqual(budget_watch.flags(now=10.0), [])

    def test_oversized_json_integers_are_ignored_in_every_numeric_field(self):
        huge = 10 ** 400
        for field in ("maximum", "used", "last_used"):
            with self.subTest(counter_field=field):
                counter = dict(maximum=3, used=1, last_used=1)
                counter[field] = huge
                self.write(self.config / "call-budget.json", {"t": counter}, [])
                self.assertEqual(budget_watch.flags(now=10.0), [])
        for field in ("started_at", "seconds", "finished_at"):
            with self.subTest(attempt_field=field):
                failed = {"task_id": "t", "engine": "grok", "started_at": 1, "result_status": "UNKNOWN", field: huge}
                self.write(self.config / "call-budget.json", {}, [failed, attempt("t", "muse", 8, 1, "PASSED")])
                self.assertEqual(budget_watch.flags(now=10.0), [])

    def test_registered_private_ledgers_are_included_and_bad_files_skipped(self):
        private = Path(self.tmp.name) / "private.json"
        self.write(private, {"private-task": {"maximum": 5, "used": 2, "last_used": 1000.0}}, [])
        bad = Path(self.tmp.name) / "bad.json"
        bad.write_text("{not json")
        (self.config / ledger_registry.REGISTRY_NAME).write_text(
            "\n".join(json.dumps({"path": str(p), "ts": 1}) for p in (private, bad)) + "\n")
        flags = budget_watch.flags(now=1500.0)
        self.assertEqual([(f["task"], f["ledger"]) for f in flags], [("private-task", str(private))])


if __name__ == "__main__":
    unittest.main()
