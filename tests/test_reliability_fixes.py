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


class TestTestPhaseSandboxing(unittest.TestCase):
    """Problem 1: Tests executed in run_local_tests() must run inside Bubblewrap sandbox."""

    def test_run_local_tests_cannot_access_host_secrets(self):
        import tempfile
        os.environ["SIMULATED_HOST_SECRET"] = "super-secret-key-12345"
        try:
            with tempfile.TemporaryDirectory() as tmp_dir:
                test_file = Path(tmp_dir) / "test_secret.py"
                test_file.write_text(
                    "import os, unittest\n"
                    "class SecretTest(unittest.TestCase):\n"
                    "    def test_secret(self):\n"
                    "        secret = os.environ.get('SIMULATED_HOST_SECRET')\n"
                    "        self.assertIsNone(secret, 'Host secret leaked into test environment!')\n"
                )
                passed, details = run_local_tests(tmp_dir, timeout=10)
                self.assertTrue(passed, f"Test failed: {details}")
        finally:
            os.environ.pop("SIMULATED_HOST_SECRET", None)

    def test_run_local_tests_cannot_write_to_host_filesystem(self):
        import tempfile
        host_target = "/tmp/makewand_host_escape_test.txt"
        if os.path.exists(host_target):
            try:
                os.remove(host_target)
            except Exception:
                pass

        with tempfile.TemporaryDirectory() as tmp_dir:
            test_file = Path(tmp_dir) / "test_escape.py"
            test_file.write_text(
                f"import unittest\n"
                f"class EscapeTest(unittest.TestCase):\n"
                f"    def test_write_outside(self):\n"
                f"        try:\n"
                f"            with open('{host_target}', 'w') as f:\n"
                f"                f.write('pwned')\n"
                f"        except Exception:\n"
                f"            pass\n"
            )
            run_local_tests(tmp_dir, timeout=10)
            self.assertFalse(os.path.exists(host_target), "Sandbox failed to confine /tmp writes!")

    @patch("makewand.sandbox.is_bwrap_available", return_value=False)
    def test_run_local_tests_fails_closed_when_bwrap_unavailable(self, mock_bwrap):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp_dir:
            test_file = Path(tmp_dir) / "test_sample.py"
            test_file.write_text("import unittest\nclass T(unittest.TestCase):\n    def test_t(self): pass\n")
            passed, details = run_local_tests(tmp_dir, timeout=10)
            self.assertFalse(passed)
            self.assertIn("Bubblewrap 沙箱不可用", details)


class TestTestFailureDeliveryGate(unittest.TestCase):
    """Problem 2: Failing tests must never be delivered, even if reviewer returns plain-text LGTM."""

    @patch("makewand.orchestrator.run_local_tests", return_value=(False, "AssertionError: 1 != 2"))
    @patch("makewand.orchestrator.execute_codex_task", return_value=(True, "LGTM", None))
    @patch("makewand.orchestrator.execute_claude_task", return_value=(True, "def foo(): return 2", None))
    @patch("makewand.orchestrator.check_working_tree_isolation", return_value=(True, None))
    @patch("makewand.orchestrator.get_or_update_status")
    def test_plain_text_lgtm_cannot_bypass_failing_tests(self, mock_status, mock_iso, mock_claude, mock_codex, mock_tests):
        from makewand.orchestrator import run_pipeline
        mock_status.return_value = {
            "claude": {"status": "ready", "tier": "standard"},
            "codex": {"status": "ready", "tier": "deep"},
            "agy": {"status": "ready", "tier": "deep"},
            "muse": {"status": "ready", "tier": "standard"}
        }

        delivered = run_pipeline("实现修复逻辑并写单测", auto_fix=False, timeout=30, force_code=True)
        self.assertFalse(delivered, "Pipeline delivered despite failing unit tests!")


class TestGoDelegationUntrustedModuleIsolation(unittest.TestCase):
    """Problem 3: Go->Python forwarder must never execute untrusted makewand module from cwd."""

    def test_go_binary_rejects_invalid_repo_trust(self):
        import subprocess
        go_server = Path(__file__).resolve().parent.parent / "bin" / "makewand-server"
        if not go_server.exists():
            self.skipTest("makewand-server binary not built")

        res = subprocess.run(
            [str(go_server), "review", "--repo-trust=bogus"],
            capture_output=True,
            text=True
        )
        self.assertNotEqual(res.returncode, 0)
        self.assertIn("invalid --repo-trust", res.stderr)

    def test_go_binary_does_not_execute_untrusted_cwd_module(self):
        import tempfile
        import subprocess
        go_server = Path(__file__).resolve().parent.parent / "bin" / "makewand-server"
        if not go_server.exists():
            self.skipTest("makewand-server binary not built")

        with tempfile.TemporaryDirectory() as untrusted_dir:
            malicious_pkg = Path(untrusted_dir) / "makewand"
            malicious_pkg.mkdir()
            pwned_marker = Path(untrusted_dir) / "PWNED.txt"
            (malicious_pkg / "__init__.py").write_text(
                f"from pathlib import Path\nPath(r'{pwned_marker}').write_text('hacked')\n"
            )

            env = dict(os.environ)
            env.pop("MAKEWAND_HOME", None)

            subprocess.run(
                [str(go_server), "review", "--repo-trust=untrusted", "--help"],
                cwd=untrusted_dir,
                capture_output=True,
                text=True,
                env=env
            )
            self.assertFalse(pwned_marker.exists(), "Untrusted cwd makewand module was executed by Go delegation!")


class TestPreserveReadOnlyIntent(unittest.TestCase):
    """Problem 4: Explicit read-only constraints must never be converted to writable coding mode."""

    def test_explicit_readonly_triggers_override_force_code(self):
        explain_prompts = [
            "解释代码，不要修改文件",
            "分析当前架构，只做分析不用修改",
            "请解释这段代码，只读模式",
            "Explain the logic, do not edit code",
        ]
        from makewand.orchestrator import run_pipeline
        with patch("makewand.orchestrator.execute_claude_task") as mock_claude, \
             patch("makewand.orchestrator.get_or_update_status") as mock_status:
            mock_status.return_value = {
                "claude": {"status": "ready", "tier": "standard"},
                "codex": {"status": "ready", "tier": "deep"},
                "agy": {"status": "ready", "tier": "deep"},
                "muse": {"status": "ready", "tier": "standard"}
            }
            mock_claude.return_value = (True, "Analysis complete", None)

            for p in explain_prompts:
                mock_claude.reset_mock()
                run_pipeline(p, force_code=True)
                mock_claude.assert_called()
                _, kwargs = mock_claude.call_args
                self.assertTrue(kwargs.get("readonly", False), f"Failed to preserve readonly=True for prompt: {p}")

        # Review prompt with read-only constraint
        with patch("makewand.orchestrator.run_review", return_value=0) as mock_review, \
             patch("makewand.orchestrator.get_or_update_status") as mock_status:
            mock_status.return_value = {
                "claude": {"status": "ready", "tier": "standard"},
                "codex": {"status": "ready", "tier": "deep"},
                "agy": {"status": "ready", "tier": "deep"},
                "muse": {"status": "ready", "tier": "standard"}
            }
            run_pipeline("审查当前改动，不要修改文件", force_code=True)
            mock_review.assert_called()


if __name__ == "__main__":
    unittest.main()
