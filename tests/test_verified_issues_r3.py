"""Round-3 admission and credential regressions; no real provider calls."""
try:
    import _isolation
except ImportError:
    from tests import _isolation

import contextlib
import io
import json
import os
import re
import threading
import time
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest

from makewand import account_leases as leases, call_budget as budget, sandbox
from makewand.execution_contract import ExecutionRequest
from makewand.execution_runtime import execute
try:
    from test_verified_issues_r2 import isolated
except ImportError:  # python3 -m unittest tests.<module>
    from tests.test_verified_issues_r2 import isolated


def test_default_busy_wait_admits_within_cap(isolated, monkeypatch):
    monkeypatch.delenv("MAKEWAND_LEASE_WAIT_SECONDS")
    clock = [100.0]

    def reserve_once(*args, **kwargs):
        if clock[0] < 105:
            raise budget.ConcurrencyBusyError("busy")
        return "admitted"

    with patch.object(budget, "time", SimpleNamespace(monotonic=lambda: clock[0],
            sleep=lambda seconds: clock.__setitem__(0, clock[0] + seconds))), \
            patch.object(budget, "_reserve_once", side_effect=reserve_once):
        assert budget.reserve("agy", "fast", deadline_monotonic=160) == "admitted"
    assert 105 <= clock[0] < 130


@pytest.mark.parametrize("cap,deadline,expected", [(None, None, 30), ("2", 160, 2), ("80", 101, .5)])
def test_wait_cap_and_no_deadline_fallback(isolated, monkeypatch, cap, deadline, expected):
    if cap is None:
        monkeypatch.delenv("MAKEWAND_LEASE_WAIT_SECONDS")
    else:
        monkeypatch.setenv("MAKEWAND_LEASE_WAIT_SECONDS", cap)
    clock = [100.0]
    error = budget.ConcurrencyBusyError
    with patch.object(budget, "time", SimpleNamespace(monotonic=lambda: clock[0],
            sleep=lambda seconds: clock.__setitem__(0, clock[0] + seconds))), \
            patch.object(budget, "_reserve_once", side_effect=budget.ConcurrencyBusyError("busy")):
        with pytest.raises(error):
            budget.reserve("agy", "fast", deadline_monotonic=deadline)
    assert clock[0] == pytest.approx(100 + expected)


def test_busy_wait_is_nonterminal_without_spending(isolated, monkeypatch):
    monkeypatch.delenv("MAKEWAND_LEASE_WAIT_SECONDS")
    first = budget.reserve("agy", "fast", task_id="holder")
    try:
        result = execute(ExecutionRequest(engine="agy", task_id="waiter", stage="test", timeout_ms=100),
                         lambda timeout: pytest.fail("busy worker must not launch"))
        assert result.status == "FAILED"
        assert result.error_kind == "concurrency_busy"
        assert result.outcome_known
        assert len(json.loads(Path(os.environ["MAKEWAND_CALL_BUDGET_FILE"]).read_text())["attempts"]) == 1
    finally:
        budget.complete(first, True, 0)


@pytest.mark.parametrize("provider", ["grok", "agy", "muse", "codex", "claude", None])
def test_foreign_account_homes_masked_after_private_mounts(isolated, monkeypatch, provider):
    work = isolated.parent / "work"
    work.mkdir()
    roots = {name: isolated.parent / name for name in ("codex", "claude")}
    for name, root in roots.items():
        root.mkdir()
        monkeypatch.setenv("CODEX_HOME" if name == "codex" else "CLAUDE_CONFIG_DIR", str(root))
    # Make the fixture paths visible through the root bind, independently of
    # the machine's masked-root policy. Test the mount builder, not bwrap itself.
    with patch.object(sandbox, "resolve_trusted_bwrap", return_value="/usr/bin/bwrap"), \
            patch.object(sandbox, "_effective_masked_roots", return_value=[]):
        with sandbox.provider_home_session(provider or "grok", [str(work)]) as home:
            argv = sandbox.wrap_bwrap(["/usr/bin/true"], str(work), readonly=True,
                provider_name=provider, is_provider=provider is not None,
                provider_home=home if provider else None)
    for name, root in roots.items():
        if name == provider:
            assert any(argv[i] == "--bind" and argv[i + 2] == str(root)
                       for i in range(len(argv) - 2))
            assert not any(argv[i:i + 2] == ["--tmpfs", str(root)] for i in range(len(argv) - 1))
        else:
            assert any(argv[i:i + 2] == ["--tmpfs", str(root)] for i in range(len(argv) - 1))
            assert any(argv[i:i + 2] == ["--remount-ro", str(root)] for i in range(len(argv) - 1))


