"""pytest entry point: isolate all Makewand state before any test module is collected.

pytest imports this conftest before collecting ``tests/``; the module-level
import below redirects HOME/XDG/Makewand state into a private temporary root,
puts AI CLI stubs first on PATH, removes proxy settings and refuses network
access other than a test's own loopback servers (see ``tests/_isolation.py``),
so a plain ``pytest`` run can never touch the real ``~/.config/makewand``,
start a real agent CLI or reach a real endpoint.
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


def pytest_terminal_summary(terminalreporter):
    # Every refused name lookup or connection (off-host, a local proxy, Ollama):
    # a test that tried to reach the network shows up here, not silently.
    blocked = _isolation.blocked_connections()
    if blocked:
        terminalreporter.section("makewand test isolation: blocked network access")
        for line in blocked:
            terminalreporter.write_line(line)
