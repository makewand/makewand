"""Repository metadata is never written through a link from init.templateDir (symlink-exclude).

git init copies the template directory as it is, symbolic links included: a user whose
global init.templateDir links info/exclude (or config) to a shared file gets that link
in every new repository, makewand's private copies included. DagStaging wrote the
repository's exclude rules into the staging copy's info/exclude with open(..., "wb"),
which followed the link and overwrote the user's shared template file; the private
copies (clone_isolated_worktree) and the ephemeral baseline of a non-Git directory
(init_git_baseline) wrote makewand's identity with git config through a linked config,
and the ephemeral marker followed a link of its name. Every such write now replaces
the link with a regular file of the repository (git_helper.write_repository_file) and
the shared files stay byte for byte as they were.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import json
import os
import stat
import subprocess
import unittest
from unittest.mock import patch

from makewand import git_helper, task_dag
from makewand.git_helper import clone_isolated_worktree, init_git_baseline, run_git_cmd
from test_fix_orch_dag_r3 import DagFixture, git

SHARED_EXCLUDE = b"# the user's shared template exclude\n*.template-secret\n"
REPO_EXCLUDE = b"local-only/\n"


@unittest.skipUnless(git_helper.HOST_TRANSACTION_SUPPORTED, "POSIX directory handles")
class TemplateLinkTests(DagFixture):
    def setUp(self):
        super().setUp()
        # The repository under test exists before the template is configured.
        self.repo = self.make_repo(files={"app.py": "A = 1\n"})
        (self.repo / ".git" / "info").mkdir(exist_ok=True)
        (self.repo / ".git" / "info" / "exclude").write_bytes(REPO_EXCLUDE)
        self.shared = self.root / "shared"
        self.shared.mkdir()
        (self.shared / "exclude").write_bytes(SHARED_EXCLUDE)
        (self.shared / "config").write_bytes(b"[core]\n\trepositoryformatversion = 0\n")
        template = self.root / "template"
        (template / "info").mkdir(parents=True)
        os.symlink(self.shared / "exclude", template / "info" / "exclude")
        os.symlink(self.shared / "config", template / "config")
        # A link named like the ephemeral marker, leading nowhere yet.
        os.symlink(self.shared / "marker", template / git_helper.EPHEMERAL_GIT_MARKER)
        home = self.root / "home"
        home.mkdir()
        (home / ".gitconfig").write_text(f"[init]\n\ttemplateDir = {template}\n", encoding="utf-8")
        env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
        env.update(HOME=str(home), GIT_CONFIG_NOSYSTEM="1")
        # git init writes its own core settings through a linked config (git's behaviour, for
        # every repository the user creates): let it do that once before taking the snapshot.
        subprocess.run(["git", "init", "-q", str(self.root / "normalise")], check=True, env=env)
        self.assertTrue((self.root / "normalise" / ".git" / "config").is_symlink())
        self.shared_config = (self.shared / "config").read_bytes()
        patcher = patch.dict(os.environ, {"HOME": str(home)})
        patcher.start()
        self.addCleanup(patcher.stop)

    def assert_shared_files_untouched(self):
        self.assertEqual((self.shared / "exclude").read_bytes(), SHARED_EXCLUDE)
        config = (self.shared / "config").read_bytes()
        self.assertNotIn(b"Makewand", config)
        self.assertNotIn(b"makewand@local", config)
        self.assertNotIn(b"excludesFile", config)
        self.assertEqual(config, self.shared_config)
        self.assertFalse(os.path.lexists(self.shared / "marker"))

    @staticmethod
    def regular(path):
        info = os.lstat(path)
        return stat.S_ISREG(info.st_mode) and info.st_nlink == 1

    def test_dag_staging_copies_the_exclude_rules_without_writing_the_template_file(self):
        staging = task_dag.DagStaging(str(self.repo))
        self.addCleanup(staging.discard)
        self.assert_shared_files_untouched()
        exclude = staging.workspace / ".git" / "info" / "exclude"
        self.assertTrue(self.regular(exclude))
        self.assertEqual(exclude.read_bytes(), REPO_EXCLUDE)
        for copy in (staging.baseline, staging.workspace):
            config = copy / ".git" / "config"
            self.assertTrue(self.regular(config), config)
            self.assertIn(b"Makewand", config.read_bytes())
        # The copy honours the repository's own rule.
        (staging.workspace / "local-only").mkdir()
        (staging.workspace / "local-only" / "data.txt").write_text("local\n")
        code, _, _ = run_git_cmd(["git", "check-ignore", "-q", "local-only/data.txt"], cwd=str(staging.workspace))
        self.assertEqual(code, 0)

    def test_dag_staging_with_a_core_excludes_file_keeps_the_template_config(self):
        excludes = self.root / "repo-excludes"
        excludes.write_text("*.cache-only\n")
        git(self.repo, "config", "core.excludesFile", str(excludes))
        staging = task_dag.DagStaging(str(self.repo))
        self.addCleanup(staging.discard)
        self.assert_shared_files_untouched()
        self.assertIn(str(excludes).encode(), (staging.workspace / ".git" / "config").read_bytes())

    def test_isolated_copy_never_writes_its_identity_into_the_template_config(self):
        copy = self.root / "copy"
        clone_isolated_worktree(str(self.repo), copy)
        self.assert_shared_files_untouched()
        self.assertTrue(self.regular(copy / ".git" / "config"))
        self.assertIn(b"Makewand", (copy / ".git" / "config").read_bytes())
        # The link to the template's exclude file is left as git made it: nothing writes it.
        self.assertEqual(git(copy, "log", "--format=%s", "-1").strip(), "Makewand isolated baseline")

    def test_ephemeral_baseline_never_writes_through_template_links(self):
        plain = self.root / "plain"
        plain.mkdir()
        (plain / "notes.txt").write_text("notes\n")
        created, error = init_git_baseline(str(plain), ephemeral=True)
        self.addCleanup(git_helper._remove_git_dir, plain / ".git")
        self.assertTrue(created, error)
        self.assert_shared_files_untouched()
        git_dir = plain / ".git"
        self.assertTrue(self.regular(git_dir / "config"))
        self.assertIn(b"Makewand", (git_dir / "config").read_bytes())
        marker = git_dir / git_helper.EPHEMERAL_GIT_MARKER
        self.assertTrue(self.regular(marker))
        self.assertEqual(json.loads(marker.read_text(encoding="utf-8"))["pid"], os.getpid())


@unittest.skipUnless(git_helper.HOST_TRANSACTION_SUPPORTED, "POSIX directory handles")
class WriteRepositoryFileTests(DagFixture):
    def setUp(self):
        super().setUp()
        self.git_dir = self.root / "repo.git"
        (self.git_dir / "info").mkdir(parents=True)
        self.shared = self.root / "shared"
        self.shared.mkdir()
        (self.shared / "exclude").write_bytes(SHARED_EXCLUDE)

    def test_a_link_at_the_file_is_replaced_never_followed(self):
        os.symlink(self.shared / "exclude", self.git_dir / "info" / "exclude")
        git_helper.write_repository_file(self.git_dir, "info/exclude", REPO_EXCLUDE)
        self.assertEqual((self.shared / "exclude").read_bytes(), SHARED_EXCLUDE)
        self.assertFalse((self.git_dir / "info" / "exclude").is_symlink())
        self.assertEqual((self.git_dir / "info" / "exclude").read_bytes(), REPO_EXCLUDE)

    def test_a_dangling_link_is_replaced_and_its_target_never_created(self):
        os.symlink(self.shared / "missing", self.git_dir / "info" / "exclude")
        git_helper.write_repository_file(self.git_dir, "info/exclude", REPO_EXCLUDE)
        self.assertFalse(os.path.lexists(self.shared / "missing"))
        self.assertEqual((self.git_dir / "info" / "exclude").read_bytes(), REPO_EXCLUDE)

    def test_a_linked_directory_above_the_file_is_replaced_never_followed(self):
        (self.git_dir / "info").rmdir()
        os.symlink(self.shared, self.git_dir / "info")
        git_helper.write_repository_file(self.git_dir, "info/exclude", REPO_EXCLUDE)
        self.assertEqual(sorted(path.name for path in self.shared.iterdir()), ["exclude"])
        self.assertEqual((self.shared / "exclude").read_bytes(), SHARED_EXCLUDE)
        self.assertFalse((self.git_dir / "info").is_symlink())
        self.assertEqual((self.git_dir / "info" / "exclude").read_bytes(), REPO_EXCLUDE)

    def test_a_hard_linked_file_is_replaced_and_the_other_name_kept(self):
        os.link(self.shared / "exclude", self.git_dir / "info" / "exclude")
        git_helper.write_repository_file(self.git_dir, "info/exclude", REPO_EXCLUDE)
        self.assertEqual((self.shared / "exclude").read_bytes(), SHARED_EXCLUDE)
        self.assertEqual((self.git_dir / "info" / "exclude").read_bytes(), REPO_EXCLUDE)

    def test_a_regular_file_is_rewritten_in_place_and_missing_directories_are_made(self):
        target = self.git_dir / "info" / "exclude"
        target.write_bytes(b"old rules, longer than the new ones\n")
        target.chmod(0o640)
        git_helper.write_repository_file(self.git_dir, "info/exclude", REPO_EXCLUDE)
        self.assertEqual(target.read_bytes(), REPO_EXCLUDE)
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o640)
        git_helper.write_repository_file(self.git_dir, "new/dir/file", b"x")
        self.assertEqual((self.git_dir / "new" / "dir" / "file").read_bytes(), b"x")

    def test_refusals(self):
        linked = self.root / "linked.git"
        os.symlink(self.shared, linked)
        with self.assertRaises(OSError):
            git_helper.write_repository_file(linked, "exclude", REPO_EXCLUDE)
        self.assertEqual((self.shared / "exclude").read_bytes(), SHARED_EXCLUDE)
        with self.assertRaises(IsADirectoryError):
            git_helper.write_repository_file(self.git_dir, "info", REPO_EXCLUDE)
        for invalid in ("../escape", "info/../../escape", "", "info//exclude"):
            with self.subTest(rel=invalid), self.assertRaises(OSError):
                git_helper.write_repository_file(self.git_dir, invalid, REPO_EXCLUDE)
        self.assertFalse(os.path.lexists(self.root / "escape"))

    def test_the_attributes_gate_is_never_created_through_a_link(self):
        repo = self.make_repo("gated")
        info = repo / ".git" / "info"
        info.mkdir(exist_ok=True)
        (info / "attributes").write_text("*.txt filter=harmful\n")
        os.symlink(self.shared / "gate-target", info / "attributes.mw_gate")
        code, _, error = run_git_cmd(["git", "status", "--porcelain"], cwd=str(repo))
        self.assertNotEqual(code, 0)
        self.assertIn("Cannot shield Git attributes", error)
        self.assertFalse(os.path.lexists(self.shared / "gate-target"))
        self.assertEqual((info / "attributes").read_text(), "*.txt filter=harmful\n")
        os.unlink(info / "attributes.mw_gate")
        code, _, error = run_git_cmd(["git", "status", "--porcelain"], cwd=str(repo))
        self.assertEqual(code, 0, error)
        self.assertTrue(self.regular(info / "attributes.mw_gate"))
        self.assertEqual((info / "attributes").read_text(), "*.txt filter=harmful\n")

    regular = staticmethod(TemplateLinkTests.regular)

    def test_own_repository_file_keeps_the_content_and_detaches_the_link(self):
        config = self.shared / "config"
        config.write_bytes(b"[core]\n\tbare = false\n")
        os.symlink(config, self.git_dir / "config")
        git_helper._own_repository_file(self.git_dir, "config")
        self.assertFalse((self.git_dir / "config").is_symlink())
        self.assertEqual((self.git_dir / "config").read_bytes(), b"[core]\n\tbare = false\n")
        (self.git_dir / "config").write_bytes(b"changed\n")
        self.assertEqual(config.read_bytes(), b"[core]\n\tbare = false\n")


if __name__ == "__main__":
    unittest.main()
