"""A host-mode delivery hard block reports UNVERIFIED (exit 11), not FAILED (n1-status).

HostWorkspaceTransaction.finalize_success rolls back an already reviewed change
when the delivery check finds something outside the reviewed diff. The change
was never judged defective, so the pipeline outcome takes the transaction's
outcome_status/failure_reason and prints the reason behind a neutral prefix: only
the reason says whether the rollback was complete (msg-rollback). Only provider
dispatch, routing, the sandbox probe and the test runner are mocked; the real
transaction runs on a temporary repository.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import contextlib
import io
import os
import re
import tempfile
import unittest
from unittest.mock import patch

from makewand import orchestrator as orch
from makewand.execution_contract import EXIT_FAILED, EXIT_UNVERIFIED
from makewand.git_helper import HostWorkspaceTransaction, run_git_cmd

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")
HELPERS = "def double(x):\n    return 2 * x\n"


class HostBlockStatusTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-host-block-")
        self.addCleanup(temporary.cleanup)
        self.repo = Path(temporary.name) / "repo"
        self.repo.mkdir()
        (self.repo / "app.py").write_text("VALUE = 1\n")
        (self.repo / ".gitignore").write_text("__pycache__/\n.envrc\n")
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "."], ["commit", "-qm", "base"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(self.repo))
            self.assertEqual(code, 0, error)

    def run_task(self, coder):
        engines = ("claude", "codex")

        def provider(engine):
            def execute(prompt, **kwargs):
                if kwargs.get("readonly"):
                    tag = VERDICT_TAG.search(prompt).group(0)
                    return True, f'Reviewed the complete diff.\n{tag}: {{"pass": true, "defects": []}}', None
                return coder(Path(kwargs["cwd"]))
            return execute

        out = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=(True, None)))
            stack.enter_context(patch("makewand.sandbox.verify_writable_sandbox_or_authorized", return_value=(True, None, None)))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={name: {"status": "healthy"} for name in engines}))
            stack.enter_context(patch.object(orch, "select_optimal_engine_pair", return_value=(
                list(engines), list(reversed(engines)), dict(primary_coder="claude", primary_reviewer="codex", reasons=[]))))
            stack.enter_context(patch("makewand.config.is_provider_enabled", side_effect=lambda name: name in engines))
            stack.enter_context(patch("makewand.config.get_active_providers", return_value=list(engines)))
            stack.enter_context(patch("makewand.usage.record_engine_usage"))
            stack.enter_context(patch("makewand.memory.format_memory_hints_for_prompt", return_value=""))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, "1 passed in 0.01s")))
            for name in engines:
                stack.enter_context(patch.object(orch, f"execute_{name}_task", side_effect=provider(name)))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(out))
            result = orch.run_workflow("Add helpers.double and use it in app.py", cwd=str(self.repo),
                                       force_code=True, auto_fix=False, total_timeout=60)
        return result, out.getvalue()

    @staticmethod
    def reviewed_change_with_unreviewed_envrc(workspace):
        (workspace / "app.py").write_text("from helpers import double\nVALUE = double(1)\n")
        (workspace / "helpers.py").write_text(HELPERS)
        # Ignored, so never shown to the reviewer, and executed by direnv.
        (workspace / ".envrc").write_text("export PATH=/tmp/evil:$PATH\n")
        return True, "implemented", None

    def test_delivery_hard_block_is_unverified_with_exit_11_and_reason(self):
        result, log = self.run_task(self.reviewed_change_with_unreviewed_envrc)
        self.assertEqual(result.status, "UNVERIFIED", log)
        self.assertEqual(result.exit_code, EXIT_UNVERIFIED)
        self.assertIn("UNVERIFIED", result.error)
        self.assertIn(".envrc", result.error)
        rejection = [line for line in log.splitlines() if "拒绝确认交付" in line]
        self.assertEqual(len(rejection), 1, log)
        self.assertIn(result.error, rejection[0])
        # The reviewed change was rolled back, nothing unreviewed stays behind.
        self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 1\n")
        self.assertFalse((self.repo / "helpers.py").exists())
        self.assertFalse((self.repo / ".envrc").exists())

    def test_transaction_outcome_status_and_reason_are_reported(self):
        def finalize(blocked_status):
            def finalize_success(txn):
                txn.rollback("simulated delivery block")
                txn.succeeded = False
                txn.outcome_status = blocked_status
                txn.failure_reason = None if blocked_status is None else "SIMULATED delivery block reason"
            return finalize_success

        def coder(workspace):
            (workspace / "app.py").write_text("VALUE = 2\n")
            return True, "implemented", None

        for blocked_status, exit_code in (("UNVERIFIED", EXIT_UNVERIFIED), (None, EXIT_FAILED)):
            with self.subTest(outcome_status=blocked_status), \
                    patch.object(HostWorkspaceTransaction, "finalize_success", finalize(blocked_status)):
                result, log = self.run_task(coder)
            self.assertEqual(result.exit_code, exit_code, log)
            self.assertEqual(result.status, blocked_status or "FAILED")
            if blocked_status:
                self.assertEqual(result.error, "SIMULATED delivery block reason")
                self.assertIn("拒绝确认交付: SIMULATED delivery block reason", log)
            else:
                # Without a reason the transaction's own report tells what happened.
                for claim in ("已执行回滚", "改动已回滚"):
                    self.assertNotIn(claim, result.error)
                self.assertIn(f"拒绝确认交付: {result.error}", log)
            self.assertEqual((self.repo / "app.py").read_text(), "VALUE = 1\n")

    def test_rejection_line_never_claims_a_rollback_the_reason_does_not_report(self):
        """An incomplete rollback (e2e vl_tamper, exit 11) must not be announced as '改动已回滚'."""
        incomplete = ("交付核验触发安全阻断，结论未验证 (UNVERIFIED): 本次任务改动了未纳入审查的文件: "
                      "node_modules/leftpad/index.js；改动回滚未能完全恢复，以下任务前已存在的文件无法恢复: "
                      "node_modules/leftpad/index.js")

        def finalize_success(txn):
            txn.rollback("simulated delivery block")
            txn.succeeded, txn.outcome_status, txn.failure_reason = False, "UNVERIFIED", incomplete

        def coder(workspace):
            (workspace / "app.py").write_text("VALUE = 2\n")
            return True, "implemented", None

        with patch.object(HostWorkspaceTransaction, "finalize_success", finalize_success):
            result, log = self.run_task(coder)
        self.assertEqual(result.exit_code, EXIT_UNVERIFIED, log)
        self.assertEqual(result.error, incomplete)
        rejection = [line for line in log.splitlines() if "拒绝确认交付" in line]
        self.assertEqual(len(rejection), 1, log)
        self.assertIn(incomplete, rejection[0])
        self.assertNotIn("改动已回滚", rejection[0])

    def test_real_blocked_delivery_reports_the_transactions_rollback_outcome(self):
        """The real transaction: a pre-existing ignored file changed behind the reviewed diff."""
        library = self.repo / "node_modules" / "leftpad" / "index.js"
        (self.repo / ".gitignore").write_text("__pycache__/\n.envrc\nnode_modules/\n")
        for arguments in (["add", ".gitignore"], ["commit", "-qm", "ignore node_modules"]):
            code, _, error = run_git_cmd(["git", *arguments], cwd=str(self.repo))
            self.assertEqual(code, 0, error)
        library.parent.mkdir(parents=True)
        library.write_text("module.exports = 1;            \n")

        def tamper(workspace):
            target = workspace / "node_modules" / "leftpad" / "index.js"
            before, info = target.read_bytes(), target.stat()
            target.write_bytes(b"require('x');" + b" " * (len(before) - 14) + b"\n")
            os.utime(target, ns=(info.st_atime_ns, info.st_mtime_ns))
            (workspace / "app.py").write_text("VALUE = 2\n")
            return True, "implemented", None

        result, log = self.run_task(tamper)
        self.assertEqual(result.status, "UNVERIFIED", log)
        self.assertEqual(result.exit_code, EXIT_UNVERIFIED)
        self.assertIn("node_modules/leftpad/index.js", result.error)
        rejection = [line for line in log.splitlines() if "拒绝确认交付" in line]
        self.assertEqual(len(rejection), 1, log)
        self.assertIn(result.error, rejection[0])
        # The line states a rollback outcome only through the transaction's reason.
        self.assertEqual("改动已回滚" in rejection[0], "改动已回滚" in result.error, rejection[0])


if __name__ == "__main__":
    unittest.main()
