"""
Sandboxed Claude runs on the user's long-lived token (`claude setup-token`).

A copy of ~/.claude/.credentials.json refreshed inside the sandbox spends the
refresh token the host sessions still hold, so one side is logged out. With a
token kept at ~/.config/makewand/credential-backups/claude/setup-token the run
gets a private login made from that token and no copy of the interactive one.
"""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import account_identity, sandbox
from makewand.execution_runtime import _context
from makewand.sandbox import SandboxConfigError, is_bwrap_available, wrap_bwrap

BWRAP_OK = is_bwrap_available()
TOKEN = "sk-ant-oat01-" + "Ab0_-" * 12
OTHER_TOKEN = "sk-ant-oat01-" + "Zz9_-" * 12
HOST_LOGIN = '{"claudeAiOauth": {"accessToken": "OLD", "refreshToken": "HOSTREFRESH", "expiresAt": 1}}\n'
ENV = "CLAUDE_CODE_OAUTH_TOKEN"


@contextlib.contextmanager
def fake_home():
    # Resolved: a symlinked temporary directory would make every token file
    # look visible to a sandbox and hide the checks under test.
    home = Path(tempfile.mkdtemp(prefix="setup-token-home-")).resolve()
    try:
        with patch.dict(os.environ, {"HOME": str(home)}):
            yield home
    finally:
        subprocess.run(["chmod", "-R", "u+rwx", str(home)], check=False)
        shutil.rmtree(home, ignore_errors=True)


@contextlib.contextmanager
def temp_workspace():
    workspace = Path(tempfile.mkdtemp(prefix="setup-token-ws-")).resolve()
    try:
        yield workspace
    finally:
        shutil.rmtree(workspace, ignore_errors=True)


@contextlib.contextmanager
def admitted(binding):
    """The credential fingerprints a dispatch took when it was admitted."""
    marker = _context.set({"_credential_binding": binding})
    try:
        yield
    finally:
        _context.reset(marker)


def token_path(home):
    return home / sandbox.CREDENTIAL_BACKUP_DIR / sandbox.CLAUDE_SETUP_TOKEN_REL


def make_claude(home, token=TOKEN, mode=0o600, root=True):
    if root:
        (home / ".claude" / "projects").mkdir(parents=True)
        (home / ".claude" / ".credentials.json").write_text(HOST_LOGIN)
    (home / ".claude.json").write_text('{"oauthAccount": {"accountUuid": "u-1"}}\n')
    if token is not None:
        path = token_path(home)
        path.parent.mkdir(parents=True, mode=0o700)
        path.write_text(token + "\n")
        path.chmod(mode)
    return home / ".claude"


def clean_env():
    """No login selection from the developer's shell leaks into a test."""
    return patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": "", "ANTHROPIC_API_KEY": "", "CLAUDE_API_KEY": "", ENV: ""})


def run(workspace, payload, provider="claude", is_provider=True):
    stderr = io.StringIO()
    with contextlib.redirect_stderr(stderr):
        with sandbox.provider_home_session(provider, [str(workspace)]) as home:
            cmd = wrap_bwrap(["bash", "-c", payload], workspace=str(workspace), is_provider=is_provider,
                             provider_name=provider if is_provider else None, readonly=True,
                             allow_network=False, provider_home=home if is_provider else None)
            result = subprocess.run(cmd, cwd=str(workspace), capture_output=True, text=True, timeout=60)
    return cmd, result, stderr.getvalue()


PAYLOAD = (
    'test -e ~/.claude/.credentials.json && { printf LOGIN=; cat ~/.claude/.credentials.json; }; echo\n'
    f'echo "ENV=${{{ENV}:-}}"\n'
    'ls ~/.config/makewand >/dev/null 2>&1 && echo STORE=visible || echo STORE=hidden\n'
    'echo \'{"claudeAiOauth": {"accessToken": "NEW", "refreshToken": "R2", "expiresAt": 2}}\''
    ' > ~/.claude/.credentials.json\n'
)


def sandbox_login(stdout):
    line = next((row for row in stdout.splitlines() if row.startswith("LOGIN=")), None)
    return json.loads(line[len("LOGIN="):])["claudeAiOauth"] if line else None


