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
            self.assertIn("工作区扫描未能完成", err.getvalue())

    def test_tmp_workspace_scan_is_shallow_and_fast(self):
        # Directly in /tmp, the broad workspace scanned here, whatever TMPDIR says
        # (a TMPDIR of /tmp/x would put it a level deeper, outside the fixture).
        base = Path(tempfile.mkdtemp(prefix="g4-scan-", dir="/tmp"))  # /tmp/g4-scan-xxx
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

    def test_bfs_prioritizes_shallow_sockets_before_deep_cache_truncation(self):
        with tempfile.TemporaryDirectory() as ws:
            root = Path(ws)
            sock_path = root / "sub" / "agent.sock"
            sock_path.parent.mkdir()
            s = socket.socket(socket.AF_UNIX)
            s.bind(str(sock_path))
            deep_dir = root / "zz_reports" / "run1" / "autonomous" / "bulk_shards" / "shard"
            deep_dir.mkdir(parents=True)
            deep_sock = deep_dir / "hidden.sock"
            s_deep = socket.socket(socket.AF_UNIX)
            s_deep.bind(str(deep_sock))
            for i in range(200):
                (deep_dir.parent / f"img_{i:03d}.jpg").write_text("x")
            err = io.StringIO()
            try:
                with patch.object(sandbox, "READONLY_SCAN_ENTRY_LIMIT", 50), patch("sys.stderr", err):
                    cmd = wrap_bwrap(["true"], workspace=ws, readonly=True)
            finally:
                s.close()
                s_deep.close()
            masked = {cmd[i + 2] for i, x in enumerate(cmd[:-2]) if x == "--ro-bind" and cmd[i + 1] == "/dev/null"}
            tmpfs_masked = {cmd[i + 1] for i, x in enumerate(cmd[:-1]) if x == "--tmpfs"}
            remount_ro = {cmd[i + 1] for i, x in enumerate(cmd[:-1]) if x == "--remount-ro"}
            self.assertIn(str(sock_path), masked)
            self.assertIn(str(deep_dir.parent), tmpfs_masked)
            self.assertIn(str(deep_dir.parent), remount_ro)
            self.assertIn("工作区扫描未能完成", err.getvalue())

    def test_roi_image_cache_is_masked_with_tmpfs_without_truncating(self):
        with tempfile.TemporaryDirectory() as ws:
            root = Path(ws)
            (root / ".git").mkdir()
            cache_dir = root / "reports" / "autonomous" / "roi-image-cache"
            (cache_dir / "shard0").mkdir(parents=True)
            for i in range(100):
                (cache_dir / "shard0" / f"crop_{i}.jpg").write_text("x")
            seen = []
            real_scandir = os.scandir

            def spy(path):
                seen.append(str(path))
                return real_scandir(path)

            with patch("makewand.sandbox.os.scandir", side_effect=spy):
                ro_cmd = wrap_bwrap(["true"], workspace=ws, readonly=True)
                rw_cmd = wrap_bwrap(["true"], workspace=ws, readonly=False)
            self.assertNotIn(str(cache_dir), seen)
            self.assertNotIn(str(cache_dir / "shard0"), seen)
            for cmd in (ro_cmd, rw_cmd):
                tmpfs_masked = {cmd[i + 1] for i, x in enumerate(cmd[:-1]) if x == "--tmpfs"}
                remount_ro = {cmd[i + 1] for i, x in enumerate(cmd[:-1]) if x == "--remount-ro"}
                self.assertIn(str(cache_dir), tmpfs_masked)
                self.assertIn(str(cache_dir), remount_ro)

    def test_late_socket_in_large_directory_still_masked_on_truncation(self):
        with tempfile.TemporaryDirectory() as ws:
            root = Path(ws)
            for i in range(150):
                (root / f"a_{i:03d}.txt").write_text("x")
            late_sock = root / "zzzz_late.sock"
            (root / "zz_sub").mkdir()
            s = socket.socket(socket.AF_UNIX)
            s.bind(str(late_sock))
            err = io.StringIO()
            try:
                with patch.object(sandbox, "READONLY_SCAN_ENTRY_LIMIT", 30), patch("sys.stderr", err):
                    cmd = wrap_bwrap(["true"], workspace=ws, readonly=True)
            finally:
                s.close()
            masked = {cmd[i + 2] for i, x in enumerate(cmd[:-2]) if x == "--ro-bind" and cmd[i + 1] == "/dev/null"}
            tmpfs_masked = {cmd[i + 1] for i, x in enumerate(cmd[:-1]) if x == "--tmpfs"}
            self.assertIn(str(late_sock), masked)
            self.assertIn(str(root / "zz_sub"), tmpfs_masked)

    def test_oserror_during_scandir_fails_closed(self):
        with tempfile.TemporaryDirectory() as ws:
            root = Path(ws)
            sub = root / "sub"
            sub.mkdir()
            real_scandir = os.scandir

            def fail_sub(path):
                if os.fspath(path) == str(sub):
                    raise OSError(5, "Input/output error")
                return real_scandir(path)

            def fail_root(path):
                if os.fspath(path) == str(root):
                    raise OSError(24, "Too many open files")
                return real_scandir(path)

            err = io.StringIO()
            with patch("makewand.sandbox.os.scandir", side_effect=fail_sub), patch("sys.stderr", err):
                with self.assertRaises(sandbox.SandboxConfigError):
                    wrap_bwrap(["true"], workspace=ws, readonly=False)
                ro_cmd = wrap_bwrap(["true"], workspace=ws, readonly=True)
                tmpfs_masked = {ro_cmd[i + 1] for i, x in enumerate(ro_cmd[:-1]) if x == "--tmpfs"}
                self.assertIn(str(sub), tmpfs_masked)

            with patch("makewand.sandbox.os.scandir", side_effect=fail_root):
                with self.assertRaises(sandbox.SandboxConfigError):
                    wrap_bwrap(["true"], workspace=ws, readonly=True)

    def test_scan_timeout_truncates_readonly_and_fails_closed_writable(self):
        with tempfile.TemporaryDirectory() as ws:
            root = Path(ws)
            (root / "sub").mkdir()
            for i in range(20):
                (root / "sub" / f"f{i}").write_text("x")
            err = io.StringIO()
            clock = [100.0]

            def fake_monotonic():
                clock[0] += 2.0
                return clock[0]

            with patch("makewand.sandbox.time.monotonic", side_effect=fake_monotonic), \
                    patch.dict(os.environ, {"MAKEWAND_SANDBOX_SCAN_TIMEOUT_SECONDS": "1.0"}), \
                    patch("sys.stderr", err):
                cmd = wrap_bwrap(["true"], workspace=ws, readonly=True)
                self.assertIn(ws, ro_bound(cmd))
                tmpfs_masked = {cmd[i + 1] for i, x in enumerate(cmd[:-1]) if x == "--tmpfs"}
                self.assertIn(str(root / "sub"), tmpfs_masked)
                self.assertIn("工作区扫描未能完成", err.getvalue())
                with self.assertRaises(sandbox.SandboxConfigError):
                    wrap_bwrap(["true"], workspace=ws, readonly=False)

    def test_deep_workspace_under_broad_mount_root_scans_spine_and_masks_omitted_siblings(self):
        base = Path(tempfile.mkdtemp(prefix="g4-deep-", dir="/tmp"))
        socks = []
        try:
            ws_dir = base / "a" / "b" / "c" / "d" / "ws"
            ws_inner = ws_dir / "inner"
            ws_inner.mkdir(parents=True)
            ws_sock = ws_inner / "agent.sock"
            sibling_deep = base / "other1" / "other2" / "other3"
            sibling_deep.mkdir(parents=True)
            sibling_sock = sibling_deep / "sib.sock"
            for sp in (ws_sock, sibling_sock):
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
                entries = real_scandir(path)
                return FixtureRootEntries(entries) if os.fspath(path) == "/tmp" else entries

            with home_outside_tmp(), patch("makewand.sandbox.os.scandir", side_effect=fixture_scandir):
                cmd = wrap_bwrap(["true"], workspace=str(ws_dir), worktree_root="/tmp", readonly=True)
            masked = {cmd[i + 2] for i, x in enumerate(cmd[:-2]) if x == "--ro-bind" and cmd[i + 1] == "/dev/null"}
            tmpfs_masked = {cmd[i + 1] for i, x in enumerate(cmd[:-1]) if x == "--tmpfs"}
            remount_ro = {cmd[i + 1] for i, x in enumerate(cmd[:-1]) if x == "--remount-ro"}
            self.assertIn(str(ws_sock), masked)
            self.assertIn(str(base / "other1" / "other2"), tmpfs_masked)
            self.assertIn(str(base / "other1" / "other2"), remount_ro)
            self.assertNotIn(str(ws_dir), tmpfs_masked)
            self.assertNotIn(str(ws_inner), tmpfs_masked)
        finally:
            for sk in socks:
                sk.close()
            shutil.rmtree(base, ignore_errors=True)

    def test_readonly_root_workspace_scans_root(self):
        seen_roots = []
        seen_skip_paths = []
        real_scan = sandbox._scan_tree

        def spy_scan(roots, **kwargs):
            seen_roots.append(list(roots))
            seen_skip_paths.append(set(kwargs.get("skip_paths") or ()))
            return real_scan(roots, **kwargs)

        with patch("makewand.sandbox._scan_tree", side_effect=spy_scan):
            cmd = wrap_bwrap(["true"], workspace="/", readonly=True)
        self.assertIn(["/"], seen_roots)
        root_idx = seen_roots.index(["/"])
        self.assertNotIn("/var", seen_skip_paths[root_idx])
        self.assertIn("--ro-bind", cmd)

    def test_isdir_failure_on_root_does_not_silently_skip_scan_root(self):
        with tempfile.TemporaryDirectory() as ws:
            sock_path = Path(ws) / "root.sock"
            s = socket.socket(socket.AF_UNIX)
            s.bind(str(sock_path))
            try:
                with patch("makewand.sandbox.os.path.isdir", return_value=False):
                    _, socks, truncated = sandbox._scan_tree(
                        [ws], max_depth=None, entry_limit=100, collect_git=False
                    )
                self.assertIn(str(sock_path), socks)
                self.assertFalse(truncated)
            finally:
                s.close()

    def test_separate_non_broad_repo_root_not_truncated_by_broad_mount_root(self):
        base = Path(tempfile.mkdtemp(prefix="g4-repo-", dir="/tmp"))
        repo_base = Path(tempfile.mkdtemp(prefix="g4-mainrepo-", dir="/var/tmp"))
        socks = []
        try:
            ws_dir = base / "wt"
            ws_dir.mkdir(parents=True)
            deep_repo_dir = repo_base / "d1" / "d2" / "d3" / "d4"
            deep_repo_dir.mkdir(parents=True)
            repo_sock = deep_repo_dir / "repo.sock"
            sk = socket.socket(socket.AF_UNIX)
            sk.bind(str(repo_sock))
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
                entries = real_scandir(path)
                return FixtureRootEntries(entries) if os.fspath(path) == "/tmp" else entries

            with home_outside_tmp(), patch("makewand.sandbox.os.scandir", side_effect=fixture_scandir):
                cmd = wrap_bwrap(
                    ["true"],
                    workspace=str(ws_dir),
                    worktree_root="/tmp",
                    repo_root=str(repo_base),
                    readonly=True,
                )
            masked = {cmd[i + 2] for i, x in enumerate(cmd[:-2]) if x == "--ro-bind" and cmd[i + 1] == "/dev/null"}
            tmpfs_masked = {cmd[i + 1] for i, x in enumerate(cmd[:-1]) if x == "--tmpfs"}
            self.assertIn(str(repo_sock), masked)
            for anc in (repo_base, repo_base / "d1", repo_base / "d1" / "d2", repo_base / "d1" / "d2" / "d3", deep_repo_dir):
                self.assertNotIn(str(anc), tmpfs_masked)
        finally:
            for sk in socks:
                sk.close()
            shutil.rmtree(base, ignore_errors=True)
            shutil.rmtree(repo_base, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
