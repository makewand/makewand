"""Pre-registered assertions run outside the candidate process."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from acceptance import check

CASES = [({"values": [], "width": 1}, []), ({"values": [2, 4, 6, 8], "width": 2}, [3.0, 5.0, 7.0]),
 ({"values": [-2, 0, 2], "width": 3}, [0.0]), ({"values": [7], "width": 2}, []),
 ({"values": [1, 2], "width": 1}, [1.0, 2.0]), ({"values": [1], "width": 0}, ValueError),
 ({"values": [1], "width": -1}, ValueError), ({"values": [1], "width": True}, ValueError),
 ({"values": [1], "width": 1.0}, ValueError)]
check(sys.argv[1], "windows.py", "moving_averages", CASES)
