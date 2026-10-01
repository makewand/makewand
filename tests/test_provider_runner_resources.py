"""Offline resource regressions for the actual provider subprocess runner."""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import os
import signal
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand.providers import base


@unittest.skipUnless(os.name == "posix", "runner pipes and new-session groups require POSIX")
class ProviderRunnerResourceTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="makewand-runner-resource-")
        self.addCleanup(self.directory.cleanup)

    def run_child(self, script, **options):
        return base.run_subprocess([sys.executable, "-I", "-c", script],
                                   cwd=self.directory.name, **options)

    def assert_not_running(self, pid):
        deadline = time.monotonic() + 2
        while time.monotonic() < deadline:
            try:
                os.kill(pid, 0)
            except ProcessLookupError:
                return
            # An orphan killed by this runner may await reaping by the host's
            # init process. It is already terminated and cannot keep executing.
            status = Path(f"/proc/{pid}/stat")
            try:
                if status.read_text().split(")", 1)[1].split()[0] == "Z":
                    return
            except (FileNotFoundError, ProcessLookupError):
                return
            time.sleep(.01)
        self.fail(f"runner child {pid} is still executing")

    def cleanup_child(self, pid):
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass

    def test_nonstream_preserves_separate_streams_and_newlines(self):
        code, out, err, error = self.run_child(
            "import os; os.write(1, b'out\\r\\n'); os.write(2, b'err\\r\\n')")
        self.assertEqual((code, out, err, error), (0, "out\n", "err\n", None))

    def test_large_stdout_and_stderr_share_one_hard_byte_limit(self):
        limit = 8192
        for script in (
            "import os; os.write(1, b'a' * 5000); os.write(2, b'b' * 5000)",
            "import os; os.write(2, b'b' * 20000)",
        ):
            with self.subTest(script=script), patch.object(base, "MAX_OUTPUT_BYTES", limit):
                code, out, err, error = self.run_child(script, timeout=2)
                self.assertEqual(code, -1)
                self.assertEqual(error.execution_status, "UNKNOWN")
                self.assertIn("stdout/stderr limit", error)
                self.assertEqual(len(out.encode()) + len(err.encode()), limit)

    def test_exact_byte_limit_is_allowed(self):
        with patch.object(base, "MAX_OUTPUT_BYTES", 1024):
            actual = self.run_child("import os; os.write(1, b'a' * 512); os.write(2, b'b' * 512)")
        self.assertEqual(actual, (0, "a" * 512, "b" * 512, None))

    def test_stream_limit_stops_display_and_returns_unknown(self):
        shown = io.StringIO()
        with patch.object(base, "MAX_OUTPUT_BYTES", 4096), contextlib.redirect_stdout(shown):
            code, out, err, error = self.run_child(
                "import os; os.write(1, b'a' * 3000); os.write(2, b'b' * 3000)", stream=True)
        self.assertEqual(code, -1)
        self.assertEqual(error.execution_status, "UNKNOWN")
        self.assertEqual(len(out.encode()), 4096)
        self.assertEqual(err, "")
        self.assertEqual(shown.getvalue(), out)

    def test_partial_lines_and_split_utf8_are_collected_and_displayed(self):
        shown = io.StringIO()
        script = "import os,time; os.write(1,b'\\xe4'); time.sleep(.03); os.write(1,b'\\xb8\\xadline\\nnext')"
        with contextlib.redirect_stdout(shown):
            actual = self.run_child(script, stream=True, print_prefix="P", timeout=2)
        self.assertEqual(actual, (0, "中line\nnext", "", None))
        self.assertEqual(shown.getvalue(), "中line\nP next")

    def test_blocked_stdin_and_partial_line_do_not_block_timeout(self):
        for stream in (False, True):
            with self.subTest(stream=stream), contextlib.redirect_stdout(io.StringIO()):
                started = time.monotonic()
                code, out, err, error = self.run_child(
                    "import os,time; os.write(1,b'partial'); time.sleep(10)",
                    timeout=.2, input_text="x" * (4 * 1024 * 1024), stream=stream)
                self.assertEqual(code, -1)
                self.assertEqual(error.execution_status, "TIMEOUT")
                self.assertEqual(out, "partial")
                self.assertEqual(err, "")
                self.assertLess(time.monotonic() - started, 2)

    def test_large_stdin_roundtrip_has_no_blocking_writer_thread(self):
        payload = "中" * 20000
        actual = self.run_child("import sys; data=sys.stdin.buffer.read(); print(len(data))",
                                input_text=payload, timeout=2)
        self.assertEqual(actual, (0, f"{len(payload.encode())}\n", "", None))

    def test_nonfinite_or_nonpositive_deadline_is_rejected_before_spawn(self):
        for timeout in (float("nan"), float("inf"), 0, -1):
            with self.subTest(timeout=timeout), patch.object(base.subprocess, "Popen") as spawn:
                code, out, err, error = self.run_child("raise SystemExit(0)", timeout=timeout)
            self.assertEqual((code, out, err, error.execution_status), (-1, "", "", "FAILED"))
            spawn.assert_not_called()

    def test_unsupported_python_display_returns_typed_partial_output(self):
        class BrokenDisplay(io.StringIO):
            def write(self, text):
                raise BrokenPipeError("closed display")

        with contextlib.redirect_stdout(BrokenDisplay()):
            code, out, err, error = self.run_child(
                "import os,time; os.write(1,b'partial'); time.sleep(10)", stream=True, timeout=2)
        self.assertEqual((code, out, err), (-1, "partial", ""))
        self.assertEqual(error.execution_status, "UNKNOWN")

    def test_fd_stream_display_preserves_output_and_reaps_writer(self):
        read_fd, write_fd = os.pipe()
        launched = []
        real_popen = base.subprocess.Popen

        def remember_process(*args, **options):
            process = real_popen(*args, **options)
            launched.append(process)
            return process

        try:
            with os.fdopen(write_fd, "w", encoding="utf-8") as target:
                with contextlib.redirect_stdout(target), patch.object(base.subprocess, "Popen", side_effect=remember_process):
                    actual = self.run_child("print('中line\\nnext')", stream=True, print_prefix="P", timeout=2)
                self.assertTrue(os.get_blocking(target.fileno()))
            shown = os.read(read_fd, 1000).decode()
        finally:
            os.close(read_fd)
        self.assertEqual(actual, (0, "中line\nnext\n", "", None))
        self.assertEqual(shown, "中line\nP next\nP ")
        self.assertEqual(len(launched), 2)
        self.assertTrue(all(process.poll() is not None for process in launched))

    def test_running_provider_timeout_remains_timeout_with_fd_display(self):
        with tempfile.TemporaryFile(mode="w+", encoding="utf-8") as target, contextlib.redirect_stdout(target):
            code, out, err, error = self.run_child(
                "import os,time; os.write(1,b'partial'); time.sleep(10)", stream=True, timeout=.3)
        self.assertEqual((code, out, err, error.execution_status), (-1, "partial", "", "TIMEOUT"))

    def test_blocked_fd_display_deadline_returns_unknown_without_process_leaks(self):
        self.check_blocked_display("import os; os.write(1,b'blocked')", queue_limit=None)

    def test_blocked_fd_display_queue_is_bounded_and_cancels_provider(self):
        self.check_blocked_display("import os,time; os.write(1,b'a'*20000); time.sleep(10)", queue_limit=1024)

    def check_blocked_display(self, script, *, queue_limit):
        read_fd, write_fd = os.pipe()
        os.set_blocking(write_fd, False)
        try:
            while True:
                os.write(write_fd, b"full" * 1024)
        except BlockingIOError:
            pass
        os.set_blocking(write_fd, True)
        launched = []
        real_popen = base.subprocess.Popen

        def remember_process(*args, **options):
            process = real_popen(*args, **options)
            launched.append(process)
            return process

        try:
            with os.fdopen(write_fd, "w", encoding="utf-8") as target, contextlib.ExitStack() as stack:
                stack.enter_context(contextlib.redirect_stdout(target))
                stack.enter_context(patch.object(base.subprocess, "Popen", side_effect=remember_process))
                if queue_limit is not None:
                    stack.enter_context(patch.object(base, "MAX_STREAM_QUEUE_BYTES", queue_limit))
                started = time.monotonic()
                code, out, err, error = self.run_child(script, stream=True, timeout=.3)
                elapsed = time.monotonic() - started
                self.assertTrue(os.get_blocking(target.fileno()))
                terminated_before_test_cleanup = all(process.poll() is not None for process in launched)
        finally:
            os.close(read_fd)
            for process in launched:
                if process.poll() is None:
                    base.kill_process_tree(process)
        self.assertEqual((code, err, error.execution_status), (-1, "", "UNKNOWN"))
        self.assertTrue(out)
        self.assertIn("Stream display", error)
        self.assertLess(elapsed, 2)
        self.assertEqual(len(launched), 2)
        self.assertTrue(terminated_before_test_cleanup)

    @unittest.skipUnless(sys.platform.startswith("linux"), "orphan termination observation uses Linux /proc")
    def test_normal_parent_exit_cleans_child_with_closed_output_pipes(self):
        child = "import signal,time; signal.signal(signal.SIGTERM,signal.SIG_IGN); time.sleep(10)"
        script = ("import subprocess,sys; p=subprocess.Popen([sys.executable,'-I','-c'," + repr(child) + "],"
                  "stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL); print(p.pid,flush=True)")
        code, out, err, error = self.run_child(script, timeout=2)
        pid = int(out.strip())
        self.addCleanup(self.cleanup_child, pid)
        self.assertEqual((code, err, error), (0, "", None))
        self.assert_not_running(pid)

    @unittest.skipUnless(sys.platform.startswith("linux"), "orphan termination observation uses Linux /proc")
    def test_normal_parent_exit_cleans_child_that_keeps_output_open(self):
        child = "import os,time;\nwhile True:\n os.write(1,b'child-output'); time.sleep(.01)"
        script = ("import subprocess,sys,time; p=subprocess.Popen([sys.executable,'-I','-c'," + repr(child) + "]);"
                  " print(p.pid,flush=True); time.sleep(.05)")
        started = time.monotonic()
        code, out, err, error = self.run_child(script, timeout=2)
        pid = int(out.splitlines()[0])
        self.addCleanup(self.cleanup_child, pid)
        self.assertEqual((code, err, error), (0, "", None))
        self.assertLess(time.monotonic() - started, 1)
        self.assert_not_running(pid)

    @unittest.skipUnless(sys.platform.startswith("linux"), "process termination observation uses Linux /proc")
    def test_timeout_terminates_sigterm_ignoring_process(self):
        code, out, err, error = self.run_child(
            "import os,signal,time; signal.signal(signal.SIGTERM,signal.SIG_IGN); print(os.getpid(),flush=True); time.sleep(10)",
            timeout=.2)
        pid = int(out.strip())
        self.addCleanup(self.cleanup_child, pid)
        self.assertEqual((code, err, error.execution_status), (-1, "", "TIMEOUT"))
        self.assert_not_running(pid)

    def test_pass_fds_still_reaches_child(self):
        read_fd, write_fd = os.pipe()
        try:
            os.write(write_fd, b"passed-fd")
            os.close(write_fd)
            write_fd = None
            actual = self.run_child(f"import os; print(os.read({read_fd},100).decode())", pass_fds=(read_fd,))
        finally:
            os.close(read_fd)
            if write_fd is not None:
                os.close(write_fd)
        self.assertEqual(actual, (0, "passed-fd\n", "", None))


if __name__ == "__main__":
    unittest.main()
