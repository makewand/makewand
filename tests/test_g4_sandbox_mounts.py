"""
G4 regression tests: host filesystem exposure of the bwrap sandbox.

Findings covered:
  py-security#4 — `--ro-bind / /` exposed every other project, other users'
  homes and data disks (/home, /mnt, /srv, ...) read-only; SENSITIVE_HOME_DIRS
  was dead code. Workspaces below a masked prefix must keep working.
  replay-F01-F06-adaptive#6 (part 2) — host sockets below /var stayed reachable.
  Also: a writable workspace of / or HOME exposed the whole host read-write, and
  a repo_root that is an ancestor of the workspace turned the workspace read-only.
"""

import contextlib
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from makewand import sandbox
from makewand.sandbox import wrap_bwrap, is_bwrap_available, run_in_sandbox

BWRAP_OK = is_bwrap_available()
REPO_ROOT = Path(__file__).resolve().parent.parent


@contextlib.contextmanager
def fake_home():
    home = Path(tempfile.mkdtemp(prefix="g4-fake-home-"))
    try:
        with patch.dict(os.environ, {"HOME": str(home)}):
            yield home
    finally:
        shutil.rmtree(home, ignore_errors=True)


def _masked_prefix_of(path: Path):
    for root in sandbox.MASKED_HOST_ROOTS:
        if os.path.isdir(root) and str(path).startswith(root.rstrip("/") + "/"):
            return root
    return None


def sh(cmd, cwd):
    return subprocess.run(cmd, cwd=str(cwd), capture_output=True, text=True, timeout=60)


class TestMaskedHostRoots(unittest.TestCase):
    def test_masked_roots_are_hidden_before_workspace_is_rebound(self):
        base = Path(tempfile.mkdtemp(prefix="g4-ws-"))
        try:
            cmd = wrap_bwrap(["true"], workspace=str(base))
            tmpfs_targets = [cmd[i + 1] for i, x in enumerate(cmd) if x == "--tmpfs"]
            for root in sandbox.MASKED_HOST_ROOTS:
                if os.path.isdir(root) and not os.path.islink(root):
                    self.assertIn(root, tmpfs_targets)
            ws_bind = [i for i, x in enumerate(cmd) if x == "--bind" and cmd[i + 1] == str(base)]
            last_mask = max(i for i, x in enumerate(cmd) if x == "--tmpfs" and cmd[i + 1] in sandbox.MASKED_HOST_ROOTS)
            self.assertTrue(ws_bind and ws_bind[0] > last_mask)
        finally:
            shutil.rmtree(base, ignore_errors=True)

    def test_sensitive_home_dirs_is_wired(self):
        # SENSITIVE_HOME_DIRS used to be dead code; it now drives the HOME re-mask.
        with fake_home() as home:
            (home / ".ssh").mkdir()
            cmd = wrap_bwrap(["true"], workspace=str(home), readonly=True)
            idx = [i for i, x in enumerate(cmd) if x == "--tmpfs" and cmd[i + 1] == str(home / ".ssh")]
            self.assertTrue(idx, "sensitive dirs must be re-masked when HOME is mounted")


