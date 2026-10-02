"""
Unit tests for git resilience and shadow tracking.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import os
import uuid
import unittest
import tempfile
import shutil
from pathlib import Path
from makewand.git_helper import ensure_git_worktree, get_git_diff

class TestGitHelper(unittest.TestCase):
    def setUp(self):
        self.test_dir = Path(tempfile.mkdtemp(prefix="makewand_git_test_"))

    def tearDown(self):
        shutil.rmtree(self.test_dir, ignore_errors=True)

    def test_ensure_git_worktree_non_git(self):
        # Fresh non-git directory
        self.assertFalse((self.test_dir / ".git").exists())
        inited = ensure_git_worktree(str(self.test_dir))
        self.assertTrue(inited)
        self.assertTrue((self.test_dir / ".git").exists())

    def test_get_git_diff(self):
        ensure_git_worktree(str(self.test_dir))
        sample_file = self.test_dir / "sample.py"
        sample_file.write_text("print('hello world')\n", encoding="utf-8")
        diff = get_git_diff(str(self.test_dir))
        self.assertIn("sample.py", diff)
        self.assertIn("hello world", diff)

    def test_get_git_diff_deletion(self):
        sample_file = self.test_dir / "original.py"
        sample_file.write_text("print('initial')\n", encoding="utf-8")
        ensure_git_worktree(str(self.test_dir))
        sample_file.unlink()
        diff = get_git_diff(str(self.test_dir))
        self.assertIn("deleted file mode", diff)
        self.assertIn("original.py", diff)

    def test_root_dir_protection(self):
        # Root and /tmp should not be initialized
        self.assertFalse(ensure_git_worktree("/"))
        self.assertFalse(ensure_git_worktree("/tmp"))

    def test_check_working_tree_isolation(self):
        from unittest.mock import patch
        from makewand.git_helper import check_working_tree_isolation, is_protected_production_path

        # 1. Production tree guard
        with patch("makewand.git_helper.get_protected_paths", return_value=[Path("/mock/system/protected_repo")]):
            prod_safe, prod_msg = is_protected_production_path("/mock/system/protected_repo")
            self.assertTrue(prod_safe)
            self.assertIn("生产封印", prod_msg)

            safe, msg = check_working_tree_isolation("/mock/system/protected_repo")
            self.assertFalse(safe)
            self.assertIn("生产保护警报", msg)

        # 2. Multi-Session Overlap Detection (bidirectional)
        fake_active = {
            "/mock/user/active_workspace": {
                "source": "external_terminal",
                "tty": "pts/99",
                "pid": 12345,
                "ai_type": "codex",
                "cwd": "/mock/user/active_workspace"
            }
        }
        with patch("makewand.git_helper.get_active_interactive_working_trees", return_value=fake_active):
            # Target is identical
            safe, msg = check_working_tree_isolation("/mock/user/active_workspace")
            self.assertFalse(safe)
            self.assertIn("pts/99", msg)

            # Target is a subpath of active session
            safe, msg = check_working_tree_isolation("/mock/user/active_workspace/pkg/api")
            self.assertFalse(safe)
            self.assertIn("pts/99", msg)

            # Target is parent path enclosing active session (bidirectional detection)
            safe, msg = check_working_tree_isolation("/mock/user")
            self.assertFalse(safe)
            self.assertIn("pts/99", msg)

            # Target is safe
            safe, msg = check_working_tree_isolation("/tmp/safe_isolated_dir")
            self.assertTrue(safe)
            self.assertIsNone(msg)

        # 3. Dirty Working Tree Guard
        ensure_git_worktree(str(self.test_dir))
        (self.test_dir / "dirty.py").write_text("print('wip')\n", encoding="utf-8")
        safe, msg = check_working_tree_isolation(str(self.test_dir))
        self.assertFalse(safe)
        self.assertIn("Dirty Working Tree", msg)

    def test_create_ephemeral_shadow_worktree(self):
        from makewand.git_helper import create_ephemeral_shadow_worktree, run_git_cmd

        # Setup git repo with baseline
        ensure_git_worktree(str(self.test_dir))
        code_file = self.test_dir / "main.py"
        code_file.write_text("print('baseline')\n", encoding="utf-8")
        run_git_cmd("git add main.py && git commit -m 'commit main.py'", cwd=str(self.test_dir))

        # Add dirty tracked change
        code_file.write_text("print('dirty modification')\n", encoding="utf-8")

        # Add untracked file with space and unicode
        sub_dir = self.test_dir / "pkg"
        sub_dir.mkdir(parents=True, exist_ok=True)
        untracked_file = sub_dir / "测试 文件.txt"
        untracked_file.write_text("untracked content", encoding="utf-8")

        # Create shadow worktree from subdirectory
        wt_dir, branch_name, cleanup = create_ephemeral_shadow_worktree(str(sub_dir), prefix="test_guard")
        try:
            self.assertTrue(Path(wt_dir).exists())
            self.assertTrue(branch_name.startswith("makewand/test_guard_"))

            # Verify subdirectory structure was preserved
            self.assertEqual(Path(wt_dir).name, "pkg")

            # Verify dirty tracked changes were carried forward to shadow repo
            shadow_root = Path(wt_dir).parent
            shadow_main = shadow_root / "main.py"
            self.assertTrue(shadow_main.exists())
            self.assertEqual(shadow_main.read_text(encoding="utf-8"), "print('dirty modification')\n")

            # Verify untracked unicode file was carried forward
            shadow_untracked = Path(wt_dir) / "测试 文件.txt"
            self.assertTrue(shadow_untracked.exists())
            self.assertEqual(shadow_untracked.read_text(encoding="utf-8"), "untracked content")
        finally:
            cleanup()
            self.assertFalse(Path(wt_dir).parent.exists())

    def test_symlink_containment_and_jailbreak_defense(self):
        from makewand.git_helper import create_ephemeral_shadow_worktree, run_git_cmd
        from makewand.sandbox import is_bwrap_available, wrap_bwrap, run_subprocess
        from unittest.mock import patch

        ensure_git_worktree(str(self.test_dir))
        orig_file = self.test_dir / "orig.txt"
        orig_file.write_text("hello original\n", encoding="utf-8")
        run_git_cmd("git add orig.txt && git commit -m 'commit orig.txt'", cwd=str(self.test_dir))

        # 1. Absolute symlink pointing into the repo
        alias_file = self.test_dir / "alias.txt"
        if alias_file.exists() or alias_file.is_symlink():
            alias_file.unlink()
        os.symlink(orig_file.resolve(), alias_file)

        # 2. External symlink pointing outside repo
        ext_tmp = Path(tempfile.gettempdir()) / f"makewand_ext_target_{uuid.uuid4().hex[:6]}.txt"
        ext_tmp.write_text("external pristine\n", encoding="utf-8")
        ext_link = self.test_dir / "ext_link.txt"
        if ext_link.exists() or ext_link.is_symlink():
            ext_link.unlink()
        os.symlink(ext_tmp.resolve(), ext_link)

        try:
            wt_dir, branch_name, cleanup = create_ephemeral_shadow_worktree(str(self.test_dir), prefix="test_symlink")
            try:
                shadow_alias = Path(wt_dir) / "alias.txt"
                self.assertTrue(shadow_alias.exists())

                # Internal absolute symlink must retain pristine target path without /tmp/... rewriting
                self.assertEqual(os.readlink(shadow_alias), str(orig_file.resolve()))

                # External symlink defense: MUST be preserved as a symlink (mode 120000),
                # NEVER converted to regular file (preventing external data leakage into git objects)
                shadow_ext = Path(wt_dir) / "ext_link.txt"
                self.assertTrue(shadow_ext.is_symlink())
                self.assertEqual(os.readlink(shadow_ext), str(ext_tmp.resolve()))
                code, out, _ = run_git_cmd(["git", "ls-files", "-s", "ext_link.txt"], cwd=str(wt_dir))
                self.assertEqual(code, 0)
                self.assertTrue(out.startswith("120000"), f"Expected mode 120000, got: {out}")

                # Sandbox write-through confinement verification:
                # If bwrap is available, executing with wrap_bwrap prevents write-through to ext_tmp!
                if is_bwrap_available():
                    sandboxed_script = """
