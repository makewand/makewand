"""A token-dense read-only prompt gets one larger Ollama window instead of an UNVERIFIED answer (ollama-ctx).

Round 4 sized num_ctx from a generous estimate (half the UTF-8 bytes, a token per
digit), which saves memory for ordinary prompts but is too small for base64 or
hex blobs at about 1.5 bytes a token: a 61 KB review on a 128k model ended
UNVERIFIED with num_ctx 39692 while 41316 tokens were needed, or was refused by a
newer Ollama (local_probe2.py L6/L6b; round 3 passed it). When the reported
prompt_eval_count fills the window, or the server refuses the prompt as too long,
the request is now sent once more with a window of one token per byte (no
tokenizer exceeds that), capped at the model's trained length; a cut prompt is
still never accepted. Fake HTTP servers on the loopback interface; no model runs.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import math
import random
import string
import unittest

import test_fix_r4_local_whole_prompt as r4
from makewand.providers import local

NONCE = r4.NONCE


def dense_tokens(text):
    """A tokenizer that needs a token for every 1.5 bytes (base64 / hex blobs)."""
    return math.ceil(len(text.encode("utf-8")) / 1.5)


def dense_review_prompt(kib):
    rnd = random.Random(7)
    body = "".join("+" + "".join(rnd.choice(string.ascii_letters + "+/") for _ in range(76)) + "\n"
                   for _ in range(kib * 1024 // 78))
    return (f"Review this diff. The last line must start with MAKEWAND_VERDICT_{NONCE}:\n"
            f"<<<MAKEWAND_REVIEWED_DIFF_{NONCE}_BEGIN>>>\n{body}<<<MAKEWAND_REVIEWED_DIFF_{NONCE}_END>>>\n")


class DenseOllama(r4.FakeOllama):
    """An Ollama whose model needs settings["bpt"] bytes a token (1.5: base64 / hex blobs).
    "old" servers cut a prompt over the window silently; "new" ones refuse it (truncate=false)."""

    def tokens(self, text):
        return math.ceil(len(text.encode("utf-8")) / self.server.settings.get("bpt", 1.5))

    def process(self, messages, window):
        texts = [str(message.get("content") or "") for message in messages]
        total = sum(self.tokens(text) + 4 for text in texts)
        if total <= window:
            return "".join(texts), total, True
        if self.tokens(texts[-1]) + 4 <= window:
            return texts[-1], self.tokens(texts[-1]) + 4, False
        joined = "".join(texts)
        return joined[:16] + joined[-int((window - 4) * self.server.settings.get("bpt", 1.5)):], window, False

    def do_POST(self):
        if self.path == "/api/chat" and self.server.settings.get("mode") == "new":
            body = self.body()
            self.server.requests.append((self.path, body))
            window = min((body.get("options") or {}).get("num_ctx", 2048), self.server.settings["context_length"])
            total = sum(self.tokens(str(message.get("content") or "")) + 4 for message in body["messages"])
            if body.get("truncate") is False and total > window:
                return self.reply(400, {"error": "the input length exceeds the context length"})
            seen, processed, whole = self.process(body["messages"], window)
            self.server.seen_whole.append(whole)
            return self.reply(200, {"model": body.get("model"), "message": {"role": "assistant", "content": r4.answer(seen)},
                                    "done": True, "done_reason": "stop", "prompt_eval_count": processed,
                                    "eval_count": self.server.settings["eval_count"]})
        return super().do_POST()


class DensePromptTests(r4.LocalEndpointTestCase):
    handler = DenseOllama

    def windows(self):
        return [body["options"]["num_ctx"] for body in self.server.posted("/api/chat")]

    def prompt_bytes(self, prompt):
        return len(prompt.encode("utf-8")) + len(self.system_prompt().encode("utf-8"))

    def test_window_the_dense_prompt_fills_is_enlarged_once(self):
        """local_probe2.py L6: UNVERIFIED with num_ctx 39692 while 41316 tokens were needed."""
        prompt = dense_review_prompt(60)
        self.start()
        result = self.run_local(prompt)
        self.assertTrue(result[0], result[2])
        estimated, enlarged = self.windows()
        self.assertLess(estimated, dense_tokens(prompt))
        self.assertEqual(enlarged, self.prompt_bytes(prompt) + local.OLLAMA_TEMPLATE_TOKENS + local.OLLAMA_ANSWER_TOKENS)
        self.assertEqual(self.server.seen_whole, [False, True])

    def test_refused_dense_prompt_is_sent_once_more_with_room_for_it(self):
        """local_probe2.py L6b: a newer Ollama refused the prompt over the estimated window."""
        prompt = dense_review_prompt(60)
        self.start(mode="new")
        result = self.run_local(prompt)
        self.assertTrue(result[0], result[2])
        self.assertEqual(len(self.windows()), 2)
        self.assertEqual(self.server.seen_whole, [True])

    def test_no_larger_window_than_the_model_has(self):
        prompt = dense_review_prompt(60)
        for mode in ("old", "new"):
            with self.subTest(mode=mode):
                self.start(context_length=32768, mode=mode)
                result = self.run_local(prompt)
                self.assertFalse(result[0])
                self.assertEqual(result.status, "UNVERIFIED")
                self.assertEqual(self.windows(), [32768])  # capped at the model's length: nothing to retry

    def test_retry_that_still_cannot_hold_the_prompt_is_unverified(self):
        prompt = dense_review_prompt(60)
        self.start()
        system = self.system_prompt()
        estimated = (local.estimate_prompt_tokens(system) + local.estimate_prompt_tokens(prompt)
                     + local.OLLAMA_TEMPLATE_TOKENS + local.OLLAMA_ANSWER_TOKENS)
        needed = dense_tokens(system) + dense_tokens(prompt) + 8
        self.assertLess(estimated, needed)
        self.server.settings["context_length"] = (estimated + needed) // 2  # more than the estimate, less than the prompt
        result = self.run_local(prompt)
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertEqual(self.windows(), [estimated, (estimated + needed) // 2])
        self.assertEqual(self.server.seen_whole, [False, False])

    def test_unknown_model_length_is_never_retried(self):
        prompt = dense_review_prompt(60)
        self.start(show="legacy")
        result = self.run_local(prompt)
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertEqual(len(self.windows()), 1)

    def test_ordinary_prompt_still_needs_one_request_with_the_estimated_window(self):
        prompt = r4.review_prompt(60)
        self.start(bpt=4)
        result = self.run_local(prompt)
        self.assertTrue(result[0], result[2])
        self.assertEqual(len(self.windows()), 1)
        self.assertLess(self.windows()[0], self.prompt_bytes(prompt))

    def test_ceiling_is_one_token_per_byte(self):
        self.assertEqual(local.ceiling_prompt_tokens("abc"), 3)
        self.assertEqual(local.ceiling_prompt_tokens("审查"), 6)
        self.assertEqual(local.ceiling_prompt_tokens("bad \udce9"), 7)  # an undecodable byte never raises
        self.assertGreaterEqual(local.ceiling_prompt_tokens(dense_review_prompt(4)), dense_tokens(dense_review_prompt(4)))


if __name__ == "__main__":
    unittest.main()
