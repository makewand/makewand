"""A provider CLI launched by makewand must not be able to dispatch makewand again."""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import contextlib
import io
import json
import subprocess
import sys
import tempfile
import unittest
import shutil
from pathlib import Path
from unittest.mock import patch

import makewand.cli as cli

LAUNCHER = str(Path(__file__).resolve().parent.parent / "bin" / "makewand")


def _run(args, worker):
    env = dict(os.environ)
    env.pop(cli.PROVIDER_WORKER_ENV, None)
    if worker:
        env[cli.PROVIDER_WORKER_ENV] = "1"
    return subprocess.run([sys.executable, "-I", LAUNCHER, *args], env=env,
                          capture_output=True, text=True, timeout=60, stdin=subprocess.DEVNULL)


class TestProviderWorkerGuard(unittest.TestCase):
    def test_dispatching_commands_refused_inside_worker(self):
        for args in (["run", "x"], ["review"], ["race", "x"], ["ensemble", "x"], ["agy", "x"], ["claude", "x"]):
            with self.subTest(args=args):
                r = _run(args, worker=True)
                self.assertEqual(r.returncode, 2, r.stderr)
                self.assertIn("递归", r.stderr)

    def test_native_go_and_probe_commands_refused_inside_worker(self):
        # Go-native commands are refused before delegation; --probe makes real model calls.
        for args in (["chat"], ["new", "x"], ["serve"], ["doctor"], ["status", "--probe"], ["quota", "--probe"], []):
            with self.subTest(args=args):
                r = _run(args, worker=True)
                self.assertEqual(r.returncode, 2, r.stderr)
                self.assertIn("递归", r.stderr)

    def test_safe_commands_allowed_inside_worker(self):
        self.assertEqual(cli.WORKER_SAFE_SUBCOMMANDS, {"search", "repomap"})
        self.assertFalse({"state", "token", "user", "quota", "usage", "audit"} & cli.GO_WORKER_SAFE_COMMANDS)

    def test_shared_go_python_policy(self):
        cases = json.loads((Path(__file__).parent / "testdata/provider_worker_policy.json").read_text())
        with patch.dict(os.environ, {cli.PROVIDER_WORKER_ENV: "1"}):
            for case in cases:
                with self.subTest(argv=case["argv"]):
                    self.assertEqual(cli._provider_worker_refusal(None, case["argv"]) is None,
                                     case["allowed"])
        result = subprocess.run([sys.executable, "-I", "scripts/generate_cli_contract.py", "--check"],
                                cwd=Path(__file__).parent.parent, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_delegation_checks_full_path_before_go_launch(self):
        cases = json.loads((Path(__file__).parent / "testdata/provider_worker_policy.json").read_text())
        for case in cases:
            if case["allowed"]:
                continue
            with self.subTest(argv=case["argv"]), patch.dict(os.environ, {cli.PROVIDER_WORKER_ENV: "1"}), \
                    patch.object(sys, "argv", ["makewand", *case["argv"]]), \
                    patch.object(cli, "delegate_to_go_server") as delegate, \
                    contextlib.redirect_stderr(io.StringIO()) as stderr:
                with self.assertRaises(SystemExit) as raised:
                    cli.main()
                self.assertEqual(raised.exception.code, 2)
                self.assertIn("只读", stderr.getvalue())
                delegate.assert_not_called()

    def test_static_queries_delegate_with_literal_arguments(self):
        for argv in (["help"], ["completion", "bash"], ["completion", "fish", "--no-descriptions"]):
            with self.subTest(argv=argv), patch.dict(os.environ, {cli.PROVIDER_WORKER_ENV: "1"}), \
                    patch.object(sys, "argv", ["makewand", *argv]), \
                    patch.object(cli, "delegate_to_go_server", return_value=0) as delegate:
                self.assertEqual(cli.main(), 0)
                delegate.assert_called_once_with(argv)

    def test_worker_delegation_never_builds_a_dev_cache(self):
        with patch.dict(os.environ, {cli.PROVIDER_WORKER_ENV: "1", "MAKEWAND_DEV": "1"}), \
                patch.object(cli, "_find_go_binary", return_value=None), \
                patch.object(cli, "_build_dev_go_binary") as build, \
                contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                cli.delegate_to_go_server(["completion", "bash"])
            build.assert_not_called()

    def test_repository_queries_have_no_config_budget_or_daemon_side_effects(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            repo = root / "repo"
            repo.mkdir()
            source = repo / "sample.py"
            source.write_text("def fixture_needle():\n    return 1\n")
            config = root / "config"
            # Inherited switches must not activate the daemon or initialize a budget.
            environment = {cli.PROVIDER_WORKER_ENV: "1", "MAKEWAND_CONFIG_DIR": str(config),
                           "MAKEWAND_DAEMON": "1", "MAKEWAND_MAX_MODEL_CALLS": "1",
                           "MAKEWAND_CALL_BUDGET_FILE": str(root / "budget.json"),
                           "PYTHONDONTWRITEBYTECODE": "1"}
            for argv in (["-C", str(repo), "search", "fixture_needle"],
                         ["repomap", "--cwd=" + str(repo), "--json", "--max-lines=5"]):
                with self.subTest(argv=argv), patch.dict(os.environ, environment):
                    result = _run(argv, worker=True)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertIn("fixture_needle", result.stdout)
                    self.assertFalse(config.exists())
                    self.assertFalse((root / "budget.json").exists())
                    self.assertEqual(list(repo.iterdir()), [source])

    def test_guard_does_not_trust_a_command_hint_or_abbreviated_flag(self):
        with patch.dict(os.environ, {cli.PROVIDER_WORKER_ENV: "1"}):
            self.assertIsNotNone(cli._provider_worker_refusal("help", ["--cwd", "help", "state", "backup"]))
            self.assertIsNotNone(cli._provider_worker_refusal("search", ["search", "x", "--prob"]))

    def test_worker_repomap_does_not_execute_git_fsmonitor_hook(self):
        if os.name == "nt" or not shutil.which("git"):
            self.skipTest("POSIX fake Git hook fixture")
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory)
            subprocess.run(["git", "init", "-q", str(repo)], check=True, capture_output=True)
            (repo / "sample.py").write_text("def fixture_symbol():\n    return 1\n")
            subprocess.run(["git", "-C", str(repo), "add", "sample.py"], check=True, capture_output=True)
            sentinel = repo / "hook-ran"
            hook = repo / "fake-hook"
            hook.write_text('#!/bin/sh\nprintf invoked > "' + str(sentinel) + '"\n')
            hook.chmod(0o700)
            subprocess.run(["git", "-C", str(repo), "config", "core.fsmonitor", str(hook)],
                           check=True, capture_output=True)
            before = {str(p.relative_to(repo)): p.read_bytes() for p in repo.rglob("*") if p.is_file()}
            with patch.dict(os.environ, {"PYTHONDONTWRITEBYTECODE": "1"}):
                result = _run(["repomap", "-C", str(repo), "--json"], worker=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("fixture_symbol", result.stdout)
            self.assertFalse(sentinel.exists())
            after = {str(p.relative_to(repo)): p.read_bytes() for p in repo.rglob("*") if p.is_file()}
            self.assertEqual(before, after)

    def test_worker_search_does_not_execute_ripgrep_preprocessor(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            repo = root / "repo"
            repo.mkdir()
            (repo / "sample.py").write_text("fixture_needle\n")
            sentinel = root / "preprocessor-ran"
            hook = root / "fake-preprocessor"
            hook.write_text('#!/bin/sh\nprintf invoked > "' + str(sentinel) + '"\n')
            hook.chmod(0o700)
            rg_config = root / "ripgrep.conf"
            rg_config.write_text("--pre=" + str(hook) + "\n")
            with patch.dict(os.environ, {"RIPGREP_CONFIG_PATH": str(rg_config), "PYTHONDONTWRITEBYTECODE": "1"}):
                result = _run(["search", "fixture_needle", "-C", str(repo)], worker=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("fixture_needle", result.stdout)
            self.assertFalse(sentinel.exists())
            from makewand.search import safe_search
            with patch.dict(os.environ, {cli.PROVIDER_WORKER_ENV: "1"}), \
                    patch("makewand.search.subprocess.Popen") as spawn:
                self.assertTrue(safe_search("fixture_needle", root_path=str(repo)))
                spawn.assert_not_called()

    def test_extra_env_reaches_the_child_process(self):
        # The host launch passes the marker through the environment, not an
        # `env` prefix (absent on Windows).
        from makewand.providers.base import run_subprocess
        code, out, _err, _ex = run_subprocess(
            [sys.executable, "-I", "-c", "import os; print(os.environ.get('MAKEWAND_PROVIDER_WORKER'))"],
            timeout=30, extra_env={cli.PROVIDER_WORKER_ENV: "1"})
        self.assertEqual((code, out.strip()), (0, "1"))

    def test_guard_inactive_without_marker(self):
        r = _run(["run", "--help"], worker=False)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertNotIn("递归", r.stderr)


if __name__ == "__main__":
    unittest.main()