@pytest.mark.parametrize("layout", ["broad", "workspace", "worktree", "overlapping-account"])
def test_unsafe_foreign_account_home_fails_closed(isolated, monkeypatch, layout):
    work = isolated.parent / "work"
    work.mkdir()
    foreign = {"broad": "/tmp", "workspace": str(work), "worktree": str(work / "account"),
               "missing": str(isolated.parent / "missing"), "overlapping-account": str(isolated.parent / "accounts")}[layout]
    if layout == "worktree":
        Path(foreign).mkdir()
    if layout == "overlapping-account":
        active = Path(foreign) / "codex"
        active.mkdir(parents=True)
        monkeypatch.setenv("CODEX_HOME", str(active))
    monkeypatch.setenv("CLAUDE_CONFIG_DIR", foreign)
    with patch.object(sandbox, "resolve_trusted_bwrap", return_value="/usr/bin/bwrap"):
        with pytest.raises(sandbox.SandboxConfigError, match="CLAUDE_CONFIG_DIR"):
            sandbox.wrap_bwrap(["/usr/bin/true"], str(work), readonly=True,
                               is_provider=True, provider_name="codex")


def test_foreign_symlink_alias_and_real_target_masked(isolated, monkeypatch):
    target = isolated.parent / "stored-account"
    target.mkdir()
    alias = isolated.parent / "alias"
    alias.symlink_to(target, target_is_directory=True)
    work = isolated.parent / "work"
    work.mkdir()
    monkeypatch.setenv("CODEX_HOME", str(alias))
    with patch.object(sandbox, "resolve_trusted_bwrap", return_value="/usr/bin/bwrap"), \
            patch.object(sandbox, "_effective_masked_roots", return_value=[]):
        argv = sandbox.wrap_bwrap(["/usr/bin/true"], str(work), readonly=True)
    for path in (alias, target):
        assert any(argv[i:i + 2] == ["--tmpfs", str(path)] for i in range(len(argv) - 1))


@pytest.mark.parametrize("name", ["antigravity-api", "ANTIGRAVITY-API", "antigravity-api:api"])
def test_antigravity_api_sibling_has_canonical_lease_keys(isolated, name):
    assert leases.keys(name, name.removesuffix(":api") + ":api:fixture") == leases.keys("agy", "agy:api:fixture")


@pytest.mark.parametrize("snapshot", [(True, "owner"), (None, None)])
def test_windows_marked_worker_kept_for_live_or_uninspectable_owner(isolated, snapshot):
    from makewand import lease_process
    worker = {"pid": 200, "token": "worker", "boot": "windows", "job": "owner-kill-on-close"}
    owner = {"pid": 100, "token": "owner", "boot": "windows"}
    data = {"leases": {"old": {"keys": leases.keys("codex", None), "owner": owner,
            "workers": [worker], "launch_pending": False, "expires_at": 0}}}
    with patch.object(leases.sys, "platform", "win32"), \
            patch.object(lease_process, "_windows_snapshot", side_effect=lambda pid: snapshot if pid == 100 else (False, None)):
        assert leases.workers_alive(data["leases"]["old"])
        with pytest.raises(budget.ConcurrencyBusyError):
            leases.admission(data, "codex", None)
    assert "old" in data["leases"]


