"""Fixed visible smoke examples; independent acceptance remains external."""
import unittest

from log_parse import parse_level
from logs_api import summarize_logs


class SmokeTests(unittest.TestCase):
    def test_valid_levels(self):
        self.assertEqual(parse_level("[INFO] ready"), "info")
        self.assertEqual(summarize_logs(["[INFO] ready", "[ERROR] failed"]),
                         {"debug": 0, "info": 1, "warning": 0, "error": 1})

    def test_invalid_line(self):
        self.assertIsNone(parse_level("not a log record"))