@unittest.skipUnless(BWRAP_OK, "bubblewrap not usable on this host")
class TestMaskedHostRootsLive(unittest.TestCase):
    def test_workspace_below_masked_prefix_works_and_siblings_are_hidden(self):
        prefix = _masked_prefix_of(REPO_ROOT)
        if not prefix:
            self.skipTest(f"repository checkout {REPO_ROOT} is not below a masked prefix")
        base = Path(tempfile.mkdtemp(prefix=".g4-masked-", dir=str(REPO_ROOT)))
        try:
            (base / "sibling-secret.txt").write_text("secret\n")
            ws = base / "ws"
            ws.mkdir()
            (ws / "input.txt").write_text("input\n")
            subprocess.run(["git", "init", "-q"], cwd=str(ws), check=True)
            payload = (
                "cat input.txt\n"
                "echo out > output.txt && echo WROTE\n"
                "cat ../sibling-secret.txt 2>/dev/null && echo SIBLING_VISIBLE || echo SIBLING_HIDDEN\n"
                f"test -e {REPO_ROOT}/makewand/sandbox.py && echo REPO_VISIBLE || echo REPO_HIDDEN\n"
                "git rev-parse --show-toplevel >/dev/null 2>&1 && echo GIT_OK || echo GIT_FAIL\n"
                "(echo x > ../sibling-new.txt) 2>/dev/null && echo MASK_WRITABLE || echo MASK_READONLY\n"
                "(echo x > \"$HOME/scratch\") 2>/dev/null && echo HOME_WRITABLE || echo HOME_READONLY\n"
            )
            ret, out, err, ex = run_in_sandbox(["bash", "-c", payload], workspace=str(ws))
            self.assertEqual(ret, 0, err + str(ex))
            self.assertIn("input", out)
            self.assertIn("WROTE", out)
            self.assertIn("SIBLING_HIDDEN", out)
            self.assertIn("REPO_HIDDEN", out)
            self.assertIn("GIT_OK", out)
            # masks behave like the old read-only root: writes fail instead of landing in RAM
            self.assertIn("MASK_READONLY", out)
            self.assertFalse((base / "sibling-new.txt").exists())
            # the private HOME tmpfs stays writable for CLI caches
            self.assertIn("HOME_WRITABLE", out)
            self.assertEqual((ws / "output.txt").read_text(), "out\n")
        finally:
            shutil.rmtree(base, ignore_errors=True)

    def test_other_data_prefixes_not_readable(self):
        roots = [r for r in ("/mnt", "/srv", "/media", "/root") if os.path.isdir(r)]
        if not roots:
            self.skipTest("no masked prefixes on this host")
        # Only path components of deliberately re-bound toolchain paths may show up.
        rebound = [p for p in os.environ.get("PATH", "").split(os.pathsep) if p]
        rebound += [sys.prefix, sys.base_prefix, os.path.dirname(os.path.realpath(sys.executable))]
        with tempfile.TemporaryDirectory() as ws:
            payload = "".join(f"for e in $(ls -A {r} 2>/dev/null); do echo 'ENTRY {r}/'$e; done\n" for r in roots)
            ret, out, err, ex = run_in_sandbox(["bash", "-c", payload], workspace=ws)
            self.assertEqual(ret, 0, err)
            for line in out.splitlines():
                if not line.startswith("ENTRY "):
                    continue
                entry = line[len("ENTRY "):]
                self.assertTrue(any(os.path.normpath(p).startswith(entry) for p in rebound),
                                f"{entry} is visible inside the sandbox")

    def test_readonly_home_workspace_remasks_sensitive_dirs(self):
        with fake_home() as home:
            (home / ".ssh").mkdir()
            (home / ".ssh" / "id_ed25519").write_text("PRIVATE\n")
            (home / ".aws").mkdir()
            (home / ".aws" / "credentials").write_text("AKIA\n")
            (home / ".config" / "makewand").mkdir(parents=True)
            (home / ".config" / "makewand" / "api_keys.json").write_text('{"k": "v"}\n')
            (home / "project").mkdir()
            (home / "project" / "notes.txt").write_text("visible\n")
            payload = (
                "cat ~/project/notes.txt\n"
                "cat ~/.ssh/id_ed25519 2>/dev/null && echo SSH_LEAK || echo SSH_HIDDEN\n"
                "cat ~/.aws/credentials 2>/dev/null && echo AWS_LEAK || echo AWS_HIDDEN\n"
                "cat ~/.config/makewand/api_keys.json 2>/dev/null && echo MW_LEAK || echo MW_HIDDEN\n"
            )
            cmd = wrap_bwrap(["bash", "-c", payload], workspace=str(home), readonly=True)
            res = sh(cmd, home)
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertIn("visible", res.stdout)
            self.assertNotIn("LEAK", res.stdout, res.stdout)

    def test_repo_root_ancestor_keeps_workspace_writable(self):
        with tempfile.TemporaryDirectory() as parent:
            ws = Path(parent) / "sub"
            ws.mkdir()
            (Path(parent) / "outside.txt").write_text("keep\n")
            cmd = wrap_bwrap(["bash", "-c", "echo ok > inside.txt && (echo x > ../outside.txt 2>/dev/null && echo OUTSIDE_W || echo OUTSIDE_RO)"],
                             workspace=str(ws), repo_root=parent)
            res = sh(cmd, ws)
            self.assertEqual(res.returncode, 0, res.stderr)
            self.assertEqual((ws / "inside.txt").read_text(), "ok\n")
            self.assertIn("OUTSIDE_RO", res.stdout)
            self.assertEqual((Path(parent) / "outside.txt").read_text(), "keep\n")


