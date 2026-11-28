"""A CLI that says it cannot use the requested model ends a dispatch as a known failure.

Grok Build printed "Couldn't set model 'grok-4.7': Invalid params: "unknown model id". Run 'grok
models' to see available models." twice on 10-30, recorded as UNKNOWN (exit 17). It had sent no
prompt. Both times Muse and Codex could not reach their model catalogs in the same minutes, so
the message can mean a failed fetch of the model list as well as a model that is gone: the
failure is known, but no limit is recorded. Claude Code prints "There's an issue with the selected
model (<id>). It may not exist or you may not have access to it." when the API answers that the
model is not found (the text is in its 2.1.332 bundle, error model_not_found): known failure,
recorded for that model.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.health as health
from makewand.health import cli_model_refusal

GROK = ("Couldn't set model 'grok-4.7': Invalid params: \"unknown model id\". "
        "Run 'grok models' to see available models.")
CLAUDE = ("There's an issue with the selected model (claude-opus-4-1). It may not exist or you may not have "
          "access to it. Run /model to pick a different model.")


class LinesTests(unittest.TestCase):
    def test_grok_names_the_model_with_or_without_its_error_marker(self):
        for stderr in (GROK + "\n", "Error: " + GROK + "\n", GROK + "\nError: " + GROK + "\n"):
            model, line = cli_model_refusal("grok", "", stderr)
            self.assertEqual("grok-4.7", model)
            self.assertIn("Couldn't set model", line)
        self.assertEqual("grok-4.7", cli_model_refusal("grok", GROK + "\n", None)[0])

    def test_claude_names_the_model_whatever_the_apostrophe(self):
        for text in (CLAUDE, CLAUDE.replace("'", "’"), "Error: " + CLAUDE):
            self.assertEqual("claude-opus-4-1", cli_model_refusal("claude", text + "\n", "")[0])
            self.assertEqual("claude-opus-4-1", cli_model_refusal("claude", "", text + "\n")[0])

    def test_the_same_words_in_an_answer_or_in_other_messages_are_not_a_refusal(self):
        self.assertIsNone(cli_model_refusal("grok", "The CLI printed:\n" + GROK + "\n", ""))
        self.assertIsNone(cli_model_refusal("grok", "", "note: " + GROK + "\n"))
        self.assertIsNone(cli_model_refusal("grok", "", "Couldn't set model 'grok-4.7': timed out\n"))
        self.assertIsNone(cli_model_refusal("claude", "", "I saw: " + CLAUDE + "\n"))
        self.assertIsNone(cli_model_refusal("claude", "", "There's an issue with the selected model (x).\n"))
        self.assertIsNone(cli_model_refusal("codex", "", GROK + "\n"))  # no "model" kind there
        self.assertIsNone(cli_model_refusal("unknown-cli", "", GROK + "\n"))


class DispatchTests(unittest.TestCase):
    def setUp(self):
        root = tempfile.TemporaryDirectory(prefix="mw-model-refusal-")
        self.addCleanup(root.cleanup)
        self.root = Path(root.name)
        cache = patch.object(health, "STATUS_CACHE_FILE", self.root / "status.json")
        cache.start()
        self.addCleanup(cache.stop)

    def dispatch(self, engine, model, stderr):
        from makewand.providers import claude, grok
        module = {"claude": claude, "grok": grok}[engine]
        with patch("makewand.config.has_subscription_configured", return_value=True), \
                patch("makewand.config.has_api_configured", return_value=False), \
                patch.dict(os.environ, {"MAKEWAND_API_POLICY": "subscription_only"}), \
                patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(True, "config-ack")), \
                patch("makewand.sandbox.audit_unsafe_host_exec"), \
                patch("makewand.cli_failure_log.record") as unclassified, \
                patch.object(module, "run_subprocess", return_value=(1, "", stderr, None)), \
                contextlib.redirect_stderr(io.StringIO()):
            result = getattr(module, f"execute_{engine}_task")("review this", cwd=str(self.root), model=model,
                                                               readonly=True)
        return result, unclassified

    def rows(self, engine):
        return health.load_status_cache().get(engine, {}).get("quota_limits") or []

    def test_grok_ends_known_without_a_record(self):
        result, unclassified = self.dispatch("grok", "grok-4.7", "Error: " + GROK + "\n")
        self.assertIs(type(result), tuple)  # a plain failure: FAILED, outcome known
        self.assertFalse(result[0])
        self.assertIn("没能选用型号 grok-4.7", result[2])
        self.assertIn("没有发出提示词", result[2])
        unclassified.assert_not_called()
        self.assertEqual([], self.rows("grok"))

    def test_claude_ends_known_and_records_that_model_only(self):
        result, unclassified = self.dispatch("claude", "claude-opus-4-1", CLAUDE + "\n")
        self.assertIs(type(result), tuple)
        self.assertFalse(result[0])
        self.assertIn("claude-opus-4-1", result[2])
        self.assertIn("拒绝了这次调用", result[2])
        unclassified.assert_not_called()
        rows = self.rows("claude")
        self.assertEqual(["claude-opus-4-1"], [row["quota_scope"]["model"] for row in rows])
        from makewand.model_quota import projected_status
        info = health.load_status_cache().get("claude", {})
        self.assertEqual("limited", projected_status("claude", info, "claude-opus-4-1").get("status"))
        self.assertNotEqual("limited", projected_status("claude", info, "claude-sonnet-5-5").get("status"))

    def test_other_failures_stay_unknown(self):
        result, unclassified = self.dispatch("grok", "grok-4.7", "Error: stream closed before the turn finished\n")
        self.assertEqual("UNKNOWN", result.status)
        unclassified.assert_called_once()
        result, unclassified = self.dispatch("claude", "claude-opus-4-1", "Error: something else\n")
        self.assertEqual("UNKNOWN", result.status)
        self.assertEqual([], self.rows("claude"))


if __name__ == "__main__":
    unittest.main()
