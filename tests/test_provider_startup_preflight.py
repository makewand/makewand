"""Offline launch diagnostics must not run a provider or consume its seccomp pipe."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import errno
import json
import os
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch

from makewand import sandbox
from makewand.execution_contract import ExecutionRequest
from makewand.execution_runtime import current_context, execute, execution_context
from makewand.providers.base import run_subprocess


class PrivateHomeFallbackTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.runtime, self.cache = self.root / "runtime", self.root / "cache"
        self.runtime_base = self.runtime / sandbox.PRIVATE_HOME_SUBDIR
        self.cache_base = self.cache / sandbox.PRIVATE_HOME_SUBDIR
        for path in (self.runtime_base, self.cache_base):
            path.mkdir(mode=0o700, parents=True)
        env = patch.dict(os.environ, XDG_RUNTIME_DIR=str(self.runtime), XDG_CACHE_HOME=str(self.cache))
        env.start()
        self.addCleanup(env.stop)
        self.home = sandbox.ProviderHome("codex", user_home=str(self.root / "home"))
        self.addCleanup(self.home.finalize, write_back=False)

    @unittest.skipIf(os.name != "posix" or hasattr(os, "geteuid") and os.geteuid() == 0,
                     "needs POSIX permission enforcement for a normal user")
    def test_existing_unwritable_runtime_uses_writable_private_cache(self):
        self.runtime_base.chmod(0o500)
        self.addCleanup(self.runtime_base.chmod, 0o700)
        selected = Path(self.home._ensure_private_dir())
        self.assertEqual(selected.parent, self.cache_base)
        self.assertEqual(selected.stat().st_mode & 0o777, 0o700)
        self.assertEqual(list(selected.iterdir()), [])
        self.assertEqual(self.runtime_base.stat().st_mode & 0o777, 0o500)

    def test_write_failure_cleans_partial_home_before_falling_back(self):
        temporary_file = tempfile.TemporaryFile
        failed = []
        def write_probe(*args, **kwargs):
            directory = Path(kwargs["dir"])
            if directory.parent == self.runtime_base:
                failed.append(directory)
                raise OSError(errno.ENOSPC, "fixture disk full")
            return temporary_file(*args, **kwargs)
        with patch("makewand.sandbox.tempfile.TemporaryFile", side_effect=write_probe):
            selected = Path(self.home._ensure_private_dir())
        self.assertEqual(selected.parent, self.cache_base)
        self.assertTrue(failed)
        self.assertTrue(all(not path.exists() for path in failed))

    def test_all_allowed_bases_failing_reports_environment_failure(self):
        attempted = []
        def refuse(*args, **kwargs):
            attempted.append(Path(kwargs["dir"]))
            raise PermissionError(errno.EACCES, "fixture permission denied")
        with patch("makewand.sandbox.tempfile.mkdtemp", side_effect=refuse):
            with self.assertRaisesRegex(sandbox.SandboxConfigError, "启动环境检查失败.*I/O"):
                self.home._ensure_private_dir()
        self.assertEqual(set(attempted), {self.runtime_base, self.cache_base})
        self.assertIsNone(self.home.private_dir)

    @unittest.skipUnless(os.name == "posix", "POSIX symlink fixture")
    def test_symlinked_runtime_base_is_not_used(self):
        self.runtime_base.rmdir()
        target = self.root / "untouched"
        target.mkdir()
        self.runtime_base.symlink_to(target, target_is_directory=True)
        selected = Path(self.home._ensure_private_dir())
        self.assertEqual(selected.parent, self.cache_base)
        self.assertEqual(list(target.iterdir()), [])


class SocketPreflightTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        env = patch.dict(os.environ, MAKEWAND_EXECUTION_EVIDENCE_DIR=str(self.root / "evidence"))
        env.start()
        self.addCleanup(env.stop)

    def test_failed_probe_prevents_provider_launch(self):
        command = ["fixture-provider", "private-prompt"]
        entered = []
        with patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                patch("makewand.sandbox.authorize_unsandboxed_launch", return_value="fixture"), \
                patch("makewand.sandbox.run_subprocess", return_value=(77, "", "socket: EPERM", None)) as run:
            with self.assertRaisesRegex(sandbox.SandboxConfigError, "启动环境检查失败.*模型尚未启动"):
                with sandbox.provider_sandbox_launch("grok", command, str(self.root), readonly=True):
                    entered.append(True)
        self.assertEqual(entered, [])
        probe = run.call_args.args[0]
        self.assertNotIn("fixture-provider", probe)
        self.assertNotIn("private-prompt", probe)
        self.assertIn("-I", probe)
        self.assertIn("-S", probe)

    def test_network_disabled_paths_do_not_require_network_sockets(self):
        for allow, forced in ((False, ""), (True, "1")):
            with self.subTest(allow=allow, forced=forced):
                with patch.dict(os.environ, MAKEWAND_SANDBOX_UNSHARE_NET=forced), \
                        patch("makewand.sandbox.run_subprocess") as run:
                    sandbox._provider_socket_preflight(["fixture"], ["fixture"], cwd=str(self.root),
                                                       allow_network=allow)
                run.assert_not_called()

    def test_outer_wrapper_is_shared_by_probe_and_actual_launch(self):
        command = ["fixture-provider", "private-prompt"]
        prefix = ["fixture-systemd", "--scope", "--"]
        with patch("makewand.sandbox.is_bwrap_available", return_value=False), \
                patch("makewand.sandbox.authorize_unsandboxed_launch", return_value="fixture"), \
                patch("makewand.sandbox.run_subprocess", return_value=(0, "", "", None)) as run:
            with sandbox.provider_sandbox_launch("grok", command, str(self.root), readonly=True,
                                                 command_prefix=prefix) as launch:
                self.assertEqual(launch.cmd, prefix + command)
        probe = run.call_args.args[0]
        self.assertEqual(probe[:len(prefix)], prefix)
        self.assertNotIn("fixture-provider", probe)
        self.assertNotIn("private-prompt", probe)

    def test_probe_obeys_remaining_dispatch_deadline(self):
        with execution_context(deadline_unix_ms=int((time.time() + 1) * 1000)):
            with patch("makewand.sandbox.run_subprocess", return_value=(0, "", "", None)) as run:
                sandbox._provider_socket_preflight(["fixture"], ["fixture"], cwd=str(self.root), allow_network=True)
        self.assertGreater(run.call_args.kwargs["timeout"], 0)
        self.assertLessEqual(run.call_args.kwargs["timeout"], 1)

    def test_probe_timeout_is_a_launch_environment_failure(self):
        with patch("makewand.sandbox.run_subprocess", return_value=(-9, "", "", "fixture timeout")):
            with self.assertRaisesRegex(sandbox.SandboxConfigError, "启动环境检查失败.*timeout"):
                sandbox._provider_socket_preflight(["fixture"], ["fixture"], cwd=str(self.root), allow_network=True)

    def test_mismatched_command_never_runs_any_process(self):
        with patch("makewand.sandbox.run_subprocess") as run:
            with self.assertRaises(sandbox.SandboxConfigError):
                sandbox._provider_socket_preflight(["different"], ["fixture"], cwd=str(self.root), allow_network=True)
        run.assert_not_called()

    def test_real_helper_does_not_publish_model_exit_or_add_an_attempt(self):
        request = ExecutionRequest(task_id="startup-helper", stage="review", engine="grok", readonly=True,
                                   budget_file=str(self.root / "budget.json"), max_model_calls=3)
        contexts = []
        def callback(remaining):
            before = current_context()
            sandbox._provider_socket_preflight(["fixture"], ["fixture"], cwd=str(self.root), allow_network=True)
            contexts.append((before, current_context()))
            return True, "fixture result", None
        result = execute(request, callback)
        self.assertTrue(result.success, result.error)
        self.assertEqual(contexts[0][0], contexts[0][1])
        record = json.loads((self.root / "evidence" / (result.attempt_id + ".json")).read_text())
        self.assertIsNone(record["process_exit_code"])
        self.assertEqual(len(json.loads((self.root / "budget.json").read_text())["attempts"]), 1)

    @unittest.skipUnless(os.name == "posix", "seccomp pipes on POSIX")
    def test_probe_uses_its_own_filter_and_preserves_actual_filter_bytes(self):
        bpf = sandbox.generate_seccomp_bpf_filter()
        if not bpf:
            self.skipTest("unsupported seccomp architecture")
        with sandbox.seccomp_filter_context(enabled=True, required=True) as (actual, descriptors):
            command = ["bwrap", "--seccomp", str(actual), "--", "fixture-provider"]
            def helper(probe, **kwargs):
                probe_fd = int(probe[probe.index("--seccomp") + 1])
                self.assertNotEqual(probe_fd, actual)
                self.assertNotIn(actual, kwargs["pass_fds"])
                self.assertIn(probe_fd, kwargs["pass_fds"])
                self.assertEqual(os.read(probe_fd, len(bpf)), bpf)
                self.assertIsNone(current_context().get("_evidence"))
                self.assertIsNone(current_context().get("_attempt"))
                return 0, "", "", None
            with patch("makewand.sandbox.run_subprocess", side_effect=helper):
                sandbox._provider_socket_preflight(command, ["fixture-provider"], cwd=str(self.root),
                                                   allow_network=True, seccomp_fd=actual, pass_fds=descriptors)
            self.assertEqual(os.read(actual, len(bpf)), bpf)

    @unittest.skipUnless(os.name == "posix", "bubblewrap on POSIX")
    def test_live_sandbox_can_still_launch_after_probe_consumes_its_filter(self):
        if not sandbox.is_bwrap_available():
            self.skipTest("bubblewrap unavailable")
        # A nested container may allow basic bwrap but deny the PID/proc mount
        # used by the real launch. Check that environment independently first.
        namespace_probe = [sandbox.resolve_trusted_bwrap(), "--ro-bind", "/", "/",
                           "--unshare-pid", "--proc", "/proc", "--", "/bin/true"]
        probe_code, _, probe_err, probe_error = run_subprocess(namespace_probe, timeout=10,
                                                              separate_stderr=True)
        if probe_code != 0:
            self.skipTest(f"bubblewrap PID/proc namespace unavailable: {probe_err or probe_error}")
        home, workspace = self.root / "home", self.root / "work"
        (home / ".codex").mkdir(parents=True)
        workspace.mkdir()
        command = ["/bin/sh", "-c", "printf provider-fixture"]
        with patch.dict(os.environ, HOME=str(home)), \
                patch.dict(os.environ, {"CODEX_HOME": str(home / ".codex")}):
            with sandbox.provider_sandbox_launch("codex", command, str(workspace), readonly=True,
                                                 apply_limits=False) as launch:
                code, out, err, error = run_subprocess(launch.cmd, timeout=10, pass_fds=launch.pass_fds,
                                                     extra_env=launch.env, separate_stderr=True)
        self.assertEqual((code, out, error), (0, "provider-fixture", None), err)


if __name__ == "__main__":
    unittest.main()
