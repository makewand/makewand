"""Host-side git never runs a repository's filter: the cross-vendor review of PR #93 (round 2).

Every fixture plants a filter driver whose commands append to a marker file outside the
repositories (FilterFixture): a marker line means makewand ran the filter on the host.
Covered: nested repositories and submodules (no host git command runs `git status` inside
one, the host rollback, the shadow copy's submodule checkout and the shadow delivery's
submodule commits included, while a dirty registered submodule is still detected where it
was before), the delivery patch check and the generated apply script, SHA-256 repositories,
global options that would point git at another repository, and the inherited environment.
"""
try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation
    from _verdicts import nonced
    from test_fix_git_attr_shield import PATCH, FilterFixture
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation
    from tests._verdicts import nonced
    from tests.test_fix_git_attr_shield import PATCH, FilterFixture

import contextlib
import hashlib
import io
import json
import os
import shlex
import shutil
import struct
import subprocess
import unittest
from pathlib import Path
from unittest.mock import patch

import makewand.config as makewand_config
import makewand.orchestrator as orch
from makewand import git_helper

SHA1_EMPTY_TREE = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
SHA256_EMPTY_TREE = "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321"
UTF8_BOM = b"\xef\xbb\xbf"
COMPLETED = subprocess.CompletedProcess([], 0, "", "")


def write_gitlink_index(path, gitlinks):
    """A version 2 index of gitlinks (path -> commit id) written byte by byte: git itself never
    writes a path that is absolute or holds "..", but reads one from a planted index."""
    body = b"DIRC" + struct.pack(">II", 2, len(gitlinks))
    for name, oid in sorted(gitlinks.items()):
        encoded = os.fsencode(name)
        entry = (struct.pack(">10I", 0, 0, 0, 0, 0, 0, 0o160000, 0, 0, 0) + bytes.fromhex(oid)
                 + struct.pack(">H", len(encoded)) + encoded)
        body += entry + b"\0" * (8 - len(entry) % 8)
    path.write_bytes(body + hashlib.sha1(body).digest())


class NestedFixture(FilterFixture):
    def arm(self, worktree, git_dir, name="x.txt", dirty=True):
        """Plant the filter in a nested repository: its config defines it, its info/attributes
        selects it for every path. dirty rewrites a tracked file at the same size with a newer
        mtime, so any git status there must hash it through the filter."""
        self.plant_filter(git_dir / "config")
        self.plant_attributes(git_dir)
        if dirty:
            tracked = worktree / name
            tracked.write_text(tracked.read_text().swapcase())
            future = os.stat(tracked).st_mtime + 3600
            os.utime(tracked, (future, future))

    def nested_repository(self, path, files=None):
        path.mkdir(parents=True, exist_ok=True)
        self.git(path, "init", "-q", "-b", "main")
        for name, text in (files or {"x.txt": "x\n"}).items():
            (path / name).write_text(text)
        self.git(path, "add", "-A")
        self.git(path, "commit", "-qm", "nested")
        return path

    def committed_submodule(self, name="top", register=True, ignore=None, dirty=True):
        """Layout A: a repository commits a gitlink to a nested repository with its own .git
        directory (registered in .gitmodules unless register is False) and the filter planted."""
        top = self.root / name
        top.mkdir()
        self.git(top, "init", "-q", "-b", "main")
        (top / "app.py").write_text("print(1)\n")
        sub = self.nested_repository(top / "sub")
        if register:
            (top / ".gitmodules").write_text('[submodule "sub"]\n\tpath = sub\n\turl = ./sub\n'
                                             + (f"\tignore = {ignore}\n" if ignore else ""))
        self.git(top, "add", "-A")
        self.git(top, "commit", "-qm", "init")
        self.arm(sub, sub / ".git", dirty=dirty)
        return top

    def global_config(self, **settings):
        """A user's global configuration ($HOME/.gitconfig) for the duration of the test."""
        home = self.root / "home"
        home.mkdir(exist_ok=True)
        for key, value in settings.items():
            self.git(self.root, "config", "-f", str(home / ".gitconfig"), key.replace("__", "."), value)
        patcher = patch.dict(os.environ, {"HOME": str(home), "XDG_CONFIG_HOME": str(home / "xdg")})
        patcher.start()
        self.addCleanup(patcher.stop)
        return home


