"""Round-4 regressions: offline admission, fallback and stale foreign roots."""
try:
    import _isolation
except ImportError:
    from tests import _isolation

import contextlib
import io
import json
import os
import re
import time
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest

from makewand import call_budget as budget, orchestrator as orch, sandbox
from makewand.execution_contract import ExecutionRequest
from makewand.execution_runtime import execute
try:
    from test_verified_issues_r2 import isolated
except ImportError:
    from tests.test_verified_issues_r2 import isolated


def budget_clock(sleep):
    # Limit sleep assertions to lease admission; Git and cleanup may also sleep.
    return SimpleNamespace(monotonic=time.monotonic, time=time.time, sleep=sleep)


def reject_busy_sleep(seconds):
    raise AssertionError("slept despite idle fallback")


def verdict(prompt, **kwargs):
    nonce = re.search(r"MAKEWAND_VERDICT_[0-9a-f]+(?=:)", prompt).group(0)
    return True, nonce + ': {"pass": true, "defects": []}', None


def attempts():
    return json.loads(Path(os.environ["MAKEWAND_CALL_BUDGET_FILE"]).read_text())["attempts"]


def test_default_bounded_busy_leaves_time_for_fallback(isolated, monkeypatch):
    monkeypatch.delenv("MAKEWAND_LEASE_WAIT_SECONDS")
    holder = budget.reserve("codex", "fast", task_id="holder")
    try:
        started = time.monotonic()
        result = execute(ExecutionRequest(engine="codex", task_id="waiter", stage="test", timeout_ms=240),
                         lambda timeout: pytest.fail("busy worker launched"))
        assert result.status == "FAILED" and result.error_kind == "concurrency_busy"
        assert .03 < time.monotonic() - started < .22
        fallback = execute(ExecutionRequest(engine="grok", task_id="waiter", stage="test", timeout_ms=240),
                           lambda timeout: (True, "fallback", None))
        assert fallback.status == "PASSED"
        assert len(attempts()) == 2
    finally:
        budget.complete(holder, True, 0)


def test_default_wait_is_capped_even_with_long_deadline(isolated, monkeypatch):
    monkeypatch.delenv("MAKEWAND_LEASE_WAIT_SECONDS")
    clock = [100.0]
    with patch.object(budget, "time", SimpleNamespace(monotonic=lambda: clock[0],
            sleep=lambda seconds: clock.__setitem__(0, clock[0] + seconds))), \
            patch.object(budget, "_reserve_once", side_effect=budget.ConcurrencyBusyError("busy")):
        with pytest.raises(budget.ConcurrencyBusyError):
            budget.reserve("codex", "fast", deadline_monotonic=400)
    assert clock[0] == pytest.approx(130)


@pytest.mark.parametrize("explicit_cap", [None, "10"])
def test_review_other_candidate_never_sleeps(isolated, monkeypatch, explicit_cap):
    if explicit_cap is None:
        monkeypatch.delenv("MAKEWAND_LEASE_WAIT_SECONDS")
    else:
        monkeypatch.setenv("MAKEWAND_LEASE_WAIT_SECONDS", explicit_cap)
    holder = budget.reserve("codex", "fast", task_id="holder")
    try:
        with patch.object(orch, "get_git_diff", return_value="diff fixture"), \
                patch.object(orch, "get_or_update_status", return_value={}), \
                patch.object(orch, "_engine_usable", return_value=(True, "")), \
                patch.object(orch, "execute_codex_task", side_effect=AssertionError("busy codex launched")), \
                patch.object(orch, "execute_grok_task", side_effect=verdict) as fallback, \
                patch.object(budget, "time", budget_clock(reject_busy_sleep)), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            code = orch.run_review(cwd=str(isolated.parent), output_json=True, timeout=1)
        assert code == 0, output.getvalue()
        assert json.loads(output.getvalue())["engine"] == "grok"
        fallback.assert_called_once()
        assert len(attempts()) == 2
    finally:
        budget.complete(holder, True, 0)


