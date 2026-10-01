"""Native Windows execution/delivery, exercised by the Windows runtime gate."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
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
                                            atomic_copy, copy_backup, inspect_file,
                                            validate_application_security_descriptor)
        target = self.workspace / "file.txt"
        target.write_bytes(b"original")
        original = inspect_file(target)
        security = application_security(target)
        descriptor = application_security_descriptor(target)
        validate_application_security_descriptor(descriptor, security)
        private = config.ensure_private_dir(self.root / "private")
        backup = private / "preimage.txt"
        copy_backup(target, backup)
        self.assertNotEqual(application_security(backup), security)
        atomic_copy(self.workspace, "file.txt", self.source, inspect_file(self.source))

        def before_replace(value):
            self.assertEqual(value, security)
            self.assertEqual(target.read_bytes(), self.source.read_bytes())

        atomic_copy(self.workspace, "file.txt", backup, original,
                    restore_security=descriptor, before_replace=before_replace)
        self.assertEqual(target.read_bytes(), b"original")
        self.assertEqual(application_security(target), security)


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
        from makewand.native_windows import dacl_fingerprint
        real_copy = module._atomic_copy
        calls = 0

        def injected_copy(workspace, relative, source, expected=None, **kwargs):
            nonlocal calls
            if Path(workspace) == self.workspace and expected is not None:
                calls += 1
                if calls == 2:
                    raise OSError("injected Windows apply failure")
            return real_copy(workspace, relative, source, expected, **kwargs)

        before = build_manifest(self.workspace)
        before_acls = {name: dacl_fingerprint(self.workspace / name) for name in before}
        with mock.patch.object(module, "_atomic_copy", side_effect=injected_copy):
            ok, _, message = CandidateManager.apply_candidate("windows-runtime", "B")
        self.assertFalse(ok, message)
        self.assertIn("回滚", message)
        self.assertEqual(build_manifest(self.workspace), before)
        self.assertEqual({name: dacl_fingerprint(self.workspace / name) for name in before}, before_acls)

    def crash_before_commit(self):
        repository = Path(__file__).resolve().parent.parent
        code = "\n".join([
            "import os,sys",
            "from pathlib import Path",
            "sys.path.insert(0," + repr(str(repository)) + ")",
            "from makewand import config,candidate",
            "config.CONFIG_DIR=Path(" + repr(str(config.CONFIG_DIR)) + ")",
            "config.CANDIDATES_DIR=Path(" + repr(str(config.CANDIDATES_DIR)) + ")",
            "config.BACKUPS_DIR=Path(" + repr(str(config.BACKUPS_DIR)) + ")",
            "config.ARTIFACTS_DIR=Path(" + repr(str(config.ARTIFACTS_DIR)) + ")",
            "real_write=candidate._write_application_journal",
            "def stop_at_commit(path,journal):",
            " if journal.get('state')=='committed': os._exit(86)",
            " return real_write(path,journal)",
            "candidate._write_application_journal=stop_at_commit",
            "print(candidate.CandidateManager.apply_candidate('windows-runtime','B'),flush=True)",
            "os._exit(87)",
        ])
        result = subprocess.run([sys.executable, "-I", "-c", code], capture_output=True, text=True, timeout=30)
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

    def test_deadline_and_child_cleanup(self):
        with tempfile.TemporaryDirectory() as temporary:
            marker = Path(temporary) / "escaped.txt"
            child = "import pathlib,time; time.sleep(1.5); pathlib.Path(" + repr(str(marker)) + ").write_text('escaped')"
            parent = "import subprocess,sys,time; subprocess.Popen([sys.executable,'-I','-c'," + repr(child) + "]); print('started',flush=True); time.sleep(10)"
            started = time.monotonic()
            code, out, _, error = run_subprocess([sys.executable, "-I", "-c", parent], timeout=.3)
            self.assertEqual(code, -1)
            self.assertEqual(error.execution_status, "TIMEOUT")
            self.assertLess(time.monotonic() - started, 2)
            self.assertIn("started", out)
            time.sleep(1.7)
            self.assertFalse(marker.exists(), "Job teardown must terminate descendants")

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