class NestedRepositoryTests(NestedFixture):
    """No host git command starts a command inside a nested repository (layouts A, B, C)."""

    def test_review_diff_never_enters_a_committed_submodule(self):
        # Layout A: git add -A refreshing the private index ran `git status` in sub.
        top = self.committed_submodule()
        (top / "app.py").write_text("print(2)\n")
        diff, error = git_helper.get_git_diff_status(str(top))
        self.assertIsNone(error)
        self.assertIn("print(2)", diff)
        self.assertIn("[Submodule: sub]", diff)  # its own change is still reviewed, shielded
        self.assertIn("+X", diff)
        self.assertEqual(self.marker_lines(), [])

    def test_review_diff_never_enters_a_gitlink_of_a_planted_submodule(self):
        # Layout B: the sandbox cannot write top/.git, but plants .gitmodules, a repository
        # sub whose HEAD commits a gitlink, and the nested repository it points to.
        top = self.repository(self.root / "top", {"app.py": "print(1)\n"})
        sub = top / "sub"
        sub.mkdir()
        self.git(sub, "init", "-q", "-b", "main")
        nested = self.nested_repository(sub / "nested")
        (sub / "s.txt").write_text("s\n")
        self.git(sub, "add", "-A")
        self.git(sub, "commit", "-qm", "s")
        (top / ".gitmodules").write_text('[submodule "sub"]\n\tpath = sub\n\turl = ./sub\n')
        self.arm(nested, nested / ".git")
        diff, error = git_helper.get_git_diff_status(str(top))
        self.assertIsNone(error)
        self.assertEqual(self.marker_lines(), [])

    def test_status_never_enters_a_gitlink(self):
        # A .gitmodules "ignore = none" overrides diff.ignoreSubmodules, not --ignore-submodules.
        for index, (register, ignore) in enumerate(((False, None), (True, None), (True, "none"))):
            with self.subTest(register=register, ignore=ignore):
                self.marker.unlink(missing_ok=True)
                top = self.committed_submodule(f"top{index}", register=register, ignore=ignore)
                code, _, error = git_helper.run_git_cmd(["git", "status", "--porcelain"], cwd=str(top))
                self.assertEqual(code, 0, error)
                self.assertEqual(self.marker_lines(), [])

    def test_add_all_stages_what_git_add_all_stages_without_entering_a_gitlink(self):
        top = self.committed_submodule()
        sub = top / "sub"
        gone = self.nested_repository(top / "gone")
        self.git(top, "add", "-A")
        self.git(top, "commit", "-qm", "gone")
        # The submodule moves on (committed without running the planted driver) and stays dirty.
        self.git(sub, "-c", "filter.evil.clean=cat", "-c", "filter.evil.smudge=cat", "commit", "-qam", "moved")
        self.arm(sub, sub / ".git")
        shutil.rmtree(gone)
        (top / "app.py").write_text("print(2)\n")
        (top / "new.txt").write_text("new\n")
        self.marker.unlink(missing_ok=True)
        code, _, error = git_helper.add_all(str(top))
        self.assertEqual(code, 0, error)
        self.assertEqual(self.marker_lines(), [])
        staged = self.git(top, "diff", "--cached", "--name-status", "--no-renames").split("\n")
        self.assertEqual(sorted(line for line in staged if line), ["A\tnew.txt", "D\tgone", "M\tapp.py", "M\tsub"])
        self.assertEqual(self.git(top, "ls-files", "-s", "sub").split()[1], self.git(sub, "rev-parse", "HEAD").strip())

    def test_add_all_intent_to_add_leaves_gitlinks_to_the_diff(self):
        top = self.committed_submodule()
        (top / "new.txt").write_text("new\n")
        code, _, error = git_helper.add_all(str(top), intent_to_add=True)
        self.assertEqual(code, 0, error)
        self.assertEqual(self.marker_lines(), [])
        self.assertIn("new.txt", self.git(top, "diff", "--name-only", "--ignore-submodules=dirty"))

    def test_every_hardened_command_carries_the_submodule_barrier(self):
        repo = self.repository(self.root / "repo", {"f.txt": "one\n"})
        with patch.object(git_helper.subprocess, "run", return_value=COMPLETED) as run:
            git_helper.run_git_cmd(["git", "status", "--porcelain"], cwd=str(repo))
        executed, env = run.call_args.args[0], run.call_args.kwargs["env"]
        settings = {env[f"GIT_CONFIG_KEY_{i}"]: env[f"GIT_CONFIG_VALUE_{i}"] for i in range(int(env["GIT_CONFIG_COUNT"]))}
        self.assertEqual(settings, {"diff.ignoreSubmodules": "dirty", "submodule.recurse": "false",
                                    "fetch.recurseSubmodules": "false", "push.recurseSubmodules": "no"})
        self.assertIn("--ignore-submodules=dirty", executed[executed.index("status"):])

    def test_a_users_submodule_recurse_never_enters_a_submodule(self):
        # submodule.recurse=true in a user's global configuration makes reset --hard and
        # checkout run read-tree inside each submodule, where its own smudge filter runs.
        origin = self.repository(self.root / "origin", {"x.txt": "x\n"})
        top = self.repository(self.root / "top", {"app.py": "print(1)\n"})
        self.git(top, "-c", "protocol.file.allow=always", "submodule", "add", "-q", str(origin), "sub")
        self.git(top, "commit", "-qm", "sub")
        sub, gitdir = top / "sub", top / ".git" / "modules" / "sub"
        (sub / "x.txt").write_text("y\n")
        self.git(sub, "commit", "-qam", "moved")  # the superproject's reset moves it back
        self.arm(sub, gitdir, dirty=False)
        self.global_config(submodule__recurse="true")
        for command in (["git", "reset", "--hard", "-q", "HEAD"], ["git", "checkout", "-q", "--", "."]):
            with self.subTest(command=command):
                code, _, error = git_helper.run_git_cmd(command, cwd=str(top))
                self.assertEqual(code, 0, error)
                self.assertEqual(self.marker_lines(), [])


