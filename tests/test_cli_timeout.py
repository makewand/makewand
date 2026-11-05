"""Parse real CLI arguments without discovery, dispatch or ledger access."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import sys
import io
from unittest import mock

import pytest

from makewand import cli


class Parsed(Exception):
    pass


def parsed_timeout(argv):
    resolve = cli._resolve_direct_timeout

    def capture(args):
        resolve(args)
        raise Parsed(args)

    with mock.patch.object(sys, "argv", ["makewand", *argv]), \
            mock.patch.object(cli, "_resolve_direct_timeout", capture):
        with pytest.raises(Parsed) as caught:
            cli._main()
    return caught.value.args[0].timeout


@pytest.mark.parametrize("provider", sorted(cli.TIMEOUT_DEFAULTS))
@pytest.mark.parametrize("options, expected", [
    ([], None),
    (["--total-timeout", "1800"], 1800),
    (["--total-timeout", "0.5"], 0.5),
    (["--timeout", "600"], 600),
    (["--timeout", "300", "--total-timeout", "1800"], 300),
    (["--timeout", "1800", "--total-timeout", "600"], 600),
    (["--timeout", "600", "--total-timeout", "600"], 600),
])
def test_direct_cli_timeout(provider, options, expected):
    assert parsed_timeout([provider, *options]) == (cli.TIMEOUT_DEFAULTS[provider] if expected is None else expected)


@pytest.mark.parametrize("provider", sorted(cli.TIMEOUT_DEFAULTS))
def test_direct_cli_timeout_global_total(provider):
    assert parsed_timeout(["--total-timeout", "2400", provider]) == 2400


@pytest.mark.parametrize("command, default", [
    ("review", 300), ("sandbox", 120),
])
def test_other_cli_timeout_defaults_unchanged(command, default):
    assert parsed_timeout([command]) == default
    assert parsed_timeout([command, "--total-timeout", "1800"]) == (default if command == "sandbox" else 1800)


def test_timeout_parsers_do_not_drift():
    import argparse

    def capture(parser, *args, **kwargs):
        for action in parser._actions:
            if isinstance(action, argparse._SubParsersAction):
                found = {name for name, sub in action.choices.items()
                         if any(a.dest == "timeout" and a.default is None for a in sub._actions)}
                assert found == set(cli.TIMEOUT_DEFAULTS)
        raise Parsed()

    with mock.patch.object(sys, "argv", ["makewand", "review"]), \
            mock.patch.object(argparse.ArgumentParser, "parse_args", capture):
        with pytest.raises(Parsed):
            cli._main()


@pytest.mark.parametrize("value", ["0", "-1", "nan", "inf"])
def test_invalid_total_names_correct_option(value, capsys):
    with mock.patch.object(sys, "argv", ["makewand", "codex", "task", "--total-timeout", value]):
        with pytest.raises(SystemExit) as caught:
            cli._main()
    assert caught.value.code == 2
    assert "--total-timeout must be a finite positive number" in capsys.readouterr().err


@pytest.mark.parametrize("deadline,expected", [(20000000, "long_running"), (1000000, "hung_anomaly")])
def test_observer_respects_task_deadline(deadline, expected):
    from makewand import observer
    process = dict(pid=123, comm="worker", etimes=4000, args="worker", makewand_dispatched=True)
    with mock.patch.object(observer, "_read_proc_environ", return_value={observer.DEADLINE_ENV_VAR: str(deadline)}), \
            mock.patch.object(observer.time, "time", return_value=10000):
        assert observer.classify_operation("session", ["running"], {}, process)[0] == expected


@pytest.mark.parametrize("deadline,eligible", [(20000000, False), (1000000, True)])
def test_observer_cleanup_respects_task_deadline(deadline, eligible):
    from makewand import observer
    stat = dict(pid=123, comm="worker", ppid=1, pgrp=123, tty_nr=0, starttime=10)
    env = {observer.DEADLINE_ENV_VAR: str(deadline), observer.DISPATCH_ENV_VAR: "999-other"}
    with mock.patch.object(observer, "_list_pids", return_value=[123]), \
            mock.patch.object(observer, "_read_proc_stat", return_value=stat), \
            mock.patch.object(observer, "_read_proc_environ", return_value=env), \
            mock.patch.object(observer, "_own_ancestors", return_value=set()), \
            mock.patch.object(observer, "_process_age_seconds", return_value=4000), \
            mock.patch.object(observer, "_read_proc_cmdline", return_value="worker"), \
            mock.patch.object(observer.time, "time", return_value=10000), \
            mock.patch.object(observer.os, "killpg") as kill:
        candidates = observer.find_makewand_dispatched_processes()
        assert bool(candidates) == eligible
        stale = [dict(stat, dispatch_id="999-other")]
        assert bool(observer.terminate_dispatched_processes(stale, [123])) == eligible
        assert kill.called == eligible


def test_dispatch_marker_exports_deadline(monkeypatch):
    from makewand import observer
    monkeypatch.setenv(observer.DISPATCH_ENV_VAR, "test-original")
    monkeypatch.setenv(observer.DEADLINE_ENV_VAR, "old")
    observer.mark_process_as_dispatcher(deadline_unix_ms=123456)
    assert observer.os.environ[observer.DEADLINE_ENV_VAR] == "123456"
    observer.mark_process_as_dispatcher()
    assert observer.DEADLINE_ENV_VAR not in observer.os.environ


def test_cli_exports_deadline_before_dispatch(monkeypatch):
    from makewand import observer
    from makewand.execution_runtime import current_context
    monkeypatch.setenv(observer.DISPATCH_ENV_VAR, "test-original")
    monkeypatch.setenv(observer.DEADLINE_ENV_VAR, "0")
    monkeypatch.setattr(sys, "argv", ["makewand", "review", "--total-timeout", "5400"])

    def inspect(*args):
        assert int(observer.os.environ[observer.DEADLINE_ENV_VAR]) == current_context()["deadline_unix_ms"]

    with mock.patch.object(cli, "_dispatch_parsed_command", side_effect=inspect):
        cli._main()
