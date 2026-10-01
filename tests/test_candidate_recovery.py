"""Abrupt process exits leave a durable, idempotent candidate rollback plan."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import contextlib
import shutil
import subprocess
import sys
import tempfile
import unittest
from types import SimpleNamespace
from pathlib import Path
from unittest import mock

from makewand import config
from makewand import candidate as candidate_module
from makewand.candidate import CandidateManager, build_manifest


class CandidateRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="makewand-recovery-")
        self.root = Path(self.directory.name)
        self.workspace = self.root / "workspace"
        self.workspace.mkdir()
        state = self.root / "config"
        self.patch = mock.patch.multiple(config, CONFIG_DIR=state, CANDIDATES_DIR=state / "candidates",
                                         BACKUPS_DIR=state / "backups", ARTIFACTS_DIR=self.root / "artifacts")
        self.patch.start()
        self.addCleanup(self.patch.stop)
        self.addCleanup(self.directory.cleanup)
        self.git("init", "-q")
        self.git("config", "user.name", "Recovery fixture")
        self.git("config", "user.email", "fixture@example.invalid")
        self.git("config", "core.autocrlf", "false")
        # journal.json is an ordinary project file and must not collide with
        # the recovery metadata in its private backup directory.
        for name in ("a.txt", "b.txt", "journal.json"):
            (self.workspace / name).write_bytes(("before:" + name + "\n").encode())
        self.git("add", "-A")
        self.git("commit", "-qm", "baseline")
        self.baseline = self.git("rev-parse", "HEAD").strip()
        self.candidate = self.root / "candidate"
        shutil.copytree(self.workspace, self.candidate)
        (self.candidate / "a.txt").write_bytes(b"after:a\n")
        (self.candidate / "b.txt").unlink()
        (self.candidate / "journal.json").write_bytes(b"after:journal\n")
        (self.candidate / "new.txt").write_bytes(b"new\n")
        self.before = build_manifest(self.workspace)
        CandidateManager.save_race("recovery-fixture", "fixed fixture", str(self.workspace), self.baseline,
                                   {"path": "", "success": False},
                                   {"path": str(self.candidate), "success": True, "test_passed": True, "review_passed": True},
                                   winner="B")

    def git(self, *arguments):
        result = subprocess.run(["git", *arguments], cwd=self.workspace, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout

    def crash_apply(self, *, all_changes=False, during_rollback=False):
        code = f"""import os,sys
from pathlib import Path
sys.path.insert(0,{str(Path(__file__).resolve().parent.parent)!r})
from makewand import config
from makewand import candidate
config.CONFIG_DIR=Path({str(config.CONFIG_DIR)!r})
config.CANDIDATES_DIR=Path({str(config.CANDIDATES_DIR)!r})
config.BACKUPS_DIR=Path({str(config.BACKUPS_DIR)!r})
config.ARTIFACTS_DIR=Path({str(config.ARTIFACTS_DIR)!r})
real_copy=candidate._atomic_copy
real_restore=candidate._restore_application_entry
real_write=candidate._write_private_json
count=0
def crash_copy(workspace,relative,source,expected=None,**kwargs):
    global count
    application=Path(workspace).samefile(Path({str(self.workspace)!r}))
    if application and expected is not None and 'preimages' not in Path(source).parts:
        count+=1
        if {during_rollback!r} and count==2:
            raise OSError('injected second mutation failure')
    result=real_copy(workspace,relative,source,expected,**kwargs)
    if application and not {all_changes!r} and not {during_rollback!r}:
        os._exit(86)
    return result
def crash_restore(workspace,item,folder,**kwargs):
    result=real_restore(workspace,item,folder,**kwargs)
    if {during_rollback!r}:
        os._exit(86)
    return result
def crash_commit(path,data):
    if data.get('state')=='committed':
        os._exit(86)
    return real_write(path,data)
candidate._atomic_copy=crash_copy
candidate._restore_application_entry=crash_restore
if {all_changes!r}:
    candidate._write_private_json=crash_commit
