"""
G4 regression tests: bounded build-time scans in wrap_bwrap (eng-delivery#7).

wrap_bwrap used to os.walk the whole workspace twice without any limit — for a
workspace of /tmp that meant walking every other session's repository and
binding all of their .git directories into the command line (minutes under
strace, cross-tenant paths in logs). Scans are now bounded: an entry cap and a
shallow depth for broad workspaces (/, /tmp, /var/tmp, HOME or its ancestors).
H5: node_modules and .git internals ARE walked (a pathname socket there must be
masked); a truncated scan refuses writable sessions and warns in read-only ones.
"""

import contextlib
import io
import os
import shutil
import socket
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from makewand import sandbox
from makewand.sandbox import wrap_bwrap


def ro_bound(cmd):
    return {cmd[i + 1] for i, x in enumerate(cmd[:-2]) if x == "--ro-bind"}


@contextlib.contextmanager
def home_outside_tmp():
    # Argument-only tests: HOME must not be /tmp or below it, otherwise /tmp
    # itself would be an ancestor of HOME and a writable /tmp workspace refused.
    # This nonexistent argument-only HOME is independent of the checkout's
    # location; a source copy under /tmp must not turn /tmp into HOME's parent.
    home = "/makewand-test-argument-home"
    with patch.dict(os.environ, {"HOME": home}):
        yield home


class TestBoundedWorkspaceScan(unittest.TestCase):
    def test_git_internals_and_node_modules_are_walked_for_sockets(self):
        with tempfile.TemporaryDirectory() as ws:
            root = Path(ws)
            (root / ".git" / "objects" / "aa").mkdir(parents=True)
            for i in range(50):
                (root / ".git" / "objects" / "aa" / f"obj{i}").write_text("x")
            nested = root / "node_modules" / "pkg"
            nested.mkdir(parents=True)
            (nested / ".git").mkdir()
            (root / "src" / "sub").mkdir(parents=True)
            (root / "src" / "sub" / ".git").write_text("gitdir: ../../.git/modules/sub\n")
            seen = []
            real_scandir = os.scandir

            def spy(path):
                seen.append(str(path))
                return real_scandir(path)

            with patch("makewand.sandbox.os.scandir", side_effect=spy):
                cmd = wrap_bwrap(["true"], workspace=ws)
            bound = ro_bound(cmd)
            self.assertIn(str(root / ".git"), bound)
            self.assertIn(str(root / "src" / "sub" / ".git"), bound)
            # a nested repository below node_modules is protected as well
            self.assertIn(str(nested / ".git"), bound)
            # .git internals are walked (sockets only): no .git entries recorded below .git
            self.assertIn(str(root / ".git" / "objects" / "aa"), seen)
            self.assertFalse(any(p.startswith(str(root / ".git") + "/") and p.endswith("/.git") for p in bound), bound)
            self.assertIn(str(root / "node_modules"), seen)
            self.assertIn(str(nested), seen)

    def test_entry_limit_truncates_scan_with_warning(self):
        with tempfile.TemporaryDirectory() as ws:
            root = Path(ws)
            (root / ".git").mkdir()
            for i in range(300):
                (root / f"f{i:03d}").write_text("x")
            deep = root / "zz" / "nested"
            deep.mkdir(parents=True)
            (deep / ".git").mkdir()
            err = io.StringIO()
            with patch.object(sandbox, "SCAN_ENTRY_LIMIT", 100, create=True), patch("sys.stderr", err):
                # writable: .git protection would be incomplete -> fail closed
                with self.assertRaisesRegex(sandbox.SandboxConfigError, "100"):
                    wrap_bwrap(["true"], workspace=ws)
                # read-only: no .git binds needed; the socket scan is truncated with a warning
                cmd = wrap_bwrap(["true"], workspace=ws, readonly=True)
            bound = ro_bound(cmd)
            self.assertIn(ws, bound)
            self.assertNotIn(str(deep / ".git"), bound)
            self.assertIn("截断", err.getvalue())

    def test_tmp_workspace_scan_is_shallow_and_fast(self):
        base = Path(tempfile.mkdtemp(prefix="g4-scan-"))  # /tmp/g4-scan-xxx
        socks = []
        try:
            # Writable /tmp is a forbidden root, so exercise the bounded broad
            # scan through the read-only path, where it masks pathname sockets.
            deep = base / "a" / "b" / "c"
            deep.mkdir(parents=True)
            shallow_sock, deep_sock = base / "s.sock", deep / "s.sock"  # depth 2 / 5 below /tmp
            for sp in (shallow_sock, deep_sock):
                sk = socket.socket(socket.AF_UNIX)
                sk.bind(str(sp))
                socks.append(sk)
            real_scandir = os.scandir

            class FixtureRootEntries:
                def __init__(self, entries):
                    self.entries = entries

                def __enter__(self):
                    self.entries.__enter__()
                    return self

                def __exit__(self, *error):
                    return self.entries.__exit__(*error)

                def __iter__(self):
                    return (entry for entry in self.entries if entry.name == base.name)

            def fixture_scandir(path):
                # Keep the real /tmp broad-workspace branch and real child
                # entries, but don't spend its entry budget on other sessions.
                entries = real_scandir(path)
                return FixtureRootEntries(entries) if os.fspath(path) == "/tmp" else entries

            with home_outside_tmp() as home, patch("makewand.sandbox.os.scandir", side_effect=fixture_scandir):
                self.assertFalse(Path(home).is_relative_to("/tmp"))
                t0 = time.monotonic()
                cmd = wrap_bwrap(["true"], workspace="/tmp", readonly=True)
                elapsed = time.monotonic() - t0
                with self.assertRaisesRegex(sandbox.SandboxConfigError, "/tmp"):
                    wrap_bwrap(["true"], workspace="/tmp")
            masked = {cmd[i + 2] for i, x in enumerate(cmd[:-2]) if x == "--ro-bind" and cmd[i + 1] == "/dev/null"}
            self.assertIn(str(shallow_sock), masked)
            self.assertNotIn(str(deep_sock), masked)
            for p in masked:
                if p.startswith("/tmp/"):
                    self.assertLessEqual(len(Path(p).relative_to("/tmp").parts), sandbox.BROAD_SCAN_DEPTH, p)
            self.assertLess(elapsed, 30.0)
        finally:
            for sk in socks:
                sk.close()
            shutil.rmtree(base, ignore_errors=True)

    def test_workspace_sockets_still_masked(self):
        with tempfile.TemporaryDirectory() as ws:
            sock_path = Path(ws) / "sub" / "agent.sock"
            sock_path.parent.mkdir()
            s = socket.socket(socket.AF_UNIX)
            s.bind(str(sock_path))
            try:
                cmd = wrap_bwrap(["true"], workspace=ws, readonly=True)
            finally:
                s.close()
            idx = [i for i, x in enumerate(cmd[:-2]) if x == "--ro-bind" and cmd[i + 1] == "/dev/null" and cmd[i + 2] == str(sock_path)]
            self.assertTrue(idx)


if __name__ == "__main__":
    unittest.main()
