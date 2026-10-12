"""[M-delivery-fingerprint] Opaque destination dirs cannot hide changes from validation."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.delivery as delivery
from makewand.git_helper import run_git_cmd


def git(root, *args):
    code, out, error = run_git_cmd(["git", *args], cwd=str(root))
    if code:
        raise AssertionError((args, out, error))
    return out


@unittest.skipUnless(os.name == "posix", "POSIX delivery checks")
class OpaqueDirectoryFingerprintTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="makewand-delivery-fp-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "workspace"
        self.root.mkdir()
        git(self.root, "init")
        (self.root / "app.py").write_text("print(1)\n")

    def test_non_ignored_dependency_tree_changes_are_detected(self):
        for name in ("node_modules", ".venv"):
            with self.subTest(name=name):
                package = self.root / name / "pkg"
                package.mkdir(parents=True)
                (package / "index.js").write_text("module.exports = 1;\n")
                frozen = delivery.capture_delivery_baseline(self.root)
                self.assertEqual(frozen["entries"][name][2], "tree")
                self.assertNotIn(name + "/pkg/index.js", frozen["entries"])
                self.assertTrue(delivery.check_delivery_state(frozen))
                (package / "index.js").write_text("require('child_process').exec('evil');\n")
                with self.assertRaisesRegex(OSError, name):
                    delivery.check_delivery_state(frozen)

    def test_added_file_link_and_mode_change_are_detected(self):
        opaque = self.root / ".makewand_cache"
        opaque.mkdir()
        (opaque / "a.txt").write_text("a\n")
        original_mode = (opaque / "a.txt").stat().st_mode & 0o7777
        frozen = delivery.capture_delivery_baseline(self.root)
        mutations = [
            lambda: (opaque / "b.txt").write_text("b\n"),
            lambda: os.symlink("/etc/passwd", opaque / "link"),
            lambda: os.chmod(opaque / "a.txt", original_mode ^ 0o100),
            lambda: (opaque / "sub").mkdir(),
        ]
        for mutate in mutations:
            with self.subTest(mutate=mutate):
                snapshot = {p: p.read_bytes() for p in opaque.iterdir() if p.is_file() and not p.is_symlink()}
                mutate()
                with self.assertRaises(OSError):
                    delivery.check_delivery_state(frozen)
                for child in list(opaque.iterdir()):
                    if child.is_symlink() or child.is_file():
                        if child not in snapshot:
                            child.unlink()
                    elif child.is_dir():
                        child.rmdir()
                os.chmod(opaque / "a.txt", original_mode)
                self.assertTrue(delivery.check_delivery_state(frozen))

    def test_ignored_dependency_dir_stays_pruned(self):
        (self.root / ".gitignore").write_text("node_modules/\n")
        (self.root / "node_modules" / "pkg").mkdir(parents=True)
        (self.root / "node_modules" / "pkg" / "index.js").write_text("1\n")
        frozen = delivery.capture_delivery_baseline(self.root)
        self.assertEqual(frozen["entries"]["node_modules"], ["dir", frozen["entries"]["node_modules"][1]])

    def test_digest_budget_overflow_fails_closed(self):
        opaque = self.root / ".makewand_big"
        opaque.mkdir()
        for index in range(5):
            (opaque / f"f{index}").write_text("x\n")
        with patch.object(delivery, "TREE_DIGEST_MAX_ENTRIES", 3):
            with self.assertRaisesRegex(OSError, "cannot be verified"):
                delivery.capture_delivery_baseline(self.root)
        with patch.object(delivery, "TREE_DIGEST_MAX_BYTES", 4):
            with self.assertRaisesRegex(OSError, "cannot be verified"):
                delivery.capture_delivery_baseline(self.root)

    def test_declared_internal_dir_is_excluded_only_without_tracked_files(self):
        internal = self.root / ".makewand_sandbox_home"
        internal.mkdir()
        (internal / "scratch").write_text("1\n")
        frozen = delivery.capture_delivery_baseline(self.root)
        self.assertEqual(frozen["entries"][".makewand_sandbox_home"][2], "internal")
        (internal / "scratch").write_text("2\n")
        self.assertTrue(delivery.check_delivery_state(frozen))
        git(self.root, "add", "-f", ".makewand_sandbox_home/scratch")
        with self.assertRaisesRegex(OSError, "tracks files inside makewand-internal"):
            delivery.capture_delivery_baseline(self.root)

    def test_patch_below_opaque_directory_is_refused(self):
        opaque = self.root / ".makewand_cache"
        opaque.mkdir()
        (opaque / "a.txt").write_text("a\n")
        frozen = delivery.capture_delivery_baseline(self.root)
        (opaque / "a.txt").unlink()
        with self.assertRaisesRegex(OSError, "opaque directory"):
            delivery.check_delivery_state(frozen, {".makewand_cache/a.txt": None})


if __name__ == "__main__":
    unittest.main()
