"""Fixed visible smoke examples; independent acceptance remains external."""
import unittest

from slugs import unique_slugs


class SmokeTests(unittest.TestCase):
    def test_duplicate_titles(self):
        self.assertEqual(unique_slugs(["Hello World", "Hello World"]), ["hello-world", "hello-world-2"])

    def test_empty_input(self):
        self.assertEqual(unique_slugs([]), [])
