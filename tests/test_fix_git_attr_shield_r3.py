"""Host-side git never enters a nested repository: the final verification of PR #93 (round 3).

add_all keeps git add -A out of every gitlink without pathspecs, which git add refuses once a
directory above a gitlink is ignored or a symlink, and dirty_submodule_paths reads every
registered submodule whatever its name and path hold (whitespace, quotes, non-ASCII).
"""
try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
    from test_fix_git_attr_shield_r2 import NestedFixture, ShadowDeliveryFixture
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401
    from tests.test_fix_git_attr_shield_r2 import NestedFixture, ShadowDeliveryFixture

import shutil
import unittest
from unittest.mock import patch

import makewand.config as makewand_config
from makewand import candidate, git_helper

# (.gitignore, gitlink path): the layouts git add refused with exclude pathspecs.
IGNORED_LAYOUTS = (
    ("vendor/\n", "vendor/lib"),
    ("*\n!.gitignore\n!app.py\n", ".vim/bundle/plugin"),  # a dotfiles repository
    ("build*\n", "build-deps/lib"),
)


def staged_index(fixture, repo):
    return fixture.git(repo, "ls-files", "-s")


class IgnoredOrLinkedGitlinkTests(NestedFixture):
    """A gitlink below an ignored or symlinked directory is staged as plain git add -A stages it."""

    def ignored_gitlink(self, name, ignore, path, register=True):
        top = self.repository(self.root / name, {"app.py": "print(1)\n", ".gitignore": ignore})
        sub = self.nested_repository(top / path)
        files = [path]
        if register:
            (top / ".gitmodules").write_text(f'[submodule "lib"]\n\tpath = {path}\n\turl = ./{path}\n')
            files.append(".gitmodules")
        self.git(top, "add", "-f", *files)
        self.git(top, "commit", "-qm", "lib")
        return top, sub

    def linked_gitlink(self, name, register=True):
        """a/sub committed as a gitlink, then a turned into a symlink to its old content."""
        top = self.repository(self.root / name, {"app.py": "print(1)\n"})
        self.nested_repository(top / "a" / "sub")
        if register:
            (top / ".gitmodules").write_text('[submodule "s"]\n\tpath = a/sub\n\turl = ./a/sub\n')
        self.git(top, "add", "-A")
        self.git(top, "commit", "-qm", "sub")
        elsewhere = self.root / f"{name}-elsewhere"
        (top / "a").rename(elsewhere)
        (top / "a").symlink_to(elsewhere)
        return top, elsewhere / "sub"

    def assert_stages_as_git_does(self, top, sub):
        """add_all on top (sub armed) leaves the index plain git add -A leaves on a copy."""
        (top / "app.py").write_text("print(2)\n")
        expected = self.root / (top.name + "-expected")
        shutil.copytree(top, expected, symlinks=True)
        self.git(expected, "add", "-A")
        self.arm(sub, sub / ".git")
        code, _, error = git_helper.add_all(str(top))
        self.assertEqual(code, 0, error)
        self.assertEqual(staged_index(self, top), staged_index(self, expected))
        self.assertNotRegex(self.git(top, "ls-files", "-v"), r"(?m)^[a-z] ")  # no flag left behind
        self.assertEqual(self.marker_lines(), [])

    def test_add_all_below_an_ignored_directory(self):
        for index, (ignore, path) in enumerate(IGNORED_LAYOUTS):
            with self.subTest(ignore=ignore):
                top, sub = self.ignored_gitlink(f"top{index}", ignore, path)
                # The submodule moves on: git add -A stages its new HEAD.
                self.git(sub, "commit", "-q", "--allow-empty", "-m", "moved")
                self.assert_stages_as_git_does(top, sub)
                self.assertEqual(self.git(top, "ls-files", "-s", path).split()[1],
                                 self.git(sub, "rev-parse", "HEAD").strip())

    def test_add_all_below_a_symlinked_directory(self):
        # git add -A drops a/sub, which now lies beyond a symbolic link, and stages the link a.
        top, sub = self.linked_gitlink("top")
        self.assert_stages_as_git_does(top, sub)
        self.assertEqual(self.git(top, "ls-files", "--", "a/sub"), "")
        self.assertFalse((sub / ".git" / "info" / "attributes.mw_gate").exists())

    def test_add_all_leaves_a_gitlink_the_user_marked_alone(self):
        top, sub = self.ignored_gitlink("top", "vendor/\n", "vendor/lib")
        self.git(top, "update-index", "--assume-unchanged", "vendor/lib")
        self.git(sub, "commit", "-q", "--allow-empty", "-m", "moved")
        before = self.git(top, "ls-files", "-s", "vendor/lib")
        self.arm(sub, sub / ".git")
        code, _, error = git_helper.add_all(str(top))
        self.assertEqual(code, 0, error)
        self.assertEqual(self.git(top, "ls-files", "-s", "vendor/lib"), before)
        self.assertTrue(self.git(top, "ls-files", "-v", "vendor/lib").startswith("h "))
        self.assertEqual(self.marker_lines(), [])

    def test_review_diff_below_an_ignored_or_symlinked_directory(self):
        layouts = [self.ignored_gitlink(f"top{index}", ignore, path)
                   for index, (ignore, path) in enumerate(IGNORED_LAYOUTS)]
        # git submodule status refuses a registered one (get_submodule_paths fails closed).
        layouts.append(self.linked_gitlink("linked", register=False))
        for top, sub in layouts:
            with self.subTest(top=top.name):
                (top / "app.py").write_text("print(2)\n")
                self.arm(sub, sub / ".git")
                diff, error = git_helper.get_git_diff_status(str(top))
                self.assertIsNone(error)
                self.assertIn("print(2)", diff)
                self.assertNotRegex(self.git(top, "ls-files", "-v"), r"(?m)^[a-z] ")
                self.assertEqual(self.marker_lines(), [])

    def test_shadow_copy_below_an_ignored_directory(self):
        for index, (ignore, path) in enumerate(IGNORED_LAYOUTS):
            with self.subTest(ignore=ignore):
                top, sub = self.ignored_gitlink(f"top{index}", ignore, path)
                (top / "app.py").write_text("print(2)\n")
                self.arm(sub, sub / ".git", dirty=False)
                result = git_helper.create_ephemeral_shadow_worktree(str(top))
                self.assertIsNotNone(result[0])
                self.addCleanup(result[2])
                self.assertEqual(self.marker_lines(), [])

    def test_dag_checkpoint_below_an_ignored_directory(self):
        # The staging copy flattens the user's submodules: here a stage adds one itself, as
        # git submodule add -f does, below a directory the repository ignores.
        from makewand.task_dag import DagStaging
        for index, (ignore, path) in enumerate(IGNORED_LAYOUTS):
            with self.subTest(ignore=ignore):
                top = self.repository(self.root / f"top{index}", {"app.py": "print(1)\n", ".gitignore": ignore})
                with patch.object(makewand_config, "CANDIDATES_DIR", self.root / "candidates"):
                    staging = DagStaging(str(top))
                    self.addCleanup(staging.discard)
                    sub = self.nested_repository(staging.workspace / path)
                    (staging.workspace / ".gitmodules").write_text(f'[submodule "lib"]\n\tpath = {path}\n')
                    self.git(staging.workspace, "add", "-f", path, ".gitmodules")
                    (staging.workspace / "app.py").write_text("print(2)\n")
                    self.arm(sub, sub / ".git", dirty=False)
                    staging.checkpoint("task-1")
                    self.assertEqual(self.git(staging.workspace, "show", "HEAD:app.py"), "print(2)\n")
                    self.assertEqual(self.git(staging.workspace, "ls-files", "-s", path).split()[0], "160000")
                self.assertEqual(self.marker_lines(), [])


