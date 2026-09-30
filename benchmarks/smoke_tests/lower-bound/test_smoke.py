"""Fixed visible smoke examples; independent acceptance remains external."""
import unittest

from search import lower_bound


class SmokeTests(unittest.TestCase):
    def test_insertion_between_items(self):
        self.assertEqual(lower_bound({"items": [1, 4, 7], "target": 3}), 1)

    def test_empty_input(self):
        self.assertEqual(lower_bound({"items": [], "target": 3}), 0)