def test_pipeline_coding_other_candidate_never_sleeps(isolated, monkeypatch):
    try:
        from test_workflow_scheduling import SchedulingTests
    except ImportError:
        from tests.test_workflow_scheduling import SchedulingTests
    fixture = SchedulingTests()
    fixture.setUp()
    monkeypatch.delenv("MAKEWAND_LEASE_WAIT_SECONDS")
    holder = budget.reserve("claude", "fast", task_id="holder")

    def release_for_review(engine, kwargs):
        assert engine == "codex" or kwargs.get("readonly"), "busy coder launched"
        if not kwargs.get("readonly"):
            budget.complete(holder, True, 0)

    try:
        with fixture.fixtures(callback=release_for_review), \
                patch.object(budget, "time", budget_clock(reject_busy_sleep)):
            result = orch.run_workflow("Implement value", cwd=str(fixture.repo), force_code=True,
                                       auto_fix=False, total_timeout=2, timeout=1)
        assert result.status == "PASSED", result
        assert [(name, ro) for name, ro, _, _ in fixture.calls] == [("codex", False), ("claude", True)]
        assert len(attempts()) == 3
    finally:
        budget.complete(holder, True, 0)
        fixture.doCleanups()


def test_only_usable_reviewer_waits_then_admits(isolated, monkeypatch):
    monkeypatch.delenv("MAKEWAND_LEASE_WAIT_SECONDS")
    holder = budget.reserve("codex", "fast", task_id="holder")
    sleeps = []

    def release(seconds):
        sleeps.append(seconds)
        budget.complete(holder, True, 0)

    try:
        with patch.object(orch, "get_git_diff", return_value="diff fixture"), \
                patch.object(orch, "get_or_update_status", return_value={}), \
                patch.object(orch, "_engine_usable", side_effect=lambda eng, cache: (eng == "codex", "offline")), \
                patch.object(orch, "execute_codex_task", side_effect=verdict), \
                patch.object(budget, "time", budget_clock(release)), \
                contextlib.redirect_stdout(io.StringIO()) as output:
            code = orch.run_review(cwd=str(isolated.parent), output_json=True, timeout=1)
        assert code == 0, output.getvalue()
        assert len(sleeps) == 1
        assert len(attempts()) == 2
    finally:
        budget.complete(holder, True, 0)


@pytest.mark.parametrize("variable", ["CODEX_HOME", "CLAUDE_CONFIG_DIR"])
@pytest.mark.parametrize("under_home", [False, True])
@pytest.mark.parametrize("provider", [None, "grok"])
def test_stale_foreign_root_does_not_break_verification(isolated, monkeypatch, variable, under_home, provider):
    work = isolated.parent / "work"
    work.mkdir()
    missing = (isolated if under_home else isolated.parent) / "missing-account"
    monkeypatch.setenv(variable, str(missing))
    with patch.object(sandbox, "resolve_trusted_bwrap", return_value="/usr/bin/bwrap"):
        if under_home:
            # Private HOME is writable even for a readonly workspace. A
            # foreign worker could plant config there for the next CLI run.
            with pytest.raises(sandbox.SandboxConfigError, match=variable):
                sandbox.wrap_bwrap(["/usr/bin/true"], str(work), readonly=True,
                                  is_provider=provider is not None, provider_name=provider)
            return
        argv = sandbox.wrap_bwrap(["/usr/bin/true"], str(work), readonly=True,
                                   is_provider=provider is not None, provider_name=provider)
    assert argv[-1] == "/usr/bin/true"
    assert str(missing) not in argv


def test_cancelled_race_waiter_never_admits_or_spends(isolated, monkeypatch):
    from makewand.providers.base import contestant_scope
    monkeypatch.delenv("MAKEWAND_LEASE_WAIT_SECONDS")
    holder = budget.reserve("codex", "fast", task_id="holder")
    try:
        with contestant_scope("r4-waiter") as scope:
            def cancel(seconds):
                scope.cancel_event.set()
                budget.complete(holder, True, 0)
                return True
            with patch.object(scope.cancel_event, "wait", side_effect=cancel) as wait:
                result = execute(ExecutionRequest(engine="codex", task_id="loser", stage="test", timeout_ms=500),
                                 lambda timeout: pytest.fail("cancelled worker launched"))
            wait.assert_called_once()
        assert result.status == "CANCELLED"
        assert len(attempts()) == 1
    finally:
        budget.complete(holder, True, 0)


def test_deadline_during_retry_of_busy_account_remains_busy(isolated):
    with patch.object(budget, "_reserve_once", side_effect=[
            budget.ConcurrencyBusyError("account busy"), budget.BudgetDeadlineError("late lock")]), \
            patch.object(budget.time, "sleep"):
        with pytest.raises(budget.ConcurrencyBusyError):
            budget.reserve("codex", "fast", deadline_monotonic=time.monotonic() + 1)