class ObjectFormatTests(NestedFixture):
    """The empty-tree attribute source is the repository's own object id, never a ref name."""

    def sha256_repository(self, name="s256"):
        repo = self.root / name
        repo.mkdir()
        self.git(repo, "init", "-q", "--object-format=sha256", "-b", "main")
        return repo

    def test_sha256_repository_works(self):
        repo = self.sha256_repository()
        (repo / "f.txt").write_text("one\n")
        diff, error = git_helper.get_git_diff_status(str(repo))  # unborn: diffs against its own empty tree
        self.assertIsNone(error)
        self.assertIn("+one", diff)
        self.assertEqual(git_helper.empty_tree_oid(str(repo)), SHA256_EMPTY_TREE)
        self.git(repo, "add", "-A")
        self.git(repo, "commit", "-qm", "c")
        (repo / "f.txt").write_text("one\nchanged\n")
        diff, error = git_helper.get_git_diff_status(str(repo))
        self.assertIsNone(error)
        self.assertIn("+changed", diff)
        code, out, error = git_helper.run_git_cmd(["git", "status", "--porcelain"], cwd=str(repo))
        self.assertEqual((code, out.strip()), (0, "M f.txt"), error)
        self.git(repo, "checkout", "--", "f.txt")
        for options in (("--index",), ()):
            code, _, error = git_helper.run_git_cmd(["git", "apply", *options, "-"], cwd=str(repo), input_data=PATCH.encode())
            self.assertEqual(code, 0, error)
            self.git(repo, "reset", "-q", "--hard")

    def test_a_ref_named_like_the_sha1_empty_tree_selects_no_filter(self):
        # In a SHA-256 repository the 40-hex SHA-1 id is no object id: git resolved it as
        # this branch, whose tree selects the filter for every path.
        repo = self.sha256_repository()
        (repo / "f.txt").write_text("one\n")
        (repo / ".gitattributes").write_text("* filter=evil\n")
        self.git(repo, "add", "-A")
        evil_tree = self.git(repo, "write-tree").strip()
        self.git(repo, "rm", "-q", "--cached", ".gitattributes")
        (repo / ".gitattributes").unlink()
        self.git(repo, "commit", "-qm", "c")
        evil = self.git(repo, "commit-tree", evil_tree, "-m", "evil").strip()
        self.git(repo, "update-ref", f"refs/heads/{SHA1_EMPTY_TREE}", evil)
        self.arm(repo, repo / ".git", name="f.txt", dirty=True)
        (repo / ".git" / "info" / "attributes").unlink()  # only the ref selects it
        diff, error = git_helper.get_git_diff_status(str(repo))
        self.assertIsNone(error)
        self.assertEqual(self.marker_lines(), [])

    def test_sha1_repository_is_unchanged(self):
        repo = self.repository(self.root / "s1", {"f.txt": "one\n"})
        self.assertEqual(git_helper.empty_tree_oid(str(repo)), SHA1_EMPTY_TREE)
        with patch.object(git_helper.subprocess, "run", return_value=COMPLETED) as run:
            git_helper.run_git_cmd(["git", "diff", "HEAD"], cwd=str(repo))
        self.assertIn(f"--attr-source={SHA1_EMPTY_TREE}", run.call_args.args[0])
        # A ref of that name loses to the object id (git warns that it is ambiguous).
        (repo / ".gitattributes").write_text("* filter=evil\n")
        self.git(repo, "add", ".gitattributes")
        evil = self.git(repo, "commit-tree", self.git(repo, "write-tree").strip(), "-m", "evil").strip()
        self.git(repo, "rm", "-q", "--cached", ".gitattributes")
        (repo / ".gitattributes").unlink()
        self.git(repo, "update-ref", f"refs/heads/{SHA1_EMPTY_TREE}", evil)
        self.plant_filter(repo / ".git" / "config")
        (repo / "f.txt").write_text("ONE\n")
        diff, error = git_helper.get_git_diff_status(str(repo))
        self.assertIsNone(error)
        self.assertIn("+ONE", diff)
        self.assertEqual(self.marker_lines(), [])

    def test_object_format_is_read_as_git_reads_it(self):
        # Each config, prefixed by repositoryformatversion 1, against git's own reading of it.
        cases = [
            b"[extensions]\n\tobjectFormat = sha256\n",
            b"[Extensions]\nOBJECTFORMAT=sha256\n",
            b'[extensions]\n\tobjectformat = "sha256"\n',
            b"[extensions]\n\tobjectformat = sha256 ; a comment\n",
            b"[extensions]\n\tobjectformat = sha256# a comment\n",
            b"[extensions]\n\tobjectformat = sha2\\\n56\n",
            b"[extensions]\r\n\tobjectformat = sha256\r\n",
            UTF8_BOM + b"[extensions]\n\tobjectformat = sha256\n",
            b"[extensions]\n" + UTF8_BOM + b"objectformat = sha256\n",
            b"[extensions]\n\tobjectformat = sha256\n\tobjectformat = sha1\n",
            b"[extensions]\n\tobjectformat = sha1\n[extensions]\n\tobjectformat = sha256\n",
            b'[extensions "x"]\n\tobjectformat = sha256\n',
            b"[extensions.x]\n\tobjectformat = sha256\n",
            b"[include]\n\tpath = other\n",
            b"# [extensions]\n; objectformat = sha256\n",
            b"[extensions]objectformat = sha256\n",
            b"[extensions] objectformat\t=\tsha256\t \n",
            b"[extensions]\n\tobjectformat = sha256\x00junk\n",
            b'[extensions]\n\tobjectformat = sha""256\n',
            b"[extensions]\n\tobjectformat = sha256\r\r\n",
            b"[extensions]\n\tobjectformat = sha256\\",
            b"[extensions]\n\tobjectformat = sha256",
            b"[extensions]\n\tobjectformat\n",
            b"[extensions]\n\tobjectformat = sha512\n",
            b"[extensions]\n\tobjectformat = SHA256\n",
            b'[extensions]\n\tobjectformat = "sha256\n',
            b'[extensions]\n\tobjectformat = "sha" "256"\n',
            b"[extensions]\n\tobjectformat = s\\ha256\n",
            b"[extensions\n\tobjectformat = sha256\n",
            b"[extensions]\n\t-objectformat = sha256\n",
            b"[extensions]\n\tobject_format = sha256\n",
            b"\xef\xbb[extensions]\n\tobjectformat = sha256\n",
            b"objectformat = sha256\n",
            b'[ "x"]\n\tobjectformat = sha256\n',
            b"[]\n",
        ]
        env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
        accepted = 0
        for index, case in enumerate(cases):
            with self.subTest(config=case):
                git_dir = self.root / f"fmt{index}" / ".git"
                for directory in ("objects", "refs"):
                    (git_dir / directory).mkdir(parents=True)
                (git_dir / "HEAD").write_text("ref: refs/heads/main\n")
                config = git_dir / "config"
                version = b"[core]\n\trepositoryformatversion = 1\n"
                bom = len(UTF8_BOM) if case.startswith(UTF8_BOM) else 0
                config.write_bytes(case[:bom] + version + case[bom:])
                # The parser reads every value git's config reader reads, and fails where it fails.
                listing = subprocess.run(["git", "config", "--file", str(config), "--null", "--get-all",
                                          "extensions.objectformat"], env=env, capture_output=True)
                try:
                    values = [(value or b"").split(b"\0", 1)[0] for name, value
                              in git_helper._git_config_entries(config.read_bytes()) if name == b"extensions.objectformat"]
                except ValueError:
                    values = None
                if listing.returncode in (0, 1):
                    self.assertEqual(values, listing.stdout.split(b"\0")[:-1], listing.stderr)
                else:
                    self.assertIsNone(values, listing.stderr)
                # And the object format is git's wherever git accepts the repository; where git
                # refuses it, every git command fails whatever attribute source makewand names.
                oracle = subprocess.run(["git", "rev-parse", "--show-object-format"], cwd=str(git_dir.parent),
                                        env=env, capture_output=True, text=True)
                if oracle.returncode == 0:
                    accepted += 1
                    self.assertEqual(git_helper._object_format(git_dir), oracle.stdout.strip())
        self.assertGreater(accepted, 15)

    def test_an_object_format_git_cannot_read_refuses_to_start_git(self):
        repo = self.repository(self.root / "repo", {"f.txt": "one\n"})
        self.git(repo, "config", "core.repositoryformatversion", "1")
        self.git(repo, "config", "extensions.objectFormat", "sha512")
        with patch.object(git_helper.subprocess, "run") as run:
            code, _, error = git_helper.run_git_cmd(["git", "status"], cwd=str(repo))
        self.assertNotEqual(code, 0)
        self.assertIn("Cannot shield Git attributes", error)
        run.assert_not_called()


