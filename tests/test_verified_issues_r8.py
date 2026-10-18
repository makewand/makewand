"""Round eight: reject ambiguous account paths and omit unavailable tokens."""
try:
    import _isolation
except ImportError:
    from tests import _isolation

import json
import os
from pathlib import Path
from unittest.mock import patch

import pytest

from makewand import sandbox
from makewand.account_identity import account_identity, credential_binding, verify_credential_copy
from makewand.execution_contract import ExecutionRequest
from makewand.execution_runtime import _context, execute
try:
    from test_verified_issues_r2 import isolated
except ImportError:
    from tests.test_verified_issues_r2 import isolated


@pytest.mark.parametrize("variable", ["CODEX_HOME", "CLAUDE_CONFIG_DIR"])
@pytest.mark.parametrize("owner", [False, True])
@pytest.mark.parametrize("shape", ["missing", "relative", "file", "locked", "outside-link", "work-link", "home-link"])
def test_ambiguous_account_paths_refused_before_skip(isolated, monkeypatch, variable, owner, shape):
    work, outside = isolated.parent / "work", isolated.parent / "outside"
    work.mkdir()
    (outside / "a" / "b").mkdir(parents=True)
    (work / "sub").mkdir()
    (work / "file").write_text("fixture")
    locked = isolated / "locked"
    (locked / "s").mkdir(parents=True)
    (outside / "link").symlink_to(work / "sub", target_is_directory=True)
    (work / "link").symlink_to(outside / "a" / "b", target_is_directory=True)
    (isolated / "link").symlink_to(outside / "a" / "b", target_is_directory=True)
    selected = {
        "missing": str(work) + "/missing/../../outside/x",
        "relative": "missing/../../outside/x",
        "file": str(work) + "/file/../../outside/x",
        "locked": str(isolated) + "/locked/s/../../../outside/x",
        "outside-link": str(outside) + "/link/../x",
        "work-link": str(work) + "/link/../../x",
        "home-link": str(isolated) + "/link/../../x",
    }[shape]
    monkeypatch.setenv(variable, selected)
    locked.chmod(0)
    provider = ("codex" if variable == "CODEX_HOME" else "claude") if owner else "grok"
    try:
        with patch.object(sandbox, "resolve_trusted_bwrap", return_value="/usr/bin/bwrap"), \
                pytest.raises(sandbox.SandboxConfigError, match=variable + ".*ambiguous path traversal"):
            sandbox.wrap_bwrap(["/usr/bin/true"], str(work), readonly=False,
                               is_provider=True, provider_name=provider)
    finally:
        locked.chmod(0o700)


@pytest.mark.parametrize("variable", ["CODEX_HOME", "CLAUDE_CONFIG_DIR"])
def test_leading_parent_foreign_missing_still_skipped(isolated, monkeypatch, variable):
    work = isolated.parent / "work"
    work.mkdir()
    (isolated.parent / "outside").mkdir()
    monkeypatch.setenv(variable, "../outside/missing")
    with patch.object(sandbox, "resolve_trusted_bwrap", return_value="/usr/bin/bwrap"):
        argv = sandbox.wrap_bwrap(["/usr/bin/true"], str(work), readonly=False)
    assert "../outside/missing" not in argv and variable not in argv


