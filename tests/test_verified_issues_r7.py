"""Round seven: filesystem path resolution and bounded Claude identity state."""
try:
    import _isolation
except ImportError:
    from tests import _isolation

import errno
import json
import os
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
@pytest.mark.parametrize("location", ["workspace", "home"])
@pytest.mark.parametrize("shape", ["dangling", "dangling-child", "relative-link", "link-parent", "relative-parent"])
def test_foreign_missing_resolved_root_fails_closed(isolated, monkeypatch, variable, location, shape):
    work, outside = isolated.parent / "work", isolated.parent / "outside"
    work.mkdir()
    outside.mkdir()
    target = work if location == "workspace" else isolated
    link = outside / "link"
    if shape in ("link-parent", "relative-parent"):
        (target / "sub").mkdir()
        link.symlink_to(target / "sub", target_is_directory=True)
        selected = str(link) + "/../missing"
    else:
        destination = target / "missing"
        link.symlink_to(os.path.relpath(destination, outside) if shape == "relative-link" else destination,
                        target_is_directory=True)
        selected = str(link) + ("/account" if shape == "dangling-child" else "")
    if shape == "relative-parent":
        # Child cwd is the workspace, independent of the dispatcher's cwd.
        selected = "../outside/link/../missing"
        monkeypatch.chdir(outside)
    monkeypatch.setenv(variable, selected)
    with patch.object(sandbox, "resolve_trusted_bwrap", return_value="/usr/bin/bwrap"), \
            pytest.raises(sandbox.SandboxConfigError, match=variable):
        sandbox.wrap_bwrap(["/usr/bin/true"], str(work), readonly=False)


@pytest.mark.parametrize("variable", ["CODEX_HOME", "CLAUDE_CONFIG_DIR"])
def test_relative_foreign_missing_uses_workspace(isolated, monkeypatch, variable):
    work = isolated.parent / "work"
    work.mkdir()
    monkeypatch.chdir(isolated.parent)
    monkeypatch.setenv(variable, "missing/account")
    with patch.object(sandbox, "resolve_trusted_bwrap", return_value="/usr/bin/bwrap"), \
            pytest.raises(sandbox.SandboxConfigError, match=variable):
        sandbox.wrap_bwrap(["/usr/bin/true"], str(work), readonly=False)


@pytest.mark.parametrize("error_code", [errno.ENOENT, errno.ENOTDIR, errno.EACCES])
def test_foreign_unwritable_alias_still_skipped(isolated, monkeypatch, error_code):
    work, outside = isolated.parent / "work", isolated.parent / "outside"
    work.mkdir()
    outside.mkdir()
    link = isolated.parent / "alias"
    link.symlink_to(outside, target_is_directory=True)
    selected = str(link / "missing")
    monkeypatch.setenv("CODEX_HOME", selected)
    original_stat = os.stat
    def inspect(path, *args, **kwargs):
        if str(path) in (selected, str(outside / "missing")):
            raise OSError(error_code, "fixture")
        return original_stat(path, *args, **kwargs)
    with patch.object(sandbox, "resolve_trusted_bwrap", return_value="/usr/bin/bwrap"), \
            patch.object(sandbox.os, "stat", side_effect=inspect):
        argv = sandbox.wrap_bwrap(["/usr/bin/true"], str(work), readonly=False)
    assert selected not in argv and "CODEX_HOME" not in argv


def claude_state(isolated):
    root = isolated / ".claude"
    root.mkdir()
    (root / ".credentials.json").write_text('{}')
    return isolated / ".claude.json"


@pytest.mark.parametrize("size", [1024 * 1024 + 1, 4 * 1024 * 1024])
def test_claude_identity_and_binding_share_four_mib_limit(isolated, size):
    state = claude_state(isolated)
    # Use exact byte counts at the boundary.
    raw = json.dumps({"oauthAccount": {"accountUuid": "same"}, "pad": ""}).encode()
    raw = raw[:-2] + b'x' * (size - len(raw)) + raw[-2:]
    assert len(raw) == size
    state.write_bytes(raw)
    identity = account_identity("claude")
    assert identity["identity_known"]
    assert credential_binding("claude")["files"][str(state)]["ref"] == identity["account_ref"]


@pytest.mark.parametrize("kind", ["oversized", "directory", "loop", "unreadable"])
def test_unavailable_claude_metadata_is_unknown_and_does_not_block_fallback(isolated, kind):
    state = claude_state(isolated)
    if kind == "oversized":
        state.write_bytes(b'x' * (4 * 1024 * 1024 + 1))
    elif kind == "directory":
        state.mkdir()
    elif kind == "loop":
        state.symlink_to(state.name)
    else:
        state.write_text('{"oauthAccount":{"accountUuid":"unreadable"}}')
        if os.geteuid() == 0:
            pytest.skip("root bypasses filesystem permissions")
        state.chmod(0)
        with pytest.raises(PermissionError):
            state.read_bytes()
    assert not account_identity("claude")["identity_known"]
    binding = credential_binding("claude")
    assert next(v for k, v in binding["files"].items() if k.endswith(".claude.json")) == {
        "kind": "identity", "exists": False, "ref": None}
    token = _context.set({"_credential_binding": binding})
    private = sandbox.ProviderHome("claude", str(isolated))
    try:
        private.materialize([str(isolated.parent)])
        verify_credential_copy(str(state), None)
    finally:
        private.finalize()
        _context.reset(token)
    # A transport failure remains eligible for a subsequent provider.
    failed = execute(ExecutionRequest(engine="claude", task_id="fallback", stage="test"),
                     lambda timeout: (False, "", "fixture transport failure"))
    assert failed.error_kind != "account_configuration"
    fallback = execute(ExecutionRequest(engine="grok", task_id="fallback", stage="test"),
                       lambda timeout: (True, "fallback", None))
    assert fallback.status == "PASSED"


def test_credential_read_cap_holds_if_file_grows_after_stat(isolated):
    state = claude_state(isolated)
    state.write_bytes(b'x' * (sandbox.CREDENTIAL_MAX_BYTES + 1))
    before = os.stat(state)
    original_fstat = os.fstat
    def old_size(fd):
        found = original_fstat(fd)
        if found.st_ino == before.st_ino and found.st_dev == before.st_dev:
            from types import SimpleNamespace
            return SimpleNamespace(st_mode=found.st_mode, st_size=0, st_dev=found.st_dev,
                                   st_ino=found.st_ino, st_mtime_ns=found.st_mtime_ns)
        return found
    with patch.object(sandbox.os, "fstat", side_effect=old_size):
        assert sandbox._read_host_regular(str(state)) is None
