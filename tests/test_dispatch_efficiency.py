"""Public CLI admission with a synthetic callback: no provider CLI or API."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CHILD = r'''
import json,sys
from unittest.mock import patch
sys.path.insert(0, sys.argv.pop(1))
from makewand import cli
def synthetic(args):
    result = cli._execute_direct_task("synthetic", lambda prompt, **kw: (True, "fixture", None),
                                      "fixture", timeout=1, readonly=True)
    print(json.dumps({"status": result.status, "task_id": result.task_id}))
    raise SystemExit(result.exit_code)
with patch("makewand.route_cli.cmd_route", synthetic):
    cli._main()
'''


class DispatchRequestTests(unittest.TestCase):
    def test_independent_clis_share_cap_and_authorized_increase_keeps_used_count(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            env = dict(os.environ)
            for key in tuple(env):
                if key.startswith("MAKEWAND_"):
                    env.pop(key)
            env.update(HOME=tmp, XDG_STATE_HOME=str(base / "state"), XDG_CACHE_HOME=str(base / "cache"),
                       XDG_CONFIG_HOME=str(base / "config"), MAKEWAND_CONFIG_DIR=str(base / "config"),
                       MAKEWAND_CALL_BUDGET_FILE=str(base / "calls.json"), MAKEWAND_LAYERED_BUDGET="1",
                       MAKEWAND_NO_DAEMON="1", MAKEWAND_ROUTE_QUOTA_FILE=str(base / "missing.json"))

            def invoke(*args, synthetic=True):
                prefix = [sys.executable, "-I", "-c", CHILD, str(ROOT)] if synthetic else [sys.executable, "-I", str(ROOT / "bin/makewand")]
                return subprocess.run(prefix + list(args), cwd=tmp, env=env, capture_output=True,
                                      text=True, stdin=subprocess.DEVNULL, timeout=20)

            def dispatch(maximum=3, request="user-request"):
                return invoke("route", "review", "normal", "--request-id", request, "--caller", "codex",
                              "--max-model-calls", str(maximum))

            for _ in range(3):
                result = dispatch()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(json.loads(result.stdout)["task_id"], "user-request")
            self.assertEqual(dispatch().returncode, 13)
            amended = invoke("budget", "request", "--request-id", "user-request", "--max-model-calls", "5",
                             "--set-limit", "5", "--json", synthetic=False)
            self.assertEqual(amended.returncode, 0, amended.stderr)
            report = json.loads(amended.stdout)
            self.assertEqual((report["maximum"], report["used"], report["remaining"]), (5, 3, 2))
            for _ in range(2):
                self.assertEqual(dispatch(maximum=5).returncode, 0)
            self.assertEqual(dispatch(maximum=5).returncode, 13)
            self.assertEqual(dispatch(request="independent-request").returncode, 0)
            ledger = json.loads((base / "calls.json").read_text())
            self.assertEqual(ledger["scopes"]["tasks"]["user-request"]["used"], 5)
            self.assertEqual(ledger["scopes"]["tasks"]["independent-request"]["used"], 1)

    def test_inherited_makewand_task_id_on_dispatch_command_enforces_default_budget_of_3(self):
        provider_child = r'''
import json,sys
from unittest.mock import patch
sys.path.insert(0, sys.argv.pop(1))
from makewand import cli
def synthetic(args, is_auto_routed_run=False):
    result = cli._execute_direct_task("synthetic", lambda prompt, **kw: (True, "fixture", None),
                                      "fixture", timeout=1, readonly=True)
    print(json.dumps({"status": result.status, "task_id": result.task_id}))
    raise SystemExit(result.exit_code)
with patch("makewand.cli._dispatch_parsed_command", synthetic):
    cli._main()
'''
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            env = dict(os.environ)
            for key in tuple(env):
                if key.startswith("MAKEWAND_"):
                    env.pop(key)
            env.update(HOME=tmp, XDG_STATE_HOME=str(base / "state"), XDG_CACHE_HOME=str(base / "cache"),
                       XDG_CONFIG_HOME=str(base / "config"), MAKEWAND_CONFIG_DIR=str(base / "config"),
                       MAKEWAND_NO_DAEMON="1", MAKEWAND_TASK_ID="env-task-id-99")

            def invoke_codex():
                return subprocess.run(
                    [sys.executable, "-I", "-c", provider_child, str(ROOT), "codex", "hello", "--readonly"],
                    cwd=tmp, env=env, capture_output=True, text=True, stdin=subprocess.DEVNULL, timeout=20,
                )

            for _ in range(3):
                res = invoke_codex()
                self.assertEqual(res.returncode, 0, res.stderr)
                self.assertEqual(json.loads(res.stdout)["task_id"], "env-task-id-99")
            self.assertEqual(invoke_codex().returncode, 13)
            ledger = json.loads((base / "config" / "call-budget.json").read_text())
            self.assertEqual(ledger["scopes"]["tasks"]["env-task-id-99"]["used"], 3)
            self.assertEqual(ledger["scopes"]["tasks"]["env-task-id-99"]["maximum"], 3)
