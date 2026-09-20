"""
Unit tests for search guardrail.
"""

import os
import unittest
import tempfile
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

if __name__ == "__main__":
    unittest.main()
