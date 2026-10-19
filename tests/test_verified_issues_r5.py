"""Round-5 regressions use temporary homes, fake credentials and offline workers."""
try:
    import _isolation
except ImportError:
    from tests import _isolation

import errno
import json
import os
from pathlib import Path
from unittest.mock import patch

import pytest

from makewand import call_budget as budget, orchestrator as orch, sandbox
from makewand.account_identity import credential_binding
from makewand.execution_contract import ExecutionRequest
from makewand.execution_runtime import _context, execute
try:
    from test_verified_issues_r2 import isolated
except ImportError:
    from tests.test_verified_issues_r2 import isolated


@pytest.mark.parametrize("explicit", [False, True])
def test_direct_codex_preserves_all_user_configuration(isolated, monkeypatch, explicit):
    work = isolated.parent / "work"
    work.mkdir()
    root = isolated / ("custom" if explicit else ".codex")
    root.mkdir()
    files = ["config.toml", "AGENTS.md", "AGENTS.override.md", "skills/guard/SKILL.md",
             "rules/default.rules", "hooks/guard.sh", "hooks.json", "prompts/review.md",
             "policy/default", "plugins/default", "memories/default"]
    for name in files + ["auth.json"]:
        target = root / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text('{}' if name == "auth.json" else "user configuration")
    if explicit:
        monkeypatch.setenv("CODEX_HOME", str(root))
    with patch.object(sandbox, "is_bwrap_available", return_value=False), \
            patch.object(sandbox, "authorize_unsandboxed_launch", return_value="fixture"):
        with sandbox.provider_sandbox_launch("codex", ["fixture"], str(work), readonly=False) as launch:
            effective = Path(launch.env.get("CODEX_HOME") or os.environ.get("CODEX_HOME") or isolated / ".codex")
            for name in files:
                assert (effective / name).read_text() == "user configuration"
            assert effective == root


@pytest.mark.parametrize("explicit", [False, True])
def test_direct_codex_account_switch_has_unknown_outcome(isolated, monkeypatch, explicit):
    root = isolated / ("custom" if explicit else ".codex")
    root.mkdir()
    auth = root / "auth.json"
    auth.write_text('{"tokens":{"account_id":"before"}}')
    if explicit:
        monkeypatch.setenv("CODEX_HOME", str(root))

    def worker(timeout):
        with sandbox.provider_sandbox_launch("codex", ["fixture"], str(isolated.parent), readonly=True):
            auth.write_text('{"tokens":{"account_id":"after"}}')
        return True, "cannot attribute", None

    with patch.object(sandbox, "is_bwrap_available", return_value=False), \
            patch.object(sandbox, "authorize_unsandboxed_launch", return_value="fixture"):
        result = execute(ExecutionRequest(engine="codex", task_id="switch", stage="test"), worker)
    assert result.status == "UNKNOWN" and not result.outcome_known


def test_direct_codex_rejects_login_changed_since_admission(isolated):
    root = isolated / ".codex"
    root.mkdir()
    auth = root / "auth.json"
    auth.write_text('{"tokens":{"account_id":"before"}}')
    token = _context.set({"_credential_binding": credential_binding("codex")})
    auth.write_text('{"tokens":{"account_id":"after"}}')
    try:
        with patch.object(sandbox, "is_bwrap_available", return_value=False), \
                patch.object(sandbox, "authorize_unsandboxed_launch", return_value="fixture"), \
                pytest.raises(sandbox.SandboxConfigError, match="selected login changed"):
            with sandbox.provider_sandbox_launch("codex", ["fixture"], str(isolated.parent), readonly=True):
                pytest.fail("changed account launched")
    finally:
        _context.reset(token)


@pytest.mark.parametrize("variable", ["CODEX_HOME", "HOME"])
def test_direct_codex_extra_env_cannot_switch_account_root(isolated, variable):
    other = isolated.parent / "other"
    other.mkdir()
    with patch.object(sandbox, "is_bwrap_available", return_value=False), \
            patch.object(sandbox, "authorize_unsandboxed_launch", return_value="fixture"), \
            pytest.raises(sandbox.SandboxConfigError, match="credential source differs"):
        with sandbox.provider_sandbox_launch("codex", ["fixture"], str(isolated.parent), readonly=True,
                                             extra_env={variable: str(other)}):
            pytest.fail("child environment switched admitted account")


def test_direct_codex_relative_root_cannot_switch_with_child_cwd(isolated, monkeypatch):
    monkeypatch.chdir(isolated.parent)
    root = isolated.parent / "login"
    work = isolated.parent / "work"
    for selected in (root, work / "login"):
        selected.mkdir(parents=True)
        (selected / "auth.json").write_text('{"tokens":{"account_id":"' + selected.parent.name + '"}}')
    monkeypatch.setenv("CODEX_HOME", "login")
    with patch.object(sandbox, "is_bwrap_available", return_value=False), \
            patch.object(sandbox, "authorize_unsandboxed_launch", return_value="fixture"), \
            pytest.raises(sandbox.SandboxConfigError, match="credential source differs"):
        with sandbox.provider_sandbox_launch("codex", ["fixture"], str(work), readonly=True):
            pytest.fail("relative root selected a different child account")


