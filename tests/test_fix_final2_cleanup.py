"""Cleanups of the Python orchestrator and health code (py-cleanup-a).

(a) health.cli_messages had no caller and CLI_TRANSCRIPT_PROVIDERS only a test:
    both are gone.
(b) usage.record_engine_usage(route=) was never passed, so the ledger never said
    "api" and health's "route == 'api'" branch was dead: the claude and codex
    adapters now report the route they take (cli or api) to the dispatch site
    (orchestrator.dispatch_task, the direct CLI commands), which records it.
(c) health's comment claimed Go compiles catalog patterns with (?i); it does not.
(d) `status --probe` help now names the agy quota-limit and probe-recorded triggers.
(e) orchestrator's module-level print shadowed the builtin: it is _print now.
(f) _utf8_size existed twice with different surrogate handling: one definition,
    review_contract.dispatched_size, measures text as a provider receives it.
(g) JSON surrogate escaping was written inline twice next to git_helper._json_text.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import argparse
import contextlib
import inspect
import io
import json
import os
import tempfile
import unittest
from datetime import datetime, timedelta
from types import SimpleNamespace
from unittest.mock import patch

from makewand import cli, git_helper, health, orchestrator as orch, review_contract, review_verdict, usage
from makewand.providers import local
from makewand.providers.claude import execute_claude_task


@contextlib.contextmanager
def fake_cli(calls):
    @contextlib.contextmanager
    def launch(name, cmd, cwd, **kwargs):
        yield SimpleNamespace(cmd=cmd, pass_fds=(), env={})

    def run(cmd, **kwargs):
        calls.append(("cli", cmd[0]))
        return 0, "cli answer", "", None

    with patch("makewand.sandbox.provider_sandbox_launch", side_effect=launch), \
         patch("makewand.providers.claude.run_subprocess", side_effect=run), \
         patch("makewand.providers.codex.run_subprocess", side_effect=run):
        yield


class DeadCodeTests(unittest.TestCase):
    def test_unused_health_helpers_are_gone(self):
        self.assertFalse(hasattr(health, "cli_messages"))
        self.assertFalse(hasattr(health, "CLI_TRANSCRIPT_PROVIDERS"))

    def test_the_go_regexp_comment_matches_router_cli_go(self):
        source = inspect.getsource(health)
        self.assertNotIn("regexp.MustCompile(`(?i)^(?:`", source)
        self.assertIn("cliCatalogRegexp", source)

    def test_orchestrator_does_not_shadow_print(self):
        self.assertNotIn("print", vars(orch))
        self.assertTrue(callable(orch._print))


class UsageRouteTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-final2-route-")
        self.addCleanup(temporary.cleanup)
        ledger = Path(temporary.name) / "usage.json"
        patcher = patch.dict(os.environ, {"MAKEWAND_USAGE_FILE": str(ledger)})
        patcher.start()
        self.addCleanup(patcher.stop)

    def dispatch(self, status, policy=None):
        calls = []
        cache = {"claude": {"status": status, "reason": "login expired", "source": "dispatch",
                            "updated_at": (datetime.now() - timedelta(minutes=1)).isoformat()}}
        env = {"ANTHROPIC_API_KEY": "sk-test-not-a-real-key"}
        if policy:
            env["MAKEWAND_API_POLICY"] = policy
        with patch.dict(os.environ, env), \
             patch("makewand.health.load_status_cache", return_value=cache), \
             patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.providers.api_client.call_api_chat",
                   side_effect=lambda **kwargs: calls.append(("api", kwargs["provider"])) or (True, "api answer", None)), \
             fake_cli(calls), contextlib.redirect_stderr(io.StringIO()):
            result = orch.dispatch_task("claude", "解释这段代码", cwd=tempfile.gettempdir(), readonly=True)
        return calls, result

    def test_the_ledger_records_the_route_the_adapter_took(self):
        calls, result = self.dispatch("healthy")
        self.assertEqual((calls, result[0]), ([("cli", "claude")], True))
        self.assertEqual(usage.get_dispatch_records("claude")[-1]["route"], "cli")
        calls, result = self.dispatch("needs_auth", policy="allow_paid")
        self.assertEqual((calls, result[0]), ([("api", "claude")], True))
        self.assertEqual(usage.get_dispatch_records("claude")[-1]["route"], "api")

    def test_an_api_served_success_never_clears_a_cli_login_failure_but_a_cli_one_does(self):
        health.record_engine_failure("claude", "needs_auth", "未登录")
        self.dispatch("needs_auth", policy="allow_paid")  # relayed: "route": "api"
        with patch.dict(os.environ, {"ANTHROPIC_API_KEY": "sk-test-not-a-real-key"}):
            self.assertEqual(health.load_status_cache()["claude"]["status"], "needs_auth")
        # Also once the key is gone: the entry says the API served it (health's "api" branch).
        self.assertEqual(health.load_status_cache()["claude"]["status"], "needs_auth")
        self.dispatch("needs_auth")  # the policy forbids the relay: the CLI runs, "route": "cli"
        with patch.dict(os.environ, {"ANTHROPIC_API_KEY": "sk-test-not-a-real-key"}):
            self.assertEqual(health.load_status_cache()["claude"]["status"], "healthy")

    def test_direct_commands_record_the_route_too(self):
        calls = []
        with patch("makewand.health.load_status_cache", return_value={}), \
             patch("makewand.config.has_subscription_configured", return_value=True), \
             fake_cli(calls), contextlib.redirect_stderr(io.StringIO()):
            result = cli._execute_direct_task("claude", execute_claude_task, "解释这段代码", cwd=tempfile.gettempdir(),
                                              timeout=60, tier="standard", readonly=True)
        self.assertTrue(result[0])
        self.assertEqual(usage.get_dispatch_records("claude")[-1]["route"], "cli")


class ProbeHelpTests(unittest.TestCase):
    def test_status_probe_help_names_every_agy_trigger(self):
        out = io.StringIO()
        with patch.object(sys, "argv", ["makewand", "status", "--help"]), contextlib.redirect_stdout(out), \
                self.assertRaises(SystemExit):
            cli.main()
        text = " ".join(out.getvalue().split())
        self.assertIn("quota limit has not reset", text)
        self.assertIn("real agy dispatch or an earlier real probe recorded", text)


class SizeTests(unittest.TestCase):
    def test_one_definition_measures_text_as_dispatched(self):
        self.assertFalse(hasattr(orch, "_utf8_size"))
        self.assertFalse(hasattr(orch, "_prompt_size"))
        self.assertFalse(hasattr(local, "_utf8_size"))
        self.assertIs(orch.dispatched_size, review_contract.dispatched_size)
        self.assertIs(local.dispatched_size, review_contract.dispatched_size)
        size = review_contract.dispatched_size
        self.assertEqual(size("lat\udce9n.txt"), len("lat\\xe9n.txt"))  # an undecodable byte is sent as \xNN
        self.assertEqual(size("\ud800"), len("\\ud800"))  # a lone surrogate no file name produced
        self.assertEqual(size("数据"), 6)
        self.assertEqual(size(None), 0)
        self.assertEqual(local.ceiling_prompt_tokens("bad \udce9"), size("bad \udce9"))
        # Never less than a provider receives: unescaped, a lone surrogate arrives as U+FFFD.
        self.assertGreaterEqual(size("\udce9"), len("�".encode("utf-8")))

    def test_the_review_cap_measures_the_diff_a_reviewer_receives(self):
        diff = "diff --git a/data.txt b/data.txt\n+" + "\udce9" * 30 + "\n"
        self.assertLessEqual(len(diff.encode("utf-8", "surrogateescape")), 100)
        self.assertGreater(review_contract.dispatched_size(diff), 100)
        dispatched = []
        with patch.dict(os.environ, {"MAKEWAND_REVIEW_DIFF_MAX_BYTES": "100"}), \
             patch.object(orch, "get_git_diff", return_value=diff), \
             patch.object(orch, "get_or_update_status", return_value={}), \
             patch.object(orch, "dispatch_task", side_effect=lambda *a, **k: dispatched.append(a[0])), \
             contextlib.redirect_stdout(io.StringIO()) as out:
            code = orch.run_review(cwd=tempfile.gettempdir())
        self.assertEqual(code, orch.EXIT_UNVERIFIED)
        self.assertEqual(dispatched, [])
        self.assertIn("超过复审上限 100 字节", out.getvalue())


class JsonTextTests(unittest.TestCase):
    def test_manifests_use_the_one_escaping_helper(self):
        self.assertIs(orch._json_text, git_helper._json_text)
        self.assertNotIn("ensure_ascii=True", inspect.getsource(orch))
        self.assertNotIn("ensure_ascii=True", inspect.getsource(review_verdict))
        self.assertIn("_json_text(manifest)", inspect.getsource(review_verdict._save_unverified_artifacts))
        manifest = {"patches": {"sub\udce9": "unverified.submodule-0.patch"}}
        self.assertEqual(json.loads(git_helper._json_text(manifest)), manifest)


if __name__ == "__main__":
    unittest.main()
