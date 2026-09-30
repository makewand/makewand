"""Saved task constraints cannot be bypassed by candidate selection or force."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import stat
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import candidate, config
from makewand.candidate import CandidateManager, build_manifest
from makewand.git_helper import clone_isolated_worktree, ensure_git_worktree, run_git_cmd
from makewand.protected_files import ProtectedFiles, ProtectionError


@unittest.skipUnless(os.name == "posix" and hasattr(os, "O_NOFOLLOW"), "POSIX protection contract")
class ProtectedCandidateTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="protected-candidate-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        for name, directory in (("CONFIG_DIR", "config"), ("CANDIDATES_DIR", "config/candidates"),
                                ("BACKUPS_DIR", "config/backups")):
            override = patch.object(config, name, self.root / directory)
            override.start()
            self.addCleanup(override.stop)
        config.ensure_config_dir()
        self.base = self.root / "workspace"
        self.base.mkdir()
        self.assertTrue(ensure_git_worktree(str(self.base)))
        self.source = "VALUE = 1\n\ndef left(): return 1\ndef right(): return 2\n"
        (self.base / "module.py").write_text(self.source)
        (self.base / "other.txt").write_text("original\n")
        (self.base / "test_module.py").write_text("def test_valid():\n    assert True\n")
        (self.base / "locked.txt").write_text("frozen task input\n")
        (self.base / "locked.txt").chmod(0o640)
        (self.base / "extra.txt").write_text("explicit extra input\n")
        for command in (["git", "add", "-A"], ["git", "commit", "-m", "protected fixture"]):
            self.assertEqual(run_git_cmd(command, cwd=str(self.base))[0], 0)
        self.baseline = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(self.base))[1].strip()
        self.guard = ProtectedFiles.capture(self.base, ["locked.txt"])
        race_dir = config.CANDIDATES_DIR / "protected"
        race_dir.mkdir(parents=True)
        self.a, self.b, self.frozen = [race_dir / name for name in ("A", "B", "baseline")]
        for path in (self.a, self.b, self.frozen):
            clone_isolated_worktree(str(self.base), path)
            self.guard.prepare_workspace(path)
        (self.a / "module.py").write_text(self.source.replace("left(): return 1", "left(): return 10"))
        (self.a / "other.txt").write_text("updated\n")
        (self.b / "module.py").write_text(self.source.replace("right(): return 2", "right(): return 20"))

    def save(self, *, policy=True):
        agents = []
        for label, path in (("A", self.a), ("B", self.b)):
            revision = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(path))[1].strip()
            agents.append({"model": label, "path": str(path), "baseline_commit": revision,
                           "success": True, "test_passed": True, "review_passed": True})
        kwargs = {"baseline_dir": self.frozen, "baseline_manifest": build_manifest(self.base),
                  "frozen_baseline_manifest": build_manifest(self.frozen)}
        if policy is not False:
            kwargs["protected_files"] = self.guard.to_dict() if policy is True else policy
        return CandidateManager.save_race("protected", "preserve task inputs", str(self.base),
                                          self.baseline, *agents, **kwargs)

    def replace_meta(self, race):
        candidate._write_private_json(config.CANDIDATES_DIR / "protected/meta.json", race)

    def assert_blocked(self, label="A", **options):
        before = build_manifest(self.base)
        with patch.object(candidate, "_atomic_copy") as copy, \
             patch.object(candidate, "_atomic_remove") as remove:
            success, changed, message = CandidateManager.apply_candidate("protected", label, **options)
        self.assertFalse(success, message)
        self.assertEqual(changed, [])
        self.assertIn("受保护", message)
        self.assertEqual(message.status, "UNVERIFIED")
        copy.assert_not_called()
        remove.assert_not_called()
        self.assertEqual(build_manifest(self.base), before)
        self.assertFalse(CandidateManager.get_race("protected").get("applied_at"))

    def synthesize(self, *, side_effect=None):
        with patch("makewand.orchestrator.run_local_tests", return_value=(True, "stub tests passed"),
                   side_effect=side_effect):
            return CandidateManager.create_hybrid_candidate("protected")

    def test_save_freezes_policy_and_legacy_omits_declaration(self):
        policy = self.guard.to_dict()
        self.save(policy=policy)
        policy["files"]["locked.txt"]["mode"] = 0
        self.assertEqual(CandidateManager.get_race("protected")["protected_files"], self.guard.to_dict())
        self.save(policy=False)
        self.assertNotIn("protected_files", CandidateManager.get_race("protected"))
        with patch.dict(os.environ):
            os.environ.pop("MAKEWAND_TASK_PROTECTED_PATHS", None)
            self.assertTrue(CandidateManager.apply_candidate("protected", "A", dry_run=True)[0])

    def test_environment_only_apply_constraints_and_explicit_empty_override(self):
        (self.a / "extra.txt").write_text("candidate changed env-protected input\n")
        self.save(policy=False)
        with patch.dict(os.environ, {"MAKEWAND_TASK_PROTECTED_PATHS": '["extra.txt"]'}):
            self.assert_blocked(force=True)
            success, changed, message = CandidateManager.apply_candidate("protected", "A", dry_run=True,
                                                                          protected_paths=[])
        self.assertTrue(success, message)
        self.assertTrue(changed)

    def test_invalid_policy_fails_closed_before_save_or_apply_even_with_force(self):
        with self.assertRaises(ProtectionError):
            self.save(policy={"schema": 1, "files": {"locked.txt": {"sha256": "bad", "mode": 0o640}}})
        self.save()
        race = CandidateManager.get_race("protected")
        for policy in (None, {}, {"schema": 1, "files": [], "ignored": True}):
            race["protected_files"] = policy
            self.replace_meta(race)
            self.assert_blocked(force=True)
            self.assert_blocked(force=True, dry_run=True)

    def test_sealed_a_and_b_content_violations_block_force_and_preview_without_partial_writes(self):
        (self.a / "locked.txt").write_text("A ignored constraints\n")
        (self.b / "locked.txt").write_text("B ignored constraints\n")
        self.save()
        for label in ("A", "B"):
            for dry_run in (False, True):
                self.assert_blocked(label, force=True, dry_run=dry_run)
        self.assertEqual((self.base / "other.txt").read_text(), "original\n")

    def test_sealed_permission_change_cannot_be_forced(self):
        (self.a / "locked.txt").chmod(0o600)
        self.save()
        self.assert_blocked(force=True)
        self.assert_blocked(dry_run=True)
        self.assertEqual(stat.S_IMODE((self.base / "locked.txt").stat().st_mode), 0o640)

    def test_destination_drift_is_independent_of_candidate_change_plan(self):
        self.save()
        (self.base / "locked.txt").write_text("external destination drift\n")
        self.assert_blocked(force=True)
        self.assert_blocked(force=True, dry_run=True)
        self.assertEqual((self.base / "locked.txt").read_text(), "external destination drift\n")

    def test_protected_symlink_substitution_does_not_touch_external_destination(self):
        self.save()
        external = self.root / "external"
        external.write_text("frozen task input\n")
        external.chmod(0o640)
        (self.a / "locked.txt").unlink()
        (self.a / "locked.txt").symlink_to(external)
        self.assert_blocked(force=True)
        self.assertEqual(external.read_text(), "frozen task input\n")

    def test_explicit_extra_paths_add_constraints_to_saved_policy(self):
        (self.a / "extra.txt").write_text("candidate changed extra\n")
        self.save()
        self.assert_blocked(force=True, protected_paths=["extra.txt"])
        self.assert_blocked(force=True, dry_run=True, protected_paths=["extra.txt"])
        (self.b / "locked.txt").write_text("candidate changed saved input\n")
        self.save()
        self.assert_blocked("B", force=True, protected_paths=["extra.txt"])
        self.assert_blocked("B", force=True, protected_paths=[])

    def test_invalid_extra_path_fails_before_any_application_write(self):
        self.save()
        self.assert_blocked(force=True, protected_paths=["../outside"])
        self.assert_blocked(force=True, protected_paths=["missing"])

    def test_guarded_application_changes_only_allowed_files(self):
        self.save()
        success, changed, message = CandidateManager.apply_candidate("protected", "A", protected_paths=["extra.txt"])
        self.assertTrue(success, message)
        self.assertTrue(changed)
        self.assertEqual((self.base / "other.txt").read_text(), "updated\n")
        self.assertTrue(self.guard.verify(self.base))

    def test_drift_during_conflict_detection_is_rechecked_before_backup_or_preview(self):
        self.save()
        def drift(*args, **kwargs):
            (self.base / "locked.txt").write_text("drift during conflict check\n")
            return []
        for dry_run in (False, True):
            (self.base / "locked.txt").write_text("frozen task input\n")
            with patch.object(CandidateManager, "detect_conflicts", side_effect=drift), \
                 patch.object(candidate, "_atomic_copy") as copy:
                success, changed, message = CandidateManager.apply_candidate("protected", "A", dry_run=dry_run)
            self.assertFalse(success)
            self.assertEqual(changed, [])
            self.assertEqual(message.status, "UNVERIFIED")
            copy.assert_not_called()
            self.assertEqual((self.base / "locked.txt").read_text(), "drift during conflict check\n")
            self.assertEqual((self.base / "other.txt").read_text(), "original\n")
        self.assertFalse(list(config.BACKUPS_DIR.glob("protected_*")))

    def test_deadline_status_from_hashing_is_retained(self):
        self.save()
        with patch("makewand.protected_files._deadline", side_effect=ProtectionError("expired", status="TIMEOUT")):
            success, changed, message = CandidateManager.apply_candidate("protected", "A", force=True)
        self.assertFalse(success)
        self.assertEqual(changed, [])
        self.assertEqual(message.status, "TIMEOUT")

    def test_external_drift_after_first_write_rolls_back_other_files(self):
        self.save()
        original = candidate._atomic_copy
        copies = []
        def mutate_after_copy(*args, **kwargs):
            result = original(*args, **kwargs)
            copies.append(args[1])
            if len(copies) == 1:
                (self.base / "locked.txt").write_text("external change during apply\n")
            return result
        with patch.object(candidate, "_atomic_copy", side_effect=mutate_after_copy), \
             patch.object(CandidateManager, "_mark_applied") as mark:
            success, changed, message = CandidateManager.apply_candidate("protected", "A", force=True)
        self.assertFalse(success)
        self.assertEqual(changed, [])
        self.assertEqual(message.status, "UNVERIFIED")
        self.assertGreaterEqual(len(copies), 2)
        mark.assert_not_called()
        self.assertEqual((self.base / "module.py").read_text(), self.source)
        self.assertEqual((self.base / "other.txt").read_text(), "original\n")
        self.assertEqual((self.base / "locked.txt").read_text(), "external change during apply\n")

    def test_hybrid_prepares_fresh_clone_modes_and_preserves_saved_policy_on_apply(self):
        self.save()
        ok, merged, message = self.synthesize()
        self.assertTrue(ok, message)
        self.assertTrue(self.guard.verify(merged["path"]))
        self.assertEqual(stat.S_IMODE((Path(merged["path"]) / "locked.txt").stat().st_mode), 0o640)
        verdict = 'MAKEWAND_VERDICT: {"pass": true, "defects": []}'
        approved, message = CandidateManager.approve_hybrid_candidate("protected", merged["manifest"],
                                                                    merged["changes"], verdict)
        self.assertTrue(approved, message)
        success, _, message = CandidateManager.apply_candidate("protected", "M")
        self.assertTrue(success, message)
        self.assertTrue(self.guard.verify(self.base))

    def test_hybrid_merge_mutation_is_rejected_before_tests_and_sealing(self):
        self.save()
        from makewand.merger import semantic_merge_candidate_worktrees as merge
        def corrupt(*args, **kwargs):
            result = merge(*args, **kwargs)
            (Path(args[3]) / "locked.txt").write_text("merge violated constraints\n")
            return result
        with patch("makewand.merger.semantic_merge_candidate_worktrees", side_effect=corrupt), \
             patch("makewand.orchestrator.run_local_tests") as tests:
            ok, merged, message = CandidateManager.create_hybrid_candidate("protected")
        self.assertFalse(ok)
        self.assertIsNone(merged)
        self.assertIn("受保护", message)
        tests.assert_not_called()
        self.assertNotIn("M", CandidateManager.get_race("protected")["candidates"])
        self.assertTrue(self.guard.verify(self.base))

    def test_hybrid_tests_chmod_violation_is_rejected_before_registration(self):
        self.save()
        def mutate(cwd, **kwargs):
            (Path(cwd) / "locked.txt").chmod(0o600)
            return True, "test succeeded but changed permissions"
        ok, merged, message = self.synthesize(side_effect=mutate)
        self.assertFalse(ok)
        self.assertIsNone(merged)
        self.assertIn("受保护", message)
        self.assertNotIn("M", CandidateManager.get_race("protected")["candidates"])
        self.assertTrue(self.guard.verify(self.base))

    def test_hybrid_review_and_force_apply_reject_resealed_protected_violation(self):
        self.save()
        self.assertTrue(self.synthesize()[0])
        race = CandidateManager.get_race("protected")
        merged = race["candidates"]["M"]
        path = Path(merged["path"])
        (path / "locked.txt").write_text("review changed task input\n")
        # Rebinding ordinary evidence cannot weaken the separately saved policy.
        merged["manifest"] = build_manifest(path)
        merged["input_manifest"] = candidate._input_manifest(path)
        merged["changes"] = candidate.get_candidate_files_changed(path, merged["baseline_commit"])
        self.replace_meta(race)
        verdict = 'MAKEWAND_VERDICT: {"pass": true, "defects": []}'
        approved, message = CandidateManager.approve_hybrid_candidate("protected", merged["manifest"],
                                                                    merged["changes"], verdict)
        self.assertFalse(approved)
        self.assertIn("受保护", message)
        self.assert_blocked("M", force=True)
        self.assert_blocked("M", force=True, dry_run=True)

    def test_hybrid_destination_drift_prevents_generation(self):
        self.save()
        (self.base / "locked.txt").chmod(0o600)
        with patch("makewand.git_helper.clone_isolated_worktree") as clone:
            ok, merged, message = self.synthesize()
        self.assertFalse(ok)
        self.assertIsNone(merged)
        self.assertIn("受保护", message)
        clone.assert_not_called()

    def test_hybrid_a_hash_timeout_status_survives_seal_error_forwarding(self):
        self.save()
        original = ProtectedFiles.verify
        def expire_on_a(guard, workspace):
            if Path(workspace) == self.a:
                raise ProtectionError("hash deadline expired", status="TIMEOUT")
            return original(guard, workspace)
        with patch.object(ProtectedFiles, "verify", new=expire_on_a):
            ok, merged, message = self.synthesize()
        self.assertFalse(ok)
        self.assertIsNone(merged)
        self.assertEqual(message.status, "TIMEOUT")

    def test_existing_hybrid_hash_timeout_status_survives_apply_forwarding(self):
        self.save()
        ok, merged, message = self.synthesize()
        self.assertTrue(ok, message)
        original = ProtectedFiles.verify
        def expire_on_m(guard, workspace):
            if Path(workspace) == Path(merged["path"]):
                raise ProtectionError("hash deadline expired", status="TIMEOUT")
            return original(guard, workspace)
        with patch.object(ProtectedFiles, "verify", new=expire_on_m):
            ok, _, message = CandidateManager.create_hybrid_candidate("protected")
            self.assertFalse(ok)
            self.assertEqual(message.status, "TIMEOUT")
            ok, changed, message = CandidateManager.apply_candidate("protected", "M", force=True)
        self.assertFalse(ok)
        self.assertEqual(changed, [])
        self.assertEqual(message.status, "TIMEOUT")

    def test_save_baseline_hash_timeout_is_not_converted_to_inspectable_failure(self):
        original = ProtectedFiles.verify
        def expire_on_baseline(guard, workspace):
            if Path(workspace) == self.frozen:
                raise ProtectionError("hash deadline expired", status="TIMEOUT")
            return original(guard, workspace)
        with patch.object(ProtectedFiles, "verify", new=expire_on_baseline):
            with self.assertRaises(ProtectionError) as caught:
                self.save()
        self.assertEqual(caught.exception.status, "TIMEOUT")
        self.assertIsNone(CandidateManager.get_race("protected"))


if __name__ == "__main__":
    unittest.main()
