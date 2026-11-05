"""Multi-stage DAG delivery stays truthful in the cases round 1 missed (py-orch-4 r2).

Every stage runs through the real pipeline (mocked provider CLIs, real host
transaction, review gate and candidate seal/apply). Read-only nodes review the
cumulative result and never contribute changes; a DAG started in a subdirectory
sees the whole repository; results apply cannot deliver are never offered to it;
pending candidates built on the user's work in progress apply once it is
committed; a running DAG's staging copy survives concurrent pruning; an
interrupted DAG reports what it kept. Listings label records by what happened.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import argparse
import contextlib
import io
import json
import os
import re
import shutil
import tempfile
import unittest
from datetime import datetime, timedelta
from unittest.mock import patch

from makewand import cli, config, orchestrator as orch
from makewand.execution_contract import EXIT_PASSED
from makewand.git_helper import run_git_cmd
from makewand.workflow import last_result

VERDICT_TAG = re.compile(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)")
TASK_ID = re.compile(r"DAG 拓扑子任务 (task-\d+)")
RACE_ID = re.compile(r"\b(dg_[0-9a-f]{12})\b")


def git(repo, *arguments):
    code, out, error = run_git_cmd(["git", *arguments], cwd=str(repo))
    assert code == 0, error
    return out


LEGACY_WTA_REPORT = ('MAKEWAND_RACE_VERDICT: {"pass": true, "winner": "A", "defects": []}\n'
                     "[Winner-Take-All] 选手 A (CLAUDE) 率先完成并通过所有质量验证门禁，选手 B 已被快速熔断取消。直接裁定选手 A 胜出。")
REVIEWED_WTA_REPORT = ("[Winner-Take-All] 选手 A (CLAUDE) 率先通过本地测试门禁，竞争选手已被取消。独立只读复审 (CODEX) 结论: passed\n"
                       'MAKEWAND_VERDICT_0a1b2c3d: {"pass": true, "defects": []}')
JUDGE_REPORT = 'B is cleaner.\nMAKEWAND_RACE_VERDICT_0a1b2c3d: {"pass": true, "winner": "B", "defects": []}'


class ProbeHelpTests(unittest.TestCase):
    def test_status_probe_help_names_the_real_agy_call(self):
        out = io.StringIO()
        with patch.object(sys, "argv", ["makewand", "status", "--help"]), contextlib.redirect_stdout(out), \
                self.assertRaises(SystemExit) as raised:
            cli.main()
        self.assertEqual(raised.exception.code, 0)
        text = " ".join(out.getvalue().split())
        self.assertIn("real model calls for claude/codex/muse/grok", text)
        self.assertIn("agy makes one real call", text)


if __name__ == "__main__":
    unittest.main()
