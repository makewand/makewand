"""Unit tests for makewand/collision.py hardening.

Covers:
- Cross-worktree detection using git-common-dir and registered worktrees
- Zombie and defunct process filtering
- Grandchild and wrapper process tree tracing to tmux pane PID
- False positive prevention for non-AI commands (tmux, git, grep, etc.)
- Worktree collision warnings and topology reporting
"""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import sys
import tempfile
import unittest
from unittest.mock import patch, MagicMock
from pathlib import Path

from makewand.collision import (
    get_git_repo_toplevel,
    get_git_common_dir,
    get_git_worktrees,
    get_process_parent_map,
    find_pane_for_process,
    get_active_ai_processes,
    detect_cross_session_collisions,
    format_collision_warning,
    get_all_active_sessions_report,
    EXCLUDED_NON_AI_COMMANDS,
)


class TestCollisionHardening(unittest.TestCase):
    """Test suite for hardened cross-session collision detector."""

    def test_get_git_common_dir(self):
        """Verify git-common-dir resolution for repos and worktrees."""
        with tempfile.TemporaryDirectory() as td:
            # Not a git repo
            self.assertIsNone(get_git_common_dir(td))

            # Current makewand repo
            common_dir = get_git_common_dir(os.getcwd())
            self.assertIsNotNone(common_dir)
            self.assertTrue(os.path.isdir(common_dir))
            self.assertTrue(common_dir.endswith(".git"))

    def test_filter_zombie_and_defunct_processes(self):
        """Verify zombie (state Z) and defunct processes are excluded."""
        # Simulated ps output containing zombies, defunct, and active processes
        fake_ps = (
            "PID PPID TT ELAPSED S COMMAND COMMAND\n"
            "1001 1000 pts/1 00:10:00 S python3 /home/user/bin/makewand status\n"
            "1002 1000 pts/1 00:10:00 Z codex [codex] <defunct>\n"
            "1003 1000 pts/1 00:10:00 Z+ claude <defunct>\n"
            "1004 1000 pts/1 00:10:00 S node /path/to/bin/codex\n"
        )
        with patch("subprocess.check_output", return_value=fake_ps.encode("utf-8")), \
             patch("os.readlink", return_value="/tmp/test"):
            procs = get_active_ai_processes()
            pids = [p["pid"] for p in procs]

            self.assertIn(1001, pids)
            self.assertIn(1004, pids)
            # Zombies must be filtered out
            self.assertNotIn(1002, pids)
            self.assertNotIn(1003, pids)

            # Verify state is recorded
            proc_1001 = next(p for p in procs if p["pid"] == 1001)
            self.assertEqual(proc_1001["state"], "S")
            self.assertEqual(proc_1001["ai_type"], "makewand")

    def test_false_positive_command_exclusion(self):
        """Verify non-AI commands like tmux attach or git commit are not flagged."""
        fake_ps = (
            "PID PPID TT ELAPSED S COMMAND COMMAND\n"
            "2001 1 pts/2 01:00:00 S tmux: client tmux attach -d -t =makewand\n"
            "2002 1 pts/2 01:00:00 S tmux tmux attach -t codex\n"
            "2003 1 pts/2 01:00:00 S git git commit -m 'update makewand collision'\n"
            "2004 1 pts/2 01:00:00 S grep grep -rn makewand .\n"
            "2005 1 pts/2 01:00:00 S vim vim /mnt/data/makewand/cli.py\n"
            "2006 1 pts/2 01:00:00 S strategy /usr/local/bin/strategy-app\n"
            "2007 1 pts/2 01:00:00 S agy agy --verbose\n"
            "2008 1 pts/2 01:00:00 S python3 python3 -m makewand plan\n"
        )
        with patch("subprocess.check_output", return_value=fake_ps.encode("utf-8")), \
             patch("os.readlink", return_value="/tmp/test"):
            procs = get_active_ai_processes()
            pids = [p["pid"] for p in procs]

            self.assertNotIn(2001, pids, "tmux attach -t =makewand must not be flagged")
            self.assertNotIn(2002, pids, "tmux attach -t codex must not be flagged")
            self.assertNotIn(2003, pids, "git commit must not be flagged")
            self.assertNotIn(2004, pids, "grep must not be flagged")
            self.assertNotIn(2005, pids, "vim must not be flagged")
            self.assertNotIn(2006, pids, "strategy must not match agy")

            self.assertIn(2007, pids, "agy must be flagged")
            self.assertIn(2008, pids, "python3 -m makewand must be flagged")

    def test_grandchild_process_tree_matching(self):
        """Verify grandchild and wrapper process matching: bash -> node -> codex."""
        # Top-level tmux pane shell PID = 500
        pane_map_by_pid = {
            500: {"pane_id": "main:0.0", "pane_pid": 500, "tty": "pts/5", "path": "/workspace"}
        }
        pane_map_by_tty = {
            "pts/5": pane_map_by_pid[500]
        }

        # Case 1: Direct child of bash (PPID = 500)
        proc_direct = {"pid": 600, "ppid": 500, "tty": "pts/5"}
        pane = find_pane_for_process(proc_direct, pane_map_by_pid, pane_map_by_tty)
        self.assertIsNotNone(pane)
        self.assertEqual(pane["pane_id"], "main:0.0")

        # Case 2: Grandchild (bash 500 -> node 550 -> codex 650)
        parent_map = {
            650: 550,
            550: 500,
            500: 1,
        }
        proc_grandchild = {"pid": 650, "ppid": 550, "tty": "pts/5"}
        pane = find_pane_for_process(proc_grandchild, pane_map_by_pid, pane_map_by_tty, parent_map)
        self.assertIsNotNone(pane)
        self.assertEqual(pane["pane_id"], "main:0.0")

        # Case 3: Deep wrapper chain (bash 500 -> sh 510 -> node 520 -> bin 530)
        deep_parent_map = {
            530: 520,
            520: 510,
            510: 500,
            500: 1,
        }
        proc_deep = {"pid": 530, "ppid": 520, "tty": "?"}
        pane = find_pane_for_process(proc_deep, pane_map_by_pid, pane_map_by_tty, deep_parent_map)
        self.assertIsNotNone(pane)
        self.assertEqual(pane["pane_id"], "main:0.0")

    def test_cross_worktree_collision_detection(self):
        """Verify cross-worktree sessions are recognized as same_repo_concurrent."""
        target_wt = "/mnt/data/repo-main"
        other_wt = "/mnt/data/repo-wt-feature"
        unrelated_dir = "/mnt/data/unrelated-repo"

        fake_worktrees = [
            {"path": target_wt, "head": "abc", "branch": "master"},
            {"path": other_wt, "head": "def", "branch": "feat/xyz"},
        ]

        fake_procs = [
            # Process in same worktree
            {
                "pid": 3001,
                "ppid": 1,
                "ai_type": "claude",
                "comm": "claude",
                "args": "claude",
                "tty": "pts/1",
                "state": "S",
                "cwd": target_wt,
            },
            # Process in another worktree of same repo
            {
                "pid": 3002,
                "ppid": 1,
                "ai_type": "codex",
                "comm": "codex",
                "args": "codex",
                "tty": "pts/2",
                "state": "S",
                "cwd": other_wt,
            },
            # Process in unrelated directory
            {
                "pid": 3003,
                "ppid": 1,
                "ai_type": "agy",
                "comm": "agy",
                "args": "agy",
                "tty": "pts/3",
                "state": "S",
                "cwd": unrelated_dir,
            }
        ]

        with patch("os.path.realpath", side_effect=lambda p: p), \
             patch("makewand.collision.get_git_repo_toplevel", return_value=target_wt), \
             patch("makewand.collision.get_git_common_dir", return_value="/mnt/data/repo-main/.git"), \
             patch("makewand.collision.get_git_worktrees", return_value=fake_worktrees), \
             patch("makewand.collision.get_all_active_tmux_panes", return_value=[]), \
             patch("makewand.collision.get_active_ai_processes", return_value=fake_procs), \
             patch("makewand.collision.get_process_parent_map", return_value={}):

            rep = detect_cross_session_collisions(target_wt)

            self.assertTrue(rep["has_collision"])
            self.assertEqual(rep["git_common_dir"], "/mnt/data/repo-main/.git")
            self.assertEqual(len(rep["same_worktree_sessions"]), 1)
            self.assertEqual(rep["same_worktree_sessions"][0]["pid"], 3001)

            # Cross-worktree session properly recognized
            self.assertEqual(len(rep["same_repo_sessions"]), 1)
            self.assertEqual(rep["same_repo_sessions"][0]["pid"], 3002)
            self.assertEqual(rep["same_repo_sessions"][0]["collision_type"], "same_repo_concurrent")
            self.assertEqual(rep["same_repo_sessions"][0]["risk"], "MEDIUM")

            # Warning text should include both same worktree and related worktree sessions
            warning = format_collision_warning(rep)
            self.assertIn("3001", warning)
            self.assertIn("3002", warning)
            self.assertIn("关联 Worktree 会话", warning)

    def test_get_all_active_sessions_report_with_worktrees(self):
        """Verify host-wide session reporting groups worktrees correctly."""
        main_wt = "/mnt/data/repo-main"
        other_wt = "/mnt/data/repo-wt-feature"
        common_git = "/mnt/data/repo-main/.git"

        fake_procs = [
            {"pid": 4001, "cwd": main_wt, "ai_type": "claude", "comm": "claude"},
            {"pid": 4002, "cwd": other_wt, "ai_type": "codex", "comm": "codex"},
        ]

        def fake_common(p):
            if p in (main_wt, other_wt):
                return common_git
            return None

        def fake_worktrees(p):
            if p in (main_wt, other_wt):
                return [{"path": main_wt}, {"path": other_wt}]
            return []

        with patch("makewand.collision.get_active_ai_processes", return_value=fake_procs), \
             patch("makewand.collision.get_all_active_tmux_panes", return_value=[]), \
             patch("makewand.collision.get_git_common_dir", side_effect=fake_common), \
             patch("makewand.collision.get_git_worktrees", side_effect=fake_worktrees), \
             patch("os.path.exists", return_value=True):

            report = get_all_active_sessions_report()
            self.assertEqual(report["total_active_sessions"], 2)
            # Both worktrees group under the primary repository path
            self.assertIn(main_wt, report["sessions_by_repo"])
            self.assertEqual(len(report["sessions_by_repo"][main_wt]), 2)

    def test_filter_ai_daemon_processes(self):
        """Verify background daemons (app-server, pid-updater, lsp) are filtered out."""
        fake_ps = (
            "PID PPID TT ELAPSED S COMMAND COMMAND\n"
            "5001 1 ? 06:00:00 S codex /home/user/.codex/packages/app-server-daemon/releases/0.160.0/bin/codex app-server daemon pid-update-loop\n"
            "5002 5001 ? 06:00:00 S codex /home/user/.codex/packages/app-server-daemon/releases/0.160.0/bin/codex app-server --listen unix:// --managed-daemon\n"
            "5003 5002 ? 06:00:00 S codex-code-mode /home/user/.codex/packages/app-server-daemon/releases/0.160.0/bin/codex-code-mode-host\n"
            "5004 1 pts/1 01:00:00 S codex /home/user/.local/bin/codex\n"
        )
        with patch("subprocess.check_output", return_value=fake_ps.encode("utf-8")), \
             patch("os.readlink", return_value="/tmp/test"):
            procs = get_active_ai_processes()
            pids = [p["pid"] for p in procs]

            self.assertNotIn(5001, pids, "pid-update-loop daemon must be excluded")
            self.assertNotIn(5002, pids, "managed-daemon app-server must be excluded")
            self.assertNotIn(5003, pids, "codex-code-mode-host must be excluded")
            self.assertIn(5004, pids, "interactive codex process must be retained")

    def test_deduplicate_wrapper_processes(self):
        """Verify launcher wrapper (node) is pruned when child engine (codex) is present."""
        fake_ps = (
            "PID PPID TT ELAPSED S COMMAND COMMAND\n"
            "6001 1 pts/9 02:00:00 S node node /home/user/.nvm/versions/node/v20/bin/codex\n"
            "6002 6001 pts/9 02:00:00 S codex /home/user/.codex/vendor/bin/codex\n"
        )
        with patch("subprocess.check_output", return_value=fake_ps.encode("utf-8")), \
             patch("os.readlink", return_value="/tmp/test"):
            procs = get_active_ai_processes()
            pids = [p["pid"] for p in procs]

            # 6001 is parent wrapper of 6002; only child 6002 should remain
            self.assertNotIn(6001, pids)
            self.assertIn(6002, pids)

    def test_worktree_index_lock_isolation(self):
        """Verify a linked worktree is NOT blocked by index.lock in main repo."""
        with tempfile.TemporaryDirectory() as td:
            main_repo = Path(td) / "main"
            main_repo.mkdir()
            (main_repo / ".git").mkdir()
            main_lock = main_repo / ".git" / "index.lock"
            main_lock.touch()

            wt_repo = Path(td) / "wt"
            wt_repo.mkdir()
            wt_git_dir = main_repo / ".git" / "worktrees" / "wt"
            wt_git_dir.mkdir(parents=True)
            (wt_repo / ".git").write_text(f"gitdir: {wt_git_dir}\n")

            with patch("makewand.collision.get_git_repo_toplevel", return_value=str(wt_repo)), \
                 patch("makewand.collision.get_git_common_dir", return_value=str(main_repo / ".git")), \
                 patch("makewand.collision.get_git_worktrees", return_value=[{"path": str(main_repo)}, {"path": str(wt_repo)}]), \
                 patch("makewand.collision.get_active_ai_processes", return_value=[]), \
                 patch("makewand.collision.get_all_active_tmux_panes", return_value=[]), \
                 patch("makewand.collision.get_process_parent_map", return_value={}):

                # In linked worktree, main repo index.lock must not cause git_locked
                rep = detect_cross_session_collisions(str(wt_repo))
                self.assertFalse(rep["git_locked"])
                self.assertFalse(rep["has_collision"])

                # If the linked worktree itself has index.lock, it must be detected
                wt_lock = wt_git_dir / "index.lock"
                wt_lock.touch()
                rep_locked = detect_cross_session_collisions(str(wt_repo))
                self.assertTrue(rep_locked["git_locked"])
                self.assertTrue(rep_locked["has_collision"])

    def test_format_collision_warning_parallel_only(self):
        """Verify warning message formatting when only parallel worktrees exist."""
        report = {
            "has_collision": False,
            "git_locked": False,
            "same_worktree_sessions": [],
            "same_repo_sessions": [
                {
                    "pid": 7001,
                    "ai_type": "claude",
                    "location": "main:0.0",
                    "cwd": "/mnt/data/repo-other-wt",
                }
            ],
            "suggested_worktree_cmd": "git worktree add ...",
        }
        warning = format_collision_warning(report)
        self.assertIn("同仓库其他独立 Worktree 正在并发运行", warning)
        self.assertIn("7001", warning)
        self.assertNotIn("检测到多 Session 正在共享操作相同工作区", warning)

    def test_detect_collisions_with_file_path_target(self):
        """Verify detect_cross_session_collisions handles a file path target gracefully."""
        with tempfile.NamedTemporaryFile() as tf:
            # Passing a file path should canonicalize to its directory without error
            rep = detect_cross_session_collisions(tf.name)
            self.assertIn("has_collision", rep)
            self.assertIn("collisions", rep)

