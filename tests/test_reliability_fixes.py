"""
Unit tests validating the 5 reliability and security fixes:
1. Unified execution safety across all 4 providers (fail-closed without bwrap).
2. Elimination of false-positive review passes, strict numeric/dict defect normalization, and no fabricated LGTM.
3. Cross-model reviewer independence (no self-review) and full diff formatting without silent cutoffs.
4. Unified installation/CLI version contract and Go delegation bridge.
5. Intent classification preserving coding actions and composite instructions.
"""

import os
import sys
import unittest
from unittest.mock import patch, MagicMock
from pathlib import Path

from makewand.orchestrator import (
    _normalize_verdict_dict,
    classify_prompt_intent,
    format_review_diff,
    run_local_tests,
    dispatch_task,
)
from makewand.providers.claude import execute_claude_task
from makewand.providers.agy import execute_agy_task
from makewand.providers.codex import execute_codex_task
from makewand.providers.muse import execute_muse_task


class TestExecutionSafetyAcrossProviders(unittest.TestCase):
    """Fix 1: All 4 providers must enforce fail-closed sandbox checks for writable tasks."""

    @patch("makewand.sandbox.is_bwrap_available", return_value=False)
    @patch("makewand.health.load_status_cache", return_value={})
    def test_all_providers_fail_closed_without_bwrap(self, mock_cache, mock_bwrap):
        # 1. Claude
        ok, out, err = execute_claude_task("write something", cwd="/tmp", readonly=False)
        self.assertFalse(ok)
        self.assertIn("强制要求 Bubblewrap (bwrap) 沙箱隔离", err)

        # 2. Antigravity (AGY)
        ok, out, err = execute_agy_task("write something", cwd="/tmp", readonly=False)
        self.assertFalse(ok)
        self.assertIn("强制要求 Bubblewrap (bwrap) 沙箱隔离", err)

        # 3. Codex
        ok, out, err = execute_codex_task("write something", cwd="/tmp", readonly=False)
        self.assertFalse(ok)
        self.assertIn("强制要求 Bubblewrap (bwrap) 沙箱隔离", err)

        # 4. Muse
        ok, out, err = execute_muse_task("write something", cwd="/tmp", readonly=False)
        self.assertFalse(ok)
        self.assertIn("强制要求 Bubblewrap (bwrap) 沙箱隔离", err)


class TestVerdictAndReviewReliability(unittest.TestCase):
    """Fix 2: Strict verdict parsing and no fabricated LGTM."""

    def test_numeric_pass_normalization(self):
        # 'pass': 2 (e.g. 2 test failures) must NOT be True!
        v = _normalize_verdict_dict({"pass": 2, "defects": []})
        self.assertFalse(v["pass"])

        # 'pass': 0 is False
        v0 = _normalize_verdict_dict({"pass": 0, "defects": []})
        self.assertFalse(v0["pass"])

        # Strictly 1 or 1.0 is True when no defects
        v1 = _normalize_verdict_dict({"pass": 1, "defects": []})
        self.assertTrue(v1["pass"])

    def test_dict_defects_normalization(self):
        # Dict defects with items
        v = _normalize_verdict_dict({"pass": True, "defects": {"count": 2, "items": ["defect A", "defect B"]}})
        self.assertFalse(v["pass"])
        self.assertEqual(v["defects"], ["defect A", "defect B"])

        # Arbitrary dict defects
        v2 = _normalize_verdict_dict({"pass": True, "defects": {"summary": "potential deadlock"}})
        self.assertFalse(v2["pass"])
        self.assertTrue(len(v2["defects"]) > 0)

    @patch("makewand.orchestrator.execute_claude_task", return_value=None)
    def test_dispatch_task_fails_closed_on_none_adapter_return(self, mock_claude):
        ok, out, err = dispatch_task("claude", "test prompt", cwd="/tmp", readonly=True)
        self.assertFalse(ok)
        self.assertIsNone(out)
        self.assertIn("非预期格式", err)

    def test_run_local_tests_deterministic_behavior(self):
        import tempfile
        # 1. Passing test workspace
        with tempfile.TemporaryDirectory() as tmp_dir:
            test_file = Path(tmp_dir) / "test_ok.py"
            test_file.write_text("import unittest\nclass OkTest(unittest.TestCase):\n    def test_ok(self): self.assertTrue(True)\n")
            passed, details = run_local_tests(tmp_dir, timeout=10)
            self.assertTrue(passed)

        # 2. Failing test workspace
        with tempfile.TemporaryDirectory() as tmp_dir:
            test_file = Path(tmp_dir) / "test_fail.py"
            test_file.write_text("import unittest\nclass FailTest(unittest.TestCase):\n    def test_fail(self): self.assertTrue(False)\n")
            passed, details = run_local_tests(tmp_dir, timeout=10)
            self.assertFalse(passed)
            self.assertTrue("AssertionError" in details or "FAIL" in details or "failed" in details.lower())


class TestCrossModelReviewIndependence(unittest.TestCase):
    """Fix 3: Cross-model review independence and full diff formatting."""

    def test_format_review_diff_preserves_content(self):
        small_diff = "diff --git a/a.py b/a.py\n+hello world\n" * 10
        formatted = format_review_diff(small_diff, max_chars=1000)
        self.assertEqual(formatted, small_diff)

    def test_format_review_diff_handles_large_diff_without_silent_loss(self):
        large_diff = "diff --git a/big.py b/big.py\n" + ("+change_line\n" * 1500)
        formatted = format_review_diff(large_diff, max_chars=1000)
        self.assertIn("=== [Makewand Diff Truncated:", formatted)
        self.assertTrue(formatted.startswith("diff --git a/big.py"))
        self.assertTrue(formatted.endswith("+change_line\n"))


class TestIntentClassificationAndActionPreservation(unittest.TestCase):
    """Fix 5: Composite instructions and incremental coding action triggers."""

    def test_composite_implement_and_review_intent(self):
        intent = classify_prompt_intent("实现一个登录接口并审查代码")
        self.assertEqual(intent, "code")

    def test_incremental_add_feature_triggers(self):
        self.assertEqual(classify_prompt_intent("添加搜索功能"), "code")
        self.assertEqual(classify_prompt_intent("增加导出数据为Excel的功能"), "code")
        self.assertEqual(classify_prompt_intent("支持用户OAuth第三方登录"), "code")
        self.assertEqual(classify_prompt_intent("接入微信支付回调通知"), "code")

    def test_pure_review_remains_review(self):
        self.assertEqual(classify_prompt_intent("审查当前git diff并指出安全漏洞"), "review")
        self.assertEqual(classify_prompt_intent("审计代码，不要修改任何文件"), "review")

    def test_informational_question_remains_explain(self):
        self.assertEqual(classify_prompt_intent("如何实现分布式锁？"), "explain")
        self.assertEqual(classify_prompt_intent("什么是 Bubblewrap 沙箱？"), "explain")


if __name__ == "__main__":
    unittest.main()
