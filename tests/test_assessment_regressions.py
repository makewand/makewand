"""Behavioral regressions for the independent 2026-09-27 assessment."""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
    from _verdicts import nonced
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401
    from tests._verdicts import nonced

import contextlib
import io
import json
import os
import shutil
import stat
import subprocess
import tempfile
import unittest
from datetime import datetime
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
import makewand.orchestrator as orch
from makewand.git_helper import run_git_cmd
from makewand.providers.api_client import call_api_chat


def git(path, *args):
    code, out, err = run_git_cmd(["git", *args], cwd=str(path))
    if code:
        raise AssertionError((args, out, err))
    return out.strip()


def init_repo(path):
    path.mkdir(parents=True)
    git(path, "init")
    git(path, "config", "user.name", "Assessment regression")
    git(path, "config", "user.email", "test@example.invalid")


def fixture_process(cmd, workspace, **kwargs):
    # Only executes the fixed test fixtures defined below, never model output.
    proc = subprocess.run(cmd, cwd=workspace, capture_output=True, text=True,
                          env={**os.environ, **kwargs.get("extra_env", {})}, timeout=45)
    return proc.returncode, proc.stdout, proc.stderr, None


class TestAssessmentPolicies(unittest.TestCase):
    def test_relative_quota_deadline_is_idempotent(self):
        class Clock(datetime):
            @classmethod
            def now(cls, tz=None):
                return datetime(2026, 1, 1, 12, 0, tzinfo=tz)

        for reset in ["in 2 hours", "in 1.5 hours", "in 1h 30m"]:
            with self.subTest(reset=reset), patch.object(health, "datetime", Clock):
                cache = {"codex": {"status": "limited", "updated_at": "2026-01-01T11:00:00", "resets_at": reset}}
                first = json.loads(json.dumps(health._sanitize_cache(cache)))
                for _ in range(5):
                    health._sanitize_cache(cache)
                self.assertEqual(cache, first)
                self.assertEqual(cache["codex"]["status"], "limited")

    def test_default_policy_blocks_api_before_http_even_with_credentials(self):
        with patch.dict(os.environ, {"MAKEWAND_API_POLICY": "subscription_only"}), \
             patch("makewand.providers.api_client.get_api_config", return_value={"api_key": "test-only"}), \
             patch("makewand.providers.api_client._make_http_request") as http:
            ok, _, error = call_api_chat("claude", "test")
        self.assertFalse(ok)
        self.assertIn("subscription_only", error)
        http.assert_not_called()

    def test_invalid_api_policy_fails_closed(self):
        with patch.dict(os.environ, {"MAKEWAND_API_POLICY": "allow_piad"}):
            self.assertFalse(config.is_api_allowed("codex"))

    def test_runtime_routing_obeys_api_policy(self):
        with patch.dict(os.environ, {"MAKEWAND_API_POLICY": "subscription_only"}), \
             patch.object(config, "has_subscription_configured", return_value=True), \
             patch.object(config, "has_api_configured", return_value=True), \
             patch.object(config, "load_user_config", return_value={}):
            self.assertEqual(config.get_provider_execution_mode("codex"), "subscription")
        with patch.dict(os.environ, {"MAKEWAND_API_POLICY": "allow_paid"}), \
             patch.object(config, "has_subscription_configured", return_value=True), \
             patch.object(config, "has_api_configured", return_value=True), \
             patch.object(config, "load_user_config", return_value={}):
            self.assertEqual(config.get_provider_execution_mode("codex"), "hybrid")

    def test_local_is_disabled_by_default_and_explicitly_enablable(self):
        with patch.dict(os.environ, {}, clear=True), patch.object(config, "load_user_config", return_value={}):
            self.assertFalse(config.is_provider_enabled("local"))
        with patch.dict(os.environ, {}, clear=True), patch.object(config, "load_user_config", return_value={"enabled_providers": {"local": True}}):
            self.assertTrue(config.is_provider_enabled("local"))

    def test_legacy_installer_settings_and_canonical_precedence(self):
        legacy = {"active_providers": ["claude", "codex", "agy"], "local_model_enabled": False}
        with patch.dict(os.environ, {}, clear=True), patch.object(config, "load_user_config", return_value=legacy):
            enabled = config.get_enabled_providers()
            self.assertTrue(enabled["codex"])
            self.assertFalse(enabled["local"])
            self.assertFalse(enabled["deepseek"])
            legacy["enabled_providers"] = {"local": True, "codex": False}
            enabled = config.get_enabled_providers()
            self.assertTrue(enabled["local"])
            self.assertFalse(enabled["codex"])

    def test_string_false_cannot_enable_local_provider(self):
        with patch.dict(os.environ, {}, clear=True), patch.object(config, "load_user_config", return_value={"enabled_providers": {"local": "false"}}):
            self.assertFalse(config.is_provider_enabled("local"))

