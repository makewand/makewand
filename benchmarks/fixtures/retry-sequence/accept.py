"""Pre-registered assertions run outside the candidate process."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from acceptance import check

CASES = [([], {"status": "failed", "attempts": 0, "delays": []}),
 (["ok", "fatal"], {"status": "ok", "attempts": 1, "delays": []}),
 (["transient", "transient", "ok"], {"status": "ok", "attempts": 3, "delays": [1, 2]}),
 (["fatal", "ok"], {"status": "failed", "attempts": 1, "delays": []}),
 (["transient"], {"status": "failed", "attempts": 1, "delays": []}),
 (["transient", "transient", "transient", "ok"], {"status": "failed", "attempts": 3, "delays": [1, 2]}),
 (["transient", "fatal", "ok"], {"status": "failed", "attempts": 2, "delays": [1]}),
 (["ok", "unknown"], ValueError)]
check(sys.argv[1], "retry.py", "recover_sequence", CASES)
