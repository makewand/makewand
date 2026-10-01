"""Bounded Unix IPC service with an independent process for every CLI request.

The daemon never changes process-wide cwd, argv, streams or execution policies.
Accepted requests are not retried automatically: a lost reply is an uncertain
result and must never cause a second task execution.
"""

import codecs
import json
import os
import random
import selectors
import signal
import socket
import socketserver
import stat
import subprocess
import sys
import threading
import time
import uuid
from collections import OrderedDict
from pathlib import Path
from typing import Iterable, List, Optional, Tuple

from makewand import filelock
from makewand.config import (CONFIG_DIR, ensure_config_dir, ensure_private_dir,
                             c, COLOR_BOLD, COLOR_GREEN, COLOR_YELLOW)
from makewand.daemon_context import (PROTOCOL_VERSION, MAX_REQUEST_BYTES,
    MAX_MESSAGE_BYTES, MAX_OUTPUT_BYTES, DEFAULT_TIMEOUT,
    REQUEST_READ_TIMEOUT, SEND_TIMEOUT, OUTPUT_CHUNK_BYTES, EXIT_IPC_ERROR,
    EXIT_BUSY, EXIT_TIMEOUT, EXIT_CANCELLED, validate_request, worker_environment,
    terminate_worker)

DAEMON_SOCK_NAME = "makewand.sock"
DAEMON_PID_NAME = "makewand.pid"
DAEMON_LOG_NAME = "daemon.log"
DAEMON_MAX_WORKERS = 4
DAEMON_MAX_CONNECTIONS = 12
DAEMON_MAX_RECORDS = 4096
DAEMON_RECORD_TTL = 3600.0
WORKER_PATH = Path(__file__).with_name("daemon_worker.py").resolve()


def _execution_outcome(request_id, code, error=None):
    """Canonical metadata alongside the version-1 transport exit code."""
    from makewand.execution_contract import ExecutionResult, STATUS_CODES
    status = {EXIT_IPC_ERROR: "UNKNOWN", EXIT_BUSY: "UNVERIFIED",
              EXIT_TIMEOUT: "TIMEOUT", EXIT_CANCELLED: "CANCELLED"}.get(code)
    if status is None:
        status = next((name for name, value in STATUS_CODES.items() if value == code), "UNKNOWN")
    return ExecutionResult(status == "PASSED", None, error, status=status,
        task_id=request_id, stage="command", engine="daemon",
        outcome_known=status not in ("UNKNOWN", "TIMEOUT", "CANCELLED")).to_dict()


def get_daemon_socket_path() -> Path:
    ensure_config_dir()
    runtime_dir = os.environ.get("XDG_RUNTIME_DIR")
    if runtime_dir and os.path.isabs(runtime_dir) and os.path.isdir(runtime_dir):
        try:
            return ensure_private_dir(Path(runtime_dir) / "makewand") / DAEMON_SOCK_NAME
        except OSError:
            pass
    return CONFIG_DIR / DAEMON_SOCK_NAME


def get_daemon_pid_path() -> Path:
    ensure_config_dir()
    return CONFIG_DIR / DAEMON_PID_NAME


def get_daemon_log_path() -> Path:
    ensure_config_dir()
    return CONFIG_DIR / DAEMON_LOG_NAME


def _send_message(sock, message, timeout=SEND_TIMEOUT):
    payload = (json.dumps(message, ensure_ascii=False) + "\n").encode("utf-8")
    if len(payload) > MAX_MESSAGE_BYTES:
        raise ValueError("daemon message exceeds size limit")
    sock.settimeout(timeout)
    sock.sendall(payload)


