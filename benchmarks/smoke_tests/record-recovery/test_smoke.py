"""Fixed visible smoke examples; independent acceptance remains external."""
import unittest

from records import recover_records


class SmokeTests(unittest.TestCase):
    def test_normalizes_valid_records(self):
        self.assertEqual(recover_records([{"id": " first ", "value": "2"}, {"id": "second", "value": 3}]),
                         {"records": [{"id": "first", "value": 2}, {"id": "second", "value": 3}], "errors": []})

    def test_invalid_row_is_reported(self):
        self.assertEqual(recover_records([None]),
                         {"records": [], "errors": [{"index": 0, "reason": "invalid"}]})
