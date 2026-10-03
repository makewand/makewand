"""
Unit tests for search guardrail.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import os
import json
import unittest
import tempfile
import subprocess
from pathlib import Path
from makewand.search import safe_search

class TestSearchGuardrail(unittest.TestCase):
    def test_safe_search_excludes_heavy_dirs_and_bins(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            # Create valid text file
            code_dir = root / "src"
            code_dir.mkdir()
            (code_dir / "app.py").write_text("def find_me():\n    return 'secret_key_123'\n")

            # Create data dir that should be excluded
            data_dir = root / "data"
            data_dir.mkdir()
            (data_dir / "leak.txt").write_text("secret_key_123 in data\n")

            # Create binary ext that should be excluded
            (code_dir / "cache.sqlite").write_text("secret_key_123 in sqlite\n")

            results = safe_search("secret_key_123", root_path=tmpdir)
            self.assertEqual(len(results), 1)
            self.assertEqual(results[0]["file"], os.path.join("src", "app.py"))
            self.assertEqual(results[0]["line_num"], 2)

    def test_safe_search_fallback_when_ripgrep_unavailable(self):
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            code_dir = root / "src"
            code_dir.mkdir()
            (code_dir / "foo.py").write_text("token_xyz_456\n")

            with patch("makewand.search.find_ripgrep", return_value=None):
                results = safe_search("token_xyz_456", root_path=tmpdir)
                self.assertEqual(len(results), 1)
                self.assertEqual(results[0]["file"], os.path.join("src", "foo.py"))
                self.assertEqual(results[0]["line_num"], 1)

    def test_safe_search_ripgrep_handles_regex_and_special_chars(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            (root / "sample.txt").write_text("Hello [world] (123)\n")

            results = safe_search("[world]", root_path=tmpdir)
            self.assertEqual(len(results), 1)
            self.assertEqual(results[0]["file"], "sample.txt")

    def test_safe_search_searches_hidden_config_files_and_ignores_git(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            (root / ".env").write_text("TARGET_API_KEY=abcd1234efgh\n")
            git_dir = root / ".git"
            git_dir.mkdir()
            (git_dir / "config").write_text("TARGET_API_KEY=abcd1234efgh\n")
            venv_dir = root / ".venv"
            venv_dir.mkdir()
            (venv_dir / "lib.py").write_text("TARGET_API_KEY=abcd1234efgh\n")

            results = safe_search("TARGET_API_KEY", root_path=tmpdir)
            files = [r["file"] for r in results]
            self.assertIn(".env", files)
            self.assertNotIn(os.path.join(".git", "config"), files)
            self.assertNotIn(os.path.join(".venv", "lib.py"), files)

    def test_safe_search_skips_fifos_safely(self):
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            code_dir = root / "src"
            code_dir.mkdir()
            (code_dir / "valid.py").write_text("find_fifo_test_val\n")
            fifo_path = code_dir / "pipe.fifo"
            try:
                os.mkfifo(str(fifo_path))
            except (AttributeError, OSError):
                self.skipTest("FIFO creation not supported on this platform")

            with patch("makewand.search.find_ripgrep", return_value=None):
                results = safe_search("find_fifo_test_val", root_path=tmpdir)
                self.assertEqual(len(results), 1)
                self.assertEqual(results[0]["file"], os.path.join("src", "valid.py"))

    def test_safe_search_skips_symlinks_safely(self):
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            code_dir = root / "src"
            code_dir.mkdir()
            target_file = root / "target.txt"
            target_file.write_text("symlink_token_secret\n")

            # Create symlink inside code_dir pointing to target_file
            link_path = code_dir / "link.txt"
            try:
                link_path.symlink_to(target_file)
            except (AttributeError, OSError):
                self.skipTest("Symlinks not supported on this platform")

            # With rg disabled (pure python walk fallback)
            with patch("makewand.search.find_ripgrep", return_value=None):
                results = safe_search("symlink_token_secret", root_path=str(code_dir))
                # Must skip the symlink inside code_dir
                self.assertEqual(len(results), 0)

    def test_safe_search_rg_includes_max_count(self):
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            (root / "doc.txt").write_text("match\nmatch\nmatch\n")
            captured_cmd = []

            original_popen = subprocess.Popen
            def fake_popen(cmd, *args, **kwargs):
                captured_cmd.extend(cmd)
                return original_popen(cmd, *args, **kwargs)

            with patch("makewand.search.subprocess.Popen", side_effect=fake_popen):
                results = safe_search("match", root_path=tmpdir, max_results=2)
                self.assertIn("--max-count", captured_cmd)
                self.assertIn("2", captured_cmd)
                self.assertEqual(len(results), 2)

    def test_safe_search_walk_limits_files_and_bytes(self):
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            for i in range(10):
                (root / f"f{i}.txt").write_text("walk_guard_test\n")

            with patch("makewand.search.find_ripgrep", return_value=None):
                with patch("makewand.search.MAX_WALK_VISITED_FILES", 3):
                    results = safe_search("walk_guard_test", root_path=tmpdir)
                    self.assertLessEqual(len(results), 3)

    def test_safe_search_rg_timeout(self):
        from unittest.mock import patch, MagicMock
        with tempfile.TemporaryDirectory() as tmpdir:
            # Simulate infinite lines from stdout
            mock_proc = MagicMock()
            def infinite_lines():
                while True:
                    yield '{"type":"match","data":{"path":{"text":"f.txt"},"line_number":1,"lines":{"text":"match"}}}\n'
            mock_proc.stdout = infinite_lines()
            mock_proc.terminate = MagicMock()
            mock_proc.wait = MagicMock()

            with patch("makewand.search.find_ripgrep", return_value="/usr/bin/rg"):
                with patch("makewand.search.subprocess.Popen", return_value=mock_proc):
                    with patch("makewand.search.MAX_RG_TIME_SECONDS", 0.05):
                        results = safe_search("match", root_path=tmpdir, max_results=1000000)
                        mock_proc.terminate.assert_called()

    def test_safe_search_rg_silent_blocked_process_hard_timeout(self):
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as tmpdir:
            import time
            # Real subprocess that blocks silently without producing output
            cmd = ["python3", "-c", "import time; time.sleep(10)"]
            proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
            try:
                with patch("makewand.search.find_ripgrep", return_value="/usr/bin/rg"):
                    with patch("makewand.search.subprocess.Popen", return_value=proc):
                        with patch("makewand.search.MAX_RG_TIME_SECONDS", 0.1):
                            start = time.monotonic()
                            results = safe_search("match", root_path=tmpdir)
                            elapsed = time.monotonic() - start
                            # Should terminate quickly near 0.1s, far below 10s
                            self.assertLess(elapsed, 1.5)
            finally:
                try:
                    proc.kill()
                    proc.wait(timeout=0.2)
                except Exception:
                    pass

    def test_safe_search_rg_buffered_multiline_output_preserved(self):
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as tmpdir:
            import time
            file1 = os.path.join(tmpdir, "file1.txt")
            file2 = os.path.join(tmpdir, "file2.txt")
            for f in (file1, file2):
                with open(f, "w", encoding="utf-8") as fp:
                    fp.write("match here\n")

            match1 = json.dumps({"type": "match", "data": {"path": {"text": file1}, "line_number": 1, "submatches": [{"match": {"text": "match"}}]}})
            match2 = json.dumps({"type": "match", "data": {"path": {"text": file2}, "line_number": 1, "submatches": [{"match": {"text": "match"}}]}})
            # Output both matches in a single chunk, then sleep briefly
            py_code = f"import sys, time; sys.stdout.write({repr(match1 + chr(10) + match2 + chr(10))}); sys.stdout.flush(); time.sleep(5)"
            proc = subprocess.Popen(["python3", "-c", py_code], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
            try:
                with patch("makewand.search.find_ripgrep", return_value="/usr/bin/rg"):
                    with patch("makewand.search.subprocess.Popen", return_value=proc):
                        with patch("makewand.search.MAX_RG_TIME_SECONDS", 0.5):
                            results = safe_search("match", root_path=tmpdir)
                            # Both matches must be preserved despite sleep/timeout
                            self.assertIsNotNone(results)
                            self.assertEqual(len(results), 2)
            finally:
                try:
                    proc.kill()
                    proc.wait(timeout=0.2)
                except Exception:
                    pass


if __name__ == "__main__":
    unittest.main()
