"""agy helper-script directory is writable inside the sandbox, read-only outside.

agy writes a small script to <data dir>/bin/agentapi before every run_command
(`exec ".../agy" agentapi "$@"`). That directory (``~/.gemini/antigravity-cli/bin``
on the host) used to be mounted read-only, so every sandboxed agy command
failed with "read-only file system".

Both engines now overlay a tmpfs at the bin path and re-bind the shipped host
files read-only, except ``agentapi`` (agy creates it itself; symlinks,
directories and other non-regular entries are never re-bound). The tmpfs dies
with the sandbox: host bytes and modes are unchanged and agentapi never
reaches the host. Read-only and writable sessions behave the same.

Everything runs against a throw-away HOME; no provider CLI is started (the
live cases run bash inside the real bubblewrap sandbox when it is usable).
"""

try:  # isolation must precede makewand imports
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import json
import os
import shutil
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import sandbox
from makewand.sandbox import ProviderHome, wrap_bwrap

BWRAP_OK = sandbox.is_bwrap_available()


def _ro_binds(args):
    return [(args[i + 1], args[i + 2]) for i, a in enumerate(args[:-2]) if a == "--ro-bind"]


def _binds(args):
    return [(args[i + 1], args[i + 2]) for i, a in enumerate(args[:-2]) if a == "--bind"]


def _tmpfs(args):
    return [args[i + 1] for i, a in enumerate(args[:-1]) if a == "--tmpfs"]


@contextlib.contextmanager
def _fake_home():
    home = Path(tempfile.mkdtemp(prefix="agybin-home-"))
    try:
        with patch.dict(os.environ, {"HOME": str(home)}):
            os.environ.pop("CODEX_HOME", None)
            yield home
    finally:
        shutil.rmtree(home, ignore_errors=True)


def _seed_bin(home, with_agentapi_stale=True):
    """Host ~/.gemini/antigravity-cli/bin with one of everything."""
    bindir = home / ".gemini" / "antigravity-cli" / "bin"
    bindir.mkdir(parents=True)
    (bindir / "webm_encoder").write_bytes(b"WEBM\x00ENCODER\xff")
    os.chmod(bindir / "webm_encoder", 0o755)
    (bindir / "helper.sh").write_bytes(b"#!/bin/sh\necho hi\n")
    os.chmod(bindir / "helper.sh", 0o644)
    if with_agentapi_stale:
        (bindir / "agentapi").write_bytes(b"stale host copy\n")
        os.chmod(bindir / "agentapi", 0o755)
    os.symlink("webm_encoder", bindir / "link-to-encoder")
    (bindir / "subdir").mkdir()
    (bindir / "subdir" / "inner").write_bytes(b"inner\n")
    os.mkfifo(bindir / "afifo")
    builtin = home / ".gemini" / "antigravity-cli" / "builtin"
    builtin.mkdir(parents=True)
    (builtin / "tool").write_bytes(b"builtin\n")
    (home / ".gemini" / "settings.json").write_text('{"useG1Credits": true, "theme": "dark"}\n')
    cli_settings = home / ".gemini" / "antigravity-cli" / "settings.json"
    cli_settings.write_text('{"useG1Credits": true}\n')
    return bindir


def _snapshot_dir(bindir):
    """Names, kinds, bytes and modes below bindir (never opens fifos/sockets)."""
    snap = {}
    for entry in sorted(bindir.iterdir(), key=lambda q: q.name):
        st = os.lstat(entry)
        mode = stat.S_IMODE(st.st_mode)
        if stat.S_ISLNK(st.st_mode):
            snap[entry.name] = ("link", os.readlink(entry), mode)
        elif stat.S_ISREG(st.st_mode):
            snap[entry.name] = ("file", entry.read_bytes(), mode)
        elif stat.S_ISDIR(st.st_mode):
            snap[entry.name] = ("dir", sorted(q.name for q in entry.iterdir()), mode)
        else:
            snap[entry.name] = ("other", stat.S_IFMT(st.st_mode), mode)
    return snap


