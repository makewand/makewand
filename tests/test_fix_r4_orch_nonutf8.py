"""The orchestrator never crashes on undecodable bytes in names, errors or prompts (nonutf8-orchestrator).

A file name that is not valid UTF-8 reaches the orchestrator as surrogate escapes
(os.fsdecode, git output read with surrogateescape): in the review diff when the
user sets core.quotePath=false, in git and OS error messages, in the workspace path
and in a prompt taken from argv. Printing such text to a UTF-8 stream, or sizing it
with str.encode("utf-8"), raised UnicodeEncodeError (e2e3.py nonutf8_reviewed_qp:
orchestrator.py:1652; e2e3tb.py nonutf8_unreadable: fail_and_cleanup). Every message
of the module now shows such bytes as \\xNN, sizes count the byte each escape stands
for, and providers receive the prompt as UTF-8 text. Providers, routing and the test
runner are fakes; stdout is a strict UTF-8 stream.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import os
import re
import tempfile
import unittest
from unittest.mock import patch

from makewand import orchestrator as orch
from makewand.execution_contract import EXIT_PASSED, EXIT_UNVERIFIED
from makewand.git_helper import run_git_cmd
from makewand.review_contract import reviewer_receives_whole_prompt

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")
LATIN_NAME = b"lat\xe9n.txt"


def strict_stream():
    return io.TextIOWrapper(io.BytesIO(), encoding="utf-8", errors="strict", write_through=True)


def has_surrogate(text):
    return any(0xD800 <= ord(char) <= 0xDFFF for char in text)


class NonUtf8HelperTests(unittest.TestCase):
    def test_module_messages_escape_undecodable_bytes(self):
        out = strict_stream()
        with contextlib.redirect_stdout(out):
            orch._print("name: lat\udce9n.txt", OSError("open(\"b\udcff\")"))
        self.assertEqual(out.buffer.getvalue().decode("utf-8"), 'name: lat\\xe9n.txt open("b\\xff")\n')

    def test_sizes_count_text_as_it_is_dispatched(self):
        # One definition (review_contract.dispatched_size): an undecodable byte is sent
        # as its four-byte \xNN escape, any other lone surrogate as \uXXXX (py-cleanup-a).
        self.assertEqual(orch.dispatched_size("lat\udce9n.txt"), len(b"lat\\xe9n.txt"))
        self.assertEqual(orch.dispatched_size("数据"), 6)
        self.assertEqual(orch.dispatched_size(None), 0)
        self.assertEqual(orch.dispatched_size("\ud800"), len(b"\\ud800"))  # a lone surrogate no file name produced

    def test_whole_prompt_check_measures_the_dispatched_prompt(self):
        limit = orch.reviewer_prompt_limit("agy")
        self.assertTrue(reviewer_receives_whole_prompt("agy", "x" * (limit - 4) + "\udce9"))
        self.assertFalse(reviewer_receives_whole_prompt("agy", "x" * (limit - 3) + "\udce9"))


    def test_providers_receive_utf8_text(self):
        received = []

        def execute(prompt, **kwargs):
            received.append(prompt)
            return True, "ok", None

        with patch.object(orch, "execute_codex_task", side_effect=execute), \
                patch("makewand.config.is_provider_enabled", return_value=True), \
                patch("makewand.usage.record_engine_usage"):
            result = orch.dispatch_task("codex", "review lat\udce9n.txt", cwd=None, timeout=30, readonly=True)
        self.assertTrue(result[0], result[2])
        self.assertEqual(received, ["review lat\\xe9n.txt"])


if __name__ == "__main__":
    unittest.main()
