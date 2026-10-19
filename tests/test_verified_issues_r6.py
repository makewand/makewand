"""Round six regressions: temporary homes and offline admission only."""
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

from makewand import call_budget as budget, sandbox
from makewand.account_identity import credential_binding, verify_credential_copy
from makewand.execution_contract import ExecutionRequest
from makewand.execution_runtime import _context, execute
try:
    from test_verified_issues_r2 import isolated
except ImportError:
    from tests.test_verified_issues_r2 import isolated


@pytest.mark.parametrize("location", ["workspace", "home", "alias"])
@pytest.mark.parametrize("error_code", [errno.ENOENT, errno.ENOTDIR, errno.EACCES])
def test_foreign_missing_root_in_writable_mount_fails_closed(isolated, monkeypatch, location, error_code):
    work = isolated.parent / "work"
    work.mkdir()
    base = work if location != "home" else isolated
    if location == "alias":
        base = isolated.parent / "alias"
        base.symlink_to(work, target_is_directory=True)
    selected = base / "missing" / "account"
    monkeypatch.setenv("CODEX_HOME", str(selected))
    real_selected = str(selected.resolve())
    original_stat = os.stat
    def inspect(path, *args, **kwargs):
        if str(path) in (str(selected), real_selected):
            raise OSError(error_code, "fixture")
        return original_stat(path, *args, **kwargs)
    with patch.object(sandbox, "resolve_trusted_bwrap", return_value="/usr/bin/bwrap"), \
            patch.object(sandbox.os, "stat", side_effect=inspect), \
            pytest.raises(sandbox.SandboxConfigError, match="CODEX_HOME"):
        sandbox.wrap_bwrap(["/usr/bin/true"], str(work), readonly=False)


def test_foreign_missing_root_outside_writable_mount_skipped(isolated, monkeypatch):
    work = isolated.parent / "work"
    work.mkdir()
    selected = isolated.parent / "unmounted" / "account"
    monkeypatch.setenv("CODEX_HOME", str(selected))
    with patch.object(sandbox, "resolve_trusted_bwrap", return_value="/usr/bin/bwrap"):
        argv = sandbox.wrap_bwrap(["/usr/bin/true"], str(work), readonly=False)
    assert str(selected) not in argv
    assert "CODEX_HOME" not in argv


@pytest.mark.parametrize("known", [False, True])
def test_claude_binding_ignores_normal_state_writes(isolated, known):
    root = isolated / ".claude"
    root.mkdir()
    (root / ".credentials.json").write_text('{"fixture":"same"}')
    state = isolated / ".claude.json"
    data = {"oauthAccount": {"accountUuid": "u-1"}} if known else {}
    state.write_text(json.dumps(data))
    binding = credential_binding("claude")
    data.update(numStartups=100, projects={"/x": {"lastCost": 3}})
    state.write_text(json.dumps(data))
    assert credential_binding("claude") == binding
    token = _context.set({"_credential_binding": binding})
    try:
        verify_credential_copy(str(state), state.read_bytes())
        with patch.object(sandbox, "is_bwrap_available", return_value=False), \
                patch.object(sandbox, "authorize_unsandboxed_launch", return_value="fixture"):
            with sandbox.provider_sandbox_launch("claude", ["fixture"], str(isolated.parent), readonly=True):
                data["numStartups"] += 1
                state.write_text(json.dumps(data))
    finally:
        _context.reset(token)


@pytest.mark.parametrize("change", ["account", "credentials"])
def test_claude_launch_refusal_does_not_spend(isolated, change):
    root = isolated / ".claude"
    root.mkdir()
    creds = root / ".credentials.json"
    state = isolated / ".claude.json"
    creds.write_text('{"fixture":"before"}')
    state.write_text('{"oauthAccount":{"accountUuid":"before"}}')
    def callback(timeout):
        if change == "account":
            state.write_text('{"oauthAccount":{"accountUuid":"after"}}')
        else:
            creds.write_text('{"fixture":"after"}')
        with sandbox.provider_sandbox_launch("claude", ["fixture"], str(isolated.parent), readonly=True):
            pytest.fail("changed login launched")
    with patch.object(sandbox, "is_bwrap_available", return_value=False), \
            patch.object(sandbox, "authorize_unsandboxed_launch", return_value="fixture"):
        result = execute(ExecutionRequest(engine="claude", task_id="switch", stage="test"), callback)
    assert result.error_kind == "account_configuration"
    data = json.loads(Path(os.environ["MAKEWAND_CALL_BUDGET_FILE"]).read_text())
    assert data["attempts"][0]["status"] == "released"
    assert not data["leases"]
    assert data["scopes"]["tasks"]["switch"]["used"] == 0


def test_ledger_long_running_size_is_bounded(isolated):
    path = Path(os.environ["MAKEWAND_CALL_BUDGET_FILE"])
    base = 1800000000
    sizes = []
    for day in range(14):
        with patch.object(budget.time, "time", return_value=base + day * 86400):
            for slot in range(3):
                attempt = budget.reserve("codex", "fast", task_id=f"day-{day}-{slot}")
                budget.complete(attempt, True, 0)
        sizes.append(path.stat().st_size)
    data = json.loads(path.read_text())
    assert len(data["attempts"]) <= 9
    assert len(data["scopes"]["tasks"]) <= 9
    assert len(data["scopes"]["machine"]) <= 3
    assert max(sizes[4:]) < 16000
    assert data["scopes"]["machine"][data["scopes"]["day"]]["used"] == 3