@pytest.mark.parametrize("known", [False, True])
def test_direct_codex_credential_rotation_keeps_only_known_account_attribution(isolated, known):
    root = isolated / ".codex"
    root.mkdir()
    auth = root / "auth.json"
    tokens = {"account_id": "same"} if known else {}
    auth.write_text(json.dumps({"tokens": {**tokens, "access_token": "fixture-before"}}))
    entered = []

    def worker(timeout):
        with sandbox.provider_sandbox_launch("codex", ["fixture"], str(isolated.parent), readonly=True):
            entered.append(True)
            auth.write_text(json.dumps({"tokens": {**tokens, "access_token": "fixture-after"}}))
        return True, "answer", None

    with patch.object(sandbox, "is_bwrap_available", return_value=False), \
            patch.object(sandbox, "authorize_unsandboxed_launch", return_value="fixture"):
        result = execute(ExecutionRequest(engine="codex", task_id="rotation", stage="test"), worker)
    assert entered == [True]
    assert result.status == ("PASSED" if known else "UNKNOWN")


def test_busy_dispatch_does_not_penalize_reliability(isolated):
    holder = budget.reserve("codex", "fast", task_id="holder")
    try:
        with patch("makewand.usage.record_engine_usage") as record:
            result = orch._stage_call("test", orch.dispatch_task, "codex", "fixture",
                                     cwd=str(isolated.parent), timeout=1, wait_for_capacity=False)
        assert result.error_kind == "concurrency_busy"
        record.assert_not_called()
    finally:
        budget.complete(holder, True, 0)


@pytest.mark.parametrize("success", [False, True])
def test_actual_dispatch_still_records_reliability(isolated, success):
    with patch.object(orch, "execute_codex_task", return_value=(success, "output", None if success else "failure")), \
            patch("makewand.usage.record_engine_usage") as record:
        result = orch.dispatch_task("codex", "fixture", cwd=str(isolated.parent), timeout=1)
    record.assert_called_once()
    assert record.call_args.kwargs["success"] is success


@pytest.mark.parametrize("variable", ["CODEX_HOME", "CLAUDE_CONFIG_DIR"])
@pytest.mark.parametrize("provider", [None, "grok"])
@pytest.mark.parametrize("error_code", [errno.ENOENT, errno.ENOTDIR, errno.EACCES])
def test_inaccessible_foreign_root_does_not_block_other_work(isolated, monkeypatch, variable, provider, error_code):
    work = isolated.parent / "work"
    work.mkdir()
    root = isolated.parent / "foreign"
    root.mkdir()
    monkeypatch.setenv(variable, str(root))
    original_stat = os.stat

    def inspect(path, *args, **kwargs):
        if str(path) == str(root):
            raise OSError(error_code, "stale foreign path", str(path))
        return original_stat(path, *args, **kwargs)

    with patch.object(sandbox, "resolve_trusted_bwrap", return_value="/usr/bin/bwrap"), \
            patch.object(sandbox.os, "stat", side_effect=inspect):
        argv = sandbox.wrap_bwrap(["/usr/bin/true"], str(work), readonly=True,
                                  is_provider=provider is not None, provider_name=provider)
    assert argv[-1] == "/usr/bin/true" and str(root) not in argv


def test_unexpected_foreign_root_io_error_still_fails_closed(isolated, monkeypatch):
    root = isolated.parent / "foreign"
    root.mkdir()
    work = isolated.parent / "work"
    work.mkdir()
    monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(root))
    original_stat = os.stat

    def inspect(path, *args, **kwargs):
        if str(path) == str(root):
            raise OSError(errno.EIO, "unknown filesystem state")
        return original_stat(path, *args, **kwargs)

    with patch.object(sandbox, "resolve_trusted_bwrap", return_value="/usr/bin/bwrap"), \
            patch.object(sandbox.os, "stat", side_effect=inspect), \
            pytest.raises(sandbox.SandboxConfigError, match="CLAUDE_CONFIG_DIR"):
        sandbox.wrap_bwrap(["/usr/bin/true"], str(work), readonly=True)


@pytest.mark.parametrize("value,expected", [("", 30), (" 5\t", 5), (".5", .5),
    ("+1e-2", .01), ("-0", 0), ("9223372036", 9223372036)])
def test_lease_wait_common_decimal_values(monkeypatch, value, expected):
    monkeypatch.setenv("MAKEWAND_LEASE_WAIT_SECONDS", value)
    assert budget.lease_wait_seconds() == expected


@pytest.mark.parametrize("value", [" ", "invalid", "nan", "Inf", "-1", "1e309", "1e100",
    "9223372036.854776", "1_0", "５", "0x1p0", "\u00a05"])
def test_invalid_lease_wait_is_internal_error_without_spending(isolated, monkeypatch, value):
    monkeypatch.setenv("MAKEWAND_LEASE_WAIT_SECONDS", value)
    result = execute(ExecutionRequest(engine="codex", task_id="bad-setting", stage="test"),
                     lambda timeout: pytest.fail("invalid configuration launched worker"))
    assert result.status == "INTERNAL_ERROR" and result.error_kind == "budget_configuration"
    path = Path(os.environ["MAKEWAND_CALL_BUDGET_FILE"])
    assert not path.exists() or not json.loads(path.read_text())["attempts"]
