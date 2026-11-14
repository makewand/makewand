"""
makewand keeps its own copy of agy's file token (credential_backups).

agy >= 1.5.5 keeps its login in the desktop keyring, which the sandbox cannot
reach; there agy reads ~/.gemini/antigravity-cli/antigravity-oauth-token. A host
agy run deletes that file whenever it refreshes the keyring login (seen with
strace on agy 1.5.8), so the next sandboxed run asks for a login. makewand now
copies the host file into ~/.config/makewand/credential-backups at each launch
and, while the host file is gone, copies the backup in and merges the run's
same-account refresh into it.

Everything runs against a throw-away HOME; no provider CLI is started (the live
case runs bash inside the real bubblewrap sandbox when it is usable).
"""

try:  # isolation must precede makewand imports
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

try:
    import test_fix_writeback_identity as vectors
    import test_fix_writeback_r4 as r4
except ImportError:
    from tests import test_fix_writeback_identity as vectors
    from tests import test_fix_writeback_r4 as r4

import contextlib
import fcntl
import os
import shlex
import shutil
import stat
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import sandbox
from makewand.sandbox import ProviderHome, wrap_bwrap

REL = "antigravity-cli/antigravity-oauth-token"
LOGIN = b'{"token":{"access_token":"ya29.a","refresh_token":"1//r","expiry":"2026-11-13T10:00:00Z"}}'
REFRESHED = b'{"token":{"access_token":"ya29.b","refresh_token":"1//r","expiry":"2026-11-13T11:00:00Z"}}'
# A different Google refresh token comes only from a new login, never a refresh.
OTHER_LOGIN = b'{"token":{"access_token":"ya29.c","refresh_token":"1//other","expiry":"2026-11-13T12:00:00Z"}}'


def _paths(home):
    return home / ".gemini" / REL, home / sandbox.CREDENTIAL_BACKUP_DIR / "agy" / "antigravity-oauth-token"


