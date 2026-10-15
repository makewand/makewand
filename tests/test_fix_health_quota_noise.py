"""
quota-noise (baseline-go-6): quota and login failures are detected only where a
provider CLI prints one of its own messages (health.CLI_MESSAGE_CATALOG) on a
line it writes itself, never from the model's own text or from tool output
inside the transcript. A false limited/needs_auth verdict disables a healthy
provider (muse used to match a bare "429"; all five parsers matched "rate limit"
in prose; grok and agy read "expected 401 Unauthorized" as a login failure),
and a missed one keeps dispatching to an exhausted subscription instead of
relaying to the API key (Grok Build's real messages use U+2019 apostrophes).
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import re
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
from makewand.providers import agy, claude, codex, grok, muse
from makewand.providers.agy import classify_agy_failure, parse_agy_quota
from makewand.providers.claude import parse_claude_quota
from makewand.providers.codex import parse_codex_quota
from makewand.providers.grok import parse_grok_quota
from makewand.providers.muse import parse_muse_quota

PARSERS = {"claude": parse_claude_quota, "codex": parse_codex_quota, "grok": parse_grok_quota,
           "muse": parse_muse_quota, "agy": parse_agy_quota}


def verdict(provider, output, stderr=None):
    """LIM / AUTH / REGION / "-" for one CLI run; stderr=None means one merged stream."""
    streams = (output,) if stderr is None else (output, stderr)
    limited, reason, resets = PARSERS[provider](*streams)
    if provider == "agy" and not limited:
        failure = classify_agy_failure(*streams)
        if failure:
            return {"needs_auth": "AUTH", "error": "REGION"}[failure[0]]
    if not limited:
        return "-"
    return "AUTH" if resets == "需登录授权" or "登录" in (reason or "") else "LIM"


# Model prose and tool output that a failed task may leave in the transcript.
TASK_CONTENT = [
    "===== 429 passed in 3.2s =====",
    'File "app.py", line 429, in handler',
    "Added a rate limit to the login endpoint; 1 test failed",
    "gh: API rate limit exceeded for user ID 1234.",
    "npm ERR! 429 Too Many Requests - GET https://registry.npmjs.org/x",
    "AssertionError: expected 401 Unauthorized, got 200",
    "The handler returns unauthorized.",
    "Endpoint now responds 'auth required' when token missing",
    "Implemented the per-user usage limit check",
    "requests.exceptions.HTTPError: 429 Client Error: Too Many Requests for url: https://api.example.com",
    "google.api_core.exceptions.ResourceExhausted: RESOURCE_EXHAUSTED",
    "Rate limit exceeded errors are now retried with backoff.",
    "- Too many requests now return HTTP 429 with a Retry-After header",
    "    Error: 429 Too Many Requests",
    "\tError:      \tNot equal: expected: 401 Unauthorized",
    "test_rate_limit_middleware FAILED",
    "I added `oauth/device` support and a `press enter to open` prompt to the CLI.",
    # prose that opens a line with a quota/login phrase
    "Too many requests from a single IP are now rejected with HTTP 429.",
    "Too many requests in a short window now trigger the limiter.",
    "Rate limited (429) responses are now retried with backoff.",
    "Quota exceeded for a project now raises QuotaError.",
    "Rate limit exceeded: the gh CLI refused my request, so I skipped the PR step.",
    "Usage limit reached for the free tier now shows an upgrade banner.",
    "Authentication failed. That is the expected result for the bad-password test.",
    "Session expired? The middleware now redirects to /login.",
    "Rate limit exceeded on the GitHub API, so I stopped after two files.",
    "Too many requests to the registry; I pinned versions instead.",
    "Error: rate limit exceeded when calling GitHub; I retried with a token.",
    "UNAUTHENTICATED: returned by the new interceptor when the bearer token is missing.",
    "oauth2: token refresh now retries when the access token expired.",
    # tool output a CLI relays unindented
    "Error: Request failed with status code 429",
    "[ERROR] Rate limit exceeded for client 10.0.0.1",
    "Error: Too many requests",
    "✗ Error: quota exceeded for this project",
    "ERROR: 401 Client Error: Unauthorized for url: https://pypi.internal/simple/x/",
    "fatal: Authentication failed for 'https://github.com/acme/repo.git/'",
]

# codex exec transcripts relay each command's output under its result line.
CODEX_TRANSCRIPTS = [
    "exec\nbash -lc 'curl -sf https://api.example.com' in /repo\n exited 22 in 0.31s:\nError: 429 Too Many Requests",
    "exec\nbash -lc 'curl -si https://api.github.com/rate_limit' in /repo\n succeeded in 0.4s:\nHTTP/2 429\n"
    "x-ratelimit-remaining: 0",
    "exec\nbash -lc 'pip install -i https://pypi.internal/simple x' in /repo\n exited 1 in 1.10s:\n"
    "ERROR: 401 Client Error: Unauthorized for url: https://pypi.internal/simple/x/",
]

# What the CLIs really print (strings of the installed binaries; U+2019 kept verbatim).
REAL_MESSAGES = {
    "claude": [
        ("LIM", "You've hit your limit · resets 8pm (Asia/Shanghai)"),
        ("LIM", "You've hit your session limit · resets 3pm (Asia/Shanghai)"),
        ("LIM", "You've reached your Fable limit."),
        ("LIM", "You've hit your channel's monthly spend limit."),
        ("LIM", "You've hit your team's shared budget. Switch to another model"),
        ("LIM", "You're out of usage credits · resets Oct 20, 8pm"),
        ("LIM", "Your org is out of usage · add funds to continue"),
        ("LIM", "Opus 5 requires usage credits. Buy more to keep using it."),
        ("LIM", "Claude AI usage limit reached|1760000000"),
        ("LIM", "API Error: Request rejected (429) · this may be a temporary capacity issue."),
        ("LIM", 'API Error: 429 {"type":"error","error":{"type":"rate_limit_error","message":"Rate limit exceeded"}}'),
        ("LIM", "API Error: Rate limit exceeded"),
        ("LIM", "API Error: Repeated 529 Overloaded errors"),
        ("LIM", "API Error: Server is temporarily limiting requests (not your usage limit) · Rate limit exceeded"),
    ],
    "codex": [
        ("LIM", "ERROR: You\u2019ve hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit "
                "https://chatgpt.com/settings/usage to purchase more credits or try again at Oct 20th, 2026 8:00 PM."),
        ("LIM", "ERROR: You\u2019ve hit your usage limit for GPT-6 Sol. Switch to another model now, or try again at 8:00 PM."),
        ("LIM", "ERROR: Your workspace is out of credits. Add credits to continue."),
        ("LIM", "ERROR: You hit your spend cap set in your workspace. Increase your spend cap to continue."),
        ("LIM", "ERROR: Quota exceeded. Check your plan and billing details."),
        ("LIM", "ERROR: exceeded retry limit, last status: 429 Too Many Requests, request id: abc"),
        ("LIM", "ERROR: unexpected status 429 Too Many Requests: slow down"),
        ("LIM", "ERROR: stream disconnected before completion: Rate limit reached for gpt-6 in organization org-x "
                "on tokens per min (TPM): Limit 30000, Used 30000. Please try again in 2s."),
    ],
    "grok": [
        ("LIM", "Error: You\u2019ve reached your free Grok Build usage limit for now. Get SuperGrok for much higher "
                "limits, or try again later: https://grok.com/supergrok?referrer=grok-build"),
        ("LIM", "API error (status 429): You\u2019ve hit your team\u2019s API rate limit. Ask a team admin to purchase "
                "more credits for higher limits, or try again later. See "
                "https://docs.x.ai/developers/rate-limits#rate-limit-tiers"),
        ("LIM", "Error: API error (status 429): request rejected"),
        ("LIM", "You\u2019ve hit the rate limit for your plan. Upgrade your account or try again later."),
        ("LIM", "You\u2019ve hit the credit limit for your plan."),
        ("LIM", "You\u2019ve hit your spending cap."),
        ("LIM", "Error: failed after 3 retries: request error: Rate limited (429)"),
        ("AUTH", "Authentication failed. Run `grok login`, set XAI_API_KEY, or add api_key to ~/.grok/config.toml."),
        ("AUTH", "Session expired. Run `grok login` to re-authenticate."),
        ("AUTH", "Error: Not logged in. Run `grok login`."),
        ("AUTH", "Your session expired and Grok could not renew it in the background. Run /login to sign in again."),
        ("AUTH", "Responses API returned 401 Unauthorized: invalid api key"),
        ("AUTH", "Unauthorized (401)"),
    ],
    "muse": [
        ("LIM", "Error: usage limit reached (HTTP 429 · attempt 3)"),
        ("LIM", "rate limited (HTTP 429 · attempt 2)"),
        ("AUTH", "missing meta credentials: set META_API_KEY or run muse login"),
        ("AUTH", "No Meta credentials were found. Run `muse login`."),
        ("AUTH", "Your saved Meta credentials are invalid. Run `muse login`."),
        ("AUTH", "Open this page to sign in: https://www.meta.ai/device"),
        ("AUTH", "Press Enter to open it in your browser:"),
    ],
    "agy": [
        ("LIM", "Error: You have exhausted your quota on this model."),
        ("LIM", "Error: RESOURCE_EXHAUSTED (code 429): You have exhausted your quota on this model."),
        ("LIM", "Error: rpc error: code = ResourceExhausted desc = Resource has been exhausted (e.g. check quota)."),
        ("LIM", "Error: Error 429, Message: Resource has been exhausted (e.g. check quota)., Status: RESOURCE_EXHAUSTED"),
        ("AUTH", "Error: authentication required. Run 'agy' to log in, then retry."),
        ("AUTH", "Error: failed to send message: rpc error: code = Unauthenticated desc = "
                 "Request had invalid authentication credentials."),
        ("AUTH", 'Error: oauth2: "invalid_grant" "Token has been expired or revoked."'),
        ("REGION", "Error: rpc error: code = FailedPrecondition desc = User location is not supported for the API use."),
    ],
}


class TestTranscriptContentIsNotAProviderVerdict(unittest.TestCase):
    def test_no_parser_flags_task_content(self):
        for provider in PARSERS:
            for text in TASK_CONTENT:
                with self.subTest(provider=provider, text=text):
                    self.assertEqual(verdict(provider, text), "-")

    def test_multi_line_transcript_with_tool_output(self):
        transcript = "\n".join([
            "I ran the tests:",
            "```",
            "$ pytest -q",
            "429 passed, 1 failed in 3.2s",
            "E   AssertionError: expected 401 Unauthorized, got 200",
            "```",
            "The GitHub CLI also said:",
            "gh: API rate limit exceeded for user ID 1234.",
            "Next I will add a usage limit check.",
        ])
        for provider in PARSERS:
            with self.subTest(provider=provider):
                self.assertEqual(verdict(provider, transcript), "-")

    def test_command_output_relayed_by_codex_is_not_a_verdict(self):
        for provider in PARSERS:
            for transcript in CODEX_TRANSCRIPTS:
                with self.subTest(provider=provider, transcript=transcript.splitlines()[-1]):
                    self.assertEqual(verdict(provider, transcript), "-")
        # codex's own refusal right after a command's output still counts.
        own = CODEX_TRANSCRIPTS[0] + "\nERROR: exceeded retry limit, last status: 429 Too Many Requests"
        self.assertEqual(verdict("codex", own), "LIM")

    def test_errors_quoted_inside_a_closed_code_fence_are_task_content(self):
        for provider, messages in REAL_MESSAGES.items():
            expected, message = messages[0]
            with self.subTest(provider=provider):
                answer = f"The run failed:\n```\n{message}\n```\nI added a retry."
                self.assertEqual(verdict(provider, answer), "-")
                # A truncated answer with an unclosed fence must not hide the CLI's own error after it.
                self.assertEqual(verdict(provider, f"Here is the output:\n```\npartial log line\n\n{message}\n"), expected)


class TestCliOwnMessagesAreDetected(unittest.TestCase):
    def test_real_cli_messages(self):
        for provider, messages in REAL_MESSAGES.items():
            for expected, message in messages:
                with self.subTest(provider=provider, message=message):
                    self.assertEqual(verdict(provider, message), expected)
                    # ANSI colour and the typographic apostrophe's ASCII twin read the same.
                    self.assertEqual(verdict(provider, "\x1b[31m" + message.replace("\u2019", "'") + "\x1b[0m"), expected)

    def test_message_after_the_transcript(self):
        for provider, messages in REAL_MESSAGES.items():
            expected, message = messages[0]
            with self.subTest(provider=provider):
                self.assertEqual(verdict(provider, f"Added a rate limit to the login endpoint.\n429 passed\n{message}\n"),
                                 expected)

    def test_anyhow_caused_by_chain_and_tracing_error_lines(self):
        chained = ("Error: prompt failed\n\nCaused by:\n    API error (status 429): You\u2019ve hit the rate limit "
                   "for your plan. Upgrade your account or try again later.")
        self.assertEqual(verdict("grok", chained), "LIM")
        self.assertEqual(verdict("grok", "Error: prompt failed\n\nCaused by:\n    0: request error\n"
                                         "    1: Unauthorized (401)"), "AUTH")
        self.assertEqual(verdict("codex", "Error: turn failed\n\nCaused by:\n    You\u2019ve hit your usage limit. "
                                          "Try again at 8:00 PM."), "LIM")
        self.assertEqual(verdict("codex", "2026-10-12T08:00:00.123456Z ERROR codex_core::codex: Turn error: "
                                          "You've hit your usage limit. Try again at 8:00 PM."), "LIM")
        # Retries a CLI logs at WARN level are not its verdict.
        self.assertEqual(verdict("codex", "2026-10-12T08:00:00.123456Z  WARN codex_core::client: stream error: "
                                          "unexpected status 429 Too Many Requests; retrying 1/5"), "-")

    def test_short_forms_existing_callers_rely_on(self):
        self.assertEqual(verdict("claude", "rate_limit_error: 429 Too Many Requests"), "LIM")
        self.assertEqual(verdict("claude", "Weekly limit reached · resets Oct 20, 8pm (Asia/Shanghai)"), "LIM")
        self.assertEqual(verdict("codex", "Error: 429 too many requests"), "LIM")
        self.assertEqual(verdict("codex", "weekly limit reached · resets at 10-04 10:40"), "LIM")
        self.assertEqual(verdict("grok", "Rate limit reached: resets in 45m"), "LIM")
        self.assertEqual(verdict("grok", "Please sign in with grok auth"), "AUTH")
        self.assertEqual(verdict("agy", "Error: ResourceExhausted: 429 Resource has been exhausted"), "LIM")
        # Muse Code 1.4.6 prints its failures only as "<kind> (HTTP <status> · attempt <n>)": a
        # generic 429 line is never its verdict (round 4: an answer ending in one marked it limited).
        self.assertEqual(verdict("muse", "rate limited (HTTP 429 · attempt 2)"), "LIM")
        for text in ("429 rate limit exceeded", "HTTP 429 Rate limit exceeded.", "HTTP 429 Too Many Requests"):
            with self.subTest(provider="muse", text=text):
                self.assertEqual(verdict("muse", text), "-")
        for provider in PARSERS:
            with self.subTest(provider=provider):
                if provider != "muse":
                    self.assertEqual(verdict(provider, "HTTP 429 Too Many Requests"), "LIM")
                # ... but never a client's response header or a longer sentence.
                self.assertEqual(verdict(provider, "HTTP/1.1 429 Too Many Requests"), "-")
                self.assertEqual(verdict(provider, "HTTP 429 Too Many Requests is what the proxy returns now."), "-")


class TestOnlyTheCliOwnStreamCounts(unittest.TestCase):
    """With stdout and stderr captured apart, the answer on stdout is never the CLI's verdict."""

    def test_answer_on_stdout_is_task_content(self):
        for provider in ("codex", "grok", "agy"):
            expected, message = REAL_MESSAGES[provider][0]
            with self.subTest(provider=provider):
                self.assertEqual(verdict(provider, f"The log said:\n{message}\n", "Error: stream closed\n"), "-")
                self.assertEqual(verdict(provider, "partial answer\n", f"{message}\n"), expected)

    def test_codex_weekly_warning_counts_only_on_codex_lines(self):
        warning = "⚠ Heads up, you have less than 10% of your weekly limit left. Run /status for a breakdown."
        self.assertTrue(parse_codex_quota(warning)[0])
        self.assertTrue(parse_codex_quota("", warning + "\n")[0])
        for output, stderr in ((warning + "\n", "ERROR: stream disconnected before completion: error sending request\n"),
                               ("The status table:\n    weekly limit: 3% left\n", None),
                               ("Quoted:\n```\nweekly limit: 3% left\n```\nDone.\n", None)):
            with self.subTest(output=output):
                self.assertFalse(parse_codex_quota(output, stderr)[0] if stderr is not None
                                 else parse_codex_quota(output)[0])

    def test_claude_result_text_on_stdout_counts(self):
        self.assertEqual(verdict("claude", "You've hit your limit · resets 8pm (Asia/Shanghai)\n", ""), "LIM")

    def test_cli_own_output_selects_the_streams(self):
        self.assertEqual(health.cli_own_output("grok", "answer", "error"), "error")
        self.assertEqual(health.cli_own_output("claude", "answer", "error"), "answer\nerror")
        self.assertEqual(health.cli_own_output("grok", "merged answer and error"), "merged answer and error")


