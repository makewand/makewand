"""Static login and quota samples; no real credentials, CLI or network."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand.account_identity import account_identity, account_ref, event_account_ref
from makewand.execution_runtime import account_reference
from makewand.health import _get_official_subscription_quota


class AccountQuotaTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.first = self.home / ".codex"
        self.second = self.home / "second"
        self.env = patch.dict(os.environ, HOME=str(self.home), CODEX_HOME=str(self.first))
        self.env.start()
        self.addCleanup(self.env.stop)

    def login(self, root, id):
        root.mkdir(parents=True, exist_ok=True)
        (root / "auth.json").write_text(json.dumps({"tokens": {"account_id": id,
            "access_token": "fake-secret-never-use", "id_token": "fake-jwt-never-decode"}}))

    def snapshot(self, root=None, *, age=0, version=2, entries=None):
        identity = account_identity("codex", root or self.first)
        entry = {"Provider": "codex", "IdentityKnown": identity["identity_known"],
                 "AccountRef": identity["account_ref"], "CredentialRoot": identity["credential_root"],
                 "PoolRef": identity["pool_ref"], "HasData": True,
                 "SourceAt": time.time() - age, "WeeklyPct": 70,
                 "WeeklyResetAt": time.time() + 3600}
        path = self.home / ".cache/makewand/quota-snapshot.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps({"version": version, "taken_at": time.time(), "providers": entries or [entry]}))
        return entry

    def events(self, events):
        path = self.first / "sessions/events.jsonl"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("\n".join(json.dumps(event) for event in events))

    def event(self, id, used, minutes=300, age=0, model=""):
        return {"account_id": id, "timestamp": time.time() - age,
                "payload": {"rate_limits": {"model": model, "primary": {
                    "used_percent": used, "window_minutes": minutes, "resets_at": time.time() + 3600}}}}

    def test_claude_selected_directory_is_the_private_credential_source(self):
        from makewand.sandbox import ProviderHome, SandboxConfigError
        default = self.home / ".claude"
        selected = self.home / "claude-B"
        for root, name in ((default, "A"), (selected, "B")):
            root.mkdir()
            (root / ".credentials.json").write_text(json.dumps({"fixture": name}))
            (root / ".claude.json").write_text(json.dumps({"oauthAccount": {"accountUuid": name}}))
        (self.home / ".claude.json").write_text((default / ".claude.json").read_text())
        with patch.dict(os.environ, CLAUDE_CONFIG_DIR=str(selected)):
            home = ProviderHome("claude", user_home=str(self.home))
            try:
                home.materialize()
                self.assertEqual(account_reference("claude"), account_ref("claude", "B"))
                self.assertEqual([c.host_path for c in home.credentials], [str(selected / ".credentials.json")])
                private = Path(home.credentials[0].private_root)
                self.assertEqual(json.loads((private / ".credentials.json").read_text())["fixture"], "B")
                self.assertEqual(account_identity("claude", private)["account_ref"], account_ref("claude", "B"))
            finally:
                home.finalize(write_back=False)
        with patch.dict(os.environ, CLAUDE_CONFIG_DIR=str(self.home / "missing")):
            with self.assertRaises(SandboxConfigError):
                ProviderHome("claude", user_home=str(self.home)).materialize()

    def test_same_account_different_roots_shares_pool_and_runtime_reference(self):
        self.login(self.first, "A")
        self.login(self.second, "A")
        self.snapshot()
        first_ref = account_reference("codex")
        with patch.dict(os.environ, CODEX_HOME=str(self.second)):
            quota = _get_official_subscription_quota("codex")
            self.assertEqual(quota["percentage"], 30)
            self.assertEqual(quota["credential_root"], str(self.second))
            self.assertEqual(first_ref, account_reference("codex"))

    def test_account_switch_rejects_cached_and_interleaved_session_evidence(self):
        self.login(self.first, "A")
        self.snapshot()
        self.events([self.event("A", 1)])
        self.login(self.first, "B")
        self.assertEqual(_get_official_subscription_quota("codex")["status"], "unknown")
        self.events([self.event("B", 44), self.event("A", 1)])
        self.assertEqual(_get_official_subscription_quota("codex")["percentage"], 56)

    def test_quota_gate_does_not_survive_account_switch_or_expiry(self):
        from makewand.health import _sanitize_cache
        for change in ("account", "expiry"):
            with self.subTest(change=change):
                self.login(self.first, "A")
                entry = self.snapshot()
                entry["WeeklyPct"] = 99
                self.snapshot(entries=[entry])
                cache = {"codex": {"status": "unknown"}}
                _sanitize_cache(cache)
                self.assertEqual(cache["codex"]["status"], "limited")
                if change == "account":
                    self.login(self.first, "B")
                else:
                    entry["SourceAt"] = time.time() - 1800
                    self.snapshot(entries=[entry])
                _sanitize_cache(cache)
                self.assertEqual(cache["codex"]["status"], "unknown")
        # A CLI refusal has its own reset semantics and must remain limited.
        cache = {"codex": {"status": "limited", "reason": "CLI quota refusal",
                           "resets_at": "2099-01-01T00:00:00", "updated_at": "2026-10-16T00:00:00"}}
        self.assertEqual(_sanitize_cache(cache)["codex"]["status"], "limited")

    def test_multiplied_accounts_do_not_overwrite_or_mix_pools(self):
        self.login(self.first, "A")
        a = self.snapshot()
        self.login(self.second, "B")
        b = self.snapshot(self.second)
        b["WeeklyPct"] = 99
        self.snapshot(entries=[b, a])
        self.assertEqual(_get_official_subscription_quota("codex")["percentage"], 30)
        with patch.dict(os.environ, CODEX_HOME=str(self.second)):
            self.assertEqual(_get_official_subscription_quota("codex")["percentage"], 1)

    def test_default_root_rejects_old_unbound_snapshot_and_session(self):
        self.login(self.first, "A")
        self.snapshot(version=1)
        event = self.event("A", 1)
        del event["account_id"]
        self.events([event])
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["status"], "unknown")
        self.assertIn("unbound_account_identity", quota["unverified_reasons"])

    def test_unknown_roots_are_isolated_without_asserting_independent_pools(self):
        first = account_identity("codex", self.first)
        second = account_identity("codex", self.second)
        self.assertFalse(first["identity_known"])
        self.assertIsNone(first["pool_ref"])
        self.assertNotEqual(first["account_ref"], second["account_ref"])
        self.snapshot()
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["status"], "unknown")
        self.assertFalse(quota["identity_known"])

    def test_symlink_uses_canonical_root_and_account(self):
        self.login(self.first, "A")
        alias = self.home / "alias"
        alias.symlink_to(self.first, target_is_directory=True)
        self.assertEqual(account_identity("codex", self.first), account_identity("codex", alias))
        self.snapshot()
        with patch.dict(os.environ, CODEX_HOME=str(alias)):
            self.assertEqual(_get_official_subscription_quota("codex")["percentage"], 30)

    def test_stale_bound_snapshot_stays_unknown_after_touch(self):
        self.login(self.first, "A")
        self.snapshot(age=3600)
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["status"], "unknown")
        self.assertIn("stale_observation", quota["unverified_reasons"])

    def test_30_day_window_and_model_cap_are_distinct(self):
        self.login(self.first, "A")
        self.events([self.event("A", 10, 300), self.event("A", 20, 10080),
                     self.event("A", 70, 43200), self.event("A", 100, 10080, model="model-only")])
        quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["percentage"], 30)
        self.assertEqual(quota["window_minutes"], 43200)
        self.assertEqual(len(quota["windows"]), 4)
        self.assertTrue(any(w["model"] == "model-only" for w in quota["windows"]))

    def test_session_metadata_does_not_override_explicit_account(self):
        self.login(self.first, "A")
        event = self.event("A", 40)
        del event["account_id"]
        self.events([{"type": "session_meta", "payload": {"account_id": "A"}},
                     event, self.event("B", 1)])
        self.assertEqual(_get_official_subscription_quota("codex")["percentage"], 60)

    def test_claude_plain_uuid_and_missing_codex_id_never_decode_tokens(self):
        root = self.home / "custom-claude"
        root.mkdir()
        (root / ".claude.json").write_text(json.dumps({"oauthAccount": {"accountUuid": "claude-A"}}))
        with patch.dict(os.environ, CLAUDE_CONFIG_DIR=str(root)):
            self.assertEqual(account_reference("claude"), account_ref("claude", "claude-A"))
        self.login(self.first, "")
        identity = account_identity("codex")
        self.assertFalse(identity["identity_known"])
        self.assertNotIn("fake-secret", json.dumps(identity))

    def test_identity_projection_never_decodes_secret_fields(self):
        self.login(self.first, "A")
        loads = json.loads
        decoded = []
        def recording_loads(value, *args, **kwargs):
            decoded.append(value)
            return loads(value, *args, **kwargs)
        with patch("makewand.account_identity.json.loads", side_effect=recording_loads):
            identity = account_identity("codex")
        self.assertTrue(identity["identity_known"])
        self.assertFalse(any("fake-secret" in value or "fake-jwt" in value for value in decoded))
        for document in ('{"tokens":{"account_id":123}}',
                         '{"tokens":{"account_id":"A"}, "broken":}',
                         '{"tokens":{"access_token":{"account_id":"A"}}}'):
            (self.first / "auth.json").write_text(document)
            self.assertFalse(account_identity("codex")["identity_known"])

    def test_login_change_during_quota_read_invalidates_evidence(self):
        self.login(self.first, "A")
        self.events([self.event("A", 1)])
        def switch_login(record):
            self.login(self.first, "B")
            return event_account_ref(record)
        with patch("makewand.account_identity.event_account_ref", side_effect=switch_login):
            quota = _get_official_subscription_quota("codex")
        self.assertEqual(quota["status"], "unknown")
        self.assertIn("account_changed_during_read", quota["unverified_reasons"])


if __name__ == "__main__":
    unittest.main()
