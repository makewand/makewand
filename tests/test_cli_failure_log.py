"""UNKNOWN provider exits keep a bounded, redacted tail of the CLI's stderr."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import cli_failure_log
from makewand.providers.base import model_process_failure


class CliFailureLogTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        evidence = Path(self.tmp.name) / "cache" / "execution-evidence"
        env = patch.dict(os.environ, {"MAKEWAND_EXECUTION_EVIDENCE_DIR": str(evidence)})
        env.start()
        self.addCleanup(env.stop)
        self.log = evidence.parent / cli_failure_log.LOG_NAME

    def entries(self):
        if not self.log.exists():
            return []
        return [json.loads(line) for line in self.log.read_text().splitlines()]

    def test_unknown_exit_keeps_the_last_stderr_lines_owner_only(self):
        stderr = "\n".join(f"line {i}" for i in range(30)) + "\nError: 402 Grok Build usage balance exhausted\n"
        result = model_process_failure("grok", 1, "answer text", stderr)
        self.assertEqual(result.status, "UNKNOWN")
        [entry] = self.entries()
        self.assertEqual((entry["engine"], entry["exit_code"]), ("grok", 1))
        self.assertEqual(len(entry["stderr_tail"]), cli_failure_log.MAX_LINES)
        self.assertEqual(entry["stderr_tail"][-1], "Error: 402 Grok Build usage balance exhausted")
        self.assertNotIn("answer text", self.log.read_text())
        if os.name != "nt":
            self.assertEqual(os.stat(self.log).st_mode & 0o777, 0o600)

    def test_timeout_and_empty_stderr_are_not_logged(self):
        class Timeout(Exception):
            execution_status = "TIMEOUT"
        model_process_failure("codex", -9, "", "killed", Timeout())
        model_process_failure("codex", 1, "", "")
        model_process_failure("codex", 1, "", None)
        self.assertEqual(self.entries(), [])

    def test_secrets_are_redacted_and_lines_shortened(self):
        cli_failure_log.record("grok", 1, "Authorization: Bearer abc.def\nkey sk-" + "a" * 30
                               + "\n" + "x" * 1000 + "\nGITHUB github_pat_" + "B" * 20)
        [entry] = self.entries()
        text = json.dumps(entry)
        for secret in ("abc.def", "sk-aaaa", "github_pat_BBBB"):
            self.assertNotIn(secret, text)
        self.assertTrue(all(len(line) <= cli_failure_log.MAX_LINE_CHARS for line in entry["stderr_tail"]))

    def test_log_is_cut_back_to_its_newer_half_past_the_cap(self):
        with patch.object(cli_failure_log, "MAX_BYTES", 2000):
            for i in range(60):
                cli_failure_log.record("muse", i, f"failure number {i}")
        entries = self.entries()
        self.assertLessEqual(self.log.stat().st_size, 2000)
        self.assertEqual(entries[-1]["exit_code"], 59)
        self.assertGreater(len(entries), 3)
        self.assertEqual([e["exit_code"] for e in entries], sorted(e["exit_code"] for e in entries))

    def test_an_already_oversized_log_keeps_its_newest_entries(self):
        self.log.parent.mkdir(parents=True)
        self.log.write_text("".join(json.dumps({"exit_code": i, "stderr_tail": [f"failure number {i}"]}) + "\n"
                                    for i in range(100)))
        self.assertGreater(self.log.stat().st_size, 2000)
        with patch.object(cli_failure_log, "MAX_BYTES", 2000):
            cli_failure_log.record("grok", 100, "Error: latest failure")
        entries = self.entries()
        self.assertLessEqual(self.log.stat().st_size, 2000)
        self.assertEqual(entries[-2]["exit_code"], 99)
        self.assertEqual(entries[-1]["exit_code"], 100)
        self.assertTrue(all(entry["exit_code"] >= 80 for entry in entries))

    def test_a_partial_last_entry_is_dropped_before_appending(self):
        self.log.parent.mkdir(parents=True)
        for count in (0, 1, 100):
            with self.subTest(complete_entries=count):
                complete = "".join(json.dumps({"exit_code": i, "stderr_tail": [f"failure number {i}"]}) + "\n"
                                   for i in range(count))
                self.log.write_text(complete + '{"partial": ')
                with patch.object(cli_failure_log, "MAX_BYTES", 2000):
                    cli_failure_log.record("grok", count, "Error: latest failure")
                entries = self.entries()
                self.assertLessEqual(self.log.stat().st_size, 2000)
                self.assertEqual(entries[-1]["exit_code"], count)
                if count:
                    self.assertEqual(entries[-2]["exit_code"], count - 1)
                else:
                    self.assertEqual(len(entries), 1)

    def test_only_the_clis_own_lines_are_kept(self):
        header = ("OpenAI Codex v0.162.1 (research preview)\n--------\nworkdir: /repo\nmodel: gpt-6\n"
                  "session id: 0199a1b2-c3d4-7e8f-9a0b-1c2d3e4f5a6b\n--------\nuser\nSECRET PROMPT\n\n")
        stderr = (header + "codex\nTHE ANSWER\n\nexec\nbash -lc 'cat .env' in /repo succeeded:\nFILE CONTENT\n\n"
                  "ERROR: stream disconnected before completion: error sending request\n")
        cli_failure_log.record("codex", 1, stderr)
        [entry] = self.entries()
        self.assertEqual(entry["stderr_tail"], ["ERROR: stream disconnected before completion: error sending request"])

    def test_long_codex_sections_are_classified_before_taking_the_tail(self):
        own_error = "ERROR: stream disconnected before completion: error sending request"
        for section in ("user", "thinking", "exec", "codex"):
            for ending in ("", "tokens used\n1234\n"):
                with self.subTest(section=section, ending=ending):
                    # Each section exceeds the old 64 KiB pre-classification slice.
                    body = f"private {section} content\n" * 5000 + "Error: private detail\nprivate continuation\n"
                    stderr = f"OpenAI Codex v0.162.1\n{section}\n" + body + ending
                    self.assertEqual(cli_failure_log.tail_lines("codex", stderr), [])
                    self.assertEqual(cli_failure_log.tail_lines("codex", stderr + own_error + "\n"), [own_error])

    def test_uncatalogued_engines_never_log_stderr(self):
        for engine in ("aider", "unknown-provider"):
            with self.subTest(engine=engine):
                stderr = "private model output\nError: failed"
                self.assertEqual(cli_failure_log.tail_lines(engine, stderr), [])
                model_process_failure(engine, 1, "", stderr)
        self.assertEqual(self.entries(), [])

    def test_merged_streams_are_not_logged(self):
        from makewand.providers.base import CapturedStderr
        cli_failure_log.record("grok", 1, CapturedStderr("the model answer\nError: failed\n", apart=False))
        self.assertEqual(self.entries(), [])

    def test_common_credential_forms_are_redacted(self):
        for line, secret in (("OPENAI_API_KEY=meta-secret-1234567890", "meta-secret"),
                             ('{"api_key": "secret-12345"}', "secret-12345"),
                             ("https://user:pa55@host/repo", "pa55"), ("AKIA" + "ABCDEFGHIJKLMNOP", "AKIA"),
                             ("hf_abcdefghijklmnopqrstu", "hf_abc"), ("password=hunter2", "hunter2"),
                             ("error: --api-key s3cr3t-value-123 rejected", "s3cr3t"),
                             ("GROK_DEPLOYMENT_KEY=abcd.efgh.ijkl", "abcd.efgh"),
                             ('{"password": "alpha beta-secret-tail"}', "beta-secret-tail"),
                             ("secret='alpha beta-secret-tail'", "beta-secret-tail"),
                             ("--api-key 'alpha beta-secret-tail' rejected", "beta-secret-tail"),
                             (r'{"password": "alpha \"beta\" gamma-tail"}', "gamma-tail"),
                             (r"secret='alpha \'beta\' gamma-tail'", "gamma-tail"),
                             ('api_key="alpha beta-secret-tail', "beta-secret-tail")):
            with self.subTest(line=line):
                self.assertNotIn(secret, cli_failure_log.redact(line))

    def test_a_held_lock_is_skipped_not_waited_for(self):
        from makewand import filelock
        self.log.parent.mkdir(parents=True)
        holder = os.open(self.log, os.O_RDWR | os.O_CREAT, 0o600)
        self.addCleanup(os.close, holder)
        filelock.flock(holder, filelock.LOCK_EX)
        try:
            cli_failure_log.record("grok", 1, "Error: something")
        finally:
            filelock.flock(holder, filelock.LOCK_UN)
        self.assertEqual(self.log.read_text(), "")

    def test_hardlinked_log_does_not_change_the_other_file(self):
        self.log.parent.mkdir(parents=True)
        outside = Path(self.tmp.name) / "outside"
        outside.write_bytes(b"outside sentinel\n")
        os.link(outside, self.log)
        cli_failure_log.record("grok", 1, "Error: something")
        self.assertEqual(outside.read_bytes(), b"outside sentinel\n")

    def test_unavailable_private_opener_skips_log_without_changing_unknown(self):
        with patch("makewand.accounting_files.AccountingFiles.open", side_effect=PermissionError("not private")):
            result = model_process_failure("grok", 1, "answer", "Error: something")
        self.assertEqual(result.status, "UNKNOWN")
        self.assertFalse(result.outcome_known)
        self.assertFalse(self.log.exists())

    @unittest.skipIf(os.name == "nt", "POSIX symlink-race fixture")
    def test_symlink_swap_after_precheck_never_changes_the_target(self):
        self.log.parent.mkdir(parents=True)
        outside = Path(self.tmp.name) / "outside"
        outside.write_bytes(b"outside sentinel\n")
        outside.chmod(0o644)
        before_mode = outside.stat().st_mode
        self.log.symlink_to(outside)
        # Model a link appearing after the cheap precheck, before the open.
        with patch.object(Path, "is_symlink", return_value=False):
            cli_failure_log.record("grok", 1, "Error: something")
        self.assertEqual(outside.read_bytes(), b"outside sentinel\n")
        self.assertEqual(outside.stat().st_mode, before_mode)

    @unittest.skipIf(os.name == "nt", "POSIX permission bits")
    def test_existing_wider_log_is_tightened(self):
        self.log.parent.mkdir(parents=True)
        self.log.write_text("")
        os.chmod(self.log, 0o644)
        cli_failure_log.record("grok", 1, "Error: something")
        self.assertEqual(os.stat(self.log).st_mode & 0o777, 0o600)

    @unittest.skipUnless(hasattr(os, "mkfifo"), "needs FIFOs")
    def test_non_regular_log_is_skipped_without_blocking(self):
        self.log.parent.mkdir(parents=True)
        os.mkfifo(self.log)
        cli_failure_log.record("grok", 1, "Error: something")
        self.assertTrue(Path(self.log).is_fifo())


if __name__ == "__main__":
    unittest.main()
