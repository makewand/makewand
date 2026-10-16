"""
Credential write-back robustness (round 4 of py-sec-3).

* muse mints the api_key of its auth.json only in its OAuth login record (an
  access token next to the key, no key mechanism). A key the user saved with
  `muse auth set --api-key-stdin` or pasted in the pasted_key lane stays a
  protected field, at the top level and in a nested provider entry alike, so a
  sandboxed agent cannot swap it (tests/testdata/credential_writeback_cases.json
  holds the merge cases both engines run; the live case is the round-3
  verifier repro).
* A credential copy that existed at launch and is deleted, replaced by a
  symlink, FIFO or directory, grown past the size limit or made unreadable in
  the sandbox is reported once, naming the file: it may have held a rotated
  refresh token, so the host login can stop working.
* A login made inside the sandbox while the provider's whole root directory is
  absent on the host is reported as well, in read-only and writable runs; the
  root gets an empty private directory inside the sandbox only, never on the
  host, and only where bubblewrap can create that mount point.

Everything runs against a throw-away HOME; no provider CLI is started (the live
cases run bash inside the real bubblewrap sandbox when it is usable).
"""

try:  # isolation must precede makewand imports
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

try:
    import test_fix_writeback_identity as vectors
except ImportError:
    from tests import test_fix_writeback_identity as vectors

import json
import os
import shlex
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import sandbox
from makewand.sandbox import ProviderHome, wrap_bwrap

BWRAP_OK = sandbox.is_bwrap_available()


def _binds(args):
    return [(args[i + 1], args[i + 2]) for i, a in enumerate(args[:-2]) if a == "--bind"]


def _ro_binds(args):
    return [(args[i + 1], args[i + 2]) for i, a in enumerate(args[:-2]) if a == "--ro-bind"]


def _live(provider, payload, ws, readonly):
    """Runs payload in the provider's sandbox like the provider launchers do;
    returns the completed process and the warnings printed."""
    with patch("makewand.sandbox._warn") as warn:
        with sandbox.provider_sandbox_launch(provider, ["bash", "-c", payload], str(ws), readonly=readonly,
                                             allow_network=False) as launch:
            res = subprocess.run(launch.cmd, cwd=str(ws), capture_output=True, text=True,
                                 pass_fds=launch.pass_fds, timeout=60)
    return res, [c.args[0] for c in warn.call_args_list]


def _workspace(test):
    ws = Path(tempfile.mkdtemp(prefix="wbr4-ws-"))
    test.addCleanup(shutil.rmtree, ws, True)
    return ws


class TestMuseMintShape(unittest.TestCase):
    def test_only_the_oauth_login_record_mints_its_key(self):
        shape = sandbox._cred_mint_shape
        login = {"access_token": "at", "mechanism": "bearer", "obtained_via": "device_code", "api_key": "mk"}
        self.assertTrue(shape(login))
        self.assertTrue(shape({"accessToken": "at", "api_key": "mk"}))  # no mechanism recorded
        for name, obj in (("saved key", {"mechanism": "api_key", "api_key": "k"}),
                          ("pasted key", {"mechanism": "api_key", "obtained_via": "pasted_key", "api_key": "k"}),
                          ("no access token", {"api_key": "k", "api_base_url": "https://api.example"}),
                          ("empty access token", dict(login, access_token="")),
                          ("old access token, key mechanism", dict(login, mechanism="api_key")),
                          ("old access token, spelled API-Key", dict(login, mechanism="API-Key")),
                          ("old access token, pasted key", dict(login, obtained_via="pasted_key")),
                          ("mechanism not a string", dict(login, mechanism=None))):
            with self.subTest(name):
                self.assertFalse(shape(obj))


