"""
G3 reliability regressions: provider adapters.

Covers py-reliability#11 (local provider reniced the whole makewand process) and
py-reliability#8 (API timeout was only a per-recv timeout, so a trickling server
or retry chain could run far past it).
"""

import http.server
import json
import os
import socketserver
import threading
import time
import unittest
from unittest.mock import patch

from makewand.providers.api_client import _make_http_request
from makewand.providers.local import execute_local_task


class TestLocalProviderDoesNotReniceParent(unittest.TestCase):
    def _run(self, readonly):
        with patch("makewand.config.is_provider_enabled", return_value=True), \
             patch("makewand.providers.local.is_local_model_available", return_value=(True, "qwen-test", ["qwen-test"])), \
             patch("makewand.providers.local.has_active_gpu_training", return_value=(False, "")), \
             patch("makewand.providers.local.get_free_gpu_vram_mb", return_value=None), \
             patch("makewand.providers.local.unload_local_model"), \
             patch("makewand.providers.api_client.call_api_chat", return_value=(True, "answer", None)) as chat, \
             patch("os.nice") as nice:
            ok, out, err = execute_local_task("explain this", cwd="/tmp", readonly=readonly)
        return ok, chat, nice

    def test_no_os_nice_on_the_makewand_process(self):
        before = os.getpriority(os.PRIO_PROCESS, 0)
        ok, _, nice = self._run(readonly=False)
        self.assertTrue(ok)
        nice.assert_not_called()
        self.assertEqual(os.getpriority(os.PRIO_PROCESS, 0), before)

    def test_readonly_uses_reviewer_role(self):
        _, chat, _ = self._run(readonly=True)
        self.assertEqual(chat.call_args.kwargs["role"], "reviewer")


class _TrickleHandler(http.server.BaseHTTPRequestHandler):
    mode = "body"

    def log_message(self, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(length)
        if self.server.mode == "503":
            self.send_response(503)
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"no")
            return
        self.send_response(200)
        if self.server.mode == "stream":
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            chunk = json.dumps({"choices": [{"delta": {"content": "x"}}]})
            payload = f"data: {chunk}\n".encode()
        else:
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", "200")
            self.end_headers()
            payload = b"a"
        try:
            for _ in range(40):  # ~8s of trickle, each write well inside the per-recv timeout
                self.wfile.write(payload)
                self.wfile.flush()
                time.sleep(0.2)
        except (BrokenPipeError, ConnectionResetError, OSError):
            pass


class _Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class TestApiTotalDeadline(unittest.TestCase):
    def start(self, mode):
        server = _Server(("127.0.0.1", 0), _TrickleHandler)
        server.mode = mode
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return f"http://127.0.0.1:{server.server_address[1]}/v1/chat/completions"

    def _call(self, url, stream, **kw):
        started = time.monotonic()
        code, body, err = _make_http_request(url, {"content-type": "application/json"}, {"x": 1},
                                             timeout=1, stream=stream, **kw)
        return time.monotonic() - started, code, body, err

    def test_non_streaming_trickle_is_capped(self):
        elapsed, code, _, err = self._call(self.start("body"), stream=False)
        self.assertLess(elapsed, 2.5, "old behaviour: kept reading for the whole trickle")
        self.assertEqual(code, -1)
        self.assertIn("Total timeout", err)

    def test_streaming_trickle_is_capped(self):
        elapsed, code, body, err = self._call(self.start("stream"), stream=True)
        self.assertLess(elapsed, 2.5)
        self.assertEqual(code, -1)
        self.assertIn("Total timeout", err)
        self.assertTrue(set(body) <= {"x"}, "partial stream text is returned for diagnostics")

    def test_retries_stay_inside_total_deadline(self):
        elapsed, code, _, err = self._call(self.start("503"), stream=False, max_retries=5, backoff_factor=0.4)
        # Old behaviour: 0.4 + 0.8 + 1.6 + 3.2 + 6.4 s of backoff regardless of timeout=1.
        self.assertLess(elapsed, 2.0)
        self.assertNotEqual(code, 200)


if __name__ == "__main__":
    unittest.main()
