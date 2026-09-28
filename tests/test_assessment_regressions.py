"""Behavioral regressions for the independent 2026-09-27 assessment."""

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
import tempfile
import unittest
from datetime import datetime
from pathlib import Path
from unittest.mock import patch

import makewand.candidate as candidate
import makewand.config as config
import makewand.health as health
import makewand.orchestrator as orch
from makewand.git_helper import run_git_cmd
from makewand.providers.api_client import call_api_chat


def git(path, *args):
    code, out, err = run_git_cmd(["git", *args], cwd=str(path))
    if code:
        raise AssertionError((args, out, err))
    return out.strip()


def init_repo(path):
    path.mkdir(parents=True)
    git(path, "init")
    git(path, "config", "user.name", "Assessment regression")
    git(path, "config", "user.email", "test@example.invalid")


def fixture_process(cmd, workspace, **kwargs):
    # Only executes the fixed test fixtures defined below, never model output.
    proc = subprocess.run(cmd, cwd=workspace, capture_output=True, text=True,
                          env={**os.environ, **kwargs.get("extra_env", {})}, timeout=15)
    return proc.returncode, proc.stdout, proc.stderr, None


class AssessmentFixtures(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="makewand-regression-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        for name, path in [("CONFIG_DIR", "config"), ("CANDIDATES_DIR", "config/candidates"),
                           ("BACKUPS_DIR", "config/backups")]:
            self.stack.enter_context(patch.object(config, name, self.root / path))
        config.ensure_config_dir()

    def make_candidate(self):
        base = self.root / "base"
        init_repo(base)
        (base / "run.sh").write_text("#!/bin/sh\necho before\n")
        (base / "run.sh").chmod(0o755)
        (base / "second.txt").write_text("before\n")
        git(base, "add", "-A")
        git(base, "commit", "-m", "baseline")
        baseline = git(base, "rev-parse", "HEAD")
        path = config.CANDIDATES_DIR / "mode" / "agent_a"
        shutil.copytree(base, path)
        (path / "run.sh").write_text("#!/bin/sh\necho after\n")
        (path / "second.txt").write_text("after\n")
        candidate.CandidateManager.save_race(
            "mode", "edit", str(base), baseline,
            {"path": str(path), "success": True, "test_passed": True,
             "review_passed": True, "baseline_commit": baseline}, {}, winner="A")
        return base, path

    def test_candidate_preserves_executable_mode(self):
        base, _ = self.make_candidate()
        ok, _, message = candidate.CandidateManager.apply_candidate("mode")
        self.assertTrue(ok, message)
        self.assertEqual(stat.S_IMODE((base / "run.sh").stat().st_mode), 0o755)
        self.assertIn("after", (base / "run.sh").read_text())

    def test_mode_only_change_after_review_is_rejected_even_with_force(self):
        base, path = self.make_candidate()
        (path / "run.sh").chmod(0o644)
        ok, _, _ = candidate.CandidateManager.apply_candidate("mode", force=True)
        self.assertFalse(ok)
        self.assertIn("before", (base / "run.sh").read_text())

    def test_rollback_restores_content_and_mode(self):
        base, path = self.make_candidate()
        (path / "run.sh").chmod(0o644)
        data = candidate.CandidateManager.get_race("mode")
        data["candidates"]["A"]["manifest"] = candidate.build_manifest(path)
        (config.CANDIDATES_DIR / "mode/meta.json").write_text(json.dumps(data))
        real_copy = candidate._atomic_copy
        count = 0

        def fail_second(*args, **kwargs):
            nonlocal count
            count += 1
            if count == 2:
                raise OSError("controlled write failure")
            return real_copy(*args, **kwargs)

        with patch.object(candidate, "_atomic_copy", side_effect=fail_second):
            ok, _, _ = candidate.CandidateManager.apply_candidate("mode")
        self.assertFalse(ok)
        self.assertEqual(stat.S_IMODE((base / "run.sh").stat().st_mode), 0o755)
        self.assertIn("before", (base / "run.sh").read_text())
        self.assertEqual((base / "second.txt").read_text(), "before\n")

    def test_candidate_rejected_by_judge_requires_explicit_override(self):
        base, _ = self.make_candidate()
        meta = config.CANDIDATES_DIR / "mode/meta.json"
        data = json.loads(meta.read_text())
        data["candidates"]["A"]["review_passed"] = False
        meta.write_text(json.dumps(data))
        ok, _, _ = candidate.CandidateManager.apply_candidate("mode", candidate_label="A")
        self.assertFalse(ok)
        self.assertIn("before", (base / "run.sh").read_text())

    def test_git_only_tampering_cannot_delete_a_reviewed_file(self):
        base, path = self.make_candidate()
        frozen = candidate.build_manifest(path)
        git(path, "rm", "--cached", "second.txt")
        (path / ".git/info/exclude").write_text("second.txt\n")
        self.assertEqual(candidate.build_manifest(path), frozen)
        ok, _, message = candidate.CandidateManager.apply_candidate("mode", force=True)
        self.assertFalse(ok, message)
        self.assertEqual((base / "second.txt").read_text(), "before\n")
        self.assertIn("before", (base / "run.sh").read_text())

    def test_reviewed_deletion_is_applied(self):
        base, path = self.make_candidate()
        (path / "second.txt").unlink()
        race = candidate.CandidateManager.get_race("mode")
        info = dict(race["candidates"]["A"])
        info.pop("manifest")
        info.pop("changes")
        candidate.CandidateManager.save_race(
            "mode", "delete second", str(base), race["baseline_commit"],
            info, {}, winner="A")
        ok, _, message = candidate.CandidateManager.apply_candidate("mode")
        self.assertTrue(ok, message)
        self.assertFalse((base / "second.txt").exists())

    @unittest.skipUnless(shutil.which("npm"), "npm required")
    def test_native_node_tests_directory_is_not_python_or_jest(self):
        repo = self.root / "node"
        (repo / "tests").mkdir(parents=True)
        (repo / "package.json").write_text(json.dumps({"scripts": {"test": "node --test"}}))
        (repo / "tests/ok.test.js").write_text("require('node:test')('passes', () => {});\n")
        with patch("makewand.sandbox.run_in_sandbox", side_effect=fixture_process) as runner:
            ok, details = orch.run_local_tests(str(repo))
        self.assertTrue(ok, details)
        self.assertEqual(len(runner.call_args_list), 1)

    def test_successful_test_that_rewrites_source_is_rejected(self):
        repo = self.root / "python"
        repo.mkdir()
        (repo / "app.py").write_text("VALUE = 1\n")
        (repo / "test_app.py").write_text(
            "import unittest\nfrom pathlib import Path\n"
            "class TestWrite(unittest.TestCase):\n"
            "    def test_write(self):\n"
            "        Path('app.py').write_text('UNVERIFIED = True\\n')\n")
        with patch("makewand.sandbox.run_in_sandbox", side_effect=fixture_process):
            ok, details = orch.run_local_tests(str(repo))
        self.assertFalse(ok)
        self.assertIn("app.py", details)

    def test_untracked_source_in_cache_named_directory_is_still_protected(self):
        repo = self.root / "cache-named-source"
        init_repo(repo)
        (repo / "target").mkdir()
        (repo / "target/app.py").write_text("VALUE = 1\n")
        (repo / "test_app.py").write_text(
            "import unittest\nfrom pathlib import Path\n"
            "class TestWrite(unittest.TestCase):\n"
            "    def test_write(self):\n"
            "        Path('target/app.py').write_text('not valid Python!\\n')\n")
        with patch("makewand.sandbox.run_in_sandbox", side_effect=fixture_process):
            ok, details = orch.run_local_tests(str(repo))
        self.assertFalse(ok)
        self.assertIn("target/app.py", details)

    def test_pipeline_rejects_review_time_mutation(self):
        repo = self.root / "pipeline"
        init_repo(repo)
        (repo / "app.py").write_text("BASE = 1\n")
        git(repo, "add", "-A")
        git(repo, "commit", "-m", "baseline")

        def dispatch(engine, prompt, cwd, readonly=False, **kwargs):
            (Path(cwd) / "app.py").write_text("UNREVIEWED = True\n" if readonly else "APPROVED = 2\n")
            return True, 'MAKEWAND_VERDICT: {"pass": true, "defects": []}' if readonly else "implemented", None

        with contextlib.ExitStack() as s:
            s.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            s.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=(True, None)))
            s.enter_context(patch.object(orch, "get_or_update_status", return_value={}))
            s.enter_context(patch.object(orch, "select_optimal_engine_pair", return_value=(
                ["codex"], ["claude"], {"primary_coder": "codex", "primary_reviewer": "claude", "reasons": []})))
            s.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
            s.enter_context(patch("makewand.memory.format_memory_hints_for_prompt", return_value=""))
            s.enter_context(contextlib.redirect_stdout(io.StringIO()))
            self.assertFalse(orch.run_pipeline("Implement update", cwd=str(repo), force_code=True, auto_fix=False))

    def run_race_fixture(self, verdict):
        repo = self.root / "race"
        init_repo(repo)
        (repo / "app.py").write_text("BASE = 1\n")
        git(repo, "add", "-A")
        git(repo, "commit", "-m", "baseline")

        def dispatch(engine, prompt, cwd, **kwargs):
            (Path(cwd) / "app.py").write_text("CANDIDATE = 1\n")
            return True, "implemented", None

        with contextlib.ExitStack() as s:
            s.enter_context(patch.object(orch, "CANDIDATES_DIR", config.CANDIDATES_DIR))
            s.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            s.enter_context(patch.object(orch, "get_or_update_status", return_value={}))
            s.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
            s.enter_context(patch.object(orch, "run_local_tests", return_value=(True, None)))
            s.enter_context(patch.object(orch, "execute_agy_task", return_value=(True, verdict, None)))
            s.enter_context(contextlib.redirect_stdout(io.StringIO()))
            code = orch.run_race("Implement update", cwd=str(repo), engine_a="codex", engine_b="claude")
        return code, candidate.CandidateManager.get_race()

    def test_race_natural_language_rejection_never_picks_fastest(self):
        code, race = self.run_race_fixture("两套候选方案均存在严重安全缺陷，拒绝采纳任何方案。")
        self.assertEqual(code, orch.EXIT_UNVERIFIED)
        self.assertIsNone(race["winner"])
        self.assertFalse(race["candidates"]["A"]["review_passed"])

    def test_race_structured_rejection(self):
        code, race = self.run_race_fixture('MAKEWAND_RACE_VERDICT: {"pass": false, "winner": null, "defects": ["wrong"]}')
        self.assertEqual(code, orch.EXIT_FAILED)
        self.assertIsNone(race["winner"])

    def test_race_explicit_acceptance_selects_only_approved_candidate(self):
        code, race = self.run_race_fixture('MAKEWAND_RACE_VERDICT: {"pass": true, "winner": "B", "defects": []}')
        self.assertEqual(code, orch.EXIT_PASSED)
        self.assertEqual(race["winner"], "B")
        self.assertFalse(race["candidates"]["A"]["review_passed"])


