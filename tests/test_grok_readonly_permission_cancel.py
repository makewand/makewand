"""An unattended Grok turn cancelled by a permission prompt exits 0; it is not a result."""
try:
    import _isolation
except ImportError:
    from tests import _isolation

import fnmatch
import json
import unittest
from unittest.mock import patch

from makewand.execution_contract import ExecutionResult
from makewand.providers import grok
from makewand.providers.grok import execute_grok_task
try:
    from test_multi_model_optimization import _FakeProviderHome
except ImportError:  # python3 -m unittest tests.<module>
    from tests.test_multi_model_optimization import _FakeProviderHome


def _reply(text, stop):
    return json.dumps({"text": text, "stopReason": stop, "sessionId": "s", "requestId": "r", "num_turns": 1})


def _values(command, flag):
    return [command[i + 1] for i, value in enumerate(command) if value == flag]


class GrokReadonlyReplyTests(unittest.TestCase):
    def _run(self, result, **kwargs):
        with _FakeProviderHome() as home, patch("makewand.sandbox.is_bwrap_available", return_value=True), \
                patch("makewand.sandbox._provider_socket_preflight"), \
                patch("makewand.providers.grok.run_subprocess", return_value=result) as run:
            kwargs.setdefault("readonly", True)
            outcome = execute_grok_task("review", cwd=str(home.workspace), **kwargs)
        return outcome, run.call_args[0][0]

    def test_a_finished_turn_returns_only_the_text(self):
        (ok, text, error), command = self._run((0, _reply("the answer", "end_turn"), "", None))
        self.assertEqual((ok, text, error), (True, "the answer", None))
        self.assertEqual(command[command.index("--output-format") + 1], "json")

    def test_a_cancelled_turn_is_a_known_failure(self):
        outcome, _ = self._run((0, _reply("", "cancelled"), "", None))
        self.assertIsInstance(outcome, ExecutionResult)
        self.assertFalse(outcome.success)
        self.assertEqual((outcome.status, outcome.error_kind, outcome.outcome_known), ("FAILED", "incomplete_turn", True))
        self.assertIn("cancelled", outcome.error)

    def test_every_incomplete_stop_reason_fails(self):
        for stop in ("refusal", "max_tokens", "max_turn_requests"):
            outcome, _ = self._run((0, _reply("partial", stop), "", None))
            self.assertFalse(outcome.success, stop)
            self.assertEqual(outcome.output, "partial")

    def test_output_that_is_not_the_json_object_is_kept(self):
        for output in ("plain words", "[1, 2]", '{"stopReason": "end_turn"}'):
            (ok, text, _), _ = self._run((0, output, "", None))
            self.assertEqual((ok, text), (True, output))

    def test_a_cancelled_turn_without_text_still_fails(self):
        outcome, _ = self._run((0, '{"stopReason": "cancelled"}', "", None))
        self.assertFalse(outcome.success)
        self.assertEqual(outcome.error_kind, "incomplete_turn")

    def test_a_json_error_still_reaches_limit_detection(self):
        message = "API error (status 402 Payment Required): limit reached"
        with patch("makewand.providers.grok.parse_grok_quota", return_value=(False, "", None)) as parse:
            self._run((1, json.dumps({"type": "error", "message": message}), "", None))
        self.assertEqual(parse.call_args[0][0], message)

    def test_a_limit_in_a_json_error_is_recorded_with_stderr_captured_apart(self):
        message = "You've reached your free Grok Build usage limit"
        for stderr in ("", "unrelated line"):
            self.assertEqual(grok.parse_grok_quota(message, stderr)[0], False)
            with patch("makewand.health.record_engine_limit") as record:
                self._run((1, json.dumps({"type": "error", "message": message}), stderr, None))
            record.assert_called_once()

    def test_a_failing_answer_on_stdout_is_not_read_as_a_limit(self):
        with patch("makewand.health.record_engine_limit") as record:
            self._run((1, "You've reached your free Grok Build usage limit", "", None))
        record.assert_not_called()

    def test_streaming_stays_plain_text(self):
        (ok, text, _), command = self._run((0, "live words", "", None), stream=True)
        self.assertEqual((ok, text), (True, "live words"))
        self.assertEqual(command[command.index("--output-format") + 1], "plain")

    def test_a_writable_task_gets_no_read_only_rules(self):
        (ok, text, _), command = self._run((0, "done", "", None), readonly=False)
        self.assertEqual((ok, text), (True, "done"))
        self.assertEqual(_values(command, "--allow") + _values(command, "--deny"), [])
        self.assertEqual(command[command.index("--output-format") + 1], "plain")

    def test_read_only_flags_cover_pipelines_and_refuse_what_can_write_or_execute(self):
        _, command = self._run((0, _reply("ok", "end_turn"), "", None))
        allow, deny = _values(command, "--allow"), _values(command, "--deny")
        for rule in ("Bash(cat *)", "Bash(sha256sum)", "Bash(sha256sum *)", "Bash(git diff)", "Bash(git rev-parse)"):
            self.assertIn(rule, allow)
        for rule in ("Bash(rg *--pre*)", "Bash(sort *--co*)", "Bash(rg *--hostname-bin*)", "Bash(git branch [a-zA-Z0-9_]*)", "Bash(git * --ext*)", "Bash(git difftool*)", "Bash(git branch *--d*)", "Bash(git branch *--t*)",
                     "Bash(sort *-[a-zA-Z]o*)", "Bash(uniq * [a-zA-Z0-9_./~]*)", "Bash(git log[a-zA-Z0-9_.-]*)", "Bash(sha256sum[a-zA-Z0-9_.-]*)"):
            self.assertIn(rule, deny)
        for name in ("sed", "find", "awk", "python", "xargs", "tee", "bash", "sh", "file"):
            self.assertFalse([rule for rule in allow if rule in (f"Bash({name})", f"Bash({name} *)")], name)
        # A rule without a trailing space is a bare prefix: "tr" would also approve truncate.
        for name in ("tr", "ps", "ls", "cat", "df", "du", "cut", "nl"):
            self.assertNotIn(f"Bash({name})", allow)


