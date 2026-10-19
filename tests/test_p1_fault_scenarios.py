"""P1-T delivery gates with real Git, fake CLI children and local static tests.

No makewand dispatch command or installed provider executable is started. The
routing/sandbox transport is replaced; provider adapters, subprocess runner,
execution accounting, review nonce, local test gate and delivery checks are real.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from makewand import config, orchestrator as orch
from makewand.delivery import capture_delivery_baseline
from makewand.execution_contract import ExecutionRequest, STATUS_CODES
from makewand.execution_runtime import execute
from makewand.git_helper import ShadowWorktreeResult, run_git_cmd
from makewand.providers import claude as claude_provider, codex as codex_provider
from makewand.providers.base import run_subprocess
from makewand.workflow import last_result, remember_result


FAKE_CLI = Path(__file__).parent / "fixtures" / "p1_fault_cli.py"


def git(workspace, *args):
    code, out, err = run_git_cmd(["git", *args], cwd=str(workspace))
    if code:
        raise AssertionError((args, out, err))
    return out.strip()


@unittest.skipUnless(os.name == "posix", "generated Bash delivery and SIGKILL require POSIX")
class FakeCLIFaultTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="makewand-p1-fault-")
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        (self.repo / "app.txt").write_text("BASE\n", encoding="utf-8")
        # unittest is also understood by pytest. The failure is static, never
        # decided by the fake model's claims or a mocked test result.
        (self.repo / "test_acceptance.py").write_text(
            "import unittest\nclass Acceptance(unittest.TestCase):\n"
            "    def test_contract(self):\n        self.assertTrue(True)\n", encoding="utf-8")
        for args in (("init", "-q"), ("config", "user.name", "P1 fault fixture"),
                     ("config", "user.email", "fixture@example.invalid"),
                     ("add", "-A"), ("commit", "-qm", "baseline")):
            git(self.repo, *args)
        self.baseline = git(self.repo, "rev-parse", "HEAD")
        self.shadow = self.root / "shadow"
        shutil.copytree(self.repo, self.shadow)
        self.branch = "makewand/p1-fault-fixture"
        self.artifacts = self.root / "artifacts"
        self.ledger = self.root / "budget.json"
        self.calls_file = self.root / "calls.jsonl"
        self.results = []
        self.test_results = []
        self.before = capture_delivery_baseline(self.repo)

    def calls(self):
        return [json.loads(line) for line in self.calls_file.read_text().splitlines()]

    def run_pipeline(self, coder="pass", reviewer="pass", failing_test=False):
        if failing_test:
            for workspace in (self.repo, self.shadow):
                test = workspace / "test_acceptance.py"
                test.write_text(test.read_text().replace("assertTrue(True)", "assertTrue(False)"))
            # Keep the deliberately failing test in the baseline, so neither
            # coder nor reviewer can replace it to earn an approval.
            for workspace in (self.repo, self.shadow):
                git(workspace, "add", "-A")
                git(workspace, "commit", "-qm", "failing acceptance baseline")
            self.baseline = git(self.shadow, "rev-parse", "HEAD")
            self.before = capture_delivery_baseline(self.repo)

        shadow = ShadowWorktreeResult(
            str(self.shadow), self.branch, lambda: None,
            baseline_commit=self.baseline, repo_head=git(self.repo, "rev-parse", "HEAD"),
            repo_root=str(self.repo), worktree_root=str(self.shadow), sub_baselines={})

        def dispatch(engine, prompt, cwd=None, readonly=False, **kwargs):
            # The real runtime reserves and settles each child, so an UNKNOWN
            # result is tested across the accounting and workflow boundary.
            mode = reviewer if readonly else coder
            spec = self.root / f"spec-{len(self.results)}.json"
            spec.write_text(json.dumps({"role": "reviewer" if readonly else "coder", "mode": mode,
                                        "workspace": cwd, "calls": str(self.calls_file)}))
            request = ExecutionRequest(task_id="p1-fault", stage="review" if readonly else "implementation",
                                       engine=engine, readonly=readonly, prompt=prompt, cwd=cwd)

            def child(timeout):
                self.assertIn(engine, ("codex", "claude"), "unexpected backup provider dispatch")
                provider = codex_provider if engine == "codex" else claude_provider
                adapter = provider.execute_codex_task if engine == "codex" else provider.execute_claude_task
                launch = contextlib.nullcontext(SimpleNamespace(
                    cmd=[sys.executable, "-I", str(FAKE_CLI), str(spec)], pass_fds=(), env={}))

                def transport(cmd, **options):
                    # The fake CLI reads every prompt from stdin, regardless
                    # of the real adapter's argv/stdin size cutoff.
                    return run_subprocess(cmd, **{**options, "input_text": prompt})

                with contextlib.ExitStack() as stack:
                    stack.enter_context(patch("makewand.config.has_subscription_configured", return_value=True))
                    stack.enter_context(patch("makewand.health.load_status_cache", return_value={}))
                    stack.enter_context(patch("makewand.discovery.get_provider_model_tier", return_value={
                        "model": "offline-fixture-model", "effort": "none"}))
                    stack.enter_context(patch("makewand.sandbox.provider_sandbox_launch", return_value=launch))
                    stack.enter_context(patch.object(provider, "run_subprocess", side_effect=transport))
                    return adapter(prompt, cwd=cwd, readonly=readonly,
                                   timeout=min(timeout, .75 if mode == "timeout" else 10))

            result = execute(request, child)
            self.results.append(result)
            return result

        def local_transport(cmd, workspace, timeout, extra_env=None, **kwargs):
            # Only our static local fixture tests run on the host. No shell,
            # model binary or network operation is present in their source.
            result = subprocess.run(cmd, cwd=workspace, timeout=timeout, capture_output=True, text=True,
                                    env={**os.environ, **(extra_env or {}), "PYTEST_DISABLE_PLUGIN_AUTOLOAD": "1"})
            return result.returncode, result.stdout, result.stderr, None

        local_tests = orch.run_local_tests

        def observe_tests(*args, **kwargs):
            result = local_tests(*args, **kwargs)
            self.test_results.append(result)
            return result

        self.log = io.StringIO()
        remember_result(None)
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.dict(os.environ, {
                "MAKEWAND_CALL_BUDGET_FILE": str(self.ledger), "MAKEWAND_MAX_MODEL_CALLS": "12",
                "MAKEWAND_EXECUTION_EVIDENCE_DIR": str(self.root / "evidence")}))
            stack.enter_context(patch.object(config, "ARTIFACTS_DIR", self.artifacts))
            for name, value in (
                ("check_load_backpressure", True),
                ("check_working_tree_isolation", (False, "owned shadow fixture")),
                ("create_ephemeral_shadow_worktree", shadow),
                ("get_or_update_status", {}),
                ("select_optimal_engine_pair", (["codex", "grok"], ["claude", "muse"], {
                    "primary_coder": "codex", "primary_reviewer": "claude", "reasons": []})),
            ):
                stack.enter_context(patch.object(orch, name, return_value=value))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
            stack.enter_context(patch.object(orch, "run_local_tests", side_effect=observe_tests))
            stack.enter_context(patch("makewand.sandbox.run_in_sandbox", side_effect=local_transport))
            stack.enter_context(patch("makewand.sandbox.verify_writable_sandbox_or_authorized", return_value=(True, None, None)))
            stack.enter_context(patch("makewand.memory.format_memory_hints_for_prompt", return_value=""))
            stack.enter_context(patch("makewand.memory.format_playbook_for_prompt", return_value=""))
            stack.enter_context(contextlib.redirect_stdout(self.log))
            return orch.run_pipeline("Update app.txt", cwd=str(self.repo), force_code=True,
                                     auto_fix=True, max_fix=2, timeout=30)

    def assert_blocked(self, status):
        result = last_result()
        self.assertFalse(result.success, self.log.getvalue())
        self.assertEqual(result.status, status, self.log.getvalue())
        self.assertEqual(result.exit_code, STATUS_CODES[status])
        self.assertEqual(capture_delivery_baseline(self.repo), self.before)
        self.assertEqual(list(self.artifacts.glob("delivery_*/apply_delivery.sh")), [])
        code, _, _ = run_git_cmd(["git", "rev-parse", "--verify", "refs/heads/" + self.branch], cwd=str(self.repo))
        self.assertNotEqual(code, 0, "rejected shadow branch was published")

    def assert_unknown_not_replayed(self, expected):
        self.assert_blocked(expected)
        result = self.results[-1]
        self.assertEqual(result.status, expected)
        self.assertFalse(result.outcome_known)
        self.assertIn("测试通过" if not result.readonly else "MAKEWAND_VERDICT_", result.output)
        attempts = json.loads(self.ledger.read_text())["attempts"]
        self.assertEqual(len(attempts), len(self.results))
        self.assertEqual(attempts[-1]["result_status"], expected)
        self.assertFalse(attempts[-1]["outcome_known"])
        record = json.loads((self.root / "evidence" / (result.attempt_id + ".json")).read_text())
        self.assertEqual(record["result_status"], expected)
        self.assertFalse(record["outcome_known"])
        self.assertEqual(record["process_exit_code"], {
            "timeout": -1, "killed": -9, "nonzero_pass": 7,
        }[self.calls()[-1]["mode"]])
        # No backup model, verdict follow-up or auto-fix after uncertain output.
        self.assertEqual([r.engine for r in self.results], ["codex", "claude"] if result.readonly else ["codex"])
        self.assertEqual(len(self.calls()), len(self.results))

    def test_missing_verdict_preserves_patch_after_exactly_one_followup(self):
        self.assertFalse(self.run_pipeline(reviewer="missing_verdict"))
        self.assert_blocked("UNVERIFIED")
        self.assertEqual([c["role"] for c in self.calls()], ["coder", "reviewer", "reviewer"])
        self.assertEqual([r.engine for r in self.results], ["codex", "claude", "claude"])
        patches = list(self.artifacts.glob("unverified_*/unverified.patch"))
        self.assertEqual(len(patches), 1)
        self.assertIn("+APPROVED", patches[0].read_text())

    def test_forged_test_success_cannot_override_local_failure(self):
        # All fake reviewers approve and the coder claims tests passed; actual
        # local failures must still block every repair round and final delivery.
        self.assertFalse(self.run_pipeline(failing_test=True))
        self.assert_blocked("FAILED")
        self.assertEqual(len(self.test_results), 3)
        self.assertTrue(all(not ok and "AssertionError" in details for ok, details in self.test_results))
        self.assertIn("测试通过", self.results[0].output)
        self.assertTrue(all("\"pass\": true" in r.output for r in self.results if r.readonly))

    def test_coder_timeout_does_not_replay_partial_patch(self):
        self.assertFalse(self.run_pipeline(coder="timeout"))
        self.assert_unknown_not_replayed("TIMEOUT")

    def test_reviewer_timeout_cannot_authorize_delivery_with_partial_pass(self):
        self.assertFalse(self.run_pipeline(reviewer="timeout"))
        self.assert_unknown_not_replayed("TIMEOUT")

    def test_killed_coder_does_not_replay_partial_patch(self):
        self.assertFalse(self.run_pipeline(coder="killed"))
        self.assert_unknown_not_replayed("UNKNOWN")

    def test_killed_reviewer_cannot_authorize_delivery_with_partial_pass(self):
        self.assertFalse(self.run_pipeline(reviewer="killed"))
        self.assert_unknown_not_replayed("UNKNOWN")

    def test_changed_patch_after_approval_is_not_published(self):
        self.assertFalse(self.run_pipeline(reviewer="mutate_after_review"))
        self.assert_blocked("FAILED")
        self.assertEqual((self.shadow / "app.txt").read_text(), "UNREVIEWED\n")
        self.assertEqual(len(self.calls()), 2)
        self.assertIn("审查期间产物", self.log.getvalue())

    def test_no_patch_despite_completion_claim_is_not_delivered(self):
        self.assertFalse(self.run_pipeline(coder="no_patch"))
        self.assert_blocked("FAILED")
        self.assertEqual((self.shadow / "app.txt").read_text(), "BASE\n")
        self.assertIn("任务完成", self.results[0].output)

    def test_nonzero_coder_exit_overrides_success_prose(self):
        self.assertFalse(self.run_pipeline(coder="nonzero_pass"))
        self.assert_unknown_not_replayed("UNKNOWN")

    def test_nonzero_reviewer_exit_overrides_valid_pass_line(self):
        self.assertFalse(self.run_pipeline(reviewer="nonzero_pass"))
        self.assert_unknown_not_replayed("UNKNOWN")

    def test_zero_exit_cannot_override_structured_failed_verdict(self):
        self.assertFalse(self.run_pipeline(reviewer="failed_verdict"))
        self.assert_blocked("FAILED")
        self.assertTrue(all(r.status == "PASSED" for r in self.results))
        self.assertEqual(sum(c["role"] == "coder" for c in self.calls()), 3)

    def delivery_script(self):
        scripts = list(self.artifacts.glob("delivery_*/apply_delivery.sh"))
        self.assertEqual(len(scripts), 1, self.log.getvalue())
        return scripts[0]

    def test_success_control_requires_explicit_application(self):
        self.assertTrue(self.run_pipeline(), self.log.getvalue())
        self.assertTrue(last_result().success)
        self.assertEqual(len(self.calls()), 2)
        self.assertEqual(capture_delivery_baseline(self.repo), self.before)
        self.assertEqual((self.shadow / "app.txt").read_text(), "APPROVED\n")
        applied = subprocess.run(["bash", str(self.delivery_script())], capture_output=True, text=True, timeout=15)
        self.assertEqual(applied.returncode, 0, applied.stderr)
        self.assertEqual((self.repo / "app.txt").read_text(), "APPROVED\n")

    def test_fake_apply_success_without_writing_patch_is_rejected(self):
        self.assertTrue(self.run_pipeline(), self.log.getvalue())
        actual_git = shutil.which("git")
        wrapper_dir = self.root / "bin"
        wrapper_dir.mkdir()
        wrapper = wrapper_dir / "git"
        marker = self.root / "apply-calls.jsonl"
        wrapper.write_text(
            "#!" + sys.executable + "\nimport json,pathlib,subprocess,sys\n"
            "if 'apply' in sys.argv and '--check' not in sys.argv and '--reverse' not in sys.argv:\n"
            "    with pathlib.Path(" + repr(str(marker)) + ").open('a') as log:\n"
            "        log.write(json.dumps(sys.argv[1:]) + '\\n')\n"
            "    print('Patch applied. 测试通过，任务完成。')\n"
            "    sys.exit(0)\n"
            "sys.exit(subprocess.run([" + repr(actual_git) + "] + sys.argv[1:]).returncode)\n")
        wrapper.chmod(0o755)
        rejected = subprocess.run(["bash", str(self.delivery_script())], capture_output=True, text=True, timeout=15,
                                  env={**os.environ, "PATH": str(wrapper_dir) + os.pathsep + os.environ["PATH"]})
        self.assertEqual(len(marker.read_text().splitlines()), 1)
        self.assertIn("任务完成", rejected.stdout)
        self.assertNotEqual(rejected.returncode, 0)
        self.assertIn("Delivery destination check failed", rejected.stderr)
        self.assertNotIn("所有补丁已应用并校验交付内容", rejected.stdout)
        self.assertEqual(capture_delivery_baseline(self.repo), self.before)


if __name__ == "__main__":
    unittest.main()
