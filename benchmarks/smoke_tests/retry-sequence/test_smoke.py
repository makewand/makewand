"""Fixed visible smoke examples; independent acceptance remains external."""
import unittest

from retry import recover_sequence


class SmokeTests(unittest.TestCase):
    def test_stops_after_recovery(self):
        self.assertEqual(recover_sequence(["transient", "ok", "fatal"]),
                         {"status": "ok", "attempts": 2, "delays": [1]})

    def test_empty_sequence(self):
        self.assertEqual(recover_sequence([]), {"status": "failed", "attempts": 0, "delays": []})
