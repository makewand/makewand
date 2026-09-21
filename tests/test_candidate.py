"""
Unit tests for Makewand Candidate Lifecycle Management (inspect / apply / discard).
"""

import os
import shutil
import tempfile
import unittest
from pathlib import Path

from makewand.candidate import CandidateManager
from makewand.git_helper import ensure_git_worktree, run_git_cmd
import makewand.config as config

class TestCandidateLifecycle(unittest.TestCase):
    def setUp(self):
        self.test_dir = tempfile.mkdtemp(prefix="test_makewand_cand_")
        self.orig_config_dir = config.CONFIG_DIR
        self.orig_cand_dir = config.CANDIDATES_DIR
        self.orig_backups_dir = config.BACKUPS_DIR

        # Point to temp config
        config.CONFIG_DIR = Path(self.test_dir) / "config"
        config.CANDIDATES_DIR = config.CONFIG_DIR / "candidates"
        config.BACKUPS_DIR = config.CONFIG_DIR / "backups"
        config.ensure_config_dir()

        # Create a mock base workspace
        self.base_ws = Path(self.test_dir) / "workspace"
        self.base_ws.mkdir(parents=True, exist_ok=True)
        ensure_git_worktree(str(self.base_ws))
        (self.base_ws / "main.py").write_text("print('version 1')\n", encoding="utf-8")
        run_git_cmd("git add -A && git commit -m 'initial main.py'", cwd=str(self.base_ws))

    def tearDown(self):
        config.CONFIG_DIR = self.orig_config_dir
        config.CANDIDATES_DIR = self.orig_cand_dir
        config.BACKUPS_DIR = self.orig_backups_dir
        shutil.rmtree(self.test_dir, ignore_errors=True)

    def test_save_and_list_races(self):
        race_id = "rc_test123"
        wt_a = config.CANDIDATES_DIR / race_id / "agent_a"
        wt_b = config.CANDIDATES_DIR / race_id / "agent_b"
        wt_a.mkdir(parents=True, exist_ok=True)
        wt_b.mkdir(parents=True, exist_ok=True)

        CandidateManager.save_race(
            race_id=race_id,
            prompt="优化主程序",
            base_cwd=str(self.base_ws),
            baseline_commit="abc1234",
            agent_a={"model": "Codex", "path": str(wt_a), "duration": 10.2, "success": True, "diff": "+print(2)"},
            agent_b={"model": "Claude", "path": str(wt_b), "duration": 8.1, "success": True, "diff": "+print(3)"},
            judge_report="推荐采纳选手 B",
            winner="B"
        )

        races = CandidateManager.list_races()
        self.assertEqual(len(races), 1)
        self.assertEqual(races[0]["race_id"], race_id)
        self.assertEqual(races[0]["winner"], "B")

        fetched = CandidateManager.get_race(race_id)
        self.assertIsNotNone(fetched)
        self.assertEqual(fetched["prompt"], "优化主程序")

    def test_apply_dry_run_and_success(self):
        race_id = "rc_test456"
        wt_b = config.CANDIDATES_DIR / race_id / "agent_b"
        wt_b.mkdir(parents=True, exist_ok=True)

        # Candidate modifies main.py and creates helper.py
        ensure_git_worktree(str(wt_b))
        (wt_b / "main.py").write_text("print('version 2 from candidate B')\n", encoding="utf-8")
        (wt_b / "helper.py").write_text("def help(): pass\n", encoding="utf-8")

        CandidateManager.save_race(
            race_id=race_id,
            prompt="升级代码",
            base_cwd=str(self.base_ws),
            baseline_commit="def5678",
            agent_a={"model": "Codex", "path": "", "duration": 10.0, "success": False, "diff": ""},
            agent_b={"model": "Claude", "path": str(wt_b), "duration": 8.0, "success": True, "diff": "..."},
            judge_report="推荐选手 B",
            winner="B"
        )

        # 1. Dry run: should NOT modify files in base_ws
        ok, preview, msg = CandidateManager.apply_candidate(race_id, candidate_label="B", dry_run=True)
        self.assertTrue(ok)
        self.assertIn("Dry-run", msg)
        self.assertEqual((self.base_ws / "main.py").read_text(encoding="utf-8"), "print('version 1')\n")
        self.assertFalse((self.base_ws / "helper.py").exists())

        # 2. Real Apply: should update main.py and create helper.py
        ok, applied, msg = CandidateManager.apply_candidate(race_id, candidate_label="B", dry_run=False)
        self.assertTrue(ok)
        self.assertIn("成功应用", msg)
        self.assertEqual((self.base_ws / "main.py").read_text(encoding="utf-8"), "print('version 2 from candidate B')\n")
        self.assertTrue((self.base_ws / "helper.py").exists())
        self.assertEqual((self.base_ws / "helper.py").read_text(encoding="utf-8"), "def help(): pass\n")

    def test_apply_conflict_detection(self):
        race_id = "rc_test789"
        wt_b = config.CANDIDATES_DIR / race_id / "agent_b"
        wt_b.mkdir(parents=True, exist_ok=True)
        ensure_git_worktree(str(wt_b))
        (wt_b / "main.py").write_text("candidate change\n", encoding="utf-8")

        CandidateManager.save_race(
            race_id=race_id,
            prompt="升级代码",
            base_cwd=str(self.base_ws),
            baseline_commit="",
            agent_a={},
            agent_b={"model": "Claude", "path": str(wt_b)},
            winner="B"
        )

        # Simulate user locally modifying main.py in base_ws during race
        (self.base_ws / "main.py").write_text("user local conflict edit\n", encoding="utf-8")

        # Without force: must detect conflict and reject
        ok, conflicts, msg = CandidateManager.apply_candidate(race_id, candidate_label="B", force=False)
        self.assertFalse(ok)
        self.assertIn("冲突", msg)
        self.assertIn("main.py", conflicts)
        self.assertEqual((self.base_ws / "main.py").read_text(encoding="utf-8"), "user local conflict edit\n")

        # With force: overwrites
        ok, applied, msg = CandidateManager.apply_candidate(race_id, candidate_label="B", force=True)
        self.assertTrue(ok)
        self.assertEqual((self.base_ws / "main.py").read_text(encoding="utf-8"), "candidate change\n")

    def test_apply_symlink_defense(self):
        race_id = "rc_symlink_test"
        wt_b = config.CANDIDATES_DIR / race_id / "agent_b"
        wt_b.mkdir(parents=True, exist_ok=True)
        ensure_git_worktree(str(wt_b))

        # 1. Candidate file itself is a symlink
        evil_link = wt_b / "evil.py"
        evil_link.symlink_to("/etc/hosts")

        CandidateManager.save_race(
            race_id=race_id,
            prompt="安全测试",
            base_cwd=str(self.base_ws),
            baseline_commit="",
            agent_a={},
            agent_b={"model": "Claude", "path": str(wt_b), "success": True},
            winner="B"
        )

        ok, _, msg = CandidateManager.apply_candidate(race_id, candidate_label="B")
        self.assertFalse(ok)
        self.assertIn("符号链接", msg)

        # Remove symlink and create normal file
        evil_link.unlink()
        (wt_b / "normal.py").write_text("safe content\n", encoding="utf-8")

        # 2. Target file in base_ws is a symlink pointing outside
        target_link = self.base_ws / "normal.py"
        target_link.symlink_to("/tmp")

        # Re-save race to update manifest
        CandidateManager.save_race(
            race_id=race_id,
            prompt="安全测试",
            base_cwd=str(self.base_ws),
            baseline_commit="",
            agent_a={},
            agent_b={"model": "Claude", "path": str(wt_b), "success": True},
            winner="B"
        )

        ok, _, msg = CandidateManager.apply_candidate(race_id, candidate_label="B")
        self.assertFalse(ok)
        self.assertIn("符号链接", msg)

    def test_apply_unreviewed_mutation_rejected(self):
        race_id = "rc_mutation_test"
        wt_b = config.CANDIDATES_DIR / race_id / "agent_b"
        wt_b.mkdir(parents=True, exist_ok=True)
        ensure_git_worktree(str(wt_b))
        (wt_b / "feature.py").write_text("def foo(): return 1\n", encoding="utf-8")

        CandidateManager.save_race(
            race_id=race_id,
            prompt="防篡改测试",
            base_cwd=str(self.base_ws),
            baseline_commit="",
            agent_a={},
            agent_b={"model": "Claude", "path": str(wt_b), "success": True},
            winner="B"
        )

        # Mutate the file in wt_b after race has been saved
        (wt_b / "feature.py").write_text("def foo(): return 'malicious'\n", encoding="utf-8")

        ok, _, msg = CandidateManager.apply_candidate(race_id, candidate_label="B")
        self.assertFalse(ok)
        self.assertIn("哈希校验不匹配", msg)

    def test_apply_failed_candidate_protection(self):
        race_id = "rc_failed_cand_test"
        wt_b = config.CANDIDATES_DIR / race_id / "agent_b"
        wt_b.mkdir(parents=True, exist_ok=True)
        ensure_git_worktree(str(wt_b))
        (wt_b / "broken.py").write_text("broken code\n", encoding="utf-8")

        CandidateManager.save_race(
            race_id=race_id,
            prompt="失败候选测试",
            base_cwd=str(self.base_ws),
            baseline_commit="",
            agent_a={},
            agent_b={"model": "Claude", "path": str(wt_b), "success": False},
            winner=None
        )

        # No winner declared, label omitted: must reject
        ok, _, msg = CandidateManager.apply_candidate(race_id, candidate_label=None)
        self.assertFalse(ok)
        self.assertIn("未决出胜者", msg)

        # Explicit label but candidate failed: must reject without --force
        ok, _, msg = CandidateManager.apply_candidate(race_id, candidate_label="B", force=False)
        self.assertFalse(ok)
        self.assertIn("失败/未完成", msg)

        # With --force: allowed
        ok, _, msg = CandidateManager.apply_candidate(race_id, candidate_label="B", force=True)
        self.assertTrue(ok)
        self.assertTrue((self.base_ws / "broken.py").exists())

    def test_discard_race(self):
        race_id = "rc_discard1"
        (config.CANDIDATES_DIR / race_id).mkdir(parents=True, exist_ok=True)
        CandidateManager.save_race(
            race_id=race_id,
            prompt="临时测试",
            base_cwd=str(self.base_ws),
            baseline_commit="",
            agent_a={},
            agent_b={},
        )
        self.assertTrue((config.CANDIDATES_DIR / race_id).exists())

        ok, msg = CandidateManager.discard_race(race_id)
        self.assertTrue(ok)
        self.assertFalse((config.CANDIDATES_DIR / race_id).exists())

if __name__ == "__main__":
    unittest.main()

