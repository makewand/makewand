#!/usr/bin/env python3
"""Standard unittest gate with hermetic Makewand state, independent of pytest.

Isolation (temporary HOME/XDG/config/usage/artifacts/shadow paths, scrubbed
credentials and policy switches, AI CLI stubs on PATH, blocked local model
endpoint) is shared with pytest through ``tests/_isolation.py`` so both entry
points behave the same.
"""
import sys
import unittest
from pathlib import Path

root = Path(__file__).resolve().parent.parent
tests_dir = root / "tests"
sys.path.insert(0, str(root))
sys.path.insert(0, str(tests_dir))

import _isolation  # noqa: E402  (activates isolation before makewand is imported)

_isolation.activate()

if len(sys.argv) > 1:
    suite = unittest.defaultTestLoader.loadTestsFromNames(sys.argv[1:])
else:
    suite = unittest.defaultTestLoader.discover(str(tests_dir), top_level_dir=str(tests_dir))
result = unittest.TextTestRunner(verbosity=2).run(suite)
sys.exit(not result.wasSuccessful())