@unittest.skipUnless(BWRAP_OK, "bubblewrap not usable on this host")
class TestLiveMuseSavedKey(unittest.TestCase):
    """The round-3 verifier repro: a sandboxed muse run rewrites a saved API key."""

    SHAPES = {
        "nested provider entry": lambda key: {"schema_version": 1, "providers": {"meta": {
            "mechanism": "api_key", "api_key": key}}},
        "pasted_key lane": lambda key: {"schema_version": 1, "providers": {"meta": {
            "mechanism": "api_key", "obtained_via": "pasted_key", "api_key": key}}},
        "top level": lambda key: {"mechanism": "api_key", "api_key": key},
    }

    @staticmethod
    def _swap(new_doc):
        doc = shlex.quote(json.dumps(new_doc))
        return (f"printf '%s' {doc} > ~/.config/muse/auth.json.tmp && "
                "mv -f ~/.config/muse/auth.json.tmp ~/.config/muse/auth.json; echo answer")

    def _auth(self, home, doc):
        auth = home / ".config" / "muse" / "auth.json"
        auth.parent.mkdir(parents=True)
        auth.write_text(json.dumps(doc))
        os.chmod(auth, 0o600)
        return auth

    def test_saved_key_swap_is_refused_and_reported(self):
        for name, shape in self.SHAPES.items():
            for readonly in (True, False):
                with self.subTest(shape=name, readonly=readonly), vectors.fake_home() as home:
                    auth = self._auth(home, shape("user-saved-key"))
                    before = auth.read_bytes()
                    res, warnings = _live("muse", self._swap(shape("attacker-key")), _workspace(self), readonly)
                    self.assertIn("answer", res.stdout, res.stderr)
                    self.assertEqual(auth.read_bytes(), before)
                    self.assertEqual(len(warnings), 1, warnings)
                    self.assertIn(str(auth), warnings[0])
                    self.assertIn("api_key", warnings[0])

    def test_key_of_the_oauth_login_record_is_still_reminted(self):
        def login(key):
            return {"schema_version": 1, "providers": {"meta": {
                "access_token": "muse-at-1", "mechanism": "bearer", "obtained_via": "device_code", "api_key": key}}}

        with vectors.fake_home() as home:
            auth = self._auth(home, login("mk-1"))
            res, warnings = _live("muse", self._swap(login("mk-2")), _workspace(self), True)
            self.assertIn("answer", res.stdout, res.stderr)
            self.assertEqual(json.loads(auth.read_bytes()), login("mk-2"))
            self.assertEqual(warnings, [])


@unittest.skipIf(os.name != "posix", "private provider homes need bubblewrap (POSIX)")
class TestUnusablePrivateCopyIsReported(unittest.TestCase):
    OLD = b'{"tokens":{"access_token":"a1","refresh_token":"r1"}}'
    REFRESH = b'{"tokens":{"access_token":"a2","refresh_token":"r2"}}'

    def _finalize(self, home, tamper, provider="codex", rel="auth.json"):
        h, priv, host = vectors._materialized_home(home, provider, rel, self.OLD)
        private_dir = h.private_dir
        tamper(priv)
        with patch("makewand.sandbox._warn") as warn:
            h.finalize()
        self.assertFalse(os.path.exists(private_dir))
        return host, [c.args[0] for c in warn.call_args_list]

    def test_each_unusable_copy_is_reported_once(self):
        elsewhere = Path(tempfile.mkdtemp(prefix="wbr4-else-"))
        self.addCleanup(shutil.rmtree, elsewhere, True)
        (elsewhere / "auth.json").write_bytes(self.REFRESH)  # a valid refresh the merge would accept

        def replace(create):
            def tamper(priv):
                priv.unlink()
                create(priv)
            return tamper

        cases = {
            "deleted": (lambda priv: priv.unlink(), "已被删除"),
            "symlink to a valid refresh": (replace(lambda p: p.symlink_to(elsewhere / "auth.json")), "非普通文件"),
            "fifo": (replace(os.mkfifo), "非普通文件"),
            "directory": (replace(lambda p: p.mkdir()), "非普通文件"),
            "symlinked parent directory": (lambda priv: (shutil.rmtree(priv.parent), priv.parent.symlink_to(elsewhere)),
                                           "非普通文件"),
            "oversized": (lambda priv: priv.write_bytes(b'{"x":"' + b"a" * sandbox.CREDENTIAL_MAX_BYTES + b'"}'),
                          "MiB 上限"),
            "unreadable": (lambda priv: os.chmod(priv, 0), "无法读取"),
        }
        for name, (tamper, text) in cases.items():
            if name == "unreadable" and os.geteuid() == 0:
                continue  # root reads a file without permissions
            with self.subTest(name), vectors.fake_home() as home:
                host, messages = self._finalize(home, tamper)
                self.assertEqual(host.read_bytes(), self.OLD)
                self.assertEqual(len(messages), 1, messages)
                self.assertIn(str(host), messages[0])
                self.assertIn(text, messages[0])
                self.assertIn("请在宿主重新登录", messages[0])

    def test_nested_credential_behind_a_symlinked_directory(self):
        rel = "antigravity-cli/antigravity-oauth-token"
        with vectors.fake_home() as home:
            elsewhere = home / "elsewhere"
            elsewhere.mkdir()
            (elsewhere / "antigravity-oauth-token").write_bytes(self.REFRESH)
            host, messages = self._finalize(
                home, lambda priv: (shutil.rmtree(priv.parent), priv.parent.symlink_to(elsewhere)), "agy", rel)
            self.assertEqual(host.read_bytes(), self.OLD)
            self.assertEqual(len(messages), 1, messages)
            self.assertIn(str(host), messages[0])

    def test_absent_copy_is_silent_until_something_unusable_appears(self):
        with vectors.fake_home() as home:
            h, priv, host = vectors._materialized_home(home, "codex", "auth.json", None)
            with patch("makewand.sandbox._warn") as warn:
                h.write_back()
            warn.assert_not_called()
            priv.mkdir()
            with patch("makewand.sandbox._warn") as warn:
                h.finalize()
            messages = [c.args[0] for c in warn.call_args_list]
            self.assertEqual(len(messages), 1, messages)
            self.assertIn(str(host), messages[0])
            self.assertIn("请在宿主直接登录", messages[0])
            self.assertFalse(host.exists())


