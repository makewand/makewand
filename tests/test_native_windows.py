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
from makewand.candidate import CandidateManager, build_manifest, _atomic_copy, _atomic_remove
from makewand.native_windows import relative_parts
from makewand.protected_files import ProtectedFiles, ProtectionError
from makewand.providers.base import run_subprocess


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

    def test_copy_remove_and_protected_readonly(self):
        from makewand.native_windows import inspect_file
        expected = inspect_file(self.source)
        _atomic_copy(str(self.workspace), "nested/file.txt", self.source, expected)
        target = self.workspace / "nested" / "file.txt"
        self.assertEqual(target.read_bytes(), b"candidate\r\n")
        target.chmod(stat.S_IREAD)
        protection = ProtectedFiles.capture(self.workspace, ["nested/file.txt"])
        target.chmod(stat.S_IWRITE)
        with self.assertRaises(ProtectionError):
            protection.verify(self.workspace)
        protection.prepare_workspace(self.workspace)
        protection.verify(self.workspace)
        target.chmod(stat.S_IWRITE)
        _atomic_remove(str(self.workspace), "nested/file.txt")
        self.assertFalse(target.exists())

    def test_deep_local_paths_keep_native_copy_security_and_manifest_identity(self):
        from makewand.native_windows import inspect_file, manifest, application_security
        from makewand.windows_paths import filesystem_path
        deep_root = self.root / "deep"
        deep = deep_root
        while len(str(deep)) < 310:
            deep /= "long-path-component-0123456789"
        Path(filesystem_path(deep)).mkdir(parents=True)
        source, workspace = deep / "source.txt", deep / "workspace"
        Path(filesystem_path(source)).write_bytes(b"exact deep payload\r\n")
        Path(filesystem_path(workspace)).mkdir()
        try:
            expected = inspect_file(source)
            _atomic_copy(str(workspace), "nested/protected.txt", source, expected)
            target = workspace / "nested/protected.txt"
            Path(filesystem_path(target)).chmod(stat.S_IREAD)
            protection = ProtectedFiles.capture(workspace, ["nested/protected.txt"])
            original_security = application_security(target)
            protection.verify(workspace)
            records = manifest(workspace)
            self.assertEqual(set(records), {"nested/protected.txt"})
            self.assertEqual(records["nested/protected.txt"], inspect_file(target))
            self.assertEqual(Path(filesystem_path(target)).read_bytes(), b"exact deep payload\r\n")
            self.assertEqual(application_security(target), original_security)
            _atomic_remove(str(workspace), "nested/protected.txt")
            self.assertFalse(Path(filesystem_path(target)).exists())
        finally:
            for child in Path(filesystem_path(deep)).rglob("*"):
                if child.is_file():
                    child.chmod(stat.S_IWRITE)
            shutil.rmtree(filesystem_path(deep_root))

    def test_private_cleanup_binds_original_root_and_is_idempotent(self):
        from makewand.native_windows import private_tree_identity, remove_private_tree, application_security
        original = config.ensure_private_dir(self.root / "owned-private")
        saved = original / "original.txt"
        saved.write_bytes(b"complete original private bytes\r\n")
        saved.chmod(stat.S_IREAD)
        original_identity = private_tree_identity(original)
        retired = self.root / "retired-private"
        original.rename(retired)
        replacement = config.ensure_private_dir(original)
        replacement_file = replacement / "replacement.txt"
        replacement_file.write_bytes(b"replacement must remain unchanged\r\n")
        before = (replacement_file.read_bytes(), stat.S_IMODE(replacement_file.stat().st_mode),
                  application_security(replacement_file))
        replacement_identity = private_tree_identity(replacement)
        try:
            with self.assertRaisesRegex(ValueError, "root identity changed"):
                remove_private_tree(original, original_identity)
            self.assertEqual((replacement_file.read_bytes(), stat.S_IMODE(replacement_file.stat().st_mode),
                              application_security(replacement_file)), before)
            self.assertEqual((retired / "original.txt").read_bytes(), b"complete original private bytes\r\n")
            self.assertEqual(private_tree_identity(retired), original_identity)
            remove_private_tree(retired, original_identity)
            self.assertFalse(retired.exists())
            remove_private_tree(retired, original_identity)
            self.assertEqual((replacement_file.read_bytes(), stat.S_IMODE(replacement_file.stat().st_mode),
                              application_security(replacement_file)), before)
        finally:
            remove_private_tree(retired, original_identity)
            remove_private_tree(replacement, replacement_identity)

    def test_private_cleanup_preflight_preserves_outside_links_and_readonly_files(self):
        from makewand.native_windows import private_tree_identity, remove_private_tree, application_security
        outside = self.root / "outside-cleanup"
        outside.mkdir()
        sentinel = outside / "sentinel.txt"
        sentinel.write_bytes(b"complete outside protected bytes\r\n")
        sentinel.chmod(stat.S_IREAD)
        before = (sentinel.read_bytes(), stat.S_IMODE(sentinel.stat().st_mode), application_security(sentinel))
        for kind in ("hardlink", "junction"):
            with self.subTest(kind=kind):
                owned = config.ensure_private_dir(self.root / ("owned-" + kind))
                legitimate = owned / "00-legitimate-readonly.txt"
                legitimate.write_bytes(b"legitimate private bytes\r\n")
                legitimate.chmod(stat.S_IREAD)
                legitimate_before = (legitimate.read_bytes(), stat.S_IMODE(legitimate.stat().st_mode),
                                     application_security(legitimate))
                identity = private_tree_identity(owned)
                link = owned / "outside-link"
                if kind == "hardlink":
                    os.link(sentinel, link)
                else:
                    self.junction(link, outside)
                try:
                    with self.assertRaises(ValueError):
                        remove_private_tree(owned, identity)
                    self.assertEqual((legitimate.read_bytes(), stat.S_IMODE(legitimate.stat().st_mode),
                                      application_security(legitimate)), legitimate_before)
                    self.assertEqual((sentinel.read_bytes(), stat.S_IMODE(sentinel.stat().st_mode),
                                      application_security(sentinel)), before)
                finally:
                    if kind == "hardlink":
                        _atomic_remove(owned, link.name)
                    else:
                        link.rmdir()
                    remove_private_tree(owned, identity)
                self.assertFalse(owned.exists())
                self.assertEqual((sentinel.read_bytes(), stat.S_IMODE(sentinel.stat().st_mode),
                                  application_security(sentinel)), before)

    def test_junction_parent_and_workspace_are_rejected_without_external_write(self):
        outside = self.root / "outside"
        outside.mkdir()
        sentinel = outside / "file.txt"
        sentinel.write_bytes(b"outside")
        junction = self.workspace / "junction"
        self.junction(junction, outside)
        try:
            with self.assertRaises((ValueError, OSError)):
                _atomic_copy(str(self.workspace), "junction/file.txt", self.source)
            with self.assertRaises((ValueError, OSError)):
                _atomic_remove(str(self.workspace), "junction/file.txt")
            with self.assertRaises((ValueError, OSError)):
                build_manifest(self.workspace)
            with self.assertRaises(ProtectionError):
                ProtectedFiles.capture(self.workspace, ["junction/file.txt"])
            self.assertEqual(sentinel.read_bytes(), b"outside")
        finally:
            junction.rmdir()

    def test_pinned_parent_cannot_be_renamed_during_copy(self):
        from makewand.native_windows import pinned_directory
        parent = self.workspace / "nested"
        parent.mkdir()
        with pinned_directory(parent):
            with self.assertRaises(OSError):
                parent.rename(self.workspace / "moved")
        parent.rename(self.workspace / "moved")

    def test_source_identity_cannot_change_while_reading(self):
        from makewand.native_windows import regular_reader
        with regular_reader(self.source) as reader:
            with self.assertRaises(OSError):
                self.source.write_bytes(b"attacker")
            with self.assertRaises(OSError):
                self.source.rename(self.root / "moved.txt")
            self.assertEqual(reader.read(), b"candidate\r\n")

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

    def test_exact_dacl_preserves_aces_across_protection_transitions(self):
        import ctypes
        from ctypes import wintypes
        from makewand.native_windows import _application_security_descriptor, _application_security_value, _set_dacl
        descriptor = _application_security_descriptor(self.source)
        advapi = ctypes.WinDLL("advapi32", use_last_error=True)
        set_control = advapi.SetSecurityDescriptorControl
        set_control.argtypes = [ctypes.c_void_p, wintypes.WORD, wintypes.WORD]
        set_control.restype = wintypes.BOOL
        get_control = advapi.GetSecurityDescriptorControl
        get_control.argtypes = [ctypes.c_void_p, ctypes.POINTER(wintypes.WORD), ctypes.POINTER(wintypes.DWORD)]
        get_control.restype = wintypes.BOOL
        for protected in (True, False, True):
            with self.subTest(protected=protected):
                self.assertTrue(set_control(descriptor, 0x1000, 0x1000 if protected else 0))
                _set_dacl(self.source, descriptor, protected=protected)
                actual = _application_security_descriptor(self.source)
                self.assertEqual(_application_security_value(actual), _application_security_value(descriptor))
                control, revision = wintypes.WORD(), wintypes.DWORD()
                self.assertTrue(get_control(actual, ctypes.byref(control), ctypes.byref(revision)))
                self.assertEqual(bool(control.value & 0x1000), protected)

    def test_private_open_file_acl_is_bound_to_the_handle_and_rejects_hardlinks(self):
        from makewand.native_windows import application_security, ensure_private_file_descriptor
        target = self.workspace / "audit.log"
        target.write_bytes(b"original")
        before = application_security(target)
        fd = os.open(target, os.O_WRONLY | os.O_APPEND | os.O_BINARY)
        try:
            ensure_private_file_descriptor(fd)
            os.write(fd, b"\nentry")
        finally:
            os.close(fd)
        private = application_security(target)
        self.assertNotEqual(before, private)
        self.assertEqual(target.read_bytes(), b"original\nentry")
        outside = self.root / "outside-hardlink.log"
        os.link(target, outside)
        fd = os.open(target, os.O_WRONLY | os.O_APPEND | os.O_BINARY)
        try:
            with self.assertRaises(ValueError):
                ensure_private_file_descriptor(fd)
        finally:
            os.close(fd)
        self.assertEqual(application_security(outside), private)
        self.assertEqual(outside.read_bytes(), b"original\nentry")

    def test_readonly_unicode_replacement_does_not_change_an_outside_hardlink(self):
        from makewand.native_windows import atomic_copy, atomic_remove, inspect_file
        relative = "中文-😀.txt"
        target = self.workspace / relative
        target.write_bytes(b"before")
        target.chmod(stat.S_IREAD)
        outside = self.root / "outside.txt"
        os.link(target, outside)
        atomic_copy(self.workspace, relative, self.source, inspect_file(self.source))
        self.assertEqual(target.read_bytes(), b"candidate\r\n")
        self.assertEqual(outside.read_bytes(), b"before")
        self.assertFalse(outside.stat().st_mode & stat.S_IWRITE)
        target.chmod(stat.S_IREAD)
        atomic_remove(self.workspace, relative)
        self.assertFalse(target.exists())
        self.assertEqual(outside.read_bytes(), b"before")
        self.assertFalse(outside.stat().st_mode & stat.S_IWRITE)

    def test_replacement_preserves_existing_restrictive_dacl(self):
        from makewand.native_windows import _security_descriptor, _set_dacl, dacl_fingerprint, inspect_file
        state = config.ensure_private_dir(self.root / "private")
        target = self.workspace / "file.txt"
        target.write_bytes(b"before")
        _set_dacl(target, _security_descriptor(state, 4))
        before = dacl_fingerprint(target)
        _atomic_copy(str(self.workspace), "file.txt", self.source, inspect_file(self.source))
        self.assertEqual(dacl_fingerprint(target), before)

    def test_original_security_restores_across_a_private_backup_directory(self):
        from makewand.native_windows import (application_security, application_security_descriptor,
                                            atomic_copy, copy_backup, dacl_fingerprint, inspect_file,
                                            validate_application_security_descriptor)
        target = self.workspace / "file.txt"
        target.write_bytes(b"original")
        legacy_security = dacl_fingerprint(target)
        original = inspect_file(target)
        security = application_security(target)
        descriptor = application_security_descriptor(target)
        validate_application_security_descriptor(descriptor, security)
        private = config.ensure_private_dir(self.root / "private")
        backup = private / "preimage.txt"
        copy_backup(target, backup)
        self.assertNotEqual(application_security(backup), security)
        atomic_copy(self.workspace, "file.txt", self.source, inspect_file(self.source))
        self.assertEqual(dacl_fingerprint(target), legacy_security)
        self.assertEqual(application_security(target), security)

        def before_replace(value):
            self.assertEqual(value, security)
            self.assertEqual(target.read_bytes(), self.source.read_bytes())

        atomic_copy(self.workspace, "file.txt", backup, original,
                    restore_security=descriptor, before_replace=before_replace)
        self.assertEqual(target.read_bytes(), b"original")
        self.assertEqual(application_security(target), security)
        self.assertEqual(dacl_fingerprint(target), legacy_security)
        self.assertEqual(application_security_descriptor(target), descriptor)

    def test_fixed_handle_security_seals_the_saved_ace_flags(self):
        import ctypes
        from ctypes import wintypes
        from makewand.native_windows import (_api, _application_security_value, _open,
                                            _open_file_security_descriptor, _security_descriptor,
                                            application_security)
        target = self.workspace / "inherited.txt"
        target.write_bytes(b"fixture")
        saved = _security_descriptor(target, 4)
        expected = _application_security_value(saved)
        self.assertEqual(application_security(target), expected)
        handle = _open(target, access=0x20000 | 0x80)
        shown = ctypes.c_void_p()
        kernel, advapi = _api(), ctypes.WinDLL("advapi32", use_last_error=True)
        getter = advapi.GetSecurityInfo
        getter.argtypes = [wintypes.HANDLE, ctypes.c_int, wintypes.DWORD, ctypes.c_void_p,
                          ctypes.c_void_p, ctypes.c_void_p, ctypes.c_void_p, ctypes.POINTER(ctypes.c_void_p)]
        getter.restype = wintypes.DWORD
        try:
            self.assertEqual(getter(handle, 1, 4, None, None, None, None, ctypes.byref(shown)), 0)
            presented = _application_security_value(shown)
            self.assertEqual(_application_security_value(_open_file_security_descriptor(handle)), expected)
            get_control = advapi.GetSecurityDescriptorControl
            get_control.argtypes = [ctypes.c_void_p, ctypes.POINTER(wintypes.WORD), ctypes.POINTER(wintypes.DWORD)]
            get_control.restype = wintypes.BOOL
            get_dacl = advapi.GetSecurityDescriptorDacl
            get_dacl.argtypes = [ctypes.c_void_p, ctypes.POINTER(wintypes.BOOL), ctypes.POINTER(ctypes.c_void_p),
                                ctypes.POINTER(wintypes.BOOL)]
            get_dacl.restype = wintypes.BOOL

            def summary(descriptor):
                control, revision = wintypes.WORD(), wintypes.DWORD()
                present, defaulted, acl = wintypes.BOOL(), wintypes.BOOL(), ctypes.c_void_p()
                self.assertTrue(get_control(descriptor, ctypes.byref(control), ctypes.byref(revision)))
                self.assertTrue(get_dacl(descriptor, ctypes.byref(present), ctypes.byref(acl), ctypes.byref(defaulted)))
                entries = []
                if acl:
                    size = ctypes.c_uint16.from_address(acl.value + 2).value
                    count = ctypes.c_uint16.from_address(acl.value + 4).value
                    raw = ctypes.string_at(acl, size)
                    offset = 8
                    for _ in range(count):
                        entry_size = int.from_bytes(raw[offset + 2:offset + 4], "little")
                        entries.append((raw[offset], raw[offset + 1], raw[offset + 4:offset + 8].hex()))
                        offset += entry_size
                return hex(control.value), entries

            # Keep evidence of legacy/current presentation differences without
            # weakening any saved-byte or ACE-inheritance preservation check.
            print("Windows saved/presented DACL seals:", expected, presented,
                  "control/type/ACE flags/mask:", summary(saved), summary(shown))
            self.assertEqual(_application_security_value(_security_descriptor(target, 4)), expected)
        finally:
            if shown:
                kernel.LocalFree.argtypes = [ctypes.c_void_p]
                kernel.LocalFree.restype = ctypes.c_void_p
                kernel.LocalFree(shown)
            kernel.CloseHandle(handle)