class TestDispatchWriteBack(unittest.TestCase):
    """execute_<engine>_task with a fake run_subprocess: what reaches the status cache."""

    MODULES = {"grok": grok, "muse": muse, "agy": agy, "codex": codex, "claude": claude}

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-fix-qn-")
        root = Path(self._tmp.name)
        for p in (patch.object(config, "CONFIG_DIR", root / "cfg"),
                  patch.object(health, "STATUS_CACHE_FILE", root / "cfg" / "status.json"),
                  patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json")):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        (root / "cfg").mkdir(parents=True)

    def dispatch(self, engine, out, err, *, api=False, stream=False):
        module = self.MODULES[engine]
        with patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=api), \
             patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(True, "config-ack")), \
             patch("makewand.sandbox.audit_unsafe_host_exec"), \
             patch("makewand.providers.muse.detect_muse_guard", return_value=(False, None)), \
             patch("makewand.providers.api_client.call_api_chat", return_value=(True, "relayed", None)) as relay, \
             patch.object(module, "run_subprocess", return_value=(1, out, err, None)):
            result = getattr(module, f"execute_{engine}_task")("review this", cwd=self._tmp.name, readonly=True,
                                                               stream=stream)
        return result, health.load_status_cache().get(engine, {}).get("status"), relay

    def test_real_grok_quota_messages_mark_limited_and_relay_to_the_api(self):
        for _, message in REAL_MESSAGES["grok"][:3]:
            with self.subTest(message=message):
                result, status, relay = self.dispatch("grok", "", message + "\n")
                self.assertEqual(status, "limited")
                relay.assert_not_called()
                health.STATUS_CACHE_FILE.unlink()
                # With an API key the refusal itself hands the task to the xAI API.
                result, status, relay = self.dispatch("grok", "", message + "\n", api=True)
                self.assertEqual(status, "limited")
                relay.assert_called_once()
                self.assertEqual(result[1], "relayed")
                health.STATUS_CACHE_FILE.unlink()

    def test_stream_mode_reads_the_merged_output(self):
        _, status, _ = self.dispatch("grok", "Working...\n" + REAL_MESSAGES["grok"][0][1] + "\n", "", stream=True)
        self.assertEqual(status, "limited")

    def test_prose_and_tool_output_on_stdout_do_not_disable_the_provider(self):
        cases = [
            ("grok", "I added per-IP throttling.\nToo many requests from a single IP are now rejected with HTTP 429.\n",
             "Error: stream closed before the turn finished\n"),
            ("codex", "codex\nI added per-IP throttling.\nToo many requests from a single IP are now rejected.\n",
             "ERROR: stream disconnected before completion: error sending request\n"),
            ("muse", "exec pip install -i https://pypi.internal/simple x\nERROR: 401 Client Error: Unauthorized for url: "
                     "https://pypi.internal/simple/x/\nThe private index rejected the request; I stopped.\n",
             "error: turn failed\n"),
            ("agy", "Rate limit exceeded on the GitHub API, so I stopped after two files.\n", "Error: exit status 1\n"),
            ("claude", "", "Error: Reached max turns (25)\n"),
        ]
        for engine, out, err in cases:
            with self.subTest(engine=engine):
                result, status, relay = self.dispatch(engine, out, err)
                self.assertFalse(result[0])
                self.assertEqual(status, "unknown")
                relay.assert_not_called()


class TestCatalogIsMirrorableData(unittest.TestCase):
    """router/cli.go mirrors CLI_MESSAGE_CATALOG, so every pattern must be RE2 syntax."""

    def test_patterns_are_re2_compatible_and_compile(self):
        kinds = {"quota", "rate", "overload", "auth", "region"}
        for provider, table in health.CLI_MESSAGE_CATALOG.items():
            self.assertIn(provider, health.CLI_OWN_STREAMS)
            for kind, patterns in table.items():
                self.assertIn(kind.replace(".marked", ""), kinds)
                for pattern in patterns:
                    with self.subTest(provider=provider, kind=kind, pattern=pattern):
                        self.assertIsNone(re.search(r"\(\?[=!<]|\(\?P|\\[1-9]|\(\?-|\(\?>|[*+?}]\+", pattern))
                        re.compile(pattern, re.IGNORECASE)


if __name__ == "__main__":
    unittest.main()
