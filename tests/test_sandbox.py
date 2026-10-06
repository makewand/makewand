"""
Unit tests for bubblewrap sandbox bridge.
"""

import os
import sys
import unittest
import tempfile
from pathlib import Path
from makewand.sandbox import is_bwrap_available, wrap_bwrap, run_in_sandbox

class TestSandbox(unittest.TestCase):
    def test_bwrap_availability(self):
        self.assertTrue(is_bwrap_available())

    def test_wrap_bwrap_command_structure(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            wrapped = wrap_bwrap(["echo", "hello"], workspace=tmpdir, allow_network=False)
            self.assertIn("--ro-bind", wrapped)
            self.assertIn("--bind", wrapped)
            self.assertIn("--unshare-net", wrapped)
            self.assertIn("echo", wrapped)

    def test_run_in_sandbox_execution(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            ret, out, err, ex = run_in_sandbox(["echo", "sandbox_active"], workspace=tmpdir)
            self.assertEqual(ret, 0)
            self.assertIn("sandbox_active", out)

            # Test write confinement: writing inside workspace succeeds
            test_file = os.path.join(tmpdir, "test.txt")
            ret, _, _, _ = run_in_sandbox(["bash", "-c", f"echo secret > {test_file}"], workspace=tmpdir)
            self.assertEqual(ret, 0)
            self.assertTrue(os.path.exists(test_file))

    def test_general_sandbox_isolates_home_credentials(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            # 1. Verify wrap_bwrap for general code (is_provider=False) uses tmpfs for user_home
            user_home = str(Path.home())
            wrapped_general = wrap_bwrap(["echo", "hi"], workspace=tmpdir, is_provider=False)
            self.assertIn("--tmpfs", wrapped_general)
            # Find where --tmpfs user_home is specified
            tmpfs_indices = [i for i, x in enumerate(wrapped_general) if x == "--tmpfs"]
            tmpfs_targets = [wrapped_general[i + 1] for i in tmpfs_indices if i + 1 < len(wrapped_general)]
            self.assertIn(user_home, tmpfs_targets)

            # 2. Verify wrap_bwrap for provider (is_provider=True) also strictly isolates user_home with tmpfs
            wrapped_provider = wrap_bwrap(["echo", "hi"], workspace=tmpdir, is_provider=True)
            p_tmpfs_indices = [i for i, x in enumerate(wrapped_provider) if x == "--tmpfs"]
            p_tmpfs_targets = [wrapped_provider[i + 1] for i in p_tmpfs_indices if i + 1 < len(wrapped_provider)]
            self.assertIn(user_home, p_tmpfs_targets)

    def test_subdirectory_mounts_full_worktree(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            # Initialize a git repo with a root file and a subpackage
            from makewand.git_helper import run_git_cmd
            run_git_cmd(["git", "init"], cwd=tmpdir)
            run_git_cmd(["git", "config", "user.name", "test"], cwd=tmpdir)
            run_git_cmd(["git", "config", "user.email", "test@test"], cwd=tmpdir)
            root_file = Path(tmpdir) / "root.txt"
            root_file.write_text("root content\n")
            sub_dir = Path(tmpdir) / "pkg"
            sub_dir.mkdir()
            sub_file = sub_dir / "sub.txt"
            sub_file.write_text("sub content\n")
            run_git_cmd(["git", "add", "-A"], cwd=tmpdir)
            run_git_cmd(["git", "commit", "-m", "init"], cwd=tmpdir)

            # Run inside subdirectory
            wrapped = wrap_bwrap(["cat", "../root.txt"], workspace=str(sub_dir))
            # mount_root should be tmpdir
            bind_indices = [i for i, x in enumerate(wrapped) if x in ("--bind", "--ro-bind")]
            mounted_dirs = [wrapped[i + 1] for i in bind_indices if i + 1 < len(wrapped)]
            self.assertIn(os.path.abspath(tmpdir), mounted_dirs)

    def test_sandbox_cargo_credentials_blocked(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            # Test that in general execution, reading ~/.cargo/credentials.toml fails / is blocked
            ret, out, err, ex = run_in_sandbox(["cat", os.path.expanduser("~/.cargo/credentials.toml")], workspace=tmpdir)
            self.assertNotEqual(ret, 0)

    def test_workspace_cannot_expand_sandbox_via_gitdir(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            # Create an external directory with a sentinel file
            victim_dir = Path(tmpdir) / "victim"
            victim_dir.mkdir()
            sentinel = victim_dir / "secret.txt"
            sentinel.write_text("original content\n")

            # Workspace attempts to point .git to victim_dir
            ws = Path(tmpdir) / "ws"
            ws.mkdir()
            (ws / ".git").write_text(f"gitdir: {victim_dir}\n")

            # Run in sandbox attempting to modify the sentinel file
            ret, out, err, _ = run_in_sandbox(["bash", "-c", f"echo hacked > {sentinel}"], workspace=str(ws), repo_root=str(victim_dir))
            # Must fail, and sentinel content must be intact!
            self.assertNotEqual(ret, 0)
            self.assertEqual(sentinel.read_text(), "original content\n")

    def test_core_worktree_cannot_expand_sandbox_writable_mount(self):
        import shutil
        test_base = Path.cwd() / ".test_worktree_defense"
        test_base.mkdir(parents=True, exist_ok=True)
        try:
            from makewand.git_helper import run_git_cmd
            # Create a victim parent dir with a secret file
            parent_dir = test_base / "parent"
            parent_dir.mkdir(parents=True, exist_ok=True)
            secret = parent_dir / "parent_secret.txt"
            secret.write_text("protected parent data\n")

            # Create an untrusted repo in a child dir
            ws = parent_dir / "child_repo"
            ws.mkdir(parents=True, exist_ok=True)
            run_git_cmd(["git", "init"], cwd=str(ws))
            run_git_cmd(["git", "config", "user.name", "test"], cwd=str(ws))
            run_git_cmd(["git", "config", "user.email", "test@test"], cwd=str(ws))
            (ws / "child.txt").write_text("child data\n")
            run_git_cmd(["git", "add", "-A"], cwd=str(ws))
            run_git_cmd(["git", "commit", "-m", "init"], cwd=str(ws))

            # Set core.worktree to parent directory
            run_git_cmd(["git", "config", "core.worktree", str(parent_dir)], cwd=str(ws))

            import shlex
            # A hidden parent below /tmp may be a writable tmpfs directory;
            # only attempt to overwrite the existing host target.
            command = ["bash", "-c", f"test -f {shlex.quote(str(secret))} && echo overwritten > {shlex.quote(str(secret))}"]
            wrapped = wrap_bwrap(command, workspace=str(ws))
            writable_mounts = [wrapped[i + 1:i + 3] for i, arg in enumerate(wrapped) if arg == "--bind"]
            self.assertEqual(writable_mounts, [[str(ws.resolve()), str(ws.resolve())]])
            ret, out, err, _ = run_in_sandbox(command, workspace=str(ws))
            # The attempt must fail and protected host data must remain intact.
            self.assertNotEqual(ret, 0)
            self.assertEqual(secret.read_text(), "protected parent data\n")
        finally:
            shutil.rmtree(test_base, ignore_errors=True)

    def test_git_directory_rename_and_tamper_blocked_in_sandbox(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            from makewand.git_helper import run_git_cmd
            ws = Path(tmpdir) / "repo"
            ws.mkdir()
            run_git_cmd(["git", "init"], cwd=str(ws))
            run_git_cmd(["git", "config", "user.name", "test"], cwd=str(ws))
            run_git_cmd(["git", "config", "user.email", "test@test"], cwd=str(ws))
            (ws / "file.txt").write_text("hello\n")
            run_git_cmd(["git", "add", "."], cwd=str(ws))
            run_git_cmd(["git", "commit", "-m", "init"], cwd=str(ws))

            # Attempt to rename .git inside sandbox (must fail due to mount point protection)
            ret_rename, _, _, _ = run_in_sandbox(["python3", "-c", "import os; os.rename('.git', '.git.bak')"], workspace=str(ws), readonly=False)
            self.assertNotEqual(ret_rename, 0)
            self.assertTrue((ws / ".git").is_dir())
            self.assertFalse((ws / ".git.bak").exists())

            # Attempt to modify .git/config inside sandbox (must fail with read-only fs error)
            ret_write, _, _, _ = run_in_sandbox(["bash", "-c", "echo malicious >> .git/config"], workspace=str(ws), readonly=False)
            self.assertNotEqual(ret_write, 0)

    def test_unix_domain_socket_and_dbus_isolation_in_sandbox(self):
        import socket
        with tempfile.TemporaryDirectory() as tmpdir:
            ws = Path(tmpdir) / "socket_ws"
            ws.mkdir()
            sock_path = ws / "test.sock"
            s = socket.socket(socket.AF_UNIX)
            s.bind(str(sock_path))
            s.listen(1)

            # Inside sandbox, attempting to connect to the AF_UNIX socket must fail
            code = (
                "import socket, sys\n"
                "s = socket.socket(socket.AF_UNIX)\n"
                f"try:\n"
                f"    s.connect({repr(str(sock_path))})\n"
                f"    sys.exit(0)\n"
                f"except Exception:\n"
                f"    sys.exit(42)\n"
            )
            ret, out, err, _ = run_in_sandbox([sys.executable, "-c", code], workspace=str(ws))
            s.close()
            # Must exit with 42 (connection failed / rejected)
            self.assertEqual(ret, 42)

            # Also verify DBUS env vars and /run/user are not leaked even for muse
            # (fake HOME: provider mounts must never touch the real ~/.config/muse from tests)
            from unittest.mock import patch
            fake_home = Path(tmpdir) / "home"
            (fake_home / ".config" / "muse").mkdir(parents=True)
            with patch.dict(os.environ, {"HOME": str(fake_home), "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1/bus"}):
                cmd_muse = wrap_bwrap(["muse", "run"], workspace=str(ws), is_provider=True)
            self.assertNotIn("/run/user", " ".join(cmd_muse))
            setenv_indices = [i for i, x in enumerate(cmd_muse) if x == "--setenv"]
            setenv_vars = [cmd_muse[i + 1] for i in setenv_indices if i + 1 < len(cmd_muse)]
            self.assertNotIn("DBUS_SESSION_BUS_ADDRESS", setenv_vars)
            self.assertIn("--unsetenv", cmd_muse)
            self.assertIn("--unshare-pid", cmd_muse)

    def test_seccomp_bpf_filter_blocks_dangerous_syscalls(self):
        from makewand.sandbox import generate_seccomp_bpf_filter, is_bwrap_available, run_in_sandbox
        bpf = generate_seccomp_bpf_filter()
        self.assertIsNotNone(bpf)
        self.assertGreater(len(bpf), 16)

        if not is_bwrap_available():
            self.skipTest("bwrap not available")

        with tempfile.TemporaryDirectory() as tmpdir:
            # Inside the sandbox with seccomp, calling ptrace should return -1 with EPERM (errno 1)
            code = (
                "import ctypes, sys, platform\n"
                "libc = ctypes.CDLL(None, use_errno=True)\n"
                "nr = 117 if platform.machine().lower() in ('aarch64', 'arm64') else 101\n"
                "res = libc.syscall(nr, 0, 0, 0, 0)\n"
                "err = ctypes.get_errno()\n"
                "print(f'res={res},err={err}')\n"
                "sys.exit(0 if (res == -1 and err == 1) else 1)\n"
            )
            ret, out, err, _ = run_in_sandbox(
                [sys.executable, "-c", code],
                workspace=tmpdir,
                enable_seccomp=True
            )
            self.assertEqual(ret, 0, f"Expected ptrace blocked with EPERM, got: {out} {err}")

    def test_proc_masking_hardens_kernel_symbols(self):
        from makewand.sandbox import wrap_bwrap
        with tempfile.TemporaryDirectory() as tmpdir:
            cmd = wrap_bwrap(["echo", "hi"], workspace=tmpdir)
            joined = " ".join(cmd)
            self.assertIn("--proc /proc", joined)
            if os.path.exists("/proc/kallsyms"):
                self.assertIn("--ro-bind /dev/null /proc/kallsyms", joined)

    def test_posix_sandbox_rlimits(self):
        if os.name == "posix":
            import subprocess
            code = (
                "import os, sys, resource\n"
                "from makewand.sandbox import apply_posix_sandbox_rlimits\n"
                "apply_posix_sandbox_rlimits()\n"
                "fsize_soft, _ = resource.getrlimit(resource.RLIMIT_FSIZE)\n"
                "assert fsize_soft == 1024 * 1024 * 1024, f'unexpected fsize: {fsize_soft}'\n"
                "as_soft, _ = resource.getrlimit(resource.RLIMIT_AS)\n"
                "assert as_soft == 16 * 1024 * 1024 * 1024, f'unexpected as: {as_soft}'\n"
            )
            res = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True)
            self.assertEqual(res.returncode, 0, f"rlimits test failed: {res.stderr}")

    def test_sandbox_codex_uncreated_override_masked_with_dev_null(self):
        """Regression test for P0/P1-S2: AGENTS.override.md and uncreated files must not trigger /dev/null mount,
        must not leave touch traces on host, and must be cleaned up if created during sandbox execution."""
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as tmpdir:
            fake_home = Path(tmpdir) / "home"
            fake_codex = fake_home / ".codex"
            fake_codex.mkdir(parents=True)
            ws = Path(tmpdir) / "ws"
            ws.mkdir()

            override_path = str(fake_codex / "AGENTS.override.md")
            hooks_json_path = str(fake_codex / "hooks.json")

            policy_path = str(fake_codex / "policy")
            canary_outside = Path(tmpdir) / "canary.txt"
            canary_outside.write_text("precious host data", encoding="utf-8")

            with patch.dict(os.environ, {"HOME": str(fake_home)}, clear=False):
                # Ensure CODEX_HOME does not interfere with fake_home/.codex
                os.environ.pop("CODEX_HOME", None)

                # 1. wrap_bwrap must NOT generate --ro-bind /dev/null for uncreated override files
                cmd = wrap_bwrap(["codex", "exec"], workspace=str(ws), is_provider=True, provider_name="codex")
                ro_bind_indices = [i for i, x in enumerate(cmd) if x == "--ro-bind"]
                ro_bind_pairs = [(cmd[i + 1], cmd[i + 2]) for i in ro_bind_indices if i + 2 < len(cmd)]
                self.assertNotIn(("/dev/null", override_path), ro_bind_pairs)
                self.assertNotIn(("/dev/null", hooks_json_path), ro_bind_pairs)
                self.assertNotIn((override_path, override_path), ro_bind_pairs)

                # 2. Executing run_in_sandbox must not create the uncreated files on host
                ret, out, err, _ = run_in_sandbox(
                    ["bash", "-c", "echo sandbox_ran"],
                    workspace=str(ws),
                    is_provider=True,
                    provider_name="codex",
                )
                self.assertEqual(ret, 0)
                self.assertFalse(os.path.lexists(override_path), f"{override_path} was touched/created on host")
                self.assertFalse(os.path.lexists(hooks_json_path), f"{hooks_json_path} was touched/created on host")

                # 3a. If an uncreated file was created during execution, run_in_sandbox cleans it up in finally
                ret, out, err, _ = run_in_sandbox(
                    ["python3", "-c", f"open({override_path!r}, 'w').write('malicious')"],
                    workspace=str(ws),
                    is_provider=True,
                    provider_name="codex",
                )
                self.assertEqual(ret, 0)
                self.assertFalse(os.path.lexists(override_path), f"{override_path} was not cleaned up after sandbox run")

                # 3b. If a protected directory was created with read-only permissions (0555 dir + 0444 file),
                # robust cleanup must properly remove it without failing silently
                cleanup_code = (
                    f"import os\n"
                    f"os.mkdir({policy_path!r})\n"
                    f"fp = os.path.join({policy_path!r}, 'sub.txt')\n"
                    f"open(fp, 'w').write('sub')\n"
                    f"os.chmod(fp, 0o444)\n"
                    f"os.chmod({policy_path!r}, 0o555)\n"
                )
                ret, out, err, _ = run_in_sandbox(
                    ["python3", "-c", cleanup_code],
                    workspace=str(ws),
                    is_provider=True,
                    provider_name="codex",
                )
                self.assertEqual(ret, 0)
                self.assertFalse(os.path.lexists(policy_path), f"{policy_path} read-only directory was not cleaned up")

                # 3c. If a symlink was planted pointing to outside files, the symlink is removed but the target is intact
                symlink_code = (
                    f"import os\n"
                    f"os.symlink({str(canary_outside)!r}, {override_path!r})\n"
                )
                ret, out, err, _ = run_in_sandbox(
                    ["python3", "-c", symlink_code],
                    workspace=str(ws),
                    is_provider=True,
                    provider_name="codex",
                )
                self.assertEqual(ret, 0)
                self.assertFalse(os.path.lexists(override_path), f"{override_path} symlink was not cleaned up")
                self.assertTrue(canary_outside.exists(), "Outside canary file was mistakenly removed")
                self.assertEqual(canary_outside.read_text(encoding="utf-8"), "precious host data")

                # 4. If an override file already existed on host, it must be protected read-only
                Path(override_path).write_text("user override content", encoding="utf-8")
                cmd2 = wrap_bwrap(["codex", "exec"], workspace=str(ws), is_provider=True, provider_name="codex")
                ro_bind_indices2 = [i for i, x in enumerate(cmd2) if x == "--ro-bind"]
                ro_bind_pairs2 = [(cmd2[i + 1], cmd2[i + 2]) for i in ro_bind_indices2 if i + 2 < len(cmd2)]
                self.assertIn((override_path, override_path), ro_bind_pairs2)

    def test_sandbox_lifecycle_context_manager(self):
        from unittest.mock import patch
        from makewand.sandbox import sandbox_lifecycle
        with tempfile.TemporaryDirectory() as tmpdir:
            fake_home = Path(tmpdir) / "home"
            fake_codex = fake_home / ".codex"
            fake_codex.mkdir(parents=True)
            override_path = str(fake_codex / "AGENTS.override.md")

            with patch.dict(os.environ, {"HOME": str(fake_home)}):
                os.environ.pop("CODEX_HOME", None)
                with sandbox_lifecycle(is_provider=True, provider_name="codex"):
                    # Simulate an uncreated file created during execution
                    Path(override_path).write_text("injected_by_malicious_model")
                    self.assertTrue(os.path.exists(override_path))
                # Must be cleaned up upon exiting sandbox_lifecycle
                self.assertFalse(os.path.exists(override_path))

    def test_apply_posix_sandbox_rlimits_tightens_and_never_widens(self):
        from unittest.mock import patch
        from makewand.sandbox import apply_posix_sandbox_rlimits
        import resource

        # Case 1: nproc_limit < cur_soft -> tightens down
        with patch.dict(os.environ, {"MAKEWAND_SANDBOX_RLIMIT_NPROC": "500"}):
            with patch("resource.getrlimit", return_value=(1000, 2000)):
                with patch("resource.setrlimit") as mock_setrlimit:
                    apply_posix_sandbox_rlimits()
                    mock_setrlimit.assert_any_call(resource.RLIMIT_NPROC, (500, 2000))

        # Case 2: nproc_limit > cur_soft -> does NOT widen
        with patch.dict(os.environ, {"MAKEWAND_SANDBOX_RLIMIT_NPROC": "5000"}):
            with patch("resource.getrlimit", return_value=(1000, 2000)):
                with patch("resource.setrlimit") as mock_setrlimit:
                    apply_posix_sandbox_rlimits()
                    mock_setrlimit.assert_any_call(resource.RLIMIT_NPROC, (1000, 2000))

    def test_collect_uncreated_sensitive_files_scope_isolation(self):
        from makewand.sandbox import _collect_uncreated_sensitive_files
        # When is_provider=False, must return empty list (never tracks ~/.codex or other provider files)
        tracked_non_provider = _collect_uncreated_sensitive_files(is_provider=False)
        self.assertEqual(tracked_non_provider, [])

        tracked_cmd_non_provider = _collect_uncreated_sensitive_files(is_provider=False, cmd=["python3", "-m", "unittest"])
        self.assertEqual(tracked_cmd_non_provider, [])

        # When is_provider=False, even if provider_name is given, must return empty list
        self.assertEqual(_collect_uncreated_sensitive_files(is_provider=False, provider_name="codex"), [])
        self.assertEqual(_collect_uncreated_sensitive_files(is_provider=False, provider_name="claude"), [])

        # When is_provider=True, only the active provider is tracked
        tracked_claude = _collect_uncreated_sensitive_files(is_provider=True, provider_name="claude")
        for p in tracked_claude:
            self.assertIn(".claude", p)
            self.assertNotIn(".codex", p)

        tracked_codex = _collect_uncreated_sensitive_files(is_provider=True, provider_name="codex")
        for p in tracked_codex:
            self.assertIn(".codex", p)
            self.assertNotIn(".claude", p)

        tracked_cmd_claude = _collect_uncreated_sensitive_files(is_provider=True, cmd=["claude", "-p", "hello"])
        for p in tracked_cmd_claude:
            self.assertIn(".claude", p)
            self.assertNotIn(".codex", p)


if __name__ == "__main__":
    unittest.main()


