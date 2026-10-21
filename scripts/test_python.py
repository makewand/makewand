#!/usr/bin/env python3
"""Standard unittest gate with hermetic Makewand state, independent of pytest.

Isolation (temporary HOME/XDG/config/usage/artifacts/shadow paths, scrubbed
credentials and policy switches, AI CLI stubs on PATH, blocked local model
endpoint) is shared with pytest through ``tests/_isolation.py`` so both entry
points behave the same.

unittest collects only ``TestCase`` methods. Tests written for pytest
(module-level ``test*`` functions, ``Test*`` classes that are not ``TestCase``
subclasses) contribute nothing here, so this gate fails rather than pass
without them: a run or a named module whose pytest-style tests would go unrun,
a named module that yields no tests, and a run with no tests at all. The
complete suite, which CI runs, is ``python3 -I -m pytest -q -p no:cacheprovider tests``.
"""
import inspect
import sys
import unittest
from pathlib import Path

root = Path(__file__).resolve().parent.parent
tests_dir = root / "tests"
sys.path.insert(0, str(root))
sys.path.insert(0, str(tests_dir))

import _isolation  # noqa: E402  (activates isolation before makewand is imported)

_isolation.activate()

PYTEST_GATE = "python3 -I -m pytest -q -p no:cacheprovider tests"


def pytest_only_tests(module):
    """Names in ``module`` that pytest collects as tests and unittest cannot.

    pytest's default rules: functions named ``test*``, and ``Test*`` classes
    without ``__init__`` that are not ``TestCase`` subclasses, imported or not.
    """
    found = []
    for name, value in sorted(vars(module).items()):
        if name.startswith("test") and inspect.isfunction(value):
            found.append(name)
        elif (name.startswith("Test") and inspect.isclass(value)
              and not issubclass(value, unittest.TestCase) and value.__init__ is object.__init__
              and any(attr.startswith("test") and callable(getattr(value, attr, None)) for attr in dir(value))):
            found.append(name)
    return found


def main(names):
    loader = unittest.defaultTestLoader
    if names:
        parts = {name: loader.loadTestsFromName(name) for name in names}
        suite = unittest.TestSuite(parts.values())
        # Only a whole module is checked: naming a class or method is a choice.
        modules = [name for name in names if "." not in name]
    else:
        parts = {}
        suite = loader.discover(str(tests_dir), top_level_dir=str(tests_dir))
        modules = [path.stem for path in sorted(tests_dir.glob("test*.py"))]
    problems = {}
    for name in modules:
        module = sys.modules.get(name)
        skipped = pytest_only_tests(module) if module is not None else []
        if skipped:
            shown = ", ".join(skipped[:3]) + (", ..." if len(skipped) > 3 else "")
            problems[name] = f"{len(skipped)} pytest-style test(s) that unittest cannot run ({shown})"
    for name, part in parts.items():
        if not part.countTestCases():
            problems.setdefault(name, "no unittest tests")
    if not suite.countTestCases() and not problems:
        problems["the run"] = "no tests were collected"

    result = unittest.TextTestRunner(verbosity=2).run(suite)
    # Reported after the run: a successful run must still end with unittest's
    # footer, which the native Windows gate parses.
    if problems:
        print("\nscripts/test_python.py: this run leaves tests unrun:", file=sys.stderr)
        for name, problem in problems.items():
            print(f"  {name}: {problem}", file=sys.stderr)
        print(f"Run the complete suite with: {PYTEST_GATE}", file=sys.stderr)
    return 0 if result.wasSuccessful() and not problems else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
