"""Fixed visible smoke examples; independent acceptance remains external."""
import unittest

from runs import encode_runs


class SmokeTests(unittest.TestCase):
    def test_consecutive_values(self):
        self.assertEqual(encode_runs(["a", "a", "b"]), [["a", 2], ["b", 1]])

    def test_empty_iterator(self):
        self.assertEqual(encode_runs(iter([])), [])
