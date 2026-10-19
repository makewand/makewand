"""Local and OpenRouter verdicts rest on the whole prompt, or fail closed (n3-reviewer-capacity b).

Ollama silently drops the start of a prompt longer than its context window, and
its OpenAI-compatible endpoint cannot set the window; a read-only local dispatch
therefore runs on the native /api/chat endpoint with a window sized for the whole
prompt and counts only when the reported token counts show nothing was cut (an
endpoint without the native API: when its reported prompt token count is
plausible for the whole prompt). OpenRouter compresses an over-long prompt ("middle-out")
unless transforms are disabled; with transforms off it refuses the prompt, which
is UNVERIFIED. Both endpoints are fake HTTP servers on the loopback interface.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import http.server
import io
import json
import math
import os
import re
import tempfile
import threading
import unittest
from unittest.mock import patch

from makewand import orchestrator as orch
from makewand.execution_contract import EXIT_PASSED, EXIT_UNVERIFIED
from makewand.git_helper import run_git_cmd
from makewand.providers import local
from makewand.providers.api_client import DEFAULT_SYSTEM_PROMPTS, call_api_chat

MODEL = "fixture-coder:7b"
NONCE = "0a1b2c3d"


def tokens(text):
    """The fake models' tokenizer: four UTF-8 bytes a token."""
    return math.ceil(len(text.encode("utf-8")) / 4)


def answer(seen):
    """A model approves what it received, inferring the per-task verdict tag from the
    instructions or, when they were cut off, from the nonce of the diff fences."""
    match = re.search(r"MAKEWAND_(?:VERDICT|REVIEWED_DIFF)_([0-9a-f]{8})", seen)
    tag = f"MAKEWAND_VERDICT_{match.group(1)}" if match else "MAKEWAND_VERDICT"
    return f'Reviewed what I received.\n{tag}: {{"pass": true, "defects": []}}'


