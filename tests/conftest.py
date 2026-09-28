"""pytest entry point: isolate all Makewand state before any test module is collected.

pytest imports this conftest before collecting ``tests/``; the module-level
import below redirects HOME/XDG/Makewand state into a private temporary root,
puts AI CLI stubs first on PATH and blocks the local model endpoint (see
``tests/_isolation.py``), so a plain ``pytest`` run can never touch the real
``~/.config/makewand`` or start a real agent CLI.
"""

import sys
from pathlib import Path

_TESTS_DIR = str(Path(__file__).resolve().parent)
if _TESTS_DIR not in sys.path:  # e.g. --import-mode=importlib
    sys.path.insert(0, _TESTS_DIR)

import _isolation  # noqa: E402  (must run before makewand is imported)

ISOLATION = _isolation.activate()


def pytest_configure(config):
    # Idempotent; guarantees isolation even if this conftest is loaded late.
    _isolation.activate()


def pytest_report_header(config):
    return f"makewand test isolation root: {ISOLATION.root}"
