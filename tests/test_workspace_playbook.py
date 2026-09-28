"""
Unit tests for Makewand Workspace Playbook & Repo Knowledge (makewand/memory.py).
"""

try:  # 测试隔离必须先于 makewand 导入
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import tempfile
import unittest
from pathlib import Path
from makewand.memory import (
    load_workspace_playbook,
    save_workspace_playbook,
    record_verified_command,
    record_project_convention,
    format_playbook_for_prompt
)

class TestWorkspacePlaybook(unittest.TestCase):

    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.cwd = self.temp_dir.name
        # Make a mock .git directory to anchor repo playbook
        (Path(self.cwd) / ".git").mkdir()

    def tearDown(self):
        self.temp_dir.cleanup()

    def test_record_and_load_verified_command(self):
        record_verified_command(self.cwd, "test", "pytest -q")
        record_verified_command(self.cwd, "test", "pytest -q")  # duplicate
        record_verified_command(self.cwd, "build", "go build ./...")

        pb = load_workspace_playbook(self.cwd)
        self.assertEqual(pb["verified_test_commands"], ["pytest -q"])
        self.assertEqual(pb["verified_build_commands"], ["go build ./..."])

    def test_record_project_convention(self):
        record_project_convention(self.cwd, "Do not edit release-candidates directly")
        pb = load_workspace_playbook(self.cwd)
        self.assertIn("Do not edit release-candidates directly", pb["project_conventions"])

    def test_format_playbook_for_prompt(self):
        # Empty initially
        self.assertEqual(format_playbook_for_prompt(self.cwd), "")

        record_verified_command(self.cwd, "test", "pytest -q")
        record_project_convention(self.cwd, "All tests must run with -race")

        text = format_playbook_for_prompt(self.cwd)
        self.assertIn("【工程专属构建与测试指南 (Workspace Playbook)】", text)
        self.assertIn("• 已验证测试指令: pytest -q", text)
        self.assertIn("All tests must run with -race", text)
