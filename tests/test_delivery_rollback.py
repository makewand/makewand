"""Generated delivery scripts preserve edits made after a patch was applied."""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import os
import shutil
import stat
import sys
import unittest

try:
    import test_delivery_binding as binding
except ImportError:
    from tests import test_delivery_binding as binding


@unittest.skipUnless(os.name == "posix" and shutil.which("bash"), "POSIX delivery scripts require bash")
class DeliveryRollbackTests(unittest.TestCase):
    def setUp(self):
        self.fixture = binding.DeliveryBindingTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)

    def apply_with_wrapper(self, program):
        real_git = shutil.which("git")
        self.assertIsNotNone(real_git)
        wrapper_dir = self.fixture.root / "wrapper"
        wrapper_dir.mkdir()
        wrapper = wrapper_dir / "git"
        wrapper.write_text(
            "#!" + sys.executable + "\n"
            "import pathlib, subprocess, sys\n"
            "args = sys.argv[1:]\n"
            "real_git = " + repr(real_git) + "\n"
            "mutating_apply = 'apply' in args and '--check' not in args and '--reverse' not in args\n"
            + program,
            encoding="utf-8",
        )
        wrapper.chmod(0o755)
        return self.fixture.apply_delivery(
            env=dict(os.environ, PATH=str(wrapper_dir) + os.pathsep + os.environ["PATH"])
        )

    def test_mode_only_patch_rollback_preserves_external_content_and_mode(self):
        target = self.fixture.base / "app.txt"
        target.chmod(0o644)
        (self.fixture.shadow / "app.txt").chmod(0o644)
        self.assertTrue(self.fixture.run_pipeline(coder=lambda path: (path / "app.txt").chmod(0o755)))
        injected = self.fixture.root / "external-edit-injected"
        program = (
            "code = subprocess.run([real_git] + args).returncode\n"
            "if code == 0 and mutating_apply:\n"
            "    target = pathlib.Path(" + repr(str(target)) + ")\n"
            "    if not target.stat().st_mode & 0o100:\n"
            "        sys.exit('mode-only patch did not apply')\n"
            "    target.write_text('human\\n')\n"
            "    target.chmod(0o700)\n"
            "    pathlib.Path(" + repr(str(injected)) + ").write_text('injected')\n"
            "sys.exit(code)\n"
        )
        result = self.apply_with_wrapper(program)
        self.assertTrue(injected.exists(), result.stderr)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(target.read_text(), "human\n")
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o700)

    def test_later_patch_failure_preserves_chmod_after_submodule_postimage(self):
        source = self.fixture.root / "submodule-source"
        source.mkdir()
        binding.git(source, "init")
        binding.git(source, "config", "user.name", "Rollback regression")
        binding.git(source, "config", "user.email", "test@example.invalid")
        (source / "module.txt").write_text("before\n")
        binding.git(source, "add", "-A")
        binding.git(source, "commit", "-m", "module baseline")
        binding.git(self.fixture.base, "-c", "protocol.file.allow=always", "submodule", "add", str(source), "dependency")
        self.fixture.refresh_baseline()
        child = self.fixture.base / "dependency"
        target = child / "module.txt"
        self.fixture.sub_baselines = {"dependency": binding.git(child, "rev-parse", "HEAD")}

        def coder(path):
            (path / "app.txt").write_text("APPROVED\n")
            (path / "dependency/module.txt").write_text("reviewed module\n")
            (path / "dependency/module.txt").chmod(0o755)

        self.assertTrue(self.fixture.run_pipeline(coder=coder))
        injected = self.fixture.root / "later-edit-injected"
        child_applied = self.fixture.root / "submodule-applied"
        program = (
            "cwd = args[args.index('-C') + 1] if '-C' in args else ''\n"
            "if mutating_apply and cwd == " + repr(str(self.fixture.base)) + ":\n"
            "    if not pathlib.Path(" + repr(str(child_applied)) + ").exists():\n"
            "        sys.exit('submodule patch was not applied first')\n"
            "    pathlib.Path(" + repr(str(target)) + ").chmod(0o700)\n"
            "    pathlib.Path(" + repr(str(injected)) + ").write_text('injected')\n"
            "    sys.exit('injected later patch failure')\n"
            "code = subprocess.run([real_git] + args).returncode\n"
            "if code == 0 and mutating_apply and cwd == " + repr(str(child)) + ":\n"
            "    pathlib.Path(" + repr(str(child_applied)) + ").write_text('applied')\n"
            "sys.exit(code)\n"
        )
        result = self.apply_with_wrapper(program)
        self.assertTrue(injected.exists(), result.stderr)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(target.read_text(), "reviewed module\n")
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o700)
        self.assertEqual((self.fixture.base / "app.txt").read_text(), "BASE\n")

    def test_missing_checkpoint_preserves_chmod_after_mode_only_patch(self):
        target = self.fixture.base / "app.txt"
        target.chmod(0o644)
        (self.fixture.shadow / "app.txt").chmod(0o644)
        self.assertTrue(self.fixture.run_pipeline(coder=lambda path: (path / "app.txt").chmod(0o755)))
        injected = self.fixture.root / "checkpoint-write-failed"
        wrapper_dir = self.fixture.root / "wrapper"
        wrapper_dir.mkdir()
        wrapper = wrapper_dir / "python3"
        wrapper.write_text(
            "#!" + sys.executable + "\n"
            "import pathlib, subprocess, sys\n"
            "marker = pathlib.Path(" + repr(str(injected)) + ")\n"
            "if sys.argv[-1].endswith('/main.json') and not marker.exists():\n"
            "    target = pathlib.Path(" + repr(str(target)) + ")\n"
            "    if not target.stat().st_mode & 0o100:\n"
            "        sys.exit('mode-only patch did not apply')\n"
            "    target.chmod(0o700)\n"
            "    marker.write_text('checkpoint failed after external chmod')\n"
            "    sys.exit('injected checkpoint write failure')\n"
            "sys.exit(subprocess.run([" + repr(sys.executable) + "] + sys.argv[1:], stdin=sys.stdin).returncode)\n",
            encoding="utf-8",
        )
        wrapper.chmod(0o755)
        result = self.fixture.apply_delivery(
            env=dict(os.environ, PATH=str(wrapper_dir) + os.pathsep + os.environ["PATH"])
        )
        self.assertTrue(injected.exists(), result.stderr)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(target.read_text(), "BASE\n")
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o700)

    def apply_after_checkpoint_injection(self, injection):
        marker = self.fixture.root / "postimage-injected"
        wrapper_dir = self.fixture.root / "wrapper"
        wrapper_dir.mkdir()
        wrapper = wrapper_dir / "python3"
        wrapper.write_text(
            "#!" + sys.executable + "\nimport pathlib,subprocess,sys\n"
            "marker = pathlib.Path(" + repr(str(marker)) + ")\n"
            "code = subprocess.run([" + repr(sys.executable) + "] + sys.argv[1:], stdin=sys.stdin).returncode\n"
            "if code == 0 and sys.argv[-1].endswith('/main.json') and not marker.exists():\n"
            + injection
            + "    marker.write_text('injected after valid postimage checkpoint')\n"
            "sys.exit(code)\n",
            encoding="utf-8",
        )
        wrapper.chmod(0o755)
        result = self.fixture.apply_delivery(
            env=dict(os.environ, PATH=str(wrapper_dir) + os.pathsep + os.environ["PATH"])
        )
        self.assertTrue(marker.exists(), result.stderr)
        return result

    def test_oversized_unrelated_ignored_cache_does_not_block_safe_reversal(self):
        (self.fixture.base / ".gitignore").write_text(".cache/\n")
        (self.fixture.base / ".cache").mkdir()
        self.fixture.refresh_baseline()
        self.assertTrue(self.fixture.run_pipeline())
        cache = self.fixture.base / ".cache/background-cache.bin"
        result = self.apply_after_checkpoint_injection(
            "    with pathlib.Path(" + repr(str(cache)) + ").open('wb') as stream:\n"
            "        stream.truncate(513 * 1024 * 1024)\n"
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("max_bytes limit", result.stderr)
        self.assertIn("本次已应用补丁已撤销", result.stderr)
        self.assertEqual((self.fixture.base / "app.txt").read_text(), "BASE\n")
        self.assertEqual(cache.stat().st_size, 513 * 1024 * 1024)

    def test_valid_checkpoint_still_preserves_later_external_content_and_mode(self):
        self.assertTrue(self.fixture.run_pipeline())
        target = self.fixture.base / "app.txt"
        result = self.apply_after_checkpoint_injection(
            "    target = pathlib.Path(" + repr(str(target)) + ")\n"
            "    target.write_text('human after checkpoint\\n')\n"
            "    target.chmod(0o700)\n"
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("external content or mode edit", result.stderr)
        self.assertEqual(target.read_text(), "human after checkpoint\n")
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o700)


if __name__ == "__main__":
    unittest.main()
