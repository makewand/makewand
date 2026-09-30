"""Pre-registered assertions run outside the candidate process."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from acceptance import check

CASES = [([], {"debug": 0, "info": 0, "warning": 0, "error": 0}),
 (["[INFO] hello", " [error] failed ", "[WARNING] beware", "[DEBUG] detail", "[INFO] again", "ERROR outside", "[INFO]"],
  {"debug": 1, "info": 2, "warning": 1, "error": 1}),
 (["[TRACE] unknown", "[ERROR]text", None, 4, "[info]  good", "[warning]  "],
  {"debug": 0, "info": 1, "warning": 0, "error": 0})]
check(sys.argv[1], "logs_api.py", "summarize_logs", CASES)
check(sys.argv[1], "log_parse.py", "parse_level", [("[INFO] hello", "info"), (" [wArNiNg] hi ", "warning"), ("[ERROR]text", None),
 ("[DEBUG] ", None), ("[TRACE] hi", None), (None, None), (4, None)])
