"""Assertions execute outside the candidate process."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from acceptance import check

check(sys.argv[1], "intervals.py", "merge_intervals", [([], []), ([(2, 4), (1, 3)], [(1, 4)]),
                        ([(1, 2), (2, 3), (5, 5)], [(1, 3), (5, 5)]),
                        ([(1, 2), (3, 4)], [(1, 2), (3, 4)]),
                        ([(-8, -2), (-5, 1), (0, 0)], [(-8, 1)]),
                        ([(1, 9), (2, 3), (1, 9)], [(1, 9)]),
                        ([(3, 4), (1, 3)], [(1, 4)], "iterator"),
                        ([(5, 1)], ValueError)])
