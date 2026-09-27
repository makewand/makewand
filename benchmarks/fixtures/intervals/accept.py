"""Independent, versioned acceptance cases, executed outside candidate cwd."""
import importlib.util
import sys
from pathlib import Path

spec = importlib.util.spec_from_file_location("candidate", Path(sys.argv[1]) / "intervals.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
merge = module.merge_intervals
for value, expected in [([], []), ([(2, 4), (1, 3)], [(1, 4)]),
                        ([(1, 2), (2, 3), (5, 5)], [(1, 3), (5, 5)]),
                        ([(1, 2), (3, 4)], [(1, 2), (3, 4)]),
                        ([(-8, -2), (-5, 1), (0, 0)], [(-8, 1)]),
                        ([(1, 9), (2, 3), (1, 9)], [(1, 9)])]:
    original = list(value)
    actual = merge(value)
    assert actual == expected, (value, actual, expected)
    assert value == original, "input mutated"
assert merge(iter([(3, 4), (1, 3)])) == [(1, 4)]
try:
    merge([(5, 1)])
except ValueError:
    pass
else:
    raise AssertionError("reversed bounds accepted")
print("independent acceptance passed")
