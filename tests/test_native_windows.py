"""Native Windows execution/delivery, exercised by the Windows runtime gate."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import queue
import shutil
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

from makewand import config
from makewand.native_windows import relative_parts
from makewand.providers.base import run_subprocess


@unittest.skipUnless(os.name == "nt", "requires real native Windows accounting handles")
class NativeWindowsAccountingTests(unittest.TestCase):
    """Required by the native gate, using kernel sharing and real hardlinks."""

    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="makewand-accounting-")
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name).resolve()
        self.parent = self.root / "chosen"
        self.parent.mkdir()
        self.ledger = self.parent / "calls.json"

    def reserve(self, path=None):
        from makewand import call_budget
        return call_budget.reserve("synthetic", "fast", budget_file=path or self.ledger,
                                   max_model_calls=4, deadline_monotonic=time.monotonic() + 2)

    def test_entire_parent_chain_cannot_be_renamed_during_accounting(self):
        from makewand import call_budget
        nested = self.parent / "nested"
        nested.mkdir()
        self.ledger = nested / "calls.json"
        with call_budget._ledger(self.ledger, 4) as (files, data):
            for parent in (nested, self.parent):
                with self.subTest(parent=parent.name), self.assertRaises(OSError):
                    parent.rename(self.root / (parent.name + "-moved"))
            call_budget._save(files, data)
        self.assertEqual(json.loads(self.ledger.read_bytes()), data)


    def test_shared_lock_and_atomic_publication_preserve_completion(self):
        from makewand import accounting_files, call_budget, filelock
        identifier = self.reserve()
        with accounting_files.pin(self.ledger) as files:
            fd = files.open(files.lock_name, writable=True)
            try:
                filelock.flock(fd, filelock.LOCK_EX | filelock.LOCK_NB)
                began = time.monotonic()
                with self.assertRaises(call_budget.BudgetDeadlineError):
                    call_budget.reserve("synthetic", "fast", budget_file=self.ledger,
                                        max_model_calls=4, deadline_monotonic=began + .05)
                self.assertGreaterEqual(time.monotonic() - began, .04)
                with self.assertRaises(OSError):
                    Path(str(self.ledger) + ".lock").unlink()
            finally:
                filelock.flock(fd, filelock.LOCK_UN)
                os.close(fd)
        call_budget.complete(identifier, False, 0, result_status="UNKNOWN", outcome_known=False,
                             budget_file=self.ledger, max_model_calls=4)
        entry = json.loads(self.ledger.read_bytes())["attempts"][0]
        self.assertEqual(entry["id"], identifier)
        self.assertEqual(entry["status"], "completed")
        self.assertEqual(entry["result_status"], "UNKNOWN")
        self.assertFalse(entry["outcome_known"])
        self.assertEqual(list(self.parent.glob(".call-budget-*")), [])

    def test_initial_junction_alias_and_operator_git_parent_remain_supported(self):
        from makewand import call_budget
        nested = self.parent / ".git" / "operator-accounting"
        self.ledger = nested / "calls.json"
        first = self.reserve()
        alias = self.root / "parent-alias"
        result = subprocess.run([os.environ.get("COMSPEC", "cmd.exe"), "/d", "/c", "mklink", "/J", str(alias), str(self.parent)],
                                capture_output=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr.decode(errors="replace"))
        try:
            second = self.reserve(alias / ".git" / "operator-accounting" / "calls.json")
            call_budget.complete(first, True, 0, result_status="PASSED",
                                 budget_file=self.ledger, max_model_calls=4)
            self.assertEqual([entry["id"] for entry in json.loads(self.ledger.read_bytes())["attempts"]],
                             [first, second])
            self.assertFalse((alias / ".git" / "operator-accounting" / "calls.json").is_symlink())
        finally:
            # Remove only the junction, leaving its real target for TempDir.
            os.rmdir(alias)

    def test_replaced_temporary_cleanup_deletes_only_its_original_handle(self):
        from makewand import accounting_files, call_budget
        original_replace = accounting_files.AccountingFiles.replace
        moved = self.root / "original-created-temporary"
        replacements = []

        def substitute(files, name, fd):
            source = files.path.parent / name
            source.rename(moved)
            source.write_bytes(b"replacement-sentinel\n")
            replacements.append(source)
            return original_replace(files, name, fd)

        with mock.patch.object(accounting_files.AccountingFiles, "replace", substitute):
            with self.assertRaises(call_budget.BudgetError):
                self.reserve()
        self.assertEqual(len(replacements), 1)
        self.assertEqual(replacements[0].read_bytes(), b"replacement-sentinel\n")
        self.assertFalse(self.ledger.exists())
        self.assertFalse(moved.exists())


class WindowsPathTests(unittest.TestCase):
    def test_original_components_are_checked_before_normalization(self):
        from makewand.windows_paths import extended_local_path, windows_git_directory
        from makewand.native_windows import _absolute_components
        for path in (r"C:\workspace\trailing.", "C:\\workspace\\trailing ",
                     r"C:\bad.\..\safe", r"relative.\safe", "relative \\safe",
                     r"C:\workspace\file:stream", r"C:\workspace\NUL.txt"):
            with self.subTest(path=path), mock.patch("makewand.windows_paths.ntpath.abspath") as normalize:
                with self.assertRaises(ValueError):
                    extended_local_path(path)
                with self.assertRaises(ValueError):
                    _absolute_components(path)
                normalize.assert_not_called()
        # Legal relative navigation keeps its meaning; it is not trimmed or
        # reinterpreted as a differently named component to pass validation.
        with mock.patch("makewand.windows_paths.ntpath.abspath", return_value=r"C:\workspace\safe") as normalize:
            self.assertEqual(extended_local_path(r".\child\..\safe"), r"\\?\C:\workspace\safe")
            normalize.assert_called_once_with(r".\child\..\safe")
        with mock.patch("makewand.windows_paths.ntpath.abspath", return_value=r"C:\workspace\bad."):
            with self.assertRaises(ValueError):
                extended_local_path(r"C:\workspace\safe")
        with self.assertRaises(ValueError):
            _absolute_components(r"C:\workspace\.git\objects")
        long_root = "C:\\workspace\\" + "long-segment\\" * 30
        with self.assertRaisesRegex(OSError, "canonical workspace root"):
            windows_git_directory(long_root)
        with mock.patch("makewand.windows_paths.ntpath.realpath", return_value=long_root):
            with self.assertRaisesRegex(OSError, "canonical workspace root"):
                windows_git_directory(r"C:\SHORT~1")

    def test_extended_call_paths_preserve_names_and_reject_device_namespaces(self):
        from makewand.windows_paths import extended_local_path
        logical = "C:\\workspace\\" + ("deep-name\\" * 35) + "中文 file.txt"
        self.assertGreater(len(logical), 300)
        self.assertEqual(extended_local_path(logical), "\\\\?\\" + logical)
        self.assertEqual(extended_local_path("C:/workspace/.git/objects"),
                         "\\\\?\\C:\\workspace\\.git\\objects")
        for path in ("\\\\server\\share\\file", "\\\\?\\C:\\workspace\\file",
                     "\\\\.\\C:\\workspace\\file", "C:\\workspace\\NUL.txt",
                     "C:\\workspace\\trailing.", "C:\\workspace\\trailing ",
                     "C:\\workspace\\file:stream", "C:\\workspace\\a\0b"):
            with self.subTest(path=path), self.assertRaises(ValueError):
                extended_local_path(path)

    def test_private_state_policy_does_not_duplicate_system_permissions(self):
        from makewand.native_windows import _private_state_sddl
        user = "S-1-5-21-1-2-3-1001"
        for directory in (False, True):
            for sid, expected_grants in (("S-1-5-18", 1), (user, 2)):
                with self.subTest(directory=directory, system=sid == "S-1-5-18"):
                    policy = _private_state_sddl(sid, directory=directory)
                    self.assertTrue(policy.startswith("D:P"))
                    self.assertEqual(policy.count("(A;"), expected_grants)
                    self.assertEqual(policy.count(";;;SY)"), 1)
                    self.assertEqual(policy.count(";;;" + user + ")"), int(sid == user))
                    self.assertEqual("OICI" in policy, directory)

    def test_npm_shim_launches_node_with_literal_prompt_arguments(self):
        from makewand.windows_process import resolve_windows_command
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            node = root / "node.exe"
            node.write_bytes(b"fixture")
            entry = root / "node_modules" / "provider" / "cli.js"
            entry.parent.mkdir(parents=True)
            entry.write_text("fixture", encoding="utf-8")
            shim = root / "provider.cmd"
            shim.write_text('SET "_prog=%dp0%\\node.exe"\n"%_prog%" "%dp0%\\node_modules\\provider\\cli.js" %*\n', encoding="utf-8")
            prompt = 'a & del sentinel | echo %SECRET% > file'
            with mock.patch("makewand.windows_process.shutil.which", return_value=str(shim)):
                resolved = resolve_windows_command(["provider", prompt])
            self.assertEqual(resolved, [str(node), str(entry), prompt])

    def test_unknown_batch_launcher_is_not_sent_to_a_shell(self):
        from makewand.windows_process import resolve_windows_command
        with tempfile.TemporaryDirectory() as temporary:
            shim = Path(temporary) / "unknown.cmd"
            shim.write_text("@echo off\n%*", encoding="utf-8")
            with mock.patch("makewand.windows_process.shutil.which", return_value=str(shim)), self.assertRaises(ValueError):
                resolve_windows_command(["unknown", "a & command"])

    def test_ambiguous_and_alternate_stream_names_are_rejected(self):
        for value in ("C:/outside", "../outside", "a\\outside", "a:file", "NUL", "COM1.txt", "LPT¹",
                      "a/../b", "a//b", "a/./b", "trailing.", "trailing ", ".git/config", "a\0b"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                relative_parts(value)

    def test_regular_relative_names_are_supported(self):
        self.assertEqual(relative_parts("src/中文 file.py"), ["src", "中文 file.py"])

    def test_windows_backends_are_not_silently_emulated_on_other_platforms(self):
        if os.name == "nt":
            self.skipTest("The native backend exists on this platform")
        from makewand.native_windows import pinned_directory
        with self.assertRaises(OSError), pinned_directory("C:\\workspace"):
            self.fail("A Windows backend cannot use POSIX path behavior")


@unittest.skipUnless(os.name == "nt", "requires real native Windows handles and Jobs")
class NativeWindowsFileTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="makewand-windows-")
        self.root = Path(self.directory.name)
        self.workspace = self.root / "workspace"
        self.workspace.mkdir()
        self.source = self.root / "source.txt"
        self.source.write_bytes(b"candidate\r\n")

    def tearDown(self):
        for path in self.root.rglob("*"):
            if path.is_file():
                try:
                    path.chmod(stat.S_IWRITE)
                except OSError:
                    pass
        self.directory.cleanup()

    def junction(self, path, target):
        result = subprocess.run([os.environ.get("COMSPEC", "cmd.exe"), "/d", "/c", "mklink", "/J", str(path), str(target)],
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


    def test_pinned_parent_cannot_be_renamed_during_copy(self):
        from makewand.native_windows import pinned_directory
        parent = self.workspace / "nested"
        parent.mkdir()
        with pinned_directory(parent):
            with self.assertRaises(OSError):
                parent.rename(self.workspace / "moved")
        parent.rename(self.workspace / "moved")


    def test_private_directory_uses_protected_windows_acl(self):
        from makewand.native_windows import _security_descriptor
        import ctypes
        from ctypes import wintypes
        state = config.ensure_private_dir(self.root / "private")
        descriptor = _security_descriptor(state, 4)
        control, revision = wintypes.WORD(), wintypes.DWORD()
        advapi = ctypes.WinDLL("advapi32", use_last_error=True)
        getter = advapi.GetSecurityDescriptorControl
        getter.argtypes = [ctypes.c_void_p, ctypes.POINTER(wintypes.WORD), ctypes.POINTER(wintypes.DWORD)]
        self.assertTrue(getter(descriptor, ctypes.byref(control), ctypes.byref(revision)))
        self.assertTrue(control.value & 0x1000, "DACL must be protected against inherited broad access")


@unittest.skipUnless(os.name == "nt", "requires real Windows Job Objects")
class NativeWindowsProcessTests(unittest.TestCase):
    def test_active_process_limit_is_enforced_by_the_job(self):
        from makewand.windows_process import WindowsJob
        job = WindowsJob(active_process_limit=1)
        try:
            with tempfile.TemporaryDirectory() as temporary:
                marker = Path(temporary) / "child.txt"
                child = "from pathlib import Path; Path(" + repr(str(marker)) + ").write_text('ran')"
                code = "import subprocess,sys\ntry:\n subprocess.run([sys.executable,'-I','-c'," + repr(child) + "])\nexcept OSError:\n pass\nprint('done')"
                proc = job.start([sys.executable, "-I", "-c", code], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                output, error = proc.communicate(timeout=5)
                self.assertEqual(proc.returncode, 0, error)
                self.assertEqual(output.strip(), b"done")
                self.assertFalse(marker.exists(), "The job's child limit must prevent descendant execution")
        finally:
            job.close()

    def test_split_output_and_stdin(self):
        code, out, err, error = run_subprocess([sys.executable, "-I", "-c",
                                              "import sys; print(sys.stdin.read()); print('stderr',file=sys.stderr)"],
                                             input_text="input", timeout=5)
        self.assertEqual((code, out, err, error), (0, "input\n", "stderr\n", None))

    def test_deadline_before_child_readiness(self):
        with tempfile.TemporaryDirectory() as temporary:
            marker = Path(temporary) / "escaped.txt"
            ready = Path(temporary) / "ready.txt"
            parent = ("import pathlib,time; time.sleep(10); pathlib.Path(" + repr(str(ready))
                      + ").write_text('ready'); pathlib.Path(" + repr(str(marker)) + ").write_text('escaped')")
            started = time.monotonic()
            code, out, _, error = run_subprocess([sys.executable, "-I", "-c", parent], timeout=.3)
            self.assertEqual(code, -1)
            self.assertEqual(error.execution_status, "TIMEOUT")
            self.assertLess(time.monotonic() - started, 2)
            self.assertEqual(out, "")
            self.assertFalse(ready.exists(), "The real short deadline must apply during initialization")
            time.sleep(1.7)
            self.assertFalse(marker.exists(), "Job teardown must terminate descendants")

    def test_ready_descendant_termination_and_pipe_cleanup(self):
        from makewand.windows_process import WindowsJob
        with tempfile.TemporaryDirectory() as temporary:
            marker = Path(temporary) / "escaped.txt"
            ready = Path(temporary) / "ready.txt"
            armed = Path(temporary) / "armed.txt"
            child = "\n".join([
                "import pathlib,time",
                "ready=pathlib.Path(" + repr(str(ready)) + ")",
                "armed=pathlib.Path(" + repr(str(armed)) + ")",
                "ready.write_text('ready')",
                "deadline=time.monotonic()+30",
                "while not armed.exists():",
                " if time.monotonic()>=deadline: raise SystemExit(7)",
                " time.sleep(.005)",
                "time.sleep(1.5)",
                "pathlib.Path(" + repr(str(marker)) + ").write_text('escaped')",
            ])
            parent = "\n".join([
                "import pathlib,subprocess,sys,time",
                "subprocess.Popen([sys.executable,'-I','-c'," + repr(child) + "])",
                "ready=pathlib.Path(" + repr(str(ready)) + ")",
                "deadline=time.monotonic()+30",
                "while not ready.exists():",
                " if time.monotonic()>=deadline: raise SystemExit(8)",
                " time.sleep(.005)",
                "print('started',flush=True)",
                "time.sleep(10)",
            ])
            job = WindowsJob()
            proc = None
            reader = None
            timer = None
            try:
                startup_deadline = time.monotonic() + 30
                proc = job.start([sys.executable, "-I", "-c", parent], stdin=subprocess.DEVNULL,
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE, bufsize=0)
                first_line = queue.Queue(maxsize=1)
                reader = threading.Thread(target=lambda: first_line.put(proc.stdout.readline()), daemon=True)
                reader.start()
                line = first_line.get(timeout=max(.001, startup_deadline - time.monotonic()))
                reader.join(timeout=2)
                self.assertFalse(reader.is_alive(), "Readiness pipe reader must finish before collection")
                self.assertEqual(line, b"started\r\n")
                self.assertTrue(ready.exists(), "A real descendant must be ready before termination is timed")
                started = time.monotonic()
                timer = threading.Timer(.3, job.terminate)
                timer.start()
                armed.write_text("armed")
                output, error = proc.communicate(timeout=2)
                self.assertLess(time.monotonic() - started, 2)
                self.assertNotEqual(proc.returncode, 0, error)
                self.assertEqual(line + output, b"started\r\n")
                time.sleep(1.7)
                self.assertFalse(marker.exists(), "Ready descendants must not escape the terminated Job")
            finally:
                if timer is not None:
                    timer.cancel()
                    timer.join(timeout=2)
                job.close()
                if proc is not None:
                    proc.wait(timeout=2)
                    if reader is not None:
                        reader.join(timeout=2)
                    for pipe in (proc.stdout, proc.stderr):
                        if pipe is not None:
                            pipe.close()

    def test_successful_parent_exit_also_cleans_descendants(self):
        with tempfile.TemporaryDirectory() as temporary:
            marker = Path(temporary) / "escaped.txt"
            child = "import pathlib,time; time.sleep(1); pathlib.Path(" + repr(str(marker)) + ").write_text('escaped')"
            parent = "import subprocess,sys; subprocess.Popen([sys.executable,'-I','-c'," + repr(child) + "]); print('done',flush=True)"
            code, out, _, error = run_subprocess([sys.executable, "-I", "-c", parent], timeout=5)
            self.assertEqual((code, out, error), (0, "done\n", None))
            time.sleep(1.2)
            self.assertFalse(marker.exists())

    def test_shared_output_limit_does_not_silently_truncate_success(self):
        with mock.patch("makewand.providers.base.MAX_OUTPUT_BYTES", 4096):
            code, out, err, error = run_subprocess([sys.executable, "-I", "-c",
                                                  "import os; os.write(1,b'a'*3000); os.write(2,b'b'*3000)"], timeout=5)
        self.assertEqual(code, -1)
        self.assertEqual(error.execution_status, "UNKNOWN")
        self.assertEqual(len(out.encode()) + len(err.encode()), 4096)

    def test_stream_uses_native_job_runner(self):
        display = io.StringIO()
        with contextlib.redirect_stdout(display):
            code, out, _, error = run_subprocess([sys.executable, "-I", "-c", "print('hello')"], stream=True, timeout=5)
        self.assertEqual((code, out, error), (0, "hello\r\n", None))
        self.assertEqual(display.getvalue(), "hello\r\n")

    def test_fd_stream_writer_finishes_before_deadline(self):
        with tempfile.TemporaryFile(mode="w+b") as display, mock.patch("sys.stdout", display):
            code, out, _, error = run_subprocess([sys.executable, "-I", "-c", "print('hello')"], stream=True, timeout=5)
            display.seek(0)
            shown = display.read()
        self.assertEqual((code, out, error), (0, "hello\r\n", None))
        self.assertEqual(shown, b"hello\r\n")


if __name__ == "__main__":
    unittest.main()
