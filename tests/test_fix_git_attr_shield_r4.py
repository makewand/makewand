"""No workspace-parent Git or configured default merge driver runs on the host."""
try:
    import _isolation  # noqa: F401
    from test_fix_git_attr_shield import FilterFixture
except ImportError:
    from tests import _isolation  # noqa: F401
    from tests.test_fix_git_attr_shield import FilterFixture

import os
import shlex
from unittest.mock import patch

from makewand import delivery, git_helper
from makewand.providers.api_client import apply_agentic_code_output


class ParentGitTests(FilterFixture):
    def workspace(self):
        repo = self.repository(self.root / "repo", {"seed": "seed\n"})
        subdir = repo / "subdir"
        subdir.mkdir()
        (subdir / "f.txt").write_text("one\n")
        self.git(repo, "add", "-A")
        self.git(repo, "commit", "-qm", "subdir")
        binary = repo / "bin" / "git"
        binary.parent.mkdir()
        binary.write_text("#!/bin/sh\necho workspace-git >> " + shlex.quote(str(self.marker)) + "\nexit 0\n")
        binary.chmod(0o755)
        return repo, subdir, binary

    def test_api_from_subdirectory_rejects_parent_workspace_git(self):
        repo, subdir, binary = self.workspace()
        diff = ("--- a/subdir/f.txt\n+++ b/subdir/f.txt\n@@ -1 +1,2 @@\n one\n+two\n")
        with patch.dict(os.environ, {"PATH": str(binary.parent) + os.pathsep + os.environ["PATH"]}), \
                patch.object(git_helper, "_RESOLVED_GIT_BINARY", None):
            modified = apply_agentic_code_output("```diff\n" + diff + "```\n", str(subdir))
        self.assertEqual(self.marker_lines(), [])
        self.assertEqual(modified, ["subdir/f.txt"])
        self.assertEqual((subdir / "f.txt").read_text(), "one\ntwo\n")

    def test_cached_parent_binary_is_refused_from_a_subdirectory(self):
        _, subdir, binary = self.workspace()
        with patch.object(git_helper, "_RESOLVED_GIT_BINARY", str(binary)):
            with self.assertRaises(PermissionError):
                git_helper.resolve_safe_git_binary(cwd=subdir)
        self.assertEqual(self.marker_lines(), [])

    def test_nested_repository_and_outside_alias_cannot_hide_parent_binary(self):
        repo, subdir, binary = self.workspace()
        nested = subdir / "nested"
        nested.mkdir()
        self.git(nested, "init", "-q")
        alias = self.root / "alias"
        alias.symlink_to(binary.parent, target_is_directory=True)
        with patch.dict(os.environ, {"PATH": str(alias) + os.pathsep + os.environ["PATH"]}), \
                patch.object(git_helper, "_RESOLVED_GIT_BINARY", None):
            chosen = git_helper.resolve_safe_git_binary(cwd=nested)
        self.assertFalse(chosen.startswith(str(repo) + os.sep))
        self.assertEqual(self.marker_lines(), [])


class DefaultMergeTests(FilterFixture):
    def merge_fixture(self, theirs):
        repo = self.repository(self.root / "repo", {"f.txt": "base\nsecond\nthird\n"})
        base = self.git(repo, "rev-parse", "HEAD").strip()
        (repo / "f.txt").write_text(theirs)
        diff = self.git(repo, "diff", "--binary", "--full-index")
        self.git(repo, "reset", "--hard", base)
        (repo / "f.txt").write_text("ours\nsecond\nthird\n")
        self.git(repo, "add", "-A")
        self.git(repo, "commit", "-qm", "ours")
        for name in ("audit", "text", "binary", "makewand_builtin_text_0", "makewand_builtin_text_1"):
            self.git(repo, "config", "merge." + name + ".driver",
                     "echo driver >> " + shlex.quote(str(self.marker)) + "; exit 0")
        self.git(repo, "config", "merge.default", "audit")
        return repo, diff

    def test_conflicting_threeway_uses_builtin_conflict_without_running_driver(self):
        repo, diff = self.merge_fixture("theirs\nsecond\nthird\n")
        code, _, error = git_helper.run_git_cmd(["git", "apply", "--3way", "-"], cwd=repo, input_data=diff.encode())
        self.assertNotEqual(code, 0, error)
        self.assertEqual(self.marker_lines(), [])
        self.assertIn("<<<<<<<", (repo / "f.txt").read_text())
        self.assertIn("ours", (repo / "f.txt").read_text())
        self.assertIn("theirs", (repo / "f.txt").read_text())

    def test_nonconflicting_threeway_still_combines_both_changes(self):
        repo, diff = self.merge_fixture("base\nsecond\ntheirs\n")
        code, _, error = git_helper.run_git_cmd(["git", "apply", "--3way", "-"], cwd=repo, input_data=diff.encode())
        self.assertEqual(code, 0, error)
        self.assertEqual(self.marker_lines(), [])
        self.assertEqual((repo / "f.txt").read_text(), "ours\nsecond\ntheirs\n")

    def test_standalone_delivery_selects_a_default_outside_all_custom_names(self):
        repo, _ = self.merge_fixture("theirs\nsecond\nthird\n")
        env = delivery._apply_environment(str(repo))
        values = {env["GIT_CONFIG_KEY_" + str(i)]: env["GIT_CONFIG_VALUE_" + str(i)]
                  for i in range(int(env["GIT_CONFIG_COUNT"]))}
        self.assertEqual(values["merge.default"], "makewand_builtin_text_2")
        self.assertEqual(self.marker_lines(), [])
