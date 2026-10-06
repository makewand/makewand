"""Real-Git delivery regressions: immutable reviewed commits and mixed tests."""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
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
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.orchestrator as orch
from makewand.git_helper import ShadowWorktreeResult, run_git_cmd
import makewand.config as makewand_config
from makewand.delivery import capture_delivery_baseline, delivery_shell_guard


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
        self.sub_baselines = {}
        self.reviewed = False
        self.artifacts_root = _isolation.artifacts_root()
        self.artifacts_before = set(self.artifacts_root.glob("delivery_*"))
        self.addCleanup(self.clean_artifacts)

    def delivery_artifacts(self):
        result = set()
        for path in set(self.artifacts_root.glob("delivery_*")) - self.artifacts_before:
            manifest = path / "delivery_manifest.json"
            if manifest.exists() and json.loads(manifest.read_text()).get("repo_root") == str(self.base):
                result.add(path)
        return result

    def clean_artifacts(self):
        for path in self.delivery_artifacts():
            shutil.rmtree(path, ignore_errors=True)

    def refresh_baseline(self):
        git(self.base, "add", "-A")
        git(self.base, "commit", "-m", "fixture baseline")
        self.baseline = git(self.base, "rev-parse", "HEAD")
        shutil.rmtree(self.shadow)
        shutil.copytree(self.base, self.shadow)

    def apply_delivery(self, env=None):
        outputs = self.delivery_artifacts()
        self.assertEqual(len(outputs), 1)
        script = next(iter(outputs)) / "apply_delivery.sh"
        return subprocess.run(["bash", str(script)], capture_output=True, text=True,
                              timeout=30, env=env, cwd="/")

    def assert_drift_rejected(self):
        changed = capture_delivery_baseline(self.base)
        result = self.apply_delivery()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("Delivery destination check failed", result.stderr)
        self.assertEqual(capture_delivery_baseline(self.base), changed,
                         "preflight rejection must not modify the user's current state")

    def run_pipeline(self, intercept=None, coder=None, shadow_hook=None, **pipeline_options):
        shadow = ShadowWorktreeResult(
            str(self.shadow), self.branch, lambda: None,
            baseline_commit=self.baseline, repo_head=git(self.base, "rev-parse", "HEAD"),
            repo_root=str(self.base), worktree_root=str(self.shadow),
            sub_baselines=self.sub_baselines)

        def dispatch(engine, prompt, cwd, readonly=False, **kwargs):
            if readonly:
                self.reviewed = True
                return True, 'MAKEWAND_VERDICT: {"pass": true, "defects": []}', None
            if coder is None:
                (Path(cwd) / "app.txt").write_text("APPROVED\n")
            else:
                coder(Path(cwd))
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
                ("run_local_tests", (True, "fixture tests passed")),
            ]:
                if name == "create_ephemeral_shadow_worktree" and shadow_hook is not None:
                    def create_shadow(*args, **kwargs):
                        shadow_hook()
                        return shadow
                    stack.enter_context(patch.object(orch, name, side_effect=create_shadow))
                else:
                    stack.enter_context(patch.object(orch, name, return_value=value))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
            stack.enter_context(patch.object(orch, "run_git_cmd", side_effect=git_intercept))
            stack.enter_context(patch("makewand.memory.format_memory_hints_for_prompt", return_value=""))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            return orch.run_pipeline("Implement update", cwd=str(self.base), force_code=True, auto_fix=False,
                                     **pipeline_options)

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

    def test_race_index_changes_cannot_hide_reviewed_files(self):
        import makewand.config as config
        candidates = self.root / "candidates"
        (self.base / "test_app.py").write_text("def test_fixture():\n    assert True\n")

        def dispatch(engine, prompt, cwd, readonly=False, **kwargs):
            if readonly:
                return judge(engine, prompt, cwd=cwd, **kwargs)
            (Path(cwd) / "app.txt").write_text("APPROVED\n")
            return True, "implemented", None

        judge_calls = []
        def judge(*args, **kwargs):
            judge_calls.append((args, kwargs))
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
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, "fixture tests passed")))
            stack.enter_context(patch.object(orch, "execute_agy_task", side_effect=AssertionError("race judging must use the unified dispatcher")))
            stack.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = orch.run_race("Implement update", cwd=str(self.base), engine_a="codex", engine_b="claude")
        # Inspection now constructs its own index from the frozen baseline.
        # Removing a real index entry cannot hide the reviewed application plan.
        self.assertEqual(code, 0)
        self.assertEqual(len(judge_calls), 1)
        race = json.loads(next(candidates.glob("*/meta.json")).read_text())
        self.assertEqual(race["candidates"]["A"]["changes"]["app.txt"], "M")
        self.assertIn("app.txt", race["candidates"]["A"]["manifest"])

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

    def test_apply_rejects_far_from_hunk_edit_without_changing_any_file(self):
        lines = [f"line {index}\n" for index in range(30)]
        (self.base / "app.txt").write_text("".join(lines))
        self.refresh_baseline()

        def coder(path):
            reviewed = list(lines)
            reviewed[0] = "APPROVED\n"
            (path / "app.txt").write_text("".join(reviewed))

        self.assertTrue(self.run_pipeline(coder=coder))
        lines[-1] = "USER CHANGE AFTER DELIVERY\n"
        (self.base / "app.txt").write_text("".join(lines))
        self.assert_drift_rejected()
        self.assertEqual((self.base / "app.txt").read_text(), "".join(lines))

    def test_apply_rejects_nonpatch_file_edits(self):
        (self.base / "keep.txt").write_text("original\n")
        self.refresh_baseline()
        self.assertTrue(self.run_pipeline())
        (self.base / "keep.txt").write_text("user edit\n")
        self.assert_drift_rejected()

    def test_apply_rejects_new_untracked_files(self):
        self.assertTrue(self.run_pipeline())
        (self.base / "new.txt").write_text("user file\n")
        self.assert_drift_rejected()

    def test_apply_rejects_ignored_cache_file_edits(self):
        (self.base / ".gitignore").write_text(".cache/\n")
        cache = self.base / ".cache"
        cache.mkdir()
        (cache / "state").write_text("original ignored input\n")
        self.refresh_baseline()
        self.assertTrue(self.run_pipeline())
        (cache / "state").write_text("changed ignored input\n")
        self.assert_drift_rejected()

    def test_apply_rejects_permission_only_drift(self):
        self.assertTrue(self.run_pipeline())
        (self.base / "app.txt").chmod(0o600)
        self.assert_drift_rejected()

    def test_apply_rejects_new_empty_directories(self):
        self.assertTrue(self.run_pipeline())
        (self.base / "user-directory").mkdir()
        self.assert_drift_rejected()

    def test_apply_rejects_head_change_with_identical_files(self):
        self.assertTrue(self.run_pipeline())
        git(self.base, "commit", "--allow-empty", "-m", "user advanced HEAD")
        self.assert_drift_rejected()

    def test_apply_rejects_branch_switch_to_same_commit(self):
        self.assertTrue(self.run_pipeline())
        git(self.base, "checkout", "-b", "user-branch")
        self.assert_drift_rejected()

    def test_apply_rejects_recreated_destination_root(self):
        self.assertTrue(self.run_pipeline())
        previous = self.root / "previous-base"
        self.base.rename(previous)
        shutil.copytree(previous, self.base)
        self.assert_drift_rejected()
        self.assertEqual((previous / "app.txt").read_text(), "BASE\n")

    def test_apply_rejects_drift_during_generation_before_publication(self):
        def coder(path):
            (path / "app.txt").write_text("APPROVED\n")
            (self.base / "user.txt").write_text("created while generating\n")

        self.assertFalse(self.run_pipeline(coder=coder))
        self.assert_not_published()
        self.assertEqual((self.base / "app.txt").read_text(), "BASE\n")
        self.assertEqual((self.base / "user.txt").read_text(), "created while generating\n")

    def test_drift_while_copying_shadow_is_rejected_before_dispatch(self):
        calls = []

        def during_copy():
            (self.base / "app.txt").write_text("USER SAVE DURING SHADOW COPY\n")

        self.assertFalse(self.run_pipeline(coder=lambda path: calls.append(path), shadow_hook=during_copy))
        self.assertEqual(calls, [])
        self.assertEqual((self.base / "app.txt").read_text(), "USER SAVE DURING SHADOW COPY\n")
        self.assert_not_published()

    def test_apply_normal_standalone_binary_symlink_and_executable_delivery(self):
        script = self.base / "run tool.sh"
        script.write_text("#!/bin/sh\necho before\n")
        script.chmod(0o755)
        (self.base / "data.bin").write_bytes(b"\x00\xffbefore\x00")
        (self.base / "keep.txt").write_text("unchanged\n")
        (self.base / "keep.txt").chmod(0o664)
        (self.base / "shortcut").symlink_to("app.txt")
        self.refresh_baseline()

        def coder(path):
            (path / "app.txt").write_text("APPROVED\n")
            (path / "run tool.sh").write_text("#!/bin/sh\necho after\n")
            (path / "data.bin").write_bytes(b"\x00\xffafter\x00")
            (path / "shortcut").unlink()
            (path / "shortcut").symlink_to("keep.txt")
            (path / "new-directory").mkdir()
            (path / "new-directory" / "new.txt").write_text("new reviewed file\n")

        self.assertTrue(self.run_pipeline(coder=coder))
        env = dict(os.environ, PYTHONPATH="/makewand-sdk-is-not-installed")
        result = self.apply_delivery(env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.base / "app.txt").read_text(), "APPROVED\n")
        self.assertEqual((self.base / "data.bin").read_bytes(), b"\x00\xffafter\x00")
        self.assertEqual(os.readlink(self.base / "shortcut"), "keep.txt")
        self.assertTrue(stat.S_IMODE(script.stat().st_mode) & 0o100)
        self.assertEqual(stat.S_IMODE((self.base / "keep.txt").stat().st_mode), 0o664)

    def test_apply_preserves_original_dirty_baseline(self):
        # The shadow baseline contains user changes which are not in host HEAD.
        (self.base / "user.txt").write_text("user baseline\n")
        (self.shadow / "user.txt").write_text("user baseline\n")
        git(self.shadow, "add", "-A")
        git(self.shadow, "commit", "-m", "dirty baseline snapshot")
        self.baseline = git(self.shadow, "rev-parse", "HEAD")
        self.assertTrue(self.run_pipeline())
        result = self.apply_delivery()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.base / "app.txt").read_text(), "APPROVED\n")
        self.assertEqual((self.base / "user.txt").read_text(), "user baseline\n")

    def test_apply_ignores_callers_git_redirection_and_config_injection(self):
        other = self.root / "other"
        shutil.copytree(self.base, other)
        self.assertTrue(self.run_pipeline())
        env = dict(os.environ, GIT_DIR=str(other / ".git"), GIT_WORK_TREE=str(other),
                   GIT_INDEX_FILE=str(other / ".git/index"), GIT_COMMON_DIR=str(other / ".git"),
                   GIT_CONFIG_COUNT="1", GIT_CONFIG_KEY_0="core.worktree",
                   GIT_CONFIG_VALUE_0=str(other))
        result = self.apply_delivery(env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.base / "app.txt").read_text(), "APPROVED\n")
        self.assertEqual((other / "app.txt").read_text(), "BASE\n")

    def test_sdk_configured_snapshot_seconds_are_clamped_to_total_deadline(self):
        with patch.dict(os.environ, {"MAKEWAND_DELIVERY_MAX_SECONDS": "60"}):
            with patch("makewand.delivery.capture_delivery_baseline", wraps=capture_delivery_baseline) as capture:
                self.assertTrue(self.run_pipeline(total_timeout=20))
        first = capture.call_args_list[0].kwargs
        self.assertEqual(first["limits"]["max_seconds"], 60)
        self.assertGreater(first["timeout"], 10)
        self.assertLessEqual(first["timeout"], 20)
        manifest = json.loads((next(iter(self.delivery_artifacts())) / "delivery_manifest.json").read_text())
        self.assertEqual(manifest["destination_baseline"]["snapshot_limits"]["max_seconds"], 60)

    def test_generated_script_uses_frozen_budget_despite_caller_override(self):
        with patch.dict(os.environ, {"MAKEWAND_DELIVERY_MAX_BYTES": "64"}):
            self.assertTrue(self.run_pipeline())
        (self.base / "cache.bin").write_bytes(b"x" * 1024)
        result = self.apply_delivery(env=dict(os.environ, MAKEWAND_DELIVERY_MAX_BYTES=str(1024 ** 3)))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("max_bytes limit (64;", result.stderr)
        self.assertEqual((self.base / "app.txt").read_text(), "BASE\n")
        self.assertEqual((self.base / "cache.bin").stat().st_size, 1024)

    def test_generated_script_embeds_one_helper_and_one_large_baseline(self):
        (self.base / ".gitignore").write_text(".cache/\n")
        cache = self.base / ".cache"
        cache.mkdir()
        for index in range(1000):
            (cache / f"{index:05}.txt").write_text("cache\n")
        self.refresh_baseline()
        self.assertTrue(self.run_pipeline())
        script = next(iter(self.delivery_artifacts())) / "apply_delivery.sh"
        self.assertLess(script.stat().st_size, 250000, "Do not duplicate the full baseline per guard")
        self.assertEqual(script.read_text().count("delivery_check()"), 1)
        result = self.apply_delivery(env=dict(os.environ, PYTHONPATH="/makewand-sdk-is-not-installed"))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.base / "app.txt").read_text(), "APPROVED\n")

    def assert_private_helper_mutation_rejected(self, mutation):
        self.assertTrue(self.run_pipeline())
        marker = self.root / "private-helper-mutated"
        wrapper_dir = self.root / "wrapper"
        wrapper_dir.mkdir()
        wrapper = wrapper_dir / "python3"
        wrapper.write_text(
            "#!" + sys.executable + "\nimport pathlib,subprocess,sys\n"
            "marker = pathlib.Path(" + repr(str(marker)) + ")\n"
            "code = subprocess.run([" + repr(sys.executable) + "] + sys.argv[1:], stdin=sys.stdin).returncode\n"
            "directory = pathlib.Path(sys.argv[-1])\n"
            "if code == 0 and directory.is_dir() and directory.name.startswith('makewand-delivery-state.') and not marker.exists():\n"
            + mutation + "    marker.write_text('mutated')\n"
            "sys.exit(code)\n",
            encoding="utf-8",
        )
        wrapper.chmod(0o755)
        result = self.apply_delivery(env=dict(os.environ, PATH=str(wrapper_dir) + os.pathsep + os.environ["PATH"]))
        self.assertTrue(marker.exists(), result.stderr)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Delivery destination check failed", result.stderr)
        self.assertEqual((self.base / "app.txt").read_text(), "BASE\n")
        return result

    def test_private_frozen_binding_cannot_be_changed_before_preflight(self):
        result = self.assert_private_helper_mutation_rejected(
            "    (directory / 'binding.json').write_text('{}')\n"
        )
        self.assertIn("helper or binding changed", result.stderr)

    def test_private_helper_symlink_is_never_followed(self):
        outside = self.root / "outside-helper.py"
        outside.write_text("raise RuntimeError('outside helper was executed')\n")
        result = self.assert_private_helper_mutation_rejected(
            "    (directory / 'helper.py').unlink()\n"
            "    (directory / 'helper.py').symlink_to(" + repr(str(outside)) + ")\n"
        )
        self.assertNotIn("outside helper was executed", result.stderr)
        self.assertEqual(outside.read_text(), "raise RuntimeError('outside helper was executed')\n")

    def test_apply_pins_worktree_despite_local_core_worktree_configuration(self):
        other = self.root / "other"
        shutil.copytree(self.base, other)
        self.assertTrue(self.run_pipeline())
        git(self.base, "config", "core.worktree", str(other))
        result = self.apply_delivery()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.base / "app.txt").read_text(), "APPROVED\n")
        self.assertEqual((other / "app.txt").read_text(), "BASE\n")

    def test_apply_supports_linked_worktree_gitfile(self):
        linked = self.root / "linked-worktree"
        git(self.base, "worktree", "add", "-b", "linked-fixture", str(linked))
        self.base = linked
        self.baseline = git(linked, "rev-parse", "HEAD")
        shutil.rmtree(self.shadow)
        git(self.root, "clone", "--shared", str(linked), str(self.shadow))
        self.assertTrue((linked / ".git").is_file())
        self.assertTrue(self.run_pipeline())
        result = self.apply_delivery()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((linked / "app.txt").read_text(), "APPROVED\n")

    def test_shadow_link_normalization_cannot_publish_an_unapplicable_delivery(self):
        (self.base / "keep.txt").write_text("keep\n")
        (self.base / "shortcut").symlink_to(self.base / "app.txt")
        self.refresh_baseline()
        (self.shadow / "shortcut").unlink()
        (self.shadow / "shortcut").symlink_to("app.txt")
        git(self.shadow, "add", "-A")
        git(self.shadow, "commit", "-m", "normalized shadow baseline")
        self.baseline = git(self.shadow, "rev-parse", "HEAD")

        def coder(path):
            (path / "shortcut").unlink()
            (path / "shortcut").symlink_to("keep.txt")

        self.assertFalse(self.run_pipeline(coder=coder))
        self.assert_not_published()
        self.assertEqual(os.readlink(self.base / "shortcut"), str(self.base / "app.txt"))

    def test_postimage_verification_rejects_a_successful_git_without_application(self):
        self.assertTrue(self.run_pipeline())
        actual_git = shutil.which("git")
        wrapper_dir = self.root / "wrapper"
        wrapper_dir.mkdir()
        wrapper = wrapper_dir / "git"
        wrapper.write_text("#!" + sys.executable + "\nimport subprocess,sys\n"
                           "if 'apply' in sys.argv and '--check' not in sys.argv and '--reverse' not in sys.argv:\n"
                           "    sys.exit(0)\n"
                           "sys.exit(subprocess.run([" + repr(actual_git) + "] + sys.argv[1:]).returncode)\n")
        wrapper.chmod(0o755)
        result = self.apply_delivery(env=dict(os.environ, PATH=str(wrapper_dir) + os.pathsep + os.environ["PATH"]))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Delivery destination check failed", result.stderr)
        self.assertEqual((self.base / "app.txt").read_text(), "BASE\n")

    def test_standalone_guard_supports_non_git_workspaces_and_external_symlinks(self):
        root = self.root / "non-git"
        root.mkdir()
        (root / "file.bin").write_bytes(b"\x00\xff")
        outside = self.root / "external.txt"
        outside.write_text("outside\n")
        (root / "link").symlink_to(outside)
        frozen = capture_delivery_baseline(root)
        script = self.root / "guard.sh"
        script.write_text("#!/usr/bin/env bash\nset -euo pipefail\n" + delivery_shell_guard(frozen))
        valid = subprocess.run(["bash", str(script)], capture_output=True, text=True, timeout=30)
        self.assertEqual(valid.returncode, 0, valid.stderr)
        outside.write_text("external content is not followed\n")
        valid = subprocess.run(["bash", str(script)], capture_output=True, text=True, timeout=30)
        self.assertEqual(valid.returncode, 0, valid.stderr)
        (root / "new.txt").write_text("drift\n")
        rejected = subprocess.run(["bash", str(script)], capture_output=True, text=True, timeout=30)
        self.assertNotEqual(rejected.returncode, 0)

    def test_apply_submodule_payload_and_head_drift(self):
        source = self.root / "submodule-source"
        source.mkdir()
        git(source, "init")
        git(source, "config", "user.name", "Submodule regression")
        git(source, "config", "user.email", "test@example.invalid")
        (source / "module.txt").write_text("before\n")
        git(source, "add", "-A")
        git(source, "commit", "-m", "module baseline")
        git(self.base, "-c", "protocol.file.allow=always", "submodule", "add", str(source), "dependency")
        self.refresh_baseline()
        child = self.base / "dependency"
        self.sub_baselines = {"dependency": git(child, "rev-parse", "HEAD")}

        def coder(path):
            (path / "app.txt").write_text("APPROVED\n")
            (path / "dependency/module.txt").write_text("reviewed module\n")

        self.assertTrue(self.run_pipeline(coder=coder))
        git(child, "-c", "user.name=Submodule regression", "-c", "user.email=test@example.invalid",
            "commit", "--allow-empty", "-m", "changed child HEAD")
        self.assert_drift_rejected()
        git(child, "reset", "--hard", self.sub_baselines["dependency"])
        result = self.apply_delivery()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((child / "module.txt").read_text(), "reviewed module\n")


def _has_pytest():
    try:
        import pytest  # noqa: F401
        return True
    except ImportError:
        return bool(shutil.which("pytest"))


class MixedTestDiscoveryTests(unittest.TestCase):
    @unittest.skipUnless(_has_pytest(), "pytest required for root python discovery test")
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
                # This regression exercises discovery, without loading unrelated
                # pytest plugins installed in the developer's Python environment.
                fixture_env = {**os.environ, **kwargs.get("extra_env", {}),
                               "PYTEST_DISABLE_PLUGIN_AUTOLOAD": "1"}
                result = subprocess.run(cmd, cwd=workspace, capture_output=True, text=True,
                                        env=fixture_env, timeout=20)
                return result.returncode, result.stdout, result.stderr, None

            with patch("makewand.sandbox.run_in_sandbox", side_effect=fixture_process):
                passed, details = orch.run_local_tests(str(root))
            self.assertTrue(passed, details)
            self.assertIn("1 passed", details)
            self.assertNotEqual(commands[0][-1], "tests")
