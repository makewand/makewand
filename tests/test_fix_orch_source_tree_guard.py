"""Tests never run a writing task against the makewand checkout itself (test-hazard).

A host-mode task snapshots the whole repository of its cwd and rolls it back on
failure. A test that calls run_pipeline without a temporary cwd therefore used to
snapshot the checkout under test and later revert edits and delete files created
meanwhile in it. Under the test harness the pipeline now refuses such a target
before any lock, transaction, shadow worktree or model call, and every pipeline
call in tests/ passes an explicit cwd.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import ast
import contextlib
import io
import os
import tempfile
import unittest
from unittest.mock import patch

import makewand
from makewand import git_helper, orchestrator as orch
from makewand.execution_contract import EXIT_UNVERIFIED
from makewand.git_helper import run_git_cmd

TESTS_DIR = Path(__file__).resolve().parent
# Entry points that may write to (or snapshot) their cwd, and the positional
# index of cwd when it is not passed by keyword (None: keyword only).
PIPELINE_ENTRY_POINTS = {"run_pipeline": 1, "_run_pipeline_impl": 1, "run_workflow": None,
                         "run_race": 1, "_run_race_impl": 1}


class PipelineCallAuditTests(unittest.TestCase):
    def test_every_pipeline_call_in_tests_passes_an_explicit_cwd(self):
        """Without cwd a pipeline would work on the process cwd: the checkout under test."""
        missing = []
        for path in sorted(TESTS_DIR.glob("test_*.py")):
            tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
            for node in ast.walk(tree):
                if not isinstance(node, ast.Call):
                    continue
                func = node.func
                if isinstance(func, ast.Name):
                    name = func.id
                elif isinstance(func, ast.Attribute):
                    base = func.value
                    while isinstance(base, ast.Attribute):
                        base = base.value
                    if isinstance(base, ast.Name) and base.id == "self":
                        continue  # fixture helpers, which pass their own temporary cwd
                    name = func.attr
                else:
                    continue
                if name not in PIPELINE_ENTRY_POINTS:
                    continue
                keywords = {keyword.arg for keyword in node.keywords}
                if "cwd" in keywords or None in keywords or any(isinstance(arg, ast.Starred) for arg in node.args):
                    continue
                position = PIPELINE_ENTRY_POINTS[name]
                if position is not None and len(node.args) > position:
                    continue
                missing.append(f"{path.name}:{node.lineno}: {ast.get_source_segment(path.read_text(encoding='utf-8'), node)}")
        self.assertEqual(missing, [], "pipeline calls without an explicit (temporary) cwd")


if __name__ == "__main__":
    unittest.main()