class GlobalOptionTests(NestedFixture):
    """run_git_cmd shields the repository its cwd names; a caller cannot point git elsewhere."""

    def test_options_that_point_git_elsewhere_are_refused(self):
        repo = self.repository(self.root / "repo", {"f.txt": "one\n"})
        other = str(self.root / "other")
        refused = [
            ["git", "-C", other, "status"], ["git", "--git-dir", other, "status"],
            ["git", f"--git-dir={other}", "status"], ["git", "--work-tree", other, "status"],
            ["git", f"--work-tree={other}", "status"], ["git", "--namespace", "n", "status"],
            ["git", "--namespace=n", "status"], ["git", "--bare", "status"],
            ["git", "-c", "include.path=/x", "status"], ["git", "-c", "Include.Path=/x", "status"],
            ["git", "-c", " include.path=/x", "status"],
            ["git", "-c", "includeIf.gitdir:/x/.path=/y", "status"], ["git", "-c", "INCLUDEIF.onbranch:m.path=/y", "status"],
            ["git", "--config-env", "core.x=HOME", "status"], ["git", "--config-env=core.x=HOME", "status"],
            ["git", "--attr-source", "HEAD", "status"], ["git", "--attr-source=HEAD", "status"],
            ["git", "--exec-path=/x", "status"], f"git -C {shlex.quote(other)} status",
        ]
        for command in refused:
            with self.subTest(command=command), patch.object(git_helper.subprocess, "run") as run:
                code, _, error = git_helper.run_git_cmd(command, cwd=str(repo))
                self.assertEqual(code, -1)
                self.assertIn("cwd", error)
                run.assert_not_called()
        allowed = [["git", "-c", "user.name=x", "status"], ["git", "--no-replace-objects", "--literal-pathspecs", "status"],
                   ["git", "commit", "-C", "HEAD", "--dry-run"], ["git", "-c", "core.includeIf=x", "status"]]
        for command in allowed:
            with self.subTest(command=command), patch.object(git_helper.subprocess, "run", return_value=COMPLETED) as run:
                git_helper.run_git_cmd(command, cwd=str(repo))
                run.assert_called()

    def test_the_subcommand_is_found_behind_options_with_values(self):
        for command, index in ((["git", "-c", "a=b", "status"], 3), (["git", "--shallow-file", "x", "log"], 3),
                               (["git", "--no-pager", "-c", "a=b", "--literal-pathspecs", "diff"], 5),
                               (["git", "--attr-source", "x", "diff"], 3), (["git", "--config-env", "a=B", "add"], 3)):
            with self.subTest(command=command):
                self.assertEqual(git_helper._git_subcommand_index(command), index)

    def test_apply_cannot_be_pointed_at_another_repository(self):
        # The filter is selected by target's info/attributes, which --attr-source does not
        # override and which a shield of cwd's repository never reaches. Without --index, git
        # 2.55 crashes on "--attr-source=<oid> -C <dir> apply" before reading the patch.
        target = self.repository(self.root / "target", {"f.txt": "one\n"})
        self.plant_filter(target / ".git" / "config")
        self.plant_attributes(target / ".git")
        other = self.root / "other"
        other.mkdir()
        for command in (["git", "-C", str(target), "apply", "--index", "-"], ["git", "-C", str(target), "apply", "-"]):
            with self.subTest(command=command):
                code, _, error = git_helper.run_git_cmd(command, cwd=str(other), input_data=PATCH.encode())
                self.assertNotEqual(code, 0)
                self.assertEqual((target / "f.txt").read_text(), "one\n")
                self.assertEqual(self.marker_lines(), [])

    def test_a_clone_never_checks_out_and_takes_no_attribute_source(self):
        for command in (["git", "clone", "--shared", "a", "b"], ["git", "-c", "protocol.file.allow=always", "clone", "a", "b"]):
            with self.subTest(command=command), patch.object(git_helper.subprocess, "run", return_value=COMPLETED) as run:
                git_helper.run_git_cmd(command, cwd=str(self.root))
                executed = run.call_args.args[0]
                self.assertEqual(executed[executed.index("clone") + 1], "--no-checkout")
                self.assertFalse([argument for argument in executed if argument.startswith("--attr-source")])


