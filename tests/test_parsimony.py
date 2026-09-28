"""
Unit tests for Patch Parsimony & Structural Impact Scoring (Agentless-inspired).
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import tempfile
import unittest
from pathlib import Path

from makewand.orchestrator import compute_patch_parsimony
from makewand.candidate import CandidateManager
import makewand.config as config


class TestPatchParsimony(unittest.TestCase):
    def test_compute_patch_parsimony_empty_diff(self):
        res = compute_patch_parsimony("")
        self.assertEqual(res["files_touched"], 0)
        self.assertEqual(res["lines_added"], 0)
        self.assertEqual(res["lines_deleted"], 0)
        self.assertEqual(res["total_churn"], 0)
        self.assertEqual(res["parsimony_ratio"], 1.0)
        self.assertIn("0 files", res["summary"])

    def test_compute_patch_parsimony_surgical_bugfix(self):
        surgical_diff = """diff --git a/core/calc.py b/core/calc.py
--- a/core/calc.py
+++ b/core/calc.py
@@ -10,3 +10,3 @@ def add(a, b):
-    return a - b
+    return a + b
"""
        res = compute_patch_parsimony(surgical_diff)
        self.assertEqual(res["files_touched"], 1)
        self.assertEqual(res["lines_added"], 1)
        self.assertEqual(res["lines_deleted"], 1)
        self.assertEqual(res["total_churn"], 2)
        # Surgical fix should maintain high parsimony score (~0.96)
        self.assertGreaterEqual(res["parsimony_ratio"], 0.90)
        self.assertIn("1 files, +1/-1 lines", res["summary"])

    def test_compute_patch_parsimony_bloated_refactor(self):
        # Sprawling refactor across 5 files with 120 lines changed
        diff_chunks = []
        for i in range(5):
            diff_chunks.append(f"""diff --git a/pkg/mod{i}.py b/pkg/mod{i}.py
--- a/pkg/mod{i}.py
+++ b/pkg/mod{i}.py
@@ -1,10 +1,15 @@
""" + "\n".join(f"-old_line_{j}" for j in range(10)) + "\n" + "\n".join(f"+new_line_{j}" for j in range(14)))

        bloated_diff = "\n".join(diff_chunks)
        res = compute_patch_parsimony(bloated_diff)
        self.assertEqual(res["files_touched"], 5)
        self.assertEqual(res["lines_added"], 70)
        self.assertEqual(res["lines_deleted"], 50)
        self.assertEqual(res["total_churn"], 120)
        # Bloated multi-file change has significantly lower parsimony
        self.assertLess(res["parsimony_ratio"], 0.35)

    def test_parsimony_preference_ordering(self):
        # Surgical 1-line fix vs medium 20-line refactor
        diff_tight = """diff --git a/fix.py b/fix.py
--- a/fix.py
+++ b/fix.py
@@ -1,1 +1,1 @@
-return None
+return True
"""
        diff_bloated = """diff --git a/a.py b/a.py
--- a/a.py
+++ b/a.py
@@ -1,10 +1,10 @@
""" + "\n".join(f"+line_{i}" for i in range(25)) + """
diff --git a/b.py b/b.py
--- a/b.py
+++ b/b.py
@@ -1,10 +1,10 @@
""" + "\n".join(f"-line_{i}" for i in range(15))

        score_tight = compute_patch_parsimony(diff_tight)["parsimony_ratio"]
        score_bloated = compute_patch_parsimony(diff_bloated)["parsimony_ratio"]
        self.assertGreater(score_tight, score_bloated)

    def test_parsimony_stored_in_candidate_manager(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            test_dir = Path(tmpdir)
            orig_config = config.CONFIG_DIR
            orig_cands = config.CANDIDATES_DIR
            try:
                config.CONFIG_DIR = test_dir / "config"
                config.CANDIDATES_DIR = config.CONFIG_DIR / "candidates"
                config.ensure_config_dir()

                race_id = "rc_parsimony_test"
                wt_a = config.CANDIDATES_DIR / race_id / "agent_a"
                wt_b = config.CANDIDATES_DIR / race_id / "agent_b"
                wt_a.mkdir(parents=True, exist_ok=True)
                wt_b.mkdir(parents=True, exist_ok=True)

                pars_a = compute_patch_parsimony("diff --git a/a.py b/a.py\n+x=1\n")
                pars_b = compute_patch_parsimony("diff --git a/b.py b/b.py\n+y=2\n+z=3\n")

                CandidateManager.save_race(
                    race_id=race_id,
                    prompt="Fix bug",
                    base_cwd=str(test_dir),
                    baseline_commit="",
                    agent_a={
                        "model": "Codex",
                        "path": str(wt_a),
                        "duration": 5.0,
                        "success": True,
                        "diff": "+x=1",
                        "parsimony": pars_a,
                    },
                    agent_b={
                        "model": "Claude",
                        "path": str(wt_b),
                        "duration": 6.0,
                        "success": True,
                        "diff": "+y=2\n+z=3",
                        "parsimony": pars_b,
                    },
                    judge_report="推荐选手 A（高精简度补丁）",
                    winner="A",
                )

                saved = CandidateManager.get_race(race_id)
                self.assertIsNotNone(saved)
                cand_a_meta = saved["candidates"]["A"]
                cand_b_meta = saved["candidates"]["B"]
                self.assertIn("parsimony", cand_a_meta)
                self.assertIn("parsimony", cand_b_meta)
                self.assertEqual(cand_a_meta["parsimony"]["lines_added"], 1)
                self.assertEqual(cand_b_meta["parsimony"]["lines_added"], 2)
            finally:
                config.CONFIG_DIR = orig_config
                config.CANDIDATES_DIR = orig_cands


if __name__ == "__main__":
    unittest.main()
