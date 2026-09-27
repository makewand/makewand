#!/usr/bin/env python3
"""Standard unittest gate with temporary Makewand state, independent of pytest."""
import os
import sys
import tempfile
import unittest
from pathlib import Path

root = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(root))
with tempfile.TemporaryDirectory(prefix="makewand-python-tests-") as temporary:
    state = Path(temporary)
    os.environ["MAKEWAND_CONFIG_DIR"] = str(state / "config")
    os.environ["MAKEWAND_USAGE_FILE"] = str(state / "usage.json")
    # Provider credentials must not change routing in unit tests.
    for key in list(os.environ):
        if (key.endswith("_API_KEY") or key in ("MAKEWAND_HOME", "MAKEWAND_API_POLICY", "ANTHROPIC_AUTH_TOKEN")
                or key.startswith(("MAKEWAND_ENABLE_", "MAKEWAND_DISABLE_"))):
            os.environ.pop(key, None)
    import makewand.config as config
    config.CONFIG_DIR = state / "config"
    config.CONFIG_FILE = config.CONFIG_DIR / "config.json"
    config.API_KEYS_FILE = config.CONFIG_DIR / "api_keys.json"
    config.STATUS_CACHE_FILE = config.CONFIG_DIR / "status.json"
    config.CANDIDATES_DIR = config.CONFIG_DIR / "candidates"
    config.BACKUPS_DIR = config.CONFIG_DIR / "backups"
    config.LEGACY_TRIO_CACHE = state / "legacy" / "trio_status.json"
    # Preserve normal defaults, but never load provider status from a user file.
    if len(sys.argv) > 1:
        sys.path.insert(0, str(root / "tests"))
        suite = unittest.defaultTestLoader.loadTestsFromNames(sys.argv[1:])
    else:
        suite = unittest.defaultTestLoader.discover(str(root / "tests"))
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    sys.exit(not result.wasSuccessful())