class EnvironmentTests(NestedFixture):
    def test_attribute_source_and_discovery_variables_are_dropped(self):
        inherited = {"GIT_ATTR_SOURCE": "HEAD", "GIT_CEILING_DIRECTORIES": "/", "GIT_NAMESPACE": "n",
                     "GIT_DISCOVERY_ACROSS_FILESYSTEM": "1", "PATH": "/usr/bin"}
        clean = git_helper._sanitize_git_env(inherited)
        self.assertEqual({key for key in inherited if key in clean}, {"PATH"})

    def test_an_inherited_attribute_source_does_not_break_apply(self):
        # git 2.55 segfaults on work-tree apply under any attribute source.
        repo = self.repository(self.root / "repo", {"f.txt": "one\n"})
        with patch.dict(os.environ, {"GIT_ATTR_SOURCE": SHA1_EMPTY_TREE}):
            code, _, error = git_helper.run_git_cmd(["git", "apply", "-"], cwd=str(repo), input_data=PATCH.encode())
        self.assertEqual(code, 0, error)
        self.assertEqual((repo / "f.txt").read_text(), "one\ntwo\n")

    def test_an_inherited_git_config_hides_no_filter_driver(self):
        # git config reads only $GIT_CONFIG, like --file; git apply reads the repository's
        # configuration all the same. The driver listing must see what apply would run.
        repo = self.repository(self.root / "repo", {"f.txt": "one\n", ".gitattributes": "*.txt filter=evil\n"})
        self.plant_filter(repo / ".git" / "config")
        with patch.dict(os.environ, {"GIT_CONFIG": os.devnull}):
            code, _, error = git_helper.run_git_cmd(["git", "apply", "-"], cwd=str(repo), input_data=PATCH.encode())
        self.assertEqual(code, 0, error)
        self.assertEqual(self.marker_lines(), [])
        self.assertEqual((repo / "f.txt").read_text(), "one\ntwo\n")
        self.assertNotIn("GIT_CONFIG", git_helper._sanitize_git_env({"GIT_CONFIG": os.devnull}))

    def test_filter_drivers_are_listed_inside_the_shield(self):
        # One shield from the driver listing until git apply exits: info/attributes never
        # comes back in between, as it did around a separate listing command.
        repo = self.repository(self.root / "repo", {"f.txt": "one\n", ".gitattributes": "*.txt filter=evil\n"})
        self.plant_filter(repo / ".git" / "config")
        self.plant_attributes(repo / ".git")
        moves = []
        rename = Path.rename

        def recording(source, target):
            moves.append((Path(source).name, Path(target).name))
            return rename(source, target)

        with patch.object(Path, "rename", recording):
            code, _, error = git_helper.run_git_cmd(["git", "apply", "-"], cwd=str(repo), input_data=PATCH.encode())
        self.assertEqual(code, 0, error)
        self.assertEqual(len([move for move in moves if "attributes" in move[0]]), 2, moves)
        self.assertEqual(self.marker_lines(), [])
        self.assertEqual((repo / ".git" / "info" / "attributes").read_text(), "* filter=evil\n")


if __name__ == "__main__":
    unittest.main()
