"""Verified P0 regressions; fake credentials and temporary ledgers only."""
try:
    import _isolation
except ImportError:
    from tests import _isolation

import json
import os
import subprocess
import sys
import time
from pathlib import Path
from unittest.mock import patch

import pytest

from makewand import account_leases as leases, call_budget as budget
from makewand.account_identity import credential_binding
from makewand.execution_contract import ExecutionRequest
from makewand.execution_runtime import _context, execute, provider_process_lease
from makewand.sandbox import ProviderHome, SandboxConfigError, provider_sandbox_launch


@pytest.fixture
def isolated(tmp_path, monkeypatch):
    home = tmp_path / "home"
    home.mkdir()
    for key, value in {
        "HOME": home, "USERPROFILE": home, "CODEX_HOME": "", "CLAUDE_CONFIG_DIR": "",
        "MAKEWAND_CONFIG_DIR": tmp_path / "config", "MAKEWAND_PROJECT_ROOT": tmp_path,
        "MAKEWAND_CALL_BUDGET_FILE": tmp_path / "calls.json", "MAKEWAND_LAYERED_BUDGET": "1",
        "MAKEWAND_MAX_MODEL_CALLS": "100", "MAKEWAND_PROJECT_DAILY_CALLS": "100",
        "MAKEWAND_MACHINE_DAILY_CALLS": "100", "MAKEWAND_ACCOUNT_CONCURRENCY": "1",
        "MAKEWAND_POOL_CONCURRENCY": "1", "MAKEWAND_LEASE_WAIT_SECONDS": "0.2",
    }.items():
        monkeypatch.setenv(key, str(value))
    token = _context.set(None)
    yield home
    _context.reset(token)


def test_dead_owner_launch_fence_is_reclaimed(isolated):
    first = budget.reserve("codex", "fast", task_id="first")
    budget.lease_update(first, "launch")
    path = Path(os.environ["MAKEWAND_CALL_BUDGET_FILE"])
    data = json.loads(path.read_text())
    data["leases"][first]["owner"]["boot"] = "previous-boot"
    data["leases"][first]["expires_at"] = 0
    path.write_text(json.dumps(data))
    second = budget.reserve("codex", "fast", task_id="second")
    assert first not in json.loads(path.read_text())["leases"]
    budget.complete(second, True, 0)


def test_cleanup_lease_write_retries(isolated):
    with patch("makewand.call_budget.lease_update", side_effect=[OSError("transient"), None]) as update:
        provider_process_lease("stopped")
    assert update.call_count == 2


def test_completed_dead_workers_reclaimed_with_live_daemon_owner(isolated):
    first = budget.reserve("codex", "fast", task_id="first")
    path = Path(os.environ["MAKEWAND_CALL_BUDGET_FILE"])
    data = json.loads(path.read_text())
    data["attempts"][0]["status"] = "completed"
    data["leases"][first]["expires_at"] = 0
    path.write_text(json.dumps(data))
    second = budget.reserve("codex", "fast", task_id="second")
    budget.complete(second, True, 0)


def test_windows_job_worker_reclaimed_only_after_owner_death(isolated):
    from makewand import lease_process
    worker = {"pid": 200, "token": "worker", "boot": "windows", "job": "owner-kill-on-close"}
    owner = {"pid": 100, "token": "old-owner", "boot": "windows"}
    data = {"leases": {"old": {"keys": leases.keys("codex", None), "owner": owner,
            "workers": [worker], "launch_pending": False, "expires_at": 0}}}
    # The owner PID is reused (also covers a reboot); worker inspection alone
    # cannot prove Job tree death. Only the owner-held Job marker permits it.
    with patch.object(sys, "platform", "win32"), patch.object(lease_process, "_windows_snapshot", return_value=(True, "new-owner")):
        leases.admission(data, "codex", None)
    assert "old" not in data["leases"]


@pytest.mark.parametrize("canonical,aliases", [
    ("codex", ["openai", "CODEX"]), ("claude", ["anthropic", "CLAUDE"]),
    ("agy", ["google", "gemini", "antigravity"]), ("grok", ["xai"]),
    ("local", ["ollama"]), ("qwen", ["dashscope", "aliyun"]),
])
def test_api_alias_keys_and_capacity(isolated, canonical, aliases):
    wanted = leases.keys(canonical, canonical + ":api:fixture")
    first = budget.reserve(canonical, "fast", task_id="first", account_ref=canonical + ":api:fixture")
    for alias in aliases:
        assert leases.keys(alias, alias + ":api:fixture") == wanted
        with pytest.raises(budget.BudgetError, match="concurrency"):
            budget.reserve(alias, "fast", task_id="second", account_ref=alias + ":api:fixture")
    budget.complete(first, True, 0)