class GrokReadonlyRuleMatchingTests(unittest.TestCase):
    """fnmatch has the *, ? and [...] globs of Grok's rule matching."""

    def _denied(self, command):
        return any(fnmatch.fnmatchcase(command, rule[5:-1]) for rule in grok._READONLY_DENY_RULES)

    def test_commands_that_run_programs_or_write_are_denied(self):
        for command in ("git logattack", "sha256sum-evil x", "sorted", "sort -uo out", "sort -ro out", "sort -o out",
                        "sort --output=out", "sort --compress-p=x big", "uniq in out", "uniq -c in out",
                        "git branch new", "git branch new HEAD", "git branch --track new HEAD", "git branch -t new HEAD",
                        "git branch --del old", "git branch -vd old", "git branch -a -D old", "git branch --no-track new", "git branch --set-up=origin/x",
                        "git log --outp=x", "git diff --ext-d", "git log --textc", "rg --pre=x y", "rg --hostname-bin=x y"):
            self.assertTrue(self._denied(command), command)

    def test_ordinary_read_commands_are_not_denied(self):
        for command in ("git log --oneline -5", "git diff HEAD~1 HEAD", "git status --short", "git branch", "git branch -a",
                        "git branch -v --contains HEAD", "git branch --list", "git branch --format=%(refname)", "git branch --merged", "git show HEAD:README.md", "git rev-parse HEAD",
                        "sort", "sort -u", "sort -k1,1 -n file.txt", "sort -r report.txt", "uniq", "uniq -c",
                        "sha256sum", "sha256sum a.txt", "rg -n pattern src", "grep -- -o file"):
            self.assertFalse(self._denied(command), command)


if __name__ == "__main__":
    unittest.main()
