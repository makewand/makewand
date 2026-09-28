"""
G4 regression tests: bounded build-time scans in wrap_bwrap (eng-delivery#7).

wrap_bwrap used to os.walk the whole workspace twice without any limit — for a
workspace of /tmp that meant walking every other session's repository and
binding all of their .git directories into the command line (minutes under
strace, cross-tenant paths in logs). Scans are now bounded: no descent into .git
internals or node_modules, an entry cap, and a shallow depth for broad
workspaces (/, /tmp, /var/tmp, HOME or its ancestors).
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
    home = str(Path(__file__).resolve().parent)
    with patch.dict(os.environ, {"HOME": home}):
        yield home


class TestBoundedWorkspaceScan(unittest.TestCase):
    def test_git_internals_and_node_modules_are_not_walked(self):
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
            self.assertNotIn(str(nested / ".git"), bound)
            self.assertFalse(any("/.git/" in p or p.endswith("/.git") for p in seen if p.startswith(ws)), seen)
            self.assertFalse(any("node_modules" in p for p in seen if p.startswith(ws)), seen)

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
                cmd = wrap_bwrap(["true"], workspace=ws)
            bound = ro_bound(cmd)
            self.assertNotIn(str(deep / ".git"), bound)
            self.assertIn("截断", err.getvalue())

    def test_tmp_workspace_scan_is_shallow_and_fast(self):
        base = Path(tempfile.mkdtemp(prefix="g4-scan-"))  # /tmp/g4-scan-xxx
        try:
            (base / ".git").mkdir()  # depth 2 below /tmp
            deep = base / "a" / "b" / "c"
            deep.mkdir(parents=True)
            (deep / ".git").mkdir()  # depth 5 below /tmp
            with home_outside_tmp():
                t0 = time.monotonic()
                cmd = wrap_bwrap(["true"], workspace="/tmp")
                elapsed = time.monotonic() - t0
            bound = ro_bound(cmd)
            self.assertIn(str(base / ".git"), bound)
            self.assertNotIn(str(deep / ".git"), bound)
            for p in bound:
                if p.startswith("/tmp/") and p.endswith("/.git"):
                    self.assertLessEqual(len(Path(p).relative_to("/tmp").parts), sandbox.BROAD_SCAN_DEPTH, p)
            self.assertLess(elapsed, 30.0)
        finally:
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
