"""Read-only local answers rest on the whole prompt on every endpoint shape (local-truncation, round 4).

Round 3 made a read-only local dispatch run on Ollama's native /api/chat with a
context window sized for the prompt. Four gaps remained, each reproduced here
against fake HTTP servers on the loopback interface (no model runs):

* A server whose /api/show reports no context length (no model_info, or none of
  its keys) may still cap the window silently at the model's trained length; the
  16 bytes/token floor let such a cut through. The floor is now 6 bytes a token,
  and without a reported length only a prompt and answer that fit the smallest
  model window (OLLAMA_MIN_CONTEXT_TOKENS) count.
* A response without eval_count was read as zero answer tokens.
* An endpoint that only offers the OpenAI-compatible API (vLLM, llama.cpp, LM
  Studio) could not answer any read-only request; it now answers one whose
  usage.prompt_tokens is plausible for the whole prompt.
* num_ctx was the prompt's UTF-8 size plus the answer budget, 3-4 times the
  tokens a prompt needs; it now comes from a generous token estimate.
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
from makewand.execution_contract import EXIT_UNVERIFIED
from makewand.git_helper import run_git_cmd
from makewand.providers import local
from makewand.providers.api_client import DEFAULT_SYSTEM_PROMPTS

MODEL = "fixture-coder:7b"
NONCE = "0a1b2c3d"


def tokens(text):
    """The fake models' tokenizer: four UTF-8 bytes a token."""
    return math.ceil(len(text.encode("utf-8")) / 4)


def answer(seen):
    """Approves what the model received, with the verdict tag it can still see."""
    match = re.search(r"MAKEWAND_(?:VERDICT|REVIEWED_DIFF)_([0-9a-f]{8})", seen)
    tag = f"MAKEWAND_VERDICT_{match.group(1)}" if match else "MAKEWAND_VERDICT"
    return f'Reviewed what I received.\n{tag}: {{"pass": true, "defects": []}}'