@unittest.skipIf(os.name != "posix", "private provider homes need bubblewrap (POSIX)")
class TestAgyBinMountPlan(unittest.TestCase):
    def test_bin_gets_tmpfs_with_shipped_files_read_only(self):
        with _fake_home() as home:
            bindir = _seed_bin(home)
            before = _snapshot_dir(bindir)
            h = ProviderHome("agy", str(home))
            try:
                args = h.materialize([])
                self.assertIn(str(bindir), _tmpfs(args))
                ro = _ro_binds(args)
                self.assertIn((str(bindir / "webm_encoder"),) * 2, ro)
                self.assertIn((str(bindir / "helper.sh"),) * 2, ro)
                # agentapi is never mounted from the host (stale or not).
                for src, dst in ro:
                    self.assertNotEqual(os.path.basename(src), "agentapi", args)
                    self.assertNotEqual(os.path.basename(dst), "agentapi", args)
                # Symlinks, directories and fifos are never re-bound.
                for p in ("link-to-encoder", "subdir", "afifo"):
                    for src, dst in ro:
                        self.assertNotIn(str(bindir / p), (src, dst), args)
                # The whole directory is not bound (that would be read-only).
                self.assertNotIn((str(bindir), str(bindir)), ro + _binds(args))
                # Other protected paths are still read-only, without a tmpfs.
                builtin = home / ".gemini" / "antigravity-cli" / "builtin"
                self.assertIn((str(builtin), str(builtin)), ro)
                self.assertNotIn(str(builtin), _tmpfs(args))
                # The sanitized settings are still what agy reads.
                host_settings = str(home / ".gemini" / "settings.json")
                shadows = [src for src, dst in ro if dst == host_settings]
                self.assertTrue(shadows, args)
                self.assertNotEqual(shadows[-1], host_settings)
                doc = json.loads(Path(shadows[-1]).read_text(encoding="utf-8"))
                self.assertIs(doc["useG1Credits"], False)
            finally:
                h.finalize()
            after = _snapshot_dir(bindir)
            self.assertEqual(after, before)

    def test_missing_bin_gets_empty_tmpfs(self):
        with _fake_home() as home:
            gemini = home / ".gemini"
            gemini.mkdir(parents=True)
            (gemini / "settings.json").write_text("{}\n")
            h = ProviderHome("agy", str(home))
            try:
                args = h.materialize([])
                bindir = str(gemini / "antigravity-cli" / "bin")
                self.assertIn(bindir, _tmpfs(args))
                for src, dst in _ro_binds(args):
                    self.assertFalse(dst == bindir or dst.startswith(bindir + os.sep), args)
                    self.assertFalse(src == bindir or src.startswith(bindir + os.sep), args)
            finally:
                h.finalize()
            self.assertFalse(os.path.lexists(gemini / "antigravity-cli" / "bin"))

    def test_absent_gemini_gets_empty_tmpfs(self):
        with _fake_home() as home:
            h = ProviderHome("agy", str(home))
            try:
                args = h.materialize([])
                root = str(home / ".gemini")
                self.assertEqual([dst for _, dst in _binds(args) if dst == root], [root], args)
                self.assertIn(os.path.join(root, "antigravity-cli", "bin"), _tmpfs(args))
            finally:
                h.finalize()
            self.assertFalse(os.path.lexists(home / ".gemini"))

    def test_other_providers_keep_read_only_bin(self):
        with _fake_home() as home:
            grokbin = home / ".grok" / "bin"
            grokbin.mkdir(parents=True)
            (grokbin / "grok").write_bytes(b"tool\n")
            h = ProviderHome("grok", str(home))
            try:
                args = h.materialize([])
                self.assertIn((str(grokbin), str(grokbin)), _ro_binds(args))
                self.assertNotIn(str(grokbin), _tmpfs(args))
            finally:
                h.finalize()

    def test_readonly_and_writable_wrap_plans(self):
        for readonly in (False, True):
            with self.subTest(readonly=readonly), _fake_home() as home:
                _seed_bin(home)
                ws = Path(tempfile.mkdtemp(prefix="agybin-ws-"))
                self.addCleanup(shutil.rmtree, ws, True)
                with patch("makewand.sandbox.resolve_trusted_bwrap", return_value="/usr/bin/bwrap"):
                    with sandbox.provider_home_session("agy", [str(ws)]) as session:
                        cmd = wrap_bwrap(["bash", "-c", "true"], workspace=str(ws), is_provider=True,
                                         provider_name="agy", readonly=readonly, provider_home=session)
                bindir = str(home / ".gemini" / "antigravity-cli" / "bin")
                self.assertIn(bindir, _tmpfs(cmd), cmd)
                ro = _ro_binds(cmd)
                self.assertIn((os.path.join(bindir, "webm_encoder"),) * 2, ro, cmd)
                for src, dst in ro:
                    self.assertNotEqual(os.path.basename(src), "agentapi", cmd)
                    self.assertNotEqual(os.path.basename(dst), "agentapi", cmd)


