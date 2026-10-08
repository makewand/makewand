"""Frozen delivery budgets and scoped postimage verification on real files."""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import hashlib
import os
import stat
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand.delivery import (capture_delivery_baseline, check_delivery_checkpoint,
                               check_delivery_state, snapshot_limits,
                               write_delivery_checkpoint)


class DeliveryLimitsTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="makewand-delivery-limits-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "workspace"
        self.root.mkdir()
        (self.root / "app.txt").write_text("before\n")

    def test_limits_are_explicit_bounded_and_report_the_rejected_setting(self):
        for name, value in [("MAX_ENTRIES", "0"), ("MAX_BYTES", "-1"),
                            ("MAX_SECONDS", "nan"), ("MAX_SECONDS", "301"),
                            ("MAX_ENTRIES", "1000001"), ("MAX_BYTES", str(16 * 1024 ** 3 + 1))]:
            with self.subTest(name=name, value=value):
                with patch.dict(os.environ, {"MAKEWAND_DELIVERY_" + name: value}):
                    with self.assertRaisesRegex(ValueError, name.lower()):
                        snapshot_limits()

    def test_frozen_limits_do_not_read_later_environment_overrides(self):
        limits = {"max_entries": 10, "max_bytes": 32, "max_seconds": 20}
        frozen = capture_delivery_baseline(self.root, limits=limits)
        self.assertEqual(frozen["snapshot_limits"], limits)
        (self.root / "ignored-cache.bin").write_bytes(b"x" * 64)
        with patch.dict(os.environ, {"MAKEWAND_DELIVERY_MAX_BYTES": str(1024 ** 3)}):
            with self.assertRaisesRegex(OSError, r"max_bytes limit .*entries=.*bytes=.*elapsed="):
                check_delivery_state(frozen)

    def test_entry_limit_reports_progress_before_application(self):
        (self.root / "another.txt").write_text("another\n")
        with self.assertRaisesRegex(OSError, r"max_entries limit \(1; entries=2, bytes=.*elapsed="):
            capture_delivery_baseline(self.root, limits={"max_entries": 1, "max_bytes": 1024, "max_seconds": 10})

    def test_byte_limit_rejects_large_ignored_file_without_reading_its_contents(self):
        (self.root / ".gitignore").write_text("cache.bin\n")
        with (self.root / "cache.bin").open("wb") as stream:
            stream.truncate(513 * 1024 ** 2)
        with self.assertRaisesRegex(OSError, r"max_bytes limit .*next_file_bytes=537919488"):
            capture_delivery_baseline(self.root)

    def test_effective_deadline_can_shorten_a_larger_configured_time_budget(self):
        with self.assertRaisesRegex(OSError, r"max_seconds limit .*effective_seconds=0"):
            capture_delivery_baseline(self.root, timeout=0,
                                      limits={"max_entries": 100, "max_bytes": 1024, "max_seconds": 60})

    def test_explicit_larger_budget_hashes_and_still_protects_large_ignored_files(self):
        (self.root / ".gitignore").write_text("cache.bin\n")
        cache = self.root / "cache.bin"
        with cache.open("wb") as stream:
            stream.truncate(513 * 1024 ** 2)
        frozen = capture_delivery_baseline(self.root,
                                          limits={"max_entries": 100, "max_bytes": 1024 ** 3, "max_seconds": 10})
        with cache.open("r+b") as stream:
            stream.seek(-1, os.SEEK_END)
            stream.write(b"x")
        with self.assertRaisesRegex(OSError, "content, permissions or entry set changed: 'cache.bin'"):
            check_delivery_state(frozen)

    def test_legacy_snapshot_keeps_original_defaults_in_a_changed_environment(self):
        frozen = capture_delivery_baseline(self.root)
        frozen.pop("snapshot_limits")
        with patch.dict(os.environ, {"MAKEWAND_DELIVERY_MAX_BYTES": "invalid"}):
            self.assertTrue(check_delivery_state(frozen))

    def test_heavy_dependency_dir_pruned_and_does_not_hit_entry_limit(self):
        nm = self.root / "node_modules"
        for i in range(15):
            pkg = nm / f"pkg_{i}"
            pkg.mkdir(parents=True)
            (pkg / "index.js").write_text("console.log(1);\n")
        # max_entries is 5, but node_modules contains 30+ items.
        # Pruning should ensure only "app.txt" and "node_modules" are counted.
        frozen = capture_delivery_baseline(self.root, limits={"max_entries": 5, "max_bytes": 1024 * 1024, "max_seconds": 10})
        self.assertIn("node_modules", frozen["entries"])
        self.assertEqual(frozen["entries"]["node_modules"][0], "dir")
        self.assertNotIn("node_modules/pkg_0", frozen["entries"])
        self.assertTrue(check_delivery_state(frozen))

    def test_unreadable_ignored_directory_does_not_crash_baseline(self):
        unreadable = self.root / "unreadable_dir"
        unreadable.mkdir()
        try:
            unreadable.chmod(0o000)
            frozen = capture_delivery_baseline(self.root)
            self.assertIn("unreadable_dir", frozen["entries"])
            self.assertEqual(frozen["entries"]["unreadable_dir"][0], "dir")
            self.assertEqual(frozen["entries"]["unreadable_dir"][1], 0)
            self.assertTrue(check_delivery_state(frozen))
        finally:
            unreadable.chmod(0o755)

    def test_unreadable_git_ignored_directory_does_not_crash_baseline(self):
        from makewand.git_helper import run_git_cmd
        run_git_cmd(["git", "init"], cwd=str(self.root))
        run_git_cmd(["git", "config", "user.name", "Test"], cwd=str(self.root))
        run_git_cmd(["git", "config", "user.email", "test@example.invalid"], cwd=str(self.root))
        (self.root / ".gitignore").write_text("data/\n")
        run_git_cmd(["git", "add", "app.txt", ".gitignore"], cwd=str(self.root))
        run_git_cmd(["git", "commit", "-m", "init"], cwd=str(self.root))
        postgres = self.root / "data/postgres"
        postgres.mkdir(parents=True)
        try:
            postgres.chmod(0o000)
            frozen = capture_delivery_baseline(self.root)
            self.assertIn("data/postgres", frozen["entries"])
            self.assertEqual(frozen["entries"]["data/postgres"][0], "dir")
            self.assertTrue(check_delivery_state(frozen))
        finally:
            postgres.chmod(0o755)


class DeliveryScopedCheckpointTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="makewand-delivery-scoped-")
        self.addCleanup(self.temp.cleanup)
        self.parent = Path(self.temp.name)
        self.root = self.parent / "workspace"
        (self.root / "payload").mkdir(parents=True)
        self.target = self.root / "payload/app.txt"
        self.target.write_text("before\n")
        self.target.chmod(0o644)
        self.frozen = capture_delivery_baseline(self.root)
        self.target.write_text("approved\n")
        self.changes = {"payload/app.txt": ["file", hashlib.sha256(b"approved\n").hexdigest(), 0o644]}
        self.checkpoint = self.parent / "checkpoint.json"
        write_delivery_checkpoint(self.frozen, self.changes, self.checkpoint)

    def test_unrelated_oversized_cache_does_not_prevent_safe_rollback_check(self):
        with (self.root / "unrelated-cache.bin").open("wb") as stream:
            stream.truncate(513 * 1024 ** 2)
        self.assertTrue(check_delivery_checkpoint(self.frozen, self.changes, self.checkpoint))
        with self.assertRaisesRegex(OSError, "max_bytes limit"):
            check_delivery_state(self.frozen, self.changes)

    def test_exact_external_mode_change_still_prevents_reversal(self):
        self.target.chmod(0o600)
        with self.assertRaisesRegex(OSError, "external content or mode edit"):
            check_delivery_checkpoint(self.frozen, self.changes, self.checkpoint)
        self.assertEqual(stat.S_IMODE(self.target.stat().st_mode), 0o600)

    def test_external_content_change_still_prevents_reversal(self):
        self.target.write_text("human save\n")
        with self.assertRaisesRegex(OSError, "external content or mode edit"):
            check_delivery_checkpoint(self.frozen, self.changes, self.checkpoint)
        self.assertEqual(self.target.read_text(), "human save\n")

    def test_ancestor_permissions_changed_after_checkpoint_prevent_reversal(self):
        (self.root / "payload").chmod(0o700)
        with self.assertRaisesRegex(OSError, "ancestor identity or mode changed"):
            check_delivery_checkpoint(self.frozen, self.changes, self.checkpoint)

    def test_replaced_ancestor_directory_prevents_reversal_even_with_identical_file(self):
        (self.root / "payload").rename(self.parent / "old-payload")
        (self.root / "payload").mkdir()
        self.target.write_text("approved\n")
        self.target.chmod(0o644)
        with self.assertRaisesRegex(OSError, "ancestor identity or mode changed"):
            check_delivery_checkpoint(self.frozen, self.changes, self.checkpoint)

    def test_symlinked_ancestor_is_never_followed(self):
        (self.root / "payload").rename(self.parent / "outside")
        (self.root / "payload").symlink_to(self.parent / "outside", target_is_directory=True)
        with self.assertRaisesRegex(OSError, "ancestor must be a real directory"):
            check_delivery_checkpoint(self.frozen, self.changes, self.checkpoint)
        self.assertEqual((self.parent / "outside/app.txt").read_text(), "approved\n")

    def test_missing_checkpoint_never_authorizes_reversal(self):
        self.checkpoint.unlink()
        with self.assertRaisesRegex(OSError, "postimage was not captured"):
            check_delivery_checkpoint(self.frozen, self.changes, self.checkpoint)

    def test_new_repository_marker_in_patch_ancestry_prevents_reversal(self):
        from makewand.git_helper import run_git_cmd
        code, _, error = run_git_cmd(["git", "init"], cwd=str(self.root / "payload"))
        self.assertEqual(code, 0, error)
        with self.assertRaisesRegex(OSError, "identity or HEAD changed"):
            check_delivery_checkpoint(self.frozen, self.changes, self.checkpoint)
