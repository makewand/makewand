"""Fixed visible smoke examples; independent acceptance remains external."""
import unittest

from query import parse_query


class SmokeTests(unittest.TestCase):
    def test_repeated_key(self):
        self.assertEqual(parse_query("name=alice&name=bob"), {"name": ["alice", "bob"]})

    def test_empty_input(self):
        self.assertEqual(parse_query(""), {})
