"""Duplicated logic that had drifted apart now has one definition (drift-dedupe).

* candidate.workspace_dirty_files parsed `git status -z --porcelain` itself: no rename
  pairs (the second name of a rename became garbage and the old name was lost), names
  decoded with UTF-8 "replace" and stripped of leading/trailing spaces, and the files
  of an untracked directory reported as the directory. It now uses
  git_helper.git_status_paths, the parser of get_dirty_files: --no-renames, byte-exact
  names (os.fsdecode), every untracked file. detect_conflicts' committed-change check
  decoded names the same lossy way.
* The directory names whose untracked files no isolated copy receives were computed
  twice from CLONE_EXCLUDED_DIRS (clone_isolated_worktree and build_manifest(host=True)
  must agree): now git_helper.CLONE_UNCOPIED_DIRS.
* HostWorkspaceTransaction.hooks_shared had no caller.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import os
import tempfile
import unittest
from unittest.mock import patch

from makewand import candidate as candidate_module, git_helper
from makewand.candidate import (CandidateManager, build_manifest, committable_work_in_progress, file_record,
                                workspace_dirty_files)
from makewand.git_helper import clone_isolated_worktree, run_git_cmd

LATIN = os.fsdecode(b"lat\xe9n.txt")  # not valid UTF-8: a surrogate escape


def git(repo, *arguments):
    code, out, error = run_git_cmd(["git", *arguments], cwd=str(repo))
    assert code == 0, error
    return out


class DirtyFilesTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="makewand-drift-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        for name, text in {"old.txt": "old\n", " lead.txt": "lead\n", "trail.txt ": "trail\n",
                           LATIN: "latin\n", "kept.txt": "kept\n"}.items():
            (self.repo / name).write_text(text)
        for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                          ["config", "user.email", "fixture@example.invalid"], ["add", "-A"], ["commit", "-qm", "base"]):
            git(self.repo, *arguments)

    def test_renames_spaces_undecodable_names_and_untracked_directories(self):
        git(self.repo, "mv", "old.txt", "new.txt")
        for name in (" lead.txt", "trail.txt ", LATIN):
            (self.repo / name).write_text("work in progress\n")
        (self.repo / "fresh").mkdir()
        (self.repo / "fresh" / "module.py").write_text("NEW = 1\n")
        dirty = workspace_dirty_files(self.repo)
        self.assertEqual(dirty, {"old.txt", "new.txt", " lead.txt", "trail.txt ", LATIN, "fresh/module.py"})
        self.assertEqual(dirty, set(git_helper.get_dirty_files(str(self.repo))))
        self.assertEqual(git_helper.git_status_paths(self.repo), git_helper.get_dirty_files(str(self.repo)))

    def test_outside_git_there_are_no_dirty_files(self):
        plain = self.root / "plain"
        plain.mkdir()
        (plain / "a.txt").write_text("a\n")
        self.assertEqual(workspace_dirty_files(plain), set())
        self.assertIsNone(git_helper.git_status_paths(plain))

    def test_committable_work_in_progress_sees_the_exact_names(self):
        for name in (" lead.txt", "trail.txt ", LATIN):
            (self.repo / name).write_text("the work the candidate was built on\n")
        baseline = {name: file_record(self.repo / name) for name in (" lead.txt", "trail.txt ", LATIN, "kept.txt")}
        git(self.repo, "mv", "old.txt", "renamed.txt")  # an uncommitted deletion of old.txt
        conflicts = [" lead.txt", "trail.txt ", LATIN, "old.txt", "kept.txt"]
        self.assertEqual(committable_work_in_progress(self.repo, conflicts, baseline),
                         [" lead.txt", "trail.txt ", LATIN, "old.txt"])

    def test_detect_conflicts_reads_committed_changes_byte_exact(self):
        base = git(self.repo, "rev-parse", "HEAD").strip()
        candidate = self.root / "candidate"
        clone_isolated_worktree(str(self.repo), candidate)
        candidate_base = git(candidate, "rev-parse", "HEAD").strip()
        (candidate / LATIN).write_text("candidate edit\n")
        (candidate / " lead.txt").write_text("candidate edit\n")
        # The user commits changes to both files after the candidate's baseline.
        (self.repo / LATIN).write_text("user commit\n")
        (self.repo / " lead.txt").write_text("user commit\n")
        git(self.repo, "commit", "-qam", "user")
        conflicts = CandidateManager.detect_conflicts(str(self.repo), candidate, baseline_manifest=None,
                                                      baseline_commit=base, candidate_baseline_commit=candidate_base)
        self.assertEqual(sorted(conflicts), sorted([LATIN, " lead.txt"]))


class SharedDefinitionTests(unittest.TestCase):
    def test_copies_and_host_manifests_share_the_uncopied_directory_names(self):
        self.assertIs(candidate_module._uncopied_output_names(), git_helper.CLONE_UNCOPIED_DIRS)
        self.assertEqual(git_helper.CLONE_UNCOPIED_DIRS,
                         git_helper.CLONE_EXCLUDED_DIRS - {"__pycache__", ".pytest_cache", ".mypy_cache",
                                                           ".ruff_cache", ".cache"})
        with tempfile.TemporaryDirectory(prefix="makewand-drift-") as temporary:
            repo = Path(temporary) / "repo"
            (repo / "dist").mkdir(parents=True)
            (repo / "app.py").write_text("A = 1\n")
            for arguments in (["init", "-q"], ["config", "user.name", "Fixture"],
                              ["config", "user.email", "fixture@example.invalid"], ["add", "-A"],
                              ["commit", "-qm", "base"]):
                git(repo, *arguments)
            (repo / "dist" / "bundle.js").write_text("untracked build output\n")
            # One definition: changing it changes what the copy receives and what the host binds alike.
            for names, expected in ((git_helper.CLONE_UNCOPIED_DIRS, False),
                                    (git_helper.CLONE_UNCOPIED_DIRS - {"dist"}, True)):
                with self.subTest(dist_copied=expected), patch.object(git_helper, "CLONE_UNCOPIED_DIRS", names):
                    copy = Path(temporary) / f"copy-{expected}"
                    clone_isolated_worktree(str(repo), copy)
                    self.assertEqual((copy / "dist" / "bundle.js").exists(), expected)
                    self.assertEqual("dist/bundle.js" in build_manifest(repo, host=True), expected)

    def test_unused_hooks_shared_property_is_gone(self):
        self.assertFalse(hasattr(git_helper.HostWorkspaceTransaction, "hooks_shared"))


if __name__ == "__main__":
    unittest.main()
