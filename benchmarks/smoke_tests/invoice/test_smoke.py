"""Fixed visible smoke examples; independent acceptance remains external."""
import unittest

from invoice_api import total_invoice
from pricing import price_line


class SmokeTests(unittest.TestCase):
    def test_line_rounding_and_total(self):
        line = {"quantity": 3, "unit_price": "1.235"}
        self.assertEqual(price_line(line), "3.71")
        self.assertEqual(total_invoice([line]), "3.71")

    def test_empty_invoice(self):
        self.assertEqual(total_invoice([]), "0.00")
