"""Host-side git never runs a clean/smudge filter that a repository delivers (py-sec-1).

Every fixture plants a filter driver whose commands append to a marker file outside
the repositories: a marker line means makewand ran the filter on the host.
"""
try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import os
import shlex
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand import git_helper


@unittest.skipIf(os.name == "nt", "fixtures use POSIX shell filters and executable scripts")
class FilterFixture(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand_attr_shield_")
        self.addCleanup(temporary.cleanup)
        self.root = Path(os.path.realpath(temporary.name))
        self.marker = self.root / "marker"
        self.filter_script = self.root / "bin" / "filter"
        self.filter_script.parent.mkdir()
        self.filter_script.write_text(f'#!/bin/sh\nprintf "%s:%s\\n" "$1" "$2" >> {shlex.quote(str(self.marker))}\ncat\n')
        self.filter_script.chmod(0o755)

    def git(self, cwd, *arguments):
        """Fixture setup runs plain git, never makewand's hardened path."""
        env = dict(os.environ, GIT_AUTHOR_NAME="Fixture", GIT_AUTHOR_EMAIL="fixture@example.invalid",
                   GIT_COMMITTER_NAME="Fixture", GIT_COMMITTER_EMAIL="fixture@example.invalid")
        return subprocess.run(["git", *arguments], cwd=str(cwd), env=env, check=True,
                              capture_output=True, text=True).stdout

    def plant_filter(self, config, name="evil", required=False):
        for kind in ("clean", "smudge"):
            command = f"{shlex.quote(str(self.filter_script))} {kind} %f"
            self.git(self.root, "config", "-f", str(config), f"filter.{name}.{kind}", command)
        if required:
            self.git(self.root, "config", "-f", str(config), f"filter.{name}.required", "true")

    def plant_attributes(self, git_dir, line="* filter=evil"):
        (git_dir / "info").mkdir(parents=True, exist_ok=True)
        (git_dir / "info" / "attributes").write_text(line + "\n")

    def committed_git_dir(self, destination, files):
        """A git directory (HEAD, objects, refs, config) whose main branch commits ``files``."""
        seed = self.root / ("seed-" + destination.name.strip())
        seed.mkdir()
        self.git(seed, "init", "-q", "-b", "main")
        for name, text in files.items():
            (seed / name).write_text(text)
        self.git(seed, "add", "-A")
        self.git(seed, "commit", "-qm", "seed")
        shutil.move(str(seed / ".git"), str(destination))
        shutil.rmtree(seed)
        return destination

    def linked_git_dir(self, worktree, common):
        """A .git directory holding only HEAD and a commondir naming ``common``."""
        dot_git = worktree / ".git"
        dot_git.mkdir(parents=True)
        (dot_git / "HEAD").write_text("ref: refs/heads/main\n")
        (dot_git / "commondir").write_text(os.path.relpath(common, dot_git) + "\n")
        return dot_git

    def repository(self, path, files):
        path.mkdir(parents=True, exist_ok=True)
        self.git(path, "init", "-q", "-b", "main")
        for name, text in files.items():
            (path / name).write_text(text)
        self.git(path, "add", "-A")
        self.git(path, "commit", "-qm", "init")
        return path

    def marker_lines(self):
        return self.marker.read_text().split() if self.marker.exists() else []


class CommondirShieldTests(FilterFixture):
    """py-sec-1: info/attributes of every git and common directory git uses is shielded."""

    def test_review_diff_shields_the_commondir_of_a_git_directory(self):
        common = self.committed_git_dir(self.root / "common", {"f.txt": "one\n"})
        repo = self.root / "repo"
        self.linked_git_dir(repo, common)
        (repo / "f.txt").write_text("one\nchanged\n")
        self.plant_filter(common / "config")
        self.plant_attributes(common)

        diff, error = git_helper.get_git_diff_status(str(repo))
        self.assertIsNone(error)
        self.assertIn("+changed", diff)
        self.assertEqual(self.marker_lines(), [])
        self.assertEqual((common / "info" / "attributes").read_text(), "* filter=evil\n")
        self.assertIn(common / "info" / "attributes", git_helper._get_git_info_attributes_paths(repo))

    def test_review_diff_shields_the_commondir_of_a_nested_repository(self):
        # The sandbox cannot write the top-level .git but can plant a nested
        # repository, a common directory in the work tree and .gitmodules.
        top = self.repository(self.root / "top", {"a.txt": "a\n"})
        common = self.committed_git_dir(top / "c", {"s.txt": "s\n"})
        sub = top / "sub"
        self.linked_git_dir(sub, common)
        (sub / "s.txt").write_text("s\nchanged\n")
        (top / ".gitmodules").write_text('[submodule "sub"]\n\tpath = sub\n\turl = ./sub\n')
        self.plant_filter(common / "config")
        self.plant_attributes(common)

        diff, error = git_helper.get_git_diff_status(str(top))
        self.assertIsNone(error)
        self.assertIn("+changed", diff)
        self.assertEqual(self.marker_lines(), [])

    def test_implicit_bare_repository_with_a_work_tree_is_shielded(self):
        # git passes over an invalid .git directory and uses the directory itself.
        bare = self.committed_git_dir(self.root / "bare", {"a.txt": "a\n"})
        self.git(bare, "config", "core.bare", "false")
        self.git(bare, "config", "core.worktree", ".")
        (bare / ".git").mkdir()
        (bare / "a.txt").write_text("a\nchanged\n")
        self.plant_filter(bare / "config")
        self.plant_attributes(bare)

        code, _, error = git_helper.run_git_cmd(["git", "add", "a.txt"], cwd=str(bare))
        self.assertEqual(code, 0, error)
        self.assertEqual(self.marker_lines(), [])
        self.assertEqual((bare / "info" / "attributes").read_text(), "* filter=evil\n")

    def test_a_gitfile_path_is_read_exactly_as_git_reads_it(self):
        # "gitdir:  x" names " x": git keeps every byte after "gitdir: ".
        repo = self.root / "repo"
        repo.mkdir()
        target = self.committed_git_dir(repo / " x", {"f.txt": "one\n"})
        (repo / ".git").write_text("gitdir:  x\n")
        (repo / "f.txt").write_text("one\nchanged\n")
        self.plant_filter(target / "config")
        self.plant_attributes(target)

        diff, error = git_helper.get_git_diff_status(str(repo))
        self.assertIsNone(error)
        self.assertIn("+changed", diff)
        self.assertEqual(self.marker_lines(), [])

    def test_commondir_content_is_read_exactly_as_git_reads_it(self):
        git_dir = self.root / "gitdir"
        git_dir.mkdir()
        for content, expected in ((b"../c\r\n", self.root / "c"),
                                  (b"../c\0ignored", self.root / "c"),
                                  (b" ../c \n", git_dir / " .." / "c "),
                                  (b"\n", git_dir),
                                  (str(self.root / "abs").encode() + b"\n", self.root / "abs")):
            with self.subTest(content=content):
                (git_dir / "commondir").write_bytes(content)
                self.assertEqual(git_helper._git_common_dir(git_dir), Path(os.path.realpath(expected)))

    def test_head_is_validated_exactly_as_git_validates_it(self):
        head = self.root / "HEAD"
        for content, valid in ((b"ref: refs/heads/main\n", True), (b"ref:\t refs/x", True),
                               (b"ref:\x0brefs/x", False), (b"ref: heads/main", False),
                               (b"a" * 40, True), (b"A" * 39 + b"\n", False), (b"", False)):
            with self.subTest(content=content):
                head.write_bytes(content)
                self.assertIs(git_helper._valid_git_head(head), valid)
        head.unlink()
        head.symlink_to("refs/heads/main")
        self.assertTrue(git_helper._valid_git_head(head))
        head.unlink()
        os.mkfifo(head)
        self.assertFalse(git_helper._valid_git_head(head))  # never waits on the FIFO

    def test_an_unreadable_commondir_refuses_to_start_git(self):
        repo = self.repository(self.root / "repo", {"f.txt": "one\n"})
        commondir = repo / ".git" / "commondir"

        def empty():
            commondir.write_bytes(b"")

        def directory():
            commondir.mkdir()

        def dangling():
            commondir.symlink_to(self.root / "missing")

        def fifo():
            os.mkfifo(commondir)

        for plant in (empty, directory, dangling, fifo):
            with self.subTest(commondir=plant.__name__):
                plant()
                try:
                    with patch.object(git_helper.subprocess, "run") as command:
                        code, _, error = git_helper.run_git_cmd(["git", "status"], cwd=str(repo))
                    self.assertNotEqual(code, 0)
                    self.assertIn("Cannot shield Git attributes", error)
                    command.assert_not_called()
                finally:
                    if commondir.is_dir() and not commondir.is_symlink():
                        commondir.rmdir()
                    else:
                        commondir.unlink()


if __name__ == "__main__":
    unittest.main()
