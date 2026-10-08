"""
Tests for [P1] arch-product-5: macOS and host execution guidance when Bubblewrap is unavailable.

Validates:
1. verify_writable_sandbox_or_authorized helper behavior across Linux and Darwin.
2. Platform-specific guidance for macOS users (pointing to read-only review, remote runner, or authorized host exec).
3. CLI providers (Claude, Codex, AGY, Grok, Muse, Aider) fail-closed with structured guidance when unauthorized.
4. CLI providers succeed and audit host execution when authorized via MAKEWAND_UNSAFE_HOST_EXEC=1.
5. Orchestrator run_pipeline and run_race upfront fail-fast behavior avoiding repeated engine failures.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import io
import json
import os
import socket
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch, MagicMock

import makewand.config as mw_config
from makewand import sandbox
from makewand.sandbox import (
    get_writable_sandbox_guidance,
    verify_writable_sandbox_or_authorized,
)
from makewand.providers.claude import execute_claude_task
from makewand.providers.codex import execute_codex_task
from makewand.providers.agy import execute_agy_task
from makewand.providers.grok import execute_grok_task
from makewand.providers.muse import execute_muse_task
from makewand.providers.aider import execute_aider_task


class TestWritableSandboxVerification(unittest.TestCase):
    def test_bwrap_available_allows_execution_on_any_platform(self):
        with patch("makewand.sandbox.is_bwrap_available", return_value=True):
            for plat in ("darwin", "linux", "win32"):
                with patch("sys.platform", plat):
                    ok, source, err = verify_writable_sandbox_or_authorized("claude", repo_trust="trusted")
                    self.assertTrue(ok)
                    self.assertIsNone(source)
                    self.assertIsNone(err)

    def test_untrusted_repo_refuses_when_bwrap_unavailable(self):
        with patch("makewand.sandbox.is_bwrap_available", return_value=False):
            ok, source, err = verify_writable_sandbox_or_authorized("claude", repo_trust="untrusted")
            self.assertFalse(ok)
            self.assertIsNone(source)
            self.assertIn("不可信仓库", err)
            self.assertIn("Bubblewrap", err)

    def test_darwin_guidance_when_bwrap_unavailable_and_unauthorized(self):
        with patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("sys.platform", "darwin"), \
             patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(False, None)):
            ok, source, err = verify_writable_sandbox_or_authorized("claude", repo_trust="trusted")
            self.assertFalse(ok)
            self.assertIsNone(source)
            self.assertIn("macOS", err)
            self.assertIn("Bubblewrap", err)
            self.assertIn("makewand review", err)
            self.assertIn("--remote-url", err)
            self.assertIn("MAKEWAND_UNSAFE_HOST_EXEC=1", err)
            self.assertIn("unsafe_exec_audit.jsonl", err)

    def test_linux_guidance_when_bwrap_unavailable_and_unauthorized(self):
        with patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("sys.platform", "linux"), \
             patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(False, None)):
            ok, source, err = verify_writable_sandbox_or_authorized("codex", repo_trust="trusted")
            self.assertFalse(ok)
            self.assertIsNone(source)
            self.assertIn("sudo apt install bubblewrap", err)
            self.assertIn("makewand review", err)
            self.assertIn("MAKEWAND_UNSAFE_HOST_EXEC=1", err)

    def test_authorized_host_exec_allows_execution(self):
        with patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(True, "config-ack")):
            ok, source, err = verify_writable_sandbox_or_authorized("claude", repo_trust="trusted")
            self.assertTrue(ok)
            self.assertEqual(source, "config-ack")
            self.assertIsNone(err)


class TestProvidersHostExecution(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.cfg_dir = Path(self._tmp.name) / "config"
        self.ws = Path(self._tmp.name) / "ws"
        self.ws.mkdir()
        self.cfg_dir.mkdir(parents=True, exist_ok=True)
        self._patches = [
            patch.object(mw_config, "CONFIG_DIR", self.cfg_dir),
            patch("makewand.sandbox.is_bwrap_available", return_value=False),
            patch.dict(sandbox._host_exec_session, {"warned": False, "declined": False}),
        ]
        for p in self._patches:
            p.start()
        self.addCleanup(self._cleanup)

    def _cleanup(self):
        for p in reversed(self._patches):
            p.stop()
        self._tmp.cleanup()

    def _write_valid_ack(self):
        cfg = {
            "language": "zh",
            "unsafe_host_exec_ack_version": sandbox.UNSAFE_HOST_EXEC_ACK_CURRENT_VERSION,
            "unsafe_host_exec_ack_at": "2026-10-08T00:00:00Z",
            "unsafe_host_exec_ack_host": socket.gethostname(),
        }
        (self.cfg_dir / "config.json").write_text(json.dumps(cfg), encoding="utf-8")

    def _read_audit_records(self):
        audit_file = self.cfg_dir / sandbox.UNSAFE_HOST_EXEC_AUDIT_FILE
        if not audit_file.exists():
            return []
        return [json.loads(line) for line in audit_file.read_text(encoding="utf-8").splitlines() if line.strip()]

    def test_provider_fails_closed_with_darwin_guidance_when_unauthorized(self):
        with patch("sys.platform", "darwin"), \
             patch.dict(os.environ, {}, clear=True), \
             patch("makewand.health.load_status_cache", return_value={"claude": {"status": "healthy"}}), \
             patch("makewand.config.has_subscription_configured", return_value=True):
            ok, out, err = execute_claude_task("write something", cwd=str(self.ws), readonly=False)
            self.assertFalse(ok)
            self.assertIn("macOS", err)
            self.assertIn("MAKEWAND_UNSAFE_HOST_EXEC=1", err)
            self.assertEqual(len(self._read_audit_records()), 0)

    def test_all_providers_succeed_and_audit_when_authorized(self):
        self._write_valid_ack()
        env_patch = {"MAKEWAND_UNSAFE_HOST_EXEC": "1"}

        providers = [
            ("claude", execute_claude_task, "makewand.providers.claude"),
            ("codex", execute_codex_task, "makewand.providers.codex"),
            ("agy", execute_agy_task, "makewand.providers.agy"),
            ("grok", execute_grok_task, "makewand.providers.grok"),
            ("muse", execute_muse_task, "makewand.providers.muse"),
            ("aider", execute_aider_task, "makewand.providers.aider"),
        ]

        for p_name, p_fn, p_module in providers:
            with self.subTest(provider=p_name):
                with patch.dict(os.environ, env_patch), \
                     patch("makewand.health.load_status_cache", return_value={p_name: {"status": "healthy"}}), \
                     patch("makewand.config.has_subscription_configured", return_value=True), \
                     patch("makewand.config.is_provider_enabled", return_value=True), \
                     patch("makewand.providers.base.run_subprocess", return_value=(0, "success", "", None)), \
                     patch("makewand.providers.aider.is_aider_available", return_value=True), \
                     patch("makewand.providers.muse.detect_muse_guard", return_value=(False, None)), \
                     patch("makewand.providers.muse.get_muse_executable", return_value=("muse", [])):
                    # For modules that imported run_subprocess at module level
                    mod = sys.modules.get(p_module)
                    if mod and hasattr(mod, "run_subprocess"):
                        with patch.object(mod, "run_subprocess", return_value=(0, "success", "", None)):
                            ok, out, err = p_fn("write test code", cwd=str(self.ws), readonly=False)
                    else:
                        ok, out, err = p_fn("write test code", cwd=str(self.ws), readonly=False)
                    self.assertTrue(ok, f"{p_name} failed: {err}")
                    records = self._read_audit_records()
                    self.assertGreater(len(records), 0)
                    latest = records[-1]
                    self.assertEqual(latest["context"], f"provider:{p_name}")
                    self.assertEqual(latest["source"], "config-ack")


class TestOrchestratorUpfrontFailFast(unittest.TestCase):
    def test_run_pipeline_fails_fast_when_writable_and_unauthorized(self):
        from makewand.orchestrator import run_pipeline

        with tempfile.TemporaryDirectory() as tmp:
            with patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                 patch("sys.platform", "darwin"), \
                 patch.dict(os.environ, {}, clear=True), \
                 patch("makewand.health.get_or_update_status", return_value={
                     "claude": {"status": "healthy"},
                     "codex": {"status": "healthy"},
                 }), \
                 patch("makewand.orchestrator.select_optimal_engine_pair", return_value=(
                     ["claude", "codex"], ["codex"], {"primary_coder": "claude", "primary_reviewer": "codex"}
                 )), \
                 patch("makewand.orchestrator.dispatch_task") as mock_dispatch:
                res = run_pipeline("请实现一个新特性", cwd=tmp, force_code=True)
                self.assertFalse(res)
                mock_dispatch.assert_not_called()

    def test_run_race_fails_fast_when_unauthorized(self):
        from makewand.orchestrator import run_race, EXIT_UNVERIFIED

        with tempfile.TemporaryDirectory() as tmp:
            with patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                 patch("sys.platform", "darwin"), \
                 patch.dict(os.environ, {}, clear=True), \
                 patch("makewand.health.get_or_update_status", return_value={
                     "claude": {"status": "healthy"},
                     "codex": {"status": "healthy"},
                 }), \
                 patch("makewand.config.get_active_providers", return_value=["claude", "codex"]), \
                 patch("makewand.config.is_provider_enabled", return_value=True):
                code = run_race("编写测试", cwd=tmp, engine_a="claude", engine_b="codex")
                self.assertEqual(code, EXIT_UNVERIFIED)


if __name__ == "__main__":
    unittest.main()
