"""Policy parsing and atomic persistence must fail before provider side effects."""
try:
    import _isolation
except ImportError:
    from tests import _isolation

import contextlib
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from makewand import config

ROOT = Path(__file__).resolve().parent.parent


class ConfigFailureBoundaries(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.config_dir = self.root / "config"
        self.config_dir.mkdir(mode=0o700)
        for name, path in {
            "CONFIG_DIR": self.config_dir,
            "CONFIG_FILE": self.config_dir / "config.json",
            "API_KEYS_FILE": self.config_dir / "api_keys.json",
            "CANDIDATES_DIR": self.config_dir / "candidates",
            "BACKUPS_DIR": self.config_dir / "backups",
        }.items():
            patcher = patch.object(config, name, path)
            patcher.start()
            self.addCleanup(patcher.stop)

    def test_missing_files_are_optional_but_invalid_policy_is_fatal(self):
        self.assertEqual(config.load_user_config(), {})
        self.assertEqual(config.load_api_keys(), {})
        for value in (b'{"active_providers":[]}X', b"[]", b"null", b'"config"', b"1", b"true", b"\xff"):
            with self.subTest(value=value):
                config.CONFIG_FILE.write_bytes(value)
                with self.assertRaises(config.ConfigError):
                    config.load_user_config()
                with patch.dict(os.environ, {"MAKEWAND_ENABLE_PROVIDERS": "claude", "MAKEWAND_API_POLICY": "allow_paid"}, clear=True):
                    with self.assertRaises(config.ConfigError):
                        config.is_provider_enabled("claude")
                    with self.assertRaises(config.ConfigError):
                        config.get_api_policy()
        config.CONFIG_FILE.unlink()
        config.CONFIG_FILE.mkdir()
        with self.assertRaises(config.ConfigError):
            config.load_user_config()

    def test_broken_credentials_warn_and_resolve_independent_environment_fields(self):
        config.CONFIG_FILE.write_text(json.dumps({"claude_api_key": "offline-flat", "claude_model": "flat-model"}))
        env = {"ANTHROPIC_API_KEY": "offline-env", "ANTHROPIC_MODEL": "env-model", "ANTHROPIC_BASE_URL": "http://127.0.0.1:1"}
        for value in ("{", "[]", "null"):
            with self.subTest(value=value), patch.dict(os.environ, env, clear=True), patch.object(config, "_credential_warnings", set()):
                config.API_KEYS_FILE.write_text(value)
                stderr = io.StringIO()
                with contextlib.redirect_stderr(stderr):
                    result = config.get_api_config("claude")
                self.assertEqual(result, {"api_key": "offline-env", "model": "env-model", "base_url": "http://127.0.0.1:1"})
                self.assertIn("ignoring this optional credentials source", stderr.getvalue())
                self.assertFalse(config.is_api_allowed("claude"))
                self.assertEqual(config.API_KEYS_FILE.read_text(), value)

    def test_atomic_saves_keep_shared_fields_and_create_private_files(self):
        config.CONFIG_FILE.write_text(json.dumps({"claude_model": "native-model", "api_policy": "subscription_only", "custom": {"keep": True}}))
        config.CONFIG_FILE.chmod(0o644)
        self.assertTrue(config.save_user_config({"enabled_providers": {"claude": False}}))
        result = json.loads(config.CONFIG_FILE.read_text())
        self.assertEqual(result["claude_model"], "native-model")
        self.assertEqual(result["custom"], {"keep": True})
        self.assertEqual(result["enabled_providers"], {"claude": False})
        config.API_KEYS_FILE.write_text(json.dumps({"claude": {"api_key": "old", "opaque": "keep"}, "codex": {"api_key": "other"}}))
        config.API_KEYS_FILE.chmod(0o644)
        self.assertTrue(config.save_api_key("claude", "new", model="fixture-model"))
        keys = json.loads(config.API_KEYS_FILE.read_text())
        self.assertEqual(keys["codex"], {"api_key": "other"})
        self.assertEqual(keys["claude"], {"api_key": "new", "opaque": "keep", "model": "fixture-model"})
        if os.name != "nt":
            for path in (config.CONFIG_FILE, config.API_KEYS_FILE):
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
        self.assertFalse(list(self.config_dir.glob(".*-*.json")))

    def test_write_and_replace_failure_preserve_old_documents(self):
        for filename, save in (("config.json", lambda: config.save_user_config({"new": True})),
                               ("api_keys.json", lambda: config.save_api_key("claude", "new"))):
            path = self.config_dir / filename
            original = '{"claude":{"api_key":"offline-old"},"shared":"keep"}'
            path.write_text(original)
            path.chmod(0o600)
            for point in ("fsync", "replace"):
                with self.subTest(filename=filename, failure=point), patch.object(config.os, point, side_effect=OSError("injected save failure")):
                    self.assertFalse(save())
                self.assertEqual(path.read_text(), original)
                self.assertFalse(list(self.config_dir.glob(".*-*.json")))
            if filename == "config.json":
                self.assertFalse(config.save_user_config({"not_json": object()}))
                self.assertEqual(path.read_text(), original)

    def test_saving_never_replaces_a_damaged_existing_document(self):
        for value in ("{", "[]", "null"):
            with self.subTest(value=value):
                config.CONFIG_FILE.write_text(value)
                config.API_KEYS_FILE.write_text(value)
                self.assertFalse(config.save_user_config({"enabled_providers": {"claude": True}}))
                self.assertFalse(config.save_api_key("claude", "offline-key"))
                self.assertEqual(config.CONFIG_FILE.read_text(), value)
                self.assertEqual(config.API_KEYS_FILE.read_text(), value)

    def test_invalid_authorization_updates_do_not_replace_valid_policy(self):
        original = '{"enabled_providers":{"claude":false},"native_field":"keep"}'
        config.CONFIG_FILE.write_text(original)
        for update in ({"enabled_providers": []}, {"enabled_providers": {"claude": "false"}},
                       {"enabled_providers": {"claude": None}}, {"active_providers": "claude"},
                       {"active_providers": ["claude", 3]}, {"local_model_enabled": 1}):
            with self.subTest(update=update):
                self.assertFalse(config.save_user_config(update))
                self.assertEqual(config.CONFIG_FILE.read_text(), original)
        self.assertTrue(config.save_user_config({"enabled_providers": None, "active_providers": None, "local_model_enabled": None}))
        self.assertEqual(config.load_user_config()["native_field"], "keep")


class CLIConfigFailureBoundaries(unittest.TestCase):
    def test_disabled_then_damaged_policy_never_starts_a_fake_provider(self):
        if os.name == "nt":
            self.skipTest("POSIX executable fixtures")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for path in (root / "config", root / "home", root / "bin", root / "work"):
                path.mkdir()
            log = root / "calls"
            for provider in ("claude", "codex", "agy", "gemini", "muse", "grok", "cursor", "copilot", "gh"):
                fixture = root / "bin" / provider
                fixture.write_text('#!/bin/sh\nprintf "started\\n" >> "$OFFLINE_PROVIDER_LOG"\nprintf "offline fixture\\n"\n')
                fixture.chmod(0o700)
            env = {"HOME": str(root / "home"), "PATH": str(root / "bin") + os.pathsep + "/usr/bin:/bin",
                   "MAKEWAND_CONFIG_DIR": str(root / "config"), "MAKEWAND_NO_DAEMON": "1",
                   "MAKEWAND_API_POLICY": "subscription_only", "OFFLINE_PROVIDER_LOG": str(log), "LANG": "C.UTF-8"}
            document = json.dumps({"active_providers": [], "enabled_providers": {"claude": False}, "local_model_enabled": False})
            path = root / "config" / "config.json"
            commands = ((["status", "--json"], 0), (["models"], 0), (["run", "explain 2 + 2", "--timeout", "1"], 11))
            for contents, damaged in ((document, False), (document + "X", True), ("[]", True), ("null", True),
                                      ('{"enabled_providers":{"claude":"false"}}', True),
                                      ('{"enabled_providers":{"claude":null}}', True),
                                      ('{"active_providers":["claude",3]}', True),
                                      ('{"local_model_enabled":"false"}', True)):
                path.write_text(contents)
                for argv, normal_code in commands:
                    with self.subTest(contents=contents, argv=argv):
                        log.write_text("")
                        process = subprocess.run([sys.executable, "-I", "-B", str(ROOT / "bin/makewand"), *argv],
                                                 cwd=root / "work", env=env, input="", text=True, capture_output=True, timeout=10)
                        self.assertEqual(process.returncode, 2 if damaged else normal_code, process.stderr)
                        self.assertEqual(log.read_text(), "", "invalid or disabled policy started a provider")
                        self.assertNotIn("Traceback", process.stderr)
                        if damaged:
                            self.assertIn("configuration error", process.stderr)


if __name__ == "__main__":
    unittest.main()
