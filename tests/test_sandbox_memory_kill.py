"""A sandbox that reaches its memory cap: only the process that used it up dies, and a failure says why.

2026-11-28 21:29: a reviewer's experiment inside the read-only sandbox used up the scope's
8 GB. The kernel killed it, and with the default OOMPolicy (stop) the service manager then
stopped the whole scope: the reviewer died after 17 minutes, makewand saw exit -15 and reported
UNKNOWN with one agy warning line as the reason. The scope now takes OOMPolicy=continue where
systemd accepts it, has a name, and a failed run looks up that name in the user journal.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import re
import subprocess
import sys
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from makewand import sandbox
from makewand.providers import base

KERNEL = "fe6faa94e7774663a0da52717891d8ef"
OOMD = "d989611b15e44c9dbf31e3c81256e4ed"
OTHER = "39f53479d3a045ac8e11786248231fbf"  # "Started ..."


def binaries(*names):
    return lambda name: f"/usr/bin/{name}" if name in names else None


class ScopeOptionsTests(unittest.TestCase):
    def setUp(self):
        saved = dict(sandbox._limits_probe)
        self.addCleanup(lambda: (sandbox._limits_probe.clear(), sandbox._limits_probe.update(saved)))
        sandbox._limits_probe.update(systemd=None, oom_policy=None)
        for name in (sandbox.SANDBOX_MEMORY_MAX_ENV, sandbox.SANDBOX_TASKS_MAX_ENV):
            os.environ.pop(name, None)

    def probe(self, accepts_oom_policy, works=True):
        calls = []

        def run(args, **_kwargs):
            calls.append(args)
            ok = works and (accepts_oom_policy or "OOMPolicy=continue" not in args)
            return SimpleNamespace(returncode=0 if ok else 1)

        with patch("subprocess.run", side_effect=run):
            usable = sandbox._systemd_scope_usable("/usr/bin/systemd-run")
        return usable, calls

    def prefix(self):
        with patch("makewand.sandbox._trusted_system_binary", side_effect=binaries("systemd-run")):
            return sandbox.sandbox_resource_limit_prefix()

    def test_a_scope_that_takes_the_policy_gets_it_and_a_fresh_name(self):
        usable, calls = self.probe(accepts_oom_policy=True)
        self.assertTrue(usable)
        self.assertEqual(1, len(calls))
        first, second = self.prefix(), self.prefix()
        for prefix in (first, second):
            self.assertIn("OOMPolicy=continue", prefix)
            self.assertEqual("-p", prefix[prefix.index("OOMPolicy=continue") - 1])
            units = [arg for arg in prefix if arg.startswith("--unit=")]
            self.assertEqual(1, len(units))
            self.assertRegex(units[0], r"^--unit=makewand-sandbox-[0-9a-f]{16}$")
            for limit in ("MemoryMax=8G", "MemorySwapMax=0", "TasksMax=512", "--collect"):
                self.assertIn(limit, prefix)
            self.assertEqual("--", prefix[-1])
        self.assertNotEqual([a for a in first if a.startswith("--unit=")], [a for a in second if a.startswith("--unit=")])

    def test_an_older_systemd_keeps_the_limits_without_the_policy(self):
        usable, calls = self.probe(accepts_oom_policy=False)
        self.assertTrue(usable)
        self.assertEqual(2, len(calls))
        prefix = self.prefix()
        self.assertNotIn("OOMPolicy=continue", prefix)
        self.assertIn("MemoryMax=8G", prefix)
        self.assertTrue(any(arg.startswith("--unit=makewand-sandbox-") for arg in prefix))

    def test_no_usable_scope_falls_back_as_before(self):
        usable, calls = self.probe(accepts_oom_policy=True, works=False)
        self.assertFalse(usable)
        self.assertEqual(2, len(calls))
        with patch("makewand.sandbox._trusted_system_binary", side_effect=binaries("systemd-run", "prlimit")):
            prefix = sandbox.sandbox_resource_limit_prefix()
        self.assertEqual("/usr/bin/prlimit", prefix[0])


class JournalLookupTests(unittest.TestCase):
    UNIT = "makewand-sandbox-0123456789abcdef"
    CMD = ["/usr/bin/systemd-run", "--user", "--scope", "--quiet", "--collect", f"--unit={UNIT}",
           "-p", "OOMPolicy=continue", "-p", "MemoryMax=8G", "-p", "MemorySwapMax=0", "--", "/usr/bin/bwrap", "agy"]

    def lookup(self, answers, cmd=None, attempts=1, journalctl=True):
        calls = []
        answers = list(answers)

        def run(args, **_kwargs):
            calls.append(args)
            ids = answers.pop(0) if answers else []
            out = "".join(json.dumps({"MESSAGE_ID": i, "__CURSOR": "c"}) + "\n" for i in ids)
            return SimpleNamespace(returncode=0, stdout=out.encode())

        names = ("systemd-run", "journalctl") if journalctl else ("systemd-run",)
        with patch("makewand.sandbox._trusted_system_binary", side_effect=binaries(*names)), \
                patch("subprocess.run", side_effect=run), patch("makewand.sandbox.time.sleep"):
            found = sandbox.sandbox_scope_memory_kill(self.CMD if cmd is None else cmd, attempts=attempts)
        return found, calls

    def test_the_kernel_record_of_the_scope_is_found(self):
        found, calls = self.lookup([[OTHER, KERNEL]])
        self.assertEqual(("kernel", "8G"), found)
        self.assertEqual(1, len(calls))
        self.assertEqual("/usr/bin/journalctl", calls[0][0])
        self.assertIn(f"--unit={self.UNIT}.scope", calls[0])
        self.assertIn("--user", calls[0])

    def test_a_systemd_oomd_record_is_told_apart(self):
        self.assertEqual(("systemd-oomd", "8G"), self.lookup([[OOMD]])[0])

    def test_no_record_is_none_and_a_late_record_is_waited_for(self):
        found, calls = self.lookup([[OTHER], [OTHER]], attempts=1)
        self.assertIsNone(found)
        self.assertEqual(1, len(calls))
        found, calls = self.lookup([[OTHER], [], [KERNEL]], attempts=3)
        self.assertEqual(("kernel", "8G"), found)
        self.assertEqual(3, len(calls))

    def test_only_a_named_makewand_scope_is_looked_up(self):
        for cmd in (["/usr/bin/bwrap", "agy"],
                    ["/usr/bin/systemd-run", "--user", "--scope", "--", "agy"],
                    ["/usr/bin/systemd-run", "--user", "--scope", "--unit=other-name", "--", "agy"],
                    ["/usr/bin/systemd-run", "--user", "--scope", "--unit=makewand-sandbox-x;rm", "--", "agy"],
                    ["/usr/bin/systemd-run", "--user", "--scope", "--", "agy", f"--unit={self.UNIT}"],
                    "systemd-run --unit=" + self.UNIT):
            found, calls = self.lookup([[KERNEL]], cmd=cmd)
            self.assertIsNone(found, cmd)
            self.assertEqual([], calls, cmd)

    def test_without_journalctl_or_with_a_broken_journal_it_is_none(self):
        self.assertEqual((None, []), self.lookup([[KERNEL]], journalctl=False))
        with patch("makewand.sandbox._trusted_system_binary", side_effect=binaries("journalctl")), \
                patch("subprocess.run", side_effect=subprocess.TimeoutExpired("journalctl", 5)):
            self.assertIsNone(sandbox.sandbox_scope_memory_kill(self.CMD))
        with patch("makewand.sandbox._trusted_system_binary", side_effect=binaries("journalctl")), \
                patch("subprocess.run", return_value=SimpleNamespace(returncode=0, stdout=b"not json\n[1]\n\"x\"\n")):
            self.assertIsNone(sandbox.sandbox_scope_memory_kill(self.CMD))


class RunnerTests(unittest.TestCase):
    """run_subprocess asks for a diagnosis on a nonzero exit, harder on a signal exit."""

    def run_shell(self, script, found):
        calls = []

        def lookup(cmd, attempts=1):
            calls.append((list(cmd), attempts))
            return found

        with patch("makewand.sandbox.sandbox_scope_memory_kill", side_effect=lookup):
            result = base.run_subprocess(["sh", "-c", script], timeout=30, separate_stderr=True)
        return result, calls

    def test_a_killed_process_in_a_scope_with_a_record_says_so(self):
        (code, _out, _err, error), calls = self.run_shell("kill -KILL $$", ("kernel", "8G"))
        self.assertEqual(-9, code)
        self.assertEqual(1, len(calls))
        self.assertEqual((["-c", "kill -KILL $$"], 3), (calls[0][0][1:], calls[0][1]))  # the command as launched
        self.assertEqual("UNKNOWN", error.execution_status)
        self.assertEqual("kernel", error.memory_kill)
        self.assertIn("沙箱达到内存上限（MemoryMax=8G）", error)
        self.assertIn("退出码 -9", error)
        self.assertIn("MAKEWAND_SANDBOX_MEMORY_MAX", error)

    def test_bwraps_signal_status_counts_as_a_signal(self):
        (code, _out, _err, error), calls = self.run_shell("exit 137", ("systemd-oomd", "8G"))
        self.assertEqual(137, code)
        self.assertEqual(3, calls[0][1])
        self.assertIn("systemd-oomd", error)
        self.assertEqual("systemd-oomd", error.memory_kill)

    def test_an_ordinary_failure_is_looked_up_once_and_kept_as_it_was(self):
        (code, _out, _err, error), calls = self.run_shell("exit 1", None)
        self.assertEqual(1, code)
        self.assertEqual(1, calls[0][1])
        self.assertEqual("Command returned exit code 1", error)
        self.assertFalse(hasattr(error, "memory_kill"))

    def test_success_is_not_looked_up(self):
        (code, _out, _err, error), calls = self.run_shell("exit 0", ("kernel", "8G"))
        self.assertEqual((0, None, []), (code, error, calls))

    def test_a_lookup_that_raises_changes_nothing(self):
        with patch("makewand.sandbox.sandbox_scope_memory_kill", side_effect=RuntimeError("journal")):
            code, _out, _err, error = base.run_subprocess(["sh", "-c", "exit 3"], timeout=30, separate_stderr=True)
        self.assertEqual((3, "Command returned exit code 3"), (code, error))


class FailureMessageTests(unittest.TestCase):
    def test_the_reason_comes_before_the_providers_last_lines_and_is_logged_as_memory(self):
        note = base.ProcessExecutionError("沙箱达到内存上限（MemoryMax=8G），内核杀掉了沙箱里的进程", "UNKNOWN")
        note.memory_kill = "kernel"
        stderr = base.CapturedStderr("warning: --mode plan has no effect while slash command expansion is disabled.\n")
        with patch("makewand.cli_failure_log.record_message") as message, patch("makewand.cli_failure_log.record") as plain:
            result = base.model_process_failure("agy", -15, "", stderr, note, readonly=True)
        self.assertEqual("UNKNOWN", result.status)
        self.assertFalse(result.outcome_known)
        lines = result[2].splitlines()
        self.assertTrue(lines[0].startswith("沙箱达到内存上限"), lines)
        self.assertIn("warning: --mode plan", lines[1])
        message.assert_called_once()
        self.assertEqual(("agy", "memory"), message.call_args[0][:2])
        plain.assert_not_called()

    def test_without_a_memory_kill_the_message_is_as_before(self):
        error = base.ProcessExecutionError("Command returned exit code -15", "UNKNOWN")
        with patch("makewand.cli_failure_log.record") as plain:
            result = base.model_process_failure("agy", -15, "", "provider says\n", error, readonly=True)
        self.assertEqual("provider says\n", result[2])
        plain.assert_called_once()


class MuseScopeTests(unittest.TestCase):
    def test_the_muse_slice_scope_gets_the_same_options(self):
        from makewand.providers.muse import execute_muse_task
        saved = dict(sandbox._limits_probe)
        self.addCleanup(lambda: (sandbox._limits_probe.clear(), sandbox._limits_probe.update(saved)))
        sandbox._limits_probe.update(systemd=True, oom_policy=True)
        with patch("makewand.config.has_subscription_configured", return_value=True), \
                patch("makewand.health.load_status_cache", return_value={}), \
                patch("makewand.sandbox.is_bwrap_available", return_value=True), \
                patch("makewand.providers.muse._is_dbus_systemd_available", return_value=True), \
                patch("makewand.providers.muse.detect_muse_guard", return_value=(True, "/opt/muse/muse")), \
                patch("makewand.providers.muse.verify_muse_guard", return_value=(True, "")), \
                patch("makewand.sandbox.wrap_bwrap", side_effect=lambda cmd, **kwargs: ["bwrap"] + cmd), \
                patch("makewand.sandbox._provider_socket_preflight"), \
                patch("makewand.providers.muse.run_subprocess", return_value=(0, "ok", "", None)) as run:
            ok, _out, _err = execute_muse_task("echo test", cwd="/tmp", readonly=True)
        self.assertTrue(ok)
        cmd = run.call_args[0][0]
        self.assertIn("--slice=muse", cmd)
        self.assertIn("OOMPolicy=continue", cmd)
        self.assertTrue(any(re.fullmatch(r"--unit=makewand-sandbox-[0-9a-f]{16}", str(arg)) for arg in cmd), cmd)
        self.assertLess(cmd.index("OOMPolicy=continue"), cmd.index("bwrap"))


if __name__ == "__main__":
    unittest.main()
