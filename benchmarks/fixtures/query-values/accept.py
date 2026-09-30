"""Pre-registered assertions run outside the candidate process."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from acceptance import check

CASES = [("", {}), ("a=1&a=2&b=&flag", {"a": ["1", "2"], "b": [""], "flag": [""]}),
 ("q=hello+world&%71=again&x=%E4%B8%AD", {"q": ["hello world", "again"], "x": ["中"]}),
 ("&&a=x%26y%3Dz&&=empty", {"a": ["x&y=z"], "": ["empty"]}),
 ("a=%ZZ", ValueError), ("a=%1", ValueError), ("a=%FF", ValueError)]
check(sys.argv[1], "query.py", "parse_query", CASES)
