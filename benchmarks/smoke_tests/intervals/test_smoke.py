"""Fixed visible smoke examples; independent acceptance remains external."""
import unittest

from intervals import merge_intervals


class SmokeTests(unittest.TestCase):
    def test_overlapping_intervals(self):
        self.assertEqual(merge_intervals([(4, 7), (1, 3), (3, 5)]), [(1, 7)])

    def test_empty_input(self):
        self.assertEqual(merge_intervals([]), [])
