"""Real IPC/process regressions; all workers and state are temporary and model-free."""

import contextlib
import io
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import uuid
from concurrent.futures import ThreadPoolExecutor
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation

_isolation.activate()

from makewand import daemon
from makewand.daemon_context import (
    PROTOCOL_VERSION, EXIT_IPC_ERROR, EXIT_BUSY, EXIT_TIMEOUT, EXIT_CANCELLED,
    MAX_REQUEST_BYTES, validate_request, worker_environment,
)

ROOT = Path(__file__).resolve().parent.parent
WORKER_FIXTURE = """
import json, os, sys, time, subprocess
from pathlib import Path
sys.path.insert(0, %r)
request = json.loads(sys.stdin.buffer.readline())
argv = request['argv']
mode = argv[0]
if mode == 'write':
    label, barrier = argv[1:]
    Path('result.txt').write_text(label + ':' + os.getcwd())
    barrier = Path(barrier)
    (barrier / label).touch()
    deadline = time.monotonic() + 3
    while len(list(barrier.iterdir())) < 2 and time.monotonic() < deadline:
        time.sleep(0.01)
    print(label + ':' + os.getcwd(), flush=True)
elif mode == 'env':
    from makewand.config import get_api_policy
    print(json.dumps({'policy': get_api_policy(),
                      'stale': os.environ.get('MAKEWAND_STALE_ONLY'),
                      'stdin': request.get('stdin')}), flush=True)
elif mode in ('long', 'background'):
    child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)'],
                             start_new_session=True)
    Path('child.pid').write_text(str(child.pid))
    print('ready', flush=True)
    if mode == 'long':
        time.sleep(30)
elif mode == 'output':
    print('x' * 32768, flush=True)
    time.sleep(30)
elif mode == 'once':
    count = Path('count')
    count.write_text(str(int(count.read_text()) + 1) if count.exists() else '1')
    print('done', flush=True)
else:
    from makewand import cli
    from makewand.daemon_worker import execute_request
    from makewand.config import get_api_policy
    cli.cmd_status = lambda args: print(json.dumps(
        {'args': vars(args), 'policy': get_api_policy()}), flush=True)
    execute_request(request)
""" % str(ROOT)


def _request(arguments, invocation_directory, **overrides):
    value = {"version": PROTOCOL_VERSION, "request_id": uuid.uuid4().hex,
             "cmd": "execute", "argv": arguments, "cwd": str(invocation_directory),
             "env": dict(os.environ), "timeout": 5, "stdin": ""}
    value.update(overrides)
    return value


def _connect(path, request):
    client = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    client.settimeout(5)
    client.connect(str(path))
    client.sendall((json.dumps(request) + "\n").encode())
    return client


def _messages(client, until="exit"):
    messages, buffer = [], bytearray()
    deadline = time.monotonic() + 6
    while True:
        message = daemon._read_message(client, buffer, deadline)
        messages.append(message)
        if message["type"] == until:
            return messages


def _output(messages):
    return "".join(message["data"] for message in messages if message["type"] == "out")


def _running(pid):
    try:
        raw = Path(f"/proc/{pid}/stat").read_text()
        return raw[raw.rindex(")") + 2:].split()[0] != "Z"
    except OSError:
        return False