def test_real_exhaustion_during_busy_retry_fails_without_another_wait(isolated, monkeypatch):
    monkeypatch.setenv("MAKEWAND_MACHINE_DAILY_CALLS", "2")
    monkeypatch.setenv("MAKEWAND_LEASE_WAIT_SECONDS", "10")
    first = budget.reserve("codex", "fast", task_id="holder")
    waiting, retry = threading.Event(), threading.Event()
    sleeps = []

    def wait(seconds):
        sleeps.append(seconds)
        waiting.set()
        assert retry.wait(5)

    def contender():
        return execute(ExecutionRequest(engine="codex", task_id="waiter", stage="test", timeout_ms=3000),
                       lambda timeout: pytest.fail("exhausted worker must not launch"))

    from concurrent.futures import ThreadPoolExecutor
    with patch.object(budget, "time", SimpleNamespace(monotonic=time.monotonic, time=time.time, sleep=wait)), \
            ThreadPoolExecutor(max_workers=1) as pool:
        future = pool.submit(contender)
        try:
            assert waiting.wait(5)
            other = budget.reserve("grok", "fast", task_id="other")
        finally:
            retry.set()
        result = future.result(timeout=5)
    assert result.status == "BUDGET_EXHAUSTED"
    assert len(sleeps) == 1, "real exhaustion must bypass the busy backoff"
    assert len(json.loads(Path(os.environ["MAKEWAND_CALL_BUDGET_FILE"]).read_text())["attempts"]) == 2
    budget.complete(first, True, 0)
    budget.complete(other, True, 0)


def test_review_falls_back_after_actual_busy_admission(isolated, monkeypatch):
    from makewand import orchestrator as orch
    monkeypatch.setenv("MAKEWAND_LEASE_WAIT_SECONDS", "0")
    first = budget.reserve("codex", "fast", task_id="holder")

    def verdict(prompt, **kwargs):
        nonce = re.search(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)", prompt).group(0)
        return True, nonce + ': {"pass": true, "defects": []}', None

    with patch.object(orch, "get_git_diff", return_value="diff fixture"), \
            patch.object(orch, "get_or_update_status", return_value={}), \
            patch.object(orch, "_engine_usable", return_value=(True, "")), \
            patch.object(orch, "execute_codex_task", side_effect=AssertionError("busy codex launched")), \
            patch.object(orch, "execute_grok_task", side_effect=verdict) as fallback, \
            contextlib.redirect_stdout(io.StringIO()) as output:
        code = orch.run_review(cwd=str(isolated.parent), output_json=True)
    assert code == 0, output.getvalue()
    assert json.loads(output.getvalue())["engine"] == "grok"
    fallback.assert_called_once()
    assert len(json.loads(Path(os.environ["MAKEWAND_CALL_BUDGET_FILE"]).read_text())["attempts"]) == 2
    budget.complete(first, True, 0)


@pytest.mark.parametrize("stream", [False, True])
@pytest.mark.parametrize("change", ["account", "root", "refresh"])
def test_direct_claude_post_run_switch_is_unknown(isolated, monkeypatch, stream, change):
    try:
        from test_provider_outcomes import ProviderOutcomeTests
    except ImportError:  # python3 -m unittest tests.<module>
        from tests.test_provider_outcomes import ProviderOutcomeTests
    from makewand.workflow import provider_outcome
    metadata = isolated / ".claude.json"
    metadata.write_text('{"oauthAccount":{"accountUuid":"A"}}')

    def finished(*args, **kwargs):
        if change == "account":
            metadata.write_text('{"oauthAccount":{"accountUuid":"B"}}')
        elif change == "root":
            alternate = isolated / "alternate"
            alternate.mkdir()
            (alternate / ".claude.json").write_text('{"oauthAccount":{"accountUuid":"A"}}')
            monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(alternate))
        else:
            metadata.write_text('{"oauthAccount":{"accountUuid":"A"},"refreshed":true}')
        return 0, "ok", "", None

    fixture = ProviderOutcomeTests()
    with fixture.adapter("claude") as (adapter, process, api):
        process.side_effect = finished
        result = provider_outcome(adapter("fixture", cwd=str(isolated.parent), readonly=True, stream=stream))
    process.assert_called_once()
    api.assert_not_called()
    if change == "refresh":
        assert result[0]
    else:
        assert result.status == "UNKNOWN"
        assert not result.outcome_known