_LIVE_NESTED = None


def _live_bwrap_runs():
    """True when a real agy sandbox can start here (nested bwrap may fail)."""
    global _LIVE_NESTED
    if _LIVE_NESTED is None:
        _LIVE_NESTED = False
        if BWRAP_OK:
            try:
                with _fake_home():
                    ws = tempfile.mkdtemp(prefix="agybin-probe-")
                    try:
                        with sandbox.provider_sandbox_launch(
                                "agy", ["true"], ws, readonly=False, allow_network=False) as launch:
                            res = subprocess.run(launch.cmd, cwd=ws, capture_output=True,
                                                 pass_fds=launch.pass_fds, timeout=120)
                        _LIVE_NESTED = res.returncode == 0
                    finally:
                        shutil.rmtree(ws, ignore_errors=True)
            except Exception:
                _LIVE_NESTED = False
    return _LIVE_NESTED


@unittest.skipUnless(BWRAP_OK, "bubblewrap not usable on this host")
class TestLiveAgyBinWritable(unittest.TestCase):
    PAYLOAD = (
        'set -u; BIN="$HOME/.gemini/antigravity-cli/bin"; '
        'printf \'#!/bin/sh\\nexec "agy" agentapi "$@"\\n\' > "$BIN/agentapi"; '
        'chmod +x "$BIN/agentapi"; '
        'echo "AGENTAPI:"; cat "$BIN/agentapi"; '
        'echo "WEBM:"; cat "$BIN/webm_encoder"; '
        'echo DONE'
    )

    def test_agentapi_writable_host_untouched(self):
        if not _live_bwrap_runs():
            self.skipTest("bwrap cannot nest here (e.g. /proc mount fails)")
        for readonly in (False, True):
            with self.subTest(readonly=readonly), _fake_home() as home:
                bindir = _seed_bin(home)
                before = _snapshot_dir(bindir)
                ws = Path(tempfile.mkdtemp(prefix="agybin-live-"))
                self.addCleanup(shutil.rmtree, ws, True)
                with patch("makewand.sandbox._warn") as warn:
                    with sandbox.provider_sandbox_launch(
                            "agy", ["bash", "-c", self.PAYLOAD], str(ws),
                            readonly=readonly, allow_network=False) as launch:
                        # Bytes: the seeded webm_encoder is binary (it ends in 0xff).
                        res = subprocess.run(launch.cmd, cwd=str(ws), capture_output=True,
                                             pass_fds=launch.pass_fds, timeout=120)
                warnings = [c.args[0] for c in warn.call_args_list]
                err = res.stderr.decode("utf-8", "replace")
                self.assertEqual(res.returncode, 0, err)
                self.assertIn(b"DONE", res.stdout, err)
                self.assertIn(b'exec "agy" agentapi', res.stdout, err)
                self.assertIn(b"WEBM:\nWEBM\x00ENCODER\xff", res.stdout, err)
                self.assertEqual(warnings, [], warnings)
                # Host bytes and modes unchanged; the sandbox agentapi is gone.
                after = _snapshot_dir(bindir)
                self.assertEqual(after, before)
                self.assertEqual((bindir / "agentapi").read_bytes(), b"stale host copy\n")


if __name__ == "__main__":
    unittest.main()