class DaemonValidationTests(unittest.TestCase):
    def test_canonical_outcomes_preserve_legacy_transport_exit_codes(self):
        from makewand.execution_contract import ExecutionResult
        request_id = uuid.uuid4().hex
        for code, status, canonical in ((0, "PASSED", 0), (EXIT_TIMEOUT, "TIMEOUT", 16),
                                        (EXIT_CANCELLED, "CANCELLED", 12), (EXIT_IPC_ERROR, "UNKNOWN", 17)):
            value = ExecutionResult.from_dict(daemon._execution_outcome(request_id, code))
            self.assertEqual((value.status, value.exit_code, value.task_id), (status, canonical, request_id))

    def test_environment_is_a_request_snapshot(self):
        with patch.dict(os.environ, {"MAKEWAND_API_POLICY": "allow_paid",
                                     "MAKEWAND_STALE_ONLY": "daemon startup"}):
            env = worker_environment({"MAKEWAND_API_POLICY": "subscription_only", "PATH": "/test"})
        self.assertEqual(env["MAKEWAND_API_POLICY"], "subscription_only")
        self.assertNotIn("MAKEWAND_STALE_ONLY", env)
        self.assertEqual(env["MAKEWAND_INSIDE_DAEMON"], "1")
        self.assertEqual(env["MAKEWAND_NO_DAEMON"], "1")

    def test_invalid_requests_fail_before_execution(self):
        with tempfile.TemporaryDirectory() as directory:
            for override in ({"version": 99}, {"argv": "status"}, {"argv": ["a\0b"]},
                             {"env": None}, {"env": {"BAD=KEY": "x"}},
                             {"cwd": "relative"}, {"stdin": 3}, {"timeout": float("nan")},
                             {"timeout": True}, {"timeout": 0}, {"request_id": "../bad"}):
                with self.subTest(override=override), self.assertRaises(ValueError):
                    validate_request(_request(["status"], directory, **override))

    def test_state_hardlinks_are_rejected_without_truncation(self):
        with tempfile.TemporaryDirectory() as directory:
            original = Path(directory) / "original"
            original.write_text("keep")
            linked = Path(directory) / "pid"
            os.link(original, linked)
            with self.assertRaises(PermissionError):
                daemon._private_open(linked, os.O_WRONLY | os.O_TRUNC)
            self.assertEqual(original.read_text(), "keep")

    def test_stale_pid_is_never_signalled(self):
        with patch("makewand.daemon.is_daemon_running", return_value=(False, 424242)), \
             patch("makewand.daemon.os.kill") as kill, contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(daemon.stop_daemon(), 0)
        kill.assert_not_called()

    def test_client_does_not_replay_after_eof_or_transmission_failure(self):
        class FakeSocket:
            def __init__(self, fail_send=False):
                self.sent = []
                self.fail_send = fail_send
            def settimeout(self, timeout): pass
            def connect(self, path): pass
            def sendall(self, data):
                self.sent.append(json.loads(data))
                if self.fail_send:
                    raise BrokenPipeError("lost after request transmission")
            def recv(self, size): return b""
            def close(self): pass
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "socket"
            path.touch()
            for fail_send in (False, True):
                fake = FakeSocket(fail_send)
                with self.subTest(fail_send=fail_send), \
                     patch("makewand.daemon.get_daemon_socket_path", return_value=path), \
                     patch("makewand.daemon._probe_daemon", return_value={"type": "pong", "version": 1}), \
                     patch("makewand.daemon.socket.socket", return_value=fake), \
                     contextlib.redirect_stderr(io.StringIO()):
                    result = daemon.try_dispatch_via_daemon(["status"], cwd=directory)
                self.assertEqual(result, EXIT_IPC_ERROR)
                self.assertEqual(sum(item["cmd"] == "execute" for item in fake.sent), 1)

    def test_legacy_protocol_is_rejected_before_sending_execution(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "socket"
            path.touch()
            with patch("makewand.daemon.get_daemon_socket_path", return_value=path), \
                 patch("makewand.daemon._probe_daemon", return_value={"type": "pong", "pid": 42}), \
                 patch("makewand.daemon.socket.socket") as socket_factory, \
                 contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(daemon.try_dispatch_via_daemon(["status"], cwd=directory), EXIT_IPC_ERROR)
            socket_factory.assert_not_called()


@unittest.skipUnless(os.name == "posix" and hasattr(socket, "AF_UNIX"), "Unix IPC")
class DaemonSocketTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="makewand-ipc-test-")
        self.root = Path(self.temp.name)
        self.fixture = self.root / "worker.py"
        self.fixture.write_text(WORKER_FIXTURE)
        self.server = None
        self.thread = None
        self.clients = []

    def start_server(self, **kwargs):
        path = self.root / "ipc.sock"
        options = {"worker_command": [sys.executable, "-I", "-u", str(self.fixture)]}
        options.update(kwargs)
        self.server = daemon.ThreadedUnixSocketServer(str(path), **options)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        return path

    def connect(self, request):
        client = _connect(self.root / "ipc.sock", request)
        self.clients.append(client)
        return client

    def execute(self, request):
        client = self.connect(request)
        try:
            return _messages(client)
        finally:
            client.close()

    def wait_completed(self, request_id):
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            status = self.server.request_status(request_id)
            if status["status"] == "completed":
                return status
            time.sleep(0.02)
        self.fail("request did not terminate")

    def tearDown(self):
        for client in self.clients:
            client.close()
        if self.server is not None:
            self.server.shutdown_workers()
            self.server.shutdown()
            self.server.server_close()
            self.thread.join(timeout=2)
        self.temp.cleanup()

    def test_concurrent_requests_keep_cwd_and_output_independent(self):
        self.start_server(max_workers=2)
        a, b, barrier = (self.root / name for name in ("a", "b", "barrier"))
        for path in (a, b, barrier):
            path.mkdir()
        original_cwd, original_stdout, original_argv = os.getcwd(), sys.stdout, list(sys.argv)
        with ThreadPoolExecutor(max_workers=2) as executor:
            fa = executor.submit(self.execute, _request(["write", "A", str(barrier)], a))
            fb = executor.submit(self.execute, _request(["write", "B", str(barrier)], b))
            result_a, result_b = fa.result(), fb.result()
        self.assertEqual(_output(result_a).strip(), "A:" + str(a))
        self.assertEqual(_output(result_b).strip(), "B:" + str(b))
        self.assertEqual((a / "result.txt").read_text(), "A:" + str(a))
        self.assertEqual((b / "result.txt").read_text(), "B:" + str(b))
        self.assertEqual(os.getcwd(), original_cwd)
        self.assertIs(sys.stdout, original_stdout)
        self.assertEqual(sys.argv, original_argv)

    def test_client_fee_policy_does_not_inherit_daemon_startup(self):
        self.start_server()
        client_env = dict(os.environ)
        client_env.pop("MAKEWAND_STALE_ONLY", None)
        client_env["MAKEWAND_API_POLICY"] = "subscription_only"
        with patch.dict(os.environ, {"MAKEWAND_API_POLICY": "allow_paid",
                                     "MAKEWAND_STALE_ONLY": "stale"}):
            messages = self.execute(_request(["env"], self.root, env=client_env, stdin="pipe text"))
        output = json.loads(_output(messages))
        self.assertEqual(output, {"policy": "subscription_only", "stale": None, "stdin": "pipe text"})
        self.assertEqual(messages[-1]["code"], 0)
        self.assertEqual(messages[-1]["result"]["status"], "PASSED")
        self.assertEqual(messages[-1]["result"]["task_id"], messages[-1]["request_id"])

    def test_full_cli_parser_keeps_status_flags(self):
        self.start_server()
        messages = self.execute(_request(["status", "--json", "--probe"], self.root))
        self.assertEqual(messages[-1]["code"], 0, messages)
        args = json.loads(_output(messages))["args"]
        self.assertIs(args["json"], True)
        self.assertIs(args["probe"], True)

    def test_public_client_preserves_cli_flags_environment_and_stdin(self):
        path = self.start_server()
        output = io.StringIO()
        with patch("makewand.daemon.get_daemon_socket_path", return_value=path), \
             patch.dict(os.environ, {"MAKEWAND_API_POLICY": "subscription_only"}), \
             contextlib.redirect_stdout(output):
            result = daemon.try_dispatch_via_daemon(["status", "--json", "--probe"],
                                                    cwd=str(self.root), stdin="pipe text", timeout=3)
        data = json.loads(output.getvalue())
        self.assertEqual(result, 0)
        self.assertEqual(data["policy"], "subscription_only")
        self.assertIs(data["args"]["json"], True)
        self.assertIs(data["args"]["probe"], True)

    def test_connection_limit_bounds_incomplete_requests(self):
        path = self.start_server(max_connections=1, request_read_timeout=1)
        slow = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.clients.append(slow)
        slow.connect(str(path))
        slow.sendall(b"{")
        deadline = time.monotonic() + 1
        while self.server.connection_slots._value and time.monotonic() < deadline:
            time.sleep(0.01)
        messages = self.execute(_request(["once"], self.root))
        self.assertEqual(messages[-1]["code"], EXIT_BUSY)
        self.assertFalse((self.root / "count").exists())

    def test_actual_worker_keeps_search_and_repomap_options_and_relative_cwd(self):
        self.start_server(worker_command=[sys.executable, "-I", "-u", str(daemon.WORKER_PATH)])
        project = self.root / "project"
        project.mkdir()
        (project / "a.py").write_text("def alpha():\n    return 'MATCH'\ndef beta():\n    return 'MATCH'\n")
        (project / "b.py").write_text("def gamma():\n    return 'MATCH'\n")
        cases = [
            ["repomap", "-C", "project", "--json", "--max-lines", "3", "--max-files", "1"],
            ["search", "MATCH", "-C", "project", "--max-results", "1", "--max-depth", "0"],
        ]
        for argv in cases:
            with self.subTest(argv=argv):
                request = _request(argv, self.root)
                messages = self.execute(request)
                standalone = subprocess.run([sys.executable, "-I", "-u", str(daemon.WORKER_PATH)],
                    input=json.dumps({"argv": argv, "stdin": ""}) + "\n", text=True,
                    cwd=self.root, env=worker_environment(request["env"]), capture_output=True, timeout=5)
                self.assertEqual(messages[-1]["code"], standalone.returncode, messages)
                self.assertEqual(_output(messages), standalone.stdout)
                if argv[0] == "repomap":
                    json.loads(_output(messages))

    def test_disconnect_cancels_worker_and_separate_session_descendant(self):
        self.start_server()
        request = _request(["long"], self.root)
        client = self.connect(request)
        self.assertEqual(daemon._read_message(client, bytearray(), time.monotonic() + 2)["type"], "accepted")
        _messages(client, until="out")
        child_pid = int((self.root / "child.pid").read_text())
        client.close()
        status = self.wait_completed(request["request_id"])
        self.assertEqual(status["code"], EXIT_CANCELLED)
        self.assertFalse(_running(child_pid))

    def test_explicit_cancel_and_result_lookup(self):
        path = self.start_server()
        request = _request(["long"], self.root)
        client = self.connect(request)
        _messages(client, until="out")
        with patch("makewand.daemon.get_daemon_socket_path", return_value=path):
            status = daemon.query_daemon_request(request["request_id"], cancel=True)
            self.assertIn(status["status"], ("running", "completed"))
            messages = _messages(client)
            result = daemon.query_daemon_request(request["request_id"])
        self.assertEqual(messages[-1]["code"], EXIT_CANCELLED)
        self.assertEqual(result["code"], EXIT_CANCELLED)

    def test_execution_deadline_cancels_process_tree(self):
        self.start_server()
        started = time.monotonic()
        messages = self.execute(_request(["long"], self.root, timeout=0.25))
        self.assertEqual(messages[-1]["code"], EXIT_TIMEOUT, messages)
        self.assertLess(time.monotonic() - started, 2)
        self.assertFalse(_running(int((self.root / "child.pid").read_text())))

    def test_deadline_cancels_descendant_after_worker_has_exited(self):
        self.start_server()
        messages = self.execute(_request(["background"], self.root, timeout=0.25))
        self.assertEqual(messages[-1]["code"], EXIT_TIMEOUT, messages)
        self.assertFalse(_running(int((self.root / "child.pid").read_text())))

    def test_output_limit_terminates_worker(self):
        self.start_server(max_output_bytes=1024)
        messages = self.execute(_request(["output"], self.root))
        self.assertEqual(messages[-1]["code"], EXIT_IPC_ERROR)
        self.assertIn("output limit", messages[-1]["error"])
        # The cancelled request releases its execution slot.
        next_messages = self.execute(_request(["once"], self.root))
        self.assertEqual(next_messages[-1]["code"], 0)

    def test_worker_limit_refuses_execution_and_ping_remains_available(self):
        self.start_server(max_workers=1)
        long_request = _request(["long"], self.root)
        client = self.connect(long_request)
        _messages(client, until="out")
        messages = self.execute(_request(["once"], self.root))
        self.assertEqual(messages[-1]["code"], EXIT_BUSY)
        self.assertFalse((self.root / "count").exists())
        ping = self.connect({"cmd": "ping"})
        pong = _messages(ping, until="pong")[-1]
        self.assertEqual(pong["active_requests"], 1)
        self.assertEqual(pong["max_workers"], 1)

    def test_duplicate_request_id_never_reexecutes(self):
        self.start_server()
        request = _request(["once"], self.root)
        self.assertEqual(self.execute(request)[-1]["code"], 0)
        self.assertEqual(self.execute(request)[-1]["code"], EXIT_BUSY)
        self.assertEqual((self.root / "count").read_text(), "1")

    def test_request_record_limit_preserves_deduplication(self):
        self.start_server()
        with patch("makewand.daemon.DAEMON_MAX_RECORDS", 1):
            first = _request(["once"], self.root)
            self.assertEqual(self.execute(first)[-1]["code"], 0)
            self.assertEqual(self.execute(_request(["once"], self.root))[-1]["code"], EXIT_BUSY)
            self.assertEqual(self.execute(first)[-1]["code"], EXIT_BUSY)
        self.assertEqual((self.root / "count").read_text(), "1")

    def test_oversize_request_is_rejected(self):
        path = self.start_server()
        client = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.clients.append(client)
        client.settimeout(3)
        client.connect(str(path))
        client.sendall(b"x" * (MAX_REQUEST_BYTES + 1))
        message = daemon._read_message(client, bytearray(), time.monotonic() + 3)
        self.assertEqual(message["code"], 2)
        self.assertIn("size limit", message["error"])

    def test_absolute_read_deadline_stops_trickling_request(self):
        path = self.start_server(request_read_timeout=0.15)
        client = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.clients.append(client)
        client.settimeout(2)
        client.connect(str(path))
        client.sendall(b"{")
        time.sleep(0.08)
        client.sendall(b" ")
        started = time.monotonic()
        message = daemon._read_message(client, bytearray(), started + 2)
        self.assertEqual(message["code"], 2)
        self.assertLess(time.monotonic() - started, 0.3)