import sys
try:
    with open("ext_link.txt", "w") as f:
        f.write("corrupted external\\n")
except OSError:
    pass
"""
                    cmd = wrap_bwrap(["python3", "-c", sandboxed_script], workspace=str(wt_dir), repo_root=str(self.test_dir))
                    run_subprocess(cmd)
                    # Host ext_tmp MUST remain untouched!
                    self.assertEqual(ext_tmp.read_text(encoding="utf-8"), "external pristine\n")
            finally:
                cleanup()

            # Fail-closed defense: when bwrap is mocked as unavailable, external symlinks must block shadow creation
            with patch("makewand.sandbox.is_bwrap_available", return_value=False):
                res_blocked = create_ephemeral_shadow_worktree(str(self.test_dir), prefix="test_blocked")
                self.assertIsNone(res_blocked[0], "Expected create_ephemeral_shadow_worktree to fail-closed when bwrap is unavailable")

        finally:
            if ext_tmp.exists():
                ext_tmp.unlink()

    def test_submodule_baseline_recording_clean_and_tracked(self):
        from makewand.git_helper import create_ephemeral_shadow_worktree, run_git_cmd, get_git_diff

        with tempfile.TemporaryDirectory() as base_tmp:
            base_dir = Path(base_tmp)
            main_repo = base_dir / "main_repo"
            main_repo.mkdir()
            run_git_cmd(["git", "init"], cwd=str(main_repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(main_repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(main_repo))
            (main_repo / "README.md").write_text("main repo")
            run_git_cmd(["git", "add", "-A"], cwd=str(main_repo))
            run_git_cmd(["git", "commit", "-m", "init main"], cwd=str(main_repo))

            # Create child submodule repo
            child_repo = base_dir / "child_repo"
            child_repo.mkdir()
            run_git_cmd(["git", "init"], cwd=str(child_repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(child_repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(child_repo))
            (child_repo / "subfile.txt").write_text("sub initial")
            run_git_cmd(["git", "add", "-A"], cwd=str(child_repo))
            run_git_cmd(["git", "commit", "-m", "init sub"], cwd=str(child_repo))

            # Add child as submodule to main_repo
            run_git_cmd(["git", "-c", "protocol.file.allow=always", "submodule", "add", str(child_repo.resolve()), "libs/child"], cwd=str(main_repo))
            run_git_cmd(["git", "commit", "-m", "add submodule"], cwd=str(main_repo))

            # Clean submodule (no untracked files)
            res = create_ephemeral_shadow_worktree(str(main_repo), prefix="test_sub")
            try:
                self.assertIsNotNone(res.effective_dir)
                # Submodule baseline must be recorded even when submodule has NO untracked files!
                self.assertIn("libs/child", res.sub_baselines)
                sub_base = res.sub_baselines["libs/child"]
                self.assertIsNotNone(sub_base)

                # Simulate coder making modifications and committing inside the submodule in shadow worktree
                sub_in_shadow = Path(res.worktree_root) / "libs/child"
                (sub_in_shadow / "subfile.txt").write_text("sub modified by coder")
                run_git_cmd(["git", "add", "-A"], cwd=str(sub_in_shadow))
                run_git_cmd(["git", "commit", "-m", "sub commit in task"], cwd=str(sub_in_shadow))

                # Extract diff with sub_baselines
                diff_out = get_git_diff(res.worktree_root, base_rev=res.baseline_commit, sub_baselines=res.sub_baselines)
                # Code changes inside submodule must appear in the diff!
                self.assertIn("sub modified by coder", diff_out)
            finally:
                res.cleanup()

    def test_shadow_worktree_result_metadata(self):
        from makewand.git_helper import create_ephemeral_shadow_worktree, run_git_cmd

        ensure_git_worktree(str(self.test_dir))
        f = self.test_dir / "version.txt"
        f.write_text("1.0\n", encoding="utf-8")
        run_git_cmd("git add version.txt && git commit -m 'initial version'", cwd=str(self.test_dir))

        # Dirty modification
        f.write_text("1.1-dirty\n", encoding="utf-8")

        res = create_ephemeral_shadow_worktree(str(self.test_dir), prefix="test_meta")
        try:
            # 3-tuple backward compatibility
            self.assertEqual(len(res), 3)
            wt_dir, branch, cleanup = res
            self.assertEqual(wt_dir, res.effective_dir)
            self.assertEqual(branch, res.branch_name)

            # Metadata attributes
            self.assertIsNotNone(res.baseline_commit)
            self.assertIsNotNone(res.repo_head)
            self.assertNotEqual(res.baseline_commit, res.repo_head)
            self.assertEqual(res.repo_root, str(self.test_dir.resolve()))
            self.assertTrue(Path(res.worktree_root).exists())
        finally:
            res.cleanup()

    def test_clone_isolated_worktree_preserves_symlinks(self):
        from makewand.git_helper import clone_isolated_worktree
        with tempfile.TemporaryDirectory() as td:
            src = Path(td) / "src"
            dst = Path(td) / "dst"
            src.mkdir()
            target_file = Path(td) / "external_target.txt"
            target_file.write_text("external content")

            # Create symlink pointing outside src
            link = src / "ext_link"
            os.symlink(str(target_file), link)

            clone_isolated_worktree(str(src), dst)
            dst_link = dst / "ext_link"
            self.assertTrue(dst_link.is_symlink())
            # The destination should be a symlink, NOT a regular file
            self.assertTrue(os.path.islink(dst_link))

    def test_submodule_diff_without_explicit_sub_baselines(self):
        from makewand.git_helper import get_git_diff, run_git_cmd
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            main_repo = root / "main"
            child_repo = root / "child"
            main_repo.mkdir()
            child_repo.mkdir()

            run_git_cmd(["git", "init"], cwd=str(main_repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(main_repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(main_repo))
            (main_repo / "main.txt").write_text("main")
            run_git_cmd(["git", "add", "-A"], cwd=str(main_repo))
            run_git_cmd(["git", "commit", "-m", "init main"], cwd=str(main_repo))

            run_git_cmd(["git", "init"], cwd=str(child_repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(child_repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(child_repo))
            (child_repo / "sub.txt").write_text("sub v1")
            run_git_cmd(["git", "add", "-A"], cwd=str(child_repo))
            run_git_cmd(["git", "commit", "-m", "init child"], cwd=str(child_repo))

            run_git_cmd(["git", "-c", "protocol.file.allow=always", "submodule", "add", str(child_repo.resolve()), "child_sub"], cwd=str(main_repo))
            run_git_cmd(["git", "commit", "-m", "add submodule"], cwd=str(main_repo))

            # Commit a change inside child_sub
            sub_in_main = main_repo / "child_sub"
            (sub_in_main / "sub.txt").write_text("sub v2 committed in child")
            run_git_cmd(["git", "add", "-A"], cwd=str(sub_in_main))
            run_git_cmd(["git", "commit", "-m", "commit inside child"], cwd=str(sub_in_main))

            # get_git_diff with sub_baselines=None (simulating independent review)
            diff_out = get_git_diff(str(main_repo), sub_baselines=None)
            self.assertIn("sub v2 committed in child", diff_out)

    def test_git_info_attributes_shielded_in_linked_worktree(self):
        from makewand.git_helper import run_git_cmd
        with tempfile.TemporaryDirectory(prefix="makewand_git_attr_") as td:
            root = Path(td)
            repo = root / "repo"
            linked = root / "linked"
            marker = root / "clean_marker"
            repo.mkdir()

            run_git_cmd(["git", "init"], cwd=str(repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(repo))
            (repo / "f.txt").write_text("base")
            run_git_cmd(["git", "add", "f.txt"], cwd=str(repo))
            run_git_cmd(["git", "commit", "-m", "init"], cwd=str(repo))

            run_git_cmd(["git", "worktree", "add", "-b", "linked-br", str(linked)], cwd=str(repo))

            # Configure harmful clean filter in main repo
            run_git_cmd(["git", "config", "filter.harmful.clean", f"cat; printf leaked > {marker}"], cwd=str(repo))
            (repo / ".git" / "info" / "attributes").write_text("*.txt filter=harmful\n")

            (linked / "f.txt").write_text("modified")
            rc, out, err = run_git_cmd(["git", "add", "-A"], cwd=str(linked))
            self.assertFalse(marker.exists(), "host clean filter must not execute in linked worktree!")

    def test_get_dirty_files_git_and_fallback(self):
        from makewand.git_helper import get_dirty_files, run_git_cmd
        with tempfile.TemporaryDirectory() as td:
            repo = Path(td) / "repo"
            repo.mkdir()
            run_git_cmd(["git", "init"], cwd=str(repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(repo))
            run_git_cmd(["git", "config", "user.email", "tester@test.local"], cwd=str(repo))

            # Initial commit
            (repo / "tracked.py").write_text("print('v1')\n")
            run_git_cmd(["git", "add", "tracked.py"], cwd=str(repo))
            run_git_cmd(["git", "commit", "-m", "init"], cwd=str(repo))

            # Clean repo: empty dirty files
            self.assertEqual(get_dirty_files(str(repo)), [])

            # Modify tracked, add untracked
            (repo / "tracked.py").write_text("print('v2')\n")
            (repo / "untracked.py").write_text("x = 1\n")
            dirty = get_dirty_files(str(repo))
            self.assertIn("tracked.py", dirty)
            self.assertIn("untracked.py", dirty)

            # Non-git directory fallback
            non_git = Path(td) / "non_git"
            non_git.mkdir()
            (non_git / "script.py").write_text("y = 2\n")
            res_non_git = get_dirty_files(str(non_git))
            self.assertIn("script.py", res_non_git)

    def test_git_info_attributes_shielded_concurrently(self):
        import concurrent.futures
        from makewand.git_helper import run_git_cmd
        with tempfile.TemporaryDirectory() as td:
            repo = Path(td) / "repo"
            marker = Path(td) / "clean_marker"
            repo.mkdir()

            run_git_cmd(["git", "init"], cwd=str(repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(repo))
            (repo / "f.txt").write_text("base")
            run_git_cmd(["git", "add", "f.txt"], cwd=str(repo))
            run_git_cmd(["git", "commit", "-m", "init"], cwd=str(repo))

            run_git_cmd(["git", "config", "filter.harmful.clean", f"cat; printf leaked > {marker}"], cwd=str(repo))
            attr_file = repo / ".git" / "info" / "attributes"
            attr_content = "*.txt filter=harmful\n"
            attr_file.write_text(attr_content)

            def worker(idx):
                # Run git status, diff, or rev-parse
                cmd = ["git", "status"] if idx % 2 == 0 else ["git", "diff"]
                code, out, err = run_git_cmd(cmd, cwd=str(repo))
                return code, err

            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as executor:
                futures = [executor.submit(worker, i) for i in range(16)]
                for f in concurrent.futures.as_completed(futures):
                    code, err = f.result()
                    self.assertEqual(code, 0, f"Git command failed: {err}")

            self.assertFalse(marker.exists(), "host clean filter executed during concurrent operations!")
            self.assertTrue(attr_file.exists(), "attributes file was deleted or not restored!")
            self.assertEqual(attr_file.read_text(), attr_content)

    def test_git_info_attributes_orphan_self_healing(self):
        from makewand.git_helper import run_git_cmd
        with tempfile.TemporaryDirectory() as td:
            repo = Path(td) / "repo"
            repo.mkdir()
            run_git_cmd(["git", "init"], cwd=str(repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(repo))

            info_dir = repo / ".git" / "info"
            info_dir.mkdir(parents=True, exist_ok=True)
            orphan = info_dir / "attributes.makewand_shield_99999_0"
            content = "*.dat filter=special\n"
            orphan.write_text(content)

            code, out, err = run_git_cmd(["git", "status"], cwd=str(repo))
            self.assertEqual(code, 0, f"Git status failed: {err}")

            attr_file = info_dir / "attributes"
            self.assertTrue(attr_file.exists(), "attributes file was not self-healed from orphan!")
            self.assertEqual(attr_file.read_text(), content)
            self.assertFalse(orphan.exists(), "orphan shield file was not cleaned up!")

    def test_git_info_attributes_worktree_deduplication(self):
        from makewand.git_helper import run_git_cmd
        with tempfile.TemporaryDirectory() as td:
            main_repo = Path(td) / "main"
            wt_repo = Path(td) / "worktree"
            main_repo.mkdir()

            run_git_cmd(["git", "init"], cwd=str(main_repo))
            run_git_cmd(["git", "config", "user.name", "Tester"], cwd=str(main_repo))
            run_git_cmd(["git", "config", "user.email", "test@test.local"], cwd=str(main_repo))
            (main_repo / "f.txt").write_text("base")
            run_git_cmd(["git", "add", "f.txt"], cwd=str(main_repo))
            run_git_cmd(["git", "commit", "-m", "init"], cwd=str(main_repo))

            run_git_cmd(["git", "worktree", "add", str(wt_repo), "-b", "wt-branch"], cwd=str(main_repo))

            main_info = main_repo / ".git" / "info"
            main_info.mkdir(parents=True, exist_ok=True)
            main_attr = main_info / "attributes"
            content = "*.txt -text\n"
            main_attr.write_text(content)

            # In linked worktree, run_git_cmd must succeed without deadlocks or errors
            code, out, err = run_git_cmd(["git", "status"], cwd=str(wt_repo))
            self.assertEqual(code, 0, f"worktree git status failed: {err}")
            self.assertTrue(main_attr.exists())
            self.assertEqual(main_attr.read_text(), content)

    def test_reflink_copy_where_supported(self):
        from makewand.git_helper import _is_reflink_supported, _copy_file_with_reflink
        with tempfile.TemporaryDirectory() as td:
            src = Path(td) / "src.txt"
            dst = Path(td) / "dst.txt"
            src.write_text("reflink content verification")
            _copy_file_with_reflink(src, dst)
            self.assertTrue(dst.exists())
            self.assertEqual(dst.read_text(), "reflink content verification")
            # Probe reflink support boolean doesn't crash and caches result
            supported = _is_reflink_supported()
            self.assertIsInstance(supported, bool)

    def test_reflink_copy_fallback_on_readonly_file_and_cross_device(self):
        from makewand.git_helper import _copy_file_with_reflink
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as td:
            src = Path(td) / "ro.txt"
            dst = Path(td) / "dst_ro.txt"
            src.write_text("read only content")
            os.chmod(src, 0o444)

            # Simulate cross-device link failure in copy_file_range
            with patch("os.copy_file_range", side_effect=OSError(18, "Invalid cross-device link")):
                _copy_file_with_reflink(src, dst)
                self.assertTrue(dst.exists())
                self.assertEqual(dst.read_text(), "read only content")
                self.assertEqual(dst.stat().st_mode & 0o777, 0o444)

    def test_sanitize_git_env(self):
        from makewand.git_helper import _sanitize_git_env
        input_env = {
            "PATH": "/usr/bin:/bin",
            "USER": "tester",
            "GIT_DIR": "/evil/dir",
            "GIT_WORK_TREE": "/evil/wt",
            "GIT_INDEX_FILE": "/evil/index",
            "GIT_COMMON_DIR": "/evil/common",
            "GIT_HOOKS_PATH": "/evil/hooks",
            "GIT_EXEC_PATH": "/evil/libexec",
            "GIT_EXTERNAL_DIFF": "/evil/diff",
            "GIT_DIFF_OPTS": "-u",
            "GIT_PAGER": "cat",
            "GIT_EDITOR": "/evil/editor",
            "GIT_SEQUENCE_EDITOR": "/evil/seq_editor",
            "GIT_SSH": "evil_ssh",
            "GIT_SSH_COMMAND": "evil_ssh_cmd",
            "GIT_ASKPASS": "evil_askpass",
            "GIT_CONFIG_COUNT": "1",
            "GIT_CONFIG_KEY_0": "core.fsmonitor",
            "GIT_CONFIG_VALUE_0": "evil.sh",
            "GIT_OPTIONAL_LOCKS": "1",
        }
        sanitized = _sanitize_git_env(input_env)
        self.assertEqual(sanitized["PATH"], "/usr/bin:/bin")
        self.assertEqual(sanitized["USER"], "tester")
        self.assertEqual(sanitized["GIT_OPTIONAL_LOCKS"], "0")
        for k in input_env:
            if k not in ("PATH", "USER", "GIT_OPTIONAL_LOCKS"):
                self.assertNotIn(k, sanitized, f"Dangerous var {k} should have been filtered")


if __name__ == "__main__":
    unittest.main()