def test_capacity_wait_then_admit_across_processes(isolated):
    first = budget.reserve("agy", "fast", task_id="first")
    code = """import sys
from makewand.execution_contract import ExecutionRequest
from makewand.execution_runtime import execute
r = execute(ExecutionRequest(engine='agy', task_id='second', stage='test', timeout_ms=3000), lambda timeout: (True, 'ok', None))
print(r.status, flush=True)
"""
    env = dict(os.environ, MAKEWAND_LEASE_WAIT_SECONDS="2")
    child = subprocess.Popen([sys.executable, "-c", code], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        time.sleep(.3)
        assert child.poll() is None, "contending invocation aborted before capacity was released"
        budget.complete(first, True, 0)
        out, err = child.communicate(timeout=5)
        assert child.returncode == 0, err
        assert out.strip() == "PASSED", (out, err)
    finally:
        if child.poll() is None:
            child.kill()
        child.communicate(timeout=5)


def test_capacity_timeout_is_retryable_and_uncharged(isolated):
    budget.reserve("agy", "fast", task_id="first")
    called = []
    started = time.monotonic()
    result = execute(ExecutionRequest(engine="agy", task_id="second", stage="test", timeout_ms=1500),
                     lambda timeout: called.append(True))
    assert result.status == "FAILED"  # known pre-dispatch refusal: workflow may try another engine
    assert result.error_kind == "concurrency_busy"
    assert result.exit_code != 13
    assert time.monotonic() - started >= .15
    assert not called
    assert len(json.loads(Path(os.environ["MAKEWAND_CALL_BUDGET_FILE"]).read_text())["attempts"]) == 1


@pytest.mark.parametrize("explicit", [False, True])
def test_direct_claude_preserves_settings_hooks_and_config_path(isolated, explicit, monkeypatch):
    work = isolated.parent / "work"
    work.mkdir()
    root = isolated / ("custom" if explicit else ".claude")
    root.mkdir()
    (root / ".credentials.json").write_text("{}")
    (root / "settings.json").write_text('{"permissions":{"deny":["Bash(rm *)"]},"hooks":{"PreToolUse":[]}}')
    (root / "hooks").mkdir()
    (root / "hooks" / "guard.sh").write_text("exit 1\n")
    if explicit:
        monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(root))
    with patch("makewand.sandbox.is_bwrap_available", return_value=False), \
         patch("makewand.sandbox.authorize_unsandboxed_launch", return_value="fixture"):
        with provider_sandbox_launch("claude", ["fixture"], str(work), readonly=False) as launch:
            effective = Path(launch.env.get("CLAUDE_CONFIG_DIR") or os.environ.get("CLAUDE_CONFIG_DIR") or isolated / ".claude")
            assert effective == root
            assert (effective / "settings.json").read_text() == (root / "settings.json").read_text()
            assert (effective / "hooks/guard.sh").is_file()


@pytest.mark.parametrize("provider,layout", [("claude", "home"), ("claude", "file"), ("codex", "file")])
def test_symlinked_credentials_materialize(isolated, provider, layout, monkeypatch):
    root = isolated / ("." + provider)
    root.mkdir()
    name = "auth.json" if provider == "codex" else ".credentials.json"
    target = isolated / "stored.json"
    target.write_text("{}")
    if layout == "file":
        (root / name).symlink_to(target)
    else:
        (root / name).write_text("{}")
        alias = isolated.parent / "linked-home"
        alias.symlink_to(isolated, target_is_directory=True)
        monkeypatch.setenv("HOME", str(alias))
    if provider == "claude":
        (isolated / ".claude.json").write_text("{}")
    binding = credential_binding(provider)
    token = _context.set({"_credential_binding": binding})
    home = ProviderHome(provider)
    try:
        home.materialize()
    finally:
        home.finalize(write_back=False)
        _context.reset(token)


def test_retargeted_credential_symlink_refuses_account_switch(isolated):
    root = isolated / ".codex"
    root.mkdir()
    one, two = isolated / "one.json", isolated / "two.json"
    one.write_text('{"tokens":{"account_id":"one"}}')
    two.write_text('{"tokens":{"account_id":"two"}}')
    link = root / "auth.json"
    link.symlink_to(one)
    token = _context.set({"_credential_binding": credential_binding("codex")})
    home = ProviderHome("codex")
    link.unlink()
    link.symlink_to(two)
    try:
        with pytest.raises(SandboxConfigError, match="selected login changed"):
            home.materialize()
    finally:
        home.finalize(write_back=False)
        _context.reset(token)
