"""Model-free regression checks for quota scope and trusted pre-launch refusals."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

from datetime import datetime, timedelta
from contextlib import contextmanager
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from makewand import health, model_quota


class ModelQuotaScopeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.identity = {"credential_root": str(self.root), "account_ref": "fixture-account-A",
                         "identity_known": True, "pool_ref": "fixture-pool-A"}
        for target, value in (("makewand.health.STATUS_CACHE_FILE", self.root / "status.json"),
                              ("makewand.health.LEGACY_TRIO_CACHE", self.root / "legacy.json")):
            guard = patch(target, value)
            guard.start()
            self.addCleanup(guard.stop)
        for guard in (patch("makewand.health.ensure_config_dir"),
                      patch("makewand.health._get_official_subscription_quota", return_value=None),
                      patch("makewand.model_quota.selected_scope", side_effect=lambda provider: dict(self.identity))):
            guard.start()
            self.addCleanup(guard.stop)
        self.reset = (datetime.now() + timedelta(hours=1)).isoformat()

    def info(self, provider):
        return health.load_status_cache().get(provider, {})

    def test_agy_native_failure_does_not_block_opus_or_sonnet(self):
        health.record_engine_limit("agy", "fixture native refusal", self.reset, model="gemini-4-argon-high")
        for model in ("claude-opus-5-5-high", "claude-sonnet-5-5-high"):
            self.assertIsNone(model_quota.quota_reason("agy", model, quota=False, info=self.info("agy")))
        for model in ("gemini-4-argon-high", "gemini-4-argon-medium"):
            self.assertIsNotNone(model_quota.quota_reason("agy", model, quota=False, info=self.info("agy")))

    def test_agy_opus_refusal_does_not_block_sonnet(self):
        health.record_engine_limit("agy", "fixture opus refusal", self.reset, model="claude-opus-5-5-high")
        self.assertIsNotNone(model_quota.quota_reason("agy", "claude-opus-5-5-low", quota=False, info=self.info("agy")))
        self.assertIsNone(model_quota.quota_reason("agy", "claude-sonnet-5-5-high", quota=False, info=self.info("agy")))

    def test_legacy_native_google_refusal_preserves_native_guard_not_proxy_guard(self):
        health.save_status_cache({"agy": {"status": "limited", "reason": "Google AI 配额暂时耗尽",
                                           "resets_at": self.reset, "updated_at": datetime.now().isoformat()}})
        data = self.info("agy")
        self.assertEqual(data["status"], "unknown")
        self.assertIsNotNone(model_quota.quota_reason("agy", "gemini-4-argon-high", quota=False, info=data))
        self.assertIsNone(model_quota.quota_reason("agy", "claude-opus-5-5-high", quota=False, info=data))
        self.assertIsNone(model_quota.quota_reason("agy", "claude-sonnet-5-5-high", quota=False, info=data))

    def test_agy_adapter_passes_opus_and_sonnet_after_legacy_gemini_refusal(self):
        from makewand.providers.agy import execute_agy_task
        from makewand.discovery import DiscoveredAgyModels
        health.save_status_cache({"agy": {"status": "limited", "reason": "Google AI 配额暂时耗尽",
                                           "resets_at": self.reset, "updated_at": datetime.now().isoformat()}})
        models = ["claude-opus-5-5-high", "claude-sonnet-5-5-high", "gemini-4-argon-high"]
        @contextmanager
        def launch(provider, cmd, cwd, **kwargs):
            yield SimpleNamespace(cmd=cmd, pass_fds=(), env={})
        with patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.discovery._discover_local_agy_models", return_value=DiscoveredAgyModels([(m, "") for m in models], None, None)), \
             patch("makewand.sandbox.provider_sandbox_launch", side_effect=launch), \
             patch("makewand.providers.agy.run_subprocess", return_value=(0, "fixture answer", "", None)) as run:
            for model in models[:2]:
                result = execute_agy_task("fixture", cwd=str(self.root), model=model, readonly=True)
                self.assertTrue(result[0], result)
                self.assertIn(model, run.call_args.args[0])
            run.reset_mock()
            result = execute_agy_task("fixture", cwd=str(self.root), model=models[2], readonly=True)
            self.assertFalse(result[0])
            run.assert_not_called()

    def test_new_opus_refusal_preserves_legacy_gemini_guard(self):
        self.root.joinpath("status.json").write_text(json.dumps({"agy": {
            "status": "limited", "reason": "Google AI 配额暂时耗尽", "resets_at": self.reset,
            "updated_at": datetime.now().isoformat()}}), encoding="utf-8")
        health.record_engine_limit("agy", "fixture opus refusal", self.reset, model="claude-opus-5-5-high")
        data = self.info("agy")
        self.assertIsNotNone(model_quota.quota_reason("agy", "gemini-4-argon-high", quota=False, info=data))
        self.assertIsNotNone(model_quota.quota_reason("agy", "claude-opus-5-5-high", quota=False, info=data))
        self.assertIsNone(model_quota.quota_reason("agy", "claude-sonnet-5-5-high", quota=False, info=data))

    def test_version_probe_preserves_other_models_refusal_without_spending_a_model(self):
        health.record_engine_limit("agy", "fixture argon refusal", self.reset, model="gemini-4-argon-high")
        with patch("makewand.config.get_all_supported_providers", return_value=["agy"]), \
             patch("makewand.health.probe_model", return_value={"status": "healthy", "verified": False,
                   "reason": "fixture version only", "updated_at": datetime.now().isoformat()}), \
             patch("makewand.health._probe_agy_for_real") as generated_probe:
            data = health.get_or_update_status(force_probe=True)["agy"]
        generated_probe.assert_not_called()
        self.assertIsNotNone(model_quota.quota_reason("agy", "gemini-4-argon-high", quota=False, info=data))
        self.assertIsNone(model_quota.quota_reason("agy", "claude-opus-5-5-high", quota=False, info=data))
        self.assertIsNone(model_quota.quota_reason("agy", "claude-sonnet-5-5-high", quota=False, info=data))

    def test_route_keeps_agy_opus_while_native_gemini_is_blocked(self):
        from makewand.route_select import Policy, select
        from makewand.outcome_ledger import OutcomeLedger
        health.record_engine_limit("agy", "fixture native refusal", self.reset, model="gemini-4-argon-high")
        result = select("review", "hard", risk="high", self_tool="codex", not_tool="codex",
                        full=True, policy=Policy(), quota_reader=lambda: {},
                        status_reader=health.load_status_cache, ledger=OutcomeLedger(self.root / "outcomes.jsonl"),
                        discovered={"agy": {"gemini-4-argon-high": ["high"], "claude-opus-5-5-high": ["high"]}})
        self.assertTrue(any(row.provider == "agy" and "claude-opus" in row.model for row in result.candidates))
        self.assertFalse(any(row.provider == "agy" and "gemini-4-argon" in row.model for row in result.candidates))
        self.assertTrue(any(provider == "agy" and "部分模型受限" in reason for provider, reason in result.skipped))

    def test_concurrent_model_refusals_preserve_each_observation(self):
        with ThreadPoolExecutor(max_workers=4) as pool:
            list(pool.map(lambda i: health.record_engine_limit("agy", "fixture " + str(i), self.reset,
                                                               model="gemini-fixture-" + str(i)), range(8)))
        data = self.info("agy")
        self.assertEqual(len(data["quota_limits"]), 8)
        for i in range(8):
            self.assertIsNotNone(model_quota.quota_reason("agy", "gemini-fixture-" + str(i), quota=False, info=data))

    def test_other_account_does_not_inherit_refusal(self):
        health.record_engine_limit("codex", "fixture refusal", self.reset, model="gpt-6.1-sol")
        self.identity.update(account_ref="fixture-account-B", pool_ref="fixture-pool-B")
        self.assertIsNone(model_quota.quota_reason("codex", "gpt-6.1-sol", quota=False, info=self.info("codex")))

    def test_new_account_write_does_not_erase_previous_account(self):
        health.record_engine_limit("codex", "fixture refusal A", self.reset, model="gpt-6.1-sol")
        self.identity.update(account_ref="fixture-account-B", pool_ref="fixture-pool-B")
        health.record_engine_limit("codex", "fixture refusal B", self.reset, model="gpt-6-luna")
        self.identity.update(account_ref="fixture-account-A", pool_ref="fixture-pool-A")
        self.assertIn("refusal A", model_quota.quota_reason("codex", "gpt-6.1-sol", quota=False, info=self.info("codex")))
        self.assertEqual(len(self.info("codex")["quota_limits"]), 2)

    def test_reset_is_unknown_without_new_success(self):
        before = {"agy": {"status": "limited", "reason": "fixture refusal",
                           "resets_at": (datetime.now() - timedelta(minutes=1)).isoformat(),
                           "updated_at": (datetime.now() - timedelta(hours=1)).isoformat()}}
        after = health._sanitize_cache(before)["agy"]
        self.assertEqual(after["status"], "unknown")
        self.assertIs(after["verified"], False)

    def test_unknown_quota_never_becomes_local_percentage(self):
        with patch("makewand.usage.get_predictive_pacing_status", return_value={}):
            result = health.calculate_provider_quota("grok", {"status": "healthy"})
        self.assertIsNone(result["percentage"])
        self.assertEqual(result["source"], "unknown")

    def test_unbound_percentage_text_never_becomes_current_official_quota(self):
        from makewand.pacing import calculate_dynamic_pacing, PACING_LIMITED
        info = {"status": "healthy", "reason": "old account: 0% left", "resets_at": self.reset,
                "updated_at": datetime.now().isoformat()}
        for provider in ("codex", "claude", "grok", "muse"):
            with self.subTest(provider=provider):
                quota = health.calculate_provider_quota(provider, info)
                self.assertEqual(quota["source"], "unknown")
                self.assertIsNone(quota["percentage"])
                self.assertNotEqual(calculate_dynamic_pacing(provider, info)["pacing_state"], PACING_LIMITED)

    def quota(self, model, used, minutes=300):
        return dict(self.identity, windows=[{"model": model, "used_percent": used,
                                            "window_minutes": minutes, "pool_ref": self.identity["pool_ref"],
                                            "observed_at": datetime.now().astimezone().isoformat(),
                                            "resets_at": self.reset}])

    def test_short_and_month_windows_block(self):
        for minutes in (300, 43200):
            self.assertIsNotNone(model_quota.quota_reason("codex", "gpt-6.1-sol",
                quota=self.quota("", 100, minutes), info={}))

    def test_fable_sublimit_does_not_block_opus(self):
        quota = self.quota("Fable", 100, 10080)
        self.assertIsNotNone(model_quota.quota_reason("claude", "claude-fable-5-1[1m]", quota=quota, info={}))
        self.assertIsNone(model_quota.quota_reason("claude", "claude-opus-5-5", quota=quota, info={}))

    def test_claude_family_alias_and_long_context_alias_share_refusal(self):
        health.record_engine_limit("claude", "fixture alias refusal", self.reset, model="opus[1m]")
        for model in ("opus", "opus[1m]", "claude-opus-5-5", "claude-opus-5-5[1m]"):
            self.assertIsNotNone(model_quota.quota_reason("claude", model, quota=False, info=self.info("claude")))
        self.assertIsNone(model_quota.quota_reason("claude", "sonnet", quota=False, info=self.info("claude")))

    def test_account_shared_window_still_blocks_all_models(self):
        for model in ("claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1"):
            self.assertIsNotNone(model_quota.quota_reason("claude", model, quota=self.quota("", 100), info={}))

    def test_stale_foreign_pool_and_opaque_windows_never_block(self):
        cases = [{"observed_at": (datetime.now().astimezone() - timedelta(hours=1)).isoformat()},
                 {"observed_at": (datetime.now().astimezone() + timedelta(hours=1)).isoformat()},
                 {"resets_at": (datetime.now().astimezone() - timedelta(seconds=1)).isoformat()},
                 {"pool_ref": "other-pool"}, {"scope": "unknown", "limit_id": "opaque"}]
        for fields in cases:
            with self.subTest(fields=fields):
                quota = self.quota("", 100)
                quota["windows"][0].update(fields)
                self.assertIsNone(model_quota.quota_reason("codex", "gpt-6.1-sol", quota=quota, info={}))

    def test_cached_refusal_before_transport_does_not_spend_budget(self):
        result, attempt = self.execute_fixture(lambda request: model_quota.before_cli_launch("agy", request.model), limited=True)
        self.assertEqual(result.error_kind, "quota_preflight")
        self.assertEqual(attempt["status"], "released")
        self.assertEqual(attempt["resolved_model"], "gemini-4-argon-high")

    def test_generic_failure_still_spends_budget(self):
        result, attempt = self.execute_fixture(lambda request: (False, None, "fixture failure"))
        self.assertEqual(result.status, "FAILED")
        self.assertNotEqual(attempt["status"], "released")

    def test_refusal_after_transport_claim_cannot_refund_budget(self):
        from makewand.execution_runtime import mark_provider_invocation
        def callback(request):
            mark_provider_invocation()
            return model_quota.refuse_before_launch("fixture actual refusal")
        result, attempt = self.execute_fixture(callback)
        self.assertEqual(result.status, "FAILED")
        self.assertNotEqual(attempt["status"], "released")

    def execute_fixture(self, callback, limited=False):
        from makewand.execution_contract import ExecutionRequest
        from makewand.execution_runtime import execute
        budget = self.root / "budget.json"
        if limited:
            health.record_engine_limit("agy", "fixture refusal", self.reset, model="gemini-4-argon-high")
        env = {"MAKEWAND_CALL_BUDGET_FILE": str(budget), "MAKEWAND_MAX_MODEL_CALLS": "3",
               "MAKEWAND_EXECUTION_EVIDENCE_DIR": str(self.root / "evidence"),
               "MAKEWAND_EXECUTION_EVENTS_FILE": str(self.root / "events.jsonl"),
               "MAKEWAND_TASK_ID": "offline-quota-refusal", "MAKEWAND_LAYERED_BUDGET": "0"}
        with patch.dict(os.environ, env), patch("makewand.layered_budget.default_path", return_value=budget):
            request = ExecutionRequest(task_id=env["MAKEWAND_TASK_ID"], stage="review", engine="agy",
                                       model="gemini-4-argon-high", cwd=str(self.root), readonly=True,
                                       timeout_ms=10000, budget_file=str(budget), max_model_calls=3)
            result = execute(request, lambda timeout: callback(request))
        attempt = json.loads(budget.read_text())["attempts"][0]
        return result, attempt


if __name__ == "__main__":
    unittest.main()
