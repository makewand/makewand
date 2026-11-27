"""
Sandboxed Claude runs with the user's long-lived token (`claude setup-token`).

A copy of ~/.claude/.credentials.json refreshed inside the sandbox spends the
refresh token the host sessions still hold, so one side is logged out. With a
token kept at ~/.config/makewand/credential-backups/claude/setup-token the run
gets CLAUDE_CODE_OAUTH_TOKEN and no copy of the interactive login.
"""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import sandbox
from makewand.sandbox import SandboxConfigError, is_bwrap_available, wrap_bwrap

BWRAP_OK = is_bwrap_available()
TOKEN = "sk-ant-oat01-" + "Ab0_-" * 12
HOST_LOGIN = '{"claudeAiOauth": {"accessToken": "OLD", "refreshToken": "R", "expiresAt": 1}}\n'
ENV = sandbox.CLAUDE_SETUP_TOKEN_ENV


@contextlib.contextmanager
def fake_home():
    home = Path(tempfile.mkdtemp(prefix="setup-token-home-"))
    try:
        with patch.dict(os.environ, {"HOME": str(home)}):
            yield home
    finally:
        shutil.rmtree(home, ignore_errors=True)


@contextlib.contextmanager
def temp_workspace():
    workspace = Path(tempfile.mkdtemp(prefix="setup-token-ws-"))
    try:
        yield workspace
    finally:
        shutil.rmtree(workspace, ignore_errors=True)


def make_claude(home, token=TOKEN, mode=0o600):
    root = home / ".claude"
    (root / "projects").mkdir(parents=True)
    (root / ".credentials.json").write_text(HOST_LOGIN)
    (home / ".claude.json").write_text('{"oauthAccount": {"accountUuid": "u-1"}}\n')
    if token is not None:
        path = home / sandbox.CREDENTIAL_BACKUP_DIR / sandbox.CLAUDE_SETUP_TOKEN_REL
        path.parent.mkdir(parents=True, mode=0o700)
        path.write_text(token + "\n")
        path.chmod(mode)
    return root


def clean_env():
    """No login selection from the developer's shell leaks into a test."""
    return patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": "", "ANTHROPIC_API_KEY": "", "CLAUDE_API_KEY": "", ENV: ""})


def run(workspace, payload):
    stderr = io.StringIO()
    with contextlib.redirect_stderr(stderr):
        with sandbox.provider_home_session("claude", [str(workspace)]) as home:
            cmd = wrap_bwrap(["bash", "-c", payload], workspace=str(workspace), is_provider=True,
                             provider_name="claude", readonly=True, allow_network=False, provider_home=home)
            result = subprocess.run(cmd, cwd=str(workspace), capture_output=True, text=True, timeout=60)
    return cmd, result, stderr.getvalue()


def setenv(cmd, name):
    return [cmd[i + 2] for i, arg in enumerate(cmd[:-2]) if arg == "--setenv" and cmd[i + 1] == name]


