"""Fixed visible smoke examples; independent acceptance remains external."""
import unittest

from paths import normalize_relative_path


class SmokeTests(unittest.TestCase):
    def test_relative_normalization(self):
        self.assertEqual(normalize_relative_path("src/./pkg/../app.py"), "src/app.py")

    def test_root_escape_is_rejected(self):
        with self.assertRaises(ValueError):
            normalize_relative_path("../escape")