@unittest.skipUnless(BWRAP_OK, "bubblewrap not usable on this host")
class TestClaudeSetupToken(unittest.TestCase):
    def assert_token_run(self, ws):
        cmd, result, warnings = run(ws, PAYLOAD)
        self.assertEqual(result.returncode, 0, result.stderr)
        login = sandbox_login(result.stdout)
        self.assertEqual(login["accessToken"], TOKEN)
        self.assertIsNone(login["refreshToken"])
        self.assertGreater(login["expiresAt"], 1795000000000 + 300 * 86400000)
        self.assertNotIn("HOSTREFRESH", result.stdout)
        # Neither a command line nor the environment carries the token.
        self.assertFalse(any(TOKEN in argument for argument in cmd))
        self.assertIn("ENV=\n", result.stdout)
        self.assertIn("STORE=hidden", result.stdout)
        self.assertNotIn("凭据", warnings)

    def test_token_replaces_the_copy_of_the_interactive_login(self):
        with clean_env(), fake_home() as home, temp_workspace() as ws:
            root = make_claude(home)
            self.assert_token_run(ws)
            # What the run wrote over its private login is neither written back nor reported.
            self.assertEqual((root / ".credentials.json").read_text(), HOST_LOGIN)

    def test_token_works_without_a_host_claude_directory(self):
        with clean_env(), fake_home() as home, temp_workspace() as ws:
            make_claude(home, root=False)
            self.assert_token_run(ws)
            self.assertFalse((home / ".claude").exists())

    def test_without_a_token_the_login_is_copied_and_refreshes_written_back(self):
        with clean_env(), fake_home() as home, temp_workspace() as ws:
            root = make_claude(home, token=None)
            _, result, _ = run(ws, PAYLOAD)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(sandbox_login(result.stdout)["refreshToken"], "HOSTREFRESH")
            self.assertIn('"NEW"', (root / ".credentials.json").read_text())

    def test_another_selected_login_ignores_the_token(self):
        for name in ("ANTHROPIC_API_KEY", "CLAUDE_API_KEY"):
            with self.subTest(name), clean_env(), fake_home() as home, temp_workspace() as ws:
                make_claude(home)
                with patch.dict(os.environ, {name: "k"}):
                    _, result, _ = run(ws, PAYLOAD)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(sandbox_login(result.stdout)["refreshToken"], "HOSTREFRESH")

    def test_alternate_config_directory_ignores_the_token(self):
        with clean_env(), fake_home() as home, temp_workspace() as ws:
            make_claude(home)
            other = home / "other"
            other.mkdir()
            (other / ".credentials.json").write_text(HOST_LOGIN.replace("HOSTREFRESH", "OTHERREFRESH"))
            with patch.dict(os.environ, {"CLAUDE_CONFIG_DIR": str(other)}):
                payload = 'cat "$CLAUDE_CONFIG_DIR/.credentials.json"; echo "ENV=${%s:-}"\n' % ENV
                _, result, _ = run(ws, payload)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("OTHERREFRESH", result.stdout)
            self.assertNotIn(TOKEN, result.stdout)

    def test_host_environment_token_never_enters_the_sandbox(self):
        for kept in (TOKEN, None):
            with self.subTest(kept=bool(kept)), clean_env(), fake_home() as home, temp_workspace() as ws:
                make_claude(home, token=kept)
                with patch.dict(os.environ, {ENV: OTHER_TOKEN}):
                    cmd, result, _ = run(ws, PAYLOAD)
                self.assertFalse(any(OTHER_TOKEN in argument for argument in cmd))
                self.assertIn("ENV=\n", result.stdout)
                self.assertNotIn(OTHER_TOKEN, result.stdout)

    def test_other_sandboxes_get_neither_the_token_nor_its_file(self):
        payload = ('cat ~/.claude/.credentials.json 2>/dev/null; cat ~/.config/makewand/credential-backups/claude/*'
                   ' 2>/dev/null; env; echo DONE\n')
        for provider, is_provider in (("codex", True), ("claude", False)):
            with self.subTest(provider=provider, is_provider=is_provider), clean_env(), fake_home() as home, \
                    temp_workspace() as ws:
                make_claude(home)
                (home / ".codex").mkdir()
                cmd, result, _ = run(ws, payload, provider=provider, is_provider=is_provider)
                self.assertIn("DONE", result.stdout, result.stderr)
                self.assertNotIn(TOKEN, result.stdout)
                self.assertFalse(any(TOKEN in argument for argument in cmd))