@unittest.skipUnless(BWRAP_OK, "bubblewrap not usable on this host")
class TestClaudeSetupToken(unittest.TestCase):
    PAYLOAD = (
        'test -e ~/.claude/.credentials.json && echo LOGIN=present || echo LOGIN=absent\n'
        f'echo "TOKEN=${{{ENV}:-}}"\n'
        'ls ~/.config/makewand >/dev/null 2>&1 && echo STORE=visible || echo STORE=hidden\n'
        'echo \'{"claudeAiOauth": {"accessToken": "NEW", "refreshToken": "R2", "expiresAt": 2}}\''
        ' > ~/.claude/.credentials.json\n'
    )

    def test_token_replaces_the_copy_of_the_interactive_login(self):
        with clean_env(), fake_home() as home, temp_workspace() as ws:
            root = make_claude(home)
            cmd, result, warnings = run(ws, self.PAYLOAD)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(setenv(cmd, ENV), [TOKEN])
            self.assertIn("LOGIN=absent", result.stdout)
            self.assertIn(f"TOKEN={TOKEN}", result.stdout)
            self.assertIn("STORE=hidden", result.stdout)
            # A credential file the run creates is neither written back nor reported.
            self.assertEqual((root / ".credentials.json").read_text(), HOST_LOGIN)
            self.assertNotIn("凭据", warnings)

    def test_without_a_token_the_login_is_copied_and_refreshes_written_back(self):
        with clean_env(), fake_home() as home, temp_workspace() as ws:
            root = make_claude(home, token=None)
            cmd, result, _ = run(ws, self.PAYLOAD)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(setenv(cmd, ENV), [])
            self.assertIn("LOGIN=present", result.stdout)
            self.assertIn("TOKEN=\n", result.stdout)
            self.assertIn('"NEW"', (root / ".credentials.json").read_text())

    def test_another_selected_login_ignores_the_token(self):
        for name, value in (("ANTHROPIC_API_KEY", "k"), ("CLAUDE_API_KEY", "k")):
            with self.subTest(name), clean_env(), fake_home() as home, temp_workspace() as ws:
                make_claude(home)
                with patch.dict(os.environ, {name: value}):
                    cmd, result, _ = run(ws, self.PAYLOAD)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(setenv(cmd, ENV), [])
                self.assertIn("LOGIN=present", result.stdout)

    def test_host_environment_token_is_not_passed_through(self):
        with clean_env(), fake_home() as home, temp_workspace() as ws:
            make_claude(home, token=None)
            with patch.dict(os.environ, {ENV: "from-the-host-shell-000000"}):
                cmd, result, _ = run(ws, self.PAYLOAD)
            self.assertEqual(setenv(cmd, ENV), [])
            self.assertIn("TOKEN=\n", result.stdout)


class TestClaudeSetupTokenFile(unittest.TestCase):
    def token(self, home):
        return sandbox._claude_setup_token(str(home))

    def test_absent_file_means_no_token(self):
        with clean_env(), fake_home() as home:
            make_claude(home, token=None)
            self.assertIsNone(self.token(home))

    def test_alternate_config_directory_ignores_the_token(self):
        with clean_env(), fake_home() as home:
            make_claude(home)
            with patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": str(home / "other")}):
                self.assertIsNone(self.token(home))

    def test_unusable_file_refuses_the_launch(self):
        cases = {
            "wide mode": dict(mode=0o640),
            "two lines": dict(token=TOKEN + "\nsecond"),
            "too short": dict(token="short"),
            "option-like": dict(token="--bind" + "a" * 30),
            "not ascii": dict(token="é" * 30),
        }
        for label, arguments in cases.items():
            with self.subTest(label), clean_env(), fake_home() as home:
                make_claude(home, **arguments)
                with self.assertRaises(SandboxConfigError) as caught:
                    self.token(home)
                self.assertNotIn(arguments.get("token", TOKEN), str(caught.exception))

    def test_symlinked_file_refuses_the_launch(self):
        with clean_env(), fake_home() as home, contextlib.redirect_stderr(io.StringIO()):
            make_claude(home)
            path = home / sandbox.CREDENTIAL_BACKUP_DIR / sandbox.CLAUDE_SETUP_TOKEN_REL
            real = home / "elsewhere"
            path.rename(real)
            path.symlink_to(real)
            with self.assertRaises(SandboxConfigError):
                self.token(home)

    def test_symlinked_store_refuses_the_launch(self):
        with clean_env(), fake_home() as home, contextlib.redirect_stderr(io.StringIO()):
            make_claude(home)
            store = home / sandbox.CREDENTIAL_BACKUP_DIR / "claude"
            moved = home / "moved"
            store.rename(moved)
            store.symlink_to(moved)
            with self.assertRaises(SandboxConfigError):
                self.token(home)


if __name__ == "__main__":
    unittest.main()
