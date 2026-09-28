"""
G4 regression tests: read-only Claude tasks (replay-0926-memory#7).

Commit 72de16a switched read-only Claude tasks to
`--allowed-tools Read,Grep,Glob --dangerously-skip-permissions`. --allowedTools
only pre-approves tools, and bypass mode approves everything anyway, so a
"read-only review" had Bash/Write/Edit — on a host without bwrap it ran with
full write access. Read-only tasks must restrict the built-in tool set itself
and never use a bypass permission mode.
"""

import os
import shutil
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from makewand.providers.claude import execute_claude_task


def _value_after(cmd, flag):
    idx = cmd.index(flag)
    return cmd[idx + 1]


class TestClaudeReadonlyFlags(unittest.TestCase):
    def setUp(self):
        self.home = tempfile.mkdtemp(prefix="g4-claude-home-")
        self.ws = tempfile.mkdtemp(prefix="g4-claude-ws-")
        self._patches = [
            patch.dict(os.environ, {"HOME": self.home}),
            patch("makewand.config.has_subscription_configured", return_value=True),
            patch("makewand.health.load_status_cache", return_value={}),
            patch("makewand.providers.claude.run_subprocess", return_value=(0, "ok", "", None)),
        ]
        self.mocks = [p.start() for p in self._patches]
        self.run_mock = self.mocks[-1]

    def tearDown(self):
        for p in reversed(self._patches):
            p.stop()
        shutil.rmtree(self.home, ignore_errors=True)
        shutil.rmtree(self.ws, ignore_errors=True)

    def _cmd(self, readonly, bwrap):
        with patch("makewand.sandbox.is_bwrap_available", return_value=bwrap):
            ok, out, err = execute_claude_task("review this", cwd=self.ws, readonly=readonly)
        self.assertTrue(ok, err)
        return list(self.run_mock.call_args[0][0])

    def _assert_readonly_flags(self, cmd):
        self.assertNotIn("--dangerously-skip-permissions", cmd)
        self.assertNotIn("--allow-dangerously-skip-permissions", cmd)
        self.assertEqual(_value_after(cmd, "--tools"), "Read,Grep,Glob")
        self.assertEqual(_value_after(cmd, "--allowedTools"), "Read,Grep,Glob")
        mode = _value_after(cmd, "--permission-mode")
        self.assertNotIn(mode, ("bypassPermissions", "acceptEdits", "auto"))
        self.assertEqual(mode, "dontAsk")
        self.assertEqual(_value_after(cmd, "--setting-sources"), "user")
        self.assertIn("--strict-mcp-config", cmd)
        self.assertIn("--no-session-persistence", cmd)

    def test_readonly_task_never_bypasses_permissions_inside_bwrap(self):
        cmd = self._cmd(readonly=True, bwrap=True)
        self.assertEqual(os.path.basename(cmd[0]), "bwrap")
        self._assert_readonly_flags(cmd)

    def test_readonly_task_without_bwrap_is_still_tool_restricted(self):
        # trusted + no bwrap runs the CLI on the host: the tool restriction is the only guard
        cmd = self._cmd(readonly=True, bwrap=False)
        self.assertEqual(cmd[0], "claude")
        self._assert_readonly_flags(cmd)

    def test_variadic_tool_flags_are_not_followed_by_positional_prompt(self):
        cmd = self._cmd(readonly=True, bwrap=False)
        prompt_idx = cmd.index("review this")
        self.assertLess(prompt_idx, cmd.index("--tools"))
        for flag in ("--tools", "--allowedTools"):
            nxt = cmd[cmd.index(flag) + 2]
            self.assertTrue(nxt.startswith("--"), f"{flag} value must be followed by another option, got {nxt!r}")

    def test_writable_task_keeps_bypass_only_inside_bwrap(self):
        cmd = self._cmd(readonly=False, bwrap=True)
        self.assertEqual(os.path.basename(cmd[0]), "bwrap")
        self.assertIn("--dangerously-skip-permissions", cmd)
        self.assertNotIn("--tools", cmd)

    def test_readonly_tool_constant(self):
        from makewand.providers.claude import CLAUDE_READONLY_TOOLS
        self.assertEqual(CLAUDE_READONLY_TOOLS, "Read,Grep,Glob")


if __name__ == "__main__":
    unittest.main()
