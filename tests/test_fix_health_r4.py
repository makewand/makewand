"""
py-health, round 4: what each provider CLI prints itself, checked against the
installed binaries and bundles (makewand/cli_message_catalog.json).

1. muse-catalog: Muse Code 1.4.6 prints its failures only as "<kind> (HTTP
   <status> · attempt <n>)"; "rate limited (retry after" is its /feedback upload
   receipt and "Too Many Requests" its HTTP reason-phrase table. An answer or a
   tool's output ending in "HTTP 429 Too Many Requests", "429 Rate limit
   exceeded" or "rate limited (retry after 30s)" is not its verdict, and no
   reset time is read from it.
2. codex-events: codex 0.162.1 prints API errors in its ERROR events as
   "unexpected status <status>: <the API's message>, url: ..., request id: ..."
   and relays the API's own rate-limit and quota messages ("Rate limit reached
   for <model> in organization <org> ...", "You exceeded your current quota
   ..."), which count wherever its transcript prints them, as do the short
   forms the catalog lists. codex's main ends a failed run with "Error:
   <message>" (anyhow: a blank line, "Caused by:" and the indented chain): that
   report counts after the agent's messages or the token count, never inside a
   command's relayed output or the prompt echo. codex's own usage warnings
   (whole lines) trip the quota reserve gate wherever they appear outside the
   prompt echo; the same words inside a sentence of the answer do not.
3. catalog-misses: Claude Code's "Your organization is out of usage credits"
   and Gemini CLI's formats ("Usage limit reached for <model>.", "[API Error:
   You have exhausted your capacity/daily quota ...]", the JSON error report of
   --output-format json with "code": 429, the TerminalQuotaError report) are
   refusals; Gemini CLI has its own catalog entry.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
from makewand.providers import codex, muse
from makewand.providers.claude import parse_claude_quota
from makewand.providers.codex import parse_codex_quota
from makewand.providers.muse import parse_muse_quota

A = "\u2019"
CODEX_HDR = ("OpenAI Codex v0.162.1 (research preview)\n--------\nworkdir: /repo\nmodel: gpt-6\nprovider: openai\n"
             "approval: never\nsandbox: read-only\nreasoning effort: high\nreasoning summaries: auto\n"
             "session id: 0199a1b2-c3d4-7e8f-9a0b-1c2d3e4f5a6b\n--------\nuser\nreview this\n\n")
CODEX_GENERIC = "ERROR: stream disconnected before completion: error sending request\n"
CHAIN = ("Error: turn failed\n\nCaused by:\n    0: request failed\n"
         f"    1: You{A}ve hit your usage limit. Try again at 8:00 PM.\n")
WEEKLY = "\u26a0 Heads up, you have less than 5% of your weekly limit left. Run /status for a breakdown.\n"


def muse_verdict(output, stderr=None):
    limited, _reason, resets = parse_muse_quota(output, stderr) if stderr is not None else parse_muse_quota(output)
    return "-" if not limited else "AUTH" if resets == "需登录授权" else "LIM"


def gemini_kinds(output, stderr=None):
    return health.find_cli_messages("gemini", ("quota", "rate"), output, stderr)


class TestMuseCountsOnlyItsRealFailureFormats(unittest.TestCase):
    NOT_MUSE = ["HTTP 429 Too Many Requests", "429 Rate limit exceeded", "HTTP 429 Rate limit exceeded.",
                "429 rate limit exceeded", "rate limited (retry after 30s)", "error: rate limited (retry after 30s)",
                "Not uploaded: rate limited (retry after 30s)"]

    def test_generic_429_lines_ending_an_answer_are_not_muse_verdicts(self):
        for line in self.NOT_MUSE:
            with self.subTest(line=line):
                answer = f"I called the endpoint and it now answers:\n{line}\n"
                self.assertEqual(muse_verdict(answer, "error: turn failed\n"), "-")
                self.assertEqual(muse_verdict(answer + "error: turn failed\n"), "-")
                self.assertEqual(muse_verdict(line + "\n"), "-")
                self.assertEqual(muse_verdict("", line + "\n"), "-")

    def test_no_reset_time_is_taken_from_the_answer(self):
        limited, reason, resets = parse_muse_quota("The upload said:\nrate limited (retry after 30s)\n",
                                                   "error: turn failed\n")
        self.assertEqual((limited, reason, resets), (False, "", None))

    def test_muse_failure_formats_still_count(self):
        for line in ("rate limited (HTTP 429 \u00b7 attempt 2)", "request failed (HTTP 429 \u00b7 attempt 1)",
                     "Error: usage limit reached (HTTP 429 \u00b7 attempt 3)"):
            with self.subTest(line=line):
                self.assertEqual(muse_verdict("partial answer\n", line + "\n"), "LIM")
                self.assertEqual(muse_verdict("partial answer\n" + line + "\n"), "LIM")

    def test_catalog_lists_only_the_attempt_formats(self):
        rate = health.CLI_MESSAGE_CATALOG["muse"]["rate"]
        self.assertTrue(all("attempt" in pattern for pattern in rate), rate)


class TestCodexErrorEventsCarryTheApiMessages(unittest.TestCase):
    # How codex 0.162.1 prints an API error in an ERROR event, and the API messages it relays.
    API_EVENTS = [
        "ERROR: unexpected status 429 Too Many Requests: You exceeded your current quota, please check your plan "
        "and billing details., url: https://api.openai.com/v1/responses, request id: req_1",
        "ERROR: unexpected status 429 Too Many Requests: Rate limit reached for gpt-6 in organization org-x on "
        "tokens per min (TPM): Limit 30000, Used 30000., url: https://api.openai.com/v1/responses",
        "ERROR: unexpected status 403 Forbidden: You exceeded your current quota, please check your plan and "
        "billing details.",
        "ERROR: Rate limit reached for gpt-6 in organization org-x on tokens per min (TPM): Limit 30000, "
        "Used 30000. Please try again in 2s.",
        "ERROR: You exceeded your current quota, please check your plan and billing details.",
        "ERROR: Usage limit reached.",
        "ERROR: weekly limit reached \u00b7 resets Oct 20",
    ]

    def test_events_count_after_every_section(self):
        for event in self.API_EVENTS:
            for section in ("codex\nWorking.\n", "thinking\nThe build failed; retry.\n",
                            "exec\nbash -lc 'make test' in /repo exited 1 in 1.10s:\nFAILED x\n", ""):
                with self.subTest(event=event[:50], section=section[:12]):
                    transcript = CODEX_HDR + section + event + "\n"
                    self.assertTrue(parse_codex_quota("", transcript, prompt="review this")[0])
                    self.assertTrue(parse_codex_quota("", transcript)[0])
                    self.assertTrue(parse_codex_quota(transcript)[0])

    def test_the_same_messages_without_the_event_marker_stay_task_content(self):
        for line in ("You exceeded your current quota, please check your plan and billing details.",
                     "Rate limit reached for gpt-4o in organization org-abc on tokens per min (TPM): Limit 30000.",
                     "Usage limit reached.",
                     "ERROR:root:upstream said: You exceeded your current quota, please check your plan.",
                     " ERROR: You exceeded your current quota, please check your plan and billing details."):
            for section in ("codex\nI made the change.\n", "exec\nbash -lc 'make test' in /repo exited 1 in 1.10s:\n"):
                with self.subTest(line=line[:40], section=section[:6]):
                    transcript = CODEX_HDR + section + line + "\n\n" + CODEX_GENERIC
                    self.assertFalse(parse_codex_quota("", transcript, prompt="review this")[0])


class TestCodexFatalReport(unittest.TestCase):
    """codex's main ends a failed run with its error, after whatever section it was in."""

    def test_fatal_error_after_the_transcript_counts(self):
        cases = {
            "anyhow chain after the agent's message": CODEX_HDR + "codex\nWorking.\n" + CHAIN,
            "Error: line after the agent's message":
                CODEX_HDR + f"codex\nWorking.\nError: You{A}ve hit your usage limit. Try again at 8:00 PM.\n",
            "Error: line after the token count":
                CODEX_HDR + "codex\nWorking.\ntokens used\n12\n"
                "Error: exceeded retry limit, last status: 429 Too Many Requests\n",
            "chain after reasoning, trailing blank lines": CODEX_HDR + "thinking\nRetrying.\n" + CHAIN + "\n\n",
            "timestamped sections": CODEX_HDR.replace("user\n", "[2026-10-12T08:00:00] user\n")
                + "[2026-10-12T08:00:01] codex\nHi\n" + CHAIN,
            "after an own error event": CODEX_HDR + "codex\nok\n" + CODEX_GENERIC + CHAIN,
        }
        for name, transcript in cases.items():
            with self.subTest(case=name):
                limited, reason, resets = parse_codex_quota("", transcript, prompt="review this")
                self.assertTrue(limited)
                self.assertTrue(parse_codex_quota(transcript)[0])
        _limited, reason, resets = parse_codex_quota("", cases["anyhow chain after the agent's message"])
        self.assertEqual(resets, "8:00 PM")
        self.assertIn("使用额度耗尽", reason)

    def test_an_error_that_is_not_the_fatal_report_stays_content(self):
        cases = {
            "a command's output": CODEX_HDR + "exec\nbash -lc 'curl -sf https://api.example.com' in /repo\n"
                                              " exited 22 in 0.31s:\nError: 429 Too Many Requests\n",
            "a command's anyhow chain": CODEX_HDR + "exec\nbash -lc 'cargo run' in /repo exited 1 in 1.10s:\n" + CHAIN,
            "not the last line": CODEX_HDR + f"codex\nError: You{A}ve hit your usage limit.\nSo I stopped.\n",
            "a chain without the blank line": CODEX_HDR + "codex\nWorking.\nError: turn failed\nCaused by:\n"
                                                          f"    You{A}ve hit your usage limit.\n",
            "indented lines without a chain": CODEX_HDR + f"codex\nError: turn failed\n    You{A}ve hit your usage "
                                                          "limit.\n",
            "an indented Error: line": CODEX_HDR + f"codex\nThe log:\n  Error: You{A}ve hit your usage limit.\n",
            "lower-case error:": CODEX_HDR + f"codex\nWorking.\nerror: You{A}ve hit your usage limit.\n",
        }
        for name, transcript in cases.items():
            with self.subTest(case=name):
                self.assertFalse(parse_codex_quota("", transcript, prompt="review this")[0])
        # Without the prompt, the echo is the "user" section: a prompt ending in an error is content.
        echo = CODEX_HDR.replace("review this\n\n", f"Fix this log:\nError: You{A}ve hit your usage limit.\n")
        self.assertFalse(parse_codex_quota("", echo)[0])

    def test_the_go_router_reads_the_same_rule_from_the_catalog(self):
        spec = health.cli_catalog_entry("codex")["transcript"]
        rules = health._Transcript(spec)
        self.assertIsNotNone(rules.fatal_error)
        self.assertIsNotNone(rules.fatal_after)
        self.assertTrue(rules.fatal_error.match("Error: x"))
        self.assertFalse(rules.fatal_error.match("ERROR: x"))
        self.assertFalse(rules.fatal_error.match("error: x"))
        for header in ("codex", "thinking", "tokens used", "[2026-10-12T08:00:01] codex",
                       "OpenAI Codex v0.162.1 (research preview)"):
            self.assertTrue(rules.fatal_after.match(header), header)
        for header in ("user", "exec", "exec bash -lc 'ls' in /repo", " exited 22 in 0.31s:", "apply_patch",
                       "codex said"):
            self.assertFalse(rules.fatal_after.match(header), header)


