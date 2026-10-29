"""
windows-merged, round 4: native Windows keeps a CLI's stderr apart from its
answer in stream mode too.

run_subprocess used to call run_windows_subprocess without separate_stderr, and
the Windows runner merged stderr into stdout in stream mode, so on Windows agy's
generic warnings ("Warning: failed to record sidecar event: context canceled",
"Warning: <dir> is not present in your active PATH.") became the answer of an
exit-0 run, a warning led a real answer, a glog header ending in ':' right
before the answer dropped its first line, and codex/grok/agy answer lines were
classified after the CLI's stderr text. Now both pipes are read concurrently,
stdout is displayed as it arrives and stderr whole lines at a time, and the
result says the streams were captured apart (CapturedStderr). Where a runner
still merges the streams (the historical contract of test doubles), an output
of agy's own notices only is never an answer.

The Windows runner is exercised on any platform through its own abstractions:
WindowsJob is replaced by a job that starts the process with plain Popen, and
run_subprocess dispatches to it when os.name says "nt".
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import contextlib
import io
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.config as config
import makewand.health as health
from makewand import windows_process
from makewand.providers import agy, base, codex, grok

KEYRING = "I1012 08:11:57.965555       1 auth.go:157] ChainedAuth: authenticated via keyring (effective: keyring)\n"
CODEX_HDR = ("OpenAI Codex v0.162.1 (research preview)\n--------\nworkdir: /repo\n--------\nuser\nreview this\n\n")
LIMITS = 10 * 1024 * 1024, 256 * 1024


class FakeJob:
    """WindowsJob without Job Objects: plain Popen, terminate() kills what is still running."""

    def __init__(self, **_limits):
        self.procs = []

    def start(self, cmd, **kwargs):
        kwargs.pop("creationflags", None)
        proc = subprocess.Popen(cmd, **kwargs)
        proc._makewand_job = self
        self.procs.append(proc)
        return proc

    def terminate(self):
        for proc in self.procs:
            if proc.poll() is None:
                proc.kill()

    def close(self):
        pass


class _WindowsOs:
    """The os module as run_subprocess sees it on native Windows."""
    name = "nt"

    def __getattr__(self, attribute):
        return getattr(os, attribute)


def cli_script(out, err, code=0, *, stderr_first=False):
    """A fake CLI writing `out` to stdout and `err` to stderr (as bytes) and exiting with `code`."""
    writes = [("stdout", out), ("stderr", err)]
    if stderr_first:
        writes.reverse()
    body = "".join("sys.%s.buffer.write(%r)\nsys.%s.flush()\n" % (name, text.encode(), name) for name, text in writes)
    return [sys.executable, "-I", "-c", "import sys\n" + body + "sys.exit(%d)\n" % code]


def run_windows(cmd, **kwargs):
    shown = io.StringIO()
    with patch.object(windows_process, "WindowsJob", FakeJob), contextlib.redirect_stdout(shown):
        result = windows_process.run_windows_subprocess(cmd, timeout=60, output_limit=LIMITS[0],
                                                        stream_queue_limit=LIMITS[1], **kwargs)
    return result, shown.getvalue()


class TestWindowsRunnerCapturesStderrApart(unittest.TestCase):
    def test_stopped_lease_failure_still_closes_child_pipes(self):
        from makewand.call_budget import BudgetConfigError
        from makewand import execution_runtime
        processes = []
        original = subprocess.Popen

        def start(*args, **kwargs):
            proc = original(*args, **kwargs)
            processes.append(proc)
            return proc

        def lease(action, pid=None):
            if action == "stopped":
                raise BudgetConfigError("capacity state lost")

        with patch.object(subprocess, "Popen", side_effect=start), \
                patch.object(execution_runtime, "provider_process_lease", side_effect=lease):
            with self.assertRaises(BudgetConfigError):
                run_windows(cli_script("done", ""))
        self.assertTrue(processes)
        for proc in processes:
            self.assertIsNotNone(proc.poll())
            self.assertTrue(all(pipe is None or pipe.closed for pipe in (proc.stdin, proc.stdout, proc.stderr)))

    def test_windows_branch_preserves_worker_and_proxy_environment(self):
        import json
        extra = {"MAKEWAND_PROVIDER_WORKER": "1", "HTTP_PROXY": "http://fixture:8181",
                 "HTTPS_PROXY": "http://fixture:8181", "ALL_PROXY": "http://fixture:8181"}
        command = [sys.executable, "-I", "-c",
                   "import os,json; print(json.dumps({k:os.getenv(k) for k in " + repr(list(extra)) + "}))"]
        original = dict(os.environ)
        with patch.object(base, "os", _WindowsOs()), patch.object(windows_process, "WindowsJob", FakeJob):
            code, out, err, error = base.run_subprocess(command, timeout=30, extra_env=extra)
        self.assertEqual((code, error), (0, None), err)
        self.assertEqual(json.loads(out), extra)
        self.assertEqual(dict(os.environ), original)

    def test_stream_mode_keeps_stderr_apart_when_asked(self):
        (code, out, err, error), shown = run_windows(cli_script("The fix is in bank.py.\n", "Warning: x\n"),
                                                     stream=True, separate_stderr=True, print_prefix="[AGY Live]")
        self.assertEqual((code, out, str(err), error), (0, "The fix is in bank.py.\n", "Warning: x\n", None))
        self.assertTrue(base.stderr_captured_apart(err, True))
        # Both streams are still shown live.
        self.assertIn("The fix is in bank.py.", shown)
        self.assertIn("Warning: x", shown)

    def test_stream_mode_without_separate_stderr_keeps_the_merged_contract(self):
        (code, out, err, _error), _shown = run_windows(cli_script("answer\n", "Warning: x\n"), stream=True)
        self.assertEqual(code, 0)
        self.assertIn("answer", out)
        self.assertIn("Warning: x", out)
        self.assertEqual(str(err), "")
        self.assertFalse(base.stderr_captured_apart(err, True))

    def test_non_stream_mode_is_apart(self):
        (code, out, err, _error), shown = run_windows(cli_script("answer\r\n", "Error: x\r\n", 3))
        self.assertEqual((code, out, str(err)), (3, "answer\n", "Error: x\n"))
        self.assertTrue(base.stderr_captured_apart(err, False))
        self.assertEqual(shown, "")

    def test_both_pipes_are_drained_concurrently(self):
        # A child filling stderr before writing stdout deadlocks a reader of only one pipe.
        script = ("import sys\nsys.stderr.buffer.write(b'e' * 400000 + b'\\n'); sys.stderr.flush()\n"
                  "sys.stdout.buffer.write(b'o' * 400000 + b'\\n'); sys.stdout.flush()\n")
        (code, out, err, error), _shown = run_windows([sys.executable, "-I", "-c", script], stream=True,
                                                      separate_stderr=True)
        self.assertEqual((code, error), (0, None))
        self.assertEqual((out, str(err)), ("o" * 400000 + "\n", "e" * 400000 + "\n"))

    def test_stderr_is_shown_whole_lines_at_a_time(self):
        script = ("import sys,time\nsys.stderr.write('Warn'); sys.stderr.flush(); time.sleep(.2)\n"
                  "sys.stdout.write('answer\\n'); sys.stdout.flush(); time.sleep(.2)\n"
                  "sys.stderr.write('ing: x\\n'); sys.stderr.flush()\n")
        (_code, out, err, _error), shown = run_windows([sys.executable, "-I", "-c", script], stream=True,
                                                       separate_stderr=True)
        self.assertEqual((out, str(err)), ("answer\n", "Warning: x\n"))
        # The unfinished "Warn" is held back until its line is complete, never cut into the answer.
        self.assertIn("Warning: x", shown)
        self.assertNotIn("Warnanswer", shown)

    def test_run_subprocess_on_windows_passes_separate_stderr(self):
        with patch.object(base, "os", _WindowsOs()), patch.object(windows_process, "WindowsJob", FakeJob), \
             contextlib.redirect_stdout(io.StringIO()):
            code, out, err, _error = base.run_subprocess(cli_script("answer\n", "Warning: x\n"), timeout=60,
                                                         stream=True, separate_stderr=True)
            self.assertEqual((code, out, str(err)), (0, "answer\n", "Warning: x\n"))
            self.assertTrue(base.stderr_captured_apart(err, True))
            _code, merged, err, _error = base.run_subprocess(cli_script("answer\n", "Warning: x\n"), timeout=60,
                                                             stream=True)
            self.assertIn("Warning: x", merged)
            self.assertFalse(base.stderr_captured_apart(err, True))


class DispatchCase(unittest.TestCase):
    MODULES = {"agy": agy, "codex": codex, "grok": grok}

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="mw-fix-win-")
        root = Path(self._tmp.name)
        for p in (patch.object(config, "CONFIG_DIR", root / "cfg"),
                  patch.object(health, "STATUS_CACHE_FILE", root / "cfg" / "status.json"),
                  patch.object(health, "LEGACY_TRIO_CACHE", root / "nolegacy" / "trio.json")):
            p.start()
            self.addCleanup(p.stop)
        self.addCleanup(self._tmp.cleanup)
        (root / "cfg").mkdir(parents=True)

    def dispatch(self, engine, runner, *, stream=True):
        module = self.MODULES[engine]
        with patch("makewand.config.has_subscription_configured", return_value=True), \
             patch("makewand.config.has_api_configured", return_value=False), \
             patch("makewand.sandbox.is_bwrap_available", return_value=False), \
             patch("makewand.sandbox._agy_host_credits_disabled", return_value=True), \
             patch("makewand.sandbox.resolve_unsafe_host_exec", return_value=(True, "config-ack")), \
             patch("makewand.sandbox.audit_unsafe_host_exec"), \
             patch.object(module, "run_subprocess", side_effect=runner), \
             contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            result = getattr(module, f"execute_{engine}_task")("review this", cwd=self._tmp.name, readonly=True,
                                                               stream=stream)
        status = health.load_status_cache().get(engine, {}).get("status")
        if health.STATUS_CACHE_FILE.exists():
            health.STATUS_CACHE_FILE.unlink()
        return result, status


def windows_cli(out, err, code=0, calls=None, *, stderr_first=False):
    """run_subprocess on native Windows (mocked platform) running a fake CLI."""
    def run(cmd, **kwargs):
        if calls is not None:
            calls.append(kwargs)
        kwargs.pop("pass_fds", None)
        kwargs.pop("extra_env", None)
        with patch.object(base, "os", _WindowsOs()), patch.object(windows_process, "WindowsJob", FakeJob):
            return base.run_subprocess(cli_script(out, err, code, stderr_first=stderr_first), **kwargs)
    return run


class TestWindowsStreamDispatch(DispatchCase):
    def test_agy_notices_are_never_the_answer(self):
        for name, err in (("generic warning", KEYRING + "Warning: failed to record sidecar event: context canceled\n"),
                          ("PATH warning", "Warning: /home/alice/.local/bin is not present in your active PATH.\n"),
                          ("status line", "terminating 1 background task(s) on exit\n")):
            with self.subTest(case=name):
                calls = []
                result, _status = self.dispatch("agy", windows_cli("", err, 0, calls))
                self.assertTrue(calls[0]["stream"])
                self.assertTrue(calls[0]["separate_stderr"])
                self.assertFalse(result[0])

    def test_agy_answer_is_stdout_as_printed(self):
        cases = {
            "warning before the answer": ("The fix is in bank.py.\n",
                                          "Warning: failed to record sidecar event: context canceled\n",
                                          "The fix is in bank.py."),
            "glog header ending in ':' before the answer": (
                "The fix is in bank.py.\nSecond line.\n",
                "W1012 08:12:30.100000     188 retry.go:88] attempt 1 failed, retrying:\n",
                "The fix is in bank.py.\nSecond line."),
            "the model's own warning prose": ("Warning: the old API is deprecated.\nUse v2.\n", "",
                                              "Warning: the old API is deprecated.\nUse v2."),
        }
        for name, (out, err, answer) in cases.items():
            for stderr_first in (True, False):  # agy logs before, or after, the answer
                with self.subTest(case=name, stderr_first=stderr_first):
                    result, _status = self.dispatch("agy", windows_cli(out, err, 0, stderr_first=stderr_first))
                    self.assertTrue(result[0])
                    self.assertEqual(result[1], answer)

    def test_answer_lines_are_not_classified_as_cli_text(self):
        answer = ("I made the change.\nYou have exhausted your quota on this model.\nUsage limit reached.\n"
                  "Error: You've hit your usage limit. Try again at 8:00 PM.\nDone.\n")
        generic = {"agy": KEYRING + "Error: UNAVAILABLE (code 503): The service is currently unavailable.\n",
                   "grok": "Error: stream closed before the turn finished\n",
                   "codex": CODEX_HDR + "codex\nok\nERROR: stream disconnected before completion: error sending request\n"}
        own = {"agy": KEYRING + "Error: RESOURCE_EXHAUSTED (code 429): You have exhausted your quota on this model.\n",
               "grok": "Error: You’ve reached your free Grok Build usage limit for now.\n",
               "codex": CODEX_HDR + "codex\nok\nERROR: You’ve hit your usage limit. Try again at 8:00 PM.\n"}
        for engine in ("agy", "grok", "codex"):
            with self.subTest(engine=engine):
                result, status = self.dispatch(engine, windows_cli(answer, generic[engine], 1))
                self.assertFalse(result[0])
                self.assertEqual(status, "unknown")
                _result, status = self.dispatch(engine, windows_cli(answer, own[engine], 1))
                self.assertEqual(status, "limited")


class TestMergedFallback(DispatchCase):
    """A runner that still merges the streams (plain strings in stream mode, the historical contract)."""

    def test_notice_only_output_is_never_an_answer(self):
        for err in (KEYRING + "Warning: failed to record sidecar event: context canceled\n",
                    "Warning: /home/alice/.local/bin is not present in your active PATH.\n",
                    "warning: run ended with no output and no recorded error\n"):
            with self.subTest(err=err[-50:]):
                result, _status = self.dispatch("agy", lambda *args, **kwargs: (0, err, "", None))
                self.assertFalse(result[0])

    def test_an_answer_with_warnings_stays_whole(self):
        merged = "Warning: the old API is deprecated.\nUse v2.\n"
        result, _status = self.dispatch("agy", lambda *args, **kwargs: (0, merged, "", None))
        self.assertTrue(result[0])
        self.assertEqual(result[1], "Warning: the old API is deprecated.\nUse v2.")


if __name__ == "__main__":
    unittest.main()
