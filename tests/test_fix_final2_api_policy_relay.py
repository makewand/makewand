"""An API key relays work only when the API policy allows paid calls (api-policy-relay).

The claude and codex adapters relayed a logged-out (needs_auth) CLI to the API
whenever a key was configured, and engine selection and the ensemble counted
such an engine as usable. Under the default subscription_only policy
call_api_chat then refuses: anyone with ANTHROPIC_API_KEY or OPENAI_API_KEY in
the environment and a stale needs_auth verdict never ran the CLI for 30
minutes, was told to enable paid API use, and the "No usable execution
provider" guard (exit 11) never fired. Every relay site now asks one helper,
config.api_relay_available (a key AND an allowed policy): without it the CLI
runs, or the guard fires when nothing else is usable. Providers are fakes.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import os
import tempfile
import unittest
from datetime import datetime, timedelta
from types import SimpleNamespace
from unittest.mock import patch

from makewand import config, engine_selection, health, orchestrator as orch
from makewand.ensemble import resolve_ensemble_providers
from makewand.git_helper import run_git_cmd
from makewand.providers.claude import execute_claude_task
from makewand.providers.codex import execute_codex_task
from makewand.providers.agy import execute_agy_task
from makewand.providers.grok import execute_grok_task
from makewand.providers.muse import execute_muse_task

ADAPTERS = {"claude": (execute_claude_task, "ANTHROPIC_API_KEY"), "codex": (execute_codex_task, "OPENAI_API_KEY"),
            "agy": (execute_agy_task, "GEMINI_API_KEY"), "grok": (execute_grok_task, "XAI_API_KEY"),
            "muse": (execute_muse_task, "META_API_KEY")}


def live(status, reason=""):
    return {"status": status, "reason": reason, "updated_at": (datetime.now() - timedelta(minutes=1)).isoformat()}


def environment(key=None, policy=None):
    values = {"MAKEWAND_API_POLICY": policy} if policy else {}
    if key:
        values[key] = "sk-test-not-a-real-key"
    return patch.dict(os.environ, values)


class HelperTests(unittest.TestCase):
    def test_a_key_relays_only_under_an_allowing_policy(self):
        for key, policy, expected in ((None, None, False), ("ANTHROPIC_API_KEY", None, False),
                                      ("ANTHROPIC_API_KEY", "subscription_only", False),
                                      ("ANTHROPIC_API_KEY", "allow_paid", True), (None, "allow_paid", False)):
            with self.subTest(key=key, policy=policy), environment(key, policy):
                self.assertIs(config.api_relay_available("claude"), expected)
                self.assertEqual(bool(config.api_relay_blocked_note("claude")),
                                 bool(key) and policy != "allow_paid")


@contextlib.contextmanager
def fake_cli(calls, code=0, out="cli answer", err=""):
    """The provider CLI: the sandbox launch and the process are fakes that record the call."""
    @contextlib.contextmanager
    def launch(name, cmd, cwd, **kwargs):
        yield SimpleNamespace(cmd=cmd, pass_fds=(), env={})

    def run(cmd, **kwargs):
        calls.append(("cli", cmd[0]))
        return code, out, err, None

    with patch("makewand.sandbox.provider_sandbox_launch", side_effect=launch), \
         patch("makewand.providers.muse.detect_muse_guard", return_value=(False, None)), \
         patch("makewand.providers.muse.get_muse_executable", return_value=("muse", [])), \
         patch("makewand.providers.claude.run_subprocess", side_effect=run), \
         patch("makewand.providers.codex.run_subprocess", side_effect=run), \
         patch("makewand.providers.agy.run_subprocess", side_effect=run), \
         patch("makewand.providers.grok.run_subprocess", side_effect=run), \
         patch("makewand.providers.muse.run_subprocess", side_effect=run):
        yield


class AdapterTests(unittest.TestCase):
    def dispatch(self, engine, status, key, policy, subscribed=True, cli=None, cwd=tempfile.gettempdir()):
        execute, key_env = ADAPTERS[engine]
        calls = []
        self.routes = []
        self.api_requests = []

        def api(**kwargs):
            self.api_requests.append(kwargs)
            calls.append(("api", kwargs.get("provider")))
            return True, "api answer", None

        with environment(key_env if key else None, policy), \
             patch("makewand.health.load_status_cache", return_value={engine: live(status, "login expired")}), \
             patch("makewand.config.has_subscription_configured", return_value=subscribed), \
             patch("makewand.providers.api_client.call_api_chat", side_effect=api), \
             patch("makewand.usage.note_dispatch_route", side_effect=self.routes.append), \
             fake_cli(calls, **(cli or {})), contextlib.redirect_stderr(io.StringIO()):
            result = execute("审查这段代码", readonly=True, cwd=cwd)
        return calls, tuple(result)

    def test_logged_out_cli_with_a_key_the_policy_forbids_runs_the_cli(self):
        for engine in ADAPTERS:
            with self.subTest(engine=engine):
                calls, result = self.dispatch(engine, "needs_auth", key=True, policy=None)
                self.assertEqual(calls, [("cli", engine)])
                self.assertEqual(result[:2], (True, "cli answer"))

    def test_logged_out_cli_with_a_key_and_paid_api_allowed_is_relayed(self):
        for engine in ADAPTERS:
            with self.subTest(engine=engine):
                calls, result = self.dispatch(engine, "needs_auth", key=True, policy="allow_paid")
                self.assertEqual(calls, [("api", engine)])
                self.assertEqual(result[:2], (True, "api answer"))

    def test_post_cli_relay_keeps_the_normalized_working_directory(self):
        for engine in ADAPTERS:
            with self.subTest(engine=engine), \
                    patch(f'makewand.providers.{engine}.parse_{engine}_quota', return_value=(True, 'fixture quota', None)), \
                    patch('makewand.health.record_engine_limit'):
                calls, result = self.dispatch(engine, 'healthy', key=True, policy='allow_paid', cwd=None,
                                               cli={'code': 1, 'out': '', 'err': 'fixture quota'})
                self.assertEqual(calls, [('cli', engine), ('api', engine)])
                self.assertEqual(self.api_requests[0]['cwd'], os.getcwd())
                self.assertTrue(result[0])

    def test_without_a_key_the_cli_runs_under_either_policy(self):
        for engine in ADAPTERS:
            for policy in (None, "allow_paid"):
                with self.subTest(engine=engine, policy=policy):
                    calls, _ = self.dispatch(engine, "needs_auth", key=False, policy=policy)
                    self.assertEqual(calls, [("cli", engine)])

    def test_limited_or_missing_cli_with_a_forbidden_key_says_the_policy_blocks_the_relay(self):
        for engine in ADAPTERS:
            for status, subscribed in (("limited", True), ("healthy", False)):
                with self.subTest(engine=engine, status=status, subscribed=subscribed):
                    calls, result = self.dispatch(engine, status, key=True, policy=None, subscribed=subscribed)
                    self.assertEqual(calls, [])
                    self.assertFalse(result[0])
                    self.assertIn("subscription_only", result[2])
                    self.assertIn("MAKEWAND_API_POLICY=allow_paid", result[2])

    def test_a_quota_refusal_is_relayed_only_under_an_allowing_policy(self):
        limit = {"code": 1, "out": "", "err": "Claude AI usage limit reached|1999999999\n"}
        codex_limit = {"code": 1, "out": "", "err": "ERROR: You've hit your usage limit. Try again in 3 hours.\n"}
        limits = {"claude": limit, "codex": codex_limit,
                  "agy": {"code": 1, "out": "", "err": "Error: You have exhausted your quota on this model.\n"},
                  "grok": {"code": 1, "out": "", "err": "API Error (status 429)\n"},
                  "muse": {"code": 1, "out": "", "err": "Error: Rate limited (HTTP 429 · attempt 1)\n"}}
        for engine, cli in limits.items():
            with self.subTest(engine=engine), patch("makewand.health.record_engine_limit"):
                calls, result = self.dispatch(engine, "healthy", key=True, policy=None, cli=cli)
                self.assertEqual(calls, [("cli", engine)])
                self.assertIn("subscription_only", result[2])
                calls, result = self.dispatch(engine, "healthy", key=True, policy="allow_paid", cli=cli)
                self.assertEqual(calls, [("cli", engine), ("api", engine)])
                self.assertEqual(self.routes, ["cli", "api"])


class SelectionTests(unittest.TestCase):
    def route(self, cache, policy=None):
        with environment("ANTHROPIC_API_KEY", policy), \
             patch("makewand.config.get_active_providers", return_value=["claude"]), \
             patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
             patch("makewand.usage.get_engine_usage_stats", return_value={}), \
             patch("makewand.usage.get_all_engine_reliability", return_value={}):
            return engine_selection.select_optimal_engine_pair("编写Python脚本", cache=cache)

    def test_logged_out_engine_with_a_forbidden_key_is_not_usable_and_the_guard_fires(self):
        cache = {"claude": live("needs_auth", "login expired")}
        with environment("ANTHROPIC_API_KEY"):
            self.assertFalse(engine_selection._usable_or_api_relay("claude", cache))
        coders, _, meta = self.route(cache)
        self.assertEqual(coders, [])
        self.assertTrue(meta["no_usable_engines"])
        self.assertTrue(engine_selection._no_provider_detected(None, meta))

    def test_logged_out_engine_with_an_allowed_relay_stays_usable(self):
        cache = {"claude": live("needs_auth", "login expired")}
        with environment("ANTHROPIC_API_KEY", "allow_paid"):
            self.assertTrue(engine_selection._usable_or_api_relay("claude", cache))
        coders, _, meta = self.route(cache, "allow_paid")
        self.assertEqual(coders, ["claude"])
        self.assertFalse(engine_selection._no_provider_detected(None, meta))

    def test_ensemble_counts_a_relay_only_under_an_allowing_policy(self):
        cache = {"claude": live("needs_auth", "login expired"), "codex": live("healthy")}
        for policy, expected in ((None, ["codex"]), ("allow_paid", ["claude", "codex"])):
            unavailable = {}
            with self.subTest(policy=policy), environment("ANTHROPIC_API_KEY", policy), \
                 patch("makewand.health.load_status_cache", return_value=cache), \
                 patch("makewand.ensemble.has_subscription_configured", return_value=True):
                self.assertEqual(resolve_ensemble_providers(["claude", "codex"], unavailable=unavailable), expected)
            if policy is None:
                self.assertIn("subscription_only", unavailable["claude"])

    def test_probe_reports_a_missing_cli_with_a_forbidden_key_as_missing(self):
        for policy, status in ((None, "missing"), ("allow_paid", "healthy")):
            with self.subTest(policy=policy), environment("ANTHROPIC_API_KEY", policy), \
                 patch("makewand.config.has_subscription_configured", return_value=False):
                entry = health.probe_model("claude")
            self.assertEqual(entry["status"], status)
            if status == "missing":
                self.assertIn("subscription_only", entry["reason"])


class WorkflowTests(unittest.TestCase):
    def test_explain_with_a_stale_login_failure_and_a_forbidden_key_stops_with_exit_11(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-final2-relay-")
        self.addCleanup(temporary.cleanup)
        repo = Path(temporary.name) / "repo"
        repo.mkdir()
        (repo / "app.py").write_text("VALUE = 1\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            self.assertEqual(run_git_cmd(["git", *arguments], cwd=str(repo))[0], 0)
        cache = {"claude": live("needs_auth", "login expired"), "codex": live("needs_auth", "login expired")}
        calls = []
        with environment("ANTHROPIC_API_KEY"), patch.dict(os.environ, {"OPENAI_API_KEY": "sk-test-not-a-real-key"}), \
             patch.object(orch, "get_or_update_status", return_value=cache), \
             patch("makewand.health.load_status_cache", return_value=cache), \
             patch.object(orch, "check_load_backpressure", return_value=True), \
             patch("makewand.config.get_active_providers", return_value=["claude", "codex"]), \
             patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
             patch("makewand.usage.get_engine_usage_stats", return_value={}), \
             patch("makewand.usage.get_all_engine_reliability", return_value={}), \
             patch.object(orch, "classify_prompt_intent", return_value="explain"), \
             patch.object(orch, "dispatch_task", side_effect=lambda engine, *a, **k: calls.append(engine)), \
             contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            result = orch.run_workflow("解释 VALUE 的含义", cwd=str(repo), total_timeout=60)
        self.assertEqual(calls, [], "the relay used to count the logged-out engines as usable")
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertTrue(result.error.startswith("No usable execution provider: "), result.error)


if __name__ == "__main__":
    unittest.main()