class TestCodexWarningsTripTheReserveGate(unittest.TestCase):
    def setUp(self):
        patcher = patch.dict(os.environ, {health.QUOTA_RESERVE_ENV: "8"})
        patcher.start()
        self.addCleanup(patcher.stop)
        official = patch.object(health, "_get_official_subscription_quota", return_value=None)
        official.start()
        self.addCleanup(official.stop)

    def test_codex_own_warning_lines_inside_sections(self):
        cases = {
            "heads-up in the agent section": CODEX_HDR + "codex\nok\n" + WEEKLY + CODEX_GENERIC,
            "short form in the agent section": CODEX_HDR + "codex\nok\nweekly limit: 3% left\n",
            "heads-up after a command": CODEX_HDR + "exec\nbash -lc 'ls' in /repo succeeded in 0.1s:\nx\n" + WEEKLY,
            "heads-up after the echo": CODEX_HDR + WEEKLY,
        }
        for name, transcript in cases.items():
            with self.subTest(case=name):
                limited, reason, _ = parse_codex_quota("", transcript, prompt="review this")
                self.assertTrue(limited)
                self.assertIn("配额保护阈值拦截", reason)
                if name != "heads-up after the echo":  # without the prompt that line is in the echo
                    self.assertTrue(parse_codex_quota("", transcript)[0])
                    self.assertTrue(parse_codex_quota(transcript)[0])
        self.assertIn("剩余 3% < 8%", parse_codex_quota("", cases["short form in the agent section"])[1])

    def test_the_words_inside_the_answer_are_not_a_warning(self):
        cases = {
            "inside a sentence": CODEX_HDR + "codex\nThe banner now reads 'weekly limit: 3% left' in the UI.\n",
            "indented": CODEX_HDR + "codex\nThe status table:\n    weekly limit: 3% left\n",
            "fenced": CODEX_HDR + "codex\nQuoted:\n```\nweekly limit: 3% left\n```\nDone.\n",
            "in the prompt echo": CODEX_HDR.replace("review this\n", "review this\nweekly limit: 3% left\n"),
            "on the answer stream": None,
        }
        for name, transcript in cases.items():
            with self.subTest(case=name):
                if transcript is None:
                    self.assertFalse(parse_codex_quota(WEEKLY, CODEX_HDR + CODEX_GENERIC)[0])
                else:
                    self.assertFalse(parse_codex_quota("", transcript)[0])

    def test_warnings_never_reach_find_cli_messages(self):
        transcript = CODEX_HDR + "codex\nok\n" + WEEKLY
        self.assertEqual(health.find_cli_messages("codex", ("quota", "rate"), "", transcript), {})
        lines = list(health.cli_own_lines("codex", "", transcript))
        self.assertEqual(lines, [])
        warned = list(health.cli_own_lines("codex", "", transcript, warnings=True))
        self.assertEqual(warned, [WEEKLY.strip()])


