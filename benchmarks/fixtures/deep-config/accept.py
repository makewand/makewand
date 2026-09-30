"""Pre-registered assertions run outside the candidate process."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from acceptance import check, check_deep_config_keys

CASES = [({"base": {}, "override": {}}, {}),
 ({"base": {"a": 1, "nested": {"x": 2, "keep": 3}}, "override": {"nested": {"x": 9, "new": 4}, "b": 5}},
  {"a": 1, "nested": {"x": 9, "keep": 3, "new": 4}, "b": 5}),
 ({"base": {"items": [1, 2], "nullable": {"x": 1}}, "override": {"items": [3], "nullable": None}}, {"items": [3], "nullable": None}),
 ({"base": {"x": 1, "keep": {"y": [1]}}, "override": {"x": {"new": True}}}, {"x": {"new": True}, "keep": {"y": [1]}})]
DETACHED_CASES = [(*case, "detached") for case in CASES]
check(sys.argv[1], "config_api.py", "merge_config", DETACHED_CASES)
check(sys.argv[1], "config_merge.py", "merge_values", DETACHED_CASES)
check_deep_config_keys(sys.argv[1], "config_api.py", "merge_config")
check_deep_config_keys(sys.argv[1], "config_merge.py", "merge_values")