def review_prompt(kib):
    """Instructions with the verdict tag at the start and in the closing fence, like a review prompt."""
    body = "".join(f"+ENTRY_{index:05d} = ({index}, 'value-{index}')\n" for index in range(kib * 1024 // 32))
    return (f"Review this diff. The last line must start with MAKEWAND_VERDICT_{NONCE}:\n"
            f"<<<MAKEWAND_REVIEWED_DIFF_{NONCE}_BEGIN>>>\n{body}<<<MAKEWAND_REVIEWED_DIFF_{NONCE}_END>>>\n"
            f"(a plain MAKEWAND_VERDICT line without _{NONCE} is no verdict)\n")


class FakeServer(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, handler):
        super().__init__(("127.0.0.1", 0), handler)
        self.requests = []
        self.seen_whole = []
        self.url = f"http://127.0.0.1:{self.server_address[1]}"
        threading.Thread(target=self.serve_forever, daemon=True).start()

    def stop(self):
        self.shutdown()
        self.server_close()

    def posted(self, path):
        return [body for posted_path, body in self.requests if posted_path == path]


class FakeOllama(http.server.BaseHTTPRequestHandler):
    """Ollama's semantics: a prompt over num_ctx loses its system message, or keeps four
    tokens of its start and its end so that it fills the window; /v1 ignores options."""

    def log_message(self, *args):
        pass

    def reply(self, code, payload):
        data = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/v1/models":
            return self.reply(200, {"data": [{"id": MODEL}]})
        return self.reply(404, {"error": "not found"})

    def process(self, messages, window):
        texts = [str(message.get("content") or "") for message in messages]
        total = sum(tokens(text) + 4 for text in texts)
        if total <= window:
            return "".join(texts), total, True
        if self.server.half_truncation:  # llama.cpp's own server: cut to about half the window
            joined = "".join(texts)
            return joined[:16] + joined[-(window // 2 - 4) * 4:], window // 2, False
        last = texts[-1]
        if tokens(last) + 4 <= window:
            return last, tokens(last) + 4, False  # the system message is dropped
        joined = "".join(texts)
        keep = (window - 4) * 4
        return joined[:16] + joined[-keep:], window, False

    def do_POST(self):
        server = self.server
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
        server.requests.append((self.path, body))
        if self.path == "/api/generate":
            return self.reply(200, {"done": True})
        if self.path == "/api/show":
            if not server.native:
                return self.reply(404, {"error": "404 page not found"})
            info = {"general.architecture": "fixture"}
            if server.context_length:
                info["fixture.context_length"] = server.context_length
            return self.reply(200, {"model_info": info, "details": {"family": "fixture"}})
        if self.path == "/api/chat":
            if not server.native:
                return self.reply(404, {"error": "404 page not found"})
            options = body.get("options") or {}
            window = min(options.get("num_ctx", server.default_ctx), server.context_length or float("inf"))
            total = sum(tokens(str(message.get("content") or "")) + 4 for message in body["messages"])
            if server.refuse_over_window and body.get("truncate") is False and total > window:
                return self.reply(400, {"error": "the input length exceeds the context length"})
            seen, processed, whole = self.process(body["messages"], window)
            server.seen_whole.append(whole)
            response = {"model": body.get("model"), "message": {"role": "assistant", "content": answer(seen)},
                        "done": True, "done_reason": "stop", "eval_count": server.eval_count or 24}
            if server.report_counts:
                response["prompt_eval_count"] = processed
            return self.reply(200, response)
        if self.path == "/v1/chat/completions":
            seen, processed, whole = self.process(body["messages"], server.default_ctx)
            server.seen_whole.append(whole)
            return self.reply(200, {"choices": [{"message": {"role": "assistant", "content": answer(seen)},
                                                 "finish_reason": "stop"}],
                                    "usage": {"prompt_tokens": processed, "completion_tokens": 24}})
        return self.reply(404, {"error": "not found"})


class LocalWholePromptTests(unittest.TestCase):
    def setUp(self):
        self.server = FakeServer(FakeOllama)
        self.addCleanup(self.server.stop)
        self.server.native = True
        self.server.context_length = 131072
        self.server.default_ctx = 2048
        self.server.report_counts = True
        self.server.refuse_over_window = False
        self.server.half_truncation = False
        self.server.eval_count = None
        temporary = tempfile.TemporaryDirectory(prefix="makewand-whole-prompt-")
        self.addCleanup(temporary.cleanup)
        self.workspace = temporary.name

    @contextlib.contextmanager
    def local_endpoint(self, gpu_busy=False):
        with patch("makewand.config.is_provider_enabled", return_value=True), \
                patch.object(local, "has_active_gpu_training", return_value=(gpu_busy, "PID 1 (python, 9000 MiB)" if gpu_busy else "")), \
                patch.object(local, "get_free_gpu_vram_mb", return_value=20000), \
                patch.dict(os.environ, {"LOCAL_MODEL_ENDPOINT": f"{self.server.url}/v1", "LOCAL_MODEL_NAME": MODEL}), \
                contextlib.redirect_stderr(io.StringIO()), contextlib.redirect_stdout(io.StringIO()):
            yield

    def run_local(self, prompt, readonly=True, gpu_busy=False):
        with self.local_endpoint(gpu_busy):
            return local.execute_local_task(prompt, cwd=self.workspace, readonly=readonly, timeout=30)

    def system_prompt(self):
        return DEFAULT_SYSTEM_PROMPTS["reviewer"] + f"\nTarget working directory: {self.workspace}"

    def test_review_runs_on_the_native_api_with_a_window_for_the_whole_prompt(self):
        prompt = review_prompt(40)  # about 10k fake tokens; the OpenAI-compatible endpoint keeps 2048
        result = self.run_local(prompt)
        self.assertTrue(result[0], result[2])
        self.assertIn(f"MAKEWAND_VERDICT_{NONCE}:", result[1])
        self.assertEqual(self.server.posted("/v1/chat/completions"), [])
        chats = self.server.posted("/api/chat")
        self.assertEqual(len(chats), 1)
        self.assertEqual(self.server.seen_whole, [True])
        expected_window = (local.estimate_prompt_tokens(self.system_prompt()) + local.estimate_prompt_tokens(prompt)
                           + local.OLLAMA_TEMPLATE_TOKENS + local.OLLAMA_ANSWER_TOKENS)
        options = chats[0]["options"]
        self.assertEqual(options["num_ctx"], expected_window)
        self.assertEqual(options["num_predict"], local.OLLAMA_ANSWER_TOKENS)
        self.assertEqual((chats[0]["truncate"], chats[0]["shift"], chats[0]["stream"]), (False, False, False))
        self.assertEqual(chats[0]["keep_alive"], "0")
        self.assertEqual([message["role"] for message in chats[0]["messages"]], ["system", "user"])

    def test_prompt_over_the_models_context_is_unverified(self):
        self.server.context_length = 4096
        result = self.run_local(review_prompt(40))
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("4096", result[2])
        self.assertEqual(self.server.posted("/api/chat")[0]["options"]["num_ctx"], 4096)
        self.assertEqual(self.server.posted("/v1/chat/completions"), [])

    def test_dropped_system_message_is_detected(self):
        prompt = review_prompt(8)
        self.server.context_length = tokens(prompt) + 4 + 20  # the user message fits, the system message does not
        result = self.run_local(prompt)
        self.assertEqual(self.server.seen_whole, [False])
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")

    def test_truncation_that_leaves_headroom_is_detected(self):
        """A server that cuts an over-long prompt to half the window: too few tokens for its bytes."""
        self.server.context_length = 4096
        self.server.half_truncation = True
        result = self.run_local(review_prompt(40))
        self.assertEqual(self.server.seen_whole, [False])
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("2048 tokens", result[2])

    def test_prompt_that_fits_the_models_context_is_accepted(self):
        prompt = review_prompt(40)
        self.server.context_length = 32768  # below the byte-sized window: capped, and still enough
        result = self.run_local(prompt)
        self.assertTrue(result[0], result[2])
        self.assertEqual(self.server.posted("/api/chat")[0]["options"]["num_ctx"], 32768)
        self.assertEqual(self.server.seen_whole, [True])

    def test_endpoint_without_the_native_api_is_checked_by_its_prompt_token_count(self):
        """The OpenAI-compatible endpoint keeps 2048 tokens: its usage.prompt_tokens shows the cut."""
        self.server.native = False
        result = self.run_local(review_prompt(40))
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("2048 tokens", result[2])
        self.assertEqual(self.server.seen_whole, [False])
        self.assertEqual([chat["stream"] for chat in self.server.posted("/v1/chat/completions")], [False])
        self.assertEqual(self.server.posted("/api/chat"), [])

    def test_missing_token_counts_are_unverified(self):
        self.server.report_counts = False
        result = self.run_local(review_prompt(4))
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("prompt_eval_count", result[2])

    def test_context_shift_while_answering_is_unverified(self):
        prompt = review_prompt(8)
        processed = tokens(self.system_prompt()) + tokens(prompt) + 8
        margin = len(self.system_prompt().encode("utf-8")) + local.OLLAMA_TEMPLATE_TOKENS
        self.server.context_length = processed + margin + 100  # the prompt fits with room for the system message
        self.server.eval_count = margin + 100  # but the answer runs into the end of the window
        result = self.run_local(prompt)
        self.assertEqual(self.server.seen_whole, [True])
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("回答", result[2])

    def test_refused_prompt_over_the_window_is_unverified(self):
        self.server.context_length = 4096
        self.server.refuse_over_window = True  # newer Ollama with truncate=false
        result = self.run_local(review_prompt(40))
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("exceeds the context length", result[2])

    def test_gpu_guard_options_reach_the_native_request(self):
        result = self.run_local(review_prompt(4), gpu_busy=True)
        self.assertTrue(result[0], result[2])
        options = self.server.posted("/api/chat")[0]["options"]
        self.assertEqual(options["num_gpu"], 0)
        self.assertGreaterEqual(options["num_thread"], 1)

    def test_coder_dispatch_keeps_the_openai_compatible_endpoint(self):
        result = self.run_local("Implement the feature", readonly=False)
        self.assertTrue(result[0], result[2])
        self.assertEqual(len(self.server.posted("/v1/chat/completions")), 1)
        self.assertEqual(self.server.posted("/api/chat"), [])

    def run_review_command(self):
        repo = Path(self.workspace) / "repo"
        repo.mkdir()
        (repo / "tables.py").write_text("VALUE = 1\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(repo))
            self.assertEqual(code, 0, error)
        (repo / "tables.py").write_text("".join(f"ENTRY_{index:05d} = {index}\n" for index in range(2000)))
        with self.local_endpoint(), patch.object(orch, "get_or_update_status", return_value={"local": {"status": "healthy"}}), \
                patch("makewand.usage.record_engine_usage"), \
                contextlib.redirect_stdout(io.StringIO()) as out:
            code = orch.run_review(cwd=str(repo), local_only=True, output_json=True, timeout=60)
        return code, out.getvalue()

    def test_review_command_never_passes_a_truncated_diff(self):
        """makewand review --local-only on a model whose context cannot hold the diff."""
        self.server.context_length = 4096
        code, output = self.run_review_command()
        self.assertEqual(code, EXIT_UNVERIFIED, output)
        self.assertNotIn(True, self.server.seen_whole)

    def test_review_command_passes_a_diff_the_model_read_whole(self):
        code, output = self.run_review_command()
        self.assertEqual(code, EXIT_PASSED, output)
        self.assertEqual(self.server.seen_whole, [True])


class FakeOpenRouter(FakeOllama):
    """OpenRouter: over the context, middle-out by default, a 400 refusal with transforms=[]."""

    def do_POST(self):
        server = self.server
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
        server.requests.append((self.path, body))
        if self.path != "/api/v1/chat/completions":
            return self.reply(404, {"error": {"code": 404, "message": "not found"}})
        texts = "".join(str(message.get("content") or "") for message in body["messages"])
        requested = tokens(texts)
        if requested <= server.context_tokens:
            seen, whole = texts, True
        elif body.get("transforms") == []:
            return self.reply(400, {"error": {"code": 400, "message": (
                f"This endpoint's maximum context length is {server.context_tokens} tokens. However, you requested "
                f"about {requested} tokens ({requested} of text input). Please reduce the length of either one, "
                "or use the \"middle-out\" transform to compress your prompt automatically.")}})
        else:
            half = server.context_tokens * 2  # bytes kept from each end
            seen, whole = texts[:half] + texts[-half:], False
        server.seen_whole.append(whole)
        return self.reply(200, {"choices": [{"message": {"role": "assistant", "content": answer(seen)},
                                             "finish_reason": "stop"}],
                                "usage": {"prompt_tokens": tokens(seen), "completion_tokens": 24}})


class OpenRouterWholePromptTests(unittest.TestCase):
    def setUp(self):
        self.server = FakeServer(FakeOpenRouter)
        self.addCleanup(self.server.stop)
        self.server.context_tokens = 8192
        environment = patch.dict(os.environ, {"OPENROUTER_API_KEY": "fixture-not-a-key",
                                              "OPENROUTER_BASE_URL": f"{self.server.url}/api/v1",
                                              "OPENROUTER_MODEL": "fixture/model-8k",
                                              "MAKEWAND_API_POLICY": "allow_paid"})
        environment.start()
        self.addCleanup(environment.stop)

    def review(self, prompt):
        with contextlib.redirect_stderr(io.StringIO()):
            return call_api_chat(provider="openrouter", prompt=prompt, role="reviewer", readonly=True, timeout=30)

    def test_prompt_transforms_are_disabled(self):
        result = self.review(review_prompt(4))
        self.assertTrue(result[0], result[2])
        self.assertEqual(self.server.posted("/api/v1/chat/completions")[0]["transforms"], [])
        self.assertEqual(self.server.seen_whole, [True])

    def test_prompt_over_the_context_is_refused_and_unverified(self):
        result = self.review(review_prompt(40))  # about 10k fake tokens over an 8k context
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("maximum context length", result[2])
        self.assertEqual(self.server.seen_whole, [])

    def test_dispatched_review_over_the_context_is_unverified(self):
        with patch("makewand.config.is_provider_enabled", return_value=True), \
                patch("makewand.usage.record_engine_usage"), contextlib.redirect_stderr(io.StringIO()):
            result = orch.dispatch_task("openrouter", review_prompt(40), cwd=None, timeout=30, readonly=True)
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertEqual(self.server.seen_whole, [])


if __name__ == "__main__":
    unittest.main()
