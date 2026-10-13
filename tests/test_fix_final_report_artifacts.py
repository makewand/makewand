"""Host-mode delivery keeps Playwright's rewritten traces and judges reports from the run directory (report-artifacts).

Round 4 made the delivery check keep a pre-existing ignored report the run
rewrote only when it is plain data or unchanged, which blocked (exit 11) two runs
round 3 delivered:

* Playwright with trace/video 'on' rewrites test-results/<test>/trace.zip and
  video.webm (and their copies in playwright-report/data/) on every run
  (e2e_r4.py pw_rerun). Its own trace archives and videos in those directories
  now count as report data; any other rewritten archive, and code such as a
  regenerated .js asset, still blocks.
* `makewand run -C repo/sub` with only pytest.ini in sub: pytest-cov writes its
  ignored HTML report to sub/coverage. The test gate judges paths from the run
  directory and passed it; the delivery judged them from the repository root,
  found no report directory and blocked (subdir_cov). The delivery now treats the
  task's directory as a report root too.

Every repository is temporary; no provider runs.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import unittest

from makewand import git_helper
from makewand.git_helper import HostWorkspaceTransaction
from test_fix_delivery_host_block import HostDeliveryCase, git

PLAYWRIGHT_IGNORE = "/playwright-report/\n/test-results/\n"


class PlaywrightRerunTests(HostDeliveryCase):
    OUTPUT = {"test-results/t1-chromium/trace.zip": b"PK\x03\x04run1", "test-results/t1-chromium/video.webm": b"\x1aE\xdf\xa3run1",
              "test-results/t1-chromium/test-failed-1.png": b"\x89PNGrun1", "test-results/.last-run.json": b'{"status": "failed"}',
              "playwright-report/index.html": b"<html>run1</html>", "playwright-report/data/0a1b2c.zip": b"PK\x03\x04run1",
              "playwright-report/data/0a1b2c.webm": b"\x1aE\xdf\xa3run1"}

    def rerun(self, paths):
        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            for rel in paths:
                (root / rel).unlink()  # Playwright replaces its output
                (root / rel).write_bytes(self.OUTPUT[rel].replace(b"run1", b"run2"))
        return task

    def test_traces_and_videos_a_rerun_rewrites_are_kept_as_report_data(self):
        """e2e_r4.py pw_rerun: exit 11 '…被篡改或删除' in round 4, exit 0 in round 3."""
        self.commit({"app.py": "VALUE = 1\n"}, gitignore=PLAYWRIGHT_IGNORE)
        self.write(self.OUTPUT)  # left by an earlier run, ignored
        txn = self.deliver(self.rerun(list(self.OUTPUT)))
        self.assertDelivered(txn)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 2\n")
        for rel, data in self.OUTPUT.items():
            self.assertEqual((self.root / rel).read_bytes(), data.replace(b"run1", b"run2"), rel)

    def test_a_package_roots_playwright_output_counts_too(self):
        self.commit({"app.py": "VALUE = 1\n", "web/package.json": "{}\n"}, gitignore="test-results/\n")
        self.write({"web/test-results/t1/trace.zip": b"PK\x03\x04run1"})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            (root / "web/test-results/t1/trace.zip").write_bytes(b"PK\x03\x04run2")

        self.assertDelivered(self.deliver(task))

    def test_other_rewritten_archives_and_report_code_still_block(self):
        for rel in ("data/fixtures.zip", "src/test-results/t1/trace.zip", "playwright-report/trace/sw.bundle.js",
                    "test-results/t1-chromium/helper.so"):
            with self.subTest(rel=rel):
                self.new_repo("repo-" + rel.replace("/", "-"))
                self.commit({"app.py": "VALUE = 1\n"}, gitignore=PLAYWRIGHT_IGNORE + "data/\nsrc/test-results/\n")
                self.write({rel: b"old bytes"})

                def task(root, rel=rel):
                    (root / "app.py").write_text("VALUE = 2\n")
                    (root / rel).write_bytes(b"new bytes")

                txn = self.deliver(task)
                self.assertBlocked(txn, rel)

    def test_definition(self):
        self.assertTrue(git_helper._is_playwright_artifact("test-results/t1-chromium/trace.zip"))
        self.assertTrue(git_helper._is_playwright_artifact("Playwright-Report/data/0a1b.WEBM"))
        self.assertFalse(git_helper._is_playwright_artifact("test-results/t1/trace.js"))
        self.assertFalse(git_helper._is_playwright_artifact("trace.zip"))
        self.assertFalse(git_helper._is_playwright_artifact("src/test-results/trace.zip"))
        self.assertFalse(git_helper._is_playwright_artifact("node_modules/x/test-results/trace.zip"))


class RunDirectoryReportRootTests(HostDeliveryCase):
    def setUp(self):
        super().setUp()
        self.commit({"sub/app.py": "VALUE = 1\n", "sub/pytest.ini": "[pytest]\n"}, gitignore="__pycache__/\n.coverage\ncoverage/\n")

    def start_in(self, directory):
        txn = HostWorkspaceTransaction(str(directory))
        self.assertIsNone(txn.capture_pre_snapshot())
        self.assertIsNone(txn.begin())
        return txn

    @staticmethod
    def coverage_run(root):
        (root / "sub/app.py").write_text("VALUE = 2\n")
        for rel in ("sub/coverage/index.html", "sub/coverage/style_cb_9ff733b0.css", "sub/coverage/status.json",
                    "sub/coverage/coverage_html_cb_dd2e7eb5.js", "sub/coverage/.gitignore", "sub/.coverage"):
            (root / rel).parent.mkdir(parents=True, exist_ok=True)
            (root / rel).write_text("report\n")

    def test_report_the_tests_write_in_the_run_directory_is_cleaned_up(self):
        """e2e_r4.py subdir_cov: `makewand run -C repo/sub` exited 11; the gate had passed the report."""
        txn = self.start_in(self.root / "sub")
        self.coverage_run(self.root)
        self.finalize(txn)
        self.assertDelivered(txn)
        self.assertEqual((self.root / "sub/app.py").read_text(), "VALUE = 2\n")
        self.assertFalse((self.root / "sub/coverage").exists())
        self.assertFalse((self.root / "sub/.coverage").exists())

    def test_same_files_still_block_a_task_run_from_the_repository_root(self):
        txn = self.start_in(self.root)
        self.coverage_run(self.root)
        self.finalize(txn)
        self.assertBlocked(txn, "sub/coverage/index.html")

    def test_report_roots(self):
        txn = HostWorkspaceTransaction(str(self.root / "sub"))
        self.assertIsNone(txn.capture_pre_snapshot())
        self.assertEqual(txn._report_roots(), [("", str(self.root.resolve())), ("sub/", str((self.root / "sub").resolve()))])
        txn = HostWorkspaceTransaction(str(self.root))
        self.assertIsNone(txn.capture_pre_snapshot())
        self.assertEqual(txn._report_roots(), [("", str(self.root.resolve()))])
        self.assertIn("sub/app.py", git(self.root, "ls-files"))


if __name__ == "__main__":
    unittest.main()
