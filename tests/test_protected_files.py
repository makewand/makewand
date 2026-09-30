"""Filesystem regressions for frozen task constraints; no provider calls."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import hashlib
import json
import os
import shutil
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import protected_files as protection
from makewand.protected_files import ProtectedFiles, ProtectionError


@unittest.skipUnless(os.name == "posix" and hasattr(os, "O_NOFOLLOW"), "POSIX protection contract")
class ProtectedFileTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="protected-files-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.base = self.root / "base"
        self.base.mkdir()
        self.file = self.base / "input.txt"
        self.file.write_bytes(b"immutable input\n")
        self.file.chmod(0o640)

    def capture(self, paths=None):
        return ProtectedFiles.capture(self.base, ["input.txt"] if paths is None else paths)

    def clone(self):
        destination = self.root / "copy"
        shutil.copytree(self.base, destination)
        return destination

    def test_capture_deduplicates_and_preserves_full_mode_and_detached_metadata(self):
        self.file.chmod(0o1755)
        guard = self.capture(["input.txt", "input.txt"])
        self.assertEqual(guard.paths, ("input.txt",))
        metadata = guard.to_dict()
        self.assertEqual(metadata["files"]["input.txt"], {
            "sha256": hashlib.sha256(self.file.read_bytes()).hexdigest(), "mode": 0o1755})
        restored = ProtectedFiles.from_dict(json.loads(json.dumps(metadata)))
        metadata["files"]["input.txt"]["mode"] = 0
        self.assertTrue(guard.verify(self.base))
        self.assertEqual(restored.to_dict(), guard.to_dict())

    def test_environment_default_and_explicit_empty_override(self):
        with patch.dict(os.environ, {"MAKEWAND_TASK_PROTECTED_PATHS": '["input.txt"]'}):
            self.assertEqual(ProtectedFiles.capture(self.base).paths, ("input.txt",))
        with patch.dict(os.environ, {"MAKEWAND_TASK_PROTECTED_PATHS": "malformed"}):
            guard = ProtectedFiles.capture(self.base, [])
        with patch("makewand.execution_runtime.current_context", side_effect=AssertionError("empty guard is a no-op")):
            self.assertTrue(guard.verify("/missing/workspace"))
            self.assertTrue(guard.prepare_workspace("/missing/workspace"))
            self.assertEqual(guard.shell_guard("/missing/workspace"), ": # No protected files\n")

    def test_invalid_environment_and_unbounded_list_are_rejected(self):
        for value in ("", "null", '{}', '[false]', '["../outside"]'):
            with self.subTest(value=value), patch.dict(os.environ, {"MAKEWAND_TASK_PROTECTED_PATHS": value}):
                with self.assertRaises(ProtectionError):
                    ProtectedFiles.capture(self.base)
        with self.assertRaises(ProtectionError):
            self.capture(["input.txt"] * (protection.MAX_PROTECTED_FILES + 1))
        with self.assertRaises(ProtectionError):
            ProtectedFiles.capture(self.base, "input.txt")

    def test_invalid_paths_are_rejected(self):
        values = ("/input.txt", "../input.txt", "a/../input.txt", ".git/config", "a/.GiT/config",
                  "", ".", "a//b", "a/./b", "input.txt/", "C:\\input.txt", "\\server\\input.txt",
                  "input\0.txt", None, False, 3, Path("input.txt"), b"input.txt")
        for value in values:
            with self.subTest(path=value), self.assertRaises(ProtectionError):
                self.capture([value])

    def test_missing_directory_fifo_and_symlink_are_not_captured(self):
        (self.base / "directory").mkdir()
        os.mkfifo(self.base / "fifo")
        (self.base / "link").symlink_to(self.file)
        for name in ("missing", "directory", "fifo", "link"):
            with self.subTest(name=name), self.assertRaises(ProtectionError):
                self.capture([name])

    def test_symlink_parent_and_workspace_ancestor_are_rejected(self):
        (self.base / "alias").symlink_to(self.base, target_is_directory=True)
        with self.assertRaises(ProtectionError):
            self.capture(["alias/input.txt"])
        alias = self.root / "workspace-alias"
        alias.symlink_to(self.base, target_is_directory=True)
        with self.assertRaises(ProtectionError):
            ProtectedFiles.capture(alias, ["input.txt"])

    def test_content_mode_and_missing_changes_are_detected_without_writes(self):
        guard = self.capture()
        self.file.write_bytes(b"changed")
        with self.assertRaises(ProtectionError):
            guard.verify(self.base)
        self.assertEqual(self.file.read_bytes(), b"changed")
        self.file.write_bytes(b"immutable input\n")
        self.file.chmod(0o600)
        with self.assertRaises(ProtectionError):
            guard.verify(self.base)
        self.assertEqual(stat.S_IMODE(self.file.stat().st_mode), 0o600)
        self.file.unlink()
        with self.assertRaises(ProtectionError):
            guard.verify(self.base)

    def test_renamed_file_replaced_by_symlink_cannot_use_matching_external_bytes(self):
        guard = self.capture()
        external = self.root / "external"
        self.file.rename(external)
        self.file.symlink_to(external)
        with self.assertRaises(ProtectionError):
            guard.verify(self.base)
        self.assertEqual(external.read_bytes(), b"immutable input\n")

    def test_parent_replacement_by_symlink_is_rejected(self):
        directory = self.base / "nested"
        directory.mkdir()
        (directory / "input.txt").write_bytes(b"input")
        guard = self.capture(["nested/input.txt"])
        external = self.root / "external-directory"
        directory.rename(external)
        directory.symlink_to(external, target_is_directory=True)
        with self.assertRaises(ProtectionError):
            guard.verify(self.base)

    def test_prepare_restores_full_mode_only_in_matching_copy(self):
        self.file.chmod(0o1755)
        guard = self.capture()
        destination = self.clone()
        (destination / "input.txt").chmod(0o644)
        with self.assertRaises(ProtectionError):
            guard.verify(destination)
        self.assertTrue(guard.prepare_workspace(destination))
        self.assertEqual(stat.S_IMODE((destination / "input.txt").stat().st_mode), 0o1755)
        self.assertTrue(guard.verify(self.base))

    def test_prepare_validates_all_bytes_before_any_mode_restore(self):
        second = self.base / "second.txt"
        second.write_bytes(b"second")
        second.chmod(0o600)
        guard = self.capture(["input.txt", "second.txt"])
        destination = self.clone()
        first = destination / "input.txt"
        first.chmod(0o644)
        (destination / "second.txt").write_bytes(b"tampered")
        with self.assertRaises(ProtectionError):
            guard.prepare_workspace(destination)
        self.assertEqual(stat.S_IMODE(first.stat().st_mode), 0o644)

    def test_prepare_does_not_chmod_a_shared_external_inode(self):
        guard = self.capture()
        destination = self.root / "linked-copy"
        destination.mkdir()
        external = self.root / "external"
        external.write_bytes(self.file.read_bytes())
        external.chmod(0o644)
        os.link(external, destination / "input.txt")
        with self.assertRaises(ProtectionError):
            guard.prepare_workspace(destination)
        self.assertEqual(stat.S_IMODE(external.stat().st_mode), 0o644)

    def test_same_snapshot_checks_candidate_and_external_destination_independently(self):
        guard = self.capture()
        destination = self.clone()
        self.assertTrue(guard.verify(destination))
        self.file.write_bytes(b"destination drift")
        self.assertTrue(guard.verify(destination))
        with self.assertRaises(ProtectionError):
            guard.verify(self.base)
        self.assertEqual(self.file.read_bytes(), b"destination drift")

    def test_untrusted_metadata_is_strict_and_bounded(self):
        valid = self.capture().to_dict()
        invalid = [None, {}, {"schema": 1}, {"schema": True, "files": {}},
                   dict(valid, ignored=True), {"schema": 1, "files": []}]
        for record in ({"sha256": "0" * 64, "mode": True}, {"sha256": "0" * 64, "mode": -1},
                       {"sha256": "0" * 64, "mode": 0o10000}, {"sha256": "F" * 64, "mode": 0o644},
                       {"sha256": "not-a-hash", "mode": 0o644}, {"sha256": "0" * 64},
                       {"sha256": "0" * 64, "mode": 0o644, "force": True}):
            invalid.append({"schema": 1, "files": {"input.txt": record}})
        invalid.append({"schema": 1, "files": {"../outside": valid["files"]["input.txt"]}})
        invalid.append({"schema": 1, "files": {str(index): valid["files"]["input.txt"]
                                               for index in range(protection.MAX_PROTECTED_FILES + 1)}})
        for data in invalid:
            with self.subTest(data=data), self.assertRaises(ProtectionError):
                ProtectedFiles.from_dict(data)
        self.assertTrue(ProtectedFiles.from_dict({"schema": 1, "files": {}}).verify(None))

    def test_hashing_uses_bounded_chunks_and_checks_deadline_after_read(self):
        self.file.write_bytes(b"x" * (protection.HASH_CHUNK_BYTES * 3 + 1))
        real_read = os.read
        with patch.object(protection.os, "read", wraps=real_read) as reader:
            guard = self.capture()
        self.assertGreaterEqual(reader.call_count, 5)
        self.assertTrue(all(call.args[1] == protection.HASH_CHUNK_BYTES for call in reader.call_args_list))
        clock = [100.0]

        def delayed_read(*args):
            chunk = real_read(*args)
            clock[0] = 101.0
            return chunk

        with patch("makewand.execution_runtime.current_context", return_value={"_deadline_monotonic": 100.5}), \
                patch.object(protection.time, "monotonic", side_effect=lambda: clock[0]), \
                patch.object(protection.os, "read", side_effect=delayed_read):
            with self.assertRaises(ProtectionError) as error:
                guard.verify(self.base)
        self.assertEqual(error.exception.status, "TIMEOUT")

    def test_expired_capture_does_not_open_or_read_file(self):
        with patch("makewand.execution_runtime.current_context", return_value={"_deadline_monotonic": 0}), \
                patch.object(protection.os, "read", side_effect=AssertionError("expired task must not read")):
            with self.assertRaises(ProtectionError) as error:
                self.capture()
        self.assertEqual(error.exception.status, "TIMEOUT")

    def test_descriptor_hash_rejects_path_rebinding_during_read(self):
        guard = self.capture()
        real_read = os.read
        changed = [False]
        original = self.root / "renamed-original"

        def replace_during_read(*args):
            data = real_read(*args)
            if not changed[0]:
                changed[0] = True
                self.file.rename(original)
                self.file.symlink_to(original)
            return data

        with patch.object(protection.os, "read", side_effect=replace_during_read):
            with self.assertRaises(ProtectionError):
                guard.verify(self.base)
        self.assertEqual(original.read_bytes(), b"immutable input\n")

    def test_shell_guard_handles_special_paths_without_sdk_or_shell_injection(self):
        workspace = self.root / "work 'quotes';$HOME\nline"
        directory = workspace / "nested space"
        directory.mkdir(parents=True)
        name = "nested space/na'me;$(touch INJECTED)\n.txt"
        (workspace / name).write_bytes(b"data")
        guard = ProtectedFiles.capture(workspace, [name])
        script = guard.shell_guard(workspace) + "printf 'verified\\n'\n"
        environment = dict(os.environ, PYTHONPATH="/nonexistent-sdk")
        result = subprocess.run(["sh", "-c", script], cwd=self.root, env=environment,
                                text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "verified\n")
        self.assertFalse((self.root / "INJECTED").exists())

    def test_shell_guard_stops_following_apply_on_content_mode_or_symlink_tampering(self):
        guard = self.capture()
        original = self.file.read_bytes()
        external = self.root / "external"
        external.write_bytes(original)
        external.chmod(0o640)
        marker = self.root / "apply-started"
        script = guard.shell_guard(self.base) + "touch apply-started\n"
        for kind in ("content", "mode", "symlink"):
            with self.subTest(kind=kind):
                if self.file.is_symlink():
                    self.file.unlink()
                self.file.write_bytes(original)
                self.file.chmod(0o640)
                if kind == "content":
                    self.file.write_bytes(b"tampered")
                elif kind == "mode":
                    self.file.chmod(0o600)
                else:
                    self.file.unlink()
                    self.file.symlink_to(external)
                result = subprocess.run(["sh", "-c", script], cwd=self.root,
                                        text=True, capture_output=True, timeout=10)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Protected files preflight failed", result.stderr)
                self.assertFalse(marker.exists())
        self.assertEqual(external.read_bytes(), original)
        self.assertEqual(stat.S_IMODE(external.stat().st_mode), 0o640)

    def test_nonempty_protection_fails_closed_without_posix_primitives(self):
        guard = self.capture()
        with patch.object(protection.os, "supports_dir_fd", set()):
            with self.assertRaises(ProtectionError):
                guard.verify(self.base)
            self.assertTrue(ProtectedFiles().verify(self.base))

    def test_shell_postapply_failure_runs_controlled_rollback(self):
        guard = self.capture()
        self.file.write_bytes(b"changed during apply")
        script = ('rollback() { touch rolled-back; exit 19; }\n'
                  + guard.shell_guard(self.base, rollback_on_error=True)
                  + 'touch applied\n')
        result = subprocess.run(["sh", "-c", script], cwd=self.root,
                                text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 19)
        self.assertTrue((self.root / "rolled-back").exists())
        self.assertFalse((self.root / "applied").exists())
        with self.assertRaises(ProtectionError):
            guard.shell_guard(self.base, rollback_on_error="rollback; injected")


if __name__ == "__main__":
    unittest.main()
