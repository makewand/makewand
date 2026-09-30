"""Pre-registered assertions run outside the candidate process."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from acceptance import check

CASES = [([], "0.00"), ([{"quantity": 3, "unit_price": "0.10"}, {"quantity": 1, "unit_price": "1.235"}], "1.54"),
 ([{"quantity": 1, "unit_price": "0.005"}, {"quantity": 1, "unit_price": "0.005"}], "0.02"),
 ([{"quantity": 0, "unit_price": "123"}], "0.00"), ([{"quantity": -1, "unit_price": "2"}], ValueError),
 ([{"quantity": True, "unit_price": "2"}], ValueError), ([{"quantity": 1, "unit_price": "NaN"}], ValueError),
 ([{"quantity": 1, "unit_price": "Infinity"}], ValueError), ([{"quantity": 1, "unit_price": "-0.01"}], ValueError),
 ([{"quantity": 1}], ValueError)]
check(sys.argv[1], "invoice_api.py", "total_invoice", CASES)
check(sys.argv[1], "pricing.py", "price_line", [({"quantity": 2, "unit_price": "1.005"}, "2.01"), ({"quantity": 1, "unit_price": "1.235"}, "1.24"),
 ({"quantity": 0, "unit_price": "1"}, "0.00"), ({"quantity": 1.5, "unit_price": "2"}, ValueError),
 ({"quantity": 1, "unit_price": "bad"}, ValueError), ({"quantity": 1, "unit_price": "-Infinity"}, ValueError)])