candidate.CandidateManager.apply_candidate('recovery-fixture','B')
os._exit(87)
"""
        result = subprocess.run([sys.executable, "-I", "-c", code], capture_output=True, text=True, timeout=20)
        self.assertEqual(result.returncode, 86, result.stdout + result.stderr)
        journal = next(config.BACKUPS_DIR.glob("*/journal.json"))
        record = json.loads(journal.read_text(encoding="utf-8"))
        self.assertEqual(record["state"], "prepared")
        self.assertTrue(record["entries"])
        return journal

    def crash_during_temporary_write(self, *, during_rollback=False, nested=False):
        if nested:
            source = self.candidate / "nested" / "deep" / "answer.txt"
            source.parent.mkdir(parents=True)
            source.write_bytes(b"verified payload\n" * 5000)
            CandidateManager.save_race("recovery-fixture", "fixed fixture", str(self.workspace), self.baseline,
                {"path": "", "success": False}, {"path": str(self.candidate), "success": True,
                "test_passed": True, "review_passed": True}, winner="B")
        code = f"""import os,sys
from pathlib import Path
sys.path.insert(0,{str(Path(__file__).resolve().parent.parent)!r})
from makewand import config,candidate
config.CONFIG_DIR=Path({str(config.CONFIG_DIR)!r})
config.CANDIDATES_DIR=Path({str(config.CANDIDATES_DIR)!r})
config.BACKUPS_DIR=Path({str(config.BACKUPS_DIR)!r})
config.ARTIFACTS_DIR=Path({str(config.ARTIFACTS_DIR)!r})
workspace=Path({str(self.workspace)!r})
desired_parent=workspace / {'nested/deep' if nested else '.'!r}
real_fdopen=os.fdopen
real_copy=candidate._atomic_copy
rollback_started=False
mutations=0
class CrashWriter:
    def __init__(self,stream): self.stream=stream
    def __enter__(self): self.stream.__enter__(); return self
    def __exit__(self,*args): return self.stream.__exit__(*args)
    def __getattr__(self,name): return getattr(self.stream,name)
    def write(self,data):
        self.stream.write(data[:max(1,len(data)//2)])
        self.stream.flush()
        os.fsync(self.stream.fileno())
        os._exit(86)
def crash_fdopen(fd,mode='r',*args,**kwargs):
    stream=real_fdopen(fd,mode,*args,**kwargs)
    if mode=='wb' and (not {during_rollback!r} or rollback_started):
        if desired_parent.is_dir() and any(path.parent.samefile(desired_parent) for path in workspace.rglob('.makewand-*')):
            return CrashWriter(stream)
    return stream
def trigger_rollback(root,relative,source,expected=None,**kwargs):
    global rollback_started,mutations
    if Path(root).samefile(workspace) and 'preimages' not in Path(source).parts:
        mutations+=1
        if {during_rollback!r} and mutations==2:
            rollback_started=True
            raise OSError('injected failure requiring rollback')
    return real_copy(root,relative,source,expected,**kwargs)
os.fdopen=crash_fdopen
candidate._atomic_copy=trigger_rollback
candidate.CandidateManager.apply_candidate('recovery-fixture','B')
os._exit(87)
"""
        result = subprocess.run([sys.executable, "-I", "-c", code], capture_output=True, text=True, timeout=20)
        self.assertEqual(result.returncode, 86, result.stdout + result.stderr)
        journal = next(config.BACKUPS_DIR.glob("*/journal.json"))
        record = json.loads(journal.read_text(encoding="utf-8"))
        scratch = list(self.workspace.rglob(".makewand-*"))
        self.assertEqual(len(scratch), 1)
        self.assertGreater(scratch[0].stat().st_size, 0)
        self.assertIn(scratch[0].relative_to(self.workspace).as_posix(), [entry["temp"] for entry in record["entries"]])
        return journal, scratch[0]

    def test_crash_after_first_mutation_recovers_and_recovery_is_idempotent(self):
        journal = self.crash_apply()
        self.assertNotEqual(build_manifest(self.workspace), self.before)
        ok, recovered, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertTrue(ok, message)
        self.assertEqual(len(recovered), 1)
        self.assertEqual(build_manifest(self.workspace), self.before)
        self.assertEqual(json.loads(journal.read_text(encoding="utf-8"))["state"], "rolled_back")
        ok, recovered, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertTrue(ok, message)
        self.assertEqual(recovered, [])

    def test_crash_after_all_mutations_restores_deletions_additions_and_project_journal(self):
        self.crash_apply(all_changes=True)
        self.assertFalse((self.workspace / "b.txt").exists())
        self.assertTrue((self.workspace / "new.txt").exists())
        ok, _, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertTrue(ok, message)
        self.assertEqual(build_manifest(self.workspace), self.before)

    def test_hard_exit_during_rollback_can_resume(self):
        self.crash_apply(during_rollback=True)
        ok, _, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertTrue(ok, message)
        self.assertEqual(build_manifest(self.workspace), self.before)

    def test_later_user_edit_refuses_entire_recovery_without_partial_changes(self):
        self.crash_apply(all_changes=True)
        (self.workspace / "journal.json").write_bytes(b"later user edit\n")
        later = build_manifest(self.workspace)
        ok, recovered, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertFalse(ok, message)
        self.assertEqual(recovered, [])
        self.assertIn("conflicts", message)
        self.assertEqual(build_manifest(self.workspace), later)

    def test_backup_tampering_refuses_entire_recovery(self):
        journal = self.crash_apply(all_changes=True)
        (journal.parent / "preimages" / "a.txt").write_bytes(b"tampered backup")
        interrupted = build_manifest(self.workspace)
        ok, _, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertFalse(ok, message)
        self.assertIn("backup changed", message)
        self.assertEqual(build_manifest(self.workspace), interrupted)

    def test_next_apply_recovers_then_applies_sealed_candidate(self):
        self.crash_apply()
        ok, paths, message = CandidateManager.apply_candidate("recovery-fixture", "B")
        self.assertTrue(ok, message)
        self.assertEqual(len(paths), 4)
        self.assertEqual(build_manifest(self.workspace), build_manifest(self.candidate))

    def test_edit_of_later_target_is_preserved_before_write(self):
        real_copy = candidate_module._atomic_copy
        def edit_later(workspace, relative, source, expected=None, **kwargs):
            result = real_copy(workspace, relative, source, expected, **kwargs)
            if Path(workspace).samefile(self.workspace) and relative == "a.txt" and "preimages" not in Path(source).parts:
                (self.workspace / "b.txt").write_bytes(b"later user edit\n")
            return result
        with mock.patch.object(candidate_module, "_atomic_copy", side_effect=edit_later):
            ok, _, message = CandidateManager.apply_candidate("recovery-fixture", "B")
        self.assertFalse(ok, message)
        self.assertEqual((self.workspace / "a.txt").read_bytes(), b"before:a.txt\n")
        self.assertEqual((self.workspace / "b.txt").read_bytes(), b"later user edit\n")
        self.assertFalse((self.workspace / "new.txt").exists())

    def test_force_does_not_authorize_edits_after_transaction_start(self):
        (self.workspace / "b.txt").write_bytes(b"prior approved edit\n")
        real_copy = candidate_module._atomic_copy
        def edit_later(workspace, relative, source, expected=None, **kwargs):
            result = real_copy(workspace, relative, source, expected, **kwargs)
            if Path(workspace).samefile(self.workspace) and relative == "a.txt" and "preimages" not in Path(source).parts:
                (self.workspace / "b.txt").write_bytes(b"new unapproved edit\n")
            return result
        with mock.patch.object(candidate_module, "_atomic_copy", side_effect=edit_later):
            ok, _, message = CandidateManager.apply_candidate("recovery-fixture", "B", force=True)
        self.assertFalse(ok, message)
        self.assertEqual((self.workspace / "b.txt").read_bytes(), b"new unapproved edit\n")
        self.assertEqual((self.workspace / "a.txt").read_bytes(), b"before:a.txt\n")

    def test_partial_failure_does_not_rollback_over_user_edit(self):
        def fail_second(workspace, relative, **kwargs):
            self.assertEqual(relative, "b.txt")
            (self.workspace / "a.txt").write_bytes(b"user edit after apply\n")
            raise OSError("injected deletion failure")
        with mock.patch.object(candidate_module, "_atomic_remove", side_effect=fail_second):
            ok, _, message = CandidateManager.apply_candidate("recovery-fixture", "B")
        self.assertFalse(ok, message)
        self.assertIn("conflicts", message)
        self.assertEqual((self.workspace / "a.txt").read_bytes(), b"user edit after apply\n")
        self.assertEqual((self.workspace / "b.txt").read_bytes(), b"before:b.txt\n")
        journal = next(config.BACKUPS_DIR.glob("*/journal.json"))
        self.assertEqual(json.loads(journal.read_text(encoding="utf-8"))["state"], "failed")

    def test_commit_checks_every_postimage_and_preserves_external_edit(self):
        real_copy = candidate_module._atomic_copy
        def edit_first_after_final_copy(workspace, relative, source, expected=None, **kwargs):
            result = real_copy(workspace, relative, source, expected, **kwargs)
            if Path(workspace).samefile(self.workspace) and relative == "new.txt":
                (self.workspace / "a.txt").write_bytes(b"user edit before commit\n")
            return result
        with mock.patch.object(candidate_module, "_atomic_copy", side_effect=edit_first_after_final_copy):
            ok, _, message = CandidateManager.apply_candidate("recovery-fixture", "B")
        self.assertFalse(ok, message)
        self.assertIn("postimage", message)
        self.assertEqual((self.workspace / "a.txt").read_bytes(), b"user edit before commit\n")
        self.assertFalse((self.workspace / "b.txt").exists())
        self.assertTrue((self.workspace / "new.txt").exists())
        journal = next(config.BACKUPS_DIR.glob("*/journal.json"))
        self.assertEqual(json.loads(journal.read_text(encoding="utf-8"))["state"], "failed")

    def test_commit_publication_error_never_claims_rollback(self):
        real_write = candidate_module._write_private_json
        def fail_after_commit(path, data):
            result = real_write(path, data)
            if data.get("state") == "committed":
                raise OSError("injected error after atomic journal publication")
            return result
        with mock.patch.object(candidate_module, "_write_private_json", side_effect=fail_after_commit):
            ok, _, message = CandidateManager.apply_candidate("recovery-fixture", "B")
        self.assertFalse(ok, message)
        self.assertEqual(message.status, "UNKNOWN")
        self.assertEqual(build_manifest(self.workspace), build_manifest(self.candidate))
        journal = next(config.BACKUPS_DIR.glob("*/journal.json"))
        self.assertEqual(json.loads(journal.read_text(encoding="utf-8"))["state"], "committed")

    def test_commit_failure_before_publication_rolls_back(self):
        real_write = candidate_module._write_private_json
        def fail_before_commit(path, data):
            if data.get("state") == "committed":
                raise OSError("injected error before journal publication")
            return real_write(path, data)
        with mock.patch.object(candidate_module, "_write_private_json", side_effect=fail_before_commit):
            ok, _, message = CandidateManager.apply_candidate("recovery-fixture", "B")
        self.assertFalse(ok, message)
        self.assertEqual(build_manifest(self.workspace), self.before)

    def test_recovery_rechecks_each_target_after_preflight(self):
        journal = self.crash_apply(all_changes=True)
        real_restore = candidate_module._restore_application_entry
        mutated = False
        def edit_after_first_restore(root, item, folder, **kwargs):
            nonlocal mutated
            result = real_restore(root, item, folder, **kwargs)
            if not mutated:
                mutated = True
                (self.workspace / "a.txt").write_bytes(b"user edit during recovery\n")
            return result
        with mock.patch.object(candidate_module, "_restore_application_entry", side_effect=edit_after_first_restore):
            ok, _, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertFalse(ok, message)
        self.assertIn("conflicts", message)
        self.assertEqual((self.workspace / "a.txt").read_bytes(), b"user edit during recovery\n")
        self.assertEqual(json.loads(journal.read_text(encoding="utf-8"))["state"], "prepared")

    @unittest.skipUnless(os.name == "posix", "Windows pins the workspace against root replacement")
    def test_workspace_replacement_preserves_new_directory_during_failure(self):
        moved = self.root / "detached-workspace"
        real_copy = candidate_module._atomic_copy
        def move_after_first_copy(workspace, relative, source, expected=None, **kwargs):
            result = real_copy(workspace, relative, source, expected, **kwargs)
            if Path(workspace).samefile(self.workspace) and relative == "a.txt" and "preimages" not in Path(source).parts:
                self.workspace.rename(moved)
                self.workspace.mkdir()
                (self.workspace / "a.txt").write_bytes(b"replacement workspace\n")
            return result
        with mock.patch.object(candidate_module, "_atomic_copy", side_effect=move_after_first_copy):
            ok, _, message = CandidateManager.apply_candidate("recovery-fixture", "B")
        self.assertFalse(ok, message)
        self.assertIn("identity changed", message)
        self.assertEqual((self.workspace / "a.txt").read_bytes(), b"replacement workspace\n")
        self.assertEqual((moved / "a.txt").read_bytes(), b"after:a\n")

    def test_hard_exit_during_temp_write_recovers_without_residue(self):
        self.crash_during_temporary_write()
        ok, _, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertTrue(ok, message)
        self.assertEqual(build_manifest(self.workspace), self.before)
        self.assertEqual(list(self.workspace.rglob(".makewand-*")), [])

    def test_hard_exit_during_rollback_temp_write_is_resumable(self):
        self.crash_during_temporary_write(during_rollback=True)
        ok, _, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertTrue(ok, message)
        self.assertEqual(build_manifest(self.workspace), self.before)
        self.assertEqual(list(self.workspace.rglob(".makewand-*")), [])

    def test_hard_exit_during_nested_temp_write_removes_created_directories(self):
        self.crash_during_temporary_write(nested=True)
        ok, _, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertTrue(ok, message)
        self.assertEqual(build_manifest(self.workspace), self.before)
        self.assertFalse((self.workspace / "nested").exists())

    def test_unknown_scratch_content_blocks_entire_recovery(self):
        journal, scratch = self.crash_during_temporary_write()
        scratch.write_bytes(b"unrelated later user content\n")
        interrupted = build_manifest(self.workspace)
        ok, _, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertFalse(ok, message)
        self.assertIn("temporary content changed", message)
        self.assertEqual(build_manifest(self.workspace), interrupted)
        self.assertEqual(json.loads(journal.read_text(encoding="utf-8"))["state"], "prepared")

    def opened_root_swap(self, mutation):
        replacement = self.root / "replacement-workspace"
        detached = self.root / "detached-original"
        shutil.copytree(self.workspace, replacement)
        (replacement / "a.txt").write_bytes(b"independent replacement file\n")
        replacement_before = build_manifest(replacement)
        identity = os.stat(self.workspace)
        expected_identity = [identity.st_dev, identity.st_ino]
        real_open = os.open
        swapped = False
        def open_replacement_then_restore_original(path, flags, *arguments, **kwargs):
            nonlocal swapped
            if (not swapped and Path(path) == self.workspace
                    and flags & os.O_DIRECTORY and "dir_fd" not in kwargs):
                swapped = True
                self.workspace.rename(detached)
                replacement.rename(self.workspace)
                try:
                    opened = real_open(path, flags, *arguments, **kwargs)
                finally:
                    self.workspace.rename(replacement)
                    detached.rename(self.workspace)
                return opened
            return real_open(path, flags, *arguments, **kwargs)
        with mock.patch.object(candidate_module.os, "open", side_effect=open_replacement_then_restore_original):
            with self.assertRaisesRegex(ValueError, "opened workspace identity changed"):
                mutation(expected_identity)
        self.assertTrue(swapped, "fixture must capture the replacement directory handle")
        self.assertEqual(build_manifest(self.workspace), self.before)
        self.assertEqual(build_manifest(replacement), replacement_before)

    @unittest.skipUnless(os.name == "posix", "Windows pins the workspace against root replacement")
    def test_copy_checks_captured_root_identity_after_path_is_restored(self):
        self.opened_root_swap(lambda identity: candidate_module._atomic_copy(
            str(self.workspace), "a.txt", self.candidate / "a.txt",
            candidate_module.file_record(self.candidate / "a.txt"), workspace_identity=identity))

    @unittest.skipUnless(os.name == "posix", "Windows pins the workspace against root replacement")
    def test_remove_checks_captured_root_identity_after_path_is_restored(self):
        self.opened_root_swap(lambda identity: candidate_module._atomic_remove(
            str(self.workspace), "a.txt", workspace_identity=identity))

    def windows_acl_conflict(self, changed_path):
        journal = self.crash_apply(all_changes=True)
        frozen = json.loads(journal.read_text(encoding="utf-8"))
        frozen["security_schema"] = 1
        current_security = {}
        for entry in frozen["entries"]:
            original = "original-security:" + entry["path"] if entry["before"] is not None else None
            entry["before_security"] = original
            entry["before_security_descriptor"] = "sealed-original-descriptor" if original else None
            entry["after_security"] = (original or "new-inherited-security") if entry["after"] is not None else None
            current_security[entry["path"]] = entry["after_security"]
        journal.write_text(json.dumps(frozen), encoding="utf-8")
        current_security[changed_path] = "later-user-security-only-edit"
        interrupted = build_manifest(self.workspace)
        original_os = os
        windows_os = SimpleNamespace(**{**vars(os), "name": "nt"})
        original_record = candidate_module.file_record
        original_target = candidate_module._verify_safe_target_path
        def validate_without_windows_kernel(root, relative):
            with mock.patch.object(candidate_module, "os", original_os):
                return original_target(root, relative)
        def inspect_without_windows_kernel(path):
            with mock.patch.object(candidate_module, "os", original_os):
                value = original_record(Path(path))
            if value is None:
                raise FileNotFoundError(path)
            return value
        def observed_security(path):
            return current_security[Path(path).relative_to(self.workspace).as_posix()]
        # Exercise the full Windows journal protocol against sealed real files;
        # native kernel DACL operations are covered separately on Windows CI.
        with mock.patch.object(candidate_module, "os", windows_os), \
             mock.patch("makewand.native_windows.pinned_directory", side_effect=lambda *_a, **_kw: contextlib.nullcontext()), \
             mock.patch("makewand.native_windows.validate_target", side_effect=validate_without_windows_kernel), \
             mock.patch("makewand.native_windows.inspect_file", side_effect=inspect_without_windows_kernel), \
             mock.patch("makewand.native_windows.application_security", side_effect=observed_security, create=True), \
             mock.patch("makewand.native_windows.validate_application_security_descriptor", create=True), \
             mock.patch.object(candidate_module, "_restore_application_entry") as restore:
            ok, _, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertFalse(ok, message)
        self.assertIn("file security changes", message)
        restore.assert_not_called()
        self.assertEqual(build_manifest(self.workspace), interrupted)
        self.assertEqual(json.loads(journal.read_text(encoding="utf-8"))["state"], "prepared")

    @unittest.skipUnless(os.name == "posix", "protocol stub complements native Windows DACL tests")
    def test_windows_existing_file_security_conflict_blocks_all_recovery(self):
        self.windows_acl_conflict("a.txt")

    @unittest.skipUnless(os.name == "posix", "protocol stub complements native Windows DACL tests")
    def test_windows_new_file_security_conflict_blocks_all_recovery(self):
        self.windows_acl_conflict("new.txt")

    @unittest.skipUnless(os.name == "posix", "protocol stub complements native Windows DACL tests")
    def test_legacy_windows_journal_without_security_stops_before_recovery(self):
        journal = self.crash_apply(all_changes=True)
        frozen = json.loads(journal.read_text(encoding="utf-8"))
        frozen.pop("security_schema", None)
        for entry in frozen["entries"]:
            entry.pop("before_security", None)
            entry.pop("after_security", None)
            entry.pop("before_security_descriptor", None)
        journal.write_text(json.dumps(frozen), encoding="utf-8")
        interrupted = build_manifest(self.workspace)
        windows_os = SimpleNamespace(**{**vars(os), "name": "nt"})
        with mock.patch.object(candidate_module, "os", windows_os), \
             mock.patch("makewand.native_windows.pinned_directory", side_effect=lambda *_a, **_kw: contextlib.nullcontext()), \
             mock.patch.object(candidate_module, "_restore_application_entry") as restore:
            ok, _, message = CandidateManager.recover_interrupted_applications(str(self.workspace))
        self.assertFalse(ok, message)
        self.assertIn("lacks frozen file security", message)
        restore.assert_not_called()
        self.assertEqual(build_manifest(self.workspace), interrupted)
        self.assertEqual(json.loads(journal.read_text(encoding="utf-8"))["state"], "prepared")


if __name__ == "__main__":
    unittest.main()
