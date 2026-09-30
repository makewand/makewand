"""Fixed visible smoke examples; independent acceptance remains external."""
import unittest

from windows import moving_averages


class SmokeTests(unittest.TestCase):
    def test_complete_windows(self):
        self.assertEqual(moving_averages({"values": [1, 3, 5], "width": 2}), [2.0, 4.0])

    def test_window_larger_than_input(self):
        self.assertEqual(moving_averages({"values": [1, 3], "width": 4}), [])
