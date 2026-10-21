"""Tests never feed a fixed, year-less reset date to code that reads the real clock.

"Oct 20, 8pm (Asia/Shanghai)" is what the provider CLIs print, and the shared
parser resolves it to the next such moment after the observation. A test that
recorded that literal limit and asserted it was still "limited" passed until
2026-10-20 20:00 and failed on every run afterwards (CI and the release gate
included). Tests that need a reset in the future or the past build it from
datetime.now(), or pass the parser an explicit now=.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
import _isolation  # noqa: F401

import ast
import re
import unittest

TESTS_DIR = Path(__file__).resolve().parent
# Helpers that compare a reset string with the real clock unless given now=.
CLOCK_READERS = {"is_reset_time_passed", "record_engine_limit", "parse_reset_time_to_seconds_left",
                 "parse_reset_datetime"}
RESET_KEYS = {"resets_at", "reset_at"}
# "Oct 20", "Oct 20th" with no year after it, and codex's "10-04 10:40".
_MONTH_DAY = re.compile(r"\b(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*\.? \d{1,2}(?:st|nd|rd|th)?\b"
                        r"(?!,? \d{4})")
_NUMERIC_MONTH_DAY = re.compile(r"(?<![\d-])\d{1,2}-\d{1,2} \d{1,2}:\d{2}\b")


def _year_less_date(node):
    return (isinstance(node, ast.Constant) and isinstance(node.value, str)
            and bool(_MONTH_DAY.search(node.value) or _NUMERIC_MONTH_DAY.search(node.value)))


def fixed_reset_dates(source, filename="<test>"):
    """(line, text) of year-less reset literals that reach the real clock."""
    found = []
    for node in ast.walk(ast.parse(source, filename)):
        if isinstance(node, ast.Call):
            name = getattr(node.func, "attr", None) or getattr(node.func, "id", None)
            if name in CLOCK_READERS and not any(k.arg == "now" for k in node.keywords):
                found += [(arg.lineno, arg.value) for arg in [*node.args, *(k.value for k in node.keywords)]
                          if _year_less_date(arg)]
        elif isinstance(node, ast.Dict):
            found += [(value.lineno, value.value) for key, value in zip(node.keys, node.values)
                      if isinstance(key, ast.Constant) and key.value in RESET_KEYS and _year_less_date(value)]
    return found


class FixedResetDateGuardTests(unittest.TestCase):
    def test_no_test_records_a_fixed_year_less_reset(self):
        offenders = []
        for path in sorted(TESTS_DIR.rglob("*.py")):
            for line, text in fixed_reset_dates(path.read_text(encoding="utf-8"), str(path)):
                offenders.append(f"{path.relative_to(TESTS_DIR)}:{line}: {text!r}")
        self.assertEqual(offenders, [], "build reset dates from datetime.now() or pass now=")

    def test_scanner_flags_the_shapes_that_break(self):
        source = "\n".join([
            'health.record_engine_limit("claude", "limit", "Oct 20, 8pm (Asia/Shanghai)")',
            'is_reset_time_passed("Oct 20th, 8pm", "")',
            'parse_reset_time_to_seconds_left(resets_at_str="10-04 10:40")',
            'status = {"claude": {"status": "limited", "resets_at": "Dec 31, 11pm"}}',
        ])
        self.assertEqual([line for line, _ in fixed_reset_dates(source)], [1, 2, 3, 4])

    def test_scanner_leaves_clock_independent_uses_alone(self):
        source = "\n".join([
            'parse_reset_datetime("Oct 20, 8pm", now=NOW)',
            'is_reset_time_passed("2026-09-20T00:00:00")',
            'is_reset_time_passed("Sep 27th, 2026 10:58 AM")',
            'parse_reset_time_to_seconds_left("2026-09-28 07:53")',
            'parse_claude_quota("Weekly limit reached · resets Oct 20, 8pm (Asia/Shanghai)")',
            'health.record_engine_limit("claude", "limit", _future_month_day("8pm"))',
        ])
        self.assertEqual(fixed_reset_dates(source), [])


if __name__ == "__main__":
    unittest.main()
