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
import unittest

from makewand import health

TESTS_DIR = Path(__file__).resolve().parent
# Helpers that compare a reset string with the real clock unless given now=.
CLOCK_READERS = {"is_reset_time_passed", "record_engine_limit", "parse_reset_time_to_seconds_left",
                 "parse_reset_datetime"}
RESET_KEYS = {"resets_at", "reset_at"}


def _year_less_date(node):
    """A literal health.parse_reset_datetime resolves as a date without a year.

    Uses the parser's own patterns, in its order, so the guard follows any
    format the parser learns: "Oct 20", "20th of oct", codex's "10-04 10:40".
    """
    if not (isinstance(node, ast.Constant) and isinstance(node.value, str)):
        return False
    if health._RESET_EPOCH_RE.search(node.value):
        return False
    _, text = health._reset_zone(node.value.strip())
    if health._RESET_ISO_RE.search(text):
        return False
    named = health._RESET_NAMED_DATE_RE.search(text)
    if named:
        return not (named.group("year") or named.group("year2"))
    numeric = health._RESET_NUMERIC_DATE_RE.search(text)
    return bool(numeric) and health._first_clock(text, exclude=numeric.span()) is not None


def fixed_reset_dates(source, filename="<test>"):
    """(line, text) of year-less reset literals that reach the real clock."""
    found = []
    for node in ast.walk(ast.parse(source, filename)):
        if isinstance(node, ast.Call):
            name = getattr(node.func, "attr", None) or getattr(node.func, "id", None)
            if name in CLOCK_READERS and not any(k.arg == "now" for k in node.keywords):
                found += [(arg.lineno, arg.value) for arg in [*node.args, *(k.value for k in node.keywords)]
                          if _year_less_date(arg)]
            else:  # dict(resets_at=...), live("limited", resets_at=...)
                found += [(k.value.lineno, k.value.value) for k in node.keywords
                          if k.arg in RESET_KEYS and _year_less_date(k.value)]
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
            'cache = dict(claude=live("limited", resets_at="20th of oct, 8pm"))',
            'is_reset_time_passed("4 Nov", "")',
            'record_engine_limit("codex", "limit", "10-04 8pm")',
            'is_reset_time_passed("10-04, 10:40", "")',
            'is_reset_time_passed("10:40 on 10-04 (Asia/Shanghai)", "")',
        ])
        self.assertEqual(sorted(line for line, _ in fixed_reset_dates(source)), list(range(1, 10)))

    def test_scanner_leaves_clock_independent_uses_alone(self):
        source = "\n".join([
            'parse_reset_datetime("Oct 20, 8pm", now=NOW)',
            'is_reset_time_passed("2026-09-20T00:00:00")',
            'is_reset_time_passed("Sep 27th, 2026 10:58 AM")',
            'parse_reset_time_to_seconds_left("2026-09-28 07:53")',
            'is_reset_time_passed("4 Nov 2026, 8pm")',
            'is_reset_time_passed("4 Nov. 2026, 8pm")',
            'is_reset_time_passed("20th of Oct. 2026")',
            'is_reset_time_passed("10-04")',
            'is_reset_time_passed("Claude AI usage limit reached|1760000000")',
            'cache = dict(grok=live("limited", resets_at="in 3 hours"))',
            'parse_claude_quota("Weekly limit reached · resets Oct 20, 8pm (Asia/Shanghai)")',
            'health.record_engine_limit("claude", "limit", _future_month_day("8pm"))',
        ])
        self.assertEqual(fixed_reset_dates(source), [])


if __name__ == "__main__":
    unittest.main()