@pytest.mark.parametrize("provider", ["codex", "claude"])
@pytest.mark.parametrize("kind", ["unreadable", "oversized", "directory"])
def test_unavailable_token_is_omitted_and_fallback_remains_available(isolated, monkeypatch, provider, kind):
    monkeypatch.setenv("MAKEWAND_MAX_MODEL_CALLS", "1")
    root = isolated / ("." + provider)
    root.mkdir()
    state = root / ("auth.json" if provider == "codex" else ".credentials.json")
    if kind == "directory":
        state.mkdir()
    elif kind == "oversized":
        state.write_bytes(b'x' * (sandbox.CREDENTIAL_MAX_BYTES + 1))
    else:
        state.write_text('{}')
        if os.geteuid() == 0:
            pytest.skip("root bypasses filesystem permissions")
        state.chmod(0)
        with pytest.raises(PermissionError):
            state.read_bytes()
    assert not account_identity(provider)["identity_known"]
    binding = credential_binding(provider)
    assert binding["files"][str(state)] is None
    work = isolated.parent / "work"
    work.mkdir()
    token = _context.set({"_credential_binding": binding})
    private = sandbox.ProviderHome(provider, str(isolated))
    try:
        mounts = private.materialize([str(work)])
        verify_credential_copy(str(state), None)
        assert not list(Path(private.private_dir).rglob(state.name))
        assert str(state) not in mounts
    finally:
        private.finalize()
        _context.reset(token)
    # A pre-invocation refusal is released; fallback can spend the only call.
    def refuse(timeout):
        from makewand.account_identity import refuse_credential_launch
        refuse_credential_launch("fixture provider has no readable login")
    failed = execute(ExecutionRequest(engine=provider, task_id="fallback", stage="test"), refuse)
    assert failed.error_kind == "account_configuration"
    data = json.loads(Path(os.environ["MAKEWAND_CALL_BUDGET_FILE"]).read_text())
    assert data["attempts"][0]["status"] == "released"
    assert data["scopes"]["tasks"]["fallback"]["used"] == 0
    fallback = execute(ExecutionRequest(engine="grok", task_id="fallback", stage="test"),
                       lambda timeout: (True, "fallback", None))
    assert fallback.status == "PASSED"
    # Unavailable -> readable still invalidates the admitted snapshot.
    if kind == "directory":
        state.rmdir()
    else:
        state.chmod(0o600)
    state.write_text('{"fixture":"readable"}')
    token = _context.set({"_credential_binding": binding})
    try:
        with pytest.raises(sandbox.SandboxConfigError, match="selected login changed"):
            verify_credential_copy(str(state), state.read_bytes())
    finally:
        _context.reset(token)


@pytest.mark.parametrize("provider", ["codex", "claude"])
def test_readable_token_change_still_refused(isolated, provider):
    root = isolated / ("." + provider)
    root.mkdir()
    state = root / ("auth.json" if provider == "codex" else ".credentials.json")
    state.write_text('{"fixture":"before"}')
    binding = credential_binding(provider)
    state.write_text('{"fixture":"after"}')
    token = _context.set({"_credential_binding": binding})
    try:
        with pytest.raises(sandbox.SandboxConfigError, match="selected login changed"):
            verify_credential_copy(str(state), state.read_bytes())
    finally:
        _context.reset(token)


@pytest.mark.parametrize("provider,variable", [("codex", "CODEX_HOME"), ("claude", "CLAUDE_CONFIG_DIR")])
def test_direct_owner_rejects_ambiguous_existing_account(isolated, monkeypatch, provider, variable):
    outside = isolated.parent / "outside"
    (outside / "sub").mkdir(parents=True)
    (outside / "account").mkdir()
    (outside / "link").symlink_to(outside / "sub", target_is_directory=True)
    monkeypatch.setenv(variable, str(outside) + "/link/../account")
    with patch.object(sandbox, "is_bwrap_available", return_value=False), \
            patch.object(sandbox, "authorize_unsandboxed_launch", return_value="fixture"), \
            pytest.raises(sandbox.SandboxConfigError, match=variable + ".*ambiguous path traversal"):
        with sandbox.provider_sandbox_launch(provider, ["fixture"], str(isolated.parent), readonly=True):
            pass


@pytest.mark.parametrize("value,expected", [
    ("../outside/missing", True), ("../../outside/missing", True),
    ("./../outside/missing", True), ("user..name/account", True),
    ("missing/../../outside/x", False), ("../outside/link/../x", False),
])
def test_account_path_component_rule(value, expected):
    assert sandbox._unambiguous_account_path(value) is expected
