"""The test gate's .gitignore fallback follows git's own pattern rules (gitignore-fallback).

orchestrator._gitignore_fallback_ignored decides what the root .gitignore covers
only where `git check-ignore` cannot run (no work tree, a pathspec git refuses).
It matched with fnmatch, so "*" crossed "/" ("docs/*.md" covered
docs/sub/b.md) and "**/tmp" missed a root-level tmp/x; escapes, trailing spaces,
character classes and "a/**/b" were not git's either. It now ports git's
dir.c parsing and wildmatch, and every case below is checked against real
`git check-ignore`.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import os
import shutil
import subprocess
import tempfile
import unittest
import time
from unittest.mock import patch

from makewand import git_helper, orchestrator as orch

PATHS = ["a", "x/a", "x/y/a", "docs/a.md", "docs/sub/b.md", "x/docs/a.md", "tmp/x", "a/tmp/x", "a/b/tmp", "tmpx",
         "abc/x", "abc/x/y", "abc", "xabc/y", "a/b", "a/x/b", "a/x/y/b", "a/b/c", "ab", "x.pyc", "x.pyo", "x.py",
         "d/x.pyc", "root.txt", "a/root.txt", "build/x", "src/build/y", "build", "foo/bar", "foo/bar/baz", "foo",
         "keep.log", "x.log", "d/keep.log", "out/reports/x.xml", "out/x.xml", "1a.txt", "a1.txt", "#file",
         "!important", "foo ", "bar", "  lead", "lead", "café", "x*y", "xay", "q?", "qa", "x[y]", "xy", "]",
         "a]", "-", "b-c", "ab/cd", "abxcd", "dir/x/y/z.md", "dir/z.md", "z.md", "x/foo/bar", "foo/a/bar",
         "lat\udce9n.txt", "sp ace", "Upper", "upper", "X1", "x/z", "zz/x/z", "y/q/z", "a\\b"]

PATTERN_SETS = [
    ["docs/*.md"], ["**/tmp"], ["abc/**"], ["a/**/b"], ["*.py[co]"], ["/root.txt"], ["build/"], ["foo/*"],
    ["*.log", "!keep.log"], ["!keep.log", "*.log"], ["out/*", "!out/reports/"], ["[[:digit:]]*.txt"],
    ["\\#file"], ["\\!important"], ["foo\\ "], ["bar  "], ["  lead"], ["caf?"], ["caf??"], ["x\\*y"], ["q\\?"],
    ["x\\[y]"], ["[]a]"], ["[!]a]"], ["[a-c]"], ["b[-]c"], ["a**/b"], ["**"], ["*"], ["a/*"], ["**/foo/bar"],
    ["foo/**/"], ["/**/x"], ["dir/**/*.md"], ["a?c"], ["ab/cd", "!ab/"], ["a*cd"], ["a[/]b"], ["a\\"], ["[a-"],
    ["[[:bogus:]]"], ["[[:alpha:]x]"], ["[[:alpha]"], ["**/"], ["/"], ["!"], ["**/*.c", "!a/b.c"],
    ["lat?n.txt"], ["lat\udce9n.txt"], ["sp\\ ace"], ["[[:upper:]]*"], ["[[:xdigit:]][[:digit:]]"], ["x/**/z"],
    ["***/z"], ["x**z"], ["*/z"], ["[^a]b"], ["\\a"], ["a/b/"], ["x/**", "!x/z"], ["*", "!*/", "!*.md"],
    ["**/" * 6 + "x/y"], ["**/" * 8 + "x/y"], ["*x" * 1100],
    ["docs/", "!docs/a.md"], ["build/*", "!build/x"], ["a/", "!a/b"], ["*.c\r"], ["# comment", "", "x.log"],
]


def git_check_ignore(lines, paths):
    """The paths real git calls ignored by a root .gitignore holding lines (and nothing else)."""
    with tempfile.TemporaryDirectory(prefix="makewand-gitignore-oracle-") as scratch:
        repo = os.path.join(scratch, "repo")
        no_excludes = os.path.join(scratch, "no-excludes")
        open(no_excludes, "wb").close()
        subprocess.run(["git", "init", "-q", repo], check=True)
        with open(os.path.join(repo, ".gitignore"), "wb") as handle:
            handle.write(b"\n".join(os.fsencode(line) for line in lines) + b"\n")
        result = subprocess.run(["git", "-c", "core.excludesFile=" + no_excludes, "-c", "core.ignoreCase=false",
                                 "check-ignore", "--no-index", "--stdin", "-z"], cwd=repo,
                                input=b"\0".join(os.fsencode(p) for p in paths) + b"\0", capture_output=True)
        if result.returncode not in (0, 1):
            raise AssertionError(result.stderr)
        return {os.fsdecode(name) for name in result.stdout.split(b"\0") if name}


@unittest.skipIf(shutil.which("git") is None, "needs git")
class FallbackMatchesGitTests(unittest.TestCase):
    def test_every_pattern_and_path_as_git_check_ignore_decides(self):
        # Windows: no backslash (a separator there) and no undecodable name.
        paths = [p for p in PATHS if os.name != "nt" or ("\\" not in p and p.isprintable() and p == os.fsdecode(os.fsencode(p)))]
        mismatches = []
        for lines in PATTERN_SETS:
            truth = git_check_ignore(lines, paths)
            for path in paths:
                if orch._gitignore_fallback_ignored(path, lines) != (path in truth):
                    mismatches.append((lines, path, path in truth))
        self.assertEqual(mismatches, [])

    def test_the_reported_cases(self):
        ignored = orch._gitignore_fallback_ignored
        cases = [
            (["docs/*.md"], "docs/a.md", True), (["docs/*.md"], "docs/sub/b.md", False),  # "*" never crosses "/"
            (["**/tmp"], "tmp/x", True), (["**/tmp"], "a/b/tmp/x", True), (["**/tmp"], "atmp/x", False),
            (["abc/**"], "abc/x/y", True), (["abc/**"], "abc", False),  # everything inside, not abc itself
            (["a/**/b"], "a/b", True), (["a/**/b"], "a/x/y/b", True), (["a/**/b"], "a/xb", False),
            (["*.py[co]"], "pkg/m.pyc", True), (["*.py[co]"], "pkg/m.py", False), (["[!a]x"], "bx", True),
            (["x\\*y"], "x*y", True), (["x\\*y"], "xay", False),  # an escaped character is literal
            (["foo  "], "foo", True), (["foo\\ "], "foo ", True), (["foo\\ "], "foo", False),  # trailing spaces
            (["\\#file"], "#file", True), (["\\!important"], "!important", True),
            (["build/"], "build", False), (["build/"], "src/build/x", True),  # directories only
            (["out/", "!out/keep.txt"], "out/keep.txt", True),  # nothing inside an ignored directory comes back
            (["caf?"], "café", False), (["caf??"], "café", True),  # "?" is one byte, as in git
        ]
        for lines, path, expected in cases:
            with self.subTest(lines=lines, path=path):
                self.assertIs(ignored(path, lines), expected)
                self.assertIs(path in git_check_ignore(lines, [path]), expected)


class HostilePatternTests(unittest.TestCase):
    def test_deep_starstar_and_long_star_patterns_are_bounded_without_recursion(self):
        start = time.monotonic()
        for count in (6, 8):
            self.assertFalse(orch._gitignore_fallback_ignored('/'.join(['d'] * 30 + ['z']), ['**/' * count + 'x/y']))
        self.assertTrue(orch._gitignore_fallback_ignored('x' * 1100, ['*x' * 1100]))
        self.assertLess(time.monotonic() - start, 2)

    def test_exhausted_budget_keeps_the_input_even_with_an_earlier_ignore_rule(self):
        with patch.object(orch, '_WILDMATCH_MAX_WORK', 10):
            self.assertFalse(orch._gitignore_fallback_ignored('x' * 30, ['*', '!' + '*x' * 30]))


class FallbackInTheGateTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-gitignore-gate-")
        self.addCleanup(temporary.cleanup)
        self.plain = Path(temporary.name) / "plain"
        self.plain.mkdir()
        self.assertIsNone(git_helper.find_git_root(self.plain))

    def test_the_gate_reads_the_gitignore_as_git_does_outside_a_work_tree(self):
        # CRLF line ends, a byte-order mark, a pattern with trailing spaces and one with "**".
        (self.plain / ".gitignore").write_bytes(b"\xef\xbb\xbfdocs/*.md\r\n**/tmp\r\nnotes.txt   \r\n# c\r\n")
        changed = ["docs/a.md", "docs/sub/b.md", "tmp/x", "a/tmp/y", "notes.txt", "src/app.py"]
        kept = orch._filter_non_deliverable_test_changes(changed, str(self.plain))
        self.assertEqual(kept, ["docs/sub/b.md", "src/app.py"])

    def test_non_utf8_gitignore_comments_never_use_the_windows_fsdecoder(self):
        (self.plain / '.gitignore').write_bytes(b'# \xd6\xd0\xce\xc4\n*.log\n')
        with patch('makewand.git_helper.run_git_cmd', return_value=(128, b'', b'')), \
                patch('makewand.git_helper.tracked_paths', return_value=set()), \
                patch.object(orch.os, 'fsdecode', side_effect=UnicodeDecodeError('utf-8', b'\xff', 0, 1, 'invalid')):
            self.assertEqual(orch._filter_non_deliverable_test_changes(['app.py', 'server.log'], str(self.plain)), ['app.py'])

    @unittest.skipIf(os.name == "nt", "file names are not bytes on Windows")
    def test_an_undecodable_name_matches_its_own_bytes(self):
        (self.plain / ".gitignore").write_bytes(b"lat\xe9n.txt\n")
        kept = orch._filter_non_deliverable_test_changes(["lat\udce9n.txt", "latin.txt"], str(self.plain))
        self.assertEqual(kept, ["latin.txt"])


if __name__ == "__main__":
    unittest.main()