@unittest.skipUnless(os.name == "posix" and hasattr(socket, "AF_UNIX"), "Unix lifecycle")
class DaemonLifecycleTests(unittest.TestCase):
    def test_background_start_is_singleton_and_stop_is_graceful(self):
        with tempfile.TemporaryDirectory(prefix="makewand-daemon-background-") as directory:
            root = Path(directory)
            env = dict(os.environ)
            env.update(MAKEWAND_CONFIG_DIR=str(root / "config"), XDG_RUNTIME_DIR=str(root))
            script = root / "lifecycle.py"
            script.write_text(
                "import sys\n"
                f"sys.path.insert(0, {str(ROOT)!r})\n"
                "from makewand.daemon import start_daemon, stop_daemon, is_daemon_running\n"
                "try:\n"
                "    assert start_daemon() == 0\n"
                "    running, pid = is_daemon_running()\n"
                "    assert running\n"
                "    assert start_daemon() == 0\n"
                "    assert is_daemon_running() == (True, pid)\n"
                "finally:\n"
                "    assert stop_daemon() == 0\n"
                "assert not is_daemon_running()[0]\n")
            result = subprocess.run([sys.executable, "-I", str(script)], env=env,
                                    capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertFalse((root / "makewand" / daemon.DAEMON_SOCK_NAME).exists())

    def test_startup_failure_cleans_socket_without_overwriting_unsafe_pid(self):
        with tempfile.TemporaryDirectory(prefix="makewand-daemon-bad-pid-") as directory:
            root = Path(directory)
            config = root / "config"
            config.mkdir()
            original = root / "original"
            original.write_text("keep")
            os.link(original, config / daemon.DAEMON_PID_NAME)
            env = dict(os.environ)
            env.update(MAKEWAND_CONFIG_DIR=str(config), XDG_RUNTIME_DIR=str(root))
            result = subprocess.run([sys.executable, "-I", "-u", str(daemon.WORKER_PATH), "--serve"],
                env=env, capture_output=True, text=True, timeout=3)
            self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
            self.assertEqual(original.read_text(), "keep")
            self.assertFalse((root / "makewand" / daemon.DAEMON_SOCK_NAME).exists())

    def test_foreground_signal_shutdown_cleans_own_state_and_does_not_deadlock(self):
        with tempfile.TemporaryDirectory(prefix="makewand-daemon-life-") as directory:
            root = Path(directory)
            env = dict(os.environ)
            env.update(MAKEWAND_CONFIG_DIR=str(root / "config"), XDG_RUNTIME_DIR=str(root))
            process = subprocess.Popen([sys.executable, "-I", "-u", str(daemon.WORKER_PATH), "--serve"],
                env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                sock_path = root / "makewand" / daemon.DAEMON_SOCK_NAME
                deadline = time.monotonic() + 5
                while not sock_path.exists() and process.poll() is None and time.monotonic() < deadline:
                    time.sleep(0.02)
                self.assertTrue(sock_path.exists(), process.communicate(timeout=1) if process.poll() is not None else "")
                client = _connect(sock_path, {"cmd": "ping"})
                try:
                    pong = _messages(client, until="pong")[-1]
                finally:
                    client.close()
                self.assertEqual(pong["pid"], process.pid)
                self.assertEqual(sock_path.stat().st_mode & 0o777, 0o600)
                process.send_signal(signal.SIGTERM)
                stdout, stderr = process.communicate(timeout=3)
                self.assertEqual(process.returncode, 0, stdout + stderr)
                self.assertFalse(sock_path.exists())
                self.assertFalse((root / "config" / daemon.DAEMON_PID_NAME).exists())
            finally:
                if process.poll() is None:
                    process.kill()
                process.communicate(timeout=2)


if __name__ == "__main__":
    unittest.main()
