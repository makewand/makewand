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
from unittest.mock import patch, mock_open, MagicMock

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


class TestMuseSandboxPassthrough(unittest.TestCase):
    def test_muse_sandboxed_direct_passthrough_no_disable_sandbox(self):
        from makewand.providers.muse import get_muse_executable
        wrapper_content = '#!/usr/bin/env bash\n# muse-guard\nREAL="/home/alice/.local/libexec/muse-bin/muse"\n'
        with patch("builtins.open", mock_open(read_data=wrapper_content)), \
             patch("os.path.isfile", return_value=True), \
             patch("os.access", return_value=True), \
             patch("shutil.which", return_value="/home/alice/.local/bin/muse"):
            exe, flags = get_muse_executable(sandboxed=True)
            self.assertTrue(exe.endswith("muse-bin/muse"))
            self.assertNotIn("--disable-sandbox", flags)
            self.assertEqual(flags, [])

    def test_muse_host_with_dbus_returns_default(self):
        from makewand.providers.muse import get_muse_executable
        with patch("makewand.providers.muse.detect_muse_guard", return_value=(False, None)), \
             patch("shutil.which", return_value="/home/alice/.local/bin/muse"):
            exe, flags = get_muse_executable(sandboxed=False)
            self.assertEqual(exe, "/home/alice/.local/bin/muse")
            self.assertEqual(flags, [])

    def test_dbus_check_detects_dead_socket_path(self):
        from makewand.providers.muse import _is_dbus_systemd_available
        with patch.dict("os.environ", {"DBUS_SESSION_BUS_ADDRESS": "unix:path=/nonexistent/bus/socket"}), \
             patch("shutil.which", return_value="/usr/bin/systemd-run"):
            self.assertFalse(_is_dbus_systemd_available())

    def test_muse_guard_fail_closed_when_inactive(self):
        from makewand.providers.muse import verify_muse_guard
        import subprocess
        mock_res = MagicMock()
        mock_res.returncode = 3
        with patch.dict("os.environ", {"MUSE_ALLOW_UNGUARDED": "0"}, clear=False), \
             patch("shutil.which", return_value="/bin/systemctl"), \
             patch("subprocess.run", return_value=mock_res):
            ok, reason = verify_muse_guard()
            self.assertFalse(ok)
            self.assertIn("muse-guard", reason)

    def test_muse_guard_bypassed_with_env_flag(self):
        from makewand.providers.muse import verify_muse_guard
        with patch.dict("os.environ", {"MUSE_ALLOW_UNGUARDED": "1"}, clear=False):
            ok, _ = verify_muse_guard()
            self.assertTrue(ok)

    def test_execute_muse_task_wrapped_in_systemd_run_slice(self):
        from makewand.providers.muse import execute_muse_task
        with patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.health.load_status_cache", return_value={}), \
             patch("makewand.sandbox.is_bwrap_available", return_value=True), \
             patch("makewand.providers.muse._is_dbus_systemd_available", return_value=True), \
             patch("makewand.providers.muse.detect_muse_guard", return_value=(True, "/home/alice/.local/libexec/muse-bin/muse")), \
             patch("makewand.providers.muse.verify_muse_guard", return_value=(True, "")), \
             patch("makewand.sandbox.wrap_bwrap", side_effect=lambda cmd, **kwargs: ["bwrap"] + cmd), \
             patch("makewand.providers.muse.run_subprocess", return_value=(0, "ok", "", None)) as mock_run:
            success, out, err = execute_muse_task("echo test", cwd="/tmp", readonly=True)
            self.assertTrue(success)
            self.assertTrue(mock_run.called)
            called_cmd = mock_run.call_args[0][0]
            # Verify systemd-run in muse.slice wraps bwrap
            self.assertEqual(called_cmd[0], "systemd-run")
            self.assertIn("--slice=muse", called_cmd)
            self.assertIn("-p", called_cmd)
            self.assertIn("MemoryMax=16G", called_cmd)
            self.assertIn("-p", called_cmd)
            self.assertIn("MemorySwapMax=0", called_cmd)
            self.assertIn("bwrap", called_cmd)
            # Verify no --disable-sandbox
            self.assertNotIn("--disable-sandbox", called_cmd)

    def test_execute_muse_task_fails_closed_when_guard_down(self):
        from makewand.providers.muse import execute_muse_task
        with patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.health.load_status_cache", return_value={}), \
             patch("makewand.providers.muse.detect_muse_guard", return_value=(True, "/home/alice/.local/libexec/muse-bin/muse")), \
             patch("makewand.providers.muse.verify_muse_guard", return_value=(False, "guard inactive")), \
             patch("makewand.providers.muse.run_subprocess") as mock_run:
            success, out, err = execute_muse_task("echo test", cwd="/tmp", readonly=True)
            self.assertFalse(success)
            self.assertIn("fail-closed", str(err))
            self.assertFalse(mock_run.called)


if __name__ == "__main__":
    unittest.main()