class TestDangerousWorkspaces(unittest.TestCase):
    def test_writable_home_workspace_refused(self):
        with fake_home() as home:
            with self.assertRaises(sandbox.SandboxConfigError):
                wrap_bwrap(["true"], workspace=str(home))

    def test_writable_root_and_home_ancestor_refused(self):
        with fake_home() as home:
            for ws in ("/", str(home.parent)):
                with self.subTest(ws=ws), self.assertRaises(sandbox.SandboxConfigError):
                    wrap_bwrap(["true"], workspace=ws)

    def test_run_in_sandbox_home_workspace_fails_closed_without_executing(self):
        with fake_home() as home:
            marker = home / "ran.txt"
            with patch("makewand.sandbox.is_bwrap_available", return_value=True):
                ret, out, err, category = run_in_sandbox(["bash", "-c", f"touch {marker}"], workspace=str(home))
            self.assertNotEqual(ret, 0)
            self.assertEqual(category, "SandboxConfigError")
            self.assertFalse(marker.exists())

    def test_readonly_root_workspace_does_not_remount_root(self):
        cmd = wrap_bwrap(["true"], workspace="/", readonly=True)
        ro_root = [i for i, x in enumerate(cmd) if x == "--ro-bind" and cmd[i + 1] == "/" and cmd[i + 2] == "/"]
        self.assertEqual(len(ro_root), 1, "mounting / again would undo every mask")


class TestHostVarSockets(unittest.TestCase):
    def setUp(self):
        sandbox._host_var_cache["masks"] = None
        sandbox._host_var_cache["at"] = 0.0

    tearDown = setUp

    def test_var_socket_ancestors_are_masked(self):
        with tempfile.TemporaryDirectory() as fake_var:
            deep = Path(fake_var) / "lib" / "libvirt" / "qemu" / "domain-1"
            deep.mkdir(parents=True)
            shallow_dir = Path(fake_var) / "spool"
            shallow_dir.mkdir()
            socks = []
            for p in (deep / "monitor.sock", shallow_dir / "log.sock"):
                s = socket.socket(socket.AF_UNIX)
                s.bind(str(p))
                socks.append(s)
            try:
                with patch.object(sandbox, "HOST_VAR_ROOT", fake_var):
                    masks = sandbox._host_var_socket_masks()
            finally:
                for s in socks:
                    s.close()
            self.assertIn(str(Path(fake_var) / "lib" / "libvirt"), masks)
            self.assertIn(str(shallow_dir / "log.sock"), masks)

    @unittest.skipUnless(BWRAP_OK, "bubblewrap not usable on this host")
    def test_real_host_var_sockets_not_reachable(self):
        found = []
        for root, dirs, files in os.walk("/var"):
            if root.count(os.sep) > 7 or root.startswith(("/var/tmp", "/var/run", "/var/lock")):
                dirs[:] = []
                continue
            for f in files:
                p = os.path.join(root, f)
                try:
                    import stat as _st
                    if _st.S_ISSOCK(os.lstat(p).st_mode):
                        found.append(p)
                except OSError:
                    pass
            if len(found) >= 3:
                break
        if not found:
            self.skipTest("no pathname sockets below /var on this host")
        with tempfile.TemporaryDirectory() as ws:
            payload = "".join(f"test -S {p} && echo 'REACHABLE {p}' || echo 'MASKED {p}'\n" for p in found)
            ret, out, err, _ = run_in_sandbox(["bash", "-c", payload], workspace=ws)
            self.assertEqual(ret, 0, err)
            self.assertNotIn("REACHABLE", out, out)


if __name__ == "__main__":
    unittest.main()