class DaemonIOStream:
    """Compatibility stream adapter; execution workers use OS stdout pipes."""
    encoding = "utf-8"
    errors = "replace"

    def __init__(self, sock, stream_type, lock):
        self.sock, self.stream_type, self.lock = sock, stream_type, lock

    def write(self, value):
        if not value:
            return 0
        with self.lock:
            for offset in range(0, len(value), OUTPUT_CHUNK_BYTES // 4):
                payload = {"type": self.stream_type,
                           "data": value[offset:offset + OUTPUT_CHUNK_BYTES // 4]}
                self.sock.sendall((json.dumps(payload, ensure_ascii=False) + "\n").encode())
        return len(value)

    def writelines(self, lines: Iterable[str]):
        for line in lines:
            self.write(line)

    def flush(self):
        pass

    def isatty(self):
        return True

    def writable(self):
        return True

    def readable(self):
        return False

    def seekable(self):
        return False


class ThreadedUnixSocketServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True
    block_on_close = False
    # SO_REUSEADDR has no purpose for Unix sockets.
    allow_reuse_address = False

    def __init__(self, address, handler=None, *, max_workers=DAEMON_MAX_WORKERS,
                 max_connections=DAEMON_MAX_CONNECTIONS, max_output_bytes=MAX_OUTPUT_BYTES,
                 request_read_timeout=REQUEST_READ_TIMEOUT, worker_command=None):
        self.start_time = time.time()
        self.stopping = threading.Event()
        self.worker_slots = threading.BoundedSemaphore(max_workers)
        self.connection_slots = threading.BoundedSemaphore(max_connections)
        self.max_workers = max_workers
        self.max_output_bytes = max_output_bytes
        self.request_read_timeout = request_read_timeout
        self.worker_command = worker_command or [sys.executable, "-I", "-u", str(WORKER_PATH)]
        self.records = OrderedDict()
        self.records_lock = threading.Lock()
        super().__init__(address, handler or DaemonRequestHandler)

    def process_request(self, request, client_address):
        if self.stopping.is_set() or not self.connection_slots.acquire(blocking=False):
            try:
                _send_message(request, {"type": "exit", "code": EXIT_BUSY,
                                        "error": "daemon connection limit reached"})
            except (OSError, ValueError):
                pass
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except BaseException:
            self.connection_slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self.connection_slots.release()

    def reserve(self, request_id):
        with self.records_lock:
            now = time.monotonic()
            expired = [key for key, value in self.records.items()
                       if value.get("finished") is not None
                       and now - value["finished"] > DAEMON_RECORD_TTL]
            for key in expired:
                del self.records[key]
            if request_id in self.records:
                return False
            # Never evict a live deduplication record to accept a new task.
            if len(self.records) >= DAEMON_MAX_RECORDS:
                return False
            self.records[request_id] = {"status": "accepted", "cancel": threading.Event(),
                                       "process": None, "finished": None}
            return True

    def request_status(self, request_id):
        with self.records_lock:
            record = self.records.get(request_id)
            if record is None:
                return {"request_id": request_id, "status": "unknown"}
            return {"request_id": request_id, **{key: value for key, value in record.items()
                    if key in ("status", "code", "error", "result")}}

    def cancel_request(self, request_id):
        with self.records_lock:
            record = self.records.get(request_id)
            if record is None or record["finished"] is not None:
                return False
            record["cancel"].set()
            return True

    def finish_execution(self, request_id, code, error=None):
        with self.records_lock:
            record = self.records[request_id]
            record.update(status="completed", code=code, error=error,
                          result=_execution_outcome(request_id, code, error),
                          process=None, finished=time.monotonic())

    def shutdown_workers(self):
        self.stopping.set()
        with self.records_lock:
            active = [record for record in self.records.values() if record["finished"] is None]
            for record in active:
                record["cancel"].set()
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            with self.records_lock:
                if all(record["finished"] is not None for record in active):
                    return
            time.sleep(0.02)


class DaemonRequestHandler(socketserver.StreamRequestHandler):
    def _reply(self, message):
        _send_message(self.request, message)

    def handle(self):
        try:
            request = _read_message(self.request, bytearray(),
                time.monotonic() + self.server.request_read_timeout, MAX_REQUEST_BYTES)
            command = request.get("cmd")
            if command == "ping":
                with self.server.records_lock:
                    active = sum(record["finished"] is None for record in self.server.records.values())
                self._reply({"type": "pong", "version": PROTOCOL_VERSION, "pid": os.getpid(),
                             "uptime": time.time() - self.server.start_time,
                             "active_requests": active, "max_workers": self.server.max_workers})
                return
            if command in ("result", "cancel"):
                if request.get("version") != PROTOCOL_VERSION:
                    raise ValueError("unsupported daemon protocol")
                request_id = request.get("request_id")
                if not isinstance(request_id, str):
                    raise ValueError("request_id is required")
                if command == "cancel":
                    self.server.cancel_request(request_id)
                self._reply({"type": "result", **self.server.request_status(request_id)})
                return
            validate_request(request)
            if self.server.stopping.is_set() or not self.server.worker_slots.acquire(blocking=False):
                self._reply({"type": "exit", "code": EXIT_BUSY,
                             "request_id": request["request_id"], "error": "daemon worker limit reached"})
                return
            try:
                request_id = request["request_id"]
                if not self.server.reserve(request_id):
                    self._reply({"type": "exit", "code": EXIT_BUSY, "request_id": request_id,
                                 "error": "request already accepted or result retention limit reached"})
                    return
                self._run_worker(request)
            finally:
                self.server.worker_slots.release()
        except (OSError, ValueError, TypeError) as exc:
            try:
                self._reply({"type": "exit", "code": 2, "error": str(exc)[:1024]})
            except OSError:
                pass

    def _run_worker(self, request):
        request_id = request["request_id"]
        deadline = time.monotonic() + request.get("timeout", DEFAULT_TIMEOUT)
        process = None
        code, error = EXIT_IPC_ERROR, None
        selector = selectors.DefaultSelector()
        writer = None
        try:
            with self.server.records_lock:
                cancel = self.server.records[request_id]["cancel"]
            if cancel.is_set() or self.server.stopping.is_set():
                code, error = EXIT_CANCELLED, "daemon request cancelled before execution"
                return
            self._reply({"type": "accepted", "version": PROTOCOL_VERSION,
                         "request_id": request_id})
            environment = worker_environment(request["env"])
            environment["MAKEWAND_DAEMON_REQUEST_ID"] = request_id
            process = subprocess.Popen(self.server.worker_command, cwd=request["cwd"],
                env=environment, stdin=subprocess.PIPE,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
            process.request_id = request_id
            with self.server.records_lock:
                record = self.server.records[request_id]
                record.update(status="running", process=process)
                cancel = record["cancel"]
            payload = (json.dumps({"argv": request["argv"], "stdin": request.get("stdin", ""),
                                  "request_id": request_id,
                                  "deadline_unix_ms": int(time.time() * 1000 + max(0, deadline - time.monotonic()) * 1000)},
                                  ensure_ascii=False)
                       + "\n").encode()

            def feed():
                try:
                    process.stdin.write(payload)
                    process.stdin.flush()
                except (OSError, ValueError):
                    pass
                finally:
                    process.stdin.close()

            writer = threading.Thread(target=feed, daemon=True)
            writer.start()
            decoders = {}
            for stream, kind in ((process.stdout, "out"), (process.stderr, "err")):
                os.set_blocking(stream.fileno(), False)
                selector.register(stream, selectors.EVENT_READ, kind)
                decoders[kind] = codecs.getincrementaldecoder("utf-8")("replace")
            selector.register(self.request, selectors.EVENT_READ, "client")
            total_output, open_streams, controls = 0, 2, bytearray()
            while open_streams or process.poll() is None:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    code, error = EXIT_TIMEOUT, "daemon request deadline exceeded"
                    break
                if cancel.is_set() or self.server.stopping.is_set():
                    code, error = EXIT_CANCELLED, "daemon request cancelled"
                    break
                for key, _ in selector.select(min(0.05, remaining)):
                    if key.data == "client":
                        chunk = self.request.recv(1024)
                        if not chunk:
                            code, error = EXIT_CANCELLED, "client disconnected; task cancelled"
                            cancel.set()
                            break
                        controls.extend(chunk)
                        if len(controls) > 4096:
                            raise ValueError("control message exceeds size limit")
                        if b"\n" in controls:
                            control_line, _, rest = controls.partition(b"\n")
                            controls = bytearray(rest)
                            control = json.loads(control_line)
                            if (not isinstance(control, dict) or control.get("cmd") != "cancel"
                                    or control.get("request_id") != request_id):
                                raise ValueError("invalid request control message")
                            cancel.set()
                        continue
                    chunk = os.read(key.fileobj.fileno(), OUTPUT_CHUNK_BYTES)
                    if not chunk:
                        selector.unregister(key.fileobj)
                        open_streams -= 1
                        data = decoders[key.data].decode(b"", final=True)
                    else:
                        total_output += len(chunk)
                        if total_output > self.server.max_output_bytes:
                            code, error = EXIT_IPC_ERROR, "daemon output limit exceeded"
                            cancel.set()
                            break
                        data = decoders[key.data].decode(chunk)
                    if data:
                        _send_message(self.request, {"type": key.data, "request_id": request_id,
                                                   "data": data}, min(SEND_TIMEOUT, remaining))
                if cancel.is_set():
                    if error is None:
                        code, error = EXIT_CANCELLED, "daemon request cancelled"
                    break
            else:
                returncode = process.wait()
                code = returncode if returncode >= 0 else 128 - returncode
        except (OSError, ValueError, TypeError) as exc:
            code, error = EXIT_IPC_ERROR, str(exc)[:1024]
        finally:
            if process is not None:
                if error is not None or process.poll() is None:
                    terminate_worker(process)
                if writer is not None:
                    writer.join(timeout=1)
                for stream in (process.stdout, process.stderr):
                    stream.close()
            selector.close()
            self.server.finish_execution(request_id, code, error)
            try:
                self._reply({"type": "exit", "request_id": request_id, "code": code,
                             "result": _execution_outcome(request_id, code, error),
                             **({"error": error} if error else {})})
            except OSError:
                pass


def _read_message(sock, buffer, deadline, limit=MAX_MESSAGE_BYTES):
    while b"\n" not in buffer:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("daemon response deadline exceeded")
        sock.settimeout(remaining)
        chunk = sock.recv(min(8192, limit + 1 - len(buffer)))
        if not chunk:
            raise ConnectionError("daemon disconnected without a terminal result")
        buffer.extend(chunk)
        if len(buffer) > limit and b"\n" not in buffer:
            raise ValueError("daemon response exceeds size limit")
    line, _, remainder = buffer.partition(b"\n")
    buffer[:] = remainder
    if len(line) > limit:
        raise ValueError("daemon response exceeds size limit")
    message = json.loads(line)
    if not isinstance(message, dict):
        raise ValueError("invalid daemon response")
    return message


def _read_pid(path):
    fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(fd, "r") as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1:
            raise ValueError("invalid PID file")
        pid = int(handle.read(64).strip())
    if pid <= 0:
        raise ValueError("invalid PID")
    return pid


def _probe_daemon(path):
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        sock.settimeout(0.6)
        sock.connect(str(path))
        _send_message(sock, {"cmd": "ping"}, timeout=0.6)
        return _read_message(sock, bytearray(), time.monotonic() + 0.6)
    finally:
        sock.close()


def is_daemon_running() -> Tuple[bool, Optional[int]]:
    try:
        pid = _read_pid(get_daemon_pid_path())
    except (OSError, ValueError):
        return False, None
    try:
        response = _probe_daemon(get_daemon_socket_path())
        # Recognise a legacy daemon as live: never unlink its active socket or
        # launch a second server over an instance from before the upgrade.
        return response.get("type") == "pong" and response.get("pid") == pid, pid
    except (OSError, ValueError):
        return False, pid


def try_dispatch_via_daemon(argv: List[str], cwd: Optional[str] = None,
                            timeout: Optional[float] = None,
                            stdin: Optional[str] = None,
                            max_retries: Optional[int] = None,
                            fallback_on_busy: Optional[bool] = None) -> Optional[int]:
    """Return None before connecting or on slot saturation fallback; cwd is the invocation directory.

    Once connected and accepted, transmission or result errors return a nonzero exit code.
    If the daemon rejects a request prior to acceptance due to slot saturation (EXIT_BUSY),
    the client performs exponential backoff retries and, if still saturated, falls back
    to returning None so the command can run as a standalone process.
    """
    if os.environ.get("MAKEWAND_NO_DAEMON") == "1" or os.environ.get("MAKEWAND_INSIDE_DAEMON") == "1":
        return None
    sock_path = get_daemon_socket_path()
    if not sock_path.exists():
        return None
    duration = DEFAULT_TIMEOUT if timeout is None else timeout
    overall_deadline = time.monotonic() + duration

    if max_retries is None:
        try:
            max_retries = int(os.environ.get("MAKEWAND_DAEMON_MAX_RETRIES", "3"))
        except ValueError:
            max_retries = 3
    if max_retries < 0:
        max_retries = 0

    if fallback_on_busy is None:
        allow_fallback = (
            os.environ.get("MAKEWAND_DAEMON_FALLBACK", "1").strip().lower() not in ("0", "false", "no")
            and os.environ.get("MAKEWAND_DAEMON_REQUIRE") != "1"
        )
    else:
        allow_fallback = bool(fallback_on_busy)

    base_request = {"version": PROTOCOL_VERSION, "cmd": "execute",
                    "argv": list(argv), "cwd": os.path.abspath(cwd or os.getcwd()),
                    "env": dict(os.environ), "timeout": duration, "stdin": stdin or ""}
    try:
        validate_request(dict(base_request, request_id=uuid.uuid4().hex))
        payload_test = (json.dumps(dict(base_request, request_id=uuid.uuid4().hex), ensure_ascii=False) + "\n").encode()
        if len(payload_test) > MAX_REQUEST_BYTES:
            raise ValueError("daemon request exceeds size limit")
    except (ValueError, TypeError) as exc:
        print(f"Makewand daemon: {exc}", file=sys.stderr)
        return 2

    try:
        pong = _probe_daemon(sock_path)
    except (OSError, ValueError):
        # No execution request has been sent, so standalone execution is safe.
        return None
    if pong.get("type") != "pong" or pong.get("version") != PROTOCOL_VERSION:
        print("Makewand daemon protocol mismatch; restart the daemon or use --no-daemon",
              file=sys.stderr)
        return EXIT_IPC_ERROR

    backoff_base = 0.05
    last_busy_error = None
    is_busy = False
    request_id = ""

    try:
        for attempt in range(max_retries + 1):
            remaining = overall_deadline - time.monotonic()
            if remaining < 0.01:
                break

            request_id = uuid.uuid4().hex
            timeout_val = max(0.01, min(remaining, duration))
            attempt_request = dict(base_request, request_id=request_id, timeout=timeout_val)
            try:
                payload = (json.dumps(attempt_request, ensure_ascii=False) + "\n").encode()
                if len(payload) > MAX_REQUEST_BYTES:
                    raise ValueError("daemon request exceeds size limit")
            except (ValueError, TypeError) as exc:
                print(f"Makewand daemon: {exc}", file=sys.stderr)
                return 2

            sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            try:
                sock.settimeout(min(0.6, remaining))
                sock.connect(str(sock_path))
            except OSError:
                sock.close()
                if allow_fallback or attempt == 0:
                    return None
                return EXIT_IPC_ERROR

            attempt_deadline = time.monotonic() + timeout_val
            is_busy = False
            accepted = False
            try:
                sock.settimeout(min(SEND_TIMEOUT, remaining))
                sock.sendall(payload)
                buffer, total_output = bytearray(), 0
                while True:
                    message = _read_message(sock, buffer, attempt_deadline)
                    kind = message.get("type")
                    if message.get("request_id") not in (None, request_id):
                        raise ValueError("daemon response request_id mismatch")
                    if kind == "accepted":
                        if (accepted or message.get("version") != PROTOCOL_VERSION
                                or message.get("request_id") != request_id):
                            raise ValueError("invalid daemon acknowledgment")
                        accepted = True
                    elif kind in ("out", "err"):
                        if not accepted or not isinstance(message.get("data"), str):
                            raise ValueError("invalid daemon output")
                        total_output += len(message["data"].encode("utf-8"))
                        if total_output > MAX_OUTPUT_BYTES:
                            raise ValueError("daemon output exceeds size limit")
                        stream = sys.stdout if kind == "out" else sys.stderr
                        stream.write(message["data"])
                        stream.flush()
                    elif kind == "exit":
                        code = message.get("code")
                        if isinstance(code, bool) or not isinstance(code, int) or not 0 <= code <= 255:
                            raise ValueError("invalid daemon exit code")
                        if not accepted and code == 0:
                            raise ValueError("daemon did not acknowledge request execution")

                        if not accepted and code == EXIT_BUSY:
                            # Daemon slot saturation: rejected before acceptance (worker or connection limit)
                            is_busy = True
                            last_busy_error = message.get("error") or "daemon slots saturated"
                            break

                        if "result" in message:
                            from makewand.execution_contract import ExecutionResult
                            from makewand.workflow import remember_result
                            result = ExecutionResult.from_dict(message["result"])
                            if result.task_id != request_id:
                                raise ValueError("daemon result task_id mismatch")
                            if result.status != _execution_outcome(request_id, code).get("status"):
                                raise ValueError("daemon result does not match transport outcome")
                            remember_result(result)
                        if message.get("error"):
                            print(f"Makewand daemon: {message['error']} (request {request_id})", file=sys.stderr)
                        return code
                    else:
                        raise ValueError("invalid daemon response type")
            except (OSError, ValueError, TypeError) as exc:
                print(f"Makewand daemon: {exc}; result uncertain, task was not replayed "
                      f"(request {request_id})", file=sys.stderr)
                return EXIT_TIMEOUT if isinstance(exc, TimeoutError) else EXIT_IPC_ERROR
            finally:
                if accepted:
                    try:
                        _send_message(sock, {"version": PROTOCOL_VERSION, "cmd": "cancel",
                                            "request_id": request_id}, timeout=0.1)
                    except OSError:
                        pass
                sock.close()

            if is_busy:
                if attempt < max_retries:
                    backoff = backoff_base * (2 ** attempt) + random.uniform(0.005, 0.02)
                    if time.monotonic() + backoff < overall_deadline:
                        time.sleep(backoff)
                        continue
                break
    except KeyboardInterrupt:
        return EXIT_CANCELLED

    if is_busy or last_busy_error:
        if last_busy_error:
            print(f"Makewand daemon: {last_busy_error} (request {request_id})", file=sys.stderr)
        if allow_fallback:
            print("Makewand daemon: workers saturated after retries; falling back to standalone process",
                  file=sys.stderr)
            return None
        return EXIT_BUSY

    if allow_fallback:
        return None
    return EXIT_TIMEOUT


def query_daemon_request(request_id, *, cancel=False, timeout=0.6):
    """Inspect/cancel a request without replaying it (records retained for one hour)."""
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sock:
        sock.settimeout(timeout)
        sock.connect(str(get_daemon_socket_path()))
        _send_message(sock, {"version": PROTOCOL_VERSION,
                            "cmd": "cancel" if cancel else "result", "request_id": request_id}, timeout)
        return _read_message(sock, bytearray(), time.monotonic() + timeout)


def _private_open(path, flags):
    fd = os.open(path, (flags & ~os.O_TRUNC) | os.O_CREAT | os.O_NONBLOCK
                 | getattr(os, "O_NOFOLLOW", 0), 0o600)
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1:
        os.close(fd)
        raise PermissionError(f"refusing unsafe daemon state file: {path}")
    os.fchmod(fd, 0o600)
    if flags & os.O_TRUNC:
        os.ftruncate(fd, 0)
    return fd


def _unlink_owned(path, identity):
    try:
        info = os.lstat(path)
        if (info.st_dev, info.st_ino) == identity:
            path.unlink()
    except FileNotFoundError:
        pass


def _serve_daemon():
    ensure_config_dir()
    lock_fd = _private_open(CONFIG_DIR / "daemon.lock", os.O_RDWR)
    server, sock_identity, pid_identity = None, None, None
    previous_handlers = {}
    sock_path, pid_path = get_daemon_socket_path(), get_daemon_pid_path()
    try:
        try:
            filelock.flock(lock_fd, filelock.LOCK_EX | filelock.LOCK_NB)
        except OSError:
            print("Makewand daemon is already running or starting", file=sys.stderr)
            return 1
        if os.path.lexists(sock_path):
            info = os.lstat(sock_path)
            if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid():
                raise PermissionError(f"refusing unsafe daemon socket entry: {sock_path}")
            sock_path.unlink()
        server = ThreadedUnixSocketServer(str(sock_path))
        info = os.lstat(sock_path)
        sock_identity = (info.st_dev, info.st_ino)
        os.chmod(sock_path, 0o600)
        pid_fd = _private_open(pid_path, os.O_WRONLY | os.O_TRUNC)
        with os.fdopen(pid_fd, "w") as handle:
            handle.write(str(os.getpid()))
            handle.flush()
            os.fsync(handle.fileno())
            info = os.fstat(handle.fileno())
            pid_identity = (info.st_dev, info.st_ino)

        def request_shutdown(signum, frame):
            # shutdown() in the serving thread deadlocks; only set a loop flag.
            server.stopping.set()

        for signum in (signal.SIGTERM, signal.SIGINT):
            previous_handlers[signum] = signal.signal(signum, request_shutdown)
        print(f"Makewand daemon ready (PID {os.getpid()}, socket {sock_path})", flush=True)
        server.timeout = 0.2
        while not server.stopping.is_set():
            server.handle_request()
        return 0
    finally:
        if server is not None:
            server.shutdown_workers()
            server.server_close()
        if sock_identity is not None:
            _unlink_owned(sock_path, sock_identity)
        if pid_identity is not None:
            _unlink_owned(pid_path, pid_identity)
        for signum, handler in previous_handlers.items():
            signal.signal(signum, handler)
        os.close(lock_fd)


def start_daemon(foreground: bool = False) -> int:
    if os.name != "posix":
        print("Makewand daemon requires Unix sockets; use --no-daemon on this platform", file=sys.stderr)
        return 2
    running, pid = is_daemon_running()
    if running:
        print(c(f"✔ Makewand 守护进程已在运行中 (PID: {pid})", COLOR_GREEN))
        return 0
    if foreground:
        try:
            return _serve_daemon()
        except OSError as exc:
            print(f"Makewand daemon startup failed: {exc}", file=sys.stderr)
            return 1
    log_path = get_daemon_log_path()
    try:
        fd = _private_open(log_path, os.O_WRONLY | os.O_APPEND)
        with os.fdopen(fd, "wb") as log:
            child = subprocess.Popen([sys.executable, "-I", "-u", str(WORKER_PATH), "--serve"],
                stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True)
    except OSError as exc:
        print(f"Makewand daemon startup failed: {exc}", file=sys.stderr)
        return 1
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        running, pid = is_daemon_running()
        if running:
            print(c(f"🚀 Makewand 常驻守护进程已启动 (PID: {pid})", COLOR_GREEN + COLOR_BOLD))
            return 0
        if child.poll() is not None:
            break
        time.sleep(0.05)
    print(f"Makewand daemon did not become ready; inspect {log_path}", file=sys.stderr)
    return 1


def stop_daemon() -> int:
    running, pid = is_daemon_running()
    if not running:
        print(c("ℹ️ Makewand 守护进程当前未运行", COLOR_YELLOW))
        return 0
    pid_fd = None
    try:
        if hasattr(os, "pidfd_open") and hasattr(signal, "pidfd_send_signal"):
            pid_fd = os.pidfd_open(pid)
            signal.pidfd_send_signal(pid_fd, signal.SIGTERM)
        else:
            os.kill(pid, signal.SIGTERM)
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if not is_daemon_running()[0]:
                print(c("✔ Makewand 守护进程已停止", COLOR_GREEN))
                return 0
            time.sleep(0.05)
        print("Makewand daemon has not stopped; active requests were not replayed", file=sys.stderr)
        return 1
    except ProcessLookupError:
        return 0
    except OSError as exc:
        print(f"Makewand daemon stop failed: {exc}", file=sys.stderr)
        return 1
    finally:
        if pid_fd is not None:
            os.close(pid_fd)


def daemon_status_cmd() -> int:
    running, pid = is_daemon_running()
    print(c("Makewand 常驻守护进程: " + ("运行中" if running else "未运行"), COLOR_BOLD))
    if running:
        print(f"• PID: {pid}\n• IPC: {get_daemon_socket_path()}\n• 日志: {get_daemon_log_path()}")
        print(f"• 每请求独立进程，最多 {DAEMON_MAX_WORKERS} 个并发任务；断连不自动重试")
    else:
        print("• 运行 'makewand daemon start' 启动服务")
    return 0
