"""Pre-registered assertions run outside the candidate process."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from acceptance import check

CASES = [([], {"records": [], "errors": []}),
 ([{"id": " a ", "value": "+02"}, None, {"id": "b", "value": -3}, {"id": "a", "value": 9}],
  {"records": [{"id": "a", "value": 2}, {"id": "b", "value": -3}], "errors": [{"index": 1, "reason": "invalid"}, {"index": 3, "reason": "duplicate"}]}),
 ([{"id": "x", "value": True}, {"id": "y", "value": 2.0}, {"id": "z", "value": "٢"}, {"id": " ", "value": 1}, {"id": "ok", "value": " -4 "}],
  {"records": [{"id": "ok", "value": -4}], "errors": [{"index": 0, "reason": "invalid"}, {"index": 1, "reason": "invalid"}, {"index": 2, "reason": "invalid"}, {"index": 3, "reason": "invalid"}]}),
 ([{"id": "x", "value": 1}, {"id": "x", "value": "bad"}, {"id": "x", "value": 2}],
  {"records": [{"id": "x", "value": 1}], "errors": [{"index": 1, "reason": "invalid"}, {"index": 2, "reason": "duplicate"}]})]
check(sys.argv[1], "records.py", "recover_records", CASES)