def _write(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    os.chmod(path, 0o600)


def _materialize(home):
    (home / ".gemini").mkdir(exist_ok=True)
    h = ProviderHome("agy", str(home))
    with patch("makewand.sandbox._warn") as warn:
        h.materialize([])
    cred = next(c for c in h.credentials if c.rel == REL)
    return h, cred, Path(cred.private_root, REL), [c.args[0] for c in warn.call_args_list]


def _finalize(h):
    with patch("makewand.sandbox._warn") as warn:
        h.finalize()
    return [c.args[0] for c in warn.call_args_list]


class TestBackupSource(unittest.TestCase):
    def test_host_file_is_copied_into_the_backup_and_stays_the_source(self):
        with vectors.fake_home() as home:
            host, backup = _paths(home)
            _write(host, LOGIN)
            h, cred, priv, warnings = _materialize(home)
            self.assertEqual(warnings, [])
            self.assertEqual(cred.host_path, str(host))
            self.assertEqual(priv.read_bytes(), LOGIN)
            self.assertEqual(backup.read_bytes(), LOGIN)
            self.assertEqual(stat.S_IMODE(backup.stat().st_mode), 0o600)
            for d in (backup.parent, backup.parent.parent):
                self.assertEqual(stat.S_IMODE(d.stat().st_mode) & 0o077, 0, d)
            priv.write_bytes(REFRESHED)
            self.assertEqual(_finalize(h), [])
            self.assertEqual(host.read_bytes(), REFRESHED)

    def test_backup_follows_a_new_host_login(self):
        with vectors.fake_home() as home:
            host, backup = _paths(home)
            _write(backup, OTHER_LOGIN)
            _write(host, LOGIN)
            h, cred, priv, warnings = _materialize(home)
            self.assertEqual(warnings, [])
            self.assertEqual(backup.read_bytes(), LOGIN)
            _finalize(h)

    def test_unchanged_backup_is_not_rewritten(self):
        with vectors.fake_home() as home:
            host, backup = _paths(home)
            _write(host, LOGIN)
            _write(backup, LOGIN)
            before = backup.stat()
            h, _cred, _priv, _warnings = _materialize(home)
            after = backup.stat()
            self.assertEqual((before.st_ino, before.st_mtime_ns), (after.st_ino, after.st_mtime_ns))
            _finalize(h)

    def test_deleted_host_file_is_replaced_by_the_backup_which_takes_the_refresh(self):
        with vectors.fake_home() as home:
            host, backup = _paths(home)
            _write(backup, LOGIN)
            h, cred, priv, warnings = _materialize(home)
            self.assertEqual(warnings, [])
            self.assertEqual(cred.host_path, str(backup))
            self.assertEqual(priv.read_bytes(), LOGIN)
            priv.write_bytes(REFRESHED)
            self.assertEqual(_finalize(h), [])
            self.assertEqual(backup.read_bytes(), REFRESHED)
            self.assertEqual(stat.S_IMODE(backup.stat().st_mode), 0o600)
            self.assertFalse(os.path.lexists(host))

    def test_backup_refuses_another_account(self):
        with vectors.fake_home() as home:
            _host, backup = _paths(home)
            _write(backup, LOGIN)
            h, _cred, priv, _warnings = _materialize(home)
            priv.write_bytes(OTHER_LOGIN)
            warnings = _finalize(h)
            self.assertEqual(backup.read_bytes(), LOGIN)
            self.assertEqual(len(warnings), 1, warnings)
            self.assertIn(str(backup), warnings[0])

    def test_no_host_file_and_no_backup_creates_nothing(self):
        with vectors.fake_home() as home:
            host, _backup = _paths(home)
            h, cred, priv, warnings = _materialize(home)
            self.assertEqual(warnings, [])
            self.assertEqual(cred.host_path, str(host))
            self.assertFalse(os.path.lexists(priv))
            self.assertFalse(os.path.lexists(home / sandbox.CREDENTIAL_BACKUP_DIR))
            _finalize(h)

    def test_symlinked_backup_is_not_used(self):
        with vectors.fake_home() as home:
            host, backup = _paths(home)
            target = home / "elsewhere"
            _write(target, LOGIN)
            backup.parent.mkdir(parents=True)
            backup.symlink_to(target)
            h, cred, priv, _warnings = _materialize(home)
            self.assertEqual(cred.host_path, str(host))
            self.assertFalse(os.path.lexists(priv))
            _finalize(h)

    def test_symlinked_backup_is_replaced_not_followed(self):
        with vectors.fake_home() as home:
            host, backup = _paths(home)
            target = home / "elsewhere"
            _write(target, OTHER_LOGIN)
            backup.parent.mkdir(parents=True)
            backup.symlink_to(target)
            _write(host, LOGIN)
            h, _cred, _priv, _warnings = _materialize(home)
            self.assertFalse(backup.is_symlink())
            self.assertEqual(backup.read_bytes(), LOGIN)
            self.assertEqual(target.read_bytes(), OTHER_LOGIN)
            _finalize(h)

    def test_backup_failure_is_reported_and_the_run_uses_the_host_file(self):
        with vectors.fake_home() as home:
            host, _backup = _paths(home)
            _write(host, LOGIN)
            blocker = home / sandbox.CREDENTIAL_BACKUP_DIR
            _write(blocker, b"not a directory")
            h, cred, priv, warnings = _materialize(home)
            self.assertEqual(cred.host_path, str(host))
            self.assertEqual(priv.read_bytes(), LOGIN)
            self.assertEqual(len(warnings), 1, warnings)
            self.assertIn("makewand 保管的凭据副本", warnings[0])
            _finalize(h)

    def test_backup_resolving_outside_home_is_not_used(self):
        """A symlinked ~/.config/makewand pointing where a sandbox's read-only
        root shows the copy (outside HOME and the masked roots): the copy is
        neither written nor read."""
        outside = Path(tempfile.mkdtemp(prefix="agybackup-outside-"))
        self.addCleanup(shutil.rmtree, outside, True)
        with vectors.fake_home() as home:
            host, backup = _paths(home)
            (home / ".config").mkdir()
            (home / ".config" / "makewand").symlink_to(outside)
            outside_copy = outside / "credential-backups" / "agy" / "antigravity-oauth-token"
            _write(outside_copy, OTHER_LOGIN)
            _write(host, LOGIN)
            h, cred, priv, warnings = _materialize(home)
            self.assertEqual(cred.host_path, str(host))
            self.assertEqual(priv.read_bytes(), LOGIN)
            self.assertEqual(outside_copy.read_bytes(), OTHER_LOGIN)
            self.assertEqual(len(warnings), 1, warnings)
            self.assertIn(str(outside), warnings[0])
            _finalize(h)
            host.unlink()
            h, cred, priv, _warnings = _materialize(home)
            self.assertEqual(cred.host_path, str(host))
            self.assertFalse(os.path.lexists(priv))
            _finalize(h)

    def test_launch_copy_rereads_the_host_file_under_the_lock(self):
        """A host login replaced while the launch waits for the backup lock is
        what the backup gets, not the content read before the lock."""
        with vectors.fake_home() as home:
            host, backup = _paths(home)
            _write(host, LOGIN)
            real_lock = sandbox._credential_lock

            @contextlib.contextmanager
            def lock_while_host_changes(path):
                _write(host, OTHER_LOGIN)
                with real_lock(path) as locked:
                    yield locked

            with patch("makewand.sandbox._credential_lock", lock_while_host_changes):
                h, _cred, priv, _warnings = _materialize(home)
            self.assertEqual(priv.read_bytes(), LOGIN)
            self.assertEqual(backup.read_bytes(), OTHER_LOGIN)
            _finalize(h)

    def test_busy_backup_lock_leaves_the_copy_to_its_holder(self):
        with vectors.fake_home() as home:
            host, backup = _paths(home)
            _write(backup, OTHER_LOGIN)
            _write(host, LOGIN)
            fd = os.open(sandbox._credential_lock_path(str(backup)), os.O_RDWR | os.O_CREAT, 0o600)
            self.addCleanup(os.close, fd)
            fcntl.flock(fd, fcntl.LOCK_EX)
            with patch("makewand.sandbox._CRED_LOCK_WAIT", 0.2):
                h, cred, priv, warnings = _materialize(home)
            self.assertEqual(cred.host_path, str(host))
            self.assertEqual(priv.read_bytes(), LOGIN)
            self.assertEqual(backup.read_bytes(), OTHER_LOGIN)
            self.assertEqual(len(warnings), 1, warnings)
            self.assertIn(str(backup), warnings[0])
            _finalize(h)

    def test_symlinked_backup_directory_is_not_used(self):
        """credential-backups or its agy directory linked into HOME (a later
        workspace) would put the copy where the mask does not reach."""
        for depth in (1, 2):
            with self.subTest(depth=depth), vectors.fake_home() as home:
                host, backup = _paths(home)
                link = backup.parent if depth == 1 else backup.parent.parent
                project = home / "project"
                (project / "agy").mkdir(parents=True)
                link.parent.mkdir(parents=True)
                link.symlink_to(project)
                _write(host, LOGIN)
                h, cred, priv, warnings = _materialize(home)
                self.assertEqual(cred.host_path, str(host))
                self.assertEqual(priv.read_bytes(), LOGIN)
                self.assertEqual([p.name for p in project.rglob("antigravity-oauth-token")], [])
                self.assertEqual(len(warnings), 1, warnings)
                self.assertIn("符号链接", warnings[0])
                _finalize(h)

    def test_symlinked_ancestor_inside_home_is_not_used(self):
        """~/.config/makewand linked into ~/.local/bin, which every sandbox
        mounts read-only, would put the copy where the sandboxes see it."""
        with vectors.fake_home() as home:
            host, _backup = _paths(home)
            state = home / ".local" / "bin" / "makewand-state"
            state.mkdir(parents=True)
            (home / ".config").mkdir()
            (home / ".config" / "makewand").symlink_to(state)
            _write(host, LOGIN)
            h, cred, priv, warnings = _materialize(home)
            self.assertEqual(cred.host_path, str(host))
            self.assertEqual(priv.read_bytes(), LOGIN)
            self.assertEqual(list(state.rglob("antigravity-oauth-token")), [])
            self.assertEqual(len(warnings), 1, warnings)
            self.assertIn("符号链接", warnings[0])
            _finalize(h)

    def test_backup_stands_in_when_the_whole_provider_root_is_missing(self):
        with vectors.fake_home() as home:
            host, backup = _paths(home)
            _write(backup, LOGIN)
            h = ProviderHome("agy", str(home))
            with patch("makewand.sandbox._warn") as warn:
                h.materialize([])
            self.assertEqual(warn.call_args_list, [])
            cred = next(c for c in h.credentials if c.rel == REL)
            priv = Path(cred.private_root, REL)
            self.assertTrue(cred.backup)
            self.assertEqual(cred.host_path, str(backup))
            self.assertEqual(priv.read_bytes(), LOGIN)
            priv.write_bytes(REFRESHED)
            self.assertEqual(_finalize(h), [])
            self.assertEqual(backup.read_bytes(), REFRESHED)
            self.assertFalse(os.path.lexists(home / ".gemini"))

    def test_concurrently_made_backup_directory_is_accepted(self):
        with vectors.fake_home() as home:
            target = home / sandbox.CREDENTIAL_BACKUP_DIR / "agy"
            target.mkdir(parents=True)
            real_isdir = os.path.isdir
            seen = []

            def isdir_racing(path):
                # The first check misses the directory another launch is making.
                if str(path) == str(target) and not seen:
                    seen.append(path)
                    return False
                return real_isdir(path)

            with patch("makewand.sandbox.os.path.isdir", isdir_racing):
                sandbox._make_private_dirs(str(target))
            self.assertTrue(seen)
            self.assertTrue(target.is_dir())

    def test_backup_replaced_by_a_symlink_during_the_run_is_not_written_through(self):
        with vectors.fake_home() as home:
            _host, backup = _paths(home)
            _write(backup, LOGIN)
            h, cred, priv, _warnings = _materialize(home)
            self.assertTrue(cred.backup)
            target = home / "elsewhere"
            _write(target, LOGIN)  # same content as the launch snapshot
            backup.unlink()
            backup.symlink_to(target)
            priv.write_bytes(REFRESHED)
            warnings = _finalize(h)
            self.assertEqual(target.read_bytes(), LOGIN)
            self.assertTrue(backup.is_symlink())
            self.assertEqual(len(warnings), 1, warnings)
            self.assertIn("符号链接", warnings[0])

    def test_only_agy_keeps_backups(self):
        kept = {name: list(p.get("credential_backups", {})) for name, p in sandbox.PROVIDER_PROFILES.items()
                if p.get("credential_backups")}
        self.assertEqual(kept, {"agy": [(".gemini", REL)]})


class TestBackupMask(unittest.TestCase):
    def _cmd(self, ws):
        with patch("makewand.sandbox.resolve_trusted_bwrap", return_value="/usr/bin/bwrap"):
            return wrap_bwrap(["bash", "-c", "true"], workspace=str(ws), readonly=True)

    def test_workspace_containing_the_backups_masks_them(self):
        with vectors.fake_home() as home:
            _host, backup = _paths(home)
            _write(backup, LOGIN)
            backups = str(home / sandbox.CREDENTIAL_BACKUP_DIR)
            for ws in (home / ".config", home / ".config" / "makewand"):
                with self.subTest(ws=str(ws)):
                    cmd = self._cmd(ws)
                    self.assertIn(backups, _tmpfs(cmd), cmd)
                    # after the workspace mount, so the mask wins
                    mask = max(i for i, a in enumerate(cmd[:-1]) if a == "--tmpfs" and cmd[i + 1] == backups)
                    bind = max(i for i, a in enumerate(cmd[:-2])
                               if a in ("--bind", "--ro-bind") and cmd[i + 2] == str(ws))
                    self.assertGreater(mask, bind, cmd)

    def test_backups_not_made_yet_are_created_and_masked(self):
        """A copy this launch or another one makes after the mounts are planned
        must land under the mask."""
        with vectors.fake_home() as home:
            (home / ".config").mkdir()
            backups = home / sandbox.CREDENTIAL_BACKUP_DIR
            cmd = self._cmd(home / ".config")
            self.assertIn(str(backups), _tmpfs(cmd), cmd)
            self.assertTrue(backups.is_dir())
            for d in (backups, backups.parent):
                self.assertEqual(stat.S_IMODE(d.stat().st_mode) & 0o077, 0, d)

    def test_symlinked_workspace_masks_the_backups_where_it_shows_them(self):
        with vectors.fake_home() as home:
            _host, backup = _paths(home)
            _write(backup, LOGIN)
            link = home / "cfg-link"
            link.symlink_to(home / ".config")
            cmd = self._cmd(link)
            self.assertIn(str(link / "makewand" / "credential-backups"), _tmpfs(cmd), cmd)

    def test_workspace_inside_the_backups_is_refused(self):
        with vectors.fake_home() as home:
            _host, backup = _paths(home)
            _write(backup, LOGIN)
            link = home / "agy-link"
            link.symlink_to(backup.parent)
            for ws in (backup.parent, backup.parent.parent, link):
                with self.subTest(ws=str(ws)), self.assertRaises(sandbox.SandboxConfigError):
                    self._cmd(ws)

    def test_workspace_elsewhere_adds_no_mask(self):
        with vectors.fake_home() as home:
            _host, backup = _paths(home)
            _write(backup, LOGIN)
            ws = home / "project"
            ws.mkdir()
            self.assertNotIn(str(home / sandbox.CREDENTIAL_BACKUP_DIR), self._cmd(ws))


def _tmpfs(args):
    return [args[i + 1] for i, a in enumerate(args[:-1]) if a == "--tmpfs"]


@unittest.skipUnless(r4.BWRAP_OK, "bubblewrap not usable on this host")
class TestLiveBackup(unittest.TestCase):
    TOKEN = ".gemini/" + REL

    def test_sandbox_reads_the_backup_cannot_see_it_and_its_refresh_lands_there(self):
        with vectors.fake_home() as home:
            host, backup = _paths(home)
            (home / ".gemini").mkdir()
            _write(backup, LOGIN)
            payload = ("cat ~/" + self.TOKEN + "; echo; "
                       "ls -a ~/.config/makewand 2>/dev/null | grep -c credential-backups; "
                       "printf '%s' " + shlex.quote(REFRESHED.decode()) + " > ~/" + self.TOKEN)
            ws = Path(tempfile.mkdtemp(prefix="agybackup-ws-"))
            self.addCleanup(shutil.rmtree, ws, True)
            res, warnings = r4._live("agy", payload, ws, True)
            self.assertEqual(res.returncode, 0, res.stderr)
            lines = res.stdout.splitlines()
            self.assertEqual(lines[0], LOGIN.decode(), res.stdout)
            self.assertEqual(lines[1], "0", res.stdout)
            self.assertEqual(warnings, [])
            self.assertEqual(backup.read_bytes(), REFRESHED)
            self.assertFalse(os.path.lexists(host))


if __name__ == "__main__":
    unittest.main()