class IgnoredGitlinkDeliveryTests(ShadowDeliveryFixture):
    def test_shadow_delivery_below_an_ignored_directory(self):
        (self.base / ".gitignore").write_text("vendor/\n")
        nested = self.nested_repository(self.base / "vendor" / "lib")
        (self.base / ".gitmodules").write_text('[submodule "lib"]\n\tpath = vendor/lib\n\turl = ./vendor/lib\n')
        self.git(self.base, "add", "-f", ".gitignore", ".gitmodules", "vendor/lib")
        self.git(self.base, "commit", "-qm", "lib")
        self.reset_shadow()
        self.sub_baselines = {"vendor/lib": self.git(nested, "rev-parse", "HEAD").strip()}

        def coder(shadow):
            (shadow / "app.txt").write_text("APPROVED\n")
            self.arm(shadow / "vendor" / "lib", shadow / "vendor" / "lib" / ".git", dirty=False)

        self.assertTrue(self.run_pipeline(coder), self.output)
        self.assertEqual(self.apply_delivery().returncode, 0)
        self.assertEqual((self.base / "app.txt").read_text(), "APPROVED\n")
        self.assertEqual(self.marker_lines(), [])


class SubmoduleNameTests(NestedFixture):
    """A registered submodule is read whatever its name and path hold."""

    NAMES = ("my lib", "tab\tlib", "quo\"te's", "lib ü", "  padded  ")

    def named_submodule(self, top_name, name, path):
        top = self.repository(self.root / top_name, {"app.py": "print(1)\n"})
        sub = self.nested_repository(top / path)
        self.git(top, "config", "-f", ".gitmodules", f"submodule.{name}.path", path)
        self.git(top, "config", "-f", ".gitmodules", f"submodule.{name}.url", "./sub")
        self.git(top, "add", "-A")
        self.git(top, "commit", "-qm", "sub")
        return top, sub

    def test_a_dirty_submodule_whose_name_holds_whitespace_is_detected(self):
        top, sub = self.named_submodule("top", "my lib", "my lib")
        self.assertEqual(git_helper.dirty_submodule_paths(str(top)), [])
        self.arm(sub, sub / ".git")
        self.assertEqual(git_helper.dirty_submodule_paths(str(top)), ["my lib"])
        self.assertIn("my lib", git_helper.git_status_paths(str(top)))
        with patch.object(git_helper, "get_active_interactive_working_trees", return_value={}):
            safe, message = git_helper.check_working_tree_isolation(str(top))
        self.assertFalse(safe)
        self.assertIn("Dirty Working Tree", message)
        self.assertEqual(self.marker_lines(), [])

    def test_any_legal_name_and_path(self):
        for index, name in enumerate(self.NAMES):
            path = f"dir {index}/{name.strip()} ü \"q\""
            with self.subTest(name=name):
                top, sub = self.named_submodule(f"top{index}", name, path)
                self.assertIn(path, git_helper.get_submodule_paths(str(top)))
                (sub / "untracked.txt").write_text("u\n")
                self.assertEqual(git_helper.dirty_submodule_paths(str(top)), [path])
                head = self.git(top, "rev-parse", "HEAD").strip()
                self.assertEqual(candidate.get_candidate_files_changed(top, head).get(path), "M")


if __name__ == "__main__":
    unittest.main()
