"""Codex session logs read for quota (health.py), as the Go reader reads them.

- A session's session_meta, the first line of its log, still binds tail events
  that name no account when more than 4 MiB of transcript follows it. The line
  counts only when complete within 64 KiB.
- Only regular files compete for the eight newest logs: a FIFO is never opened
  and does not push a log out.
"""

try:
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import contextlib
import json
import os
import tempfile
import threading
import time
import unittest
from datetime import datetime, timezone
from pathlib import Path
from unittest.mock import patch

from makewand.health import _get_official_subscription_quota

TAIL_BYTES = 4 * 1024 * 1024


def iso(at):
    return datetime.fromtimestamp(at, timezone.utc).isoformat()


class CodexSessionLogQuotaTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.home = Path(temp.name)
        self.codex = self.home / ".codex"
        self.sessions = self.codex / "sessions"
        self.sessions.mkdir(parents=True)
        (self.codex / "auth.json").write_text(json.dumps({"tokens": {"account_id": "A"}}))
        env = patch.dict(os.environ, HOME=str(self.home), CODEX_HOME=str(self.codex))
        env.start()
        self.addCleanup(env.stop)

    @staticmethod
    def rate_limits(used, minutes, account=None):
        now = time.time()
        event = {"timestamp": iso(now - 60), "payload": {"rate_limits": {"primary": {
            "used_percent": used, "window_minutes": minutes, "resets_at": now + 3600}}}}
        if account is not None:
            event["account_id"] = account
        return json.dumps(event)

    @staticmethod
    def session_meta(account, instructions):
        # Codex's own session_meta lines are 20 to 23 KB.
        return json.dumps({"timestamp": iso(time.time() - 3600), "type": "session_meta",
                           "payload": {"account_id": account, "base_instructions": {"text": "i" * instructions}}})

    def write_long_session(self, first, last):
        transcript = json.dumps({"timestamp": iso(time.time()), "type": "response_item",
                                 "payload": {"type": "message", "role": "assistant",
                                             "content": [{"type": "output_text", "text": "words " * 40}]}}) + "\n"
        repeats = (TAIL_BYTES + 1024 * 1024) // len(transcript) + 1
        with (self.sessions / "rollout.jsonl").open("w", encoding="utf-8") as stream:
            stream.write(first + "\n")
            stream.write(transcript * repeats)  # more than 4 MiB follows the first line
            stream.write(last + "\n")

    def used_percent(self):
        quota = _get_official_subscription_quota("codex")
        if not quota or quota.get("percentage") is None:
            return None
        return 100 - quota["percentage"]

    def test_the_first_line_session_meta_binds_the_tail_of_a_long_session(self):
        self.write_long_session(self.session_meta("A", 22 * 1024), self.rate_limits(45, 10080))
        self.assertEqual(self.used_percent(), 45)

    def test_another_accounts_session_meta_binds_nothing_to_this_login(self):
        self.write_long_session(self.session_meta("B", 22 * 1024), self.rate_limits(45, 10080))
        self.assertIsNone(self.used_percent())

    def test_a_first_line_past_64_kib_is_not_read(self):
        self.write_long_session(self.session_meta("A", 64 * 1024), self.rate_limits(45, 10080))
        self.assertIsNone(self.used_percent())

    @unittest.skipUnless(hasattr(os, "mkfifo"), "FIFOs need POSIX")
    def test_newer_fifos_do_not_displace_a_session_log(self):
        log = self.sessions / "events.jsonl"
        log.write_text(self.rate_limits(30, 300, account="A") + "\n", encoding="utf-8")
        older = time.time() - 60
        os.utime(log, (older, older))
        fifos = [self.sessions / f"fifo-{i}.jsonl" for i in range(8)]
        for fifo in fifos:
            os.mkfifo(fifo)
        result = {}
        reader = threading.Thread(target=lambda: result.update(quota=_get_official_subscription_quota("codex")),
                                  daemon=True)
        reader.start()
        reader.join(10)
        if reader.is_alive():
            # Unblock the reader stuck opening a FIFO so the run can go on.
            deadline = time.monotonic() + 5
            while reader.is_alive() and time.monotonic() < deadline:
                for fifo in fifos:
                    with contextlib.suppress(OSError):
                        os.close(os.open(fifo, os.O_WRONLY | os.O_NONBLOCK))
                reader.join(.05)
            self.fail("reading the session logs blocked on a FIFO")
        self.assertEqual(result["quota"]["percentage"], 70)


if __name__ == "__main__":
    unittest.main()