class TestCatalogMisses(unittest.TestCase):
    JSON_REPORT = ('{\n  "session_id": "4f3c2b1a-0000-4000-8000-000000000001",\n  "error": {\n'
                   '    "type": "TerminalQuotaError",\n'
                   '    "message": "You have exhausted your daily quota on this model.",\n    "code": 429\n  }\n}\n')

    def test_claude_organization_usage_credits(self):
        for line in ("Your organization is out of usage credits. Contact your admin to add more.",
                     "Your organization's usage credit cap is reached for this period. Contact your admin to raise it.",
                     f"Your organization{A}s usage credit cap is reached for this period."):
            with self.subTest(line=line):
                self.assertTrue(parse_claude_quota(line + "\n", "")[0])
                self.assertTrue(parse_claude_quota(line)[0])

    def test_gemini_cli_formats(self):
        cases = {
            "usage limit": "Usage limit reached for all Pro models.\nAccess resets at 8:00 PM.\n"
                           "/stats model for usage details\n/model to switch models.\n",
            "usage limit, model id": "Usage limit reached for gemini-3-pro-preview.\n",
            "capacity": "[API Error: You have exhausted your capacity on this model. Your quota will reset after "
                        "2s.]\n",
            "daily quota": "[API Error: You have exhausted your daily quota on this model. (Status: "
                           "RESOURCE_EXHAUSTED)]\nPlease wait and try again later. To increase your limits, request a "
                           "quota increase through AI Studio, or switch to another /auth method\n",
            "json report": self.JSON_REPORT,
            "json report behind [ERROR]": "[ERROR] " + self.JSON_REPORT.replace("TerminalQuotaError", "Error"),
            "critical error": "An unexpected critical error occurred:TerminalQuotaError: You have exhausted your "
                              "daily quota on this model.\n    at classifyGoogleError (file:///gemini.js:1:1)\n",
            "google api": "Error: googleapi: Error 429: Quota exceeded for quota metric 'Generate Content API "
                          "requests per minute'\n",
        }
        for name, stderr in cases.items():
            with self.subTest(case=name):
                self.assertTrue(gemini_kinds("", stderr), "stderr apart")
                if name != "usage limit":  # merged, only the report gemini ends with counts
                    self.assertTrue(gemini_kinds("partial answer\n" + stderr), "merged, report last")
        found = gemini_kinds("", self.JSON_REPORT)
        self.assertEqual(set(found), {"quota", "rate"})
        self.assertTrue(found["quota"].startswith('{ "session_id": "4f3c2b1a'), found)

    def test_gemini_noise_is_not_a_refusal(self):
        for stderr in ("Usage limit reached for the free tier now shows an upgrade banner.\n",
                       '{\n  "session_id": "x",\n  "error": {\n    "type": "FatalAuthenticationError",\n'
                       '    "message": "Failed to login.",\n    "code": 41\n  }\n}\n',
                       "You exhausted the retry budget.\n", "Quota exceeded for a project now raises QuotaError.\n",
                       "HTTP/1.1 429 Too Many Requests\n"):
            with self.subTest(stderr=stderr[:40]):
                self.assertEqual(gemini_kinds("", stderr), {})
        # Only the report gemini ends with counts in a merged stream: the model's JSON before it does not.
        answer = '{\n  "error": {\n    "type": "TerminalQuotaError",\n    "code": 429\n  }\n}\nThat is the shape.\n'
        self.assertEqual(gemini_kinds(answer + "Done.\n"), {})

    def test_json_reports_are_gemini_only(self):
        self.assertIs(health.cli_catalog_entry("gemini").get("json_reports"), True)
        for provider in ("claude", "codex", "grok", "muse", "agy"):
            self.assertNotIn("json_reports", health.cli_catalog_entry(provider))
        # A pretty JSON body is never joined for another CLI: its indented lines stay skipped.
        self.assertEqual(health.find_cli_messages("agy", ("quota",), "", '{\n  "message": "You have exhausted '
                                                                          'your quota on this model."\n}\n'), {})

    def test_json_report_lines_join_only_open_brackets(self):
        lines = ["[ERROR] {", '  "a": [', "    1,", "    2", "  ]", "}", "after", "{}", "  {", "x"]
        skipped = set()
        joined = health._json_report_lines(lines, skipped)
        self.assertEqual(joined, {0: '[ERROR] { "a": [ 1, 2 ] }'})
        self.assertEqual(skipped, {1, 2, 3, 4, 5})
        # An unterminated report ends at the first line that is not JSON-shaped.
        skipped = set()
        self.assertEqual(health._json_report_lines(["{", '  "a": 1,', "plain text", "}"], skipped),
                         {0: '{ "a": 1,'})
        self.assertEqual(skipped, {1})


class DispatchCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-fix-r4-")
        root = Path(self._tmp.name)
        for p in (patch.object(config, "CONFIG_DIR", root / "cfg"),
                  patch.object(health, "STATUS_CACHE_FILE", root / "cfg" / "status.json"),
                  patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json")):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        (root / "cfg").mkdir(parents=True)

    def dispatch(self, module, code, out, err, *, stream=False):
        engine = module.__name__.rsplit(".", 1)[-1]
        with patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("makewand.sandbox._agy_host_credits_disabled", return_value=True), \
             patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(True, "config-ack")), \
             patch("makewand.sandbox.audit_unsafe_host_exec"), \
             patch("makewand.providers.muse.detect_muse_guard", return_value=(False, None)), \
             patch.object(module, "run_subprocess", return_value=(code, out, err, None)), \
             contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            result = getattr(module, f"execute_{engine}_task")("review this", cwd=self._tmp.name, readonly=True,
                                                               stream=stream)
        from makewand.model_quota import projected_status
        from makewand.discovery import get_provider_model_tier
        info = projected_status(engine, health.load_status_cache().get(engine, {}),
                                get_provider_model_tier(engine)["model"])
        return result, info.get("status")


class TestDispatchWriteBack(DispatchCase):
    def test_codex_fatal_chain_marks_codex_limited(self):
        _result, status = self.dispatch(codex, 1, "", CODEX_HDR + "codex\nWorking.\n" + CHAIN)
        self.assertEqual(status, "limited")

    def test_codex_relayed_api_quota_message_marks_codex_limited(self):
        event = "ERROR: You exceeded your current quota, please check your plan and billing details.\n"
        _result, status = self.dispatch(codex, 1, "", CODEX_HDR + "codex\nWorking.\n" + event)
        self.assertEqual(status, "limited")

    def test_muse_answer_ending_in_a_generic_429_leaves_muse_unknown(self):
        result, status = self.dispatch(muse, 1, "I called the endpoint and it now answers:\n"
                                                "HTTP 429 Too Many Requests\n", "error: turn failed\n")
        self.assertFalse(result[0])
        self.assertEqual(status, "unknown")


class TestParityCorpusCoversRoundFour(unittest.TestCase):
    def test_corpus_holds_the_round_four_cases(self):
        corpus = json.loads((Path(__file__).resolve().parent / "testdata" / "cli_message_parity.json")
                            .read_text(encoding="utf-8"))
        ids = {case["id"] for case in corpus["cases"]}
        for prefix in ("r4-gemini-json", "r4-codex-fatal", "r4-codex-event", "r4-muse"):
            self.assertTrue(any(cid.startswith(prefix) for cid in ids), prefix)


if __name__ == "__main__":
    unittest.main()
