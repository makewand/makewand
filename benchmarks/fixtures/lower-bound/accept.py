"""Pre-registered assertions run outside the candidate process."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from acceptance import check

CASES = [({"items": [], "target": 2}, 0), ({"items": [1, 2, 2, 4], "target": 2}, 1),
 ({"items": [-5, -2, 0], "target": -3}, 1), ({"items": [1, 3], "target": 2}, 1),
 ({"items": [1, 3], "target": 8}, 2), ({"items": [3, 1], "target": 2}, ValueError)]
check(sys.argv[1], "search.py", "lower_bound", CASES)
