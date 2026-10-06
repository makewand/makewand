"""Portable checks for native pipeline sealing; Windows runtime is separate."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import tempfile
import os
import contextlib
import shutil
import stat
import unittest
from pathlib import Path
from unittest import mock

from makewand.artifact import workspace_snapshot
from makewand.candidate import CandidateManager
from makewand.git_helper import run_git_cmd
from makewand.native_delivery import create_native_shadow_worktree, prepare_native_delivery, seal_native_delivery
from makewand.protected_files import ProtectedFiles
from makewand import config
from makewand.windows_paths import filesystem_path


class NativeDeliverySealingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="makewand-native-delivery-")
        self.addCleanup(self.temp.cleanup)
        self.host = Path(self.temp.name)
        (self.host / "app.txt").write_bytes(b"before\n")
        (self.host / "protected.txt").write_bytes(b"keep\n")

    def prepare(self, cwd=None):
        cwd = self.host if cwd is None else cwd
        protection = ProtectedFiles.capture(cwd, ["protected.txt"])
        shadow = create_native_shadow_worktree(str(cwd))
        self.addCleanup(shadow[2])
        capsule = prepare_native_delivery(str(cwd), shadow, protection)
        self.addCleanup(CandidateManager.discard_race, capsule["race_id"])
        return shadow, capsule

    def test_tested_reviewed_payload_is_deliverable_without_host_git(self):
        shadow, capsule = self.prepare()
        (capsule["source"] / "app.txt").write_bytes(b"after\n")
        reviewed = workspace_snapshot(capsule["source"])
        race_id = seal_native_delivery(capsule, "fixture", reviewed, "fixture review", "fixture test passed")
        self.assertEqual((self.host / "app.txt").read_bytes(), b"before\n")
        self.assertFalse((self.host / ".git").exists())
        ok, _, message = CandidateManager.apply_candidate(race_id, "A")
        self.assertTrue(ok, message)
        self.assertEqual((self.host / "app.txt").read_bytes(), b"after\n")
        self.assertEqual((self.host / "protected.txt").read_bytes(), b"keep\n")
        self.assertTrue(Path(shadow[0]).exists())

    def test_review_drift_is_rejected_before_saved_approval(self):
        _, capsule = self.prepare()
        (capsule["source"] / "app.txt").write_bytes(b"reviewed\n")
        reviewed = workspace_snapshot(capsule["source"])
        (capsule["source"] / "app.txt").write_bytes(b"unreviewed\n")
        with self.assertRaises(OSError):
            seal_native_delivery(capsule, "fixture", reviewed, "review", "test passed")
        self.assertIsNone(CandidateManager.get_race(capsule["race_id"]))
        self.assertEqual((self.host / "app.txt").read_bytes(), b"before\n")

    def test_candidate_git_configuration_never_enters_delivery_repository(self):
        _, capsule = self.prepare()
        code, _, error = run_git_cmd(["git", "config", "filter.candidate.clean", "candidate-command"], cwd=str(capsule["source"]))
        self.assertEqual(code, 0, error)
        (capsule["source"] / "app.txt").write_bytes(b"after\n")
        seal_native_delivery(capsule, "fixture", workspace_snapshot(capsule["source"]), "review", "test passed")
        code, _, _ = run_git_cmd(["git", "config", "--get", "filter.candidate.clean"], cwd=str(capsule["candidate"]))
        self.assertNotEqual(code, 0)

    def test_subdirectory_protection_rebases_to_repository_root(self):
        sub = self.host / "src"
        sub.mkdir()
        (sub / "protected.txt").write_bytes(b"nested keep\n")
        (sub / "app.txt").write_bytes(b"nested before\n")
        for argv in (["git", "init", "-q"], ["git", "config", "user.name", "Fixture"],
                     ["git", "config", "user.email", "fixture@example.invalid"], ["git", "add", "-A"],
                     ["git", "commit", "-qm", "baseline"]):
            code, _, error = run_git_cmd(argv, cwd=str(self.host))
            self.assertEqual(code, 0, error)
        shadow, capsule = self.prepare(sub)
        self.assertEqual(Path(shadow[0]).name, "src")
        (capsule["source"] / "src" / "app.txt").write_bytes(b"nested after\n")
        race_id = seal_native_delivery(capsule, "fixture", workspace_snapshot(capsule["source"]), "review", "test passed")
        metadata = CandidateManager.get_race(race_id)
        self.assertIn("src/protected.txt", metadata["protected_files"]["files"])
        ok, _, message = CandidateManager.apply_candidate(race_id, "A")
        self.assertTrue(ok, message)
        self.assertEqual((sub / "app.txt").read_bytes(), b"nested after\n")
        self.assertEqual((sub / "protected.txt").read_bytes(), b"nested keep\n")

    def test_deep_shadow_clone_keeps_ignore_rules_exact_delivery_and_private_cleanup(self):
        # Git for Windows cannot enter a >MAX_PATH canonical root. Exercise
        # supported roots with genuine >310-character source/delivery files.
        deep_root = self.host / "deep"
        source = deep_root / "workspace"
        Path(filesystem_path(source)).mkdir(parents=True)
        self.assertLess(len(str(source)), 248)
        nested = Path("nested")
        while len(str(source / nested / "app.txt")) <= 310:
            nested /= "long-path-component-0123456789"
        app = nested / "app.txt"
        protected = nested / "protected.txt"
        ignored = nested / ".env"
        self.assertGreater(len(str(source / app)), 310)
        self.assertGreater(len(str(source / protected)), 310)
        Path(filesystem_path(source / nested)).mkdir(parents=True)
        for name, payload in {app: b"before\r\n", protected: b"keep\r\n",
                              Path(".gitignore"): b".env\n", ignored: b"fixture-secret\n"}.items():
            Path(filesystem_path(source / name)).write_bytes(payload)
        shadow = None
        capsule = None
        try:
            with mock.patch.object(config, "SHADOW_WORKTREES_DIR", deep_root / "shadow-state"):
                protection = ProtectedFiles.capture(source, [protected.as_posix()])
                shadow = create_native_shadow_worktree(str(source))
                self.assertGreater(len(str(Path(shadow[0]) / app)), 310)
                self.assertLessEqual(len(Path(shadow.worktree_root).name), 12)
                self.assertFalse(str(shadow.worktree_root).startswith("\\\\?\\"))
                self.assertFalse(Path(filesystem_path(Path(shadow[0]) / ignored)).exists())
                capsule = prepare_native_delivery(str(source), shadow, protection)
                Path(filesystem_path(capsule["source"] / app)).write_bytes(b"after\r\n")
                race_id = seal_native_delivery(capsule, "fixture", workspace_snapshot(capsule["source"]),
                                              "independent fixture review", "fixture test passed")
                ok, _, message = CandidateManager.apply_candidate(race_id, "A")
                self.assertTrue(ok, message)
                self.assertEqual(Path(filesystem_path(source / app)).read_bytes(), b"after\r\n")
                self.assertEqual(Path(filesystem_path(source / protected)).read_bytes(), b"keep\r\n")
                self.assertEqual(Path(filesystem_path(source / ignored)).read_bytes(), b"fixture-secret\n")
                shadow[2]()
                self.assertFalse(Path(filesystem_path(shadow.worktree_root)).exists())
        finally:
            if capsule is not None:
                CandidateManager.discard_race(capsule["race_id"])
            if shadow is not None:
                shadow[2]()
            for child in Path(filesystem_path(deep_root)).rglob("*"):
                if child.is_file():
                    child.chmod(stat.S_IWRITE)
            shutil.rmtree(filesystem_path(deep_root))

    def test_overlong_git_root_is_refused_before_target_creation_or_copy(self):
        from makewand.git_helper import clone_isolated_worktree
        deep_root = self.host / "refused-deep-root"
        source = deep_root
        while len(str(source)) <= 310:
            source /= "long-path-component-0123456789"
        Path(filesystem_path(source)).mkdir(parents=True)
        original = b"complete original protected bytes"
        Path(filesystem_path(source / "protected.txt")).write_bytes(original)
        target = self.host / "must-not-be-created"
        try:
            with contextlib.ExitStack() as contexts:
                if os.name != "nt":
                    # Linux checks refusal ordering with an unavailable
                    # capability; the Windows gate calls the actual policy.
                    contexts.enter_context(mock.patch("makewand.git_helper.os.name", "nt"))
                    contexts.enter_context(mock.patch("makewand.git_helper.windows_git_directory",
                        side_effect=OSError("Git for Windows requires a canonical workspace root shorter than 260 UTF-16 code units")))
                with self.assertRaisesRegex(OSError, "canonical workspace root"):
                    clone_isolated_worktree(str(source), target)
            self.assertFalse(target.exists())
            self.assertEqual(Path(filesystem_path(source / "protected.txt")).read_bytes(), original)
        finally:
            shutil.rmtree(filesystem_path(deep_root))


if __name__ == "__main__":
    unittest.main()
