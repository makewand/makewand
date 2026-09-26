"""
Unit tests for Makewand Interactive Console / REPL.
"""

import os
import unittest
from pathlib import Path
from unittest.mock import patch, MagicMock

from makewand.interactive import (
    render_welcome_card,
    get_git_branch,
    format_short_path,
    get_model_status_badges,
    SLASH_COMMANDS,
)


class TestInteractiveConsole(unittest.TestCase):
    def test_render_welcome_card(self):
        cwd = os.getcwd()
        card = render_welcome_card(cwd, width=66)
        self.assertIn("╭", card)
        self.assertIn("╰", card)
        self.assertIn("│", card)
        self.assertIn("Makewand (v3.1.0)", card)
        self.assertIn("/help", card)

    def test_get_git_branch(self):
        cwd = os.getcwd()
        branch = get_git_branch(cwd)
        # Should be master on this repo
        self.assertIsNotNone(branch)
        self.assertEqual(branch, "master")

    def test_format_short_path(self):
        home = str(Path.home())
        test_p = os.path.join(home, "dev", "project")
        short = format_short_path(test_p)
        self.assertEqual(short, "~/dev/project")

        non_home = "/path/to/workspace/test"
        self.assertEqual(format_short_path(non_home), "/path/to/workspace/test")

    def test_slash_commands_presence(self):
        expected = ["/help", "/status", "/diff", "/review", "/compact", "/clear", "/exit"]
        for cmd in expected:
            self.assertIn(cmd, SLASH_COMMANDS)

    def test_model_status_badges(self):
        mock_cache = {
            "agy": {"status": "healthy"},
            "claude": {"status": "limited"},
            "codex": {"status": "limited"},
        }
        badges = get_model_status_badges(mock_cache)
        self.assertIn("AGY", badges)
        self.assertIn("Claude", badges)
        self.assertIn("Codex", badges)


if __name__ == "__main__":
    unittest.main()
