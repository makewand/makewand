"""Fixed visible smoke examples; independent acceptance remains external."""
import unittest

from config_api import merge_config
from config_merge import merge_values


class SmokeTests(unittest.TestCase):
    def test_nested_override_preserves_other_keys(self):
        payload = {"base": {"service": {"port": 80, "host": "localhost"}},
                   "override": {"service": {"port": 8080}}}
        expected = {"service": {"port": 8080, "host": "localhost"}}
        self.assertEqual(merge_values(payload), expected)
        self.assertEqual(merge_config(payload), expected)

    def test_empty_input(self):
        self.assertEqual(merge_values({"base": {}, "override": {}}), {})
        self.assertEqual(merge_config({"base": {}, "override": {}}), {})