@unittest.skipIf(os.name != "posix", "private provider homes need bubblewrap (POSIX)")
class TestLoginWithoutHostRoot(unittest.TestCase):
    """A provider root missing on the host gets an empty private directory; a
    login made in it is reported, never written back, and nothing is created on
    the host."""

    CASES = (("claude", ".claude", ".credentials.json"), ("codex", ".codex", "auth.json"),
             ("agy", ".gemini", "antigravity-cli/antigravity-oauth-token"), ("grok", ".grok", "auth.json"),
             ("muse", ".config/muse", "auth.json"))

    def test_login_in_a_missing_root_is_reported(self):
        login = b'{"access_token":"new","refresh_token":"new"}'
        for provider, root_rel, rel in self.CASES:
            for logged_in in (False, True):
                with self.subTest(provider=provider, logged_in=logged_in), vectors.fake_home() as home:
                    root = home / root_rel
                    h = ProviderHome(provider, str(home))
                    args = h.materialize([])
                    privs = [src for src, dst in _binds(args) if dst == str(root)]
                    self.assertEqual(len(privs), 1, args)
                    self.assertTrue(privs[0].startswith(h.private_dir + os.sep), privs)
                    if provider == "agy":
                        found = sorted(str(f.relative_to(privs[0]))
                                       for f in Path(privs[0]).rglob("*") if f.is_file())
                        self.assertEqual(found, ["antigravity-cli/settings.json", "settings.json"])
                        for srel in found:
                            self.assertIs(json.loads(Path(privs[0], srel).read_text(encoding="utf-8"))["useG1Credits"], False)
                    else:
                        self.assertEqual(os.listdir(privs[0]), [])
                    if logged_in:
                        target = Path(privs[0], rel)
                        target.parent.mkdir(parents=True, exist_ok=True)
                        target.write_bytes(login)
                    private_dir = h.private_dir
                    with patch("makewand.sandbox._warn") as warn:
                        h.finalize()
                    messages = [c.args[0] for c in warn.call_args_list]
                    self.assertFalse(os.path.lexists(root))
                    self.assertFalse(os.path.exists(private_dir))
                    if not logged_in:
                        self.assertEqual(messages, [])
                        continue
                    self.assertEqual(len(messages), 1, messages)
                    self.assertIn(str(root / rel), messages[0])
                    self.assertIn("请在宿主直接登录", messages[0])

    def test_roots_without_credentials_get_nothing(self):
        with vectors.fake_home() as home:
            h = ProviderHome("muse", str(home))
            args = h.materialize([])
            self.assertNotIn(str(home / ".local/share/muse"), [dst for _, dst in _binds(args)])
            h.finalize()
            h = ProviderHome("aider", str(home))
            self.assertEqual(h.materialize([]), [])
            self.assertIsNone(h.private_dir)
            h.finalize()

    def test_mount_point_creatable(self):
        home = "/home/u"
        creatable = sandbox._mount_point_creatable
        self.assertTrue(creatable(["--ro-bind", "/", "/", "--tmpfs", "/home", "--tmpfs", home], home + "/.claude"))
        self.assertFalse(creatable(["--ro-bind", "/", "/"], home + "/.claude"))
        self.assertFalse(creatable(["--tmpfs", home, "--ro-bind", home, home], home + "/.claude"))
        self.assertFalse(creatable(["--tmpfs", home, "--bind", "/srv/x", "/home"], home + "/.claude"))
        self.assertFalse(creatable(["--tmpfs", home, "--ro-bind", home + "/.config", home + "/.config"],
                                   home + "/.config/muse"))
        self.assertTrue(creatable(["--tmpfs", home, "--ro-bind", home + "/.local/bin", home + "/.local/bin"],
                                  home + "/.local/share/muse"))
        self.assertTrue(creatable(["--tmpfs", home, "--setenv", "--bind", home, "--chdir", "/"], home + "/.claude"))

    def _wrap(self, home, provider, workspace, **kwargs):
        with patch("makewand.sandbox.resolve_trusted_bwrap", return_value="/usr/bin/bwrap"):
            with sandbox.provider_home_session(provider, [str(workspace)]) as session:
                return wrap_bwrap(["bash"], workspace=str(workspace), is_provider=True, provider_name=provider,
                                  provider_home=session, **kwargs)

    def test_no_private_directory_where_bwrap_cannot_create_the_mount_point(self):
        with vectors.fake_home() as home:
            # A read-only review of HOME itself shows HOME read-only.
            cmd = self._wrap(home, "claude", home, readonly=True)
            self.assertNotIn(str(home / ".claude"), [dst for _, dst in _binds(cmd)])
            # A read-only bind of ~/.config covers ~/.config/muse.
            (home / ".config").mkdir()
            ws = _workspace(self)
            self.assertIn(str(home / ".config/muse"), [dst for _, dst in _binds(self._wrap(home, "muse", ws))])
            cmd = self._wrap(home, "muse", ws, extra_ro_binds=[str(home / ".config")])
            self.assertIn((str(home / ".config"), str(home / ".config")), _ro_binds(cmd))
            self.assertNotIn(str(home / ".config/muse"), [dst for _, dst in _binds(cmd)])
            self.assertFalse(os.path.lexists(home / ".claude"))
            self.assertFalse(os.path.lexists(home / ".config/muse"))