class TestAssessmentPolicies(unittest.TestCase):
    def test_relative_quota_deadline_is_idempotent(self):
        class Clock(datetime):
            @classmethod
            def now(cls, tz=None):
                return datetime(2026, 1, 1, 12, 0, tzinfo=tz)

        for reset in ["in 2 hours", "in 1.5 hours", "in 1h 30m"]:
            with self.subTest(reset=reset), patch.object(health, "datetime", Clock):
                cache = {"codex": {"status": "limited", "updated_at": "2026-01-01T11:00:00", "resets_at": reset}}
                first = json.loads(json.dumps(health._sanitize_cache(cache)))
                for _ in range(5):
                    health._sanitize_cache(cache)
                self.assertEqual(cache, first)
                self.assertEqual(cache["codex"]["status"], "limited")

    def test_default_policy_blocks_api_before_http_even_with_credentials(self):
        with patch.dict(os.environ, {"MAKEWAND_API_POLICY": "subscription_only"}), \
             patch("makewand.providers.api_client.get_api_config", return_value={"api_key": "test-only"}), \
             patch("makewand.providers.api_client._make_http_request") as http:
            ok, _, error = call_api_chat("claude", "test")
        self.assertFalse(ok)
        self.assertIn("subscription_only", error)
        http.assert_not_called()

    def test_invalid_api_policy_fails_closed(self):
        with patch.dict(os.environ, {"MAKEWAND_API_POLICY": "allow_piad"}):
            self.assertFalse(config.is_api_allowed("codex"))

    def test_runtime_routing_obeys_api_policy(self):
        with patch.dict(os.environ, {"MAKEWAND_API_POLICY": "subscription_only"}), \
             patch.object(config, "has_subscription_configured", return_value=True), \
             patch.object(config, "has_api_configured", return_value=True), \
             patch.object(config, "load_user_config", return_value={}):
            self.assertEqual(config.get_provider_execution_mode("codex"), "subscription")
        with patch.dict(os.environ, {"MAKEWAND_API_POLICY": "allow_paid"}), \
             patch.object(config, "has_subscription_configured", return_value=True), \
             patch.object(config, "has_api_configured", return_value=True), \
             patch.object(config, "load_user_config", return_value={}):
            self.assertEqual(config.get_provider_execution_mode("codex"), "hybrid")

    def test_local_is_disabled_by_default_and_explicitly_enablable(self):
        with patch.dict(os.environ, {}, clear=True), patch.object(config, "load_user_config", return_value={}):
            self.assertFalse(config.is_provider_enabled("local"))
        with patch.dict(os.environ, {}, clear=True), patch.object(config, "load_user_config", return_value={"enabled_providers": {"local": True}}):
            self.assertTrue(config.is_provider_enabled("local"))

    def test_legacy_installer_settings_and_canonical_precedence(self):
        legacy = {"active_providers": ["claude", "codex", "agy"], "local_model_enabled": False}
        with patch.dict(os.environ, {}, clear=True), patch.object(config, "load_user_config", return_value=legacy):
            enabled = config.get_enabled_providers()
            self.assertTrue(enabled["codex"])
            self.assertFalse(enabled["local"])
            self.assertFalse(enabled["deepseek"])
            legacy["enabled_providers"] = {"local": True, "codex": False}
            enabled = config.get_enabled_providers()
            self.assertTrue(enabled["local"])
            self.assertFalse(enabled["codex"])

    def test_string_false_cannot_enable_local_provider(self):
        with patch.dict(os.environ, {}, clear=True), patch.object(config, "load_user_config", return_value={"enabled_providers": {"local": "false"}}):
            self.assertFalse(config.is_provider_enabled("local"))

    def test_race_verdict_requires_boolean_and_unambiguous_winner(self):
        for value in ['{"pass":"true","winner":"A","defects":[]}',
                      '{"pass":true,"winner":"A","defects":["wrong"]}',
                      '{"pass":false,"winner":"A","defects":[]}',
                      '{"pass":true,"winner":"C","defects":[]}']:
            with self.subTest(value=value):
                self.assertIsNone(orch.parse_race_verdict("MAKEWAND_RACE_VERDICT: " + value))