class TestClaudeSetupTokenAdmission(unittest.TestCase):
    """Building the private home needs no working bubblewrap."""

    def test_private_root_stays_known_to_execution_evidence(self):
        with clean_env(), fake_home() as home, temp_workspace() as ws:
            make_claude(home)
            with sandbox.provider_home_session("claude", [str(ws)]) as provider_home:
                provider_home.materialize([str(ws)])
                copies = [c for c in provider_home.credentials if c.rel == ".credentials.json"]
                self.assertEqual([c.detached for c in copies], [True])
                self.assertTrue(os.path.isdir(copies[0].private_root))
                self.assertEqual(provider_home.identity_file[1], ".claude.json")

    def refused_at_launch(self, binding, ws):
        with admitted(binding), sandbox.provider_home_session("claude", [str(ws)]) as provider_home:
            with self.assertRaises(SandboxConfigError):
                provider_home.materialize([str(ws)])

    def test_token_changed_after_admission_refuses_the_launch(self):
        with clean_env(), fake_home() as home, temp_workspace() as ws:
            make_claude(home)
            binding = account_identity.credential_binding("claude")
            token_path(home).write_text(OTHER_TOKEN + "\n")
            self.refused_at_launch(binding, ws)

    def test_token_added_or_removed_after_admission_refuses_the_launch(self):
        for before, after in ((None, TOKEN), (TOKEN, None)):
            with self.subTest(before=bool(before)), clean_env(), fake_home() as home, temp_workspace() as ws:
                make_claude(home, token=before)
                binding = account_identity.credential_binding("claude")
                if after is None:
                    token_path(home).unlink()
                else:
                    token_path(home).parent.mkdir(parents=True, mode=0o700)
                    token_path(home).write_text(after + "\n")
                    token_path(home).chmod(0o600)
                self.refused_at_launch(binding, ws)

    def test_host_login_refreshed_after_admission_does_not_refuse_a_token_run(self):
        with clean_env(), fake_home() as home, temp_workspace() as ws:
            root = make_claude(home)
            binding = account_identity.credential_binding("claude")
            (root / ".credentials.json").write_text(HOST_LOGIN.replace("OLD", "REFRESHED"))
            with admitted(binding), sandbox.provider_home_session("claude", [str(ws)]) as provider_home:
                provider_home.materialize([str(ws)])
                self.assertEqual(provider_home.setup_token, TOKEN)

    def test_host_login_refreshed_after_admission_still_refuses_a_copied_login(self):
        with clean_env(), fake_home() as home, temp_workspace() as ws:
            root = make_claude(home, token=None)
            binding = account_identity.credential_binding("claude")
            (root / ".credentials.json").write_text(HOST_LOGIN.replace("OLD", "REFRESHED"))
            self.refused_at_launch(binding, ws)

    def test_admitted_token_run_works_without_a_host_claude_directory(self):
        with clean_env(), fake_home() as home, temp_workspace() as ws:
            make_claude(home, root=False)
            binding = account_identity.credential_binding("claude")
            with admitted(binding), sandbox.provider_home_session("claude", [str(ws)]) as provider_home:
                provider_home.materialize([str(ws)])
                copy = next(c for c in provider_home.credentials if c.rel == ".credentials.json")
                login = json.loads((Path(copy.private_root) / copy.rel).read_text())["claudeAiOauth"]
                self.assertEqual(login["accessToken"], TOKEN)

    def test_host_run_ignores_the_token_file_and_keeps_binding_the_login(self):
        with clean_env(), fake_home() as home:
            root = make_claude(home)
            view = account_identity.binding_view
            before = account_identity.credential_binding("claude")
            token_path(home).write_text(OTHER_TOKEN + "\n")
            self.assertEqual(view(before, False), view(account_identity.credential_binding("claude"), False))
            self.assertNotEqual(view(before, True), view(account_identity.credential_binding("claude"), True))
            (root / ".credentials.json").write_text(HOST_LOGIN.replace("OLD", "REFRESHED"))
            self.assertNotEqual(view(before, False), view(account_identity.credential_binding("claude"), False))

    def test_binding_fingerprints_token_and_login_without_starting_a_process(self):
        with clean_env(), fake_home() as home, \
                patch.object(sandbox, "run_subprocess", side_effect=AssertionError("helper process")):
            root = make_claude(home)
            binding = account_identity.credential_binding("claude")
            token, login = os.path.realpath(token_path(home)), os.path.realpath(root / ".credentials.json")
            self.assertEqual((binding["setup_token"], binding["login"]), (token, login))
            self.assertNotIn(TOKEN, json.dumps(binding))
            self.assertNotIn(login, account_identity.binding_view(binding, True)["files"])
            self.assertNotIn(token, account_identity.binding_view(binding, False)["files"])
            token_path(home).unlink()
            binding = account_identity.credential_binding("claude")
            self.assertNotIn("setup_token", binding)
            self.assertIs(account_identity.binding_view(binding, True), binding)
            self.assertEqual(list(binding["files"]), [login, os.path.realpath(home / ".claude.json")])


