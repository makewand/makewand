"""
G3 reliability regressions: `observe --clean-hung` must only ever touch processes
makewand itself dispatched, and only after explicit confirmation.

Covers runtime-state#9, replay-0926-memory#11 and py-reliability#1 (SIGTERM of
the user's Claude Code sessions, vim, ssh, psql, tail and long python jobs).
"""

import json
import os
import signal
import stat
import subprocess
import sys
import tempfile
import time
import unittest
import uuid
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.observer as observer
from makewand.observer import classify_operation, observe_all_dialogs

DISPATCH_ENV_VAR = "MAKEWAND_DISPATCH_ID"


def find_makewand_dispatched_processes(**kw):
    return observer.find_makewand_dispatched_processes(**kw)


def terminate_dispatched_processes(candidates, confirm_pids):
    return observer.terminate_dispatched_processes(candidates, confirm_pids)

USER_PROCS = [
    {"pid": 41001, "comm": "claude", "etimes": 3600, "args": "claude --resume"},
    {"pid": 41002, "comm": "vim", "etimes": 7200, "args": "vim notes.md"},
    {"pid": 41003, "comm": "ssh", "etimes": 9000, "args": "ssh prod-db"},
    {"pid": 41004, "comm": "psql", "etimes": 5400, "args": "psql analytics"},
    {"pid": 41005, "comm": "tail", "etimes": 4000, "args": "tail -f app.log"},
    {"pid": 41006, "comm": "python3", "etimes": 3000, "args": "python3 long_analysis.py"},
]


class TestUserSessionsAreNeverTargets(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-g3-observer-")
        p = patch.object(config, "CONFIG_DIR", Path(self._tmp.name) / "cfg")
        p.start()
        self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)

    def test_long_running_user_process_is_not_hung(self):
        for proc in USER_PROCS:
            with self.subTest(comm=proc["comm"]):
                cat, note = classify_operation("work", ["Working on refactor", "esc to interrupt"], {"load_1m": 1.0},
                                               long_proc=dict(proc, makewand_dispatched=False))
                self.assertNotEqual(cat, "hung_anomaly", note)

    def test_dispatched_process_can_be_flagged(self):
        proc = {"pid": 42001, "comm": "codex", "etimes": 3600, "args": "codex exec task", "makewand_dispatched": True}
        cat, _ = classify_operation("work", ["..."], {"load_1m": 1.0}, long_proc=proc)
        self.assertEqual(cat, "hung_anomaly")

    def test_clean_hung_without_confirmation_sends_no_signal(self):
        procs = iter(USER_PROCS)
        with patch.object(observer, "get_active_tmux_sessions", return_value=[f"s{i}" for i in range(len(USER_PROCS))]), \
             patch.object(observer, "get_external_ai_sessions", return_value=[]), \
             patch.object(observer, "get_session_cwd", return_value="/tmp"), \
             patch.object(observer, "capture_session_pane", return_value=["Working on refactor"]), \
             patch.object(observer, "get_session_long_running_process", side_effect=lambda s, threshold_seconds=900: dict(next(procs))), \
             patch.object(observer, "find_makewand_dispatched_processes", return_value=[], create=True), \
             patch("os.kill") as kill, patch("os.killpg") as killpg:
            report = observe_all_dialogs(save_report=False, clean_hung=True)
        kill.assert_not_called()
        killpg.assert_not_called()
        self.assertEqual(report["cleaned_pids"], [])
        self.assertFalse(any(s["category"] == "hung_anomaly" for s in report["sessions"]))

    def test_candidates_are_listed_but_not_killed_without_confirm(self):
        cand = {"pid": 43001, "pgid": 43001, "comm": "codex", "args": "codex exec", "etimes": 4000,
                "starttime": 1, "dispatch_id": "1-abc", "dispatcher_pid": 1, "dispatcher_alive": False}
        with patch.object(observer, "get_active_tmux_sessions", return_value=[]), \
             patch.object(observer, "get_external_ai_sessions", return_value=[]), \
             patch.object(observer, "find_makewand_dispatched_processes", return_value=[cand], create=True), \
             patch("os.kill") as kill, patch("os.killpg") as killpg:
            report = observe_all_dialogs(save_report=False, clean_hung=True)
        self.assertEqual(report["hung_candidates"], [cand])
        kill.assert_not_called()
        killpg.assert_not_called()

    def test_report_is_private_and_under_config_dir(self):
        with patch.object(observer, "get_active_tmux_sessions", return_value=[]), \
             patch.object(observer, "get_external_ai_sessions", return_value=[]):
            observe_all_dialogs(save_report=True)
        path = Path(config.CONFIG_DIR) / "dialog_observations.json"
        self.assertTrue(path.exists(), "report must follow MAKEWAND_CONFIG_DIR, not ~/.config")
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)


