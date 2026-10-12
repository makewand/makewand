"""[M-AST-merge] Python merges must not bind one name to two different imports."""
try:
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import unittest
from unittest.mock import patch

import makewand.merger as merger


class ImportBindingMergeTests(unittest.TestCase):
    def test_same_position_imports_only_branch_rejects_rebinding(self):
        base = "import os\n\nx = 1\n"
        code_a = "import os\nimport json as codec\n\nx = 1\n"
        code_b = "import os\nimport pickle as codec\n\nx = 1\n"
        # The imports-only branch alone would concatenate both imports.
        ok, merged = merger._merge_disjoint_text(base, code_a, code_b)
        self.assertTrue(ok)
        self.assertIn("import json as codec", merged)
        self.assertIn("import pickle as codec", merged)
        with patch.object(merger, "line_level_3way_merge", return_value=(False, code_a, True)):
            ok, _, strategy = merger.ast_merge_python_file(base, code_a, code_b)
        self.assertFalse(ok)
        self.assertEqual(strategy, "import_binding_conflict")

    def test_git_merge_file_path_rejects_rebinding(self):
        base = "import os\n\n\ndef f():\n    return 1\n\n\ndef g():\n    return 2\n"
        code_a = base.replace("import os\n", "import os\nimport json as codec\n")
        code_b = base + "\n\nfrom pickle import loads as codec\n"
        clean, merged, conflict = merger.line_level_3way_merge(base, code_a, code_b)
        self.assertTrue(clean and not conflict, merged)
        ok, _, strategy = merger.ast_merge_python_file(base, code_a, code_b)
        self.assertFalse(ok)
        self.assertEqual(strategy, "import_binding_conflict")

    def test_from_import_rebinding_across_modules_rejected(self):
        base = "import os\n\nx = 1\n"
        code_a = "import os\nfrom json import loads\n\nx = 1\n"
        code_b = "import os\nfrom pickle import loads\n\nx = 1\n"
        ok, _, strategy = merger.ast_merge_python_file(base, code_a, code_b)
        self.assertFalse(ok)
        self.assertEqual(strategy, "import_binding_conflict")

    def test_compatible_imports_still_merge(self):
        base = "import os\n\nx = 1\n"
        code_a = "import os\nimport json\n\nx = 1\n"
        code_b = "import os\nimport re\nimport os.path\n\nx = 1\n"
        ok, merged, _ = merger.ast_merge_python_file(base, code_a, code_b)
        self.assertTrue(ok)
        self.assertIn("import json", merged)
        self.assertIn("import re", merged)

    def test_same_binding_added_identically_by_both_sides_merges(self):
        base = "import os\n\nx = 1\n"
        code_a = "import os\nimport json as codec\n\nx = 1\n"
        code_b = "import os\nimport json as codec\n\nx = 2\n"
        ok, merged, _ = merger.ast_merge_python_file(base, code_a, code_b)
        self.assertTrue(ok, merged)

    def test_preexisting_rebinding_in_one_side_is_not_blamed_on_merge(self):
        base = "try_first = 1\nimport json as codec\nimport pickle as codec\n\nx = 1\n"
        code_a = base.replace("x = 1", "x = 2")
        code_b = base.replace("try_first = 1", "try_first = 3")
        ok, merged, _ = merger.ast_merge_python_file(base, code_a, code_b)
        self.assertTrue(ok, merged)

    def test_merge_file_content_surfaces_conflict(self):
        import tempfile
        from pathlib import Path
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            paths = {}
            for name, text in (("base", "import os\n\nx = 1\n"),
                               ("a", "import os\nimport json as codec\n\nx = 1\n"),
                               ("b", "import os\nimport pickle as codec\n\nx = 1\n")):
                paths[name] = root / (name + ".py")
                paths[name].write_text(text)
            ok, content, detail = merger.merge_file_content("mod.py", paths["base"], paths["a"], paths["b"])
        self.assertFalse(ok)
        self.assertIsNone(content)
        self.assertIn("import_binding_conflict", detail)


if __name__ == "__main__":
    unittest.main()
