"""Real-Git delivery regressions: immutable reviewed commits and mixed tests."""

import contextlib
import io
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.orchestrator as orch
from makewand.git_helper import ShadowWorktreeResult, run_git_cmd
import makewand.config as makewand_config


def git(path, *args):
    code, out, error = run_git_cmd(["git", *args], cwd=str(path))
    if code:
        raise AssertionError((args, out, error))
    return out.strip()


class DeliveryBindingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="makewand-delivery-binding-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.base = self.root / "base"
        self.base.mkdir()
        git(self.base, "init")
        git(self.base, "config", "user.name", "Delivery regression")
        git(self.base, "config", "user.email", "test@example.invalid")
        (self.base / "app.txt").write_text("BASE\n")
        git(self.base, "add", "-A")
        git(self.base, "commit", "-m", "baseline")
        self.baseline = git(self.base, "rev-parse", "HEAD")
        self.shadow = self.root / "shadow"
        shutil.copytree(self.base, self.shadow)
        self.branch = "makewand/delivery-regression"
        self.reviewed = False
        self.artifacts_before = set(Path(makewand_config.ARTIFACTS_DIR).glob("delivery_*"))
        self.addCleanup(self.clean_artifacts)

    def delivery_artifacts(self):
        result = set()
        for path in set(Path(makewand_config.ARTIFACTS_DIR).glob("delivery_*")) - self.artifacts_before:
            manifest = path / "delivery_manifest.json"
            if manifest.exists() and json.loads(manifest.read_text()).get("repo_root") == str(self.base):
                result.add(path)
        return result

    def clean_artifacts(self):
        for path in self.delivery_artifacts():
            shutil.rmtree(path, ignore_errors=True)

    def run_pipeline(self, intercept=None):
        shadow = ShadowWorktreeResult(
            str(self.shadow), self.branch, lambda: None,
            baseline_commit=self.baseline, repo_head=self.baseline,
            repo_root=str(self.base), worktree_root=str(self.shadow))

        def dispatch(engine, prompt, cwd, readonly=False, **kwargs):
            if readonly:
                self.reviewed = True
                return True, 'MAKEWAND_VERDICT: {"pass": true, "defects": []}', None
            (Path(cwd) / "app.txt").write_text("APPROVED\n")
            return True, "implemented", None

        def git_intercept(command, **kwargs):
            if intercept:
                result = intercept(command, kwargs)
                if result is not None:
                    return result
            return run_git_cmd(command, **kwargs)

        with contextlib.ExitStack() as stack:
            for name, value in [
                ("check_load_backpressure", True),
                ("check_working_tree_isolation", (False, "controlled shadow fixture")),
                ("create_ephemeral_shadow_worktree", shadow),
                ("get_or_update_status", {}),
                ("select_optimal_engine_pair", (["codex"], ["claude"], {
                    "primary_coder": "codex", "primary_reviewer": "claude", "reasons": []})),
                ("run_local_tests", (True, None)),
            ]:
                stack.enter_context(patch.object(orch, name, return_value=value))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
            stack.enter_context(patch.object(orch, "run_git_cmd", side_effect=git_intercept))
            stack.enter_context(patch("makewand.memory.format_memory_hints_for_prompt", return_value=""))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            return orch.run_pipeline("Implement update", cwd=str(self.base), force_code=True, auto_fix=False)

    def assert_not_published(self):
        code, _, _ = run_git_cmd(["git", "rev-parse", "--verify", "refs/heads/" + self.branch], cwd=str(self.base))
        self.assertNotEqual(code, 0)

    def test_change_after_final_review_check_before_staging_is_rejected(self):
        mutated = False

        def intercept(command, kwargs):
            nonlocal mutated
            if self.reviewed and not mutated and command == ["git", "add", "-A"]:
                mutated = True
                (self.shadow / "app.txt").write_text("UNREVIEWED\n")

        self.assertFalse(self.run_pipeline(intercept))
        self.assertTrue(mutated)
        self.assert_not_published()

    def test_clean_commit_rewritten_during_commit_is_rejected(self):
        mutated = False

        def intercept(command, kwargs):
            nonlocal mutated
            if self.reviewed and not mutated and "commit" in command:
                result = run_git_cmd(command, **kwargs)
                mutated = True
                (self.shadow / "app.txt").write_text("UNREVIEWED\n")
                git(self.shadow, "add", "-A")
                git(self.shadow, "commit", "--amend", "--no-edit")
                self.assertEqual(git(self.shadow, "status", "--porcelain"), "")
                return result

        self.assertFalse(self.run_pipeline(intercept))
        self.assertTrue(mutated)
        self.assert_not_published()

    def test_hooks_cannot_change_delivery_and_fixed_commit_is_exported(self):
        hook = self.shadow / ".git/hooks/post-commit"
        hook.write_text("#!/bin/sh\nprintf 'UNREVIEWED\\n' > app.txt\n")
        hook.chmod(0o755)
        self.assertTrue(self.run_pipeline())
        self.assertEqual(git(self.base, "show", self.branch + ":app.txt"), "APPROVED")
        new_artifacts = self.delivery_artifacts()
        self.assertEqual(len(new_artifacts), 1)
        manifest = json.loads((next(iter(new_artifacts)) / "delivery_manifest.json").read_text())
        self.assertEqual(manifest["verified_commit"], git(self.base, "rev-parse", self.branch))
        self.assertEqual(manifest["verified_tree"], git(self.base, "rev-parse", self.branch + "^{tree}"))

    def test_moving_head_at_publication_cannot_replace_verified_commit(self):
        (self.shadow / "app.txt").write_text("UNREVIEWED\n")
        git(self.shadow, "add", "-A")
        git(self.shadow, "commit", "-m", "unreviewed alternate")
        alternate = git(self.shadow, "rev-parse", "HEAD")
        git(self.shadow, "reset", "--hard", self.baseline)
        moved = False

        def intercept(command, kwargs):
            nonlocal moved
            if "push" in command and not moved:
                moved = True
                git(self.shadow, "reset", "--hard", alternate)
                self.assertNotIn("HEAD:", command[-1])

        self.assertTrue(self.run_pipeline(intercept))
        self.assertTrue(moved)
        self.assertEqual(git(self.shadow, "show", "HEAD:app.txt"), "UNREVIEWED")
        self.assertEqual(git(self.base, "show", self.branch + ":app.txt"), "APPROVED")
        new_artifacts = self.delivery_artifacts()
        manifest = json.loads((next(iter(new_artifacts)) / "delivery_manifest.json").read_text())
        patch_text = Path(manifest["main_patch"]).read_text()
        self.assertIn("+APPROVED", patch_text)
        self.assertNotIn("UNREVIEWED", patch_text)

    def test_race_metadata_change_during_review_returns_unverified_without_traceback(self):
        import makewand.config as config
        candidates = self.root / "candidates"

        def dispatch(engine, prompt, cwd, **kwargs):
            (Path(cwd) / "app.txt").write_text("APPROVED\n")
            return True, "implemented", None

        def judge(*args, **kwargs):
            candidate = next(candidates.glob("*/agent_a"))
            git(candidate, "rm", "--cached", "app.txt")
            (candidate / ".git/info/exclude").write_text("app.txt\n")
            return True, 'MAKEWAND_RACE_VERDICT: {"pass": true, "winner": "A", "defects": []}', None

        with contextlib.ExitStack() as stack:
            for obj, name, value in [(orch, "CANDIDATES_DIR", candidates),
                                     (config, "CANDIDATES_DIR", candidates),
                                     (config, "CONFIG_DIR", self.root / "config")]:
                stack.enter_context(patch.object(obj, name, value))
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={}))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, None)))
            stack.enter_context(patch.object(orch, "execute_agy_task", side_effect=judge))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = orch.run_race("Implement update", cwd=str(self.base), engine_a="codex", engine_b="claude")
        self.assertEqual(code, orch.EXIT_UNVERIFIED)
        self.assertEqual(list(candidates.iterdir()), [])

    def test_verified_tree_preserves_binary_symlink_and_executable_inputs(self):
        script = self.shadow / "run tool.sh"
        script.write_text("#!/bin/sh\necho approved\n")
        script.chmod(0o755)
        (self.shadow / "data.bin").write_bytes(b"\x00\xffreviewed\n\x00")
        try:
            (self.shadow / "shortcut").symlink_to("app.txt")
        except OSError:
            self.skipTest("symlinks unavailable")
        snapshot = orch.workspace_snapshot(self.shadow)
        frozen = orch._freeze_delivery_inputs(str(self.shadow), snapshot)
        git(self.shadow, "add", "-A")
        git(self.shadow, "commit", "-m", "mixed reviewed inputs")
        commit = git(self.shadow, "rev-parse", "HEAD")
        tree = orch._verify_delivery_commit(str(self.shadow), commit, frozen[""], {})
        self.assertEqual(tree, git(self.shadow, "rev-parse", commit + "^{tree}"))

    def test_commit_blob_verification_rejects_git_metadata_deletion(self):
        snapshot = orch.workspace_snapshot(self.shadow)
        frozen = orch._freeze_delivery_inputs(str(self.shadow), snapshot)
        git(self.shadow, "rm", "--cached", "app.txt")
        (self.shadow / ".git/info/exclude").write_text("app.txt\n")
        git(self.shadow, "commit", "-m", "metadata-only deletion")
        commit = git(self.shadow, "rev-parse", "HEAD")
        self.assertEqual(orch.workspace_snapshot(self.shadow), snapshot)
        with self.assertRaisesRegex(OSError, "removed reviewed paths"):
            orch._verify_delivery_commit(str(self.shadow), commit, frozen[""], {})


class MixedTestDiscoveryTests(unittest.TestCase):
    def test_root_python_tests_are_not_hidden_by_node_tests_directory(self):
        with tempfile.TemporaryDirectory(prefix="makewand-mixed-tests-") as directory:
            root = Path(directory)
            git(root, "init")
            (root / ".gitignore").write_text("__pycache__/\n.pytest_cache/\n")
            (root / "tests").mkdir()
            (root / "tests/node.test.js").write_text("require('node:test')('ok', () => {});\n")
            (root / "test_python.py").write_text("def test_root():\n    assert True\n")
            commands = []

            def fixture_process(cmd, workspace, **kwargs):
                commands.append(cmd)
                result = subprocess.run(cmd, cwd=workspace, capture_output=True, text=True,
                                        env={**os.environ, **kwargs.get("extra_env", {})}, timeout=20)
                return result.returncode, result.stdout, result.stderr, None

            with patch("makewand.sandbox.run_in_sandbox", side_effect=fixture_process):
                passed, details = orch.run_local_tests(str(root))
            self.assertTrue(passed, details)
            self.assertIn("1 passed", details)
            self.assertNotEqual(commands[0][-1], "tests")
