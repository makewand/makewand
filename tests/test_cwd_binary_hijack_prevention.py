"""
Tests verifying defense against cwd binary hijacking (crossplat-2 / exec.ErrDot parity).
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import stat
import tempfile
from pathlib import Path
import pytest

from makewand.git_helper import _sanitize_git_env, resolve_safe_git_binary
from makewand.providers.base import safe_which, check_cli_installed
from makewand.windows_process import resolve_windows_command


def test_sanitize_git_env_includes_no_default_cwd_in_exe_path():
    env = _sanitize_git_env()
    assert env.get("NoDefaultCurrentDirectoryInExePath") == "1"
    assert env.get("GIT_OPTIONAL_LOCKS") == "0"


def test_safe_which_refuses_binary_in_cwd(monkeypatch):
    with tempfile.TemporaryDirectory() as td:
        ws = Path(td).resolve()
        fake_bin = ws / "claude"
        fake_bin.write_text("#!/bin/sh\necho fake\n")
        fake_bin.chmod(fake_bin.stat().st_mode | stat.S_IEXEC)

        # Set PATH to include cwd
        monkeypatch.setenv("PATH", f"{ws}{os.pathsep}/usr/bin{os.pathsep}/bin")

        # Resolving relative to ws must refuse fake_bin in cwd
        result = safe_which("claude", cwd=str(ws))
        # It must NOT return the fake binary in cwd
        assert result != str(fake_bin)
        assert not check_cli_installed("claude", cwd=str(ws)) or result != str(fake_bin)


def test_resolve_safe_git_binary_refuses_git_in_cwd(monkeypatch):
    import makewand.git_helper as gh
    with tempfile.TemporaryDirectory() as td:
        ws = Path(td).resolve()
        fake_git = ws / ("git.exe" if os.name == "nt" else "git")
        fake_git.write_text("#!/bin/sh\necho malicious\n")
        fake_git.chmod(fake_git.stat().st_mode | stat.S_IEXEC)

        # 1. When cached binary points into cwd
        monkeypatch.setattr(gh, "_RESOLVED_GIT_BINARY", str(fake_git))
        with pytest.raises(PermissionError, match="Refusing to execute git binary found inside workspace cwd"):
            gh.resolve_safe_git_binary(cwd=str(ws))

        # 2. When PATH points only to cwd, resolution must refuse it
        monkeypatch.setattr(gh, "_RESOLVED_GIT_BINARY", None)
        monkeypatch.setenv("PATH", str(ws))
        with pytest.raises(PermissionError, match="Refusing to execute git binary found inside workspace cwd"):
            gh.resolve_safe_git_binary(cwd=str(ws))


def test_resolve_windows_command_refuses_cwd_binary(monkeypatch):
    with tempfile.TemporaryDirectory() as td:
        ws = Path(td).resolve()
        fake_cmd = ws / "claude.cmd"
        fake_cmd.write_text("@echo off\r\necho evil\r\n")

        monkeypatch.setenv("PATH", f"{ws}{os.pathsep}/usr/bin")
        with pytest.raises(PermissionError, match="Refusing to execute binary found inside workspace cwd"):
            resolve_windows_command(["claude.cmd", "arg1"], cwd=str(ws))


def test_absolute_binary_outside_and_inside_cwd():
    with tempfile.TemporaryDirectory() as td_ws, tempfile.TemporaryDirectory() as td_tools:
        ws = Path(td_ws).resolve()
        tools = Path(td_tools).resolve()

        # Binary outside cwd
        outside_bin = tools / "safe_tool"
        outside_bin.write_text("#!/bin/sh\necho ok\n")
        outside_bin.chmod(outside_bin.stat().st_mode | stat.S_IEXEC)

        # safe_which allows absolute binary outside cwd
        assert safe_which(str(outside_bin), cwd=str(ws)) == str(outside_bin)
        res = resolve_windows_command([str(outside_bin), "arg"], cwd=str(ws))
        assert res[0] == str(outside_bin)

        # Binary inside cwd
        inside_bin = ws / "inside_tool"
        inside_bin.write_text("#!/bin/sh\necho evil\n")
        inside_bin.chmod(inside_bin.stat().st_mode | stat.S_IEXEC)

        assert safe_which(str(inside_bin), cwd=str(ws)) is None
        with pytest.raises(PermissionError, match="Refusing to execute binary found inside workspace cwd"):
            resolve_windows_command([str(inside_bin), "arg"], cwd=str(ws))


def test_run_git_cmd_substitutes_safe_git_binary(monkeypatch):
    import makewand.git_helper as gh
    intercepted_cmd = []

    def fake_run(exec_cmd, **kwargs):
        intercepted_cmd.extend(exec_cmd)
        import subprocess
        return subprocess.CompletedProcess(args=exec_cmd, returncode=0, stdout="", stderr="")

    monkeypatch.setattr(gh.subprocess, "run", fake_run)
    safe_path = "/usr/bin/custom_git"
    monkeypatch.setattr(gh, "resolve_safe_git_binary", lambda cwd=None: safe_path)

    # 1. safe=True with list
    intercepted_cmd.clear()
    gh.run_git_cmd(["git", "status"], safe=True)
    assert intercepted_cmd[0] == safe_path

    # 2. safe=True with str
    intercepted_cmd.clear()
    gh.run_git_cmd("git status", safe=True)
    assert intercepted_cmd[0] == safe_path

    # 3. safe=False with list
    intercepted_cmd.clear()
    gh.run_git_cmd(["git", "status"], safe=False)
    assert intercepted_cmd[0] == safe_path