@unittest.skipUnless(os.name == "nt", "requires real native Windows delivery")
class NativeWindowsCandidateTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="makewand-win-candidate-")
        self.root = Path(self.directory.name)
        state = self.root / "config"
        self.patch = mock.patch.multiple(config, CONFIG_DIR=state, CANDIDATES_DIR=state / "candidates",
                                         BACKUPS_DIR=state / "backups", ARTIFACTS_DIR=self.root / "artifacts")
        self.patch.start()
        self.addCleanup(self.patch.stop)
        self.addCleanup(self.directory.cleanup)
        self.workspace = self.root / "workspace"
        self.workspace.mkdir()
        self.git("init", "-q")
        self.git("config", "user.name", "Windows runtime test")
        self.git("config", "user.email", "runtime@example.invalid")
        self.git("config", "core.autocrlf", "false")
        (self.workspace / "a.txt").write_bytes(b"before\n")
        (self.workspace / "b.txt").write_bytes(b"delete\n")
        (self.workspace / "protected.txt").write_bytes(b"keep\n")
        self.git("add", "-A")
        self.git("commit", "-qm", "baseline")
        self.baseline = self.git("rev-parse", "HEAD").strip()
        self.candidate = self.root / "candidate"
        shutil.copytree(self.workspace, self.candidate)
        (self.candidate / "a.txt").write_bytes(b"after\n")
        (self.candidate / "b.txt").unlink()
        (self.candidate / "new.txt").write_bytes(b"new\n")
        protection = ProtectedFiles.capture(self.workspace, ["protected.txt"])
        CandidateManager.save_race("windows-runtime", "fixed fixture", str(self.workspace), self.baseline,
                                   {"path": "", "success": False},
                                   {"path": str(self.candidate), "success": True, "test_passed": True, "review_passed": True},
                                   winner="B", protected_files=protection.to_dict())

    def git(self, *arguments):
        result = subprocess.run(["git", *arguments], cwd=self.workspace, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout

    def test_sealed_reviewed_candidate_applies_add_modify_delete(self):
        ok, preview, message = CandidateManager.apply_candidate("windows-runtime", "B", dry_run=True)
        self.assertTrue(ok, message)
        self.assertEqual(len(preview), 3)
        self.assertEqual((self.workspace / "a.txt").read_bytes(), b"before\n")
        ok, paths, message = CandidateManager.apply_candidate("windows-runtime", "B")
        self.assertTrue(ok, message)
        self.assertEqual(len(paths), 3)
        self.assertEqual((self.workspace / "a.txt").read_bytes(), b"after\n")
        self.assertFalse((self.workspace / "b.txt").exists())
        self.assertEqual((self.workspace / "new.txt").read_bytes(), b"new\n")
        self.assertEqual((self.workspace / "protected.txt").read_bytes(), b"keep\n")

    def test_mid_apply_failure_restores_bytes_deletions_and_additions(self):
        from makewand import candidate as module
        from makewand.native_windows import application_security, application_security_descriptor, dacl_fingerprint
        real_copy = module._atomic_copy
        calls = 0

        def injected_copy(workspace, relative, source, expected=None, **kwargs):
            nonlocal calls
            if os.path.samefile(workspace, self.workspace) and expected is not None:
                calls += 1
                if calls == 2:
                    raise OSError("injected Windows apply failure")
            return real_copy(workspace, relative, source, expected, **kwargs)

        before = build_manifest(self.workspace)
        before_acls = {name: dacl_fingerprint(self.workspace / name) for name in before}
        before_seals = {name: application_security(self.workspace / name) for name in before}
        before_descriptors = {name: application_security_descriptor(self.workspace / name) for name in before}
        with mock.patch.object(module, "_atomic_copy", side_effect=injected_copy):
            ok, _, message = CandidateManager.apply_candidate("windows-runtime", "B")
        self.assertFalse(ok, message)
        self.assertIn("回滚", message)
        self.assertEqual(build_manifest(self.workspace), before)
        self.assertEqual({name: dacl_fingerprint(self.workspace / name) for name in before}, before_acls)
        self.assertEqual({name: application_security(self.workspace / name) for name in before}, before_seals)
        self.assertEqual({name: application_security_descriptor(self.workspace / name) for name in before}, before_descriptors)

    def crash_before_commit(self):
        try:
            from test_candidate_recovery import _run_crash_fixture
        except ImportError:
            from tests.test_candidate_recovery import _run_crash_fixture

        repository = Path(__file__).resolve().parent.parent
        code = "\n".join([
            "import os,sys",
            "from pathlib import Path",
            "sys.path.insert(0," + repr(str(repository)) + ")",
            "from makewand import config,candidate",
            "_fixture_phase('imports-ready')",
            "config.CONFIG_DIR=Path(" + repr(str(config.CONFIG_DIR)) + ")",
            "config.CANDIDATES_DIR=Path(" + repr(str(config.CANDIDATES_DIR)) + ")",
            "config.BACKUPS_DIR=Path(" + repr(str(config.BACKUPS_DIR)) + ")",
            "config.ARTIFACTS_DIR=Path(" + repr(str(config.ARTIFACTS_DIR)) + ")",
            "_fixture_phase('config-ready')",
            "real_write=candidate._write_application_journal",
            "def stop_at_commit(path,journal):",
            " _fixture_phase('journal-'+str(journal.get('state','none')))",
            " if journal.get('state')=='committed':",
            "  _fixture_phase('crash-before-commit')",
            "  os._exit(86)",
            " return real_write(path,journal)",
            "candidate._write_application_journal=stop_at_commit",
            "_fixture_phase('apply-start')",
            "print(candidate.CandidateManager.apply_candidate('windows-runtime','B'),flush=True)",
            "_fixture_phase('apply-returned-without-crash')",
            "os._exit(87)",
        ])
        result = _run_crash_fixture(code)
        self.assertEqual(result.returncode, 86, result.stdout + result.stderr)
        self.assertEqual((self.workspace / "a.txt").read_bytes(), b"after\n")
        self.assertFalse((self.workspace / "b.txt").exists())
        self.assertEqual((self.workspace / "new.txt").read_bytes(), b"new\n")
        journals = list(config.BACKUPS_DIR.glob("*/journal.json"))
        self.assertEqual(len(journals), 1)
        journal = json.loads(journals[0].read_text(encoding="utf-8"))
        self.assertEqual(journal["state"], "prepared")
        for entry in journal["entries"]:
            if entry["after"] is not None:
                self.assertTrue(entry["after_security"])
        return journals[0], journal

    def assert_acl_conflict_preserves_whole_batch(self, relative):
        from makewand.native_windows import _security_descriptor, _set_dacl, application_security
        journal_path, _ = self.crash_before_commit()
        postimages = build_manifest(self.workspace)
        target = self.workspace / relative
        original = application_security(target)
        private = config.ensure_private_dir(self.root / "edited-acl")
        _set_dacl(target, _security_descriptor(private, 4))
        edited = application_security(target)
        self.assertNotEqual(edited, original)
        self.assertEqual(build_manifest(self.workspace), postimages, "This edit changes only Windows security")
        ok, restored, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertFalse(ok, message)
        self.assertEqual(restored, [])
        self.assertIn("security", message.lower())
        self.assertEqual(build_manifest(self.workspace), postimages, "A later ACL conflict must prevent every rollback")
        self.assertEqual(application_security(target), edited)
        self.assertEqual(json.loads(journal_path.read_text(encoding="utf-8"))["state"], "prepared")

    def test_existing_file_acl_edit_blocks_the_whole_interrupted_batch(self):
        self.assert_acl_conflict_preserves_whole_batch("a.txt")

    def test_new_file_acl_edit_blocks_the_whole_interrupted_batch(self):
        self.assert_acl_conflict_preserves_whole_batch("new.txt")

    def test_damaged_original_acl_descriptor_blocks_every_restore(self):
        from makewand import candidate as module
        journal_path, journal = self.crash_before_commit()
        postimages = build_manifest(self.workspace)
        entry = next(entry for entry in journal["entries"] if entry["path"] == "a.txt")
        entry["before_security_descriptor"] = "invalid-base64"
        module._write_application_journal(journal_path, journal)
        ok, restored, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertFalse(ok, message)
        self.assertEqual(restored, [])
        self.assertEqual(build_manifest(self.workspace), postimages)
        self.assertTrue(journal_path.exists(), "Invalid evidence must be retained for manual review")

    def test_post_review_mutation_is_not_applied(self):
        (self.candidate / "a.txt").write_bytes(b"unreviewed")
        ok, _, message = CandidateManager.apply_candidate("windows-runtime", "B", force=True)
        self.assertFalse(ok, message)
        self.assertEqual((self.workspace / "a.txt").read_bytes(), b"before\n")


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
