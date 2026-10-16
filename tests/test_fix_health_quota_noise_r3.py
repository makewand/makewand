"""
quota-noise, round 3: a provider CLI's quota or login verdict is read only from
the lines the CLI writes itself (makewand/cli_message_catalog.json).

- Muse Code reads both streams, so a model or tool line on its answer stream
  ("Authentication failed.", "Rate limited - ...", "Usage limit reached.",
  "HTTP 429 Too Many Requests", "Missing meta credentials are now ...") is not
  its verdict: only its own formats ("<kind> (HTTP <status> · attempt <n>)",
  "not logged in: run /login to add an API key", "Invalid API key (HTTP 401)")
  in the error report it ends with count.
- codex exec's transcript (stderr) echoes the prompt, the agent's messages and
  the output of every command it ran; none of that is codex's, even a tool's
  own "ERROR:" or timestamped log line. codex's "ERROR: <message>" events count.
- Stream mode keeps stderr apart from the answer (run_subprocess
  separate_stderr); where a stream is merged anyway only the error report the
  CLI ends with counts.
- The catalog is one data file that router/cli.go mirrors: RE2 syntax only.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import contextlib
import io
import json
import re
import subprocess
import sys
import tempfile
import unittest
from datetime import datetime, timedelta
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
from makewand.providers import agy, base, claude, codex, grok, muse
from makewand.providers.claude import parse_claude_quota
from makewand.providers.codex import parse_codex_quota
from makewand.providers.muse import parse_muse_quota

REPO_ROOT = Path(__file__).resolve().parent.parent
CATALOG_FILE = REPO_ROOT / "makewand" / "cli_message_catalog.json"
A = "\u2019"
CODEX_HDR = ("OpenAI Codex v0.162.1 (research preview)\n--------\nworkdir: /repo\nmodel: gpt-6\nprovider: openai\n"
             "approval: never\nsandbox: read-only\nreasoning effort: high\nreasoning summaries: auto\n"
             "session id: 0199a1b2-c3d4-7e8f-9a0b-1c2d3e4f5a6b\n--------\nuser\nreview this\n\n")
CODEX_GENERIC = "ERROR: stream disconnected before completion: error sending request\n"
KEYRING = "I1012 08:11:57.965555       1 auth.go:157] ChainedAuth: authenticated via keyring (effective: keyring)\n"
GENERIC_ERR = {"codex": CODEX_HDR + CODEX_GENERIC, "grok": "Error: stream closed before the turn finished\n",
               "muse": "error: turn failed\n", "agy": KEYRING + "Error: UNAVAILABLE (code 503): The service is "
                                                                 "currently unavailable.\n"}

# Model prose and tool output (round-2 verifier corpus) that is never a CLI's verdict.
PROSE = [
    "Rate limited - the gh CLI refused my request, so I skipped the PR step.",
    "Rate limited \u2014 GitHub refused the request, I used the cache.",
    "Authentication failed - the test now expects a 401.",
    "Authentication failed.",
    "Usage limit reached.",
    "Rate limited.",
    "Usage limit reached \u00b7 the banner now shows the reset time.",
    "Quota exceeded. Check your plan and billing details.",
    "You've hit your usage limit.",
    "Rate limit reached for gpt-4o in organization org-abc on tokens per min (TPM): Limit 30000, Used 30000.",
    "You exceeded your current quota, please check your plan and billing details.",
    "Resource has been exhausted (e.g. check quota).",
    "Request had invalid authentication credentials. The fixture now returns that.",
    "Missing meta credentials are now reported with a clear error.",
    "HTTP 429 Too Many Requests",
    "Session expired. Run `grok login` again is what the docs say.",
    "Open this page to sign in: is the new prompt label.",
    "Google Pay is not available in your country, so checkout falls back to cards.",
    "Gemini Nano is not supported in your region yet; I gated the feature.",
]
TOOL = [
    "ERROR: 401 Client Error: Unauthorized for url: https://pypi.internal/simple/x/",
    "Error: 429 Too Many Requests",
    "ERROR: 429 Too Many Requests",
    "ERROR:__main__:OpenAI call failed: Rate limit reached for gpt-4o in organization org-abc on tokens per min (TPM): "
    "Limit 30000",
    "ERROR:root:upstream said: You exceeded your current quota, please check your plan and billing details.",
    "2026-10-12 08:00:00 ERROR rate limit reached for gpt-4o on requests per min",
    "Authentication failed.",
    "Usage limit reached.",
    "Rate limited.",
]

# Muse Code 1.4.6's own failure texts (strings of the binary).
MUSE_REAL = [
    ("LIM", "Error: usage limit reached (HTTP 429 \u00b7 attempt 1)"),
    ("LIM", "usage limit reached (HTTP 429 \u00b7 attempt 3)"),
    ("LIM", "rate limited (HTTP 429 \u00b7 attempt 2)"),
    ("LIM", "request failed (HTTP 429 \u00b7 attempt 1)"),
    ("AUTH", "authentication failed (HTTP 401 \u00b7 attempt 1)"),
    ("AUTH", "not logged in: run /login to add an API key"),
    ("AUTH", "Invalid API key (HTTP 401)"),
    ("AUTH", "missing meta credentials: set META_API_KEY or run muse login"),
    ("AUTH", "Not logged in \u2014 run /login to get started, or set META_API_KEY"),
    ("AUTH", "Open this page to sign in: https://www.meta.ai/device"),
    ("AUTH", "No Meta credentials were found. Run `muse login`."),
    ("AUTH", "still unauthorized after a token refresh; run `muse login` again"),
    ("AUTH", "your saved API key was rejected \u2014 run `muse login` or add a new key"),
    ("AUTH", "The login request was denied."),
]
# codex 0.162.1's own error events.
CODEX_REAL = [
    f"ERROR: You{A}ve hit your usage limit. Upgrade to Plus to continue using Codex "
    "(https://chatgpt.com/explore/plus), or try again at 8:00 PM.",
    "ERROR: Your workspace is out of credits. Add credits to continue.",
    "ERROR: You hit your spend cap set in your workspace. Increase your spend cap to continue.",
    "ERROR: Quota exceeded. Check your plan and billing details.",
    "ERROR: exceeded retry limit, last status: 429 Too Many Requests, request id: req_123",
    "ERROR: unexpected status 429 Too Many Requests: {\"error\":{\"type\":\"usage_limit_reached\"}}",
    "ERROR: stream disconnected before completion: Rate limit reached for gpt-6 in organization org-x on tokens per "
    "min (TPM): Limit 30000, Used 30000. Please try again in 2s.",
    f"2026-10-12T08:00:00.123456Z ERROR codex_core::codex: Turn error: You{A}ve hit your usage limit. Try again at "
    "8:00 PM.",
]


def muse_verdict(output, stderr=None):
    limited, _reason, resets = parse_muse_quota(output, stderr) if stderr is not None else parse_muse_quota(output)
    return "-" if not limited else "AUTH" if resets == "需登录授权" else "LIM"


class TestMuseCountsOnlyItsOwnFormats(unittest.TestCase):
    def test_model_and_tool_lines_are_not_muse_verdicts(self):
        for line in PROSE + TOOL:
            with self.subTest(line=line):
                # Captured apart (muse's split is unknown, so both streams are read) ...
                self.assertEqual(muse_verdict(f"I made the change.\n{line}\nDone.\n", "error: turn failed\n"), "-")
                self.assertEqual(muse_verdict(f"Ran the tests:\n{line}\nI stopped there.\n", "error: turn failed\n"),
                                 "-")
                # ... or merged (stream mode), the CLI's own error first or last.
                self.assertEqual(muse_verdict(f"error: turn failed\nI made the change.\n{line}\nDone.\n"), "-")
                self.assertEqual(muse_verdict(f"I made the change.\n{line}\nerror: turn failed\n"), "-")

    def test_muse_code_own_messages(self):
        for expected, message in MUSE_REAL:
            with self.subTest(message=message):
                self.assertEqual(muse_verdict("", message + "\n"), expected)
                self.assertEqual(muse_verdict(message + "\n", ""), expected)
                self.assertEqual(muse_verdict("Working on it.\n" + message + "\n"), expected)
                self.assertEqual(muse_verdict("partial answer\n", message + "\n"), expected)

    def test_loose_shapes_of_muse_labels_do_not_count(self):
        for message in ("Rate limited.", "Usage limit reached.", "Authentication failed.",
                        "Authentication failed - the test now expects a 401.", "Rate limited \u00b7 retry later",
                        "missing meta credentials", "Open this page to sign in: is the new prompt label."):
            with self.subTest(message=message):
                self.assertEqual(muse_verdict("", message + "\n"), "-")


class TestCodexTranscriptStructure(unittest.TestCase):
    """codex exec writes its transcript to stderr; only its own events count."""

    def test_agent_messages_and_prompt_echo_are_content(self):
        for line in PROSE:
            with self.subTest(line=line):
                transcript = CODEX_HDR + f"codex\nI made the change.\n{line}\n\n" + CODEX_GENERIC
                self.assertFalse(parse_codex_quota(f"I made the change.\n{line}\n", transcript)[0])
                echo = CODEX_HDR.replace("review this\n", f"review this diff:\n {line}\n") + CODEX_GENERIC
                self.assertFalse(parse_codex_quota("", echo)[0])
                # Stream mode merged by the runner: the transcript, then the answer.
                self.assertFalse(parse_codex_quota(transcript + f"I made the change.\n{line}\nDone.\n")[0])

    def test_relayed_command_output_is_content(self):
        for line in TOOL:
            for output in (f"{line}\nmore output\n", f"ERROR: build failed\n{line}\n"):
                with self.subTest(output=output):
                    transcript = (CODEX_HDR + "exec\nbash -lc 'make test' in /repo exited 1 in 1.10s:\n" + output
                                  + "\n" + CODEX_GENERIC)
                    self.assertFalse(parse_codex_quota("", transcript)[0])

    def test_codex_own_events_count_after_every_section(self):
        for event in CODEX_REAL:
            for section in ("", "codex\nI will run the tests.\n\n",
                            "exec\nbash -lc 'make test' in /repo exited 1 in 1.10s:\nERROR: build failed\n\n",
                            "thinking\nThe build failed; retry.\n"):
                with self.subTest(event=event[:40], section=section[:12]):
                    self.assertTrue(parse_codex_quota("", CODEX_HDR + section + event + "\n")[0])

    def test_prompt_echo_is_skipped_when_the_prompt_is_known(self):
        # A review prompt may quote codex's own error texts (here: a test log).
        prompt = ("Fix the failing test. Its log:\nERROR: You've hit your usage limit.\n```\n"
                  "ERROR: exceeded retry limit, last status: 429\n```")
        transcript = CODEX_HDR.replace("review this\n", prompt + "\n") + CODEX_GENERIC
        self.assertFalse(parse_codex_quota("", transcript, prompt=prompt)[0])
        # codex's own lines right after the echo still count.
        warning = "\u26a0 Heads up, you have less than 5% of your weekly limit left. Run /status for a breakdown.\n"
        self.assertTrue(parse_codex_quota("", CODEX_HDR + warning, prompt="review this")[0])
        self.assertTrue(parse_codex_quota("", CODEX_HDR + "ERROR: 429 Too Many Requests\n", prompt="review this")[0])

    def test_an_indented_copy_of_an_event_is_content(self):
        transcript = CODEX_HDR + "codex\nThe diff:\n ERROR: You've hit your usage limit.\n" + CODEX_GENERIC
        self.assertFalse(parse_codex_quota("", transcript)[0])

    def test_text_without_transcript_structure_keeps_the_legacy_reading(self):
        self.assertTrue(parse_codex_quota("You have hit your usage limit. Try again at 10:58 AM.")[0])
        self.assertTrue(parse_codex_quota("", "HTTP 429 Too Many Requests")[0])
        self.assertTrue(parse_codex_quota("Error: turn failed\n\nCaused by:\n    0: request failed\n"
                                          f"    1: You{A}ve hit your usage limit. Try again at 8:00 PM.\n")[0])


class DispatchCase(unittest.TestCase):
    MODULES = {"grok": grok, "muse": muse, "agy": agy, "codex": codex, "claude": claude}

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-fix-qn3-")
        root = Path(self._tmp.name)
        for p in (patch.object(config, "CONFIG_DIR", root / "cfg"),
                  patch.object(health, "STATUS_CACHE_FILE", root / "cfg" / "status.json"),
                  patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json")):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        (root / "cfg").mkdir(parents=True)

    def dispatch(self, engine, runner, *, stream=False, prompt="review this"):
        module = self.MODULES[engine]
        with patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("makewand.sandbox._agy_host_credits_disabled", return_value=True), \
             patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(True, "config-ack")), \
             patch("makewand.sandbox.audit_unsafe_host_exec"), \
             patch("makewand.providers.muse.detect_muse_guard", return_value=(False, None)), \
             patch.object(module, "run_subprocess", side_effect=runner), \
             contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            result = getattr(module, f"execute_{engine}_task")(prompt, cwd=self._tmp.name, readonly=True,
                                                               stream=stream)
        status = health.load_status_cache().get(engine, {}).get("status")
        if health.STATUS_CACHE_FILE.exists():
            health.STATUS_CACHE_FILE.unlink()
        return result, status


def captured(code, out, err):
    """A run_subprocess double: plain strings, the historical contract (stream merges stderr)."""
    return lambda *args, **kwargs: (code, out, err, None)


def real_cli(code, out, err, calls):
    """The real runner on a fake CLI that writes `out` to stdout and `err` to stderr."""
    script = ("import sys\nsys.stdout.write(%r)\nsys.stdout.flush()\nsys.stderr.write(%r)\nsys.stderr.flush()\n"
              "sys.exit(%d)\n" % (out, err, code))

    def run(cmd, **kwargs):
        calls.append(kwargs)
        return base.run_subprocess([sys.executable, "-I", "-c", script], **kwargs)
    return run


class TestDispatchReadsOnlyTheCliOwnLines(DispatchCase):
    def test_round_two_noise_cases_leave_the_provider_unknown(self):
        cases = [
            ("muse", "I made the change.\nAuthentication failed - the test now expects a 401.\nDone.\n", GENERIC_ERR["muse"]),
            ("muse", "I made the change.\nAuthentication failed.\nDone.\n", GENERIC_ERR["muse"]),
            ("muse", "Ran the tests:\nAuthentication failed.\nI stopped there.\n", GENERIC_ERR["muse"]),
            ("muse", "I made the change.\nHTTP 429 Too Many Requests\nDone.\n", GENERIC_ERR["muse"]),
            ("muse", "I made the change.\nMissing meta credentials are now reported with a clear error.\nDone.\n",
             GENERIC_ERR["muse"]),
            ("codex", "I made the change.\nQuota exceeded. Check your plan and billing details.\n",
             CODEX_HDR + "codex\nI made the change.\nQuota exceeded. Check your plan and billing details.\n\n"
             + CODEX_GENERIC),
            ("codex", "", CODEX_HDR.replace("review this\n", "review this diff:\n You've hit your usage limit.\n")
             + CODEX_GENERIC),
            ("codex", "", CODEX_HDR + "exec\nbash -lc 'make test' in /repo exited 1 in 1.10s:\nERROR:__main__:OpenAI "
             "call failed: Rate limit reached for gpt-4o in organization org-abc on tokens per min (TPM): Limit 30000\n"
             "more output\n\n" + CODEX_GENERIC),
            ("codex", "", CODEX_HDR + "exec\nbash -lc 'make test' in /repo exited 1 in 1.10s:\nERROR: build failed\n"
             "Error: 429 Too Many Requests\n\n" + CODEX_GENERIC),
        ]
        for engine, out, err in cases:
            for stream in (False, True):
                with self.subTest(engine=engine, out=out[:50], err=err[-60:], stream=stream):
                    merged = err + out if stream else out  # stream: the runner merged stderr first
                    result, status = self.dispatch(engine, captured(1, merged, "" if stream else err), stream=stream)
                    self.assertFalse(result[0])
                    self.assertEqual(status, "unknown")

    def test_own_messages_still_mark_the_provider(self):
        cases = [("muse", "", "not logged in: run /login to add an API key\n", "needs_auth"),
                 ("muse", "", "Invalid API key (HTTP 401)\n", "needs_auth"),
                 ("muse", "partial\n", "usage limit reached (HTTP 429 \u00b7 attempt 3)\n", "limited"),
                 ("codex", "", CODEX_HDR + CODEX_REAL[0] + "\n", "limited"),
                 ("codex", "", CODEX_HDR + "exec\nbash -lc 'make test' in /repo exited 1 in 1.10s:\nFAILED x\n"
                  + CODEX_REAL[4] + "\n", "limited")]
        for engine, out, err, expected in cases:
            for stream in (False, True):
                with self.subTest(engine=engine, err=err[-60:], stream=stream):
                    # Merged in time order: codex logs its transcript before the
                    # answer, the others end with their error.
                    merged = (err + out if engine == "codex" else out + err) if stream else out
                    _result, status = self.dispatch(engine, captured(1, merged, "" if stream else err), stream=stream)
                    self.assertEqual(status, expected)


class TestStreamModeKeepsStderrApart(DispatchCase):
    def test_runner_keeps_stderr_apart_when_asked(self):
        script = "import sys; sys.stdout.write('answer line\\n'); sys.stdout.flush(); sys.stderr.write('Error: x\\n')"
        shown = io.StringIO()
        with contextlib.redirect_stdout(shown):
            code, out, err, _ = base.run_subprocess([sys.executable, "-I", "-c", script], timeout=30, stream=True,
                                                    separate_stderr=True)
        self.assertEqual((code, out, str(err)), (0, "answer line\n", "Error: x\n"))
        self.assertTrue(base.stderr_captured_apart(err, True))
        self.assertIn("answer line", shown.getvalue())
        self.assertIn("Error: x", shown.getvalue())  # the CLI's own lines are still shown live
        # Without separate_stderr stream mode still merges, and says so.
        with contextlib.redirect_stdout(io.StringIO()):
            code, out, err, _ = base.run_subprocess([sys.executable, "-I", "-c", script], timeout=30, stream=True)
        self.assertIn("Error: x", out)
        self.assertEqual(str(err), "")
        self.assertFalse(base.stderr_captured_apart(err, True))
        # Test doubles returning plain strings keep the historical contract.
        self.assertFalse(base.stderr_captured_apart("", True))
        self.assertTrue(base.stderr_captured_apart("", False))

    def test_stream_dispatch_reads_the_cli_stderr_not_the_answer(self):
        answer = "I made the change.\nYou have exhausted your quota on this model.\nUsage limit reached.\n" \
                 "Error: UNAUTHENTICATED: the interceptor now returns this for missing tokens.\nDone.\n"
        own_limit = {"codex": CODEX_HDR + CODEX_REAL[0] + "\n",
                     "grok": f"Error: You{A}ve reached your free Grok Build usage limit for now.\n",
                     "muse": "usage limit reached (HTTP 429 \u00b7 attempt 3)\n",
                     "agy": KEYRING + "Error: RESOURCE_EXHAUSTED (code 429): You have exhausted your quota on this "
                                      "model.\n"}
        for engine in ("codex", "grok", "muse", "agy"):
            with self.subTest(engine=engine):
                calls = []
                result, status = self.dispatch(engine, real_cli(1, answer, GENERIC_ERR[engine], calls), stream=True)
                self.assertTrue(calls[0]["stream"])
                self.assertTrue(calls[0]["separate_stderr"])
                self.assertFalse(result[0])
                self.assertEqual(status, "unknown")
                _result, status = self.dispatch(engine, real_cli(1, answer, own_limit[engine], []), stream=True)
                self.assertEqual(status, "limited")

    def test_merged_stream_counts_only_the_final_error_report(self):
        cases = [("grok", "I made the change.\nSession expired. Run `grok login` again is what the docs say.\n",
                  "Error: stream closed before the turn finished\n"),
                 ("grok", "I made the change.\nHTTP 429 Too Many Requests\n", "Error: stream closed\n"),
                 ("agy", KEYRING + "Partial answer.\nError: UNAUTHENTICATED: the interceptor now returns this.\n",
                  "Error: UNAVAILABLE (code 503)\n"),
                 ("agy", KEYRING + "Partial answer.\nResource has been exhausted (e.g. check quota).\n",
                  "Error: UNAVAILABLE (code 503)\n"),
                 ("agy", "I made the change.\nRequest had invalid authentication credentials. The fixture returns that.\n",
                  "Error: UNAVAILABLE (code 503): The service is currently unavailable.\n"),
                 ("muse", "I made the change.\nRate limited.\n", "error: turn failed\n")]
        for engine, answer, own in cases:
            with self.subTest(engine=engine, answer=answer[-50:]):
                result, status = self.dispatch(engine, captured(1, answer + own, ""), stream=True)
                self.assertFalse(result[0])
                self.assertEqual(status, "unknown")


class TestClaudeEpochReset(unittest.TestCase):
    def test_epoch_reset_in_the_future_stays_limited(self):
        # RC17 of the round-2 corpus uses 1760000000 (2025-10-09), already past:
        # the limit then heals at once. A future reset keeps it.
        future = int((datetime.now() + timedelta(days=2)).timestamp())
        limited, _reason, resets = parse_claude_quota(f"Claude AI usage limit reached|{future}\n", "")
        self.assertTrue(limited)
        self.assertFalse(health.is_reset_time_passed(resets, datetime.now().isoformat()))


def _catalog_strings(node, path=()):
    """Every pattern string of the catalog file, with its path."""
    if isinstance(node, dict):
        for key, value in node.items():
            if key in ("_doc", "names", "own_streams", "answer_streams", "selection", "version"):
                continue
            yield from _catalog_strings(value, path + (key,))
    elif isinstance(node, list):
        for index, value in enumerate(node):
            yield from _catalog_strings(value, path + (index,))
    elif isinstance(node, str):
        yield "/".join(map(str, path)), node


class TestCatalogIsOneMirrorableDataFile(unittest.TestCase):
    def setUp(self):
        self.catalog = json.loads(CATALOG_FILE.read_text(encoding="utf-8"))

    def test_health_reads_the_data_file(self):
        self.assertEqual(health.CLI_MESSAGE_CATALOG_FILE.resolve(), CATALOG_FILE.resolve())
        providers = self.catalog["providers"]
        self.assertEqual(set(providers), {"claude", "codex", "grok", "muse", "agy", "gemini"})
        self.assertEqual(health.CLI_MESSAGE_CATALOG,
                         {p: {k: tuple(v) for k, v in e["kinds"].items()} for p, e in providers.items()})
        for provider, entry in providers.items():
            with self.subTest(provider=provider):
                self.assertEqual(health.CLI_OWN_STREAMS[provider], tuple(entry["own_streams"]))
                self.assertEqual(health.CLI_NAMES[provider], tuple(entry["names"]))
                self.assertLessEqual(set(entry["own_streams"]) | set(entry["answer_streams"]), {"stdout", "stderr"})
                self.assertEqual(set(entry["selection"]), {"apart", "merged"})
                self.assertLessEqual(set(entry["selection"].values()), {"lines", "final_report", "transcript"})
                self.assertEqual("transcript" in entry["selection"].values(), "transcript" in entry)
                self.assertIn(entry.get("json_reports", False), (True, False))
                for kind in entry["kinds"]:
                    self.assertIn(kind.replace(".marked", ""), {"quota", "rate", "overload", "auth", "region"})
        # Only codex exec writes a transcript (its rules are read by health._transcript_rules).
        self.assertEqual([p for p, entry in providers.items() if "transcript" in entry], ["codex"])

    def test_every_pattern_is_re2_syntax_and_compiles(self):
        strings = list(_catalog_strings(self.catalog))
        self.assertGreater(len(strings), 100)
        for path, pattern in strings:
            with self.subTest(path=path, pattern=pattern):
                # RE2: no lookaround, backreference, atomic/possessive group, conditional or \Z.
                self.assertIsNone(re.search(r"\(\?[=!]|\(\?<[=!]|\(\?P=|\(\?\(|\\[1-9]|\(\?>|[*+?}]\+|\\Z", pattern))
                health.cli_catalog_regex(pattern)

    def test_the_package_ships_the_catalog(self):
        with tempfile.TemporaryDirectory(prefix="mw-stage-") as out:
            proc = subprocess.run([sys.executable, "-I", str(REPO_ROOT / "scripts" / "stage_python_engine.py"),
                                   str(Path(out) / "bundle"), "0.0.0-test"], capture_output=True, text=True,
                                  timeout=120)
            self.assertEqual(proc.returncode, 0, proc.stderr)
            staged = Path(out) / "bundle" / "lib" / "makewand" / "python" / "makewand" / "cli_message_catalog.json"
            self.assertEqual(staged.read_bytes(), CATALOG_FILE.read_bytes())


if __name__ == "__main__":
    unittest.main()