def review_prompt(kib):
    body = "".join(f"+ENTRY_{index:05d} = ({index}, 'value-{index}')\n" for index in range(kib * 1024 // 32))
    return (f"Review this diff. The last line must start with MAKEWAND_VERDICT_{NONCE}:\n"
            f"<<<MAKEWAND_REVIEWED_DIFF_{NONCE}_BEGIN>>>\n{body}<<<MAKEWAND_REVIEWED_DIFF_{NONCE}_END>>>\n")


def module_source(lines):
    return "".join(f"ENTRY_{index:05d} = ({index}, 'value-{index}')\n" for index in range(lines))


class FakeServer(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, handler, **settings):
        super().__init__(("127.0.0.1", 0), handler)
        self.settings = dict(context_length=131072, show="arch", reported_context_length=None, report_prompt=True,
                             report_eval=True, eval_count=24, default_ctx=2048, over_window="cut",
                             report_usage=True, show_reply=None, show_imitation=False)
        self.settings.update(settings)
        self.requests = []
        self.seen_whole = []
        self.url = f"http://127.0.0.1:{self.server_address[1]}"
        threading.Thread(target=self.serve_forever, daemon=True).start()

    def stop(self):
        self.shutdown()
        self.server_close()

    def posted(self, path):
        return [body for posted_path, body in self.requests if posted_path == path]


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, code, payload):
        data = payload.encode("utf-8") if isinstance(payload, str) else json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def body(self):
        return json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")

    @staticmethod
    def process(messages, window):
        """Ollama's runner: over the window, the system message goes first; a last message
        that alone is too long keeps a few tokens of its start and its end, filling the window."""
        texts = [str(message.get("content") or "") for message in messages]
        total = sum(tokens(text) + 4 for text in texts)
        if total <= window:
            return "".join(texts), total, True
        if tokens(texts[-1]) + 4 <= window:
            return texts[-1], tokens(texts[-1]) + 4, False
        joined = "".join(texts)
        return joined[:16] + joined[-(window - 4) * 4:], window, False


class FakeOllama(Handler):
    """An Ollama server that caps num_ctx silently at the model's real context length."""

    def do_GET(self):
        if self.path == "/v1/models":
            return self.reply(200, {"data": [{"id": MODEL}]})
        return self.reply(404, "404 page not found")

    def do_POST(self):
        settings = self.server.settings
        body = self.body()
        self.server.requests.append((self.path, body))
        if self.path == "/api/generate":
            return self.reply(200, {"done": True})
        if self.path == "/api/show":
            if settings["show"] == "legacy":  # before model_info: details and the modelfile only
                return self.reply(200, {"modelfile": "FROM fixture", "parameters": "", "template": "{{ .Prompt }}",
                                        "details": {"family": "fixture"}})
            info = {"general.architecture": "fixture"}
            if settings["show"] == "arch":
                info["fixture.context_length"] = settings["reported_context_length"] or settings["context_length"]
            return self.reply(200, {"model_info": info, "details": {"family": "fixture"}, "modelfile": "FROM fixture"})
        if self.path == "/api/chat":
            requested = (body.get("options") or {}).get("num_ctx", settings["default_ctx"])
            window = min(requested, settings["context_length"])
            seen, processed, whole = self.process(body["messages"], window)
            self.server.seen_whole.append(whole)
            response = {"model": body.get("model"), "message": {"role": "assistant", "content": answer(seen)},
                        "done": True, "done_reason": "stop"}
            if settings["report_prompt"]:
                response["prompt_eval_count"] = processed
            if settings["report_eval"]:
                response["eval_count"] = settings["eval_count"]
            return self.reply(200, response)
        if self.path == "/v1/chat/completions":
            seen, processed, whole = self.process(body["messages"], settings["default_ctx"])
            self.server.seen_whole.append(whole)
            return self.reply(200, {"choices": [{"message": {"role": "assistant", "content": answer(seen)},
                                                 "finish_reason": "stop"}],
                                    "usage": {"prompt_tokens": processed, "completion_tokens": 24}})
        return self.reply(404, "404 page not found")


class FakeOpenAICompatible(Handler):
    """vLLM / llama.cpp / LM Studio: only the OpenAI-compatible API, no window to set."""

    def do_GET(self):
        if self.path == "/v1/models":
            return self.reply(200, {"data": [{"id": MODEL}]})
        return self.reply(404, {"detail": "Not Found"})

    def do_POST(self):
        settings = self.server.settings
        body = self.body()
        self.server.requests.append((self.path, body))
        if self.path == "/v1/chat/completions":
            window = settings["context_length"]
            texts = [str(message.get("content") or "") for message in body["messages"]]
            total = sum(tokens(text) + 4 for text in texts)
            if total > window and settings["over_window"] == "refuse":
                return self.reply(400, {"object": "error", "type": "BadRequestError", "message": (
                    f"This model's maximum context length is {window} tokens. However, you requested {total} tokens.")})
            seen, processed, whole = self.process(body["messages"], window)
            self.server.seen_whole.append(whole)
            response = {"choices": [{"message": {"role": "assistant", "content": "The answer is 42.\n" + answer(seen)},
                                     "finish_reason": "stop"}]}
            if settings["report_usage"]:
                response["usage"] = {"prompt_tokens": processed, "completion_tokens": 24}
            return self.reply(200, response)
        if self.path == "/api/show" and settings["show_imitation"]:  # part of Ollama's API, without /api/chat
            return self.reply(200, {"model_info": {"general.architecture": "llama", "llama.context_length": 32768}})
        if settings["show_reply"] is not None:
            return self.reply(*settings["show_reply"])
        return self.reply(404, {"detail": "Not Found"})


class LocalEndpointTestCase(unittest.TestCase):
    handler = FakeOllama

    def start(self, handler=None, **settings):
        self.server = FakeServer(handler or self.handler, **settings)
        self.addCleanup(self.server.stop)
        temporary = tempfile.TemporaryDirectory(prefix="makewand-local-r4-")
        self.addCleanup(temporary.cleanup)
        self.workspace = temporary.name
        return self.server

    @contextlib.contextmanager
    def local_endpoint(self):
        with patch("makewand.config.is_provider_enabled", return_value=True), \
                patch.object(local, "has_active_gpu_training", return_value=(False, "")), \
                patch.object(local, "get_free_gpu_vram_mb", return_value=20000), \
                patch.dict(os.environ, {"LOCAL_MODEL_ENDPOINT": f"{self.server.url}/v1", "LOCAL_MODEL_NAME": MODEL}), \
                contextlib.redirect_stderr(io.StringIO()), contextlib.redirect_stdout(io.StringIO()):
            yield

    def run_local(self, prompt, readonly=True):
        with self.local_endpoint():
            return local.execute_local_task(prompt, cwd=self.workspace, readonly=readonly, timeout=30)

    def system_prompt(self):
        return DEFAULT_SYSTEM_PROMPTS["reviewer"] + f"\nTarget working directory: {self.workspace}"


class UnreportedContextLengthTests(LocalEndpointTestCase):
    def test_silent_cap_of_an_unreported_length_is_unverified(self):
        """local_pipeline_probe E4: a 144 KB prompt cut to 32768 tokens (4.4 bytes a token)."""
        for show in ("legacy", "none"):
            with self.subTest(show=show):
                self.start(context_length=32768, show=show)
                result = self.run_local(review_prompt(140))
                self.assertEqual(self.server.seen_whole, [False])
                self.assertFalse(result[0])
                self.assertEqual(result.status, "UNVERIFIED")
                self.assertIn("context_length", result[2])

    def test_large_cut_is_caught_by_the_token_floor(self):
        """local_probe C8/C9: 32768 tokens processed of a 234 KB prompt."""
        self.start(context_length=32768, show="legacy")
        result = self.run_local(review_prompt(230))
        self.assertEqual(self.server.seen_whole, [False])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("可能已被截断", result[2])

    def test_server_that_reports_more_than_it_honours_is_caught_by_the_token_floor(self):
        """local_probe C10: /api/show says 128k, the server caps at 32k."""
        self.start(context_length=32768, reported_context_length=131072)
        result = self.run_local(review_prompt(230))
        self.assertEqual(self.server.seen_whole, [False])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("32768 tokens", result[2])

    def test_small_question_on_a_server_without_model_info_is_answered(self):
        self.start(context_length=4096, show="legacy")
        result = self.run_local("What does tables.py do? Answer briefly.")
        self.assertTrue(result[0], result[2])
        self.assertEqual(self.server.seen_whole, [True])

    def test_floor_accepts_ordinary_code_and_prose(self):
        """Real text stays under the floor: four bytes a token passes, seven does not."""
        prompt_bytes = 60000
        self.assertIsNone(local.prompt_token_floor_error(prompt_bytes // 4, prompt_bytes))
        self.assertIsNotNone(local.prompt_token_floor_error(prompt_bytes // 7, prompt_bytes))


class MissingCountTests(LocalEndpointTestCase):
    def test_missing_eval_count_is_unverified(self):
        """local_probe C7b: the answer ran into the end of the window, eval_count omitted."""
        prompt = review_prompt(2)
        self.start(report_eval=False, eval_count=4000)
        result = self.run_local(prompt)
        self.assertEqual(self.server.seen_whole, [True])
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("eval_count", result[2])

    def test_whole_prompt_error_requires_both_counts(self):
        self.assertIsNotNone(local.whole_prompt_error({"prompt_eval_count": 100}, 4096, 10, 100))
        self.assertIsNotNone(local.whole_prompt_error({"eval_count": 10}, 4096, 10, 100))
        self.assertIsNone(local.whole_prompt_error({"prompt_eval_count": 100, "eval_count": 10}, 4096, 10, 100))


class WindowSizeTests(LocalEndpointTestCase):
    def test_window_comes_from_a_token_estimate_not_the_byte_size(self):
        prompt = review_prompt(100)
        self.start()
        result = self.run_local(prompt)
        self.assertTrue(result[0], result[2])
        window = self.server.posted("/api/chat")[0]["options"]["num_ctx"]
        prompt_bytes = len(prompt.encode("utf-8")) + len(self.system_prompt().encode("utf-8"))
        needed = tokens(prompt) + tokens(self.system_prompt()) + 8
        self.assertEqual(window, local.estimate_prompt_tokens(self.system_prompt()) + local.estimate_prompt_tokens(prompt)
                         + local.OLLAMA_TEMPLATE_TOKENS + local.OLLAMA_ANSWER_TOKENS)
        self.assertLess(window, prompt_bytes)  # round 3: prompt_bytes + 8704
        self.assertGreater(window, needed + local.OLLAMA_ANSWER_TOKENS)
        self.assertEqual(self.server.seen_whole, [True])

    def test_estimate_is_generous_for_code_digits_and_cjk(self):
        code = "    def lookup(self, key):\n        return self._table.get(key)\n" * 50
        self.assertGreaterEqual(local.estimate_prompt_tokens(code), len(code) // 3)
        self.assertLessEqual(local.estimate_prompt_tokens(code), len(code) // 2 + 1)
        self.assertEqual(local.estimate_prompt_tokens("1234567890"), 10)
        self.assertGreaterEqual(local.estimate_prompt_tokens("审查以下代码改动" * 10), 80)
        self.assertGreater(local.estimate_prompt_tokens("bad \udce9 name"), 0)  # an undecodable byte never raises

    def test_window_is_capped_at_the_models_length(self):
        self.start(context_length=32768)
        result = self.run_local(review_prompt(100))
        self.assertEqual(self.server.posted("/api/chat")[0]["options"]["num_ctx"], 32768)
        self.assertTrue(result[0], result[2])


class OpenAICompatibleEndpointTests(LocalEndpointTestCase):
    handler = FakeOpenAICompatible

    def test_question_to_an_openai_compatible_endpoint_is_answered(self):
        """explain_vllm_probe / local_probe C12."""
        self.start(context_length=32768)
        result = self.run_local("What does tables.py do? Answer briefly.")
        self.assertTrue(result[0], result[2])
        self.assertIn("The answer is 42", result[1])
        chats = self.server.posted("/v1/chat/completions")
        self.assertEqual(len(chats), 1)
        self.assertIs(chats[0]["stream"], False)
        self.assertEqual([message["role"] for message in chats[0]["messages"]], ["system", "user"])

    def test_review_that_fits_is_accepted(self):
        self.start(context_length=32768)
        result = self.run_local(review_prompt(40))
        self.assertTrue(result[0], result[2])
        self.assertEqual(self.server.seen_whole, [True])

    def test_cut_prompt_is_unverified_by_its_token_count(self):
        self.start(context_length=4096)  # cut to 4096 tokens of a 100 KB prompt
        result = self.run_local(review_prompt(100))
        self.assertEqual(self.server.seen_whole, [False])
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("4096 tokens", result[2])

    def test_missing_prompt_token_count_is_unverified(self):
        self.start(context_length=32768, report_usage=False)
        result = self.run_local(review_prompt(4))
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("usage.prompt_tokens", result[2])

    def test_refused_prompt_over_the_context_is_unverified(self):
        self.start(context_length=4096, over_window="refuse")
        result = self.run_local(review_prompt(100))
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("maximum context length", result[2])
        self.assertEqual(self.server.seen_whole, [])

    def test_lm_studio_style_reply_to_api_show_counts_as_no_native_api(self):
        self.start(context_length=32768, show_reply=(200, {"error": "Unexpected endpoint or method. (POST /api/show)"}))
        result = self.run_local("What does tables.py do?")
        self.assertTrue(result[0], result[2])
        self.assertEqual(len(self.server.posted("/v1/chat/completions")), 1)

    def test_api_show_without_api_chat_falls_back_to_the_openai_compatible_api(self):
        self.start(context_length=32768, show_imitation=True)
        result = self.run_local("What does tables.py do?")
        self.assertTrue(result[0], result[2])
        self.assertEqual([path for path, _ in self.server.requests if path.startswith("/api/") and path != "/api/generate"],
                         ["/api/show", "/api/chat"])
        self.assertEqual(len(self.server.posted("/v1/chat/completions")), 1)

    def test_failed_probe_is_unverified_without_a_fallback(self):
        """A server error on /api/show proves nothing about the endpoint: no OpenAI-compatible request."""
        self.start(context_length=32768, show_reply=(500, {"detail": "Internal Server Error"}))
        result = self.run_local("What does tables.py do?")
        self.assertFalse(result[0])
        self.assertEqual(result.status, "UNVERIFIED")
        self.assertIn("/api/show", result[2])
        self.assertEqual(self.server.posted("/v1/chat/completions"), [])


class PipelineTests(LocalEndpointTestCase):
    """run_workflow --local-only through the real providers/local.py read-only path."""

    def make_repo(self):
        repo = Path(self.workspace) / "repo"
        repo.mkdir()
        (repo / "app.py").write_text("VALUE = 1\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(repo))
            self.assertEqual(code, 0, error)
        return repo

    def run_workflow(self, prompt, **options):
        repo = self.make_repo()
        real = local.execute_local_task

        def local_engine(task, **kwargs):
            if not kwargs.get("readonly"):
                Path(kwargs["cwd"], "tables.py").write_text(module_source(4000))  # about 130 KB
                return True, "implemented", None
            return real(task, **kwargs)

        out = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch("makewand.config.is_provider_enabled", return_value=True))
            stack.enter_context(patch.object(local, "has_active_gpu_training", return_value=(False, "")))
            stack.enter_context(patch.object(local, "get_free_gpu_vram_mb", return_value=20000))
            stack.enter_context(patch.dict(os.environ, {"LOCAL_MODEL_ENDPOINT": f"{self.server.url}/v1",
                                                        "LOCAL_MODEL_NAME": MODEL, "MAKEWAND_STRICT_NONCE": "1"}))
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch("makewand.git_helper.get_active_interactive_working_trees", return_value={}))
            stack.enter_context(patch("makewand.sandbox.verify_writable_sandbox_or_authorized", return_value=(True, None, None)))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={"local": {"status": "healthy"}}))
            stack.enter_context(patch("makewand.usage.record_engine_usage"))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, "1 passed in 0.01s")))
            stack.enter_context(patch.object(orch, "execute_local_task", side_effect=local_engine))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(out))
            result = orch.run_workflow(prompt, cwd=str(repo), local_only=True, total_timeout=120, **options)
        return result, out.getvalue(), repo

    def test_review_cut_by_an_unreported_cap_is_never_delivered(self):
        """local_pipeline_probe E4: before, PASSED with the model having seen part of the diff."""
        self.start(context_length=32768, show="legacy")
        result, log, repo = self.run_workflow("Add lookup tables module", force_code=True, workflow="single",
                                              risk="low", auto_fix=False)
        self.assertEqual(result.status, "UNVERIFIED", log)
        self.assertEqual(result.exit_code, EXIT_UNVERIFIED)
        self.assertEqual(self.server.seen_whole, [False])
        self.assertFalse((repo / "tables.py").exists())

    def test_question_to_an_openai_compatible_endpoint_through_the_workflow(self):
        """explain_vllm_probe: round 3 ended UNVERIFIED (exit 11) whatever the prompt size."""
        self.start(FakeOpenAICompatible, context_length=32768)
        result, log, _ = self.run_workflow("Explain what app.py does, read-only, do not modify anything")
        self.assertEqual(result.status, "PASSED", log)
        self.assertIn("The answer is 42", log)


if __name__ == "__main__":
    unittest.main()
