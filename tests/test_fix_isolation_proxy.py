"""
test-isolation-proxy: this host exports a loopback HTTPS_PROXY, and the test
isolation let every loopback connection through, so an API relay inside a test
reached the real endpoint through that proxy (one Gemini request with a dummy
key). The isolation now removes every proxy variable and refuses, loudly, name
resolution of non-local hosts and connections to any loopback port that no
socket of the test process has bound; a test's own fake servers still work.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import http.server
import json
import os
import socket
import subprocess
import sys
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from pathlib import Path
from unittest.mock import patch

TESTS_DIR = Path(__file__).resolve().parent
PROXY_VARS = ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy",
              "no_proxy")

# A stand-in for a local proxy or service in another process: it records every
# connection it accepts and answers nothing.
LISTENER = r"""
import socket, sys
server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
server.bind(("127.0.0.1", 0))
server.listen(8)
print(server.getsockname()[1], flush=True)
server.settimeout(20)
try:
    while True:
        conn, _ = server.accept()
        with open(sys.argv[1], "a") as log:
            log.write("accepted\n")
        conn.close()
except OSError:
    pass
"""


class OtherProcessListener:
    def __init__(self, test):
        self.log = Path(tempfile.mkdtemp(prefix="mw-iso-listener-")) / "accepted.log"
        self.log.write_text("")
        self.proc = subprocess.Popen([sys.executable, "-I", "-c", LISTENER, str(self.log)], stdout=subprocess.PIPE,
                                     text=True)
        test.addCleanup(self.stop)
        self.port = int(self.proc.stdout.readline())

    def accepted(self):
        return self.log.read_text().splitlines()

    def stop(self):
        self.proc.kill()
        self.proc.wait(timeout=10)
        self.proc.stdout.close()


class TestProxySettingsAreRemoved(unittest.TestCase):
    def run_child(self, env):
        code = ("import json, os, sys, urllib.request\n"
                f"sys.path.insert(0, {str(TESTS_DIR)!r})\n"
                "import _isolation\n"
                "print(json.dumps({'env': sorted(k for k in os.environ if k.upper().endswith('_PROXY')),"
                " 'urllib': urllib.request.getproxies()}))\n")
        proc = subprocess.run([sys.executable, "-I", "-c", code], env=env, capture_output=True, text=True, timeout=60)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        return json.loads(proc.stdout.strip().splitlines()[-1])

    def test_fresh_and_inherited_isolation_remove_every_proxy_variable(self):
        proxies = {name: "http://127.0.0.1:7890" for name in PROXY_VARS}
        proxies.update(NO_PROXY="localhost", no_proxy="localhost")
        with tempfile.TemporaryDirectory(prefix="mw-iso-home-") as home:
            fresh = {k: v for k, v in os.environ.items() if k != "MAKEWAND_TEST_ISOLATION_ROOT"}
            fresh.update(proxies, HOME=home)
            inherited = dict(os.environ, **proxies)  # a child of this isolated process
            for name, env in (("fresh", fresh), ("inherited", inherited)):
                with self.subTest(isolation=name):
                    self.assertEqual(self.run_child(env), {"env": [], "urllib": {}})
        self.assertEqual([k for k in os.environ if k.upper().endswith("_PROXY")], [])


class TestLoopbackIsOnlyTheTestsOwn(unittest.TestCase):
    def test_a_loopback_port_of_another_process_is_refused(self):
        listener = OtherProcessListener(self)
        before = len(_isolation.blocked_connections())
        with self.assertRaises(ConnectionRefusedError) as raised:
            socket.create_connection(("127.0.0.1", listener.port), timeout=5)
        self.assertIsInstance(raised.exception, _isolation.IsolationNetworkError)
        self.assertIn("blocked by makewand test isolation", str(raised.exception))
        probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.addCleanup(probe.close)
        self.assertNotEqual(probe.connect_ex(("127.0.0.1", listener.port)), 0)
        self.assertEqual(listener.accepted(), [])
        blocked = _isolation.blocked_connections()[before:]
        self.assertTrue(any(str(listener.port) in line for line in blocked), blocked)

    def test_a_request_through_a_loopback_proxy_never_leaves(self):
        # A test that sets a proxy itself (or a library that finds one) gets nowhere.
        listener = OtherProcessListener(self)
        proxy = f"http://127.0.0.1:{listener.port}"
        with patch.dict(os.environ, {"HTTPS_PROXY": proxy, "https_proxy": proxy, "HTTP_PROXY": proxy}):
            opener = urllib.request.build_opener(urllib.request.ProxyHandler())
            with self.assertRaises(urllib.error.URLError) as raised:
                opener.open("https://generativelanguage.googleapis.com/v1beta/models", timeout=5)
        self.assertIn("makewand test isolation", str(raised.exception.reason))
        self.assertEqual(listener.accepted(), [])

    def test_non_local_names_are_not_resolved(self):
        with self.assertRaises(socket.gaierror) as raised:
            socket.getaddrinfo("makewand-isolation-probe.invalid", 443)
        self.assertIsInstance(raised.exception, _isolation.IsolationResolutionError)
        self.assertIn("blocked by makewand test isolation", str(raised.exception))
        self.assertTrue(socket.getaddrinfo("localhost", 80))
        self.assertTrue(socket.getaddrinfo("127.0.0.1", 80))

    def test_the_tests_own_loopback_server_still_works(self):
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b"fixture")

            def log_message(self, *args):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(thread.join, 5)
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        url = f"http://127.0.0.1:{server.server_address[1]}/"
        with urllib.request.urlopen(url, timeout=5) as response:
            self.assertEqual(response.read(), b"fixture")
        with urllib.request.urlopen(url.replace("127.0.0.1", "localhost"), timeout=5) as response:
            self.assertEqual(response.read(), b"fixture")

    def test_a_refused_connection_is_reported_on_stderr(self):
        listener = OtherProcessListener(self)
        code = ("import socket, sys\n"
                f"sys.path.insert(0, {str(TESTS_DIR)!r})\n"
                "import _isolation\n"
                "try:\n"
                f"    socket.create_connection(('127.0.0.1', {listener.port}), timeout=5)\n"
                "except ConnectionRefusedError as error:\n"
                "    print(type(error).__name__)\n")
        proc = subprocess.run([sys.executable, "-I", "-c", code], env=dict(os.environ), capture_output=True, text=True,
                              timeout=60)
        self.assertEqual(proc.stdout.strip(), "IsolationNetworkError")
        self.assertIn(f"makewand test isolation: blocked ('127.0.0.1', {listener.port})", proc.stderr)
        self.assertEqual(listener.accepted(), [])


if __name__ == "__main__":
    unittest.main()