class TestClaudeSetupTokenFile(unittest.TestCase):
    def token(self, home):
        found = sandbox._claude_setup_token(str(home))
        return found[0] if found else None

    def refused(self, home, problem):
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SandboxConfigError) as caught:
            self.token(home)
        self.assertIn(problem, str(caught.exception))
        return str(caught.exception)

    def test_usable_file_gives_the_token(self):
        with clean_env(), fake_home() as home:
            make_claude(home)
            self.assertEqual(self.token(home), TOKEN)

    def test_absent_file_or_directory_means_no_token(self):
        with clean_env(), fake_home() as home:
            make_claude(home, token=None)
            self.assertIsNone(self.token(home))
            (home / sandbox.CREDENTIAL_BACKUP_DIR).mkdir(parents=True)
            self.assertIsNone(self.token(home))

    def test_another_selected_login_ignores_the_token(self):
        for name in ("CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "CLAUDE_API_KEY"):
            with self.subTest(name), clean_env(), fake_home() as home:
                make_claude(home)
                with patch.dict(os.environ, {name: str(home / "other")}):
                    self.assertIsNone(self.token(home))

    def test_unusable_content_refuses_the_launch(self):
        cases = {
            "wide mode": (dict(mode=0o640), "权限过宽"),
            "two lines": (dict(token=TOKEN + "\nsecond"), "单行令牌"),
            "too short": (dict(token="short"), "单行令牌"),
            "option-like": (dict(token="--bind" + "a" * 30), "单行令牌"),
            "not ascii": (dict(token="é" * 30), "单行令牌"),
            "empty": (dict(token=""), "单行令牌"),
        }
        for label, (arguments, problem) in cases.items():
            with self.subTest(label), clean_env(), fake_home() as home:
                make_claude(home, **arguments)
                message = self.refused(home, problem)
                self.assertNotIn(arguments.get("token") or TOKEN, message)

    def test_symlinked_file_or_store_refuses_the_launch(self):
        for label in ("file", "store"):
            with self.subTest(label), clean_env(), fake_home() as home:
                make_claude(home)
                target = token_path(home) if label == "file" else token_path(home).parent
                moved = home / "moved"
                target.rename(moved)
                target.symlink_to(moved)
                self.refused(home, "普通文件")

    def test_entry_that_cannot_be_examined_refuses_the_launch(self):
        if os.geteuid() == 0:
            self.skipTest("root traverses any directory")
        with clean_env(), fake_home() as home:
            make_claude(home)
            token_path(home).parent.chmod(0o600)  # no search permission: lstat fails with EACCES
            self.refused(home, "普通文件")
        with clean_env(), fake_home() as home:
            make_claude(home, token=None)
            store = home / sandbox.CREDENTIAL_BACKUP_DIR
            store.mkdir(parents=True)
            (store / "claude").write_text("a file where the directory belongs\n")  # ENOTDIR
            self.refused(home, "普通文件")


if __name__ == "__main__":
    unittest.main()
