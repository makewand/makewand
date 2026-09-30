"""Pre-registered assertions run outside the candidate process."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from acceptance import check

CASES = [([], []), (["a", "a", "b", "a"], [["a", 2], ["b", 1], ["a", 1]]),
 (["", "", "中", "中", "🙂"], [["", 2], ["中", 2], ["🙂", 1]]),
 (["x", "x", "x"], [["x", 3]], "iterator")]
check(sys.argv[1], "runs.py", "encode_runs", CASES)