def test_concurrency_limit_can_increase_after_quiescence(isolated, monkeypatch):
    first = budget.reserve("codex", "fast", task_id="first")
    budget.complete(first, True, 0)
    monkeypatch.setenv("MAKEWAND_ACCOUNT_CONCURRENCY", "2")
    monkeypatch.setenv("MAKEWAND_POOL_CONCURRENCY", "2")
    second = budget.reserve("codex", "fast", task_id="second")
    try:
        third = budget.reserve("codex", "fast", task_id="third", wait_for_capacity=False)
        budget.complete(third, True, 0)
    finally:
        budget.complete(second, True, 0)


def test_compaction_keeps_lifetime_charges(isolated, monkeypatch):
    monkeypatch.setenv("MAKEWAND_LAYERED_BUDGET", "")
    monkeypatch.delenv("MAKEWAND_MACHINE_DAILY_CALLS")
    monkeypatch.delenv("MAKEWAND_PROJECT_DAILY_CALLS")
    monkeypatch.setenv("MAKEWAND_MAX_MODEL_CALLS", "2")
    base = 1800000000
    for slot in range(2):
        with patch.object(budget.time, "time", return_value=base):
            attempt = budget.reserve("codex", "fast", task_id=f"legacy-{slot}")
            budget.complete(attempt, True, 0)
    with patch.object(budget.time, "time", return_value=base + 4 * 86400):
        with budget._ledger() as (path, data):
            budget._save(path, data)
        assert budget.remaining_capacity() == 0
        with pytest.raises(budget.BudgetError, match="exhausted"):
            budget.reserve("codex", "fast")
    data = json.loads(Path(os.environ["MAKEWAND_CALL_BUDGET_FILE"]).read_text())
    assert not data["attempts"] and data["archived_used"] == 2


def test_compaction_keeps_active_lease_holds_and_recent_charge(isolated):
    from makewand import ledger_compaction
    base = 1800000000
    with patch.object(budget.time, "time", return_value=base):
        live = budget.reserve("codex", "fast", task_id="live")
        old = budget.reserve("grok", "fast", task_id="old")
        budget.complete(old, True, 0)
    with patch.object(budget.time, "time", return_value=base + 4 * 86400):
        with budget._ledger() as (path, data):
            live_entry = next(e for e in data["attempts"] if e["id"] == live)
            keys = live_entry["scopes"]
            # A still-running worker can outlive retention, even after a
            # recorded outcome; its lease and all counters must survive.
            live_entry.update(status="completed", finished_at=base)
            data["holds"] = {"fixture": {"remaining": 1, "expires_at": base + 5 * 86400, "scopes": keys}}
            budget._save(path, data)
            ledger_compaction.compact(data)  # repeated writes are idempotent
            assert [e["id"] for e in data["attempts"]] == [live]
            assert live in data["leases"] and "fixture" in data["holds"]
            for group, key in keys:
                assert key in data["scopes"][group]
    budget.complete(live, True, 0)


def test_materialize_allows_claude_state_changes(isolated):
    root = isolated / ".claude"
    root.mkdir()
    (root / ".credentials.json").write_text('{}')
    state = isolated / ".claude.json"
    state.write_text('{"oauthAccount":{"accountUuid":"same"},"numStartups":1}')
    binding = credential_binding("claude")
    state.write_text('{"oauthAccount":{"accountUuid":"same"},"numStartups":2}')
    work = isolated.parent / "work"
    work.mkdir()
    token = _context.set({"_credential_binding": binding})
    home = sandbox.ProviderHome("claude", str(isolated))
    try:
        home.materialize([str(work)])
        assert home.private_dir is not None
    finally:
        home.finalize()
        _context.reset(token)


def test_account_refusal_after_invocation_is_never_refunded(isolated):
    from makewand.account_identity import refuse_credential_launch
    from makewand.execution_runtime import mark_provider_invocation
    def callback(timeout):
        mark_provider_invocation()
        refuse_credential_launch("late account check")
    result = execute(ExecutionRequest(engine="claude", task_id="invoked", stage="test"), callback)
    assert result.status == "UNKNOWN"
    data = json.loads(Path(os.environ["MAKEWAND_CALL_BUDGET_FILE"]).read_text())
    assert data["attempts"][0]["status"] == "completed"
    assert data["scopes"]["tasks"]["invoked"]["used"] == 1


def test_compaction_upgrades_old_go_completion_timestamp(isolated):
    from makewand import ledger_compaction
    now = 1800000000
    data = {"schema": 1, "maximum": 10, "attempts": [
        {"id": "old-go", "status": "completed", "started_at": now - 4*86400, "seconds": 1},
        {"id": "long-job", "status": "completed", "started_at": now - 4*86400, "seconds": 4*86400},
        {"id": "unknown-finish", "status": "released", "started_at": now - 4*86400},
    ]}
    with patch.object(budget.time, "time", return_value=now):
        ledger_compaction.compact(data)
    assert [e["id"] for e in data["attempts"]] == ["long-job", "unknown-finish"]
    assert data["archived_used"] == 1
    assert all(e["finished_at"] == now for e in data["attempts"])
