"""E1: agy full model/effort discovery, CLI --effort, and credits-off.

All agy CLI output is fixture text injected into the parser / cache sync;
no real AI CLI is ever executed (tests/_isolation stubs them anyway).
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

import makewand.discovery as discovery
from makewand.discovery import (
    DiscoveredAgyModels,
    _discover_local_agy_models,
    _sync_agy_models_cache,
    discover_available_models,
    get_provider_model_tier,
    normalize_provider_effort,
    parse_agy_models_output,
    resolve_agy_model,
)

AGY_FIXTURE_MODELS = [
    "gemini-4-argon-low", "gemini-4-argon-medium", "gemini-4-argon-high",
    "gemini-3.8-flash-low", "gemini-3.8-flash-medium", "gemini-3.8-flash-high",
    "claude-opus-5-5-low", "claude-opus-5-5-medium", "claude-opus-5-5-high",
    "claude-sonnet-5-5-low", "claude-sonnet-5-5-medium", "claude-sonnet-5-5-high",
    "gpt-oss-120b-medium",
]
AGY_FIXTURE_TEXT = "".join(f"{m}\tDisplay {m}\n" for m in AGY_FIXTURE_MODELS)
AVAILABLE = [(m, f"Display {m}") for m in AGY_FIXTURE_MODELS]


class _HomeCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-e1-")
        self.addCleanup(self._tmp.cleanup)
        self.home = Path(self._tmp.name) / "home"
        self.home.mkdir(parents=True)
        env = {k: v for k, v in os.environ.items()
               if k not in ("GEMINI_CONFIG_DIR", "AGY_CONFIG_DIR", "ANTIGRAVITY_CONFIG_DIR", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "MUSE_DATA_DIR")}
        env["HOME"] = str(self.home)
        for p in (patch.dict(os.environ, env, clear=True),
                  patch("pathlib.Path.home", return_value=self.home),
                  patch("makewand.config.is_provider_enabled", return_value=True),
                  patch.dict(discovery._AGY_FAILED_REFRESHES, clear=True)):
            p.start()
            self.addCleanup(p.stop)

    def write_cache(self, models, age_seconds=0):
        cache = self.home / ".cache" / "makewand" / "agy_models_cache.json"
        cache.parent.mkdir(parents=True, exist_ok=True)
        cache.write_text(json.dumps({"updated_at": time.time(), "models": [{"id": i, "description": d} for i, d in models]}), encoding="utf-8")
        if age_seconds:
            stamp = time.time() - age_seconds
            os.utime(cache, (stamp, stamp))
        return cache


class TestParseAgyModelsOutput(unittest.TestCase):
    def test_tab_separated_fixture_parses_all_suffixed_ids(self):
        parsed = parse_agy_models_output(AGY_FIXTURE_TEXT)
        self.assertEqual([slug for slug, _ in parsed], AGY_FIXTURE_MODELS)
        self.assertEqual(parsed[0][1], "Display gemini-4-argon-low")

    def test_whitespace_spinner_and_ansi_are_tolerated_and_invalid_skipped(self):
        text = ("\u280b Fetching available models...\n"
                "\x1b[32mgemini-4-argon-high\tGemini 4 Argon (High)\x1b[0m\n"
                "\nnot a model!!\n--bad\tBad\n"
                "gemini-3.8-flash-low  Gemini 3.8 Flash (Low)\n")
        parsed = parse_agy_models_output(text)
        self.assertEqual([s for s, _ in parsed], ["gemini-4-argon-high", "gemini-3.8-flash-low"])

    def test_empty_output_parses_to_nothing(self):
        self.assertEqual(parse_agy_models_output(""), [])
        self.assertEqual(parse_agy_models_output("No models available\n"), [])


class TestAgyCacheAndFallback(_HomeCase):
    def test_injected_output_is_cached_and_discoverable(self):
        cache = self.home / ".cache" / "makewand" / "agy_models_cache.json"
        result = _sync_agy_models_cache(cache_file=cache, max_age=86400, output_text=AGY_FIXTURE_TEXT)
        self.assertEqual(result, cache)
        stored = json.loads(cache.read_text(encoding="utf-8"))
        self.assertEqual([m["id"] for m in stored["models"]], AGY_FIXTURE_MODELS)
        # Fresh cache: discover reads it without running any CLI.
        with patch("subprocess.run") as run:
            models = [slug for slug, _ in _discover_local_agy_models().models]
            run.assert_not_called()
        self.assertEqual(sorted(models), sorted(AGY_FIXTURE_MODELS))
        avail = discover_available_models()["agy"]
        self.assertEqual(avail["source"], "detected")
        for mid in AGY_FIXTURE_MODELS:
            self.assertTrue(any(mid in entry for entry in avail["available"]), mid)

    def test_cache_ttl_is_one_day(self):
        # 23h old: still fresh, injected refresh must not be consulted.
        cache = self.write_cache([("gemini-1.0-old", "Old")], age_seconds=23 * 3600)
        result = _sync_agy_models_cache(cache_file=cache, output_text=AGY_FIXTURE_TEXT)
        self.assertEqual(result, cache)
        self.assertEqual([m["id"] for m in json.loads(cache.read_text(encoding="utf-8"))["models"]], ["gemini-1.0-old"])
        # 25h old: stale, refresh replaces it.
        cache = self.write_cache([("gemini-1.0-old", "Old")], age_seconds=25 * 3600)
        marker = cache.with_name(cache.name + ".attempt")
        if marker.exists():
            marker.unlink()
        discovery._AGY_FAILED_REFRESHES.clear()
        _sync_agy_models_cache(cache_file=cache, output_text=AGY_FIXTURE_TEXT)
        self.assertEqual([m["id"] for m in json.loads(cache.read_text(encoding="utf-8"))["models"]], AGY_FIXTURE_MODELS)

    def test_failed_refresh_falls_back_to_previous_cache(self):
        cache = self.write_cache([("gemini-3.8-flash-high", "Gemini 3.8 Flash (High)")], age_seconds=3 * 86400)
        runner = MagicMock(return_value=MagicMock(returncode=1, stdout=""))
        result = _sync_agy_models_cache(cache_file=cache, runner=runner)
        self.assertIsNone(result)
        self.assertEqual([m["id"] for m in json.loads(cache.read_text(encoding="utf-8"))["models"]], ["gemini-3.8-flash-high"])
        models = [slug for slug, _ in _discover_local_agy_models().models]
        self.assertIn("gemini-3.8-flash-high", models)

    def test_output_without_gemini_does_not_replace_cache(self):
        cache = self.write_cache([("gemini-3.8-flash-high", "x")], age_seconds=3 * 86400)
        result = _sync_agy_models_cache(cache_file=cache, output_text="claude-sonnet-5-5-high\tClaude\n")
        self.assertIsNone(result)
        self.assertEqual([m["id"] for m in json.loads(cache.read_text(encoding="utf-8"))["models"]], ["gemini-3.8-flash-high"])

    def test_fallback_to_gemini_config_when_no_cache(self):
        # No makewand cache: the existing .gemini sources remain the fallback.
        settings = self.home / ".gemini" / "antigravity-cli" / "settings.json"
        settings.parent.mkdir(parents=True)
        settings.write_text(json.dumps({"model": "Gemini 3.8 Flash (High)"}), encoding="utf-8")
        result = get_provider_model_tier("agy", "standard")
        self.assertTrue(result["is_dynamic"])
        self.assertEqual(result["model"], "gemini-3.8-flash")


class TestResolveAgyModel(unittest.TestCase):
    def test_suffixed_id_is_used_verbatim_without_effort(self):
        for model in ("gemini-4-argon-low", "gemini-4-argon-medium", "gemini-4-argon-high", "gpt-oss-120b-medium"):
            with self.subTest(model=model):
                self.assertEqual(resolve_agy_model(model, "high", AVAILABLE), (model, None))
                self.assertEqual(resolve_agy_model(model, None, AVAILABLE), (model, None))
                self.assertEqual(resolve_agy_model(model, "low", AVAILABLE), (model, None))

    def test_base_plus_effort_picks_highest_variant_not_exceeding_request(self):
        cases = {
            "low": "gemini-4-argon-low",
            "medium": "gemini-4-argon-medium",
            "high": "gemini-4-argon-high",
            "xhigh": "gemini-4-argon-high",
            "max": "gemini-4-argon-high",
            "ultra": "gemini-4-argon-high",
        }
        for effort, expected in cases.items():
            with self.subTest(effort=effort):
                self.assertEqual(resolve_agy_model("gemini-4-argon", effort, AVAILABLE), (expected, None))

    def test_request_below_all_variants_picks_lowest_available(self):
        self.assertEqual(resolve_agy_model("gemini-4-argon", "minimal", AVAILABLE), ("gemini-4-argon-low", None))
        # Only a -medium variant exists for this base.
        self.assertEqual(resolve_agy_model("gpt-oss-120b", "low", AVAILABLE), ("gpt-oss-120b-medium", None))
        self.assertEqual(resolve_agy_model("gpt-oss-120b", "high", AVAILABLE), ("gpt-oss-120b-medium", None))

    def test_model_without_variants_uses_effort_flag(self):
        self.assertEqual(resolve_agy_model("some-plain-model", "high", AVAILABLE), ("some-plain-model", "high"))
        self.assertEqual(resolve_agy_model("some-plain-model", "medium", AVAILABLE), ("some-plain-model", "medium"))
        self.assertEqual(resolve_agy_model("some-plain-model", "xhigh", AVAILABLE), ("some-plain-model", "xhigh"))
        self.assertEqual(resolve_agy_model("some-plain-model", "max", AVAILABLE), ("some-plain-model", "max"))
        self.assertEqual(resolve_agy_model("some-plain-model", "ultra", AVAILABLE), ("some-plain-model", "max"))

    def test_base_without_effort_stays_bare(self):
        self.assertEqual(resolve_agy_model("gemini-4-argon", None, AVAILABLE), ("gemini-4-argon", None))


class TestNormalizeAndCliEffort(unittest.TestCase):
    def test_agy_effort_passes_through_and_clamps_ultra(self):
        self.assertEqual(normalize_provider_effort("agy", "low"), "low")
        self.assertEqual(normalize_provider_effort("agy", "medium"), "medium")
        self.assertEqual(normalize_provider_effort("agy", "high"), "high")
        self.assertEqual(normalize_provider_effort("agy", "xhigh"), "xhigh")
        self.assertEqual(normalize_provider_effort("agy", "max"), "max")
        self.assertEqual(normalize_provider_effort("agy", "ultra"), "max")

    def test_other_providers_keep_existing_clamping(self):
        self.assertEqual(normalize_provider_effort("grok", "xhigh"), "xhigh")
        self.assertEqual(normalize_provider_effort("grok", "max"), "xhigh")
        self.assertEqual(normalize_provider_effort("grok", "ultra"), "xhigh")
        self.assertEqual(normalize_provider_effort("claude", "xhigh"), "xhigh")
        self.assertEqual(normalize_provider_effort("claude", "ultra"), "max")
        self.assertEqual(normalize_provider_effort("muse", "ultra"), "ultra")
        self.assertEqual(normalize_provider_effort("muse", "xhigh"), "xhigh")
        # Codex without a cache passes the level through (model clamp is cache-driven).
        self.assertEqual(normalize_provider_effort("codex", "xhigh"), "xhigh")
        self.assertEqual(normalize_provider_effort("codex", "ultra"), "ultra")

    def test_cli_accepts_xhigh_and_ultra_and_rejects_bogus(self):
        import makewand.cli as cli
        captured = {}

        def fake_dispatch(args, is_auto_routed_run=False):
            captured["effort"] = getattr(args, "effort", None)
            captured["subcommand"] = args.subcommand

        original_argv = sys.argv
        original_cwd = os.getcwd()
        try:
            for value in ("low", "medium", "high", "xhigh", "max", "ultra"):
                with self.subTest(effort=value):
                    captured.clear()
                    with patch.object(cli, "_dispatch_parsed_command", side_effect=fake_dispatch), \
                         patch("makewand.daemon.try_dispatch_via_daemon", return_value=None):
                        sys.argv = ["makewand", "agy", "hello", "--effort", value]
                        cli._main()
                    self.assertEqual(captured.get("effort"), value)
                    self.assertEqual(captured.get("subcommand"), "agy")
            with self.assertRaises(SystemExit) as ctx:
                sys.argv = ["makewand", "agy", "hello", "--effort", "bogus"]
                cli._main()
            self.assertEqual(ctx.exception.code, 2)
        finally:
            sys.argv = original_argv
            os.chdir(original_cwd)


class TestAgyProviderCommandConstruction(_HomeCase):
    def _write_host_settings(self, cli_value, root_value):
        cli = self.home / ".gemini" / "antigravity-cli" / "settings.json"
        cli.parent.mkdir(parents=True, exist_ok=True)
        root = self.home / ".gemini" / "settings.json"
        if cli_value is not None:
            cli.write_text(json.dumps({"useG1Credits": cli_value}), encoding="utf-8")
        if root_value is not None:
            root.write_text(json.dumps({"useG1Credits": root_value}), encoding="utf-8")

    def _dispatch_cmd(self, model, effort):
        from makewand.providers import agy as agy_provider
        discovered = DiscoveredAgyModels(AVAILABLE, None, None)
        workspace = Path(self._tmp.name) / "ws"
        workspace.mkdir(exist_ok=True)
        # No-bwrap direct runs are only allowed when both host settings
        # already prove useG1Credits is false (see TestAgyNoSandboxCredits).
        self._write_host_settings(False, False)
        with patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("makewand.sandbox._agy_host_credits_disabled", return_value=True), \
             patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(True, "config-ack")), \
             patch("makewand.sandbox.audit_unsafe_host_exec"), \
             patch.object(discovery, "_discover_local_agy_models", return_value=discovered), \
             patch.object(agy_provider, "run_subprocess", return_value=(0, "ok", "", None)) as run:
            agy_provider.execute_agy_task("hello", cwd=str(workspace), model=model, effort=effort, readonly=True)
        return run.call_args[0][0]

    def test_suffixed_model_verbatim_no_effort_flag(self):
        cmd = self._dispatch_cmd("gemini-4-argon-high", "low")
        self.assertEqual(cmd[cmd.index("--model") + 1], "gemini-4-argon-high")
        self.assertNotIn("--effort", cmd)

    def test_base_model_selects_variant_for_effort(self):
        cmd = self._dispatch_cmd("gemini-4-argon", "medium")
        self.assertEqual(cmd[cmd.index("--model") + 1], "gemini-4-argon-medium")
        self.assertNotIn("--effort", cmd)
        cmd = self._dispatch_cmd("gemini-4-argon", "xhigh")
        self.assertEqual(cmd[cmd.index("--model") + 1], "gemini-4-argon-high")
        self.assertNotIn("--effort", cmd)

    def test_plain_model_uses_effort_flag(self):
        cmd = self._dispatch_cmd("some-plain-model", "xhigh")
        self.assertEqual(cmd[cmd.index("--model") + 1], "some-plain-model")
        self.assertEqual(cmd[cmd.index("--effort") + 1], "xhigh")


class TestAgyCreditsOff(unittest.TestCase):
    def _materialize(self, home: Path):
        from makewand.sandbox import ProviderHome
        ph = ProviderHome("agy", str(home))
        try:
            args = ph.materialize([str(home / "proj")])
            priv = Path(ph._private_roots[".gemini"])
            return ph, args, priv
        except Exception:
            ph.finalize(write_back=False)
            raise

    def test_private_home_forces_use_g1_credits_false_and_preserves_host(self):
        with tempfile.TemporaryDirectory() as td:
            home = Path(td) / "home"
            cli_settings = home / ".gemini" / "antigravity-cli" / "settings.json"
            cli_settings.parent.mkdir(parents=True)
            cli_settings.write_text(json.dumps({"useG1Credits": True, "theme": "dark", "model": "x"}), encoding="utf-8")
            root_settings = home / ".gemini" / "settings.json"
            root_settings.write_text(json.dumps({"useG1Credits": True, "other": 1}), encoding="utf-8")
            (home / ".gemini" / "oauth_creds.json").write_text("{}", encoding="utf-8")
            before_cli = cli_settings.read_text(encoding="utf-8")
            before_root = root_settings.read_text(encoding="utf-8")
            ph, args, priv = self._materialize(home)
            try:
                # The file agy actually reads in the sandbox is the private
                # antigravity-cli/settings.json (ro-bound over the host path).
                effective = json.loads((priv / "antigravity-cli" / "settings.json").read_text(encoding="utf-8"))
                self.assertIs(effective["useG1Credits"], False)
                self.assertEqual(effective["theme"], "dark")
                self.assertIs(json.loads((priv / "settings.json").read_text(encoding="utf-8"))["useG1Credits"], False)
                # Each sanitized file is bound read-only at the host location.
                for rel in ("settings.json", "antigravity-cli/settings.json"):
                    dst = str(priv / rel)
                    host = str(home / ".gemini" / rel)
                    idx = args.index(dst)
                    self.assertEqual(args[idx - 1], "--ro-bind")
                    self.assertEqual(args[idx + 1], host)
            finally:
                ph.finalize(write_back=False)
            self.assertEqual(cli_settings.read_text(encoding="utf-8"), before_cli)
            self.assertEqual(root_settings.read_text(encoding="utf-8"), before_root)
            self.assertIs(json.loads(cli_settings.read_text(encoding="utf-8"))["useG1Credits"], True)

    def test_private_home_without_host_settings_still_disables_credits(self):
        with tempfile.TemporaryDirectory() as td:
            home = Path(td) / "home"
            (home / ".gemini").mkdir(parents=True)
            ph, args, priv = self._materialize(home)
            try:
                for rel in ("settings.json", "antigravity-cli/settings.json"):
                    self.assertIs(json.loads((priv / rel).read_text(encoding="utf-8"))["useG1Credits"], False)
            finally:
                ph.finalize(write_back=False)
            self.assertFalse((home / ".gemini" / "settings.json").exists())
            self.assertFalse((home / ".gemini" / "antigravity-cli" / "settings.json").exists())

    def test_private_home_without_any_gemini_dir_still_disables_credits(self):
        with tempfile.TemporaryDirectory() as td:
            home = Path(td) / "home"
            home.mkdir()
            ph, args, priv = self._materialize(home)
            try:
                self.assertIs(json.loads((priv / "antigravity-cli" / "settings.json").read_text(encoding="utf-8"))["useG1Credits"], False)
            finally:
                ph.finalize(write_back=False)
            self.assertFalse((home / ".gemini").exists())



class TestAgyNoSandboxCredits(_HomeCase):
    """P0: without bwrap, agy must fail closed unless both host settings
    already prove useG1Credits is false."""

    def _write(self, rel, value):
        path = self.home / ".gemini" / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        if isinstance(value, str):
            path.write_text(value, encoding="utf-8")
        else:
            path.write_text(json.dumps({"useG1Credits": value}), encoding="utf-8")

    def _execute(self):
        from makewand.providers import agy as agy_provider
        workspace = Path(self._tmp.name) / "ws"
        workspace.mkdir(exist_ok=True)
        discovered = DiscoveredAgyModels(AVAILABLE, None, None)
        with patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(True, "config-ack")), \
             patch("makewand.sandbox.audit_unsafe_host_exec"), \
             patch.object(discovery, "_discover_local_agy_models", return_value=discovered), \
             patch.object(agy_provider, "run_subprocess", return_value=(0, "ok", "", None)) as run:
            result = agy_provider.execute_agy_task(
                "hello", cwd=str(workspace), model="gemini-4-argon-low", readonly=True)
        return result, run

    def test_host_true_blocks_and_never_runs_agy(self):
        self._write("settings.json", True)
        self._write("antigravity-cli/settings.json", True)
        (ok, _out, err), run = self._execute()
        self.assertFalse(ok)
        self.assertIn("useG1Credits", err)
        self.assertIn("沙箱", err)
        self.assertIn("credits", err)
        run.assert_not_called()

    def test_one_true_or_missing_or_malformed_still_blocks(self):
        cases = [
            (False, True), (True, False), (False, None), (None, False),
            ("not json", False), (False, "false"),
        ]
        for cli_value, root_value in cases:
            with self.subTest(cli=cli_value, root=root_value):
                for rel in ("settings.json", "antigravity-cli/settings.json"):
                    path = self.home / ".gemini" / rel
                    if path.exists():
                        path.unlink()
                if root_value is not None:
                    self._write("settings.json", root_value)
                if cli_value is not None:
                    self._write("antigravity-cli/settings.json", cli_value)
                (ok, _out, err), run = self._execute()
                self.assertFalse(ok)
                self.assertIn("useG1Credits", err)
                run.assert_not_called()

    def test_config_dir_variable_blocks_even_with_both_false(self):
        # agy could read settings elsewhere than the ~/.gemini files checked.
        self._write("settings.json", False)
        self._write("antigravity-cli/settings.json", False)
        for name in ("GEMINI_CONFIG_DIR", "AGY_CONFIG_DIR", "ANTIGRAVITY_CONFIG_DIR"):
            with self.subTest(name=name), patch.dict(os.environ, {name: str(self.home / "alt")}):
                (ok, _out, err), run = self._execute()
                self.assertFalse(ok)
                self.assertIn(name, err)
                run.assert_not_called()

    def test_both_false_allows_authorized_direct_run(self):
        self._write("settings.json", False)
        self._write("antigravity-cli/settings.json", False)
        (ok, out, err), run = self._execute()
        self.assertTrue(ok, err)
        self.assertEqual(out, "ok")
        run.assert_called_once()

    def test_other_providers_are_not_blocked_by_agy_settings(self):
        from makewand.sandbox import provider_sandbox_launch
        self._write("settings.json", True)
        self._write("antigravity-cli/settings.json", True)
        workspace = Path(self._tmp.name) / "workspace"
        workspace.mkdir()
        # The real agy check runs (and would fail on these settings): codex must not use it.
        with patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(True, "config-ack")), \
             patch("makewand.sandbox.audit_unsafe_host_exec"):
            with provider_sandbox_launch("codex", ["codex", "exec", "hi"], str(workspace), readonly=True) as launch:
                self.assertEqual(launch.cmd, ["codex", "exec", "hi"])
                self.assertFalse(launch.sandboxed)

    def test_sandboxed_agy_drops_config_dir_variables_from_extra_env(self):
        from contextlib import contextmanager
        from makewand.sandbox import provider_sandbox_launch
        captured = {}

        @contextmanager
        def fake_seccomp(enabled=True):
            yield None, ()

        @contextmanager
        def fake_home(provider, roots):
            yield None

        def fake_wrap(cmd, **kwargs):
            captured.update(kwargs)
            return list(cmd)

        extra = {"AGY_CONFIG_DIR": "/elsewhere", "gemini_config_dir": "/elsewhere",
                 "ANTIGRAVITY_CONFIG_DIR": "/elsewhere", "KEEP_ME": "1"}
        for provider, kept in (("agy", {"KEEP_ME": "1"}), ("codex", extra)):
            captured.clear()
            with self.subTest(provider=provider), \
                    patch("makewand.sandbox.is_bwrap_available", return_value=True), \
                    patch("makewand.sandbox.seccomp_filter_context", fake_seccomp), \
                    patch("makewand.sandbox.provider_home_session", fake_home), \
                    patch("makewand.sandbox.wrap_bwrap", fake_wrap):
                with provider_sandbox_launch(provider, [provider, "-p", "x"], str(self.home), readonly=True,
                                             extra_env=dict(extra), apply_limits=False):
                    pass
                self.assertEqual(captured["extra_env"], kept)


if __name__ == "__main__":
    unittest.main()
