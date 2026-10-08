"""Tests for py-orch-2: purging transient pyc and hard-blocking unreviewed ignored/binary changes."""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import shutil
import tempfile
import unittest
from pathlib import Path

from makewand.git_helper import (
    HostWorkspaceTransaction,
    _is_regenerable_cache,
    run_git_cmd,
    HOST_TRANSACTION_SUPPORTED,
)


def git(path, *args):
    code, out, error = run_git_cmd(["git", *args], cwd=str(path))
    if code != 0:
        raise AssertionError(f"git command failed: {args}, stdout={out}, stderr={error}")
    return out.strip()


class TestIgnoredAndPycDeliverySecurity(unittest.TestCase):
    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory(prefix="makewand_py_orch_2_")
        self.addCleanup(self.temp_dir.cleanup)
        self.root = Path(self.temp_dir.name)
        git(self.root, "init")
        git(self.root, "config", "user.name", "Security Test")
        git(self.root, "config", "user.email", "security@example.invalid")

    def test_is_regenerable_cache(self):
        # 1. Valid pyc in __pycache__ with matching .py source exists
        pkg = self.root / "pkg"
        pkg.mkdir()
        py_file = pkg / "mod.py"
        py_file.write_text("def hello(): pass\n", encoding="utf-8")
        cache_dir = pkg / "__pycache__"
        cache_dir.mkdir()
        valid_pyc = "pkg/__pycache__/mod.cpython-312.pyc"
        (cache_dir / "mod.cpython-312.pyc").write_bytes(b"bytecode")
        self.assertTrue(_is_regenerable_cache(valid_pyc, str(self.root)))

        # 2. Orphan pyc in __pycache__ with NO matching .py source
        orphan_pyc = "pkg/__pycache__/orphan.cpython-312.pyc"
        (cache_dir / "orphan.cpython-312.pyc").write_bytes(b"evil_bytecode")
        self.assertFalse(_is_regenerable_cache(orphan_pyc, str(self.root)))

        # 3. Loose pyc in workspace root or package directory (not inside __pycache__)
        loose_pyc = "pkg/loose.pyc"
        (pkg / "loose.pyc").write_bytes(b"evil_bytecode")
        self.assertFalse(_is_regenerable_cache(loose_pyc, str(self.root)))

        # 4. Binary/shared object inside __pycache__
        so_file = "pkg/__pycache__/mod.so"
        (cache_dir / "mod.so").write_bytes(b"binary")
        self.assertFalse(_is_regenerable_cache(so_file, str(self.root)))

        # 5. Non-python cache like pytest_cache
        self.assertTrue(_is_regenerable_cache(".pytest_cache/v/cache/nodeids", str(self.root)))
        self.assertTrue(_is_regenerable_cache(".mypy_cache/3.12/mod.data.json", str(self.root)))

    @unittest.skipUnless(HOST_TRANSACTION_SUPPORTED, "Requires POSIX host transaction support")
    def test_transient_pyc_purged_on_clean_delivery(self):
        # Set up a tracked python file
        app_file = self.root / "app.py"
        app_file.write_text("print('version 1')\n", encoding="utf-8")
        git(self.root, "add", "app.py")
        git(self.root, "commit", "-m", "initial commit")

        txn = HostWorkspaceTransaction(str(self.root))
        err = txn.capture_pre_snapshot()
        self.assertIsNone(err)
        err = txn.begin()
        self.assertIsNone(err)

        # Task modifies tracked file
        app_file.write_text("print('version 2')\n", encoding="utf-8")
        # Task/test execution generates __pycache__/app.cpython-312.pyc
        cache_dir = self.root / "__pycache__"
        cache_dir.mkdir(exist_ok=True)
        pyc_file = cache_dir / "app.cpython-312.pyc"
        pyc_file.write_bytes(b"\x00\x00\x00\x00bytecode_sample")

        self.assertTrue(pyc_file.exists())
        txn.finalize_success()

        # Delivery succeeded
        self.assertTrue(txn.succeeded)
        self.assertEqual(txn.state, "committed")
        # Tracked modification is retained
        self.assertEqual(app_file.read_text(encoding="utf-8"), "print('version 2')\n")
        # Transient pyc was purged before delivery to prevent poisoned bytecode delivery
        self.assertFalse(pyc_file.exists())

    @unittest.skipUnless(HOST_TRANSACTION_SUPPORTED, "Requires POSIX host transaction support")
    def test_unreviewed_ignored_file_modification_is_blocked_and_rolled_back(self):
        # Create .gitignore and .env
        gitignore = self.root / ".gitignore"
        gitignore.write_text(".env\n*.log\n", encoding="utf-8")
        env_file = self.root / ".env"
        env_file.write_text("SECRET_KEY=original_secure_key\n", encoding="utf-8")
        app_file = self.root / "main.py"
        app_file.write_text("print('hello')\n", encoding="utf-8")

        git(self.root, "add", ".gitignore", "main.py")
        git(self.root, "commit", "-m", "initial commit")

        txn = HostWorkspaceTransaction(str(self.root))
        err = txn.capture_pre_snapshot()
        self.assertIsNone(err)
        err = txn.begin()
        self.assertIsNone(err)

        # Task tampers with .env and main.py
        env_file.write_text("SECRET_KEY=exfiltrated_malicious_key\n", encoding="utf-8")
        app_file.write_text("print('tampered')\n", encoding="utf-8")

        txn.finalize_success()

        # Delivery must be blocked!
        self.assertFalse(txn.succeeded)
        self.assertEqual(txn.state, "rolled_back")
        # .env must be restored to original contents
        self.assertEqual(env_file.read_text(encoding="utf-8"), "SECRET_KEY=original_secure_key\n")
        # Tracked file must be rolled back to baseline
        self.assertEqual(app_file.read_text(encoding="utf-8"), "print('hello')\n")

    @unittest.skipUnless(HOST_TRANSACTION_SUPPORTED, "Requires POSIX host transaction support")
    def test_rogue_binary_or_orphan_pyc_is_blocked_and_rolled_back(self):
        gitignore = self.root / ".gitignore"
        gitignore.write_text("*.so\n", encoding="utf-8")
        app_file = self.root / "main.py"
        app_file.write_text("print('baseline')\n", encoding="utf-8")

        git(self.root, "add", ".gitignore", "main.py")
        git(self.root, "commit", "-m", "initial commit")

        txn = HostWorkspaceTransaction(str(self.root))
        err = txn.capture_pre_snapshot()
        self.assertIsNone(err)
        err = txn.begin()
        self.assertIsNone(err)

        # Task creates a rogue loose .pyc and rogue .so
        rogue_pyc = self.root / "malicious.pyc"
        rogue_pyc.write_bytes(b"\x00\x00\x00\x00malicious_bytecode")
        rogue_so = self.root / "exploit.so"
        rogue_so.write_bytes(b"\x7fELFmalicious_binary")

        txn.finalize_success()

        # Delivery must be blocked!
        self.assertFalse(txn.succeeded)
        self.assertEqual(txn.state, "rolled_back")
        # Rogue binary and pyc must be deleted by rollback!
        self.assertFalse(rogue_pyc.exists())
        self.assertFalse(rogue_so.exists())

    @unittest.skipUnless(HOST_TRANSACTION_SUPPORTED, "Requires POSIX host transaction support")
    def test_new_ignored_file_is_blocked_and_rolled_back(self):
        gitignore = self.root / ".gitignore"
        gitignore.write_text("*.secret\n", encoding="utf-8")
        app_file = self.root / "main.py"
        app_file.write_text("print('baseline')\n", encoding="utf-8")

        git(self.root, "add", ".gitignore", "main.py")
        git(self.root, "commit", "-m", "initial commit")

        txn = HostWorkspaceTransaction(str(self.root))
        err = txn.capture_pre_snapshot()
        self.assertIsNone(err)
        err = txn.begin()
        self.assertIsNone(err)

        # Task creates a new ignored file
        secret_file = self.root / "planted.secret"
        secret_file.write_text("planted data\n", encoding="utf-8")

        txn.finalize_success()

        # Delivery must be blocked!
        self.assertFalse(txn.succeeded)
        self.assertEqual(txn.state, "rolled_back")
        # New ignored file must be deleted by rollback!
        self.assertFalse(secret_file.exists())