@unittest.skipIf(os.name != "posix", "private provider homes need bubblewrap (POSIX)")
class TestAgySyntheticSettingsNeverWrittenBack(unittest.TestCase):
    """The sanitized agy settings copies in the private home are makewand's own
    artifacts: tampering with them is not a login, is never written back to the
    host (also through a symlinked ~/.gemini) and never warns."""

    HOST_FILES = {
        "settings.json": b'{"useG1Credits": true, "theme": "dark"}\n',
        "antigravity-cli/settings.json": b'{"useG1Credits": true, "model": "x"}\n',
    }

    def test_tampered_private_settings_never_reach_the_host(self):
        for symlinked in (False, True):
            with self.subTest(symlinked=symlinked), vectors.fake_home() as home:
                real = home / "relocated" / "gemini" if symlinked else home / ".gemini"
                real.mkdir(parents=True)
                for rel, content in self.HOST_FILES.items():
                    target = real / rel
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_bytes(content)
                if symlinked:
                    (home / ".gemini").symlink_to(real, target_is_directory=True)
                h = ProviderHome("agy", str(home))
                h.materialize([])
                priv = Path(h._private_roots[".gemini"])
                (priv / "settings.json").write_bytes(b'{"useG1Credits": true, "pwned": 1}\n')
                (priv / "antigravity-cli" / "settings.json").write_bytes(b'{"useG1Credits": true, "pwned": 2}\n')
                with patch("makewand.sandbox._warn") as warn:
                    written = h.write_back()
                self.assertEqual(written, [])
                warn.assert_not_called()
                private_dir = h.private_dir
                with patch("makewand.sandbox._warn") as warn:
                    h.finalize()
                warn.assert_not_called()
                self.assertFalse(os.path.exists(private_dir))
                for rel, content in self.HOST_FILES.items():
                    self.assertEqual((real / rel).read_bytes(), content)


@unittest.skipUnless(BWRAP_OK, "bubblewrap not usable on this host")
class TestLiveLoginWithoutHostRoot(unittest.TestCase):
    TOKEN = ".gemini/antigravity-cli/antigravity-oauth-token"
    LOGIN = ("mkdir -p ~/.gemini/antigravity-cli && printf '%s' "
             + shlex.quote('{"token":{"access_token":"ya29.N","refresh_token":"1//N"}}')
             + " > ~/" + TOKEN + " && echo LOGGEDIN")

    def test_login_is_reported_in_read_only_and_writable_runs(self):
        for readonly in (True, False):
            with self.subTest(readonly=readonly), vectors.fake_home() as home:
                res, warnings = _live("agy", self.LOGIN, _workspace(self), readonly)
                self.assertIn("LOGGEDIN", res.stdout, res.stderr)
                self.assertEqual(len(warnings), 1, warnings)
                self.assertIn(str(home / self.TOKEN), warnings[0])
                self.assertIn("请在宿主直接登录", warnings[0])
                self.assertFalse(os.path.lexists(home / ".gemini"))

    def test_run_without_login_is_silent(self):
        with vectors.fake_home() as home:
            res, warnings = _live("agy", "echo ok", _workspace(self), True)
            self.assertIn("ok", res.stdout, res.stderr)
            self.assertEqual(warnings, [])
            self.assertFalse(os.path.lexists(home / ".gemini"))

    def test_read_only_review_of_home_still_launches(self):
        """HOME is visible read-only there, so no mount point can be created at
        the missing root: nothing is bound and the run still starts."""
        with vectors.fake_home() as home:
            res, warnings = _live("agy", "echo ok", home, True)
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertIn("ok", res.stdout)
            self.assertFalse(os.path.lexists(home / ".gemini"))


if __name__ == "__main__":
    unittest.main()