class TestCandidateFilter(unittest.TestCase):
    """Only marked, terminal-less, non-shell, top-of-tree processes qualify."""

    def test_filter_rules(self):
        stats = {
            101: {"pid": 101, "comm": "codex", "ppid": 1, "pgrp": 101, "tty_nr": 0, "starttime": 10},
            102: {"pid": 102, "comm": "claude", "ppid": 1, "pgrp": 102, "tty_nr": 34816, "starttime": 10},
            103: {"pid": 103, "comm": "claude", "ppid": 1, "pgrp": 103, "tty_nr": 0, "starttime": 10},
            104: {"pid": 104, "comm": "bash", "ppid": 1, "pgrp": 104, "tty_nr": 0, "starttime": 10},
            105: {"pid": 105, "comm": "node", "ppid": 101, "pgrp": 101, "tty_nr": 0, "starttime": 10},
            106: {"pid": 106, "comm": "vim", "ppid": 1, "pgrp": 106, "tty_nr": 0, "starttime": 10},
        }
        marks = {101: "900-aaa", 102: "901-bbb", 104: "902-ccc", 105: "900-aaa", 106: "903-ddd"}
        with patch.object(observer, "_list_pids", return_value=list(stats)), \
             patch.object(observer, "_read_proc_stat", side_effect=lambda pid: stats.get(pid)), \
             patch.object(observer, "get_dispatch_id", side_effect=lambda pid: marks.get(pid)), \
             patch.object(observer, "_process_age_seconds", return_value=4000.0), \
             patch.object(observer, "_read_proc_cmdline", return_value="cmd"), \
             patch.object(observer, "_own_ancestors", return_value=set()), \
             patch.dict(os.environ, {DISPATCH_ENV_VAR: "self-000"}):
            candidates = find_makewand_dispatched_processes(min_age_seconds=1800)
        self.assertEqual([c["pid"] for c in candidates], [101])
        self.assertEqual(candidates[0]["dispatcher_pid"], 900)


class TestRealProcessOwnership(unittest.TestCase):
    """End-to-end with real child processes (no AI CLI involved)."""

    def _spawn(self, marked):
        env = {k: v for k, v in os.environ.items() if k != DISPATCH_ENV_VAR}
        if marked:
            env[DISPATCH_ENV_VAR] = f"999999-{uuid.uuid4().hex[:12]}"
        proc = subprocess.Popen(["sleep", "300"], env=env, start_new_session=True,
                                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(lambda: (proc.kill(), proc.wait()) if proc.poll() is None else None)
        return proc, env.get(DISPATCH_ENV_VAR)

    def test_only_confirmed_marked_process_is_terminated(self):
        marked, mark = self._spawn(True)
        unmarked, _ = self._spawn(False)
        time.sleep(0.2)
        candidates = find_makewand_dispatched_processes(min_age_seconds=0)
        mine = [c for c in candidates if c["pid"] in (marked.pid, unmarked.pid)]
        self.assertEqual([c["pid"] for c in mine], [marked.pid])
        self.assertEqual(mine[0]["dispatch_id"], mark)

        # No confirmation -> nothing happens.
        self.assertEqual(terminate_dispatched_processes(mine, confirm_pids=[]), [])
        self.assertIsNone(marked.poll())
        # Confirming the unmarked PID does nothing either.
        self.assertEqual(terminate_dispatched_processes(mine, confirm_pids=[unmarked.pid]), [])
        self.assertIsNone(unmarked.poll())
        # Explicitly confirmed marked PID -> SIGTERM.
        cleaned = terminate_dispatched_processes(mine, confirm_pids=[marked.pid])
        self.assertEqual([c["pid"] for c in cleaned], [marked.pid])
        self.assertEqual(marked.wait(5), -signal.SIGTERM)
        self.assertIsNone(unmarked.poll())

    def test_cli_marks_itself_as_dispatcher(self):
        saved = os.environ.get(DISPATCH_ENV_VAR)
        try:
            dispatch_id = observer.mark_process_as_dispatcher()
            self.assertEqual(os.environ[DISPATCH_ENV_VAR], dispatch_id)
            self.assertTrue(dispatch_id.startswith(f"{os.getpid()}-"))
            out = subprocess.run([sys.executable, "-c", f"import os; print(os.environ.get('{DISPATCH_ENV_VAR}'))"],
                                 capture_output=True, text=True, timeout=30).stdout.strip()
            self.assertEqual(out, dispatch_id, "children must inherit the dispatch marker")
        finally:
            if saved is None:
                os.environ.pop(DISPATCH_ENV_VAR, None)
            else:
                os.environ[DISPATCH_ENV_VAR] = saved


if __name__ == "__main__":
    unittest.main()
