"""Portable checks for native pipeline sealing; Windows runtime is separate."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import tempfile
import unittest
from pathlib import Path

from makewand.artifact import workspace_snapshot
from makewand.candidate import CandidateManager
from makewand.git_helper import run_git_cmd
from makewand.native_delivery import create_native_shadow_worktree, prepare_native_delivery, seal_native_delivery
from makewand.protected_files import ProtectedFiles


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


if __name__ == "__main__":
    unittest.main()
