"""
Unit tests for makewand/providers/api_client.py.
Verifies HTTP requests, retries with exponential backoff, status code handling (500, 502, 504, 429),
network timeouts, token headers, streaming parsing, and multi-provider dispatching.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import io
import json
import socket
import unittest
import urllib.error
from unittest.mock import patch, MagicMock, call

from makewand.providers.api_client import (
    _make_http_request,
    call_api_chat,
    DEFAULT_SYSTEM_PROMPTS,
)


class TestMakeHttpRequest(unittest.TestCase):
    """Tests for low-level HTTP request helper _make_http_request."""

    @patch("urllib.request.urlopen")
    def test_successful_json_response(self, mock_urlopen):
        mock_resp = MagicMock()
        mock_resp.status = 200
        mock_resp.read.return_value = b'{"response": "ok"}'
        mock_resp.__enter__.return_value = mock_resp
        mock_urlopen.return_value = mock_resp

        code, body, err = _make_http_request(
            url="https://api.example.com/v1/chat",
            headers={"Content-Type": "application/json", "Authorization": "Bearer test-key"},
            data={"model": "test-model", "prompt": "hello"},
            timeout=30,
            stream=False,
        )

        self.assertEqual(code, 200)
        self.assertEqual(body, '{"response": "ok"}')
        self.assertIsNone(err)
        self.assertEqual(mock_urlopen.call_count, 1)

    @patch("urllib.request.urlopen")
    def test_streaming_sse_openai_format(self, mock_urlopen):
        mock_resp = MagicMock()
        mock_resp.status = 200
        mock_resp.__enter__.return_value = mock_resp
        mock_resp.__iter__.return_value = iter([
            b'data: {"choices": [{"delta": {"content": "Hello"}}]}\n',
            b'data: {"choices": [{"delta": {"content": " world!"}}]}\n',
            b'data: [DONE]\n',
        ])
        mock_urlopen.return_value = mock_resp

        code, body, err = _make_http_request(
            url="https://api.example.com/v1/chat/completions",
            headers={"Content-Type": "application/json"},
            data={"stream": True},
            stream=True,
        )

        self.assertEqual(code, 200)
        self.assertEqual(body, "Hello world!")
        self.assertIsNone(err)

    @patch("urllib.request.urlopen")
    def test_streaming_sse_anthropic_format(self, mock_urlopen):
        mock_resp = MagicMock()
        mock_resp.status = 200
        mock_resp.__enter__.return_value = mock_resp
        mock_resp.__iter__.return_value = iter([
            b'data: {"delta": {"text": "Anthropic"}}\n',
            b'data: {"delta": {"text": " response"}}\n',
            b'data: [DONE]\n',
        ])
        mock_urlopen.return_value = mock_resp

        code, body, err = _make_http_request(
            url="https://api.anthropic.com/v1/messages",
            headers={"x-api-key": "test"},
            data={"stream": True},
            stream=True,
        )

        self.assertEqual(code, 200)
        self.assertEqual(body, "Anthropic response")
        self.assertIsNone(err)

    @patch("sys.stdout", new_callable=io.StringIO)
    @patch("urllib.request.urlopen")
    def test_streaming_with_print_prefix(self, mock_urlopen, mock_stdout):
        mock_resp = MagicMock()
        mock_resp.status = 200
        mock_resp.__enter__.return_value = mock_resp
        mock_resp.__iter__.return_value = iter([
            b'data: {"choices": [{"delta": {"content": "Live"}}]}\n',
            b'data: {"choices": [{"delta": {"content": " output"}}]}\n',
            b'data: [DONE]\n',
        ])
        mock_urlopen.return_value = mock_resp

        code, body, err = _make_http_request(
            url="https://api.example.com",
            headers={},
            data={},
            stream=True,
            print_prefix="[Model] ",
        )

        self.assertEqual(code, 200)
        self.assertEqual(body, "Live output")
        self.assertIn("Live output\n", mock_stdout.getvalue())

    @patch("time.sleep")
    @patch("urllib.request.urlopen")
    def test_retries_on_500_502_504_exhausted(self, mock_urlopen, mock_sleep):
        """Tests retrying on 500/502/504 errors up to max_retries."""
        for error_code in (500, 502, 504):
            mock_urlopen.reset_mock()
            mock_sleep.reset_mock()

            fp = io.BytesIO(f"Internal server error {error_code}".encode("utf-8"))
            http_err = urllib.error.HTTPError(
                url="https://api.example.com",
                code=error_code,
                msg="Server Error",
                hdrs={},
                fp=fp,
            )
            mock_urlopen.side_effect = http_err

            code, body, err = _make_http_request(
                url="https://api.example.com",
                headers={},
                data={},
                max_retries=2,
                backoff_factor=0.2,
            )

            self.assertEqual(code, error_code)
            self.assertIn(f"HTTP Error {error_code}", err)
            # 1 initial + 2 retries = 3 calls
            self.assertEqual(mock_urlopen.call_count, 3)
            # Should have slept twice with exponential backoff: 0.2, 0.4
            self.assertEqual(mock_sleep.call_count, 2)
            mock_sleep.assert_has_calls([call(0.2), call(0.4)])

    @patch("time.sleep")
    @patch("urllib.request.urlopen")
    def test_retry_recovery_on_502(self, mock_urlopen, mock_sleep):
        """Tests recovery after a transient 502 Bad Gateway error."""
        fp = io.BytesIO(b"Bad Gateway")
        http_err = urllib.error.HTTPError("https://api.example.com", 502, "Bad Gateway", {}, fp)

        mock_resp = MagicMock()
        mock_resp.status = 200
        mock_resp.read.return_value = b'{"status": "recovered"}'
        mock_resp.__enter__.return_value = mock_resp

        mock_urlopen.side_effect = [http_err, mock_resp]

        code, body, err = _make_http_request(
            url="https://api.example.com",
            headers={},
            data={},
            max_retries=3,
            backoff_factor=0.1,
        )

        self.assertEqual(code, 200)
        self.assertEqual(body, '{"status": "recovered"}')
        self.assertIsNone(err)
        self.assertEqual(mock_urlopen.call_count, 2)
        mock_sleep.assert_called_once_with(0.1)

    @patch("time.sleep")
    @patch("urllib.request.urlopen")
    def test_retries_on_429_exhausted(self, mock_urlopen, mock_sleep):
        """Tests retrying on 429 Rate Limit error up to max_retries."""
        fp = io.BytesIO(b'{"error": "rate limit exceeded"}')
        http_err = urllib.error.HTTPError(
            url="https://api.example.com",
            code=429,
            msg="Too Many Requests",
            hdrs={},
            fp=fp,
        )
        mock_urlopen.side_effect = http_err

        code, body, err = _make_http_request(
            url="https://api.example.com",
            headers={},
            data={},
            max_retries=2,
            backoff_factor=0.25,
        )

        self.assertEqual(code, 429)
        self.assertIn("HTTP Error 429", err)
        self.assertEqual(mock_urlopen.call_count, 3)
        self.assertEqual(mock_sleep.call_count, 2)
        mock_sleep.assert_has_calls([call(0.25), call(0.5)])

    @patch("time.sleep")
    @patch("urllib.request.urlopen")
    def test_retry_recovery_on_429(self, mock_urlopen, mock_sleep):
        """Tests recovery after transient 429 rate limit."""
        fp = io.BytesIO(b"Rate limited")
        http_err = urllib.error.HTTPError("https://api.example.com", 429, "Rate limited", {}, fp)

        mock_resp = MagicMock()
        mock_resp.status = 200
        mock_resp.read.return_value = b'{"status": "ok_after_429"}'
        mock_resp.__enter__.return_value = mock_resp

        mock_urlopen.side_effect = [http_err, mock_resp]

        code, body, err = _make_http_request(
            url="https://api.example.com",
            headers={},
            data={},
            max_retries=2,
            backoff_factor=0.1,
        )

        self.assertEqual(code, 200)
        self.assertEqual(body, '{"status": "ok_after_429"}')
        self.assertIsNone(err)
        self.assertEqual(mock_urlopen.call_count, 2)
        mock_sleep.assert_called_once_with(0.1)

    @patch("urllib.request.urlopen")
    def test_streaming_sse_empty_choices_chunk(self, mock_urlopen):
        """Tests that empty choices chunk (e.g. usage-only chunk) does not raise IndexError."""
        mock_resp = MagicMock()
        mock_resp.status = 200
        mock_resp.__enter__.return_value = mock_resp
        mock_resp.__iter__.return_value = iter([
            b'data: {"choices": [{"delta": {"content": "Data chunk"}}]}\n',
            b'data: {"choices": [], "usage": {"total_tokens": 12}}\n',
            b'data: [DONE]\n',
        ])
        mock_urlopen.return_value = mock_resp

        code, body, err = _make_http_request(
            url="https://api.example.com/v1/chat/completions",
            headers={},
            data={"stream": True},
            stream=True,
        )

        self.assertEqual(code, 200)
        self.assertEqual(body, "Data chunk")
        self.assertIsNone(err)

    @patch("time.sleep")
    @patch("urllib.request.urlopen")
    def test_no_retry_on_client_errors(self, mock_urlopen, mock_sleep):
        """Tests that client errors (400, 401, 403, 404) fail immediately without retry."""
        for client_code in (400, 401, 403, 404):
            mock_urlopen.reset_mock()
            mock_sleep.reset_mock()

            fp = io.BytesIO(b'{"error": "bad request"}')
            http_err = urllib.error.HTTPError("https://api.example.com", client_code, "Client Error", {}, fp)
            mock_urlopen.side_effect = http_err

            code, body, err = _make_http_request(
                url="https://api.example.com",
                headers={},
                data={},
                max_retries=3,
            )

            self.assertEqual(code, client_code)
            self.assertEqual(mock_urlopen.call_count, 1)
            mock_sleep.assert_not_called()

    @patch("time.sleep")
    @patch("urllib.request.urlopen")
    def test_retries_on_network_timeout(self, mock_urlopen, mock_sleep):
        """Tests retry on TimeoutError / URLError timeout."""
        mock_urlopen.side_effect = urllib.error.URLError(socket.timeout("timed out"))

        code, body, err = _make_http_request(
            url="https://api.example.com",
            headers={},
            data={},
            max_retries=2,
            backoff_factor=0.5,
        )

        self.assertEqual(code, -1)
        self.assertIn("Network/URL Error", err)
        self.assertEqual(mock_urlopen.call_count, 3)
        self.assertEqual(mock_sleep.call_count, 2)

    @patch("time.sleep")
    @patch("urllib.request.urlopen")
    def test_timeout_error_recovering(self, mock_urlopen, mock_sleep):
        """Tests network timeout recovering on retry."""
        mock_resp = MagicMock()
        mock_resp.status = 200
        mock_resp.read.return_value = b'{"ok": true}'
        mock_resp.__enter__.return_value = mock_resp

        mock_urlopen.side_effect = [TimeoutError("connection timed out"), mock_resp]

        code, body, err = _make_http_request(
            url="https://api.example.com",
            headers={},
            data={},
            max_retries=2,
            backoff_factor=0.2,
        )

        self.assertEqual(code, 200)
        self.assertEqual(body, '{"ok": true}')
        self.assertIsNone(err)
        self.assertEqual(mock_urlopen.call_count, 2)
        mock_sleep.assert_called_once_with(0.2)

    @patch("urllib.request.urlopen")
    def test_general_exception_handling(self, mock_urlopen):
        mock_urlopen.side_effect = ValueError("invalid configuration")

        code, body, err = _make_http_request(
            url="https://api.example.com",
            headers={},
            data={},
        )

        self.assertEqual(code, -1)
        self.assertIn("Execution Exception: invalid configuration", err)


class TestCallApiChat(unittest.TestCase):
    """Tests for provider-specific logic in call_api_chat."""

    def setUp(self):
        # These are transport tests under an explicitly enabled billing policy.
        policy = patch("makewand.config.get_api_policy", return_value="allow_paid")
        policy.start()
        self.addCleanup(policy.stop)

    @patch("makewand.providers.api_client.get_api_config")
    def test_claude_missing_key(self, mock_get_cfg):
        mock_get_cfg.return_value = {"api_key": "", "base_url": ""}
        ok, out, err = call_api_chat(provider="claude", prompt="test")
        self.assertFalse(ok)
        self.assertIn("未配置 ANTHROPIC_API_KEY", err)

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_claude_token_headers_and_parsing(self, mock_get_cfg, mock_req):
        mock_get_cfg.return_value = {
            "api_key": "sk-ant-mock-key-12345678901234567890",
            "base_url": "https://api.anthropic.com",
        }
        resp_payload = {
            "content": [
                {"type": "text", "text": "Claude response line 1\n"},
                {"type": "text", "text": "Claude response line 2"},
            ]
        }
        mock_req.return_value = (200, json.dumps(resp_payload), None)

        ok, out, err = call_api_chat(
            provider="claude",
            prompt="write code",
            tier="standard",
            cwd="/workspace/test",
            role="coder",
        )

        self.assertTrue(ok)
        self.assertEqual(out, "Claude response line 1\nClaude response line 2")
        self.assertIsNone(err)

        endpoint, headers, data = mock_req.call_args[0][:3]
        self.assertEqual(endpoint, "https://api.anthropic.com/v1/messages")
        self.assertEqual(headers["x-api-key"], "sk-ant-mock-key-12345678901234567890")
        self.assertEqual(headers["anthropic-version"], "2023-06-01")
        self.assertEqual(headers["content-type"], "application/json")
        self.assertEqual(data["model"], "claude-3-7-sonnet-20250219")
        self.assertIn("Target working directory: /workspace/test", data["system"])

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_claude_fast_tier_model(self, mock_get_cfg, mock_req):
        mock_get_cfg.return_value = {"api_key": "mock-key", "base_url": ""}
        mock_req.return_value = (200, json.dumps({"content": [{"type": "text", "text": "fast"}]}), None)

        ok, out, err = call_api_chat(provider="claude", prompt="fast prompt", tier="fast")
        self.assertTrue(ok)
        data = mock_req.call_args[0][2]
        self.assertEqual(data["model"], "claude-3-5-haiku-20241022")

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_claude_http_error(self, mock_get_cfg, mock_req):
        mock_get_cfg.return_value = {"api_key": "mock-key", "base_url": ""}
        mock_req.return_value = (500, "Server Error", "HTTP Error 500: Server Error")

        ok, out, err = call_api_chat(provider="claude", prompt="test")
        self.assertFalse(ok)
        self.assertIn("HTTP Error 500", err)

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_claude_json_parse_error(self, mock_get_cfg, mock_req):
        mock_get_cfg.return_value = {"api_key": "mock-key", "base_url": ""}
        mock_req.return_value = (200, "invalid-json-response", None)

        ok, out, err = call_api_chat(provider="claude", prompt="test")
        self.assertFalse(ok)
        self.assertIn("JSON parse error", err)

    @patch("makewand.providers.api_client.get_api_config")
    def test_gemini_missing_key(self, mock_get_cfg):
        mock_get_cfg.return_value = {"api_key": "", "base_url": ""}
        ok, out, err = call_api_chat(provider="gemini", prompt="test")
        self.assertFalse(ok)
        self.assertIn("未配置 GEMINI_API_KEY", err)

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_gemini_token_param_and_parsing(self, mock_get_cfg, mock_req):
        mock_get_cfg.return_value = {
            "api_key": "AIzaSyFakeGeminiTokenKey123",
            "base_url": "https://generativelanguage.googleapis.com",
        }
        gemini_resp = {
            "candidates": [
                {
                    "content": {
                        "parts": [{"text": "Gemini answer part 1"}, {"text": " and part 2"}]
                    }
                }
            ]
        }
        mock_req.return_value = (200, json.dumps(gemini_resp), None)

        ok, out, err = call_api_chat(provider="gemini", prompt="solve math", tier="deep")
        self.assertTrue(ok)
        self.assertEqual(out, "Gemini answer part 1 and part 2")

        endpoint = mock_req.call_args[0][0]
        self.assertIn("key=AIzaSyFakeGeminiTokenKey123", endpoint)
        self.assertIn("/v1beta/models/gemini-2.5-pro:generateContent", endpoint)

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_gemini_fast_tier_model(self, mock_get_cfg, mock_req):
        mock_get_cfg.return_value = {"api_key": "mock-gemini-key", "base_url": ""}
        mock_req.return_value = (200, json.dumps({"candidates": [{"content": {"parts": [{"text": "fast flash"}]}}]}), None)

        ok, out, err = call_api_chat(provider="agy", prompt="fast prompt", tier="fast")
        self.assertTrue(ok)
        endpoint = mock_req.call_args[0][0]
        self.assertIn("models/gemini-2.0-flash:generateContent", endpoint)

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_gemini_empty_candidates(self, mock_get_cfg, mock_req):
        mock_get_cfg.return_value = {"api_key": "mock-gemini-key", "base_url": ""}
        mock_req.return_value = (200, json.dumps({"candidates": []}), None)

        ok, out, err = call_api_chat(provider="gemini", prompt="test")
        self.assertFalse(ok)
        self.assertIn("No candidates returned", err)

    @patch("makewand.providers.api_client.get_api_config")
    def test_openai_missing_key(self, mock_get_cfg):
        mock_get_cfg.return_value = {"api_key": "", "base_url": ""}
        ok, out, err = call_api_chat(provider="openai", prompt="test")
        self.assertFalse(ok)
        self.assertIn("未配置 OPENAI_API_KEY", err)

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_openai_token_headers_and_parsing(self, mock_get_cfg, mock_req):
        mock_get_cfg.return_value = {
            "api_key": "sk-test-token-123",
            "base_url": "https://api.openai.com/v1",
        }
        openai_resp = {
            "choices": [
                {"message": {"role": "assistant", "content": "OpenAI generated answer"}}
            ]
        }
        mock_req.return_value = (200, json.dumps(openai_resp), None)

        ok, out, err = call_api_chat(provider="codex", prompt="implement test", tier="deep")
        self.assertTrue(ok)
        self.assertEqual(out, "OpenAI generated answer")

        endpoint, headers, data = mock_req.call_args[0][:3]
        self.assertEqual(endpoint, "https://api.openai.com/v1/chat/completions")
        self.assertEqual(headers["Authorization"], "Bearer sk-test-token-123")
        self.assertEqual(headers["content-type"], "application/json")
        self.assertEqual(data["model"], "o1")

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_local_ollama_no_key_required(self, mock_get_cfg, mock_req):
        mock_get_cfg.return_value = {"api_key": "ollama", "base_url": "http://localhost:11434/v1"}
        resp = {"choices": [{"message": {"content": "local model output"}}]}
        mock_req.return_value = (200, json.dumps(resp), None)

        ok, out, err = call_api_chat(provider="local", prompt="offline task")
        self.assertTrue(ok)
        self.assertEqual(out, "local model output")

        headers, data = mock_req.call_args[0][1:3]
        self.assertNotIn("Authorization", headers)
        self.assertEqual(data["keep_alive"], "0")

    @patch("makewand.providers.api_client.get_api_config")
    def test_all_provider_missing_keys(self, mock_get_cfg):
        mock_get_cfg.return_value = {"api_key": "", "base_url": ""}
        cases = [
            ("grok", "XAI_API_KEY"),
            ("muse", "META_API_KEY"),
            ("deepseek", "DEEPSEEK_API_KEY"),
            ("qwen", "DASHSCOPE_API_KEY"),
            ("openrouter", "OPENROUTER_API_KEY"),
            ("siliconflow", "SILICONFLOW_API_KEY"),
            ("kimi", "MOONSHOT_API_KEY"),
            ("glm", "ZHIPU_API_KEY"),
        ]
        for prov, env_name in cases:
            ok, out, err = call_api_chat(provider=prov, prompt="test")
            self.assertFalse(ok)
            self.assertIn(env_name, err)

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_reviewer_system_prompt_and_extra_params(self, mock_get_cfg, mock_req):
        mock_get_cfg.return_value = {"api_key": "key", "base_url": "https://api.openai.com/v1"}
        mock_req.return_value = (200, json.dumps({"choices": [{"message": {"content": "review done"}}]}), None)

        ok, out, err = call_api_chat(
            provider="openai",
            prompt="review this diff",
            role="reviewer",
            extra_params={"max_retries": 1, "backoff_factor": 0.1, "max_tokens": 1024},
        )

        self.assertTrue(ok)
        data = mock_req.call_args[0][2]
        self.assertIn("MAKEWAND_VERDICT", data["messages"][0]["content"])
        self.assertEqual(data["max_tokens"], 1024)
        # Extra params for retry should have been forwarded to _make_http_request
        self.assertEqual(mock_req.call_args[1]["max_retries"], 1)
        self.assertEqual(mock_req.call_args[1]["backoff_factor"], 0.1)

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_claude_streaming(self, mock_get_cfg, mock_req):
        mock_get_cfg.return_value = {"api_key": "test-key", "base_url": "https://api.anthropic.com"}
        mock_req.return_value = (200, "streamed text", None)

        ok, out, err = call_api_chat(provider="claude", prompt="stream please", stream=True)
        self.assertTrue(ok)
        self.assertEqual(out, "streamed text")

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_gemini_streaming_and_json_error(self, mock_get_cfg, mock_req):
        mock_get_cfg.return_value = {"api_key": "test-key", "base_url": ""}
        mock_req.return_value = (200, "invalid-json", None)

        ok, out, err = call_api_chat(provider="gemini", prompt="test")
        self.assertFalse(ok)
        self.assertIn("JSON parse error", err)

        # Gemini with stream and print_prefix
        mock_req.return_value = (200, json.dumps({"candidates": [{"content": {"parts": [{"text": "streamed gemini"}]}}]}), None)
        ok, out, err = call_api_chat(provider="gemini", prompt="test", stream=True, print_prefix="Prefix: ")
        self.assertTrue(ok)
        self.assertEqual(out, "streamed gemini")

        # Gemini HTTP error
        mock_req.return_value = (503, "Unavailable", "HTTP Error 503")
        ok, out, err = call_api_chat(provider="gemini", prompt="test")
        self.assertFalse(ok)
        self.assertIn("503", err)

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_openai_streaming_and_errors(self, mock_get_cfg, mock_req):
        mock_get_cfg.return_value = {"api_key": "test-key", "base_url": ""}

        # Stream
        mock_req.return_value = (200, "openai streamed", None)
        ok, out, err = call_api_chat(provider="openai", prompt="stream", stream=True)
        self.assertTrue(ok)
        self.assertEqual(out, "openai streamed")

        # Non-200
        mock_req.return_value = (504, "Gateway Timeout", "HTTP Error 504")
        ok, out, err = call_api_chat(provider="openai", prompt="test")
        self.assertFalse(ok)
        self.assertIn("504", err)

        # Empty choices
        mock_req.return_value = (200, json.dumps({"choices": []}), None)
        ok, out, err = call_api_chat(provider="openai", prompt="test")
        self.assertFalse(ok)
        self.assertIn("No choices returned", err)

        # JSON parse error
        mock_req.return_value = (200, "bad json", None)
        ok, out, err = call_api_chat(provider="openai", prompt="test")
        self.assertFalse(ok)
        self.assertIn("JSON parse error", err)

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_provider_model_selection(self, mock_get_cfg, mock_req):
        mock_req.return_value = (200, json.dumps({"choices": [{"message": {"content": "ok"}}]}), None)
        providers_tiers = [
            ("grok", "standard", "grok-2-latest", "https://api.x.ai/v1/chat/completions"),
            ("muse", "standard", "llama-3.3-70b-instruct", "https://api.meta.ai/v1/chat/completions"),
            ("deepseek", "deep", "deepseek-reasoner", "https://api.deepseek.com/v1/chat/completions"),
            ("deepseek", "fast", "deepseek-chat", "https://api.deepseek.com/v1/chat/completions"),
            ("qwen", "deep", "qwen-max", "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"),
            ("qwen", "standard", "qwen2.5-coder-32b-instruct", "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"),
            ("openrouter", "deep", "deepseek/deepseek-r1", "https://openrouter.ai/api/v1/chat/completions"),
            ("openrouter", "fast", "auto", "https://openrouter.ai/api/v1/chat/completions"),
            ("siliconflow", "deep", "deepseek-ai/DeepSeek-R1", "https://api.siliconflow.cn/v1/chat/completions"),
            ("siliconflow", "standard", "deepseek-ai/DeepSeek-V3", "https://api.siliconflow.cn/v1/chat/completions"),
            ("kimi", "standard", "kimi-latest", "https://api.moonshot.cn/v1/chat/completions"),
            ("glm", "deep", "glm-4-plus", "https://open.bigmodel.cn/api/paas/v4/chat/completions"),
            ("glm", "standard", "codegeex-4", "https://open.bigmodel.cn/api/paas/v4/chat/completions"),
        ]

        for prov, tier, expected_model, expected_endpoint in providers_tiers:
            mock_get_cfg.return_value = {"api_key": "mock-key", "base_url": ""}
            ok, out, err = call_api_chat(provider=prov, prompt="test prompt", tier=tier)
            self.assertTrue(ok)
            endpoint = mock_req.call_args[0][0]
            data = mock_req.call_args[0][2]
            self.assertEqual(endpoint, expected_endpoint)
            self.assertEqual(data["model"], expected_model)

    @patch("makewand.providers.api_client._make_http_request")
    @patch("makewand.providers.api_client.get_api_config")
    def test_go_modes_interoperability_in_call_api_chat(self, mock_get_cfg, mock_req):
        """Tests that Go mode names (power, balanced) are properly normalized in call_api_chat."""
        mock_get_cfg.return_value = {"api_key": "mock-key", "base_url": ""}
        mock_req.return_value = (200, json.dumps({"choices": [{"message": {"content": "ok"}}]}), None)

        # power -> deep -> o1
        ok, out, err = call_api_chat(provider="codex", prompt="test", tier="power")
        self.assertTrue(ok)
        self.assertEqual(mock_req.call_args[0][2]["model"], "o1")

        # power -> deep -> deepseek-reasoner
        ok, out, err = call_api_chat(provider="deepseek", prompt="test", tier="power")
        self.assertTrue(ok)
        self.assertEqual(mock_req.call_args[0][2]["model"], "deepseek-reasoner")

        # balanced -> standard -> gpt-4o
        ok, out, err = call_api_chat(provider="codex", prompt="test", tier="balanced")
        self.assertTrue(ok)
        self.assertEqual(mock_req.call_args[0][2]["model"], "gpt-4o")

        # balanced -> standard -> qwen2.5-coder-32b-instruct
        ok, out, err = call_api_chat(provider="qwen", prompt="test", tier="balanced")
        self.assertTrue(ok)
        self.assertEqual(mock_req.call_args[0][2]["model"], "qwen2.5-coder-32b-instruct")


if __name__ == "__main__":
    unittest.main()
