"""Task-created __pycache__/*.pyc must never enter a shadow delivery patch."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import json
import unittest
from pathlib import Path

import makewand.orchestrator as orch
import test_delivery_binding as binding
from test_delivery_binding import git


class TransientBytecodeDeliveryTests(binding.DeliveryBindingTests):
    """Reuses the real-Git shadow delivery harness (repository has no .gitignore)."""

    def delivered_patch(self):
        artifacts = self.delivery_artifacts()
        self.assertEqual(len(artifacts), 1)
        return (next(iter(artifacts)) / "makewand_delivery.patch").read_bytes()

    def test_task_created_pycache_is_excluded_without_gitignore(self):
        self.assertFalse((self.base / ".gitignore").exists())

        def coder(cwd):
            (cwd / "bank.py").write_text("BALANCE = 1\n")
            (cwd / "__pycache__").mkdir()
            (cwd / "__pycache__" / "bank.cpython-312.pyc").write_bytes(b"\x00\x01pyc")
            (cwd / "__pycache__" / "test_bank.cpython-312.pyc").write_bytes(b"\x00\x02pyc")
            (cwd / ".pytest_cache" / "v").mkdir(parents=True)
            (cwd / ".pytest_cache" / "v" / "lastfailed").write_text("{}\n")
            (cwd / "loose.pyo").write_bytes(b"\x00")

        self.assertTrue(self.run_pipeline(coder=coder))
        patch = self.delivered_patch()
        self.assertIn(b"bank.py", patch)
        for name in (b"__pycache__", b".pyc", b".pytest_cache", b"loose.pyo"):
            self.assertNotIn(name, patch)
        tree = git(self.base, "ls-tree", "-r", "--name-only", self.branch).splitlines()
        self.assertEqual(sorted(tree), ["app.txt", "bank.py"])

    def test_baseline_tracked_bytecode_is_kept(self):
        (self.base / "__pycache__").mkdir()
        (self.base / "__pycache__" / "vendored.pyc").write_bytes(b"\x00tracked")
        self.refresh_baseline()

        def coder(cwd):
            (cwd / "app.txt").write_text("APPROVED\n")
            (cwd / "__pycache__" / "vendored.pyc").write_bytes(b"\x00changed")
            (cwd / "__pycache__" / "new.pyc").write_bytes(b"\x00new")

        self.assertTrue(self.run_pipeline(coder=coder))
        patch = self.delivered_patch()
        self.assertIn(b"vendored.pyc", patch)
        self.assertNotIn(b"new.pyc", patch)

    def test_purge_helper_skips_tracked_links_and_nested_repositories(self):
        root = self.shadow
        (root / "pkg" / "__pycache__").mkdir(parents=True)
        (root / "pkg" / "__pycache__" / "m.pyc").write_bytes(b"x")
        nested = root / "vendor_repo"
        nested.mkdir()
        git(nested, "init")
        (nested / "x.pyc").write_bytes(b"x")
        outside = Path(self.temp.name) / "outside.pyc"
        outside.write_bytes(b"keep")
        (root / "link_dir").symlink_to(outside.parent, target_is_directory=True)
        removed = orch._purge_transient_bytecode(str(root), self.baseline)
        self.assertEqual(removed, ["pkg/__pycache__/m.pyc"])
        self.assertFalse((root / "pkg" / "__pycache__").exists())
        self.assertTrue((nested / "x.pyc").exists())
        self.assertTrue(outside.exists())


# Only run the new cases here; the inherited suite runs in its own module.
for _name in [n for n in dir(binding.DeliveryBindingTests) if n.startswith("test_")]:
    if not hasattr(TransientBytecodeDeliveryTests, _name) or _name in TransientBytecodeDeliveryTests.__dict__:
        continue
    setattr(TransientBytecodeDeliveryTests, _name, None)


if __name__ == "__main__":
    unittest.main()
