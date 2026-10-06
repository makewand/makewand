"""Protected non-Git pipeline delivery uses real sealing and guarded apply."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import config, git_helper, orchestrator
from makewand.git_helper import create_ephemeral_shadow_worktree, run_git_cmd


@unittest.skipUnless(os.name == "posix", "POSIX sealed-patch delivery; Windows uses the existing native candidate transaction")
class ProtectedNonGitPipelineDeliveryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="makewand-non-git-delivery-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.host = self.root / "source"
        self.host.mkdir()
        (self.host / "app.py").write_text("VALUE = 0\n")
        (self.host / "keep.bin").write_bytes(b"original protected\x00bytes\n")
        self.smoke = "import unittest\nfrom app import VALUE\nclass Smoke(unittest.TestCase):\n    def test_value(self):\n        self.assertEqual(VALUE, 7)\n"
        (self.host / "test_smoke.py").write_text(self.smoke)
        self.artifacts = self.root / "artifacts"
        self.shadows = self.root / "shadows"
        self.enter = contextlib.ExitStack()
        self.addCleanup(self.enter.close)
        self.enter.enter_context(patch.object(config, "ARTIFACTS_DIR", self.artifacts))
        self.enter.enter_context(patch.object(config, "SHADOW_WORKTREES_DIR", self.shadows))
        self.protected = {name: ((self.host / name).read_bytes(), stat.S_IMODE((self.host / name).stat().st_mode))
                          for name in ("keep.bin", "test_smoke.py")}

    def prepare_delivery(self, approved=True):
        reviews = []
        def coder(prompt, cwd=None, **kwargs):
            self.assertNotEqual(Path(cwd), self.host)
            (Path(cwd) / "app.py").write_text("VALUE = 7\n")
            (Path(cwd) / "new.txt").write_text("reviewed new content\n")
            return True, "public fixture implemented", None

        def reviewer(prompt, **kwargs):
            reviews.append(kwargs)
            self.assertTrue(kwargs["readonly"])
            verdict = {"pass": approved, "defects": [] if approved else ["[P2] fixture review rejection"]}
            return True, "MAKEWAND_VERDICT: " + json.dumps(verdict), None

        # Provider responses alone are controlled; copy/Git baseline, actual
        # local Python tests, frozen destination, commit/tree/patch validation,
        # protected seals and apply script all execute their real code.
        with patch.object(orchestrator, "get_or_update_status", return_value={"claude": {"status": "healthy"}, "codex": {"status": "healthy"}}), \
             patch("makewand.usage.get_burn_rate_penalty", return_value=(0.0, None)), \
             patch.object(orchestrator, "execute_claude_task", side_effect=coder), \
             patch.object(orchestrator, "execute_codex_task", side_effect=reviewer), \
             contextlib.redirect_stdout(io.StringIO()):
            result = orchestrator.run_pipeline("Implement VALUE = 7 and save new.txt", cwd=str(self.host),
                force_code=True, forced_engine="claude", stream=False, auto_fix=False,
                workflow="pipeline", protected_paths=["keep.bin", "test_smoke.py"], timeout=30, total_budget=90)
        self.assertEqual(result, approved)
        self.assertEqual(len(reviews), 1)
        self.assertEqual((self.host / "app.py").read_text(), "VALUE = 0\n")
        self.assertFalse((self.host / "new.txt").exists())
        self.assertFalse((self.host / ".git").exists())
        deliveries = list(self.artifacts.glob("delivery_*/delivery_manifest.json"))
        if not approved:
            self.assertEqual(deliveries, [])
            self.assert_protection()
            return None
        self.assertEqual(len(deliveries), 1, "successful isolated pipeline must publish one sealed delivery")
        manifest_path = deliveries[0]
        manifest = json.loads(manifest_path.read_text())
        self.assertEqual(manifest["repo_root"], str(self.host))
        self.assertIsNone(manifest["delivered_branch"])
        self.assertIsNone(manifest["repo_head"])
        self.assertRegex(manifest["baseline_commit"], r"^[0-9a-f]{40}$")
        self.assertNotEqual(manifest["baseline_commit"], manifest["verified_commit"])
        self.assertEqual(manifest["protected_base_cwd"], str(self.host))
        self.assertEqual(set(manifest["protected_files"]["files"]), set(self.protected))
        self.assertEqual(manifest["destination_baseline"]["repositories"], {})
        self.assertEqual(set(manifest["expected_patch_changes"]), {"app.py", "new.txt"})
        self.assert_protection()
        return manifest_path.parent / "apply_delivery.sh"

    def assert_protection(self):
        for name, (content, mode) in self.protected.items():
            self.assertEqual((self.host / name).read_bytes(), content)
            self.assertEqual(stat.S_IMODE((self.host / name).stat().st_mode), mode)

    def apply(self, script):
        return subprocess.run(["bash", str(script)], capture_output=True, text=True, timeout=30, env=dict(os.environ))

    def test_reviewed_non_git_shadow_is_sealed_and_applies_without_host_git(self):
        script = self.prepare_delivery()
        result = self.apply(script)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.host / "app.py").read_text(), "VALUE = 7\n")
        self.assertEqual((self.host / "new.txt").read_text(), "reviewed new content\n")
        self.assertFalse((self.host / ".git").exists())
        self.assert_protection()

    def test_frozen_non_git_destination_conflict_blocks_apply_without_writes(self):
        script = self.prepare_delivery()
        (self.host / "app.py").write_text("external edit\n")
        before = {name: path.read_bytes() for name, path in ((p.name, p) for p in self.host.iterdir())}
        result = self.apply(script)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(before, {p.name: p.read_bytes() for p in self.host.iterdir()})
        self.assertFalse((self.host / ".git").exists())
        self.assert_protection()

    def test_standalone_shadow_returns_its_actual_baseline(self):
        result = create_ephemeral_shadow_worktree(str(self.host))
        self.assertIsNotNone(result[0])
        self.addCleanup(result[2])
        self.assertIsNone(result[1])
        code, actual, error = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=result.worktree_root)
        self.assertEqual(code, 0, error)
        self.assertEqual(result.baseline_commit, actual.strip())
        self.assertFalse((self.host / ".git").exists())

    def test_independent_review_rejection_cannot_publish_delivery(self):
        self.prepare_delivery(approved=False)
        self.assertEqual((self.host / "app.py").read_text(), "VALUE = 0\n")

    def test_unreadable_isolated_baseline_rejects_and_cleans_only_shadow(self):
        original = git_helper.run_git_cmd
        def reject_head(command, *args, **kwargs):
            if command == ["git", "rev-parse", "HEAD"]:
                return 1, "", "synthetic unreadable private baseline"
            return original(command, *args, **kwargs)
        with patch.object(git_helper, "run_git_cmd", side_effect=reject_head), contextlib.redirect_stderr(io.StringIO()):
            result = create_ephemeral_shadow_worktree(str(self.host))
        self.assertEqual(result, (None, None, None))
        self.assertEqual(list(self.shadows.iterdir()), [])
        self.assertEqual((self.host / "app.py").read_text(), "VALUE = 0\n")
        self.assertFalse((self.host / ".git").exists())
        self.assert_protection()


if __name__ == "__main__":
    unittest.main(verbosity=2)
