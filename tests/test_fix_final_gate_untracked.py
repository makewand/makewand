"""The test gate never takes a tracked file for test output or for an ignored file (gate-untracked).

orchestrator._filter_non_deliverable_test_changes lets a test run change test
output and gitignored files, but not tested inputs. After git check-ignore it
also matched the root .gitignore by hand, with a substring rule: a tracked
src/environment.js counted as ignored for env/, src/distance.js for dist,
lib/rebuild.js for build/ and a force-added src/coverage/reporter.js for
coverage/, so a test that rewrote them passed the gate (r4_fallback.py). The hand
matching now runs only where git check-ignore cannot run at all (exit code other
than 0 or 1), never decides for a tracked path, and follows .gitignore rules
instead of substrings. Every repository is temporary.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import os
import tempfile
import unittest
from unittest.mock import patch

from makewand import git_helper, orchestrator as orch
from makewand.git_helper import run_git_cmd


def git(root, *arguments):
    code, out, error = run_git_cmd(["git", *arguments], cwd=str(root))
    if code != 0:
        raise AssertionError(f"git {arguments}: {out} {error}")
    return out


class GateTestCase(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-final-gate-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)

    def write(self, root, files):
        for rel, content in files.items():
            (root / rel).parent.mkdir(parents=True, exist_ok=True)
            (root / rel).write_text(content)

    def make_repo(self, gitignore, tracked=(), name="repo"):
        """A repository whose .gitignore is gitignore and that tracks the paths tracked even when ignored."""
        root = self.base / name
        root.mkdir()
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"], ["config", "user.email", "fixture@example.invalid"]):
            git(root, *arguments)
        self.write(root, {".gitignore": gitignore, "app.py": "VALUE = 1\n"})
        self.write(root, {rel: "x = 1;\n" for rel in tracked})
        git(root, "add", "-A")
        if tracked:
            git(root, "add", "-f", "--", *tracked)
        git(root, "commit", "-qm", "base")
        return root


class TrackedPathTests(GateTestCase):
    def test_tracked_file_whose_path_matches_an_ignore_pattern_is_a_tested_input(self):
        for gitignore, rel in (("env/\n", "src/environment.js"), ("coverage/\n", "src/coverage/reporter.js"),
                               ("dist\n", "src/distance.js"), ("build/\n", "lib/rebuild.js"),
                               ("coverage/\n", "coverage/reporter.js")):
            with self.subTest(gitignore=gitignore.strip(), rel=rel):
                repo = self.make_repo(gitignore, tracked=[rel], name="repo-" + rel.replace("/", "-"))
                self.assertIn(rel, git(repo, "ls-files"))
                self.assertEqual(orch._filter_non_deliverable_test_changes([rel], str(repo)), [rel])

    def test_untracked_file_whose_name_only_contains_a_pattern_is_a_tested_input(self):
        repo = self.make_repo("dist\nenv/\n")
        self.write(repo, {"src/distance.js": "d\n", "src/environment.js": "e\n"})
        self.assertEqual(orch._filter_non_deliverable_test_changes(["src/distance.js", "src/environment.js"], str(repo)),
                         ["src/distance.js", "src/environment.js"])

    def test_untracked_ignored_files_and_test_output_still_pass(self):
        repo = self.make_repo("results/\n*.db\n")
        self.write(repo, {"results/out.xml": "<x/>\n", "fixtures.db": "db\n", "junit-unit.xml": "<t/>\n"})
        changed = ["results/out.xml", "fixtures.db", "junit-unit.xml", "app.py"]
        self.assertEqual(orch._filter_non_deliverable_test_changes(changed, str(repo)), ["app.py"])


class CheckIgnoreUnavailableTests(GateTestCase):
    def check_ignore_answers(self, code):
        """git check-ignore exits with code; every other git command runs."""
        real = git_helper.run_git_cmd

        def fake(command, *args, **kwargs):
            if isinstance(command, list) and command[:2] == ["git", "check-ignore"]:
                return code, b"", b"fatal: check-ignore cannot run here\n"
            return real(command, *args, **kwargs)
        return patch.object(git_helper, "run_git_cmd", side_effect=fake)

    def test_hand_matching_never_decides_for_a_tracked_path(self):
        repo = self.make_repo("coverage/\n*.db\n", tracked=["src/coverage/reporter.js"])
        self.write(repo, {"src/coverage/new.js": "n\n", "fixtures.db": "db\n"})
        changed = ["src/coverage/reporter.js", "src/coverage/new.js", "fixtures.db", "app.py"]
        with self.check_ignore_answers(128):
            kept = orch._filter_non_deliverable_test_changes(changed, str(repo))
        self.assertEqual(kept, ["src/coverage/reporter.js", "app.py"])

    def test_hand_matching_never_runs_when_git_answered(self):
        repo = self.make_repo("dist\n")
        self.write(repo, {"dist": "a file named dist\n"})
        with self.check_ignore_answers(1):  # git looked: nothing is ignored
            self.assertEqual(orch._filter_non_deliverable_test_changes(["dist", "app.py"], str(repo)), ["dist", "app.py"])

    def test_tracked_paths_that_git_cannot_list_fail_closed(self):
        repo = self.make_repo("coverage/\n")
        real = git_helper.run_git_cmd

        def fake(command, *args, **kwargs):
            if isinstance(command, list) and command[:2] in (["git", "check-ignore"], ["git", "ls-files"]):
                return 128, b"", b"fatal: index file corrupt\n"
            return real(command, *args, **kwargs)
        with patch.object(git_helper, "run_git_cmd", side_effect=fake):
            with self.assertRaises(OSError):
                orch._filter_non_deliverable_test_changes(["src/coverage/x.js"], str(repo))


class OutsideGitTests(GateTestCase):
    def test_root_gitignore_rules_decide_outside_a_work_tree(self):
        plain = self.base / "plain"
        plain.mkdir()
        self.assertIsNone(git_helper.find_git_root(plain))
        (plain / ".gitignore").write_text("/dist\nenv/\n*.out\n!keep.out\nbuild/*.cfg\n# comment\n")
        expected = {"dist/app.js": False, "src/dist/app.js": True, "src/environment.js": True, "env/bin/activate": False,
                    "a.out": False, "nested/b.out": False, "keep.out": True, "build/x.cfg": False, "src/build/x.cfg": True,
                    "server.py": True, "env": True}
        kept = orch._filter_non_deliverable_test_changes(sorted(expected), str(plain))
        self.assertEqual(kept, sorted(rel for rel, stays in expected.items() if stays))

    def test_fallback_pattern_semantics(self):
        ignored = orch._gitignore_fallback_ignored
        self.assertTrue(ignored("pkg/coverage/x.js", ["coverage/"]))
        self.assertFalse(ignored("pkg/coverage", ["coverage/"]))  # a file, while the pattern names directories
        self.assertFalse(ignored("src/distance.js", ["dist"]))
        self.assertTrue(ignored("a/b/c.tmp1", ["*.tmp1"]))
        self.assertFalse(ignored("x/dist/a", ["/dist"]))
        self.assertTrue(ignored("dist/a", ["/dist"]))
        self.assertFalse(ignored("keep.out", ["*.out", "!keep.out"]))
        self.assertTrue(ignored("keep.out", ["!keep.out", "*.out"]))  # the last matching pattern decides
        self.assertTrue(ignored(os.path.join("w", "x.out").replace(os.sep, "\\"), ["*.out"]))


if __name__ == "__main__":
    unittest.main()
