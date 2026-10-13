"""
Credential write-back robustness (round 3 of py-sec-3).

* A failure while writing back one credential file (a document the merge
  cannot handle, an unexpected error) is reported and never escapes
  ProviderHome.finalize() or provider_sandbox_launch: the other files of the
  run are still written back and the provider's answer is kept. Lone
  surrogate escapes, which crashed the re-encoding of a merged document, are
  invalid JSON for the strict decoder.
* Removing the private home cannot raise either, however deep the tree a
  sandboxed process left in it.
* makewand's own write-backs of one host credential file are serialized by an
  advisory lock both engines share (the sidecar ".<name>.makewand-lock"), and
  the host file's identity (device, inode, size, mtime_ns, digest) is checked
  again immediately before the rename: a host change that lands meanwhile is
  kept, not overwritten.

Everything runs against a throw-away HOME; no provider CLI is started (the live
case runs bash inside the real bubblewrap sandbox when it is usable).
"""

try:  # isolation must precede makewand imports
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

try:
    import test_fix_writeback_identity as vectors
except ImportError:
    from tests import test_fix_writeback_identity as vectors

import glob
import json
import os
import shlex
import shutil
import subprocess
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import sandbox
from makewand.sandbox import ProviderHome

try:
    import fcntl
except ImportError:  # pragma: no cover - POSIX only
    fcntl = None

# Two credential files of one agy run: a refused document first, a legitimate
# refresh after it.
OAUTH = ".gemini/oauth_creds.json"
TOKEN = ".gemini/antigravity-cli/antigravity-oauth-token"
TOKEN_REFRESH = "agy token refresh with a rotated id_token (same sub/email)"
OAUTH_REFRESH = "gemini refresh with reordered scopes: the scopes stay as on the host"


def _case(name):
    return next(c for c in vectors.load_cases() if c["name"] == name)


def _agy_home(home, oauth_new=None, token_new=None):
    """A materialized agy ProviderHome whose private copies hold the given sandbox documents."""
    files = {OAUTH: _case(OAUTH_REFRESH), TOKEN: _case(TOKEN_REFRESH)}
    for rel, case in files.items():
        host = home / rel
        host.parent.mkdir(parents=True, exist_ok=True)
        host.write_bytes(vectors.file_bytes(case["host"]))
        os.chmod(host, 0o600)
    h = ProviderHome("agy", str(home))
    h.materialize([])
    for rel, new in ((OAUTH, oauth_new), (TOKEN, token_new)):
        if new is not None:
            cred = next(c for c in h.credentials if c.rel == rel.split("/", 1)[1])
            Path(cred.private_root, cred.rel).write_bytes(new)
    return h


