"""Real Git byte preservation and private host audit runtime regressions."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import ctypes
import json
import ntpath
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from makewand import config, git_helper, orchestrator, sandbox
from makewand.artifact import workspace_snapshot


@unittest.skipUnless(shutil.which("git"), "requires Git")
class MetadataPathAdapterTests(unittest.TestCase):
    def test_baseline_and_shadow_convert_metadata_paths_once(self):
        # Model the API namespace on Linux without making pathlib choose its
        # unavailable Windows implementation. Child paths retain the marker,
        # just as a real Win32 API path retains its extended prefix.
        class APIPath(type(Path())):
            pass

        def api_path(path):
            if isinstance(path, APIPath):
                raise ValueError("Windows filesystem operations require an ordinary local drive path")
            return APIPath(path)

        windows_os = mock.Mock(wraps=os)
        windows_os.name = "nt"

        def native_metadata(callback):
            def call(git_dir, *args, **kwargs):
                self.assertNotIsInstance(git_dir, APIPath)
                with mock.patch.object(git_helper, "HOST_TRANSACTION_SUPPORTED", False), \
                     mock.patch.object(git_helper, "os", windows_os):
                    return callback(git_dir, *args, **kwargs)
            return call

        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary) / "source"
            source.mkdir()
            (source / "answer.py").write_bytes(b"answer = 42\n")
            shadow = Path(temporary) / "shadow"
            with mock.patch.object(git_helper, "_fs_path", side_effect=api_path), \
                 mock.patch.object(git_helper, "_own_repository_file", native_metadata(git_helper._own_repository_file)), \
                 mock.patch.object(git_helper, "_own_repository_directory", native_metadata(git_helper._own_repository_directory)), \
                 mock.patch.object(git_helper, "write_repository_file", native_metadata(git_helper.write_repository_file)):
                ok, error = git_helper.init_git_baseline(str(source), ephemeral=True)
                self.assertTrue(ok, error)
                git_helper.clone_isolated_worktree(str(source), shadow)
            self.assertTrue((source / ".git" / git_helper.EPHEMERAL_GIT_MARKER).is_file())
            self.assertTrue((shadow / ".git" / "info").is_dir())
            self.assertEqual((shadow / "answer.py").read_bytes(), b"answer = 42\n")

    def test_windows_home_ignores_home_variable(self):
        # The Go/Python wire fixture must override USERPROFILE as well as HOME.
        with mock.patch.dict(os.environ, {"HOME": r"C:\fixture", "USERPROFILE": r"C:\runner"}):
            self.assertEqual(ntpath.expanduser("~/.cache/makewand"), r"C:\runner/.cache/makewand")
            with mock.patch.dict(os.environ, {"USERPROFILE": os.environ["HOME"]}):
                self.assertEqual(ntpath.expanduser("~/.cache/makewand"), r"C:\fixture/.cache/makewand")


@unittest.skipUnless(shutil.which("git"), "requires Git")
class GitRuntimeRegressionTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-git-runtime-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        for command in (["git", "init", "-q"], ["git", "config", "user.name", "Runtime fixture"],
                        ["git", "config", "user.email", "fixture@example.invalid"]):
            self.git(command)

    def git(self, command, *, binary=False):
        code, output, error = git_helper.run_git_cmd(command, cwd=self.repo, binary=binary)
        self.assertEqual(code, 0, error)
        return output

    def test_chinese_commit_stdout_ignores_legacy_windows_code_page(self):
        (self.repo / "app.py").write_bytes(b"answer = 42\n")
        self.git(["git", "add", "-A"])
        # Force the old TextIOWrapper default on every OS. The actual Git child
        # emits a UTF-8 Chinese subject, including bytes undefined in cp1252.
        with mock.patch("subprocess._text_encoding", return_value="cp1252"):
            output = self.git(["git", "commit", "-m", "修复中文测试"])
        self.assertIn("修复中文测试", output)
        self.assertIn("修复中文测试", self.git(["git", "log", "-1", "--format=%s"]))

    def test_binary_git_objects_are_not_decoded(self):
        payload = b"\xff\x8d\x00keep exactly\r\n"
        (self.repo / "protected.bin").write_bytes(payload)
        self.git(["git", "add", "-A"])
        self.git(["git", "commit", "-qm", "binary baseline"])
        self.assertEqual(self.git(["git", "show", "HEAD:protected.bin"], binary=True), payload)

    def test_non_utf8_gitignore_in_a_plain_workspace_does_not_need_fsdecode(self):
        plain = self.root / "plain"
        plain.mkdir()
        (plain / ".gitignore").write_bytes(b"# \xd6\xd0\xce\xc4\n*.log\n")
        self.assertEqual(orchestrator._filter_non_deliverable_test_changes(
            ["app.py", "server.log"], str(plain)), ["app.py"])

    def test_text_pipe_preserves_non_utf8_bytes_without_reader_failure(self):
        code, output, error = git_helper.run_git_cmd(
            [sys.executable, "-c", "import os; os.write(1, b'\\xff\\x8d')"], cwd=self.repo)
        self.assertEqual(code, 0, error)
        self.assertEqual(output.encode("utf-8", "surrogateescape"), b"\xff\x8d")

    def test_isolated_crlf_tree_matches_review_with_autocrlf_enabled(self):
        home = self.root / "git-home"
        home.mkdir()
        (home / ".gitconfig").write_text("[core]\n\tautocrlf = true\n", encoding="utf-8")
        source = self.root / "source"
        source.mkdir()
        protected = b"keep exactly\r\nsecond protected line\r\n"
        (source / "protected.txt").write_bytes(protected)
        (source / "app.py").write_bytes(b"answer = 0\n")
        shadow = self.root / "shadow"
        with mock.patch.dict(os.environ, {"HOME": str(home), "USERPROFILE": str(home),
                                          "XDG_CONFIG_HOME": str(home / ".config")}):
            inherited = subprocess.run(["git", "config", "--global", "--get", "core.autocrlf"],
                                       capture_output=True, encoding="utf-8", check=False)
            self.assertEqual(inherited.returncode, 0, inherited.stderr)
            self.assertEqual(inherited.stdout.strip(), "true")
            git_helper.clone_isolated_worktree(str(source), shadow)
            self.repo = shadow
            self.assertEqual(self.git(["git", "show", "HEAD:protected.txt"], binary=True), protected)
            baseline = self.git(["git", "rev-parse", "HEAD"]).strip()
            (shadow / "app.py").write_bytes(b"answer = 42\n")
            expected = orchestrator._freeze_delivery_inputs(str(shadow), workspace_snapshot(shadow))[""]
            self.git(["git", "add", "-A"])
            self.git(["git", "commit", "-qm", "修复功能并保留受保护文件"])
            commit = self.git(["git", "rev-parse", "HEAD"]).strip()
            tree = orchestrator._verify_delivery_commit(str(shadow), commit, expected, {})
            self.assertRegex(tree, r"^[0-9a-f]{40,64}$")
            self.assertEqual(self.git(["git", "show", "HEAD:protected.txt"], binary=True), protected)
            self.assertEqual((shadow / "protected.txt").read_bytes(), protected)
            self.assertEqual((source / "protected.txt").read_bytes(), protected)
            patch = self.git(["git", "diff", "--binary", "--full-index", baseline, commit], binary=True)
            exported = self.root / "exported"
            shutil.copytree(source, exported)
            # The delivery shell script and native patch export use this exact
            # invocation, independently of run_git_cmd's safe configuration.
            for arguments in (("--check", "--binary"), ("--binary",)):
                result = subprocess.run(["git", "-c", "core.autocrlf=false", "apply", *arguments, "-"],
                                        cwd=exported, input=patch, capture_output=True, check=False)
                self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((exported / "app.py").read_bytes(), b"answer = 42\n")
            self.assertEqual((exported / "protected.txt").read_bytes(), protected)
            reversed_patch = subprocess.run(["git", "-c", "core.autocrlf=false", "apply", "--reverse", "--binary", "-"],
                                            cwd=exported, input=patch, capture_output=True, check=False)
            self.assertEqual(reversed_patch.returncode, 0, reversed_patch.stderr)
            self.assertEqual((exported / "app.py").read_bytes(), b"answer = 0\n")
            self.assertEqual((exported / "protected.txt").read_bytes(), protected)


class HostAuditRuntimeRegressionTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-audit-runtime-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.config_dir = self.root / "private-config"
        patcher = mock.patch.object(config, "CONFIG_DIR", self.config_dir)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_audit_appends_complete_utf8_records_with_private_permissions(self):
        with mock.patch("makewand.sandbox._warn") as warning:
            for _ in range(2):
                sandbox.audit_unsafe_host_exec("测试上下文", ["fixture", "中文参数"], str(self.root), "config-ack")
        warning.assert_not_called()
        path = self.config_dir / sandbox.UNSAFE_HOST_EXEC_AUDIT_FILE
        records = [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()]
        self.assertEqual(len(records), 2)
        self.assertTrue(all(record["context"] == "测试上下文" and record["args"] == ["中文参数"] for record in records))
        if os.name != "nt":
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertEqual(stat.S_IMODE(self.config_dir.stat().st_mode), 0o700)

    def test_audit_refuses_hardlinks_without_changing_external_file(self):
        config.ensure_private_dir(self.config_dir)
        external = self.root / "external.txt"
        external.write_bytes(b"external preimage")
        path = self.config_dir / sandbox.UNSAFE_HOST_EXEC_AUDIT_FILE
        try:
            os.link(external, path)
        except OSError as error:
            self.skipTest(f"filesystem does not support hard links: {error}")
        with mock.patch("makewand.sandbox._warn") as warning:
            sandbox.audit_unsafe_host_exec("fixture", ["fixture"], str(self.root), "config-ack")
        warning.assert_called_once()
        self.assertEqual(external.read_bytes(), b"external preimage")

    @unittest.skipUnless(os.name == "nt", "requires native Windows DACLs")
    def test_windows_audit_protects_exact_open_handle_before_append(self):
        import msvcrt
        from ctypes import wintypes
        from makewand import native_windows

        advapi = ctypes.WinDLL("advapi32", use_last_error=True)
        kernel = native_windows._api()
        converter = advapi.ConvertSecurityDescriptorToStringSecurityDescriptorW
        converter.argtypes = [ctypes.c_void_p, wintypes.DWORD, wintypes.DWORD,
                              ctypes.POINTER(wintypes.LPWSTR), ctypes.c_void_p]
        converter.restype = wintypes.BOOL
        kernel.LocalFree.argtypes = [ctypes.c_void_p]
        kernel.LocalFree.restype = ctypes.c_void_p
        advapi.OpenProcessToken.argtypes = [wintypes.HANDLE, wintypes.DWORD, ctypes.POINTER(wintypes.HANDLE)]
        advapi.OpenProcessToken.restype = wintypes.BOOL
        advapi.GetTokenInformation.argtypes = [wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p,
                                              wintypes.DWORD, ctypes.POINTER(wintypes.DWORD)]
        advapi.GetTokenInformation.restype = wintypes.BOOL
        advapi.ConvertSidToStringSidW.argtypes = [ctypes.c_void_p, ctypes.POINTER(wintypes.LPWSTR)]
        advapi.ConvertSidToStringSidW.restype = wintypes.BOOL
        advapi.GetSecurityDescriptorDacl.argtypes = [ctypes.c_void_p, ctypes.POINTER(wintypes.BOOL),
                                                    ctypes.POINTER(ctypes.c_void_p), ctypes.POINTER(wintypes.BOOL)]
        advapi.GetSecurityDescriptorDacl.restype = wintypes.BOOL
        advapi.GetAce.argtypes = [ctypes.c_void_p, wintypes.DWORD, ctypes.POINTER(ctypes.c_void_p)]
        advapi.GetAce.restype = wintypes.BOOL
        kernel.GetCurrentProcess.restype = wintypes.HANDLE
        token, sid_text = wintypes.HANDLE(), wintypes.LPWSTR()
        self.assertTrue(advapi.OpenProcessToken(kernel.GetCurrentProcess(), 8, ctypes.byref(token)))
        try:
            needed = wintypes.DWORD()
            advapi.GetTokenInformation(token, 1, None, 0, ctypes.byref(needed))
            data = ctypes.create_string_buffer(needed.value)
            self.assertTrue(advapi.GetTokenInformation(token, 1, data, needed.value, ctypes.byref(needed)))
            user = ctypes.cast(data, ctypes.POINTER(ctypes.c_void_p))[0]
            self.assertTrue(advapi.ConvertSidToStringSidW(user, ctypes.byref(sid_text)))
            user_sid = sid_text.value
        finally:
            if sid_text:
                kernel.LocalFree(ctypes.cast(sid_text, ctypes.c_void_p))
            kernel.CloseHandle(token)
        protected = native_windows.ensure_private_file_descriptor
        captured = []
        actual_accounts = []

        def protect_and_inspect(fd):
            self.assertEqual(os.fstat(fd).st_size, 0, "privacy must be set before audit bytes are appended")
            protected(fd)
            descriptor = native_windows._open_file_security_descriptor(msvcrt.get_osfhandle(fd))
            text = wintypes.LPWSTR()
            self.assertTrue(converter(descriptor, 1, 4, ctypes.byref(text), None))
            try:
                captured.append(text.value)
            finally:
                kernel.LocalFree(ctypes.cast(text, ctypes.c_void_p))
            # SDDL may render the actual user as "LA" (local Administrator).
            # Compare the SID values in the real ACEs, preserving exact owner
            # and SYSTEM authority rather than depending on that presentation.
            present, defaulted, acl = wintypes.BOOL(), wintypes.BOOL(), ctypes.c_void_p()
            self.assertTrue(advapi.GetSecurityDescriptorDacl(descriptor, ctypes.byref(present),
                                                             ctypes.byref(acl), ctypes.byref(defaulted)))
            self.assertTrue(present.value and acl.value)
            for index in range(ctypes.c_ushort.from_address(acl.value + 4).value):
                ace = ctypes.c_void_p()
                self.assertTrue(advapi.GetAce(acl, index, ctypes.byref(ace)))
                self.assertEqual(ctypes.c_ubyte.from_address(ace.value).value, 0, "ACE must allow access")
                self.assertEqual(ctypes.c_ubyte.from_address(ace.value + 1).value, 0, "ACE must not inherit access")
                self.assertEqual(ctypes.c_uint32.from_address(ace.value + 4).value, 0x1F01FF, "ACE must grant file full access")
                account = wintypes.LPWSTR()
                self.assertTrue(advapi.ConvertSidToStringSidW(ace.value + 8, ctypes.byref(account)))
                try:
                    actual_accounts.append(account.value)
                finally:
                    kernel.LocalFree(ctypes.cast(account, ctypes.c_void_p))

        with mock.patch.object(native_windows, "ensure_private_file_descriptor", side_effect=protect_and_inspect), \
                mock.patch("makewand.sandbox._warn") as warning:
            sandbox.audit_unsafe_host_exec("native-fixture", ["fixture", "中文"], str(self.root), "config-ack")
        warning.assert_not_called()
        self.assertEqual(len(captured), 1)
        self.assertTrue(captured[0].startswith("D:P"), captured[0])
        expected_accounts = {"S-1-5-18", user_sid}
        # The SYSTEM token and the fallback SYSTEM trustee are the same SID.
        # Its one full-access ACE must remain exact; ordinary users require two.
        self.assertEqual(len(actual_accounts), len(expected_accounts), captured[0])
        self.assertEqual(set(actual_accounts), expected_accounts, captured[0])
        record = json.loads((self.config_dir / sandbox.UNSAFE_HOST_EXEC_AUDIT_FILE).read_text(encoding="utf-8"))
        self.assertEqual(record["args"], ["中文"])


@unittest.skipUnless(os.name == "nt", "requires native Windows junctions")
class MetadataJunctionTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-metadata-junction-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.shared = self.root / "shared"
        self.shared.mkdir()
        (self.shared / "keep").write_bytes(b"shared template\n")
        self.git_dir = self.root / "repo.git"
        self.git_dir.mkdir()

    def junction(self, path):
        result = subprocess.run(["cmd", "/c", "mklink", "/J", str(path), str(self.shared)],
                                capture_output=True, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(git_helper._is_reparse_path(path))

    def test_junction_metadata_root_is_refused(self):
        linked = self.root / "linked.git"
        self.junction(linked)
        with self.assertRaises(OSError):
            git_helper.write_repository_file(linked, "marker", b"new")
        self.assertEqual(list(self.shared.iterdir()), [self.shared / "keep"])

    def test_junction_metadata_parent_is_replaced_without_writing_its_target(self):
        info = self.git_dir / "info"
        self.junction(info)
        git_helper.write_repository_file(self.git_dir, "info/exclude", b"local/\n")
        self.assertFalse(git_helper._is_reparse_path(info))
        self.assertEqual((info / "exclude").read_bytes(), b"local/\n")
        self.assertEqual(list(self.shared.iterdir()), [self.shared / "keep"])

    def test_junction_at_metadata_file_becomes_a_regular_file(self):
        marker = self.git_dir / "marker"
        self.junction(marker)
        git_helper.write_repository_file(self.git_dir, "marker", b"new")
        self.assertFalse(git_helper._is_reparse_path(marker))
        self.assertEqual(marker.read_bytes(), b"new")
        self.assertEqual((self.shared / "keep").read_bytes(), b"shared template\n")


if __name__ == "__main__":
    unittest.main()
