"""Model-catalog metadata and review independence, using static fixtures only."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import argparse
import contextlib
import io
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import cli, discovery, orchestrator as orch
from makewand.account_identity import account_ref
from makewand.health import calculate_provider_quota


class ModelMetadataTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        for patcher in (
            patch("pathlib.Path.home", return_value=self.root),
            patch.dict(os.environ, {"CODEX_HOME": "", "CLAUDE_CONFIG_DIR": "",
                "GEMINI_CONFIG_DIR": str(self.root / ".gemini"), "AGY_CONFIG_DIR": "",
                "MAKEWAND_EXECUTION_EVIDENCE_DIR": str(self.root / "evidence")}),
            patch("makewand.providers.local.is_local_model_available", return_value=(False, None, [])),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)

    def write(self, path, value):
        target = self.root / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(json.dumps(value), encoding="utf-8")
        return target

    def agy_catalog(self):
        return self.write(".gemini/models_cache.json", {"models": [
            {"id": "gemini-3.8-flash", "description": "fast"},
            {"id": "claude-opus-5-5", "description": "deep"},
            {"id": "argon-99-pro", "description": "flagship"},
        ]})

    def claude_catalog(self):
        self.write(".claude/cache/model-catalog/catalog.json", {"catalog": {"config": {"models": [
            {"id": "claude-sonnet-5", "name": "Sonnet", "description": "balanced"},
            {"id": "claude-opus-5-5", "name": "Opus", "description": "deep"},
            {"id": "claude-haiku-5", "name": "Haiku", "description": "fast"},
        ]}}})

    def test_agy_foreign_entries_are_visible_with_origin_but_not_default(self):
        cache = self.agy_catalog()
        result = discovery.discover_available_models()["agy"]
        candidates = {item["model"]: item for item in result["candidates"]}
        self.assertEqual(set(candidates), {"gemini-3.8-flash", "claude-opus-5-5", "argon-99-pro"})
        for mid, candidate in candidates.items():
            self.assertEqual(candidate["source"], str(cache))
            self.assertIsNotNone(candidate["cached_at"])
            self.assertFalse(candidate["call_id_verified"])
            self.assertIsNone(candidate["account_ref"])
            self.assertIsNone(candidate["pool_ref"])
            self.assertIsNone(candidate["actual_model"])
            self.assertEqual(candidate["auto_select"], mid.startswith("gemini"))
        self.assertEqual(candidates["claude-opus-5-5"]["model_family"], "claude")
        for tier in ("fast", "standard", "deep"):
            self.assertEqual(discovery.get_provider_model_tier("agy", tier)["model"], "gemini-3.8-flash")

    def test_unverified_foreign_configured_default_does_not_override_safety(self):
        self.agy_catalog()
        self.write(".gemini/settings.json", {"model": "argon-99-pro"})
        self.assertEqual(discovery.get_provider_model_tier("agy")["model"], "gemini-3.8-flash")
        self.assertEqual(discovery.discover_available_models()["agy"]["current_default"], "gemini-3.8-flash")
        # An explicit per-call choice is still allowed and identified as explicit.
        choice = discovery.model_candidate("agy", model="argon-99-pro")
        self.assertEqual(choice["model"], "argon-99-pro")
        self.assertEqual(choice["source"], "explicit")

    def test_only_successful_observed_id_verifies_catalog_entry(self):
        self.agy_catalog()
        evidence = dict(schema=1, engine="agy", actual_model="argon-99-pro", result_status="PASSED",
                        outcome_known=True, process_exit_code=0, source="cli_json")
        path = self.write("evidence/sample.json", evidence)
        for changes in ({"outcome_known": False}, {"result_status": "UNKNOWN"},
                        {"process_exit_code": 1}, {"source": "api"}, {"engine": "claude"}):
            path.write_text(json.dumps(dict(evidence, **changes)))
            self.assertNotEqual(discovery.get_provider_model_tier("agy", "deep")["model"], "argon-99-pro")
        path.write_text(json.dumps(evidence))
        self.assertEqual(discovery.get_provider_model_tier("agy", "deep")["model"], "gemini-3.8-flash")
        entry = next(row for row in discovery.discover_available_models()["agy"]["candidates"] if row["model"] == "argon-99-pro")
        self.assertTrue(entry["call_id_verified"])
        self.assertFalse(entry["auto_select"])
        self.assertEqual(entry["verification_source"], "execution_evidence")

    def test_verified_agy_claude_stays_visible_without_becoming_default(self):
        self.agy_catalog()
        self.claude_catalog()
        self.write("evidence/claude.json", dict(schema=1, engine="agy", actual_model="claude-opus-5-5",
            result_status="PASSED", outcome_known=True, process_exit_code=0, source="cli_json"))
        self.write(".gemini/settings.json", {"model": "claude-opus-5-5"})
        self.assertEqual(discovery.get_provider_model_tier("agy", "deep")["model"], "gemini-3.8-flash")
        catalog = discovery.discover_available_models()["agy"]
        self.assertEqual(catalog["current_default"], "gemini-3.8-flash")
        entry = next(row for row in catalog["candidates"] if row["model"] == "claude-opus-5-5")
        self.assertTrue(entry["call_id_verified"])
        self.assertFalse(entry["auto_select"])
        self.assertEqual(entry["model_family"], "claude")

    def test_status_json_keeps_agy_quota_source_and_observation_time(self):
        stamp = "2026-10-15T10:00:00Z"
        status = cli.build_status_json({"agy": {"status": "healthy", "updated_at": stamp}})
        quota = status["providers"]["agy"]["quota"]
        self.assertIsNone(quota["percentage"])
        self.assertFalse(quota["is_unlimited"])
        self.assertEqual(quota["updated_at"], stamp)
        self.assertEqual(quota["source"], "unknown")

    def test_candidates_bind_account_and_pool_without_secrets(self):
        self.claude_catalog()
        self.write(".claude.json", {"oauthAccount": {"accountUuid": "fixture-account"}, "access_token": "FAKE_SECRET"})
        candidates = discovery.discover_available_models()["claude"]["candidates"]
        for candidate in candidates:
            self.assertEqual(candidate["account_ref"], account_ref("claude", "fixture-account"))
            self.assertEqual(candidate["pool_ref"], candidate["account_ref"] + ":subscription")
            self.assertEqual(candidate["model_family"], "claude")
        self.assertNotIn("FAKE_SECRET", json.dumps(candidates))

    def test_opus_selection_and_nonselection_are_explained(self):
        self.claude_catalog()
        for tier, selected in (("standard", False), ("deep", True)):
            choice = discovery.get_provider_model_tier("claude", tier)
            opus = next(row for row in choice["ranking"] if "opus" in row["model"])
            self.assertEqual(opus["selected"], selected)
            self.assertIn(f"tier={tier}", choice["selection_reason"])
            self.assertTrue(opus["reason"])
        explicit = discovery.model_candidate("claude", "fast", model="opus")
        self.assertEqual(explicit["model"], "opus")
        self.assertIn("显式", explicit["selection_reason"])

    def test_explicit_opus_reaches_cli_despite_fast_tier(self):
        from makewand.providers.claude import execute_claude_task
        from makewand.sandbox import SandboxUnavailableError
        with patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.health.load_status_cache", return_value={}), \
             patch("makewand.sandbox.provider_sandbox_launch", side_effect=SandboxUnavailableError("fixture stop")) as launch:
            execute_claude_task("fixture", cwd=str(self.root), readonly=True, tier="fast", model="opus")
        command = launch.call_args.args[1]
        self.assertEqual(command[command.index("--model") + 1], "opus")

    def test_agy_quota_stays_unknown_for_every_health_status(self):
        for status in ("healthy", "warning", "limited", "needs_auth", "unknown", "missing"):
            quota = calculate_provider_quota("agy", {"status": status, "updated_at": "2026-10-15T10:00:00Z"})
            self.assertIsNone(quota["percentage"])
            self.assertFalse(quota["is_unlimited"])
            self.assertEqual(quota["source"], "unknown")
            self.assertIn("2026-10-15T10:00:00Z", quota["desc"])

    def test_models_human_and_json_show_metadata_and_unknown_quota(self):
        self.agy_catalog()
        self.claude_catalog()
        with contextlib.redirect_stdout(io.StringIO()) as output:
            cli.cmd_models(argparse.Namespace(json=False))
        for text in ("argon-99-pro", "默认不选", "缓存时间", "额度未知", "Opus", "score="):
            self.assertIn(text, output.getvalue())
        with contextlib.redirect_stdout(io.StringIO()) as output:
            cli.cmd_models(argparse.Namespace(json=True))
        self.assertEqual(len(json.loads(output.getvalue())["agy"]["candidates"]), 3)


if __name__ == "__main__":
    unittest.main()