class TestWriteBackFailureNeverEscapes(unittest.TestCase):
    def test_lone_surrogate_is_refused_and_the_next_file_is_written_back(self):
        """The verifier repro: a lone surrogate escape in a token plus a benign change."""
        with vectors.fake_home() as home:
            bad = json.dumps(dict(json.loads(vectors.file_bytes(_case(OAUTH_REFRESH)["sandbox"])),
                                  scope="openid")).replace('"ya29.VICTIM-2"', '"ya29.\\ud800x"').encode()
            self.assertIn(b"\\ud800", bad)
            token_new = vectors.file_bytes(_case(TOKEN_REFRESH)["sandbox"])
            h = _agy_home(home, oauth_new=bad, token_new=token_new)
            oauth_before = (home / OAUTH).read_bytes()
            with patch("makewand.sandbox._warn") as warn:
                h.finalize()
            messages = [c.args[0] for c in warn.call_args_list]
            self.assertEqual(len(messages), 1, messages)
            self.assertIn(str(home / OAUTH), messages[0])
            self.assertEqual((home / OAUTH).read_bytes(), oauth_before)
            self.assertEqual(vectors.canonical(json.loads((home / TOKEN).read_bytes())),
                             vectors.canonical(vectors.expand(_case(TOKEN_REFRESH)["expect"]["host"])))

    def test_unexpected_failure_for_one_file_is_reported_and_the_others_are_written_back(self):
        real_merge = sandbox._credential_writeback_merge

        def merge(old, new, minted_keys=()):
            if b"cloud-platform" in new:  # oauth_creds.json
                raise RuntimeError("merge bug")
            return real_merge(old, new, minted_keys)

        with vectors.fake_home() as home:
            h = _agy_home(home, oauth_new=vectors.file_bytes(_case(OAUTH_REFRESH)["sandbox"]),
                          token_new=vectors.file_bytes(_case(TOKEN_REFRESH)["sandbox"]))
            private_dir = h.private_dir
            oauth_before = (home / OAUTH).read_bytes()
            with patch("makewand.sandbox._credential_writeback_merge", side_effect=merge), \
                    patch("makewand.sandbox._warn") as warn:
                h.finalize()
            messages = [c.args[0] for c in warn.call_args_list]
            self.assertEqual(len(messages), 1, messages)
            self.assertIn(str(home / OAUTH), messages[0])
            self.assertIn("RuntimeError", messages[0])
            self.assertNotIn("merge bug", messages[0])
            self.assertEqual((home / OAUTH).read_bytes(), oauth_before)
            self.assertEqual(vectors.canonical(json.loads((home / TOKEN).read_bytes())),
                             vectors.canonical(vectors.expand(_case(TOKEN_REFRESH)["expect"]["host"])))
            self.assertFalse(os.path.exists(private_dir))

    @unittest.skipUnless(os.mkdir in os.supports_dir_fd, "builds the tree with dir_fd (POSIX)")
    def test_deeply_nested_private_tree_is_removed_without_raising(self):
        """A tree deeper than the recursion limit, as a sandboxed process can leave one."""
        with vectors.fake_home() as home:
            h = _agy_home(home)
            private_dir = h.private_dir
            fd = os.open(h.credentials[0].private_root, os.O_RDONLY)
            try:
                for _ in range(1500):
                    os.mkdir("d", dir_fd=fd)
                    child = os.open("d", os.O_RDONLY, dir_fd=fd)
                    os.close(fd)
                    fd = child
            finally:
                os.close(fd)
            h.finalize()
            self.assertFalse(os.path.exists(private_dir))

    @unittest.skipUnless(sandbox.is_bwrap_available(), "bubblewrap not usable on this host")
    def test_provider_sandbox_launch_keeps_the_answer(self):
        """End to end through provider_sandbox_launch (the round-2 verifier repro)."""
        with vectors.fake_home() as home:
            ws = Path(tempfile.mkdtemp(prefix="wbr3-ws-"))
            try:
                for rel, name in ((OAUTH, OAUTH_REFRESH), (TOKEN, TOKEN_REFRESH)):
                    host = home / rel
                    host.parent.mkdir(parents=True, exist_ok=True)
                    host.write_bytes(vectors.file_bytes(_case(name)["host"]))
                    os.chmod(host, 0o600)
                bad = json.dumps(dict(json.loads(vectors.file_bytes(_case(OAUTH_REFRESH)["sandbox"])),
                                      scope="openid")).replace('"ya29.VICTIM-2"', '"ya29.\\ud800x"')
                new = {OAUTH: bad, TOKEN: vectors.file_bytes(_case(TOKEN_REFRESH)["sandbox"]).decode()}
                payload = "".join(f"printf '%s' {shlex.quote(text)} > ~/{rel}.tmp && mv -f ~/{rel}.tmp ~/{rel}; "
                                  for rel, text in new.items()) + "echo agent-answer"
                oauth_before = (home / OAUTH).read_bytes()
                with patch("makewand.sandbox._warn"):
                    with sandbox.provider_sandbox_launch("agy", ["bash", "-c", payload], str(ws), readonly=True,
                                                         allow_network=False) as launch:
                        res = subprocess.run(launch.cmd, cwd=str(ws), capture_output=True, text=True,
                                             pass_fds=launch.pass_fds, timeout=60)
                self.assertIn("agent-answer", res.stdout, res.stderr)
                self.assertEqual((home / OAUTH).read_bytes(), oauth_before)
                self.assertEqual(vectors.canonical(json.loads((home / TOKEN).read_bytes())),
                                 vectors.canonical(vectors.expand(_case(TOKEN_REFRESH)["expect"]["host"])))
            finally:
                shutil.rmtree(ws, ignore_errors=True)


