"""
G3 reliability regressions: CLI honesty and safety.

Covers arch-product#5 (Python quota numbers are local estimates; `quota --json`
missing), arch-product#6 (subcommands advertised but missing, detect-only tools,
implicit sudo), arch-product#9 (`models` presented hardcoded lists as detected
versions and claimed "zero hardcoding"), go-engine-tui-cmd#9 (installed
`go run ./cmd/makewand` fallback in the source root, no Go/Python version check).
"""

import contextlib
import io
import json
import os
import stat
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

import makewand.cli as cli
import makewand.config as config
import makewand.health as health
from makewand import __version__

DISPATCH_ENV_VAR = "MAKEWAND_DISPATCH_ID"


class _CliCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-g3-cli-")
        root = Path(self._tmp.name)
        self.root = root
        patches = [
            patch.object(config, "CONFIG_DIR", root / "cfg"),
            patch.object(config, "CONFIG_FILE", root / "cfg" / "config.json"),
            patch.object(config, "API_KEYS_FILE", root / "cfg" / "api_keys.json"),
            patch.object(config, "CANDIDATES_DIR", root / "cfg" / "candidates"),
            patch.object(config, "BACKUPS_DIR", root / "cfg" / "backups"),
            patch.object(config, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json"),
            patch.object(health, "STATUS_CACHE_FILE", root / "cfg" / "status.json"),
            patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json"),
            patch.dict(os.environ, {}, clear=False),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        saved = os.environ.get(DISPATCH_ENV_VAR)
        self.addCleanup(lambda: os.environ.pop(DISPATCH_ENV_VAR, None) if saved is None else os.environ.__setitem__(DISPATCH_ENV_VAR, saved))

    def run_main(self, *argv):
        out, err = io.StringIO(), io.StringIO()
        code = 0
        with patch.object(sys, "argv", ["makewand", *argv]), contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            try:
                cli.main()
            except SystemExit as exc:
                code = exc.code if isinstance(exc.code, int) else (0 if exc.code is None else 1)
        return code, out.getvalue(), err.getvalue()


class TestArchivedCommands(_CliCase):
    def test_archived_commands_are_refused_instead_of_becoming_a_prompt(self):
        from makewand.command_contract import ARCHIVED_COMMANDS
        prefixes = [[], ["-C", "directory with spaces"], ["--cwd=directory with spaces"],
                    ["--repo-trust", "untrusted"], ["--debug", "--approval=safe"],
                    ["-Cdirectory with spaces"], ["--max-model-calls", "12"],
                    ["--call-budget-file=temporary-ledger.json"]]
        self.assertIn("serve", ARCHIVED_COMMANDS)
        for command in sorted(ARCHIVED_COMMANDS):
            for prefix in prefixes:
                with self.subTest(command=command, prefix=prefix), \
                        patch("makewand.orchestrator.dispatch_task", side_effect=AssertionError("dispatched")):
                    code, _, err = self.run_main(*prefix, command, "--help")
                    self.assertEqual(code, cli.EXIT_USAGE_ERROR)
                    self.assertIn("已归档", err)

    def test_a_prompt_that_merely_starts_with_an_archived_word_is_not_refused(self):
        self.assertIsNone(cli._archived_command(["serve the app"]))
        self.assertIsNone(cli._archived_command(["search", "serve"]))
        self.assertIsNone(cli._archived_command(["-C", "serve"]))

    def test_help_word_shows_help(self):
        code, out, _ = self.run_main("help")
        self.assertEqual(code, 0)
        self.assertIn("usage:", out)


class TestQuotaHonesty(_CliCase):
    """arch-product#5."""

    def cache(self):
        return {
            "claude": {"status": "healthy", "reason": "Claude Code 订阅运行正常", "resets_at": None, "updated_at": "2099-01-01T00:00:00"},
            "codex": {"status": "healthy", "reason": "官方 42% left", "resets_at": None, "updated_at": "2099-01-01T00:00:00"},
            "muse": {"status": "needs_auth", "reason": "等待 OAuth", "resets_at": None, "updated_at": "2099-01-01T00:00:00"},
        }

    def test_quota_json_exposes_sources(self):
        with patch("makewand.cli.get_or_update_status", return_value=self.cache()), \
             patch("makewand.config.get_provider_execution_mode", return_value="subscription"):
            code, out, _ = self.run_main("quota", "--json")
        self.assertEqual(code, 0)
        data = json.loads(out)
        self.assertEqual(data["engine"], "python")
        self.assertIn("不是官方", data["quota_semantics"])
        self.assertEqual(data["providers"]["claude"]["quota"]["source"], "unknown")
        self.assertIsNone(data["providers"]["claude"]["quota"]["percentage"])
        self.assertEqual(data["providers"]["codex"]["quota"]["source"], "unknown")
        self.assertIsNone(data["providers"]["codex"]["quota"]["percentage"], "bare health text has no account/window binding")
        self.assertIsNone(data["providers"]["grok"]["quota"]["percentage"], "never probed is not 0%")
        self.assertIn("muse login", data["providers"]["muse"]["quota"]["desc"])

    def test_status_text_labels_estimates(self):
        with patch("makewand.cli.get_or_update_status", return_value=self.cache()), \
             patch("makewand.config.get_active_providers", return_value=["claude", "muse"]), \
             patch("makewand.config.get_provider_execution_mode", return_value="subscription"), \
             patch("makewand.cli.select_optimal_engine_pair", return_value=(["claude"], ["muse"], {})):
            code, out, _ = self.run_main("status")
        self.assertEqual(code, 0)
        self.assertIn("本地调用计数仅用于削峰", out)
        self.assertIn("不是官方剩余配额", out)
        self.assertNotIn("剩余额度:", out)
        self.assertIn("muse login", out)

    def test_probe_json_flag(self):
        with patch("makewand.cli.get_or_update_status", return_value=self.cache()), \
             patch("makewand.config.get_provider_execution_mode", return_value="subscription"):
            code, out, _ = self.run_main("probe", "--json")
        self.assertEqual(code, 0)
        data = json.loads(out)
        self.assertIn("providers", data)


class TestProviderCommands(_CliCase):
    """arch-product#6."""

    def test_enable_alias_and_no_implicit_sudo(self):
        calls = []

        def fake_run(cmd, *a, **k):
            calls.append(list(cmd))
            return MagicMock(returncode=0, stdout="inactive\n", stderr="")

        with patch("subprocess.run", side_effect=fake_run):
            code, out, err = self.run_main("enable", "ollama")
        self.assertEqual(code, 0, err)
        self.assertTrue(config.load_user_config()["enabled_providers"]["local"])
        self.assertFalse(any("sudo" in c for c in calls), f"no implicit sudo: {calls}")
        self.assertIn("sudo systemctl start ollama", out)

    def test_manage_service_uses_non_interactive_sudo(self):
        calls = []

        def fake_run(cmd, *a, **k):
            calls.append(list(cmd))
            return MagicMock(returncode=0, stdout="inactive\n", stderr="")

        with patch("subprocess.run", side_effect=fake_run):
            code, _, _ = self.run_main("disable", "local", "--manage-service")
        self.assertEqual(code, 0)
        self.assertIn(["sudo", "-n", "systemctl", "stop", "ollama"], calls)

    def test_unknown_provider_is_an_explicit_usage_error(self):
        for verb in ("enable", "disable"):
            code, _, err = self.run_main(verb, "notatool")
            self.assertEqual(code, 2)
            self.assertIn("未知或不支持的工具名称", err)
            self.assertIn("claude", err)

    def test_api_subcommands_exist(self):
        with patch("makewand.orchestrator.dispatch_task", return_value=(True, "ok", None)) as dispatch, \
             patch("makewand.usage.record_engine_usage"):
            code, out, _ = self.run_main("glm", "hi", "--readonly")
        self.assertEqual(code, 0)
        self.assertEqual(dispatch.call_args[0][0], "glm")

    def test_detect_only_tools_fail_explicitly(self):
        code, _, err = self.run_main("cursor", "do something")
        self.assertEqual(code, 2)
        self.assertIn("尚无执行适配器", err)


class TestModelsHonesty(_CliCase):
    """arch-product#9."""

    def test_builtin_lists_are_not_reported_as_detected(self):
        fake_home = self.root / "home"
        fake_home.mkdir()
        with patch("makewand.discovery.Path.home", return_value=fake_home), \
             patch("makewand.providers.local.is_local_model_available", return_value=(False, "", [])):
            code, out, _ = self.run_main("models")
        self.assertEqual(code, 0)
        self.assertNotIn("检测到版本", out)
        self.assertNotIn("零硬编码", out)
        self.assertIn("内置参考列表", out)

    def test_detected_lists_are_labelled_with_their_source(self):
        fake_home = self.root / "home"
        (fake_home / ".grok").mkdir(parents=True)
        (fake_home / ".grok" / "models_cache.json").write_text(json.dumps({"models": {"grok-x-1": {}}}))
        from makewand.discovery import discover_available_models
        with patch("makewand.discovery.Path.home", return_value=fake_home):
            models = discover_available_models()
        self.assertEqual(models["grok"]["source"], "detected")
        self.assertEqual(models["grok"]["available"], ["grok-x-1"])
        self.assertEqual(models["claude"]["source"], "builtin")


if __name__ == "__main__":
    unittest.main()
