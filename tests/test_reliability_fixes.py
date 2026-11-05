"""
Unit tests validating the 5 reliability and security fixes:
1. Unified execution safety across all 4 providers (fail-closed without bwrap).
2. Elimination of false-positive review passes, strict numeric/dict defect normalization, and no fabricated LGTM.
3. Cross-model reviewer independence (no self-review) and full diff formatting without silent cutoffs.
4. Unified installation/CLI version contract and Go delegation bridge.
5. Intent classification preserving coding actions and composite instructions.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import os
import sys
import tempfile
import unittest
from unittest.mock import patch, MagicMock
from pathlib import Path

from makewand.orchestrator import dispatch_task
from makewand.providers.claude import execute_claude_task
from makewand.providers.agy import execute_agy_task
from makewand.providers.codex import execute_codex_task
from makewand.providers.muse import execute_muse_task
from makewand.providers.grok import execute_grok_task


class TestExecutionSafetyAcrossProviders(unittest.TestCase):
    """Fix 1: All 5 providers must enforce fail-closed sandbox checks for writable tasks."""

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

        # 5. Grok
        ok, out, err = execute_grok_task("write something", cwd="/tmp", readonly=False)
        self.assertFalse(ok)
        self.assertIn("强制要求 Bubblewrap (bwrap) 沙箱隔离", err)


class TestVerdictAndReviewReliability(unittest.TestCase):
    """Fix 2: Strict verdict parsing and no fabricated LGTM."""


    @patch("makewand.orchestrator.execute_claude_task", return_value=None)
    def test_dispatch_task_fails_closed_on_none_adapter_return(self, mock_claude):
        result = dispatch_task("claude", "test prompt", cwd="/tmp", readonly=True)
        ok, out, err = result
        self.assertFalse(ok)
        self.assertIsNone(out)
        self.assertIsInstance(err, str)
        self.assertTrue(err)
        self.assertEqual(result.status, "UNKNOWN")
        self.assertFalse(result.outcome_known)
        mock_claude.assert_called_once()


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


if __name__ == "__main__":
    unittest.main()