@unittest.skipIf(fcntl is None, "flock(2) is POSIX only")
class TestCredentialLockAndIdentityRecheck(unittest.TestCase):
    OLD = b'{"access_token":"a1","refresh_token":"r1"}'
    NEW = b'{"access_token":"a2","refresh_token":"r1"}'

    def _home(self, home):
        h, priv, host = vectors._materialized_home(home, "codex", "auth.json", self.OLD)
        priv.write_bytes(self.NEW)
        return h, host

    @staticmethod
    def _hold_lock(host):
        """Holds the sidecar lock like another makewand write-back (any engine) would."""
        fd = os.open(str(host.parent / ("." + host.name + ".makewand-lock")), os.O_RDWR | os.O_CREAT, 0o600)
        fcntl.flock(fd, fcntl.LOCK_EX)
        return fd

    def test_lock_path_is_the_sidecar_both_engines_use(self):
        self.assertEqual(sandbox._credential_lock_path("/home/u/.codex/auth.json"),
                         "/home/u/.codex/.auth.json.makewand-lock")

    def test_host_change_while_waiting_for_the_lock_is_kept(self):
        def refresh_in_place(host):
            host.write_bytes(b'{"access_token":"host","refresh_token":"r1"}')

        def replace_identical(host):
            tmp = host.with_name(host.name + ".new")
            tmp.write_bytes(self.OLD)
            os.replace(tmp, host)

        def touch(host):
            later = time.time() + 3600
            os.utime(host, (later, later))

        for change, want in ((refresh_in_place, b'{"access_token":"host","refresh_token":"r1"}'),
                             (replace_identical, self.OLD), (touch, self.OLD)):
            with self.subTest(change.__name__), vectors.fake_home() as home:
                h, host = self._home(home)
                fd = self._hold_lock(host)
                try:
                    with patch("makewand.sandbox._warn") as warn:
                        worker = threading.Thread(target=h.finalize)
                        worker.start()
                        # The synced temporary file next to the host file shows
                        # the write-back has merged and now waits for the lock.
                        deadline = time.monotonic() + 10
                        while not glob.glob(str(host.parent / ".makewand-wb-*")):
                            self.assertTrue(worker.is_alive(),
                                            "write-back finished while another write-back held the lock")
                            self.assertLess(time.monotonic(), deadline, "write-back never reached the lock")
                            time.sleep(0.01)
                        change(host)
                        fcntl.flock(fd, fcntl.LOCK_UN)
                        worker.join(10)
                        self.assertFalse(worker.is_alive())
                finally:
                    os.close(fd)
                self.assertEqual(host.read_bytes(), want)
                messages = [c.args[0] for c in warn.call_args_list]
                self.assertEqual(len(messages), 1, messages)
                self.assertIn("在运行期间被其他会话修改", messages[0])
                self.assertEqual(glob.glob(str(host.parent / ".makewand-wb-*")), [])

    def test_lock_timeout_keeps_the_host_copy(self):
        with vectors.fake_home() as home:
            h, host = self._home(home)
            fd = self._hold_lock(host)
            try:
                with patch("makewand.sandbox._CRED_LOCK_WAIT", 0.2), patch("makewand.sandbox._warn") as warn:
                    h.finalize()
            finally:
                os.close(fd)
            self.assertEqual(host.read_bytes(), self.OLD)
            messages = [c.args[0] for c in warn.call_args_list]
            self.assertEqual(len(messages), 1, messages)
            self.assertIn("等待锁超时", messages[0])
            self.assertEqual(glob.glob(str(host.parent / ".makewand-wb-*")), [])

    def test_write_back_proceeds_once_the_lock_is_free(self):
        with vectors.fake_home() as home:
            h, host = self._home(home)
            os.close(self._hold_lock(host))  # an earlier write-back that finished
            with patch("makewand.sandbox._warn") as warn:
                h.finalize()
            warn.assert_not_called()
            self.assertEqual(host.read_bytes(), self.NEW)


if __name__ == "__main__":
    unittest.main()
